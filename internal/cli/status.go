package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/daemon"
	"github.com/yasyf/cc-pool/internal/pool"
)

var (
	hdrStyle  = lipgloss.NewStyle().Bold(true)
	dimStyle  = lipgloss.NewStyle().Faint(true)
	bestStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	badStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

func newStatusCmd() *cobra.Command {
	var watch bool
	var live bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show a live table of per-account usage, score, and sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				if err := requireInit(m); err != nil {
					return err
				}
				render := func() error {
					snaps, src, err := gatherStatus(cmd.Context(), m, live)
					if err != nil {
						return err
					}
					out := renderTable(snaps, src)
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
			})
		},
	}
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "refresh continuously")
	cmd.Flags().BoolVar(&live, "live", false, "force live sampling even if the daemon is running")
	return cmd
}

// gatherStatus prefers the daemon's cached view, falling back to live sampling.
func gatherStatus(ctx context.Context, m *pool.Manager, forceLive bool) ([]pool.Snapshot, string, error) {
	if !forceLive {
		if resp, err := daemon.NewClient().Status(); err == nil && resp.OK {
			return fromDaemon(resp.Accounts), "daemon", nil
		}
	}
	snaps, err := m.Snapshots(ctx, true, pool.DefaultFreshFor)
	return snaps, "live", err
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
		}
		s.Account.ID = a.ID
		s.Account.ConfigDir = a.ConfigDir
		s.Account.Label = a.Label
		s.Account.IsZero = a.IsZero
		s.Account.OverlayKind = a.OverlayKind
		out = append(out, s)
	}
	return out
}

// renderTable produces a styled fixed-width table, best account highlighted.
func renderTable(snaps []pool.Snapshot, source string) string {
	// Sort by score desc for display; best first.
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].Score > snaps[j].Score })

	var b strings.Builder
	header := fmt.Sprintf("%-8s %-22s %8s %7s %7s %6s %-10s %s",
		"ACCT", "LABEL", "SCORE", "5h", "7d", "SESS", "RESET", "FLAGS")
	b.WriteString(hdrStyle.Render(header))
	b.WriteByte('\n')

	for i, s := range snaps {
		acct := fmt.Sprintf("acct-%02d", s.Account.ID)
		label := truncate(s.Account.Label, 22)
		rem5 := fmt.Sprintf("%5.0f%%", s.Remaining5h)
		rem7 := fmt.Sprintf("%5.0f%%", s.Remaining7d)
		sess := fmt.Sprintf("%d", s.ActiveSessions)
		reset := "-"
		if !s.Resets5h.IsZero() {
			reset = humanizeUntil(time.Until(s.Resets5h))
		}
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
		row := fmt.Sprintf("%-8s %-22s %8.1f %7s %7s %6s %-10s %s",
			acct, label, s.Score, rem5, rem7, sess, reset, strings.Join(flags, " "))
		if i == 0 {
			row = bestStyle.Render("▸ ") + row
		} else {
			row = "  " + row
		}
		b.WriteString(row)
		b.WriteByte('\n')
	}
	b.WriteString(dimStyle.Render(fmt.Sprintf("source: %s · %s", source, time.Now().Format(time.Kitchen))))
	return b.String()
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
