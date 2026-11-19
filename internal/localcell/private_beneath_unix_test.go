//go:build linux || darwin

package localcell

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// beneathTree makes a row's held directory, holding in/zzfrom, and one outside it holding zzfrom; the row closes the root.
func beneathTree(t *testing.T) (held, out string, root *os.Root) {
	held, out = filepath.Join(t.TempDir(), "held"), filepath.Join(t.TempDir(), "out")
	err := errors.Join(os.MkdirAll(filepath.Join(held, "in"), 0o700), os.Mkdir(out, 0o700), os.WriteFile(filepath.Join(held, "in", "zzfrom"), []byte("inside"), 0o600), os.WriteFile(filepath.Join(out, "zzfrom"), []byte("outside"), 0o600))
	root, openErr := os.OpenRoot(held)
	if err != nil || openErr != nil {
		t.Fatal("cannot make the private beneath row tree")
	}
	t.Cleanup(func() { _ = root.Close() })
	return held, out, root
}

// beneathSnapshot is every name below dirs with its mode, inode, size (not a directory's) and a regular file's bytes.
func beneathSnapshot(t *testing.T, dirs ...string) map[string]string {
	snapshot := map[string]string{}
	for _, dir := range dirs {
		if filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
			info, statErr := os.Lstat(path)
			var data []byte
			if err = errors.Join(err, statErr); err == nil && info.Mode().IsRegular() {
				data, err = os.ReadFile(path)
			}
			if err == nil {
				snapshot[path] = fmt.Sprint(info.Mode(), info.Sys().(*syscall.Stat_t).Ino, map[bool]int64{false: info.Size()}[info.IsDir()], string(data))
			}
			return err
		}) != nil {
			t.Fatal("cannot snapshot the private beneath row tree")
		}
	}
	return snapshot
}

// beneathPlant puts kind at path: a link (to in, out, a file in it, dev on another device, or nothing), file, FIFO or directory.
func beneathPlant(t *testing.T, path, out, kind string) {
	var tree, devices syscall.Stat_t
	if kind == "link_volume" && (errors.Join(syscall.Stat(out, &tree), syscall.Stat("/dev", &devices)) != nil || tree.Dev == devices.Dev) {
		t.Fatal("the dev directory is not on another device than the row tree")
	}
	link := func(target string) func() error { return func() error { return os.Symlink(target, path) } }
	if plant, known := map[string]func() error{"absent": func() error { return nil }, "link_inside": link("in"), "link_outside": link(out), "link_dir": link(out), "link_file": link(filepath.Join(out, "zzfrom")),
		"link_volume": link("/dev"), "dangling": link(filepath.Join(out, "zzabsent")), "file": func() error { return os.WriteFile(path, []byte("planted"), 0o600) },
		"fifo": func() error { return unix.Mkfifo(path, 0o600) }, "directory": func() error { return os.Mkdir(path, 0o700) }}[kind]; !known || plant() != nil {
		t.Fatal("cannot plant the private beneath row kind")
	}
}

// beneathOps runs each operation on name, or in the directory dir, with want, marking called when a given function runs.
func beneathOps(root *os.Root, name, dir string, called *bool, want privateBeneathWant) map[string]func() error {
	at, synced, reached := func(int, string) error { *called = true; return nil }, func(int) error { *called = true; return nil }, func(int, privateBeneathID) error { *called = true; return nil }
	return map[string]func() error{"at": func() error { return privateBeneathAt(root, name, want, at) },
		"mkdir":     func() error { _, err := privateBeneathMkdir(root, name, want, synced); return err },
		"create":    func() error { _, _, err := privateBeneathCreateTemp(root, dir, want, "zzlabel"); return err },
		"publish":   func() error { return privateBeneathPublish(root, dir, "zzfrom", "zzleaf", want) },
		"replace":   func() error { return privateBeneathReplace(root, dir, "zzfrom", "zzleaf", want) },
		"lstat":     func() error { _, err := privateBeneathLstat(root, name, want); return err },
		"open":      func() error { _, err := privateBeneathOpen(root, name, want, "zzlabel"); return err },
		"directory": func() error { return privateBeneathDirectory(root, name, want, reached) },
		"list":      func() error { _, err := privateBeneathList(root, name, want); return err },
		"remove":    func() error { return privateBeneathRemove(root, name, want) }}
}

