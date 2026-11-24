package server

import (
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/shi-gg/linkdave/server/audio/filter"
	"github.com/shi-gg/linkdave/server/protocol"
)

const (
	WRITE_TIMEOUT    = 10 * time.Second
	PONG_TIMEOUT     = 60 * time.Second
	PING_PERIOD      = (PONG_TIMEOUT * 9) / 10
	MAX_MESSAGE_SIZE = 512 * 1024 // 512KB

	// MIGRATION_RETENTION keeps a settled snapshot around so a repeated
	// PlayerMigrate request can replay it verbatim instead of re-advancing the
	// playback position.
	MIGRATION_RETENTION = 2 * time.Minute

	// DEFAULT_MIGRATION_CONFIRM_TIMEOUT bounds how long a handed-off snapshot
	// stays recoverable. If the client neither confirms nor rejects in time, the
	// handshake is treated as successful so the old node does not hold a migrated
	// player (or its voice conn) forever. Set LINKDAVE_MIGRATION_CONFIRM_TIMEOUT_MS
	// to override; a value of 0 leaves confirmation to the drain deadline.
	DEFAULT_MIGRATION_CONFIRM_TIMEOUT = 3 * time.Second
)

// Migration is the frozen handshake snapshot for one guild. The position is
// computed exactly once, when the migration begins, so any repeated request
// observes the same value instead of advancing the position a second time.
type Migration struct {
	ID          string
	GuildID     snowflake.ID
	Target      string
	URL         string
	Position    int64
	State       string
	RequesterID string
	Filters     *filter.Filters
	Settled     bool
	Pending     bool
	UpdatedAt   time.Time
}

func (m *Migration) snapshot() protocol.MigrateReadyData {
	return protocol.MigrateReadyData{
		GuildID:     m.GuildID,
		MigrationID: m.ID,
		Target:      m.Target,
		URL:         m.URL,
		Position:    m.Position,
		State:       m.State,
		RequesterID: m.RequesterID,
		Filters:     m.Filters.Normalize(),
	}
}

type Player struct {
	mutex       sync.RWMutex
	guildID     snowflake.ID
	channelID   snowflake.ID
	state       string
	currentURL  string
	position    int64
	startedAt   time.Time
	requesterID string
	filters     *filter.Filters
	migrating   bool
}

type Client struct {
	server     *Server
	conn       *websocket.Conn
	sendCh     chan any
	sessionID  string
	clientName string

	players         map[snowflake.ID]*Player
	migrations      map[snowflake.ID]*Migration
	migrationTimers map[snowflake.ID]*time.Timer
	playersMu       sync.RWMutex

	closeChan chan struct{}
	closeOnce sync.Once
}

func NewClient(server *Server, conn *websocket.Conn, clientName string) *Client {
	return &Client{
		server:          server,
		conn:            conn,
		sendCh:          make(chan any, 256),
		sessionID:       uuid.New().String(),
		clientName:      clientName,
		players:         make(map[snowflake.ID]*Player),
		migrations:      make(map[snowflake.ID]*Migration),
		migrationTimers: make(map[snowflake.ID]*time.Timer),
		closeChan:       make(chan struct{}),
	}
}

func (c *Client) readPump() {
	defer func() {
		c.close()
	}()

	c.conn.SetReadLimit(MAX_MESSAGE_SIZE)
	c.conn.SetReadDeadline(time.Now().Add(PONG_TIMEOUT))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(PONG_TIMEOUT))
		return nil
	})

	for {
		msgType, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.server.logger.Error("websocket read error", slog.Any("error", err))
			}
			return
		}
		c.server.handleMessage(c, msgType, message)
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(PING_PERIOD)
	defer func() {
		ticker.Stop()
		c.close()
	}()

	for {
		select {
		case message, ok := <-c.sendCh:
			c.conn.SetWriteDeadline(time.Now().Add(WRITE_TIMEOUT))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			data, err := json.Marshal(message)
			if err != nil {
				c.server.logger.Error("failed to marshal message", slog.Any("error", err))
				continue
			}

			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				c.server.logger.Error("failed to write message", slog.Any("error", err))
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(WRITE_TIMEOUT))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-c.closeChan:
			return
		}
	}
}

func (c *Client) send(msg protocol.Message) {
	select {
	case c.sendCh <- msg:
	default:
		c.server.logger.Warn("client send buffer full, dropping message")
	}
}

func (c *Client) close() {
	c.closeOnce.Do(func() {
		close(c.closeChan)
		c.conn.Close()
		c.server.unregisterClient(c)
		c.server.logger.Info("client disconnected", slog.String("session", c.sessionID))
	})
}

