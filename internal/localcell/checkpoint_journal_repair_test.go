//go:build linux || darwin

package localcell

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const repairLog = "journal/operations.jsonl"

func repairRecord(t *testing.T, op, step, state string) string {
	t.Helper()
	e := journalEntry{Operation: op, Step: step, State: state, At: time.Unix(1700000000, 123456789).UTC()}
	if state == "failed" {
		e.Cause = errCheckpointCopy.Error()
	}
	return string(drillJSON(t, e)) + "\n"
}

func repairSeed(t *testing.T, c *Cell, data string) {
	t.Helper()
	drillAssert(t, testWriteFile(c.path(repairLog), []byte(data), 0600) == nil, "cannot seed log")
}

// Check journal contents separately, retaining every name and mode here.
func repairTree(t *testing.T, c *Cell) map[string]retainedEntry {
	t.Helper()
	tree := checkpointTreeState(t, c)
	if e, ok := tree[repairLog]; ok {
		tree[repairLog] = retainedEntry{mode: e.mode}
	}
	return tree
}

func repairDiagnostic(t *testing.T, err error, class, failure string) {
	t.Helper()
	d := ClassifyCheckpointFailure(err)
	drillAssert(t, d.Class == class, failure)
	drillAssert(t, d.Reason == "", "repair diagnostic exposed a reason")
	drillAssert(t, !d.CleanupIncomplete, "repair claimed incomplete cleanup")
}

func TestRepairCheckpointJournalWriter(t *testing.T) {
	f, _ := checkpointFixture(t, "legacy", false)
	c := locklessDirs(t, locklessPurge(t, f.cell, ""), "journal")
	repairSeed(t, c, "")
	tree, outside := repairTree(t, c), heldOutside(t)
	kept := heldTree(t, outside)
	check := func(t *testing.T, prefix, tail string) {
		t.Helper()
		repairSeed(t, c, prefix+tail)
		status, err := c.RepairCheckpointJournal(t.Context())
		drillAssert(t, err == nil, "a released append prefix was refused")
		drillAssert(t, status == "tail_discarded", "eligible repair status changed")
		got := retainedRead(t, c, repairLog)
		drillAssert(t, bytes.HasPrefix(got, []byte(prefix)), "repair changed a validated byte")
		drillAssert(t, len(got) == len(prefix), "repair retained bytes beyond the cutoff")
		drillAssert(t, maps.Equal(tree, repairTree(t, c)), "repair changed a nonjournal object")
		drillAssert(t, maps.Equal(kept, heldTree(t, outside)), "repair changed a nonjournal object")
	}
	cuts := func(name, prefix, record string) {
		for n := 1; n < len(record); n++ {
			t.Run(name+"/"+strconv.Itoa(n), func(t *testing.T) { check(t, prefix, record[:n]) })
		}
	}
	for _, writer := range []struct{ op, steps string }{{"checkpoint", "copy inventory witness"}, {"checkpoint_delete", "unseal remove"}} {
		record := func(step, state string) string { return repairRecord(t, writer.op, step, state) }
		prefix := record("operation", "started")
		cuts(writer.op+"/begin", "", prefix)
		for _, step := range strings.Fields(writer.steps) {
			start, done, fail := record(step, "started"), record(step, "done"), record(step, "failed")
			cuts(writer.op+"/"+step+"/started", prefix, start)
			prefix += start
			cuts(writer.op+"/"+step+"/done", prefix, done)
			cuts(writer.op+"/"+step+"/failed", prefix, fail)
			cuts(writer.op+"/"+step+"/failure-close", prefix+fail, record("operation", "done"))
			prefix += done
		}
		cuts(writer.op+"/close", prefix, record("operation", "done"))
	}
	t.Run("only-padding", func(t *testing.T) { check(t, "", "\x00") })
	t.Run("streamed-padding", func(t *testing.T) { check(t, "", "{"+strings.Repeat("\x00", 131072)) })
}

