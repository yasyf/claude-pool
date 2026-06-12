package overlay

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Fixture values are deliberately compact (no whitespace inside values):
// json.Marshal compacts json.RawMessage values, so only compact inputs can be
// asserted byte-identical across a merge or split.
const (
	mergePrivate = `{
		"hasCompletedOnboarding": true,
		"theme": "dark",
		"oauthAccount": {"accountUuid":"acct-own"},
		"userID": "acct-user",
		"anonymousId": "acct-anon",
		"projects": {"/acct":{"history":["mine"]}},
		"firstStartTime": "2026-01-01T00:00:00Z",
		"numStartups": 7,
		"acctOnly": "survives"
	}`
	mergeBase = `{
		"hasCompletedOnboarding": true,
		"theme": "light",
		"claudeInChromeDefaultEnabled": true,
		"oauthAccount": {"accountUuid":"base-own"},
		"userID": "base-user",
		"anonymousId": "base-anon",
		"projects": {"/base":{"history":["theirs"]}},
		"firstStartTime": "2020-01-01T00:00:00Z",
		"numStartups": 9999
	}`
)

// raw decodes b into top-level raw messages so per-key values can be compared
// byte-exactly (the merge/split contract is byte-exact round-tripping).
func raw(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return m
}

func TestMergeClaudeJSON(t *testing.T) {
	cases := map[string]struct {
		private, base string
		wantChanged   bool
		want          map[string]string // key → expected raw JSON value
		absent        []string
		wantErr       bool
	}{
		"base wins on shareable keys": {
			private: mergePrivate, base: mergeBase, wantChanged: true,
			want: map[string]string{"theme": `"light"`, "claudeInChromeDefaultEnabled": `true`},
		},
		"private blacklisted keys survive byte-identical": {
			private: mergePrivate, base: mergeBase, wantChanged: true,
			want: map[string]string{
				"oauthAccount":   `{"accountUuid":"acct-own"}`,
				"userID":         `"acct-user"`,
				"anonymousId":    `"acct-anon"`,
				"projects":       `{"/acct":{"history":["mine"]}}`,
				"firstStartTime": `"2026-01-01T00:00:00Z"`,
				"numStartups":    `7`,
			},
		},
		"base oauthAccount never leaks into a private file lacking one": {
			private: `{"theme": "dark"}`, base: mergeBase, wantChanged: true,
			want:   map[string]string{"theme": `"light"`},
			absent: []string{"oauthAccount", "userID", "anonymousId", "projects", "firstStartTime", "numStartups"},
		},
		"private-only key shows through": {
			private: mergePrivate, base: mergeBase, wantChanged: true,
			want: map[string]string{"acctOnly": `"survives"`},
		},
		"identical shareable keys report unchanged": {
			private: `{"theme": "dark", "numStartups": 7}`, base: `{"theme": "dark", "numStartups": 9999}`,
			wantChanged: false,
			want:        map[string]string{"theme": `"dark"`, "numStartups": `7`},
		},
		"unparseable private errors": {private: `{not json`, base: mergeBase, wantErr: true},
		"unparseable base errors":    {private: mergePrivate, base: `{not json`, wantErr: true},
		"null private errors":        {private: `null`, base: mergeBase, wantErr: true},
		"null base errors":           {private: mergePrivate, base: `null`, wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			merged, changed, err := MergeClaudeJSON([]byte(tc.private), []byte(tc.base))
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			got := raw(t, merged)
			for k, want := range tc.want {
				if string(got[k]) != want {
					t.Errorf("merged[%q] = %s, want %s", k, got[k], want)
				}
			}
			for _, k := range tc.absent {
				if v, ok := got[k]; ok {
					t.Errorf("merged[%q] = %s, want absent", k, v)
				}
			}
		})
	}
}

// TestMergeClaudeJSONNilBase pins the degenerate contract: no base means the
// private bytes come back verbatim — formatting included — with no change.
func TestMergeClaudeJSONNilBase(t *testing.T) {
	private := []byte("{ \"theme\":   \"dark\" }\n")
	merged, changed, err := MergeClaudeJSON(private, nil)
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v, want false,nil", changed, err)
	}
	if !bytes.Equal(merged, private) {
		t.Fatalf("merged = %q, want private verbatim %q", merged, private)
	}
}

