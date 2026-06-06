package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/daemon"
	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/service"
	"github.com/yasyf/cc-pool/internal/version"
)

func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage the background daemon (LaunchAgent)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "install",
			Short: "Install and start the user LaunchAgent",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, _ []string) error { return runServiceInstall(cmd) },
		},
		newServiceUninstallCmd(),
		&cobra.Command{
			Use:   "status",
			Short: "Show daemon/LaunchAgent status",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				out := cmd.OutOrStdout()
				if service.IsBrewManaged() {
					fmt.Fprintln(out, "Management: Homebrew (brew services)")
					if info, err := service.BrewInfo(); err == nil {
						fmt.Fprintln(out, info)
					}
				} else {
					fmt.Fprintf(out, "Management: self-managed LaunchAgent (loaded: %v)\n", service.Loaded())
				}
				if resp, err := daemon.NewClient().Health(); err == nil && resp.OK {
					fmt.Fprintf(out, "Daemon: running (%s)\n", resp.Version)
				} else {
					fmt.Fprintln(out, "Daemon: not responding")
				}
				fmt.Fprintf(out, "Socket: %s\n", pool.SocketPath())
				return nil
			},
		},
	)
	return cmd
}

func newServiceUninstallCmd() *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the LaunchAgent (and with --purge, all pool state)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if service.IsBrewManaged() {
				if err := service.BrewStop(); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "brew services stop: %v\n", err)
				}
				// Remove any stale self-rolled agent from before the brew switch.
				_ = service.Uninstall()
				fmt.Fprintln(out, "✓ Daemon stopped (brew services)")
			} else {
				if err := service.Uninstall(); err != nil {
					return err
				}
				fmt.Fprintln(out, "✓ LaunchAgent removed")
			}

			// Always unmount any fuse overlays and re-assert nothing is left mounted.
			_ = withManager(func(m *pool.Manager) error {
				accts, _ := m.Store.ListAccounts()
				for _, a := range accts {
					if a.OverlayKind != string(overlay.KindFuse) {
						continue
					}
					_ = overlay.For(overlay.KindFuse).Teardown(pool.ClaudeDir(), a.ConfigDir)
				}
				return nil
			})

			if !purge {
				fmt.Fprintln(out, "  (pool accounts and state preserved; re-run `clp service install` to resume)")
				return nil
			}
			return purgeAll(cmd)
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove all pool accounts, dirs, and state (never touches ~/.claude)")
	return cmd
}

// purgeAll removes every pool account (overlay + Keychain items) and the
// state dir (which contains the account dirs). ~/.claude and plain claude's
// canonical credential are never touched.
func purgeAll(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	err := withManager(func(m *pool.Manager) error {
		accts, err := m.Store.ListAccounts()
		if err != nil {
			return err
		}
		for _, a := range accts {
			if err := m.Remove(a.ID, true); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "remove acct-%02d: %v\n", a.ID, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	_ = os.RemoveAll(pool.StateDir())
	fmt.Fprintln(out, "✓ Purged all pool state (~/.claude left intact)")
	return nil
}

// runServiceInstall starts the daemon: via `brew services` when Homebrew-managed
// (booting out any stale self-rolled agent first), else the self-rolled
// LaunchAgent.
func runServiceInstall(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	if service.IsBrewManaged() {
		// A source-build `clp service install` leaves a self-rolled
		// com.yasyf.cc-pool agent that would run alongside the brew one.
		// Boot it out before delegating.
		_ = service.Uninstall()
		if err := service.BrewStart(); err != nil {
			return fmt.Errorf("brew services start: %w", err)
		}
		fmt.Fprintln(out, "✓ Daemon started via brew services")
		return nil
	}
	if err := service.Install(); err != nil {
		return err
	}
	fmt.Fprintln(out, "✓ Daemon installed and started")
	return nil
}

// ensureDaemon makes sure a daemon at the current version is responding,
// silently starting (or restarting, after an upgrade) it as needed.
// Best-effort: failures are warnings, never fatal — no calling flow requires
// the daemon (selection and add validation fall back to direct sampling).
func ensureDaemon(cmd *cobra.Command) {
	want := version.String()
	if daemonAt(want) {
		return
	}
	if resp, err := daemon.NewClient().Health(); err == nil && resp.OK {
		// launchd's KeepAlive holds the old binary's image alive across
		// upgrades indefinitely; restart it onto the current binary.
		fmt.Fprintf(cmd.OutOrStdout(), "  restarting cc-pool daemon (%s → %s)…\n", resp.Version, want)
		if service.IsBrewManaged() {
			// `brew services start` is a no-op on a running service; stop
			// first, and say so if that fails (the stale daemon would
			// otherwise survive behind a "restarted" message).
			if err := service.BrewStop(); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: brew services stop: %v\n", err)
			}
		}
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "  starting the cc-pool daemon…")
	}
	if err := runServiceInstall(cmd); err != nil {
		// Re-check at the wanted version: a concurrent start or an
		// already-bootstrapped agent can fail the install while leaving a
		// healthy current daemon behind.
		if daemonAt(want) {
			return
		}
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: daemon start failed: %v\n  (run `clp service install` from a GUI session to enable background polling)\n", err)
		return
	}
	if !waitDaemon(want, 3*time.Second) {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"warning: daemon not responding at the current version yet; check `clp service status`")
	}
}

// daemonAt reports whether a healthy daemon at exactly wantVersion responds.
// Version-checked so a stale pre-upgrade daemon never counts as success.
func daemonAt(wantVersion string) bool {
	resp, err := daemon.NewClient().Health()
	return err == nil && resp.OK && resp.Version == wantVersion
}

// waitDaemon polls until a daemon at wantVersion responds or timeout elapses.
func waitDaemon(wantVersion string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if daemonAt(wantVersion) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
