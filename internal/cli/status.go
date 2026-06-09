package cli

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/daemon"
	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/version"
)

func newStatusCmd() *cobra.Command {
	var watch bool
	var live bool
	var plain bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show per-account usage, score, and sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				if err := requireInit(m); err != nil {
					return err
				}
				return runStatus(cmd, m, watch, live, plain)
			})
		},
	}
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "refresh continuously (plain mode)")
	cmd.Flags().BoolVar(&live, "live", false, "force live sampling even if the daemon is running")
	cmd.Flags().BoolVar(&plain, "plain", false, "print the plain table instead of the interactive TUI")
	return cmd
}

// runStatus shows account status. On an interactive terminal it launches the
// TUI (which refreshes itself); piped or with --plain it prints the plain
// table, once or continuously under --watch. Both `ccp status` and bare `ccp`
// dispatch here.
func runStatus(cmd *cobra.Command, m *pool.Manager, watch, live, plain bool) error {
	if isTTY() && !plain {
		return runStatusTUI(cmd, m, live)
	}
	render := func() error {
		snaps, err := gatherStatus(cmd.Context(), m, live)
		if err != nil {
			return err
		}
		out := renderTable(snaps)
		if watch {
			fmt.Fprint(cmd.OutOrStdout(), "\033[H\033[2J") // clear
		}
		fmt.Fprintln(cmd.OutOrStdout(), out)
		return nil
	}
	if !watch {
		return render()
	}
	for {
		if err := render(); err != nil {
			return err
		}
		select {
		case <-cmd.Context().Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

// gatherStatus prefers the daemon's cached view, falling back to live sampling.
func gatherStatus(ctx context.Context, m *pool.Manager, forceLive bool) ([]pool.Snapshot, error) {
	if !forceLive {
		resp, err := daemon.NewClient().Status()
		if daemonStatusUsable(resp, err) {
			return fromDaemon(resp.Accounts), nil
		}
	}
	return m.Snapshots(ctx, true, pool.DefaultFreshFor)
}

// daemonStatusUsable reports whether a status response can be rendered directly.
// A transport error, a not-OK reply, or a version-skewed (pre-upgrade) daemon —
// which omits newer wire fields like Components — must fall back to live
// sampling so the render is never partial.
func daemonStatusUsable(resp *daemon.Response, err error) bool {
	return err == nil && resp != nil && resp.OK && resp.Version == version.String()
}

// fromDaemon converts daemon account statuses into Snapshots for rendering.
func fromDaemon(accs []daemon.AccountStatus) []pool.Snapshot {
	out := make([]pool.Snapshot, 0, len(accs))
	for _, a := range accs {
		s := pool.Snapshot{
			Score:          a.Score,
			HasUsage:       a.HasUsage,
			Remaining5h:    a.Remaining5h,
			Remaining7d:    a.Remaining7d,
			Util5h:         100 - a.Remaining5h,
			Util7d:         100 - a.Remaining7d,
			ActiveSessions: a.ActiveSessions,
			RateLimited:    a.RateLimited,
			Stale:          a.Stale,
			Resets5h:       a.Resets5h,
			Resets7d:       a.Resets7d,
			Components:     a.Components,
		}
		s.Account.ID = a.ID
		s.Account.ConfigDir = a.ConfigDir
		s.Account.Label = a.Label
		s.Account.OverlayKind = a.OverlayKind
		out = append(out, s)
	}
	return out
}

// renderTable produces a styled fixed-width table, best account highlighted.
func renderTable(snaps []pool.Snapshot) string {
	if len(snaps) == 0 {
		return "No accounts yet. Run `ccp add` to add one.\n"
	}
	// Sort by score desc for display; best first.
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].Score > snaps[j].Score })

	var b strings.Builder
	// Two leading spaces align the header with the rows' marker gutter ("▸ "/"  ").
	header := fmt.Sprintf("  %-24s %8s %8s %8s %5s %-17s",
		"ACCOUNT", "SCORE", "5h used", "7d used", "LIVE", "RESETS")
	b.WriteString(hdrStyle.Render(header))
	b.WriteByte('\n')

	for i, s := range snaps {
		label := truncate(accountName(s.Account.Label), 24)
		used5 := fmt.Sprintf("%.0f%%", s.Util5h)
		used7 := fmt.Sprintf("%.0f%%", s.Util7d)
		reset := humanizeReset(s.Resets5h)
		row := fmt.Sprintf("%-24s %8.1f %8s %8s %5d %-17s",
			label, s.Score, used5, used7, s.ActiveSessions, reset)
		if flags := snapshotFlags(s); flags != "" {
			row += " " + flags
		}
		if i == 0 {
			row = bestStyle.Render("▸ ") + row
		} else {
			row = "  " + row
		}
		b.WriteString(row)
		b.WriteByte('\n')
	}
	b.WriteString(dimStyle.Render("▸ = next pick · score higher = emptier · 5h/7d = % used"))
	b.WriteByte('\n')
	b.WriteString(dimStyle.Render(fmt.Sprintf("updated %s", time.Now().Format(clockLayout))))
	return b.String()
}

