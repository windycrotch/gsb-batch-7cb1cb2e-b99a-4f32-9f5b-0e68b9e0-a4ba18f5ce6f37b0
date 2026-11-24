package api

import (
	"errors"
	"fmt"
	"strings"
)

// User flags bitmask constants (Discord API)
const (
	FlagDiscordEmployee      int64 = 1 << 0  // 1: Discord Staff
	FlagPartneredServerOwner int64 = 1 << 1  // 2: Partnered Server Owner
	FlagHypeSquadEvents      int64 = 1 << 2  // 4: HypeSquad Events
	FlagBugHunterLevel1      int64 = 1 << 3  // 8: Bug Hunter Level 1
	FlagHouseBravery         int64 = 1 << 6  // 64: HypeSquad Bravery
	FlagHouseBrilliance      int64 = 1 << 7  // 128: HypeSquad Brilliance
	FlagHouseBalance         int64 = 1 << 8  // 256: HypeSquad Balance
	FlagEarlySupporter       int64 = 1 << 9  // 512: Early Supporter
	FlagTeamPseudoUser       int64 = 1 << 10 // 1024: Team Pseudo User
	FlagBugHunterLevel2      int64 = 1 << 14 // 16384: Bug Hunter Level 2
	FlagVerifiedBot          int64 = 1 << 16 // 65536: Verified Bot
	FlagEarlyVerifiedBotDev  int64 = 1 << 17 // 131072: Early Verified Bot Developer
	FlagCertifiedModerator   int64 = 1 << 18 // 262144: Discord Certified Moderator
	FlagBotHTTPInteractions  int64 = 1 << 19 // 524288: Bot HTTP Interactions
	FlagActiveDeveloper      int64 = 1 << 22 // 4194304: Active Developer
)

// HypeSquad House IDs
const (
	HypeSquadHouseBravery    = 1
	HypeSquadHouseBrilliance = 2
	HypeSquadHouseBalance    = 3
)

// UserMe represents the current authenticated Discord user (/users/@me).
type UserMe struct {
	ID          string  `json:"id"`
	Username    string  `json:"username"`
	GlobalName  *string `json:"global_name"`
	MFAEnabled  bool    `json:"mfa_enabled"`
	Email       *string `json:"email"`
	Locale      string  `json:"locale"` // account UI locale, authoritative for X-Discord-Locale
	Flags       int64   `json:"flags"`
	PublicFlags int64   `json:"public_flags"`
	PremiumType int     `json:"premium_type"` // 0: None, 1: Nitro Classic, 2: Nitro, 3: Nitro Basic
}

// EffectiveFlags returns the combined bitmask of Flags and PublicFlags.
func (u *UserMe) EffectiveFlags() int64 {
	if u == nil {
		return 0
	}
	return u.Flags | u.PublicFlags
}

// HasHypeSquad checks whether the user belongs to any HypeSquad House.
func (u *UserMe) HasHypeSquad() bool {
	flags := u.EffectiveFlags()
	return (flags & (FlagHouseBravery | FlagHouseBrilliance | FlagHouseBalance)) != 0
}

// GetHypeSquadHouse returns the house ID (1, 2, or 3) and descriptive name, or (0, "None").
func (u *UserMe) GetHypeSquadHouse() (int, string) {
	flags := u.EffectiveFlags()
	if flags&FlagHouseBravery != 0 {
		return HypeSquadHouseBravery, "House of Bravery"
	}
	if flags&FlagHouseBrilliance != 0 {
		return HypeSquadHouseBrilliance, "House of Brilliance"
	}
	if flags&FlagHouseBalance != 0 {
		return HypeSquadHouseBalance, "House of Balance"
	}
	return 0, "None"
}

