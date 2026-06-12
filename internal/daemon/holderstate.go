package daemon

import (
	"sync"
	"time"

	"github.com/yasyf/cc-pool/internal/mountd"
	"github.com/yasyf/cc-pool/internal/version"
)

// holderRefreshFloor rate-limits select-path cache refreshes: a fuse row the
// cache cannot vouch for triggers at most one holder round-trip per floor, so
// a pool with a genuinely down mount cannot turn every select into holder
// RPCs.
const holderRefreshFloor = 5 * time.Second

// holderState is the daemon's cache of mount-holder truth: reachability, the
// holder's version, and per-dir liveness of every mount the holder owns. The
// select path keys fuse readiness on it instead of stat-ing through
// mountpoints — an lstat through a dead fuse-t NFS mount can hang — so it is
// primed at serve start, refreshed by the startup reconcile, once per
// scheduler poll, and once per supervision tick, updated in place after a
// successful mount, and lazily refreshed by mountReady when it cannot vouch
// for a fuse dir (see refreshIfStale). Respawn/backoff policy lives in
// superviseHolder; this is only the cache it keys on.
type holderState struct {
	mu          sync.Mutex
	healthy     bool
	version     string
	mounts      map[string]bool // dir -> Live, per the holder's last List
	refreshedAt time.Time
	// bases mirrors the holder's dir -> base registry from the last
	// successful List. Unlike mounts it SURVIVES markUnhealthy: it exists so
	// reviveHolder can remount a dead holder's pre-row mounts (`ccp add`'s
	// login window — no account row names the dir yet), and by the time the
	// revive runs the cache has already been marked unhealthy. Replaced
	// wholesale by the next successful refresh; a deliberate dismount
	// (noteUnmounted) drops its entry so a later revive cannot resurrect it.
	bases map[string]string
	// everMounted records that a holder served at least one mount at some
	// point in this daemon's lifetime. It survives markUnhealthy: a dead
	// holder may still be worth respawning for its orphaned mirrors even when
	// no fuse row remains in the store.
	everMounted bool
	// spawnErr is the daemon's latest failed holder-spawn attempt, surfaced
	// via HolderStatus; "" when the last spawn succeeded or none was needed.
	spawnErr string
	// tccErr is the latest mount-blocked-pending-TCC guidance (the holder's
	// "Network Volumes" grant walkthrough), kept for status/doctor rendering;
	// "" when no mount is TCC-blocked. Cleared by the next successful mount,
	// which proves the grant landed (the grant is per holder process, so one
	// live mount clears it for all).
	tccErr string

	// gen counts in-place cache mutations (noteMounted, noteUnmounted,
	// markUnhealthy). refresh snapshots it before its RPCs and discards the
	// polled snapshot if it changed by install time: an in-place update is
	// event truth newer than a List computed holder-side before the event, so
	// installing the snapshot over it would be a lost update — un-vouching a
	// live fresh mount (and rate-limit-suppressing mountReady's backstop for
	// the next floor), or re-vouching mirrors a replace just swept.
	gen uint64
}

// refresh polls the holder once (Health + List) and replaces the cache. Any
// failure marks the holder unhealthy and clears the mounts — a cache that
// cannot vouch for a dir must not let selection trust it. The RPCs run
// outside the lock; a snapshot raced by an in-place update is discarded (see
// gen).
func (h *holderState) refresh(c *mountd.Client) {
	h.mu.Lock()
	startGen := h.gen
	h.mu.Unlock()
	ver, err := c.Health()
	if err != nil {
		h.markUnhealthy()
		return
	}
	mounts, err := c.List()
	if err != nil {
		h.markUnhealthy()
		return
	}
	m := make(map[string]bool, len(mounts))
	b := make(map[string]string, len(mounts))
	for _, mi := range mounts {
		m[mi.Dir] = mi.Live
		b[mi.Dir] = mi.Base
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gen != startGen {
		// An in-place update landed while this snapshot was in flight; the
		// snapshot may predate it. Drop it — refreshedAt deliberately stays
		// put, so the next refreshIfStale re-polls promptly.
		return
	}
	h.healthy, h.version, h.mounts, h.bases, h.refreshedAt = true, ver, m, b, time.Now()
	if len(m) > 0 {
		h.everMounted = true
	}
}

// view snapshots holder reachability and the version it reported.
func (h *holderState) view() (healthy bool, version string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.healthy, h.version
}

// hadMounts reports whether a holder ever served a mount in this daemon's
// lifetime (survives markUnhealthy — see everMounted).
func (h *holderState) hadMounts() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.everMounted
}

