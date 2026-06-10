package pool

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-pool/internal/score"
	"github.com/yasyf/cc-pool/internal/store"
)

func openTestManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Manager{Store: st, LockDir: t.TempDir()}
}

func TestStickyPick(t *testing.T) {
	// Truncate to seconds: SelectedAt round-trips through the store as Unix
	// seconds, and the TTL-boundary case needs an exact comparison.
	now := time.Now().Truncate(time.Second)
	// acct-1 ranks first; acct-2 is the sticky target the record points at.
	healthy := []score.Result{
		{AccountID: 1, Score: 80, Available: true, Components: score.Components{RawRemaining5h: 90}},
		{AccountID: 2, Score: 50, Available: true, Components: score.Components{RawRemaining5h: 50}},
	}
	cases := []struct {
		name       string
		cwd        string // cwd passed to StickyPick ("" disables)
		record     bool   // whether to write a sticky row for /proj -> recordID
		recordID   int
		recordedAt time.Time
		ranked     []score.Result
		wantID     int
		wantOK     bool
	}{
		{"honored over rank-1", "/proj", true, 2, now.Add(-30 * time.Minute), healthy, 2, true},
		{"expired", "/proj", true, 2, now.Add(-StickyTTL - time.Minute), healthy, 0, false},
		{"exactly at TTL still sticky", "/proj", true, 2, now.Add(-StickyTTL), healthy, 2, true},
		{"near-full abandoned", "/proj", true, 2, now, []score.Result{
			{AccountID: 1, Score: 80, Available: true, Components: score.Components{RawRemaining5h: 90}},
			{AccountID: 2, Score: 5, Available: true, Components: score.Components{RawRemaining5h: score.StickyMinRemaining5h - 1}},
		}, 0, false},
		{"rate-limited abandoned", "/proj", true, 2, now, []score.Result{
			{AccountID: 1, Score: 80, Available: true, Components: score.Components{RawRemaining5h: 90}},
			{AccountID: 2, Score: -50, Available: false, Components: score.Components{RawRemaining5h: 50}},
		}, 0, false},
		// The 2026-06-10 incident: the pinned account is exhausted but its
		// imminent reset keeps eff5 high — the pin must still be abandoned.
		{"exhausted pin abandoned", "/proj", true, 2, now, []score.Result{
			{AccountID: 1, Score: 80, Available: true, Components: score.Components{RawRemaining5h: 60}},
			{AccountID: 2, Score: 65, Available: false, Exhausted: true, Components: score.Components{Eff5: 93, RawRemaining5h: 0}},
		}, 0, false},
		{"account deleted", "/proj", true, 9, now, healthy, 0, false},
		{"empty cwd disabled", "", true, 2, now, healthy, 0, false},
		{"no record", "/other", false, 0, time.Time{}, healthy, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := openTestManager(t)
			if tc.record {
				if err := m.Store.UpsertSticky("/proj", tc.recordID, tc.recordedAt); err != nil {
					t.Fatal(err)
				}
			}
			r, ok := m.StickyPick(tc.cwd, tc.ranked, now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && r.AccountID != tc.wantID {
				t.Fatalf("picked acct %d, want %d", r.AccountID, tc.wantID)
			}
		})
	}
}

