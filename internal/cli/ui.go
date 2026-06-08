package cli

import (
	"fmt"
	"io"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// Shared output styles. Color numbers are ANSI so they adapt to the user's
// terminal palette: 10 green, 11 yellow, 9 red, 8 dim gray.
var (
	hdrStyle  = lipgloss.NewStyle().Bold(true)
	dimStyle  = lipgloss.NewStyle().Faint(true)
	bestStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	badStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	okStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
)

// The semantic printers below are the single source of truth for line-level
// output. They all write flush-left with no ad-hoc indentation, so printed
// lines stay aligned with each other and with the interactive forms.

// step prints a top-level progress line, undecorated.
func step(w io.Writer, format string, a ...any) {
	fmt.Fprintln(w, fmt.Sprintf(format, a...))
}

// success prints a completed action with a green check.
func success(w io.Writer, format string, a ...any) {
	fmt.Fprintln(w, okStyle.Render("✓")+" "+fmt.Sprintf(format, a...))
}

// note prints a dimmed secondary line beneath a step.
func note(w io.Writer, format string, a ...any) {
	fmt.Fprintln(w, dimStyle.Render(fmt.Sprintf(format, a...)))
}

// warn prints a warning. Callers pass the command's stderr.
func warn(w io.Writer, format string, a ...any) {
	fmt.Fprintln(w, warnStyle.Render("warning:")+" "+fmt.Sprintf(format, a...))
}

// fail prints a failed action with a red cross. Callers pass the command's
// stderr.
func fail(w io.Writer, format string, a ...any) {
	fmt.Fprintln(w, badStyle.Render("✗")+" "+fmt.Sprintf(format, a...))
}

// plural renders a count with its noun, pluralized with a trailing "s" (e.g.
// plural(1, "account") == "1 account", plural(3, "account") == "3 accounts").
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// clpTheme is the huh form theme, applied to every interactive prompt so the
// forms share one look and sit consistently with the printed lines. It softens
// the Charm base by dimming descriptions.
func clpTheme() *huh.Theme {
	t := huh.ThemeCharm()
	t.Focused.Description = t.Focused.Description.Foreground(lipgloss.Color("245"))
	t.Blurred.Description = t.Blurred.Description.Foreground(lipgloss.Color("245"))
	return t
}
