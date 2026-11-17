package localcell

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The one record a verb appends to close its own kind of interrupted operation.
var (
	checkpointLeftoverClose       = []string{"checkpoint/operation/done"}
	checkpointDeleteLeftoverClose = []string{"checkpoint_delete/operation/done"}
)

// leftoverFixture is checkpointFixture plus complete checkpoints made by Checkpoint.
func leftoverFixture(t *testing.T, layout string, complete int) (*retainedFixture, *checkpointRecorder, []string) {
	t.Helper()
	f, r := checkpointFixture(t, layout, false)
	for range complete {
		if f.cell.Checkpoint(t.Context(), checkpointOptions(f)) != nil {
			t.Fatal("cannot prepare a complete checkpoint")
		}
	}
	return f, r, checkpointIDs(t, f.cell)
}

func leftoverCreate(t *testing.T, f *retainedFixture) func() error {
	return func() error { return f.cell.Checkpoint(t.Context(), checkpointOptions(f)) }
}

func leftoverDelete(t *testing.T, c *Cell, checkpointID string) func() error {
	return func() error {
		return c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: checkpointID})
	}
}

// leftoverCrash runs call with one failpoint armed and requires the crash.
func leftoverCrash(t *testing.T, c *Cell, operation, cut string, call func() error) {
	t.Helper()
	c.cfg.Failpoint = checkpointFailpoint(operation, cut)
	err := call()
	if c.cfg.Failpoint = nil; !errors.Is(err, errInterrupted) {
		t.Fatal("the leftover crash was not injected")
	}
}

// leftoverStep saves the journal bytes just before call and requires exactly
// want appended after them; the row judges the returned error.
func leftoverStep(t *testing.T, f *retainedFixture, call func() error, want ...[]string) error {
	t.Helper()
	f.prefix = retainedRead(t, f.cell, "journal/operations.jsonl")
	err := call()
	retainedAssertRecords(t, f, slices.Concat(want...))
	return err
}

// leftoverSubtree keeps only one checkpoint's entries of a tree state.
func leftoverSubtree(tree map[string]retainedEntry, checkpointID string) map[string]retainedEntry {
	maps.DeleteFunc(tree, func(name string, _ retainedEntry) bool {
		return name != "recovery/"+checkpointID && !strings.HasPrefix(name, "recovery/"+checkpointID+"/")
	})
	return tree
}

// leftoverOpen requires the journal to hold exactly this open operation.
func leftoverOpen(t *testing.T, c *Cell, operation, step string) {
	t.Helper()
	if p, err := c.readJournal(); err != nil || p.tornTail || p.pending == nil || *p.pending != (Interruption{Operation: operation, Step: step}) {
		t.Fatal("the journal does not hold the expected open operation")
	}
}

