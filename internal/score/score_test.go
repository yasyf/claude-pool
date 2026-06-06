package score

import (
	"testing"
	"time"
)

func TestScorePrefersMoreRemaining(t *testing.T) {
	now := time.Now()
	full := Score(Input{AccountID: 1, HasUsage: true, SampleTS: now, Util5h: 10, Util7d: 5}, now)
	drained := Score(Input{AccountID: 2, HasUsage: true, SampleTS: now, Util5h: 90, Util7d: 80}, now)
	if full.Score <= drained.Score {
		t.Fatalf("expected emptier account to score higher: full=%.2f drained=%.2f", full.Score, drained.Score)
	}
}

func TestRateLimitMakesUnavailable(t *testing.T) {
	now := time.Now()
	r := Score(Input{AccountID: 1, HasUsage: true, SampleTS: now, Util5h: 0, RateLimited: true}, now)
	if r.Available {
		t.Fatal("rate-limited account must be unavailable")
	}
	if r.Components.RateLimitPenalty != PenRateLimit {
		t.Fatalf("expected rate-limit penalty %v, got %v", PenRateLimit, r.Components.RateLimitPenalty)
	}
}

func TestStaleWhenOld(t *testing.T) {
	now := time.Now()
	r := Score(Input{AccountID: 1, HasUsage: true, SampleTS: now.Add(-5 * time.Minute), Util5h: 0}, now)
	if !r.Stale {
		t.Fatal("old sample must be stale")
	}
}

func TestSessionPenalty(t *testing.T) {
	now := time.Now()
	idle := Score(Input{AccountID: 1, HasUsage: true, SampleTS: now, Util5h: 0}, now)
	busy := Score(Input{AccountID: 2, HasUsage: true, SampleTS: now, Util5h: 0, ActiveSessions: 3}, now)
	if diff := idle.Score - busy.Score; diff != WSession*3 {
		t.Fatalf("expected session penalty %.1f, got %.1f", WSession*3, diff)
	}
}

func TestRankTieBreakBySoonestReset(t *testing.T) {
	now := time.Now()
	// Equal score; account 2 resets sooner -> should rank first.
	inputs := []Input{
		{AccountID: 1, HasUsage: true, SampleTS: now, Util5h: 50, Util7d: 50, Resets5h: now.Add(2 * time.Hour)},
		{AccountID: 2, HasUsage: true, SampleTS: now, Util5h: 50, Util7d: 50, Resets5h: now.Add(1 * time.Hour)},
	}
	ranked := Rank(inputs, now)
	if ranked[0].AccountID != 2 {
		t.Fatalf("tie should break to soonest reset (acct 2), got acct %d", ranked[0].AccountID)
	}
}

func TestPickSkipsRateLimited(t *testing.T) {
	now := time.Now()
	inputs := []Input{
		{AccountID: 1, HasUsage: true, SampleTS: now, Util5h: 0, RateLimited: true},
		{AccountID: 2, HasUsage: true, SampleTS: now, Util5h: 30},
	}
	best, ok := Pick(Rank(inputs, now))
	if !ok || best.AccountID != 2 {
		t.Fatalf("expected to pick available acct 2, got ok=%v id=%d", ok, best.AccountID)
	}
}

func TestPickNoneWhenAllRateLimited(t *testing.T) {
	now := time.Now()
	inputs := []Input{
		{AccountID: 1, HasUsage: true, SampleTS: now, RateLimited: true},
		{AccountID: 2, HasUsage: true, SampleTS: now, RateLimited: true},
	}
	if _, ok := Pick(Rank(inputs, now)); ok {
		t.Fatal("expected no available account")
	}
}

func TestNeverSampledIsSelectableButPenalized(t *testing.T) {
	now := time.Now()
	known := Score(Input{AccountID: 1, HasUsage: true, SampleTS: now, Util5h: 20}, now)
	unknown := Score(Input{AccountID: 2, HasUsage: false}, now)
	if !unknown.Available {
		t.Fatal("never-sampled account should still be available")
	}
	if unknown.Score >= known.Score {
		t.Fatal("never-sampled account should score below a known-good one due to stale penalty")
	}
}

