package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yasyf/cc-pool/internal/mountd"
	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/peerpid"
	"github.com/yasyf/cc-pool/internal/procscan"
	"github.com/yasyf/cc-pool/internal/store"
	"github.com/yasyf/cc-pool/internal/version"
)

const (
	// defaultSuperviseInterval is the holder supervision cadence: a crashed
	// holder is respawned and its mirrors remounted in ~10s instead of the
	// scheduler's ~3.5-minute poll.
	defaultSuperviseInterval = 10 * time.Second

	// spawnBackoffBase/spawnBackoffCap bound the respawn backoff: consecutive
	// spawn failures double the wait 10s → 20s → … capped at 10 minutes.
	spawnBackoffBase = 10 * time.Second
	spawnBackoffCap  = 10 * time.Minute

	// remountBackoffBase/remountBackoffCap bound the per-row remount backoff
	// for fuse rows the holder cannot vouch for (see retryUnvouchedFuseRows):
	// consecutive failed heals double the wait 10s → 20s → … capped at 2
	// minutes. The cap sits deliberately under the 180s scheduler period —
	// supervision must never be the slower recovery path.
	remountBackoffBase = 10 * time.Second
	remountBackoffCap  = 2 * time.Minute

	// defaultHolderGoneWait bounds waiting for a retiring holder to release
	// its socket after acking Shutdown — the holder's own sweep runs under a
	// 60s op deadline and the client's Shutdown timeout is 65s, so this sits
	// just above both.
	defaultHolderGoneWait = 70 * time.Second
)

// backoffAfter returns the wait after `failures` consecutive failures: base
// doubling per failure, capped at limit. Zero or negative failure counts
// never shrink below base.
func backoffAfter(failures int, base, limit time.Duration) time.Duration {
	d := base
	for i := 1; i < failures && d < limit; i++ {
		d *= 2
	}
	if d > limit {
		d = limit
	}
	return d
}

// spawnBackoff returns the wait after `failures` consecutive holder-spawn
// failures: spawnBackoffBase doubling per failure, capped at spawnBackoffCap.
func spawnBackoff(failures int) time.Duration {
	return backoffAfter(failures, spawnBackoffBase, spawnBackoffCap)
}

// supervisor is superviseHolder's tick-local state: respawn backoff
// bookkeeping and the last-logged conditions backing once-per-transition
// logging. Only the supervise goroutine touches it — no lock.
type supervisor struct {
	sawUnhealthy bool      // last tick found the holder unreachable
	failures     int       // consecutive spawn failures
	retryAt      time.Time // backoff: earliest next spawn attempt
	lastSpawnErr string    // last logged spawn-failure text
	lastDefer    string    // last logged skew-replace deferral reason
	// spawnedSkew is the version the last daemon-initiated spawn actually
	// produced when it differs from ours (see noteSpawnedVersion): the
	// reverse-skew steady state superviseTick must never re-replace.
	spawnedSkew string
	// rowRetry is the per-account remount backoff ledger for fuse rows the
	// holder cannot vouch for (see retryUnvouchedFuseRows). Lazily
	// initialized; like the rest of supervisor, only the supervise goroutine
	// touches it — no lock.
	rowRetry map[int]rowRetryState
}

// rowRetryState is one fuse row's remount-backoff bookkeeping in
// supervisor.rowRetry.
type rowRetryState struct {
	failures int       // consecutive failed heal attempts
	retryAt  time.Time // backoff: earliest next heal attempt
}

// superviseHolder watches the detached mount holder until ctx is cancelled:
// it respawns a dead holder (under exponential backoff) and remounts the fuse
// rows the fresh holder does not serve, and replaces a version-skewed holder
// when — and only when — the pool is provably idle. Started after the startup
// reconcile so it never races the initial mounts.
func (s *Server) superviseHolder(ctx context.Context) {
	interval := s.superviseInterval
	if interval <= 0 {
		interval = defaultSuperviseInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.superviseTick(ctx)
		}
	}
}

