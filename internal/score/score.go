// Package score implements cc-pool's account-selection scoring. Higher is
// better; select picks argmax. There are no roles — the best account across the
// whole pool wins.
package score

import (
	"sort"
	"time"
)

// Scoring coefficients and knobs. All are package vars so they can be tuned or
// disabled in tests. Setting BarrierKnee=0 and RunwayWeight=0 reduces the score
// to the exact baseline 0.70·rem5 + 0.25·rem7 − 2·sessions − 100·rl − 20·stale.
var (
	W5h          = 0.70
	W7d          = 0.25
	WSession     = 2.00
	PenRateLimit = 100.0
	PenStale     = 20.0
	StaleAfter   = 90 * time.Second

	// FiveHourWindow / SevenDayWindow are the window lengths used to credit an
	// imminent reset: depletion is discounted by how much of the window is still
	// ahead before it refills.
	FiveHourWindow = 5 * time.Hour
	SevenDayWindow = 7 * 24 * time.Hour

	// BarrierKnee is the remaining-% below which a convex low-headroom penalty
	// kicks in (so a nearly-exhausted window can't be masked by the other).
	BarrierKnee = 20.0

	// RunwayWeight / RunwayHorizon shape the burn-rate term: an account whose
	// effective 5h headroom would be drained within RunwayHorizon is downranked,
	// up to RunwayWeight points.
	RunwayWeight  = 15.0
	RunwayHorizon = 5 * time.Hour

	// StickyMinEff5 is the effective-5h-remaining floor (percent) below which a
	// sticky selection is abandoned: with this little headroom the resumed
	// session would hit the limit anyway, so cache continuity is worthless.
	StickyMinEff5 = 10.0
)

// Input is everything the scorer needs about one account.
type Input struct {
	AccountID int

	HasUsage bool      // false if we have never sampled this account
	SampleTS time.Time // when the latest usage sample was taken
	Util5h   float64   // percent used 0..100 of the 5-hour window
	Util7d   float64   // percent used 0..100 of the 7-day window
	Resets5h time.Time // when the 5-hour window resets (also the tie-break)
	Resets7d time.Time // when the 7-day window resets

	// Burn5hPerHour is the recent rate of change of util_5h in percent/hour,
	// from usage history. Zero means unknown or idle (no runway penalty).
	Burn5hPerHour float64

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
	Eff5             float64 // reset-aware effective 5h remaining
	Eff7             float64 // reset-aware effective 7d remaining
	Remaining5h      float64 // W5h · Eff5 (weighted contribution)
	Remaining7d      float64 // W7d · Eff7
	SessionPenalty   float64
	RateLimitPenalty float64
	StalePenalty     float64
	Barrier5h        float64
	Barrier7d        float64
	RunwayPenalty    float64
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

// Score computes the score for one account. For a healthy account (windows far
// from a reset and well above the barrier knee, no measured burn) it equals the
// baseline 0.70·rem5 + 0.25·rem7 − penalties; the reset-aware, barrier, and
// runway terms only engage near limits.
func Score(in Input, now time.Time) Result {
	util5, util7 := in.Util5h, in.Util7d
	if !in.HasUsage {
		// Unknown usage: assume empty so a never-sampled account is still
		// selectable, but the stale penalty keeps it behind known-good ones.
		util5, util7 = 0, 0
	}

	// Reset-aware effective remaining: discount depletion by how much of the
	// window is still ahead before it refills. frac=1 (reset far/unknown) gives
	// the plain remaining; frac→0 (imminent reset) gives ~100.
	eff5 := 100 - windowFrac(in.Resets5h, now, FiveHourWindow)*util5
	eff7 := 100 - windowFrac(in.Resets7d, now, SevenDayWindow)*util7

	c := Components{
		Eff5:           eff5,
		Eff7:           eff7,
		Remaining5h:    W5h * eff5,
		Remaining7d:    W7d * eff7,
		SessionPenalty: WSession * float64(in.ActiveSessions),
		Barrier5h:      barrier(eff5),
		Barrier7d:      barrier(eff7),
		RunwayPenalty:  runwayPenalty(eff5, in.Burn5hPerHour),
	}
	if in.RateLimited {
		c.RateLimitPenalty = PenRateLimit
	}
	stale := in.stale(now)
	if stale {
		c.StalePenalty = PenStale
	}

	score := c.Remaining5h + c.Remaining7d -
		c.SessionPenalty - c.RateLimitPenalty - c.StalePenalty -
		c.Barrier5h - c.Barrier7d - c.RunwayPenalty
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

// windowFrac is the fraction of a usage window still ahead before it resets, in
// [0,1]. A zero/far reset returns 1 (the depletion counts fully); an imminent
// reset returns ~0 (the depletion is about to be refilled).
func windowFrac(resetsAt, now time.Time, window time.Duration) float64 {
	if resetsAt.IsZero() || window <= 0 {
		return 1
	}
	frac := resetsAt.Sub(now).Seconds() / window.Seconds()
	switch {
	case frac < 0:
		return 0
	case frac > 1:
		return 1
	default:
		return frac
	}
}

// barrier is a convex low-headroom penalty: zero above BarrierKnee, rising
// linearly to BarrierKnee as effective remaining approaches zero.
func barrier(remaining float64) float64 {
	if remaining >= BarrierKnee {
		return 0
	}
	if remaining < 0 {
		remaining = 0
	}
	return BarrierKnee - remaining
}

// runwayPenalty downranks an account being actively drained: if its effective
// 5h headroom would be exhausted within RunwayHorizon at the current burn rate,
// penalize up to RunwayWeight points. Zero/negative burn (idle or unknown) → 0.
func runwayPenalty(eff5, burnPerHour float64) float64 {
	if burnPerHour <= 0 || RunwayWeight <= 0 || RunwayHorizon <= 0 {
		return 0
	}
	runwayHours := eff5 / burnPerHour
	horizon := RunwayHorizon.Hours()
	frac := 1 - runwayHours/horizon
	if frac <= 0 {
		return 0
	}
	if frac > 1 {
		frac = 1
	}
	return RunwayWeight * frac
}

// UsableForSticky reports whether a previously-selected account can keep
// serving a sticky session: it must be available (not rate-limited) and have
// at least StickyMinEff5 effective 5h headroom.
func UsableForSticky(r Result) bool {
	return r.Available && r.Components.Eff5 >= StickyMinEff5
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