// snapshotFlags renders the colored status tokens (stale / rate-limited /
// no-data) for one account, or "" when the account is healthy. Shared by the
// plain table and the TUI.
func snapshotFlags(s pool.Snapshot) string {
	var flags []string
	if s.Stale {
		flags = append(flags, warnStyle.Render("stale"))
	}
	if s.RateLimited {
		flags = append(flags, badStyle.Render("rate-limited"))
	}
	if !s.HasUsage {
		flags = append(flags, dimStyle.Render("no-data"))
	}
	return strings.Join(flags, " ")
}

// accountName returns an account's display label, or a placeholder when it is
// unnamed. The internal acct-NN id is shown only by `ccp list` and `ccp doctor`.
func accountName(label string) string {
	if label == "" {
		return "(unnamed)"
	}
	return label
}

// usageSuffix renders raw 5h/7d percent-used as " · 5h X% used · 7d Y% used"
// for the `select`/`run` diagnostic lines, each figure health-tinted. It returns
// "" when usage is unknown — a never-sampled pick, or a daemon predating the wire
// fields — so we never print a fabricated 0%.
func usageSuffix(hasUsage bool, used5, used7 float64) string {
	if !hasUsage {
		return ""
	}
	pct5 := usageStyle(used5).Render(fmt.Sprintf("%.0f%%", used5))
	pct7 := usageStyle(used7).Render(fmt.Sprintf("%.0f%%", used7))
	return dimStyle.Render(" · 5h ") + pct5 + dimStyle.Render(" used · 7d ") + pct7 + dimStyle.Render(" used")
}

// daemonAccountName resolves a daemon SelectedID to a display name, degrading to
// "account" when the id is nil or unknown.
func daemonAccountName(m *pool.Manager, id *int) string {
	if id != nil {
		if a, err := m.Store.GetAccount(*id); err == nil {
			return accountName(a.Label)
		}
	}
	return "account"
}

// clockLayout is the shared spelling for every human-facing wall-clock time in
// the status UI: 12-hour with AM/PM, e.g. "3:58 PM".
const clockLayout = "3:04 PM"

// humanizeReset renders an absolute rate-limit reset time in the user's local
// zone with smart day context — "3:58 PM" today, "tomorrow 3:58 PM", "Tue 3:58
// PM" within the week, "Jun 10, 3:58 PM" beyond. The zero time (no active
// window) renders "-".
func humanizeReset(t time.Time) string {
	return humanizeResetAt(t, time.Now())
}

// humanizeResetAt is humanizeReset with an injectable now for deterministic
// tests. Both ends are normalized to Local so the clock and day arithmetic match
// the user's wall clock regardless of the zone the reset arrived in (the /usage
// RFC3339 path and the daemon's JSON can carry a non-local offset).
func humanizeResetAt(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	t, now = t.Local(), now.Local()
	switch days := calendarDaysFrom(now, t); {
	case days <= 0: // today, or a past reset from stale data
		return t.Format(clockLayout)
	case days == 1:
		return "tomorrow " + t.Format(clockLayout)
	case days < 7:
		return t.Format("Mon " + clockLayout)
	default:
		return t.Format("Jan 2, " + clockLayout)
	}
}

// calendarDaysFrom returns the count of whole local calendar days from now to t
// (0 = same day, 1 = next day, negative = past). Anchoring both ends at local
// midnight and rounding keeps it correct across DST (a 23h or 25h day).
func calendarDaysFrom(now, t time.Time) int {
	y0, m0, d0 := now.Date()
	y1, m1, d1 := t.Date()
	start := time.Date(y0, m0, d0, 0, 0, 0, 0, now.Location())
	end := time.Date(y1, m1, d1, 0, 0, 0, 0, now.Location())
	return int(math.Round(end.Sub(start).Hours() / 24))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
