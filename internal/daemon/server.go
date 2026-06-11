package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yasyf/cc-pool/internal/oauth"
	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/procscan"
	"github.com/yasyf/cc-pool/internal/score"
	"github.com/yasyf/cc-pool/internal/store"
	"github.com/yasyf/cc-pool/internal/version"
)

// reservationTTL is how long a select-reservation suppresses re-picking the
// same account before the real claude process is visible to procscan.
const reservationTTL = 30 * time.Second

// preflightTimeout bounds a best-effort preflight refresh so shutdown is never
// blocked on a slow network refresh.
const preflightTimeout = 8 * time.Second

// defaultEvictTimeout bounds how long a starting daemon waits for a
// version-skewed holder to release the socket after being told to step down.
const defaultEvictTimeout = 5 * time.Second

// Server is the running daemon.
type Server struct {
	m        *pool.Manager
	socket   string
	snapshot string // status mirror path; tests point it into a temp dir
	log      *log.Logger

	// evictTimeout bounds the wait for a skewed holder to release the socket.
	evictTimeout time.Duration

	// triggerShutdown cancels serve's context, ending the daemon. It is set once
	// in serve before the accept loop starts; the go-statement that spawns each
	// handler establishes the happens-before, so handlers read it without a lock.
	triggerShutdown context.CancelFunc

	// wg tracks every daemon goroutine (scheduler, connection handlers,
	// preflight refreshes); serve Waits on it before tearing down mounts and
	// before Run's deferred m.Close() closes the database under them.
	wg sync.WaitGroup

	mu           sync.Mutex
	reservations map[int]time.Time // accountID -> reserved-at
	converting   map[int]bool      // accountID -> overlay conversion in flight
	polling      map[int]bool      // accountID -> scheduler/reconcile owns the dir this iteration
	rlStreak     map[int]int       // accountID -> consecutive 429 count

	// fuseGateFn overrides the migrate handler's fuse-capability gate; nil
	// means the real check (FuseBuilt + probe mount). Tests inject outcomes
	// alongside Manager.OverlayFor.
	fuseGateFn func() string

	// migrateBudget bounds one migrate request's conversion work; zero means
	// defaultMigrateBudget. Tests shrink it to pin the out-of-time path.
	migrateBudget time.Duration
}

// Run is the entry point for `cc-pool daemon`. It blocks until the process
// is signalled.
func Run(ctx context.Context) error {
	m, err := pool.Open()
	if err != nil {
		return err
	}
	defer m.Close()

	s := &Server{
		m:            m,
		socket:       pool.SocketPath(),
		snapshot:     pool.StatusSnapshotPath(),
		log:          log.New(os.Stderr, "[cc-pool] ", log.LstdFlags),
		evictTimeout: defaultEvictTimeout,
		reservations: map[int]time.Time{},
		converting:   map[int]bool{},
		polling:      map[int]bool{},
		rlStreak:     map[int]int{},
	}
	return s.serve(ctx)
}

// detectClaudeVersion runs `claude --version` (best-effort) to stamp the UA.
func detectClaudeVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "--version").Output()
	if err != nil {
		return ""
	}
	// Output looks like "2.1.166 (Claude Code)"; take the leading version token.
	fields := strings.Fields(string(out))
	if len(fields) > 0 {
		return fields[0]
	}
	return ""
}

func (s *Server) serve(ctx context.Context) error {
	ln, err := s.listen()
	if err != nil {
		return err
	}
	// closeListener unlinks the socket exactly once. *net.UnixListener.Close
	// unlinks the socket file and is NOT idempotent: a second Close (the late
	// deferred one, after a slow teardown) would delete a successor daemon's
	// freshly-bound socket. The sync.Once pins the unlink to the first close, at
	// ctx-cancel time. No explicit os.Remove for the same reason.
	var closeOnce sync.Once
	closeListener := func() { closeOnce.Do(func() { _ = ln.Close() }) }
	defer closeListener()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// stop cancels ctx, so it doubles as the over-the-socket shutdown trigger
	// (OpShutdown). Set before the accept loop spawns any handler.
	s.triggerShutdown = stop

	s.log.Printf("daemon %s started; socket=%s", version.String(), s.socket)

	// Detect the claude version, establish mounts, then run the scheduler in one
	// goroutine, off the accept path so Health is responsive from the first
	// instant. detectClaudeVersion runs `claude --version` (a heavy Node CLI, up
	// to a 3s timeout): kept off the pre-bind path here so a slow probe can't make
	// a freshly-started daemon look "not responding" to a waiting `ccp add`. It
	// only stamps the OAuth User-Agent, whose sole consumer is the scheduler's
	// first poll, so running it before reconcileOverlays/scheduler preserves
	// ordering. The three stay sequential in one goroutine — not bare ones —
	// because reconcileOverlays must finish before the scheduler's first poll,
	// which can also touch fuse Setup (a check-then-act on the same mountpoint).
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		oauth.SetUserAgentVersion(detectClaudeVersion())
		s.reconcileOverlays(ctx)
		s.scheduler(ctx)
	}()

	// Break the accept loop on shutdown.
	go func() {
		<-ctx.Done()
		closeListener()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			// Back off on a transient accept error (e.g. EMFILE) instead of
			// busy-spinning a core until the next shutdown.
			s.log.Printf("accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.handle(ctx, conn) }()
	}

	s.wg.Wait()
	s.teardownMounts()
	s.log.Printf("daemon stopped")
	return nil
}

