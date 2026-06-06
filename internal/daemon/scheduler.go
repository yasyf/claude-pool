package daemon

import (
	"context"
	"time"

	"github.com/yasyf/claude-pool/internal/procscan"
)

const (
	// The /usage and token endpoints rate-limit aggressively: polling every
	// 30–60s can trip a 30+ minute 429 with no Retry-After. 180s + jitter keeps
	// us well clear while still feeling live for start-of-session selection.
	basePollInterval = 180 * time.Second
	pollJitter       = 30 * time.Second

	// Per-account spacing so N accounts don't hit the shared-IP bucket at once.
	perAccountSpacing = 2 * time.Second

	// Exponential rate-limit backoff: 3, 6, 12, … capped at 15 minutes.
	rateLimitBackoffBase = 3 * time.Minute
	rateLimitBackoffCap  = 15 * time.Minute
)

// rlBackoff returns the backoff duration for a given consecutive-429 streak.
func rlBackoff(streak int) time.Duration {
	d := rateLimitBackoffBase
	for i := 1; i < streak && d < rateLimitBackoffCap; i++ {
		d *= 2
	}
	if d > rateLimitBackoffCap {
		d = rateLimitBackoffCap
	}
	return d
}

// scheduler runs the periodic usage poll + idle-only refresh + session
// reconciliation until ctx is cancelled.
func (s *Server) scheduler(ctx context.Context) {
	// Prime immediately so status/select have data right away.
	s.pollOnce(ctx)
	for {
		d := basePollInterval + jitter(pollJitter, time.Now().UnixNano())
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
			s.pollOnce(ctx)
		}
	}
}

// pollOnce reconciles sessions, then samples usage for every account,
// refreshing the token only for idle accounts.
func (s *Server) pollOnce(ctx context.Context) {
	sessions, err := procscan.Scan()
	if err != nil {
		s.log.Printf("procscan: %v", err)
	}
	if alive := procscan.AlivePIDs(sessions); alive != nil {
		switch n, err := s.m.Store.CloseDeadSessions(alive); {
		case err != nil:
			s.log.Printf("close dead sessions: %v", err)
		case n > 0:
			s.log.Printf("reconciled %d ended session(s)", n)
		}
	}

	accts, err := s.m.Store.ListAccounts()
	if err != nil {
		s.log.Printf("list accounts: %v", err)
		return
	}
	for i, a := range accts {
		// Respect an exponential rate-limit backoff keyed on the consecutive-429
		// streak for this account.
		if last, ok, _ := s.m.Store.LatestUsageSample(a.ID); ok && last.RateLimited &&
			time.Since(last.TS) < rlBackoff(s.rlStreak[a.ID]) {
			continue
		}
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(perAccountSpacing):
			}
		}
		// Re-assert the overlay so long-lived setups pick up new top-level
		// ~/.claude entries without an explicit sync (symlink relinks; fuse
		// health-checks). acct-00 is a no-op.
		if err := s.m.SyncOverlay(a); err != nil {
			s.log.Printf("acct-%02d overlay sync: %v", a.ID, err)
		}

		idle := procscan.CountByConfigDir(sessions, a.ConfigDir, s.m.DefaultDir) == 0

		// A previously checked-out account that is now idle may carry a token
		// rotated by its live session — adopt it before sampling. (For acct-00
		// this propagates plain claude's rotated token; SampleUsage never
		// POST-refreshes acct-00.)
		if idle {
			if err := s.m.AdoptRotatedToken(a); err != nil {
				s.log.Printf("acct-%02d adopt rotated token: %v", a.ID, err)
			}
		}

		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, rateLimited, err := s.m.SampleUsage(cctx, a, idle)
		cancel()
		// rlStreak is scheduler-local (this goroutine only) — no lock needed.
		if rateLimited {
			s.rlStreak[a.ID]++
		} else if err == nil {
			s.rlStreak[a.ID] = 0
		}
		if err != nil {
			s.log.Printf("acct-%02d sample: %v", a.ID, err)
		}
	}
}

// jitter returns a deterministic-ish jitter in [0, max) derived from seed,
// avoiding Math.random (unavailable / non-reproducible).
func jitter(max time.Duration, seed int64) time.Duration {
	if max <= 0 {
		return 0
	}
	if seed < 0 {
		seed = -seed
	}
	return time.Duration(seed % int64(max))
}
