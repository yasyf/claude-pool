package pool

import (
	"context"
	"time"

	"github.com/yasyf/cc-pool/internal/procscan"
	"github.com/yasyf/cc-pool/internal/score"
	"github.com/yasyf/cc-pool/internal/store"
)

// Snapshot is a fully-resolved per-account view for status/list rendering. It
// is provider- and transport-neutral so both the live CLI path and the daemon
// produce the same shape.
type Snapshot struct {
	Account        store.Account
	Score          float64
	HasUsage       bool
	Util5h         float64 // percent used 0..100
	Util7d         float64
	Remaining5h    float64
	Remaining7d    float64
	ActiveSessions int
	RateLimited    bool
	Stale          bool
	Resets5h       time.Time
	Resets7d       time.Time
	Burn5hPerHour  float64
	SampleAge      time.Duration
}

// Snapshots returns a scored view of every account. When live is true, stale
// usage is sampled synchronously first (the no-daemon path).
func (m *Manager) Snapshots(ctx context.Context, live bool, fresh time.Duration) ([]Snapshot, error) {
	accts, err := m.Store.ListAccounts()
	if err != nil {
		return nil, err
	}
	sessions, _ := procscan.Scan()
	if live {
		m.sampleStale(ctx, accts, sessions, fresh)
	}
	now := time.Now()

	inputs := make([]score.Input, len(accts))
	for i, a := range accts {
		in, err := m.scoreInput(a, sessions, now)
		if err != nil {
			return nil, err
		}
		inputs[i] = in
	}
	results := make(map[int]score.Result)
	for _, r := range score.Rank(inputs, now) {
		results[r.AccountID] = r
	}

	out := make([]Snapshot, 0, len(accts))
	for i, a := range accts {
		in := inputs[i]
		r := results[a.ID]
		s := Snapshot{
			Account:        a,
			Score:          r.Score,
			HasUsage:       in.HasUsage,
			Util5h:         in.Util5h,
			Util7d:         in.Util7d,
			Remaining5h:    100 - in.Util5h,
			Remaining7d:    100 - in.Util7d,
			ActiveSessions: in.ActiveSessions,
			RateLimited:    in.RateLimited,
			Stale:          r.Stale,
			Resets5h:       in.Resets5h,
			Resets7d:       in.Resets7d,
			Burn5hPerHour:  in.Burn5hPerHour,
		}
		if in.HasUsage {
			s.SampleAge = now.Sub(in.SampleTS)
		}
		out = append(out, s)
	}
	return out, nil
}