// listen binds the unix socket with 0600 perms, evicting a version-skewed
// holder first (see evictHolder) and refusing only a live same-version peer.
func (s *Server) listen() (net.Listener, error) {
	if err := s.evictHolder(); err != nil {
		return nil, err
	}
	_ = os.Remove(s.socket) // clear any stale socket the evicted holder left
	// Only the socket's parent dir is needed here (in production that is the
	// state dir); deriving it from s.socket keeps tests off the real ~/.cc-pool.
	if err := os.MkdirAll(filepath.Dir(s.socket), 0o700); err != nil {
		return nil, fmt.Errorf("ensure socket dir: %w", err)
	}
	ln, err := net.Listen("unix", s.socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(s.socket, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// evictHolder makes way when the socket is already held. A holder at our own
// version is a genuine double-start and is refused. A version-skewed holder —
// an orphan launchd no longer tracks (a detached EnsureRunning spawn, or a
// pre-upgrade image KeepAlive held), which `brew services stop` cannot kill —
// is told to step down (OpShutdown) and waited out so we can rebind. Only the
// non-holder ever evicts, and only on a version mismatch, so two daemons can
// never mutually evict each other.
func (s *Server) evictHolder() error {
	c := &Client{socket: s.socket}
	resp, err := c.Health()
	if err != nil {
		return nil // no live holder: stale or missing socket
	}
	if resp.Version == version.String() {
		return errors.New("another cc-pool daemon at the same version is already running")
	}
	s.log.Printf("evicting version-skewed daemon (%s) holding the socket", resp.Version)
	if _, err := c.Shutdown(); err != nil {
		return fmt.Errorf("evict holder %s: %w", resp.Version, err)
	}
	if !c.WaitGone(s.evictTimeout) {
		// Acked OpShutdown but wedged: kill the exact socket holder so we can
		// rebind, rather than exiting and leaving launchd to retry against it.
		if _, err := c.KillHolder(); err != nil {
			s.log.Printf("kill holder: %v", err)
		}
		if !c.WaitGone(s.evictTimeout) {
			return fmt.Errorf("holder %s did not release the socket within %s", resp.Version, s.evictTimeout)
		}
	}
	return nil
}

// handle serves one connection. ctx is the daemon's lifecycle context (bounds
// shutdown); the conn deadline independently bounds a single slow client.
func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		writeResp(conn, Response{OK: false, Error: "bad request: " + err.Error()})
		return
	}
	if req.Op == OpMigrate {
		// A migrate legitimately outlives the 10s deadline: a probe mount plus
		// up to an 8s mount wait (and a bounded rollback) per account. Stay
		// under the client's 150s so the server, not a dead socket, reports
		// the outcome.
		_ = conn.SetDeadline(time.Now().Add(140 * time.Second))
	}
	resp := s.dispatch(ctx, req)
	resp.Proto = ProtocolVersion
	writeResp(conn, resp)
}

func writeResp(conn net.Conn, r Response) {
	r.Proto = ProtocolVersion
	_ = json.NewEncoder(conn).Encode(r)
}

func (s *Server) dispatch(ctx context.Context, req Request) Response {
	switch req.Op {
	case OpHealth:
		return Response{OK: true, Version: version.String()}
	case OpStatus:
		return s.handleStatus(ctx)
	case OpSelect:
		return s.handleSelect(ctx, req)
	case OpCheckin:
		return s.handleCheckin(ctx, req)
	case OpMigrate:
		return s.handleMigrate(ctx, req)
	case OpShutdown:
		return s.handleShutdown()
	default:
		return Response{OK: false, Error: "unknown op: " + string(req.Op)}
	}
}

// handleShutdown replies OK, then cancels serve's context so this instance steps
// down and releases the socket — the only eviction that works on an orphan
// launchd no longer tracks. Cancelling the ctx closes the listener, never this
// live connection, so the OK reply (written by handle after dispatch returns)
// still lands; wg.Wait then drains this handler normally. Idempotent on repeats.
func (s *Server) handleShutdown() Response {
	s.triggerShutdown()
	return Response{OK: true, Version: version.String()}
}

// handleStatus returns scored snapshots from cached samples (no live fetch).
func (s *Server) handleStatus(ctx context.Context) Response {
	accts, err := s.statuses(ctx)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	// Version lets the client detect a pre-upgrade daemon (which omits newer wire
	// fields like Components) and fall back to live sampling.
	return Response{OK: true, Version: version.String(), Accounts: accts}
}

// statuses assembles the wire view of every account from cached samples — the
// single mapping shared by the socket status op and the on-disk snapshot.
func (s *Server) statuses(ctx context.Context) ([]AccountStatus, error) {
	snaps, err := s.m.Snapshots(ctx, false, 0)
	if err != nil {
		return nil, err
	}
	return ToStatuses(snaps), nil
}

// handleSelect picks the best available account from cached scores, applying
// short-lived reservations to avoid two selects colliding, and records a
// reservation for the winner.
func (s *Server) handleSelect(ctx context.Context, req Request) Response {
	snaps, err := s.m.Snapshots(ctx, false, 0)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	if len(snaps) == 0 {
		return Response{OK: false, Error: pool.ErrNoAccounts.Error()}
	}

	// Forced account.
	if req.Account != nil {
		for _, sn := range snaps {
			if sn.Account.ID == *req.Account {
				if !s.mountReady(sn.Account) {
					if sn.Account.OverlayKind == string(overlay.KindFuse) {
						return Response{OK: false, Error: fmt.Sprintf("acct-%02d's fuse mount is not up yet; retry shortly", sn.Account.ID)}
					}
					return Response{OK: false, Error: fmt.Sprintf("acct-%02d's dir is unexpectedly a mountpoint (wedged unmount?); see `ccp doctor` and the daemon log", sn.Account.ID)}
				}
				if !s.tryReserve(sn.Account.ID) {
					return Response{OK: false, Error: fmt.Sprintf("acct-%02d is migrating overlays; retry shortly", sn.Account.ID)}
				}
				if !req.NoMark && req.PID > 0 {
					if _, err := s.m.Store.OpenSession(sn.Account.ID, req.PID, sn.Account.ConfigDir, req.Cwd, time.Now()); err != nil {
						s.log.Printf("open session for acct-%02d pid %d: %v", sn.Account.ID, req.PID, err)
					}
				}
				s.recordSticky(req.Cwd, sn.Account.ID)
				id := sn.Account.ID
				return Response{OK: true, Dir: sn.Account.ConfigDir, SelectedID: &id,
					Remaining5h: sn.Remaining5h, Remaining7d: sn.Remaining7d, HasUsage: sn.HasUsage}
			}
		}
		return Response{OK: false, Error: fmt.Sprintf("account %d not found", *req.Account)}
	}

	// Reconcile session rows against reality before consulting the pin: a
	// claude that just exited must read as warm (bind), not live (hold), and
	// pollOnce's ~3.5-minute cadence is too coarse for a quick resume.
	if sessions, err := procscan.Scan(); err == nil {
		if _, cerr := s.m.Store.CloseDeadSessions(procscan.AlivePIDs(sessions), time.Now()); cerr != nil {
			s.log.Printf("close dead sessions: %v", cerr)
		}
	}

	// An account mid-conversion or whose mirror is not mounted yet (daemon
	// still establishing mounts after startup, or a failed mount pending
	// fallback) cannot serve a session — its config dir is not in a usable
	// shape. Exclude rather than penalize.
	usable := make([]pool.Snapshot, 0, len(snaps))
	for _, sn := range snaps {
		if s.isConverting(sn.Account.ID) || !s.mountReady(sn.Account) {
			continue
		}
		usable = append(usable, sn)
	}
	if len(usable) == 0 {
		soonest := soonestReset(snaps)
		resp := Response{OK: false, Error: pool.ErrNoneAvailable.Error(), NoneAvailable: true}
		if !soonest.IsZero() {
			resp.SoonestReset = &soonest
		}
		s.log.Printf("select: %s -> none available (all accounts migrating or unmounted)", req.Cwd)
		return resp
	}

	ranked, bySnap := s.rankWithReservations(usable)
	pin, outcome := s.m.StickyPick(req.Cwd, ranked, time.Now())
	r := pin
	fallback := false
	if outcome != pool.StickyBind {
		var ok bool
		r, ok = score.Pick(ranked)
		if !ok && !req.NoFallback {
			// Every account is exhausted (or worse): launch on the least-bad
			// exhausted one rather than refusing; the client warns loudly.
			r, ok = score.PickFallback(ranked)
			fallback = true
		}
		if !ok {
			soonest := soonestReset(snaps)
			resp := Response{OK: false, Error: pool.ErrNoneAvailable.Error(), NoneAvailable: true}
			when := "unknown"
			if !soonest.IsZero() {
				resp.SoonestReset = &soonest
				when = soonest.Format(time.RFC3339)
			}
			s.log.Printf("select: %s -> none available (soonest reset %s)", req.Cwd, when)
			return resp
		}
	}
	best := bySnap[r.AccountID]
	if !req.NoMark {
		if !s.tryReserve(best.Account.ID) {
			// A conversion claimed the winner between the filter above and
			// here — vanishingly rare; the client just retries.
			return Response{OK: false, Error: fmt.Sprintf("acct-%02d began migrating overlays mid-select; retry shortly", best.Account.ID)}
		}
		if req.PID > 0 {
			if _, err := s.m.Store.OpenSession(best.Account.ID, req.PID, best.Account.ConfigDir, req.Cwd, time.Now()); err != nil {
				s.log.Printf("open session for acct-%02d pid %d: %v", best.Account.ID, req.PID, err)
			}
		}
	}
	// Record regardless of NoMark (cache continuity is established by no-mark
	// selects too) — but never over a held pin, unless the free ranking landed
	// on the pinned account anyway, which is genuine pin activity.
	if !outcome.Held() || best.Account.ID == pin.AccountID {
		s.recordSticky(req.Cwd, best.Account.ID)
	}
	s.log.Printf("select%s: %s -> acct-%02d (score %.1f · 5h %.0f%% used · 7d %.0f%% used%s)",
		selectKind(outcome, fallback), req.Cwd, best.Account.ID,
		r.Score, best.Util5h, best.Util7d, runnerUp(ranked, r.AccountID, fallback))
	id := best.Account.ID
	// Preflight refresh the winner if idle and expiring soon (best-effort).
	// The Add(1) runs inside an already-tracked handler goroutine, so the
	// counter is ≥1 here and can never race a zero-counter Wait.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		pctx, cancel := context.WithTimeout(ctx, preflightTimeout)
		defer cancel()
		if err := s.m.PreflightRefresh(pctx, best.Account); err != nil {
			s.log.Printf("acct-%02d preflight refresh: %v", best.Account.ID, err)
		}
	}()
	resp := Response{OK: true, Dir: best.Account.ConfigDir, SelectedID: &id,
		Sticky:      outcome == pool.StickyBind,
		Remaining5h: best.Remaining5h, Remaining7d: best.Remaining7d, HasUsage: best.HasUsage,
		ExhaustedFallback: fallback, ExtraEnabled: best.ExtraEnabled}
	if outcome == pool.StickyHoldManual {
		held := pin.AccountID
		resp.PinHeldAccount = &held
	}
	if fallback && !r.ExhaustedUntil.IsZero() {
		// Tell the client when the fallback pick actually recovers — the latest
		// reset among its pegged windows, not necessarily the 5h one.
		resp.SoonestReset = &r.ExhaustedUntil
	}
	return resp
}

