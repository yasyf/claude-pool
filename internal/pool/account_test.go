package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
	m := &Manager{Store: st}
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
	m := &Manager{Store: st}
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
