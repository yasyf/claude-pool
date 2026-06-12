package mountd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrHolderUnavailable means the mount-holder socket could not be reached.
var ErrHolderUnavailable = errors.New("mount holder not running")

// Wire error-class sentinels. The client maps Response.ErrClass onto these so
// drivers classify with errors.Is; the holder's raw Error string — which
// carries the user-facing guidance, e.g. the TCC grant walkthrough — is
// preserved in the returned error's message.
var (
	// ErrTCCDenied: the mount was issued but never came live — almost always
	// the one-time macOS "Network Volumes" grant.
	ErrTCCDenied = errors.New("mount blocked pending TCC grant")
	// ErrMountTimeout: the mount did not come live within the holder's
	// bounded wait in a process whose "Network Volumes" grant is already
	// proven — NOT the TCC condition. Transient; drivers retry, never surface
	// TCC guidance for it, and must never treat it as grounds to convert.
	ErrMountTimeout = errors.New("mount timed out under a proven grant")
	// ErrMountFailed: the mount failed outright.
	ErrMountFailed = errors.New("mount failed")
	// ErrUnmountWedged: the unmount did not take; the dir is still a live
	// mountpoint and must not be treated as torn down.
	ErrUnmountWedged = errors.New("unmount wedged")
	// ErrForeignMount: the dir is a mountpoint the holder does not own; it
	// must be unmounted before the holder will mount there.
	ErrForeignMount = errors.New("foreign mount in the way")
	// ErrBusy: another operation is in flight on the same dir. Transient;
	// safe to retry once it completes.
	ErrBusy = errors.New("dir busy with another operation")
	// ErrBaseMismatch: the dir is already mounted by the holder but mirrors a
	// different base; it must be unmounted before mounting the new base.
	// Unreachable for canonical-base callers (every production mount uses
	// pool.ClaudeDir(), and a different HOME relocates the socket itself).
	// Registry state, never a mount verdict: drivers unmount-then-retry —
	// handleUnmount tears down by the REGISTERED base — exactly like
	// ErrForeignMount, and must never treat it as grounds to convert.
	ErrBaseMismatch = errors.New("dir already mirrors a different base")
	// ErrUnknownClass: the holder sent an error class this client predates
	// (forward skew: a newer holder behind an older driver — the protocol's
	// sanctioned evolution path, since new failure modes get new classes).
	// The condition is unclassifiable, so drivers must fail toward retry:
	// treating it as a genuine mount failure would let additive protocol
	// evolution trigger the one action a holder blip must never trigger
	// (fuse→symlink conversion). The raw class and the holder's message stay
	// in the error text.
	ErrUnknownClass = errors.New("unrecognized holder error class")
)

// Client is a short-lived connection to the mount-holder socket.
type Client struct {
	// Socket is the holder's unix socket path.
	Socket string
}

// NewClient returns a client for the given holder socket path.
func NewClient(socket string) *Client { return &Client{Socket: socket} }

