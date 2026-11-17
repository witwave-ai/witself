//go:build linux || darwin

package localcell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// readers5Planter puts one object at the first name a hook offers: a FIFO
// ("fifo", or "fifo_held" with a writer held) or an empty regular file
// ("file"), with the directory at that name kept at name+".saved".
type readers5Planter struct {
	mu            sync.Mutex
	kind, planted string
	writer        *os.File
	err           error
}

func (p *readers5Planter) put(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.planted != "" {
		return
	}
	p.planted = name
	if p.kind != "file" {
		p.err = checkpointReaderPlant(name, p.kind, &p.writer)
		return
	}
	if p.err = os.Rename(name, name+".saved"); p.err == nil {
		if p.err = os.WriteFile(name+".file", nil, 0o600); p.err == nil {
			p.err = os.Rename(name+".file", name)
		}
	}
}

// unblock ends a call that waits on the planted FIFO, as the watchdog's
// unblock: the held writer closes, or a writer opens and closes at the name.
func (p *readers5Planter) unblock() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.writer != nil {
		_ = p.writer.Close()
		p.writer = nil
	} else if p.kind == "fifo" && p.planted != "" {
		if opened, err := os.OpenFile(p.planted, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = opened.Close()
		}
	}
}

// restore removes the planted object and renames the original back while the
// original still exists; only the discard of a checkpoint directory below
// recovery/ removes both, which the row then checks on recovery/ itself.
func (p *readers5Planter) restore(c *Cell) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.writer != nil {
		_ = p.writer.Close()
		p.writer = nil
	}
	if p.planted == "" || p.err != nil {
		return false
	}
	if _, err := os.Lstat(p.planted + ".saved"); err == nil {
		return os.Remove(p.planted) == nil && os.Rename(p.planted+".saved", p.planted) == nil
	}
	return strings.HasPrefix(p.planted, c.recoveryDir()+string(filepath.Separator)+"ckpt_")
}

// readers5Seam replaces a directory sync seam, which must still be
// syncDirectory, with one that offers the name to the planter first and then
// calls syncDirectory; the seam is restored when the test ends.
func readers5Seam(t *testing.T, seam *func(string) error, accept func(string) bool, p *readers5Planter) {
	t.Helper()
	saved := *seam
	if reflect.ValueOf(saved).Pointer() != reflect.ValueOf(syncDirectory).Pointer() {
		t.Fatal("the directory sync seam no longer defaults to syncDirectory")
	}
	t.Cleanup(func() { *seam = saved })
	*seam = func(name string) error {
		if accept(name) {
			p.put(name)
		}
		return saved(name)
	}
}

// readers5NothingLeft checks recovery/ itself (R8): it exists as a directory
// and lists no checkpoint; a recovery/ that cannot be read is not empty.
func readers5NothingLeft(c *Cell) bool {
	info, statErr := os.Lstat(c.recoveryDir())
	entries, listErr := os.ReadDir(c.recoveryDir())
	return statErr == nil && info.IsDir() && listErr == nil &&
		!slices.ContainsFunc(entries, func(entry os.DirEntry) bool { return strings.HasPrefix(entry.Name(), "ckpt_") })
}

// readers5JournalClosed requires the journal itself, a regular file, with no
// open operation and no torn tail.
func readers5JournalClosed(c *Cell) bool {
	info, statErr := os.Lstat(c.path("journal", "operations.jsonl"))
	progress, err := c.readJournal()
	return statErr == nil && info.Mode().IsRegular() && err == nil && progress.pending == nil && !progress.tornTail
}

// A FIFO put at pg/ after admission, at a checkpoint directory being synced or
// at a parent synced inside the checkpoint, or a regular file at a directory
// being synced, is refused at once as a failed copy: the copy is discarded,
// recovery/ lists no checkpoint and the journal is closed. "_writer_held" pins
// the refusal that already came after the open.
func TestReaders5NeverWait(t *testing.T) {
	for _, row := range []struct{ name, hook, kind string }{
		{"pgdata_root", "control", "fifo"},
		{"pgdata_root_writer_held", "control", "fifo_held"},
		{"checkpoint_sync", "checkpoint", "fifo"},
		{"checkpoint_sync_file", "checkpoint", "file"},
		{"parent_sync", "parent", "fifo"},
	} {
		for _, layout := range []string{"legacy", "selected"} {
			t.Run(row.name+"/"+layout, func(t *testing.T) {
				f, _ := checkpointFixture(t, layout, false)
				c, ops := f.cell, f.cell.checkpointTest
				p := &readers5Planter{kind: row.kind}
				t.Cleanup(p.unblock)
				switch row.hook {
				case "control":
					// The first control call runs at admission, after the plan.
					control, calls := ops.control, 0
					ops.control = func(ctx context.Context) (pgControlData, error) {
						if calls++; calls == 1 {
							p.put(c.path("pg"))
						}
						return control(ctx)
					}
				case "checkpoint":
					// The first sync is the last planned directory (deepest first).
					readers5Seam(t, &syncCheckpointDirectory, func(string) bool { return true }, p)
				case "parent":
					readers5Seam(t, &syncPrivateDirParent, func(name string) bool {
						rel, err := filepath.Rel(c.recoveryDir(), name)
						return err == nil && strings.HasPrefix(rel, "ckpt_") && strings.Contains(rel, string(filepath.Separator))
					}, p)
				}
				err := readerWatchdog(t, func() error { return c.Checkpoint(t.Context(), checkpointOptions(f)) }, p.unblock)
				restored, failure := p.restore(c), ClassifyCheckpointFailure(err)
				if !restored || failure.Class != "local_checkpoint_copy_failed" || failure.CleanupIncomplete || !readers5NothingLeft(c) || !readers5JournalClosed(c) {
					t.Fatalf("the create row ended as class %q: cleanup incomplete %t, planted and restored %t, nothing left %t, journal closed %t",
						failure.Class, failure.CleanupIncomplete, restored, readers5NothingLeft(c), readers5JournalClosed(c))
				}
			})
		}
	}
}

