package localcell

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var drillRootPlatformRows, drillRootPlatformDeleteRows = func(*testing.T) {}, func(*testing.T) {}
var drillRootLink = func(t *testing.T, _, _ string) { t.Skip("drill link fixture needs platform support") }
var drillRootLock = func(t *testing.T, _ string) func() {
	t.Skip("drill lock fixture needs platform support")
	return nil
}

func drillRootFixture(t *testing.T) (*Cell, drillRecord) {
	t.Helper()
	f, _ := checkpointFixture(t, "legacy", false)
	v := drillFixture()
	v.Binding.InstallationID = f.manifest.InstallationID
	_, err := drillParseRecord(drillJSON(t, v))
	drillAssert(t, err == nil, "cannot prepare the drill root fixture")
	return f.cell, v
}

func drillRootPlant(t *testing.T, c *Cell, name string, v drillRecord) string {
	t.Helper()
	dir := filepath.Join(c.recoveryDir(), name)
	for _, path := range []string{c.recoveryDir(), dir, filepath.Join(dir, "pg")} {
		drillAssert(t, privateDir(path, true) == nil, "cannot prepare the drill root fixture")
	}
	data := drillJSON(t, v)
	_, err := drillParseRecord(data)
	drillAssert(t, err == nil && writeAtomicBytes(filepath.Join(dir, drillRootRecordName), data, nil) == nil &&
		writeAtomicBytes(filepath.Join(dir, "pg", "payload"), []byte("inert payload"), nil) == nil &&
		c.validateRecovery() == nil, "cannot prepare the drill root fixture")
	return dir
}

// drillRootStages counts the delete's stage calls and passes each to act.
func drillRootStages(c *Cell, act func(stage string, call int)) map[string]int {
	calls := map[string]int{}
	c.drillRootTest = &drillRootOperations{stage: func(stage string) {
		calls[stage]++
		act(stage, calls[stage])
	}}
	return calls
}

// drillRootAt runs act on one call of stage, or on its first ten calls when call is zero.
func drillRootAt(c *Cell, stage string, call int, act func()) map[string]int {
	return drillRootStages(c, func(name string, n int) {
		if name == stage && (n == call || call == 0 && n <= 10) {
			act()
		}
	})
}

func drillRootRefused(t *testing.T, c *Cell, name, reason, message string) {
	t.Helper()
	prior := c.drillRootTest
	defer func() { c.drillRootTest = prior }()
	calls, before := drillRootAt(c, "", 0, nil), checkpointTreeState(t, c)
	drillAssert(t, c.deleteDrillLocked(name) == (drillRootError{reason: reason}) && calls["sweep"] == 0 && calls["directory"] == 0 &&
		reflect.DeepEqual(before, checkpointTreeState(t, c)), message)
}

func drillRootBystanders(t *testing.T, c *Cell, v drillRecord) map[string]retainedEntry {
	t.Helper()
	state := checkpointTreeState(t, c)
	prefix := "recovery/" + v.DrillID
	for name := range state {
		if name == prefix || strings.HasPrefix(name, prefix+"/") {
			delete(state, name)
		}
	}
	return state
}

func drillRootAddBystanders(t *testing.T, c *Cell, v drillRecord) {
	t.Helper()
	checkpointPlant(t, c, v.Binding.CheckpointID, v.Binding.CheckpointID, "pg/payload")
	drillAssert(t, writeAtomicBytes(filepath.Join(c.recoveryDir(), "unknown"), []byte("bystander"), nil) == nil,
		"cannot prepare the drill root fixture")
	v.DrillID, v.Binding.InstallationID = "drill_bbbbbbbbbbbbbbbb", "another_installation"
	drillRootPlant(t, c, v.DrillID, v)
}

// drillRootKept reports whether dir still holds v's record and payload.
func drillRootKept(t *testing.T, dir string, v drillRecord) bool {
	record, recordErr := os.ReadFile(filepath.Join(dir, drillRootRecordName))
	payload, payloadErr := os.ReadFile(filepath.Join(dir, "pg", "payload"))
	return recordErr == nil && payloadErr == nil && string(record) == string(drillJSON(t, v)) && string(payload) == "inert payload"
}

func drillRootGone(c *Cell, name string) bool {
	_, err := os.Lstat(filepath.Join(c.recoveryDir(), name))
	return os.IsNotExist(err)
}