func TestCheckpointLeftover(t *testing.T) {
	for _, cut := range []string{"operation_started", "copy", "inventory", "witness"} {
		for _, layout := range []string{"legacy", "selected"} {
			t.Run("create-after-crash/"+cut+"/"+layout, func(t *testing.T) {
				f, _, _ := leftoverFixture(t, layout, 0)
				c, create := f.cell, leftoverCreate(t, f)
				leftoverCrash(t, c, "checkpoint", cut, create)
				if cut == "operation_started" || cut == "witness" {
					if leftoverStep(t, f, create, checkpointLeftoverClose, checkpointRecords) != nil {
						t.Fatal("checkpoint did not close its own leftover and succeed")
					}
					return
				}
				orphan, before := checkpointIDs(t, c), checkpointTreeState(t, c)
				if err := create(); len(orphan) != 1 || !checkpointSameError(err, "local_checkpoint_incomplete") {
					t.Fatal("the orphan did not refuse the next checkpoint")
				}
				checkpointUnchanged(t, c, before)
				inspection, err := c.Inspect(t.Context())
				if err != nil || inspection.CheckpointListing != "listed" || !reflect.DeepEqual(inspection.Checkpoints, []CheckpointSummary{{ID: orphan[0]}}) ||
					!reflect.DeepEqual(inspection.Interrupted, &Interruption{Operation: "checkpoint", Step: cut}) {
					t.Fatal("inspection did not show the orphan and its interrupted step")
				}
				checkpointUnchanged(t, c, before)
				if leftoverStep(t, f, leftoverDelete(t, c, orphan[0]), checkpointLeftoverClose, checkpointDeleteRecords) != nil ||
					leftoverStep(t, f, create, checkpointRecords) != nil {
					t.Fatal("the orphan was not deleted by its identifier before a new checkpoint")
				}
			})
		}
	}
	for _, cut := range []string{"operation", "unseal", "remove"} {
		t.Run("delete-after-crash/"+cut, func(t *testing.T) {
			f, _, ids := leftoverFixture(t, "legacy", 2)
			c, target := f.cell, leftoverDelete(t, f.cell, ids[0])
			if cut != "operation" {
				leftoverCrash(t, c, "checkpoint_delete", cut, target)
			} else if c.begin("checkpoint_delete") != nil {
				t.Fatal("cannot plant an open deletion record")
			}
			if cut == "remove" {
				before := checkpointTreeState(t, c)
				if err := target(); !checkpointSameError(err, "local_checkpoint_identity_mismatch") {
					t.Fatal("a deletion whose target is gone was not refused")
				}
				checkpointUnchanged(t, c, before)
				leftoverOpen(t, c, "checkpoint_delete", "remove")
				if leftoverStep(t, f, leftoverCreate(t, f), checkpointDeleteLeftoverClose, checkpointRecords) != nil {
					t.Fatal("the next checkpoint did not close the deletion leftover")
				}
				return
			}
			sibling := leftoverSubtree(checkpointTreeState(t, c), ids[1])
			err := leftoverStep(t, f, target, checkpointDeleteLeftoverClose, checkpointDeleteRecords)
			if err != nil || !slices.Equal(checkpointIDs(t, c), ids[1:]) || !reflect.DeepEqual(sibling, leftoverSubtree(checkpointTreeState(t, c), ids[1])) {
				t.Fatal("the repeated deletion did not close its leftover and delete only its target")
			}
		})
	}
	t.Run("purged-root", func(t *testing.T) {
		c, ids := purgedCheckpointFixture(t)
		f, target := &retainedFixture{cell: c}, leftoverDelete(t, c, ids[0])
		leftoverCrash(t, c, "checkpoint_delete", "unseal", target)
		sibling := leftoverSubtree(checkpointTreeState(t, c), ids[1])
		err := leftoverStep(t, f, target, checkpointDeleteLeftoverClose, checkpointDeleteRecords)
		after := checkpointTreeState(t, c)
		if _, manifest := after["manifest.json"]; err != nil || manifest || !slices.Equal(checkpointIDs(t, c), ids[1:]) || !reflect.DeepEqual(sibling, leftoverSubtree(after, ids[1])) {
			t.Fatal("the purged root deletion did not close and finish without a manifest")
		}
	})
	t.Run("residue/failed-step", func(t *testing.T) {
		f, _, _ := leftoverFixture(t, "legacy", 0)
		c := f.cell
		if errors.Join(c.begin("checkpoint"), c.journal("checkpoint", "copy", "started"),
			c.journalCause("checkpoint", "copy", "failed", "local_checkpoint_copy_failed")) != nil {
			t.Fatal("cannot plant a failed copy step")
		}
		if leftoverStep(t, f, leftoverCreate(t, f), checkpointLeftoverClose, checkpointRecords) != nil {
			t.Fatal("a failed step leftover was not closed by the next checkpoint")
		}
	})
	for _, row := range []struct{ name, cut, drop, at string }{
		{"copy-temporary", "copy", "", "pg/base"},
		{"inventory-temporary", "inventory", checkpointInventoryName, "."},
		{"witness-temporary", "witness", checkpointWitnessName, "."},
	} {
		t.Run("residue/"+row.name, func(t *testing.T) {
			f, _, _ := leftoverFixture(t, "legacy", 0)
			c, create := f.cell, leftoverCreate(t, f)
			leftoverCrash(t, c, "checkpoint", row.cut, create)
			orphan := checkpointIDs(t, c)
			if len(orphan) != 1 {
				t.Fatal("the crash did not leave exactly one orphan")
			}
			dir := c.path("recovery", orphan[0])
			if row.drop != "" && os.Remove(filepath.Join(dir, row.drop)) != nil {
				t.Fatal("cannot remove a crashed checkpoint document")
			}
			if temporary, err := createPrivateTemp(filepath.Join(dir, filepath.FromSlash(row.at))); err != nil || temporary.Close() != nil {
				t.Fatal("cannot plant a private temporary in the orphan")
			}
			before := checkpointTreeState(t, c)
			if err := create(); !checkpointSameError(err, "local_checkpoint_incomplete") {
				t.Fatal("the orphan with residue did not refuse the next checkpoint")
			}
			checkpointUnchanged(t, c, before)
			if leftoverDelete(t, c, orphan[0])() != nil || len(checkpointIDs(t, c)) != 0 || create() != nil {
				t.Fatal("the orphan with residue was not deleted by its identifier")
			}
		})
	}
	t.Run("residue/remove-partial", func(t *testing.T) {
		f, _, ids := leftoverFixture(t, "legacy", 1)
		c, target := f.cell, leftoverDelete(t, f.cell, ids[0])
		leftoverCrash(t, c, "checkpoint_delete", "unseal", target)
		if errors.Join(c.journal("checkpoint_delete", "unseal", "done"), c.journal("checkpoint_delete", "remove", "started"),
			os.RemoveAll(c.path("recovery", ids[0], "pg", "base"))) != nil {
			t.Fatal("cannot plant a partial removal")
		}
		if leftoverStep(t, f, target, checkpointDeleteLeftoverClose, checkpointDeleteRecords) != nil || len(checkpointIDs(t, c)) != 0 {
			t.Fatal("a partial removal was not finished by the repeated deletion")
		}
	})
	t.Run("orphan-kept", func(t *testing.T) {
		f, _, ids := leftoverFixture(t, "legacy", 1)
		c := f.cell
		leftoverCrash(t, c, "checkpoint", "copy", leftoverCreate(t, f))
		orphan := slices.DeleteFunc(checkpointIDs(t, c), func(id string) bool { return id == ids[0] })
		if len(orphan) != 1 {
			t.Fatal("the crash did not leave exactly one orphan")
		}
		kept := leftoverSubtree(checkpointTreeState(t, c), orphan[0])
		err := leftoverStep(t, f, leftoverDelete(t, c, ids[0]), checkpointLeftoverClose, checkpointDeleteRecords)
		if err != nil || !slices.Equal(checkpointIDs(t, c), orphan) || !reflect.DeepEqual(kept, leftoverSubtree(checkpointTreeState(t, c), orphan[0])) {
			t.Fatal("deleting another checkpoint changed the orphan")
		}
	})
	for _, kind := range []string{"install", "start", "stop", "remove", "reinstall", "cleanup-only", "repair-step", "torn-open", "torn-only", "cleanup-and-leftover",
		"repair-step-delete", "torn-open-delete", "cleanup-and-leftover-create"} {
		t.Run("foreign/"+kind, func(t *testing.T) {
			f, r, ids := leftoverFixture(t, "legacy", 1)
			c := f.cell
			cleanup := func() error {
				m := f.manifest
				m.Cleanup = &journalEntry{Operation: "remove", Step: "runtime_cleanup", State: "started", At: time.Now().UTC()}
				return writeAtomic(c.path("manifest.json"), &m, nil)
			}
			torn := func() error {
				checkpointWrite(t, c, "journal/operations.jsonl", string(retainedRead(t, c, "journal/operations.jsonl"))+`{"operation":`)
				return nil
			}
			plant := map[string]func() error{"cleanup-only": cleanup, "torn-only": torn,
				"repair-step": func() error {
					return errors.Join(c.begin("checkpoint"), c.journal("checkpoint", "revert_stop", "started"))
				},
				"torn-open":            func() error { return errors.Join(c.begin("checkpoint"), torn()) },
				"cleanup-and-leftover": func() error { return errors.Join(c.begin("checkpoint_delete"), cleanup()) },
				"repair-step-delete": func() error {
					return errors.Join(c.begin("checkpoint_delete"), c.journal("checkpoint_delete", "revert_stop", "started"))
				},
				"torn-open-delete":            func() error { return errors.Join(c.begin("checkpoint_delete"), torn()) },
				"cleanup-and-leftover-create": func() error { return errors.Join(c.begin("checkpoint"), cleanup()) },
			}[kind]
			if plant == nil {
				plant = func() error { return c.begin(kind) }
			}
			if plant() != nil {
				t.Fatal("cannot plant the foreign journal state")
			}
			calls, before := maps.Clone(r.calls), checkpointTreeState(t, c)
			for _, err := range []error{leftoverCreate(t, f)(), leftoverDelete(t, c, ids[0])()} {
				if !checkpointSameError(err, "local_checkpoint_state_unsupported") {
					t.Fatal("a foreign journal state was not refused with the state class")
				}
			}
			if checkpointUnchanged(t, c, before); !maps.Equal(calls, r.calls) {
				t.Fatal("a foreign journal refusal reached a checkpoint boundary")
			}
		})
	}
	own, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		t.Fatal("cannot observe the test process identity")
	}
	fence := func(t *testing.T, f *retainedFixture, _ *checkpointRecorder) { updateTestFence(t, f.cell) }
	live := func(t *testing.T, f *retainedFixture, _ *checkpointRecorder) { checkpointRecord(t, f.cell, own) }
	inProduction := checkpointSyntheticControl
	inProduction.State = "in production"
	// target is empty for Checkpoint; "first" deletes the first checkpoint.
	for _, row := range []struct {
		name, target, want string
		complete           int
		setup              func(*testing.T, *retainedFixture, *checkpointRecorder)
	}{
		{"fence", "", "local_maintenance_fence_active", 0, fence},
		{"live-record", "", "local_checkpoint_state_unsupported", 0, live},
		{"limit", "", "local_checkpoint_limit_reached", 2, func(*testing.T, *retainedFixture, *checkpointRecorder) {}},
		{"control-state", "", "local_checkpoint_state_unsupported", 0, func(_ *testing.T, _ *retainedFixture, r *checkpointRecorder) {
			r.readings = []pgControlData{inProduction}
		}},
		{"storage", "", "local_checkpoint_storage_insufficient", 0, func(_ *testing.T, f *retainedFixture, r *checkpointRecorder) {
			need := checkpointNeed(f, false)
			r.free = func(string) (uint64, uint64, error) { return need - 1, need, nil }
		}},
		{"allocation", "", "cannot allocate local cell checkpoint identity", 0, func(_ *testing.T, _ *retainedFixture, r *checkpointRecorder) {
			r.nextID = func() (string, error) { return "", errors.New("synthetic generator failure") }
		}},
		{"delete-fence", "first", "local_maintenance_fence_active", 1, fence},
		{"delete-live-record", "first", "local_checkpoint_state_unsupported", 1, live},
		{"delete-unknown-id", "ckpt_zzzzzzzzzzzzzzzz", "local_checkpoint_identity_mismatch", 1, func(*testing.T, *retainedFixture, *checkpointRecorder) {}},
		{"delete-non-private", "first", "local_checkpoint_state_unsupported", 1, func(t *testing.T, f *retainedFixture, _ *checkpointRecorder) {
			recoveryBoundaryNonPrivate(t, f.cell.path("recovery", checkpointIDs(t, f.cell)[0], "pg", "base", "1", "1259"), false)
		}},
	} {
		t.Run("refusal-writes-nothing/"+row.name, func(t *testing.T) {
			// The same refusal without and then with a leftover from c.begin.
			for _, leftover := range []bool{false, true} {
				f, r, ids := leftoverFixture(t, "legacy", row.complete)
				c, call := f.cell, leftoverCreate(t, f)
				if row.target == "first" {
					call = leftoverDelete(t, c, ids[0])
				} else if row.target != "" {
					call = leftoverDelete(t, c, row.target)
				}
				if row.setup(t, f, r); leftover && c.begin("checkpoint") != nil {
					t.Fatal("cannot plant an open checkpoint record")
				}
				before := checkpointTreeState(t, c)
				if err := call(); !checkpointSameError(err, row.want) {
					t.Fatal("a refusal differs from the expected refusal")
				}
				if checkpointUnchanged(t, c, before); leftover {
					leftoverOpen(t, c, "checkpoint", "operation")
				}
			}
		})
	}
	t.Run("closer-sync-fails", func(t *testing.T) {
		f, _, _ := leftoverFixture(t, "legacy", 0)
		c, create := f.cell, leftoverCreate(t, f)
		if c.begin("checkpoint") != nil {
			t.Fatal("cannot plant an open checkpoint record")
		}
		original, failed := syncFile, false
		t.Cleanup(func() { syncFile = original })
		syncFile = func(file *os.File) error {
			data, _ := os.ReadFile(file.Name())
			last := data[bytes.LastIndexByte(bytes.TrimSuffix(data, []byte("\n")), '\n')+1:]
			if !failed && filepath.Base(file.Name()) == "operations.jsonl" && bytes.Contains(last, []byte(`"step":"operation","state":"done"`)) {
				failed = true
				return errors.New("synthetic journal sync failure")
			}
			return original(file)
		}
		err := leftoverStep(t, f, create, checkpointLeftoverClose)
		if syncFile = original; !failed || !checkpointSameError(err, "cannot sync local cell journal") {
			t.Fatal("a failed closing sync did not end the checkpoint before its begin")
		}
		if leftoverStep(t, f, create, checkpointRecords) != nil {
			t.Fatal("the checkpoint after a failed closing sync did not succeed")
		}
	})
	t.Run("still-running-delete", func(t *testing.T) {
		f, _, ids := leftoverFixture(t, "legacy", 1)
		c := f.cell
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		var syncs atomic.Int32
		unblock := func() { once.Do(func() { close(release) }) }
		original := syncCheckpointRoot
		t.Cleanup(func() { syncCheckpointRoot = original })
		syncCheckpointRoot = func(held *os.Root) error {
			if syncs.Add(1) == 1 {
				close(entered)
				<-release
			}
			return original(held)
		}
		f.prefix = retainedRead(t, c, "journal/operations.jsonl")
		result := StartJoinedTestRunForTest(t, unblock, reinstallContentionBound, func() error {
			return c.DeleteCheckpoint(context.Background(), DeleteCheckpointOptions{CheckpointID: ids[0]})
		})
		select {
		case <-entered:
		case <-result:
			t.Fatal("deletion returned before its unseal barrier")
		case <-time.After(reinstallContentionBound):
			t.Fatal("deletion did not reach its unseal barrier")
		}
		other, err := New(c.cfg)
		if err != nil {
			t.Fatal("cannot build a second cell for the same root")
		}
		other.checkpointTest = c.checkpointTest
		held := retainedSnapshot(t, c)
		for _, contender := range []func() error{func() error { return other.Checkpoint(t.Context(), checkpointOptions(f)) }, leftoverDelete(t, other, ids[0])} {
			started := time.Now()
			if err := contender(); !errors.Is(err, ErrLocked) || time.Since(started) > reinstallContentionBound || !reflect.DeepEqual(held, retainedSnapshot(t, c)) {
				t.Fatal("a live deletion was not refused to a contender without a write")
			}
		}
		if inspection, err := other.Inspect(t.Context()); err != nil || inspection.CheckpointListing != "busy" || !reflect.DeepEqual(held, retainedSnapshot(t, c)) {
			t.Fatal("inspection of a live deletion was not busy and read only")
		}
		unblock()
		select {
		case err := <-result:
			if err != nil {
				t.Fatal("the released deletion did not succeed")
			}
		case <-time.After(reinstallContentionBound):
			t.Fatal("the released deletion did not return")
		}
		retainedAssertRecords(t, f, checkpointDeleteRecords)
	})
}

