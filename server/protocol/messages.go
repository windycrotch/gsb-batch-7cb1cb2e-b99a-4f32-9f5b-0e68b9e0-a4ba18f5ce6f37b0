package protocol

import (
	"github.com/disgoorg/snowflake/v2"
	"github.com/shi-gg/linkdave/server/audio/filter"
)

type Message struct {
	Op   uint8 `json:"op"`
	Data any   `json:"d,omitempty"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type VoiceServerEvent struct {
	Token    string `json:"token"`
	GuildID  string `json:"guild_id"`
	Endpoint string `json:"endpoint"`
}

type VoiceUpdateData struct {
	ClientID  snowflake.ID     `json:"client_id"`
	GuildID   snowflake.ID     `json:"guild_id"`
	ChannelID snowflake.ID     `json:"channel_id"`
	SessionID string           `json:"session_id"`
	Event     VoiceServerEvent `json:"event"`
}

type PlayData struct {
	GuildID   snowflake.ID `json:"guild_id"`
	URL       string       `json:"url"`
	StartTime int64        `json:"start_time,omitempty"`
}

type GuildData struct {
	GuildID snowflake.ID `json:"guild_id"`
}

type SeekData struct {
	GuildID  snowflake.ID `json:"guild_id"`
	Position int64        `json:"position"`
}

type PlayerUpdateData struct {
	GuildID snowflake.ID `json:"guild_id"`
	State   string       `json:"state"`
}

type ReadyData struct {
	SessionID string `json:"session_id"`
	Resumed   bool   `json:"resumed"`
}

type TrackInfo struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Duration    int64  `json:"duration,omitempty"`
	RequesterID string `json:"requester_id,omitempty"`
}

type TrackStartData struct {
	GuildID snowflake.ID `json:"guild_id"`
	Track   TrackInfo    `json:"track"`
}

type TrackEndData struct {
	GuildID snowflake.ID `json:"guild_id"`
	Track   TrackInfo    `json:"track"`
	Reason  string       `json:"reason"`
}

type TrackErrorData struct {
	GuildID snowflake.ID `json:"guild_id"`
	Track   TrackInfo    `json:"track"`
	Error   string       `json:"error"`
}

type VoiceConnectData struct {
	GuildID   snowflake.ID `json:"guild_id"`
	ChannelID snowflake.ID `json:"channel_id"`
}

type VoiceDisconnectData struct {
	GuildID snowflake.ID `json:"guild_id"`
	Reason  string       `json:"reason,omitempty"`
}

type StatsData struct {
	Players       int    `json:"players"`
	PlayingTracks int    `json:"playing_tracks"`
	Uptime        int64  `json:"uptime"`
	Memory        uint64 `json:"memory"`
}

type NodeDrainingData struct {
	Reason     string `json:"reason"`
	DeadlineMs int64  `json:"deadline_ms"`
}

// PlayerMigrateData starts a handshake. Target and MigrationID are optional:
// a client may retry with the same MigrationID and will get the identical
// frozen snapshot back instead of a re-advanced position.
type PlayerMigrateData struct {
	GuildID     snowflake.ID `json:"guild_id"`
	MigrationID string       `json:"migration_id,omitempty"`
	Target      string       `json:"target,omitempty"`
}

type MigrateReadyData struct {
	GuildID     snowflake.ID    `json:"guild_id"`
	MigrationID string          `json:"migration_id"`
	Target      string          `json:"target,omitempty"`
	URL         string          `json:"url"`
	Position    int64           `json:"position"`
	State       string          `json:"state"`
	RequesterID string          `json:"requester_id,omitempty"`
	Filters     *filter.Filters `json:"filters,omitempty"`
}

// MigrateSettleData is sent by the client once the handshake has actually
// succeeded on the target node, or with Success=false when it could not reach
// the target. A failed settle restores the player on the old node so a drain
// keeps waiting instead of exiting early.
type MigrateSettleData struct {
	GuildID     snowflake.ID `json:"guild_id"`
	MigrationID string       `json:"migration_id,omitempty"`
	Success     bool         `json:"success"`
	Reason      string       `json:"reason,omitempty"`
}

// MigrationResultData reports the outcome of a settle attempt or a rejected
// handshake back to the client.
type MigrationResultData struct {
	GuildID     snowflake.ID `json:"guild_id"`
	MigrationID string       `json:"migration_id,omitempty"`
	Outcome     string       `json:"outcome"`
	Reason      string       `json:"reason,omitempty"`
}

type StatsResponse struct {
	Version      string `json:"version"`
	Runtime      string `json:"runtime"`
	Uptime       int64  `json:"uptime_ms"`
	NumGoroutine int    `json:"num_goroutines"`
	Memory       uint64 `json:"memory"`
}

type RequestPlay struct {
	URL         string          `json:"url"`
	StartTime   int64           `json:"start_time,omitempty"`
	RequesterID string          `json:"requester_id,omitempty"`
	Filters     *filter.Filters `json:"filters,omitempty"`
}

type RequestSeek struct {
	Position int64 `json:"position"`
}
