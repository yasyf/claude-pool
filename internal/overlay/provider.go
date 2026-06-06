// Package overlay makes a pool account dir present the live contents of
// ~/.claude with writes shared straight back, so a pooled session sees the same
// projects/skills/settings as plain `claude`. Two interchangeable providers:
//
//   - symlink (default + always-available fallback): symlink each top-level
//     entry of ~/.claude into the account dir.
//   - fuse (preferred when fuse-t is installed; built with -tags fuse): an
//     in-process passthrough mirror mounted via fuse-t.
//
// Both yield the same observable result. A small set of entries is held back
// from sharing because they are instance-local runtime state that would
// conflict across concurrent sessions; see ExcludedEntries.
package overlay

// Kind identifies an overlay provider.
type Kind string

const (
	KindSymlink Kind = "symlink"
	KindFuse    Kind = "fuse"
)

// ExcludedEntries are top-level ~/.claude entries that must NOT be shared
// across accounts. Each excluded entry becomes a private, empty per-account
// directory instead.
//
//   - daemon: Claude Code's own PID-keyed worker supervisor (daemon/roster.json
//     records a supervisorPid + worker registry). Sharing it makes two sessions
//     fight over one supervisor.
//   - ide:    per-process IDE lock/socket files; a pooled session must not
//     advertise itself on acct-00's IDE registry.
var ExcludedEntries = map[string]bool{
	"daemon": true,
	"ide":    true,
}

// skipEntries are never linked or mirrored (noise / OS cruft).
var skipEntries = map[string]bool{
	".DS_Store": true,
}

// Provider establishes and maintains an overlay of base at accountDir.
type Provider interface {
	Kind() Kind

	// Setup makes accountDir reflect base. Idempotent.
	Setup(base, accountDir string) error

	// Sync re-asserts the overlay, picking up new top-level entries in base
	// and repairing drift. Idempotent. Safe to call repeatedly.
	Sync(base, accountDir string) error

	// Health returns nil if the overlay is intact, else a descriptive error.
	Health(base, accountDir string) error

	// Teardown removes the overlay from accountDir. It must never touch base.
	Teardown(base, accountDir string) error
}

// For returns the provider for a given kind. Unknown/empty kinds fall back to
// the symlink provider. The fuse provider is only returned when the binary was
// built with -tags fuse AND fuse-t is usable; otherwise this returns symlink.
func For(kind Kind) Provider {
	if kind == KindFuse {
		if p, ok := fuseProvider(); ok {
			return p
		}
	}
	return &SymlinkProvider{}
}