// selectKind renders the select log qualifier. A bind and a fallback are
// mutually exclusive (an unusable pin never binds); a held pin coexists with
// fallback only when the free ranking itself collapsed to the least-bad pick,
// and the fallback warning is the more urgent of the two.
func selectKind(outcome pool.StickyOutcome, fallback bool) string {
	switch {
	case outcome == pool.StickyBind:
		return " (sticky)"
	case fallback:
		return " (exhausted-fallback)"
	case outcome.Held():
		return " (pin-held)"
	default:
		return ""
	}
}

// runnerUp renders the next-best servable account after winnerID for the
// select log, empty when there is none. A fallback pick means nothing is
// Available, so candidates widen to PickFallback's own predicate — otherwise
// the one select kind that most needs forensic context would never log one.
func runnerUp(ranked []score.Result, winnerID int, fallback bool) string {
	for _, r := range ranked {
		if r.AccountID == winnerID {
			continue
		}
		if !r.Available && !(fallback && !r.RateLimited) {
			continue
		}
		return fmt.Sprintf(" · runner-up acct-%02d %.1f", r.AccountID, r.Score)
	}
	return ""
}

// recordSticky upserts the cwd->account sticky record, logging (not failing)
// on error.
func (s *Server) recordSticky(cwd string, accountID int) {
	if err := s.m.RecordSticky(cwd, accountID, time.Now()); err != nil {
		s.log.Printf("record sticky for %s: %v", cwd, err)
	}
}

