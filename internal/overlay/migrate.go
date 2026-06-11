package overlay

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// This file holds the untagged primitives that overlay conversion (and crash
// repair) is built from. They compile in every build variant: even a non-fuse
// binary must be able to recognize a fuse account's private backing dir and
// move stranded private files back, and must be able to refuse symlink
// operations on a live mountpoint.

// FusePrivateRoot is the fuse provider's per-account private backing dir: a
// sibling of the mountpoint (accountDir + ".private"). Private entries
// (PrivateEntry names) physically live there while the account uses the fuse
// overlay; the mirror redirects their paths so they remain visible through the
// mount. The path is never exported as CLAUDE_CONFIG_DIR and never hashed for
// Keychain service names.
func FusePrivateRoot(accountDir string) string {
	return accountDir + ".private"
}

// MovePrivateEntries relocates every top-level private entry (PrivateEntry
// names, which include the ExcludedEntries dirs) from one private root to the
// other via same-volume rename. Shared symlinks and unclassified entries are
// left untouched. It is idempotent and resumable: already-moved entries are
// skipped, so re-running after a crash converges. A directory that exists on
// both sides is merged child-by-child (fuse Setup pre-creates empty excluded
// dirs in the backing root); a file that exists on both sides is a collision
// and fails loudly with both copies intact — identity files are never
// clobbered. Per-entry failures are collected with errors.Join, like Sync.
func MovePrivateEntries(from, to string) error {
	if from == "" || to == "" {
		return fmt.Errorf("move private entries: empty root (from %q, to %q)", from, to)
	}
	if from == to {
		return fmt.Errorf("move private entries: from and to are both %q", from)
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return fmt.Errorf("read private root %q: %w", from, err)
	}
	if err := os.MkdirAll(to, 0o700); err != nil {
		return fmt.Errorf("mkdir private root %q: %w", to, err)
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if !PrivateEntry(name) {
			continue
		}
		src := filepath.Join(from, name)
		// A symlink at a private name is our own stale artifact from before the
		// name was classified private (same invariant as assertNoSymlink):
		// remove it, never move it.
		if fi, err := os.Lstat(src); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(src); err != nil {
				errs = append(errs, fmt.Errorf("remove stale private link %q: %w", src, err))
			}
			continue
		}
		if err := moveEntry(src, filepath.Join(to, name)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// moveEntry renames src to dst. An existing destination directory is merged;
// an existing destination file is a collision.
func moveEntry(src, dst string) error {
	dfi, err := os.Lstat(dst)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat %q: %w", dst, err)
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("move %q: %w", src, err)
		}
		return nil
	}
	sfi, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("stat %q: %w", src, err)
	}
	if sfi.IsDir() && dfi.IsDir() {
		return mergeDir(src, dst)
	}
	return fmt.Errorf("private entry collision: %q already exists, refusing to clobber it with %q", dst, src)
}

// mergeDir moves src's children into dst (recursing into shared subdirs) and
// removes the then-empty src. OS cruft (.DS_Store) is dropped, not merged.
func mergeDir(src, dst string) error {
	children, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read %q: %w", src, err)
	}
	var errs []error
	for _, c := range children {
		if skipEntries[c.Name()] {
			if err := os.Remove(filepath.Join(src, c.Name())); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err := moveEntry(filepath.Join(src, c.Name()), filepath.Join(dst, c.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("remove merged dir %q: %w", src, err)
	}
	return nil
}

// HasPrivateEntries reports whether dir holds meaningful per-account private
// state: a private file, or a private dir with at least one entry. The empty
// excluded dirs that fuse Setup pre-creates do not count — they are shape, not
// state. Used to detect files stranded in a fuse private root by an
// interrupted conversion. A missing dir trivially has none.
func HasPrivateEntries(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read private root %q: %w", dir, err)
	}
	for _, e := range entries {
		if !PrivateEntry(e.Name()) {
			continue
		}
		if !e.IsDir() {
			return true, nil
		}
		children, err := os.ReadDir(filepath.Join(dir, e.Name()))
		if err != nil {
			return false, fmt.Errorf("read private dir %q: %w", filepath.Join(dir, e.Name()), err)
		}
		if len(children) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// Mounted reports whether dir is currently a mountpoint (its device id differs
// from its parent's). Symlink-provider operations consult it to refuse writing
// symlinks "into" a live fuse mirror — those writes would pass through to the
// real ~/.claude.
func Mounted(dir string) bool {
	var ds, ps syscall.Stat_t
	if syscall.Lstat(dir, &ds) != nil {
		return false
	}
	if syscall.Lstat(filepath.Dir(dir), &ps) != nil {
		return false
	}
	return ds.Dev != ps.Dev
}