// Every operation refuses an intermediate component that is not a directory on the held device, naming nothing and
// changing nothing; a link at the last component is refused, or removed as itself by remove, its target untouched.
func TestPrivateBeneathRefuses(t *testing.T) {
	for _, kind := range []string{"absent", "link_inside", "link_outside", "link_volume", "dangling", "file", "fifo"} {
		for op := range beneathOps(nil, "", "", nil, nil) {
			t.Run(op+"/"+kind, func(t *testing.T) {
				held, out, root := beneathTree(t)
				beneathPlant(t, filepath.Join(held, "zzmid"), out, kind)
				called, before := false, beneathSnapshot(t, held, out)
				err := beneathOps(root, "zzmid/zzleaf", "zzmid", &called, nil)[op]()
				if err == nil || strings.Contains(err.Error(), "zz") || strings.Contains(err.Error(), held) || strings.Contains(err.Error(), out) || called || !maps.Equal(before, beneathSnapshot(t, held, out)) {
					t.Fatal("an operation through a planted component did not refuse with a nameless error, or ran its function or changed a name")
				}
				if errors.Is(err, fs.ErrNotExist) != (kind == "absent") || errors.Is(err, unix.EXDEV) {
					t.Fatal("an operation through a planted component gave not-exist for another kind, or followed it to another device")
				}
			})
		}
	}
	for _, op := range []string{"mkdir", "create", "open", "directory", "remove"} {
		t.Run(op+"/link_last", func(t *testing.T) {
			held, out, root := beneathTree(t)
			saved, last := privateBeneathTempSuffix, map[bool]string{true: ".pending-7", false: "zzlast"}[op == "create"]
			privateBeneathTempSuffix = func() string { return "7" }
			t.Cleanup(func() { privateBeneathTempSuffix = saved })
			beneathPlant(t, filepath.Join(held, "in", last), out, "link_outside")
			called, before := false, beneathSnapshot(t, out)
			err := beneathOps(root, "in/"+last, "in", &called, nil)[op]()
			if _, linkErr := os.Lstat(filepath.Join(held, "in", last)); called || !maps.Equal(before, beneathSnapshot(t, out)) || op == "remove" && (err != nil || !errors.Is(linkErr, fs.ErrNotExist)) ||
				op != "remove" && (err == nil || linkErr != nil || strings.Contains(err.Error(), last)) || (op == "mkdir" || op == "create") && !errors.Is(err, fs.ErrExist) || op == "remove" && privateBeneathRemove(root, "in/"+last, nil) != nil {
				t.Fatal("an operation on a link at the last component reached its target, or neither refused it (exist for a creation) nor removed it itself")
			}
		})
	}
}

// A recorded directory replaced at its name refuses the next operation through it as moved; the replacement's passes.
func TestPrivateBeneathIdentity(t *testing.T) {
	held, out, root := beneathTree(t)
	recorded, err := privateBeneathMkdir(root, "in/zzdir", nil, privateBeneathSync)
	if err != nil || os.Rename(filepath.Join(held, "in", "zzdir"), filepath.Join(held, "in", "zzaside")) != nil {
		t.Fatal("cannot record and move aside the identity row directory")
	}
	replaced, err := privateBeneathMkdir(root, "in/zzdir", nil, privateBeneathSync)
	want := func(id privateBeneathID) privateBeneathWant {
		return func(p string) (privateBeneathID, bool) { return id, p == "in/zzdir" }
	}
	before, called := beneathSnapshot(t, held, out), false
	for _, run := range beneathOps(root, "in/zzdir/zzleaf", "in/zzdir", &called, want(recorded)) {
		if err := run(); err != errPrivateBeneathMoved || called {
			t.Fatal("an operation through a replaced recorded directory was not refused as moved")
		}
	}
	directoryErr := privateBeneathDirectory(root, "in/zzdir", want(recorded), func(int, privateBeneathID) error { return nil })
	if err != nil || directoryErr != errPrivateBeneathMoved || !maps.Equal(before, beneathSnapshot(t, held, out)) {
		t.Fatal("the replaced recorded directory itself was not refused as moved, or a directory was written")
	}
	if _, err := privateBeneathMkdir(root, "in/zzdir/zzsub", want(replaced), privateBeneathSync); err != nil || privateBeneathDirectory(root, "in/zzdir", want(replaced), func(int, privateBeneathID) error { return nil }) != nil || privateBeneathRemove(root, "in/zzdir/zzsub", want(replaced)) != nil {
		t.Fatal("an operation through a directory with its recorded identity was refused")
	}
}

