//go:build fuse && cgo && darwin

package pool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/store"
)

// TestFuseConvertRoundTrip runs the production symlink→fuse→symlink conversion
// against a real fuse-t mount in temp dirs — the CI form of the live rollout
// (and its rollback rehearsal). Requires fuse-t and the one-time "Network
// Volumes" grant; skips like TestFuseMirrorRoundTrip when a mount cannot come
// up.
func TestFuseConvertRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(base, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Probe first so a grant-less machine skips instead of failing.
	probeSrc, probeMnt := t.TempDir(), t.TempDir()
	if err := (&overlay.FuseProvider{}).Setup(probeSrc, probeMnt); err != nil {
		t.Skipf("fuse-t mount unavailable (acceptable; symlink is the default): %v", err)
	}
	if err := (&overlay.FuseProvider{}).Teardown(probeSrc, probeMnt); err != nil {
		t.Fatalf("probe teardown: %v", err)
	}

	dir := filepath.Join(home, "acct-01")
	if err := (&overlay.SymlinkProvider{}).Setup(base, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(identityJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backups", "b.bak"), []byte("bak"), 0o600); err != nil {
		t.Fatal(err)
	}

	st := openTestStore(t)
	a := store.Account{ID: 1, ConfigDir: dir, KeychainService: "svc", KeychainAccount: "user", OverlayKind: "symlink"}
	if err := st.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Store: st}

	fused, err := m.ConvertOverlay(a, overlay.KindFuse)
	if err != nil {
		t.Fatalf("convert to fuse: %v", err)
	}
	t.Cleanup(func() { _ = (&overlay.FuseProvider{}).Teardown(base, dir) })
	if fused.OverlayKind != "fuse" {
		t.Fatalf("row = %s, want fuse", fused.OverlayKind)
	}

	priv := overlay.FusePrivateRoot(dir)
	// Identity readable THROUGH the live mount, physically homed in .private.
	if got, err := os.ReadFile(filepath.Join(dir, ".claude.json")); err != nil || string(got) != identityJSON {
		t.Fatalf("identity through mount = %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(priv, ".claude.json")); err != nil || string(got) != identityJSON {
		t.Fatalf("identity in private root = %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "backups", "b.bak")); err != nil || string(got) != "bak" {
		t.Fatalf("backups through mount = %q err=%v", got, err)
	}
	// Shared entries serve from base through the mirror.
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatalf("shared entry not visible through mount: %v", err)
	}
	// Hazard canary: nothing leaked into the real base — no identity, and not
	// a single symlink (a symlink written through the mirror would land here).
	if _, err := os.Lstat(filepath.Join(base, ".claude.json")); !os.IsNotExist(err) {
		t.Fatal("identity leaked into base")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 {
			t.Fatalf("symlink %q leaked into base", e.Name())
		}
	}

	// Reverse — the rollout's rollback command.
	back, err := m.ConvertOverlay(fused, overlay.KindSymlink)
	if err != nil {
		t.Fatalf("convert back to symlink: %v", err)
	}
	if back.OverlayKind != "symlink" {
		t.Fatalf("row = %s, want symlink", back.OverlayKind)
	}
	if overlay.Mounted(dir) {
		t.Fatal("dir still mounted after reverse conversion")
	}
	if got, err := os.ReadFile(filepath.Join(dir, ".claude.json")); err != nil || string(got) != identityJSON {
		t.Fatalf("identity after reverse = %q err=%v", got, err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "projects")); err != nil || target != filepath.Join(base, "projects") {
		t.Fatalf("projects link after reverse = %q err=%v", target, err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "backups", "b.bak")); err != nil || string(got) != "bak" {
		t.Fatalf("backups after reverse = %q err=%v", got, err)
	}
}
