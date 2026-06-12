// Package peerpid identifies and kills the exact process holding a unix
// socket, via getsockopt(LOCAL_PEERPID). The target is resolved from the
// socket's own peer credentials — never by process name — so a kill can only
// ever land on the process on the other end of the dialed socket.
package peerpid

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ErrUnreachable means the socket could not be dialed, so it has no peer.
var ErrUnreachable = errors.New("socket unreachable")

// lookupPID and killProc are overridable in tests so the kill path can be
// asserted without dialing a real socket or signalling a real process.
var (
	lookupPID = PeerPID
	killProc  = syscall.Kill
)

// Kill force-terminates the process currently holding the unix socket at
// socketPath, identified by its peer credentials (LOCAL_PEERPID) — never by
// name, so it can only target the exact process on the other end of this
// socket. It never signals pid<=1 or the caller's own process. Returns the
// killed pid (0 if the peer is gone or is us) and any error other than ESRCH
// (already dead).
func Kill(socketPath string) (int, error) {
	pid, err := lookupPID(socketPath)
	if err != nil {
		return 0, err
	}
	return killResolved(pid)
}

// KillPid is Kill gated on peer identity: the socket's current peer is
// resolved and compared against wantPID in one step, and the signal — when it
// fires — lands on that same resolved pid, never on a re-resolved one. A
// separate check-then-Kill would re-dial inside Kill and SIGKILL whoever
// holds the socket at kill time, so a successor that bound the socket between
// the check and the kill could be shot; here a mismatched peer is refused
// with no signal sent. Returns the killed pid (0 when nothing was signalled)
// and ErrUnreachable when the socket has no peer at all.
func KillPid(socketPath string, wantPID int) (int, error) {
	pid, err := lookupPID(socketPath)
	if err != nil {
		return 0, err
	}
	if pid != wantPID {
		return 0, fmt.Errorf("socket %s is held by pid %d, not pid %d; refusing to kill", socketPath, pid, wantPID)
	}
	return killResolved(pid)
}

// killResolved SIGKILLs an already-resolved peer pid, sparing pid<=1 and the
// caller's own process; ESRCH (already dead) is success.
func killResolved(pid int) (int, error) {
	if pid <= 1 || pid == os.Getpid() {
		return 0, nil
	}
	if err := killProc(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return pid, fmt.Errorf("kill socket peer pid %d: %w", pid, err)
	}
	return pid, nil
}

// PeerPID reads the pid of the process on the other end of the unix socket at
// socketPath via getsockopt(SOL_LOCAL, LOCAL_PEERPID). ErrUnreachable if the
// socket cannot be dialed.
func PeerPID(socketPath string) (int, error) {
	conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
	if err != nil {
		return 0, ErrUnreachable
	}
	defer conn.Close()
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("syscall conn: %w", err)
	}
	var pid int
	var opErr error
	if err := raw.Control(func(fd uintptr) {
		pid, opErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, fmt.Errorf("control fd: %w", err)
	}
	if opErr != nil {
		return 0, fmt.Errorf("getsockopt LOCAL_PEERPID: %w", opErr)
	}
	return pid, nil
}
