package pool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/store"
)

// TestPoolNeverTouchesDefaultKeychainItem pins the #1 safety invariant: no
// CredentialStore op — read, write, or delete — ever names the canonical
// unsuffixed item plain `claude` owns ("Claude Code-credentials"). The sole
// sanctioned canonical access is the read-only CanonicalReader seam used by
// adoption (pinned separately in adopt_test.go). Every credential op,
// including a full AdoptCredential, is driven through a fake that logs the
// services named; the canonical name must never appear, and every op must use
// exactly the involved account's own suffixed service.
func TestPoolNeverTouchesDefaultKeychainItem(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "user")
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

	// Drive a full adoption of plain claude's login into a second account. The
	// canonical credential arrives through the read-only CanonicalReader seam;
	// every CredentialStore op it triggers must still name only the pending
	// account's own suffixed service.
	if err := os.WriteFile(ClaudeJSONPath(),
		[]byte(`{"oauthAccount": {"accountUuid": "u-main", "emailAddress": "me@example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	canonical := &keychain.Credential{}
	canonical.ClaudeAiOauth.AccessToken = "at-canon"
	canonical.ClaudeAiOauth.RefreshToken = fo.currentRT // accepted by the fake provider
	m.Canonical = &fakeCanonical{cred: canonical}
	dir2 := filepath.Join(t.TempDir(), "acct-02")
	if err := os.MkdirAll(dir2, 0o700); err != nil {
		t.Fatal(err)
	}
	svc2 := keychain.ServiceName(dir2)
	p := &PendingAdd{Index: 2, ConfigDir: dir2, KeychainService: svc2, OverlayKind: "symlink"}
	want := Identity{AccountUUID: "u-main", EmailAddress: "me@example.com"}
	if err := m.AdoptCredential(context.Background(), p, want); err != nil {
		t.Fatalf("AdoptCredential: %v", err)
	}

	touched := fk.touchedServices()
	if len(touched) == 0 {
		t.Fatal("no keychain ops recorded; the test exercised nothing")
	}
	for i, s := range touched {
		if s == "Claude Code-credentials" {
			t.Fatalf("op %d named the canonical unsuffixed item", i)
		}
		if s != svc && s != svc2 {
			t.Errorf("op %d named service %q, want %q or %q", i, s, svc, svc2)
		}
	}
	if del := fk.deletedServices(); len(del) != 1 || del[0] != svc {
		t.Errorf("deletes = %v, want exactly [%q]", del, svc)
	}
}
