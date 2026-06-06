package pool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/oauth"
	"github.com/yasyf/cc-pool/internal/store"
)

// RefreshLeadTime is how close to expiry an idle account's token is refreshed
// preemptively (the spec's "<10 min" pre-flight window).
const RefreshLeadTime = 10 * time.Minute

// ErrNeedsLogin indicates the stored refresh token is gone/revoked and the
// account must be re-logged-in interactively.
var ErrNeedsLogin = errors.New("account needs re-login (refresh token missing or revoked)")

// ErrRefuseAcctZeroRefresh is returned by refresh when asked to POST-refresh
// acct-00, whose single-use refresh token is owned by plain `claude`.
var ErrRefuseAcctZeroRefresh = errors.New("refusing to POST-refresh acct-00 (shared single-use token owned by plain claude)")

// credServices returns the Keychain services that must be kept in sync for an
// account, primary first. acct-00 has two: the canonical un-suffixed item plain
// `claude` uses (source of truth) and a suffixed mirror that makes
// `CLAUDE_CONFIG_DIR=~/.claude claude` work.
func (m *Manager) credServices(a store.Account) []string {
	if a.IsZero {
		return []string{keychain.DefaultServiceName(), a.KeychainService}
	}
	return []string{a.KeychainService}
}

// readCredential reads the freshest credential across an account's services.
// Caller must hold acctLock(a.ID).
func (m *Manager) readCredential(a store.Account) (*keychain.Credential, string, error) {
	var best *keychain.Credential
	var bestSvc string
	for _, svc := range m.credServices(a) {
		c, err := m.Keychain.Read(svc, a.KeychainAccount)
		if err != nil {
			if errors.Is(err, keychain.ErrNotFound) {
				continue
			}
			return nil, "", err
		}
		if best == nil || c.ClaudeAiOauth.ExpiresAt > best.ClaudeAiOauth.ExpiresAt {
			best, bestSvc = c, svc
		}
	}
	if best == nil {
		return nil, "", keychain.ErrNotFound
	}
	return best, bestSvc, nil
}

// writeCredentialAll writes cred to every service in the account's sync set, so
// acct-00's canonical item and mirror never diverge. Caller must hold
// acctLock(a.ID).
func (m *Manager) writeCredentialAll(a store.Account, cred *keychain.Credential) error {
	for _, svc := range m.credServices(a) {
		if err := m.Keychain.Write(svc, a.KeychainAccount, cred); err != nil {
			return fmt.Errorf("write credential to %q: %w", svc, err)
		}
	}
	return nil
}

// EnsureFreshToken returns the account's credential, refreshing it if the access
// token expires within `within` and allowRefresh is true. A successful refresh
// is written back to all of the account's services and logged. allowRefresh
// should be false for accounts with a live session (that session owns refresh).
func (m *Manager) EnsureFreshToken(ctx context.Context, a store.Account, within time.Duration, allowRefresh bool) (*keychain.Credential, bool, error) {
	mu := m.acctLock(a.ID)
	mu.Lock()
	defer mu.Unlock()
	return m.ensureFreshToken(ctx, a, within, allowRefresh)
}

// ensureFreshToken is EnsureFreshToken's body; split out so SampleUsage can
// compose it with fetchUsage's 401-retry under ONE continuous critical section
// (sync.Mutex is not reentrant). Caller must hold acctLock(a.ID).
func (m *Manager) ensureFreshToken(ctx context.Context, a store.Account, within time.Duration, allowRefresh bool) (*keychain.Credential, bool, error) {
	// acct-00's refresh token is the one plain `claude` uses (the canonical and
	// mirror items share it). Refreshing it here would consume that single-use
	// token out from under a possibly-live plain claude → forced re-login. So we
	// NEVER POST-refresh acct-00; plain claude (or a pooled acct-00 session)
	// owns its lifecycle, and we only propagate whatever token it rotates to.
	if a.IsZero {
		allowRefresh = false
	}
	cred, _, err := m.readCredential(a)
	if err != nil {
		return nil, false, err
	}
	if !cred.ExpiresWithin(within) || !allowRefresh {
		return cred, false, nil
	}
	if !cred.HasRefreshToken() {
		return cred, false, ErrNeedsLogin
	}
	refreshed, err := m.refresh(ctx, a, cred)
	if err != nil {
		_ = m.Store.LogRefresh(a.ID, false, err.Error())
		var re *oauth.RefreshError
		if errors.As(err, &re) && re.Revoked() {
			return cred, false, ErrNeedsLogin
		}
		// Transient: fall back to the (stale) credential we have.
		return cred, false, err
	}
	_ = m.Store.LogRefresh(a.ID, true, "")
	return refreshed, true, nil
}

