//go:build linux || darwin

package localcell

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Only the ordinary journal and its two held directories count. The append
// descriptor's flag distinguishes the post-write sync from a prefix sync.
type barrierTrace struct {
	events     []string
	fail       string
	readOnly   bool
	prefixSize int64
}

func traceBarrier(t *testing.T, c *Cell) *barrierTrace {
	t.Helper()
	trace := new(barrierTrace)
	file, directory, publish := syncFile, syncCheckpointRoot, syncJournalDirectory
	t.Cleanup(func() { syncFile, syncCheckpointRoot, syncJournalDirectory = file, directory, publish })
	visit := func(stage string) error {
		trace.events = append(trace.events, stage)
		if stage == trace.fail {
			return os.ErrInvalid
		}
		return nil
	}
	syncFile = func(f *os.File) error {
		if filepath.Clean(f.Name()) == c.path(repairLog) {
			flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
			drillAssert(t, err == nil, "cannot observe journal descriptor")
			stage := "file"
			if flags&unix.O_APPEND != 0 {
				stage = "append"
			} else {
				trace.readOnly = flags&unix.O_ACCMODE == unix.O_RDONLY
				info, err := f.Stat()
				drillAssert(t, err == nil, "cannot stat journal at prefix sync")
				trace.prefixSize = info.Size()
			}
			if err := visit(stage); err != nil {
				return err
			}
		}
		return file(f)
	}
	syncCheckpointRoot = func(d *os.Root) error {
		switch filepath.Clean(d.Name()) {
		case c.path("journal"):
			if err := visit("journal"); err != nil {
				return err
			}
		case c.cfg.Root:
			if err := visit("root"); err != nil {
				return err
			}
		}
		return directory(d)
	}
	syncJournalDirectory = func(path string) error {
		stage := "publish"
		if path == c.cfg.Root {
			stage = "publish-root"
		}
		if path == c.cfg.Root || path == c.path("journal") {
			if err := visit(stage); err != nil {
				return err
			}
		}
		return publish(path)
	}
	return trace
}

func barrierFixture(t *testing.T, verb string) (*retainedFixture, func() error) {
	t.Helper()
	f, _ := checkpointFixture(t, "legacy", false)
	if verb == "create" {
		return f, leftoverCreate(t, f)
	}
	checkpointPlant(t, f.cell, locklessTarget, "", "data")
	return f, leftoverDelete(t, f.cell, locklessTarget)
}

func checkBarrierRecords(t *testing.T, f *retainedFixture, verb string, pending bool) {
	t.Helper()
	want := checkpointRecords
	if verb == "delete" {
		want = checkpointDeleteRecords
	}
	if pending {
		records := retainedRead(t, f.cell, repairLog)
		drillAssert(t, bytes.HasPrefix(records, f.prefix), "retry lost a validated record")
		first := bytes.SplitAfterN(records[len(f.prefix):], []byte{'\n'}, 2)[0]
		drillAssert(t, journalDirectorySyncSingleRecord(first, "checkpoint", "operation", "done"), "leftover close was not the first append")
		want = slices.Concat(checkpointLeftoverClose, want)
	}
	retainedAssertRecords(t, f, want)
}

