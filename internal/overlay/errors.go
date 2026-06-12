package overlay

import (
	"errors"
	"fmt"
	"time"
)

// This file holds untagged sentinels for the fuse failure modes callers must
// classify without string matching (the mount-holder server maps them onto
// wire error classes). They compile in every build variant so a non-fuse
// binary can still errors.Is against errors that crossed a process boundary.
var (
	// ErrMountNotLive means a fuse mount was issued but never came live in a
	// process that has NOT yet hosted any live mount — on macOS almost always
	// the one-time "Network Volumes" TCC grant. FuseProvider.Setup wraps its
	// mount-timeout error with it only while the grant is unproven; once any
	// mount in the process has come live, timeouts wrap ErrMountTimeout
	// instead.
	ErrMountNotLive = errors.New("fuse mount did not come up")

	// ErrMountTimeout means a fuse mount timed out in a process that has
	// ALREADY hosted a live mount, so the macOS "Network Volumes" TCC grant is
	// proven and this is transient fuse-t slowness — never the TCC condition.
	// Callers retry; they must never convert the provider or surface TCC
	// guidance for it. Honest gap: a grant revoked mid-process still reads as
	// this — established NFS mounts survive revocation, there is no public TCC
	// query API for Network Volumes, and attempting a mount is the only
	// observable — and a holder restart resets the deduction.
	ErrMountTimeout = errors.New("fuse mount did not come up in time")

	// ErrUnmountWedged means an unmount did not take: the dir is still a live
	// mountpoint and must not be treated as torn down (RemoveAll through it
	// would reach the backing ~/.claude). FuseProvider.Teardown wraps its
	// refusal with it.
	ErrUnmountWedged = errors.New("unmount did not take")
)

// mountWaitErr composes FuseProvider.Setup's mount-up timeout error. proven
// reports whether this process has already hosted a live mount: unproven
// timeouts presume the one-time "Network Volumes" TCC grant and carry the
// walkthrough (wrapping ErrMountNotLive); proven timeouts are transient
// fuse-t slowness (wrapping ErrMountTimeout) and must never carry TCC
// guidance.
func mountWaitErr(accountDir string, waited time.Duration, proven bool) error {
	if !proven {
		return fmt.Errorf("%w: %s (presumed missing macOS TCC grant: this failed attempt is what creates the toggle under System Settings ▸ Privacy & Security ▸ Network Volumes — grant Network Volumes access once and mounts retry automatically)", ErrMountNotLive, accountDir)
	}
	return fmt.Errorf("%w: %s after %s; this process already hosts live mounts, so the Network Volumes grant is proven — transient fuse-t slowness, retrying", ErrMountTimeout, accountDir, waited)
}
