package procscan

import "testing"

const sample = `  501 /Users/me/.local/bin/claude --session-id abc FOO=bar CLAUDE_CONFIG_DIR=/Users/me/.cc-pool/accounts/acct-01 PATH=/usr/bin
  777 claude --dangerously-skip-permissions PATH=/usr/bin LANG=en_US.UTF-8
  888 fish -c claude CLAUDE_CONFIG_DIR=/Users/me/.cc-pool/accounts/acct-02
  999 /usr/bin/node /some/script.js CLAUDE_CONFIG_DIR=/Users/me/.cc-pool/accounts/acct-03
 1010 /Users/me/.local/bin/claude CLAUDE_CONFIG_DIR=/Users/me/.claude
`

func TestParse(t *testing.T) {
	got := parse(sample)
	// Expect: pid 501 (acct-01), pid 777 (default, no env), pid 1010 (~/.claude).
	// pid 888 is `fish` (argv0 != claude). pid 999 is node. Both excluded.
	if len(got) != 3 {
		t.Fatalf("got %d sessions, want 3: %+v", len(got), got)
	}
	byPID := map[int]string{}
	for _, s := range got {
		byPID[s.PID] = s.ConfigDir
	}
	if byPID[501] != "/Users/me/.cc-pool/accounts/acct-01" {
		t.Errorf("pid 501 dir = %q", byPID[501])
	}
	if _, ok := byPID[777]; !ok || byPID[777] != "" {
		t.Errorf("pid 777 should be present with empty config dir, got %q ok=%v", byPID[777], ok)
	}
	if byPID[1010] != "/Users/me/.claude" {
		t.Errorf("pid 1010 dir = %q", byPID[1010])
	}
	if _, ok := byPID[888]; ok {
		t.Error("fish wrapper should be excluded")
	}
	if _, ok := byPID[999]; ok {
		t.Error("node process should be excluded")
	}
}

func TestCountByConfigDir(t *testing.T) {
	sessions := parse(sample)
	cases := map[string]struct {
		configDir string
		want      int
	}{
		"exact match":           {"/Users/me/.cc-pool/accounts/acct-01", 1},
		"no sessions for dir":   {"/Users/me/.cc-pool/accounts/acct-99", 0},
		"explicit ~/.claude":    {"/Users/me/.claude", 1}, // pid 1010 only; never a pool account
		"empty matches nothing": {"", 0},                  // plain claude (pid 777) maps to no pool account
	}
	for name, tc := range cases {
		if n := CountByConfigDir(sessions, tc.configDir); n != tc.want {
			t.Errorf("%s: CountByConfigDir(%q) = %d, want %d", name, tc.configDir, n, tc.want)
		}
	}
}