// superviseTick runs one supervision pass. Split from the loop so tests drive
// ticks deterministically.
func (s *Server) superviseTick(ctx context.Context) {
	s.holder.refresh(s.holderClient())
	healthy, ver := s.holder.view()
	if !healthy {
		s.reviveHolder(ctx)
		return
	}
	if s.sup.sawUnhealthy {
		s.sup.sawUnhealthy = false
		s.log.Printf("mount holder reachable again (%s)", ver)
	}
	if ver == "" {
		// Healthy but version unknown: noteMounted vouches without a version,
		// and this tick's refresh can be discarded by the cache's gen race.
		// Not skew evidence — wireStatus's Skewed guard agrees — so never
		// launch a replace on it; the next refresh restores polled truth.
		return
	}
	if ver == version.String() || ver == s.sup.spawnedSkew {
		// Steady state: healthy at our version, or at the version our own
		// spawns produce (a newer on-disk binary under an older daemon — see
		// noteSpawnedVersion; replacing would mint the same version again,
		// sweeping every mirror each tick forever). Reset the backoff, clear
		// any stale spawn-error/deferral surface, and retry whatever fuse
		// rows this healthy holder still cannot vouch for.
		s.resetSpawnBackoff()
		s.sup.lastDefer = ""
		s.retryUnvouchedFuseRows(ctx)
		return
	}
	s.replaceSkewedHolder(ctx)
}

// reviveHolder handles an unreachable holder: when fuse rows (or mounts a
// holder previously served) need one and this build can spawn it, respawn
// under exponential backoff, then remount whatever the fresh holder does not
// serve — fuse rows AND the dead holder's pre-row mounts (`ccp add`'s login
// window) — closing the crash→remount loop at the supervision cadence.
func (s *Server) reviveHolder(ctx context.Context) {
	if !s.sup.sawUnhealthy {
		s.sup.sawUnhealthy = true
		s.log.Printf("mount holder unreachable")
	}
	accts, err := s.m.Store.ListAccounts()
	if err != nil {
		s.log.Printf("holder supervision: list accounts: %v", err)
		return
	}
	fuse := make([]store.Account, 0, len(accts))
	rowDirs := make(map[string]bool, len(accts))
	for _, a := range accts {
		rowDirs[a.ConfigDir] = true
		if a.OverlayKind == string(overlay.KindFuse) {
			fuse = append(fuse, a)
		}
	}
	if len(fuse) == 0 && !s.holder.hadMounts() {
		return // nothing for a holder to serve
	}
	if !s.canSpawnHolder() {
		// A pure build cannot spawn a holder; attempting would refuse with
		// the same error every tick. The transition above already logged.
		return
	}
	if time.Now().Before(s.sup.retryAt) {
		return
	}
	// Snapshot the dead holder's dir->base registry BEFORE the spawn:
	// verifySpawnedHolder's refresh installs the fresh holder's (empty) List,
	// after which the pre-row mounts' bases are gone from the cache.
	carry := s.holder.carriedBases()
	if err := s.spawn(); err != nil {
		s.noteSpawnFailure(err)
		return
	}
	if !s.verifySpawnedHolder() {
		return
	}
	s.sup.sawUnhealthy = false
	s.log.Printf("mount holder respawned")
	s.remountFuseRows(ctx, fuse)
	s.remountCarriedDirs(ctx, rowDirs, carry)
}

// verifySpawnedHolder re-polls the holder after a spawn that reported success
// and confirms it actually answers health checks. A socket held open by an
// unresponsive process defeats EnsureRunning's Available() short-circuit — the
// spawn "succeeds" without spawning anything — so success is only believed
// when the refreshed cache vouches for it; otherwise the attempt is booked as
// a failure (engaging the backoff and the once-per-error-text logging) and
// the unhealthy transition state is left untouched. A genuine success resets
// the backoff, clears any surfaced spawn error, and records the version the
// spawn actually produced (see noteSpawnedVersion).
func (s *Server) verifySpawnedHolder() bool {
	s.holder.refresh(s.holderClient())
	healthy, ver := s.holder.view()
	if !healthy {
		s.noteSpawnFailure(fmt.Errorf("spawn reported success but the holder on %s failed its health check (socket held by an unresponsive process?)", s.holderSocket))
		return false
	}
	s.resetSpawnBackoff()
	s.noteSpawnedVersion(ver)
	return true
}

