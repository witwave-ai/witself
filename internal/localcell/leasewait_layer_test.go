package localcell

import (
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const leaseLayerID = "ckpt_abcdefghijklmnop"

// leaseJournalPlant gives c a journal holding one closed stop, then, by
// prepare, an open stop, an open retained remove or a torn tail, all with the
// placeholder; it returns the journal's progress then.
func leaseJournalPlant(c *Cell, m *Manifest, prepare string) (journalProgress, error) {
	bg := context.Background()
	err := errors.Join(c.journal(bg, "stop", "operation", "started"), c.journal(bg, "stop", "operation", "done"))
	switch data, readErr := os.ReadFile(readerJournalPath(c)); prepare {
	case "stop":
		err = errors.Join(err, c.begin(bg, "stop"))
	case "remove":
		err = errors.Join(err, c.begin(bg, "remove"), c.retainRemoval(m))
	case "torn":
		err = errors.Join(err, readErr, testWriteFile(readerJournalPath(c), append(data, `{"operation":"stop","st`...), 0o600))
	}
	progress, readErr := c.readJournal(bg)
	return progress, errors.Join(err, readErr)
}

// leaseRecordPlant prepares c for an LC-LEASE-2C2 R5 row: postmaster.pid of
// an unallocated PID, a fence, or the owner lock without the run lock.
func leaseRecordPlant(t *testing.T, c *Cell, prepare string) error {
	switch prepare {
	case "record":
		record := strconv.Itoa(checkpointUnallocatedPID) + "\n" + c.path("pg") + "\n1\n"
		return errors.Join(testMkdir(c.path("pg"), 0o700), testWriteFile(c.path("pg", "postmaster.pid"), []byte(record), 0o600))
	case "fence":
		updateTestFence(t, c)
		return nil
	}
	lock, err := acquireLock(context.Background(), c.cfg.Root, true)
	if err != nil {
		return err
	}
	return errors.Join(lock.Close(), os.Remove(c.path("run", "operation.lock")))
}

// LC-LEASE-2B R5: with its context already done, each threaded declaration
// still makes its first attempt and gives its normal result for an unleased
// file on every system; one check per expectation, each with its own text
// (Windows and macOS run it in CI only).
func TestLeaseWaitLayerUnleased(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	refused, opened := errors.New("unleased layer row refused"), func(f *os.File, err error) (bool, error) { return f != nil && f.Close() == nil, err }
	for _, row := range []struct {
		name string
		call func(c *Cell, want maintenanceFence, dir string) (bool, error)
	}{
		{"openRegular", func(c *Cell, _ maintenanceFence, _ string) (bool, error) {
			return opened(openRegular(done, c.path("manifest.json"), refused, refused, refused))
		}},
		{"openRegularWriter", func(c *Cell, _ maintenanceFence, _ string) (bool, error) {
			return opened(openRegularWriter(done, c.path("tmp", "unleased"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600, refused, refused))
		}},
		{"openServiceRecord", func(c *Cell, _ maintenanceFence, _ string) (bool, error) {
			return opened(openServiceRecord(done, c.path("journal", "maintenance.json"), refused))
		}},
		{"checkpointComplete", func(_ *Cell, _ maintenanceFence, dir string) (bool, error) {
			_, complete, err := checkpointComplete(done, dir, leaseLayerID)
			return complete, err
		}},
		{"readMaintenanceFenceJSON", func(c *Cell, want maintenanceFence, _ string) (bool, error) {
			var fence maintenanceFence
			err := readMaintenanceFenceJSON(done, c.path("journal", "maintenance.json"), &fence)
			return fence.Generation == want.Generation && fence.OperationID == want.OperationID, err
		}},
		{"readMaintenanceFence", func(c *Cell, want maintenanceFence, _ string) (bool, error) {
			fence, err := c.readMaintenanceFence(done)
			return fence != nil && fence.Generation == want.Generation && fence.OperationID == want.OperationID, err
		}},
		{"acquireLock", func(c *Cell, _ maintenanceFence, _ string) (bool, error) {
			lock, err := acquireLock(done, c.cfg.Root, true)
			return lock != nil && lock.Close() == nil, err
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, _ := unitCell(t)
			normal, err := row.call(c, updateTestFence(t, c), checkpointPlant(t, c, leaseLayerID, leaseLayerID))
			if err != nil {
				t.Fatal("a threaded declaration refused an unleased file under a done context")
			}
			if !normal {
				t.Fatal("a threaded declaration gave another result for an unleased file under a done context")
			}
		})
	}
	// LC-LEASE-2C1 R5: the journal layer under a done context, nothing leased.
	state := func(c *Cell) journalProgress { progress, _ := c.readJournal(context.Background()); return progress }
	for _, row := range []struct {
		name, prepare string
		call          func(c *Cell, m *Manifest, p journalProgress) (bool, error)
	}{
		{"readJournal", "stop", func(c *Cell, _ *Manifest, _ journalProgress) (bool, error) {
			p, err := c.readJournal(done)
			return p.pending != nil && p.pending.Operation == "stop", err
		}},
		{"journal", "", func(c *Cell, _ *Manifest, _ journalProgress) (bool, error) {
			err := c.journal(done, "stop", "operation", "started")
			return state(c).pending != nil, err
		}},
		{"journalCause", "", func(c *Cell, _ *Manifest, _ journalProgress) (bool, error) {
			err := c.journalCause(done, "stop", "operation", "started", "unleased")
			return state(c).pending != nil, err
		}},
		{"step", "stop", func(c *Cell, _ *Manifest, _ journalProgress) (bool, error) {
			err := c.step(done, "stop", "stop", func() error { return nil })
			return state(c).done["stop"], err
		}},
		{"begin", "", func(c *Cell, _ *Manifest, _ journalProgress) (bool, error) {
			err := c.begin(done, "stop")
			return state(c).pending != nil, err
		}},
		{"finish", "stop", func(c *Cell, m *Manifest, _ journalProgress) (bool, error) {
			err := c.finish(done, m, "stop")
			return state(c).pending == nil && m.LastOperation == "stop", err
		}},
		{"discardJournalTail", "torn", func(c *Cell, _ *Manifest, p journalProgress) (bool, error) {
			err := c.discardJournalTail(done, p)
			return !state(c).tornTail, err
		}},
		{"finishRemoval", "remove", func(c *Cell, m *Manifest, _ journalProgress) (bool, error) {
			err := c.finishRemoval(done, m, false)
			return state(c).pending == nil, err
		}},
		{"finishRetainedRemoval", "remove", func(c *Cell, m *Manifest, _ journalProgress) (bool, error) {
			err := c.finishRetainedRemoval(done, m)
			return state(c).pending == nil, err
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, m := unitCell(t)
			progress, err := leaseJournalPlant(c, m, row.prepare)
			if err != nil {
				t.Fatal("cannot prepare the unleased journal row")
			}
			normal, err := row.call(c, m, progress)
			if err != nil {
				t.Fatal("a journal declaration refused an unleased journal under a done context")
			}
			if !normal {
				t.Fatal("a journal declaration gave another result for an unleased journal under a done context")
			}
		})
	}
	// LC-LEASE-2C2 R5: the process record, the fence, the lockless lock probe.
	admitted := errors.New("unleased lockless row admitted")
	for _, row := range []struct {
		name, prepare string
		call          func(c *Cell) (bool, error)
	}{
		{"postmaster", "record", func(c *Cell) (bool, error) {
			pid, err := c.postmaster(done)
			return pid == checkpointUnallocatedPID, err
		}},
		{"maintenanceFenceAllows", "fence", func(c *Cell) (bool, error) { return true, c.maintenanceFenceAllows(done, 1) }},
		{"refuseBeforeLockNames", "lockless", func(c *Cell) (bool, error) {
			return c.refuseBeforeLockNames(done, func() error { return admitted }) == admitted, nil
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, _ := unitCell(t)
			if leaseRecordPlant(t, c, row.prepare) != nil {
				t.Fatal("cannot prepare the unleased record or fence row")
			}
			normal, err := row.call(c)
			if err != nil {
				t.Fatal("a record or fence declaration refused an unleased file under a done context")
			}
			if !normal {
				t.Fatal("a record or fence declaration gave another result for an unleased file under a done context")
			}
		})
	}
}

// leaseWaitDetachedRows are TestLeaseWaitDetached's rows that only Linux runs
// (leasewait_detached_linux_test.go adds them), by name.
var leaseWaitDetachedRows = map[string]func(*testing.T){}

// LC-LEASE-2C2 R3: the detached value keeps its parent's Done, Err and Deadline
// before and after the cancel and carries the wait's mark, on every system
// (Windows and macOS in CI only); Linux rows wait out a lease under it.
func TestLeaseWaitDetached(t *testing.T) {
	t.Run("value", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		detached := leaseWaitDetached(parent)
		for _, phase := range []string{"before", "after"} {
			if phase == "after" {
				cancel()
			}
			if detached.Done() != parent.Done() {
				t.Fatalf("the detached value does not share its parent's Done channel %s the cancel", phase)
			}
			if detached.Err() != parent.Err() {
				t.Fatalf("the detached value does not give its parent's error %s the cancel", phase)
			}
			at, ok := detached.Deadline()
			if want, wantOK := parent.Deadline(); !at.Equal(want) || ok != wantOK {
				t.Fatalf("the detached value does not keep its parent's deadline %s the cancel", phase)
			}
		}
		if detached.Value(leaseWaitDetachedKey{}) == nil {
			t.Fatal("the detached value does not carry the lease wait mark")
		}
	})
	for _, name := range slices.Sorted(maps.Keys(leaseWaitDetachedRows)) {
		t.Run(name, leaseWaitDetachedRows[name])
	}
}

// The LC-LEASE-2B rows of the lease wait contract, with LF and CRLF endings:
// each mutation reports its key with both, and a method of another receiver
// that shares a threaded name is no call into the layer (no key at all).
func TestLeaseWaitLayerSourceContract(t *testing.T) {
	sources := map[string]string{}
	for name, source := range admissionReadSources(t) {
		sources[name] = strings.ReplaceAll(source, "\r\n", "\n")
	}
	const writer, load = "\tf, err := openPrivateWriter(ctx, path, flags, mode)\n", "func (s *supervisor) load(d ServiceDescriptor) (result error) {\n"
	const start, regular = "func (c *Cell) Start(ctx context.Context) error {\n", "openPrivateReader, nil, path, inspect, irregular, open)\n"
	for _, row := range []struct{ name, file, before, after, want string }{
		{"writer_cut_dropped", "files.go", writer + "\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn nil, cut\n\t}\n", writer, "lease_wait_cut:files.go"},
		{"fence_json_cut_refused", "update_fence.go", "\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn cut\n\t} else if err != nil {\n\t\treturn refused\n\t}\n",
			"\tif err != nil {\n\t\treturn refused\n\t}\n", "lease_wait_cut:update_fence.go"},
		{"fence_cut_active", "update_fence.go", "\tif err := read(path, &fence); errors.As(err, new(*leaseWaitCut)) {\n\t\treturn nil, err\n\t} else if err != nil || !fence.valid() {\n",
			"\tif err := read(path, &fence); err != nil || !fence.valid() {\n", "lease_wait_cut:update_fence.go"},
		{"layer_context_dropped", "files.go", "\treturn openRegularBy(ctx, " + regular, "\treturn openRegularBy(context.Background(), " + regular, "lease_wait_placeholder:files.go:openRegular"},
		{"placeholder_added", "lifecycle.go", start, start + "\t_ = readMaintenanceFenceJSON(context.Background(), c.path(\"journal\", \"maintenance.json\"), new(maintenanceFence))\n",
			"lease_wait_placeholder:lifecycle.go:(*Cell).Start"},
		{"field_call_counted", "service_supervisor.go", load, load + "\t_, _ = s.cell.readMaintenanceFence(context.Background())\n", "lease_wait_placeholder:service_supervisor.go:(*supervisor).load"},
		{"other_receiver_ignored", "service_supervisor.go", load, "func (s *supervisor) readMaintenanceFence(context.Context) {}\n\n" + load + "\ts.readMaintenanceFence(context.Background())\n", ""},
		// LC-LEASE-2C1: step keeps its cut, the append passes its own context, and
		// (*supervisor).step is not (*Cell).step (no key at all).
		{"step_cut_dropped", "files.go", "\t\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\t\t// A wait that ctx ended crosses as the value-free cut only.\n" +
			"\t\t\treturn fmt.Errorf(\"local cell %s: %s failed: %w\", operation, step, cut)\n\t\t}\n", "", "lease_wait_cut:files.go"},
		{"journal_context_dropped", "files.go", "\tf, err := openRegularWriter(ctx, path, os.O_WRONLY|", "\tf, err := openRegularWriter(context.Background(), path, os.O_WRONLY|",
			"lease_wait_placeholder:files.go:(*Cell).journalCause"},
		{"method_by_type", "service_supervisor.go", "err := s.step(ctx)", "err := s.step(context.Background())", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			if strings.Count(sources[row.file], row.before) != 1 {
				t.Fatal("lease wait layer mutation anchor is not unique")
			}
			mutated := maps.Clone(sources)
			mutated[row.file] = strings.Replace(sources[row.file], row.before, row.after, 1)
			for _, ending := range []string{"\n", "\r\n"} {
				if failures := leaseWaitContract(referencesEnding(mutated, ending)); row.want == "" && len(failures) != 0 || row.want != "" && !slices.Contains(failures, row.want) {
					t.Fatalf("lease wait layer contract reported keys %v against the row", failures)
				}
			}
		})
	}
}
