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
// credential reads, writes, and deletes. Discover resolves the account (-a)
// label actually stored on a service's item — items written by `claude /login`
// carry whatever label claude derived at the time, which may not match a label
// recomputed later, so deletions of claude-written items must discover first.
type CredentialStore interface {
	Read(service, account string) (*keychain.Credential, error)
	Write(service, account string, cred *keychain.Credential) error
	Delete(service, account string) error
	Discover(service string) (string, error)
}

// CanonicalReader is the read-only seam onto plain claude's canonical
// unsuffixed Keychain item, used solely by `clp add` adoption. It is kept
// separate from CredentialStore on purpose: the seam has no write or delete
// methods, so mutating the canonical item is impossible to express through it
// (safety rule 1's read-only exception, enforced by the type system).
type CanonicalReader interface {
	CanonicalExists() bool
	ReadCanonical() (*keychain.Credential, error)
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

func (sysKeychain) Delete(service, account string) error {
	return keychain.Delete(service, account)
}

func (sysKeychain) Discover(service string) (string, error) {
	return keychain.DiscoverAccount(service)
}

// sysCanonical adapts package keychain's canonical accessors to
// CanonicalReader.
type sysCanonical struct{}

func (sysCanonical) CanonicalExists() bool {
	return keychain.CanonicalExists()
}

func (sysCanonical) ReadCanonical() (*keychain.Credential, error) {
	return keychain.ReadCanonical()
}

// Manager is the high-level façade over the store, the OAuth client, and the
// Keychain/overlay machinery. CLI commands, the TUI wizards, and the daemon all
// go through it.
type Manager struct {
	Store     *store.Store
	OAuth     Refresher
	Keychain  CredentialStore
	Canonical CanonicalReader

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
		Store:     st,
		OAuth:     oauth.New(),
		Keychain:  sysKeychain{},
		Canonical: sysCanonical{},
	}, nil
}

// Close releases resources.
func (m *Manager) Close() error {
	if m.Store != nil {
		return m.Store.Close()
	}
	return nil
}

// Meta keys recording pool-level state in the store's meta table.
const (
	// metaInitialized marks that the pool was set up via `clp init` (or add's
	// auto-init) — deliberately distinct from "the DB file exists", which any
	// read-only command creates as a side effect of opening the Manager.
	metaInitialized = "initialized"
	// metaOverlayKind records the overlay provider chosen at init, so new
	// accounts keep using it and a re-init never flips providers under live
	// accounts.
	metaOverlayKind = "overlay_kind"
)

// Initialized reports whether the pool has been set up (`clp init` or `clp
// add`'s auto-init).
func (m *Manager) Initialized() (bool, error) {
	_, ok, err := m.Store.GetMeta(metaInitialized)
	if err != nil {
		return false, err
	}
	return ok, nil
}
