//go:build linux || darwin

package localcell

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// treeAt is what stands at name, never followed: its modes and bytes and a link's target; nil when nothing does.
func treeAt(t *testing.T, name string) any {
	if _, err := os.Lstat(name); err != nil {
		return nil
	}
	target, _ := os.Readlink(name)
	return [2]any{heldTree(t, name), target}
}

// treePlant puts kind at name, the original there moved to the cell's tmp/ (outside the cell for link_out, where a
// leaf without an original links to an outside file); each moved original and link target is kept.
func treePlant(t *testing.T, c *Cell, name, kind string, kept heldTrees) {
	moved := c.path("tmp", "moved")
	if kind == "link_out" {
		moved = filepath.Join(testPrivateRoot(t), "moved")
	}
	if _, err := os.Lstat(name); err == nil {
		heldMove(t, name, moved)
	} else if kind == "link_out" && testWriteFile(moved, []byte("outside target"), 0o600) != nil {
		t.Fatal("cannot prepare the tree row outside target")
	}
	err := map[string]func() error{"dir": func() error { return testMkdir(name, 0o700) },
		"link_cell": func() error { return os.Symlink(moved, name) }, "link_out": func() error { return os.Symlink(moved, name) },
		"dangling": func() error { return os.Symlink(filepath.Join(t.TempDir(), "absent"), name) },
		"file":     func() error { return testWriteFile(name, []byte("planted file"), 0o600) },
		"fifo":     func() error { return syscall.Mkfifo(name, 0o600) }}[kind]()
	if err != nil {
		t.Fatal("cannot plant the tree row object")
	}
	if _, err := os.Lstat(moved); err == nil {
		kept[moved] = heldTree(t, moved)
	}
}

// treeSame reports whether each kept tree still stands, unchanged.
func treeSame(t *testing.T, kept heldTrees) bool {
	for dir, tree := range kept {
		if _, err := os.Lstat(dir); err != nil || !reflect.DeepEqual(tree, heldTree(t, dir)) {
			return false
		}
	}
	return true
}

// A same-user object put at a directory the copy made (its parent synced: syncCheckpointTreeParent), at one it is about
// to sync (syncCheckpointTreeDirectory), at a name it is about to publish (checkpointTest.beforeStep "publish", the
// k-th), at recovery/ (after the checkpoint directory's parent sync), or at the checkpoint after the copy
// (checkpointRecorder.after) is refused before anything is written through or into it: nothing it leads to changes, a
// plant below the checkpoint is as planted when the discard begins, the journal closes, and cleanup is incomplete
// exactly where the plant stands at the checkpoint's name or recovery/, which then holds it as planted; elsewhere the
// call removed its own.
func TestCheckpointTreeCopyRefuses(t *testing.T) {
	dirs, leaves := []string{"dir", "link_cell", "link_out", "dangling", "file", "fifo"}, []string{"dir", "link_out", "dangling", "file", "fifo"}
	for _, row := range []struct {
		stage, name string
		at          int
		kinds       []string
	}{
		{"mkdir", "", 0, dirs}, {"mkdir", "pg/base/1", 0, dirs}, {"publish", "", 1, dirs}, {"publish", "pg", 1, dirs},
		{"publish", "pg/global", 3, dirs}, {"publish", "run", 6, dirs}, {"publish", "pg/global/pg_control", 3, leaves},
		{"recovery", "", 0, leaves}, {"afterCopy", "", 0, []string{"dir", "link_out", "dangling", "file"}}, {"sync", "pg/base/1", 0, dirs},
	} {
		for _, kind := range row.kinds {
			t.Run(row.stage+"#"+row.name+"/"+kind, func(t *testing.T) {
				f, r := checkpointFixture(t, "legacy", false)
				c, kept, calls, reached, want := f.cell, heldTrees{}, 0, false, any(nil)
				at, left, inspected, through := c.path("recovery", heldTarget, filepath.FromSlash(row.name)), row.name == "", false, false
				if r.nextID = func() (string, error) { return heldTarget, nil }; row.stage == "recovery" {
					at = c.recoveryDir()
				}
				plant := func(now bool) {
					if now && !reached {
						reached = true
						treePlant(t, c, at, kind, kept)
						want = treeAt(t, at)
					}
				}
				original, directory := syncCheckpointTreeParent, syncCheckpointTreeDirectory
				t.Cleanup(func() { syncCheckpointTreeParent, syncCheckpointTreeDirectory = original, directory })
				syncCheckpointTreeParent = func(made string, sync func() error) error {
					err := original(made, sync)
					plant(row.stage == "recovery" && made == heldTarget || row.stage == "mkdir" && made == path.Join(heldTarget, row.name))
					return err
				}
				syncCheckpointTreeDirectory = func(name string, sync func() error) error {
					plant(row.stage == "sync" && name == path.Join(heldTarget, row.name))
					return directory(name, sync)
				}
				c.checkpointTest.beforeStep = func(step, _ string) {
					if step == "publish" {
						calls++
						plant(row.stage == "publish" && calls == row.at)
					} else if step == "hold" && reached && !left {
						inspected, through = true, !reflect.DeepEqual(treeAt(t, at), want)
					}
				}
				r.after = func(string) { plant(row.stage == "afterCopy") }
				failure := ClassifyCheckpointFailure(c.Checkpoint(t.Context(), checkpointOptions(f)))
				if !reached {
					t.Fatal("the tree row never reached its planting stage")
				}
				if failure.Class != map[bool]string{true: "local_checkpoint_witness_mismatch", false: "local_checkpoint_copy_failed"}[row.stage == "afterCopy"] {
					t.Fatalf("the tree row ended in class %q", failure.Class)
				}
				if failure.CleanupIncomplete != left {
					t.Fatalf("the tree row reported cleanup incomplete %t", failure.CleanupIncomplete)
				}
				if !treeSame(t, kept) {
					t.Fatal("the tree row changed a tree its plant leads to")
				}
				if !left && !inspected {
					t.Fatal("the tree row's discard never began after its plant")
				}
				if through {
					t.Fatal("the tree row wrote through or into its plant before the discard")
				}
				if !readers5JournalClosed(c) {
					t.Fatal("the tree row left its journal open or torn")
				}
				if left && !reflect.DeepEqual(treeAt(t, at), want) {
					t.Fatal("the tree row changed the object planted at its name")
				}
				if !left && treeAt(t, c.path("recovery", heldTarget)) != nil {
					t.Fatal("the tree row left its own checkpoint directory")
				}
			})
		}
	}
}