// GetBadges returns a slice of formatted badges representing the account's decorations.
func (u *UserMe) GetBadges() []string {
	if u == nil {
		return nil
	}
	var badges []string
	flags := u.EffectiveFlags()

	if flags&FlagDiscordEmployee != 0 {
		badges = append(badges, "👨‍💼 Staff")
	}
	if flags&FlagPartneredServerOwner != 0 {
		badges = append(badges, "🤝 Partner")
	}
	if flags&FlagHypeSquadEvents != 0 {
		badges = append(badges, "🎉 HypeSquad Events")
	}
	if flags&FlagHouseBravery != 0 {
		badges = append(badges, "🛡️ HypeSquad Bravery")
	}
	if flags&FlagHouseBrilliance != 0 {
		badges = append(badges, "💎 HypeSquad Brilliance")
	}
	if flags&FlagHouseBalance != 0 {
		badges = append(badges, "⚖️ HypeSquad Balance")
	}
	if flags&FlagBugHunterLevel1 != 0 {
		badges = append(badges, "🐛 Bug Hunter L1")
	}
	if flags&FlagBugHunterLevel2 != 0 {
		badges = append(badges, "🐛 Bug Hunter L2")
	}
	if flags&FlagEarlySupporter != 0 {
		badges = append(badges, "⭐ Early Supporter")
	}
	if flags&FlagEarlyVerifiedBotDev != 0 {
		badges = append(badges, "🤖 Early Bot Dev")
	}
	if flags&FlagCertifiedModerator != 0 {
		badges = append(badges, "🛡️ Certified Mod")
	}
	if flags&FlagActiveDeveloper != 0 {
		badges = append(badges, "🛠️ Active Developer")
	}

	switch u.PremiumType {
	case 1:
		badges = append(badges, "🚀 Nitro Classic")
	case 2:
		badges = append(badges, "🚀 Nitro")
	case 3:
		badges = append(badges, "🚀 Nitro Basic")
	}

	return badges
}

// FormatBadges returns bracketed string tags of all user badges, or "None".
func (u *UserMe) FormatBadges() string {
	badges := u.GetBadges()
	if len(badges) == 0 {
		return "None"
	}
	var formatted []string
	for _, b := range badges {
		formatted = append(formatted, "["+b+"]")
	}
	var res string
	for i, f := range formatted {
		if i > 0 {
			res += " "
		}
		res += f
	}
	return res
}

// QuestResponse represents the top-level payload from GET /api/v9/quests/@me.
type QuestResponse struct {
	Quests                      []Quest         `json:"quests"`
	ExcludedQuests              []ExcludedQuest `json:"excluded_quests"`
	QuestEnrollmentBlockedUntil *string         `json:"quest_enrollment_blocked_until"`
	QuestAccessSuspendedUntil   *string         `json:"quest_access_suspended_until"`
}

// ExcludedQuest is one quest Discord withheld from the account. It carries the
// id only (plus an optional replacement campaign) because the full config is
// never sent for ineligible quests.
type ExcludedQuest struct {
	ID               string  `json:"id"`
	ReplacementID    *string `json:"replacement_id,omitempty"`
	DiscoveredRegion string  `json:"-"`
}

// Quest represents a single Discord quest object.
type Quest struct {
	ID                    string      `json:"id"`
	Config                QuestConfig `json:"config"`
	UserStatus            *UserStatus `json:"user_status"`
	TrafficMetadataRaw    *string     `json:"traffic_metadata_raw"`
	TrafficMetadataSealed *string     `json:"traffic_metadata_sealed"`
	TargetedContent       []int       `json:"targeted_content,omitempty"`
	RegionProxied         bool        `json:"region_proxied,omitempty"`
	FromCache             bool        `json:"from_cache,omitempty"`
	FromSnapshot          bool        `json:"from_snapshot,omitempty"`
	SnapshotFiller        bool        `json:"snapshot_filler,omitempty"`
	Provenance            string      `json:"provenance,omitempty"`
	Source                string      `json:"source,omitempty"`
	DiscoveredRegion      string      `json:"discovered_region,omitempty"`
	DiscoveredLocale      string      `json:"discovered_locale,omitempty"`
	DiscoveredTimezone    string      `json:"discovered_timezone,omitempty"`

	// EgressBinding is a non-secret fingerprint persisted with the quest. The
	// raw proxy URL is kept only in memory so proxy credentials are not written
	// to the snapshot.
	EgressBinding string `json:"egress_binding,omitempty"`
	EgressID      string `json:"egress_id,omitempty"`
	EgressKey     string `json:"egress_key,omitempty"`
	RouteKey      string `json:"route_key,omitempty"`
	Egress        string `json:"egress,omitempty"`
	EgressURL     string `json:"-"`
	ProxyBinding  string `json:"proxy_binding,omitempty"`
	Binding       string `json:"binding,omitempty"`
	BoundProxy    string `json:"-"`
	BoundProxyURL string `json:"-"`
	ProxyURL      string `json:"-"`
	Proxy         string `json:"-"`
	Endpoint      string `json:"-"`
	EgressBound   bool   `json:"egress_bound,omitempty"`
	EgressPinned  bool   `json:"egress_pinned,omitempty"`
	RequireEgress bool   `json:"require_egress,omitempty"`

	SeenIn []string `json:"-"`
}