// handleCheckin closes sessions for a pid and adopts any rotated token.
func (s *Server) handleCheckin(ctx context.Context, req Request) Response {
	sessions, err := s.m.Store.ListActiveSessions()
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	for _, se := range sessions {
		if se.PID != req.PID {
			continue
		}
		if err := s.m.Store.CloseSession(se.ID, time.Now()); err != nil {
			s.log.Printf("checkin close session %d: %v", se.ID, err)
		}
		if a, err := s.m.Store.GetAccount(se.AccountID); err == nil {
			actx, cancel := context.WithTimeout(ctx, preflightTimeout)
			if err := s.m.AdoptRotatedToken(actx, a); err != nil {
				s.log.Printf("acct-%02d adopt rotated token on checkin: %v", a.ID, err)
			}
			cancel()
		}
	}
	return Response{OK: true}
}

// tryReserve records a short-lived reservation for an account, refusing while
// an overlay conversion holds it (the conversion is about to remake the dir a
// launching claude would land in).
func (s *Server) tryReserve(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.converting[id] {
		return false
	}
	s.reservations[id] = time.Now()
	return true
}

// beginConvert claims an account for overlay conversion iff it has no live
// reservation, no conversion already in flight, and the scheduler/reconcile is
// not mid-iteration on its dir. The check-and-claim is one critical section,
// closing the race against tryReserve and beginPoll; the converting flag — not
// the mutex — then owns the account across the conversion's I/O, the same way
// reservations bridge select→spawn.
func (s *Server) beginConvert(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.reservations[id]; ok && time.Since(t) <= reservationTTL {
		return false
	}
	if s.converting[id] || s.polling[id] {
		return false
	}
	if s.converting == nil {
		s.converting = map[int]bool{}
	}
	s.converting[id] = true
	return true
}

