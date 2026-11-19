//go:build linux

package localcell

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A write lease on global/pg_control is not refused at once (LC-S7D2F4B2R
// DOC-1): pg_controldata opens it without O_NONBLOCK, so create's admission
// waits until its --timeout budget kills the run, and nothing is journaled or
// changed. The stand-in's shell redirection opens the file the same way; it
// first reads the unleased file, so only the lease can hold it, and its open
// must have started the lease's break. A refused lease fails the row.
func TestCheckpointControlLease(t *testing.T) {
	breakTime, err := os.ReadFile("/proc/sys/fs/lease-break-time")
	if seconds, parseErr := strconv.Atoi(strings.TrimSpace(string(breakTime))); err != nil || parseErr != nil || seconds < 10 {
		t.Fatal("this kernel breaks write leases sooner than the control lease row can wait")
	}
	f, r := checkpointFixture(t, "legacy", false)
	c := f.cell
	r.production, c.cfg.Binaries.PostgresDir, c.cfg.Timeout = true, testPrivateRoot(t), 2*time.Second
	standIn := "#!/bin/sh\n: <\"$2/global/pg_control\" || exit 1\nprintf '%s' '" + controlReading(checkpointSyntheticControl) + "'\n"
	if testWriteFile(filepath.Join(c.cfg.Binaries.PostgresDir, "pg_controldata"), []byte(standIn), 0o700) != nil {
		t.Fatal("cannot write the blocking stand-in control program")
	}
	if reading, err := c.checkpointControl(t.Context()); err != nil || reading != checkpointSyntheticControl {
		t.Fatal("the blocking stand-in control program cannot read an unleased control file")
	}
	before := checkpointTreeState(t, c)
	lease := checkpointLease(t, c, "pg/global/pg_control")
	started := time.Now()
	failure := ClassifyCheckpointFailure(c.Checkpoint(t.Context(), checkpointOptions(f)))
	elapsed := time.Since(started)
	progress, journalErr := c.readJournal(context.Background())
	held, _ := unix.FcntlInt(lease.Fd(), unix.F_GETLEASE, 0)
	if elapsed < c.cfg.Timeout || failure.Class != "local_checkpoint_operation_failed" || failure.CleanupIncomplete || r.calls["control"] != 1 || journalErr != nil ||
		progress.pending != nil || progress.tornTail || held != unix.F_RDLCK || !reflect.DeepEqual(checkpointLeasedTree(t, c, lease), before) {
		t.Fatal("a leased control file did not hold create for its whole budget with nothing journaled or changed")
	}
	t.Logf("leased_create_elapsed=%s", elapsed.Round(time.Millisecond))
}
