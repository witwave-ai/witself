package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// LC-WINERR-5. On Windows a regular file where a directory is expected reads as
// path not found, a not-exist error; on Unix it is ENOTDIR. These rows plant a
// regular file at the nearest directory and run on every platform: Unix refuses
// before the change too, Windows only after it.

const winerr5Planted = "synthetic regular file at a directory name"

// winerr5Plant replaces the empty directory at path with a private regular file.
func winerr5Plant(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cannot clear the directory before the regular file plant")
	}
	if err := testWriteFile(path, []byte(winerr5Planted), 0o600); err != nil {
		t.Fatal("cannot plant the regular file at the directory name")
	}
}

func winerr5Kept(t *testing.T, path string) {
	t.Helper()
	if data, err := os.ReadFile(path); err != nil || string(data) != winerr5Planted {
		t.Fatal("the planted regular file changed")
	}
}

// The walk the Windows branch takes, run directly on every platform.
func TestWinerr5NearestAncestor(t *testing.T) {
	root := testPrivateRoot(t)
	file, dir := filepath.Join(root, "file"), filepath.Join(root, "dir")
	if testMkdir(dir, 0o700) != nil || testWriteFile(file, []byte(winerr5Planted), 0o600) != nil {
		t.Fatal("cannot prepare the nearest ancestor rows")
	}
	for _, row := range []struct {
		name string
		path string
		want bool
	}{
		{"directory_parent", filepath.Join(dir, "absent"), true},
		{"missing_below_directory", filepath.Join(dir, "missing", "absent"), true},
		{"file_parent", filepath.Join(file, "absent"), false},
		{"missing_below_file", filepath.Join(file, "missing", "absent"), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			if nearestAncestorIsDirectory(row.path) != row.want {
				t.Fatal("the nearest existing ancestor was misjudged")
			}
			winerr5Kept(t, file)
		})
	}
}

// Each way into openRegularBy, with a regular file at the reader's directory:
// the reader's own refusal, the text Unix gives; with the directory and no
// file, absent.
func TestWinerr5ReadersParentFile(t *testing.T) {
	for _, row := range []struct {
		name, parent, want string
		read               func(c *Cell) (absent bool, err error)
	}{
		{"journal", "journal", "invalid local cell journal", func(c *Cell) (bool, error) {
			progress, err := c.readJournal(context.Background())
			return err == nil && progress.pending == nil && progress.durableBytes == 0, err
		}},
		{"process_record", "run", "cannot inspect local cell record", func(c *Cell) (bool, error) {
			state, err := c.processState(t.Context(), "server")
			return err == nil && state == (ProcessState{}), err
		}},
		{"postmaster", "pg", "invalid PostgreSQL process record", func(c *Cell) (bool, error) {
			pid, err := c.postmaster(context.Background())
			return err == nil && pid == 0, err
		}},
		{"service_record", "service", "service_record_not_private", func(c *Cell) (bool, error) {
			f, err := openServiceRecord(context.Background(), c.path("service", "control.secret"), errors.New("cannot open the synthetic record"))
			if f != nil {
				_ = f.Close()
			}
			return errors.Is(err, os.ErrNotExist), err
		}},
	} {
		t.Run(row.name+"/directory", func(t *testing.T) {
			c, _ := unitCell(t)
			if privateDir(c.path(row.parent), true) != nil {
				t.Fatal("cannot prepare the reader directory")
			}
			before := retainedSnapshot(t, c)
			if absent, _ := row.read(c); !absent {
				t.Fatal("an absent file below a directory was not read as absent")
			}
			if !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
				t.Fatal("the absent read changed the tree")
			}
		})
		t.Run(row.name+"/file", func(t *testing.T) {
			c, _ := unitCell(t)
			winerr5Plant(t, c.path(row.parent))
			before := retainedSnapshot(t, c)
			if absent, err := row.read(c); absent || err == nil || err.Error() != row.want {
				t.Fatal("a regular file at the reader directory was not refused")
			}
			if !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
				t.Fatal("the refused read changed the tree")
			}
			winerr5Kept(t, c.path(row.parent))
		})
	}
}

