// Package pool owns the canonical filesystem layout, account model, and
// per-account lifecycle helpers for cc-pool.
//
// Two distinct trees exist and must not be confused:
//
//   - ~/.claude      The canonical Claude Code config dir: plain `claude`'s
//     home and the shared overlay base. NEVER moved, never
//     registered as a pool account, never read or written — the
//     pool never touches plain claude's credential or state.
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

// ClaudeDir is the canonical Claude config dir (~/.claude): plain `claude`'s
// home and the shared overlay base — never a pool account.
func ClaudeDir() string {
	return filepath.Join(mustHome(), ".claude")
}

// ClaudeJSONPath is plain claude's primary state file (~/.claude.json — in
// $HOME, NOT inside ~/.claude). With CLAUDE_CONFIG_DIR set, claude reads and
// writes $CONFIG_DIR/.claude.json instead; new accounts are seeded from this
// file so they inherit onboarding state and settings.
func ClaudeJSONPath() string {
	return filepath.Join(mustHome(), ".claude.json")
}

// AccountsDir is the parent of all account dirs (~/.cc-pool/accounts).
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

// AccountDir returns the config-dir path for account index n (n >= 1).
//
// The returned path is exactly the string ccp emits for CLAUDE_CONFIG_DIR and
// the string we hash for the per-dir Keychain service name; the two MUST stay
// byte-identical, so do not realpath or normalize divergently elsewhere.
func AccountDir(n int) string {
	if n < 1 {
		panic(fmt.Sprintf("AccountDir(%d): account indexes start at 1", n))
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
