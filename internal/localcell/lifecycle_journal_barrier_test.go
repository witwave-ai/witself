//go:build linux || darwin

package localcell

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func lifecycleWriterFixture(t *testing.T, op string, binaries Binaries) (*retainedFixture, func() error) {
	t.Helper()
	switch op {
	case "install":
		c, _ := unitCell(t)
		c.cfg.Binaries, c.cfg.Timeout = binaries, 30*time.Second
		drillAssert(t, os.Remove(c.path("manifest.json")) == nil, "cannot prepare an uninstalled cell")
		lock, err := c.own(t.Context(), true)
		drillAssert(t, err == nil, "cannot prepare install owner lock")
		drillAssert(t, lock.Close() == nil, "cannot release install owner lock")
		repairSeed(t, c, repairRecord(t, "stop", "operation", "started")+repairRecord(t, "stop", "operation", "done"))
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
			defer cancel()
			if c.stopAll(ctx) != nil {
				t.Error("cannot stop synthetic install children")
			}
		})
		return &retainedFixture{cell: c, prefix: retainedRead(t, c, repairLog)}, func() error { return c.Install(t.Context(), InstallOptions{}) }
	case "reinstall":
		f, recorder := reinstallFixture(t, "legacy", false)
		return f, func() error {
			err := f.cell.Reinstall(t.Context(), reinstallOptions(f))
			f.launches = recorder.calls
			return err
		}
	}
	f := retainedRemovalFixture(t, "legacy", false)
	switch op {
	case "start":
		ports, err := f.cell.allocateAvailablePorts(f.manifest.Ports, "")
		drillAssert(t, err == nil, "cannot allocate available fixture ports")
		f.manifest.Ports = ports
		drillAssert(t, writeAtomic(f.cell.path("manifest.json"), &f.manifest, nil) == nil, "cannot publish fixture ports")
		return f, func() error { return f.cell.Start(t.Context()) }
	case "stop":
		return f, func() error { return f.cell.Stop(t.Context()) }
	default:
		return f, func() error { return f.cell.Remove(t.Context(), RemoveOptions{}) }
	}
}

// CheckLifecycleInstallBarrierForTest shares the external suite's synthetic
// PostgreSQL tools while recording the actual sync calls inside the package.
func CheckLifecycleInstallBarrierForTest(t *testing.T, binaries Binaries) {
	checkLifecycleWriterBarrier(t, "install", binaries)
}

func TestLifecycleJournalBarrier(t *testing.T) {
	for _, op := range []string{"start", "stop", "remove", "reinstall"} {
		t.Run(op, func(t *testing.T) { checkLifecycleWriterBarrier(t, op, Binaries{}) })
	}
}

func checkLifecycleWriterBarrier(t *testing.T, op string, binaries Binaries) {
	t.Helper()
	for _, state := range []string{"healthy", "second-name", "mode-0640", "retry-file", "retry-journal", "retry-empty", "pending", "torn", "file", "journal", "root"} {
		t.Run(state, func(t *testing.T) {
			f, call := lifecycleWriterFixture(t, op, binaries)
			c := f.cell
			trace := traceBarrier(t, c)
			switch state {
			case "retry-file", "retry-journal":
				cutCheckpointSettlement(t, f, trace, strings.TrimPrefix(state, "retry-"))
			case "retry-empty": // a torn first record, truncated to byte zero, its file sync failed
				f.prefix = nil
				cutCheckpointSettlement(t, f, trace, "file")
			case "second-name":
				drillAssert(t, os.Link(c.path(repairLog), filepath.Join(t.TempDir(), "alias")) == nil, "cannot link writer journal")
			case "mode-0640":
				drillAssert(t, os.Chmod(c.path(repairLog), 0640) == nil, "cannot set writer journal mode")
			case "pending", "torn":
				tail := repairRecord(t, "checkpoint", "operation", "started")
				if state == "torn" {
					tail = tail[:len(tail)/2]
				}
				repairSeed(t, c, string(f.prefix)+tail)
				before := retainedSnapshot(t, c)
				err := call()
				want := errJournalInterrupted
				if op == "reinstall" {
					want = errReinstallState
				}
				drillAssert(t, len(trace.events) == 0, "refused writer reached prefix sync")
				drillAssert(t, errors.Is(err, want), "journal refusal changed writer error")
				drillAssert(t, maps.Equal(before, retainedSnapshot(t, c)), "journal refusal changed writer tree")
				drillAssert(t, len(f.launches) == 0, "journal refusal ran a process operation")
				return
			case "file", "journal", "root":
				trace.fail = state
				before := retainedSnapshot(t, c)
				err := call()
				stages := []string{"file", "journal", "root"}
				drillAssert(t, checkpointSameError(err, "cannot make local cell journal durable"), "writer prefix failure changed storage error")
				drillAssert(t, slices.Equal(trace.events, stages[:slices.Index(stages, state)+1]), "writer continued past a failed prefix sync")
				drillAssert(t, maps.Equal(before, retainedSnapshot(t, c)), "writer prefix failure changed the cell")
				drillAssert(t, len(f.launches) == 0, "writer prefix failure ran a process operation")
				return
			}
			err := call()
			assertWriterPrefix(t, f, trace)
			drillAssert(t, err == nil, "admitted journal writer failed")
			assertLifecycleRecords(t, f, op)
		})
	}
}

