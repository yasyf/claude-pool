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
// .claude.json after /login.
type Identity struct {
	AccountUUID  string
	EmailAddress string
}

// AccountIdentity returns a pool account's identity from its private
// .claude.json, written by that account's own login.
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
	}, nil
}
