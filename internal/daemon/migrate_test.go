package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/store"
)

// fakeFuseProv stands in for the fuse provider so handler-level conversion
// runs without a live mount. Mechanics (file moves, identity verification,
// rollback ordering) are pinned by internal/pool's convert tests; these tests
// pin the daemon's gating and wiring.
type fakeFuseProv struct {
	mu          sync.Mutex
	setups      int
	teardowns   int
	setupErr    error
	teardownErr error
}

func (f *fakeFuseProv) Kind() overlay.Kind            { return overlay.KindFuse }
func (f *fakeFuseProv) Sync(base, dir string) error   { return nil }
func (f *fakeFuseProv) Health(base, dir string) error { return nil }
func (f *fakeFuseProv) PrivateRoot(dir string) string { return overlay.FusePrivateRoot(dir) }

func (f *fakeFuseProv) Setup(base, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setups++
	return f.setupErr
}

func (f *fakeFuseProv) Teardown(base, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teardowns++
	return f.teardownErr
}

func (f *fakeFuseProv) setupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setups
}

// newMigrateServer wires newTestServer for migration: hermetic HOME, account
// dirs that exist on disk, the fake fuse provider behind the Manager seam, and
// an open fuse gate.
func newMigrateServer(t *testing.T) (*Server, map[int]string, *fakeFuseProv) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	s, dirs := newTestServer(t)
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeFuseProv{}
	s.m.OverlayFor = func(kind overlay.Kind) overlay.Provider {
		if kind == overlay.KindFuse {
			return fake
		}
		return &overlay.SymlinkProvider{}
	}
	s.fuseGateFn = func() string { return "" }
	return s, dirs, fake
}

func migrateReq(account *int, to string) Request {
	return Request{Op: OpMigrate, Account: account, To: to}
}

func outcomes(resp Response) map[int]MigrationOutcome {
	m := map[int]MigrationOutcome{}
	for _, r := range resp.Migrations {
		m[r.ID] = r.Outcome
	}
	return m
}

func kindOf(t *testing.T, s *Server, id int) string {
	t.Helper()
	a, err := s.m.Store.GetAccount(id)
	if err != nil {
		t.Fatal(err)
	}
	return a.OverlayKind
}

func TestHandleMigrateConvertsIdleAccounts(t *testing.T) {
	s, _, fake := newMigrateServer(t)

	resp := s.handleMigrate(t.Context(), migrateReq(nil, "fuse"))
	if !resp.OK {
		t.Fatalf("migrate failed: %s", resp.Error)
	}
	got := outcomes(resp)
	if got[1] != MigrationDone || got[2] != MigrationDone {
		t.Fatalf("outcomes = %v, want both done", got)
	}
	if kindOf(t, s, 1) != "fuse" || kindOf(t, s, 2) != "fuse" {
		t.Fatal("rows not flipped to fuse")
	}
	if fake.setupCount() != 2 {
		t.Fatalf("fuse setups = %d, want 2", fake.setupCount())
	}
	// The new-account default follows the migration.
	v, ok, err := s.m.Store.GetMeta("overlay_kind")
	if err != nil || !ok || v != "fuse" {
		t.Fatalf("meta overlay_kind = %q ok=%v err=%v, want fuse", v, ok, err)
	}

	// Re-running is free and truthful.
	resp = s.handleMigrate(t.Context(), migrateReq(nil, "fuse"))
	if !resp.OK {
		t.Fatalf("re-run failed: %s", resp.Error)
	}
	got = outcomes(resp)
	if got[1] != MigrationAlready || got[2] != MigrationAlready {
		t.Fatalf("re-run outcomes = %v, want both already", got)
	}
	if fake.setupCount() != 2 {
		t.Fatalf("re-run mounted again: setups = %d", fake.setupCount())
	}
}

func TestHandleMigrateReverse(t *testing.T) {
	s, dirs, fake := newMigrateServer(t)
	if resp := s.handleMigrate(t.Context(), migrateReq(nil, "fuse")); !resp.OK {
		t.Fatalf("forward migrate failed: %s", resp.Error)
	}

	resp := s.handleMigrate(t.Context(), migrateReq(nil, "symlink"))
	if !resp.OK {
		t.Fatalf("reverse migrate failed: %s", resp.Error)
	}
	got := outcomes(resp)
	if got[1] != MigrationDone || got[2] != MigrationDone {
		t.Fatalf("outcomes = %v, want both done", got)
	}
	if kindOf(t, s, 1) != "symlink" || kindOf(t, s, 2) != "symlink" {
		t.Fatal("rows not flipped back to symlink")
	}
	if fake.teardowns != 2 {
		t.Fatalf("fuse teardowns = %d, want 2", fake.teardowns)
	}
	if v, _, _ := s.m.Store.GetMeta("overlay_kind"); v != "symlink" {
		t.Fatalf("meta overlay_kind = %q, want symlink after retreat", v)
	}
	// The dirs are usable symlink accounts again.
	for _, dir := range dirs {
		if _, err := os.Readlink(filepath.Join(dir, "plans")); err != nil {
			t.Fatalf("symlink overlay not re-asserted in %s: %v", dir, err)
		}
	}
}

