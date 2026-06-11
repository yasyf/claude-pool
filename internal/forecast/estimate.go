package forecast

import (
	"time"

	"github.com/yasyf/cc-pool/internal/score"
	"github.com/yasyf/cc-pool/internal/store"
)

// Estimate is one account's 5h-window display forecast. The zero value means
// "no projection": the account is idle, stale, rate-limited, exhausted, or
// has too little history — the snapshot omits the fields and the widget
// renders nothing.
type Estimate struct {
	// BurnPerHour is the smoothed drain of the 5h window, percent/hour.
	BurnPerHour float64
	// AtReset is the projected REMAINING percent at Resets5h, clamped to
	// 0..100; 0 when the reset time is unknown (or depletion lands first,
	// which DepletedAt signals).
	AtReset float64
	// DepletedAt is when remaining hits 0 at the current burn, whole seconds;
	// zero when a reset refills the window first.
	DepletedAt time.Time
}

// Estimate5h computes the display forecast from recent samples (newest
// first). exhausted is score's exhausted gate: a pegged window whose reset is
// pending. Projections anchor at the latest sample's timestamp — utilization
// is known as of the sample, not as of now.
func Estimate5h(samples []store.UsageSample, exhausted bool, now time.Time) Estimate {
	if len(samples) == 0 || exhausted {
		return Estimate{}
	}
	latest := samples[0]
	if latest.RateLimited || now.Sub(latest.TS) > score.DisplayStaleAfter {
		return Estimate{}
	}
	if !latest.Resets5h.IsZero() && !latest.Resets5h.After(now) {
		return Estimate{} // the sample predates the refill it projects across
	}
	burn := Burn5h(samples, now)
	if burn <= 0 {
		return Estimate{}
	}
	est := Estimate{BurnPerHour: burn}
	if !latest.Resets5h.IsZero() {
		projected := latest.Util5h + burn*latest.Resets5h.Sub(latest.TS).Hours()
		est.AtReset = max(0, min(100, 100-projected))
	}
	depleted := latest.TS.Add(hours((100 - latest.Util5h) / burn)).Truncate(time.Second)
	if depleted.After(now) && (latest.Resets5h.IsZero() || depleted.Before(latest.Resets5h)) {
		est.DepletedAt = depleted
	}
	return est
}

func hours(h float64) time.Duration {
	return time.Duration(h * float64(time.Hour))
}
