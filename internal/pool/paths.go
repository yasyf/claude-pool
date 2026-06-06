// Package pool owns the canonical filesystem layout, account model, and
// per-account lifecycle helpers for cc-pool.
//
// Two distinct trees exist and must not be confused:
//
//   - ~/.claude      The canonical Claude Code config dir. NEVER moved.
//     It is the shared base AND acct-00, so plain `claude`
//     keeps working untouched.
//   - ~/.cc-pool/    cc-pool's OWN state (sqlite db, daemon socket, logs),
//     plus accounts/ holding the pool account dirs
//     (acct-01, acct-02, ...). Each account dir is a real,
//     unique path so it gets its own Keychain item.
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

// AccountsDir is the parent of all non-zero account dirs (~/.cc-pool/accounts).
func AccountsDir() string {
	return filepath.Join(StateDir(), "accounts")
}

// StateDir is cc-pool's own private state directory (~/.cc-pool).
func StateDir() string {
	return filepath.Join(mustHome(), ".cc-pool")
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
//   - n >= 1 maps to ~/.cc-pool/accounts/acct-NN.
//
// The returned path is exactly the string clp emits for CLAUDE_CONFIG_DIR and
// the string we hash for the per-dir Keychain service name; the two MUST stay
// byte-identical, so do not realpath or normalize divergently elsewhere.
func AccountDir(n int) string {
	if n == AcctZero {
		return ClaudeDir()
	}
	return filepath.Join(AccountsDir(), AccountDirName(n))
}

// EnsureStateDir creates ~/.cc-pool with 0700 perms if missing.
func EnsureStateDir() error {
	return os.MkdirAll(StateDir(), 0o700)
}

// EnsureAccountsDir creates ~/.cc-pool/accounts with 0700 perms if missing.
func EnsureAccountsDir() error {
	return os.MkdirAll(AccountsDir(), 0o700)
}
