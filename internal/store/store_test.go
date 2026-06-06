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
	a := Account{ID: 0, ConfigDir: "/home/.claude", KeychainService: "svc0", KeychainAccount: "me", IsZero: true, Label: "acct-00"}
	if err := s.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero || got.ConfigDir != a.ConfigDir {
		t.Fatalf("got %+v", got)
	}
	// Update label.
	a.Label = "renamed"
	if err := s.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAccount(0)
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
	s.UpsertAccount(Account{ID: 0, ConfigDir: "a", KeychainService: "s", KeychainAccount: "u", IsZero: true})
	s.UpsertAccount(Account{ID: 1, ConfigDir: "b", KeychainService: "s", KeychainAccount: "u"})
	if n, _ := s.NextAccountIndex(); n != 2 {
		t.Fatalf("next index = %d, want 2", n)
	}
	// Remove 1 -> reused.
	s.DeleteAccount(1)
	if n, _ := s.NextAccountIndex(); n != 1 {
		t.Fatalf("reused index = %d, want 1", n)
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
