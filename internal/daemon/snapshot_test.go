package daemon

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-pool/internal/store"
	"github.com/yasyf/cc-pool/internal/version"
)

// readSnapshot reads and decodes the snapshot file, failing the test on any error.
func readSnapshot(t *testing.T, path string) StatusSnapshot {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap StatusSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, data)
	}
	return snap
}

func TestWriteStatusSnapshotRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)
	dir := t.TempDir()
	s.snapshot = filepath.Join(dir, "status.json")

	if err := s.writeStatusSnapshot(t.Context()); err != nil {
		t.Fatal(err)
	}

	snap := readSnapshot(t, s.snapshot)
	if snap.Proto != ProtocolVersion {
		t.Errorf("proto = %d, want %d", snap.Proto, ProtocolVersion)
	}
	if snap.Version != version.String() {
		t.Errorf("version = %q, want %q", snap.Version, version.String())
	}
	if !snap.GeneratedAt.Equal(snap.GeneratedAt.Truncate(time.Second)) {
		t.Errorf("generated_at %v carries sub-second precision", snap.GeneratedAt)
	}
	if age := time.Since(snap.GeneratedAt); age < 0 || age > time.Minute {
		t.Errorf("generated_at %v is not recent (age %v)", snap.GeneratedAt, age)
	}

	// The harness seeds acct-1 at util 10 and acct-2 at util 50.
	want5h := map[int]float64{1: 90, 2: 50}
	if len(snap.Accounts) != len(want5h) {
		t.Fatalf("accounts = %d, want %d: %+v", len(snap.Accounts), len(want5h), snap.Accounts)
	}
	for _, a := range snap.Accounts {
		if want, ok := want5h[a.ID]; !ok || a.Remaining5h != want {
			t.Errorf("acct-%02d remaining_5h = %.1f, want %.1f", a.ID, a.Remaining5h, want)
		}
		if !a.HasUsage {
			t.Errorf("acct-%02d has_usage = false on a sampled account", a.ID)
		}
	}

	// Atomic write must leave neither temp files nor anything else behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "status.json" {
		t.Errorf("snapshot dir not clean: %v", entries)
	}
	info, err := os.Stat(s.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("snapshot perms = %o, want 600", perm)
	}
}