// noteSpawnedVersion records the version a daemon-initiated spawn actually
// produced. The spawn execs the binary at the holder's install path, which a
// brew upgrade may have swapped under this still-running daemon — so a fresh
// holder can legitimately report a NEWER version than ours (the reverse of
// the skew the replace exists for). That version is this daemon's steady
// state, not grounds for another replace: re-replacing would exec the same
// binary, observe the same skew, and sweep+remount every mirror each idle
// tick forever. superviseTick treats a holder at this version as settled;
// Skewed stays surfaced on the status wire, and the log (once per distinct
// version) says how to converge.
func (s *Server) noteSpawnedVersion(ver string) {
	switch {
	case ver == "":
		// Unknown: the verification refresh was discarded by an in-place
		// cache update. The next tick's refresh restores polled truth.
	case ver == version.String():
		s.sup.spawnedSkew = ""
	case ver != s.sup.spawnedSkew:
		s.sup.spawnedSkew = ver
		s.log.Printf("spawned mount holder reports %s but this daemon is %s (binary swapped by an upgrade?); serving as-is — restart the daemon to converge", ver, version.String())
	}
}

// replaceSkewedHolder retires a healthy holder running a different build and
// remounts every fuse row on a fresh holder at our version. This is the ONLY
// path that ever stops a serving holder besides `ccp service uninstall`, so
// it runs solely when the pool is provably idle (skewReplaceGate) — and the
// gate returns with the replace claims HELD on every fuse row, so from
// gate-clear through the old holder's mount sweep and the remount below, no
// select can reserve a fuse dir and no conversion can begin (the sweep runs
// before the Shutdown reply lands; without the claims that whole window would
// be live). A blocked leg defers to a later tick, logged once per reason,
// with Skewed already surfaced on the status wire. The replace is ctx-aware
// between steps and inside the WaitGone legs so a daemon shutdown never
// stalls behind it; no Server mutex is held across the Shutdown/WaitGone
// RPCs. On any step failing, the claims are released and the state is left
// for the next tick — the gate re-runs. An errored Shutdown RPC is treated as
// outcome-unknown, not as nothing-happened: the sweep runs before the reply,
// so the cache stops vouching and the gone-wait runs with the claims still
// held — a holder observed gone continues the replace; one still serving
// defers (never killed: it may never have received the Shutdown).
func (s *Server) replaceSkewedHolder(ctx context.Context) {
	_, ver := s.holder.view()
	fuse, reason := s.skewReplaceGate()
	if reason != "" {
		if reason != s.sup.lastDefer {
			s.sup.lastDefer = reason
			s.log.Printf("deferring replacement of version-skewed mount holder (%s): %s", ver, reason)
		}
		return
	}
	defer s.endReplace(accountIDs(fuse))
	s.sup.lastDefer = ""
	if ctx.Err() != nil {
		return
	}
	// Capture the holder's identity before asking it to exit: if it wedges,
	// the kill must land only on this exact process — never on a successor
	// (e.g. a CLI Setup-spawned holder) that bound the socket in between.
	oldPID, pidErr := s.peerPIDOf(s.holderSocket)
	s.log.Printf("replacing version-skewed mount holder (%s) with %s", ver, version.String())
	cl := s.holderClient()
	failed, shutdownErr := cl.Shutdown()
	// The holder sweeps its mounts BEFORE the Shutdown reply lands, so an
	// errored RPC (a blown client deadline mid-sweep, a wire blip after
	// delivery) is not proof the sweep did not run: nothing may serve the
	// mirrors until the remount below, so the cache must stop vouching either
	// way, and the replace claims must stay held across any in-flight sweep —
	// wait the holder out before deciding anything.
	s.holder.markUnhealthy()
	if shutdownErr == nil && len(failed) > 0 {
		s.log.Printf("skewed holder reported %d dir(s) that would not unmount", len(failed))
	}
	wait := s.holderGoneWait
	if wait <= 0 {
		wait = defaultHolderGoneWait
	}
	switch gone := cl.WaitGoneContext(ctx, wait); {
	case ctx.Err() != nil && !gone:
		s.log.Printf("daemon shutting down mid-replace; the next daemon's reconcile finishes the job")
		return
	case !gone && shutdownErr != nil:
		// No ack and still serving: the holder may never have received the
		// Shutdown at all, and a healthy serving holder is never killed.
		// Defer; the next tick's refresh restores cache truth and re-gates.
		s.log.Printf("skewed holder shutdown: %v; holder still serving, retrying next tick", shutdownErr)
		return
	case !gone:
		if !s.reapWedgedHolder(ctx, cl, oldPID, pidErr, wait) {
			return
		}
	case shutdownErr != nil:
		s.log.Printf("skewed holder shutdown errored (%v) but the holder released its socket; continuing the replace", shutdownErr)
	}
	if ctx.Err() != nil {
		return
	}
	if err := s.spawn(); err != nil {
		s.noteSpawnFailure(err)
		return
	}
	if !s.verifySpawnedHolder() {
		return
	}
	s.remountReplacedRows(ctx, fuse)
	s.log.Printf("mount holder replaced at %s", version.String())
}