func TestHandleMigrateFuseGateBlocks(t *testing.T) {
	s, _, fake := newMigrateServer(t)
	s.fuseGateFn = func() string { return "grant Network Volumes access" }

	resp := s.handleMigrate(t.Context(), migrateReq(nil, "fuse"))
	if resp.OK || !strings.Contains(resp.Error, "grant Network Volumes access") {
		t.Fatalf("resp = %+v, want gate error", resp)
	}
	if len(resp.Migrations) != 0 || fake.setupCount() != 0 {
		t.Fatal("gate failure disturbed accounts")
	}
	if kindOf(t, s, 1) != "symlink" {
		t.Fatal("row changed despite gate failure")
	}
}

func TestHandleMigrateValidation(t *testing.T) {
	s, _, _ := newMigrateServer(t)

	if resp := s.handleMigrate(t.Context(), migrateReq(nil, "zfs")); resp.OK || !strings.Contains(resp.Error, "unknown overlay kind") {
		t.Fatalf("unknown kind: %+v", resp)
	}
	nine := 9
	if resp := s.handleMigrate(t.Context(), migrateReq(&nine, "fuse")); resp.OK || !strings.Contains(resp.Error, "account 9 not found") {
		t.Fatalf("unknown account: %+v", resp)
	}
}

func TestHandleMigrateSingleAccount(t *testing.T) {
	s, _, _ := newMigrateServer(t)
	two := 2
	resp := s.handleMigrate(t.Context(), migrateReq(&two, "fuse"))
	if !resp.OK || len(resp.Migrations) != 1 || resp.Migrations[0].ID != 2 || resp.Migrations[0].Outcome != MigrationDone {
		t.Fatalf("resp = %+v, want acct-2 done only", resp)
	}
	if kindOf(t, s, 1) != "symlink" || kindOf(t, s, 2) != "fuse" {
		t.Fatal("wrong rows flipped")
	}
}

func TestHandleMigrateBusyWhenReserved(t *testing.T) {
	s, _, fake := newMigrateServer(t)
	if !s.tryReserve(1) {
		t.Fatal("tryReserve failed on a free account")
	}

	resp := s.handleMigrate(t.Context(), migrateReq(nil, "fuse"))
	if !resp.OK {
		t.Fatalf("migrate failed: %s", resp.Error)
	}
	got := outcomes(resp)
	if got[1] != MigrationBusy || got[2] != MigrationDone {
		t.Fatalf("outcomes = %v, want acct-1 busy, acct-2 done", got)
	}
	if kindOf(t, s, 1) != "symlink" {
		t.Fatal("reserved account was converted")
	}
	if fake.setupCount() != 1 {
		t.Fatalf("setups = %d, want 1", fake.setupCount())
	}
	if s.isConverting(1) {
		t.Fatal("busy refusal leaked a converting claim")
	}

	// Reservation expired: the sweep picks up the straggler — the rollout's
	// re-run-as-sessions-free-up loop.
	s.mu.Lock()
	s.reservations[1] = time.Now().Add(-reservationTTL - time.Second)
	s.mu.Unlock()
	resp = s.handleMigrate(t.Context(), migrateReq(nil, "fuse"))
	got = outcomes(resp)
	if got[1] != MigrationDone || got[2] != MigrationAlready {
		t.Fatalf("sweep outcomes = %v, want acct-1 done, acct-2 already", got)
	}
}

func TestConvertClaimExcludesReservations(t *testing.T) {
	s, _, _ := newMigrateServer(t)

	if !s.beginConvert(1) {
		t.Fatal("beginConvert failed on a free account")
	}
	if s.tryReserve(1) {
		t.Fatal("tryReserve succeeded on a converting account")
	}
	if s.beginConvert(1) {
		t.Fatal("double beginConvert succeeded")
	}
	s.endConvert(1)
	if !s.tryReserve(1) {
		t.Fatal("tryReserve failed after endConvert")
	}
	if s.beginConvert(1) {
		t.Fatal("beginConvert succeeded over a live reservation")
	}
	s.mu.Lock()
	s.reservations[1] = time.Now().Add(-reservationTTL - time.Second)
	s.mu.Unlock()
	if !s.beginConvert(1) {
		t.Fatal("beginConvert failed over an expired reservation")
	}
}

