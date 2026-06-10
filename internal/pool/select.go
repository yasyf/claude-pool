package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yasyf/cc-pool/internal/procscan"
	"github.com/yasyf/cc-pool/internal/score"
	"github.com/yasyf/cc-pool/internal/store"
)

// ErrNoAccounts means the pool is empty.
var ErrNoAccounts = errors.New("no accounts in the pool — run `ccp add`")

// ErrNoneAvailable means no account can serve: every account is rate-limited,
// or — for NoFallback callers — exhausted.
var ErrNoneAvailable = errors.New("no account is currently available (all exhausted or rate-limited)")

// SelectOptions tunes a selection.
type SelectOptions struct {
	// Live samples usage synchronously for accounts whose latest sample is
	// older than FreshFor (the M2/no-daemon path). When false, only cached
	// samples are used (the daemon keeps them fresh).
	Live bool
	// FreshFor is the cache window for Live sampling.
	FreshFor time.Duration
	// Cwd is the caller's working directory, keying select stickiness.
	// Empty disables stickiness.
	Cwd string
	// NoFallback returns ErrNoneAvailable instead of a least-bad exhausted
	// pick. Set by --wait callers, which would discard the pick (and its
	// sticky rewrite) to keep waiting.
	NoFallback bool
}

// DefaultFreshFor is the default cache window for live selection.
const DefaultFreshFor = 60 * time.Second

// SelectResult is a ranked selection outcome.
type SelectResult struct {
	Best     store.Account
	Result   score.Result
	Ranked   []score.Result
	Sticky   bool    // the pick honored a sticky record rather than the ranking
	HasUsage bool    // the pick has at least one usage sample (false = never sampled)
	Util5h   float64 // the pick's raw 5h percent used (0 when never sampled)
	Util7d   float64 // the pick's raw 7d percent used (0 when never sampled)
	// ExhaustedFallback means every account was exhausted and Best is the
	// least-bad pick: it will bill extra-usage credits (if enabled) or
	// rate-limit until its reset. Callers must warn loudly.
	ExhaustedFallback bool
	// ExtraEnabled reports whether the pick has pay-as-you-go overage billing
	// enabled, for the fallback warning.
	ExtraEnabled bool
	byID         map[int]store.Account
}

// Select scores all accounts and returns the best available one.
func (m *Manager) Select(ctx context.Context, opts SelectOptions) (*SelectResult, error) {
	accts, err := m.Store.ListAccounts()
	if err != nil {
		return nil, err
	}
	if len(accts) == 0 {
		return nil, ErrNoAccounts
	}

	sessions, _ := procscan.Scan() // best-effort; nil on error

	if opts.Live {
		m.sampleStale(ctx, accts, sessions, opts.FreshFor)
	}

	now := time.Now()
	inputs := make([]score.Input, 0, len(accts))
	byID := make(map[int]store.Account, len(accts))
	inByID := make(map[int]score.Input, len(accts))
	extraByID := make(map[int]bool, len(accts))
	for _, a := range accts {
		byID[a.ID] = a
		in, sample, err := m.scoreInput(a, sessions, now)
		if err != nil {
			return nil, err
		}
		inByID[a.ID] = in
		extraByID[a.ID] = sample.ExtraEnabled
		inputs = append(inputs, in)
	}

	ranked := score.Rank(inputs, now)
	if r, ok := m.StickyPick(opts.Cwd, ranked, now); ok {
		// Best-effort: stickiness must never fail a select.
		_ = m.RecordSticky(opts.Cwd, r.AccountID, now)
		ri := inByID[r.AccountID]
		return &SelectResult{Best: byID[r.AccountID], Result: r, Ranked: ranked, Sticky: true, HasUsage: ri.HasUsage, Util5h: ri.Util5h, Util7d: ri.Util7d, byID: byID}, nil
	}
	best, ok := score.Pick(ranked)
	fallback := false
	if !ok && !opts.NoFallback {
		// Every account is exhausted (or worse). Launch on the least-bad
		// exhausted one rather than refusing — the caller warns loudly.
		best, ok = score.PickFallback(ranked)
		fallback = true
	}
	if !ok {
		return &SelectResult{Ranked: ranked, byID: byID}, ErrNoneAvailable
	}
	_ = m.RecordSticky(opts.Cwd, best.AccountID, now) // best-effort, as above
	bi := inByID[best.AccountID]
	return &SelectResult{Best: byID[best.AccountID], Result: best, Ranked: ranked,
		HasUsage: bi.HasUsage, Util5h: bi.Util5h, Util7d: bi.Util7d,
		ExhaustedFallback: fallback, ExtraEnabled: extraByID[best.AccountID], byID: byID}, nil
}

