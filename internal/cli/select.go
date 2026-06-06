package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/daemon"
	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/store"
)

func newSelectCmd() *cobra.Command {
	var (
		noDaemon bool
		wait     bool
		account  int
		fresh    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "select",
		Short: "Print the config dir of the emptiest account (the hot path)",
		Long: `select scores every account and prints ONLY the chosen account's config
dir to stdout, so it composes as:

    CLAUDE_CONFIG_DIR=$(clp select) claude

Diagnostics go to stderr. When the daemon is running, select reads its cached
scores; otherwise it samples usage live.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				if err := requireInit(m); err != nil {
					return err
				}
				var acctPtr *int
				if cmd.Flags().Changed("account") {
					acctPtr = &account
				}
				// Best-effort: an unreadable cwd just disables stickiness, never
				// fails select.
				cwd, _ := os.Getwd()

				// Fast path: ask the daemon for its cached pick.
				if !noDaemon {
					if dir, done, err := selectViaDaemon(cmd, acctPtr, wait, cwd); done {
						if err != nil {
							return err
						}
						fmt.Fprintln(cmd.OutOrStdout(), dir)
						return nil
					}
				}

				// Live path (no daemon): sample + score synchronously.
				return selectLive(cmd, m, acctPtr, wait, fresh, cwd)
			})
		},
	}
	cmd.Flags().BoolVar(&noDaemon, "no-daemon", false, "do not use the daemon; sample usage live")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until an account is available instead of failing")
	cmd.Flags().IntVar(&account, "account", 0, "force a specific account id")
	cmd.Flags().DurationVar(&fresh, "fresh", pool.DefaultFreshFor, "reuse cached usage newer than this (live mode)")
	return cmd
}

// selectViaDaemon attempts a daemon-served selection. done=false means the
// daemon was unreachable and the caller should fall back to live selection.
func selectViaDaemon(cmd *cobra.Command, account *int, wait bool, cwd string) (dir string, done bool, err error) {
	cl := daemon.NewClient()
	// Auto-spawn a detached daemon if none is running (≤2s), then fall through
	// to live selection if it still doesn't come up.
	if !cl.EnsureRunning(2 * time.Second) {
		return "", false, nil
	}
	// pid 0: clp exits before `claude` starts, so its pid is useless for
	// session tracking. We still want a reservation (anti-thundering-herd), but
	// no session row — procscan attributes the real claude process. `clp run`
	// is the path that records real-pid sessions.
	resp, ok := cl.Select(account, 0, false, cwd)
	if !ok {
		return "", false, nil
	}
	if resp.OK && resp.Dir != "" {
		if resp.Sticky {
			fmt.Fprintf(cmd.ErrOrStderr(), "selected via daemon (sticky): %s\n", resp.Dir)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "selected via daemon: %s\n", resp.Dir)
		}
		return resp.Dir, true, nil
	}
	if !resp.OK && wait && resp.SoonestReset != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "all accounts busy; waiting until %s\n", resp.SoonestReset.Format(time.Kitchen))
		// Fall back to live waiting loop for simplicity.
		return "", false, nil
	}
	if resp.Error != "" {
		return "", true, errors.New(resp.Error)
	}
	return "", true, pool.ErrNoneAvailable
}

// selectLive performs a daemonless selection, optionally waiting.
func selectLive(cmd *cobra.Command, m *pool.Manager, account *int, wait bool, fresh time.Duration, cwd string) error {
	if account != nil {
		a, err := m.Store.GetAccount(*account)
		if err != nil {
			return err
		}
		_ = m.RecordSticky(cwd, a.ID, time.Now()) // best-effort: anchor future selects here
		return emitChoice(cmd, m, a, fmt.Sprintf("forced acct-%02d", a.ID))
	}
	opts := pool.SelectOptions{Live: true, FreshFor: fresh, Cwd: cwd}
	for {
		sr, err := m.Select(cmd.Context(), opts)
		if errors.Is(err, pool.ErrNoneAvailable) {
			if !wait {
				fmt.Fprintln(cmd.ErrOrStderr(), "no account available (all rate-limited)")
				return err
			}
			reset, ok := sr.SoonestReset()
			d := 15 * time.Second
			if ok {
				fmt.Fprintf(cmd.ErrOrStderr(), "all rate-limited; soonest reset %s\n", reset.Format(time.Kitchen))
				if until := time.Until(reset); until > 0 && until < d {
					d = until
				}
			}
			select {
			case <-cmd.Context().Done():
				return cmd.Context().Err()
			case <-time.After(d):
				continue
			}
		}
		if err != nil {
			return err
		}
		reason := fmt.Sprintf("acct-%02d score %.1f", sr.Best.ID, sr.Result.Score)
		if sr.Sticky {
			reason = fmt.Sprintf("acct-%02d (sticky)", sr.Best.ID)
		}
		return emitChoice(cmd, m, sr.Best, reason)
	}
}

// emitChoice preflight-refreshes the chosen account, prints its dir to stdout
// (and only its dir), and a diagnostic line to stderr.
func emitChoice(cmd *cobra.Command, m *pool.Manager, a store.Account, reason string) error {
	// Re-assert the overlay so the launched session sees any new top-level
	// ~/.claude entries (replaces the old explicit `clp sync`).
	if err := m.SyncOverlay(a); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: overlay sync: %v\n", err)
	}
	if err := m.PreflightRefresh(cmd.Context(), a); err != nil {
		if errors.Is(err, pool.ErrNeedsLogin) {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: acct-%02d needs re-login (`clp add` or `claude /login`)\n", a.ID)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
		}
	}
	fmt.Fprintln(cmd.OutOrStdout(), a.ConfigDir)
	fmt.Fprintf(cmd.ErrOrStderr(), "selected %s\n", reason)
	return nil
}
