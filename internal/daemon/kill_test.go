package daemon

import (
	"errors"
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

// setKillSeams overrides holderPID and killProc for the duration of a test,
// restoring both afterward so no test ever signals a real process.
func setKillSeams(t *testing.T, hp func(*Client) (int, error), kp func(int, syscall.Signal) error) {
	t.Helper()
	oldHP, oldKP := holderPID, killProc
	holderPID, killProc = hp, kp
	t.Cleanup(func() { holderPID, killProc = oldHP, oldKP })
}

// TestKillHolderSpares pins that we never signal our own process, pid 0, or
// pid 1 — the holder PID is read from peer credentials, but a bug there must
// never turn into a self-kill or an init-kill.
func TestKillHolderSpares(t *testing.T) {
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
			setKillSeams(t,
				func(*Client) (int, error) { return tc.pid, nil },
				func(int, syscall.Signal) error { called = true; return nil })
			pid, err := NewClient().KillHolder()
			if pid != 0 || err != nil {
				t.Fatalf("KillHolder(%s) = (%d, %v), want (0, nil)", tc.name, pid, err)
			}
			if called {
				t.Fatalf("killProc was called for spared pid %d", tc.pid)
			}
		})
	}
}

// TestKillHolderSignals pins the signal sent and the error handling: a live
// holder gets a SIGKILL, ESRCH (already dead) is success, EPERM surfaces.
func TestKillHolderSignals(t *testing.T) {
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
			setKillSeams(t,
				func(*Client) (int, error) { return 999001, nil },
				func(pid int, sig syscall.Signal) error { got = killCall{pid, sig}; return tc.killErr })
			pid, err := NewClient().KillHolder()
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

// TestKillHolderHolderPIDError pins that an unreachable holder is reported, not
// turned into a kill of pid 0.
func TestKillHolderHolderPIDError(t *testing.T) {
	called := false
	setKillSeams(t,
		func(*Client) (int, error) { return 0, ErrDaemonUnavailable },
		func(int, syscall.Signal) error { called = true; return nil })
	pid, err := NewClient().KillHolder()
	if pid != 0 || !errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("KillHolder = (%d, %v), want (0, ErrDaemonUnavailable)", pid, err)
	}
	if called {
		t.Fatal("killProc was called despite a holderPID error")
	}
}

// TestPeerPID pins the getsockopt(LOCAL_PEERPID) plumbing: against an
// in-process fake daemon the peer is us, so the call must return our own pid.
func TestPeerPID(t *testing.T) {
	f := newFakeDaemon(t, "0.0.0-old", false)
	c := &Client{socket: f.socket}
	pid, err := c.peerPID()
	if err != nil {
		t.Fatalf("peerPID: %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("peerPID = %d, want our own pid %d (the fake daemon is in-process)", pid, os.Getpid())
	}
}

// TestPeerPIDUnavailable pins that a missing socket reports ErrDaemonUnavailable
// rather than a bogus pid.
func TestPeerPIDUnavailable(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ccp-pp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	c := &Client{socket: filepath.Join(dir, "nope.sock")}
	if _, err := c.peerPID(); !errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("peerPID on a missing socket: err = %v, want ErrDaemonUnavailable", err)
	}
}
