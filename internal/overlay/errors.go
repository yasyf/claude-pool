package overlay

import "errors"

// This file holds untagged sentinels for the two fuse failure modes callers
// must classify without string matching (the mount-holder server maps them
// onto wire error classes). They compile in every build variant so a non-fuse
// binary can still errors.Is against errors that crossed a process boundary.
var (
	// ErrMountNotLive means a fuse mount was issued but never came live —
	// on macOS almost always the one-time "Network Volumes" TCC grant.
	// FuseProvider.Setup wraps its mount-timeout error with it.
	ErrMountNotLive = errors.New("fuse mount did not come up")

	// ErrUnmountWedged means an unmount did not take: the dir is still a live
	// mountpoint and must not be treated as torn down (RemoveAll through it
	// would reach the backing ~/.claude). FuseProvider.Teardown wraps its
	// refusal with it.
	ErrUnmountWedged = errors.New("unmount did not take")
)
