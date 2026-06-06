package pool

import (
	"context"
	"fmt"
	"sync"

	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/oauth"
	"github.com/yasyf/cc-pool/internal/store"
)

// Refresher is the slice of *oauth.Client the Manager needs: token refresh and
// usage sampling. Consumer-defined so tests can fake provider behavior.
type Refresher interface {
	Refresh(ctx context.Context, flightKey, refreshToken string) (*oauth.TokenResponse, error)
	Usage(ctx context.Context, accessToken string) (*oauth.Usage, error)
}

// CredentialStore is the slice of package keychain the Manager needs for
// credential reads and writes.
type CredentialStore interface {
	Read(service, account string) (*keychain.Credential, error)
	Write(service, account string, cred *keychain.Credential) error
}

// sysKeychain adapts package keychain's process-global functions to
// CredentialStore.
type sysKeychain struct{}

func (sysKeychain) Read(service, account string) (*keychain.Credential, error) {
	return keychain.Read(service, account)
}

func (sysKeychain) Write(service, account string, cred *keychain.Credential) error {
	return keychain.Write(service, account, cred)
}

// Manager is the high-level façade over the store, the OAuth client, and the
// Keychain/overlay machinery. CLI commands, the TUI wizards, and the daemon all
// go through it.
type Manager struct {
	Store      *store.Store
	OAuth      Refresher
	Keychain   CredentialStore
	DefaultDir string // ~/.claude (acct-00)

	// muMap guards locks; locks holds one mutex per account ID serializing that
	// account's credential read→refresh→write cycle. These per-account mutexes
	// are DELIBERATELY held across Keychain and OAuth I/O — a documented
	// exception to the no-locks-across-I/O rule: concurrent mutation of one
	// account's single-use refresh token is never safe (double-spend gets
	// invalid_grant; a stale write-back clobbers the rotated token), so
	// serializing the whole cycle is the point. muMap itself is only ever held
	// for the map access, never across an op.
	muMap sync.Mutex
	locks map[int]*sync.Mutex
}

// acctLock returns the mutex serializing credential operations for an account.
func (m *Manager) acctLock(id int) *sync.Mutex {
	m.muMap.Lock()
	defer m.muMap.Unlock()
	if m.locks == nil {
		m.locks = map[int]*sync.Mutex{}
	}
	if m.locks[id] == nil {
		m.locks[id] = &sync.Mutex{}
	}
	return m.locks[id]
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
		Keychain:   sysKeychain{},
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
