package mountd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/cc-pool/internal/overlay"
)

// fakeHolderEnv flips the spawned test binary into a fast-failing holder, so
// TestEnsureRunningSpawnTimesOut can exercise the real fork without the child
// recursively running this suite (and re-spawning grandchildren).
const fakeHolderEnv = "CCP_MOUNTD_TEST_FAKE_HOLDER"

// TestMain doubles as the spawned mount-holder when EnsureRunning's real
// spawn path is under test: holderCmd execs THIS test binary, and the env var
// turns it into a holder that dies before ever serving the socket.
func TestMain(m *testing.M) {
	if os.Getenv(fakeHolderEnv) == "1" {
		fmt.Fprintln(os.Stderr, "fake holder: exiting without serving")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestHolderCmd(t *testing.T) {
	socket := "/tmp/ccp-test/m.sock"
	logPath := filepath.Join(t.TempDir(), "holder.log")

	cmd, logFile, err := holderCmd(socket, logPath)
	if err != nil {
		t.Fatalf("holderCmd: %v", err)
	}
	defer logFile.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{exe, "mount-holder", "--socket", socket}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Errorf("argv = %q, want %q", cmd.Args, wantArgs)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Errorf("SysProcAttr = %+v, want Setsid", cmd.SysProcAttr)
	}
	if cmd.Stdin != nil {
		t.Errorf("Stdin = %v, want nil", cmd.Stdin)
	}
	if cmd.Stdout != logFile || cmd.Stderr != logFile {
		t.Errorf("Stdout/Stderr = %v/%v, want the log file %v", cmd.Stdout, cmd.Stderr, logFile)
	}
	if logFile.Name() != logPath {
		t.Errorf("log file = %q, want %q", logFile.Name(), logPath)
	}
	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("log perms = %o, want 0600", got)
	}
}

func TestHolderCmdUnopenableLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "missing", "holder.log")
	if _, _, err := holderCmd("/tmp/ccp-test/m.sock", logPath); err == nil {
		t.Fatal("holderCmd with an unopenable log path succeeded, want error")
	}
}