// SPEC1 report 3.3's residual: fn moves its parent away and creates through it there; the next operation refuses.
func TestPrivateBeneathMovedInFlight(t *testing.T) {
	held, out, root := beneathTree(t)
	err := privateBeneathAt(root, "in/zznew", nil, func(dir int, last string) error {
		return errors.Join(os.Rename(filepath.Join(held, "in"), filepath.Join(out, "zzmoved")), unix.Mkdirat(dir, last, 0o700))
	})
	_, landed := os.Lstat(filepath.Join(out, "zzmoved", "zznew"))
	if _, again := privateBeneathLstat(root, "in/zznew", nil); err != nil || landed != nil || again == nil {
		t.Fatal("a creation through a moved parent did not land in it, or its name still resolved")
	}
}

// Holding the system root read-only, a walk through dev, an open of it and a directory at it refuse with EXDEV.
func TestPrivateBeneathDevice(t *testing.T) {
	var system, devices syscall.Stat_t
	root, err := os.OpenRoot("/")
	if err = errors.Join(err, syscall.Stat("/", &system), syscall.Stat("/dev", &devices)); err != nil || system.Dev == devices.Dev {
		t.Fatal("the system root and its dev directory are not held apart on two devices, so this row cannot run")
	}
	defer func() { _ = root.Close() }()
	called := false
	walkErr := privateBeneathAt(root, "dev/zz", nil, func(int, string) error { called = true; return nil })
	opened, openErr := privateBeneathOpen(root, "dev", nil, "zzlabel")
	if directoryErr := beneathOps(root, "dev", "", &called, nil)["directory"](); !errors.Is(walkErr, unix.EXDEV) || !errors.Is(openErr, unix.EXDEV) || opened != nil || !errors.Is(directoryErr, unix.EXDEV) || called {
		t.Fatal("a walk, open or directory from the system root crossed into the dev directory's device")
	}
}

// The removal refuses a directory on another device (by the seam) and removes links as themselves, never their targets.
func TestPrivateBeneathRemoveBelow(t *testing.T) {
	for _, row := range []string{"device", "links"} {
		t.Run(row, func(t *testing.T) {
			held, out, root := beneathTree(t)
			box := filepath.Join(held, "in")
			err := errors.Join(os.MkdirAll(filepath.Join(box, "zzsub", "zzdeep"), 0o700), os.WriteFile(filepath.Join(box, "zzsub", "zzdeep", "zzkept"), nil, 0o600), os.Symlink(out, filepath.Join(box, "zzlinkdir")), os.Symlink(filepath.Join(out, "zzfrom"), filepath.Join(box, "zzlinkfile")), unix.Mkfifo(filepath.Join(box, "zzfifo"), 0o600))
			if saved := privateBeneathSameDevice; row == "device" {
				privateBeneathSameDevice = func(uint64, uint64) bool { return false }
				t.Cleanup(func() { privateBeneathSameDevice = saved })
			}
			outside := beneathSnapshot(t, out)
			removed := privateBeneathDirectory(root, "in", nil, func(fd int, _ privateBeneathID) error { return privateBeneathRemoveBelow(fd) })
			entries, listErr := os.ReadDir(box)
			if _, deepErr := os.Lstat(filepath.Join(box, "zzsub", "zzdeep", "zzkept")); row == "device" && (!errors.Is(removed, unix.EXDEV) || deepErr != nil) {
				t.Fatal("the removal below a directory descended a directory on another device")
			}
			if err != nil || listErr != nil || !maps.Equal(outside, beneathSnapshot(t, out)) || row == "links" && (removed != nil || len(entries) != 0) {
				t.Fatal("the removal below a directory reached a link target or left an entry")
			}
		})
	}
}