func TestCheckpointJournalBarrierAdmitted(t *testing.T) {
	for _, verb := range []string{"create", "delete"} {
		for _, state := range []string{"clean", "pending", "empty", "absent", "second-name", "mode-0640"} {
			t.Run(verb+"/"+state, func(t *testing.T) {
				f, call := barrierFixture(t, verb)
				switch state {
				case "second-name":
					drillAssert(t, os.Link(f.cell.path(repairLog), filepath.Join(t.TempDir(), "alias")) == nil, "cannot link fixture journal")
				case "mode-0640":
					drillAssert(t, os.Chmod(f.cell.path(repairLog), 0640) == nil, "cannot set fixture journal mode")
				case "pending":
					drillAssert(t, f.cell.begin(t.Context(), "checkpoint") == nil, "cannot prepare open checkpoint")
				case "empty":
					repairSeed(t, f.cell, "")
				case "absent":
					drillAssert(t, os.Remove(f.cell.path(repairLog)) == nil, "cannot remove fixture journal")
				}
				f.prefix = nil
				if state != "absent" {
					f.prefix = retainedRead(t, f.cell, repairLog)
				}
				trace := traceBarrier(t, f.cell)
				err := call()
				stages := slices.DeleteFunc(slices.Clone(trace.events), func(s string) bool { return s == "append" })
				if state == "absent" {
					drillAssert(t, slices.Equal(stages, []string{"publish", "publish-root"}), "barrier created an absent log")
					drillAssert(t, slices.Index(trace.events, "publish") == 1, "first journal name was acknowledged without directory sync")
					drillAssert(t, slices.Index(trace.events, "publish-root") == 2, "first publication did not sync the root before the next append")
				} else {
					drillAssert(t, slices.Equal(stages, []string{"file", "journal", "root"}), "barrier count changed for an existing log")
					drillAssert(t, slices.Index(trace.events, "append") == 3, "an append preceded the prefix sync")
					drillAssert(t, trace.readOnly, "prefix sync used a writable descriptor")
					drillAssert(t, trace.prefixSize == int64(len(f.prefix)), "an append preceded the prefix sync")
				}
				drillAssert(t, err == nil, "the admitted verb did not finish after repair")
				checkBarrierRecords(t, f, verb, state == "pending")
			})
		}
	}
}

// The first-file cut publishes a new directory and file by ordinary mkdir,
// create and short write, with no intervening directory or file sync. The
// pending cut follows a real committed begin. Each cut is physically visible.
func TestCheckpointJournalBarrierRetry(t *testing.T) {
	for _, verb := range []string{"create", "delete"} {
		for _, history := range []string{"zero", "pending"} {
			t.Run(verb+"/"+history, func(t *testing.T) {
				f, call := barrierFixture(t, verb)
				c := f.cell
				tail := repairRecord(t, "checkpoint", "operation", "started")
				flags := os.O_WRONLY | os.O_APPEND
				if history == "zero" {
					drillAssert(t, os.Rename(c.path("journal"), filepath.Join(t.TempDir(), "old")) == nil, "cannot retire fixture journal directory")
					drillAssert(t, os.Mkdir(c.path("journal"), 0700) == nil, "cannot plant a fresh journal directory")
					flags |= os.O_CREATE | os.O_EXCL
					f.prefix = nil
				} else {
					drillAssert(t, c.begin(t.Context(), "checkpoint") == nil, "cannot prepare open checkpoint")
					f.prefix = retainedRead(t, c, repairLog)
					tail = repairRecord(t, "checkpoint", "copy", "started")
				}
				file, err := os.OpenFile(c.path(repairLog), flags, 0600)
				drillAssert(t, err == nil, "cannot open cut fixture")
				n, err := file.WriteString(tail[:len(tail)/2])
				closed := file.Close()
				drillAssert(t, err == nil && n == len(tail)/2 && closed == nil, "cut did not reach its file boundary")
				trace := traceBarrier(t, c)
				trace.fail = "file"
				status, err := c.RepairCheckpointJournal(t.Context())
				drillAssert(t, slices.Equal(trace.events, []string{"file"}), "cut did not reach its file boundary")
				drillAssert(t, err != nil, "failed settlement claimed success")
				drillAssert(t, status == "", "repair reported status on failure")
				drillAssert(t, !trace.readOnly, "repair did not sync its truncate descriptor")
				assertFileBytes(t, c.path(repairLog), f.prefix, "retry lost a validated record")
				trace.fail, trace.events = "", nil
				err = call()
				if history == "zero" {
					drillAssert(t, slices.Contains(trace.events, "file"), "retry skipped the clean-file sync")
				}
				drillAssert(t, slices.Index(trace.events, "file") == 0, "an append preceded the prefix sync")
				drillAssert(t, slices.Index(trace.events, "journal") == 1, "first journal name was acknowledged without directory sync")
				drillAssert(t, slices.Index(trace.events, "root") == 2, "journal directory was acknowledged without root sync")
				drillAssert(t, slices.Index(trace.events, "append") == 3, "an append preceded the prefix sync")
				drillAssert(t, err == nil, "the admitted verb did not finish after repair")
				drillAssert(t, trace.readOnly, "prefix sync used a writable descriptor")
				drillAssert(t, trace.prefixSize == int64(len(f.prefix)), "an append preceded the prefix sync")
				checkBarrierRecords(t, f, verb, history == "pending")
			})
		}
	}
}