// reapWedgedHolder handles a holder that acked Shutdown but kept its socket
// past the gone-wait: the SIGKILL escape hatch, deliberately loud — and gated
// on peer identity. The old holder exiting and a fresh holder binding inside
// one of WaitGone's probe gaps is indistinguishable from a wedge at the
// socket level, so the kill goes through peerpid.KillPid: the socket's
// current peer is resolved and compared against the pid captured at gate
// time INSIDE one dial, and the signal lands on that same resolved pid — a
// successor that bound the socket in between is refused, never shot. An
// unverifiable or changed peer defers to the next tick (which re-assesses
// whoever holds the socket then). Reports whether the socket is now free for
// the successor spawn.
func (s *Server) reapWedgedHolder(ctx context.Context, cl *mountd.Client, oldPID int, pidErr error, wait time.Duration) bool {
	if pidErr != nil {
		s.log.Printf("skewed holder wedged after shutdown, but its pid was not captured at gate time (%v); not killing; retrying next tick", pidErr)
		return false
	}
	pid, kerr := s.killPeerPid(s.holderSocket, oldPID)
	switch {
	case errors.Is(kerr, peerpid.ErrUnreachable):
		// Released between WaitGone's last probe and now — nothing to kill.
		return true
	case kerr != nil:
		s.log.Printf("skewed holder wedged after shutdown; kill socket peer: %v; retrying next tick", kerr)
		return false
	}
	s.log.Printf("skewed holder wedged after shutdown; killed socket peer pid %d", pid)
	if !cl.WaitGoneContext(ctx, wait) {
		s.log.Printf("skewed holder still owns %s after the kill; retrying next tick", s.holderSocket)
		return false
	}
	return true
}

