//go:build linux || darwin || windows

package localcell

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCheckpointFailureReceipt(t *testing.T) {
	const copyClass = "local_checkpoint_copy_failed"
	const copyText = "local cell checkpoint: witness failed: " + copyClass
	for _, row := range []struct {
		name, close, class, text string
		partial                  bool
	}{
		{"discard_close_sync", "sync", "local_checkpoint_operation_failed", "cannot sync local cell journal", true},
		{"discard_close_guard", "torn", copyClass, copyText, true},
		{"discard_close_ok", "ok", copyClass, "local cell checkpoint failed; cleanup incomplete", true},
		{"clean_close_sync", "sync", "local_checkpoint_operation_failed", "cannot sync local cell journal", false},
		{"clean_close_guard", "torn", copyClass, copyText, false},
		{"clean_close_ok", "ok", copyClass, copyText, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			c := f.cell
			dir := c.path("recovery", heldTarget)
			r.nextID = func() (string, error) { return heldTarget, nil }
			r.after = func(string) { r.controlErr = errors.New("synthetic control failure") }
			release := func() {}
			calls := heldAt(c, "sweep", func() {
				if row.partial {
					release = blockCheckpointReceiptSweep(t, dir)
				}
			})
			trace := traceJournalDirectorySync(t, c)
			records := slices.Concat(checkpointRecords[:6], []string{"checkpoint/witness/failed"})
			var tail []byte
			r.before = func() {
				release()
				retainedAssertRecords(t, f, records)
				switch row.close {
				case "sync":
					trace.fileErr = errors.New("synthetic closing sync failure")
				case "torn":
					tail = append(retainedRead(t, c, "journal/operations.jsonl"), '{')
					checkpointWrite(t, c, "journal/operations.jsonl", string(tail))
				}
			}
			err := c.Checkpoint(t.Context(), checkpointOptions(f))
			receipt := ClassifyCheckpointFailure(err)
			if r.calls["control"] != 2 || calls["sweep"] != 1 {
				t.Fatal("create did not reach the second control read and exactly one discard sweep")
			}
			if receipt.Class != row.class || receipt.Reason != "" {
				t.Fatal("failed create changed its refusal class or reason")
			}
			if err == nil || err.Error() != row.text {
				t.Fatal("failed create changed its refusal text")
			}
			entries, listErr := os.ReadDir(dir)
			_, witnessErr := os.Lstat(filepath.Join(dir, checkpointWitnessName))
			if row.partial && (listErr != nil || len(entries) == 0 || !errors.Is(witnessErr, os.ErrNotExist)) {
				t.Fatal("discard did not leave a nonempty witness-less checkpoint directory")
			}
			if !row.partial && !errors.Is(listErr, os.ErrNotExist) {
				t.Fatal("completed discard left a checkpoint directory")
			}
			if row.close == "torn" {
				if !bytes.Equal(tail, retainedRead(t, c, "journal/operations.jsonl")) {
					t.Fatal("create wrote past the torn journal tail")
				}
			} else {
				retainedAssertRecords(t, f, append(records, "checkpoint/operation/done"))
				if row.close == "sync" && (len(trace.events) == 0 || trace.events[len(trace.events)-1] != "file_failed") {
					t.Fatal("create did not encounter the closing journal sync failure")
				}
			}
			if receipt.CleanupIncomplete != row.partial {
				t.Fatal("failed create misreported completion of its discard")
			}
		})
	}
}

func TestCheckpointInterruptedReceipt(t *testing.T) {
	for _, row := range []struct {
		cut     string
		records int
	}{
		{"operation_started", 1}, {"copy", 2}, {"inventory", 4}, {"witness", 6},
	} {
		t.Run(row.cut, func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			c := f.cell
			r.nextID = func() (string, error) { return heldTarget, nil }
			c.cfg.Failpoint = checkpointFailpoint("checkpoint", row.cut)
			err := c.Checkpoint(t.Context(), checkpointOptions(f))
			receipt := ClassifyCheckpointFailure(err)
			if !errors.Is(err, errInterrupted) || err.Error() != errInterrupted.Error() ||
				receipt.Class != "local_checkpoint_operation_failed" || receipt.Reason != "" {
				t.Fatal("interrupted create changed its refusal identity, text, class or reason")
			}
			dir := c.path("recovery", heldTarget)
			entries, listErr := os.ReadDir(dir)
			_, witnessErr := os.Lstat(filepath.Join(dir, checkpointWitnessName))
			if row.cut == "operation_started" && !errors.Is(listErr, os.ErrNotExist) ||
				row.cut != "operation_started" && (listErr != nil || len(entries) == 0) {
				t.Fatal("interrupted create left unexpected directory state")
			}
			if row.cut == "witness" && witnessErr != nil || row.cut != "witness" && !errors.Is(witnessErr, os.ErrNotExist) {
				t.Fatal("interrupted create left unexpected witness state")
			}
			retainedAssertRecords(t, f, checkpointRecords[:row.records])
			if receipt.CleanupIncomplete != (row.cut == "copy" || row.cut == "inventory") {
				t.Fatal("interrupted create misreported its unattempted discard")
			}
		})
	}
}
