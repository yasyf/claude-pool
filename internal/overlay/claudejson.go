package overlay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// OAuthAccountKey is the top-level .claude.json key holding an account's
// per-account login identity. It is the one key pool's add-time seeding strips
// (seedClaudeJSON deliberately copies projects/userID for add-time continuity)
// and is always in ClaudeJSONPrivateKeys, so it never crosses between the base
// and an account in either merge direction.
const OAuthAccountKey = "oauthAccount"

// ClaudeJSONPrivateKeys are the top-level .claude.json keys that never
// propagate between the base ~/.claude.json and an account's private copy, in
// either direction. Every other key — including ones claude invents later —
// propagates automatically. Key names are best-effort: an absent key simply
// never matches. Contrast with seedClaudeJSON, which strips ONLY
// OAuthAccountKey at add time so a new account inherits projects/userID.
//
//   - oauthAccount:   per-account login identity, written by that account's
//     own `claude /login`.
//   - userID:         per-account telemetry identity derived at first start.
//   - anonymousId:    per-account anonymous analytics identity.
//   - projects:       per-account project state (history, allowed tools);
//     sharing it would commingle session state across accounts.
//   - firstStartTime: per-account startup record.
//   - numStartups:    per-account startup counter, bumped every launch.
var ClaudeJSONPrivateKeys = map[string]bool{
	OAuthAccountKey:  true,
	"userID":         true,
	"anonymousId":    true,
	"projects":       true,
	"firstStartTime": true,
	"numStartups":    true,
}

// MergeClaudeJSON overlays base's shareable top-level keys onto private and
// returns the merged document. Base wins on every key not in
// ClaudeJSONPrivateKeys; private-only keys survive; base's blacklisted keys
// never appear. changed reports whether any key actually differed, so callers
// can skip rewriting an already-merged file. Base values are normalized to
// json.Marshal's encoding before both the comparison and storage, so a
// pretty-printed base (claude's own writer) reports unchanged once merged —
// the output bytes are unaffected, json.Marshal re-encodes RawMessage anyway.
// A nil base returns private verbatim. The output is json.Marshal of a map —
// key-sorted, hence deterministic bytes for identical inputs (load-bearing for
// the fuse merged view's Getattr/Read coherence). Unparseable or non-object
// private or base is an error; the caller must never replace a file it could
// not parse.
func MergeClaudeJSON(private, base []byte) (merged []byte, changed bool, err error) {
	if base == nil {
		return private, false, nil
	}
	priv, err := parseObject(private, "private claude.json")
	if err != nil {
		return nil, false, err
	}
	top, err := parseObject(base, "base claude.json")
	if err != nil {
		return nil, false, err
	}
	for k, v := range top {
		if ClaudeJSONPrivateKeys[k] {
			continue
		}
		nv, err := normalizeValue(v)
		if err != nil {
			return nil, false, fmt.Errorf("normalize base claude.json key %q: %w", k, err)
		}
		if cur, ok := priv[k]; ok && bytes.Equal(cur, nv) {
			continue
		}
		priv[k] = nv
		changed = true
	}
	merged, err = json.Marshal(priv)
	if err != nil {
		return nil, false, fmt.Errorf("encode merged claude.json: %w", err)
	}
	return merged, changed, nil
}

// SplitClaudeJSON returns new base bytes with payload's shareable top-level
// keys overlaid onto base: every key not in ClaudeJSONPrivateKeys is copied
// from payload, blacklisted keys are never copied (base's own oauthAccount,
// projects, and startup counters stay verbatim), base-only keys are retained,
// and no deletions propagate — a key absent from payload is left alone.
// Unparseable or non-object payload or base is an error: never clobber a base
// you cannot parse.
func SplitClaudeJSON(payload, base []byte) ([]byte, error) {
	top, err := parseObject(payload, "claude.json payload")
	if err != nil {
		return nil, err
	}
	out, err := parseObject(base, "base claude.json")
	if err != nil {
		return nil, err
	}
	for k, v := range top {
		if ClaudeJSONPrivateKeys[k] {
			continue
		}
		out[k] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode split claude.json: %w", err)
	}
	return b, nil
}

// parseObject decodes b's top-level keys to raw values, rejecting any document
// that is not a JSON object: json.Unmarshal accepts a bare `null` and leaves
// the map nil, which would turn the merge loops into silent no-ops or nil-map
// panics.
func parseObject(b []byte, what string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", what, err)
	}
	if m == nil {
		return nil, fmt.Errorf("parse %s: not a JSON object", what)
	}
	return m, nil
}

// normalizeValue re-encodes one raw JSON value to the exact bytes json.Marshal
// emits for an embedded RawMessage: compacted, then HTML-escaped (Marshal's
// encoder compacts WITH escaping; json.Compact alone leaves <, >, & raw and
// would keep the comparison failing for values carrying them). Merged output
// values always hold this form, so base values must be normalized identically
// before the equality probe or a pretty-printed base makes every merge report
// changed. A Compact failure on bytes json.Unmarshal already accepted cannot
// happen short of a programmer error; it is returned, never silently passed
// through.
func normalizeValue(v json.RawMessage) (json.RawMessage, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, v); err != nil {
		return nil, err
	}
	var escaped bytes.Buffer
	json.HTMLEscape(&escaped, compact.Bytes())
	return escaped.Bytes(), nil
}

// WriteAtomic0600 writes data to dst via temp+rename in dst's directory, so a
// concurrent reader never sees a partial file. Creates the directory if
// missing.
func WriteAtomic0600(dst string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