// sampleStale concurrently refreshes usage for accounts whose latest sample is
// older than freshFor. Accounts with a live session are sampled WITHOUT
// refreshing their token (that session owns refresh).
func (m *Manager) sampleStale(ctx context.Context, accts []store.Account, sessions []procscan.Session, freshFor time.Duration) {
	if freshFor <= 0 {
		freshFor = DefaultFreshFor
	}
	now := time.Now()
	var wg sync.WaitGroup
	for _, a := range accts {
		if s, ok, _ := m.Store.LatestUsageSample(a.ID); ok && now.Sub(s.TS) < freshFor {
			continue // fresh enough
		}
		a := a
		allowRefresh := procscan.CountByConfigDir(sessions, a.ConfigDir) == 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			_, _, _ = m.SampleUsage(cctx, a, allowRefresh)
		}()
	}
	wg.Wait()
}

// scoreInput assembles a score.Input for one account from cached state, also
// returning the latest sample so callers can surface fields scoring ignores
// (extra usage). The sample is the zero value when the account was never
// sampled.
func (m *Manager) scoreInput(a store.Account, sessions []procscan.Session, now time.Time) (score.Input, store.UsageSample, error) {
	in := score.Input{AccountID: a.ID}
	var sample store.UsageSample
	if s, ok, err := m.Store.LatestUsageSample(a.ID); err != nil {
		return in, sample, err
	} else if ok {
		sample = s
		in.HasUsage = true
		in.SampleTS = s.TS
		in.Util5h = s.Util5h
		in.Util7d = s.Util7d
		in.Resets5h = s.Resets5h
		in.Resets7d = s.Resets7d
		in.RateLimited = s.RateLimited
		in.Burn5hPerHour = m.burnRate5h(a.ID)
	}
	in.ActiveSessions = procscan.CountByConfigDir(sessions, a.ConfigDir)
	if r, ok, _ := m.Store.LastRefresh(a.ID); ok && !r.OK {
		in.RefreshFailed = true
	}
	return in, sample, nil
}

// burnRate5h estimates the recent rate of change of util_5h in percent/hour
// from the two most recent samples. Returns 0 if there is too little history,
// the samples are too close together (<30s), or utilization decreased (a window
// reset happened, which is not a burn).
func (m *Manager) burnRate5h(accountID int) float64 {
	samples, err := m.Store.RecentUsageSamples(accountID, 2)
	if err != nil || len(samples) < 2 {
		return 0
	}
	newer, older := samples[0], samples[1]
	dt := newer.TS.Sub(older.TS)
	if dt < 30*time.Second {
		return 0
	}
	dUtil := newer.Util5h - older.Util5h
	if dUtil <= 0 {
		return 0
	}
	return dUtil / dt.Hours()
}

// PreflightRefresh refreshes the chosen account's token if it expires within
// RefreshLeadTime and the account is idle, so the launched session starts with
// a healthy token. Errors are returned but non-fatal to the caller.
func (m *Manager) PreflightRefresh(ctx context.Context, a store.Account) error {
	sessions, _ := procscan.Scan()
	idle := procscan.CountByConfigDir(sessions, a.ConfigDir) == 0
	if !idle {
		return nil
	}
	_, _, err := m.EnsureFreshToken(ctx, a, RefreshLeadTime, true)
	if err != nil && !errors.Is(err, ErrNeedsLogin) {
		return fmt.Errorf("preflight refresh: %w", err)
	}
	return err
}

// SoonestReset returns the earliest 5h reset across the pool, for `--wait`.
func (sr *SelectResult) SoonestReset() (time.Time, bool) {
	return score.SoonestReset(sr.Ranked)
}
