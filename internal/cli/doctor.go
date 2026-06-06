package cli

import (
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/yasyf/claude-pool/internal/daemon"
	"github.com/yasyf/claude-pool/internal/keychain"
	"github.com/yasyf/claude-pool/internal/overlay"
	"github.com/yasyf/claude-pool/internal/pool"
	"github.com/yasyf/claude-pool/internal/store"
)

func newDoctorCmd() *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Re-validate Keychain items and overlays; repair drift with --fix",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				out := cmd.OutOrStdout()
				ok := true
				report := func(label string, healthy bool, detail string) {
					mark := "✓"
					if !healthy {
						mark = "✗"
						ok = false
					}
					if detail != "" {
						fmt.Fprintf(out, "%s %s — %s\n", mark, label, detail)
					} else {
						fmt.Fprintf(out, "%s %s\n", mark, label)
					}
				}

				// claude binary present (auto-update can move it).
				if _, err := exec.LookPath("claude"); err != nil {
					report("claude on PATH", false, err.Error())
				} else {
					report("claude on PATH", true, "")
				}

				// Daemon.
				if resp, err := daemon.NewClient().Health(); err == nil && resp.OK {
					report("daemon", true, resp.Version)
				} else {
					report("daemon", false, "not running (run `clp service install`)")
				}

				accts, err := m.Store.ListAccounts()
				if err != nil {
					return err
				}
				for _, a := range accts {
					checkAccount(cmd, m, a, fix, report)
				}

				if !ok {
					if fix {
						fmt.Fprintln(out, "\nApplied fixes where possible; re-run `clp doctor` to confirm.")
					} else {
						fmt.Fprintln(out, "\nIssues found. Re-run with --fix to repair.")
					}
				} else {
					fmt.Fprintln(out, "\nAll checks passed.")
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "attempt to repair detected drift")
	return cmd
}

func checkAccount(cmd *cobra.Command, m *pool.Manager, a store.Account, fix bool, report func(string, bool, string)) {
	prefix := fmt.Sprintf("acct-%02d", a.ID)

	// Keychain item readable.
	if _, err := keychain.Read(a.KeychainService, a.KeychainAccount); err != nil {
		if fix {
			if _, rerr := keychain.Reassert(a.KeychainService, a.KeychainAccount); rerr == nil {
				report(prefix+" keychain", true, "re-asserted")
			} else {
				report(prefix+" keychain", false, rerr.Error())
			}
		} else {
			report(prefix+" keychain", false, err.Error())
		}
	} else {
		report(prefix+" keychain", true, "")
	}

	// acct-00 mirror must stay in sync with the canonical default item.
	if a.IsZero {
		canon, cerr := keychain.Read(keychain.DefaultServiceName(), a.KeychainAccount)
		mirror, merr := keychain.Read(a.KeychainService, a.KeychainAccount)
		inSync := cerr == nil && merr == nil && canon.ClaudeAiOauth.AccessToken == mirror.ClaudeAiOauth.AccessToken
		if !inSync && fix && cerr == nil {
			if werr := keychain.Write(a.KeychainService, a.KeychainAccount, canon); werr == nil {
				report(prefix+" mirror", true, "re-synced from canonical")
			} else {
				report(prefix+" mirror", false, werr.Error())
			}
		} else {
			detail := ""
			if !inSync {
				detail = "drifted from ~/.claude credential"
			}
			report(prefix+" mirror", inSync, detail)
		}
		return // acct-00 has no overlay to check
	}

	// Overlay health.
	prov := overlay.For(overlay.Kind(a.OverlayKind))
	if err := prov.Health(pool.ClaudeDir(), a.ConfigDir); err != nil {
		if fix {
			if serr := prov.Sync(pool.ClaudeDir(), a.ConfigDir); serr == nil {
				report(prefix+" overlay", true, "re-asserted")
			} else {
				report(prefix+" overlay", false, serr.Error())
			}
		} else {
			report(prefix+" overlay", false, err.Error())
		}
	} else {
		report(prefix+" overlay", true, string(prov.Kind()))
	}
}