// LC-READERS-6R P4: Repair of a removal cut before its cleanup clear with a
// regular file at journal. Planted before Repair, validateRoot refuses it on
// every platform; planted after Repair's own proofs (at the removal_retained
// boundary, the last before the retained sync reads the journal), the read
// must refuse too, keeping the cleanup checkpoint and syncing nothing.
func TestWinerr5RepairCutRemoval(t *testing.T) {
	for _, row := range []struct {
		name   string
		during bool
		want   error
	}{
		{"start", false, errLocalDirectory},
		{"during_repair", true, errors.New("invalid local cell journal")},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := writerRetainedCleanup(t)
			aside := filepath.Join(t.TempDir(), "journal")
			journal, err := os.ReadFile(c.path("journal", "operations.jsonl"))
			if err != nil || len(journal) == 0 {
				t.Fatal("the cut removal left no journal")
			}
			syncs, original := 0, syncFile
			t.Cleanup(func() { syncFile = original })
			syncFile = func(f *os.File) error { syncs++; return original(f) }
			var before map[string]retainedEntry
			plant := func() {
				if os.Rename(c.path("journal"), aside) != nil {
					t.Fatal("cannot move the journal directory aside")
				}
				winerr5Plant(t, c.path("journal"))
				before, syncs = retainedSnapshot(t, c), 0
			}
			if row.during {
				c.cfg.Failpoint = func(op, step string) error {
					if op == "remove" && step == "removal_retained" && before == nil {
						plant()
					}
					return nil
				}
			} else {
				plant()
			}
			err = c.Repair(t.Context())
			c.cfg.Failpoint = nil
			if before == nil || err == nil || err.Error() != row.want.Error() {
				t.Fatal("repair with a regular file at the journal directory did not refuse")
			}
			if syncs != 0 || !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
				t.Fatal("the refused repair synced or changed the tree")
			}
			if m, err := readManifest(t.Context(), c.cfg.Root); err != nil || m.Cleanup == nil || !m.RemovalRetained {
				t.Fatal("the refused repair cleared the retained cleanup checkpoint")
			}
			if moved, err := os.ReadFile(filepath.Join(aside, "operations.jsonl")); err != nil || !bytes.Equal(moved, journal) {
				t.Fatal("the journal moved aside changed")
			}
			winerr5Kept(t, c.path("journal"))
		})
	}
}

// Repair of a selected install re-reads the journal after its PostgreSQL steps
// (lifecycle.go, after verify); a regular file planted at journal during the
// last of them is refused at that read, with nothing written after the plant.
func TestWinerr5RepairSelectedInstall(t *testing.T) {
	c, _ := selectedJournalFixture(t)
	for _, step := range selectedInstallSteps {
		if c.step(context.Background(), "install", step, func() error { return nil }) != nil {
			t.Fatal("cannot prepare the selected install journal")
		}
	}
	calls := 0
	c.installerPrincipals = recordingPrincipalOperations(&calls)
	if selectedFixtureDataDirectory(c) != nil {
		t.Fatal("cannot prepare the selected install data directory")
	}
	syncs, original := 0, syncFile
	t.Cleanup(func() { syncFile = original })
	syncFile = func(f *os.File) error { syncs++; return original(f) }
	var before map[string]retainedEntry
	c.installerPrincipals.verify = func(context.Context, *Manifest) error {
		if os.Rename(c.path("journal"), filepath.Join(t.TempDir(), "journal")) != nil {
			t.Fatal("cannot move the journal directory aside")
		}
		winerr5Plant(t, c.path("journal"))
		before, syncs = retainedSnapshot(t, c), 0
		return nil
	}
	if err := c.Repair(t.Context()); before == nil || err == nil || err.Error() != "invalid local cell journal" {
		t.Fatal("selected install repair with a regular file at the journal directory did not refuse at the read")
	}
	if syncs != 0 || !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
		t.Fatal("the refused selected install repair synced or changed the tree")
	}
	winerr5Kept(t, c.path("journal"))
}
