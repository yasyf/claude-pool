package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/pool"
)

func newInitCmd() *cobra.Command {
	var noService bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Prepare the pool state dir and start the daemon (optional — `clp add` does this automatically)",
		Long: `init prepares ~/.cc-pool (state db, account dirs), records the overlay
provider, and starts the background daemon. It never touches ~/.claude or any
credential — accounts (including your main subscription) join via ` + "`clp add`" + `,
by adopting your current login or with their own independent login. Running it
explicitly is optional: ` + "`clp add`" + ` performs the same setup automatically.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				out := cmd.OutOrStdout()
				res, err := m.Init()
				if err != nil {
					return err
				}
				if res.Already {
					fmt.Fprintln(out, "✓ Pool already initialized (~/.cc-pool)")
				} else {
					fmt.Fprintln(out, "✓ Pool initialized (~/.cc-pool)")
				}
				fmt.Fprintf(out, "✓ Overlay provider: %s\n", res.OverlayKind)

				if res.OverlayKind == overlay.KindSymlink && !overlay.FuseBuilt() {
					fmt.Fprintln(out, dimStyle.Render(
						"  (tip: install fuse-t and a fuse-enabled build for a live mirror overlay: brew install macos-fuse-t/cask/fuse-t)"))
				}

				if !noService {
					ensureDaemon(cmd)
				}

				fmt.Fprintln(out, "\nNext: `clp add` to pool your subscriptions (including your main one).")
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&noService, "no-service", false, "do not start the background daemon now (`clp add` will still start it)")
	return cmd
}
