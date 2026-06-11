package cli

import (
	"errors"
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/daemon"
	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/store"
)

func newDoctorCmd() *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check accounts' Keychain items and overlays; --fix repairs drift",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(func(m *pool.Manager) error {
				out := cmd.OutOrStdout()
				ok := true
				report := func(label string, healthy bool, detail string) {
					mark := okStyle.Render("✓")
					if !healthy {
						mark = badStyle.Render("✗")
						ok = false
					}
					if detail != "" {
						fmt.Fprintf(out, "%s %s: %s\n", mark, label, detail)
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
					report("daemon", false, "not running; run `ccp service install`")
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
						fmt.Fprintln(out, "\nApplied fixes where possible; re-run `ccp doctor` to confirm.")
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

	// Credential usable: the Keychain item (readable under our ACL) or, when the
	// Keychain is unavailable (e.g. headless), claude's plaintext
	// .credentials.json fallback.
	_, kerr := keychain.Read(a.KeychainService, a.KeychainAccount)
	switch {
	case kerr == nil:
		report(prefix+" keychain", true, "")
	case errors.Is(kerr, keychain.ErrNotFound):
		// No Keychain item; accept the plaintext file backend if claude wrote one.
		if _, ferr := keychain.ReadFileCredential(a.ConfigDir); ferr == nil {
			report(prefix+" credential", true, "file")
		} else {
			report(prefix+" credential", false, kerr.Error())
		}
	case fix:
		// Item exists but our ACL can't read it (-w denied): re-assert ownership.
		if _, rerr := keychain.Reassert(a.KeychainService, a.KeychainAccount); rerr == nil {
			report(prefix+" keychain", true, "re-asserted")
		} else {
			report(prefix+" keychain", false, rerr.Error())
		}
	default:
		report(prefix+" keychain", false, kerr.Error())
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

	// Private files stranded in a fuse backing dir by an interrupted
	// migration: a non-fuse account must hold its .claude.json (and friends)
	// in the account dir itself.
	if overlay.Kind(a.OverlayKind) != overlay.KindFuse {
		priv := overlay.FusePrivateRoot(a.ConfigDir)
		switch has, herr := overlay.HasPrivateEntries(priv); {
		case herr != nil:
			report(prefix+" private files", false, herr.Error())
		case has && fix:
			// Only heal when no daemon holds the pool: a CLI-side heal cannot
			// see the daemon's converting claim, and racing an in-flight
			// conversion would move files under its teardown sequence. With a
			// daemon up, the same recovery runs under the claim via
			// `ccp migrate` (or the daemon's own startup reconcile).
			if daemon.NewClient().Available() {
				report(prefix+" private files", false, "stranded in "+priv+"; the daemon is running — re-run `ccp migrate`, or stop the daemon and re-run doctor --fix")
			} else if healed, ferr := m.HealStrandedPrivate(a); ferr != nil {
				report(prefix+" private files", false, ferr.Error())
			} else if healed {
				report(prefix+" private files", true, "restored from "+priv)
			}
		case has:
			report(prefix+" private files", false, "stranded in "+priv+" by an interrupted migration")
		}
	}
}