// TestHealthyEqualsBaseline: a healthy account (windows far from reset, above
// the barrier knee, no measured burn) scores exactly the baseline formula.
func TestHealthyEqualsBaseline(t *testing.T) {
	now := time.Now()
	in := Input{AccountID: 1, HasUsage: true, SampleTS: now, Util5h: 40, Util7d: 30}
	got := Score(in, now).Score
	want := W5h*(100-40) + W7d*(100-30) // no penalties, no guards
	if got != want {
		t.Fatalf("healthy score = %.4f, want baseline %.4f", got, want)
	}
}

func TestImminentResetRanksUp(t *testing.T) {
	now := time.Now()
	imminent := Score(Input{AccountID: 1, HasUsage: true, SampleTS: now, Util5h: 80, Resets5h: now.Add(12 * time.Minute)}, now)
	far := Score(Input{AccountID: 2, HasUsage: true, SampleTS: now, Util5h: 80, Resets5h: now.Add(4 * time.Hour)}, now)
	if imminent.Score <= far.Score {
		t.Fatalf("about-to-reset account should rank up: imminent=%.2f far=%.2f", imminent.Score, far.Score)
	}
	if imminent.Components.Eff5 < 90 {
		t.Fatalf("imminent reset should lift eff5 near full, got %.1f", imminent.Components.Eff5)
	}
}

// TestBarrierGuardsLowSevenDay: a 5h-rich account whose 7-day window is nearly
// exhausted must rank below a balanced peer (the weighted sum alone would mask it).
func TestBarrierGuardsLowSevenDay(t *testing.T) {
	now := time.Now()
	lowWeekly := Score(Input{AccountID: 1, HasUsage: true, SampleTS: now, Util5h: 10, Util7d: 92}, now)
	balanced := Score(Input{AccountID: 2, HasUsage: true, SampleTS: now, Util5h: 40, Util7d: 40}, now)
	if lowWeekly.Components.Barrier7d == 0 {
		t.Fatal("expected a 7d barrier penalty for the nearly-exhausted weekly window")
	}
	if lowWeekly.Score >= balanced.Score {
		t.Fatalf("barrier should downrank the low-weekly account: low=%.2f balanced=%.2f", lowWeekly.Score, balanced.Score)
	}
}

func TestBurnRateRunwayDownranks(t *testing.T) {
	now := time.Now()
	draining := Score(Input{AccountID: 1, HasUsage: true, SampleTS: now, Util5h: 50, Burn5hPerHour: 20}, now)
	stable := Score(Input{AccountID: 2, HasUsage: true, SampleTS: now, Util5h: 50, Burn5hPerHour: 0}, now)
	if draining.Components.RunwayPenalty == 0 {
		t.Fatal("expected a runway penalty for the actively-draining account")
	}
	if draining.Score >= stable.Score {
		t.Fatalf("burn-rate should downrank the draining account: draining=%.2f stable=%.2f", draining.Score, stable.Score)
	}
}

// TestZeroKnobsReproduceBaseline: disabling the guards recovers the exact
// baseline even for an account that would otherwise trip them.
func TestZeroKnobsReproduceBaseline(t *testing.T) {
	defer restoreKnobs(BarrierKnee, RunwayWeight)
	BarrierKnee, RunwayWeight = 0, 0
	now := time.Now()
	in := Input{AccountID: 1, HasUsage: true, SampleTS: now, Util5h: 95, Util7d: 96, Burn5hPerHour: 50}
	got := Score(in, now).Score
	want := W5h*(100-95) + W7d*(100-96)
	if got != want {
		t.Fatalf("with guards disabled, score = %.4f, want baseline %.4f", got, want)
	}
}

func restoreKnobs(knee, runway float64) { BarrierKnee, RunwayWeight = knee, runway }

func TestUsableForSticky(t *testing.T) {
	cases := []struct {
		name string
		r    Result
		want bool
	}{
		{"healthy", Result{Available: true, Components: Components{Eff5: 90}}, true},
		{"rate-limited despite headroom", Result{Available: false, Components: Components{Eff5: 90}}, false},
		{"just below floor", Result{Available: true, Components: Components{Eff5: StickyMinEff5 - 0.1}}, false},
		{"exactly at floor", Result{Available: true, Components: Components{Eff5: StickyMinEff5}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UsableForSticky(tc.r); got != tc.want {
				t.Fatalf("UsableForSticky(%+v) = %v, want %v", tc.r, got, tc.want)
			}
		})
	}
}