// TestMergeClaudeJSONIdempotentAgainstPrettyBase pins the production shape of
// the no-write short-circuit: ~/.claude.json is pretty-printed (claude's own
// writer), while merged output holds json.Marshal-compact values. Merging the
// output again against the SAME indented base must report changed=false with
// byte-identical output — otherwise every launch rewrites the account file and
// the MergeUnchanged race guard is dead code. The fixture deliberately carries
// a multi-line composite value (whitespace divergence) and a string with raw
// <, & (HTML-escape divergence — json.Marshal escapes, json.Compact does not).
func TestMergeClaudeJSONIdempotentAgainstPrettyBase(t *testing.T) {
	prettyBase := []byte(`{
  "theme": "light",
  "customApiKeyResponses": {
    "approved": ["k1", "k2"],
    "rejected": []
  },
  "cachedChangelog": "<h1>v1 & v2</h1>",
  "oauthAccount": {
    "accountUuid": "base-own"
  }
}`)
	first, changed, err := MergeClaudeJSON([]byte(mergePrivate), prettyBase)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first merge: changed = false, want true (base carries new shareable keys)")
	}
	second, changed, err := MergeClaudeJSON(first, prettyBase)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second merge against the same pretty base: changed = true, want false")
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("second merge diverged from the first:\n%s\n%s", first, second)
	}
}

// TestMergeClaudeJSONDeterministic pins byte-determinism: two merges of the
// same inputs must be byte-equal (json.Marshal key-sorts maps). The fuse
// merged view's Getattr size and Read content depend on it.
func TestMergeClaudeJSONDeterministic(t *testing.T) {
	a, _, err := MergeClaudeJSON([]byte(mergePrivate), []byte(mergeBase))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := MergeClaudeJSON([]byte(mergePrivate), []byte(mergeBase))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("two merges diverged:\n%s\n%s", a, b)
	}
}

func TestSplitClaudeJSON(t *testing.T) {
	// payload is what claude committed through a pooled session: shareable
	// changes plus its own per-account state, which must never reach base.
	payload := `{
		"theme": "solarized",
		"newSetting": 42,
		"oauthAccount": {"accountUuid":"acct-own"},
		"projects": {"/acct":{"history":["mine"]}},
		"numStartups": 7,
		"userID": "acct-user",
		"anonymousId": "acct-anon",
		"firstStartTime": "2026-01-01T00:00:00Z"
	}`
	cases := map[string]struct {
		payload, base string
		want          map[string]string
		wantErr       bool
	}{
		"shareable payload keys overlay base": {
			payload: payload, base: mergeBase,
			want: map[string]string{"theme": `"solarized"`, "newSetting": `42`},
		},
		"blacklisted payload keys never copied": {
			payload: payload, base: mergeBase,
			want: map[string]string{
				"oauthAccount":   `{"accountUuid":"base-own"}`,
				"projects":       `{"/base":{"history":["theirs"]}}`,
				"numStartups":    `9999`,
				"userID":         `"base-user"`,
				"anonymousId":    `"base-anon"`,
				"firstStartTime": `"2020-01-01T00:00:00Z"`,
			},
		},
		"base-only keys retained, deletions never propagate": {
			payload: `{"theme": "solarized"}`, base: `{"theme": "light", "keepMe": true}`,
			want: map[string]string{"theme": `"solarized"`, "keepMe": `true`},
		},
		"unparseable payload errors": {payload: `{not json`, base: mergeBase, wantErr: true},
		"unparseable base errors":    {payload: payload, base: `{not json`, wantErr: true},
		"nil base errors":            {payload: payload, base: "", wantErr: true},
		"null payload errors":        {payload: `null`, base: mergeBase, wantErr: true},
		"null base errors":           {payload: payload, base: `null`, wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var base []byte
			if tc.base != "" {
				base = []byte(tc.base)
			}
			out, err := SplitClaudeJSON([]byte(tc.payload), base)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := raw(t, out)
			for k, want := range tc.want {
				if string(got[k]) != want {
					t.Errorf("split[%q] = %s, want %s", k, got[k], want)
				}
			}
		})
	}
}

// TestMergeSplitRoundTrip rehearses the fuse write-through cycle: merge base
// over private, let the session change a shareable key, split the commit back.
// The new base must carry the change while every blacklisted base key stays
// byte-identical.
func TestMergeSplitRoundTrip(t *testing.T) {
	merged, _, err := MergeClaudeJSON([]byte(mergePrivate), []byte(mergeBase))
	if err != nil {
		t.Fatal(err)
	}
	doc := raw(t, merged)
	doc["theme"] = json.RawMessage(`"solarized"`)
	committed, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	newBase, err := SplitClaudeJSON(committed, []byte(mergeBase))
	if err != nil {
		t.Fatal(err)
	}
	got, orig := raw(t, newBase), raw(t, []byte(mergeBase))
	if string(got["theme"]) != `"solarized"` {
		t.Errorf("shareable change lost: theme = %s", got["theme"])
	}
	for k := range ClaudeJSONPrivateKeys {
		if !bytes.Equal(got[k], orig[k]) {
			t.Errorf("base[%q] changed across the round trip: %s → %s", k, orig[k], got[k])
		}
	}
}
