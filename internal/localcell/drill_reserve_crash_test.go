//go:build linux || windows

package localcell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// drillReserveCrashID is the one identifier every crash child mints.
const drillReserveCrashID = "drill_aaaaaaaaaaaaaaaa"

// TestDrillReservationChild is the crash rows' child and does nothing unless its variables name a cell and a cut: the
// checkpoint, a stage, its call and a mode ("after" counts calls from reserve_allowance on, "discard" first refuses the
// allowance's allocation with the platform's raw lack of space, "limit" lowers the file size limit to 128 KiB). It
// reserves with allowance 256 KiB in 64 KiB chunks and kills itself at the cut; a child that finishes prints one line.
// It never takes the cell's lock: the parent row holds it.
func TestDrillReservationChild(t *testing.T) {
	root, cut := os.Getenv("WITSELF_DRILL_RESERVE_CELL"), strings.Fields(os.Getenv("WITSELF_DRILL_RESERVE_CUT"))
	if root == "" || len(cut) != 4 {
		return
	}
	c, err := New(Config{Root: root})
	call, callErr := strconv.Atoi(cut[2])
	if err != nil || callErr != nil {
		fmt.Println("drill-child unopened false")
		return
	}
	allowance, counts := false, map[string]int{}
	drillReserveChunk, drillReserveNewID = 64<<10, func() (string, error) { return drillReserveCrashID, nil }
	drillReserveAllocate = func(f *os.File, offset, length uint64) error {
		if allowance && cut[3] == "discard" {
			return drillReserveFull
		}
		return drillPlatformAllocate(f, offset, length)
	}
	c.drillRootTest = &drillRootOperations{stage: func(stage string) {
		if allowance = allowance || stage == "reserve_allowance"; allowance || cut[3] != "after" {
			counts[stage]++
		}
		if stage == cut[1] && counts[stage] == call {
			drillReserveKill()
		}
	}}
	outcome, drillID := "unlimited", ""
	if restore := drillReserveLimit(cut[3] == "limit"); restore != nil {
		drillID, err = c.reserveDrillLocked(t.Context(), cut[0], "op_crash", 256<<10)
		if outcome = drillReserveOutcome(err); restore() != nil {
			outcome = "unrestored"
		}
	}
	fmt.Println("drill-child", outcome, drillID != "")
}

// drillReserveOutcome names a reservation's result by a fixed text per known value: reserved, or its closed reason
// (an incomplete cleanup unwraps to its cause).
func drillReserveOutcome(err error) string {
	var refused drillStorageError
	switch {
	case err == nil:
		return "reserved"
	case errors.As(err, &refused):
		return refused.reason
	}
	return "unclassified"
}

// drillReserveExpect fails with message, then the outcome err names (a fixed text per known value), unless ok.
func drillReserveExpect(t *testing.T, ok bool, err error, message string) {
	t.Helper()
	if cleanup := (drillCleanupError{}); !ok {
		t.Fatal(message + "; got " + map[bool]string{true: "an incomplete cleanup of "}[errors.As(err, &cleanup)] + drillReserveOutcome(err))
	}
}

// drillReserveListed names a listing by a fixed text per known kind.
func drillReserveListed(l drillRootListing, err error) string {
	switch n := len(l.declared) + len(l.remainder) + l.foreign + l.unknown; {
	case err != nil:
		return "a refused listing"
	case n != 1:
		return map[bool]string{true: "nothing", false: "several drills"}[n == 0]
	case l.declared != nil:
		return "declared"
	case l.remainder != nil:
		return "remainder"
	}
	return map[bool]string{true: "unknown", false: "foreign"}[l.unknown == 1]
}

// drillReserveChild runs the child on r's cell with cut (R6: the test binary, its coverage directory, an allowlisted
// environment) under a watchdog at half the time the package's timeout leaves, which kills it, so no child outlives
// the row; it returns only the child's fixed drill- lines, and its exit.
func drillReserveChild(t *testing.T, r *drillReserveRun, cut string) ([]string, error) {
	t.Helper()
	ctx, cover, args := t.Context(), t.TempDir(), []string{"-test.run=^TestDrillReservationChild$", "-test.count=1"}
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Until(deadline)/2)
		defer cancel()
	}
	if testing.CoverMode() != "" {
		args = append(args, "-test.gocoverdir="+cover)
	}
	child := exec.CommandContext(ctx, os.Args[0], args...)
	child.Env = []string{"GOCOVERDIR=" + cover, "WITSELF_DRILL_RESERVE_CELL=" + r.c.cfg.Root, "WITSELF_DRILL_RESERVE_CUT=" + r.id + " " + cut}
	for _, name := range map[bool][]string{true: {"SystemRoot", "TEMP", "TMP"}}[runtime.GOOS == "windows"] {
		child.Env = append(child.Env, name+"="+os.Getenv(name))
	}
	output, err := child.Output()
	drillAssert(t, ctx.Err() == nil, "the reservation's child outlived its watchdog")
	var lines []string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "drill-") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines, err
}