// endConvert releases a conversion claim.
func (s *Server) endConvert(id int) {
	s.mu.Lock()
	delete(s.converting, id)
	s.mu.Unlock()
}

// isConverting reports whether an overlay conversion holds the account.
func (s *Server) isConverting(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.converting[id]
}

// beginPoll claims an account for one scheduler/reconcile iteration — the
// Sync/Setup/fallback and refresh work that must never interleave with a
// conversion's move/teardown/mount sequence. Unlike converting, a poll claim
// does not hide the account from select (sessions can land on a dir being
// health-checked); it only excludes conversions, two-sidedly with
// beginConvert. The claim — not the mutex — owns the account across the
// iteration's I/O.
func (s *Server) beginPoll(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.converting[id] || s.polling[id] {
		return false
	}
	if s.polling == nil {
		s.polling = map[int]bool{}
	}
	s.polling[id] = true
	return true
}

// endPoll releases a poll claim.
func (s *Server) endPoll(id int) {
	s.mu.Lock()
	delete(s.polling, id)
	s.mu.Unlock()
}

// mountReady reports whether an account's overlay can serve a session right
// now. The check is kind-symmetric: a fuse row needs its mirror actually
// mounted, and a non-fuse row needs the dir NOT mounted — a live mountpoint
// under a symlink row is the wreckage of an aborted rollback (wedged unmount),
// where the dir serves a mirror whose private backing no longer holds the
// account's identity.
func (s *Server) mountReady(a store.Account) bool {
	if a.OverlayKind == string(overlay.KindFuse) {
		return overlay.Mounted(a.ConfigDir)
	}
	return !overlay.Mounted(a.ConfigDir)
}

