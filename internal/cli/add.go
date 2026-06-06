package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/yasyf/claude-pool/internal/pool"
)

func newAddCmd() *cobra.Command {
	var (
		label   string
		runNow  bool
		autoYes bool
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Pool another Claude subscription (interactive login)",
		Long: `add allocates a new account dir, sets up its overlay onto ~/.claude, then
walks you through logging that account in. After login it discovers the
account's Keychain item, takes over its ACL for prompt-free refresh, validates
with one usage call, and records it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				if err := requireInit(m); err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				pending, err := m.PrepareAdd()
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "Allocated acct-%02d → %s (overlay: %s)\n",
					pending.Index, pending.ConfigDir, pending.OverlayKind)

				doRun := runNow
				if isTTY() && !runNow && !autoYes {
					choice := "run"
					_ = huh.NewSelect[string]().
						Title("How do you want to log in this account?").
						Options(
							huh.NewOption("Run `claude /login` now in this terminal", "run"),
							huh.NewOption("I'll run the login command myself in another terminal", "manual"),
						).
						Value(&choice).
						Run()
					doRun = choice == "run"
				}

				if doRun {
					if err := runClaudeLogin(pending.ConfigDir); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "login command exited with error: %v\n", err)
					}
				} else {
					fmt.Fprintf(out, "\nRun this, complete the login, then return here:\n\n    %s\n\n", pending.LoginCommand)
					if isTTY() {
						cont := true
						_ = huh.NewConfirm().Title("Press enter once login is complete").Value(&cont).Run()
					}
				}

				// Optional human label.
				if label == "" && isTTY() {
					_ = huh.NewInput().
						Title("Label for this account (optional)").
						Placeholder("e.g. work@example.com").
						Value(&label).
						Run()
				}

				acct, err := m.FinalizeAdd(cmd.Context(), pending, label)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "could not finalize: %v\n", err)
					if shouldAbandon(cmd) {
						if aerr := m.AbandonAdd(pending); aerr != nil {
							fmt.Fprintf(cmd.ErrOrStderr(), "cleanup failed: %v\n", aerr)
						} else {
							fmt.Fprintf(out, "rolled back acct-%02d\n", pending.Index)
						}
					}
					return err
				}
				fmt.Fprintf(out, "✓ Added acct-%02d (%s). Keychain: %s\n", acct.ID, acct.Label, acct.KeychainService)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "human label for the account")
	cmd.Flags().BoolVar(&runNow, "run-login", false, "run `claude /login` immediately (non-interactive)")
	cmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "assume defaults; do not prompt")
	return cmd
}

// runClaudeLogin execs `claude /login` with CLAUDE_CONFIG_DIR set, attached to
// the current terminal so the user can complete the OAuth flow.
func runClaudeLogin(configDir string) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("`claude` not found on PATH: %w", err)
	}
	c := exec.Command(bin, "/login")
	c.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+configDir)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func shouldAbandon(cmd *cobra.Command) bool {
	if !isTTY() {
		return false
	}
	abandon := true
	_ = huh.NewConfirm().
		Title("Roll back this half-added account?").
		Value(&abandon).
		Run()
	return abandon
}