func TestWriteStatusSnapshotOverwrites(t *testing.T) {
	s, _ := newTestServer(t)
	s.snapshot = filepath.Join(t.TempDir(), "status.json")

	if err := s.writeStatusSnapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A newer sample must replace the file's view of acct-1 on the next write.
	if err := s.m.Store.InsertUsageSample(store.UsageSample{
		AccountID: 1, TS: time.Now().Add(time.Minute), Util5h: 70, Util7d: 70,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeStatusSnapshot(t.Context()); err != nil {
		t.Fatal(err)
	}

	snap := readSnapshot(t, s.snapshot)
	for _, a := range snap.Accounts {
		if a.ID == 1 && a.Remaining5h != 30 {
			t.Errorf("acct-01 remaining_5h = %.1f after newer sample, want 30", a.Remaining5h)
		}
	}
}

func TestWriteStatusSnapshotError(t *testing.T) {
	s, _ := newTestServer(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.snapshot = filepath.Join(blocker, "status.json") // parent is a regular file

	err := s.writeStatusSnapshot(t.Context())
	if err == nil {
		t.Fatal("expected an error writing under a regular file")
	}
	if !strings.Contains(err.Error(), "write status snapshot") {
		t.Errorf("error %q lacks the write-layer wrap", err)
	}
}

func TestPollOnceWritesSnapshot(t *testing.T) {
	// Redirect ClaudeDir/StateDir off the real ~/.claude and ~/.cc-pool.
	t.Setenv("HOME", t.TempDir())
	s, _ := newTestServer(t)
	s.snapshot = filepath.Join(t.TempDir(), "status.json")

	s.pollOnce(t.Context())

	snap := readSnapshot(t, s.snapshot)
	if len(snap.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2: %+v", len(snap.Accounts), snap.Accounts)
	}
}

func TestPollOnceLogsSnapshotFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := newTestServer(t)
	var buf bytes.Buffer
	s.log = log.New(&buf, "", 0)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.snapshot = filepath.Join(blocker, "status.json")

	// Must complete the poll (the snapshot write is its last step) and log the
	// failure rather than aborting.
	s.pollOnce(t.Context())

	if !strings.Contains(buf.String(), "status snapshot:") {
		t.Errorf("log missing snapshot failure:\n%s", buf.String())
	}
}

// TestStatusSnapshotJSONKeys pins the exact wire keys the Swift widget decodes.
// Renaming or re-casing any of these is a breaking change for out-of-process
// readers and must bump ProtocolVersion.
func TestStatusSnapshotJSONKeys(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 500e6, time.UTC) // sub-second: truncation must strip it

	t.Run("fully populated", func(t *testing.T) {
		full := AccountStatus{
			ID: 1, ConfigDir: "/x/acct-01", Label: "a@b.c", OverlayKind: "symlink",
			Score: 95.5, Remaining5h: 90, Remaining7d: 80, ActiveSessions: 2,
			RateLimited: true, Exhausted: true, HasUsage: true, Stale: true,
			Resets5h: now, Resets7d: now, SampleAge: "30s",
			ExtraEnabled: true, ExtraUsed: 177, ExtraLimit: 5000,
		}
		data, err := json.Marshal(NewStatusSnapshot([]AccountStatus{full}, now))
		if err != nil {
			t.Fatal(err)
		}

		var top map[string]json.RawMessage
		if err := json.Unmarshal(data, &top); err != nil {
			t.Fatal(err)
		}
		assertKeys(t, "top-level", top, []string{"proto", "version", "generated_at", "accounts"})
		if got := string(top["generated_at"]); got != `"2026-06-11T12:00:00Z"` {
			t.Errorf("generated_at = %s, want whole-second UTC", got)
		}
		// Absolute pin, not == ProtocolVersion: deployed widgets hard-reject any
		// other value. If a bump is intentional, update supportedProto in
		// widget/Sources/Widget/Provider.swift and re-ship the widget with it.
		if got := string(top["proto"]); got != "1" {
			t.Errorf("snapshot proto = %s; the on-disk format is pinned at 1 for the widget", got)
		}

		var accounts []map[string]json.RawMessage
		if err := json.Unmarshal(top["accounts"], &accounts); err != nil {
			t.Fatal(err)
		}
		assertKeys(t, "account", accounts[0], []string{
			"id", "config_dir", "label", "overlay_kind", "score",
			"remaining_5h", "remaining_7d", "active_sessions", "rate_limited",
			"exhausted", "has_usage", "stale", "resets_5h", "resets_7d",
			"sample_age", "extra_enabled", "extra_used", "extra_limit", "components",
		})

		// score.Components has no json tags, so its keys are PascalCase amid
		// snake_case siblings — the widget must skip it, never decode it.
		var components map[string]json.RawMessage
		if err := json.Unmarshal(accounts[0]["components"], &components); err != nil {
			t.Fatal(err)
		}
		assertKeys(t, "components", components, []string{
			"Eff5", "Eff7", "RawRemaining5h", "RawRemaining7d",
			"Remaining5h", "Remaining7d", "SessionPenalty", "RateLimitPenalty",
			"StalePenalty", "Barrier5h", "Barrier7d", "RunwayPenalty",
		})
	})

	t.Run("zero value omits omitempty fields", func(t *testing.T) {
		data, err := json.Marshal(NewStatusSnapshot([]AccountStatus{{}}, now))
		if err != nil {
			t.Fatal(err)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(data, &top); err != nil {
			t.Fatal(err)
		}
		var accounts []map[string]json.RawMessage
		if err := json.Unmarshal(top["accounts"], &accounts); err != nil {
			t.Fatal(err)
		}
		for _, absent := range []string{"exhausted", "extra_enabled", "extra_used", "extra_limit"} {
			if _, ok := accounts[0][absent]; ok {
				t.Errorf("zero-value account must omit %q (the widget models it as optional)", absent)
			}
		}
		// The zero time is NOT omitted; it serializes as year 1 and the widget
		// must treat it as "no active window".
		if got := string(accounts[0]["resets_5h"]); got != `"0001-01-01T00:00:00Z"` {
			t.Errorf("zero resets_5h = %s, want year-1 sentinel", got)
		}
	})

	t.Run("empty pool marshals as empty array", func(t *testing.T) {
		// Both empty-pool producers: the daemon write path (ToStatuses) and the
		// --json daemon branch, where Response.Accounts is omitempty so an empty
		// socket reply decodes as a NIL slice that NewStatusSnapshot must
		// normalize — "accounts": null would break the widget's decoder.
		for name, accounts := range map[string][]AccountStatus{
			"via ToStatuses":      ToStatuses(nil),
			"via nil socket pass": nil,
		} {
			data, err := json.Marshal(NewStatusSnapshot(accounts, now))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"accounts":[]`) {
				t.Errorf("%s: empty pool must serialize accounts as [], got %s", name, data)
			}
		}
	})
}

// assertKeys fails unless m's key set is exactly want.
func assertKeys[V any](t *testing.T, label string, m map[string]V, want []string) {
	t.Helper()
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s keys = %v, want %v", label, got, want)
	}
}
