package daemon

import (
	"errors"
	"testing"
)

// setKillSocketPeer overrides killSocketPeer for the duration of a test,
// restoring it afterward so no test ever signals a real process.
func setKillSocketPeer(t *testing.T, fn func(string) (int, error)) {
	t.Helper()
	old := killSocketPeer
	killSocketPeer = fn
	t.Cleanup(func() { killSocketPeer = old })
}

// TestKillSocketPeerDelegates pins the thin delegation: KillSocketPeer hands
// the client's own socket path — the daemon socket, never the mount-holder's —
// to peerpid.Kill and returns its result unchanged.
func TestKillSocketPeerDelegates(t *testing.T) {
	killErr := errors.New("peer kill failed")
	for _, tc := range []struct {
		name    string
		pid     int
		err     error
		wantPid int
	}{
		{"killed", 999001, nil, 999001},
		{"error-passthrough", 0, killErr, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotSocket string
			setKillSocketPeer(t, func(socket string) (int, error) {
				gotSocket = socket
				return tc.pid, tc.err
			})
			c := &Client{socket: "/tmp/ccp-test/d.sock"}
			pid, err := c.KillSocketPeer()
			if gotSocket != c.socket {
				t.Fatalf("killSocketPeer got socket %q, want the client's daemon socket %q", gotSocket, c.socket)
			}
			if pid != tc.wantPid || !errors.Is(err, tc.err) {
				t.Fatalf("KillSocketPeer = (%d, %v), want (%d, %v)", pid, err, tc.wantPid, tc.err)
			}
		})
	}
}
