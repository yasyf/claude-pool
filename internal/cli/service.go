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
		Short: "Manage the background daemon",
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
		Short: "Stop and remove the LaunchAgent; --purge also removes pool state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if service.IsBrewManaged() {
				if err := service.BrewStop(); err != nil {
					warn(cmd.ErrOrStderr(), "couldn't stop the brew service: %v", err)
				}
				// Remove any stale self-rolled agent from before the brew switch.
				_ = service.Uninstall()
				success(out, "Stopped the daemon.")
			} else {
				if err := service.Uninstall(); err != nil {
					return err
				}
				success(out, "Removed the LaunchAgent.")
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
				note(out, "Your accounts and state are preserved. Run `clp service install` to resume.")
				return nil
			}
			return purgeAll(cmd)
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove all pool accounts and state; never touches ~/.claude")
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
				warn(cmd.ErrOrStderr(), "couldn't remove acct-%02d: %v", a.ID, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	_ = os.RemoveAll(pool.StateDir())
	success(out, "Purged all pool state. ~/.claude is untouched.")
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
		success(out, "Started the daemon.")
		return nil
	}
	if err := service.Install(); err != nil {
		return err
	}
	success(out, "Installed and started the daemon.")
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
		step(cmd.OutOrStdout(), "Restarting the cc-pool daemon to pick up the new version…")
		if service.IsBrewManaged() {
			// `brew services start` is a no-op on a running service; stop
			// first, and say so if that fails (the stale daemon would
			// otherwise survive behind a "restarted" message).
			if err := service.BrewStop(); err != nil {
				warn(cmd.ErrOrStderr(), "couldn't stop the brew service: %v", err)
			}
		}
	} else {
		step(cmd.OutOrStdout(), "Starting the cc-pool daemon…")
	}
	if err := runServiceInstall(cmd); err != nil {
		// Re-check at the wanted version: a concurrent start or an
		// already-bootstrapped agent can fail the install while leaving a
		// healthy current daemon behind.
		if daemonAt(want) {
			return
		}
		warn(cmd.ErrOrStderr(),
			"couldn't start the daemon: %v; run `clp service install` from a GUI session to enable background polling", err)
		return
	}
	if !waitDaemon(want, 3*time.Second) {
		warn(cmd.ErrOrStderr(), "the daemon isn't responding yet; check `clp service status`")
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
