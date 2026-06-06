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