// skewReplaceGate evaluates every leg of the idle gate, returning the fuse
// rows for the remount and the first blocking reason — "" means clear, and
// means the replace claims are HELD on every fuse row (the caller must
// endReplace). The legs, in claim-first order (the convertAccount pattern:
// claim, then scan — so nothing can land between a clean scan and the
// holder's sweep):
//   - this build must be able to spawn the replacement: never stop a holder
//     we cannot succeed;
//   - daemon uptime ≥ reservationTTL: a freshly-started daemon's reservation
//     map is empty while a ≤30s-old select may not have exec'd its claude yet;
//   - beginReplace claims every fuse row — refusing on any live reservation,
//     any mid-poll account, or ANY in-flight conversion (a symlink→fuse
//     migrate is about to Mount through the holder being retired; its row
//     only flips at the end, so it is invisible to per-fuse-row checks);
//   - the fuse set is re-listed under the claims: a conversion that completed
//     between the listing and the claim could have flipped a row either way,
//     leaving an unclaimed fuse row (selectable mid-sweep) or a claimed
//     symlink row; a changed set defers one tick;
//   - the session scan must succeed — fail closed;
//   - every dir in the holder's List must have an account row: a pre-row dir
//     is a `ccp add` mid-login (its row lands at FinalizeAdd), invisible to
//     the claims, and the sweep would yank its mirror under the login;
//   - zero sessions on every fuse row's dir AND every dir in the holder's
//     List — kernel truth, covering mounts whose rows were deleted while a
//     teardown was refused.
func (s *Server) skewReplaceGate() (fuse []store.Account, reason string) {
	if !s.canSpawnHolder() {
		return nil, "this build cannot spawn a replacement holder (no fuse support)"
	}
	if up := time.Since(s.startedAt); up < reservationTTL {
		return nil, fmt.Sprintf("daemon up only %s; a pre-restart select may not be visible yet", up.Round(time.Second))
	}
	fuse, err := s.fuseAccounts()
	if err != nil {
		return nil, fmt.Sprintf("list accounts: %v", err)
	}
	ids := accountIDs(fuse)
	if reason := s.beginReplace(ids); reason != "" {
		return nil, reason
	}
	bail := func(why string) ([]store.Account, string) {
		s.endReplace(ids)
		return nil, why
	}
	fresh, err := s.fuseAccounts()
	if err != nil {
		return bail(fmt.Sprintf("re-list accounts: %v", err))
	}
	if !sameAccountIDs(fuse, fresh) {
		return bail("the fuse account set changed mid-gate")
	}
	sessions, err := s.scan()
	if err != nil {
		return bail(fmt.Sprintf("session scan: %v", err))
	}
	all, err := s.m.Store.ListAccounts()
	if err != nil {
		return bail(fmt.Sprintf("list account dirs: %v", err))
	}
	rowDirs := make(map[string]bool, len(all))
	for _, a := range all {
		rowDirs[a.ConfigDir] = true
	}
	dirs := make(map[string]bool, len(fuse))
	for _, a := range fuse {
		dirs[a.ConfigDir] = true
	}
	for _, dir := range s.holder.mountDirs() {
		// A holder-served dir with no account row is a `ccp add` mid-login:
		// the row only lands at FinalizeAdd, so the replace claims cannot see
		// it and the sweep would yank the mirror under the login. Defer.
		if !rowDirs[dir] {
			return bail(fmt.Sprintf("holder serves %s with no account row (an add may be in flight)", dir))
		}
		dirs[dir] = true
	}
	for dir := range dirs {
		if n := procscan.CountByConfigDir(sessions, dir); n > 0 {
			return bail(fmt.Sprintf("%d live session(s) on %s", n, dir))
		}
	}
	return fuse, ""
}

// remountFuseRows heals every fuse row the holder cache cannot vouch for,
// each under the scheduler's poll-claim discipline so supervision never races
// a poll or conversion on the same account. A claimed account is skipped, not
// raced — its owner leaves it consistent, and a later revive or poll
// re-checks. The row is re-read under the claim (the caller's list aged
// across the spawn I/O) so a row converted in the gap is left alone. Used by
// reviveHolder; the skew replace remounts under its own claims instead (see
// remountReplacedRows).
func (s *Server) remountFuseRows(ctx context.Context, accts []store.Account) {
	for _, a := range accts {
		if ctx.Err() != nil {
			return
		}
		if s.holder.ready(a.ConfigDir) {
			continue
		}
		if !s.beginPoll(a.ID) {
			s.log.Printf("acct-%02d busy; deferring its remount to the next supervision tick", a.ID)
			continue
		}
		fresh, err := s.m.Store.GetAccount(a.ID)
		switch {
		case err != nil:
			s.log.Printf("acct-%02d re-read row before remount: %v", a.ID, err)
		case fresh.OverlayKind == string(overlay.KindFuse):
			s.healFuse(fresh)
		}
		s.endPoll(a.ID)
	}
}