func TestEnsureRunningShortCircuitsWhenAvailable(t *testing.T) {
	socket := filepath.Join(shortSockDir(t), "m.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// The log path is deliberately unopenable: any spawn attempt would fail
	// loudly inside holderCmd, so a nil return proves the short-circuit.
	logPath := filepath.Join(t.TempDir(), "missing", "holder.log")
	if err := EnsureRunning(socket, logPath, time.Second); err != nil {
		t.Fatalf("EnsureRunning with a live socket = %v, want nil", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("log file stat = %v, want not-exist (no spawn)", err)
	}
}

func TestEnsureRunningPureBuildFailsLoudWithoutSpawning(t *testing.T) {
	if overlay.FuseBuilt() {
		t.Skip("fuse build can spawn a holder; the refusal path is pure-build only")
	}
	socket := filepath.Join(shortSockDir(t), "m.sock") // nothing listening
	logPath := filepath.Join(t.TempDir(), "holder.log")

	err := EnsureRunning(socket, logPath, time.Second)
	if err == nil {
		t.Fatal("EnsureRunning in a pure build with no holder succeeded, want error")
	}
	if !strings.Contains(err.Error(), "cannot host fuse mounts") {
		t.Errorf("error = %q, want it to say this binary cannot host fuse mounts", err)
	}
	// The guidance must match reality (Formula/cc-pool.rb): nothing bundles
	// fuse-t — the user installs the cask, then reinstalls to get the fuse
	// variant.
	if !strings.Contains(err.Error(), "brew install macos-fuse-t/cask/fuse-t") {
		t.Errorf("error = %q, want the fuse-t cask install command", err)
	}
	if !strings.Contains(err.Error(), "brew reinstall cc-pool") {
		t.Errorf("error = %q, want the cc-pool reinstall step", err)
	}
	// Deliberately NOT ErrHolderUnavailable: a binary that can never host or
	// spawn a holder is a permanent condition, and an unwrapped error is what
	// lets a pure-build daemon take its gated retreat from fuse to symlink
	// instead of retrying forever.
	if errors.Is(err, ErrHolderUnavailable) {
		t.Errorf("error = %v, want the pure-build refusal NOT classified as holder-unavailable", err)
	}
	if _, statErr := os.Stat(logPath); !os.IsNotExist(statErr) {
		t.Errorf("log file stat = %v, want not-exist (no spawn attempted)", statErr)
	}
}

// TestEnsureRunningSpawnFailureClassifiedHolderUnavailable pins the spawn-leg
// failure class: a spawn that cannot even be assembled (here an unopenable log
// path inside holderCmd) is a holder-availability condition, not a mount
// verdict.
func TestEnsureRunningSpawnFailureClassifiedHolderUnavailable(t *testing.T) {
	if !overlay.FuseBuilt() {
		t.Skip("pure build refuses before reaching holderCmd; the spawn leg is fuse-build only")
	}
	socket := filepath.Join(shortSockDir(t), "m.sock") // nothing listening
	logPath := filepath.Join(t.TempDir(), "missing", "holder.log")

	err := EnsureRunning(socket, logPath, time.Second)
	if err == nil {
		t.Fatal("EnsureRunning with an unopenable log path succeeded, want error")
	}
	if !errors.Is(err, ErrHolderUnavailable) {
		t.Errorf("error = %v, want errors.Is ErrHolderUnavailable", err)
	}
}

// TestSpawnedHolderReaped pins the zombie fix: a spawned holder that exits
// must be reaped (waited out), not merely Released. A reaped child disappears
// from the process table — kill(pid, 0) reports ESRCH — while an unreaped one
// would sit as a signalable zombie until this (in production: the daemon's)
// process exits, so the poll below would never see ESRCH.
func TestSpawnedHolderReaped(t *testing.T) {
	t.Setenv(fakeHolderEnv, "1") // the child (this test binary) exits immediately
	socket := filepath.Join(shortSockDir(t), "m.sock")
	logPath := filepath.Join(t.TempDir(), "holder.log")
	cmd, logFile, err := holderCmd(socket, logPath)
	if err != nil {
		t.Fatalf("holderCmd: %v", err)
	}
	defer logFile.Close()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	reap(cmd)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return // reaped: gone from the process table
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("spawned holder pid %d still in the process table: exited child not reaped (zombie)", pid)
}

func TestEnsureRunningSpawnTimesOutOnFastFailingHolder(t *testing.T) {
	if !overlay.FuseBuilt() {
		t.Skip("pure build refuses before spawning; the real spawn path is fuse-build only")
	}
	t.Setenv(fakeHolderEnv, "1") // the child (this test binary) dies before serving
	socket := filepath.Join(shortSockDir(t), "m.sock")
	logPath := filepath.Join(t.TempDir(), "holder.log")

	err := EnsureRunning(socket, logPath, time.Second)
	if err == nil {
		t.Fatal("EnsureRunning with a holder that dies before serving succeeded, want timeout error")
	}
	// The timeout is a holder-availability condition, never a mount verdict:
	// without the sentinel, healFuse's default arm would auto-convert a fuse
	// account to symlink on a slow holder start.
	if !errors.Is(err, ErrHolderUnavailable) {
		t.Errorf("error = %v, want errors.Is ErrHolderUnavailable", err)
	}
	if !strings.Contains(err.Error(), "did not come up on "+socket) {
		t.Errorf("error = %q, want the did-not-come-up copy naming the socket", err)
	}
	if !strings.Contains(err.Error(), "check "+logPath) {
		t.Errorf("error = %q, want it to point at the log %s", err, logPath)
	}
	// The spawn really happened with stderr redirected: the fake holder's
	// parting line must land in the log (poll — the detached child races us).
	deadline := time.Now().Add(5 * time.Second)
	var logData []byte
	for time.Now().Before(deadline) {
		logData, _ = os.ReadFile(logPath)
		if strings.Contains(string(logData), "fake holder") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(string(logData), "fake holder") {
		t.Errorf("holder log = %q, want the fake holder's stderr line", logData)
	}
}
