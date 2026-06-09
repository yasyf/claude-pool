package pool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestDuplicateIdentity(t *testing.T) {
	st := openTestStore(t)
	m := &Manager{Store: st}

	mkAccount := func(t *testing.T, id int, uuid, email string) store.Account {
		t.Helper()
		dir := t.TempDir()
		if uuid != "" {
			body := `{"oauthAccount":{"accountUuid":"` + uuid + `","emailAddress":"` + email + `"}}`
			if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		a := store.Account{ID: id, ConfigDir: dir, KeychainService: keychain.ServiceName(dir), KeychainAccount: "user", OverlayKind: "symlink"}
		if err := st.UpsertAccount(a); err != nil {
			t.Fatal(err)
		}
		return a
	}

	a1 := mkAccount(t, 1, "u-1", "a@example.com")
	mkAccount(t, 2, "u-2", "b@example.com")

	t.Run("matches an already-pooled subscription", func(t *testing.T) {
		dup, err := m.DuplicateIdentity(Identity{AccountUUID: "u-1", EmailAddress: "a@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if dup == nil || dup.ID != a1.ID {
			t.Fatalf("DuplicateIdentity(u-1) = %+v, want acct %d", dup, a1.ID)
		}
	})

	t.Run("a new subscription returns nil", func(t *testing.T) {
		dup, err := m.DuplicateIdentity(Identity{AccountUUID: "u-3"})
		if err != nil {
			t.Fatal(err)
		}
		if dup != nil {
			t.Fatalf("DuplicateIdentity(u-3) = %+v, want nil", dup)
		}
	})

	t.Run("an account with no readable identity is skipped, not matched", func(t *testing.T) {
		mkAccount(t, 3, "", "") // no .claude.json
		dup, err := m.DuplicateIdentity(Identity{AccountUUID: "u-1"})
		if err != nil {
			t.Fatal(err)
		}
		if dup == nil || dup.ID != 1 {
			t.Fatalf("got %+v, want acct 1 (the broken acct must be skipped, not error)", dup)
		}
	})
}

func TestInitIdempotentAndMarker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := openTestStore(t)
	m := &Manager{Store: st}

	if ok, err := m.Initialized(); err != nil || ok {
		t.Fatalf("fresh manager Initialized() = %v err=%v, want false", ok, err)
	}
	res, err := m.Init()
	if err != nil {
		t.Fatal(err)
	}
	if res.Already {
		t.Fatal("first Init reported Already")
	}
	if res.OverlayKind == "" {
		t.Fatal("Init did not record an overlay kind")
	}
	if ok, _ := m.Initialized(); !ok {
		t.Fatal("Initialized() false after Init")
	}

	res2, err := m.Init()
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Already {
		t.Fatal("second Init did not report Already")
	}
	if res2.OverlayKind != res.OverlayKind {
		t.Fatalf("re-init flipped overlay kind %q -> %q", res.OverlayKind, res2.OverlayKind)
	}
}

// TestPrepareAddRepairsHalfAddedDir reconstructs the forensic state of an add
// that died at the onboarding wizard (overlay symlinks incl. a shared backups
// link, private daemon/ide dirs, a pre-login .claude.json stub) and proves the
// normal PrepareAdd flow repairs it in place: index reused, backups converted
// to a private dir, stub overwritten with the seeded config.
func TestPrepareAddRepairsHalfAddedDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Plain claude's state: ~/.claude with shared entries, ~/.claude.json.
	base := ClaudeDir()
	for _, d := range []string{"projects", "backups"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "backups", ".claude.json.backup.1"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ClaudeJSONPath(), []byte(`{"hasCompletedOnboarding": true, "oauthAccount": {"accountUuid": "main"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// The half-added acct-01 dir from a previous broken run.
	acct := AccountDir(1)
	if err := os.MkdirAll(acct, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"daemon", "ide"} {
		if err := os.MkdirAll(filepath.Join(acct, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "projects"), filepath.Join(acct, "projects")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "backups"), filepath.Join(acct, "backups")); err != nil {
		t.Fatal(err)
	}
	stub := `{"firstStartTime": "2026-06-06T07:57:05.707Z", "userID": "fresh"}`
	if err := os.WriteFile(filepath.Join(acct, ".claude.json"), []byte(stub), 0o600); err != nil {
		t.Fatal(err)
	}

	st := openTestStore(t)
	m := &Manager{Store: st, Keychain: newFakeKeychain()}
	if _, err := m.Init(); err != nil {
		t.Fatal(err)
	}
	pending, err := m.PrepareAdd()
	if err != nil {
		t.Fatal(err)
	}

	if pending.Index != 1 {
		t.Fatalf("index = %d, want 1 (no row exists, the dir is reused)", pending.Index)
	}
	if pending.ClaudeJSONSeed != SeedCopied {
		t.Fatalf("seed outcome = %q, want %q (stub must be overwritten)", pending.ClaudeJSONSeed, SeedCopied)
	}
	// backups: shared symlink converted to a private real dir; base untouched.
	fi, err := os.Lstat(filepath.Join(acct, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		t.Fatalf("backups not converted to a private dir (mode %v)", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(base, "backups", ".claude.json.backup.1")); err != nil {
		t.Fatalf("base backups damaged: %v", err)
	}
	// Seeded config: onboarding inherited, identity stripped.
	var seeded map[string]any
	if err := json.Unmarshal(readFile(t, filepath.Join(acct, ".claude.json")), &seeded); err != nil {
		t.Fatal(err)
	}
	if seeded["hasCompletedOnboarding"] != true {
		t.Fatalf("seeded config missing onboarding state: %v", seeded)
	}
	if _, ok := seeded["oauthAccount"]; ok {
		t.Fatal("seeded config leaked the main account's oauthAccount")
	}
}

// TestPrepareAddRequiresInit pins the defense-in-depth check behind add's
// auto-init.
func TestPrepareAddRequiresInit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := openTestStore(t)
	m := &Manager{Store: st}
	if _, err := m.PrepareAdd(); err == nil || err != ErrNotInitialized {
		t.Fatalf("PrepareAdd on fresh pool = %v, want ErrNotInitialized", err)
	}
}

// TestPrepareAddPurgesStaleKeychainItem pins the stale-item fix: an abandoned
// add (or `ccp remove --keep-credential`) can leave a credential under a
// service name a later add at the same index reuses; PrepareAdd must purge it
// (else the login watcher false-positives and FinalizeAdd registers a stale
// credential) — EXCEPT for SeedKeptExisting, the documented reuse path.
func TestPrepareAddPurgesStaleKeychainItem(t *testing.T) {
	setup := func(t *testing.T) (*Manager, *fakeKeychain, string) {
		t.Helper()
		t.Setenv("HOME", t.TempDir())
		t.Setenv("USER", "tester")
		if err := os.MkdirAll(ClaudeDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		fk := newFakeKeychain()
		m := &Manager{Store: openTestStore(t), Keychain: fk}
		if _, err := m.Init(); err != nil {
			t.Fatal(err)
		}
		svc := keychain.ServiceName(AccountDir(1))
		stale := &keychain.Credential{}
		stale.ClaudeAiOauth.AccessToken = "at-stale"
		if err := fk.Write(svc, "tester", stale); err != nil {
			t.Fatal(err)
		}
		return m, fk, svc
	}

	t.Run("fresh dir purges the leftover", func(t *testing.T) {
		m, fk, svc := setup(t)
		pending, err := m.PrepareAdd()
		if err != nil {
			t.Fatal(err)
		}
		if !pending.PurgedStaleCredential {
			t.Error("PurgedStaleCredential not reported")
		}
		if _, err := fk.Read(svc, "tester"); err != keychain.ErrNotFound {
			t.Errorf("stale item survived: %v", err)
		}
		if del := fk.deletedServices(); len(del) != 1 || del[0] != svc {
			t.Errorf("deletes = %v, want exactly [%q]", del, svc)
		}
	})

	t.Run("purges an item stored under a different -a label", func(t *testing.T) {
		// The stale item carries whatever label claude derived at ITS login
		// (USER drift, fallback label); the purge must discover it by service,
		// like every consumer does, not recompute today's label.
		m, fk, svc := setup(t)
		if err := fk.Delete(svc, "tester"); err != nil {
			t.Fatal(err)
		}
		stale := &keychain.Credential{}
		stale.ClaudeAiOauth.AccessToken = "at-stale"
		if err := fk.Write(svc, "someone-else", stale); err != nil {
			t.Fatal(err)
		}
		pending, err := m.PrepareAdd()
		if err != nil {
			t.Fatal(err)
		}
		if !pending.PurgedStaleCredential {
			t.Error("PurgedStaleCredential not reported")
		}
		if _, err := fk.Read(svc, "someone-else"); err != keychain.ErrNotFound {
			t.Errorf("label-mismatched stale item survived: %v", err)
		}
	})

	t.Run("kept-existing dir keeps the credential", func(t *testing.T) {
		m, fk, svc := setup(t)
		// A logged-in .claude.json from a prior attempt → SeedKeptExisting.
		acct := AccountDir(1)
		if err := os.MkdirAll(acct, 0o700); err != nil {
			t.Fatal(err)
		}
		loggedIn := `{"oauthAccount": {"accountUuid": "u-prior"}}`
		if err := os.WriteFile(filepath.Join(acct, ".claude.json"), []byte(loggedIn), 0o600); err != nil {
			t.Fatal(err)
		}
		pending, err := m.PrepareAdd()
		if err != nil {
			t.Fatal(err)
		}
		if pending.ClaudeJSONSeed != SeedKeptExisting {
			t.Fatalf("seed outcome = %q, want %q", pending.ClaudeJSONSeed, SeedKeptExisting)
		}
		if pending.PurgedStaleCredential {
			t.Error("reuse path must not purge")
		}
		if _, err := fk.Read(svc, "tester"); err != nil {
			t.Errorf("kept credential was purged: %v", err)
		}
	})
}

// TestFinalizeAddRequiresIdentity pins the anti-adoption gate: a credential can
// land in an account dir without a real login — with a fresh CLAUDE_CONFIG_DIR
// claude adopts the global session's secret into the suffixed Keychain item (or
// the plaintext .credentials.json headless over SSH) at startup, writing no
// oauthAccount. FinalizeAdd must refuse to register such an account rather than
// pool a copy of plain claude's login. The gate precedes any credential read,
// so this is hermetic (no Keychain, no network).
func TestFinalizeAddRequiresIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(ClaudeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Store: openTestStore(t), Keychain: newFakeKeychain()}
	if _, err := m.Init(); err != nil {
		t.Fatal(err)
	}
	pending, err := m.PrepareAdd() // no ~/.claude.json to seed → no identity
	if err != nil {
		t.Fatal(err)
	}
	acct, err := m.FinalizeAdd(context.Background(), pending, "")
	if acct != nil {
		t.Fatalf("FinalizeAdd returned acct %+v, want nil when no identity was written", acct)
	}
	if !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("FinalizeAdd err = %v, want ErrNoIdentity", err)
	}
}

// TestAbandonAddDeletesKeychainItem pins that rolling back a half-added
// account also rolls back the credential its login wrote.
func TestAbandonAddDeletesKeychainItem(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "tester")
	if err := os.MkdirAll(ClaudeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	fk := newFakeKeychain()
	m := &Manager{Store: openTestStore(t), Keychain: fk}
	if _, err := m.Init(); err != nil {
		t.Fatal(err)
	}
	pending, err := m.PrepareAdd()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the interactive login landing a credential — under a label that
	// differs from today's AccountLabel(), which the rollback must discover.
	cred := &keychain.Credential{}
	cred.ClaudeAiOauth.AccessToken = "at-login"
	if err := fk.Write(pending.KeychainService, "claude-wrote-this", cred); err != nil {
		t.Fatal(err)
	}

	if err := m.AbandonAdd(pending); err != nil {
		t.Fatalf("AbandonAdd: %v", err)
	}
	if _, err := fk.Read(pending.KeychainService, "claude-wrote-this"); err != keychain.ErrNotFound {
		t.Errorf("credential survived the rollback: %v", err)
	}
	if _, err := os.Stat(pending.ConfigDir); !os.IsNotExist(err) {
		t.Errorf("account dir survived the rollback: %v", err)
	}
}

// TestConcurrentPrepareAddIndexRace documents the known index-reservation gap:
// two concurrent PrepareAdds (no row until FinalizeAdd) are handed the same
// index and thus the same dir + derived Keychain service. Fixing it needs a
// pending-row reservation; until then this pins the current behavior so the
// decision stays explicit.
func TestConcurrentPrepareAddIndexRace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(ClaudeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	st := openTestStore(t)
	m := &Manager{Store: st, Keychain: newFakeKeychain()}
	if _, err := m.Init(); err != nil {
		t.Fatal(err)
	}
	p1, err := m.PrepareAdd()
	if err != nil {
		t.Fatal(err)
	}
	p2, err := m.PrepareAdd()
	if err != nil {
		t.Fatal(err)
	}
	if p1.Index != p2.Index {
		t.Fatalf("indexes %d vs %d — the reservation gap closed; update this test and the AGENTS.md follow-up note", p1.Index, p2.Index)
	}
}