func TestSelectSkipsConvertingAccount(t *testing.T) {
	s, _, _ := newMigrateServer(t)

	// acct-1 is the emptier (better) account; converting must hide it.
	if !s.beginConvert(1) {
		t.Fatal("beginConvert failed")
	}
	resp := s.handleSelect(t.Context(), Request{Op: OpSelect, NoMark: true})
	if !resp.OK || resp.SelectedID == nil || *resp.SelectedID != 2 {
		t.Fatalf("select = %+v, want acct-2 while acct-1 converts", resp)
	}

	one := 1
	resp = s.handleSelect(t.Context(), Request{Op: OpSelect, Account: &one})
	if resp.OK || !strings.Contains(resp.Error, "migrating") {
		t.Fatalf("forced select = %+v, want migrating refusal", resp)
	}

	s.endConvert(1)
	resp = s.handleSelect(t.Context(), Request{Op: OpSelect, NoMark: true})
	if !resp.OK || *resp.SelectedID != 1 {
		t.Fatalf("select after endConvert = %+v, want acct-1", resp)
	}
}

func TestSelectExcludesUnmountedFuseAccount(t *testing.T) {
	s, _, _ := newMigrateServer(t)
	a, err := s.m.Store.GetAccount(1)
	if err != nil {
		t.Fatal(err)
	}
	a.OverlayKind = "fuse" // row says fuse, but nothing is mounted at the dir
	if err := s.m.Store.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}

	resp := s.handleSelect(t.Context(), Request{Op: OpSelect, NoMark: true})
	if !resp.OK || resp.SelectedID == nil || *resp.SelectedID != 2 {
		t.Fatalf("select = %+v, want acct-2 while acct-1's mount is down", resp)
	}

	one := 1
	resp = s.handleSelect(t.Context(), Request{Op: OpSelect, Account: &one})
	if resp.OK || !strings.Contains(resp.Error, "mount is not up") {
		t.Fatalf("forced select = %+v, want mount-not-up refusal", resp)
	}
}

