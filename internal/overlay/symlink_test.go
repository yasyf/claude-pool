package overlay

import (
	"os"
	"path/filepath"
	"testing"
)

func makeBase(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	for _, d := range []string{"projects", "skills", "daemon", "ide"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "settings.json"), []byte("{}"), 0o644); err != nil {
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
	for _, name := range []string{"daemon", "ide"} {
		fi, err := os.Lstat(filepath.Join(acct, name))
		if err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s should be a private dir, not a symlink", name)
		}
	}

	// .DS_Store is skipped entirely.
	if _, err := os.Lstat(filepath.Join(acct, ".DS_Store")); !os.IsNotExist(err) {
		t.Errorf(".DS_Store should be skipped")
	}

	if err := p.Health(base, acct); err != nil {
		t.Fatalf("Health after setup: %v", err)
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
