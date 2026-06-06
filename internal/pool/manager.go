package pool

import (
	"fmt"

	"github.com/yasyf/cc-pool/internal/oauth"
	"github.com/yasyf/cc-pool/internal/store"
)

// Manager is the high-level façade over the store, the OAuth client, and the
// Keychain/overlay machinery. CLI commands, the TUI wizards, and the daemon all
// go through it.
type Manager struct {
	Store      *store.Store
	OAuth      *oauth.Client
	DefaultDir string // ~/.claude (acct-00)
}

// Open ensures the state dir exists, opens the database, and returns a Manager.
func Open() (*Manager, error) {
	if err := EnsureStateDir(); err != nil {
		return nil, fmt.Errorf("ensure state dir: %w", err)
	}
	st, err := store.Open(DBPath())
	if err != nil {
		return nil, err
	}
	return &Manager{
		Store:      st,
		OAuth:      oauth.New(),
		DefaultDir: ClaudeDir(),
	}, nil
}

// Close releases resources.
func (m *Manager) Close() error {
	if m.Store != nil {
		return m.Store.Close()
	}
	return nil
}

// Initialized reports whether `clp init` has registered acct-00 yet.
func (m *Manager) Initialized() (bool, error) {
	accts, err := m.Store.ListAccounts()
	if err != nil {
		return false, err
	}
	for _, a := range accts {
		if a.IsZero {
			return true, nil
		}
	}
	return false, nil
}
