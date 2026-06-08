package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/pool"
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
				account, err := ccpAccountFromEnv()
				if err != nil {
					return err
				}
				cwd, _ := os.Getwd() // best-effort: an unreadable cwd just disables stickiness
				// `ccp run` is the imperative form of `ccp select | claude`: it shares
				// the exact selection pipeline, then execs instead of printing the dir.
				dir, line, err := resolveSelection(cmd, m, selectReq{account: account, cwd: cwd})
				if err != nil {
					return err
				}
				step(cmd.ErrOrStderr(), "%s", line)
				return execClaude(dir, args)
			})
		},
	}
	return cmd
}

// ccpAccountFromEnv reads the CCP_ACCOUNT override, returning nil when it is
// unset and an error when it is not a valid account id.
func ccpAccountFromEnv() (*int, error) {
	v := os.Getenv(ccpAccountEnv)
	if v == "" {
		return nil, nil
	}
	id, err := strconv.Atoi(v)
	if err != nil {
		return nil, fmt.Errorf("%s must be an account id, got %q: %w", ccpAccountEnv, v, err)
	}
	return &id, nil
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