func (c *Client) getOrCreatePlayer(guildID snowflake.ID) *Player {
	c.playersMu.Lock()
	defer c.playersMu.Unlock()

	if player, ok := c.players[guildID]; ok {
		return player
	}

	player := &Player{
		guildID: guildID,
		state:   protocol.PlayerStateIdle,
	}
	c.players[guildID] = player
	return player
}

func (c *Client) getPlayer(guildID snowflake.ID) *Player {
	c.playersMu.RLock()
	defer c.playersMu.RUnlock()
	return c.players[guildID]
}

// removePlayer forgets a player and any settled migration snapshot. It is used
// when playback truly ends; a migration in progress is cancelled first so no
// timer later resurrects a stopped player.
func (c *Client) removePlayer(guildID snowflake.ID) {
	c.playersMu.Lock()
	defer c.playersMu.Unlock()

	c.cancelMigrationLocked(guildID)
	delete(c.players, guildID)
	delete(c.migrations, guildID)
}

func (c *Client) cancelMigrationLocked(guildID snowflake.ID) {
	if timer := c.migrationTimers[guildID]; timer != nil {
		timer.Stop()
	}
	delete(c.migrationTimers, guildID)
}

// playerGuildIDs returns the guild IDs of this client's players in a stable order.
func (c *Client) playerGuildIDs() []snowflake.ID {
	c.playersMu.RLock()
	defer c.playersMu.RUnlock()

	ids := make([]snowflake.ID, 0, len(c.players))
	for id := range c.players {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// activePlayerCount counts players still owned by this node for drain purposes.
// A player whose snapshot has been handed off is excluded: it no longer needs
// the old node to wait, even though it stays in the map until it is confirmed
// (or restored if the handshake fails).
func (c *Client) activePlayerCount() int {
	c.playersMu.RLock()
	defer c.playersMu.RUnlock()

	var total int
	for _, player := range c.players {
		if !player.IsMigrating() {
			total++
		}
	}
	return total
}

func (c *Client) pendingMigrationCount() int {
	c.playersMu.RLock()
	defer c.playersMu.RUnlock()

	var total int
	for _, migration := range c.migrations {
		if migration.Pending {
			total++
		}
	}
	return total
}

// destroyAllPlayers tears down every voice connection owned by a client that is
// already gone. Release closes the audio source and voice conn without emitting
// events into a dead socket, so nothing survives the disconnect.
func (c *Client) destroyAllPlayers() {
	guildIDs := c.playerGuildIDs()

	c.playersMu.Lock()
	for id := range c.migrationTimers {
		c.cancelMigrationLocked(id)
	}
	c.players = make(map[snowflake.ID]*Player)
	c.migrations = make(map[snowflake.ID]*Migration)
	c.playersMu.Unlock()

	for _, guildID := range guildIDs {
		c.server.voiceManager.Release(c.sessionID, guildID)
	}
}

func (c *Client) pruneMigrationsLocked(now time.Time) {
	for guildID, migration := range c.migrations {
		if migration.Pending {
			continue
		}
		if now.Sub(migration.UpdatedAt) > MIGRATION_RETENTION {
			delete(c.migrations, guildID)
		}
	}
}

// beginMigration freezes the snapshot for a guild. The first call hands the
// player off (it stops counting toward the drain) but keeps it recoverable;
// later calls replay the identical snapshot, so a repeated request cannot
// advance the position or produce a second effective migration.
func (c *Client) beginMigration(guildID snowflake.ID, target, requestedID string) (protocol.MigrateReadyData, string) {
	c.playersMu.Lock()
	defer c.playersMu.Unlock()

	c.pruneMigrationsLocked(time.Now())

	if existing := c.migrations[guildID]; existing != nil {
		return existing.snapshot(), ""
	}

	player := c.players[guildID]
	if player == nil {
		return protocol.MigrateReadyData{}, protocol.MigrateReasonPlayerNotFound
	}

	url, position, state, requesterID, filters := player.GetMigrateData()
	if state == protocol.PlayerStateIdle || url == "" {
		return protocol.MigrateReadyData{}, protocol.MigrateReasonNoTrack
	}

	id := requestedID
	if id == "" {
		id = uuid.New().String()
	}

	migration := &Migration{
		ID:          id,
		GuildID:     guildID,
		Target:      target,
		URL:         url,
		Position:    position,
		State:       state,
		RequesterID: requesterID,
		Filters:     filters,
		Pending:     true,
		UpdatedAt:   time.Now(),
	}
	c.migrations[guildID] = migration
	player.SetMigrating(true)

	if timeout := c.server.MigrationConfirmTimeout(); timeout > 0 {
		migrationID := id
		c.migrationTimers[guildID] = time.AfterFunc(timeout, func() {
			c.confirmMigrationByTimeout(guildID, migrationID)
		})
	}

	return migration.snapshot(), ""
}

// settleMigration finalizes a successful handshake exactly once. Repeated
// confirmations return the same snapshot and change nothing, so a duplicate
// settle cannot tear down a player twice or race a new playback.
func (c *Client) settleMigration(guildID snowflake.ID, migrationID, reason string) (string, protocol.MigrateReadyData) {
	c.playersMu.Lock()

	migration := c.migrations[guildID]
	if migration == nil {
		c.playersMu.Unlock()
		return protocol.MigrationOutcomeNotFound, protocol.MigrateReadyData{}
	}
	if migrationID != "" && migration.ID != migrationID {
		snapshot := migration.snapshot()
		c.playersMu.Unlock()
		return protocol.MigrationOutcomeMismatch, snapshot
	}
	if migration.Settled {
		snapshot := migration.snapshot()
		c.playersMu.Unlock()
		return protocol.MigrationOutcomeAlreadySettled, snapshot
	}

	migration.Settled = true
	migration.Pending = false
	migration.UpdatedAt = time.Now()
	c.cancelMigrationLocked(guildID)
	delete(c.players, guildID)

	snapshot := migration.snapshot()
	c.playersMu.Unlock()

	// Release without an event: the player now lives on another node, so the old
	// node must not emit a misleading voiceDisconnect. This also closes the audio
	// source and the voice conn.
	c.server.voiceManager.Release(c.sessionID, guildID)
	c.server.logger.Info("player migration settled",
		slog.String("guild_id", guildID.String()),
		slog.String("migration_id", snapshot.MigrationID),
		slog.String("reason", reason),
	)
	return protocol.MigrationOutcomeSettled, snapshot
}

func (c *Client) confirmMigrationByTimeout(guildID snowflake.ID, migrationID string) {
	outcome, snapshot := c.settleMigration(guildID, migrationID, protocol.MigrationReasonConfirmTimeout)
	if outcome != protocol.MigrationOutcomeSettled {
		return
	}

	c.send(protocol.Message{
		Op: protocol.OpMigrationResult,
		Data: protocol.MigrationResultData{
			GuildID:     guildID,
			MigrationID: snapshot.MigrationID,
			Outcome:     protocol.MigrationOutcomeSettled,
			Reason:      protocol.MigrationReasonConfirmTimeout,
		},
	})
}

// failMigration rolls a failed handshake back. The player stays on the old node
// at its frozen position and counts toward the drain again, so the node keeps
// waiting instead of exiting early. Failure never marks the player idle.
func (c *Client) failMigration(guildID snowflake.ID, migrationID, reason string) (protocol.MigrateReadyData, bool) {
	c.playersMu.Lock()
	defer c.playersMu.Unlock()

	migration := c.migrations[guildID]
	if migration == nil || migration.Settled {
		return protocol.MigrateReadyData{}, false
	}
	if migrationID != "" && migration.ID != migrationID {
		return protocol.MigrateReadyData{}, false
	}

	c.cancelMigrationLocked(guildID)
	delete(c.migrations, guildID)

	if player := c.players[guildID]; player != nil {
		player.SetMigrating(false)
		player.RestoreAfterMigration(migration.Position, migration.State)
	}

	c.server.logger.Warn("player migration failed, keeping player",
		slog.String("guild_id", guildID.String()),
		slog.String("migration_id", migration.ID),
		slog.String("reason", reason),
	)

	return migration.snapshot(), true
}

// syncMigrationAfterControl mirrors a pause/resume/stop onto any pending
// handshake. A stop means nothing is left to migrate, so the handshake is
// cancelled and the client is told to stay put; pause/resume refresh the frozen
// state so the migrated state cannot contradict the old node's last report.
func (c *Client) syncMigrationAfterControl(guildID snowflake.ID, state string, position int64) {
	c.playersMu.Lock()

	migration := c.migrations[guildID]
	if migration == nil || migration.Settled || !migration.Pending {
		c.playersMu.Unlock()
		return
	}

	if state == protocol.PlayerStateIdle {
		c.cancelMigrationLocked(guildID)
		delete(c.migrations, guildID)
		if player := c.players[guildID]; player != nil {
			player.SetMigrating(false)
		}
		migrationID := migration.ID
		c.playersMu.Unlock()

		c.send(protocol.Message{
			Op: protocol.OpMigrationResult,
			Data: protocol.MigrationResultData{
				GuildID:     guildID,
				MigrationID: migrationID,
				Outcome:     protocol.MigrationOutcomeAborted,
				Reason:      protocol.TrackEndReasonStopped,
			},
		})
		return
	}

	migration.State = state
	migration.Position = position
	migration.UpdatedAt = time.Now()
	snapshot := migration.snapshot()
	c.playersMu.Unlock()

	c.send(protocol.Message{
		Op:   protocol.OpMigrateReady,
		Data: snapshot,
	})
}

// refreshMigrationPosition keeps the frozen snapshot aligned after a seek so a
// migration never reports a position the player already left.
func (c *Client) refreshMigrationPosition(guildID snowflake.ID, position int64) {
	c.playersMu.Lock()
	defer c.playersMu.Unlock()

	migration := c.migrations[guildID]
	if migration == nil || migration.Settled || !migration.Pending {
		return
	}
	migration.Position = position
	migration.UpdatedAt = time.Now()
}

// resetMigration discards any snapshot and migrating flag for a guild. Called
// when playback restarts so the next handshake takes a fresh snapshot instead of
// replaying a stale one.
func (c *Client) resetMigration(guildID snowflake.ID) {
	c.playersMu.Lock()
	defer c.playersMu.Unlock()

	c.cancelMigrationLocked(guildID)
	delete(c.migrations, guildID)
	if player := c.players[guildID]; player != nil {
		player.SetMigrating(false)
	}
}

func (p *Player) IsMigrating() bool {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.migrating
}

func (p *Player) SetMigrating(migrating bool) {
	p.mutex.Lock()
	p.migrating = migrating
	p.mutex.Unlock()
}

// RestoreAfterMigration puts a player back at the frozen snapshot position after
// a failed handshake. The state is never downgraded to idle, so a failed
// migration cannot make the player look finished.
func (p *Player) RestoreAfterMigration(position int64, state string) {
	p.mutex.Lock()
	if p.state != protocol.PlayerStateIdle {
		p.state = state
	}
	p.position = position
	p.startedAt = time.Now()
	p.mutex.Unlock()
}

func (p *Player) GetState() string {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.state
}

func (p *Player) SetState(state string) {
	p.mutex.Lock()
	p.state = state
	p.mutex.Unlock()
}

func (p *Player) GetCurrentURL() string {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.currentURL
}

func (p *Player) SetCurrentURL(url string) {
	p.mutex.Lock()
	p.currentURL = url
	p.mutex.Unlock()
}

func (p *Player) GetPosition() int64 {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.position
}

func (p *Player) SetPosition(pos int64) {
	p.mutex.Lock()
	p.position = pos
	p.mutex.Unlock()
}

func (p *Player) GetStartedAt() time.Time {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.startedAt
}

func (p *Player) SetStartedAt(t time.Time) {
	p.mutex.Lock()
	p.startedAt = t
	p.mutex.Unlock()
}

func (p *Player) GetChannelID() snowflake.ID {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.channelID
}

func (p *Player) SetChannelID(id snowflake.ID) {
	p.mutex.Lock()
	p.channelID = id
	p.mutex.Unlock()
}

func (p *Player) SetPlayingState(url string, position int64, requesterID string, filters *filter.Filters) {
	p.mutex.Lock()
	p.state = protocol.PlayerStatePlaying
	p.currentURL = url
	p.position = position
	p.startedAt = time.Now()
	p.requesterID = requesterID
	p.filters = filters.Normalize()
	p.mutex.Unlock()
}

func (p *Player) GetRequesterID() string {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.requesterID
}

func (p *Player) SetRequesterID(id string) {
	p.mutex.Lock()
	p.requesterID = id
	p.mutex.Unlock()
}

func (p *Player) SetIdleState() {
	p.mutex.Lock()
	p.state = protocol.PlayerStateIdle
	p.currentURL = ""
	p.requesterID = ""
	p.position = 0
	p.mutex.Unlock()
}

func (p *Player) SetPausedState(position int64) {
	p.mutex.Lock()
	p.state = protocol.PlayerStatePaused
	p.position = position
	p.mutex.Unlock()
}

func (p *Player) GetMigrateData() (url string, position int64, state string, requesterID string, filters *filter.Filters) {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	calculatedPos := p.position
	if p.state == protocol.PlayerStatePlaying {
		calculatedPos = time.Since(p.startedAt).Milliseconds() + p.position
	}
	return p.currentURL, calculatedPos, p.state, p.requesterID, p.filters
}
