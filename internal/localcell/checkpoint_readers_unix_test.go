//go:build linux || darwin

package localcell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// checkpointReaderRow plants one object at target, a slash path below the
// cell root (the inventory open's own path when empty), at moment: "before"
// the call, at the "admission" (first) or "witness" (second) control call, at
// the first copy "publish" or at the first inventory "open".
type checkpointReaderRow struct{ name, moment, target, want string }

// checkpointWaitRows put a FIFO at each create-path reader's name after the
// check that decided its type; a "_writer_held" row holds a writer, which
// parks a read instead of the open.
var checkpointWaitRows = []checkpointReaderRow{
	{"source", "admission", "pg/base/1/1259", "local_checkpoint_witness_mismatch"},
	{"source_writer_held", "admission", "pg/base/1/1259", "local_checkpoint_witness_mismatch"},
	{"witness", "witness", "pg/global/pg_control", "local_checkpoint_witness_mismatch"},
	{"witness_writer_held", "witness", "pg/global/pg_control", "local_checkpoint_witness_mismatch"},
	{"inventory", "open", "", "local_checkpoint_witness_mismatch"},
	{"inventory_writer_held", "open", "", "local_checkpoint_witness_mismatch"},
	{"manifest", "publish", "manifest.json", "local_checkpoint_witness_mismatch"},
	{"custody", "admission", "run/postgres.password", "local_checkpoint_witness_mismatch"},
	{"pg_version", "before", "pg/PG_VERSION", "local_checkpoint_source_unsupported"},
}

// checkpointLinkRows put a link to the moved-aside file at the same moments.
var checkpointLinkRows = []checkpointReaderRow{
	{"manifest", "publish", "manifest.json", "local_checkpoint_copy_failed"},
	{"custody", "admission", "run/postgres.password", "local_checkpoint_copy_failed"},
	{"inventory", "open", "", "local_checkpoint_witness_mismatch"},
	{"pg_version", "before", "pg/PG_VERSION", "local_checkpoint_source_unsupported"},
}

// checkpointReaderPlant keeps the original at path+".saved" and puts at path a
// link to it, or a FIFO made at an aside name and renamed in; a held FIFO gets
// a non-blocking reader, then the writer, then loses the reader.
func checkpointReaderPlant(path, kind string, writer **os.File) error {
	if err := os.Rename(path, path+".saved"); err != nil {
		return err
	}
	if kind == "link" {
		return os.Symlink(path+".saved", path)
	}
	aside := path + ".fifo"
	if err := syscall.Mkfifo(aside, 0o600); err != nil {
		return err
	}
	if kind == "fifo_held" {
		reader, err := os.OpenFile(aside, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		*writer, err = os.OpenFile(aside, os.O_WRONLY, 0)
		if closeErr := reader.Close(); err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
	}
	return os.Rename(aside, path)
}

// checkpointReaderRun plants through the existing hooks only, runs Checkpoint
// under the reader watchdog, puts the original back (a FIFO left at the
// manifest refuses readJournal) and requires the class, a complete cleanup, no
// checkpoint directory and no open journal operation. The watchdog's unblock
// opens a writer only once the call has blocked, or closes the held writer.
func checkpointReaderRun(t *testing.T, layout string, row checkpointReaderRow, kind string) {
	f, r := checkpointFixture(t, layout, false)
	c, ops := f.cell, f.cell.checkpointTest
	var mu sync.Mutex
	var planted string
	var writer *os.File
	plantErr := errors.New("checkpoint reader object not planted")
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if writer != nil {
			_ = writer.Close()
		}
	})
	put := func(hookPath string) {
		mu.Lock()
		defer mu.Unlock()
		if planted != "" {
			return
		}
		planted = hookPath
		if row.target != "" {
			planted = c.path(filepath.FromSlash(row.target))
		}
		plantErr = checkpointReaderPlant(planted, kind, &writer)
	}
	switch row.moment {
	case "before":
		put("")
	case "admission", "witness":
		control, calls := ops.control, 0
		ops.control = func(ctx context.Context) (pgControlData, error) {
			if calls++; calls == map[string]int{"admission": 1, "witness": 2}[row.moment] {
				put("")
			}
			return control(ctx)
		}
	default:
		ops.beforeStep = func(step, path string) {
			if step == row.moment {
				put(path)
			}
		}
	}
	err := readerWatchdog(t, func() error { return c.Checkpoint(t.Context(), checkpointOptions(f)) }, func() {
		mu.Lock()
		defer mu.Unlock()
		if writer != nil {
			_ = writer.Close()
			writer = nil
		} else if kind == "fifo" {
			if opened, err := os.OpenFile(planted, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = opened.Close()
			}
		}
	})
	// The original goes back unless the discard removed it with the checkpoint.
	mu.Lock()
	restoreErr := plantErr
	if restoreErr == nil {
		if _, saved := os.Lstat(planted + ".saved"); saved == nil {
			restoreErr = errors.Join(os.Remove(planted), os.Rename(planted+".saved", planted))
		}
	}
	mu.Unlock()
	failure := ClassifyCheckpointFailure(err)
	progress, journalErr := c.readJournal(context.Background())
	closed := journalErr == nil && progress.pending == nil && !progress.tornTail
	leftover := slices.ContainsFunc(checkpointIDs(t, c), func(name string) bool { return strings.HasPrefix(name, "ckpt_") })
	if restoreErr != nil || failure.Class != row.want || failure.CleanupIncomplete || !closed || leftover || (row.moment == "before" && r.calls["control"] != 0) {
		t.Fatalf("checkpoint reader row ended as class %q: cleanup incomplete %t, planted and restored %t, journal closed %t, leftover %t",
			failure.Class, failure.CleanupIncomplete, restoreErr == nil, closed, leftover)
	}
}

// Each create-path reader refuses a FIFO renamed onto its name after the check
// that decided its type at once, with the class that check gives it, whether a
// writer is held or not, and leaves nothing behind.
func TestCheckpointReadersNeverWait(t *testing.T) {
	for _, row := range checkpointWaitRows {
		kind := "fifo"
		if strings.HasSuffix(row.name, "_writer_held") {
			kind = "fifo_held"
		}
		for _, layout := range []string{"legacy", "selected"} {
			t.Run(row.name+"/"+layout, func(t *testing.T) { checkpointReaderRun(t, layout, row, kind) })
		}
	}
}

// A link put at the manifest, custody, an inventory copy or PG_VERSION at the
// same moments is refused, never followed, though it names the original file.
func TestCheckpointReadersRefuseLink(t *testing.T) {
	for _, row := range checkpointLinkRows {
		for _, layout := range []string{"legacy", "selected"} {
			t.Run(row.name+"/"+layout, func(t *testing.T) { checkpointReaderRun(t, layout, row, "link") })
		}
	}
}
