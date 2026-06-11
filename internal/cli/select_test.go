package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-pool/internal/daemon"
	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/store"
)

func selectTestManager(t *testing.T) *pool.Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertAccount(store.Account{
		ID: 5, ConfigDir: filepath.Join(t.TempDir(), "acct"), Label: "work@example.com",
		KeychainService: "ccp-test-missing", KeychainAccount: "ccp-test",
	}); err != nil {
		t.Fatal(err)
	}
	return &pool.Manager{Store: st}
}

// TestSelectionLine pins the wording of the shared selection diagnostic and its
// degraded fallbacks. The daemon's SelectedID resolves to the account label; a
// sticky pick is labelled "Reusing … (pinned)"; an unknown/absent id degrades to
// a generic "account"; and a sampled pick (HasUsage) gets its raw 5h/7d usage
// appended (100−remaining), while an unsampled one stays usage-free (no
// fabricated 0%). ANSI is stripped so the assertions hold regardless of TTY.
func TestSelectionLine(t *testing.T) {
	m := selectTestManager(t)
	id := 5
	missing := 999

	cases := map[string]struct {
		resp daemon.Response
		want string
	}{
		"named, no usage":         {daemon.Response{SelectedID: &id}, "Selected work@example.com"},
		"named sticky, no usage":  {daemon.Response{SelectedID: &id, Sticky: true}, "Reusing work@example.com (pinned)"},
		"nil id degrades":         {daemon.Response{}, "Selected account"},
		"unknown id degrades":     {daemon.Response{SelectedID: &missing}, "Selected account"},
		"named with usage":        {daemon.Response{SelectedID: &id, HasUsage: true, Remaining5h: 96, Remaining7d: 27}, "Selected work@example.com · 5h 4% used · 7d 73% used"},
		"named sticky with usage": {daemon.Response{SelectedID: &id, Sticky: true, HasUsage: true, Remaining5h: 96, Remaining7d: 27}, "Reusing work@example.com (pinned) · 5h 4% used · 7d 73% used"},
	}
	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			if got := stripANSI(daemonSelectionLine(m, &tc.resp)); got != tc.want {
				t.Errorf("daemonSelectionLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDaemonSelectOutcome pins the daemon-reply dispatch, in particular that a
// none-available reply with --wait reaches the wait loop: the daemon sets BOTH
// NoneAvailable and Error, and the old Error-first match made --wait dead code.
func TestDaemonSelectOutcome(t *testing.T) {
	cases := map[string]struct {
		resp daemon.Response
		wait bool
		want selectOutcome
	}{
		"picked":                        {daemon.Response{OK: true, Dir: "/d"}, false, outcomePicked},
		"picked ignores wait":           {daemon.Response{OK: true, Dir: "/d"}, true, outcomePicked},
		"fallback pick accepted":        {daemon.Response{OK: true, Dir: "/d", ExhaustedFallback: true}, false, outcomePicked},
		"fallback pick waits with wait": {daemon.Response{OK: true, Dir: "/d", ExhaustedFallback: true}, true, outcomeWait},
		"none available, no wait":       {daemon.Response{NoneAvailable: true, Error: "no account is currently available"}, false, outcomeFail},
		"none available, wait":          {daemon.Response{NoneAvailable: true, Error: "no account is currently available"}, true, outcomeWait},
		"real error":                    {daemon.Response{Error: "boom"}, false, outcomeError},
		"real error not masked by wait": {daemon.Response{Error: "boom"}, true, outcomeError},
		"empty reply, wait":             {daemon.Response{}, true, outcomeWait},
		"empty reply, no wait":          {daemon.Response{}, false, outcomeFail},
	}
	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			if got := daemonSelectOutcome(&tc.resp, tc.wait); got != tc.want {
				t.Errorf("daemonSelectOutcome = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWarnExhaustedFallback pins the billing warning wording: credits when
// overage is enabled, rate-limit otherwise, reset time when known.
func TestWarnExhaustedFallback(t *testing.T) {
	run := func(extraEnabled bool) string {
		var stderr bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetErr(&stderr)
		warnExhaustedFallback(cmd, "work@example.com", extraEnabled, time.Now().Add(20*time.Minute))
		return stripANSI(stderr.String())
	}
	if got := run(true); !strings.Contains(got, "WILL bill extra-usage credits") || !strings.Contains(got, "resets at") {
		t.Errorf("overage warning wrong: %q", got)
	}
	if got := run(false); !strings.Contains(got, "rate-limited until") {
		t.Errorf("rate-limit warning wrong: %q", got)
	}
	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)
	warnExhaustedFallback(cmd, "x", true, time.Time{})
	if got := stripANSI(stderr.String()); strings.Contains(got, "resets at") {
		t.Errorf("unknown reset must omit the reset clause: %q", got)
	}
}

// exhaustedPoolManager builds a Manager over a temp store whose two accounts
// are both 5h-pegged with pending resets and fresh samples — the all-exhausted
// state — with acct-2 the least-bad (emptier 7d, overage enabled). The fake
// keychain is empty, so any preflight refresh is a harmless needs-login miss.
func exhaustedPoolManager(t *testing.T) *pool.Manager {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // SyncOverlay resolves ~/.claude from HOME
	t.Setenv("USER", "user")
	st := openTestStore(t)
	now := time.Now()
	for id, util7 := range map[int]float64{1: 90, 2: 10} {
		if err := st.UpsertAccount(store.Account{
			ID: id, ConfigDir: filepath.Join(t.TempDir(), "acct"), Label: "work@example.com",
			KeychainService: "ccp-test-missing", KeychainAccount: "ccp-test", OverlayKind: "symlink",
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertUsageSample(store.UsageSample{
			AccountID: id, TS: now, Util5h: 100, Util7d: util7,
			Resets5h: now.Add(20 * time.Minute), ExtraEnabled: id == 2,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return &pool.Manager{Store: st, Keychain: emptyKeychain{}, LockDir: t.TempDir()}
}

// emptyKeychain satisfies pool.CredentialStore with no items, turning the
// preflight refresh into the needs-login warning path.
type emptyKeychain struct{}

func (emptyKeychain) Read(string, string) (*keychain.Credential, error) {
	return nil, keychain.ErrNotFound
}
func (emptyKeychain) Write(string, string, *keychain.Credential) error { return nil }
func (emptyKeychain) Delete(string, string) error                      { return nil }
func (emptyKeychain) Discover(string) (string, error)                  { return "", keychain.ErrNotFound }

// TestResolveSelectionWarnsOnExhaustedFallback pins the warning at the
// integration point: a live all-exhausted selection must emit the billing
// warning on stderr and still hand back the least-bad dir. Deleting the
// warnExhaustedFallback call site fails this test.
func TestResolveSelectionWarnsOnExhaustedFallback(t *testing.T) {
	m := exhaustedPoolManager(t)
	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())

	dir, _, err := resolveSelection(cmd, m, selectReq{noDaemon: true, cwd: "/proj"})
	if err != nil || dir == "" {
		t.Fatalf("fallback selection must succeed: dir=%q err=%v", dir, err)
	}
	out := stripANSI(stderr.String())
	if !strings.Contains(out, "WILL bill extra-usage credits") {
		t.Fatalf("billing warning missing from stderr: %q", out)
	}
	if !strings.Contains(out, "resets at") {
		t.Fatalf("warning must name the recovery time: %q", out)
	}
}

// TestWarnPinHeld pins the bypass notice's gating: silent without a held pin
// and when the pick IS the held account (the pin was honored in effect), loud
// — with the pin-kept reassurance — when an explicit pin was bypassed.
func TestWarnPinHeld(t *testing.T) {
	m := exhaustedPoolManager(t) // any manager with account rows for names
	held, selected := 2, 1
	cases := map[string]struct {
		held, selected *int
		want           string // "" = stderr must stay clean
	}{
		"no held pin":          {nil, &selected, ""},
		"pick is the held pin": {&held, &held, ""},
		"bypassed":             {&held, &selected, "manual pin to"},
		"bypassed, nil pick":   {&held, nil, "manual pin to"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetErr(&stderr)
			warnPinHeld(cmd, m, tc.held, tc.selected)
			out := stripANSI(stderr.String())
			if tc.want == "" {
				if out != "" {
					t.Fatalf("expected silence, got %q", out)
				}
				return
			}
			if !strings.Contains(out, tc.want) || !strings.Contains(out, "pin kept") {
				t.Fatalf("notice malformed: %q", out)
			}
		})
	}
}

// TestResolveSelectionWarnsOnHeldManualPin pins the notice at the integration
// point: a live selection over a dir manually pinned to an exhausted account
// must pick the healthy one AND say the pin was bypassed. Deleting the
// warnPinHeld call site fails this test.
func TestResolveSelectionWarnsOnHeldManualPin(t *testing.T) {
	m := exhaustedPoolManager(t)
	now := time.Now()
	// Heal acct-1 so the pool is no longer all-exhausted; pin to acct-2, which
	// stays pegged (unusable for stickiness).
	if err := m.Store.InsertUsageSample(store.UsageSample{
		AccountID: 1, TS: now.Add(time.Second), Util5h: 10, Util7d: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.PinManual("/proj", 2, now); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	dir, _, err := resolveSelection(cmd, m, selectReq{noDaemon: true, cwd: "/proj"})
	if err != nil || dir == "" {
		t.Fatalf("selection must succeed: dir=%q err=%v", dir, err)
	}
	out := stripANSI(stderr.String())
	if !strings.Contains(out, "manual pin to") || !strings.Contains(out, "pin kept") {
		t.Fatalf("bypass notice missing from stderr: %q", out)
	}
	// The pin must survive the bypass untouched.
	st, ok, _ := m.Store.GetSticky("/proj")
	if !ok || st.AccountID != 2 || !st.Manual {
		t.Fatalf("manual pin lost on bypass: %+v ok=%v", st, ok)
	}

	// Negative: when the fallback pick IS the held account (all exhausted,
	// pin on the least-bad), the pin was honored in effect — no notice.
	m2 := exhaustedPoolManager(t)
	if err := m2.Store.PinManual("/proj", 2, now); err != nil {
		t.Fatal(err)
	}
	var stderr2 bytes.Buffer
	cmd2 := &cobra.Command{}
	cmd2.SetErr(&stderr2)
	cmd2.SetContext(context.Background())
	if _, _, err := resolveSelection(cmd2, m2, selectReq{noDaemon: true, cwd: "/proj"}); err != nil {
		t.Fatal(err)
	}
	if out := stripANSI(stderr2.String()); strings.Contains(out, "manual pin to") {
		t.Fatalf("honored-in-effect pin must not warn: %q", out)
	}
}

// TestResolveSelectionWaitRefusesExhaustedFallback pins --wait's contract:
// over an all-exhausted pool it must wait (here: until the context cancels),
// never hand back the exhausted pick that a non-wait call accepts.
func TestResolveSelectionWaitRefusesExhaustedFallback(t *testing.T) {
	m := exhaustedPoolManager(t)
	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: the wait loop must exit on its first sleep
	cmd.SetContext(ctx)

	_, _, err := resolveSelection(cmd, m, selectReq{noDaemon: true, wait: true, cwd: "/proj"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait must block until cancelled, got %v", err)
	}
	out := stripANSI(stderr.String())
	if !strings.Contains(out, "soonest reset at") {
		t.Fatalf("wait message missing from stderr: %q", out)
	}
	if strings.Contains(out, "WILL bill") {
		t.Fatalf("--wait must never accept (or warn about) a billing fallback: %q", out)
	}
}

// TestAnnounceLineSilentWhenNotTTY is the core of the noise fix: when stdout is
// not an interactive terminal (as in tests, and under $(ccp select)), the success
// line is suppressed entirely — only the dir reaches stdout elsewhere.
func TestAnnounceLineSilentWhenNotTTY(t *testing.T) {
	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)

	announceLine(cmd, "Selected work@example.com")

	if stderr.Len() != 0 {
		t.Errorf("expected no stderr output when stdout is not a TTY, got %q", stderr.String())
	}
}
