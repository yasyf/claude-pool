//go:build fuse && cgo && darwin

// Package overlay's fuse provider: an in-process passthrough MIRROR of
// ~/.claude mounted at an account dir via fuse-t (kext-less, NFS-over-loopback,
// mounted as the user without root). A single backing dir means writes pass
// straight through to ~/.claude and are shared live — no copy-up.
//
// cgofuse drives fuse-t natively (it dlopens /usr/local/lib/libfuse-t.dylib).
// Build with: CGO_ENABLED=1 go build -tags fuse ./...
//
// Mounts are hosted in-process and block while serving, so the daemon owns
// their lifecycle (it calls Setup at startup and Teardown at shutdown). A
// short-lived CLI invocation cannot host a mount; for those, detection falls
// back to symlink until the daemon establishes the mount.
package overlay

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/winfsp/cgofuse/fuse"
)

const fuseBuilt = true

func fuseProvider() (Provider, bool) { return &FuseProvider{}, true }

func init() {
	// This machine (and many dev Macs) may have BOTH macFUSE's libfuse.2.dylib
	// and fuse-t's libfuse-t.dylib. cgofuse dlopens libfuse.2.dylib first, so
	// pin fuse-t explicitly unless the user already set the override.
	if os.Getenv("CGOFUSE_LIBFUSE_PATH") == "" {
		_ = os.Setenv("CGOFUSE_LIBFUSE_PATH", "/usr/local/lib/libfuse-t.dylib")
	}
}

// privateRootFor is the per-account backing dir holding the excluded
// (instance-local) subtrees daemon/ and ide/. It sits beside the mountpoint.
func privateRootFor(accountDir string) string {
	return accountDir + ".private"
}

// mountRegistry tracks live mounts so Teardown can unmount the right host.
var (
	mountMu sync.Mutex
	mounts  = map[string]*mountHandle{}
)

type mountHandle struct {
	host *fuse.FileSystemHost
	done chan struct{}
}

// FuseProvider mounts a passthrough mirror of base at the account dir.
type FuseProvider struct{}

func (p *FuseProvider) Kind() Kind { return KindFuse }

// PrivateRoot is the per-account backing dir beside the mountpoint. Private
// files written there are visible through the mount (mirrorFS redirects
// PrivateEntry names) and survive whether or not the mount is currently up.
func (p *FuseProvider) PrivateRoot(accountDir string) string {
	return privateRootFor(accountDir)
}

// Setup mounts a passthrough mirror of base at accountDir. It blocks only until
// the mount is live (or a timeout). The serving loop runs in a goroutine.
// Like Teardown, it refuses to operate on base itself — mounting over
// ~/.claude would shadow the user's real config dir.
func (p *FuseProvider) Setup(base, accountDir string) error {
	if accountDir == base || accountDir == "" {
		return fmt.Errorf("refusing to mount over base dir %q", accountDir)
	}
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		return err
	}
	mountMu.Lock()
	if _, ok := mounts[accountDir]; ok {
		mountMu.Unlock()
		return nil // already mounted
	}
	mountMu.Unlock()

	// Private backing for excluded (instance-local) entries.
	priv := privateRootFor(accountDir)
	for name := range ExcludedEntries {
		_ = os.MkdirAll(filepath.Join(priv, name), 0o700)
	}

	fs := newMirrorFS(base, priv)
	host := fuse.NewFileSystemHost(fs)
	host.SetCapReaddirPlus(true)
	done := make(chan struct{})

	// fuse-t mount options (its NFS backend has NO soft/timeout/retrans knobs;
	// the coherence lever is noattrcache). The backing ~/.claude is written
	// directly by plain `claude` while a pooled session reads through this
	// mirror, so disable attribute caching to avoid stale reads. nobrowse
	// keeps the mount out of Finder sidebars.
	opts := []string{
		"-o", "volname=cc-pool-" + filepath.Base(accountDir),
		"-o", "noattrcache",
		"-o", "nobrowse",
	}
	go func() {
		defer close(done)
		// Mount blocks until unmounted. ok=false means the mount failed.
		_ = host.Mount(accountDir, opts)
	}()

	if !waitMounted(base, accountDir, 8*time.Second) {
		host.Unmount()
		// Bounded wait: a mount stuck on the one-time "Network Volumes" TCC
		// grant must not hang the daemon. Fall back to symlink instead.
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		return fmt.Errorf("fuse mount of %s did not come up (grant Network Volumes access once in System Settings ▸ Privacy, then retry; symlink is used until then)", accountDir)
	}
	mountMu.Lock()
	mounts[accountDir] = &mountHandle{host: host, done: done}
	mountMu.Unlock()
	return nil
}