func cutCheckpointSettlement(t *testing.T, f *retainedFixture, trace *barrierTrace, stage string) {
	t.Helper()
	tail := repairRecord(t, "checkpoint", "operation", "started")
	repairSeed(t, f.cell, string(f.prefix)+tail[:len(tail)/2])
	trace.fail = stage
	status, err := f.cell.RepairCheckpointJournal(t.Context())
	drillAssert(t, err != nil, "injected journal settlement succeeded")
	drillAssert(t, status == "", "interrupted settlement acknowledged a prefix")
	stages := []string{"file", "journal"}
	drillAssert(t, slices.Equal(trace.events, stages[:slices.Index(stages, stage)+1]), "repair did not reach the requested cut")
	assertFileBytes(t, f.cell.path(repairLog), f.prefix, "interrupted repair changed the validated prefix")
	trace.events, trace.fail = nil, ""
}

func assertWriterPrefix(t *testing.T, f *retainedFixture, trace *barrierTrace) {
	t.Helper()
	stages := slices.DeleteFunc(slices.Clone(trace.events), func(s string) bool { return s == "append" })
	drillAssert(t, slices.Equal(stages, []string{"file", "journal", "root"}), "writer did not settle the three prefix objects exactly once")
	drillAssert(t, slices.Index(trace.events, "append") == 3, "writer appended before the prefix was durable")
	drillAssert(t, trace.prefixSize == int64(len(f.prefix)), "writer changed journal bytes before the prefix sync")
	drillAssert(t, trace.readOnly, "writer settled the prefix through a writable descriptor")
}

func assertLifecycleRecords(t *testing.T, f *retainedFixture, op string) {
	t.Helper()
	steps := map[string][]string{
		"install":   {"manifest", "password", "initdb", "configure", "postgres", "database", "stop"},
		"start":     {"postgres", "server", "server_ready", "account", "worker", "worker_ready"},
		"stop":      {"stop"},
		"remove":    {"stop", "runtime_cleanup"},
		"reinstall": {"postgres", "verified", "stop", "readmitted"},
	}
	want := []string{op + "/operation/started"}
	for _, step := range steps[op] {
		want = append(want, op+"/"+step+"/started", op+"/"+step+"/done")
	}
	want = append(want, op+"/operation/done")
	retainedAssertRecords(t, f, want)
	suffix := retainedRead(t, f.cell, repairLog)[len(f.prefix):]
	entries, ok := journalDirectorySyncLines(suffix)
	drillAssert(t, ok, "writer emitted a malformed record")
	var canonical []byte
	for _, entry := range entries {
		drillAssert(t, entry.Cause == "", "successful writer recorded a failure cause")
		canonical = append(canonical, drillJSON(t, entry)...)
		canonical = append(canonical, '\n')
	}
	drillAssert(t, bytes.Equal(suffix, canonical), "writer changed the journal wire encoding")
}

func TestCheckpointJournalBarrierRetryBeforeDirectorySync(t *testing.T) {
	for _, verb := range []string{"create", "delete"} {
		t.Run(verb, func(t *testing.T) {
			f, call := barrierFixture(t, verb)
			trace := traceBarrier(t, f.cell)
			cutCheckpointSettlement(t, f, trace, "journal")
			err := call()
			assertWriterPrefix(t, f, trace)
			drillAssert(t, err == nil, "checkpoint failed after interrupted directory settlement")
			checkBarrierRecords(t, f, verb, false)
		})
	}
}

// A lease wait that its context ends stays a cut through both barrier
// functions, nothing synced or written; released, a live context settles all three.
func TestLifecycleJournalBarrierLeaseCut(t *testing.T) {
	take, _, release := barrierLease(t)
	leaseWait.ceiling = 10 * time.Second // the context ends the wait, never the ceiling
	f := retainedRemovalFixture(t, "legacy", false)
	progress, err := f.cell.readJournal(t.Context())
	drillAssert(t, err == nil, "cannot read the lifecycle journal before its lease")
	trace := traceBarrier(t, f.cell)
	drillAssert(t, take(f.cell.path(repairLog)), "cannot lease the lifecycle journal")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err = f.cell.syncLifecycleJournalPrefix(ctx, progress)
	cut, typed := err.(*leaseWaitCut)
	drillAssert(t, typed, "lifecycle barrier did not return the lease-wait cut unchanged")
	drillAssert(t, cut.cause == ctx.Err() && errors.Is(err, context.DeadlineExceeded), "lease-wait cut lost its context's cause")
	drillAssert(t, len(trace.events) == 0, "lifecycle barrier synced after a lease-wait cut")
	release() // before the blocking read below, which would wait out the kernel's break
	assertFileBytes(t, f.cell.path(repairLog), f.prefix, "lifecycle barrier wrote after a lease-wait cut")
	drillAssert(t, f.cell.syncLifecycleJournalPrefix(t.Context(), progress) == nil, "lifecycle barrier failed after its lease was released")
	drillAssert(t, slices.Equal(trace.events, []string{"file", "journal", "root"}), "released lifecycle barrier did not settle the three prefix objects")
}
