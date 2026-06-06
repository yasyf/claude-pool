package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/store"
)

func newAddCmd() *cobra.Command {
	var (
		label   string
		runNow  bool
		autoYes bool
		count   int
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Pool one or more Claude subscriptions (interactive login)",
		Long: `add allocates a new account dir, sets up its overlay onto ~/.claude, seeds
the account's .claude.json from ~/.claude.json (so the session inherits your
settings and skips first-run onboarding), then walks you through logging that
account in. After login it discovers the account's Keychain item, takes over
its ACL for prompt-free refresh, validates with one usage call, and records it.

On a fresh machine it also initializes the pool (~/.cc-pool) and starts the
background daemon automatically — no separate ` + "`clp init`" + ` needed.

Run interactively it loops, offering to add another account after each one. Use
--count to add a fixed number, or -y to add a single account without prompts.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				if err := ensureReady(cmd, m); err != nil {
					return err
				}
				var added []store.Account
				for i := 0; ; i++ {
					lbl := ""
					if i == 0 {
						lbl = label // --label applies to the first account
					}
					acct, err := addOne(cmd, m, lbl, runNow || autoYes)
					if err != nil {
						if len(added) == 0 {
							return err // nothing added yet → propagate (nonzero exit)
						}
						fmt.Fprintf(cmd.ErrOrStderr(), "stopping after %d account(s): %v\n", len(added), err)
						break
					}
					added = append(added, *acct)
					if !addAnother(cmd, len(added), count, autoYes) {
						break
					}
				}
				summarizeAdds(cmd, m, added)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "human label for the (first) account")
	cmd.Flags().BoolVar(&runNow, "run-login", false, "run `claude /login` immediately instead of asking")
	cmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "add a single account with no prompts")
	cmd.Flags().IntVar(&count, "count", 0, "add exactly N accounts without the continue prompt")
	return cmd
}

// ensureReady auto-initializes the pool and starts the daemon, so `clp add`
// works from a fresh machine with no prior `clp init`.
func ensureReady(cmd *cobra.Command, m *pool.Manager) error {
	ok, err := m.Initialized()
	if err != nil {
		return err
	}
	if !ok {
		res, err := m.Init()
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "✓ Pool initialized (~/.cc-pool, overlay: %s)\n", res.OverlayKind)
	}
	ensureDaemon(cmd)
	return nil
}

// addOne runs the full prepare → login → finalize flow for a single account.
func addOne(cmd *cobra.Command, m *pool.Manager, label string, runNow bool) (*store.Account, error) {
	out := cmd.OutOrStdout()
	pending, err := m.PrepareAdd()
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "Allocated acct-%02d → %s (overlay: %s)\n",
		pending.Index, pending.ConfigDir, pending.OverlayKind)
	switch pending.ClaudeJSONSeed {
	case pool.SeedCopied:
		fmt.Fprintln(out, dimStyle.Render("  seeded .claude.json (settings + onboarding inherited; login sets the account)"))
	case pool.SeedNoSource:
		fmt.Fprintln(out, "  note: no ~/.claude.json found — claude will run first-run onboarding for this account")
	case pool.SeedKeptExisting:
		fmt.Fprintln(out, "  note: this dir already holds a logged-in .claude.json from an earlier attempt;")
		fmt.Fprintln(out, "  logging in again is safe — or pick the manual option and just continue to reuse it")
	}

	doRun := runNow
	if isTTY() && !runNow {
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
		return nil, err
	}
	fmt.Fprintf(out, "✓ Added acct-%02d (%s). Keychain: %s\n", acct.ID, acct.Label, acct.KeychainService)
	return acct, nil
}

// addAnother decides whether the loop continues after a successful add.
func addAnother(cmd *cobra.Command, done, count int, autoYes bool) bool {
	if count > 0 {
		return done < count
	}
	if autoYes || !isTTY() {
		return false
	}
	again := false
	if err := huh.NewConfirm().Title("Add another account?").Value(&again).Run(); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "prompt error: %v\n", err)
		return false
	}
	return again
}

// summarizeAdds prints a closing summary of what was added.
func summarizeAdds(cmd *cobra.Command, m *pool.Manager, added []store.Account) {
	if len(added) == 0 {
		return
	}
	names := make([]string, len(added))
	for i, a := range added {
		names[i] = fmt.Sprintf("acct-%02d", a.ID)
	}
	total := len(added)
	if all, err := m.Store.ListAccounts(); err == nil {
		total = len(all)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nAdded %s; pool now has %d account(s).\n",
		strings.Join(names, ", "), total)
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