func drillRootWant(t *testing.T, c *Cell, declared, remainder, foreign, unknown int, message string) {
	t.Helper()
	before := checkpointTreeState(t, c)
	l, err := c.drillRootList()
	drillAssert(t, err == nil && len(l.declared) == declared && len(l.remainder) == remainder &&
		l.foreign == foreign && l.unknown == unknown && reflect.DeepEqual(before, checkpointTreeState(t, c)), message)
}

func TestDrillRoot(t *testing.T) {
	t.Run("record_limit", func(t *testing.T) {
		c, v := drillRootFixture(t)
		dir := drillRootPlant(t, c, v.DrillID, v)
		data := drillJSON(t, v)
		data = append(data, []byte(strings.Repeat(" ", drillMaxBytes-len(data)))...)
		drillAssert(t, writeAtomicBytes(filepath.Join(dir, drillRootRecordName), data, nil) == nil, "cannot prepare the drill root fixture")
		drillRootWant(t, c, 1, 0, 0, 0, "drill root accepted an oversize valid prefix")
		drillAssert(t, writeAtomicBytes(filepath.Join(dir, drillRootRecordName), append(data, ' '), nil) == nil, "cannot prepare the drill root fixture")
		drillRootWant(t, c, 0, 0, 0, 1, "drill root accepted an oversize valid prefix")
		drillRootRefused(t, c, v.DrillID, "unknown_kind", "drill root accepted an oversize valid prefix")
	})
	t.Run("classify_not_private", func(t *testing.T) {
		v := drillFixture()
		dir := filepath.Join(t.TempDir(), "ordinary")
		drillAssert(t, os.Mkdir(dir, 0o755) == nil && privateDir(dir, false) != nil &&
			testWriteFile(filepath.Join(dir, drillRootRecordName), drillJSON(t, v), 0o600) == nil, "cannot prepare the drill root fixture")
		kind, _, _ := drillRootClassify(dir, v.DrillID, v.Binding.InstallationID)
		drillAssert(t, kind == "unknown", "drill root classified a directory that is not private")
	})
	t.Run("declared", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootWant(t, c, 0, 0, 0, 0, "drill root listing was not exact")
		dir := drillRootPlant(t, c, v.DrillID, v)
		resolved, err := c.drillDir(v.DrillID)
		drillAssert(t, err == nil && resolved == dir && (drillRootError{reason: "absent"}).Error() ==
			(drillRootError{reason: "running"}).Error(), "drill root listing was not exact")
		drillRootWant(t, c, 1, 0, 0, 0, "drill root listing was not exact")
		listing, err := c.drillRootList()
		drillAssert(t, err == nil && reflect.DeepEqual(listing.declared, []string{v.DrillID}), "drill root listing was not exact")
		summaries, unknown, err := c.listCheckpoints(context.Background())
		drillAssert(t, err == nil && len(summaries) == 0 && unknown == 1, "drill root changed the checkpoint listing")
		visits, err := recordPrivateTreeWalk(t, c.cfg.Root, nil)
		drillAssert(t, err == nil, "drill root listing was not exact")
		for _, visit := range visits {
			drillAssert(t, !strings.HasPrefix(visit.path, "recovery/"), "drill root listing was not exact")
		}
	})
	t.Run("alias", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootPlant(t, c, strings.ToUpper(v.DrillID), v)
		if _, err := os.Lstat(filepath.Join(c.recoveryDir(), v.DrillID)); os.IsNotExist(err) {
			drillRootPlant(t, c, v.DrillID, v)
		}
		before := checkpointTreeState(t, c)
		l, err := c.drillRootList()
		drillAssert(t, err == nil && l.unknown >= 1 && reflect.DeepEqual(before, checkpointTreeState(t, c)), "drill root alias was not refused")
		drillRootRefused(t, c, v.DrillID, "alias", "drill root alias was not refused")
	})
	t.Run("checkpoint_target", func(t *testing.T) {
		c, v := drillRootFixture(t)
		dir := checkpointPlant(t, c, v.Binding.CheckpointID, v.Binding.CheckpointID)
		drillAssert(t, writeAtomicBytes(filepath.Join(dir, drillRootRecordName), drillJSON(t, v), nil) == nil, "cannot prepare the drill root fixture")
		drillRootWant(t, c, 0, 0, 0, 0, "drill root accepted a checkpoint target")
		_, err := c.drillDir(v.Binding.CheckpointID)
		drillAssert(t, err == (drillRootError{reason: "identity_invalid"}), "drill root accepted a checkpoint target")
		drillRootRefused(t, c, v.Binding.CheckpointID, "checkpoint_target", "drill root accepted a checkpoint target")
	})
	t.Run("foreign_owner", func(t *testing.T) {
		c, v := drillRootFixture(t)
		v.Binding.InstallationID = "another_installation"
		drillRootPlant(t, c, v.DrillID, v)
		drillRootWant(t, c, 0, 0, 1, 0, "drill root accepted a foreign owner")
		drillRootRefused(t, c, v.DrillID, "foreign_owner", "drill root accepted a foreign owner")
	})
	for _, kind := range []string{"file", "missing", "unparseable", "mismatch", "oversize"} {
		t.Run("unknown_kind/"+kind, func(t *testing.T) {
			c, v := drillRootFixture(t)
			dir := drillRootPlant(t, c, v.DrillID, v)
			var err error
			switch kind {
			case "file":
				err = os.RemoveAll(dir)
				if err == nil {
					err = writeAtomicBytes(dir, []byte("private regular file"), nil)
				}
			case "missing":
				err = os.Remove(filepath.Join(dir, drillRootRecordName))
			case "unparseable":
				err = writeAtomicBytes(filepath.Join(dir, drillRootRecordName), []byte("invalid record"), nil)
			case "mismatch":
				v.DrillID = "drill_bbbbbbbbbbbbbbbb"
				err = writeAtomicBytes(filepath.Join(dir, drillRootRecordName), drillJSON(t, v), nil)
			case "oversize":
				err = writeAtomicBytes(filepath.Join(dir, drillRootRecordName), []byte(strings.Repeat(" ", drillMaxBytes+1)), nil)
			}
			drillAssert(t, err == nil && c.validateRecovery() == nil, "cannot prepare the drill root fixture")
			drillRootWant(t, c, 0, 0, 0, 1, "drill root accepted an unknown kind")
			drillRootRefused(t, c, drillFixture().DrillID, "unknown_kind", "drill root accepted an unknown kind")
		})
	}
	// The same file put at the drill's name after admission, at the open stage
	// where the Unix row fifo_name plants its FIFO: the held open refuses it.
	t.Run("unknown_kind/file_name", func(t *testing.T) {
		c, v := drillRootFixture(t)
		dir, aside := drillRootPlant(t, c, v.DrillID, v), c.path("aside")
		var planted map[string]retainedEntry
		calls := drillRootAt(c, "open", 1, func() {
			drillAssert(t, os.Rename(dir, aside) == nil && writeAtomicBytes(dir, []byte("private regular file"), nil) == nil,
				"cannot prepare the drill root fixture")
			planted = checkpointTreeState(t, c)
		})
		err := c.deleteDrillLocked(v.DrillID)
		data, readErr := os.ReadFile(dir)
		drillAssert(t, err == (drillRootError{reason: "unknown_kind"}) && calls["sweep"] == 0 && reflect.DeepEqual(planted, checkpointTreeState(t, c)) &&
			drillRootKept(t, aside, v) && readErr == nil && string(data) == "private regular file", "drill delete removed or followed a file put at the drill name")
	})
	t.Run("remainder", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillAssert(t, privateDir(c.recoveryDir(), true) == nil && privateDir(filepath.Join(c.recoveryDir(), v.DrillID), true) == nil,
			"cannot prepare the drill root fixture")
		drillRootWant(t, c, 0, 1, 0, 0, "drill root remainder was not handled")
		listing, err := c.drillRootList()
		drillAssert(t, err == nil && reflect.DeepEqual(listing.remainder, []string{v.DrillID}), "drill root remainder was not handled")
	})
	t.Run("manifest_absent", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootPlant(t, c, v.DrillID, v)
		drillAssert(t, os.Remove(c.path("manifest.json")) == nil, "cannot prepare the drill root fixture")
		before := checkpointTreeState(t, c)
		_, err := c.drillRootList()
		drillAssert(t, err == (drillRootError{reason: "boundary_refused"}) && reflect.DeepEqual(before, checkpointTreeState(t, c)), "drill root listing was not exact")
	})
	drillRootPlatformRows(t)
}

