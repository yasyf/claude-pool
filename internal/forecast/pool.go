package forecast

import "time"

// Mood is the pool-health alarm level, computed daemon-side so every consumer
// (widget mascot, CLI) agrees on one source of truth. Levels order
// chill < easy < uneasy < worried < alarmed < panic.
type Mood string

// Mood levels, calmest first.
const (
	MoodChill   Mood = "chill"
	MoodEasy    Mood = "easy"
	MoodUneasy  Mood = "uneasy"
	MoodWorried Mood = "worried"
	MoodAlarmed Mood = "alarmed"
	MoodPanic   Mood = "panic"
)

// worse returns the next more-alarmed level; panic is terminal.
func (m Mood) worse() Mood {
	switch m {
	case MoodChill:
		return MoodEasy
	case MoodEasy:
		return MoodUneasy
	case MoodUneasy:
		return MoodWorried
	case MoodWorried:
		return MoodAlarmed
	default:
		return MoodPanic
	}
}

// PoolAccount is one account's contribution to the pool rollup.
type PoolAccount struct {
	HasUsage    bool
	RateLimited bool
	Remaining5h float64 // percent 0..100
	Remaining7d float64 // percent 0..100
	BurnPerHour float64 // gated display burn (Estimate.BurnPerHour)
	Resets5h    time.Time
}

// Pool is the pool-wide rollup behind the widget's headline and mascot.
type Pool struct {
	// Remaining5h and Remaining7d are unweighted means over usable accounts —
	// the API exposes only percentages, never absolute plan capacity, so
	// equal weights are the only honest aggregate.
	Remaining5h float64
	Remaining7d float64
	// BurnPerHour is the summed drain across usable accounts, in
	// percent-of-one-account's-window per hour.
	BurnPerHour float64
	// NetBurnPerHour is the projected change of the pool's mean 5h remaining
	// over the next hour, in percentage points per hour, crediting 5h-window
	// refills that land inside that hour. Positive means draining; negative
	// means refills outpace burn (the pool is recovering). Unlike the summed
	// BurnPerHour it is mean-based, so it describes exactly how fast
	// Remaining5h moves.
	NetBurnPerHour float64
	// DryAt is when the pool's combined 5h remaining hits 0 at the combined
	// burn, assuming selection keeps rebalancing load freely. Zero when burn
	// is 0 or a reset refills some window first — absence means "relief
	// arrives first", not "infinite runway".
	DryAt time.Time
	Mood  Mood
}

// PoolOf rolls up account states. ok=false means no account has ever been
// sampled — the snapshot omits the pool block entirely.
//
// Usable means sampled and not rate-limited: a rate-limited account cannot
// serve, and its latest sample is the zeroed 429 placeholder, so its
// "remaining" is fabricated. Stale accounts still count toward remaining
// (last known data is the best estimate); their burn is already gated to 0.
func PoolOf(accts []PoolAccount, now time.Time) (Pool, bool) {
	sampled := false
	for _, a := range accts {
		if a.HasUsage {
			sampled = true
			break
		}
	}
	if !sampled {
		return Pool{}, false
	}

	var usable int
	var sum5, sum7, burn, drop float64
	var earliestReset time.Time
	for _, a := range accts {
		if !a.HasUsage || a.RateLimited {
			continue
		}
		usable++
		sum5 += clamp(a.Remaining5h)
		sum7 += clamp(a.Remaining7d)
		burn += a.BurnPerHour
		drop += netDrop(a, now)
		if a.Resets5h.After(now) && (earliestReset.IsZero() || a.Resets5h.Before(earliestReset)) {
			earliestReset = a.Resets5h
		}
	}
	var p Pool
	if usable > 0 {
		p.Remaining5h = sum5 / float64(usable)
		p.Remaining7d = sum7 / float64(usable)
		p.BurnPerHour = burn
		p.NetBurnPerHour = drop / float64(usable) / netBurnHorizon.Hours()
		if burn > 0 {
			dry := now.Add(hours(sum5 / burn)).Truncate(time.Second)
			if earliestReset.IsZero() || dry.Before(earliestReset) {
				p.DryAt = dry
			}
		}
	}
	p.Mood = moodOf(usable, p.Remaining5h, !p.DryAt.IsZero())
	return p, true
}

// moodOf maps pool state to an alarm level: thresholds on mean remaining,
// bumped one level worse when a dry-out is projected before any reset relief
// (the overshoot signal).
func moodOf(usable int, remaining5h float64, dryProjected bool) Mood {
	if usable == 0 {
		return MoodPanic // nothing can serve right now
	}
	var m Mood
	switch {
	case remaining5h >= 60:
		m = MoodChill
	case remaining5h >= 40:
		m = MoodEasy
	case remaining5h >= 25:
		m = MoodUneasy
	case remaining5h >= 10:
		m = MoodWorried
	default:
		m = MoodAlarmed
	}
	if dryProjected {
		m = m.worse()
	}
	return m
}

// netBurnHorizon is the NetBurnPerHour lookahead: refills landing inside it
// are credited, later ones ignored.
const netBurnHorizon = time.Hour

// netDrop projects how many points of a's clamped 5h remaining vanish over
// the next netBurnHorizon: a window resetting inside the horizon refills to
// 100 and keeps draining for the rest of it, and remaining never drains below
// 0. Negative means the refill outweighs the burn.
func netDrop(a PoolAccount, now time.Time) float64 {
	start := clamp(a.Remaining5h)
	if a.Resets5h.After(now) && !a.Resets5h.After(now.Add(netBurnHorizon)) {
		rest := netBurnHorizon - a.Resets5h.Sub(now)
		return start - max(0, 100-a.BurnPerHour*rest.Hours())
	}
	return start - max(0, start-a.BurnPerHour*netBurnHorizon.Hours())
}

func clamp(v float64) float64 { return max(0, min(100, v)) }
