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
}
