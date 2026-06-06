package pool

import (
	"time"

	"github.com/yasyf/cc-pool/internal/score"
)

// StickyTTL is how long a cwd's last selection stays sticky. Claude's prompt
// cache expires well within an hour, so older records carry no continuity
// value. Var so tests can tune it.
var StickyTTL = time.Hour

// StickyPick returns the ranked result for cwd's previously-selected account
// when stickiness should override ranking: the record is fresher than
// StickyTTL, the account still exists in ranked, and it is still usable
// (available with headroom). ok=false falls through to the normal pick.
// Best-effort by design: store errors read as a miss, never a failed select.
func (m *Manager) StickyPick(cwd string, ranked []score.Result, now time.Time) (score.Result, bool) {
	if cwd == "" {
		return score.Result{}, false
	}
	st, ok, err := m.Store.GetSticky(cwd)
	if err != nil || !ok {
		return score.Result{}, false
	}
	if now.Sub(st.SelectedAt) > StickyTTL {
		return score.Result{}, false
	}
	for _, r := range ranked {
		if r.AccountID == st.AccountID {
			return r, score.UsableForSticky(r)
		}
	}
	// The sticky account was removed from the pool.
	return score.Result{}, false
}

// RecordSticky upserts the sticky record for cwd, refreshing the sliding TTL.
// An empty cwd is a no-op.
func (m *Manager) RecordSticky(cwd string, accountID int, now time.Time) error {
	if cwd == "" {
		return nil
	}
	return m.Store.UpsertSticky(cwd, accountID, now)
}