// overlayFor resolves a kind through the Manager's injectable seam (tests fake
// the fuse provider); nil means overlay.For.
func (s *Server) overlayFor(kind overlay.Kind) overlay.Provider {
	if s.m.OverlayFor != nil {
		return s.m.OverlayFor(kind)
	}
	return overlay.For(kind)
}

// reservedCount returns the number of live reservations for an account.
func (s *Server) reservedCount(id int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.reservations[id]
	if !ok {
		return 0
	}
	if time.Since(t) > reservationTTL {
		delete(s.reservations, id)
		return 0
	}
	return 1
}

// rankWithReservations re-ranks snapshots with reservation penalties applied,
// returning the ranking plus a snapshot lookup by account id.
func (s *Server) rankWithReservations(snaps []pool.Snapshot) ([]score.Result, map[int]pool.Snapshot) {
	bySnap := map[int]pool.Snapshot{}
	inputs := make([]score.Input, 0, len(snaps))
	for _, sn := range snaps {
		bySnap[sn.Account.ID] = sn
		inputs = append(inputs, score.Input{
			AccountID:      sn.Account.ID,
			HasUsage:       sn.HasUsage,
			SampleTS:       time.Now().Add(-sn.SampleAge),
			Util5h:         sn.Util5h,
			Util7d:         sn.Util7d,
			Resets5h:       sn.Resets5h,
			Resets7d:       sn.Resets7d,
			Burn5hPerHour:  sn.Burn5hPerHour,
			ActiveSessions: sn.ActiveSessions + s.reservedCount(sn.Account.ID),
			RateLimited:    sn.RateLimited,
			RefreshFailed:  sn.Stale && !sn.HasUsage,
		})
	}
	return score.Rank(inputs, time.Now()), bySnap
}

func soonestReset(snaps []pool.Snapshot) time.Time {
	var best time.Time
	for _, sn := range snaps {
		if sn.Resets5h.IsZero() {
			continue
		}
		if best.IsZero() || sn.Resets5h.Before(best) {
			best = sn.Resets5h
		}
	}
	return best
}

// ToStatuses converts snapshots into wire AccountStatus values.
func ToStatuses(snaps []pool.Snapshot) []AccountStatus {
	out := make([]AccountStatus, 0, len(snaps))
	for _, sn := range snaps {
		out = append(out, AccountStatus{
			ID:             sn.Account.ID,
			ConfigDir:      sn.Account.ConfigDir,
			Label:          sn.Account.Label,
			OverlayKind:    sn.Account.OverlayKind,
			Score:          sn.Score,
			Remaining5h:    sn.Remaining5h,
			Remaining7d:    sn.Remaining7d,
			ActiveSessions: sn.ActiveSessions,
			RateLimited:    sn.RateLimited,
			Exhausted:      sn.Exhausted,
			HasUsage:       sn.HasUsage,
			Stale:          sn.Stale,
			Resets5h:       sn.Resets5h,
			Resets7d:       sn.Resets7d,
			SampleAge:      sn.SampleAge.Round(time.Second).String(),
			// The wire ships the gated display forecast, never the raw
			// scoring burn (which stays live on stale samples).
			Burn5hPerHour:      sn.Forecast.BurnPerHour,
			Projected5hAtReset: sn.Forecast.AtReset,
			Depleted5hAt:       sn.Forecast.DepletedAt,
			ExtraEnabled:       sn.ExtraEnabled,
			ExtraUsed:          sn.ExtraUsed,
			ExtraLimit:         sn.ExtraLimit,
			Components:         sn.Components,
		})
	}
	return out
}

// reconcileOverlays brings each account's on-disk overlay in line with its
// row at startup. It
// runs off the accept path; ctx is checked between accounts so a boot-time
// shutdown doesn't block wg.Wait for the full mount timeout of a slow account.
func (s *Server) reconcileOverlays(ctx context.Context) {
	accts, err := s.m.Store.ListAccounts()
	if err != nil {
		return
	}
	for _, a := range accts {
		if ctx.Err() != nil {
			return
		}
		if !s.beginPoll(a.ID) {
			// An OpMigrate landed before startup reconcile reached this
			// account; the conversion leaves it consistent on its own.
			s.log.Printf("acct-%02d busy converting; skipping startup reconcile", a.ID)
			continue
		}
		s.reconcileAccount(a)
		s.endPoll(a.ID)
	}
}

