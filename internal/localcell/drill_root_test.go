package localcell

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var drillRootPlatformRows = func(*testing.T) {}

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
	})
	t.Run("checkpoint_target", func(t *testing.T) {
		c, v := drillRootFixture(t)
		dir := checkpointPlant(t, c, v.Binding.CheckpointID, v.Binding.CheckpointID)
		drillAssert(t, writeAtomicBytes(filepath.Join(dir, drillRootRecordName), drillJSON(t, v), nil) == nil, "cannot prepare the drill root fixture")
		drillRootWant(t, c, 0, 0, 0, 0, "drill root accepted a checkpoint target")
		_, err := c.drillDir(v.Binding.CheckpointID)
		drillAssert(t, err == (drillRootError{reason: "identity_invalid"}), "drill root accepted a checkpoint target")
	})
	t.Run("foreign_owner", func(t *testing.T) {
		c, v := drillRootFixture(t)
		v.Binding.InstallationID = "another_installation"
		drillRootPlant(t, c, v.DrillID, v)
		drillRootWant(t, c, 0, 0, 1, 0, "drill root accepted a foreign owner")
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
