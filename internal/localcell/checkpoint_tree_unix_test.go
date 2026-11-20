//go:build linux || darwin

package localcell

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"
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

// A same-user object put at a directory the copy made (its parent synced: syncCheckpointTreeParent), at a name it is
// about to publish (checkpointTest.beforeStep "publish", the k-th), at recovery/ (after the checkpoint directory's
// parent sync), or at the checkpoint after the copy (checkpointRecorder.after) is refused before anything is written
// through it: nothing it leads to changes, the journal closes, and cleanup is incomplete exactly where the plant
// stands at the checkpoint's name or recovery/, which then holds it as planted; elsewhere the call removed its own.
func TestCheckpointTreeCopyRefuses(t *testing.T) {
	dirs, leaves := []string{"dir", "link_cell", "link_out", "dangling", "file", "fifo"}, []string{"dir", "link_out", "dangling", "file", "fifo"}
	for _, row := range []struct {
		stage, name string
		at          int
		kinds       []string
	}{
		{"mkdir", "", 0, dirs}, {"mkdir", "pg/base/1", 0, dirs}, {"publish", "", 1, dirs}, {"publish", "pg", 1, dirs},
		{"publish", "pg/global", 3, dirs}, {"publish", "run", 6, dirs}, {"publish", "pg/global/pg_control", 3, leaves},
		{"recovery", "", 0, leaves}, {"afterCopy", "", 0, []string{"dir", "link_out", "dangling", "file"}},
	} {
		for _, kind := range row.kinds {
			t.Run(row.stage+"#"+row.name+"/"+kind, func(t *testing.T) {
				f, r := checkpointFixture(t, "legacy", false)
				c, kept, calls, reached, want := f.cell, heldTrees{}, 0, false, any(nil)
				at, left := c.path("recovery", heldTarget, filepath.FromSlash(row.name)), row.name == ""
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
				original := syncCheckpointTreeParent
				t.Cleanup(func() { syncCheckpointTreeParent = original })
				syncCheckpointTreeParent = func(made string, sync func() error) error {
					err := original(made, sync)
					plant(row.stage == "recovery" && made == heldTarget || row.stage == "mkdir" && made == path.Join(heldTarget, row.name))
					return err
				}
				c.checkpointTest.beforeStep = func(step, _ string) {
					if step == "publish" {
						calls++
						plant(row.stage == "publish" && calls == row.at)
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
				heldSame(t, kept)
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
