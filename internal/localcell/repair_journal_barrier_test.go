//go:build linux || darwin || windows

package localcell

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
)

func TestRepairJournalBarrierAdmission(t *testing.T) {
	for _, row := range []string{"owner", "manifest", "maintenance", "journal", "mismatch", "discard", "no-pending", "no-pending-torn"} {
		t.Run(row, func(t *testing.T) {
			c, m := unitCell(t)
			locklessLock(t, c, ".owner.lock", row == "owner")
			data := repairRecord(t, "stop", "operation", "started")
			var wantErr, tail string
			var events []string
			switch row {
			case "owner":
				wantErr = ErrLocked.Error()
			case "manifest":
				m.LayoutVersion++
				wantErr = "unsupported local cell layout version"
			case "maintenance":
				updateTestFence(t, c)
				wantErr = errMaintenanceFenceActive.Error()
			case "journal":
				data, wantErr = "{\n", "invalid local cell journal; manual recovery required"
			case "mismatch":
				m.Cleanup = &journalEntry{Operation: "remove", Step: "runtime_cleanup", State: "started", At: m.CreatedAt}
				wantErr = "mismatched local cell journal operation"
			case "discard":
				wantErr, tail, events = "cannot sync repaired local cell journal", "{", []string{"file"}
			case "no-pending", "no-pending-torn":
				data += repairRecord(t, "stop", "operation", "done")
				if row == "no-pending-torn" {
					tail, events = "{", []string{"file"}
				}
			}
			drillAssert(t, writeAtomic(c.path("manifest.json"), m, nil) == nil, "cannot seed Repair admission manifest")
			repairSeed(t, c, data+tail)
			before := repairTree(t, c)
			trace := traceBarrier(t, c)
			if row == "discard" {
				trace.fail = "file"
			}
			err := c.Repair(t.Context())
			if wantErr == "" {
				drillAssert(t, err == nil, "Repair refused a journal without pending work")
			} else {
				drillAssert(t, err != nil && err.Error() == wantErr, "Repair admission error changed")
			}
			drillAssert(t, slices.Equal(trace.events, events), "Repair admission reached a prefix sync or append")
			drillAssert(t, !trace.prefix, "Repair synced a prefix without pending work")
			assertFileBytes(t, c.path(repairLog), []byte(data), "Repair admission changed the validated journal")
			drillAssert(t, maps.Equal(before, repairTree(t, c)), "Repair admission changed the cell")
		})
	}
}

func TestRepairJournalBarrierRetry(t *testing.T) {
	for _, row := range []string{"healthy", "hard-link", "file", "journal"} {
		t.Run(row, func(t *testing.T) {
			c, _ := unitCell(t)
			locklessLock(t, c, ".owner.lock", false)
			prefix := repairRecord(t, "checkpoint", "operation", "started")
			repairSeed(t, c, prefix)
			alias := filepath.Join(t.TempDir(), "alias")
			if row == "hard-link" {
				drillAssert(t, os.Link(c.path(repairLog), alias) == nil, "cannot link Repair journal")
			}
			trace := traceBarrier(t, c)
			if row == "file" || row == "journal" {
				repairSeed(t, c, prefix+"{")
				trace.fail = row
				_, err := c.RepairCheckpointJournal(t.Context())
				drillAssert(t, errors.Is(err, errCheckpointJournalIO), "checkpoint repair missed the cut")
				cut := []string{"file", "journal"}
				drillAssert(t, slices.Equal(trace.events, cut[:slices.Index(cut, row)+1]), "checkpoint repair failed at the wrong sync")
				assertFileBytes(t, c.path(repairLog), []byte(prefix), "checkpoint repair cut lost a validated record")
				trace.events, trace.fail = nil, ""
			}
			err := c.Repair(t.Context())
			drillAssert(t, err == nil, "Repair did not resume the validated prefix")
			drillAssert(t, slices.Equal(trace.events, slices.Concat(barrierStages, []string{"append", "append", "append"})), "Repair resumed before syncing the journal prefix exactly once")
			drillAssert(t, trace.prefixSize == int64(len(prefix)), "Repair wrote before the prefix sync")
			drillAssert(t, trace.prefix, "Repair synced the wrong prefix descriptor")
			admissionAssertJournalBytes(t, c, "checkpoint", []string{"revert_stop"})
			if row == "hard-link" {
				assertFileBytes(t, alias, retainedRead(t, c, repairLog), "Repair changed the hard-linked journal identity")
			}
		})
	}
}

