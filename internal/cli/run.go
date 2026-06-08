package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/daemon"
	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/store"
)

// ccpAccountEnv forces a specific pool account for `ccp run`. The command
// parses no flags of its own (every argument passes through to claude), so the
// account override travels out-of-band in the environment.
const ccpAccountEnv = "CCP_ACCOUNT"

func newRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run [claude args...]",
		Short: "Select an account and exec `claude`, passing every arg through",
		Long: `run picks the best account and replaces itself with ` + "`claude`" + ` (via exec)
with CLAUDE_CONFIG_DIR set, so once claude starts cc-pool is gone from the
process tree — signals, the controlling terminal, and the exit code are all claude's.

Every argument is forwarded verbatim, with no ` + "`--`" + ` separator (e.g.
` + "`ccp run --resume`" + `). Set ` + ccpAccountEnv + `=<id> to force a specific account
instead of auto-selecting.

This is the imperative equivalent of:

    CLAUDE_CONFIG_DIR=$(ccp select) claude ...`,
		// Pass every argument straight through to claude; ccp owns no flags here.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withManager(func(m *pool.Manager) error {
				if err := requireInit(m); err != nil {
					return err
				}
				dir, err := resolveRunDir(cmd, m)
				if err != nil {
					return err
				}
				return execClaude(dir, args)
			})
		},
	}
	return cmd
}

// resolveRunDir picks the account `ccp run` launches on and returns its config
// dir. CCP_ACCOUNT forces a specific account; otherwise it mirrors `ccp select`:
// ensure a daemon (so it can adopt any token claude rotates once ccp has exec'd
// away), take its reserved pick, and fall back to a live selection only when the
// daemon is unreachable.
func resolveRunDir(cmd *cobra.Command, m *pool.Manager) (string, error) {
	cwd, _ := os.Getwd() // best-effort: an unreadable cwd just disables stickiness

	if v := os.Getenv(ccpAccountEnv); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil {
			return "", fmt.Errorf("%s must be an account id, got %q: %w", ccpAccountEnv, v, err)
		}
		a, err := m.Store.GetAccount(id)
		if err != nil {
			return "", fmt.Errorf("%s=%d: %w", ccpAccountEnv, id, err)
		}
		_ = m.RecordSticky(cwd, a.ID, time.Now()) // anchor future selects here
		return prepareAccount(cmd, m, a, accountName(a.Label))
	}

	// Fast path: the daemon's cached, reserved pick. EnsureRunning keeps a daemon
	// alive to adopt any rotated token after we exec away. A daemon that responds
	// is authoritative — only an unreachable one falls through to live selection.
	cl := daemon.NewClient()
	if cl.EnsureRunning(2 * time.Second) {
		if resp, ok := cl.Select(nil, 0, false, cwd); ok {
			switch {
			case resp.OK && resp.Dir != "":
				if resp.Sticky {
					step(cmd.ErrOrStderr(), "Reusing the account pinned to this directory.")
				} else {
					step(cmd.ErrOrStderr(), "Selected the emptiest account.")
				}
				return resp.Dir, nil
			case resp.Error != "":
				return "", errors.New(resp.Error)
			default:
				return "", pool.ErrNoneAvailable
			}
		}
	}

	// Live fallback (no daemon): sample + score synchronously. Select records
	// stickiness itself.
	sr, err := m.Select(cmd.Context(), pool.SelectOptions{Live: true, Cwd: cwd})
	if err != nil {
		return "", err
	}
	reason := fmt.Sprintf("%s (score %.1f)", accountName(sr.Best.Label), sr.Result.Score)
	if sr.Sticky {
		reason = fmt.Sprintf("%s (pinned to this directory)", accountName(sr.Best.Label))
	}
	return prepareAccount(cmd, m, sr.Best, reason)
}

// prepareAccount re-asserts the account's overlay and preflight-refreshes its
// token before launch — the daemonless equivalent of what the daemon does for
// its own picks — then returns the config dir. The choice is logged to stderr;
// stdout is left clean for claude.
func prepareAccount(cmd *cobra.Command, m *pool.Manager, a store.Account, reason string) (string, error) {
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
	step(cmd.ErrOrStderr(), "selected %s", reason)
	return a.ConfigDir, nil
}

// execClaude replaces this process with `claude`, forwarding args verbatim and
// pointing it at configDir via CLAUDE_CONFIG_DIR. It does not return on success.
func execClaude(configDir string, args []string) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("`claude` not found on PATH: %w", err)
	}
	argv := append([]string{"claude"}, args...)
	if err := syscall.Exec(bin, argv, execEnv(os.Environ(), configDir)); err != nil {
		return fmt.Errorf("exec claude: %w", err)
	}
	return nil // unreachable: a successful Exec never returns
}

// execEnv returns environ with any existing CLAUDE_CONFIG_DIR dropped and
// CLAUDE_CONFIG_DIR=configDir appended, so the launched claude sees exactly one
// (a duplicate key has platform-dependent getenv precedence).
func execEnv(environ []string, configDir string) []string {
	const key = "CLAUDE_CONFIG_DIR="
	out := make([]string, 0, len(environ)+1)
	for _, e := range environ {
		if strings.HasPrefix(e, key) {
			continue
		}
		out = append(out, e)
	}
	return append(out, key+configDir)
}