// retryUnvouchedFuseRows is the steady-state heal loop: on every supervision
// tick with a healthy settled holder, each fuse row the holder cache cannot
// vouch for — a remount that failed during a revive, a TCC-blocked row
// waiting on its grant, or a mirror the holder reports present-but-dead,
// deep-wedged or plain dead — is retried under per-account exponential backoff
// (remountBackoffBase doubling to remountBackoffCap, deliberately under the
// 180s scheduler period). Each attempt runs under the scheduler's poll-claim
// discipline: a claimed account is skipped WITHOUT advancing its backoff —
// skipping is not failing — and its row is re-read under the claim so a row
// converted in the gap is left alone. A successful heal (by this loop or
// anyone else — ready() covers mounts established by any path) deletes the
// row's ledger entry; rows that left the fuse set are pruned after the pass.
// reviveHolder's one-shot remountFuseRows stays separate: a revived holder's
// rows get one immediate sweep there, and the first steady tick afterwards
// lands here ~10s later for anything it could not finish.
func (s *Server) retryUnvouchedFuseRows(ctx context.Context) {
	fuse, err := s.fuseAccounts()
	if err != nil {
		s.log.Printf("holder supervision: list accounts: %v", err)
		return
	}
	// One session scan per pass, taken lazily on the first held-dead row —
	// the count only flavors the held-dead log line.
	var sessions []procscan.Session
	scanned := false
	now := time.Now()
	inPass := make(map[int]bool, len(fuse))
	for _, a := range fuse {
		inPass[a.ID] = true
		if ctx.Err() != nil {
			return
		}
		if s.holder.ready(a.ConfigDir) {
			delete(s.sup.rowRetry, a.ID)
			continue
		}
		if now.Before(s.sup.rowRetry[a.ID].retryAt) {
			continue
		}
		if !s.beginPoll(a.ID) {
			continue // skip-don't-race; the owner leaves it consistent
		}
		fresh, err := s.m.Store.GetAccount(a.ID)
		switch {
		case err != nil:
			s.log.Printf("acct-%02d re-read row before remount: %v", a.ID, err)
			s.advanceRowBackoff(a.ID)
		case fresh.OverlayKind != string(overlay.KindFuse):
			// Converted while this pass's listing aged: its owner left it
			// consistent, and a non-fuse row needs no remount ledger.
			delete(s.sup.rowRetry, a.ID)
		default:
			if dead, wedged := s.holder.heldDead(a.ConfigDir); dead {
				if !scanned {
					scanned = true
					ses, serr := s.scan()
					if serr != nil {
						s.log.Printf("holder supervision: session scan: %v", serr)
					}
					sessions = ses
				}
				// The holder's deep-probe verdict picks the copy: a deep wedge
				// serves metadata but hangs reads, while a plain-dead registered
				// mirror (an out-of-band `umount -f`, a dead fuse-t worker, or
				// an old holder that cannot deep-probe) fails reads outright.
				// The relaunch guidance holds in both shapes — sessions on the
				// old mirror are orphaned by the remount either way.
				desc := "dead mirror (fails reads outright; unmounted out of band or its fuse worker died?)"
				if wedged {
					desc = "wedged mirror (serves metadata but hangs reads)"
				}
				s.log.Printf("acct-%02d %s; remounting under %d live session(s) — relaunch them",
					a.ID, desc, procscan.CountByConfigDir(sessions, a.ConfigDir))
			}
			if s.healFuse(fresh) == healMounted {
				delete(s.sup.rowRetry, a.ID)
			} else {
				s.advanceRowBackoff(a.ID)
			}
		}
		s.endPoll(a.ID)
	}
	for id := range s.sup.rowRetry {
		if !inPass[id] {
			delete(s.sup.rowRetry, id)
		}
	}
}

// advanceRowBackoff books one failed heal attempt against account id's
// remount ledger: the failure count grows and the next attempt waits out the
// doubled window.
func (s *Server) advanceRowBackoff(id int) {
	if s.sup.rowRetry == nil {
		s.sup.rowRetry = make(map[int]rowRetryState)
	}
	st := s.sup.rowRetry[id]
	st.failures++
	st.retryAt = time.Now().Add(backoffAfter(st.failures, remountBackoffBase, remountBackoffCap))
	s.sup.rowRetry[id] = st
}

// remountReplacedRows heals every fuse row after a holder replacement, under
// the replace's already-held converting claims — never beginPoll: those
// claims, taken at gate time, are what kept selects and conversions off these
// dirs through the old holder's sweep, and they hold until the caller's
// endReplace. The rows are stable under them (the gate verified the set and
// the replacing fence blocks new conversions), so no re-read is needed.
func (s *Server) remountReplacedRows(ctx context.Context, accts []store.Account) {
	for _, a := range accts {
		if ctx.Err() != nil {
			return
		}
		if s.holder.ready(a.ConfigDir) {
			continue
		}
		s.healFuse(a)
	}
}