func TestRepairCheckpointJournalStates(t *testing.T) {
	for _, row := range []struct{ name, want, tail string }{
		{"installed", "tail_discarded", "{"}, {"retained", "tail_discarded", "{"}, {"purged-final-delete", "tail_discarded", "{"},
		{"two-witnesses", "tail_discarded", "{"}, {"orphan-and-witness", "tail_discarded", "{"}, {"absent", "absent", ""}, {"absent-dir", "absent", ""},
		{"empty", "clean", ""}, {"completed", "clean", ""}, {"failed-sync", "clean", ""}, {"clean-pending", "clean", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, rec := checkpointFixture(t, "legacy", row.name == "retained")
			c, prefix := f.cell, string(f.prefix)
			begin := repairRecord(t, "checkpoint_delete", "operation", "started")
			switch row.name {
			case "two-witnesses", "orphan-and-witness":
				checkpointPlant(t, c, locklessTarget, locklessTarget, "sentinel")
				checkpointPlant(t, c, locklessAbsent, map[bool]string{true: locklessAbsent}[row.name == "two-witnesses"], "sentinel")
			case "purged-final-delete":
				locklessDirs(t, locklessPurge(t, c, ""), "journal")
				prefix = begin + repairRecord(t, "checkpoint_delete", "remove", "done")
			case "absent-dir":
				locklessPurge(t, c, "")
			case "empty":
				prefix = ""
			case "clean-pending":
				prefix += begin
			case "failed-sync":
				trace := traceJournalDirectorySync(t, c)
				failBegin := func() {
					trace.fileErr = os.ErrInvalid
					drillAssert(t, c.begin(t.Context(), "checkpoint") != nil, "cannot inject failed publication sync")
					trace.fileErr = nil
				}
				failBegin()
				obs, err := c.Inspect(t.Context())
				drillAssert(t, err == nil, "complete journal observation failed")
				drillAssert(t, !obs.JournalTailIncomplete, "a validated LF-terminated line was shortened")
				rec.nextID = func() (string, error) { return locklessTarget, nil }
				drillAssert(t, leftoverCreate(t, f)() == nil, "mutation changed the baseline refusal")
				drillAssert(t, leftoverDelete(t, c, locklessTarget)() == nil, "mutation changed the baseline refusal")
				failBegin()
				prefix = string(retainedRead(t, c, repairLog))
			}
			if row.name == "absent" {
				drillAssert(t, os.Remove(c.path(repairLog)) == nil, "cannot remove fixture log")
			} else if row.name != "failed-sync" && row.want != "absent" {
				repairSeed(t, c, prefix+row.tail)
			}
			if row.tail != "" {
				repairBaseline(t, f, row.name)
			}
			before := repairTree(t, c)
			status, err := c.RepairCheckpointJournal(t.Context())
			drillAssert(t, err == nil, "repair required an unrelated checkpoint or installation")
			drillAssert(t, string(status) == row.want, "clean journal status changed")
			after := repairTree(t, c)
			if row.name == "purged-final-delete" {
				_, manifest := after["manifest.json"]
				drillAssert(t, !manifest, "repair fabricated a manifest")
				p, err := c.readJournal(t.Context())
				drillAssert(t, err == nil, "settlement closed the previous operation")
				drillAssert(t, p.pending != nil, "settlement closed the previous operation")
				drillAssert(t, p.pending.Step == "remove", "repair concealed the pending purged deletion")
			}
			drillAssert(t, maps.Equal(before, after), "repair changed checkpoint contents")
			if _, exists := after[repairLog]; row.want == "absent" {
				drillAssert(t, !exists, "absent journal was created")
				return
			}
			assertFileBytes(t, c.path(repairLog), []byte(prefix), "a validated LF-terminated line was shortened")
			status, err = c.RepairCheckpointJournal(t.Context())
			drillAssert(t, err == nil, "clean journal status changed")
			drillAssert(t, status == "clean", "clean journal status changed")
			assertFileBytes(t, c.path(repairLog), []byte(prefix), "repeat appended a record")
		})
	}
}