// drillReserveResidue reads a cut's residue through roots held on recovery/ and on the drill directory the no-follow
// walk reaches: admission refuses drill_outstanding, the directory holds only the row's one name when it names one,
// the space the file system allocated to its files is at least 128 KiB when charged, and the deletion removes it,
// except the record window's temporary (S3), which the deletion refuses and the row removes through those roots once
// inspected. The caller's tree comparison, a listing of the whole cell, shows that nothing is left.
func drillReserveResidue(t *testing.T, r *drillReserveRun, only string, charged bool) {
	t.Helper()
	_, err := r.c.admitDrillStorageLocked(t.Context(), r.id, 1)
	drillStorageReason(t, err, "drill_outstanding", "admission did not see the cut's residue")
	recovery, err := os.OpenRoot(r.c.recoveryDir())
	drillAssert(t, err == nil, "cannot hold the cut's recovery directory")
	defer func() { _ = recovery.Close() }()
	drill, err := drillReserveRoot(recovery, drillReserveCrashID)
	drillAssert(t, err == nil, "the cut's residue is not a directory the no-follow walk reaches")
	names, err := drillRootNames(drill)
	drillAssert(t, err == nil && (only == "" || slices.Equal(names, []string{only})), "the cut's residue does not hold exactly its one name")
	allocated := uint64(0)
	for _, entry := range append(slices.Clone(r.inventory.Entries), checkpointInventoryEntry{Path: "../" + drillReserveAllowance}) {
		if f, err := drillPlatformOpen(drill, path.Join(drillReserveClone, entry.Path), false); err == nil {
			allocated += drillReserveCharged(t, f)
			drillAssert(t, f.Close() == nil, "cannot close a file of the cut's residue")
		}
	}
	drillAssert(t, !charged || allocated >= 128<<10, "the cut's residue is not charged the space allocated to it")
	if only != drillReservePending {
		drillAssert(t, drill.Close() == nil && r.c.deleteDrillLocked(drillReserveCrashID) == nil, "the deletion did not remove the cut's residue")
		return
	}
	drillAssert(t, r.c.deleteDrillLocked(drillReserveCrashID) == error(drillRootError{reason: "unknown_kind"}), "the deletion did not refuse the record window's residue")
	info, err := drill.Lstat(drillReservePending)
	drillAssert(t, err == nil && info.Mode().IsRegular(), "the record window's residue is not its temporary file")
	drillAssert(t, drill.Remove(drillReservePending) == nil && drill.Close() == nil && recovery.Remove(drillReserveCrashID) == nil,
		"cannot remove the inspected residue through the held roots")
}

// The reservation's child killed at every durable step and at each step of its discard; then the exit, the listing,
// admission, the space charged, the deletion and the tree as before the reservation.
func TestDrillReservationCrash(t *testing.T) {
	for _, row := range []struct {
		name, cut, kind, only string // cut: stage, call, mode; kind: the residue's listing, none when empty
		charged               bool
	}{
		{"mkdir", "reserve_mkdir 1 -", "", "", false},
		{"record", "reserve_record 1 -", "remainder", "", false},
		{"publish", "reserve_publish 1 -", "unknown", drillReservePending, false}, // the writer open, the temporary synced
		{"tree", "reserve_tree 1 -", "declared", "", false},
		{"file", "reserve_file 1 -", "declared", "", false},
		{"chunk", "reserve_chunk 3 after", "declared", "", true},
		{"verify", "reserve_verify 1 after", "declared", "", true},
		{"sync", "reserve_sync 1 -", "declared", "", true},
		{"done", "reserve_done 1 -", "declared", "", true},
		{"discard_open", "open 1 discard", "declared", "", false},
		{"discard_sweep", "sweep 2 discard", "declared", drillRootRecordName, false},
		{"discard_directory", "directory 1 discard", "remainder", "", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := drillReserveStart(t)
			before := checkpointTreeState(t, r.c)
			lines, exit := drillReserveChild(t, r, row.cut)
			drillAssert(t, drillReserveKilled(exit), "the reservation's child was not killed at its cut; got ["+strings.Join(lines, ", ")+"]")
			drillAssert(t, len(lines) == 0, "the reservation's child printed a line at its cut")
			listing, err := r.c.drillRootList()
			ids := []string{drillReserveCrashID}
			want := map[string]drillRootListing{"declared": {declared: ids}, "remainder": {remainder: ids}, "unknown": {unknown: 1}}[row.kind]
			drillAssert(t, err == nil && reflect.DeepEqual(listing, want), "the cut's residue is not listed as its row says; got "+drillReserveListed(listing, err))
			if row.kind != "" {
				drillReserveResidue(t, r, row.only, row.charged)
			}
			drillAssert(t, reflect.DeepEqual(before, checkpointTreeState(t, r.c)), "the tree is not as before the reservation")
		})
	}
}
