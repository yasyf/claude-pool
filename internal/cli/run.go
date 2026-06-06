package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/yasyf/claude-pool/internal/daemon"
	"github.com/yasyf/claude-pool/internal/pool"
	"github.com/yasyf/claude-pool/internal/store"
)

func newRunCmd() *cobra.Command {
	var account int
	cmd := &cobra.Command{
		Use:   "run [-- claude args...]",
		Short: "Select an account and exec `claude`, owning the session lifecycle",
		Long: `run picks the best account, launches ` + "`claude`" + ` with CLAUDE_CONFIG_DIR set,
and owns the resulting process — so the session is tracked precisely and the
(possibly rotated) token is adopted on exit. Best for automation.`,
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withManager(func(m *pool.Manager) error {
				if err := requireInit(m); err != nil {
					return err
				}
				var a store.Account
				var err error
				if cmd.Flags().Changed("account") {
					a, err = m.Store.GetAccount(account)
				} else {
					a, err = chooseAccount(cmd, m)
				}
				if err != nil {
					return err
				}
				if err := m.SyncOverlay(a); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: overlay sync: %v\n", err)
				}
				if err := m.PreflightRefresh(cmd.Context(), a); err != nil && !errors.Is(err, pool.ErrNeedsLogin) {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
				}
				return execClaude(cmd, m, a, args)
			})
		},
	}
	cmd.Flags().IntVar(&account, "account", 0, "force a specific account id")
	return cmd
}

// chooseAccount prefers the daemon's pick, falling back to a live selection.
func chooseAccount(cmd *cobra.Command, m *pool.Manager) (store.Account, error) {
	if resp, ok := daemon.NewClient().Select(nil, 0, true); ok && resp.OK && resp.SelectedID != nil {
		return m.Store.GetAccount(*resp.SelectedID)
	}
	sr, err := m.Select(cmd.Context(), pool.SelectOptions{Live: true})
	if err != nil {
		return store.Account{}, err
	}
	return sr.Best, nil
}

// execClaude launches claude as a child, records the session, waits, and on
// exit adopts any rotated token (directly or via the daemon).
func execClaude(cmd *cobra.Command, m *pool.Manager, a store.Account, args []string) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("`claude` not found on PATH: %w", err)
	}
	child := exec.Command(bin, args...)
	child.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+a.ConfigDir)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr

	if err := child.Start(); err != nil {
		return err
	}
	pid := child.Process.Pid
	sessID, _ := m.Store.OpenSession(a.ID, pid, a.ConfigDir)
	fmt.Fprintf(cmd.ErrOrStderr(), "running claude on acct-%02d (pid %d, %s)\n", a.ID, pid, a.ConfigDir)

	waitErr := child.Wait()

	// Session check-in: prefer the daemon (event-driven adopt), else do it here.
	if resp, err := daemon.NewClient().Checkin(pid); err != nil || resp == nil || !resp.OK {
		_ = m.AdoptRotatedToken(a)
	}
	if sessID > 0 {
		_ = m.Store.CloseSession(sessID)
	}

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}
	return waitErr
}
