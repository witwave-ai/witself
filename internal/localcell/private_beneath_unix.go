//go:build linux || darwin

package localcell

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// privateBeneathID is a directory's identity: its device and inode.
type privateBeneathID struct{ dev, ino uint64 }

// privateBeneathWant returns the identity recorded for a slash-joined prefix below the held root, if any; nil requires none.
type privateBeneathWant func(prefix string) (privateBeneathID, bool)

// privateBeneathEntry is one listed name, typed without following it.
type privateBeneathEntry struct {
	name               string
	directory, regular bool
	id                 privateBeneathID
}

// errPrivateBeneathMoved refuses a directory that is not the one recorded at its name.
var errPrivateBeneathMoved = errors.New("local cell directory is not the one recorded at its name")

// privateBeneathTempSuffix (a temporary's suffix, as os.CreateTemp draws it) and privateBeneathSameDevice: test seams.
var (
	privateBeneathTempSuffix = func() string { return strconv.FormatUint(uint64(rand.Uint32()), 10) }
	privateBeneathSameDevice = func(parent, child uint64) bool { return parent == child }
)

// privateBeneathIdentity converts stat's device and inode (Dev is int32 on darwin).
func privateBeneathIdentity(stat *unix.Stat_t) privateBeneathID {
	return privateBeneathID{uint64(stat.Dev), stat.Ino}
}

// moved reports whether want records an identity for prefix that stat is not.
func (want privateBeneathWant) moved(prefix string, stat *unix.Stat_t) bool {
	if want == nil {
		return false
	}
	id, ok := want(prefix)
	return ok && id != privateBeneathIdentity(stat)
}

// privateBeneathPart reports whether part is one name: not empty, "." or "..", no slash.
func privateBeneathPart(part string) bool {
	return part != "" && part != "." && part != ".." && !strings.Contains(part, "/")
}

// privateBeneathAt runs fn in the directory holding name's last component, reached from the held root's own descriptor
// one component at a time: each opened as a directory without following a link, on the held directory's device and,
// where want records one, exactly that directory. A name with an empty, "." or ".." component is refused before any
// system call; every descriptor it opened (fn must neither close nor keep dir) is closed; no error carries a name.
func privateBeneathAt(root *os.Root, name string, want privateBeneathWant, fn func(dir int, last string) error) error {
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if !privateBeneathPart(part) {
			return unix.EINVAL
		}
	}
	held, err := root.Open(".")
	if errno := unix.EBADF; err != nil {
		_ = errors.As(err, &errno) // the system error alone; EBADF for a closed root
		return errno
	}
	defer func() { _ = held.Close() }()
	var device, stat unix.Stat_t
	dir := int(held.Fd())
	if err := unix.Fstat(dir, &device); err != nil {
		return err
	}
	for index, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(dir, part, unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer func() { _ = unix.Close(next) }()
		if err := unix.Fstat(next, &stat); err != nil {
			return err
		}
		if stat.Dev != device.Dev {
			return unix.EXDEV
		}
		if want.moved(strings.Join(parts[:index+1], "/"), &stat) {
			return errPrivateBeneathMoved
		}
		dir = next
	}
	return fn(dir, parts[len(parts)-1])
}

// privateBeneathOn is the stat of fd, opened in dir, required on dir's device.
func privateBeneathOn(dir, fd int) (stat unix.Stat_t, err error) {
	var parent unix.Stat_t
	if err = unix.Fstat(dir, &parent); err == nil {
		err = unix.Fstat(fd, &stat)
	}
	if err == nil && stat.Dev != parent.Dev {
		err = unix.EXDEV
	}
	return stat, err
}

