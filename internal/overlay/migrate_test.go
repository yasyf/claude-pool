package overlay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestMovePrivateEntries(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, from, to string)
		wantErr string // substring; "" means success
		verify  func(t *testing.T, from, to string)
	}{
		{
			name: "moves identity, credential, and tmp siblings",
			setup: func(t *testing.T, from, to string) {
				writeFile(t, filepath.Join(from, ".claude.json"), `{"oauthAccount":"a"}`)
				writeFile(t, filepath.Join(from, ".credentials.json"), "secret")
				writeFile(t, filepath.Join(from, ".claude.json.tmp.ab12"), "tmp")
				writeFile(t, filepath.Join(from, ".last-update-result.json"), "upd")
				writeFile(t, filepath.Join(from, "remote-settings.json"), "rs")
			},
			verify: func(t *testing.T, from, to string) {
				for name, want := range map[string]string{
					".claude.json":             `{"oauthAccount":"a"}`,
					".credentials.json":        "secret",
					".claude.json.tmp.ab12":    "tmp",
					".last-update-result.json": "upd",
					"remote-settings.json":     "rs",
				} {
					if got := readFile(t, filepath.Join(to, name)); got != want {
						t.Errorf("%s = %q, want %q", name, got, want)
					}
					if _, err := os.Lstat(filepath.Join(from, name)); !os.IsNotExist(err) {
						t.Errorf("%s still present in source", name)
					}
				}
			},
		},
		{
			name: "moves excluded dirs with nested contents",
			setup: func(t *testing.T, from, to string) {
				writeFile(t, filepath.Join(from, "backups", "2026", "x.bak"), "bak")
				writeFile(t, filepath.Join(from, "daemon", "roster.json"), "roster")
			},
			verify: func(t *testing.T, from, to string) {
				if got := readFile(t, filepath.Join(to, "backups", "2026", "x.bak")); got != "bak" {
					t.Errorf("backups content = %q, want bak", got)
				}
				if got := readFile(t, filepath.Join(to, "daemon", "roster.json")); got != "roster" {
					t.Errorf("daemon content = %q, want roster", got)
				}
				if _, err := os.Lstat(filepath.Join(from, "backups")); !os.IsNotExist(err) {
					t.Error("backups still present in source")
				}
			},
		},
		{
			name: "leaves shared symlinks and unclassified entries alone",
			setup: func(t *testing.T, from, to string) {
				if err := os.Symlink("/tmp/elsewhere", filepath.Join(from, "projects")); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(from, "notes.txt"), "keep")
				writeFile(t, filepath.Join(from, ".claude.json"), "id")
			},
			verify: func(t *testing.T, from, to string) {
				if _, err := os.Lstat(filepath.Join(from, "projects")); err != nil {
					t.Errorf("shared symlink moved: %v", err)
				}
				if got := readFile(t, filepath.Join(from, "notes.txt")); got != "keep" {
					t.Errorf("unclassified file disturbed: %q", got)
				}
				if _, err := os.Lstat(filepath.Join(to, "projects")); !os.IsNotExist(err) {
					t.Error("shared symlink leaked into destination")
				}
			},
		},
		{
			name: "idempotent second run",
			setup: func(t *testing.T, from, to string) {
				writeFile(t, filepath.Join(from, ".claude.json"), "id")
				if err := MovePrivateEntries(from, to); err != nil {
					t.Fatalf("first run: %v", err)
				}
			},
			verify: func(t *testing.T, from, to string) {
				if got := readFile(t, filepath.Join(to, ".claude.json")); got != "id" {
					t.Errorf(".claude.json = %q, want id", got)
				}
			},
		},
		{
			name: "resumes a partial move",
			setup: func(t *testing.T, from, to string) {
				writeFile(t, filepath.Join(to, ".claude.json"), "already-moved")
				writeFile(t, filepath.Join(from, "backups", "b.bak"), "bak")
			},
			verify: func(t *testing.T, from, to string) {
				if got := readFile(t, filepath.Join(to, ".claude.json")); got != "already-moved" {
					t.Errorf(".claude.json = %q, want already-moved", got)
				}
				if got := readFile(t, filepath.Join(to, "backups", "b.bak")); got != "bak" {
					t.Errorf("backups not resumed: %q", got)
				}
			},
		},
		{
			name: "file collision fails loud with both copies intact",
			setup: func(t *testing.T, from, to string) {
				writeFile(t, filepath.Join(from, ".claude.json"), "src-identity")
				writeFile(t, filepath.Join(to, ".claude.json"), "dst-identity")
			},
			wantErr: "collision",
			verify: func(t *testing.T, from, to string) {
				if got := readFile(t, filepath.Join(from, ".claude.json")); got != "src-identity" {
					t.Errorf("source clobbered: %q", got)
				}
				if got := readFile(t, filepath.Join(to, ".claude.json")); got != "dst-identity" {
					t.Errorf("destination clobbered: %q", got)
				}
			},
		},
		{
			name: "merges into a pre-created empty excluded dir",
			setup: func(t *testing.T, from, to string) {
				// fuse Setup pre-creates empty excluded dirs in the backing root.
				if err := os.MkdirAll(filepath.Join(to, "backups"), 0o700); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(from, "backups", "b.bak"), "bak")
			},
			verify: func(t *testing.T, from, to string) {
				if got := readFile(t, filepath.Join(to, "backups", "b.bak")); got != "bak" {
					t.Errorf("merge lost content: %q", got)
				}
				if _, err := os.Lstat(filepath.Join(from, "backups")); !os.IsNotExist(err) {
					t.Error("merged source dir not removed")
				}
			},
		},
		{
			name: "dir merge child collision fails with both intact",
			setup: func(t *testing.T, from, to string) {
				writeFile(t, filepath.Join(from, "backups", "x"), "src")
				writeFile(t, filepath.Join(to, "backups", "x"), "dst")
			},
			wantErr: "collision",
			verify: func(t *testing.T, from, to string) {
				if got := readFile(t, filepath.Join(from, "backups", "x")); got != "src" {
					t.Errorf("source clobbered: %q", got)
				}
				if got := readFile(t, filepath.Join(to, "backups", "x")); got != "dst" {
					t.Errorf("destination clobbered: %q", got)
				}
			},
		},
		{
			name: "DS_Store inside a merged dir is dropped",
			setup: func(t *testing.T, from, to string) {
				if err := os.MkdirAll(filepath.Join(to, "backups"), 0o700); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(from, "backups", ".DS_Store"), "cruft")
				writeFile(t, filepath.Join(from, "backups", "b.bak"), "bak")
			},
			verify: func(t *testing.T, from, to string) {
				if _, err := os.Lstat(filepath.Join(to, "backups", ".DS_Store")); !os.IsNotExist(err) {
					t.Error(".DS_Store merged instead of dropped")
				}
				if got := readFile(t, filepath.Join(to, "backups", "b.bak")); got != "bak" {
					t.Errorf("merge lost content: %q", got)
				}
			},
		},
		{
			name: "stale symlink at a private name is removed, not moved",
			setup: func(t *testing.T, from, to string) {
				if err := os.Symlink("/tmp/elsewhere", filepath.Join(from, ".claude.json")); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, from, to string) {
				if _, err := os.Lstat(filepath.Join(from, ".claude.json")); !os.IsNotExist(err) {
					t.Error("stale private link still in source")
				}
				if _, err := os.Lstat(filepath.Join(to, ".claude.json")); !os.IsNotExist(err) {
					t.Error("stale private link moved to destination")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to := t.TempDir(), t.TempDir()
			tc.setup(t, from, to)
			err := MovePrivateEntries(from, to)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("MovePrivateEntries: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("MovePrivateEntries error = %v, want substring %q", err, tc.wantErr)
				}
			}
			tc.verify(t, from, to)
		})
	}
}

