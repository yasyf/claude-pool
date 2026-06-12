package peerpid

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// killCall records one killProc invocation so tests can assert the signal and
// target without ever signalling a real process.
type killCall struct {
	pid int
	sig syscall.Signal
}

// setSeams overrides lookupPID and killProc for the duration of a test,
// restoring both afterward so no test ever signals a real process.
func setSeams(t *testing.T, lp func(string) (int, error), kp func(int, syscall.Signal) error) {
	t.Helper()
	oldLP, oldKP := lookupPID, killProc
	lookupPID, killProc = lp, kp
	t.Cleanup(func() { lookupPID, killProc = oldLP, oldKP })
}

// TestKillSpares pins that we never signal our own process, pid 0, or pid 1 —
// the peer PID is read from peer credentials, but a bug there must never turn
// into a self-kill or an init-kill.
func TestKillSpares(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  int
	}{
		{"self", os.Getpid()},
		{"pid0", 0},
		{"pid1", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			setSeams(t,
				func(string) (int, error) { return tc.pid, nil },
				func(int, syscall.Signal) error { called = true; return nil })
			pid, err := Kill("unused.sock")
			if pid != 0 || err != nil {
				t.Fatalf("Kill(%s) = (%d, %v), want (0, nil)", tc.name, pid, err)
			}
			if called {
				t.Fatalf("killProc was called for spared pid %d", tc.pid)
			}
		})
	}
}

// TestKillSignals pins the signal sent and the error handling: a live peer
// gets a SIGKILL, ESRCH (already dead) is success, EPERM surfaces.
func TestKillSignals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		killErr error
		wantErr bool
	}{
		{"alive", nil, false},
		{"already-dead-ESRCH", syscall.ESRCH, false},
		{"no-perm-EPERM", syscall.EPERM, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got killCall
			setSeams(t,
				func(string) (int, error) { return 999001, nil },
				func(pid int, sig syscall.Signal) error { got = killCall{pid, sig}; return tc.killErr })
			pid, err := Kill("unused.sock")
			if pid != 999001 {
				t.Fatalf("killed pid = %d, want 999001", pid)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got.pid != 999001 || got.sig != syscall.SIGKILL {
				t.Fatalf("kill call = %+v, want SIGKILL to 999001", got)
			}
		})
	}
}

// TestKillLookupError pins that an unreachable socket is reported, not turned
// into a kill of pid 0.
func TestKillLookupError(t *testing.T) {
	called := false
	setSeams(t,
		func(string) (int, error) { return 0, ErrUnreachable },
		func(int, syscall.Signal) error { called = true; return nil })
	pid, err := Kill("unused.sock")
	if pid != 0 || !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Kill = (%d, %v), want (0, ErrUnreachable)", pid, err)
	}
	if called {
		t.Fatal("killProc was called despite a lookup error")
	}
}

// TestPeerPID pins the getsockopt(LOCAL_PEERPID) plumbing: against an
// in-process listener the peer is us, so the call must return our own pid.
// macOS caps sun_path at 104 bytes, so the socket lives under a short /tmp dir.
func TestPeerPID(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "ccp-pp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "p.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed: defined exit
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn) // hold the peer end open until the dialer hangs up
	}()

	pid, err := PeerPID(socket)
	if err != nil {
		t.Fatalf("PeerPID: %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("PeerPID = %d, want our own pid %d (the listener is in-process)", pid, os.Getpid())
	}
}

// TestPeerPIDUnreachable pins that a missing socket reports ErrUnreachable
// rather than a bogus pid.
func TestPeerPIDUnreachable(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ccp-pp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if _, err := PeerPID(filepath.Join(dir, "nope.sock")); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("PeerPID on a missing socket: err = %v, want ErrUnreachable", err)
	}
}
