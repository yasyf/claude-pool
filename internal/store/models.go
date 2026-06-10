package store

import "time"

// Account is one pool account. ID is the account index (>= 1).
type Account struct {
	ID              int
	ConfigDir       string // exact string exported as CLAUDE_CONFIG_DIR
	KeychainService string
	KeychainAccount string
	Label           string // human note, e.g. an email or alias
	OverlayKind     string // "symlink" | "fuse"
	CreatedAt       time.Time
}

// UsageSample is one poll of an account's quota windows. Utilization fields are
// stored as 0..100 "percent used" to feed scoring directly. The Extra* fields
// mirror the API's extra_usage (pay-as-you-go overage) block, for status
// display only — scoring ignores them.
type UsageSample struct {
	AccountID    int
	TS           time.Time
	Util5h       float64
	Util7d       float64
	Resets5h     time.Time
	Resets7d     time.Time
	RateLimited  bool
	ExtraEnabled bool
	ExtraUsed    float64 // overage credits consumed this month (currency cents)
	ExtraLimit   float64 // overage credit cap (currency cents)
}

// Session is a checkout of an account to a live claude process.
type Session struct {
	ID        int64
	AccountID int
	PID       int
	ConfigDir string
	StartedAt time.Time
	EndedAt   *time.Time
}

// Sticky is the last account selected for a working directory, used to keep
// resumed sessions on the same account for prompt-cache continuity.
type Sticky struct {
	Cwd        string
	AccountID  int
	SelectedAt time.Time
}

// RefreshEntry is one credential-refresh attempt.
type RefreshEntry struct {
	AccountID int
	TS        time.Time
	OK        bool
	Err       string
}
