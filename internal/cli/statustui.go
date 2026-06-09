package cli

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/score"
)

// statusRefreshInterval is how often the TUI re-polls the daemon (or live
// samples) for fresh account state.
const statusRefreshInterval = 5 * time.Second

var (
	// selectedStyle marks the row under the cursor and the detail it drives.
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	panelStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	panelTitle    = lipgloss.NewStyle().Bold(true)
)

// runStatusTUI runs the interactive status dashboard until the user quits or
// the context is cancelled. It is only reached on an interactive terminal; the
// piped/`--plain` path stays on renderTable.
func runStatusTUI(cmd *cobra.Command, m *pool.Manager, live bool) error {
	ctx := cmd.Context()
	// Restart a pre-upgrade daemon onto the current binary so the cached view it
	// serves carries the newer wire fields the detail pane renders. Best-effort;
	// gatherStatus version-gates and falls back to live regardless. Skipped under
	// --live (the user already opted out of the daemon). The alt-screen clears any
	// "restarting…" line on entry.
	if !live {
		ensureDaemon(cmd)
	}
	model := statusTUI{
		ctx: ctx,
		gather: func(c context.Context) ([]pool.Snapshot, error) {
			return gatherStatus(c, m, live)
		},
	}
	p := tea.NewProgram(model,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		tea.WithOutput(cmd.OutOrStdout()),
	)
	if _, err := p.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return err
	}
	return nil
}

// statusTUI is the Bubble Tea model for `ccp status`. It owns a sorted snapshot
// list, a cursor tracked by account id (so a re-sort on refresh never moves the
// selection), and a gather closure that re-fetches state off the UI goroutine.
type statusTUI struct {
	ctx        context.Context
	gather     func(context.Context) ([]pool.Snapshot, error)
	snaps      []pool.Snapshot
	cursorID   int
	width      int
	height     int
	err        error
	lastUpdate time.Time
	quitting   bool
}

// Bubble Tea messages.
type (
	snapsMsg struct {
		snaps []pool.Snapshot
		at    time.Time
	}
	errMsg  struct{ err error }
	tickMsg time.Time
)

func (t statusTUI) Init() tea.Cmd {
	return tea.Batch(t.refreshCmd(), tickCmd())
}

// refreshCmd fetches fresh status off the UI goroutine; live sampling never
// blocks key input.
func (t statusTUI) refreshCmd() tea.Cmd {
	return func() tea.Msg {
		snaps, err := t.gather(t.ctx)
		if err != nil {
			return errMsg{err}
		}
		return snapsMsg{snaps: snaps, at: time.Now()}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(statusRefreshInterval, func(tm time.Time) tea.Msg { return tickMsg(tm) })
}

func (t statusTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		t.width, t.height = msg.Width, msg.Height
		return t, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			t.quitting = true
			return t, tea.Quit
		case "up", "k":
			t.moveCursor(-1)
			return t, nil
		case "down", "j":
			t.moveCursor(1)
			return t, nil
		case "r":
			return t, t.refreshCmd()
		}
		return t, nil
	case snapsMsg:
		t.snaps = msg.snaps
		sort.SliceStable(t.snaps, func(i, j int) bool { return t.snaps[i].Score > t.snaps[j].Score })
		t.lastUpdate = msg.at
		t.err = nil
		t.ensureCursor()
		return t, nil
	case errMsg:
		t.err = msg.err
		return t, nil
	case tickMsg:
		return t, tea.Batch(t.refreshCmd(), tickCmd())
	}
	return t, nil
}

// sortedIndex returns the display row of the cursor's account, or 0.
func (t statusTUI) sortedIndex() int {
	for i, s := range t.snaps {
		if s.Account.ID == t.cursorID {
			return i
		}
	}
	return 0
}

// ensureCursor keeps the cursor on a real account after a refresh, defaulting
// to the best (top) account when its previous target is gone.
func (t *statusTUI) ensureCursor() {
	if len(t.snaps) == 0 {
		t.cursorID = 0
		return
	}
	for _, s := range t.snaps {
		if s.Account.ID == t.cursorID {
			return
		}
	}
	t.cursorID = t.snaps[0].Account.ID
}

func (t *statusTUI) moveCursor(d int) {
	if len(t.snaps) == 0 {
		return
	}
	i := t.sortedIndex() + d
	if i < 0 {
		i = 0
	}
	if i >= len(t.snaps) {
		i = len(t.snaps) - 1
	}
	t.cursorID = t.snaps[i].Account.ID
}

func (t statusTUI) current() pool.Snapshot {
	i := t.sortedIndex()
	if i < 0 || i >= len(t.snaps) {
		return pool.Snapshot{}
	}
	return t.snaps[i]
}

func (t statusTUI) View() string {
	if t.quitting {
		return ""
	}
	if len(t.snaps) == 0 {
		if t.err != nil {
			return fmt.Sprintf("status error: %v\n", t.err)
		}
		return "Loading account status…\n"
	}
	w := t.width
	if w <= 0 {
		w = 80
	}
	contentW := w - 4
	if contentW < 40 {
		contentW = 40
	}
	listBox := panelStyle.Width(contentW).Render(t.renderList())
	detailBox := panelStyle.Width(contentW).Render(t.renderDetail())
	footer := dimStyle.Render("↑/↓ navigate · r refresh · q quit")
	if t.err != nil {
		footer = warnStyle.Render(fmt.Sprintf("refresh failed: %v", t.err)) + "  " + footer
	}
	return lipgloss.JoinVertical(lipgloss.Left, listBox, detailBox, footer) + "\n"
}

