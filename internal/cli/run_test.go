package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/store"
)

func TestExecEnv(t *testing.T) {
	t.Run("appends CLAUDE_CONFIG_DIR when absent", func(t *testing.T) {
		got := execEnv([]string{"PATH=/bin", "HOME=/home/me"}, "/cfg")
		if last := got[len(got)-1]; last != "CLAUDE_CONFIG_DIR=/cfg" {
			t.Errorf("last env = %q, want CLAUDE_CONFIG_DIR=/cfg", last)
		}
		if n := countPrefix(got, "CLAUDE_CONFIG_DIR="); n != 1 {
			t.Errorf("CLAUDE_CONFIG_DIR count = %d, want 1", n)
		}
		if !contains(got, "PATH=/bin") || !contains(got, "HOME=/home/me") {
			t.Errorf("dropped a passthrough var: %v", got)
		}
	})

	t.Run("replaces an existing CLAUDE_CONFIG_DIR without duplicating", func(t *testing.T) {
		got := execEnv([]string{"CLAUDE_CONFIG_DIR=/old", "PATH=/bin"}, "/new")
		if n := countPrefix(got, "CLAUDE_CONFIG_DIR="); n != 1 {
			t.Fatalf("CLAUDE_CONFIG_DIR count = %d, want exactly 1 (no duplicate)", n)
		}
		if contains(got, "CLAUDE_CONFIG_DIR=/old") {
			t.Errorf("stale CLAUDE_CONFIG_DIR=/old survived: %v", got)
		}
		if !contains(got, "CLAUDE_CONFIG_DIR=/new") {
			t.Errorf("CLAUDE_CONFIG_DIR=/new missing: %v", got)
		}
	})
}

// TestResolveRunDirCcpAccount covers the two CCP_ACCOUNT error paths, which
// return before any overlay/Keychain access. The valid-id path is exercised by
// manual verification (it reaches SyncOverlay/PreflightRefresh, which need real
// state and must not be touched here).
func TestResolveRunDirCcpAccount(t *testing.T) {
	m := &pool.Manager{Store: openTestStore(t)}
	cmd := &cobra.Command{}

	t.Run("non-integer is rejected", func(t *testing.T) {
		t.Setenv(ccpAccountEnv, "not-a-number")
		_, err := resolveRunDir(cmd, m)
		if err == nil || !strings.Contains(err.Error(), "must be an account id") {
			t.Fatalf("err = %v, want an 'account id' parse error", err)
		}
	})

	t.Run("unknown id is rejected", func(t *testing.T) {
		t.Setenv(ccpAccountEnv, "999")
		_, err := resolveRunDir(cmd, m)
		if err == nil || !strings.Contains(err.Error(), "999") {
			t.Fatalf("err = %v, want a not-found error mentioning account 999", err)
		}
	})
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func countPrefix(env []string, prefix string) int {
	n := 0
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

func contains(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
