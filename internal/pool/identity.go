package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yasyf/cc-pool/internal/overlay"
)

// ErrNoIdentity means a .claude.json has no usable top-level "oauthAccount"
// identity (file missing, key absent, or accountUuid empty).
var ErrNoIdentity = errors.New("no oauthAccount identity in .claude.json")

// Identity is the top-level "oauthAccount" object claude writes into its
// .claude.json after /login. Raw preserves the verbatim JSON value — the real
// object carries fields beyond these two (org info, …) that must survive a
// write-back untouched.
type Identity struct {
	AccountUUID  string
	EmailAddress string
	Raw          json.RawMessage
}

// CanonicalIdentity returns plain claude's current identity from
// ~/.claude.json.
func CanonicalIdentity() (*Identity, error) {
	return readIdentity(ClaudeJSONPath())
}

// AccountIdentity returns a pool account's identity from its private
// .claude.json (written by that account's own login or by adoption).
func AccountIdentity(kind overlay.Kind, configDir string) (*Identity, error) {
	priv := overlay.For(kind).PrivateRoot(configDir)
	return readIdentity(filepath.Join(priv, ".claude.json"))
}

// readIdentity parses the top-level "oauthAccount" object out of a
// .claude.json. Missing file or missing/empty identity → ErrNoIdentity;
// unparseable JSON is a real error.
func readIdentity(path string) (*Identity, error) {
	src, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, ErrNoIdentity
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(src, &top); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	raw, ok := top["oauthAccount"]
	if !ok {
		return nil, ErrNoIdentity
	}
	var fields struct {
		AccountUUID  string `json:"accountUuid"`
		EmailAddress string `json:"emailAddress"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("parse oauthAccount in %s: %w", path, err)
	}
	if fields.AccountUUID == "" {
		return nil, ErrNoIdentity
	}
	return &Identity{
		AccountUUID:  fields.AccountUUID,
		EmailAddress: fields.EmailAddress,
		Raw:          raw,
	}, nil
}

// writeIdentity injects id.Raw as the top-level "oauthAccount" of an account's
// private .claude.json, preserving the rest of the document byte-for-byte at
// the value level. Used by adoption to un-strip the identity seedClaudeJSON
// removed.
func writeIdentity(prov overlay.Provider, accountDir string, id *Identity) error {
	return rewriteIdentity(prov, accountDir, id.Raw)
}

// stripIdentity removes the top-level "oauthAccount" from an account's private
// .claude.json. Used by adoption-failure cleanup so a retried add re-seeds
// cleanly. A missing file is a no-op.
func stripIdentity(prov overlay.Provider, accountDir string) error {
	return rewriteIdentity(prov, accountDir, nil)
}

// rewriteIdentity sets (raw != nil) or deletes (raw == nil) the top-level
// "oauthAccount" key of the account's private .claude.json via temp+rename.
func rewriteIdentity(prov overlay.Provider, accountDir string, raw json.RawMessage) error {
	dst := filepath.Join(prov.PrivateRoot(accountDir), ".claude.json")
	src, err := os.ReadFile(dst)
	top := map[string]json.RawMessage{}
	if err == nil {
		if err := json.Unmarshal(src, &top); err != nil {
			return fmt.Errorf("parse %s: %w", dst, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", dst, err)
	} else if raw == nil {
		return nil // nothing to strip
	}
	if raw == nil {
		delete(top, "oauthAccount")
	} else {
		top["oauthAccount"] = raw
	}
	out, err := json.Marshal(top)
	if err != nil {
		return fmt.Errorf("encode %s: %w", dst, err)
	}
	if err := writeAtomic0600(dst, out); err != nil {
		return fmt.Errorf("install %s: %w", dst, err)
	}
	return nil
}
