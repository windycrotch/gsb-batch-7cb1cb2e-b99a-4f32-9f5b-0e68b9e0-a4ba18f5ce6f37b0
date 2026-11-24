package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/gorilla/websocket"
	"github.com/shi-gg/linkdave/server/audio/source"
	"github.com/shi-gg/linkdave/server/protocol"
	"github.com/shi-gg/linkdave/server/voice"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for now
	},
}

type Server struct {
	logger       *slog.Logger
	voiceManager *voice.Manager
	clients      map[string]*Client
	clientsMu    sync.RWMutex
	startTime    time.Time
	draining     bool
	drainMu      sync.RWMutex
	version      string
	password     string

	migrationConfirmTimeout time.Duration
}

func NewServer(logger *slog.Logger, voiceManager *voice.Manager, version string, password string) *Server {
	s := &Server{
		logger:                  logger,
		voiceManager:            voiceManager,
		clients:                 make(map[string]*Client),
		startTime:               time.Now(),
		version:                 version,
		password:                password,
		migrationConfirmTimeout: DEFAULT_MIGRATION_CONFIRM_TIMEOUT,
	}
	voiceManager.SetEventHandler(s)
	s.startTickers()
	return s
}

// SetMigrationConfirmTimeout overrides how long a handed-off snapshot stays
// recoverable before the old node assumes the migration succeeded. Setting it to
// zero leaves confirmation to the drain deadline.
func (s *Server) SetMigrationConfirmTimeout(timeout time.Duration) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	s.migrationConfirmTimeout = timeout
}

func (s *Server) MigrationConfirmTimeout() time.Duration {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	return s.migrationConfirmTimeout
}

func (s *Server) startTickers() {
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for range ticker.C {
			s.sendStats()
		}
	}()
}

func (s *Server) sendStats() {
	stats := s.GetStats()
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	for _, client := range s.clients {
		client.send(protocol.Message{
			Op:   protocol.OpStats,
			Data: stats,
		})
	}
}

func (s *Server) OnTrackEnd(sessionID string, guildID snowflake.ID, src source.Source, reason string) {
	client := s.getClientBySession(sessionID)
	if client == nil {
		return
	}

	player := client.getPlayer(guildID)
	if player == nil {
		return
	}

	track := protocol.TrackInfo{
		URL:         src.URL(),
		Duration:    src.Duration(),
		RequesterID: player.GetRequesterID(),
	}

	if reason != protocol.TrackEndReasonReplaced && reason != protocol.TrackEndReasonStopped {
		player.SetIdleState()
	}

	client.send(protocol.Message{
		Op: protocol.OpTrackEnd,
		Data: protocol.TrackEndData{
			GuildID: guildID,
			Track:   track,
			Reason:  reason,
		},
	})
}

func (s *Server) OnTrackException(sessionID string, guildID snowflake.ID, src source.Source, err error) {
	client := s.getClientBySession(sessionID)
	if client == nil {
		return
	}

	player := client.getPlayer(guildID)

	track := protocol.TrackInfo{
		URL:      src.URL(),
		Duration: src.Duration(),
	}

	if player != nil {
		track.RequesterID = player.GetRequesterID()
	}

	client.send(protocol.Message{
		Op: protocol.OpTrackError,
		Data: protocol.TrackErrorData{
			GuildID: guildID,
			Track:   track,
			Error:   err.Error(),
		},
	})
}

func (s *Server) OnVoiceDisconnected(sessionID string, guildID snowflake.ID) {
	client := s.getClientBySession(sessionID)
	if client == nil {
		return
	}

	// A connection that drops while draining is a migration failure. Keep the
	// player on the old node (out of idle) so the drain keeps waiting until the
	// deadline instead of exiting early.
	if s.IsDraining() {
		if client.getPlayer(guildID) != nil {
			client.failMigration(guildID, "", protocol.DisconnectReasonConnectionLost)
			return
		}
	}

	client.removePlayer(guildID)

	client.send(protocol.Message{
		Op: protocol.OpVoiceDisconnect,
		Data: protocol.VoiceDisconnectData{
			GuildID: guildID,
			Reason:  protocol.DisconnectReasonConnectionLost,
		},
	})
}

func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.IsDraining() {
		http.Error(w, "Node is draining", http.StatusServiceUnavailable)
		return
	}

	if s.password != "" && r.URL.Query().Get("password") != s.password {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	clientName := r.Header.Get("Client-Name")
	if clientName == "" {
		clientName = "unknown"
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("failed to upgrade websocket", slog.Any("error", err))
		return
	}

	client := NewClient(s, conn, clientName)
	s.registerClient(client)

	s.logger.Info("client connected",
		slog.String("client", clientName),
		slog.String("session", client.sessionID),
		slog.String("addr", r.RemoteAddr),
	)

	client.send(protocol.Message{
		Op: protocol.OpReady,
		Data: protocol.ReadyData{
			SessionID: client.sessionID,
			Resumed:   false,
		},
	})

	go client.readPump()
	go client.writePump()
}

