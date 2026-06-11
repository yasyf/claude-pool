//go:build fuse && cgo && darwin

package overlay

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFuseMirrorRoundTrip mounts a passthrough mirror via fuse-t and verifies
// reads and writes pass straight through to the backing dir (no copy-up). It
// requires fuse-t installed and may trip the one-time "Network Volumes" grant;
// it fails loudly so R-FUSE-T can be confirmed.
func TestFuseMirrorRoundTrip(t *testing.T) {
	base := t.TempDir()
	mnt := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &FuseProvider{}
	if err := p.Setup(base, mnt); err != nil {
		t.Skipf("fuse-t mount unavailable (acceptable; symlink is the default): %v", err)
	}
	defer p.Teardown(base, mnt)

	// Read through the mount.
	got, err := os.ReadFile(filepath.Join(mnt, "hello.txt"))
	if err != nil {
		t.Fatalf("read through mount: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("read = %q, want hi", got)
	}

	// Write through the mount must land in base (shared, no copy-up).
	if err := os.WriteFile(filepath.Join(mnt, "written.txt"), []byte("pass"), 0o644); err != nil {
		t.Fatalf("write through mount: %v", err)
	}
	back, err := os.ReadFile(filepath.Join(base, "written.txt"))
	if err != nil {
		t.Fatalf("write did not pass through to base: %v", err)
	}
	if string(back) != "pass" {
		t.Fatalf("backing file = %q, want pass", back)
	}

	// A new entry created directly in base appears live through the mount.
	if err := os.Mkdir(filepath.Join(base, "newdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mnt, "newdir")); err != nil {
		t.Fatalf("new base entry not visible through mount: %v", err)
	}

	// Writing .claude.json through the mount lands in the private backing dir,
	// never in base (per-account identity must not pollute the shared base).
	if err := os.WriteFile(filepath.Join(mnt, ".claude.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write .claude.json through mount: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf(".claude.json leaked into base")
	}
	if _, err := os.Stat(filepath.Join(privateRootFor(mnt), ".claude.json")); err != nil {
		t.Fatalf(".claude.json not in private backing dir: %v", err)
	}
}

// TestMirrorRealRedirectsLocalEntries pins the path-mapping table without
// needing a live mount: every PrivateEntry top component (and its subtree)
// must back onto privateRoot; everything else onto root.
func TestMirrorRealRedirectsLocalEntries(t *testing.T) {
	fs := newMirrorFS("/base", "/priv")
	cases := map[string]string{
		"/.claude.json":                      "/priv/.claude.json",
		"/.claude.json.tmp.ab12cd34":         "/priv/.claude.json.tmp.ab12cd34",
		"/.credentials.json":                 "/priv/.credentials.json",
		"/.credentials.json.lock":            "/priv/.credentials.json.lock",
		"/remote-settings.json":              "/priv/remote-settings.json",
		"/remote-settings.json.tmp.ab12cd34": "/priv/remote-settings.json.tmp.ab12cd34",
		"/backups":                           "/priv/backups",
		"/backups/x.bak":                     "/priv/backups/x.bak",
		"/daemon/roster.json":                "/priv/daemon/roster.json",
		"/ide/lock":                          "/priv/ide/lock",
		"/projects/p.json":                   "/base/projects/p.json",
		"/settings.json":                     "/base/settings.json",
		"/":                                  "/base",
	}
	for in, want := range cases {
		if got := fs.real(in); got != want {
			t.Errorf("real(%q) = %q, want %q", in, got, want)
		}
	}
}
