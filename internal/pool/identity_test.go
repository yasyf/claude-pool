package pool

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-pool/internal/overlay"
)

func TestReadIdentity(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), ".claude.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("happy path preserves unknown fields in Raw", func(t *testing.T) {
		raw := `{"accountUuid":"u-1","emailAddress":"me@example.com","organizationUuid":"org-1"}`
		id, err := readIdentity(write(t, `{"oauthAccount": `+raw+`, "numStartups": 3}`))
		if err != nil {
			t.Fatal(err)
		}
		if id.AccountUUID != "u-1" || id.EmailAddress != "me@example.com" {
			t.Errorf("parsed fields = %+v", id)
		}
		if string(id.Raw) != raw {
			t.Errorf("Raw not verbatim:\ngot  %s\nwant %s", id.Raw, raw)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := readIdentity(filepath.Join(t.TempDir(), "nope.json"))
		if !errors.Is(err, ErrNoIdentity) {
			t.Fatalf("err = %v, want ErrNoIdentity", err)
		}
	})

	t.Run("missing oauthAccount key", func(t *testing.T) {
		_, err := readIdentity(write(t, `{"hasCompletedOnboarding": true}`))
		if !errors.Is(err, ErrNoIdentity) {
			t.Fatalf("err = %v, want ErrNoIdentity", err)
		}
	})

	t.Run("empty accountUuid", func(t *testing.T) {
		_, err := readIdentity(write(t, `{"oauthAccount": {"accountUuid": "", "emailAddress": "x@y.z"}}`))
		if !errors.Is(err, ErrNoIdentity) {
			t.Fatalf("err = %v, want ErrNoIdentity", err)
		}
	})

	t.Run("corrupt document is a real error", func(t *testing.T) {
		_, err := readIdentity(write(t, `{not json`))
		if err == nil || errors.Is(err, ErrNoIdentity) {
			t.Fatalf("err = %v, want a parse error distinct from ErrNoIdentity", err)
		}
	})

	t.Run("corrupt oauthAccount value is a real error", func(t *testing.T) {
		_, err := readIdentity(write(t, `{"oauthAccount": [1, 2]}`))
		if err == nil || errors.Is(err, ErrNoIdentity) {
			t.Fatalf("err = %v, want a parse error distinct from ErrNoIdentity", err)
		}
	})
}

func TestWriteAndStripIdentity(t *testing.T) {
	prov := &overlay.SymlinkProvider{}
	raw := json.RawMessage(`{"accountUuid":"u-1","emailAddress":"me@example.com","organizationUuid":"org-1"}`)
	id := &Identity{AccountUUID: "u-1", EmailAddress: "me@example.com", Raw: raw}

	t.Run("write injects verbatim and preserves the document", func(t *testing.T) {
		acct := t.TempDir()
		dst := filepath.Join(acct, ".claude.json")
		if err := os.WriteFile(dst, []byte(`{"hasCompletedOnboarding": true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeIdentity(prov, acct, id); err != nil {
			t.Fatal(err)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(readFile(t, dst), &top); err != nil {
			t.Fatal(err)
		}
		if string(top["oauthAccount"]) != string(raw) {
			t.Errorf("oauthAccount not verbatim: %s", top["oauthAccount"])
		}
		if string(top["hasCompletedOnboarding"]) != "true" {
			t.Errorf("document content lost: %v", top)
		}
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
		}
	})

	t.Run("write creates a missing file", func(t *testing.T) {
		acct := t.TempDir()
		if err := writeIdentity(prov, acct, id); err != nil {
			t.Fatal(err)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(readFile(t, filepath.Join(acct, ".claude.json")), &top); err != nil {
			t.Fatal(err)
		}
		if string(top["oauthAccount"]) != string(raw) {
			t.Errorf("oauthAccount = %s", top["oauthAccount"])
		}
	})

	t.Run("strip removes only oauthAccount", func(t *testing.T) {
		acct := t.TempDir()
		dst := filepath.Join(acct, ".claude.json")
		body := `{"oauthAccount": ` + string(raw) + `, "numStartups": 3}`
		if err := os.WriteFile(dst, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := stripIdentity(prov, acct); err != nil {
			t.Fatal(err)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(readFile(t, dst), &top); err != nil {
			t.Fatal(err)
		}
		if _, ok := top["oauthAccount"]; ok {
			t.Error("oauthAccount survived the strip")
		}
		if string(top["numStartups"]) != "3" {
			t.Errorf("document content lost: %v", top)
		}
	})

	t.Run("strip of a missing file is a no-op", func(t *testing.T) {
		if err := stripIdentity(prov, t.TempDir()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("fuse-shaped provider targets the private root", func(t *testing.T) {
		acct := filepath.Join(t.TempDir(), "acct-01")
		if err := os.MkdirAll(acct, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeIdentity(&privateRootProvider{}, acct, id); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(acct+".private", ".claude.json")); err != nil {
			t.Fatalf("identity not in private root: %v", err)
		}
		if _, err := os.Stat(filepath.Join(acct, ".claude.json")); !os.IsNotExist(err) {
			t.Fatal("identity must not land in the mountpoint dir")
		}
	})

	t.Run("AccountIdentity reads back what writeIdentity wrote", func(t *testing.T) {
		acct := t.TempDir()
		if err := writeIdentity(prov, acct, id); err != nil {
			t.Fatal(err)
		}
		got, err := AccountIdentity(overlay.KindSymlink, acct)
		if err != nil {
			t.Fatal(err)
		}
		if got.AccountUUID != "u-1" || got.EmailAddress != "me@example.com" {
			t.Errorf("round-trip identity = %+v", got)
		}
	})
}
