package pool

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/store"
)

// TestPoolNeverTouchesDefaultKeychainItem pins the #1 safety invariant: the
// pool must never read, write, or delete the canonical unsuffixed item plain
// `claude` owns ("Claude Code-credentials"). Every credential op is driven
// through a fake that logs the services named; the canonical name must never
// appear, and every op must use exactly the account's own suffixed service.
func TestPoolNeverTouchesDefaultKeychainItem(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	svc := keychain.ServiceName("/tmp/clp-test/acct-01")
	a := store.Account{ID: 1, ConfigDir: t.TempDir(), KeychainService: svc, KeychainAccount: "user", OverlayKind: "symlink"}
	if err := st.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}

	fk := newFakeKeychain()
	cred := &keychain.Credential{}
	cred.ClaudeAiOauth.AccessToken = "at-0"
	cred.ClaudeAiOauth.RefreshToken = "rt-0"
	// Near-expiry so SampleUsage's pre-flight must POST-refresh.
	cred.ClaudeAiOauth.ExpiresAt = time.Now().Add(time.Minute).UnixMilli()
	if err := fk.Write(svc, a.KeychainAccount, cred); err != nil {
		t.Fatal(err)
	}
	fo := &fakeOAuth{currentRT: "rt-0"}
	m := &Manager{Store: st, OAuth: fo, Keychain: fk}

	if _, _, err := m.SampleUsage(context.Background(), a, true); err != nil {
		t.Fatalf("SampleUsage: %v", err)
	}
	if got := fo.refreshes; got != 1 {
		t.Fatalf("refreshes = %d, want 1 (near-expiry token must be refreshed)", got)
	}
	if err := m.AdoptRotatedToken(a); err != nil {
		t.Fatalf("AdoptRotatedToken: %v", err)
	}
	if err := m.Remove(a.ID, true); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	touched := fk.touchedServices()
	if len(touched) == 0 {
		t.Fatal("no keychain ops recorded; the test exercised nothing")
	}
	for i, s := range touched {
		if s == "Claude Code-credentials" {
			t.Fatalf("op %d named the canonical unsuffixed item", i)
		}
		if s != svc {
			t.Errorf("op %d named service %q, want %q", i, s, svc)
		}
	}
	if del := fk.deletedServices(); len(del) != 1 || del[0] != svc {
		t.Errorf("deletes = %v, want exactly [%q]", del, svc)
	}
}
