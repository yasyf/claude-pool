package pool

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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

	t.Run("happy path parses fields, ignoring unknown keys", func(t *testing.T) {
		raw := `{"accountUuid":"u-1","emailAddress":"me@example.com","organizationUuid":"org-1"}`
		id, err := readIdentity(write(t, `{"oauthAccount": `+raw+`, "numStartups": 3}`))
		if err != nil {
			t.Fatal(err)
		}
		if id.AccountUUID != "u-1" || id.EmailAddress != "me@example.com" {
			t.Errorf("parsed fields = %+v", id)
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