// Sync is a no-op for fuse: the mirror reflects base live, including new
// entries, so there is nothing to re-assert beyond confirming health.
func (p *FuseProvider) Sync(base, accountDir string) error {
	return p.Health(base, accountDir)
}

// Health verifies the mount is live by stat-ing a known entry through it.
func (p *FuseProvider) Health(base, accountDir string) error {
	if !mountLive(base, accountDir) {
		return fmt.Errorf("fuse mount at %s is not live", accountDir)
	}
	return nil
}

// Teardown unmounts the account dir's mirror.
func (p *FuseProvider) Teardown(base, accountDir string) error {
	if accountDir == base || accountDir == "" {
		return fmt.Errorf("refusing to tear down base dir %q", accountDir)
	}
	mountMu.Lock()
	h, ok := mounts[accountDir]
	delete(mounts, accountDir)
	mountMu.Unlock()
	if !ok {
		// Not ours (e.g. left over from a prior run): best-effort unmount.
		_ = syscall.Unmount(accountDir, 0)
	} else {
		h.host.Unmount()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
		}
	}
	// Honest teardown: confirm the path is no longer a mountpoint. If the
	// unmount wedged (e.g. fuse-t issue-45), report it so callers do NOT
	// RemoveAll through a live mount into the backing ~/.claude.
	if stillMounted(accountDir) {
		return fmt.Errorf("unmount of %s did not take; refusing to treat it as torn down", accountDir)
	}
	return nil
}

// stillMounted reports whether dir is a mountpoint (its device differs from its
// parent's).
func stillMounted(dir string) bool {
	var ds, ps syscall.Stat_t
	if syscall.Lstat(dir, &ds) != nil {
		return false
	}
	if syscall.Lstat(filepath.Dir(dir), &ps) != nil {
		return false
	}
	return ds.Dev != ps.Dev
}

// waitMounted polls until base's contents are visible through accountDir.
func waitMounted(base, accountDir string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if mountLive(base, accountDir) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// mountLive reports whether accountDir currently mirrors base. It compares a
// stat of base itself (always exists) seen through the mountpoint.
func mountLive(base, accountDir string) bool {
	fi, err := os.Stat(accountDir)
	if err != nil || !fi.IsDir() {
		return false
	}
	// The mount is "live" if the dir is backed by a fuse fs; a cheap proxy is
	// that reading it does not error and base's own entries are visible.
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) == 0 {
		return err == nil
	}
	_, err = os.Lstat(filepath.Join(accountDir, entries[0].Name()))
	return err == nil
}

// probeFuse attempts a throwaway mount to confirm fuse-t works on this machine
// (and trips the one-time "Network Volumes" privacy grant). Used by Detect.
func probeFuse() bool {
	tmp, err := os.MkdirTemp("", "ccp-fuse-probe-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(tmp)
	src := filepath.Join(tmp, "src")
	mnt := filepath.Join(tmp, "mnt")
	_ = os.MkdirAll(src, 0o700)
	_ = os.MkdirAll(mnt, 0o700)
	_ = os.WriteFile(filepath.Join(src, "probe"), []byte("ok"), 0o600)

	p := &FuseProvider{}
	if err := p.Setup(src, mnt); err != nil {
		return false
	}
	defer p.Teardown(src, mnt)
	_, err = os.Stat(filepath.Join(mnt, "probe"))
	return err == nil
}