func TestCheckpointJournalBarrierFailure(t *testing.T) {
	for _, verb := range []string{"create", "delete"} {
		for i, stage := range []string{"file", "journal", "root"} {
			t.Run(verb+"/"+stage, func(t *testing.T) {
				f, call := barrierFixture(t, verb)
				drillAssert(t, f.cell.begin(t.Context(), "checkpoint") == nil, "cannot prepare open checkpoint")
				before := checkpointTreeState(t, f.cell)
				trace := traceBarrier(t, f.cell)
				trace.fail = stage
				d := ClassifyCheckpointFailure(call())
				drillAssert(t, !slices.Contains(trace.events, "append"), "sync failure allowed a journal append")
				drillAssert(t, slices.Equal(trace.events, []string{"file", "journal", "root"}[:i+1]), "barrier count changed for an existing log")
				drillAssert(t, d.Class == "local_checkpoint_operation_failed", "barrier failure changed diagnostic class")
				drillAssert(t, d.Reason == "", "barrier failure exposed a reason")
				drillAssert(t, !d.CleanupIncomplete, "barrier failure claimed incomplete cleanup")
				checkpointUnchanged(t, f.cell, before)
			})
		}
	}
}

func TestCheckpointJournalBarrierRefusal(t *testing.T) {
	for _, verb := range []string{"create", "delete"} {
		checkLateBarrierRefusals(t, verb)
		rows := []string{"dry", "ownership", "maintenance", "live", "torn", "foreign"}
		if verb == "create" {
			rows = append(rows, "incomplete", "limit")
		} else {
			rows = append(rows, "missing")
		}
		for _, row := range rows {
			t.Run(verb+"/"+row, func(t *testing.T) {
				f, call := barrierFixture(t, verb)
				c, class := f.cell, "local_checkpoint_state_unsupported"
				switch row {
				case "dry":
					drillAssert(t, os.Remove(c.path("run", "operation.lock")) == nil, "cannot remove fixture operation lock")
					class = "local_checkpoint_identity_mismatch"
					call = leftoverDelete(t, c, locklessAbsent)
					if verb == "create" {
						call = func() error { return c.Checkpoint(t.Context(), CheckpointOptions{}) }
					}
				case "ownership":
					locklessLock(t, c, ".owner.lock", true)
					class = "local_cell_locked"
				case "maintenance":
					updateTestFence(t, c)
					class = "local_maintenance_fence_active"
				case "live":
					start, alive, err := processIdentity(os.Getpid())
					drillAssert(t, err == nil && alive, "cannot identify fixture process")
					checkpointRecord(t, c, start)
				case "incomplete":
					checkpointPlant(t, c, locklessTarget, "", "data")
					class = "local_checkpoint_incomplete"
				case "limit":
					for range checkpointLimit {
						drillAssert(t, call() == nil, "cannot prepare complete checkpoint")
					}
					class = "local_checkpoint_limit_reached"
				case "missing":
					call = leftoverDelete(t, c, locklessAbsent)
					class = "local_checkpoint_identity_mismatch"
				}
				data := retainedRead(t, c, repairLog)
				switch row {
				case "foreign":
					data = []byte(repairRecord(t, "stop", "operation", "started"))
					repairSeed(t, c, string(data))
				case "torn":
					data = []byte("{")
					repairSeed(t, c, string(data))
				}
				trace := traceBarrier(t, c)
				d := ClassifyCheckpointFailure(call())
				drillAssert(t, len(trace.events) == 0, "admission refusal reached the barrier")
				drillAssert(t, d.Class == class, "checkpoint admission refusal changed class")
				assertFileBytes(t, c.path(repairLog), data, "refusal wrote journal bytes")
				if row == "dry" {
					_, err := os.Lstat(c.path("run", "operation.lock"))
					drillAssert(t, errors.Is(err, os.ErrNotExist), "refusal created a lock name")
				}
			})
		}
	}
}