// privateBeneathOpened opens the directory last in dir without following it, on dir's device, and runs fn with it.
func privateBeneathOpened(dir int, last string, fn func(fd int, stat *unix.Stat_t) error) error {
	fd, err := unix.Openat(dir, last, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	stat, err := privateBeneathOn(dir, fd)
	if err != nil {
		return err
	}
	return fn(fd, &stat)
}

// privateBeneathMkdir makes name a directory on its parent's device, owned by the effective user with permission bits
// 0700 (as privateOwnedDir), then has syncParent sync the parent; its identity is returned, a privacy refusal's too.
func privateBeneathMkdir(root *os.Root, name string, want privateBeneathWant, syncParent func(fd int) error) (id privateBeneathID, err error) {
	err = privateBeneathAt(root, name, want, func(dir int, last string) error {
		if err := unix.Mkdirat(dir, last, 0o700); err != nil {
			return err
		}
		return privateBeneathOpened(dir, last, func(_ int, stat *unix.Stat_t) error {
			if id = privateBeneathIdentity(stat); stat.Uid != uint32(unix.Geteuid()) || stat.Mode&0o777 != 0o700 {
				return errLocalDirectoryPrivate
			}
			return syncParent(dir)
		})
	})
	return id, err
}

// privateBeneathCreateTemp makes an exclusive 0600 ".pending-<decimal>" file in dir, never through a link, with
// os.CreateTemp's names and 10,000 tries; it returns the file, labelled label (never resolved), and its name.
func privateBeneathCreateTemp(root *os.Root, dir string, want privateBeneathWant, label string) (created *os.File, name string, err error) {
	if err = privateBeneathAt(root, dir+"/.pending-", want, func(parent int, _ string) error {
		for try := 0; try < 10000; try++ {
			name = ".pending-" + privateBeneathTempSuffix()
			fd, err := unix.Openat(parent, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
			if err == nil {
				created = os.NewFile(uintptr(fd), label)
			}
			if !errors.Is(err, unix.EEXIST) {
				return err
			}
		}
		return fs.ErrExist
	}); err != nil {
		name = ""
	}
	return created, name, err
}

// privateBeneathPublish renames from to to, single names in dir, after a no-follow fstatat finds nothing at to (else EEXIST).
func privateBeneathPublish(root *os.Root, dir, from, to string, want privateBeneathWant) error {
	if !privateBeneathPart(from) || !privateBeneathPart(to) {
		return unix.EINVAL
	}
	return privateBeneathAt(root, dir+"/"+to, want, func(parent int, _ string) error {
		var stat unix.Stat_t
		if err := unix.Fstatat(parent, to, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			return unix.EEXIST
		} else if !errors.Is(err, unix.ENOENT) {
			return err
		}
		return unix.Renameat(parent, from, parent, to)
	})
}

// privateBeneathReplace renames from to to, single names, in the one directory dir names, replacing what stands at to.
func privateBeneathReplace(root *os.Root, dir, from, to string, want privateBeneathWant) error {
	if !privateBeneathPart(from) || !privateBeneathPart(to) {
		return unix.EINVAL
	}
	return privateBeneathAt(root, dir+"/"+to, want, func(parent int, _ string) error { return unix.Renameat(parent, from, parent, to) })
}

// privateBeneathLstat is a no-follow fstatat of name's last component.
func privateBeneathLstat(root *os.Root, name string, want privateBeneathWant) (fs.FileInfo, error) {
	info := &privateBeneathInfo{}
	if err := privateBeneathAt(root, name, want, func(dir int, last string) error {
		info.name = last
		return unix.Fstatat(dir, last, &info.stat, unix.AT_SYMLINK_NOFOLLOW)
	}); err != nil {
		return nil, err
	}
	return info, nil
}

// privateBeneathOpen opens name read-only, on its parent's device, never following a link, waiting or taking a terminal;
// the caller decides its type by Stat before any read, and a lease refusal (EWOULDBLOCK) is returned as it comes.
func privateBeneathOpen(root *os.Root, name string, want privateBeneathWant, label string) (opened *os.File, err error) {
	err = privateBeneathAt(root, name, want, func(dir int, last string) error {
		fd, err := unix.Openat(dir, last, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		if err == nil {
			if _, err = privateBeneathOn(dir, fd); err != nil {
				_ = unix.Close(fd)
			} else {
				opened = os.NewFile(uintptr(fd), label)
			}
		}
		return err
	})
	return opened, err
}

// privateBeneathDirectory runs fn with name itself, opened as the walk opens each component (want(name) too), and its identity.
func privateBeneathDirectory(root *os.Root, name string, want privateBeneathWant, fn func(fd int, id privateBeneathID) error) error {
	return privateBeneathAt(root, name, want, func(dir int, last string) error {
		return privateBeneathOpened(dir, last, func(fd int, stat *unix.Stat_t) error {
			if want.moved(name, stat) {
				return errPrivateBeneathMoved
			}
			return fn(fd, privateBeneathIdentity(stat))
		})
	})
}

// privateBeneathDup runs use on a duplicate of the directory fd as an os.File.
func privateBeneathDup(fd int, use func(dir *os.File) error) error {
	dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(dup), "local cell directory")
	return errors.Join(use(dir), dir.Close())
}

// privateBeneathSync syncs the directory fd by (*os.File).Sync, never unix.Fsync, so darwin keeps F_FULLFSYNC.
func privateBeneathSync(fd int) error {
	return privateBeneathDup(fd, func(dir *os.File) error { return dir.Sync() })
}

// privateBeneathList lists the directory name, each entry typed by a no-follow fstatat in it.
func privateBeneathList(root *os.Root, name string, want privateBeneathWant) (entries []privateBeneathEntry, err error) {
	if err = privateBeneathDirectory(root, name, want, func(fd int, _ privateBeneathID) error {
		names, stat := []string(nil), unix.Stat_t{}
		err := privateBeneathDup(fd, func(dir *os.File) (err error) { names, err = dir.Readdirnames(-1); return err })
		for index := 0; err == nil && index < len(names); index++ {
			if err = unix.Fstatat(fd, names[index], &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
				kind := stat.Mode & unix.S_IFMT
				entries = append(entries, privateBeneathEntry{names[index], kind == unix.S_IFDIR, kind == unix.S_IFREG, privateBeneathIdentity(&stat)})
			}
		}
		return err
	}); err != nil {
		entries = nil
	}
	return entries, err
}

// privateBeneathRemove removes name as itself, as os.Root.Remove does (unlinkat, then AT_REMOVEDIR); absent is nil.
func privateBeneathRemove(root *os.Root, name string, want privateBeneathWant) error {
	return privateBeneathAt(root, name, want, func(dir int, last string) error {
		if err := unix.Unlinkat(dir, last, 0); err != nil {
			if second := unix.Unlinkat(dir, last, unix.AT_REMOVEDIR); !errors.Is(second, unix.ENOTDIR) {
				err = second
			}
			if !errors.Is(err, unix.ENOENT) {
				return err
			}
		}
		return nil
	})
}

// privateBeneathRemoveBelow removes everything below the directory fd, unread (its listing starts at the offset a dup shares):
// a name not a directory is unlinked as itself; a directory must be on fd's device (privateBeneathSameDevice, else EXDEV),
// nothing in it removed, then opened without following, on fd's device again, emptied and removed. A refusal stops it.
func privateBeneathRemoveBelow(fd int) error {
	var self, stat unix.Stat_t
	if err := unix.Fstat(fd, &self); err != nil {
		return err
	}
	var names []string
	err := privateBeneathDup(fd, func(dir *os.File) (err error) { names, err = dir.Readdirnames(-1); return err })
	for index := 0; err == nil && index < len(names); index++ {
		name := names[index]
		if err = unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil && stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			err = unix.Unlinkat(fd, name, 0)
		} else if err == nil && !privateBeneathSameDevice(uint64(self.Dev), uint64(stat.Dev)) {
			return unix.EXDEV
		} else if err == nil {
			if err = privateBeneathOpened(fd, name, func(child int, _ *unix.Stat_t) error { return privateBeneathRemoveBelow(child) }); err == nil {
				err = unix.Unlinkat(fd, name, unix.AT_REMOVEDIR)
			}
		}
		if errors.Is(err, unix.ENOENT) {
			err = nil
		}
	}
	return err
}

// privateBeneathInfo is a no-follow fstatat's fs.FileInfo: Sys is the *unix.Stat_t; Mode knows dir, file, link and FIFO.
type privateBeneathInfo struct {
	name string
	stat unix.Stat_t
}

func (i *privateBeneathInfo) Name() string       { return i.name }
func (i *privateBeneathInfo) Size() int64        { return i.stat.Size }
func (i *privateBeneathInfo) ModTime() time.Time { return time.Unix(i.stat.Mtim.Unix()) }
func (i *privateBeneathInfo) IsDir() bool        { return i.Mode().IsDir() }
func (i *privateBeneathInfo) Sys() any           { return &i.stat }

func (i *privateBeneathInfo) Mode() fs.FileMode {
	kind, known := map[uint32]fs.FileMode{unix.S_IFDIR: fs.ModeDir, unix.S_IFREG: 0, unix.S_IFLNK: fs.ModeSymlink, unix.S_IFIFO: fs.ModeNamedPipe}[uint32(i.stat.Mode&unix.S_IFMT)]
	return kind | map[bool]fs.FileMode{false: fs.ModeIrregular}[known] | fs.FileMode(i.stat.Mode&0o777)
}
