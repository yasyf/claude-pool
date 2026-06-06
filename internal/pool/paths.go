// Package pool owns the canonical filesystem layout, account model, and
// per-account lifecycle helpers for claude-pool.
//
// Two distinct trees exist and must not be confused:
//
//   - ~/.claude          The canonical Claude Code config dir. NEVER moved.
//     It is the shared base AND acct-00, so plain `claude`
//     keeps working untouched.
//   - ~/.claude.pool/    Pool account dirs (acct-01, acct-02, ...). Each is a
//     real, unique path so it gets its own Keychain item.
//   - ~/.claude-pool/    claude-pool's OWN state (sqlite db, daemon socket,
//     logs). Note the hyphen vs the dot above.
package pool

import (
	"fmt"
	"os"
	"path/filepath"
)

// AcctZero is the account index of ~/.claude itself.
const AcctZero = 0

// Home returns the current user's home directory.
func Home() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return h, nil
}

func mustHome() string {
	h, err := Home()
	if err != nil {
		panic(err)
	}
	return h
}

// ClaudeDir is the canonical Claude config dir (~/.claude). It is acct-00 and
// the shared base. It is returned as an absolute, symlink-resolved path so the
// value is stable and matches what we hash for the Keychain mirror item.
func ClaudeDir() string {
	return filepath.Join(mustHome(), ".claude")
}

// PoolDir is the parent of all non-zero account dirs (~/.claude.pool).
func PoolDir() string {
	return filepath.Join(mustHome(), ".claude.pool")
}

// StateDir is claude-pool's own private state directory (~/.claude-pool).
func StateDir() string {
	return filepath.Join(mustHome(), ".claude-pool")
}

// DBPath is the sqlite database path.
func DBPath() string {
	return filepath.Join(StateDir(), "pool.db")
}

// SocketPath is the daemon's unix socket path.
func SocketPath() string {
	return filepath.Join(StateDir(), "daemon.sock")
}

// LogPath is the daemon log path.
func LogPath() string {
	return filepath.Join(StateDir(), "daemon.log")
}

// AccountDirName is the directory basename for account index n (n >= 1).
func AccountDirName(n int) string {
	return fmt.Sprintf("acct-%02d", n)
}

// AccountDir returns the config-dir path for account index n.
//
//   - n == 0 maps to ~/.claude (acct-00, canonical).
//   - n >= 1 maps to ~/.claude.pool/acct-NN.
//
// The returned path is exactly the string clp emits for CLAUDE_CONFIG_DIR and
// the string we hash for the per-dir Keychain service name; the two MUST stay
// byte-identical, so do not realpath or normalize divergently elsewhere.
func AccountDir(n int) string {
	if n == AcctZero {
		return ClaudeDir()
	}
	return filepath.Join(PoolDir(), AccountDirName(n))
}

// EnsureStateDir creates ~/.claude-pool with 0700 perms if missing.
func EnsureStateDir() error {
	return os.MkdirAll(StateDir(), 0o700)
}

// EnsurePoolDir creates ~/.claude.pool with 0700 perms if missing.
func EnsurePoolDir() error {
	return os.MkdirAll(PoolDir(), 0o700)
}
