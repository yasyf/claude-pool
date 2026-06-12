package cli

import (
	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/mountd"
	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/pool"
)

// newMountHolderCmd is the hidden entry point for the detached mount-holder
// process spawned by mountd.EnsureRunning.
func newMountHolderCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:    "mount-holder",
		Short:  "Run the detached fuse mount holder",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// host is nil in a non-fuse build; Server.Run refuses loudly.
			host, _ := overlay.InProcessFuse()
			s := &mountd.Server{Socket: socket, Host: host, Probe: overlay.HostProbe}
			return s.Run(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&socket, "socket", pool.MountsSocketPath(), "unix socket path to serve")
	return cmd
}