// Publication refuses anything at the target with EEXIST; replace replaces a link there, never its target.
func TestPrivateBeneathPublish(t *testing.T) {
	for _, kind := range []string{"file", "link_file", "link_dir", "dangling", "directory", "fifo", "fresh", "replace_link"} {
		t.Run(kind, func(t *testing.T) {
			held, out, root := beneathTree(t)
			target, publish := filepath.Join(held, "in", "zzleaf"), map[bool]func(*os.Root, string, string, string, privateBeneathWant) error{
				true: privateBeneathReplace, false: privateBeneathPublish}[kind == "replace_link"]
			beneathPlant(t, target, out, strings.NewReplacer("fresh", "absent", "replace_link", "link_file").Replace(kind))
			before, outside := beneathSnapshot(t, held), beneathSnapshot(t, out)
			if err := publish(root, "in", "zzfrom", "zzleaf", nil); kind == "fresh" || kind == "replace_link" {
				if data, readErr := os.ReadFile(target); err != nil || readErr != nil || string(data) != "inside" || !maps.Equal(outside, beneathSnapshot(t, out)) {
					t.Fatal("a publication to a free or replaced target did not publish the temporary, or reached outside")
				}
			} else if !errors.Is(err, unix.EEXIST) || !maps.Equal(before, beneathSnapshot(t, held)) || !maps.Equal(outside, beneathSnapshot(t, out)) {
				t.Fatal("a publication onto an existing object was not refused with both intact")
			}
		})
	}
}

// The temporary creation passes over existing names (FIFO, link, file), each intact, to a fresh regular 0600 file.
func TestPrivateBeneathCreateTemp(t *testing.T) {
	held, out, root := beneathTree(t)
	in, suffixes, saved := filepath.Join(held, "in"), []string{"1", "2", "3", "4"}, privateBeneathTempSuffix
	for index, kind := range []string{"fifo", "link_file", "file"} {
		beneathPlant(t, filepath.Join(in, ".pending-"+suffixes[index]), out, kind)
	}
	privateBeneathTempSuffix = func() (suffix string) { suffix, suffixes = suffixes[0], suffixes[1:]; return suffix }
	t.Cleanup(func() { privateBeneathTempSuffix = saved })
	before := beneathSnapshot(t, held, out)
	created, name, err := privateBeneathCreateTemp(root, "in", nil, "zzlabel")
	if err != nil || name != ".pending-4" || !unixPrivateTempName(name) || created.Name() != "zzlabel" {
		t.Fatal("the temporary creation did not pass over the planted names to a fresh one")
	}
	info, statErr := created.Stat()
	after := beneathSnapshot(t, held, out)
	if delete(after, filepath.Join(in, name)); created.Close() != nil || statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !maps.Equal(before, after) {
		t.Fatal("the temporary is not a private regular file, or a planted name changed")
	}
}

// The read open returns a FIFO at once (watchdog-bounded); a Linux write lease is refused with EWOULDBLOCK, never waited.
func TestPrivateBeneathOpen(t *testing.T) {
	t.Run("fifo", func(t *testing.T) {
		held, out, root := beneathTree(t)
		fifo := filepath.Join(held, "in", "zzfifo")
		beneathPlant(t, fifo, out, "fifo")
		var opened *os.File
		writer := func() { w, _ := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); _ = w.Close() }
		err := readerWatchdog(t, func() (err error) { opened, err = privateBeneathOpen(root, "in/zzfifo", nil, "zzlabel"); return err }, writer)
		if info, statErr := opened.Stat(); err != nil || opened.Name() != "zzlabel" || opened.Close() != nil || statErr != nil || info.Mode().IsRegular() {
			t.Fatal("the read open refused a FIFO, or its Stat does not say it is not regular")
		}
	})
	if runtime.GOOS == "linux" {
		t.Run("lease", func(t *testing.T) {
			const setLease, getLease = 0x400, 0x401 // Linux's F_SETLEASE and F_GETLEASE (linux/fcntl.h), spelled out for darwin
			held, _, root := beneathTree(t)
			holder, err := os.Open(filepath.Join(held, "in", "zzfrom"))
			if err == nil {
				defer func() { _ = holder.Close() }()
				_, err = unix.FcntlInt(holder.Fd(), setLease, unix.F_WRLCK)
			}
			if err != nil {
				t.Fatal("this kernel or sandbox refused the lease the row needs")
			}
			opened, err := privateBeneathOpen(root, "in/zzfrom", nil, "zzlabel")
			lease, leaseErr := unix.FcntlInt(holder.Fd(), getLease, 0)
			if _, _ = unix.FcntlInt(holder.Fd(), setLease, unix.F_UNLCK); !errors.Is(err, syscall.EWOULDBLOCK) || opened != nil || leaseErr != nil || lease == unix.F_WRLCK {
				t.Fatal("the read open waited on a lease or opened a leased file")
			}
		})
	}
}

