package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/store"
)

// newTestServer builds a Server over a temp-dir store with two accounts:
// acct-1 emptier (util 10) than acct-2 (util 50), both freshly sampled. The
// temp config dirs guarantee procscan can never attribute a
// real claude process to them, and the empty fake keychain makes any
// best-effort preflight refresh a harmless miss.
func newTestServer(t *testing.T) (*Server, map[int]string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	dirs := map[int]string{}
	now := time.Now()
	for id, util := range map[int]float64{1: 10, 2: 50} {
		dir := filepath.Join(t.TempDir(), "acct")
		dirs[id] = dir
		if err := st.UpsertAccount(store.Account{
			ID: id, ConfigDir: dir,
			KeychainService: "ccp-test-missing", KeychainAccount: "ccp-test",
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertUsageSample(store.UsageSample{AccountID: id, TS: now, Util5h: util, Util7d: util}); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{
		m: &pool.Manager{
			Store: st, OAuth: &fakeOAuth{}, Keychain: newFakeKeychain(), LockDir: t.TempDir(),
		},
		log:          log.New(io.Discard, "", 0),
		reservations: map[int]time.Time{},
		rlStreak:     map[int]int{},
	}, dirs
}

func TestReservedCountExpiresAfterTTL(t *testing.T) {
	s := &Server{reservations: map[int]time.Time{}}

	if got := s.reservedCount(1); got != 0 {
		t.Fatalf("reservedCount before reserve = %d, want 0", got)
	}

	s.reserve(1)
	if got := s.reservedCount(1); got != 1 {
		t.Fatalf("reservedCount after reserve = %d, want 1", got)
	}

	// Backdate past the TTL: the reservation must read as expired AND be pruned.
	s.mu.Lock()
	s.reservations[1] = time.Now().Add(-reservationTTL - time.Second)
	s.mu.Unlock()
	if got := s.reservedCount(1); got != 0 {
		t.Fatalf("reservedCount after TTL = %d, want 0", got)
	}
	s.mu.Lock()
	_, ok := s.reservations[1]
	s.mu.Unlock()
	if ok {
		t.Fatal("expired reservation was not deleted")
	}
}

func TestHandleSelectRecordsSticky(t *testing.T) {
	s, dirs := newTestServer(t)
	resp := s.handleSelect(t.Context(), Request{Op: OpSelect, NoMark: true, Cwd: "/proj"})
	if !resp.OK || resp.Dir != dirs[1] {
		t.Fatalf("expected emptier acct-1 (%s), got %+v", dirs[1], resp)
	}
	if resp.Sticky {
		t.Fatal("first select must not report sticky")
	}
	st, ok, err := s.m.Store.GetSticky("/proj")
	if err != nil || !ok || st.AccountID != 1 {
		t.Fatalf("winner not recorded: %+v ok=%v err=%v", st, ok, err)
	}
}

func TestHandleSelectHonorsSticky(t *testing.T) {
	s, dirs := newTestServer(t)
	// Sticky points at the WORSE account; it must still win.
	if err := s.m.Store.UpsertSticky("/proj", 2, time.Now()); err != nil {
		t.Fatal(err)
	}
	resp := s.handleSelect(t.Context(), Request{Op: OpSelect, NoMark: true, Cwd: "/proj"})
	if !resp.OK || resp.Dir != dirs[2] || !resp.Sticky {
		t.Fatalf("expected sticky acct-2 (%s), got %+v", dirs[2], resp)
	}
}

func TestHandleSelectForcedRecordsSticky(t *testing.T) {
	s, dirs := newTestServer(t)
	acct := 2
	resp := s.handleSelect(t.Context(), Request{Op: OpSelect, Account: &acct, Cwd: "/proj"})
	if !resp.OK || resp.Dir != dirs[2] {
		t.Fatalf("expected forced acct-2 (%s), got %+v", dirs[2], resp)
	}
	if resp.Sticky {
		t.Fatal("forced select must not report sticky (ranking was not overridden)")
	}
	st, ok, err := s.m.Store.GetSticky("/proj")
	if err != nil || !ok || st.AccountID != 2 {
		t.Fatalf("forced account not recorded: %+v ok=%v err=%v", st, ok, err)
	}
}

// TestServeDrainsInFlightHandlerOnShutdown pins the shutdown ordering: serve
// must wait for in-flight request handlers before returning (after which Run's
// deferred m.Close() closes the database under them).
//
// Synchronization is structural, not sleep-based: a first connection is parked
// mid-request (its handler is wg-tracked the moment the accept loop dequeues
// it), then a second connection completes a full health round-trip. The accept
// loop is sequential and unix sockets accept FIFO, so the health response
// proves the parked connection was already accepted and tracked — only then is
// the ctx cancelled.
func TestServeDrainsInFlightHandlerOnShutdown(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	// macOS caps sun_path at 104 bytes; t.TempDir's /var/folders/... path plus
	// the long test name exceeds it, so the socket gets its own short dir.
	sockDir, err := os.MkdirTemp("/tmp", "ccp-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })

	var logBuf bytes.Buffer
	s := &Server{
		m:            &pool.Manager{Store: st},
		socket:       filepath.Join(sockDir, "d.sock"),
		log:          log.New(&logBuf, "", 0),
		reservations: map[int]time.Time{},
		rlStreak:     map[int]int{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var serveErr, closeErr error
	done := make(chan struct{})
	go func() {
		// Mirror Run's defer ordering: the DB closes as soon as serve returns.
		serveErr = s.serve(ctx)
		closeErr = st.Close()
		close(done)
	}()

	dial := func() net.Conn {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err := net.Dial("unix", s.socket)
			if err == nil {
				return conn
			}
			if time.Now().After(deadline) {
				t.Fatalf("dial daemon socket: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Park a handler mid-request: it blocks in Decode awaiting the closing brace.
	parked := dial()
	defer parked.Close()
	if _, err := parked.Write([]byte(`{"op":"status"`)); err != nil {
		t.Fatal(err)
	}

	// A full round-trip on a second connection orders the parked connection's
	// accept (and wg tracking) before the cancellation below.
	probe := dial()
	defer probe.Close()
	if _, err := probe.Write([]byte(`{"op":"health"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	var health Response
	if err := json.NewDecoder(probe).Decode(&health); err != nil || !health.OK {
		t.Fatalf("health probe failed: %+v err=%v", health, err)
	}

	cancel()

	// The structural drain assertion: with a handler still parked, serve must
	// not return. Without handler tracking, wg.Wait sees only the (instantly
	// exiting) scheduler and serve returns within this window deterministically.
	select {
	case <-done:
		t.Fatal("serve returned while a handler was still in flight")
	case <-time.After(300 * time.Millisecond):
	}

	// Finish the parked request after shutdown began; the drain must let it
	// complete against a still-open DB.
	if _, err := parked.Write([]byte("}\n")); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.NewDecoder(parked).Decode(&resp); err != nil {
		t.Fatalf("decode in-flight response: %v", err)
	}
	if !resp.OK || resp.Error != "" {
		t.Fatalf("in-flight request failed during shutdown: %+v", resp)
	}

	<-done
	if serveErr != nil {
		t.Fatalf("serve: %v", serveErr)
	}
	if closeErr != nil {
		t.Fatalf("store close: %v", closeErr)
	}
	// logBuf is safe to read here: every writer goroutine exited before done.
	if strings.Contains(logBuf.String(), "database is closed") {
		t.Fatalf("teardown raced an in-flight handler:\n%s", logBuf.String())
	}
}