// TestCheckpointLeftoverHelperProcess holds one verb inside its operation for
// TestCheckpointLeftoverAfterKill; its last argument is pgbin|argument, the
// argument being the installation (create) or checkpoint (delete) identifier.
func TestCheckpointLeftoverHelperProcess(t *testing.T) {
	if len(os.Args) < 5 || os.Args[len(os.Args)-4] != "localcell-checkpoint-leftover-helper" {
		return
	}
	mode, root, spec := os.Args[len(os.Args)-3], os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	split := strings.LastIndexByte(spec, '|')
	c, err := New(Config{Root: root, Binaries: Binaries{PostgresDir: spec[:max(split, 0)]}})
	if err != nil || split < 0 {
		t.Fatal("the helper cannot build its checkpoint cell")
	}
	r := &checkpointRecorder{calls: map[string]int{}, readings: []pgControlData{checkpointSyntheticControl}, after: func(string) {}, before: func() {},
		free: func(string) (uint64, uint64, error) { return 1 << 50, 1 << 51, nil }, nextID: checkpointNewID}
	r.install(c)
	block := func() {
		_, _ = fmt.Fprintln(os.Stdout, "blocked")
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	switch argument := spec[split+1:]; mode {
	case "create":
		publishes := 0
		c.checkpointTest.beforeStep = func(step, _ string) {
			if step == "publish" {
				if publishes++; publishes == 2 {
					block()
				}
			}
		}
		_ = c.Checkpoint(context.Background(), CheckpointOptions{InstallationID: argument})
	case "delete":
		original, syncs := syncCheckpointRoot, 0
		t.Cleanup(func() { syncCheckpointRoot = original })
		syncCheckpointRoot = func(held *os.Root) error {
			if syncs++; syncs == 1 {
				block()
			}
			return original(held)
		}
		_ = c.DeleteCheckpoint(context.Background(), DeleteCheckpointOptions{CheckpointID: argument})
	default:
		t.Fatal("the helper mode is neither create nor delete")
	}
}

// leftoverAfterKill retries call while the killed owner's lock is still held,
// for at most 10 s; every refused attempt must leave the tree unchanged.
func leftoverAfterKill(t *testing.T, f *retainedFixture, call func() error) error {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		before := checkpointTreeState(t, f.cell)
		f.prefix = retainedRead(t, f.cell, "journal/operations.jsonl")
		if err := call(); !errors.Is(err, ErrLocked) {
			return err
		}
		if checkpointUnchanged(t, f.cell, before); time.Now().After(deadline) {
			t.Fatal("the killed owner's lock was not released within the bound")
		}
	}
}

