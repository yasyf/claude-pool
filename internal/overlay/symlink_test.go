package overlay

import (
	"os"
	"path/filepath"
	"testing"
)

func makeBase(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	for _, d := range []string{"projects", "skills", "daemon", "ide", "backups"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A backup in base must never become visible to accounts.
	if err := os.WriteFile(filepath.Join(base, "backups", "seed.bak"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A .claude.json in base (private file) must never be linked into accounts.
	if err := os.WriteFile(filepath.Join(base, ".claude.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".DS_Store"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestSymlinkSetupSharesAndExcludes(t *testing.T) {
	base := makeBase(t)
	acct := filepath.Join(t.TempDir(), "acct-01")
	p := &SymlinkProvider{}
	if err := p.Setup(base, acct); err != nil {
		t.Fatal(err)
	}

	// Shared entries are symlinks pointing back into base.
	for _, name := range []string{"projects", "skills", "settings.json"} {
		target, err := os.Readlink(filepath.Join(acct, name))
		if err != nil {
			t.Fatalf("%s not a symlink: %v", name, err)
		}
		if target != filepath.Join(base, name) {
			t.Errorf("%s -> %q, want %q", name, target, filepath.Join(base, name))
		}
	}

	// Excluded entries are private real dirs (not symlinks).
	for _, name := range []string{"daemon", "ide", "backups"} {
		fi, err := os.Lstat(filepath.Join(acct, name))
		if err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s should be a private dir, not a symlink", name)
		}
	}

	// The private backups dir starts empty: base's backups never leak in.
	if _, err := os.Stat(filepath.Join(acct, "backups", "seed.bak")); !os.IsNotExist(err) {
		t.Errorf("base backup leaked into the account's private backups dir")
	}

	// .DS_Store is skipped entirely.
	if _, err := os.Lstat(filepath.Join(acct, ".DS_Store")); !os.IsNotExist(err) {
		t.Errorf(".DS_Store should be skipped")
	}

	// Base's .claude.json (a private file) is never linked into the account.
	if _, err := os.Lstat(filepath.Join(acct, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf("base .claude.json should not be linked into the account dir")
	}

	if err := p.Health(base, acct); err != nil {
		t.Fatalf("Health after setup: %v", err)
	}
}

// TestBackupsIsPrivatePerAccount pins the pollution regression: a write into
// the account's backups dir must never appear in base's backups.
func TestBackupsIsPrivatePerAccount(t *testing.T) {
	base := makeBase(t)
	acct := filepath.Join(t.TempDir(), "acct-01")
	p := &SymlinkProvider{}
	if err := p.Setup(base, acct); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(acct, "backups", ".claude.json.backup.1"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "backups", ".claude.json.backup.1")); !os.IsNotExist(err) {
		t.Fatalf("account backup leaked into base backups dir")
	}
}

// TestSyncRepairsBackupsSymlink covers accounts created before backups was
// excluded: Sync must convert the shared symlink into a private real dir.
func TestSyncRepairsBackupsSymlink(t *testing.T) {
	base := makeBase(t)
	acct := filepath.Join(t.TempDir(), "acct-01")
	if err := os.MkdirAll(acct, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "backups"), filepath.Join(acct, "backups")); err != nil {
		t.Fatal(err)
	}
	p := &SymlinkProvider{}
	if err := p.Health(base, acct); err == nil {
		t.Fatal("Health must fail while backups is a shared symlink")
	}
	if err := p.Sync(base, acct); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(acct, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		t.Fatalf("backups not converted to a private dir (mode %v)", fi.Mode())
	}
	// Base's backups dir (and its contents) survive the conversion.
	if _, err := os.Stat(filepath.Join(base, "backups", "seed.bak")); err != nil {
		t.Fatalf("base backups damaged by conversion: %v", err)
	}
}

// TestPrivateEntry pins the private-name predicate, including claude's
// atomic-write temp files.
func TestPrivateEntry(t *testing.T) {
	cases := map[string]bool{
		".claude.json":              true,
		".claude.json.tmp.ab12cd34": true,
		".claude.json.backup.123":   true,
		"daemon":                    true,
		"ide":                       true,
		"backups":                   true,
		"projects":                  false,
		"settings.json":             false,
		".claude":                   false,
		"claude.json":               false,
	}
	for name, want := range cases {
		if got := PrivateEntry(name); got != want {
			t.Errorf("PrivateEntry(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestWriteThroughSymlinkLandsInBase(t *testing.T) {
	base := makeBase(t)
	acct := filepath.Join(t.TempDir(), "acct-01")
	p := &SymlinkProvider{}
	if err := p.Setup(base, acct); err != nil {
		t.Fatal(err)
	}
	// Writing through the account's projects symlink must land in base.
	if err := os.WriteFile(filepath.Join(acct, "projects", "x.json"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "projects", "x.json")); err != nil {
		t.Fatalf("write did not pass through to base: %v", err)
	}
}

func TestSyncPicksUpNewEntry(t *testing.T) {
	base := makeBase(t)
	acct := filepath.Join(t.TempDir(), "acct-01")
	p := &SymlinkProvider{}
	if err := p.Setup(base, acct); err != nil {
		t.Fatal(err)
	}
	// New top-level entry appears in base after setup.
	if err := os.MkdirAll(filepath.Join(base, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.Health(base, acct); err == nil {
		t.Fatal("Health should report missing link for new entry")
	}
	if err := p.Sync(base, acct); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(filepath.Join(acct, "plugins")); err != nil {
		t.Fatalf("Sync did not link new entry: %v", err)
	}
}

func TestTeardownRemovesOverlayNotBase(t *testing.T) {
	base := makeBase(t)
	acct := filepath.Join(t.TempDir(), "acct-01")
	p := &SymlinkProvider{}
	if err := p.Setup(base, acct); err != nil {
		t.Fatal(err)
	}
	if err := p.Teardown(base, acct); err != nil {
		t.Fatal(err)
	}
	// Base content survives.
	if _, err := os.Stat(filepath.Join(base, "settings.json")); err != nil {
		t.Fatalf("base content destroyed: %v", err)
	}
	// Account links are gone.
	if _, err := os.Lstat(filepath.Join(acct, "projects")); !os.IsNotExist(err) {
		t.Errorf("overlay link not removed")
	}
}

func TestTeardownRefusesBase(t *testing.T) {
	base := makeBase(t)
	p := &SymlinkProvider{}
	if err := p.Teardown(base, base); err == nil {
		t.Fatal("Teardown must refuse to operate on base")
	}
}

// TestSetupAndSyncRefuseBase pins the same guard for the mutating paths:
// overlaying base onto itself would replace the user's real ~/.claude entries
// with self-referential symlinks.
func TestSetupAndSyncRefuseBase(t *testing.T) {
	base := makeBase(t)
	p := &SymlinkProvider{}
	if err := p.Setup(base, base); err == nil {
		t.Fatal("Setup must refuse to overlay base onto itself")
	}
	if err := p.Sync(base, base); err == nil {
		t.Fatal("Sync must refuse to overlay base onto itself")
	}
	if err := p.Sync(base, ""); err == nil {
		t.Fatal("Sync must refuse an empty account dir")
	}
	// The refusal must come BEFORE any mutation: base's entries are intact.
	for _, name := range []string{"projects", "settings.json", "backups"} {
		fi, err := os.Lstat(filepath.Join(base, name))
		if err != nil {
			t.Fatalf("base entry %s damaged: %v", name, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("base entry %s replaced with a symlink", name)
		}
	}
}
