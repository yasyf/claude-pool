package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// holderPID and killProc are overridable in tests so the kill path can be
// asserted without dialing a real socket or signalling a real process.
var (
	holderPID = func(c *Client) (int, error) { return c.peerPID() }
	killProc  = syscall.Kill
)

// KillHolder force-terminates the process currently holding the daemon socket,
// identified by its peer credentials (LOCAL_PEERPID) — never by name, so it can
// only target the exact process on the other end of this socket. It never
// signals pid<=1 or the caller's own process. Returns the killed pid (0 if the
// holder is gone or is us) and any error other than ESRCH (already dead).
func (c *Client) KillHolder() (int, error) {
	pid, err := holderPID(c)
	if err != nil {
		return 0, err
	}
	if pid <= 1 || pid == os.Getpid() {
		return 0, nil
	}
	if err := killProc(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return pid, fmt.Errorf("kill holder pid %d: %w", pid, err)
	}
	return pid, nil
}

// peerPID reads the pid of the process on the other end of the socket via
// getsockopt(SOL_LOCAL, LOCAL_PEERPID). ErrDaemonUnavailable if unreachable.
func (c *Client) peerPID() (int, error) {
	conn, err := net.DialTimeout("unix", c.socket, 500*time.Millisecond)
	if err != nil {
		return 0, ErrDaemonUnavailable
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