func TestDeleteDrill(t *testing.T) {
	for _, state := range []string{"reserved", "copied", "verified", "launched", "probed", "stopped"} {
		t.Run("running/"+state, func(t *testing.T) {
			c, v := drillRootFixture(t)
			v.State = state
			drillRootPlant(t, c, v.DrillID, v)
			if state == "launched" || state == "probed" {
				drillRootRefused(t, c, v.DrillID, "running", "drill delete skipped the stopped proof")
			} else {
				drillAssert(t, c.deleteDrillLocked(v.DrillID) == nil && drillRootGone(c, v.DrillID), "drill delete did not remove the drill")
			}
		})
	}
	t.Run("wrong_id", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootPlant(t, c, v.DrillID, v)
		for _, name := range []string{"", "drill_", strings.ToUpper(v.DrillID), v.DrillID[:21], v.DrillID + "a", "drill_abcdef/hijklmnop", ".."} {
			drillRootRefused(t, c, name, "identity_invalid", "drill delete accepted a wrong identifier")
		}
		drillRootRefused(t, c, "drill_bbbbbbbbbbbbbbbb", "absent", "drill delete accepted a wrong identifier")
	})
	t.Run("bystander", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootPlant(t, c, v.DrillID, v)
		drillRootAddBystanders(t, c, v)
		before, calls := drillRootBystanders(t, c, v), drillRootAt(c, "", 0, nil)
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == nil && drillRootGone(c, v.DrillID) && c.validateRecovery() == nil &&
			reflect.DeepEqual(before, drillRootBystanders(t, c, v)), "drill delete touched a bystander")
		drillAssert(t, reflect.DeepEqual(calls, map[string]int{"open": 1, "sweep": 2, "directory": 1}), "drill delete broke the stage order")
	})
	t.Run("remainder", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillAssert(t, privateDir(c.recoveryDir(), true) == nil && privateDir(filepath.Join(c.recoveryDir(), v.DrillID), true) == nil,
			"cannot prepare the drill root fixture")
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == nil && drillRootGone(c, v.DrillID), "drill root remainder was not handled")
	})
	t.Run("journal_pending", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootPlant(t, c, v.DrillID, v)
		drillAssert(t, c.begin(context.Background(), "stop") == nil, "cannot prepare the drill root fixture")
		drillRootRefused(t, c, v.DrillID, "journal_pending", "drill delete ignored a pending journal")
	})
	t.Run("journal_torn", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootPlant(t, c, v.DrillID, v)
		data := append(retainedRead(t, c, "journal/operations.jsonl"), []byte(`{"operation":`)...)
		drillAssert(t, writeAtomicBytes(c.path("journal", "operations.jsonl"), data, nil) == nil, "cannot prepare the drill root fixture")
		drillRootRefused(t, c, v.DrillID, "journal_pending", "drill delete ignored a pending journal")
	})
	t.Run("journal_before_boundary", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootPlant(t, c, v.DrillID, v)
		drillAssert(t, c.begin(context.Background(), "stop") == nil && os.RemoveAll(c.recoveryDir()) == nil && testWriteFile(c.recoveryDir(), []byte("boundary fault"), 0o600) == nil, "cannot prepare the drill root fixture")
		drillAssert(t, c.validateRecovery() != nil, "cannot prepare the drill root fixture")
		drillRootRefused(t, c, v.DrillID, "journal_pending", "drill delete ignored a pending journal")
	})
	t.Run("manifest_absent", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootPlant(t, c, v.DrillID, v)
		drillAssert(t, os.Remove(c.path("manifest.json")) == nil, "cannot prepare the drill root fixture")
		drillRootRefused(t, c, v.DrillID, "boundary_refused", "drill root listing was not exact")
	})
	for index, stage := range []string{"payload", "boundary"} {
		t.Run("sync_failure/"+stage, func(t *testing.T) {
			c, v := drillRootFixture(t)
			drillRootPlant(t, c, v.DrillID, v)
			original, calls := drillRootSync, 0
			t.Cleanup(func() { drillRootSync = original })
			drillRootSync = func(r *os.Root) error {
				if calls++; calls == index+1 {
					return drillRootError{reason: "injected"}
				}
				return original(r)
			}
			drillAssert(t, c.deleteDrillLocked(v.DrillID) == (drillRootError{reason: "remove_failed"}) && calls == index+1, "drill delete hid a failed removal")
			drillRootWant(t, c, 1-index, 0, 0, 0, "drill delete lost the record before the payload")
			drillAssert(t, index == 0 || drillRootGone(c, v.DrillID), "drill delete hid a failed removal")
		})
	}
	t.Run("locked_file", func(t *testing.T) {
		c, v := drillRootFixture(t)
		dir := drillRootPlant(t, c, v.DrillID, v)
		drillRootAddBystanders(t, c, v)
		before, release := drillRootBystanders(t, c, v), func() {}
		drillRootAt(c, "sweep", 1, func() { release = drillRootLock(t, filepath.Join(dir, "pg", "payload")) })
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == (drillRootError{reason: "remove_failed"}), "drill delete hid a failed removal")
		release()
		drillRootWant(t, c, 1, 0, 1, 0, "drill delete lost the record before the payload")
		drillAssert(t, reflect.DeepEqual(before, drillRootBystanders(t, c, v)), "drill delete touched a bystander")
		c.drillRootTest = nil
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == nil && drillRootGone(c, v.DrillID), "drill delete did not remove the drill")
	})
	t.Run("boundary_absent", func(t *testing.T) {
		c, v := drillRootFixture(t)
		_, err := os.Lstat(c.recoveryDir())
		drillAssert(t, os.IsNotExist(err), "cannot prepare the drill root fixture")
		drillRootRefused(t, c, v.DrillID, "boundary_refused", "drill delete followed a replaced recovery")
	})
	t.Run("recovery_replaced", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootPlant(t, c, v.DrillID, v)
		moved := c.path("moved")
		drillRootAt(c, "open", 1, func() {
			drillAssert(t, os.Rename(c.recoveryDir(), moved) == nil, "cannot prepare the drill root fixture")
			drillRootLink(t, c.recoveryDir(), moved)
		})
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == (drillRootError{reason: "boundary_refused"}) &&
			drillRootKept(t, filepath.Join(moved, v.DrillID), v), "drill delete followed a replaced recovery")
	})
	// A private file put at recovery's name at the open stage, where the Unix row
	// fifo_recovery plants its FIFO: refused, nothing removed.
	t.Run("file_recovery", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootPlant(t, c, v.DrillID, v)
		moved := c.path("moved")
		var planted map[string]retainedEntry
		calls := drillRootAt(c, "open", 1, func() {
			drillAssert(t, os.Rename(c.recoveryDir(), moved) == nil && testWriteFile(c.recoveryDir(), []byte("boundary fault"), 0o600) == nil,
				"cannot prepare the drill root fixture")
			planted = checkpointTreeState(t, c)
		})
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == (drillRootError{reason: "boundary_refused"}) && calls["sweep"] == 0 &&
			reflect.DeepEqual(planted, checkpointTreeState(t, c)) && drillRootKept(t, filepath.Join(moved, v.DrillID), v),
			"drill delete followed a file put at the recovery name")
	})
	t.Run("name_replaced", func(t *testing.T) {
		c, v := drillRootFixture(t)
		dir, aside := drillRootPlant(t, c, v.DrillID, v), c.path("aside")
		drillRootAt(c, "sweep", 1, func() {
			drillAssert(t, os.Rename(dir, aside) == nil && privateDir(dir, true) == nil &&
				testWriteFile(filepath.Join(dir, "kept"), []byte("kept payload"), 0o600) == nil, "cannot prepare the drill root fixture")
		})
		err := c.deleteDrillLocked(v.DrillID)
		kept, keptErr := os.ReadFile(filepath.Join(dir, "kept"))
		drillAssert(t, err == (drillRootError{reason: "boundary_refused"}) && drillRootKept(t, aside, v) && keptErr == nil &&
			string(kept) == "kept payload", "drill delete followed a replaced drill name")
	})
	t.Run("name_absent", func(t *testing.T) {
		c, v := drillRootFixture(t)
		dir, aside := drillRootPlant(t, c, v.DrillID, v), c.path("aside")
		drillRootAt(c, "sweep", 1, func() { drillAssert(t, os.Rename(dir, aside) == nil, "cannot prepare the drill root fixture") })
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == (drillRootError{reason: "boundary_refused"}) && drillRootKept(t, aside, v),
			"drill delete followed a replaced drill name")
	})
	t.Run("final_swap", func(t *testing.T) {
		c, v := drillRootFixture(t)
		dir := drillRootPlant(t, c, v.DrillID, v)
		drillRootAt(c, "directory", 1, func() {
			drillAssert(t, os.Rename(dir, c.path("aside")) == nil && testWriteFile(dir, []byte("replacement"), 0o600) == nil,
				"cannot prepare the drill root fixture")
		})
		err := c.deleteDrillLocked(v.DrillID)
		data, readErr := os.ReadFile(dir)
		drillAssert(t, err == (drillRootError{reason: "boundary_refused"}) && readErr == nil && string(data) == "replacement",
			"drill delete removed a replacement at the name")
	})
	t.Run("late_entries", func(t *testing.T) {
		c, v := drillRootFixture(t)
		dir := drillRootPlant(t, c, v.DrillID, v)
		calls := drillRootStages(c, func(stage string, call int) {
			late := ""
			if stage == "sweep" && call == 2 {
				_, err := os.Lstat(filepath.Join(dir, drillRootRecordName))
				drillAssert(t, err == nil, "drill delete lost the record before the payload")
				late = "late1"
			} else if stage == "directory" && call == 1 {
				entries, err := os.ReadDir(dir)
				drillAssert(t, err == nil && len(entries) == 0, "drill delete left a late entry behind")
				late = "late2"
			}
			drillAssert(t, late == "" || testWriteFile(filepath.Join(dir, late), []byte("late payload"), 0o600) == nil, "cannot prepare the drill root fixture")
		})
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == nil && drillRootGone(c, v.DrillID), "drill delete left a late entry behind")
		drillAssert(t, calls["sweep"] == 3 && calls["directory"] == 2, "drill delete broke the stage order")
	})
	t.Run("late_record", func(t *testing.T) {
		// After the record removal the directory loop's sweep removes every spelling of it.
		c, v := drillRootFixture(t)
		dir := drillRootPlant(t, c, v.DrillID, v)
		calls := drillRootAt(c, "directory", 1, func() {
			drillAssert(t, testWriteFile(filepath.Join(dir, drillRootRecordName), []byte("late payload"), 0o600) == nil, "cannot prepare the drill root fixture")
		})
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == nil && drillRootGone(c, v.DrillID) && calls["directory"] == 2,
			"drill delete left a late record behind")
	})
	for _, stage := range []string{"sweep", "directory"} {
		t.Run(stage+"_bound", func(t *testing.T) {
			c, v := drillRootFixture(t)
			dir := drillRootPlant(t, c, v.DrillID, v)
			calls := drillRootAt(c, stage, 0, func() {
				drillAssert(t, testWriteFile(filepath.Join(dir, "late"), []byte("late payload"), 0o600) == nil, "cannot prepare the drill root fixture")
			})
			drillAssert(t, c.deleteDrillLocked(v.DrillID) == (drillRootError{reason: "remove_failed"}) && calls[stage] == 4, "drill delete did not bound its sweeps")
			if stage == "sweep" {
				drillRootWant(t, c, 1, 0, 0, 0, "drill delete did not bound its sweeps")
			} else {
				drillRootWant(t, c, 0, 1, 0, 0, "drill delete did not bound its sweeps")
			}
			c.drillRootTest = nil
			drillAssert(t, c.deleteDrillLocked(v.DrillID) == nil && drillRootGone(c, v.DrillID), "drill delete did not bound its sweeps")
		})
	}
	t.Run("record_locked", func(t *testing.T) {
		c, v := drillRootFixture(t)
		dir, release := drillRootPlant(t, c, v.DrillID, v), func() {}
		calls := drillRootAt(c, "sweep", 2, func() { release = drillRootLock(t, filepath.Join(dir, drillRootRecordName)) })
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == (drillRootError{reason: "remove_failed"}) && calls["directory"] == 0, "drill delete hid a failed removal")
		release()
		drillRootWant(t, c, 1, 0, 0, 0, "drill delete lost the record before the payload")
		c.drillRootTest = nil
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == nil && drillRootGone(c, v.DrillID), "drill delete did not remove the drill")
	})
	t.Run("record_spelling", func(t *testing.T) {
		c, v := drillRootFixture(t)
		dir := drillRootPlant(t, c, v.DrillID, v)
		drillAssert(t, os.Remove(filepath.Join(dir, drillRootRecordName)) == nil &&
			writeAtomicBytes(filepath.Join(dir, "Drill.json"), drillJSON(t, v), nil) == nil, "cannot prepare the drill root fixture")
		if _, err := os.Lstat(filepath.Join(dir, drillRootRecordName)); err != nil {
			drillRootWant(t, c, 0, 0, 0, 1, "drill root accepted an unknown kind")
			drillRootRefused(t, c, v.DrillID, "unknown_kind", "drill root accepted an unknown kind")
			return
		}
		listed := false
		drillRootAt(c, "sweep", 2, func() {
			entries, err := os.ReadDir(dir)
			listed = err == nil && len(entries) == 1 && entries[0].Name() == "Drill.json"
		})
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == nil && listed && drillRootGone(c, v.DrillID), "drill delete did not remove the drill")
	})
	drillRootPlatformDeleteRows(t)
}