// renderList draws the account table. A 3-cell gutter carries two independent
// markers: ▸ (green) = the account `select` would pick next, ❯ = the cursor.
func (t statusTUI) renderList() string {
	hdr := hdrStyle.Render(fmt.Sprintf("   %-22s %8s %8s %8s %5s",
		"ACCOUNT", "SCORE", "5h used", "7d used", "LIVE"))
	lines := []string{hdr}
	cursor := t.sortedIndex()
	for i, s := range t.snaps {
		bestMark := " "
		if i == 0 {
			bestMark = bestStyle.Render("▸")
		}
		curMark := " "
		if i == cursor {
			curMark = selectedStyle.Render("❯")
		}
		cells := fmt.Sprintf("%-22s %8.1f %8s %8s %5d",
			truncate(accountName(s.Account.Label), 22), s.Score,
			fmt.Sprintf("%.0f%%", s.Util5h), fmt.Sprintf("%.0f%%", s.Util7d), s.ActiveSessions)
		if i == cursor {
			cells = selectedStyle.Render(cells)
		}
		row := bestMark + curMark + " " + cells
		if fl := snapshotFlags(s); fl != "" {
			row += " " + fl
		}
		lines = append(lines, row)
	}
	return strings.Join(lines, "\n")
}

// renderDetail explains the selected account's score factor-by-factor, sourced
// from the score Components so it reconciles exactly with the SCORE column.
func (t statusTUI) renderDetail() string {
	s := t.current()
	c := s.Components
	var b strings.Builder

	pick := "no"
	if len(t.snaps) > 0 && t.snaps[0].Account.ID == s.Account.ID {
		pick = "yes"
	}
	b.WriteString(panelTitle.Render(fmt.Sprintf("%s · next pick: %s", accountName(s.Account.Label), pick)))
	b.WriteByte('\n')
	b.WriteString(fmt.Sprintf("score %.1f\n", s.Score))

	// Positive contributions: reset-aware effective headroom × its weight. Labeled
	// "effective" (not "free") because the reset credit can lift it above the raw
	// remaining shown by the usage bars below; tinted by how depleted it is.
	eff5Str := usageStyle(100 - c.Eff5).Render(fmt.Sprintf("%3.0f%%", c.Eff5))
	eff7Str := usageStyle(100 - c.Eff7).Render(fmt.Sprintf("%3.0f%%", c.Eff7))
	b.WriteString(fmt.Sprintf("  5h  %s effective  ×%.2f  = %+5.1f\n", eff5Str, score.W5h, c.Remaining5h))
	b.WriteString(fmt.Sprintf("  7d  %s effective  ×%.2f  = %+5.1f\n", eff7Str, score.W7d, c.Remaining7d))

	// Penalties, only the ones actually engaged.
	var pen []string
	if c.SessionPenalty > 0 {
		pen = append(pen, fmt.Sprintf("  %-18s %+5.1f", fmt.Sprintf("sessions %d", s.ActiveSessions), -c.SessionPenalty))
	}
	if c.RateLimitPenalty > 0 {
		pen = append(pen, fmt.Sprintf("  %-18s %+5.1f", "rate-limited", -c.RateLimitPenalty))
	}
	if c.StalePenalty > 0 {
		pen = append(pen, fmt.Sprintf("  %-18s %+5.1f", "stale data", -c.StalePenalty))
	}
	if c.Barrier5h > 0 {
		pen = append(pen, fmt.Sprintf("  %-18s %+5.1f", "low 5h headroom", -c.Barrier5h))
	}
	if c.Barrier7d > 0 {
		pen = append(pen, fmt.Sprintf("  %-18s %+5.1f", "low 7d headroom", -c.Barrier7d))
	}
	if c.RunwayPenalty > 0 {
		pen = append(pen, fmt.Sprintf("  %-18s %+5.1f", "burn rate", -c.RunwayPenalty))
	}
	if len(pen) == 0 {
		b.WriteString("  penalties          none\n")
	} else {
		b.WriteString(strings.Join(pen, "\n"))
		b.WriteByte('\n')
	}

	// Raw usage bars (what's consumed) with reset timing.
	b.WriteString(usageRow("5h", s.Util5h, s.Resets5h))
	b.WriteByte('\n')
	b.WriteString(usageRow("7d", s.Util7d, s.Resets7d))
	b.WriteByte('\n')

	overlay := s.Account.OverlayKind
	if overlay == "" {
		overlay = "symlink"
	}
	meta := "overlay " + overlay
	if !t.lastUpdate.IsZero() {
		meta += " · updated " + t.lastUpdate.Format("15:04:05")
	}
	b.WriteString(dimStyle.Render(meta))
	return b.String()
}

// usageRow renders one "5h ▕████░░░▏ 58% used · resets 2h03m" line.
func usageRow(label string, usedPct float64, resets time.Time) string {
	when := "no active window"
	if !resets.IsZero() {
		when = "resets " + humanizeReset(resets)
	}
	return fmt.Sprintf("%-2s %s %3.0f%% used · %s", label, usageBar(usedPct, 16), usedPct, when)
}

// usageBar renders a fixed-width filled bar for a 0..100 used percentage, tinted
// green/yellow/red as it fills.
func usageBar(usedPct float64, width int) string {
	if usedPct < 0 {
		usedPct = 0
	}
	if usedPct > 100 {
		usedPct = 100
	}
	filled := int(math.Round(usedPct / 100 * float64(width)))
	if filled > width {
		filled = width
	}
	bar := usageStyle(usedPct).Render(strings.Repeat("█", filled)) + dimStyle.Render(strings.Repeat("░", width-filled))
	return "▕" + bar + "▏"
}
