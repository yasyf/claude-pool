package cli

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/yasyf/claude-pool/internal/overlay"
	"github.com/yasyf/claude-pool/internal/pool"
	"github.com/yasyf/claude-pool/internal/store"
)

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List pool accounts (static, no usage fetch)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				accts, err := m.Store.ListAccounts()
				if err != nil {
					return err
				}
				if len(accts) == 0 {
					fmt.Fprintln(cmd.ErrOrStderr(), "no accounts — run `clp init` then `clp add`")
					return nil
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
				fmt.Fprintln(tw, "ACCT\tLABEL\tOVERLAY\tCONFIG DIR\tKEYCHAIN SERVICE")
				for _, a := range accts {
					fmt.Fprintf(tw, "acct-%02d\t%s\t%s\t%s\t%s\n",
						a.ID, a.Label, a.OverlayKind, a.ConfigDir, a.KeychainService)
				}
				return tw.Flush()
			})
		},
	}
}

func newSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Re-assert each account's overlay (pick up new ~/.claude entries)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				accts, err := m.Store.ListAccounts()
				if err != nil {
					return err
				}
				for _, a := range accts {
					if a.IsZero {
						continue // acct-00 IS the base; nothing to overlay
					}
					prov := overlay.For(overlay.Kind(a.OverlayKind))
					if err := prov.Sync(pool.ClaudeDir(), a.ConfigDir); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "acct-%02d: %v\n", a.ID, err)
						continue
					}
					fmt.Fprintf(cmd.OutOrStdout(), "acct-%02d synced (%s)\n", a.ID, a.OverlayKind)
				}
				return nil
			})
		},
	}
}

func newRemoveCmd() *cobra.Command {
	var keepCred bool
	cmd := &cobra.Command{
		Use:   "remove <account-id>",
		Short: "Remove an account from the pool",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid account id %q", args[0])
			}
			return withManager(func(m *pool.Manager) error {
				if err := m.Remove(id, !keepCred); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "removed acct-%02d\n", id)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&keepCred, "keep-credential", false, "do not delete the account's Keychain item")
	return cmd
}

// newEnvCmd prints shell export lines to launch a chosen account, including the
// acct-00 escape hatch (CLAUDE_SECURESTORAGE_CONFIG_DIR="" forces the canonical
// un-suffixed Keychain item while CLAUDE_CONFIG_DIR points at ~/.claude).
func newEnvCmd() *cobra.Command {
	var account int
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Print shell export lines for an account (eval-friendly)",
		Long: `env prints the environment needed to launch a specific account:

    eval "$(clp env --account 1)"; claude

For acct-00 it emits the escape-hatch var so the canonical credential is used.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				if err := requireInit(m); err != nil {
					return err
				}
				var a store.Account
				var err error
				if cmd.Flags().Changed("account") {
					a, err = m.Store.GetAccount(account)
				} else {
					var sr *pool.SelectResult
					sr, err = m.Select(cmd.Context(), pool.SelectOptions{Live: true})
					if err == nil {
						a = sr.Best
					}
				}
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "export CLAUDE_CONFIG_DIR=%s\n", shellQuote(a.ConfigDir))
				if a.IsZero {
					// Force the canonical un-suffixed item for ~/.claude.
					fmt.Fprintln(out, `export CLAUDE_SECURESTORAGE_CONFIG_DIR=''`)
				}
				return nil
			})
		},
	}
	cmd.Flags().IntVar(&account, "account", 0, "account id (defaults to the best account)")
	return cmd
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
