package cli

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/store"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// TestRenderTablePlain pins the plain (non-TTY) status table: it must phrase the
// 5h/7d columns as % USED (not remaining), use the clearer headers, mark the
// next pick, flag a stale account, and carry the legend.
func TestRenderTablePlain(t *testing.T) {
	snaps := []pool.Snapshot{
		{
			Account:  store.Account{ID: 1, Label: "best@example.com"},
			Score:    93.9,
			HasUsage: true,
			Util5h:   0,
			Util7d:   13,
			// Zero Resets5h → "-" (no active window), not a bogus duration.
		},
		{
			Account:  store.Account{ID: 2, Label: "busy@example.com"},
			Score:    71.5,
			HasUsage: true,
			Util5h:   58,
			Util7d:   61,
			Stale:    true,
			Resets5h: time.Now().Add(2*time.Hour + 3*time.Minute),
		},
	}
	out := stripANSI(renderTable(snaps))

	for _, want := range []string{"5h used", "7d used", "LIVE", "RESETS"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing %q\n%s", want, out)
		}
	}
	// The old, ambiguous headers must be gone.
	for _, bad := range []string{"SESS", "FLAGS"} {
		if strings.Contains(out, bad) {
			t.Errorf("output still shows retired label %q\n%s", bad, out)
		}
	}

	// Columns show utilization (used), so a 58%-used window reads "58%", never
	// the remaining "42%".
	if !strings.Contains(out, "58%") || !strings.Contains(out, "61%") {
		t.Errorf("rows should show used%% (58/61)\n%s", out)
	}
	if strings.Contains(out, "42%") || strings.Contains(out, "39%") {
		t.Errorf("rows must not show remaining%% (42/39)\n%s", out)
	}

	if !strings.Contains(out, "▸") {
		t.Errorf("missing next-pick marker\n%s", out)
	}
	if !strings.Contains(out, "stale") {
		t.Errorf("stale account should be flagged\n%s", out)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// lines[0] header, lines[1] best account (zero reset → "-").
	if !strings.Contains(lines[1], "best@example.com") || !strings.HasSuffix(strings.TrimRight(lines[1], " "), "-") {
		t.Errorf("best row should end with '-' for an unknown reset\n%q", lines[1])
	}

	if !strings.Contains(out, "next pick") || !strings.Contains(out, "% used") {
		t.Errorf("missing legend line\n%s", out)
	}
}

// TestRenderTableEmpty keeps the friendly empty-pool message.
func TestRenderTableEmpty(t *testing.T) {
	if got := renderTable(nil); !strings.Contains(got, "ccp add") {
		t.Errorf("empty pool should suggest `ccp add`, got %q", got)
	}
}
