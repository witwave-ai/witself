package localcell

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCheckpointSignalHelperProcess runs one TestCheckpointSignalCancellation
// row: with the command's signal context it blocks once at the row's step until
// that context ends (at most 30 s), finishes the call and prints its class.
func TestCheckpointSignalHelperProcess(t *testing.T) {
	if len(os.Args) < 5 || os.Args[len(os.Args)-4] != "localcell-checkpoint-signal-helper" {
		return
	}
	row, root, spec := os.Args[len(os.Args)-3], os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	split := strings.LastIndexByte(spec, '|')
	c, err := New(Config{Root: root, Binaries: Binaries{PostgresDir: spec[:max(split, 0)]}})
	if err != nil || split < 0 {
		t.Fatal("the signal helper cannot build its checkpoint cell")
	}
	r := &checkpointRecorder{calls: map[string]int{}, readings: []pgControlData{checkpointSyntheticControl}, after: func(string) {}, before: func() {},
		free: func(string) (uint64, uint64, error) { return 1 << 50, 1 << 51, nil }, nextID: checkpointNewID}
	r.install(c)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	context.AfterFunc(ctx, stop)
	armed := true
	block := func() {
		if armed {
			armed = false
			fmt.Println("blocked")
			select {
			case <-ctx.Done():
			case <-time.After(30 * time.Second):
			}
		}
	}
	// Admission's run returns the runner's cancelled cause; the witness's run
	// returns its reading, so only the step's own context check can refuse it.
	control, call := c.checkpointTest.control, map[string]int{"create-admission": 1, "create-witness": 2}[row]
	c.checkpointTest.control = func(controlCtx context.Context) (pgControlData, error) {
		reading, err := control(controlCtx)
		if r.calls["control"] == call {
			if block(); row == "create-admission" {
				err = fixedCause("PostgreSQL command cancelled")
			}
		}
		return reading, err
	}
	stage := map[string]string{"create-copy": "publish", "create-inventory": "open", "delete-unseal": "unseal", "delete-remove": "directory"}[row]
	c.checkpointTest.beforeStep = func(step, _ string) {
		if step == stage {
			block()
		}
	}
	c.cfg.Failpoint = func(operation, step string) error {
		if row == "create-after-witness" && operation+"/"+step == "checkpoint/witness" {
			block()
		}
		return nil
	}
	if strings.HasPrefix(row, "delete-") {
		err = c.DeleteCheckpoint(ctx, DeleteCheckpointOptions{CheckpointID: spec[split+1:]})
	} else {
		err = c.Checkpoint(ctx, CheckpointOptions{InstallationID: spec[split+1:]})
	}
	failure := ClassifyCheckpointFailure(err)
	fmt.Println("result", failure.Class, failure.CleanupIncomplete)
	os.Exit(0)
}

// TestCheckpointSignalCancellation sends a real signal (Windows: Ctrl-Break to
// a new process group) to a child blocked at one step of a create or delete.
// A cancelled create closes its operation and discards its partial copy; a
// create past its last check, and a delete, which ignores the context, finish.
func TestCheckpointSignalCancellation(t *testing.T) {
	for _, row := range []struct {
		name, signal, class string
		records             []string
		complete            int
	}{
		{"create-admission", "interrupt", "local_checkpoint_operation_failed", nil, 0},
		{"create-copy", "terminate", "local_checkpoint_copy_failed", []string{"checkpoint/operation/started", "checkpoint/copy/started", "checkpoint/copy/failed", "checkpoint/operation/done"}, 0},
		{"create-inventory", "hangup", "local_checkpoint_copy_failed", []string{"checkpoint/operation/started", "checkpoint/copy/started", "checkpoint/copy/done",
			"checkpoint/inventory/started", "checkpoint/inventory/failed", "checkpoint/operation/done"}, 0},
		{"create-witness", "interrupt", "local_checkpoint_copy_failed", []string{"checkpoint/operation/started", "checkpoint/copy/started", "checkpoint/copy/done",
			"checkpoint/inventory/started", "checkpoint/inventory/done", "checkpoint/witness/started", "checkpoint/witness/failed", "checkpoint/operation/done"}, 0},
		{"create-after-witness", "terminate", "", checkpointRecords, 0},
		{"delete-unseal", "hangup", "", checkpointDeleteRecords, 2},
		{"delete-remove", "interrupt", "", checkpointDeleteRecords, 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			// Delete rows remove the first of two checkpoints.
			f, _, ids := leftoverFixture(t, "legacy", row.complete)
			c, argument, kept := f.cell, f.manifest.InstallationID, ids
			if row.complete != 0 {
				argument, kept = ids[0], ids[1:]
			}
			before := checkpointTreeState(t, c)
			f.prefix = retainedRead(t, c, "journal/operations.jsonl")
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCheckpointSignalHelperProcess$", "--",
				"localcell-checkpoint-signal-helper", row.name, c.cfg.Root, c.cfg.Binaries.PostgresDir+"|"+argument)
			child.Env = []string{}
			checkpointSignalPrepare(child)
			output, err := child.StdoutPipe()
			if err != nil || child.Start() != nil {
				t.Fatal("cannot start the checkpoint signal helper")
			}
			t.Cleanup(func() {
				_ = child.Process.Kill()
				_ = child.Wait()
			})
			reader := bufio.NewReader(output)
			if line, err := reader.ReadString('\n'); err != nil || line != "blocked\n" {
				t.Fatal("the signal helper did not block at its step")
			}
			if checkpointSignalSend(child.Process, row.signal) != nil {
				t.Fatal("cannot signal the checkpoint signal helper")
			}
			result, err := io.ReadAll(reader)
			if err != nil || child.Wait() != nil || string(result) != fmt.Sprintln("result", row.class, false) {
				t.Fatal("the signalled call did not end on its own with its class")
			}
			retainedAssertRecords(t, f, row.records)
			if progress, err := c.readJournal(); err != nil || progress.pending != nil || progress.tornTail || c.validateRecovery() != nil {
				t.Fatal("a signalled call left an open operation or an unsafe recovery tree")
			}
			// Beyond the journal only recovery/, which a create's copy makes, and
			// the row's own checkpoint change; every other one is byte-identical.
			after, got := checkpointTreeState(t, c), checkpointIDs(t, c)
			if row.name == "create-after-witness" {
				if len(got) != 1 || !checkpointSummaryOf(t, c, got[0]).Complete {
					t.Fatal("a create past its last check did not leave one complete checkpoint")
				}
				argument, kept = got[0], got
			}
			if _, made := after["recovery"]; made != (row.name != "create-admission") {
				t.Fatal("a signalled call left recovery in an unexpected state")
			} else if row.complete == 0 && made {
				before["recovery"] = after["recovery"]
			}
			if !slices.Equal(got, kept) || !reflect.DeepEqual(checkpointWithout(before, argument), checkpointWithout(after, argument)) {
				t.Fatal("a signalled call changed the cell beyond its own checkpoint")
			}
		})
	}
}
