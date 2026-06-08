package pool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yasyf/cc-pool/internal/keychain"
	"github.com/yasyf/cc-pool/internal/overlay"
	"github.com/yasyf/cc-pool/internal/store"
)

// fakeCanonical is an in-memory CanonicalReader. reads counts ReadCanonical
// calls so tests can pin that the canonical secret is fetched exactly once,
// and only after consent (never during the candidate check).
type fakeCanonical struct {
	mu    sync.Mutex
	cred  *keychain.Credential // nil → no canonical item
	reads int
}

func (f *fakeCanonical) CanonicalExists() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cred != nil
}

func (f *fakeCanonical) ReadCanonical() (*keychain.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.cred == nil {
		return nil, keychain.ErrNotFound
	}
	cp := *f.cred
	return &cp, nil
}

// canonicalOAuthJSON is the compact fixture for ~/.claude.json's oauthAccount.
// It carries a field beyond the two we parse (organizationUuid) to pin that
// unknown fields survive the adopt write-back byte-for-byte.
const canonicalOAuthJSON = `{"accountUuid":"u-main","emailAddress":"me@example.com","organizationUuid":"org-1"}`

func writeHomeClaudeJSON(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(ClaudeJSONPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func canonicalCred(rt string) *keychain.Credential {
	cred := &keychain.Credential{}
	cred.ClaudeAiOauth.AccessToken = "at-canon"
	cred.ClaudeAiOauth.RefreshToken = rt
	cred.ClaudeAiOauth.ExpiresAt = 1700000000000
	cred.ClaudeAiOauth.SubscriptionType = "max"
	return cred
}

// addPooledAccount registers an account row whose private .claude.json holds
// the given oauthAccount JSON (empty string → no identity file).
func addPooledAccount(t *testing.T, st *store.Store, id int, oauthJSON string) {
	t.Helper()
	dir := t.TempDir()
	if oauthJSON != "" {
		body := `{"oauthAccount": ` + oauthJSON + `}`
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a := store.Account{
		ID:              id,
		ConfigDir:       dir,
		KeychainService: keychain.ServiceName(dir),
		KeychainAccount: "user",
		OverlayKind:     string(overlay.KindSymlink),
	}
	if err := st.UpsertAccount(a); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptCandidate(t *testing.T) {
	cases := map[string]struct {
		homeJSON  string // "" → no ~/.claude.json
		canonical *keychain.Credential
		pooled    string // oauthAccount JSON of one pooled account ("" → none, "-" → account without identity)
		want      bool
		wantErr   bool
	}{
		"no home claude.json": {
			canonical: canonicalCred("rt-0"),
		},
		"no oauthAccount in home claude.json": {
			homeJSON:  `{"hasCompletedOnboarding": true}`,
			canonical: canonicalCred("rt-0"),
		},
		"canonical keychain item absent": {
			homeJSON: `{"oauthAccount": ` + canonicalOAuthJSON + `}`,
		},
		"same identity already pooled": {
			homeJSON:  `{"oauthAccount": ` + canonicalOAuthJSON + `}`,
			canonical: canonicalCred("rt-0"),
			pooled:    canonicalOAuthJSON,
		},
		"different identity pooled": {
			homeJSON:  `{"oauthAccount": ` + canonicalOAuthJSON + `}`,
			canonical: canonicalCred("rt-0"),
			pooled:    `{"accountUuid":"u-other","emailAddress":"other@example.com"}`,
			want:      true,
		},
		"pooled account with unreadable identity still offers": {
			homeJSON:  `{"oauthAccount": ` + canonicalOAuthJSON + `}`,
			canonical: canonicalCred("rt-0"),
			pooled:    "-",
			want:      true,
		},
		"corrupt home claude.json fails loud": {
			homeJSON:  `{not json`,
			canonical: canonicalCred("rt-0"),
			wantErr:   true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if tc.homeJSON != "" {
				writeHomeClaudeJSON(t, tc.homeJSON)
			}
			st := openTestStore(t)
			switch tc.pooled {
			case "":
			case "-":
				addPooledAccount(t, st, 1, "")
			default:
				addPooledAccount(t, st, 1, tc.pooled)
			}
			fc := &fakeCanonical{cred: tc.canonical}
			m := &Manager{Store: st, Canonical: fc}

			cand, err := m.AdoptCandidate()
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("AdoptCandidate: %v", err)
			}
			if got := cand != nil; got != tc.want {
				t.Fatalf("candidate = %v, want %v", cand, tc.want)
			}
			if cand != nil {
				if cand.Identity.AccountUUID != "u-main" || cand.Identity.EmailAddress != "me@example.com" {
					t.Errorf("identity = %+v", cand.Identity)
				}
			}
			if fc.reads != 0 {
				t.Errorf("candidate check read the canonical secret %d time(s); it must be attribute-only", fc.reads)
			}
		})
	}
}

// adoptFixture assembles a Manager plus a synthesized PendingAdd ready for
// AdoptCredential, with plain claude's identity in ~/.claude.json and the
// account dir pre-seeded with seedJSON (when non-empty).
func adoptFixture(t *testing.T, fo *fakeOAuth, fc *fakeCanonical, seedJSON string) (*Manager, *fakeKeychain, *PendingAdd) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	writeHomeClaudeJSON(t, `{"oauthAccount": `+canonicalOAuthJSON+`}`)
	t.Setenv("USER", "tester")

	dir := filepath.Join(t.TempDir(), "acct-01")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if seedJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(seedJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	fk := newFakeKeychain()
	m := &Manager{Store: openTestStore(t), OAuth: fo, Keychain: fk, Canonical: fc}
	p := &PendingAdd{
		Index:           1,
		ConfigDir:       dir,
		KeychainService: keychain.ServiceName(dir),
		OverlayKind:     overlay.KindSymlink,
	}
	return m, fk, p
}

// consented is the candidate identity the adopt tests pretend the user
// confirmed (matching the adoptFixture's ~/.claude.json).
var consented = Identity{AccountUUID: "u-main", EmailAddress: "me@example.com"}

// TestAdoptCredentialCopiesAndRefreshes is the adoption safety pin: the
// canonical secret is read exactly once, every keychain mutation targets only
// the pending account's suffixed item (the canonical item is unreachable for
// writes by type — CanonicalReader has none), the copy is immediately
// refreshed onto its own chain, and the identity (with unknown fields) lands
// verbatim in the account's .claude.json.
func TestAdoptCredentialCopiesAndRefreshes(t *testing.T) {
	fo := &fakeOAuth{currentRT: "rt-0"}
	fc := &fakeCanonical{cred: canonicalCred("rt-0")}
	m, fk, p := adoptFixture(t, fo, fc, `{"hasCompletedOnboarding": true}`)

	if err := m.AdoptCredential(context.Background(), p, consented); err != nil {
		t.Fatalf("AdoptCredential: %v", err)
	}

	if fc.reads != 1 {
		t.Errorf("canonical secret read %d time(s), want exactly 1", fc.reads)
	}
	for i, s := range fk.touchedServices() {
		if s != p.KeychainService {
			t.Errorf("op %d named service %q, want only %q", i, s, p.KeychainService)
		}
	}
	if fo.refreshes != 1 {
		t.Errorf("refreshes = %d, want 1 (the copy must immediately fork onto its own chain)", fo.refreshes)
	}
	got, err := fk.Read(p.KeychainService, "tester")
	if err != nil {
		t.Fatalf("read adopted item: %v", err)
	}
	if got.ClaudeAiOauth.RefreshToken != fo.currentRT {
		t.Errorf("adopted RT = %q, want the rotated %q", got.ClaudeAiOauth.RefreshToken, fo.currentRT)
	}
	if got.ClaudeAiOauth.AccessToken != "at-1" {
		t.Errorf("adopted AT = %q, want at-1", got.ClaudeAiOauth.AccessToken)
	}
	if got.ClaudeAiOauth.SubscriptionType != "max" {
		t.Errorf("non-token fields not preserved through the refresh: %+v", got.ClaudeAiOauth)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(readFile(t, filepath.Join(p.ConfigDir, ".claude.json")), &top); err != nil {
		t.Fatal(err)
	}
	if string(top["oauthAccount"]) != canonicalOAuthJSON {
		t.Errorf("identity not written verbatim:\ngot  %s\nwant %s", top["oauthAccount"], canonicalOAuthJSON)
	}
	if string(top["hasCompletedOnboarding"]) != "true" {
		t.Errorf("seeded content lost: %v", top)
	}
}

// TestAdoptCredentialStaleCandidate pins that AdoptCredential adopts only the
// identity the user consented to, erroring BEFORE any secret read or keychain
// write when the current login changed (or vanished) while the confirm prompt
// sat on screen.
func TestAdoptCredentialStaleCandidate(t *testing.T) {
	t.Run("login switched to a different account", func(t *testing.T) {
		fo := &fakeOAuth{currentRT: "rt-0"}
		fc := &fakeCanonical{cred: canonicalCred("rt-0")}
		m, fk, p := adoptFixture(t, fo, fc, `{"hasCompletedOnboarding": true}`)
		// The user re-logged plain claude between the prompt and the consent.
		writeHomeClaudeJSON(t, `{"oauthAccount": {"accountUuid": "u-other", "emailAddress": "other@example.com"}}`)

		if err := m.AdoptCredential(context.Background(), p, consented); err == nil {
			t.Fatal("want error, got nil")
		}
		if fc.reads != 0 {
			t.Errorf("canonical secret read %d time(s) despite stale consent", fc.reads)
		}
		if got := fk.touchedServices(); len(got) != 0 {
			t.Errorf("keychain ops despite stale consent: %v", got)
		}
		if fo.refreshes != 0 {
			t.Errorf("refreshes = %d, want 0", fo.refreshes)
		}
	})

	t.Run("login vanished", func(t *testing.T) {
		fo := &fakeOAuth{currentRT: "rt-0"}
		fc := &fakeCanonical{cred: canonicalCred("rt-0")}
		m, fk, p := adoptFixture(t, fo, fc, `{"hasCompletedOnboarding": true}`)
		writeHomeClaudeJSON(t, `{"hasCompletedOnboarding": true}`) // identity gone

		if err := m.AdoptCredential(context.Background(), p, consented); err == nil {
			t.Fatal("want error, got nil")
		}
		if got := fk.touchedServices(); len(got) != 0 {
			t.Errorf("keychain ops despite vanished login: %v", got)
		}
	})

	t.Run("canonical item vanished", func(t *testing.T) {
		fo := &fakeOAuth{currentRT: "rt-0"}
		fc := &fakeCanonical{} // no credential
		m, fk, p := adoptFixture(t, fo, fc, `{"hasCompletedOnboarding": true}`)

		if err := m.AdoptCredential(context.Background(), p, consented); err == nil {
			t.Fatal("want error, got nil")
		}
		if got := fk.touchedServices(); len(got) != 0 {
			t.Errorf("keychain ops despite missing canonical credential: %v", got)
		}
	})
}

func TestAdoptCredentialIdentityWriteFailureCleansUp(t *testing.T) {
	fo := &fakeOAuth{currentRT: "rt-0"}
	fc := &fakeCanonical{cred: canonicalCred("rt-0")}
	// An unparseable account .claude.json makes the identity write-back fail
	// after the credential copy has landed.
	m, fk, p := adoptFixture(t, fo, fc, `{not json`)

	if err := m.AdoptCredential(context.Background(), p, consented); err == nil {
		t.Fatal("want error, got nil")
	}
	if fo.refreshes != 0 {
		t.Errorf("refreshes = %d, want 0 (identity failure precedes the refresh)", fo.refreshes)
	}
	if _, err := fk.Read(p.KeychainService, "tester"); err != keychain.ErrNotFound {
		t.Errorf("suffixed item not rolled back: %v", err)
	}
	if del := fk.deletedServices(); len(del) != 1 || del[0] != p.KeychainService {
		t.Errorf("deletes = %v, want exactly [%q]", del, p.KeychainService)
	}
}

func TestAdoptCredentialRefreshFailureCleansUp(t *testing.T) {
	// The provider no longer accepts the canonical RT (e.g. plain claude
	// rotated it between the copy and our refresh).
	fo := &fakeOAuth{currentRT: "rt-elsewhere"}
	fc := &fakeCanonical{cred: canonicalCred("rt-0")}
	m, fk, p := adoptFixture(t, fo, fc, `{"hasCompletedOnboarding": true}`)

	if err := m.AdoptCredential(context.Background(), p, consented); err == nil {
		t.Fatal("want error, got nil")
	}
	if fo.invalidGrants != 1 {
		t.Errorf("invalidGrants = %d, want 1", fo.invalidGrants)
	}
	if _, err := fk.Read(p.KeychainService, "tester"); err != keychain.ErrNotFound {
		t.Errorf("suffixed item not rolled back: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(readFile(t, filepath.Join(p.ConfigDir, ".claude.json")), &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["oauthAccount"]; ok {
		t.Error("identity not stripped back out; a retried add would hit SeedKeptExisting with no credential behind it")
	}
}