func TestMovePrivateEntriesRejectsBadRoots(t *testing.T) {
	dir := t.TempDir()
	if err := MovePrivateEntries(filepath.Join(dir, "missing"), dir); err == nil {
		t.Error("missing source did not error")
	}
	if err := MovePrivateEntries(dir, dir); err == nil {
		t.Error("from == to did not error")
	}
	if err := MovePrivateEntries("", dir); err == nil {
		t.Error("empty from did not error")
	}
}

func TestHasPrivateEntries(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  bool
	}{
		{
			name:  "missing dir has none",
			setup: func(t *testing.T, dir string) { os.RemoveAll(dir) },
			want:  false,
		},
		{
			name: "empty excluded dirs are shape, not state",
			setup: func(t *testing.T, dir string) {
				for name := range ExcludedEntries {
					if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
						t.Fatal(err)
					}
				}
			},
			want: false,
		},
		{
			name:  "private file counts",
			setup: func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, ".claude.json"), "id") },
			want:  true,
		},
		{
			name:  "non-empty excluded dir counts",
			setup: func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, "backups", "b.bak"), "bak") },
			want:  true,
		},
		{
			name:  "unclassified file does not count",
			setup: func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, "notes.txt"), "x") },
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			got, err := HasPrivateEntries(dir)
			if err != nil {
				t.Fatalf("HasPrivateEntries: %v", err)
			}
			if got != tc.want {
				t.Errorf("HasPrivateEntries = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMountedGuards pins the symlink provider's refusal to operate on a live
// mountpoint — writing symlinks through a fuse mirror would land them in the
// real ~/.claude, and tearing down through one would RemoveAll the private
// backing. /dev (devfs) stands in for a mount without needing fuse.
func TestMountedGuards(t *testing.T) {
	if !Mounted("/dev") {
		t.Fatal("Mounted(/dev) = false; devfs should be a mountpoint")
	}
	if Mounted(t.TempDir()) {
		t.Fatal("Mounted(tempdir) = true")
	}

	base := t.TempDir()
	p := &SymlinkProvider{}
	if err := p.Sync(base, "/dev"); err == nil || !strings.Contains(err.Error(), "mountpoint") {
		t.Errorf("Sync on a mountpoint = %v, want mountpoint refusal", err)
	}
	if err := p.Teardown(base, "/dev"); err == nil || !strings.Contains(err.Error(), "mountpoint") {
		t.Errorf("Teardown on a mountpoint = %v, want mountpoint refusal", err)
	}
}