// Available reports whether the holder socket accepts a connection.
func (c *Client) Available() bool {
	conn, err := net.DialTimeout("unix", c.Socket, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// do sends one request and reads one response.
func (c *Client) do(req Request, timeout time.Duration) (*Response, error) {
	conn, err := net.DialTimeout("unix", c.Socket, 500*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHolderUnavailable, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	req.Proto = MountProtoVersion
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, wireErr("send request", err)
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return nil, wireErr("read response", err)
	}
	return &resp, nil
}

// wireErr wraps one I/O failure on an established holder connection. Every
// such failure — a blown deadline, an EOF or connection reset from a holder
// that died mid-request (a fuse-t fault inside Setup kills it at exactly that
// point) — leaves the op's outcome unknown. To callers that is the same
// condition as an unreachable holder, so it maps to ErrHolderUnavailable just
// like a dial failure; it must never read as an op-level failure class. The
// underlying error stays in the chain.
func wireErr(stage string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrHolderUnavailable, stage, err)
}

// respErr converts a failed response into an error: the matching class
// sentinel wrapped around the holder's raw message, ErrUnknownClass for a
// class this client predates (forward skew — retryable, never convertible),
// or a plain error when the holder sent no class at all.
func respErr(resp *Response) error {
	if resp.OK {
		return nil
	}
	var sentinel error
	switch resp.ErrClass {
	case ClassTCC:
		sentinel = ErrTCCDenied
	case ClassMountTimeout:
		sentinel = ErrMountTimeout
	case ClassMountFailed:
		sentinel = ErrMountFailed
	case ClassWedged:
		sentinel = ErrUnmountWedged
	case ClassForeignMount:
		sentinel = ErrForeignMount
	case ClassBusy:
		sentinel = ErrBusy
	case ClassBaseMismatch:
		sentinel = ErrBaseMismatch
	case "":
		return errors.New(resp.Error)
	default:
		return fmt.Errorf("%w (%s): %s", ErrUnknownClass, resp.ErrClass, resp.Error)
	}
	return fmt.Errorf("%w: %s", sentinel, resp.Error)
}

// Health probes the holder, returning its version.
func (c *Client) Health() (string, error) {
	resp, err := c.do(Request{Op: OpHealth}, 2*time.Second)
	if err != nil {
		return "", err
	}
	if err := respErr(resp); err != nil {
		return "", err
	}
	return resp.Version, nil
}

// Probe asks the holder whether it can host fuse mounts. The holder performs
// a real throwaway mount (which may sit on the one-time TCC prompt), hence
// the long timeout.
func (c *Client) Probe() (bool, error) {
	resp, err := c.do(Request{Op: OpProbe}, 25*time.Second)
	if err != nil {
		return false, err
	}
	if err := respErr(resp); err != nil {
		return false, err
	}
	return resp.FuseOK, nil
}

// Mount asks the holder to ensure a live mirror of base at dir: a fresh dir
// is mounted, the exact pair already held AND live is an idempotent OK, and a
// held-but-dead (or deep-wedged) mirror is torn down and remounted.
// errors.Is classes: ErrTCCDenied, ErrMountTimeout, ErrMountFailed,
// ErrForeignMount, ErrBaseMismatch, ErrBusy, and ErrUnmountWedged (a dead
// mirror whose corpse would not come down).
func (c *Client) Mount(base, dir string) error {
	// 25s sits above the server's 20s OpMount deadline — like Shutdown's 65s
	// over the server's 60s — so the op deadline, not the client deadline, is
	// the binding bound. The holder's worst-case single leg is a first mount
	// in a fresh holder: the liveness probe (≤2s liveProbeTimeout) plus the
	// 14s first-mount wait plus the 3s done-drain (overlay/fuse.go) = 19s,
	// under the 20s op deadline; a dead-mirror teardown's ~5s of grace timers
	// plus a proven-grant remount's 8s wait + 3s drain fits the same bound.
	// A blown client deadline maps to ErrHolderUnavailable (wireErr): the
	// holder's real error class would never reach the driver.
	resp, err := c.do(Request{Op: OpMount, Base: base, Dir: dir}, 25*time.Second)
	if err != nil {
		return err
	}
	return respErr(resp)
}

// Unmount asks the holder to unmount the mirror at dir. base is required even
// for a mount the holder has no record of (a dead holder's carcass): teardown
// refuses base==dir, so it must know base. A dir that is not mounted at all
// is an OK no-op. errors.Is classes: ErrUnmountWedged, ErrBusy.
func (c *Client) Unmount(base, dir string) error {
	// 17s sits above the server's 15s OpUnmount deadline (the Mount 25/20 and
	// Shutdown 65/60 pattern): a slow wedge — the provider's grace timers plus
	// a bounded post-force liveness stat — must report ClassWedged, not blow
	// the client deadline into ErrHolderUnavailable and mask the class R3
	// wants propagated.
	resp, err := c.do(Request{Op: OpUnmount, Base: base, Dir: dir}, 17*time.Second)
	if err != nil {
		return err
	}
	return respErr(resp)
}

// List returns the mounts the holder owns, with per-entry kernel liveness.
func (c *Client) List() ([]MountInfo, error) {
	resp, err := c.do(Request{Op: OpList}, 3*time.Second)
	if err != nil {
		return nil, err
	}
	if err := respErr(resp); err != nil {
		return nil, err
	}
	return resp.Mounts, nil
}

// Shutdown asks the holder to unmount everything and exit. It returns the
// dirs that failed to come down — empty means a clean sweep. Use WaitGone to
// confirm the holder actually released the socket.
func (c *Client) Shutdown() ([]MountInfo, error) {
	resp, err := c.do(Request{Op: OpShutdown}, 65*time.Second)
	if err != nil {
		return nil, err
	}
	if err := respErr(resp); err != nil {
		return nil, err
	}
	return resp.Mounts, nil
}

// WaitGone polls until the socket stops accepting connections or timeout
// elapses, reporting whether it went dead.
func (c *Client) WaitGone(timeout time.Duration) bool {
	return c.WaitGoneContext(context.Background(), timeout)
}

// WaitGoneContext is WaitGone bounded by ctx as well as timeout: a daemon
// shutting down mid-wait must not stall its exit for the full timeout (the
// skew replace waits ~70s per leg). Kernel truth wins over cancellation — a
// socket observed dead reports true even under a done ctx; a still-live
// socket reports false as soon as ctx ends.
func (c *Client) WaitGoneContext(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", c.Socket, 200*time.Millisecond)
		if err != nil {
			return true
		}
		conn.Close()
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false
}
