package mountd

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/yasyf/cc-pool/internal/overlay"
)

// DefaultSpawnTimeout bounds how long callers wait for a freshly spawned
// holder's socket to come up.
const DefaultSpawnTimeout = 5 * time.Second

// EnsureRunning makes sure a mount holder serves socket, auto-spawning a
// detached `cc-pool mount-holder` and waiting up to timeout for its socket if
// none is reachable. A running holder is usable by ANY build of this binary —
// the mounts live in the holder process — so only the spawn path requires the
// fuse build. A second spawn racing a starting holder is harmless: the holder
// refuses to start if the socket is already owned.
//
// Failure classes: every could-not-start-or-reach-a-holder leg (a spawn that
// fails or whose socket never comes up) wraps ErrHolderUnavailable — it is a
// holder-availability condition, never a mount verdict, so drivers retry
// instead of converting the account (the daemon's healFuse taxonomy keys on
// the sentinel). The pure-build refusal alone is deliberately unwrapped: a
// binary that can never host or spawn a holder is a permanent condition, and
// it is what drives the pure build's documented gated retreat from fuse
// accounts to symlink.
func EnsureRunning(socket, logPath string, timeout time.Duration) error {
	cl := NewClient(socket)
	if cl.Available() {
		return nil
	}
	if !overlay.FuseBuilt() {
		return fmt.Errorf("no mount holder serves %s and this binary cannot host fuse mounts; install fuse-t (brew install macos-fuse-t/cask/fuse-t) then brew reinstall cc-pool to get the fuse build", socket)
	}
	cmd, logFile, err := holderCmd(socket, logPath)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrHolderUnavailable, err)
	}
	// The child holds its own descriptor once started; this one is ours.
	defer logFile.Close()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: spawn mount holder: %w", ErrHolderUnavailable, err)
	}
	reap(cmd)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cl.Available() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%w: mount holder did not come up on %s within %s; check %s", ErrHolderUnavailable, socket, timeout, logPath)
}

// reap waits out a started detached child in the background, so its exit
// never strands a zombie in the spawner's process table. Setsid detaches the
// session, not the parent-child link: the long-lived daemon spawns holders
// from every supervise revival and skew replace, and Process.Release alone
// would leave one defunct entry per exited child (a flock-refusal loser, a
// crash-at-startup backoff attempt, every replaced holder) until the daemon
// itself exits. The goroutine's exit is the child's.
func reap(cmd *exec.Cmd) {
	go func() { _ = cmd.Wait() }()
}

// holderCmd builds the detached mount-holder command: this same binary run as
// `mount-holder --socket <socket>` in its own session, stdout and stderr
// appended to logPath.
func holderCmd(socket, logPath string) (*exec.Cmd, *os.File, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve executable: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open mount holder log: %w", err)
	}
	cmd := exec.Command(exe, "mount-holder", "--socket", socket)
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from our session
	return cmd, logFile, nil
}