// remountCarriedDirs remounts a dead holder's pre-row mounts: dirs its
// registry served that no account row names — a `ccp add` mid-login, whose
// row only lands at FinalizeAdd. Dropping them would strand the add (its
// mount died with the holder and nothing else knows the dir exists). The
// bases come from carriedBases' snapshot of the dead holder's registry; a
// dir that has since gained a row is left to remountFuseRows' claim
// discipline. Carcasses clear through the provider's foreign-mount contract,
// exactly like mountFuse.
func (s *Server) remountCarriedDirs(ctx context.Context, rowDirs map[string]bool, carry map[string]string) {
	for dir, base := range carry {
		if ctx.Err() != nil {
			return
		}
		if rowDirs[dir] || s.holder.ready(dir) {
			continue
		}
		prov := s.overlayFor(overlay.KindFuse)
		if prov.Kind() != overlay.KindFuse {
			return
		}
		err := prov.Setup(base, dir)
		if errors.Is(err, mountd.ErrForeignMount) || errors.Is(err, mountd.ErrBaseMismatch) {
			if terr := prov.Teardown(base, dir); terr != nil {
				s.log.Printf("pre-row mount %s: clear carcass: %v", dir, terr)
				continue
			}
			err = prov.Setup(base, dir)
		}
		if err != nil {
			s.log.Printf("remount pre-row mount %s: %v", dir, err)
			continue
		}
		s.holder.noteMounted(dir)
		s.log.Printf("remounted pre-row mount %s (in-flight add)", dir)
	}
}

// fuseAccounts lists the fuse-kind account rows.
func (s *Server) fuseAccounts() ([]store.Account, error) {
	accts, err := s.m.Store.ListAccounts()
	if err != nil {
		return nil, err
	}
	fuse := make([]store.Account, 0, len(accts))
	for _, a := range accts {
		if a.OverlayKind == string(overlay.KindFuse) {
			fuse = append(fuse, a)
		}
	}
	return fuse, nil
}

// accountIDs extracts the row ids, in order.
func accountIDs(accts []store.Account) []int {
	ids := make([]int, len(accts))
	for i, a := range accts {
		ids[i] = a.ID
	}
	return ids
}

// sameAccountIDs reports whether two account lists name the same ids in the
// same order (both sides come from the same ordered store query).
func sameAccountIDs(a, b []store.Account) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

// canSpawnHolder reports whether this daemon can spawn a mount holder at all:
// an injected seam vouches for itself; the real spawn needs the fuse build.
func (s *Server) canSpawnHolder() bool {
	return s.spawnHolder != nil || overlay.FuseBuilt()
}

// spawn starts a mount holder on the daemon's holder socket through the seam;
// nil means mountd.EnsureRunning.
func (s *Server) spawn() error {
	if s.spawnHolder != nil {
		return s.spawnHolder(s.holderSocket, s.holderLog, mountd.DefaultSpawnTimeout)
	}
	return mountd.EnsureRunning(s.holderSocket, s.holderLog, mountd.DefaultSpawnTimeout)
}

// killPeerPid force-terminates the process holding socket through the seam,
// but only when its peer pid matches wantPID; nil means peerpid.KillPid
// (peer credentials resolved and matched in one dial, never a name match).
func (s *Server) killPeerPid(socket string, wantPID int) (int, error) {
	if s.killHolderPeer != nil {
		return s.killHolderPeer(socket, wantPID)
	}
	return peerpid.KillPid(socket, wantPID)
}

// peerPIDOf resolves the pid holding socket through the seam; nil means
// peerpid.PeerPID.
func (s *Server) peerPIDOf(socket string) (int, error) {
	if s.peerPID != nil {
		return s.peerPID(socket)
	}
	return peerpid.PeerPID(socket)
}

// noteSpawnFailure records one failed spawn attempt: backoff bookkeeping, the
// HolderStatus.SpawnError surface, and a log line once per distinct error
// text — never per tick.
func (s *Server) noteSpawnFailure(err error) {
	s.sup.failures++
	wait := spawnBackoff(s.sup.failures)
	s.sup.retryAt = time.Now().Add(wait)
	s.holder.recordSpawnError(err.Error())
	if err.Error() != s.sup.lastSpawnErr {
		s.sup.lastSpawnErr = err.Error()
		s.log.Printf("spawn mount holder (attempt %d, next in %s): %v", s.sup.failures, wait, err)
	}
}

// resetSpawnBackoff clears the respawn backoff and any surfaced spawn error.
func (s *Server) resetSpawnBackoff() {
	s.sup.failures = 0
	s.sup.retryAt = time.Time{}
	if s.sup.lastSpawnErr != "" {
		s.sup.lastSpawnErr = ""
		s.holder.recordSpawnError("")
	}
}