func (s *Server) registerClient(client *Client) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	s.clients[client.sessionID] = client
}

func (s *Server) unregisterClient(client *Client) {
	s.clientsMu.Lock()
	delete(s.clients, client.sessionID)
	s.clientsMu.Unlock()

	// Clean up all voice connections for this client's players. This is what
	// makes a mid-handshake disconnect leave no connection or source behind.
	client.destroyAllPlayers()
}

func (s *Server) getClientBySession(sessionID string) *Client {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	return s.clients[sessionID]
}

func (s *Server) handleMessage(client *Client, msgType int, data []byte) {
	if msgType != websocket.TextMessage {
		return
	}

	var msg struct {
		Op   uint8           `json:"op"`
		Data json.RawMessage `json:"d"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		s.logger.Error("failed to unmarshal message", slog.Any("error", err))
		return
	}

	switch msg.Op {
	case protocol.OpVoiceUpdate:
		s.handleVoiceUpdate(client, msg.Data)
	case protocol.OpPlayerMigrate:
		s.handlePlayerMigrate(client, msg.Data)
	case protocol.OpMigrateSettle:
		s.handleMigrateSettle(client, msg.Data)
	default:
		s.logger.Warn("unknown op code", slog.Uint64("op", uint64(msg.Op)))
	}
}

func (s *Server) handleVoiceUpdate(client *Client, data json.RawMessage) {
	var update protocol.VoiceUpdateData
	if err := json.Unmarshal(data, &update); err != nil {
		s.logger.Error("failed to unmarshal voice update", slog.Any("error", err))
		return
	}

	s.logger.Info("voice update received",
		slog.String("guild_id", update.GuildID.String()),
		slog.String("channel_id", update.ChannelID.String()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := s.voiceManager.Connect(ctx, client.sessionID, update.ClientID, update.GuildID, update.ChannelID, update.SessionID, update.Event)
	if err != nil {
		s.logger.Error("failed to connect to voice", slog.Any("error", err))
		client.removePlayer(update.GuildID)
		if disconnectErr := s.voiceManager.Disconnect(client.sessionID, update.GuildID); disconnectErr != nil {
			s.logger.Error("failed to clean up failed voice connection", slog.Any("error", disconnectErr))
		}

		client.send(protocol.Message{
			Op: protocol.OpVoiceDisconnect,
			Data: protocol.VoiceDisconnectData{
				GuildID: update.GuildID,
				Reason:  protocol.DisconnectReasonConnectionFailed,
			},
		})

		return
	}

	player := client.getOrCreatePlayer(update.GuildID)
	player.SetChannelID(update.ChannelID)

	client.send(protocol.Message{
		Op: protocol.OpVoiceConnect,
		Data: protocol.VoiceConnectData{
			GuildID:   update.GuildID,
			ChannelID: update.ChannelID,
		},
	})
}

func (s *Server) handlePlayerMigrate(client *Client, data json.RawMessage) {
	var migrate protocol.PlayerMigrateData
	if err := json.Unmarshal(data, &migrate); err != nil {
		s.logger.Error("failed to unmarshal player migrate", slog.Any("error", err))
		return
	}

	snapshot, reason := client.beginMigration(migrate.GuildID, migrate.Target, migrate.MigrationID)
	if reason != "" {
		s.logger.Warn("player migration unavailable",
			slog.String("guild_id", migrate.GuildID.String()),
			slog.String("reason", reason),
		)
		client.send(protocol.Message{
			Op: protocol.OpMigrationResult,
			Data: protocol.MigrationResultData{
				GuildID: migrate.GuildID,
				Outcome: protocol.MigrationOutcomeNotFound,
				Reason:  reason,
			},
		})
		return
	}

	client.send(protocol.Message{
		Op:   protocol.OpMigrateReady,
		Data: snapshot,
	})

	s.logger.Info("player migration state sent",
		slog.String("guild_id", migrate.GuildID.String()),
		slog.String("migration_id", snapshot.MigrationID),
		slog.String("url", snapshot.URL),
		slog.Int64("position", snapshot.Position),
	)
}

func (s *Server) handleMigrateSettle(client *Client, data json.RawMessage) {
	var settle protocol.MigrateSettleData
	if err := json.Unmarshal(data, &settle); err != nil {
		s.logger.Error("failed to unmarshal migrate settle", slog.Any("error", err))
		return
	}

	if !settle.Success {
		snapshot, ok := client.failMigration(settle.GuildID, settle.MigrationID, settle.Reason)
		outcome := protocol.MigrationOutcomeMismatch
		if ok {
			outcome = protocol.MigrationOutcomeAborted
		}
		client.send(protocol.Message{
			Op: protocol.OpMigrationResult,
			Data: protocol.MigrationResultData{
				GuildID:     settle.GuildID,
				MigrationID: snapshot.MigrationID,
				Outcome:     outcome,
				Reason:      settle.Reason,
			},
		})
		return
	}

	outcome, snapshot := client.settleMigration(settle.GuildID, settle.MigrationID, settle.Reason)
	if outcome == protocol.MigrationOutcomeMismatch || outcome == protocol.MigrationOutcomeNotFound {
		s.logger.Warn("migration settle rejected",
			slog.String("guild_id", settle.GuildID.String()),
			slog.String("migration_id", settle.MigrationID),
			slog.String("outcome", outcome),
		)
	}

	client.send(protocol.Message{
		Op: protocol.OpMigrationResult,
		Data: protocol.MigrationResultData{
			GuildID:     settle.GuildID,
			MigrationID: snapshot.MigrationID,
			Outcome:     outcome,
		},
	})
}

func (s *Server) GetStats() protocol.StatsData {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	var totalPlayers, playingTracks int
	for _, client := range s.clients {
		client.playersMu.RLock()
		totalPlayers += len(client.players)
		for _, p := range client.players {
			if p.GetState() == protocol.PlayerStatePlaying {
				playingTracks++
			}
		}
		client.playersMu.RUnlock()
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return protocol.StatsData{
		Players:       totalPlayers,
		PlayingTracks: playingTracks,
		Uptime:        time.Since(s.startTime).Milliseconds(),
		Memory:        m.Alloc,
	}
}

func (s *Server) IsDraining() bool {
	s.drainMu.RLock()
	defer s.drainMu.RUnlock()
	return s.draining
}

func (s *Server) Drain(reason string, deadlineMs int64) {
	s.drainMu.Lock()
	alreadyDraining := s.draining
	s.draining = true
	s.drainMu.Unlock()

	if alreadyDraining {
		return
	}

	s.logger.Info("entering drain mode", slog.String("reason", reason), slog.Int64("deadline_ms", deadlineMs))

	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	for _, client := range s.clients {
		client.send(protocol.Message{
			Op: protocol.OpNodeDraining,
			Data: protocol.NodeDrainingData{
				Reason:     reason,
				DeadlineMs: deadlineMs,
			},
		})
	}
}

// PlayerCount reports players that a drain must still resolve. Players mid-way
// through a handshake are excluded here but included in PendingMigrationCount,
// so the drain waits for the whole picture without treating a handed-off player
// as a reason to block.
func (s *Server) PlayerCount() int {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	var total int
	for _, client := range s.clients {
		total += client.activePlayerCount()
	}
	return total
}

// PendingMigrationCount reports handshakes that have not been confirmed or
// rejected yet. A drain keeps waiting while either count is non-zero.
func (s *Server) PendingMigrationCount() int {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	var total int
	for _, client := range s.clients {
		total += client.pendingMigrationCount()
	}
	return total
}

// HasUnresolvedPlayers reports whether a drain still has work to do.
func (s *Server) HasUnresolvedPlayers() bool {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	for _, client := range s.clients {
		if client.activePlayerCount() > 0 || client.pendingMigrationCount() > 0 {
			return true
		}
	}
	return false
}

// ForceStopAllPlayers stops and cleans every remaining player in a stable order.
// It is the drain deadline path: no playback, source or connection may outlive
// the node.
func (s *Server) ForceStopAllPlayers() int {
	s.clientsMu.RLock()
	clients := make([]*Client, 0, len(s.clients))
	for _, client := range s.clients {
		clients = append(clients, client)
	}
	s.clientsMu.RUnlock()

	var stopped int
	for _, client := range clients {
		for _, guildID := range client.playerGuildIDs() {
			// Stop before removing so the audio source is closed by the same path
			// that owns it, then release the connection deterministically.
			if err := s.voiceManager.Stop(client.sessionID, guildID); err != nil {
				s.logger.Debug("no active playback to stop",
					slog.String("guild_id", guildID.String()),
					slog.Any("error", err),
				)
			}

			client.removePlayer(guildID)
			s.voiceManager.Release(client.sessionID, guildID)
			stopped++
		}
	}

	s.logger.Info("drain deadline cleanup complete", slog.Int("stopped_players", stopped))
	return stopped
}