func TestRepairJournalBarrierRecovery(t *testing.T) {
	for _, row := range []string{"record", "verify-all", "remove-stop", "database", "resume", "installed", "provision", "principal", "custody"} {
		if runtime.GOOS == "windows" && row == "remove-stop" {
			continue // The live stop witness starts a child process.
		}
		t.Run(row, func(t *testing.T) {
			c, m := unitCell(t)
			locklessLock(t, c, ".owner.lock", false)
			events := barrierStages
			trace := traceBarrier(t, c)
			call := func(stage string) func(context.Context, *Manifest) error {
				return func(context.Context, *Manifest) error {
					trace.events = append(trace.events, stage)
					if row == stage {
						return errPrincipalProvision
					}
					return nil
				}
			}
			c.installerPrincipals = &installerPrincipalOperations{resumePostgres: call("resume"), provision: call("provision"), credentials: call("principal"), verify: call("custody")}
			op, wantErr := "install", errPrincipalRepair.Error()
			steps := []string{"database"}
			switch row {
			case "record", "remove-stop":
				op, steps = "start", []string{"postgres"}
				pid := 123 // rejected before any process lookup
				wantErr = "invalid PostgreSQL recovery start time"
				if row == "remove-stop" {
					op, steps, wantErr = "remove", nil, "explicit PostgreSQL binary directory is required"
					child := signalSafetyChild(t)
					child.Root, child.Component, pid = c.cfg.Root, "postgres", child.PID
					drillAssert(t, writeAtomic(c.path("run", "postgres.json"), child, nil) == nil, "cannot seed Repair stop record")
					defer requireSafetyChildAlive(t, child)
				}
				drillAssert(t, testMkdir(c.path("pg"), 0700) == nil, "cannot seed Repair data directory")
				drillAssert(t, testWriteFile(c.path("pg", "postmaster.pid"), []byte(strconv.Itoa(pid)+"\n"+c.path("pg")+"\ninvalid\n"), 0600) == nil, "cannot seed Repair postmaster record")
			case "verify-all":
				op, steps, wantErr = "stop", nil, "invalid local cell process record"
				drillAssert(t, writeAtomic(c.path("run", "worker.json"), ProcessRecord{}, nil) == nil, "cannot seed Repair verification refusal")
			case "database":
				steps = nil
			case "installed", "resume":
				events = append(events, "resume")
				if row == "resume" {
					wantErr = errPrincipalProvision.Error()
				}
			case "provision":
				steps = append(steps, "principals")
				events, wantErr = append(events, "resume", "provision"), errPrincipalProvision.Error()
			case "principal":
				events = append(events, "resume", "append", "principal", "append")
				wantErr = "local cell install: principal_credentials failed: " + errPrincipalProvision.Error()
			case "custody":
				steps = append(steps, selectedInstallSteps...)
				events, wantErr = append(events, "resume", "provision", "custody"), errPrincipalProvision.Error()
			}
			if op == "install" {
				m.Installed = row == "installed"
				m.PrincipalLayoutVersion, m.PrincipalRoles = PrincipalLayoutVersion, installerRoleNames()
				drillAssert(t, writeAtomic(c.path("manifest.json"), m, nil) == nil, "cannot seed managed Repair manifest")
			}
			data := repairRecord(t, op, "operation", "started")
			for _, step := range steps {
				data += repairRecord(t, op, step, "done")
			}
			tail, stages := "{", []string{"journal", "file", "root"}
			if runtime.GOOS == "windows" {
				tail, stages = "", barrierStages // the tail discard is exercised by Admission/discard
			}
			repairSeed(t, c, data+tail)
			before := repairTree(t, c)
			for _, stage := range stages {
				trace.fail, trace.events = stage, nil
				err := c.Repair(t.Context())
				drillAssert(t, errors.Is(err, errSyncLifecycleJournal), "Repair acted before its failed prefix sync")
				observed := trace.events
				if stage == "journal" {
					drillAssert(t, len(observed) > 0 && observed[0] == "file", "Repair skipped its original tail discard")
					observed = observed[1:]
				}
				want := events[:slices.Index(events, stage)+1]
				drillAssert(t, slices.Equal(observed, want), "Repair continued after a failed prefix sync")
				drillAssert(t, maps.Equal(before, repairTree(t, c)), "failed Repair barrier changed the cell")
				assertFileBytes(t, c.path(repairLog), []byte(data), "failed Repair barrier changed the trimmed journal")
			}
			trace.fail, trace.events = "", nil
			err := c.Repair(t.Context())
			drillAssert(t, err != nil && err.Error() == wantErr, "Repair recovery error changed")
			drillAssert(t, slices.Equal(trace.events, events), "Repair recovery crossed the wrong sync or process boundary")
			drillAssert(t, maps.Equal(before, repairTree(t, c)), "Repair recovery failure changed non-journal state")
			if row == "principal" {
				f := &retainedFixture{cell: c, prefix: []byte(data)}
				retainedAssertRecords(t, f, []string{"install/principal_credentials/started", "install/principal_credentials/failed"})
			} else {
				assertFileBytes(t, c.path(repairLog), []byte(data), "Repair recovery refusal wrote journal bytes")
			}
		})
	}
}

// RepairService releases the owner lock between recovery and child shutdown.
func TestRepairServiceJournalBarriers(t *testing.T) {
	c, _ := unitCell(t)
	prepareSupervisorTest(t, c)
	repairSeed(t, c, repairRecord(t, "stop", "operation", "started"))
	trace := traceBarrier(t, c)
	if err := c.RepairService(t.Context()); err != nil {
		t.Fatal("service repair did not finish the interrupted stop")
	}
	prefix := barrierStages
	want := slices.Concat(prefix, []string{"append", "append", "append"}, prefix, []string{"append", "append", "append", "append"})
	if !slices.Equal(trace.events, want) {
		t.Fatal("service repair failed to settle each independently owned operation")
	}
}