// TestFallbackToSymlinkRestoresPrivateFiles is the regression test for the
// stranding bug: the pre-fix fallback flipped the row to symlink but left the
// account's .claude.json identity behind in the fuse private backing dir.
func TestFallbackToSymlinkRestoresPrivateFiles(t *testing.T) {
	s, dirs, fake := newMigrateServer(t)
	a, err := s.m.Store.GetAccount(1)
	if err != nil {
		t.Fatal(err)
	}
	a.OverlayKind = "fuse"
	if err := s.m.Store.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	priv := overlay.FusePrivateRoot(dirs[1])
	if err := os.MkdirAll(priv, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(priv, ".claude.json"), []byte("identity"), 0o600); err != nil {
		t.Fatal(err)
	}

	s.fallbackToSymlink(a)

	got, err := os.ReadFile(filepath.Join(dirs[1], ".claude.json"))
	if err != nil || string(got) != "identity" {
		t.Fatalf("identity not restored to the account dir: %q err=%v", got, err)
	}
	if kindOf(t, s, 1) != "symlink" {
		t.Fatal("row not flipped to symlink")
	}
	if _, err := os.Lstat(priv); !os.IsNotExist(err) {
		t.Fatal("emptied private root not removed")
	}

	// Twin negative: a wedged unmount must abort the fallback untouched.
	a, _ = s.m.Store.GetAccount(2)
	a.OverlayKind = "fuse"
	if err := s.m.Store.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	priv2 := overlay.FusePrivateRoot(dirs[2])
	if err := os.MkdirAll(priv2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(priv2, ".claude.json"), []byte("identity2"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake.teardownErr = errors.New("still mounted")

	s.fallbackToSymlink(a)

	if _, err := os.Stat(filepath.Join(priv2, ".claude.json")); err != nil {
		t.Fatalf("identity moved despite failed unmount: %v", err)
	}
	if kindOf(t, s, 2) != "fuse" {
		t.Fatal("row flipped despite failed unmount")
	}
	if _, err := os.Lstat(filepath.Join(dirs[2], "plans")); !os.IsNotExist(err) {
		t.Fatal("symlinks laid despite failed unmount")
	}
}

func TestReconcileOverlaysHealsStrandedAndFallsBack(t *testing.T) {
	s, dirs, fake := newMigrateServer(t)

	// acct-1: symlink row with an identity stranded by an interrupted
	// conversion — startup must restore it.
	priv1 := overlay.FusePrivateRoot(dirs[1])
	if err := os.MkdirAll(priv1, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(priv1, ".claude.json"), []byte("stranded"), 0o600); err != nil {
		t.Fatal(err)
	}

	// acct-2: fuse row whose mount cannot come up — startup must fall back to
	// a fully usable symlink account.
	a2, err := s.m.Store.GetAccount(2)
	if err != nil {
		t.Fatal(err)
	}
	a2.OverlayKind = "fuse"
	if err := s.m.Store.UpsertAccount(a2); err != nil {
		t.Fatal(err)
	}
	priv2 := overlay.FusePrivateRoot(dirs[2])
	if err := os.MkdirAll(priv2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(priv2, ".claude.json"), []byte("identity2"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake.setupErr = errors.New("mount did not come up")

	s.reconcileOverlays(t.Context())

	if got, err := os.ReadFile(filepath.Join(dirs[1], ".claude.json")); err != nil || string(got) != "stranded" {
		t.Fatalf("acct-1 stranded identity not healed: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dirs[2], ".claude.json")); err != nil || string(got) != "identity2" {
		t.Fatalf("acct-2 identity not restored by fallback: %q err=%v", got, err)
	}
	if kindOf(t, s, 2) != "symlink" {
		t.Fatal("acct-2 row not flipped by fallback")
	}
}

// TestConvertClaimExcludesPolling pins the two-sided scheduler exclusion: a
// poll iteration owns the dir against conversions, and vice versa — the
// check-then-act hole a plain isConverting test left open.
func TestConvertClaimExcludesPolling(t *testing.T) {
	s, _, _ := newMigrateServer(t)

	if !s.beginPoll(1) {
		t.Fatal("beginPoll failed on a free account")
	}
	if s.beginConvert(1) {
		t.Fatal("beginConvert succeeded while the scheduler holds the account")
	}
	if s.beginPoll(1) {
		t.Fatal("double beginPoll succeeded")
	}
	s.endPoll(1)
	if !s.beginConvert(1) {
		t.Fatal("beginConvert failed after endPoll")
	}
	if s.beginPoll(1) {
		t.Fatal("beginPoll succeeded while a conversion holds the account")
	}
	s.endConvert(1)
	if !s.beginPoll(1) {
		t.Fatal("beginPoll failed after endConvert")
	}
	s.endPoll(1)

	// A poll claim must NOT hide the account from select — sessions can land
	// on a dir that is merely being health-checked.
	if !s.beginPoll(2) {
		t.Fatal("beginPoll failed")
	}
	if !s.tryReserve(2) {
		t.Fatal("tryReserve refused a merely-polling account")
	}
	s.endPoll(2)
}

// TestMountFuseSweepsUnderlay pins crash recovery for a conversion killed
// between its file moves and its row flip: private files left in the mount
// underlay must be swept into the backing dir BEFORE mounting, or the mirror
// would shadow the identity and a session would mint a divergent one.
func TestMountFuseSweepsUnderlay(t *testing.T) {
	s, dirs, fake := newMigrateServer(t)
	a, err := s.m.Store.GetAccount(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirs[1], ".claude.json"), []byte("underlay-identity"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.mountFuse(a); err != nil {
		t.Fatalf("mountFuse: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(overlay.FusePrivateRoot(dirs[1]), ".claude.json"))
	if err != nil || string(got) != "underlay-identity" {
		t.Fatalf("identity not swept into backing dir: %q err=%v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dirs[1], ".claude.json")); !os.IsNotExist(err) {
		t.Fatal("identity left in the underlay")
	}
	if fake.setupCount() != 1 {
		t.Fatalf("setups = %d, want 1", fake.setupCount())
	}

	// Unavailable provider must refuse loudly, never silently degrade.
	s.m.OverlayFor = func(kind overlay.Kind) overlay.Provider { return &overlay.SymlinkProvider{} }
	if err := s.mountFuse(a); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("mountFuse with no fuse provider = %v, want unavailable error", err)
	}
}

// TestMountReadySymmetric pins the kind-symmetric gate: a fuse row needs a
// mount, a non-fuse row needs the absence of one (a mountpoint under a symlink
// row is an aborted rollback's wreckage). /dev (devfs) stands in for a mount.
func TestMountReadySymmetric(t *testing.T) {
	s, dirs, _ := newMigrateServer(t)
	if !s.mountReady(store.Account{OverlayKind: "symlink", ConfigDir: dirs[1]}) {
		t.Fatal("unmounted symlink account not ready")
	}
	if s.mountReady(store.Account{OverlayKind: "fuse", ConfigDir: dirs[1]}) {
		t.Fatal("unmounted fuse account ready")
	}
	if s.mountReady(store.Account{OverlayKind: "symlink", ConfigDir: "/dev"}) {
		t.Fatal("mounted dir under a symlink row ready")
	}
	if !s.mountReady(store.Account{OverlayKind: "fuse", ConfigDir: "/dev"}) {
		t.Fatal("mounted fuse account not ready")
	}
}

// TestHandleMigrateBudgetExhausted pins the out-of-time path: a request whose
// budget is spent reports the remaining accounts busy instead of overrunning
// the conn deadline and leaving the client a dead socket.
func TestHandleMigrateBudgetExhausted(t *testing.T) {
	s, _, fake := newMigrateServer(t)
	s.migrateBudget = time.Nanosecond

	resp := s.handleMigrate(t.Context(), migrateReq(nil, "fuse"))
	if !resp.OK {
		t.Fatalf("migrate failed: %s", resp.Error)
	}
	for _, r := range resp.Migrations {
		if r.Outcome != MigrationBusy || !strings.Contains(r.Detail, "window elapsed") {
			t.Fatalf("result = %+v, want busy/window elapsed", r)
		}
	}
	if fake.setupCount() != 0 {
		t.Fatal("conversion ran despite an exhausted budget")
	}
	if kindOf(t, s, 1) != "symlink" {
		t.Fatal("row flipped despite an exhausted budget")
	}
}

// TestConvertAccountRefetchesRow pins the stale-snapshot fix: the row is
// re-read under the claim, so a kind that changed since the caller's list is
// honored instead of double-converting (or converting from a wrong source).
func TestConvertAccountRefetchesRow(t *testing.T) {
	s, _, fake := newMigrateServer(t)
	stale, err := s.m.Store.GetAccount(1)
	if err != nil {
		t.Fatal(err)
	}
	fresh := stale
	fresh.OverlayKind = "fuse" // flipped after the caller's snapshot
	if err := s.m.Store.UpsertAccount(fresh); err != nil {
		t.Fatal(err)
	}

	res := s.convertAccount(stale, overlay.KindFuse) // stale still says symlink
	if res.Outcome != MigrationAlready {
		t.Fatalf("outcome = %s (%s), want already", res.Outcome, res.Detail)
	}
	if fake.setupCount() != 0 {
		t.Fatal("conversion ran against a stale row")
	}
}

// TestPollOnceSkipsConvertingAccount mirrors the reserved-account refresh test:
// an account mid-conversion must see no overlay sync, no refresh, no adoption.
func TestPollOnceSkipsConvertingAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := newTestServer(t)
	fo := s.m.OAuth.(*fakeOAuth)
	fo.mu.Lock()
	fo.currentRT = "rt-0"
	fo.mu.Unlock()
	fk := s.m.Keychain.(*fakeKeychain)
	a, err := s.m.Store.GetAccount(1)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's accounts share a keychain service; give acct-1 its own so
	// acct-2's (un-converting, untouched) poll can't read this credential.
	a.KeychainService = "svc-acct-1"
	if err := s.m.Store.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	cred := &keychain.Credential{}
	cred.ClaudeAiOauth.AccessToken = "at-0"
	cred.ClaudeAiOauth.RefreshToken = "rt-0"
	// Near-expiry (< RefreshLeadTime) so an idle poll must refresh.
	cred.ClaudeAiOauth.ExpiresAt = time.Now().Add(time.Minute).UnixMilli()
	if err := fk.Write(a.KeychainService, a.KeychainAccount, cred); err != nil {
		t.Fatal(err)
	}
	seedWrites := fk.writeCount()

	if !s.beginConvert(a.ID) {
		t.Fatal("beginConvert failed")
	}
	s.pollOnce(t.Context())
	if got := fo.refreshCount(); got != 0 {
		t.Fatalf("converting account was refreshed %d time(s)", got)
	}
	if got := fk.writeCount(); got != seedWrites {
		t.Fatalf("converting account's credential was written %d time(s)", got-seedWrites)
	}

	s.endConvert(a.ID)
	s.pollOnce(t.Context())
	if got := fo.refreshCount(); got != 1 {
		t.Fatalf("idle near-expiry account refreshed %d time(s), want 1", got)
	}
}
