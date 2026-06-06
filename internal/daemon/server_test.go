package daemon

import (
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-pool/internal/pool"
	"github.com/yasyf/cc-pool/internal/store"
)

// newTestServer builds a Server over a temp-dir store with two accounts:
// acct-1 emptier (util 10) than acct-2 (util 50), both freshly sampled. The
// temp DefaultDir and config dirs guarantee procscan can never attribute a
// real claude process to them, and the nonexistent keychain services make any
// best-effort preflight refresh a harmless miss.
func newTestServer(t *testing.T) (*Server, map[int]string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	dirs := map[int]string{}
	now := time.Now()
	for id, util := range map[int]float64{1: 10, 2: 50} {
		dir := filepath.Join(t.TempDir(), "acct")
		dirs[id] = dir
		if err := st.UpsertAccount(store.Account{
			ID: id, ConfigDir: dir,
			KeychainService: "clp-test-missing", KeychainAccount: "clp-test",
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertUsageSample(store.UsageSample{AccountID: id, TS: now, Util5h: util, Util7d: util}); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{
		m:            &pool.Manager{Store: st, DefaultDir: filepath.Join(t.TempDir(), "claude")},
		log:          log.New(io.Discard, "", 0),
		reservations: map[int]time.Time{},
		rlStreak:     map[int]int{},
	}, dirs
}

func TestHandleSelectRecordsSticky(t *testing.T) {
	s, dirs := newTestServer(t)
	resp := s.handleSelect(Request{Op: OpSelect, NoMark: true, Cwd: "/proj"})
	if !resp.OK || resp.Dir != dirs[1] {
		t.Fatalf("expected emptier acct-1 (%s), got %+v", dirs[1], resp)
	}
	if resp.Sticky {
		t.Fatal("first select must not report sticky")
	}
	st, ok, err := s.m.Store.GetSticky("/proj")
	if err != nil || !ok || st.AccountID != 1 {
		t.Fatalf("winner not recorded: %+v ok=%v err=%v", st, ok, err)
	}
}

func TestHandleSelectHonorsSticky(t *testing.T) {
	s, dirs := newTestServer(t)
	// Sticky points at the WORSE account; it must still win.
	if err := s.m.Store.UpsertSticky("/proj", 2, time.Now()); err != nil {
		t.Fatal(err)
	}
	resp := s.handleSelect(Request{Op: OpSelect, NoMark: true, Cwd: "/proj"})
	if !resp.OK || resp.Dir != dirs[2] || !resp.Sticky {
		t.Fatalf("expected sticky acct-2 (%s), got %+v", dirs[2], resp)
	}
}

func TestHandleSelectForcedRecordsSticky(t *testing.T) {
	s, dirs := newTestServer(t)
	acct := 2
	resp := s.handleSelect(Request{Op: OpSelect, Account: &acct, Cwd: "/proj"})
	if !resp.OK || resp.Dir != dirs[2] {
		t.Fatalf("expected forced acct-2 (%s), got %+v", dirs[2], resp)
	}
	if resp.Sticky {
		t.Fatal("forced select must not report sticky (ranking was not overridden)")
	}
	st, ok, err := s.m.Store.GetSticky("/proj")
	if err != nil || !ok || st.AccountID != 2 {
		t.Fatalf("forced account not recorded: %+v ok=%v err=%v", st, ok, err)
	}
}