// The listing types each entry without following it.
func TestPrivateBeneathList(t *testing.T) {
	held, out, root := beneathTree(t)
	for _, kind := range []string{"directory", "link_dir", "fifo"} {
		beneathPlant(t, filepath.Join(held, "in", "zz"+kind), out, kind)
	}
	entries, err := privateBeneathList(root, "in", nil)
	info, lstatErr := privateBeneathLstat(root, "in/zzlink_dir", nil)
	kinds, ids := map[string][2]bool{}, map[string]privateBeneathID{}
	for _, entry := range entries {
		kinds[entry.name], ids[entry.name] = [2]bool{entry.directory, entry.regular}, entry.id
	}
	if err != nil || len(entries) != 4 || !maps.Equal(kinds, map[string][2]bool{"zzfrom": {false, true}, "zzdirectory": {true, false}, "zzlink_dir": {}, "zzfifo": {}}) ||
		lstatErr != nil || info.Name() != "zzlink_dir" || info.Mode().Type() != fs.ModeSymlink || ids["zzlink_dir"] != privateBeneathIdentity(info.Sys().(*unix.Stat_t)) {
		t.Fatal("the listing did not type and identify its four entries without following them, or the no-follow stat disagrees")
	}
}

// A reached directory syncs through (*os.File).Sync on a duplicate.
func TestPrivateBeneathSync(t *testing.T) {
	if _, _, root := beneathTree(t); privateBeneathDirectory(root, "in", nil, func(fd int, _ privateBeneathID) error { return privateBeneathSync(fd) }) != nil {
		t.Fatal("a reached directory could not be synced")
	}
}

// A umask clearing an owner bit makes the new directory not private: refused, kept and reported. Not parallel (umask).
func TestPrivateBeneathMkdirMode(t *testing.T) {
	held, _, root := beneathTree(t)
	synced, saved := false, syscall.Umask(0o100)
	id, err := privateBeneathMkdir(root, "zzdir", nil, func(int) error { synced = true; return nil })
	syscall.Umask(saved)
	var stat syscall.Stat_t
	if err != errLocalDirectoryPrivate || synced || syscall.Lstat(filepath.Join(held, "zzdir"), &stat) != nil || id != (privateBeneathID{uint64(stat.Dev), stat.Ino}) {
		t.Fatal("a directory made without its owner bits was not refused and reported")
	}
}

// Malformed names, from and to are EINVAL before any system call: the root is closed, so a call would give EBADF.
func TestPrivateBeneathNames(t *testing.T) {
	_, _, root := beneathTree(t)
	called := root.Close() != nil
	for _, name := range []string{"", ".", "..", "a//b", "a/./b", "/a"} {
		for _, run := range beneathOps(root, name, name, &called, nil) {
			if err := run(); !errors.Is(err, unix.EINVAL) || called {
				t.Fatal("an operation did not refuse a malformed name before any system call")
			}
		}
	}
	for _, pair := range [][2]string{{"a/b", "zzleaf"}, {"zzfrom", "a/b"}, {"zzfrom", ".."}, {"", "zzleaf"}, {"zzfrom", "."}} {
		if !errors.Is(privateBeneathPublish(root, "in", pair[0], pair[1], nil), unix.EINVAL) || !errors.Is(privateBeneathReplace(root, "in", pair[0], pair[1], nil), unix.EINVAL) || !errors.Is(privateBeneathRemove(root, "zzmid/zzleaf", nil), unix.EBADF) {
			t.Fatal("a publication did not refuse a from or to that is not one name, or a valid name did not reach the closed root")
		}
	}
}

// macOS answers a link at an intermediate component ENOTDIR (LC-CKPT-WALK-1-SPEC1's macOS probe); never not-exist.
func TestPrivateBeneathDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Run("errno_at_link", func(t *testing.T) {
			held, out, root := beneathTree(t)
			beneathPlant(t, filepath.Join(held, "zzmid"), out, "link_outside")
			err := privateBeneathAt(root, "zzmid/zzleaf", nil, func(int, string) error { return nil })
			if t.Logf("errno_at_link class=%s", map[error]string{unix.ELOOP: "eloop", unix.ENOTDIR: "enotdir"}[err]); err == nil || errors.Is(err, fs.ErrNotExist) {
				t.Fatal("a link at an intermediate component was not refused, or refused as not-exist")
			}
		})
	}
}
