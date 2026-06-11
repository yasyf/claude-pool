// Package forecast computes display predictions from recent usage history:
// per-account burn rates and depletion estimates, plus the pool-wide rollup
// (mean remaining capacity, dry-out ETA, alarm mood) that the status snapshot
// ships to out-of-process readers like the macOS widget.
package forecast

import (
	"time"

	"github.com/yasyf/cc-pool/internal/store"
)

const (
	// BurnWindow is how far back the burn estimate looks.
	BurnWindow = 45 * time.Minute
	// BurnMinSpan is the minimum newest-to-oldest span; below it the API's
	// integer-percent quantization dominates the slope (±1% over 10 min is
	// already ±6%/hr of noise).
	BurnMinSpan = 10 * time.Minute
	// BurnMinSamples is the minimum usable sample count.
	BurnMinSamples = 3
	// BurnSampleLimit is how many samples callers should fetch to cover
	// BurnWindow at the daemon's ~3-minute cadence, with room for backoff gaps.
	BurnSampleLimit = 32
)

// Burn5h estimates the recent drain of the 5h window in percent/hour from
// recent samples (newest first, as store.RecentUsageSamples returns them).
// 0 means idle or unknown — either way there is nothing to project forward.
//
// Rate-limited samples are dropped first: a 429 poll records a zeroed
// placeholder whose utilization drop would otherwise read as a window reset.
// A genuine reset (utilization higher in an older sample) truncates the
// window to the post-reset segment. The estimate is the endpoint secant:
// utilization is monotone within a window, so the secant is unbiased and
// maximally smooth against the API's integer-percent quantization.
func Burn5h(samples []store.UsageSample, now time.Time) float64 {
	usable := make([]store.UsageSample, 0, len(samples))
	for _, s := range samples {
		if s.RateLimited || now.Sub(s.TS) > BurnWindow {
			continue
		}
		usable = append(usable, s)
	}
	for i := 0; i+1 < len(usable); i++ {
		if usable[i+1].Util5h > usable[i].Util5h {
			usable = usable[:i+1]
			break
		}
	}
	if len(usable) < BurnMinSamples {
		return 0
	}
	span := usable[0].TS.Sub(usable[len(usable)-1].TS)
	if span < BurnMinSpan {
		return 0
	}
	return (usable[0].Util5h - usable[len(usable)-1].Util5h) / span.Hours()
}
