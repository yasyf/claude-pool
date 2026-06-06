// Package cli wires up the cobra command tree for claude-pool. The binary is
// installed as `claude-pool` with a `clp` symlink; both dispatch here.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/yasyf/claude-pool/internal/pool"
	"github.com/yasyf/claude-pool/internal/version"
)

// NewRootCmd builds the root command and attaches all subcommands.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "claude-pool",
		Short: "Predictive multi-account load-balancing for Claude Code",
		Long: `claude-pool (clp) pools several Claude subscriptions and launches each
session on the emptiest account:

    CLAUDE_CONFIG_DIR=$(clp select) claude

Plain ` + "`claude`" + ` keeps working untouched on ~/.claude (acct-00).`,
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("{{.Version}}\n")

	root.AddCommand(
		newInitCmd(),
		newAddCmd(),
		newSelectCmd(),
		newStatusCmd(),
		newListCmd(),
		newSyncCmd(),
		newRunCmd(),
		newEnvCmd(),
		newDoctorCmd(),
		newRemoveCmd(),
		newServiceCmd(),
		newDaemonCmd(),
	)
	return root
}

// withManager opens a Manager, runs fn, and closes it.
func withManager(fn func(*pool.Manager) error) error {
	m, err := pool.Open()
	if err != nil {
		return err
	}
	defer m.Close()
	return fn(m)
}

// requireInit returns an error if the pool has not been initialized.
func requireInit(m *pool.Manager) error {
	ok, err := m.Initialized()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("pool not initialized — run `clp init` first")
	}
	return nil
}
