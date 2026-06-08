package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/store"
)

// ErrNotInitialized means the pool has not been set up yet (`ccp init` or
// `ccp add`'s auto-init).
var ErrNotInitialized = errors.New("pool not initialized")

// InitResult summarizes what `ccp init` did.
type InitResult struct {
	OverlayKind overlay.Kind
	Already     bool // the pool was already initialized
}

// Init prepares the pool: ~/.cc-pool state dirs, the overlay provider choice
// for new accounts, and the initialized marker. It never touches ~/.claude or
// the Keychain — accounts (including the user's main subscription) join via
// Add, each with its own independent `claude /login`. Idempotent.
func (m *Manager) Init() (*InitResult, error) {
	if err := EnsureStateDir(); err != nil {
		return nil, err
	}
	if err := EnsureAccountsDir(); err != nil {
		return nil, err
	}
	already, err := m.Initialized()
	if err != nil {
		return nil, err
	}
	kind, err := m.ensureOverlayKind()
	if err != nil {
		return nil, err
	}
	if err := m.Store.SetMeta(metaInitialized, "1"); err != nil {
		return nil, err
	}
	return &InitResult{OverlayKind: kind, Already: already}, nil
}

// PendingAdd describes a half-created account awaiting interactive login.
type PendingAdd struct {
	Index           int
	ConfigDir       string
	KeychainService string
	OverlayKind     overlay.Kind
	LoginCommand    string
	ClaudeJSONSeed  SeedOutcome

	// PurgedStaleCredential reports that PrepareAdd found and deleted a
	// leftover Keychain item from a previous abandoned add at this index.
	PurgedStaleCredential bool
}

// DuplicateIdentity returns an existing pool account that shares want's
// accountUuid (the same Claude subscription), or nil if none does. Accounts
// whose identity cannot be read are skipped, so one broken account dir never
// blocks the check.
func (m *Manager) DuplicateIdentity(want Identity) (*store.Account, error) {
	accounts, err := m.Store.ListAccounts()
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		a := accounts[i]
		id, err := AccountIdentity(overlay.Kind(a.OverlayKind), a.ConfigDir)
		if err != nil {
			continue
		}
		if id.AccountUUID == want.AccountUUID {
			return &a, nil
		}
	}
	return nil, nil
}

// PrepareAdd allocates the next account dir, establishes its overlay, and
// seeds its private .claude.json from plain claude's (~/.claude.json) so the
// login session inherits onboarding state and settings instead of running the
// first-run wizard. It returns the login command the user must run. Unless the
// dir is being reused (SeedKeptExisting), any stale Keychain item left under
// the dir's service by an earlier dead attempt is deleted. No account row or
// new Keychain item is created yet — FinalizeAdd does that once the login
// lands.
func (m *Manager) PrepareAdd() (*PendingAdd, error) {
	ok, err := m.Initialized()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotInitialized
	}
	n, err := m.Store.NextAccountIndex()
	if err != nil {
		return nil, err
	}
	acctDir := AccountDir(n)
	kind, err := m.ensureOverlayKind()
	if err != nil {
		return nil, err
	}
	prov := overlay.For(kind)
	if err := prov.Setup(ClaudeDir(), acctDir); err != nil {
		return nil, fmt.Errorf("set up overlay for %s: %w", acctDir, err)
	}
	seed, err := seedClaudeJSON(prov, acctDir, ClaudeJSONPath())
	if err != nil {
		return nil, fmt.Errorf("seed .claude.json for %s: %w", acctDir, err)
	}
	svc := keychain.ServiceName(acctDir)
	purged := false
	if seed != SeedKeptExisting {
		// A leftover item under this service is garbage from a dead attempt
		// (an abandoned add, or `ccp remove --keep-credential` followed by
		// index reuse); left in place, FinalizeAdd would register the stale
		// credential. Probe by service alone (Discover), not a recomputed
		// account label — the item carries whatever -a label claude stored at
		// its login. SeedKeptExisting keeps it — the documented reuse path.
		account, err := m.Keychain.Discover(svc)
		switch {
		case errors.Is(err, keychain.ErrNotFound):
			// nothing to purge
		case err != nil:
			return nil, fmt.Errorf("probe stale credential for %s: %w", acctDir, err)
		default:
			if derr := m.Keychain.Delete(svc, account); derr != nil {
				return nil, fmt.Errorf("purge stale credential for %s: %w", acctDir, derr)
			}
			purged = true
		}
	}
	return &PendingAdd{
		Index:                 n,
		ConfigDir:             acctDir,
		KeychainService:       svc,
		OverlayKind:           kind,
		LoginCommand:          fmt.Sprintf("CLAUDE_CONFIG_DIR=%s claude /login", acctDir),
		ClaudeJSONSeed:        seed,
		PurgedStaleCredential: purged,
	}, nil
}

