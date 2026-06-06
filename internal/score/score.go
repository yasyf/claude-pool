// Package score implements claude-pool's account-selection scoring. Higher is
// better; select picks argmax. There are no roles — the best account across the
// whole pool wins.
package score

import (
	"sort"
	"time"
)

// Weights are the scoring coefficients. Exposed for testing and tuning.
var (
	W5h          = 0.70
	W7d          = 0.25
	WSession     = 2.00
	PenRateLimit = 100.0
	PenStale     = 20.0
	StaleAfter   = 90 * time.Second
)

// Input is everything the scorer needs about one account.
type Input struct {
	AccountID int

	HasUsage bool      // false if we have never sampled this account
	SampleTS time.Time // when the latest usage sample was taken
	Util5h   float64   // percent used 0..100 of the 5-hour window
	Util7d   float64   // percent used 0..100 of the 7-day window
	Resets5h time.Time // when the 5-hour window resets (tie-break)

	ActiveSessions int
	RateLimited    bool // a live 429 / rate-limit observed
	RefreshFailed  bool // the most recent refresh attempt failed
}

// stale reports whether this account's data is stale or its refresh failed.
func (in Input) stale(now time.Time) bool {
	if in.RefreshFailed {
		return true
	}
	if !in.HasUsage {
		return true
	}
	return now.Sub(in.SampleTS) > StaleAfter
}

// Components is the per-term breakdown, for `status` and debugging.
type Components struct {
	Remaining5h      float64
	Remaining7d      float64
	SessionPenalty   float64
	RateLimitPenalty float64
	StalePenalty     float64
}

// Result is a scored account.
type Result struct {
	AccountID   int
	Score       float64
	Components  Components
	Resets5h    time.Time
	Stale       bool
	RateLimited bool
	Available   bool // false only if rate-limited (cannot serve right now)
}

// Score computes the score for one account.
func Score(in Input, now time.Time) Result {
	rem5 := 100 - in.Util5h
	rem7 := 100 - in.Util7d
	if !in.HasUsage {
		// Unknown usage: assume empty so a never-sampled account is still
		// selectable, but the stale penalty keeps it behind known-good ones.
		rem5, rem7 = 100, 100
	}
	c := Components{
		Remaining5h:      W5h * rem5,
		Remaining7d:      W7d * rem7,
		SessionPenalty:   WSession * float64(in.ActiveSessions),
		RateLimitPenalty: 0,
		StalePenalty:     0,
	}
	if in.RateLimited {
		c.RateLimitPenalty = PenRateLimit
	}
	stale := in.stale(now)
	if stale {
		c.StalePenalty = PenStale
	}
	score := c.Remaining5h + c.Remaining7d - c.SessionPenalty - c.RateLimitPenalty - c.StalePenalty
	return Result{
		AccountID:   in.AccountID,
		Score:       score,
		Components:  c,
		Resets5h:    in.Resets5h,
		Stale:       stale,
		RateLimited: in.RateLimited,
		Available:   !in.RateLimited,
	}
}

// Rank scores all inputs and returns results sorted best-first. Ties on score
// break toward the soonest 5-hour reset (an account about to reset is freshest
// to drain first); a zero Resets5h sorts last among ties.
func Rank(inputs []Input, now time.Time) []Result {
	results := make([]Result, len(inputs))
	for i, in := range inputs {
		results[i] = Score(in, now)
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return resetsBefore(results[i].Resets5h, results[j].Resets5h)
	})
	return results
}

// resetsBefore orders earlier non-zero reset times first; zero (unknown) last.
func resetsBefore(a, b time.Time) bool {
	switch {
	case a.IsZero() && b.IsZero():
		return false
	case a.IsZero():
		return false
	case b.IsZero():
		return true
	default:
		return a.Before(b)
	}
}

// Pick returns the best AVAILABLE account from ranked results. ok=false if no
// account is currently available (all rate-limited or the pool is empty).
func Pick(ranked []Result) (Result, bool) {
	for _, r := range ranked {
		if r.Available {
			return r, true
		}
	}
	return Result{}, false
}

// SoonestReset returns the earliest non-zero 5h reset across results, used by
// `select --wait`. ok=false if no reset time is known.
func SoonestReset(results []Result) (time.Time, bool) {
	var best time.Time
	for _, r := range results {
		if r.Resets5h.IsZero() {
			continue
		}
		if best.IsZero() || r.Resets5h.Before(best) {
			best = r.Resets5h
		}
	}
	return best, !best.IsZero()
}
