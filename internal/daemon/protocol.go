// Package daemon implements the background user-LaunchAgent: a usage poller,
// idle-only credential refresher, score cache, and unix-socket server. The CLI
// hot paths (select/status) talk to it over a 0600 unix socket using the
// newline-delimited JSON protocol defined here.
package daemon

import (
	"time"

	"github.com/yasyf/cc-pool/internal/score"
)

// ProtocolVersion is bumped on incompatible wire changes.
const ProtocolVersion = 1

// Op is a request operation.
type Op string

const (
	OpSelect   Op = "select"   // pick the best account; optionally mark a checkout
	OpStatus   Op = "status"   // return scored status for all accounts
	OpCheckin  Op = "checkin"  // release a checkout and adopt a rotated token
	OpHealth   Op = "health"   // liveness + version probe
	OpShutdown Op = "shutdown" // step down gracefully and release the socket
)

// Request is one client request (one JSON object per line).
type Request struct {
	Proto   int    `json:"proto"`
	Op      Op     `json:"op"`
	Account *int   `json:"account,omitempty"` // force a specific account (select)
	PID     int    `json:"pid,omitempty"`     // launching pid (select checkout / checkin)
	NoMark  bool   `json:"no_mark,omitempty"` // select without recording a checkout
	Cwd     string `json:"cwd,omitempty"`     // caller's working directory, keys select stickiness
	// NoFallback: report none-available instead of a least-bad exhausted pick.
	// Set by --wait clients, which would discard the pick (and its sticky
	// rewrite, reservation, and preflight side effects) to keep waiting.
	NoFallback bool `json:"no_fallback,omitempty"`
}

// AccountStatus is the per-account view returned by status/select.
type AccountStatus struct {
	ID             int       `json:"id"`
	ConfigDir      string    `json:"config_dir"`
	Label          string    `json:"label"`
	OverlayKind    string    `json:"overlay_kind"`
	Score          float64   `json:"score"`
	Remaining5h    float64   `json:"remaining_5h"`
	Remaining7d    float64   `json:"remaining_7d"`
	ActiveSessions int       `json:"active_sessions"`
	RateLimited    bool      `json:"rate_limited"`
	Exhausted      bool      `json:"exhausted,omitempty"` // a window is pegged with its reset pending
	HasUsage       bool      `json:"has_usage"`           // false only if the account was never sampled
	Stale          bool      `json:"stale"`
	Resets5h       time.Time `json:"resets_5h"`
	Resets7d       time.Time `json:"resets_7d"`
	SampleAge      string    `json:"sample_age"`
	// Extra-usage (pay-as-you-go overage) state, for status display.
	ExtraEnabled bool    `json:"extra_enabled,omitempty"`
	ExtraUsed    float64 `json:"extra_used,omitempty"`  // currency cents
	ExtraLimit   float64 `json:"extra_limit,omitempty"` // currency cents
	// Components is the per-term score breakdown, so status can explain the score.
	Components score.Components `json:"components"`
}

// Response is one server reply (one JSON object per line).
type Response struct {
	Proto       int     `json:"proto"`
	OK          bool    `json:"ok"`
	Error       string  `json:"error,omitempty"`
	Dir         string  `json:"dir,omitempty"` // select: chosen config dir
	SelectedID  *int    `json:"selected_id,omitempty"`
	Sticky      bool    `json:"sticky,omitempty"`       // select honored a sticky record
	Remaining5h float64 `json:"remaining_5h,omitempty"` // select: raw 5h remaining (100−used) of the pick
	Remaining7d float64 `json:"remaining_7d,omitempty"` // select: raw 7d remaining (100−used) of the pick
	HasUsage    bool    `json:"has_usage,omitempty"`    // select: false if the pick was never sampled
	// ExhaustedFallback: every account was exhausted and the pick is the
	// least-bad one — the client must warn that it bills credits or rate-limits.
	ExhaustedFallback bool `json:"exhausted_fallback,omitempty"`
	// ExtraEnabled: the pick has overage billing enabled (fallback warning).
	ExtraEnabled bool `json:"extra_enabled,omitempty"`
	// NoneAvailable: select found no servable account (all rate-limited or the
	// pool is empty) — a structured signal so clients don't match error strings.
	NoneAvailable bool            `json:"none_available,omitempty"`
	Accounts      []AccountStatus `json:"accounts,omitempty"` // status
	Version       string          `json:"version,omitempty"`  // health
	SoonestReset  *time.Time      `json:"soonest_reset,omitempty"`
}