// A checkpoint directory made and then refused (here its privacy, under a restrictive umask after recovery/ exists) is
// recorded by its identity, so the discard removes it (owner input 3).
func TestCheckpointTreeMkdirRefused(t *testing.T) {
	f, r := checkpointFixture(t, "legacy", false)
	r.nextID = func() (string, error) { return heldTarget, nil }
	if privateDir(f.cell.recoveryDir(), true) != nil {
		t.Fatal("cannot prepare the recovery directory")
	}
	mask := syscall.Umask(0o277)
	err := f.cell.Checkpoint(t.Context(), checkpointOptions(f))
	syscall.Umask(mask)
	if failure := ClassifyCheckpointFailure(err); failure.Class != "local_checkpoint_copy_failed" {
		t.Fatalf("the refused mkdir ended in class %q", failure.Class)
	} else if failure.CleanupIncomplete {
		t.Fatal("the refused mkdir left its directory to an incomplete cleanup")
	}
	if treeAt(t, f.cell.path("recovery", heldTarget)) != nil {
		t.Fatal("the refused mkdir left something at the checkpoint name")
	}
}

// A made directory whose parent sync fails (pg/base/1's, through the seam) fails the create, and the discard removes it.
func TestCheckpointTreeParentSyncFails(t *testing.T) {
	f, r := checkpointFixture(t, "legacy", false)
	r.nextID = func() (string, error) { return heldTarget, nil }
	original := syncCheckpointTreeParent
	t.Cleanup(func() { syncCheckpointTreeParent = original })
	syncCheckpointTreeParent = func(made string, sync func() error) error {
		if made == path.Join(heldTarget, "pg", "base", "1") {
			return errors.New("synthetic parent sync failure")
		}
		return original(made, sync)
	}
	if failure := ClassifyCheckpointFailure(f.cell.Checkpoint(t.Context(), checkpointOptions(f))); failure.Class != "local_checkpoint_copy_failed" {
		t.Fatalf("the failed parent sync ended in class %q", failure.Class)
	} else if failure.CleanupIncomplete {
		t.Fatal("the failed parent sync left its directory to an incomplete cleanup")
	}
	if treeAt(t, f.cell.path("recovery", heldTarget)) != nil {
		t.Fatal("the failed parent sync left something at the checkpoint name")
	}
}

