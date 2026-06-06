package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/yasyf/claude-pool/internal/daemon"
	"github.com/yasyf/claude-pool/internal/overlay"
	"github.com/yasyf/claude-pool/internal/pool"
	"github.com/yasyf/claude-pool/internal/service"
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
				fmt.Fprintf(out, "LaunchAgent loaded: %v\n", service.Loaded())
				cl := daemon.NewClient()
				if resp, err := cl.Health(); err == nil && resp.OK {
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
			if err := service.Uninstall(); err != nil {
				return err
			}
			fmt.Fprintln(out, "✓ LaunchAgent removed")

			// Always unmount any fuse overlays and re-assert nothing is left mounted.
			_ = withManager(func(m *pool.Manager) error {
				accts, _ := m.Store.ListAccounts()
				for _, a := range accts {
					if a.IsZero || a.OverlayKind != string(overlay.KindFuse) {
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

// purgeAll removes every pool account (overlay + keychain mirror/items), the
// pool dir, and the state dir. ~/.claude and its canonical credential are never
// touched.
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
	_ = os.RemoveAll(pool.PoolDir())
	_ = os.RemoveAll(pool.StateDir())
	fmt.Fprintln(out, "✓ Purged all pool state (~/.claude left intact)")
	return nil
}

// runServiceInstall installs the LaunchAgent and reports status.
func runServiceInstall(cmd *cobra.Command) error {
	if err := service.Install(); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "✓ Daemon installed and started")
	return nil
}
