//go:build !fuse

package overlay

// This file is compiled when the binary is built WITHOUT the fuse provider
// (the default). The symlink provider is the only one available.

const fuseBuilt = false

// fuseProvider reports that no fuse provider is available in this build.
func fuseProvider() (Provider, bool) { return nil, false }

// probeFuse always fails without the fuse build tag.
func probeFuse() bool { return false }
