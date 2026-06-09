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
	m      *pool.Manager
	socket string
	log    *log.Logger

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
	rlStreak     map[int]int       // accountID -> consecutive 429 count
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
		log:          log.New(os.Stderr, "[cc-pool] ", log.LstdFlags),
		evictTimeout: defaultEvictTimeout,
		reservations: map[int]time.Time{},
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
	// first poll, so running it before establishMounts/scheduler preserves
	// ordering. The three stay sequential in one goroutine — not bare ones —
	// because establishMounts must finish before the scheduler's first poll, which
	// can also touch fuse Setup (a check-then-act on the same mountpoint).
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		oauth.SetUserAgentVersion(detectClaudeVersion())
		s.establishMounts(ctx)
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
	snaps, err := s.m.Snapshots(ctx, false, 0)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	// Version lets the client detect a pre-upgrade daemon (which omits newer wire
	// fields like Components) and fall back to live sampling.
	return Response{OK: true, Version: version.String(), Accounts: toStatuses(snaps)}
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
				s.reserve(sn.Account.ID)
				s.recordSticky(req.Cwd, sn.Account.ID)
				id := sn.Account.ID
				return Response{OK: true, Dir: sn.Account.ConfigDir, SelectedID: &id,
					Remaining5h: sn.Remaining5h, Remaining7d: sn.Remaining7d, HasUsage: sn.HasUsage}
			}
		}
		return Response{OK: false, Error: fmt.Sprintf("account %d not found", *req.Account)}
	}

	ranked, bySnap := s.rankWithReservations(snaps)
	r, sticky := s.m.StickyPick(req.Cwd, ranked, time.Now())
	if !sticky {
		var ok bool
		r, ok = score.Pick(ranked)
		if !ok {
			soonest := soonestReset(snaps)
			resp := Response{OK: false, Error: pool.ErrNoneAvailable.Error()}
			if !soonest.IsZero() {
				resp.SoonestReset = &soonest
			}
			return resp
		}
	}
	best := bySnap[r.AccountID]
	if !req.NoMark {
		s.reserve(best.Account.ID)
		if req.PID > 0 {
			if _, err := s.m.Store.OpenSession(best.Account.ID, req.PID, best.Account.ConfigDir); err != nil {
				s.log.Printf("open session for acct-%02d pid %d: %v", best.Account.ID, req.PID, err)
			}
		}
	}
	// Record regardless of NoMark: cache continuity is established by
	// `ccp run`'s no-mark select too.
	s.recordSticky(req.Cwd, best.Account.ID)
	if sticky {
		s.log.Printf("sticky select: %s -> acct-%02d", req.Cwd, best.Account.ID)
	}
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
	return Response{OK: true, Dir: best.Account.ConfigDir, SelectedID: &id, Sticky: sticky,
		Remaining5h: best.Remaining5h, Remaining7d: best.Remaining7d, HasUsage: best.HasUsage}
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
		if err := s.m.Store.CloseSession(se.ID); err != nil {
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

// reserve records a short-lived reservation for an account.
func (s *Server) reserve(id int) {
	s.mu.Lock()
	s.reservations[id] = time.Now()
	s.mu.Unlock()
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

// toStatuses converts snapshots into wire AccountStatus values.
func toStatuses(snaps []pool.Snapshot) []AccountStatus {
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
			HasUsage:       sn.HasUsage,
			Stale:          sn.Stale,
			Resets5h:       sn.Resets5h,
			Resets7d:       sn.Resets7d,
			SampleAge:      sn.SampleAge.Round(time.Second).String(),
			Components:     sn.Components,
		})
	}
	return out
}

// establishMounts brings up fuse mounts for fuse-kind accounts at startup. It
// runs off the accept path; ctx is checked between accounts so a boot-time
// shutdown doesn't block wg.Wait for the full mount timeout of a slow account.
func (s *Server) establishMounts(ctx context.Context) {
	accts, err := s.m.Store.ListAccounts()
	if err != nil {
		return
	}
	for _, a := range accts {
		if ctx.Err() != nil {
			return
		}
		if a.OverlayKind != string(overlay.KindFuse) {
			continue
		}
		prov := overlay.For(overlay.KindFuse)
		if err := prov.Setup(pool.ClaudeDir(), a.ConfigDir); err != nil {
			s.log.Printf("acct-%02d mount failed, falling back to symlink: %v", a.ID, err)
			s.fallbackToSymlink(a)
		}
	}
}

// teardownMounts unmounts fuse mounts on shutdown.
func (s *Server) teardownMounts() {
	accts, err := s.m.Store.ListAccounts()
	if err != nil {
		return
	}
	for _, a := range accts {
		if a.OverlayKind != string(overlay.KindFuse) {
			continue
		}
		_ = overlay.For(overlay.KindFuse).Teardown(pool.ClaudeDir(), a.ConfigDir)
	}
}

// fallbackToSymlink switches an account to the symlink provider after a mount
// failure so its dir is still usable.
func (s *Server) fallbackToSymlink(a store.Account) {
	if err := (overlay.For(overlay.KindSymlink)).Setup(pool.ClaudeDir(), a.ConfigDir); err != nil {
		s.log.Printf("acct-%02d symlink fallback failed: %v", a.ID, err)
		return
	}
	a.OverlayKind = string(overlay.KindSymlink)
	if err := s.m.Store.UpsertAccount(a); err != nil {
		s.log.Printf("acct-%02d persist symlink fallback: %v", a.ID, err)
	}
}
