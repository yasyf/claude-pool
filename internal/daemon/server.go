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

// Server is the running daemon.
type Server struct {
	m      *pool.Manager
	socket string
	log    *log.Logger

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

	// Stamp our OAuth User-Agent with the detected claude version so polling
	// looks like the official client.
	oauth.SetUserAgentVersion(detectClaudeVersion())

	s := &Server{
		m:            m,
		socket:       pool.SocketPath(),
		log:          log.New(os.Stderr, "[cc-pool] ", log.LstdFlags),
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
	// No defer os.Remove(s.socket): *net.UnixListener unlinks the socket file on
	// Close. An explicit Remove here would race a restart — a successor daemon
	// that bound a fresh socket in the gap would have its live socket deleted.
	defer ln.Close()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s.log.Printf("daemon %s started; socket=%s", version.String(), s.socket)
	s.establishMounts()

	// Scheduler.
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.scheduler(ctx) }()

	// Accept loop.
	go func() {
		<-ctx.Done()
		ln.Close()
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

// listen binds the unix socket with 0600 perms, refusing to start if another
// live daemon already owns it.
func (s *Server) listen() (net.Listener, error) {
	if conn, err := net.DialTimeout("unix", s.socket, 300*time.Millisecond); err == nil {
		conn.Close()
		return nil, errors.New("another cc-pool daemon is already running")
	}
	_ = os.Remove(s.socket) // clear a stale socket
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
		return s.handleCheckin(req)
	default:
		return Response{OK: false, Error: "unknown op: " + string(req.Op)}
	}
}

// handleStatus returns scored snapshots from cached samples (no live fetch).
func (s *Server) handleStatus(ctx context.Context) Response {
	snaps, err := s.m.Snapshots(ctx, false, 0)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	return Response{OK: true, Accounts: toStatuses(snaps)}
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
				return Response{OK: true, Dir: sn.Account.ConfigDir, SelectedID: &id}
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
	// `clp run`'s no-mark select too.
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
	return Response{OK: true, Dir: best.Account.ConfigDir, SelectedID: &id, Sticky: sticky}
}

// recordSticky upserts the cwd->account sticky record, logging (not failing)
// on error.
func (s *Server) recordSticky(cwd string, accountID int) {
	if err := s.m.RecordSticky(cwd, accountID, time.Now()); err != nil {
		s.log.Printf("record sticky for %s: %v", cwd, err)
	}
}

// handleCheckin closes sessions for a pid and adopts any rotated token.
func (s *Server) handleCheckin(req Request) Response {
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
			if err := s.m.AdoptRotatedToken(a); err != nil {
				s.log.Printf("acct-%02d adopt rotated token on checkin: %v", a.ID, err)
			}
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
			IsZero:         sn.Account.IsZero,
			OverlayKind:    sn.Account.OverlayKind,
			Score:          sn.Score,
			Remaining5h:    sn.Remaining5h,
			Remaining7d:    sn.Remaining7d,
			ActiveSessions: sn.ActiveSessions,
			RateLimited:    sn.RateLimited,
			Stale:          sn.Stale,
			Resets5h:       sn.Resets5h,
			SampleAge:      sn.SampleAge.Round(time.Second).String(),
		})
	}
	return out
}

// establishMounts brings up fuse mounts for fuse-kind accounts at startup.
func (s *Server) establishMounts() {
	accts, err := s.m.Store.ListAccounts()
	if err != nil {
		return
	}
	for _, a := range accts {
		if a.IsZero || a.OverlayKind != string(overlay.KindFuse) {
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
		if a.IsZero || a.OverlayKind != string(overlay.KindFuse) {
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
