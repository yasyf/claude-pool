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

// ErrNotLoggedIn means plain `claude` has no stored credential, so there is
// nothing to register as acct-00.
var ErrNotLoggedIn = errors.New("no Claude credential found — run `claude` and log in first")

// InitResult summarizes what `clp init` did.
type InitResult struct {
	OverlayKind   overlay.Kind
	MirrorService string
	Account       store.Account
}

// Init registers ~/.claude as acct-00 without moving it. It confirms a
// credential exists, mirrors that credential into a suffixed Keychain item so
// acct-00 is launchable via CLAUDE_CONFIG_DIR=~/.claude, picks an overlay
// provider, and records the account. Idempotent.
func (m *Manager) Init(ctx context.Context) (*InitResult, error) {
	if err := EnsureStateDir(); err != nil {
		return nil, err
	}
	if err := EnsureAccountsDir(); err != nil {
		return nil, err
	}

	defaultSvc := keychain.DefaultServiceName()
	account, err := keychain.DiscoverAccount(defaultSvc)
	if errors.Is(err, keychain.ErrNotFound) {
		return nil, ErrNotLoggedIn
	} else if err != nil {
		return nil, fmt.Errorf("discover default keychain item: %w", err)
	}

	// Read the canonical credential and mirror it into the suffixed item keyed
	// on the literal path clp will emit for acct-00.
	canonical, err := keychain.Read(defaultSvc, account)
	if err != nil {
		return nil, fmt.Errorf("read default credential: %w", err)
	}
	mirrorSvc := keychain.ServiceName(ClaudeDir())
	if err := keychain.Write(mirrorSvc, account, canonical); err != nil {
		return nil, fmt.Errorf("write acct-00 mirror item: %w", err)
	}

	kind := overlay.Detect()
	acct := store.Account{
		ID:              AcctZero,
		ConfigDir:       ClaudeDir(),
		KeychainService: mirrorSvc,
		KeychainAccount: account,
		Label:           "acct-00 (~/.claude)",
		OverlayKind:     string(kind),
		IsZero:          true,
		CreatedAt:       time.Now(),
	}
	if err := m.Store.UpsertAccount(acct); err != nil {
		return nil, err
	}
	return &InitResult{OverlayKind: kind, MirrorService: mirrorSvc, Account: acct}, nil
}

// PendingAdd describes a half-created account awaiting interactive login.
type PendingAdd struct {
	Index           int
	ConfigDir       string
	KeychainService string
	OverlayKind     overlay.Kind
	LoginCommand    string
}

// PrepareAdd allocates the next account dir and establishes its overlay, then
// returns the login command the user must run. No account row or Keychain item
// is created yet — FinalizeAdd does that once the login lands.
func (m *Manager) PrepareAdd() (*PendingAdd, error) {
	ok, err := m.Initialized()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("run `clp init` first")
	}
	n, err := m.Store.NextAccountIndex()
	if err != nil {
		return nil, err
	}
	acctDir := AccountDir(n)
	kind := m.overlayKind()
	prov := overlay.For(kind)
	if err := prov.Setup(ClaudeDir(), acctDir); err != nil {
		return nil, fmt.Errorf("set up overlay for %s: %w", acctDir, err)
	}
	svc := keychain.ServiceName(acctDir)
	return &PendingAdd{
		Index:           n,
		ConfigDir:       acctDir,
		KeychainService: svc,
		OverlayKind:     kind,
		LoginCommand:    fmt.Sprintf("CLAUDE_CONFIG_DIR=%s claude /login", acctDir),
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
		IsZero:          false,
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

// AbandonAdd cleans up a prepared-but-not-finalized account dir. p must be a
// non-nil PendingAdd returned by PrepareAdd.
func (m *Manager) AbandonAdd(p *PendingAdd) error {
	if p.Index == AcctZero {
		return nil
	}
	prov := overlay.For(p.OverlayKind)
	if err := prov.Teardown(ClaudeDir(), p.ConfigDir); err != nil {
		return err
	}
	return os.RemoveAll(p.ConfigDir)
}

// Remove deletes an account from the pool: tears down its overlay, removes its
// suffixed Keychain item, and deletes its rows. acct-00's canonical ~/.claude
// and its default Keychain item are NEVER touched (only its mirror item).
func (m *Manager) Remove(id int, deleteCredential bool) error {
	a, err := m.Store.GetAccount(id)
	if err != nil {
		return err
	}
	if a.IsZero {
		// Only remove the mirror item; never the canonical default item or dir.
		_ = keychain.Delete(a.KeychainService, a.KeychainAccount)
		return m.Store.DeleteAccount(id)
	}
	prov := overlay.For(overlay.Kind(a.OverlayKind))
	if err := prov.Teardown(ClaudeDir(), a.ConfigDir); err != nil {
		return fmt.Errorf("teardown overlay: %w", err)
	}
	if err := os.RemoveAll(a.ConfigDir); err != nil {
		return fmt.Errorf("remove account dir: %w", err)
	}
	if deleteCredential {
		_ = keychain.Delete(a.KeychainService, a.KeychainAccount)
	}
	return m.Store.DeleteAccount(id)
}

// SyncOverlay re-asserts an account's overlay so it reflects the current
// ~/.claude (the symlink provider links any new top-level entry; the fuse
// provider is a live mirror, so this just health-checks). acct-00 IS the base,
// so it is a no-op. Called at launch time and periodically by the daemon, which
// is why explicit `clp sync` is unnecessary.
func (m *Manager) SyncOverlay(a store.Account) error {
	if a.IsZero {
		return nil
	}
	return overlay.For(overlay.Kind(a.OverlayKind)).Sync(ClaudeDir(), a.ConfigDir)
}

// overlayKind returns the provider kind to use for new accounts, taken from
// acct-00's recorded kind (set at init), defaulting to a fresh detection.
func (m *Manager) overlayKind() overlay.Kind {
	if a, err := m.Store.GetAccount(AcctZero); err == nil && a.OverlayKind != "" {
		return overlay.Kind(a.OverlayKind)
	}
	return overlay.Detect()
}
