//go:build fuse && cgo && darwin

package overlay

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/winfsp/cgofuse/fuse"
)

// mirrorFS is a passthrough filesystem: most operations are applied directly to
// the corresponding path under root (~/.claude), so reads and writes are shared
// with plain `claude`. There is NO copy-up. The exception is the account-local
// PrivateEntry names (daemon/, ide/, backups/, .claude.json and its atomic-write
// temp files): their whole subtrees are redirected to a private per-account
// backing dir so concurrent sessions don't fight over one supervisor/IDE
// registry and per-account state never lands in the shared base — matching the
// symlink provider's invariant.
type mirrorFS struct {
	fuse.FileSystemBase
	root        string // ~/.claude
	privateRoot string // per-account backing for ExcludedEntries
}

func newMirrorFS(root, privateRoot string) *mirrorFS {
	absRoot, _ := filepath.Abs(root)
	absPriv, _ := filepath.Abs(privateRoot)
	return &mirrorFS{root: absRoot, privateRoot: absPriv}
}

// real maps a fuse path ("/foo/bar") to its backing path: under privateRoot for
// a private top-level component, else under root.
func (fs *mirrorFS) real(path string) string {
	rel := filepath.FromSlash(path)
	if PrivateEntry(topComponent(path)) {
		return filepath.Join(fs.privateRoot, rel)
	}
	return filepath.Join(fs.root, rel)
}

// topComponent returns the first path component of a fuse path ("/daemon/x" -> "daemon").
func topComponent(path string) string {
	p := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	return p
}

func errno(err error) int {
	if err == nil {
		return 0
	}
	var e syscall.Errno
	if errors.As(err, &e) {
		return -int(e)
	}
	return -int(syscall.EIO)
}

func (fs *mirrorFS) Statfs(path string, stat *fuse.Statfs_t) int {
	var s syscall.Statfs_t
	if err := syscall.Statfs(fs.real(path), &s); err != nil {
		return errno(err)
	}
	stat.Bsize = uint64(s.Bsize)
	stat.Frsize = uint64(s.Bsize)
	stat.Blocks = s.Blocks
	stat.Bfree = s.Bfree
	stat.Bavail = s.Bavail
	stat.Files = s.Files
	stat.Ffree = s.Ffree
	stat.Namemax = 255
	return 0
}

func (fs *mirrorFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	var st syscall.Stat_t
	var err error
	if fh != ^uint64(0) {
		err = syscall.Fstat(int(fh), &st)
	} else {
		err = syscall.Lstat(fs.real(path), &st)
	}
	if err != nil {
		return errno(err)
	}
	copyStat(stat, &st)
	return 0
}

func (fs *mirrorFS) Open(path string, flags int) (int, uint64) {
	fd, err := syscall.Open(fs.real(path), flags, 0)
	if err != nil {
		return errno(err), ^uint64(0)
	}
	return 0, uint64(fd)
}

func (fs *mirrorFS) Create(path string, flags int, mode uint32) (int, uint64) {
	fd, err := syscall.Open(fs.real(path), flags|syscall.O_CREAT, mode)
	if err != nil {
		return errno(err), ^uint64(0)
	}
	return 0, uint64(fd)
}

func (fs *mirrorFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	n, err := syscall.Pread(int(fh), buff, ofst)
	if err != nil {
		return errno(err)
	}
	return n
}

func (fs *mirrorFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	n, err := syscall.Pwrite(int(fh), buff, ofst)
	if err != nil {
		return errno(err)
	}
	return n
}

func (fs *mirrorFS) Truncate(path string, size int64, fh uint64) int {
	var err error
	if fh != ^uint64(0) {
		err = syscall.Ftruncate(int(fh), size)
	} else {
		err = syscall.Truncate(fs.real(path), size)
	}
	return errno(err)
}

func (fs *mirrorFS) Fsync(path string, datasync bool, fh uint64) int {
	return errno(syscall.Fsync(int(fh)))
}

func (fs *mirrorFS) Release(path string, fh uint64) int {
	return errno(syscall.Close(int(fh)))
}

func (fs *mirrorFS) Opendir(path string) (int, uint64) {
	fd, err := syscall.Open(fs.real(path), syscall.O_RDONLY, 0)
	if err != nil {
		return errno(err), ^uint64(0)
	}
	return 0, uint64(fd)
}

