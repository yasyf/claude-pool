package overlay

// Detect chooses the best overlay provider for this machine. It prefers fuse
// (a live mirror that auto-includes new entries) when the binary was built with
// -tags fuse AND a throwaway probe-mount via fuse-t succeeds — which also walks
// the user through the one-time "Network Volumes" privacy grant. Otherwise it
// falls back to the always-available symlink provider.
func Detect() Kind {
	if probeFuse() {
		return KindFuse
	}
	return KindSymlink
}

// FuseBuilt reports whether this binary includes the fuse provider at all.
func FuseBuilt() bool { return fuseBuilt }
