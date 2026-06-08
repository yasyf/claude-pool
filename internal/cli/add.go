package cli

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/store"
)

// addOptions carries `clp add`'s flags; bare `clp` reuses the flow with the
// zero value.
type addOptions struct {
	label   string
	runNow  bool
	autoYes bool
	count   int
}

func newAddCmd() *cobra.Command {
	var opts addOptions
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Pool one or more Claude subscriptions (interactive login)",
		Long: `add allocates a new account dir, sets up its overlay onto ~/.claude, seeds
the account's .claude.json from ~/.claude.json (so the session inherits your
settings and skips first-run onboarding), then walks you through logging that
account in. After login it discovers the account's Keychain item, takes over
its ACL for prompt-free refresh, validates with one usage call, and records it.

If the account you are currently logged into with plain ` + "`claude`" + ` is not in
the pool yet, add offers to adopt it: the credential is copied (read-only —
plain claude is untouched) into the new account's own Keychain item and
immediately refreshed onto its own token chain, skipping the login entirely.

When you do log in interactively, add watches for the credential to land and
closes claude for you — no need to exit it yourself.

On a fresh machine it also initializes the pool (~/.cc-pool) and starts the
background daemon automatically — no separate ` + "`clp init`" + ` needed.

Run interactively it loops, offering to add another account after each one. Use
--count to add a fixed number, or -y to add a single account without prompts.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				return runAdd(cmd, m, opts)
			})
		},
	}
	cmd.Flags().StringVar(&opts.label, "label", "", "human label for the (first) account")
	cmd.Flags().BoolVar(&opts.runNow, "run-login", false, "run `claude /login` immediately instead of asking")
	cmd.Flags().BoolVarP(&opts.autoYes, "yes", "y", false, "add a single account with no prompts")
	cmd.Flags().IntVar(&opts.count, "count", 0, "add exactly N accounts without the continue prompt")
	return cmd
}

// runAdd is the full add flow: auto-init, then the addOne loop. Both `clp add`
// and bare `clp` (on an empty pool) dispatch here.
func runAdd(cmd *cobra.Command, m *pool.Manager, opts addOptions) error {
	if err := ensureReady(cmd, m); err != nil {
		return err
	}
	var added []store.Account
	for i := 0; ; i++ {
		lbl := ""
		if i == 0 {
			lbl = opts.label // --label applies to the first account
		}
		acct, err := addOne(cmd, m, lbl, opts)
		if err != nil {
			if len(added) == 0 {
				return err // nothing added yet → propagate (nonzero exit)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "stopping after %d account(s): %v\n", len(added), err)
			break
		}
		added = append(added, *acct)
		if !addAnother(cmd, len(added), opts.count, opts.autoYes) {
			break
		}
	}
	summarizeAdds(cmd, m, added)
	return nil
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

// addOne runs the full prepare → adopt-or-login → finalize flow for a single
// account.
func addOne(cmd *cobra.Command, m *pool.Manager, label string, opts addOptions) (*store.Account, error) {
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
	if pending.PurgedStaleCredential {
		fmt.Fprintln(out, dimStyle.Render("  discarded a stale credential from a previous attempt"))
	}

	adopted := false
	if pending.ClaudeJSONSeed != pool.SeedKeptExisting {
		adopted, err = offerAdopt(cmd, m, pending, opts)
		if err != nil {
			return nil, err
		}
	}

	if !adopted {
		if err := loginFlow(cmd, pending, opts); err != nil {
			return nil, err
		}
	}

	prompt := label == "" && isTTY() && !opts.autoYes
	label = defaultLabel(label, pending.OverlayKind, pending.ConfigDir)
	if prompt {
		_ = huh.NewInput().
			Title("Label for this account (optional)").
			Placeholder("e.g. work@example.com").
			Value(&label). // prefilled with the account email when known
			Run()
	}

	acct, err := m.FinalizeAdd(cmd.Context(), pending, label)
	if err != nil {
		if acct != nil {
			// The row is registered and the credential is live — only the
			// best-effort usage validation failed (e.g. a network blip).
			// Abandoning here would orphan the row and destroy a working
			// credential; keep the account and surface the warning instead.
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
		} else {
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
	}
	fmt.Fprintf(out, "✓ Added acct-%02d (%s). Keychain: %s\n", acct.ID, acct.Label, acct.KeychainService)
	return acct, nil
}

// defaultLabel resolves a new account's label: an explicit --label wins;
// otherwise the account's logged-in email when readable, else empty (the
// prefill is decorative — identity-read failures stay silent).
func defaultLabel(explicit string, kind overlay.Kind, configDir string) string {
	if explicit != "" {
		return explicit
	}
	id, err := pool.AccountIdentity(kind, configDir)
	if err != nil {
		return ""
	}
	return id.EmailAddress
}

// offerAdopt offers to copy plain claude's current login into the pending
// account when that login is not in the pool yet. Interactive runs confirm
// (default yes); -y/--count/non-TTY adopt automatically with a printed notice.
// Adoption failure is not fatal: it falls back to the interactive login with a
// printed reason.
func offerAdopt(cmd *cobra.Command, m *pool.Manager, p *pool.PendingAdd, opts addOptions) (bool, error) {
	cand, err := m.AdoptCandidate()
	if err != nil {
		return false, err
	}
	if cand == nil {
		return false, nil
	}
	email := cand.Identity.EmailAddress
	if auto := opts.autoYes || opts.count > 0 || !isTTY(); auto {
		fmt.Fprintf(cmd.OutOrStdout(), "✓ adopting current login (%s)\n", email)
	} else {
		use := true
		err := huh.NewConfirm().
			Title(fmt.Sprintf("Use your current login (%s)?", email)).
			Description("Copies the credential into this account — no new login needed. Plain claude is untouched.").
			Value(&use).
			Run()
		if err != nil {
			return false, err // ^C must abort the add, not launch a login
		}
		if !use {
			return false, nil
		}
	}
	if err := m.AdoptCredential(cmd.Context(), p, cand.Identity); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "could not adopt current login: %v — falling back to interactive login\n", err)
		return false, nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ adopted current login (%s)\n", email)
	return true, nil
}

// loginFlow walks the user through the interactive login: either a watched
// `claude /login` in this terminal (closed automatically once the credential
// lands) or a manual command in another terminal (polled until it lands).
func loginFlow(cmd *cobra.Command, pending *pool.PendingAdd, opts addOptions) error {
	out := cmd.OutOrStdout()
	doRun := opts.runNow || opts.autoYes
	if isTTY() && !doRun {
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
		if err := runWatchedLogin(cmd.Context(), cmd, pending); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return err // cancellation or infrastructure failure
			}
			// claude exiting nonzero is not fatal; finalize decides, as before.
			fmt.Fprintf(cmd.ErrOrStderr(), "login command exited with error: %v\n", err)
		}
		return nil
	}

	fmt.Fprintf(out, "\nRun this, complete the login, then return here:\n\n    %s\n\n", pending.LoginCommand)
	if !isTTY() {
		return nil
	}
	if _, err := keychain.DiscoverAccount(pending.KeychainService); err == nil {
		// An item already exists (the kept-existing reuse path): polling cannot
		// tell reuse from a fresh login, so keep the explicit confirm.
		cont := true
		_ = huh.NewConfirm().
			Title("Press enter once login is complete (or to reuse the existing credential)").
			Value(&cont).
			Run()
		return nil
	}
	if err := waitForCredential(cmd.Context(), out, pending.KeychainService); err != nil {
		return err
	}
	waitForIdentity(cmd.Context(), pending.OverlayKind, pending.ConfigDir, identityPostExitGrace)
	return nil
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
