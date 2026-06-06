package overlay

import (
	"fmt"
	"os"
	"path/filepath"
)

// SymlinkProvider symlinks every top-level entry of base into accountDir,
// except ExcludedEntries (which get private empty dirs) and skipEntries. New
// top-level entries that appear in base later are picked up by Sync.
type SymlinkProvider struct{}

func (p *SymlinkProvider) Kind() Kind { return KindSymlink }

// PrivateRoot is accountDir itself: private files live directly in the
// account dir alongside the symlinks.
func (p *SymlinkProvider) PrivateRoot(accountDir string) string { return accountDir }

// Setup creates accountDir and asserts all links. Idempotent.
func (p *SymlinkProvider) Setup(base, accountDir string) error {
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		return fmt.Errorf("mkdir account dir: %w", err)
	}
	return p.Sync(base, accountDir)
}

// Sync walks base's top-level entries and asserts the correct shape in
// accountDir: a symlink for shared entries, a private dir for excluded ones.
// Like Teardown, it refuses to operate on base itself — overlaying base onto
// itself would replace the user's real entries with self-referential links.
func (p *SymlinkProvider) Sync(base, accountDir string) error {
	if accountDir == base || accountDir == "" {
		return fmt.Errorf("refusing to overlay base dir %q onto itself", accountDir)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return fmt.Errorf("read base dir: %w", err)
	}
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if skipEntries[name] {
			continue
		}
		dst := filepath.Join(accountDir, name)
		if ExcludedEntries[name] {
			if err := assertPrivateDir(dst); err != nil {
				return err
			}
			continue
		}
		if PrivateEntry(name) {
			continue // private file (e.g. .claude.json): never linked into accounts
		}
		if err := assertSymlink(filepath.Join(base, name), dst); err != nil {
			return err
		}
	}
	return nil
}

// Health verifies every shared top-level entry of base is correctly linked in
// accountDir and every excluded entry is a real local dir.
func (p *SymlinkProvider) Health(base, accountDir string) error {
	entries, err := os.ReadDir(base)
	if err != nil {
		return fmt.Errorf("read base dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if skipEntries[name] {
			continue
		}
		dst := filepath.Join(accountDir, name)
		if ExcludedEntries[name] {
			if fi, err := os.Lstat(dst); err != nil || fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("excluded entry %q is missing or a symlink", name)
			}
			continue
		}
		if PrivateEntry(name) {
			continue // private files are account-local; nothing to verify against base
		}
		target, err := os.Readlink(dst)
		if err != nil {
			return fmt.Errorf("entry %q is not a symlink: %w", name, err)
		}
		if target != filepath.Join(base, name) {
			return fmt.Errorf("entry %q links to %q, want %q", name, target, filepath.Join(base, name))
		}
	}
	return nil
}

// Teardown removes the account dir's overlay. Because every shared entry is a
// symlink (removing it never touches base) and excluded entries are this
// account's own private dirs, the whole account dir can be removed. It refuses
// to operate on base as a guard against misuse.
func (p *SymlinkProvider) Teardown(base, accountDir string) error {
	if accountDir == base || accountDir == "" {
		return fmt.Errorf("refusing to tear down base dir %q", accountDir)
	}
	entries, err := os.ReadDir(accountDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		// Only remove symlinks and our excluded private dirs; leave anything
		// unexpected in place so we never destroy real user data by accident.
		full := filepath.Join(accountDir, e.Name())
		fi, err := os.Lstat(full)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 || ExcludedEntries[e.Name()] {
			if err := os.RemoveAll(full); err != nil {
				return err
			}
		}
	}
	return nil
}

// assertSymlink ensures dst is a symlink to target, replacing wrong links.
func assertSymlink(target, dst string) error {
	if cur, err := os.Readlink(dst); err == nil {
		if cur == target {
			return nil // already correct
		}
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("remove stale link %q: %w", dst, err)
		}
	} else if _, statErr := os.Lstat(dst); statErr == nil {
		// dst exists but is not a symlink — do not clobber real data.
		return fmt.Errorf("cannot link %q: a non-symlink already exists there", dst)
	}
	if err := os.Symlink(target, dst); err != nil {
		return fmt.Errorf("symlink %q -> %q: %w", dst, target, err)
	}
	return nil
}

// assertPrivateDir ensures dst is a real (non-symlink) directory.
func assertPrivateDir(dst string) error {
	fi, err := os.Lstat(dst)
	if err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			// Tolerate a concurrent Sync (daemon vs CLI) racing to convert
			// the same stale link: losing the Remove race is success.
			if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
				return err
			}
		} else if fi.IsDir() {
			return nil
		} else {
			return fmt.Errorf("excluded path %q exists as a non-dir", dst)
		}
	}
	return os.MkdirAll(dst, 0o700)
}
