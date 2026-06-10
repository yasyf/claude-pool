package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAccountCRUD(t *testing.T) {
	s := openTest(t)
	a := Account{ID: 1, ConfigDir: "/home/.cc-pool/accounts/acct-01", KeychainService: "svc1", KeychainAccount: "me", Label: "work"}
	if err := s.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(1)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigDir != a.ConfigDir || got.Label != "work" {
		t.Fatalf("got %+v", got)
	}
	// Update label.
	a.Label = "renamed"
	if err := s.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAccount(1)
	if got.Label != "renamed" {
		t.Fatalf("label not updated: %q", got.Label)
	}
	all, _ := s.ListAccounts()
	if len(all) != 1 {
		t.Fatalf("len = %d", len(all))
	}
}

func TestNextAccountIndex(t *testing.T) {
	s := openTest(t)
	if n, _ := s.NextAccountIndex(); n != 1 {
		t.Fatalf("first index = %d, want 1", n)
	}
	s.UpsertAccount(Account{ID: 1, ConfigDir: "a", KeychainService: "s", KeychainAccount: "u"})
	s.UpsertAccount(Account{ID: 2, ConfigDir: "b", KeychainService: "s", KeychainAccount: "u"})
	if n, _ := s.NextAccountIndex(); n != 3 {
		t.Fatalf("next index = %d, want 3", n)
	}
	// Remove 1 -> reused.
	s.DeleteAccount(1)
	if n, _ := s.NextAccountIndex(); n != 1 {
		t.Fatalf("reused index = %d, want 1", n)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	s := openTest(t)
	if _, ok, err := s.GetMeta("initialized"); ok || err != nil {
		t.Fatalf("absent key: ok=%v err=%v", ok, err)
	}
	if err := s.SetMeta("initialized", "1"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.GetMeta("initialized")
	if err != nil || !ok || v != "1" {
		t.Fatalf("get after set: v=%q ok=%v err=%v", v, ok, err)
	}
	// Upsert overwrites.
	if err := s.SetMeta("initialized", "2"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.GetMeta("initialized"); v != "2" {
		t.Fatalf("overwrite failed: %q", v)
	}
}

func TestUsageSampleLatest(t *testing.T) {
	s := openTest(t)
	s.UpsertAccount(Account{ID: 1, ConfigDir: "b", KeychainService: "s", KeychainAccount: "u"})
	old := UsageSample{AccountID: 1, TS: time.Now().Add(-time.Minute), Util5h: 10}
	cur := UsageSample{AccountID: 1, TS: time.Now(), Util5h: 50, Resets5h: time.Now().Add(time.Hour), RateLimited: true}
	if err := s.InsertUsageSample(old); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertUsageSample(cur); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LatestUsageSample(1)
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if got.Util5h != 50 || !got.RateLimited || got.Resets5h.IsZero() {
		t.Fatalf("latest sample wrong: %+v", got)
	}
}

func TestUsageSampleExtraUsageRoundTrip(t *testing.T) {
	s := openTest(t)
	s.UpsertAccount(Account{ID: 1, ConfigDir: "b", KeychainService: "s", KeychainAccount: "u"})
	in := UsageSample{AccountID: 1, TS: time.Now(), Util5h: 100, ExtraEnabled: true, ExtraUsed: 177, ExtraLimit: 5000}
	if err := s.InsertUsageSample(in); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LatestUsageSample(1)
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if !got.ExtraEnabled || got.ExtraUsed != 177 || got.ExtraLimit != 5000 {
		t.Fatalf("extra usage did not round-trip: %+v", got)
	}
	recent, err := s.RecentUsageSamples(1, 1)
	if err != nil || len(recent) != 1 || !recent[0].ExtraEnabled {
		t.Fatalf("recent samples missing extra usage: %+v err=%v", recent, err)
	}
}


func TestSessionsReconcile(t *testing.T) {
	s := openTest(t)
	s.UpsertAccount(Account{ID: 1, ConfigDir: "b", KeychainService: "s", KeychainAccount: "u"})
	id1, _ := s.OpenSession(1, 111, "b")
	s.OpenSession(1, 222, "b")
	if n, _ := s.ActiveSessionCount(1); n != 2 {
		t.Fatalf("active = %d, want 2", n)
	}
	// Only 222 is alive -> 111 closed.
	closed, err := s.CloseDeadSessions(map[int]bool{222: true})
	if err != nil || closed != 1 {
		t.Fatalf("closed = %d err=%v", closed, err)
	}
	if n, _ := s.ActiveSessionCount(1); n != 1 {
		t.Fatalf("active after reconcile = %d, want 1", n)
	}
	if err := s.CloseSession(id1); err != nil {
		t.Fatal(err)
	}
}

func TestSticky(t *testing.T) {
	s := openTest(t)
	s.UpsertAccount(Account{ID: 1, ConfigDir: "a", KeychainService: "s", KeychainAccount: "u"})
	s.UpsertAccount(Account{ID: 2, ConfigDir: "b", KeychainService: "s", KeychainAccount: "u"})

	if _, ok, err := s.GetSticky("/proj"); ok || err != nil {
		t.Fatalf("empty table: ok=%v err=%v", ok, err)
	}

	t0 := time.Now().Truncate(time.Second)
	if err := s.UpsertSticky("/proj", 1, t0); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetSticky("/proj")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Cwd != "/proj" || got.AccountID != 1 || !got.SelectedAt.Equal(t0) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Re-upsert (the sliding-TTL write path) overwrites both fields.
	t1 := t0.Add(time.Minute)
	if err := s.UpsertSticky("/proj", 2, t1); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetSticky("/proj")
	if got.AccountID != 2 || !got.SelectedAt.Equal(t1) {
		t.Fatalf("upsert did not overwrite: %+v", got)
	}
}

func TestPruneSticky(t *testing.T) {
	s := openTest(t)
	now := time.Now().Truncate(time.Second)
	s.UpsertSticky("/old", 1, now.Add(-2*time.Hour))
	s.UpsertSticky("/fresh", 1, now)
	n, err := s.PruneSticky(now.Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune: n=%d err=%v", n, err)
	}
	if _, ok, _ := s.GetSticky("/old"); ok {
		t.Fatal("old row should be pruned")
	}
	if _, ok, _ := s.GetSticky("/fresh"); !ok {
		t.Fatal("fresh row should survive")
	}
}

func TestDeleteAccountRemovesSticky(t *testing.T) {
	s := openTest(t)
	s.UpsertAccount(Account{ID: 1, ConfigDir: "a", KeychainService: "s", KeychainAccount: "u"})
	s.UpsertSticky("/proj", 1, time.Now())
	if err := s.DeleteAccount(1); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetSticky("/proj"); ok {
		t.Fatal("sticky row should be deleted with its account")
	}
}

func TestRefreshLog(t *testing.T) {
	s := openTest(t)
	s.UpsertAccount(Account{ID: 1, ConfigDir: "b", KeychainService: "s", KeychainAccount: "u"})
	if _, ok, _ := s.LastRefresh(1); ok {
		t.Fatal("expected no refresh yet")
	}
	s.LogRefresh(1, false, "boom")
	e, ok, err := s.LastRefresh(1)
	if err != nil || !ok || e.OK || e.Err != "boom" {
		t.Fatalf("last refresh = %+v ok=%v err=%v", e, ok, err)
	}
}
