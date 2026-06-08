package cli

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-pool/internal/daemon"
	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/store"
	"github.com/yasyf/cc-pool/internal/version"
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
	// The busy row's known reset now renders as an absolute clock (AM/PM), not a
	// relative "2h03m" duration.
	if !strings.Contains(lines[2], "AM") && !strings.Contains(lines[2], "PM") {
		t.Errorf("busy row should show an absolute reset clock, got %q", lines[2])
	}

	if !strings.Contains(out, "next pick") || !strings.Contains(out, "% used") {
		t.Errorf("missing legend line\n%s", out)
	}
}

// TestHumanizeResetAt pins the absolute-reset formatter against a fixed now
// (Monday 2026-06-08 10:00 local). Inputs are built in time.Local so the
// formatter's .Local() is a no-op and the expected strings hold in any zone.
func TestHumanizeResetAt(t *testing.T) {
	now := time.Date(2026, 6, 8, 10, 0, 0, 0, time.Local) // Monday
	at := func(mo, d, h, min int) time.Time {
		return time.Date(2026, time.Month(mo), d, h, min, 0, 0, time.Local)
	}
	cases := map[string]struct {
		in   time.Time
		want string
	}{
		"zero / no window":      {time.Time{}, "-"},
		"later today":           {at(6, 8, 15, 58), "3:58 PM"},
		"earlier today (past)":  {at(6, 8, 8, 30), "8:30 AM"},
		"yesterday (stale)":     {at(6, 7, 15, 58), "3:58 PM"},
		"tomorrow":              {at(6, 9, 15, 58), "tomorrow 3:58 PM"},
		"two days (weekday)":    {at(6, 10, 15, 58), "Wed 3:58 PM"},
		"six days (edge in)":    {at(6, 14, 9, 5), "Sun 9:05 AM"},
		"seven days (edge out)": {at(6, 15, 15, 58), "Jun 15, 3:58 PM"},
		"far future":            {at(6, 20, 15, 58), "Jun 20, 3:58 PM"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := humanizeResetAt(tc.in, now); got != tc.want {
				t.Errorf("humanizeResetAt(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRenderTableEmpty keeps the friendly empty-pool message.
func TestRenderTableEmpty(t *testing.T) {
	if got := renderTable(nil); !strings.Contains(got, "ccp add") {
		t.Errorf("empty pool should suggest `ccp add`, got %q", got)
	}
}

// TestFromDaemonHasUsageIndependentOfStale: "no-data" means never-sampled, not
// stale. A stale account that still has usage must not be mislabeled no-data
// (the bug where every stale account showed both flags despite real util%).
func TestFromDaemonHasUsageIndependentOfStale(t *testing.T) {
	snaps := fromDaemon([]daemon.AccountStatus{
		{ID: 1, Label: "stale-with-data", Stale: true, HasUsage: true, Remaining7d: 87},
		{ID: 2, Label: "never-sampled", Stale: true, HasUsage: false},
	})

	if !snaps[0].HasUsage {
		t.Fatal("a stale account with data must keep HasUsage=true")
	}
	if f := stripANSI(snapshotFlags(snaps[0])); strings.Contains(f, "no-data") || !strings.Contains(f, "stale") {
		t.Fatalf("stale-with-data must be flagged stale but not no-data, got %q", f)
	}
	if snaps[1].HasUsage {
		t.Fatal("a never-sampled account must have HasUsage=false")
	}
	if f := stripANSI(snapshotFlags(snaps[1])); !strings.Contains(f, "no-data") {
		t.Fatalf("never-sampled must be flagged no-data, got %q", f)
	}
}

// TestDaemonStatusUsable pins the version gate: only an OK response from a
// daemon at the exact current binary version is rendered directly; anything
// else (error, not-OK, empty/mismatched version) falls back to live sampling so
// a pre-upgrade daemon can't feed the TUI a partial view.
func TestDaemonStatusUsable(t *testing.T) {
	cur := version.String()
	cases := map[string]struct {
		resp *daemon.Response
		err  error
		want bool
	}{
		"current version":  {&daemon.Response{OK: true, Version: cur}, nil, true},
		"transport error":  {nil, errors.New("dial: no socket"), false},
		"not ok":           {&daemon.Response{OK: false, Version: cur}, nil, false},
		"empty version":    {&daemon.Response{OK: true, Version: ""}, nil, false},
		"mismatch version": {&daemon.Response{OK: true, Version: cur + "-old"}, nil, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := daemonStatusUsable(tc.resp, tc.err); got != tc.want {
				t.Errorf("daemonStatusUsable = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRemainingSuffix pins the headroom suffix: known usage renders rounded
// 5h/7d remaining; unknown usage renders nothing so callers never print a
// fabricated 100% for a never-sampled (or pre-upgrade-daemon) pick.
func TestRemainingSuffix(t *testing.T) {
	cases := map[string]struct {
		hasUsage   bool
		eff5, eff7 float64
		want       string
	}{
		"unknown usage":       {false, 87, 92, ""},
		"unknown ignores eff": {false, 0, 0, ""},
		"rounds to whole":     {true, 86.7, 91.4, " · 5h 87% · 7d 91% remaining"},
		"drained pick":        {true, 0, 0, " · 5h 0% · 7d 0% remaining"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := remainingSuffix(tc.hasUsage, tc.eff5, tc.eff7); got != tc.want {
				t.Errorf("remainingSuffix = %q, want %q", got, tc.want)
			}
		})
	}
}
