package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/daemon"
	"github.com/yasyf/cc-pool/internal/pool"
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
// table, once or continuously under --watch. Both `clp status` and bare `clp`
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
		if resp, err := daemon.NewClient().Status(); err == nil && resp.OK {
			return fromDaemon(resp.Accounts), nil
		}
	}
	return m.Snapshots(ctx, true, pool.DefaultFreshFor)
}

// fromDaemon converts daemon account statuses into Snapshots for rendering.
func fromDaemon(accs []daemon.AccountStatus) []pool.Snapshot {
	out := make([]pool.Snapshot, 0, len(accs))
	for _, a := range accs {
		s := pool.Snapshot{
			Score:          a.Score,
			HasUsage:       !a.Stale,
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
		return "No accounts yet. Run `clp add` to add one.\n"
	}
	// Sort by score desc for display; best first.
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].Score > snaps[j].Score })

	var b strings.Builder
	// Two leading spaces align the header with the rows' marker gutter ("▸ "/"  ").
	header := fmt.Sprintf("  %-24s %8s %8s %8s %5s %-8s",
		"ACCOUNT", "SCORE", "5h used", "7d used", "LIVE", "RESETS")
	b.WriteString(hdrStyle.Render(header))
	b.WriteByte('\n')

	for i, s := range snaps {
		label := truncate(accountName(s.Account.Label), 24)
		used5 := fmt.Sprintf("%.0f%%", s.Util5h)
		used7 := fmt.Sprintf("%.0f%%", s.Util7d)
		reset := "-"
		if !s.Resets5h.IsZero() {
			reset = humanizeUntil(time.Until(s.Resets5h))
		}
		row := fmt.Sprintf("%-24s %8.1f %8s %8s %5d %-8s",
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
	b.WriteString(dimStyle.Render(fmt.Sprintf("updated %s", time.Now().Format(time.Kitchen))))
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
// unnamed. The internal acct-NN id is shown only by `clp list` and `clp doctor`.
func accountName(label string) string {
	if label == "" {
		return "(unnamed)"
	}
	return label
}

func humanizeUntil(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
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
