package cli

import (
	"os"

	"golang.org/x/term"
)

// isTTY reports whether stdin is an interactive terminal (so we can run huh
// forms; otherwise commands use non-interactive fallbacks).
func isTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// stdoutIsTTY reports whether stdout is an interactive terminal. `clp select`
// uses it to print its human-facing "selected …" line only when a person is
// looking at the bare output, staying silent when the dir is captured via
// $(clp select) or piped onward.
func stdoutIsTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}
