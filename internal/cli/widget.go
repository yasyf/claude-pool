package cli

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// Widget app cask coordinates. The tap URL is spelled out because the tap
// repo is not named homebrew-cc-pool, so brew cannot infer it from the short
// name and `brew install yasyf/cc-pool/…` cannot auto-tap.
const (
	widgetCask    = "cc-pool-status"
	widgetTap     = "yasyf/cc-pool"
	widgetTapURL  = "https://github.com/yasyf/cc-pool"
	widgetAppName = "CCPoolStatus"
)

func newWidgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "widget",
		Short: "Install the Notification Center status widget",
		Long: `Installs the CCPoolStatus app via Homebrew (cask ` + widgetTap + `/` + widgetCask + `),
launches it so macOS discovers the widget, and prints how to enable it.
Safe to re-run: an existing install is upgraded in place.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runWidget(cmd) },
	}
}

func runWidget(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	if _, err := exec.LookPath("brew"); err != nil {
		return fmt.Errorf("the widget installs via Homebrew, which isn't on PATH (to build from source instead, see widget/README.md): %w", err)
	}
	if err := ensureWidgetTap(cmd); err != nil {
		return err
	}
	step(out, "Installing the widget app…")
	if err := brewInstallWidget(cmd); err != nil {
		return err
	}
	// Launching once registers the embedded widget extension with macOS so it
	// shows up in the widget gallery. -g keeps the agent app in the background.
	step(out, "Launching it so macOS discovers the widget…")
	if err := runStreamed(cmd, "open", "-g", "-a", widgetAppName); err != nil {
		return fmt.Errorf("launch %s: %w", widgetAppName, err)
	}
	success(out, "Widget installed.")
	fmt.Fprint(out, widgetInstructions())
	return nil
}

// ensureWidgetTap taps the cc-pool tap if it isn't already present, so the
// cask resolves even when cc-pool itself was installed some other way.
func ensureWidgetTap(cmd *cobra.Command) error {
	outBytes, err := exec.Command("brew", "tap").Output()
	if err != nil {
		return fmt.Errorf("list brew taps: %w", err)
	}
	for _, line := range strings.Split(string(outBytes), "\n") {
		if strings.TrimSpace(line) == widgetTap {
			return nil
		}
	}
	step(cmd.OutOrStdout(), "Adding the %s tap…", widgetTap)
	if err := runStreamed(cmd, "brew", "tap", widgetTap, widgetTapURL); err != nil {
		return fmt.Errorf("brew tap %s: %w", widgetTap, err)
	}
	return nil
}

// brewInstallWidget installs the cask, or upgrades it when already present.
// --no-quarantine is required: the app is ad-hoc signed (no Developer ID), so
// a quarantined copy would be blocked by Gatekeeper on launch.
func brewInstallWidget(cmd *cobra.Command) error {
	installed := exec.Command("brew", "list", "--cask", widgetCask).Run() == nil
	if installed {
		if err := runStreamed(cmd, "brew", "upgrade", "--cask", "--no-quarantine", widgetCask); err != nil {
			return fmt.Errorf("brew upgrade --cask %s: %w", widgetCask, err)
		}
		return nil
	}
	if err := runStreamed(cmd, "brew", "install", "--cask", "--no-quarantine", widgetTap+"/"+widgetCask); err != nil {
		return fmt.Errorf("brew install --cask %s: %w", widgetCask, err)
	}
	return nil
}

// runStreamed runs a command with its output streamed to the user.
func runStreamed(cmd *cobra.Command, name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
	return c.Run()
}

// widgetInstructions is the post-install walkthrough for enabling the widget.
func widgetInstructions() string {
	return `
To add the widget:
  1. Open Notification Center — click the clock in the menu bar.
  2. Scroll to the bottom and click "Edit Widgets".
  3. Search "cc-pool" and add the small or medium widget.
     (Desktop widgets work too: right-click the desktop → Edit Widgets.)

The widget refreshes every ~3 minutes while CCPoolStatus is running. To keep
that across logins: System Settings → General → Login Items → add CCPoolStatus.

Not showing up in the gallery? Run:
  killall NotificationCenter && open -ga ` + widgetAppName + `
`
}
