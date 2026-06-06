package daemon

import (
	"context"
	"time"

	"github.com/yasyf/claude-pool/internal/procscan"
)

const (
	basePollInterval = 45 * time.Second
	pollJitter       = 15 * time.Second
	rateLimitBackoff = 5 * time.Minute
)

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
	sessions, _ := procscan.Scan()
	alive := procscan.AlivePIDs(sessions)
	if alive != nil {
		if n, err := s.m.Store.CloseDeadSessions(alive); err == nil && n > 0 {
			s.log.Printf("reconciled %d ended session(s)", n)
		}
	}

	accts, err := s.m.Store.ListAccounts()
	if err != nil {
		s.log.Printf("list accounts: %v", err)
		return
	}
	for _, a := range accts {
		// Respect a rate-limit backoff: if the latest sample is rate-limited
		// and recent, skip until the backoff elapses.
		if last, ok, _ := s.m.Store.LatestUsageSample(a.ID); ok && last.RateLimited &&
			time.Since(last.TS) < rateLimitBackoff {
			continue
		}
		idle := procscan.CountByConfigDir(sessions, a.ConfigDir, s.m.DefaultDir) == 0

		// A previously checked-out account that is now idle may carry a token
		// rotated by its live session — adopt it before sampling.
		if idle {
			_ = s.m.AdoptRotatedToken(a)
		}

		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _, err := s.m.SampleUsage(cctx, a, idle)
		cancel()
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
