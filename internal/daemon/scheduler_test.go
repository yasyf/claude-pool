package daemon

import (
	"testing"
	"time"
)

func TestRLBackoffExponentialCapped(t *testing.T) {
	cases := map[int]time.Duration{
		0: rateLimitBackoffBase, // streak 0 -> base
		1: rateLimitBackoffBase, // first 429 -> base
		2: 6 * time.Minute,      // 3 -> 6
		3: 12 * time.Minute,     // 6 -> 12
		4: rateLimitBackoffCap,  // 24 capped to 15
		9: rateLimitBackoffCap,  // stays capped
	}
	for streak, want := range cases {
		if got := rlBackoff(streak); got != want {
			t.Errorf("rlBackoff(%d) = %v, want %v", streak, got, want)
		}
	}
}

func TestJitterBounded(t *testing.T) {
	for _, seed := range []int64{0, 1, 1 << 40, -7, 2_026_000_000} {
		j := jitter(pollJitter, seed)
		if j < 0 || j >= pollJitter {
			t.Errorf("jitter(%d) = %v out of [0,%v)", seed, j, pollJitter)
		}
	}
}