// refresh performs the OAuth refresh and persists the new blob, preserving the
// non-token fields from the prior credential. Caller must hold acctLock(a.ID).
//
// acct-00's refresh token is the single-use token plain `claude` owns; POST-
// refreshing it here would log the user out. This is the linchpin guard: every
// refresh path (EnsureFreshToken's pre-flight AND fetchUsage's 401 retry) funnels
// through here, so refusing acct-00 at this chokepoint is what actually upholds
// the invariant regardless of caller.
func (m *Manager) refresh(ctx context.Context, a store.Account, prev *keychain.Credential) (*keychain.Credential, error) {
	if a.IsZero {
		return nil, ErrRefuseAcctZeroRefresh
	}
	tr, err := m.OAuth.Refresh(ctx, fmt.Sprintf("acct-%d", a.ID), prev.ClaudeAiOauth.RefreshToken)
	if err != nil {
		return nil, err
	}
	next := &keychain.Credential{ClaudeAiOauth: prev.ClaudeAiOauth}
	next.ClaudeAiOauth.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" { // rotated
		next.ClaudeAiOauth.RefreshToken = tr.RefreshToken
	}
	next.ClaudeAiOauth.ExpiresAt = tr.Expiry(time.Now()).UnixMilli()
	if err := m.writeCredentialAll(a, next); err != nil {
		return nil, err
	}
	return next, nil
}

// AdoptRotatedToken re-reads an account's credential from the Keychain (where a
// live claude session may have rotated it) and propagates it across the
// account's services, keeping acct-00's mirror in lockstep with the canonical
// item. Used by the daemon on session check-in.
func (m *Manager) AdoptRotatedToken(a store.Account) error {
	mu := m.acctLock(a.ID)
	mu.Lock()
	defer mu.Unlock()
	cred, _, err := m.readCredential(a)
	if err != nil {
		return err
	}
	return m.writeCredentialAll(a, cred)
}

// SampleUsage fetches the account's usage windows, refreshing once on 401, and
// records a usage_sample. Returns the usage and whether the account is
// currently rate-limited.
func (m *Manager) SampleUsage(ctx context.Context, a store.Account, allowRefresh bool) (*oauth.Usage, bool, error) {
	usage, rateLimited, err := m.sampleUsage(ctx, a, allowRefresh)
	if err != nil {
		return nil, rateLimited, err
	}
	m.recordSample(a.ID, usage, rateLimited)
	return usage, rateLimited, nil
}

// sampleUsage holds acctLock for the full credential span: the pre-flight
// refresh AND fetchUsage's 401-retry refresh must form one atomic cycle, or
// another goroutine could rotate the token between them and the retry would
// re-POST a consumed single-use refresh token.
func (m *Manager) sampleUsage(ctx context.Context, a store.Account, allowRefresh bool) (*oauth.Usage, bool, error) {
	mu := m.acctLock(a.ID)
	mu.Lock()
	defer mu.Unlock()
	cred, _, err := m.ensureFreshToken(ctx, a, RefreshLeadTime, allowRefresh)
	if err != nil && !errors.Is(err, ErrNeedsLogin) {
		// Even on transient refresh failure we may still have a usable token.
		if cred == nil {
			return nil, false, err
		}
	}
	return m.fetchUsage(ctx, a, cred, allowRefresh)
}

// fetchUsage fetches usage, refreshing once on 401. Caller must hold
// acctLock(a.ID).
func (m *Manager) fetchUsage(ctx context.Context, a store.Account, cred *keychain.Credential, allowRefresh bool) (*oauth.Usage, bool, error) {
	usage, err := m.OAuth.Usage(ctx, cred.ClaudeAiOauth.AccessToken)
	if err == nil {
		return usage, false, nil
	}
	var ue *oauth.UsageError
	if errors.As(err, &ue) {
		if ue.RateLimited() {
			return &oauth.Usage{}, true, nil
		}
		if ue.Unauthorized() && allowRefresh && cred.HasRefreshToken() {
			refreshed, rerr := m.refresh(ctx, a, cred)
			if rerr == nil {
				if usage, err2 := m.OAuth.Usage(ctx, refreshed.ClaudeAiOauth.AccessToken); err2 == nil {
					_ = m.Store.LogRefresh(a.ID, true, "")
					return usage, false, nil
				}
			}
		}
	}
	return nil, false, err
}

// recordSample persists a usage sample (utilization stored as 0..100 percent).
func (m *Manager) recordSample(accountID int, u *oauth.Usage, rateLimited bool) {
	s := store.UsageSample{
		AccountID:    accountID,
		TS:           time.Now(),
		Util5h:       u.FiveHour.Used(),
		Util7d:       u.SevenDay.Used(),
		Util7dOpus:   u.SevenDayOpus.Used(),
		Resets5h:     u.FiveHour.ResetsAt,
		Resets7d:     u.SevenDay.ResetsAt,
		Resets7dOpus: u.SevenDayOpus.ResetsAt,
		RateLimited:  rateLimited,
	}
	// Best-effort: a failed insert self-heals on the next poll and surfaces as
	// the account going stale, so it is intentionally not escalated here.
	_ = m.Store.InsertUsageSample(s)
}