func TestRepairCheckpointJournalRefusals(t *testing.T) {
	const state, bad = "local_checkpoint_state_unsupported", "local_checkpoint_operation_failed"
	const gate, header, corrupt = "noncheckpoint state was accepted", "repair accepted an ineligible suffix", "corrupt history was shortened"
	start := repairRecord(t, "checkpoint", "operation", "started")
	foreign := repairRecord(t, "stop", "operation", "started")
	deleteStep := repairRecord(t, "checkpoint_delete", "remove", "done")
	for _, row := range []struct{ name, data, class, failure string }{
		{"foreign", foreign, state, gate}, {"revert", start + repairRecord(t, "checkpoint", "revert_stop", "started"), state, gate},
		{"cleanup", "", state, gate}, {"cleanup-masked", start, state, gate}, {"malformed-manifest", "{", bad, "unreadable history was reported clean"},
		{"foreign-tail", strings.TrimSuffix(foreign, "\n"), state, header}, {"scalar", "42", state, header},
		{"conflict", start + strings.TrimSuffix(deleteStep, "\n"), state, header}, {"closed-step", strings.TrimSuffix(deleteStep, "\n"), state, header},
		{"malformed", "{\n", bad, corrupt}, {"unknown", repairRecord(t, "invalid", "operation", "started"), bad, corrupt},
		{"mismatched", start + deleteStep, bad, corrupt}, {"unfenced", deleteStep, bad, corrupt},
		{"oversized", strings.Repeat("x", 65537), bad, corrupt}, {"two-values", "{}{}", bad, corrupt},
		{"invalid-padding", "{\x00x", bad, corrupt}, {"space", " ", bad, corrupt},
		{"no-owner", "{", state, "repair bypassed the ownership gate"}, {"busy", "{", "local_cell_locked", "repair bypassed the ownership gate"},
		{"maintenance", "{", "local_maintenance_fence_active", "repair ignored maintenance"},
		{"bad-maintenance", "{", "local_maintenance_fence_active", "repair ignored maintenance"},
		{"live", "{", state, "repair admitted a noncold cell"}, {"unproven", "{", bad, "repair admitted a noncold cell"},
		{"pid", "{", state, "repair admitted a noncold cell"}, {"expired", "{", "local_checkpoint_interrupted", "cancelled repair shortened the log"},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, _ := checkpointFixture(t, "legacy", false)
			ctx, c := t.Context(), f.cell
			repairSeed(t, c, row.data)
			switch row.name {
			case "cleanup", "cleanup-masked":
				f.manifest.Cleanup = &journalEntry{Operation: "remove", Step: "runtime_cleanup", State: "started", At: time.Now().UTC()}
				drillAssert(t, writeAtomic(c.path("manifest.json"), &f.manifest, nil) == nil, "cannot seed manifest cleanup")
			case "malformed":
				repairBaseline(t, f, row.name)
			case "malformed-manifest":
				checkpointWrite(t, c, "manifest.json", "{")
			case "no-owner":
				drillAssert(t, os.Remove(c.path(".owner.lock")) == nil, "cannot remove fixture lock")
			case "busy":
				locklessLock(t, c, ".owner.lock", true)
			case "maintenance":
				updateTestFence(t, c)
			case "bad-maintenance":
				checkpointWrite(t, c, "journal/maintenance.json", "{")
			case "live":
				start, alive, err := processIdentity(os.Getpid())
				drillAssert(t, err == nil && alive, "cannot identify fixture process")
				checkpointRecord(t, c, start)
			case "unproven":
				checkpointRecord(t, c, "other-incarnation")
			case "pid":
				locklessStale(t, c)
			case "expired":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			status, err := c.RepairCheckpointJournal(ctx)
			repairDiagnostic(t, err, row.class, row.failure)
			drillAssert(t, status == "", "repair reported status on failure")
			assertFileBytes(t, c.path(repairLog), []byte(row.data), "refusal wrote journal bytes")
			_, statErr := os.Lstat(c.path(".owner.lock"))
			drillAssert(t, os.IsNotExist(statErr) == (row.name == "no-owner"), "refusal created a lock name")
		})
	}
}

func repairBaseline(t *testing.T, f *retainedFixture, kind string) {
	t.Helper()
	c, class := f.cell, "local_checkpoint_state_unsupported"
	data := retainedRead(t, c, repairLog)
	obs, err := c.Inspect(t.Context())
	if kind == "malformed" {
		class = "local_checkpoint_operation_failed"
		repairDiagnostic(t, err, class, "mutation changed the baseline refusal")
	} else {
		drillAssert(t, err == nil, "API inspection missed the torn suffix")
		drillAssert(t, obs.JournalTailIncomplete, "API inspection missed the torn suffix")
		drillAssert(t, obs.CheckpointListing == "listed", "listing hid the surviving target")
		if kind == "two-witnesses" || kind == "orphan-and-witness" {
			drillAssert(t, len(obs.Checkpoints) == 2, "listing hid the surviving target")
			drillAssert(t, obs.Checkpoints[1].ID == locklessAbsent, "listing hid the surviving target")
			drillAssert(t, obs.Checkpoints[1].Complete == (kind == "two-witnesses"), "listing hid the surviving target")
		}
	}
	for _, mutate := range []func() error{leftoverCreate(t, f), leftoverDelete(t, c, locklessAbsent)} {
		repairDiagnostic(t, mutate(), class, "mutation changed the baseline refusal")
		assertFileBytes(t, c.path(repairLog), data, "baseline refusal changed journal bytes")
	}
}

