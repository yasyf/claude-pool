// Package procscan discovers live `claude` sessions and the account each is
// bound to, by reading process environments. On macOS 26 `ps -Eww` prints the
// environment of same-user processes (verified on the target machine), which
// exposes CLAUDE_CONFIG_DIR.
//
// A claude process with no CLAUDE_CONFIG_DIR in its environment is plain
// `claude` on ~/.claude — not a pool session; it maps to no pool account.
package procscan

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Session is a discovered live claude process.
type Session struct {
	PID       int
	ConfigDir string // value of CLAUDE_CONFIG_DIR, or "" for plain claude
}

// psBin and its args are overridable in tests.
var (
	psBin  = "/bin/ps"
	psArgs = []string{"-Eww", "-ax", "-o", "pid=,command="}
)

var configDirRE = regexp.MustCompile(`(?:^|\s)CLAUDE_CONFIG_DIR=(\S+)`)

// Scan returns all live claude sessions. claudeBaseNames are extra argv[0]
// basenames to treat as claude (besides "claude"); pass nil for the default.
func Scan() ([]Session, error) {
	out, err := exec.Command(psBin, psArgs...).Output()
	if err != nil {
		return nil, err
	}
	return parse(string(out)), nil
}

// parse extracts sessions from `ps -Eww -o pid=,command=` output.
func parse(out string) []Session {
	var sessions []Session
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimLeft(line, " ")
		if line == "" {
			continue
		}
		// First field is the pid; the remainder is "command args ENV...".
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		pid, err := strconv.Atoi(line[:sp])
		if err != nil {
			continue
		}
		rest := strings.TrimLeft(line[sp+1:], " ")
		if !isClaudeProcess(rest) {
			continue
		}
		cd := ""
		if m := configDirRE.FindStringSubmatch(rest); m != nil {
			cd = m[1]
		}
		sessions = append(sessions, Session{PID: pid, ConfigDir: cd})
	}
	return sessions
}

// isClaudeProcess reports whether a command line belongs to the claude CLI
// itself (argv[0] basename == "claude"), excluding our own ccp/cc-pool.
func isClaudeProcess(cmd string) bool {
	tok := cmd
	if i := strings.IndexByte(cmd, ' '); i >= 0 {
		tok = cmd[:i]
	}
	base := filepath.Base(tok)
	return base == "claude"
}

// CountByConfigDir counts sessions whose ConfigDir exactly matches configDir.
// An empty configDir matches nothing: no pool account has an empty dir, and
// plain-claude sessions (empty ConfigDir) belong to no pool account.
func CountByConfigDir(sessions []Session, configDir string) int {
	if configDir == "" {
		return 0
	}
	n := 0
	for _, s := range sessions {
		if s.ConfigDir == configDir {
			n++
		}
	}
	return n
}

// AlivePIDs returns the set of pids currently present, for session reconciliation.
func AlivePIDs(sessions []Session) map[int]bool {
	m := make(map[int]bool, len(sessions))
	for _, s := range sessions {
		m[s.PID] = true
	}
	return m
}