// IsSnapshotFiller reports whether a quest is display-only data restored from
// a previous complete snapshot. Several field names are accepted because the
// fallback marker has historically been represented in more than one form.
func (q Quest) IsSnapshotFiller() bool {
	return IsSnapshotFiller(q)
}

// IsSnapshotFiller reports whether q was restored from a local snapshot.
func IsSnapshotFiller(q Quest) bool {
	return q.FromCache || q.FromSnapshot || q.SnapshotFiller ||
		strings.EqualFold(q.Provenance, "snapshot") ||
		strings.EqualFold(q.Source, "snapshot") ||
		strings.EqualFold(q.DiscoveredRegion, "CACHE")
}

// EgressInfo returns the strict routing pin remembered by a quest.
func (q Quest) EgressInfo() RegionInfo {
	code := strings.TrimSpace(q.DiscoveredRegion)
	binding := q.EgressBinding
	if binding == "" {
		binding = q.Binding
	}
	if binding == "" {
		binding = q.ProxyBinding
	}
	if binding == "" {
		binding = q.EgressKey
	}
	if binding == "" {
		binding = q.RouteKey
	}
	if binding == "" {
		binding = q.EgressID
	}
	if binding == "" && q.Egress != "" && !strings.Contains(q.Egress, "://") {
		binding = q.Egress
	}
	proxyURL := firstNonEmptyString(q.ProxyURL, q.EgressURL, q.Proxy, q.BoundProxyURL, q.BoundProxy, q.Endpoint)
	if proxyURL == "" && strings.Contains(q.Egress, "://") {
		proxyURL = q.Egress
	}
	info := RegionInfo{
		Code:          code,
		Locale:        q.DiscoveredLocale,
		Timezone:      q.DiscoveredTimezone,
		ProxyURL:      proxyURL,
		BoundProxy:    proxyURL,
		BoundProxyURL: proxyURL,
		Binding:       binding,
		EgressBinding: binding,
		ProxyBinding:  binding,
		EgressID:      q.EgressID,
	}
	if q.RegionProxied || proxyURL != "" || binding != "" || q.EgressBound || q.EgressPinned || q.RequireEgress {
		info.RequireProxy = true
		info.ProxyRequired = true
		info.EgressRequired = true
		info.BoundEgress = q.EgressBound
		info.PinnedEgress = q.EgressPinned
		info.Strict = true
		info.Bound = true
		info.Pinned = true
	}
	return info
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// RegionInfo returns the quest's remembered region and egress pin. It is a
// convenience alias for callers that expect the method to match the type name.
func (q Quest) RegionInfo() RegionInfo {
	return q.EgressInfo()
}

// QuestConfig contains campaign metadata, timeframes, and task specifications.
type QuestConfig struct {
	ID            string           `json:"id"`
	ConfigVersion int              `json:"config_version"`
	StartsAt      string           `json:"starts_at"`
	ExpiresAt     string           `json:"expires_at"`
	Features      []int            `json:"features"`
	Application   *ApplicationMeta `json:"application"`
	Messages      QuestMessages    `json:"messages"`
	TaskConfig    *TaskConfig      `json:"task_config"`
	TaskConfigV2  *TaskConfig      `json:"task_config_v2"`
}

// ApplicationMeta describes the primary game or Discord application associated with the quest.
type ApplicationMeta struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Link string `json:"link"`
}

// QuestMessages contains user-visible campaign strings.
type QuestMessages struct {
	QuestName     *string `json:"quest_name"`
	GameTitle     *string `json:"game_title"`
	GamePublisher *string `json:"game_publisher"`
}

// TaskConfig maps task types to requirement specifications.
type TaskConfig struct {
	Tasks map[string]TaskRequirement `json:"tasks"`
}

// TaskRequirement defines criteria (target seconds, target apps) for a task type.
type TaskRequirement struct {
	Type         string           `json:"type"`
	Target       int              `json:"target"` // Total target seconds (e.g. 900)
	Applications []ApplicationRef `json:"applications"`
}

// ApplicationRef references a detectable application ID.
type ApplicationRef struct {
	ID string `json:"id"`
}