func (fs *mirrorFS) Readdir(path string, fill func(name string, stat *fuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	dir, err := os.Open(fs.real(path))
	if err != nil {
		return errno(err)
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return errno(err)
	}
	fill(".", nil, 0)
	fill("..", nil, 0)
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
		if !fill(name, nil, 0) {
			return 0
		}
	}
	if path == "/" {
		// Private files (e.g. a seeded .claude.json) live only in privateRoot;
		// merge them into the root listing or they'd be stat-able but unlisted.
		priv, err := os.ReadDir(fs.privateRoot)
		if err != nil {
			return 0 // listing base succeeded; a missing private root is not an error
		}
		for _, e := range priv {
			if seen[e.Name()] || !PrivateEntry(e.Name()) {
				continue
			}
			if !fill(e.Name(), nil, 0) {
				return 0
			}
		}
	}
	return 0
}

func (fs *mirrorFS) Releasedir(path string, fh uint64) int {
	return errno(syscall.Close(int(fh)))
}

func (fs *mirrorFS) Mkdir(path string, mode uint32) int {
	return errno(syscall.Mkdir(fs.real(path), mode))
}

func (fs *mirrorFS) Unlink(path string) int {
	return errno(syscall.Unlink(fs.real(path)))
}

func (fs *mirrorFS) Rmdir(path string) int {
	return errno(syscall.Rmdir(fs.real(path)))
}

func (fs *mirrorFS) Link(oldpath string, newpath string) int {
	return errno(syscall.Link(fs.real(oldpath), fs.real(newpath)))
}

func (fs *mirrorFS) Symlink(target string, newpath string) int {
	return errno(syscall.Symlink(target, fs.real(newpath)))
}

func (fs *mirrorFS) Readlink(path string) (int, string) {
	buf := make([]byte, 4096)
	n, err := syscall.Readlink(fs.real(path), buf)
	if err != nil {
		return errno(err), ""
	}
	return 0, string(buf[:n])
}

func (fs *mirrorFS) Rename(oldpath string, newpath string) int {
	return errno(syscall.Rename(fs.real(oldpath), fs.real(newpath)))
}

func (fs *mirrorFS) Chmod(path string, mode uint32) int {
	return errno(syscall.Chmod(fs.real(path), mode))
}

func (fs *mirrorFS) Chown(path string, uid uint32, gid uint32) int {
	return errno(syscall.Lchown(fs.real(path), int(uid), int(gid)))
}

func (fs *mirrorFS) Utimens(path string, tmsp []fuse.Timespec) int {
	if len(tmsp) < 2 {
		return errno(syscall.EINVAL)
	}
	tv := []syscall.Timeval{
		{Sec: tmsp[0].Sec, Usec: int32(tmsp[0].Nsec / 1000)},
		{Sec: tmsp[1].Sec, Usec: int32(tmsp[1].Nsec / 1000)},
	}
	return errno(syscall.Utimes(fs.real(path), tv))
}

// copyStat converts a Go syscall.Stat_t (darwin) into a fuse.Stat_t.
func copyStat(dst *fuse.Stat_t, src *syscall.Stat_t) {
	dst.Dev = uint64(src.Dev)
	dst.Ino = uint64(src.Ino)
	dst.Mode = uint32(src.Mode)
	dst.Nlink = uint32(src.Nlink)
	dst.Uid = src.Uid
	dst.Gid = src.Gid
	dst.Rdev = uint64(src.Rdev)
	dst.Size = src.Size
	dst.Atim = fuse.Timespec{Sec: src.Atimespec.Sec, Nsec: src.Atimespec.Nsec}
	dst.Mtim = fuse.Timespec{Sec: src.Mtimespec.Sec, Nsec: src.Mtimespec.Nsec}
	dst.Ctim = fuse.Timespec{Sec: src.Ctimespec.Sec, Nsec: src.Ctimespec.Nsec}
	dst.Birthtim = fuse.Timespec{Sec: src.Birthtimespec.Sec, Nsec: src.Birthtimespec.Nsec}
	dst.Blksize = int64(src.Blksize)
	dst.Blocks = src.Blocks
	dst.Flags = src.Flags
}
