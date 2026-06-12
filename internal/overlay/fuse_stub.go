//go:build !fuse

package overlay

// This file is compiled when the binary is built WITHOUT the fuse provider
// (the default). The symlink provider is the only one available.

const fuseBuilt = false

// InProcessFuse returns the in-process fuse provider, available only in fuse
// builds — never in this one. Once the mount-holder lands it must only be
// consumed by the holder process.
func InProcessFuse() (Provider, bool) { return nil, false }

// HostProbe attempts a throwaway in-process probe mount; it must run in the
// process that will host mounts (capability + TCC grant are per-process). It
// always fails without the fuse build tag.
func HostProbe() bool { return false }
