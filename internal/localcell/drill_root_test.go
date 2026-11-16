package localcell

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var drillRootPlatformRows = func(*testing.T) {}
var drillRootLockPayload = func(t *testing.T, _ string) func() {
	t.Skip("drill removal fixture needs platform support")
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

func drillRootRefused(t *testing.T, c *Cell, name, reason, message string) {
	t.Helper()
	prior, calls := c.drillRootTest, 0
	c.drillRootTest = &drillRootOperations{beforeRemove: func(string) { calls++ }}
	defer func() { c.drillRootTest = prior }()
	before := checkpointTreeState(t, c)
	drillAssert(t, c.deleteDrillLocked(name) == (drillRootError{reason: reason}) && calls == 0 &&
		reflect.DeepEqual(before, checkpointTreeState(t, c)), message)
}

func drillRootWant(t *testing.T, c *Cell, declared, remainder, foreign, unknown int, message string) {
	t.Helper()
	before := checkpointTreeState(t, c)
	l, err := c.drillRootList()
	drillAssert(t, err == nil && len(l.declared) == declared && len(l.remainder) == remainder &&
		l.foreign == foreign && l.unknown == unknown && reflect.DeepEqual(before, checkpointTreeState(t, c)), message)
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

func TestDrillRoot(t *testing.T) {
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
		summaries, unknown, err := c.listCheckpoints()
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
	t.Run("remainder", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillAssert(t, privateDir(c.recoveryDir(), true) == nil && privateDir(filepath.Join(c.recoveryDir(), v.DrillID), true) == nil,
			"cannot prepare the drill root fixture")
		drillRootWant(t, c, 0, 1, 0, 0, "drill root remainder was not handled")
		listing, err := c.drillRootList()
		drillAssert(t, err == nil && reflect.DeepEqual(listing.remainder, []string{v.DrillID}), "drill root remainder was not handled")
	})
	drillRootPlatformRows(t)
}

func TestDeleteDrill(t *testing.T) {
	for _, state := range []string{"reserved", "copied", "verified", "launched", "probed", "stopped"} {
		t.Run("running/"+state, func(t *testing.T) {
			c, v := drillRootFixture(t)
			v.State = state
			dir := drillRootPlant(t, c, v.DrillID, v)
			if state == "launched" || state == "probed" {
				drillRootRefused(t, c, v.DrillID, "running", "drill delete skipped the stopped proof")
			} else {
				drillAssert(t, c.deleteDrillLocked(v.DrillID) == nil, "drill delete skipped the stopped proof")
				_, err := os.Lstat(dir)
				drillAssert(t, os.IsNotExist(err), "drill delete did not remove the drill")
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
		dir := drillRootPlant(t, c, v.DrillID, v)
		drillRootAddBystanders(t, c, v)
		before := drillRootBystanders(t, c, v)
		calls := 0
		c.drillRootTest = &drillRootOperations{beforeRemove: func(string) { calls++ }}
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == nil && calls == 1 && c.validateRecovery() == nil &&
			reflect.DeepEqual(before, drillRootBystanders(t, c, v)), "drill delete touched a bystander")
		_, err := os.Lstat(dir)
		drillAssert(t, os.IsNotExist(err), "drill delete did not remove the drill")
	})
	t.Run("remainder", func(t *testing.T) {
		c, v := drillRootFixture(t)
		dir := filepath.Join(c.recoveryDir(), v.DrillID)
		drillAssert(t, privateDir(c.recoveryDir(), true) == nil && privateDir(dir, true) == nil, "cannot prepare the drill root fixture")
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == nil, "drill root remainder was not handled")
		_, err := os.Lstat(dir)
		drillAssert(t, os.IsNotExist(err), "drill delete did not remove the drill")
	})
	t.Run("journal_pending", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootPlant(t, c, v.DrillID, v)
		drillAssert(t, c.begin("stop") == nil, "cannot prepare the drill root fixture")
		drillRootRefused(t, c, v.DrillID, "journal_pending", "drill delete ignored a pending journal")
	})
	t.Run("journal_torn", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootPlant(t, c, v.DrillID, v)
		data := append(retainedRead(t, c, "journal/operations.jsonl"), []byte(`{"operation":`)...)
		drillAssert(t, writeAtomicBytes(c.path("journal", "operations.jsonl"), data, nil) == nil, "cannot prepare the drill root fixture")
		drillRootRefused(t, c, v.DrillID, "journal_pending", "drill delete ignored a pending journal")
	})
	t.Run("manifest_absent", func(t *testing.T) {
		c, v := drillRootFixture(t)
		drillRootPlant(t, c, v.DrillID, v)
		drillAssert(t, os.Remove(c.path("manifest.json")) == nil, "cannot prepare the drill root fixture")
		before := checkpointTreeState(t, c)
		_, err := c.drillRootList()
		drillAssert(t, err == (drillRootError{reason: "boundary_refused"}) && reflect.DeepEqual(before, checkpointTreeState(t, c)), "drill root listing was not exact")
		drillRootRefused(t, c, v.DrillID, "boundary_refused", "drill root listing was not exact")
	})
	for index, stage := range []string{"payload", "boundary"} {
		t.Run("sync_failure/"+stage, func(t *testing.T) {
			c, v := drillRootFixture(t)
			drillRootPlant(t, c, v.DrillID, v)
			original, calls := syncCheckpointDirectory, 0
			t.Cleanup(func() { syncCheckpointDirectory = original })
			syncCheckpointDirectory = func(path string) error {
				calls++
				if calls == index+1 {
					return drillRootError{reason: "injected"}
				}
				return original(path)
			}
			drillAssert(t, c.deleteDrillLocked(v.DrillID) == (drillRootError{reason: "remove_failed"}) && calls == index+1, "drill delete hid a failed removal")
			drillRootWant(t, c, 1-index, 0, 0, 0, "drill delete lost the record before the payload")
		})
	}
	t.Run("locked_file", func(t *testing.T) {
		c, v := drillRootFixture(t)
		dir := drillRootPlant(t, c, v.DrillID, v)
		drillRootAddBystanders(t, c, v)
		before := drillRootBystanders(t, c, v)
		var release func()
		calls := 0
		c.drillRootTest = &drillRootOperations{beforeRemove: func(path string) { calls++; release = drillRootLockPayload(t, path) }}
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == (drillRootError{reason: "remove_failed"}) && calls == 1, "drill delete hid a failed removal")
		release()
		drillRootWant(t, c, 1, 0, 1, 0, "drill delete lost the record before the payload")
		drillAssert(t, reflect.DeepEqual(before, drillRootBystanders(t, c, v)), "drill delete touched a bystander")
		c.drillRootTest = nil
		drillAssert(t, c.deleteDrillLocked(v.DrillID) == nil, "drill delete did not remove the drill")
		_, err := os.Lstat(dir)
		drillAssert(t, os.IsNotExist(err), "drill delete did not remove the drill")
	})
}