// mountDirs returns every dir in the holder's last List, live or dead —
// kernel-truth coverage for the skew-replace idle gate, including mounts
// whose account rows no longer exist.
func (h *holderState) mountDirs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	dirs := make([]string, 0, len(h.mounts))
	for dir := range h.mounts {
		dirs = append(dirs, dir)
	}
	return dirs
}

// carriedBases snapshots the holder's dir -> base registry from its last
// successful List. It survives markUnhealthy by design (see bases): a revive
// reads it to remount the dead holder's pre-row mounts.
func (h *holderState) carriedBases() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	carry := make(map[string]string, len(h.bases))
	for dir, base := range h.bases {
		carry[dir] = base
	}
	return carry
}

// recordSpawnError keeps the latest holder-spawn failure for status
// rendering; "" clears it.
func (h *holderState) recordSpawnError(msg string) {
	h.mu.Lock()
	h.spawnErr = msg
	h.mu.Unlock()
}

// refreshIfStale runs one refresh iff the cache has never been refreshed or
// its last refresh is older than holderRefreshFloor. It is the select path's
// backstop for truth the poll cadence misses: a select racing the startup
// prime (the daemon socket binds before the startup goroutine runs) and a
// mount established outside the daemon (`ccp add` mounts from the CLI
// process). Bounded socket RPC only — never a filesystem touch, per
// mountReady's contract. The zero refreshedAt reads as maximally stale.
func (h *holderState) refreshIfStale(c *mountd.Client) {
	h.mu.Lock()
	fresh := time.Since(h.refreshedAt) < holderRefreshFloor
	h.mu.Unlock()
	if fresh {
		return
	}
	h.refresh(c)
}

// markUnhealthy records an unreachable holder: every mount entry is dropped
// and the version cleared — Version "" is the wire signal for unreachable.
// bases deliberately survives (see its doc): it is the only record of a dead
// holder's pre-row mounts, read by the revive that follows this very call.
func (h *holderState) markUnhealthy() {
	h.mu.Lock()
	h.gen++
	h.healthy, h.version, h.mounts, h.refreshedAt = false, "", nil, time.Now()
	h.mu.Unlock()
}

// ready reports whether the cache vouches for a live mirror at dir.
func (h *holderState) ready(dir string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.healthy && h.mounts[dir]
}

// noteMounted records a mirror the daemon just established or adopted without
// waiting for the next refresh, so a select landing in between trusts it. It
// vouches for holder health too — a successful Setup proves a live mirror
// serves the dir — and clears any recorded TCC guidance; the next refresh
// restores polled truth.
func (h *holderState) noteMounted(dir string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen++
	h.healthy = true
	if h.mounts == nil {
		h.mounts = map[string]bool{}
	}
	h.mounts[dir] = true
	h.everMounted = true
	h.tccErr = ""
}

// noteUnmounted drops a dir the daemon just dismounted (a fuse→symlink
// conversion or fallback) without waiting for the next refresh, so neither
// selection nor HolderStatus.Mounts keeps vouching for a mirror that no
// longer exists; the next refresh restores polled truth.
func (h *holderState) noteUnmounted(dir string) {
	h.mu.Lock()
	h.gen++
	delete(h.mounts, dir)
	delete(h.bases, dir)
	h.mu.Unlock()
}

// recordTCC keeps the latest TCC-blocked mount guidance for status rendering.
func (h *holderState) recordTCC(msg string) {
	h.mu.Lock()
	h.tccErr = msg
	h.mu.Unlock()
}

// wireStatus snapshots the cache as the status op's HolderStatus. Version ""
// means the holder was unreachable at the last refresh (or a fresh mount was
// trusted via noteMounted before any refresh succeeded); Skewed is asserted
// only against a version actually reported by a healthy holder.
func (h *holderState) wireStatus() *HolderStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	live := 0
	for _, ok := range h.mounts {
		if ok {
			live++
		}
	}
	return &HolderStatus{
		Version:    h.version,
		Mounts:     live,
		Skewed:     h.healthy && h.version != "" && h.version != version.String(),
		TCCError:   h.tccErr,
		SpawnError: h.spawnErr,
	}
}
