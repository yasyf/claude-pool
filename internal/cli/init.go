package cli

import (
	"errors"
	"fmt"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/yasyf/claude-pool/internal/overlay"
	"github.com/yasyf/claude-pool/internal/pool"
	"github.com/yasyf/claude-pool/internal/service"
)

func newInitCmd() *cobra.Command {
	var noService bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Register ~/.claude as acct-00 and set up the pool (does not move ~/.claude)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				out := cmd.OutOrStdout()
				res, err := m.Init(cmd.Context())
				if errors.Is(err, pool.ErrNotLoggedIn) {
					fmt.Fprintln(cmd.ErrOrStderr(),
						"No Claude credential found. Run `claude` and log in first, then re-run `clp init`.")
					return err
				}
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "✓ Registered acct-00 → %s (plain `claude` is untouched)\n", res.Account.ConfigDir)
				fmt.Fprintf(out, "✓ Overlay provider: %s\n", res.OverlayKind)
				fmt.Fprintf(out, "✓ acct-00 mirror Keychain item: %s\n", res.MirrorService)

				if res.OverlayKind == overlay.KindSymlink && !overlay.FuseBuilt() {
					fmt.Fprintln(out, dimStyle.Render(
						"  (tip: install fuse-t and a fuse-enabled build for a live mirror overlay: brew install macos-fuse-t/cask/fuse-t)"))
				}

				// Enable the background daemon.
				if !noService {
					if service.IsBrewManaged() {
						fmt.Fprintln(out, "  Enable the daemon with: brew services start claude-pool")
					} else {
						install := isTTY()
						if isTTY() {
							_ = huh.NewConfirm().
								Title("Install the claude-pool background daemon now?").
								Description("Keeps idle accounts refreshed and scores live (recommended).").
								Value(&install).
								Run()
						}
						if install {
							if err := runServiceInstall(cmd); err != nil {
								fmt.Fprintf(cmd.ErrOrStderr(), "service install failed: %v\n", err)
							}
						} else {
							fmt.Fprintln(out, "  Run `clp service install` later to enable the daemon.")
						}
					}
				}

				fmt.Fprintln(out, "\nNext: `clp add` to pool another subscription.")
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&noService, "no-service", false, "do not offer to install the daemon")
	return cmd
}