// reconcileAccount brings one account's on-disk overlay in line with its row.
// Caller holds the poll claim.
func (s *Server) reconcileAccount(a store.Account) {
	switch overlay.Kind(a.OverlayKind) {
	case overlay.KindFuse:
		if err := s.mountFuse(a); err != nil {
			s.log.Printf("acct-%02d mount failed, falling back to symlink: %v", a.ID, err)
			s.fallbackToSymlink(a)
		}
	default:
		// At startup this daemon owns no mounts, so a live mountpoint under a
		// non-fuse row is wreckage by construction — a dead daemon's stale
		// mount or an aborted rollback's wedged unmount. It blocks every
		// symlink repair (they refuse mountpoints); force it down first.
		if overlay.Mounted(a.ConfigDir) {
			prov := s.overlayFor(overlay.KindFuse)
			if prov.Kind() != overlay.KindFuse {
				s.log.Printf("acct-%02d: dir is a stale mountpoint but this build has no fuse provider to unmount it", a.ID)
				return
			}
			if err := prov.Teardown(pool.ClaudeDir(), a.ConfigDir); err != nil {
				s.log.Printf("acct-%02d: unmount stale mountpoint: %v", a.ID, err)
				return
			}
			s.log.Printf("acct-%02d: cleared a stale mountpoint", a.ID)
		}
		// A symlink account can carry private files stranded in a fuse
		// backing dir by a conversion (or pre-fix fallback) that died
		// midway — restore them before anything launches on the account.
		healed, err := s.m.HealStrandedPrivate(a)
		if err != nil {
			s.log.Printf("acct-%02d heal stranded private files: %v", a.ID, err)
			return
		}
		if healed {
			s.log.Printf("acct-%02d restored private files stranded by an interrupted migration", a.ID)
		}
	}
}

// mountFuse establishes a fuse account's mirror. Before mounting it sweeps any
// private files out of the mount underlay (the real dir) into the backing dir:
// a conversion killed between its file moves and its row flip leaves them
// there, and mounting over them would shadow the account's identity — a
// session would then mint a divergent one in the backing dir. It also refuses,
// rather than silently degrades, when this build has no fuse provider.
func (s *Server) mountFuse(a store.Account) error {
	prov := s.overlayFor(overlay.KindFuse)
	if prov.Kind() != overlay.KindFuse {
		return errors.New("fuse provider unavailable in this build")
	}
	dir := a.ConfigDir
	if !overlay.Mounted(dir) {
		switch has, err := overlay.HasPrivateEntries(dir); {
		case err != nil:
			return fmt.Errorf("check underlay for stranded private files: %w", err)
		case has:
			if err := overlay.MovePrivateEntries(dir, overlay.FusePrivateRoot(dir)); err != nil {
				return fmt.Errorf("sweep stranded private files into backing dir: %w", err)
			}
			s.log.Printf("acct-%02d swept private files from the mount underlay into the backing dir", a.ID)
		}
	}
	return prov.Setup(pool.ClaudeDir(), dir)
}

// teardownMounts unmounts fuse mounts on shutdown.
func (s *Server) teardownMounts() {
	accts, err := s.m.Store.ListAccounts()
	if err != nil {
		return
	}
	for _, a := range accts {
		// Keyed on actual mount state, not row kind: a wedged unmount can
		// leave a live mountpoint under a symlink row, and it must not
		// outlive this daemon's tracking.
		if !overlay.Mounted(a.ConfigDir) {
			continue
		}
		prov := s.overlayFor(overlay.KindFuse)
		if prov.Kind() != overlay.KindFuse {
			continue // nothing in this build can unmount it
		}
		_ = prov.Teardown(pool.ClaudeDir(), a.ConfigDir)
	}
}

// fallbackToSymlink converts an account to the symlink provider after a mount
// failure so its dir is fully usable again. ConvertOverlay verifies the
// unmount before laying any symlink and moves the private files back out of
// the fuse backing dir — the earlier hand-rolled fallback left them stranded
// there, severing the account from its .claude.json identity. Callers must
// hold the account's poll or converting claim; the conversion must not race
// another overlay mutation on the dir.
func (s *Server) fallbackToSymlink(a store.Account) {
	if _, err := s.m.ConvertOverlay(a, overlay.KindSymlink); err != nil {
		s.log.Printf("acct-%02d symlink fallback: %v", a.ID, err)
	}
}