func TestCheckpointLeftoverAfterKill(t *testing.T) {
	for _, mode := range []string{"create", "delete"} {
		t.Run(mode, func(t *testing.T) {
			f, _, ids := leftoverFixture(t, "legacy", map[string]int{"delete": 1}[mode])
			c, create, argument := f.cell, leftoverCreate(t, f), f.manifest.InstallationID
			if mode == "delete" {
				argument = ids[0]
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCheckpointLeftoverHelperProcess$", "--",
				"localcell-checkpoint-leftover-helper", mode, c.cfg.Root, c.cfg.Binaries.PostgresDir+"|"+argument)
			child.Env = []string{}
			// Stdin stays open until Wait, so only the kill ends the blocked helper.
			_, inputErr := child.StdinPipe()
			output, outputErr := child.StdoutPipe()
			if inputErr != nil || outputErr != nil || child.Start() != nil {
				t.Fatal("cannot start the checkpoint helper process")
			}
			t.Cleanup(func() {
				_ = child.Process.Kill()
				_ = child.Wait()
			})
			if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "blocked\n" {
				t.Fatal("the helper did not block inside its operation")
			}
			held, live := checkpointTreeState(t, c), checkpointIDs(t, c)
			if len(live) != 1 || !errors.Is(create(), ErrLocked) || !errors.Is(leftoverDelete(t, c, live[0])(), ErrLocked) {
				t.Fatal("a live helper operation was not refused as locked")
			}
			if inspection, err := c.Inspect(t.Context()); err != nil || inspection.CheckpointListing != "busy" {
				t.Fatal("inspection of a live helper operation was not busy")
			}
			if checkpointUnchanged(t, c, held); child.Process.Kill() != nil {
				t.Fatal("cannot kill the checkpoint helper process")
			}
			_ = child.Wait()
			if mode == "delete" {
				if leftoverAfterKill(t, f, leftoverDelete(t, c, argument)) != nil {
					t.Fatal("the deletion after a kill did not succeed")
				}
				retainedAssertRecords(t, f, slices.Concat(checkpointDeleteLeftoverClose, checkpointDeleteRecords))
				return
			}
			before := checkpointTreeState(t, c)
			if err := leftoverAfterKill(t, f, create); !checkpointSameError(err, "local_checkpoint_incomplete") {
				t.Fatal("the killed copy did not refuse the next checkpoint as incomplete")
			}
			checkpointUnchanged(t, c, before)
			orphan, pending := checkpointIDs(t, c), 0
			if len(orphan) != 1 || checkpointSummaryOf(t, c, orphan[0]).Complete {
				t.Fatal("the killed copy did not leave exactly one incomplete orphan")
			}
			walkErr := filepath.WalkDir(c.path("recovery", orphan[0]), func(_ string, entry fs.DirEntry, err error) error {
				if err == nil && strings.HasPrefix(entry.Name(), ".pending-") {
					pending++
				}
				return err
			})
			if walkErr != nil || pending == 0 || c.validateRecovery() != nil {
				t.Fatal("the killed copy left no private pending temporary")
			}
			if leftoverStep(t, f, func() error { return errors.Join(leftoverDelete(t, c, orphan[0])(), create()) },
				checkpointLeftoverClose, checkpointDeleteRecords, checkpointRecords) != nil {
				t.Fatal("the orphan of a killed copy was not deleted before a new checkpoint")
			}
		})
	}
}
