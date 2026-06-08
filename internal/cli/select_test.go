package cli

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/store"
)

func selectTestManager(t *testing.T) *pool.Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertAccount(store.Account{
		ID: 5, ConfigDir: filepath.Join(t.TempDir(), "acct"), Label: "work@example.com",
		KeychainService: "ccp-test-missing", KeychainAccount: "ccp-test",
	}); err != nil {
		t.Fatal(err)
	}
	return &pool.Manager{Store: st}
}

// TestSelectedLine pins the wording of the select diagnostic and its degraded
// fallbacks. The daemon's SelectedID resolves to the account label; a sticky
// pick is annotated; an unknown/absent id degrades to a generic "account".
func TestSelectedLine(t *testing.T) {
	m := selectTestManager(t)
	id := 5
	missing := 999

	cases := map[string]struct {
		id     *int
		sticky bool
		want   string
	}{
		"named account":       {&id, false, "selected work@example.com"},
		"named sticky":        {&id, true, "selected work@example.com (pinned to this directory)"},
		"nil id degrades":     {nil, false, "selected account"},
		"unknown id degrades": {&missing, false, "selected account"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := selectedLine(m, tc.id, tc.sticky); got != tc.want {
				t.Errorf("selectedLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAnnounceSelectedSilentWhenNotTTY is the core of the noise fix: when stdout
// is not an interactive terminal (as in tests, and under $(ccp select)), the
// success line is suppressed entirely — only the dir reaches stdout elsewhere.
func TestAnnounceSelectedSilentWhenNotTTY(t *testing.T) {
	m := selectTestManager(t)
	id := 5
	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)

	announceSelected(cmd, m, &id, true)

	if stderr.Len() != 0 {
		t.Errorf("expected no stderr output when stdout is not a TTY, got %q", stderr.String())
	}
}