func TestRecordStickySlidingTTL(t *testing.T) {
	m := openTestManager(t)
	t0 := time.Now()
	ranked := []score.Result{{AccountID: 2, Score: 50, Available: true, Components: score.Components{RawRemaining5h: 50}}}

	if err := m.RecordSticky("/proj", 2, t0); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.StickyPick("/proj", ranked, t0.Add(50*time.Minute)); !ok {
		t.Fatal("expected sticky hit at t0+50m")
	}
	// A select at t0+50m refreshes the clock, keeping t0+100m sticky.
	if err := m.RecordSticky("/proj", 2, t0.Add(50*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.StickyPick("/proj", ranked, t0.Add(100*time.Minute)); !ok {
		t.Fatal("expected sticky hit at t0+100m after sliding refresh")
	}
	// Control: without the refresh, t0+100m is past the 1h TTL.
	if err := m.RecordSticky("/control", 2, t0); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.StickyPick("/control", ranked, t0.Add(100*time.Minute)); ok {
		t.Fatal("expected sticky miss at t0+100m without refresh")
	}

	// Empty cwd is a no-op, never an error.
	if err := m.RecordSticky("", 2, t0); err != nil {
		t.Fatalf("empty cwd: %v", err)
	}
}

// TestSelectHonorsSticky pins the slot-in location (after Rank, before Pick):
// a fresh sticky record overrides the emptier-account ranking, an expired one
// does not, and the winner is always (re)recorded.
func TestSelectHonorsSticky(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	setup := func(t *testing.T) *Manager {
		m := openTestManager(t)
		dir := t.TempDir()
		for id, util := range map[int]float64{1: 10, 2: 50} {
			if err := m.Store.UpsertAccount(store.Account{
				ID: id, ConfigDir: filepath.Join(dir, "acct", string(rune('a'+id))),
				KeychainService: "svc", KeychainAccount: "u",
			}); err != nil {
				t.Fatal(err)
			}
			if err := m.Store.InsertUsageSample(store.UsageSample{AccountID: id, TS: now, Util5h: util, Util7d: util}); err != nil {
				t.Fatal(err)
			}
		}
		return m
	}

	t.Run("fresh sticky overrides ranking", func(t *testing.T) {
		m := setup(t)
		if err := m.Store.UpsertSticky("/proj", 2, now); err != nil {
			t.Fatal(err)
		}
		sr, err := m.Select(ctx, SelectOptions{Cwd: "/proj"})
		if err != nil {
			t.Fatal(err)
		}
		if sr.Best.ID != 2 || !sr.Sticky {
			t.Fatalf("got acct %d sticky=%v, want sticky acct 2", sr.Best.ID, sr.Sticky)
		}
	})

	t.Run("expired sticky falls through and is overwritten", func(t *testing.T) {
		m := setup(t)
		if err := m.Store.UpsertSticky("/proj", 2, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		sr, err := m.Select(ctx, SelectOptions{Cwd: "/proj"})
		if err != nil {
			t.Fatal(err)
		}
		if sr.Best.ID != 1 || sr.Sticky {
			t.Fatalf("got acct %d sticky=%v, want non-sticky acct 1 (emptier)", sr.Best.ID, sr.Sticky)
		}
		st, ok, err := m.Store.GetSticky("/proj")
		if err != nil || !ok || st.AccountID != 1 {
			t.Fatalf("winner not recorded: %+v ok=%v err=%v", st, ok, err)
		}
	})

	t.Run("no cwd records nothing", func(t *testing.T) {
		m := setup(t)
		if _, err := m.Select(ctx, SelectOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := m.Store.GetSticky(""); ok {
			t.Fatal("empty cwd must not be recorded")
		}
	})
}

// TestSelectAllExhaustedFallback: when every account's 5h window is pegged with
// a pending reset, Select must still return the least-bad one — flagged so the
// caller warns — rather than erroring; only all-rate-limited errors.
func TestSelectAllExhaustedFallback(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	m := openTestManager(t)
	dir := t.TempDir()
	for id, util7 := range map[int]float64{1: 90, 2: 10} {
		if err := m.Store.UpsertAccount(store.Account{
			ID: id, ConfigDir: filepath.Join(dir, "acct", string(rune('a'+id))),
			KeychainService: "svc", KeychainAccount: "u",
		}); err != nil {
			t.Fatal(err)
		}
		if err := m.Store.InsertUsageSample(store.UsageSample{
			AccountID: id, TS: now, Util5h: 100, Util7d: util7,
			Resets5h: now.Add(20 * time.Minute), ExtraEnabled: id == 2,
		}); err != nil {
			t.Fatal(err)
		}
	}

	sr, err := m.Select(ctx, SelectOptions{})
	if err != nil {
		t.Fatalf("all-exhausted select must fall back, got %v", err)
	}
	if !sr.ExhaustedFallback || !sr.Result.Exhausted {
		t.Fatalf("fallback pick must be flagged: %+v", sr)
	}
	if sr.Best.ID != 2 {
		t.Fatalf("expected least-bad acct 2 (emptier 7d), got %d", sr.Best.ID)
	}
	if !sr.ExtraEnabled {
		t.Fatal("pick's extra-usage flag must surface for the warning")
	}

	// Negative control: rate-limited accounts cannot serve at all.
	m2 := openTestManager(t)
	if err := m2.Store.UpsertAccount(store.Account{ID: 1, ConfigDir: filepath.Join(dir, "rl"), KeychainService: "svc", KeychainAccount: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := m2.Store.InsertUsageSample(store.UsageSample{AccountID: 1, TS: now, Util5h: 100, Resets5h: now.Add(20 * time.Minute), RateLimited: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Select(ctx, SelectOptions{}); !errors.Is(err, ErrNoneAvailable) {
		t.Fatalf("all-rate-limited must error ErrNoneAvailable, got %v", err)
	}
}