// recovery/ moved out of the cell and linked back just before a tree operation that binds on its own (the temporary
// after pg is made, the first publication, the first directory sync, the failed create's sweep and directory steps) is
// refused at that operation: the moved tree never changes and cleanup is incomplete (MINOR-1 of LC-CKPT-WALK-1BR).
func TestCheckpointTreeBound(t *testing.T) {
	for _, row := range []string{"create_temp", "publish", "sync", "sweep", "directory"} {
		t.Run(row, func(t *testing.T) {
			kept, reached := heldTrees{}, false
			plant := func(c *Cell, now bool) {
				if now && !reached {
					reached = true
					treePlant(t, c, c.recoveryDir(), "link_out", kept)
				}
			}
			parent, directory := syncCheckpointTreeParent, syncCheckpointTreeDirectory
			t.Cleanup(func() { syncCheckpointTreeParent, syncCheckpointTreeDirectory = parent, directory })
			discarding, failure := row == "sweep" || row == "directory", CheckpointFailure{}
			if discarding {
				_, failure = heldDiscard(t, func(c *Cell) { heldAt(c, row, func() { plant(c, true) }) })
			} else {
				f, r := checkpointFixture(t, "legacy", false)
				r.nextID = func() (string, error) { return heldTarget, nil }
				syncCheckpointTreeParent = func(made string, sync func() error) error {
					err := parent(made, sync)
					plant(f.cell, row == "create_temp" && made == path.Join(heldTarget, "pg"))
					return err
				}
				syncCheckpointTreeDirectory = func(name string, sync func() error) error {
					plant(f.cell, row == "sync")
					return directory(name, sync)
				}
				f.cell.checkpointTest.beforeStep = func(step, _ string) { plant(f.cell, row == "publish" && step == "publish") }
				failure = ClassifyCheckpointFailure(f.cell.Checkpoint(t.Context(), checkpointOptions(f)))
			}
			if !reached {
				t.Fatal("the bound row never reached its planting stage")
			}
			if !discarding && failure.Class != "local_checkpoint_copy_failed" {
				t.Fatalf("the bound row ended in class %q", failure.Class)
			}
			if !failure.CleanupIncomplete {
				t.Fatal("the bound row reported a cleanup through a moved recovery directory")
			}
			if !treeSame(t, kept) {
				t.Fatal("a tree operation changed the moved recovery directory")
			}
		})
	}
}

// A create leaves no descriptor open on recovery/: its tree is closed (N7 of LC-CKPT-WALK-1BR). No collection runs
// while the row looks, so no finalizer can close a leaked root behind it.
func TestCheckpointTreeClosed(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	f, r := checkpointFixture(t, "legacy", false)
	r.nextID = func() (string, error) { return heldTarget, nil }
	var recovery unix.Stat_t
	if f.cell.Checkpoint(t.Context(), checkpointOptions(f)) != nil || unix.Lstat(f.cell.recoveryDir(), &recovery) != nil {
		t.Fatal("cannot create the closed tree checkpoint")
	}
	descriptors, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal("cannot list this process's descriptors")
	}
	for _, descriptor := range descriptors {
		var stat unix.Stat_t
		if fd, err := strconv.Atoi(descriptor.Name()); err == nil && unix.Fstat(fd, &stat) == nil && stat.Dev == recovery.Dev && stat.Ino == recovery.Ino {
			t.Fatal("a create left a descriptor open on the recovery directory")
		}
	}
}

// A directory put at the checkpoint's name at the discard's directory stage is not the one the call recorded: kept.
func TestCheckpointTreeDiscardSwapped(t *testing.T) {
	var want any
	c, failure := heldDiscard(t, func(c *Cell) {
		heldAt(c, "directory", func() {
			treePlant(t, c, c.path("recovery", heldTarget), "dir", heldTrees{})
			want = treeAt(t, c.path("recovery", heldTarget))
		})
	})
	if !failure.CleanupIncomplete {
		t.Fatal("the swapped discard reported a complete cleanup")
	}
	if want == nil || !reflect.DeepEqual(treeAt(t, c.path("recovery", heldTarget)), want) {
		t.Fatal("the swapped discard removed or changed the planted directory")
	}
}

// checkpointTreeRemoveDirectory removes only the empty directory recorded at the name; anything else is kept.
func TestCheckpointTreeRemoveDirectory(t *testing.T) {
	for _, row := range [][2]string{{"recorded", ""}, {"recorded_full", ""}, {"other", "dir"}, {"file", "file"}, {"link", "link_cell"}} {
		t.Run(row[0], func(t *testing.T) {
			c := &Cell{cfg: Config{Root: testPrivateRoot(t)}}
			root, err := os.OpenRoot(c.cfg.Root)
			if err != nil || testMkdir(c.path("tmp"), 0o700) != nil {
				t.Fatal("cannot hold the removal fixture")
			}
			defer func() { _ = root.Close() }()
			id, err := privateBeneathMkdir(root, "made", nil, func(int) error { return nil })
			if kind, name := row[0], c.path("made"); err == nil && row[1] != "" {
				treePlant(t, c, name, row[1], heldTrees{})
			} else if err == nil && kind == "recorded_full" {
				err = testWriteFile(filepath.Join(name, "kept"), []byte("kept"), 0o600)
			}
			if err != nil {
				t.Fatal("cannot plant the removal fixture")
			}
			before, kind := treeAt(t, c.path("made")), row[0]
			err = checkpointTreeRemoveDirectory(root, "made", id)
			switch {
			case kind == "recorded" && (err != nil || treeAt(t, c.path("made")) != nil):
				t.Fatal("the recorded directory was not removed")
			case kind == "recorded_full" && !errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, unix.EEXIST):
				t.Fatal("the full recorded directory was not refused by the system")
			case row[1] != "" && !errors.Is(err, errPrivateBeneathMoved):
				t.Fatal("another object at the name was not refused as moved")
			case kind != "recorded" && !reflect.DeepEqual(treeAt(t, c.path("made")), before):
				t.Fatal("a refused object at the name was removed or changed")
			}
		})
	}
}
