package forecast

import (
	"math"
	"testing"
	"time"

	"github.com/yasyf/cc-pool/internal/store"
)

// sample builds a usage sample age before now at the given 5h utilization.
func sample(now time.Time, age time.Duration, util5h float64) store.UsageSample {
	return store.UsageSample{AccountID: 1, TS: now.Add(-age), Util5h: util5h}
}

// rlSample is sample with the rate-limited 429 placeholder shape: zeroed
// utilization, RateLimited set — exactly what recordSample stores on a 429.
func rlSample(now time.Time, age time.Duration) store.UsageSample {
	s := sample(now, age, 0)
	s.RateLimited = true
	return s
}

func TestBurn5h(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		samples []store.UsageSample // newest first, as RecentUsageSamples returns
		want    float64
	}{
		"no samples": {nil, 0},
		"single sample": {
			[]store.UsageSample{sample(now, 0, 50)}, 0,
		},
		"two samples below min count": {
			[]store.UsageSample{sample(now, 0, 55), sample(now, 15*time.Minute, 50)}, 0,
		},
		"steady burn yields exact secant": {
			// +1%/3min over 12 minutes = 20%/hr.
			[]store.UsageSample{
				sample(now, 0, 54), sample(now, 3*time.Minute, 53),
				sample(now, 6*time.Minute, 52), sample(now, 9*time.Minute, 51),
				sample(now, 12*time.Minute, 50),
			},
			20,
		},
		"integer staircase smooths through the endpoints": {
			// Quantized API: pairs repeat, secant sees 2% over 15min = 8%/hr.
			[]store.UsageSample{
				sample(now, 0, 52), sample(now, 3*time.Minute, 52),
				sample(now, 6*time.Minute, 51), sample(now, 9*time.Minute, 51),
				sample(now, 12*time.Minute, 50), sample(now, 15*time.Minute, 50),
			},
			8,
		},
		"flat idle yields zero": {
			[]store.UsageSample{
				sample(now, 0, 50), sample(now, 6*time.Minute, 50),
				sample(now, 12*time.Minute, 50),
			},
			0,
		},
		"reset mid-window truncates to the post-reset segment": {
			// Pre-reset sample at util 90 must not poison the slope:
			// post-reset segment is (12−1)% over 15min = 44%/hr.
			[]store.UsageSample{
				sample(now, 0, 12), sample(now, 5*time.Minute, 8),
				sample(now, 10*time.Minute, 4), sample(now, 15*time.Minute, 1),
				sample(now, 20*time.Minute, 90),
			},
			44,
		},
		"post-reset segment too short yields zero": {
			[]store.UsageSample{
				sample(now, 0, 5), sample(now, 3*time.Minute, 3),
				sample(now, 6*time.Minute, 1), sample(now, 9*time.Minute, 95),
			},
			0,
		},
		"rate-limited placeholder does not fake a reset": {
			// The zeroed 429 sample sits mid-stream; dropping it keeps the
			// window intact: 4% over 12min = 20%/hr.
			[]store.UsageSample{
				sample(now, 0, 54), sample(now, 3*time.Minute, 53),
				rlSample(now, 5*time.Minute),
				sample(now, 6*time.Minute, 52), sample(now, 9*time.Minute, 51),
				sample(now, 12*time.Minute, 50),
			},
			20,
		},
		"samples beyond the window are excluded": {
			// The 50-minute-old wild sample would inflate the slope massively.
			[]store.UsageSample{
				sample(now, 0, 54), sample(now, 6*time.Minute, 52),
				sample(now, 12*time.Minute, 50), sample(now, 50*time.Minute, 1),
			},
			20,
		},
		"span below minimum yields zero": {
			[]store.UsageSample{
				sample(now, 0, 54), sample(now, 3*time.Minute, 52),
				sample(now, 6*time.Minute, 50),
			},
			0,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := Burn5h(tc.samples, now)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("Burn5h = %v, want %v", got, tc.want)
			}
		})
	}
}