// On a purged root, delete recreates journal/ and syncs the root, then its
// first append creates the journal file and syncs journal/. A FIFO at either
// is refused at once: at the root nothing is journaled and the checkpoint stays
// complete; at journal/ the open operation is left, and a second delete with
// nothing planted closes it and deletes the checkpoint.
func TestReaders5DeleteNeverWait(t *testing.T) {
	for _, row := range []string{"root_sync", "journal_directory_sync"} {
		for _, layout := range []string{"legacy", "selected"} {
			t.Run(row+"/"+layout, func(t *testing.T) {
				f, _ := checkpointFixture(t, layout, false)
				c := f.cell
				if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil || c.Remove(t.Context(), RemoveOptions{Purge: true}) != nil {
					t.Fatal("cannot prepare the purged checkpoint")
				}
				ids := checkpointIDs(t, c)
				if len(ids) != 1 || !checkpointSummaryOf(t, c, ids[0]).Complete {
					t.Fatal("the purged checkpoint fixture differs")
				}
				p := &readers5Planter{kind: "fifo"}
				t.Cleanup(p.unblock)
				if row == "root_sync" {
					readers5Seam(t, &syncPrivateDirParent, func(name string) bool { return name == c.cfg.Root }, p)
				} else {
					readers5Seam(t, &syncJournalDirectory, func(string) bool { return true }, p)
				}
				options := DeleteCheckpointOptions{CheckpointID: ids[0]}
				err := readerWatchdog(t, func() error { return c.DeleteCheckpoint(t.Context(), options) }, p.unblock)
				restored, failure := p.restore(c), ClassifyCheckpointFailure(err)
				if !restored || failure.Class != "local_checkpoint_operation_failed" || failure.CleanupIncomplete {
					t.Fatalf("the delete row ended as class %q: cleanup incomplete %t, planted and restored %t", failure.Class, failure.CleanupIncomplete, restored)
				}
				if row == "root_sync" {
					// journal/, recreated before the refused sync, is a directory and lists nothing.
					info, statErr := os.Lstat(c.path("journal"))
					entries, listErr := os.ReadDir(c.path("journal"))
					if statErr != nil || !info.IsDir() || listErr != nil || len(entries) != 0 || !checkpointSummaryOf(t, c, ids[0]).Complete {
						t.Fatal("the refused root sync journaled something or changed the checkpoint")
					}
					return
				}
				progress, journalErr := c.readJournal()
				if journalErr != nil || progress.pending == nil || progress.pending.Operation != "checkpoint_delete" || !checkpointSummaryOf(t, c, ids[0]).Complete {
					t.Fatal("the refused journal directory sync left no open delete or changed the checkpoint")
				}
				if err := readerWatchdog(t, func() error { return c.DeleteCheckpoint(t.Context(), options) }, func() {}); err != nil || !readers5NothingLeft(c) || !readers5JournalClosed(c) {
					t.Fatal("the second delete did not close the operation and delete the checkpoint")
				}
			})
		}
	}
}

// syncDirectory refuses at once whatever is not a directory at the name, a
// FIFO through a link included, with one fixed text, and still follows a link
// to a directory.
func TestSyncDirectoryNeverWaits(t *testing.T) {
	for _, row := range []struct{ name, want string }{
		{"fifo", "cannot open local cell directory for sync"},
		{"fifo_writer_held", "cannot open local cell directory for sync"},
		{"file", "cannot open local cell directory for sync"},
		{"link_to_fifo", "cannot open local cell directory for sync"},
		{"directory", ""},
		{"link_to_directory", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			name, target := filepath.Join(dir, "name"), filepath.Join(dir, "target")
			var writer *os.File
			t.Cleanup(func() {
				if writer != nil {
					_ = writer.Close()
				}
			})
			var err error
			switch row.name {
			case "fifo", "fifo_writer_held":
				if err = syscall.Mkfifo(name, 0o600); err == nil && row.name == "fifo_writer_held" {
					var reader *os.File
					if reader, err = os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
						writer, err = os.OpenFile(name, os.O_WRONLY, 0)
						err = errors.Join(err, reader.Close())
					}
				}
			case "file":
				err = os.WriteFile(name, nil, 0o600)
			case "link_to_fifo":
				err = errors.Join(syscall.Mkfifo(target, 0o600), os.Symlink(target, name))
			case "directory":
				err = os.Mkdir(name, 0o700)
			case "link_to_directory":
				err = errors.Join(os.Mkdir(target, 0o700), os.Symlink(target, name))
			}
			if err != nil {
				t.Fatal("cannot plant the object to sync")
			}
			got := readerWatchdog(t, func() error { return syncDirectory(name) }, func() {
				if writer != nil {
					_ = writer.Close()
					writer = nil
				} else if opened, err := os.OpenFile(name, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = opened.Close()
				}
			})
			switch {
			case got == nil && row.want != "":
				t.Fatal("the directory sync accepted what is not a directory")
			case got != nil && got.Error() != row.want:
				t.Fatalf("the directory sync ended with the text %q", got.Error())
			}
		})
	}
}
