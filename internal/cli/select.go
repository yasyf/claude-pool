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
	"github.com/yasyf/cc-pool/internal/version"
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
		Short: "Print the config dir of the emptiest account",
		Long: `select scores every account and prints only the chosen account's config dir
to stdout, so it composes as:

    CLAUDE_CONFIG_DIR=$(ccp select) claude

Diagnostics go to stderr. With the daemon running, select reads its cached
scores; otherwise it samples usage live.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				if err := requireInit(m); err != nil {
					return err
				}
				// Best-effort: an unreadable cwd just disables stickiness.
				cwd, _ := os.Getwd()
				req := selectReq{wait: wait, fresh: fresh, noDaemon: noDaemon, cwd: cwd}
				if cmd.Flags().Changed("account") {
					req.account = &account
				}
				dir, line, err := resolveSelection(cmd, m, req)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), dir)
				announceLine(cmd, line)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&noDaemon, "no-daemon", false, "do not use the daemon; sample usage live")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until an account is available instead of failing")
	cmd.Flags().IntVar(&account, "account", 0, "force a specific account id")
	cmd.Flags().DurationVar(&fresh, "fresh", pool.DefaultFreshFor, "reuse cached usage newer than this (live mode)")
	return cmd
}

// selectReq carries the per-invocation knobs that differ between `ccp run` and
// `ccp select`; the pipeline (forced → daemon → live) is identical.
type selectReq struct {
	account  *int          // forced pick (CCP_ACCOUNT / --account); nil = auto-score
	wait     bool          // wait for availability instead of failing (--wait)
	fresh    time.Duration // live-mode cache window (--fresh)
	noDaemon bool          // skip the daemon, sample live (--no-daemon)
	cwd      string        // keys select stickiness; empty disables it
}

// resolveSelection runs the full account-selection pipeline shared by `ccp run`
// and `ccp select` and returns the chosen config dir plus a formatted diagnostic
// line. A forced account resolves directly; otherwise it takes the daemon's
// reserved pick, falling back to live scoring (optionally waiting) when the
// daemon is unreachable or every account is busy. Picks it makes itself
// (forced/live) get an overlay sync + preflight refresh; a daemon-served pick is
// already prepared by the daemon (its poller syncs overlays and it preflights its
// own pick). Only warnings reach stderr — the caller owns dir output and printing
// the diagnostic line.
func resolveSelection(cmd *cobra.Command, m *pool.Manager, req selectReq) (dir, line string, err error) {
	// Forced account: resolve directly, no scoring (hence no headroom to report).
	if req.account != nil {
		a, err := m.Store.GetAccount(*req.account)
		if err != nil {
			return "", "", err
		}
		_ = m.RecordSticky(req.cwd, a.ID, time.Now()) // anchor future selects here
		dir, err := prepareAccount(cmd, m, a)
		// Forced pick: no scoring, so no usage to report (hasUsage=false → bare name).
		return dir, selectionLine(accountName(a.Label), false, false, 0, 0), err
	}

	// Fast path: the daemon's cached, reserved pick. EnsureRunning keeps a daemon
	// alive to adopt any token claude rotates after we exec away; a version-skewed
	// daemon scores with stale logic, so ignore it until status/add/init restarts
	// it onto the current binary.
	if !req.noDaemon {
		cl := daemon.NewClient()
		if cl.EnsureRunning(2*time.Second) && daemonAt(version.String()) {
			// pid 0, no session row: ccp exits before claude starts, so procscan
			// attributes the live process. We still reserve (anti-thundering-herd).
			if resp, ok := cl.Select(nil, 0, false, req.cwd); ok {
				switch {
				case resp.OK && resp.Dir != "":
					return resp.Dir, daemonSelectionLine(m, resp), nil
				case resp.Error != "":
					return "", "", errors.New(resp.Error)
				case req.wait:
					if resp.SoonestReset != nil {
						step(cmd.ErrOrStderr(), "All accounts are busy; waiting until %s.", humanizeReset(*resp.SoonestReset))
					}
					// fall through to the live waiting loop
				default:
					return "", "", pool.ErrNoneAvailable
				}
			}
		}
	}

	// Live path (no daemon, or waiting): sample + score synchronously. Select
	// records stickiness itself.
	opts := pool.SelectOptions{Live: true, FreshFor: req.fresh, Cwd: req.cwd}
	for {
		sr, err := m.Select(cmd.Context(), opts)
		if errors.Is(err, pool.ErrNoneAvailable) {
			if !req.wait {
				step(cmd.ErrOrStderr(), "No account is available right now; all are rate-limited.")
				return "", "", err
			}
			reset, ok := sr.SoonestReset()
			d := 15 * time.Second
			if ok {
				step(cmd.ErrOrStderr(), "All accounts are rate-limited; soonest reset at %s.", humanizeReset(reset))
				if until := time.Until(reset); until > 0 && until < d {
					d = until
				}
			}
			select {
			case <-cmd.Context().Done():
				return "", "", cmd.Context().Err()
			case <-time.After(d):
				continue
			}
		}
		if err != nil {
			return "", "", err
		}
		dir, err := prepareAccount(cmd, m, sr.Best)
		return dir, liveSelectionLine(sr), err
	}
}

// prepareAccount re-asserts the account's overlay and preflight-refreshes its
// token — the daemonless equivalent of what the daemon does for its own picks —
// then returns the config dir. Warnings go to stderr; the caller prints the
// success line.
func prepareAccount(cmd *cobra.Command, m *pool.Manager, a store.Account) (string, error) {
	if err := m.SyncOverlay(a); err != nil {
		warn(cmd.ErrOrStderr(), "couldn't sync this account's settings: %v", err)
	}
	if err := m.PreflightRefresh(cmd.Context(), a); err != nil {
		if errors.Is(err, pool.ErrNeedsLogin) {
			warn(cmd.ErrOrStderr(), "%s needs to log in again; run `ccp add` or `claude /login`", accountName(a.Label))
		} else {
			warn(cmd.ErrOrStderr(), "%v", err)
		}
	}
	return a.ConfigDir, nil
}

// announceLine prints the selection diagnostic to stderr, but only when stdout is
// an interactive terminal — captured/piped callers ($(ccp select)) get the bare
// dir on stdout and nothing else.
func announceLine(cmd *cobra.Command, line string) {
	if !stdoutIsTTY() {
		return
	}
	step(cmd.ErrOrStderr(), "%s", line)
}

// selectionLine formats the selection diagnostic: "Selected <name>", or
// "Reusing <name> (pinned)" for a sticky pick, plus raw 5h/7d percent-used when
// known. The account name is accented and the usage figures health-tinted.
func selectionLine(name string, sticky, hasUsage bool, used5, used7 float64) string {
	verb := "Selected"
	styledName := bestStyle.Render(name)
	if sticky {
		verb = "Reusing"
		styledName += dimStyle.Render(" (pinned)")
	}
	return fmt.Sprintf("%s %s%s", verb, styledName, usageSuffix(hasUsage, used5, used7))
}

// daemonSelectionLine builds the diagnostic from a daemon select reply: the name
// resolved from SelectedID (degrading to "account") plus its raw 5h/7d usage.
func daemonSelectionLine(m *pool.Manager, resp *daemon.Response) string {
	return selectionLine(daemonAccountName(m, resp.SelectedID), resp.Sticky, resp.HasUsage, 100-resp.Remaining5h, 100-resp.Remaining7d)
}

// liveSelectionLine builds the diagnostic from a live scoring result.
func liveSelectionLine(sr *pool.SelectResult) string {
	return selectionLine(accountName(sr.Best.Label), sr.Sticky, sr.HasUsage, sr.Util5h, sr.Util7d)
}