// FinalizeAdd is called after the user completes the interactive login. It
// confirms the credential landed, re-asserts ACL ownership, validates with one
// usage call, and records the account. label is an optional human note.
func (m *Manager) FinalizeAdd(ctx context.Context, p *PendingAdd, label string) (*store.Account, error) {
	account, err := keychain.DiscoverAccount(p.KeychainService)
	if errors.Is(err, keychain.ErrNotFound) {
		return nil, fmt.Errorf("no credential found for %s — was the login completed?", p.ConfigDir)
	} else if err != nil {
		return nil, err
	}

	// Re-assert: read the item Claude wrote and write it straight back so our
	// tooling owns the ACL for prompt-free refresh thereafter.
	if _, err := keychain.Reassert(p.KeychainService, account); err != nil {
		return nil, fmt.Errorf("re-assert keychain item: %w", err)
	}

	acct := store.Account{
		ID:              p.Index,
		ConfigDir:       p.ConfigDir,
		KeychainService: p.KeychainService,
		KeychainAccount: account,
		Label:           label,
		OverlayKind:     string(p.OverlayKind),
		CreatedAt:       time.Now(),
	}
	if err := m.Store.UpsertAccount(acct); err != nil {
		return nil, err
	}

	// Validate end-to-end with a single usage fetch (best-effort; a transient
	// failure here does not unwind the add).
	if _, _, err := m.SampleUsage(ctx, acct, true); err != nil {
		return &acct, fmt.Errorf("account added but usage validation failed: %w", err)
	}
	return &acct, nil
}

// AbandonAdd cleans up a prepared-but-not-finalized account dir (no store row
// exists at this stage), including any credential a completed-but-unfinalized
// login wrote to its Keychain item. The item's account label is discovered,
// not recomputed — claude wrote it. p must be a non-nil PendingAdd returned by
// PrepareAdd.
func (m *Manager) AbandonAdd(p *PendingAdd) error {
	var credErr error
	account, err := m.Keychain.Discover(p.KeychainService)
	switch {
	case errors.Is(err, keychain.ErrNotFound):
		// no credential to roll back
	case err != nil:
		credErr = fmt.Errorf("probe credential for %s: %w", p.ConfigDir, err)
	default:
		credErr = m.Keychain.Delete(p.KeychainService, account)
	}
	return errors.Join(credErr, m.removeAccountDir(overlay.For(p.OverlayKind), p.ConfigDir))
}

// Remove deletes an account from the pool: tears down its overlay, removes its
// Keychain item, and deletes its rows. ~/.claude is never touched (it is not
// an account).
func (m *Manager) Remove(id int, deleteCredential bool) error {
	a, err := m.Store.GetAccount(id)
	if err != nil {
		return err
	}
	prov := overlay.For(overlay.Kind(a.OverlayKind))
	if err := m.removeAccountDir(prov, a.ConfigDir); err != nil {
		return err
	}
	if deleteCredential {
		if err := m.Keychain.Delete(a.KeychainService, a.KeychainAccount); err != nil {
			return fmt.Errorf("delete keychain item %q: %w", a.KeychainService, err)
		}
	}
	return m.Store.DeleteAccount(id)
}

// removeAccountDir tears down an account dir's overlay and removes the dir and
// its private backing (when the provider keeps one beside the dir, as fuse
// does). Teardown refusing to operate (e.g. a wedged unmount) aborts the
// removal so we never RemoveAll through a live mount into the base.
func (m *Manager) removeAccountDir(prov overlay.Provider, configDir string) error {
	if err := prov.Teardown(ClaudeDir(), configDir); err != nil {
		return fmt.Errorf("teardown overlay: %w", err)
	}
	if err := os.RemoveAll(configDir); err != nil {
		return fmt.Errorf("remove account dir: %w", err)
	}
	if priv := prov.PrivateRoot(configDir); priv != configDir {
		if err := os.RemoveAll(priv); err != nil {
			return fmt.Errorf("remove private backing dir: %w", err)
		}
	}
	return nil
}

// SyncOverlay re-asserts an account's overlay so it reflects the current
// ~/.claude (the symlink provider links any new top-level entry; the fuse
// provider is a live mirror, so this just health-checks). Called at launch
// time and periodically by the daemon, which is why explicit `ccp sync` is
// unnecessary.
func (m *Manager) SyncOverlay(a store.Account) error {
	return overlay.For(overlay.Kind(a.OverlayKind)).Sync(ClaudeDir(), a.ConfigDir)
}

// ensureOverlayKind returns the overlay provider kind for new accounts: the
// kind recorded at init, detecting and recording one first if absent.
func (m *Manager) ensureOverlayKind() (overlay.Kind, error) {
	if v, ok, err := m.Store.GetMeta(metaOverlayKind); err != nil {
		return "", err
	} else if ok {
		return overlay.Kind(v), nil
	}
	kind := overlay.Detect()
	if err := m.Store.SetMeta(metaOverlayKind, string(kind)); err != nil {
		return "", err
	}
	return kind, nil
}