// Inject at the last admission boundary of each released verb. The parent
// rejected these observations in begin, except the masked leftover, which
// already refused in closeCheckpointLeftover.
func checkLateBarrierRefusals(t *testing.T, verb string) {
	t.Helper()
	start := func(op string) string { return repairRecord(t, op, "operation", "started") }
	type row struct{ name, journal string }
	rows := []row{{"torn", "{"}, {"foreign", start("stop")}, {"foreign-torn", start("stop") + "{"},
		{"cleanup", ""}, {"cleanup-absent", ""}, {"cleanup-torn", "{"}, {"cleanup-masked", start("checkpoint")}}
	for _, op := range []string{"install", "start", "remove", "reinstall"} {
		rows = append(rows, row{op, start(op)})
	}
	for _, op := range []string{"checkpoint", "checkpoint_delete"} {
		rows = append(rows, row{op + "-torn", start(op) + "{"}, row{op + "-revert", start(op) + repairRecord(t, op, "revert_stop", "started")})
	}
	for _, row := range rows {
		t.Run(verb+"/late-"+row.name, func(t *testing.T) {
			f, call := barrierFixture(t, verb)
			c := f.cell
			var before map[string]retainedEntry
			plant := func() {
				if strings.HasPrefix(row.name, "cleanup") {
					f.manifest.Cleanup = &journalEntry{Operation: "remove", Step: "runtime_cleanup", State: "started", At: time.Now().UTC()}
					drillAssert(t, writeAtomic(c.path("manifest.json"), &f.manifest, nil) == nil, "cannot seed manifest cleanup")
				}
				if row.name == "cleanup-absent" {
					drillAssert(t, os.Remove(c.path(repairLog)) == nil, "cannot remove fixture journal")
				} else {
					repairSeed(t, c, row.journal)
				}
				before = checkpointTreeState(t, c)
			}
			if verb == "create" {
				c.checkpointTest.newID = func() (string, error) { plant(); return checkpointNewID() }
			} else {
				c.checkpointTest.beforeStep = func(stage, _ string) {
					if stage == "hold" {
						plant()
					}
				}
			}
			trace := traceBarrier(t, c)
			err := call()
			wantText, class := "local cell operation interrupted; run repair first", "local_checkpoint_operation_failed"
			if row.name == "cleanup-masked" {
				wantText, class = errCheckpointState.Error(), "local_checkpoint_state_unsupported"
			}
			drillAssert(t, before != nil, "checkpoint missed the admission boundary")
			drillAssert(t, len(trace.events) == 0, "admission refusal reached the barrier")
			drillAssert(t, err != nil && err.Error() == wantText, "checkpoint admission refusal changed text")
			d := ClassifyCheckpointFailure(err)
			drillAssert(t, d.Class == class, "checkpoint admission refusal changed class")
			drillAssert(t, d.Reason == "", "checkpoint admission refusal exposed a reason")
			drillAssert(t, !d.CleanupIncomplete, "checkpoint admission refusal claimed incomplete cleanup")
			checkpointUnchanged(t, c, before)
		})
	}
}

func TestCheckpointJournalBarrierObservation(t *testing.T) {
	for _, row := range []string{"stat-failed", "replaced"} {
		t.Run(row, func(t *testing.T) {
			f, _ := checkpointFixture(t, "legacy", false)
			c := f.cell
			repairSeed(t, c, "")
			prog, err := c.readJournal(t.Context())
			drillAssert(t, err == nil && prog.readFile != nil, "cannot observe fixture journal")
			switch row {
			case "stat-failed":
				held, err := os.Open(c.path(repairLog))
				drillAssert(t, err == nil, "cannot open fixture journal")
				drillAssert(t, held.Close() == nil, "cannot close fixture journal")
				prog.readFile, err = held.Stat()
				drillAssert(t, err != nil, "stat fixture did not fail")
			case "replaced":
				checkpointWrite(t, c, repairLog, "")
			}
			trace := traceBarrier(t, c)
			err = c.syncCheckpointJournalPrefix(t.Context(), prog)
			if row == "stat-failed" {
				drillAssert(t, err != nil, "failed metadata was treated as absence")
			}
			drillAssert(t, len(trace.events) == 0, "a different or unproven journal was synced")
			drillAssert(t, errors.Is(err, errCheckpointJournalIO), "unproven journal did not return storage failure")
		})
	}
}

