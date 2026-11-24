package protocol

const (
	OpVoiceUpdate   uint8 = 0
	OpPlayerMigrate uint8 = 1
	OpMigrateSettle uint8 = 2
)

const (
	OpReady           uint8 = 0
	OpVoiceConnect    uint8 = 1
	OpVoiceDisconnect uint8 = 2
	OpPlayerUpdate    uint8 = 3
	OpTrackStart      uint8 = 4
	OpTrackEnd        uint8 = 5
	OpTrackError      uint8 = 6
	OpStats           uint8 = 7
	OpNodeDraining    uint8 = 8
	OpMigrateReady    uint8 = 9
	OpMigrationResult uint8 = 10
)

const (
	TrackEndReasonFinished = "finished"
	TrackEndReasonStopped  = "stopped"
	TrackEndReasonReplaced = "replaced"
	TrackEndReasonError    = "error"
)

const (
	PlayerStateIdle    = "idle"
	PlayerStatePlaying = "playing"
	PlayerStatePaused  = "paused"
)

const (
	DisconnectReasonConnectionLost   = "connection_lost"
	DisconnectReasonConnectionFailed = "connection_failed"
	DisconnectReasonRequested        = "requested"
)

// Migration failure reasons surfaced to clients that cannot obtain a snapshot.
const (
	MigrateReasonPlayerNotFound = "player_not_found"
	MigrateReasonNoTrack        = "no_track"
)

// Reasons attached to migration results.
const (
	MigrationReasonConfirmTimeout = "confirm_timeout"
)

// Outcomes reported by OpMigrationResult.
const (
	MigrationOutcomeSettled        = "settled"
	MigrationOutcomeAlreadySettled = "already_settled"
	MigrationOutcomeAborted        = "aborted"
	MigrationOutcomeNotFound       = "not_found"
	MigrationOutcomeMismatch       = "migration_mismatch"
)