// Observe each held-object boundary. Inject sync errors independently of close.
func repairIO(t *testing.T, c *Cell, visit func(string) bool) {
	t.Helper()
	fileSync, dirSync := syncFile, syncCheckpointRoot
	t.Cleanup(func() { syncFile, syncCheckpointRoot = fileSync, dirSync; c.beforeJournalSettlement = nil })
	c.beforeJournalSettlement = func(stage string, f *os.File) {
		if visit(stage) {
			_ = f.Close()
		}
	}
	syncFile = func(f *os.File) error {
		if visit("file") {
			return os.ErrInvalid
		}
		return fileSync(f)
	}
	syncCheckpointRoot = func(d *os.Root) error {
		stage := "journal"
		if filepath.Clean(d.Name()) == c.cfg.Root {
			stage = "root"
		}
		if visit(stage) {
			return os.ErrInvalid
		}
		return dirSync(d)
	}
}

func TestRepairCheckpointJournalIO(t *testing.T) {
	if len(os.Args) >= 3 && os.Args[len(os.Args)-3] == "cut-fixture" {
		c, err := New(Config{Root: os.Args[len(os.Args)-2]})
		drillAssert(t, err == nil, "cannot open cut fixture")
		repairIO(t, c, func(stage string) bool {
			if stage == os.Args[len(os.Args)-1] {
				os.Exit(73)
			}
			return false
		})
		_, _ = c.RepairCheckpointJournal(t.Context())
		t.Fatal("cut did not reach its file boundary")
	}
	for _, mode := range []string{"fault", "cut"} {
		for _, boundary := range []string{"truncate", "file", "journal", "root", "close"} {
			t.Run(mode+"/"+boundary, func(t *testing.T) {
				f, _ := checkpointFixture(t, "legacy", false)
				c, prefix := f.cell, repairRecord(t, "checkpoint", "operation", "started")
				if mode == "fault" || boundary == "truncate" {
					prefix = "" // a torn first append, including its directory durability
				}
				repairSeed(t, c, prefix+"{")
				fault, calls := mode == "fault", ""
				repairIO(t, c, func(stage string) bool {
					calls += stage + " "
					return fault && stage == boundary
				})
				if mode == "cut" {
					ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRepairCheckpointJournalIO$", "--", "cut-fixture", c.cfg.Root, boundary)
					var exit *exec.ExitError
					drillAssert(t, errors.As(cmd.Run(), &exit) && exit.ExitCode() == 73, "cut did not reach its file boundary")
				} else {
					s, err := c.RepairCheckpointJournal(t.Context())
					failure := map[string]string{"journal": "first journal name was acknowledged without directory sync", "root": "journal directory was acknowledged without root sync"}[boundary]
					if failure == "" {
						failure = "failed settlement claimed success"
					}
					repairDiagnostic(t, err, "local_checkpoint_operation_failed", failure)
					drillAssert(t, s == "", "repair reported status on failure")
				}
				want, visible, sequence := CheckpointJournalClean, prefix, "validated file journal root close "
				if boundary == "truncate" {
					want, visible, sequence = CheckpointJournalTailDiscarded, prefix+"{", "validated truncate file journal root close "
				}
				assertFileBytes(t, c.path(repairLog), []byte(visible), "retry lost a validated record")
				fault, calls = false, ""
				s, err := c.RepairCheckpointJournal(t.Context())
				drillAssert(t, calls == sequence, "clean journal was not resynced")
				drillAssert(t, err == nil, "retry skipped the clean-file sync")
				drillAssert(t, s == want, "eligible repair status changed")
				assertFileBytes(t, c.path(repairLog), []byte(prefix), "retry lost a validated record")
			})
		}
	}
}
