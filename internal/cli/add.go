package cli

import (
	"errors"
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
		Short: "Add a Claude subscription to the pool",
		Long: `add pools a Claude subscription. It sets up a new account, logs it in, and
records it so clp can route sessions to it.

Each account logs in with its own ` + "`claude /login`" + `, so it gets its own token
chain. Plain claude stays logged in and untouched.

On a fresh machine, add also sets up the pool and starts the background daemon,
so you do not need a separate ` + "`clp init`" + `.

Run it without flags to add accounts one at a time; it offers to add another
after each. Use --count to add a set number, or -y to add one and log in right
away.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				return runAdd(cmd, m, opts)
			})
		},
	}
	cmd.Flags().StringVar(&opts.label, "label", "", "name for the first account")
	cmd.Flags().BoolVar(&opts.runNow, "run-login", false, "log in immediately instead of asking how")
	cmd.Flags().BoolVarP(&opts.autoYes, "yes", "y", false, "add one account and log in right away")
	cmd.Flags().IntVar(&opts.count, "count", 0, "add exactly N accounts, no continue prompt")
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
			warn(cmd.ErrOrStderr(), "stopping after %s: %v", plural(len(added), "account"), err)
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
		if _, err := m.Init(); err != nil {
			return err
		}
		success(cmd.OutOrStdout(), "Set up cc-pool on this machine.")
	}
	ensureDaemon(cmd)
	return nil
}

// addOne runs the full prepare → login → finalize flow for a single account.
func addOne(cmd *cobra.Command, m *pool.Manager, label string, opts addOptions) (*store.Account, error) {
	out := cmd.OutOrStdout()
	pending, err := m.PrepareAdd()
	if err != nil {
		return nil, err
	}
	step(out, "Setting up a new account…")
	switch pending.ClaudeJSONSeed {
	case pool.SeedCopied:
		note(out, "Carried over your settings, so there's no first-run setup.")
	case pool.SeedNoSource:
		note(out, "No settings to carry over; this account will do first-run setup.")
	case pool.SeedKeptExisting:
		note(out, "This account has a login from an earlier attempt. Logging in again is safe, or continue to reuse it.")
	}
	if pending.PurgedStaleCredential {
		note(out, "Cleared a leftover login from a previous attempt.")
	}

	if err := loginFlow(cmd, pending, opts); err != nil {
		return nil, err
	}

	prompt := label == "" && isTTY() && !opts.autoYes
	label = defaultLabel(label, pending.OverlayKind, pending.ConfigDir)
	if prompt {
		_ = huh.NewInput().
			Title("Name for this account (optional)").
			Placeholder("e.g. work@example.com").
			Value(&label). // prefilled with the account email when known
			WithTheme(clpTheme()).
			Run()
	}

	acct, err := m.FinalizeAdd(cmd.Context(), pending, label)
	if err != nil {
		if acct != nil {
			// The row is registered and the credential is live — only the
			// best-effort usage check failed (e.g. a network blip). Keep the
			// account and surface a soft warning rather than orphan the row.
			warn(cmd.ErrOrStderr(), "added the account, but its first usage check failed; run `clp doctor` to retry")
		} else {
			fail(cmd.ErrOrStderr(), "couldn't finish adding the account: %v", err)
			if shouldAbandon(cmd) {
				if aerr := m.AbandonAdd(pending); aerr != nil {
					warn(cmd.ErrOrStderr(), "cleanup failed: %v", aerr)
				} else {
					step(out, "Rolled back the account.")
				}
			}
			return nil, err
		}
	}
	name := acct.Label
	if name == "" {
		name = "the account"
	}
	success(out, "Added %s.", name)
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

// loginFlow walks the user through the interactive login: either a watched
// `claude /login` in this terminal (closed automatically once the credential
// lands) or a manual command in another terminal (polled until it lands).
func loginFlow(cmd *cobra.Command, pending *pool.PendingAdd, opts addOptions) error {
	out := cmd.OutOrStdout()
	doRun := opts.runNow || opts.autoYes
	if isTTY() && !doRun {
		choice := "run"
		_ = huh.NewSelect[string]().
			Title("How do you want to log in?").
			Options(
				huh.NewOption("Log in now, in this terminal", "run"),
				huh.NewOption("I'll log in from another terminal", "manual"),
			).
			Value(&choice).
			WithTheme(clpTheme()).
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
			warn(cmd.ErrOrStderr(), "the login command exited with an error: %v", err)
		}
		return nil
	}

	step(out, "\nRun this in another terminal, finish the login, then come back:\n\n    %s\n", pending.LoginCommand)
	if !isTTY() {
		return nil
	}
	if _, err := keychain.DiscoverAccount(pending.KeychainService); err == nil {
		// An item already exists (the kept-existing reuse path): polling cannot
		// tell reuse from a fresh login, so keep the explicit confirm.
		cont := true
		_ = huh.NewConfirm().
			Title("Press enter when the login is done").
			Value(&cont).
			WithTheme(clpTheme()).
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
	if err := huh.NewConfirm().Title("Add another account?").Value(&again).WithTheme(clpTheme()).Run(); err != nil {
		warn(cmd.ErrOrStderr(), "prompt failed: %v", err)
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
		names[i] = a.Label
		if names[i] == "" {
			names[i] = "a new account"
		}
	}
	total := len(added)
	if all, err := m.Store.ListAccounts(); err == nil {
		total = len(all)
	}
	step(cmd.OutOrStdout(), "\nAdded %s. The pool now has %s.",
		strings.Join(names, ", "), plural(total, "account"))
}

func shouldAbandon(cmd *cobra.Command) bool {
	if !isTTY() {
		return false
	}
	abandon := true
	_ = huh.NewConfirm().
		Title("Roll back this incomplete account?").
		Value(&abandon).
		WithTheme(clpTheme()).
		Run()
	return abandon
}
