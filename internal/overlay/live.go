package overlay

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// This file holds untagged mount-liveness checks. They compile in every build
// variant: a non-fuse binary (and, once the mount-holder lands, the process
// that does not host mounts) must still be able to observe whether an account
// dir is a live mirror of base.

// StatProbes bounds wedge-prone kernel stats. fuse-t's NFS backend has no
// soft/timeout mount options, so a stat through a wedged mirror can block
// indefinitely; Do runs each stat in its own goroutine behind a timeout, and
// concurrent callers for the same key JOIN the in-flight probe (sharing its
// verdict when it answers in time) rather than stacking another stuck
// goroutine per caller. The probe goroutine's exit is the stat returning; for
// a truly wedged mount that is never — exactly the condition the bound exists
// to contain, and why the goroutine is deliberately untracked.
type StatProbes[V any] struct {
	mu       sync.Mutex
	inflight map[string]*statProbe[V]
}

// statProbe is one in-flight stat; v is valid once done closes.
type statProbe[V any] struct {
	done chan struct{}
	v    V
}

// Do runs stat keyed by key, returning its verdict and ok=true, or the zero V
// and ok=false when it does not answer within timeout. The caller chooses the
// fail direction for a timed-out probe: liveness checks read dead, teardown
// verifications read still-mounted.
func (p *StatProbes[V]) Do(key string, timeout time.Duration, stat func() V) (V, bool) {
	p.mu.Lock()
	pr, ok := p.inflight[key]
	if !ok {
		if p.inflight == nil {
			p.inflight = map[string]*statProbe[V]{}
		}
		pr = &statProbe[V]{done: make(chan struct{})}
		p.inflight[key] = pr
		go func() {
			v := stat()
			p.mu.Lock()
			pr.v = v
			delete(p.inflight, key)
			p.mu.Unlock()
			close(pr.done)
		}()
	}
	p.mu.Unlock()
	select {
	case <-pr.done:
		return pr.v, true
	case <-time.After(timeout):
		var zero V
		return zero, false
	}
}

// Inflight reports the probes currently running. Tests drain wedged probes
// against it before restoring the stat seams the probe goroutines read.
func (p *StatProbes[V]) Inflight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inflight)
}

// statProbeTimeout bounds the overlay package's own wedge-prone kernel stats
// (FuseProvider.Teardown's post-unmount verification). A var, not a const, so
// tests can shrink it.
var statProbeTimeout = 2 * time.Second

// ownProbes joins the overlay package's own bounded mountpoint stats.
var ownProbes StatProbes[bool]

// MountedWithin reports Mounted(dir) bounded by the package's stat-probe
// timeout; ok=false means the stat did not answer within the bound (a wedged
// mirror) and the caller must fail toward its safe direction — selection
// reads not-ready, sweeps are skipped, teardown verification reads
// still-mounted.
func MountedWithin(dir string) (mounted, ok bool) {
	return ownProbes.Do(dir, statProbeTimeout, func() bool { return Mounted(dir) })
}

// MountAlive reports whether accountDir currently mirrors base. It compares a
// stat of base itself (always exists) seen through the mountpoint.
func MountAlive(base, accountDir string) bool {
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

// waitMounted polls until base's contents are visible through accountDir.
func waitMounted(base, accountDir string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if MountAlive(base, accountDir) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
