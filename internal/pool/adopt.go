package pool

import (
	"context"
	"errors"
	"fmt"

	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/store"
)

// AdoptCandidate describes plain claude's current login when it is eligible
// for adoption into the pool.
type AdoptCandidate struct {
	Identity Identity
}

// AdoptCandidate returns plain claude's current login when ~/.claude.json
// holds an identity, the canonical Keychain item exists, and no pool account
// already holds the same accountUuid. (nil, nil) when there is nothing to
// offer. The existence check is attribute-only — the canonical secret is not
// read before the user consents to adoption. Pool accounts whose identity
// cannot be read are treated as non-matching: adoption can then rarely
// duplicate an already-pooled identity with a drifted .claude.json, but
// blocking every add on one broken dir would be worse.
func (m *Manager) AdoptCandidate() (*AdoptCandidate, error) {
	id, err := CanonicalIdentity()
	if errors.Is(err, ErrNoIdentity) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if !m.Canonical.CanonicalExists() {
		return nil, nil
	}
	accounts, err := m.Store.ListAccounts()
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		aid, err := AccountIdentity(overlay.Kind(a.OverlayKind), a.ConfigDir)
		if err != nil {
			continue // unreadable identity counts as non-matching
		}
		if aid.AccountUUID == id.AccountUUID {
			return nil, nil // already pooled
		}
	}
	return &AdoptCandidate{Identity: *id}, nil
}

// AdoptCredential adopts plain claude's current login into the pending
// account: read the canonical item (the only secret read, read-only) → copy it
// into the account's own suffixed item → write the identity into the account's
// private .claude.json → immediately refresh the copy so the account runs on
// its own fresh token chain. Plain claude's credential is never written or
// deleted and is unaffected throughout.
//
// want is the candidate the user consented to: if plain claude's login changed
// while the confirm prompt sat on screen (a different accountUuid is now
// current), nothing is copied and an error is returned — consent and the
// candidate-time dedup check are both bound to that identity.
//
// On any failure the pending account is rolled back to its pre-adopt state
// (suffixed item deleted, identity stripped) and the error returned; the
// caller falls back to interactive login.
func (m *Manager) AdoptCredential(ctx context.Context, p *PendingAdd, want Identity) error {
	mu := m.acctLock(p.Index)
	mu.Lock()
	defer mu.Unlock()

	// Re-read both fresh: the user may have sat on the confirm prompt for a
	// while, and the candidate snapshot could be stale.
	id, err := CanonicalIdentity()
	if err != nil {
		return fmt.Errorf("read current login identity: %w", err)
	}
	if id.AccountUUID != want.AccountUUID {
		return fmt.Errorf("current login is now %s, not the %s you confirmed",
			id.EmailAddress, want.EmailAddress)
	}
	cred, err := m.Canonical.ReadCanonical()
	if err != nil {
		return fmt.Errorf("read current login credential: %w", err)
	}

	account := keychain.AccountLabel()
	if err := m.Keychain.Write(p.KeychainService, account, cred); err != nil {
		return fmt.Errorf("copy credential to %q: %w", p.KeychainService, err)
	}

	prov := overlay.For(p.OverlayKind)
	if err := writeIdentity(prov, p.ConfigDir, id); err != nil {
		return errors.Join(
			fmt.Errorf("write adopted identity: %w", err),
			m.rollbackAdopt(p, prov, account),
		)
	}

	// Refresh the copy so the account immediately runs on its own fresh chain.
	// This predates the account row, so it cannot appear in refresh_log.
	a := store.Account{
		ID:              p.Index,
		ConfigDir:       p.ConfigDir,
		KeychainService: p.KeychainService,
		KeychainAccount: account,
	}
	if _, err := m.refresh(ctx, a, cred); err != nil {
		return errors.Join(
			fmt.Errorf("refresh adopted credential: %w", err),
			m.rollbackAdopt(p, prov, account),
		)
	}
	return nil
}

// rollbackAdopt returns the pending account to its pre-adopt state: suffixed
// item deleted, identity stripped. Errors are returned (joined by the caller)
// rather than swallowed — a lingering suffixed item would be silently
// finalized by a later add at the same index.
func (m *Manager) rollbackAdopt(p *PendingAdd, prov overlay.Provider, account string) error {
	return errors.Join(
		m.Keychain.Delete(p.KeychainService, account),
		stripIdentity(prov, p.ConfigDir),
	)
}