// barrierLease gives a kept write-lease holder's take, broken and release
// (readers6_linux_test.go). Only Linux waits a lease out (waitOutLease).
var barrierLease = func(t *testing.T) (func(string) bool, func() bool, func()) {
	t.Skip("rebound fixture needs an open that waits out a lease")
	return nil, nil, nil
}

// While the barrier's own open waits out a lease, the journal directory or the
// root moves and a hard-linked copy takes its name: the same file is reached
// through held directories no longer at their names. A leftover's manifest
// read, after its reader closed, is where the lease moves to the journal.
func TestCheckpointJournalBarrierRebound(t *testing.T) {
	for _, verb := range []string{"create", "delete"} {
		for _, moved := range []string{"journal", "root"} {
			t.Run(verb+"/"+moved, func(t *testing.T) {
				takeManifest, manifestBroken, releaseManifest := barrierLease(t)
				takeJournal, journalBroken, releaseJournal := barrierLease(t)
				// The holder keeps its lease until the row lets go, so nothing here waits for the
				// ceiling; a long one only leaves a loaded machine room to move the directory.
				leaseWait.ceiling = 10 * time.Second
				f, call := barrierFixture(t, verb)
				c, from := f.cell, f.cell.path("journal")
				if moved == "root" {
					from = c.cfg.Root
				}
				drillAssert(t, c.begin(t.Context(), "checkpoint") == nil, "cannot prepare open checkpoint")
				data := retainedRead(t, c, repairLog)
				first := true // the verb's goroutine only
				hold := func() {
					if first {
						first = false
						takeManifest(c.path("manifest.json"))
					}
				}
				if verb == "create" {
					c.checkpointTest.newID = func() (string, error) { hold(); return checkpointNewID() }
				} else {
					c.checkpointTest.beforeStep = func(stage, _ string) {
						if stage == "hold" {
							hold()
						}
					}
				}
				trace := traceBarrier(t, c)
				var refused error
				finished := make(chan struct{})
				go func() { defer close(finished); refused = call() }()
				t.Cleanup(func() { releaseManifest(); releaseJournal(); <-finished })
				seen := func(broken func() bool) bool {
					for end := time.Now().Add(leaseWait.ceiling); !broken() && time.Now().Before(end); {
						time.Sleep(leaseWait.first)
					}
					return broken()
				}
				drillAssert(t, seen(manifestBroken), "the leftover's manifest read did not meet the lease")
				drillAssert(t, takeJournal(c.path(repairLog)), "this kernel or sandbox refused the lease the row needs")
				releaseManifest()
				drillAssert(t, seen(journalBroken), "barrier open did not meet the lease")
				broke, aside := time.Now(), filepath.Join(t.TempDir(), "old")
				drillAssert(t, os.Rename(from, aside) == nil, "cannot move fixture directory aside")
				copied := filepath.WalkDir(aside, func(path string, entry fs.DirEntry, err error) error {
					switch rel, relErr := filepath.Rel(aside, path); {
					case err != nil || relErr != nil:
						return errors.Join(err, relErr)
					case entry.IsDir():
						return os.Mkdir(filepath.Join(from, rel), 0700)
					default:
						return os.Link(path, filepath.Join(from, rel))
					}
				})
				drillAssert(t, copied == nil, "cannot copy fixture directory with hard links")
				drillAssert(t, time.Since(broke) < leaseWait.ceiling/2, "barrier open could reach its lease ceiling before the release")
				releaseJournal()
				<-finished
				drillAssert(t, len(trace.events) == 0, "barrier synced a journal that was no longer at its name")
				d := ClassifyCheckpointFailure(refused)
				drillAssert(t, d.Class == "local_checkpoint_operation_failed", "barrier failure changed diagnostic class")
				drillAssert(t, d.Reason == "", "barrier failure exposed a reason")
				drillAssert(t, !d.CleanupIncomplete, "barrier failure claimed incomplete cleanup")
				assertFileBytes(t, c.path(repairLog), data, "refusal wrote journal bytes")
			})
		}
	}
}