// UserStatus stores the authenticated user's progress and enrollment state.
type UserStatus struct {
	UserID             string                  `json:"user_id"`
	QuestID            string                  `json:"quest_id"`
	EnrolledAt         *string                 `json:"enrolled_at"`
	CompletedAt        *string                 `json:"completed_at"`
	ClaimedAt          *string                 `json:"claimed_at"`
	Progress           map[string]TaskProgress `json:"progress"`
	OrbQuantityClaimed int                     `json:"orb_quantity_claimed"`
}

// TaskProgress records elapsed time or progress units for a specific task type.
type TaskProgress struct {
	Value       float64 `json:"value"` // Elapsed seconds
	EventName   string  `json:"event_name"`
	UpdatedAt   *string `json:"updated_at"`
	CompletedAt *string `json:"completed_at"`
}

// EnrollPayload is sent to POST /api/v9/quests/{qid}/enroll.
type EnrollPayload struct {
	Location              int     `json:"location"`
	IsTargeted            bool    `json:"is_targeted"`
	MetadataRaw           *string `json:"metadata_raw"`
	MetadataSealed        *string `json:"metadata_sealed"`
	TrafficMetadataRaw    *string `json:"traffic_metadata_raw"`
	TrafficMetadataSealed *string `json:"traffic_metadata_sealed"`
	CaptchaKey            *string `json:"captcha_key,omitempty"`
	CaptchaRqtoken        *string `json:"captcha_rqtoken,omitempty"`
}

// CaptchaRequiredError represents an HTTP 400 with Discord captcha challenge.
type CaptchaRequiredError struct {
	QuestID        string `json:"quest_id"`
	CaptchaKey     string `json:"captcha_key"`
	CaptchaService string `json:"captcha_service"`
	CaptchaSitekey string `json:"captcha_sitekey"`
	CaptchaRqdata  string `json:"captcha_rqdata,omitempty"`
	CaptchaRqtoken string `json:"captcha_rqtoken,omitempty"`
}

func (e *CaptchaRequiredError) Error() string {
	return "captcha verification required by Discord for quest " + e.QuestID
}

// DiscordCaptchaResponse parses error responses that include captcha requirements.
type DiscordCaptchaResponse struct {
	Message        string   `json:"message"`
	Code           int      `json:"code"`
	CaptchaKey     []string `json:"captcha_key"`
	CaptchaSitekey string   `json:"captcha_sitekey"`
	CaptchaService string   `json:"captcha_service"`
	CaptchaRqdata  string   `json:"captcha_rqdata"`
	CaptchaRqtoken string   `json:"captcha_rqtoken"`
}

// ErrQuestExpired marks a quest that Discord no longer accepts progress for
// (campaign ended while the tool was idle, already claimed, or removed).
var ErrQuestExpired = errors.New("quest has expired or ended")

// QuestExpiredError is returned when Discord answers a quest action with a
// terminal "this quest is gone" status instead of a transient failure.
type QuestExpiredError struct {
	QuestID string
	Status  int
	Detail  string
}

func (e *QuestExpiredError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("quest %s expired or ended (HTTP %d): %s", e.QuestID, e.Status, e.Detail)
	}
	return fmt.Sprintf("quest %s expired or ended (HTTP %d)", e.QuestID, e.Status)
}

// Is lets callers test with errors.Is(err, ErrQuestExpired).
func (e *QuestExpiredError) Is(target error) bool { return target == ErrQuestExpired }

// RateLimitResponse parses HTTP 429 JSON payload.
type RateLimitResponse struct {
	Message    string  `json:"message"`
	RetryAfter float64 `json:"retry_after"`
	Global     bool    `json:"global"`
}

// VideoProgressPayload is sent to POST /api/v9/quests/{qid}/video-progress.
type VideoProgressPayload struct {
	Timestamp float64 `json:"timestamp"`
}

// HeartbeatPayload is sent to POST /api/v9/quests/{qid}/heartbeat.
type HeartbeatPayload struct {
	StreamKey string `json:"stream_key"`
	Terminal  bool   `json:"terminal"`
}

// ProgressUpdateResponse is returned by heartbeat and video-progress endpoints.
type ProgressUpdateResponse struct {
	UserID      string                  `json:"user_id"`
	QuestID     string                  `json:"quest_id"`
	CompletedAt *string                 `json:"completed_at"`
	Progress    map[string]TaskProgress `json:"progress"`
}
