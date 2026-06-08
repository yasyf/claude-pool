package daemon

import (
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
	"github.com/yasyf/cc-pool/internal/version"
)

// fakeDaemon is a minimal stand-in for a running daemon holding the socket: it
// answers Health/Shutdown with a fixed version and, on OpShutdown, optionally
// stops listening (releasing the socket) to mimic a daemon stepping down. macOS
// caps sun_path at 104 bytes, so the socket lives under a short /tmp dir.
type fakeDaemon struct {
	ln            net.Listener
	socket        string
	version       string
	releaseOnStop bool
}

func newFakeDaemon(t *testing.T, ver string, releaseOnStop bool) *fakeDaemon {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "ccp-fake")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDaemon{ln: ln, socket: socket, version: ver, releaseOnStop: releaseOnStop}
	go f.serve()
	t.Cleanup(func() { f.ln.Close() })
	return f
}

func (f *fakeDaemon) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return // listener closed: defined exit
		}
		var req Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			conn.Close() // probe dial (e.g. WaitGone) with no request body
			continue
		}
		_ = json.NewEncoder(conn).Encode(Response{OK: true, Version: f.version})
		conn.Close()
		if req.Op == OpShutdown && f.releaseOnStop {
			f.ln.Close() // release the socket so a successor can rebind
			return
		}
	}
}

func testServer(socket string, evict time.Duration) *Server {
	return &Server{
		socket:       socket,
		log:          log.New(io.Discard, "", 0),
		evictTimeout: evict,
	}
}

// TestListenRefusesSameVersionHolder pins the genuine-double-start guard: a live
// holder at our own version is refused, never evicted.
func TestListenRefusesSameVersionHolder(t *testing.T) {
	f := newFakeDaemon(t, version.String(), false)
	s := testServer(f.socket, time.Second)
	if _, err := s.listen(); err == nil || !strings.Contains(err.Error(), "same version") {
		t.Fatalf("listen against a same-version holder: err = %v, want a 'same version' refusal", err)
	}
}

// TestListenEvictsSkewedHolder pins the load-bearing fix: a version-skewed
// holder that steps down on OpShutdown is evicted and the successor binds.
func TestListenEvictsSkewedHolder(t *testing.T) {
	f := newFakeDaemon(t, "0.0.0-old", true)
	s := testServer(f.socket, 3*time.Second)
	ln, err := s.listen()
	if err != nil {
		t.Fatalf("listen should evict the skewed holder and bind, got err = %v", err)
	}
	defer ln.Close()
	if ln == nil {
		t.Fatal("listen returned a nil listener after eviction")
	}
}

// TestListenSkewedHolderIgnoresShutdown pins the bounded-wait failure: a skewed
// holder that acknowledges but never releases times out (rather than wedging),
// so the successor exits and launchd retries.
func TestListenSkewedHolderIgnoresShutdown(t *testing.T) {
	f := newFakeDaemon(t, "0.0.0-old", false) // acks OpShutdown but keeps listening
	s := testServer(f.socket, 500*time.Millisecond)
	if _, err := s.listen(); err == nil || !strings.Contains(err.Error(), "did not release") {
		t.Fatalf("listen against a holder that ignores shutdown: err = %v, want a 'did not release' timeout", err)
	}
}

// TestHandleShutdownEndsServe pins the OpShutdown op end-to-end: a real serve
// loop steps down on the request, returns, and releases the socket.
func TestHandleShutdownEndsServe(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("/tmp", "ccp-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })

	s := &Server{
		m:            &pool.Manager{Store: st},
		socket:       filepath.Join(sockDir, "d.sock"),
		log:          log.New(io.Discard, "", 0),
		evictTimeout: defaultEvictTimeout,
		reservations: map[int]time.Time{},
		rlStreak:     map[int]int{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { err := s.serve(ctx); st.Close(); done <- err }()

	cl := &Client{socket: s.socket}
	deadline := time.Now().Add(5 * time.Second)
	for !cl.Available() {
		if time.Now().After(deadline) {
			t.Fatal("daemon socket never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}

	resp, err := cl.Shutdown()
	if err != nil || !resp.OK {
		t.Fatalf("shutdown: resp = %+v, err = %v", resp, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after OpShutdown")
	}
	if !cl.WaitGone(2 * time.Second) {
		t.Fatal("socket still live after shutdown")
	}
}

// TestWaitGone pins both arms: false while the socket is live, true once it is
// closed.
func TestWaitGone(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "ccp-wg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	cl := &Client{socket: socket}

	if cl.WaitGone(300 * time.Millisecond) {
		t.Fatal("WaitGone reported gone while the socket is live")
	}
	ln.Close()
	if !cl.WaitGone(2 * time.Second) {
		t.Fatal("WaitGone did not report gone after the socket was closed")
	}
}
