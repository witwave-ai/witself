package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const leaseLayerID = "ckpt_abcdefghijklmnop"

// leaseCommandDiagnostic is a value-free diagnostic the server's log holds.
const leaseCommandDiagnostic = "witself-server: listener bind failed: address in use"

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

// leaseRecordPlant sets up unleased record, fence and ownership inputs.
func leaseRecordPlant(t *testing.T, c *Cell, prepare string) error {
	switch prepare {
	case "record", "server_record", "postgres_record":
		if prepare != "record" {
			component := strings.TrimSuffix(prepare, "_record")
			if err := writeAtomic(c.path("run", component+".json"), ProcessRecord{PID: checkpointUnallocatedPID, StartTime: "stale", Root: c.cfg.Root, Component: component}, nil); err != nil {
				return err
			}
		}
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
	if err := lock.Close(); err != nil {
		return err
	}
	switch prepare {
	case "locks":
		return nil
	case "fallback":
		return os.Remove(c.path(".owner.lock"))
	default:
		return os.Remove(c.path("run", "operation.lock"))
	}
}

// LC-LEASE-2B R5: with its context already done, each threaded declaration
// still makes its first attempt and gives its normal result for an unleased
// file on every system; one check per expectation, each with its own text
// (Windows and macOS run it in CI only).
func TestLeaseWaitLayerUnleased(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	t.Run("serviceAdapterRemovalHostLock", func(t *testing.T) {
		c, _ := unitCell(t)
		release, err := serviceAdapterRemovalHostLock(done, c)
		if err != nil || release == nil {
			t.Fatal("a done context refused the unleased removal host lock")
		}
		defer release()
		second, err := serviceAdapterRemovalHostLock(t.Context(), c)
		if second != nil {
			second()
			t.Fatal("a second removal host lock returned a release while the first was held")
		}
		if !errors.Is(err, ErrLocked) {
			t.Fatal("a second removal host lock did not report contention while the first was held")
		}
	})
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
	for _, name := range []string{"own", "ownExisting"} {
		t.Run(name, func(t *testing.T) {
			c, _ := unitCell(t)
			if leaseRecordPlant(t, c, "locks") != nil {
				t.Fatal("cannot prepare both unleased ownership locks")
			}
			var lock *operationLock
			var err error
			if name == "own" {
				lock, err = c.own(done, false)
			} else {
				lock, err = c.ownExisting(done)
			}
			if err != nil {
				t.Fatal("a done context refused unleased ownership")
			}
			if lock == nil {
				t.Fatal("unleased ownership returned no lock under a done context")
			}
			if len(lock.files) != 2 {
				t.Fatal("unleased ownership did not hold both locks")
			}
			if lock.Close() != nil {
				t.Fatal("cannot close unleased ownership under a done context")
			}
		})
	}
	t.Run("verifyAll", func(t *testing.T) {
		c, _ := unitCell(t)
		identity, alive, err := processIdentity(os.Getpid())
		if err != nil || !alive {
			t.Fatal("cannot identify the test process for ownership verification")
		}
		for _, component := range []string{"postgres", "server", "worker"} {
			if writeAtomic(c.path("run", component+".json"), ProcessRecord{PID: os.Getpid(), StartTime: identity, Root: c.cfg.Root, Component: component}, nil) != nil {
				t.Fatal("cannot plant the matching component identity")
			}
		}
		if testMkdir(c.path("pg"), 0o700) != nil || testWriteFile(c.path("pg", "postmaster.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"+c.path("pg")+"\n1\n"), 0o600) != nil {
			t.Fatal("cannot plant the matching PostgreSQL identity")
		}
		if c.verifyAll(done) != nil {
			t.Fatal("a done context refused matching unleased process identities")
		}
	})
	t.Run("recoverPostgresRecord", func(t *testing.T) {
		c, _ := unitCell(t)
		if leaseRecordPlant(t, c, "record") != nil {
			t.Fatal("cannot plant the dead postmaster identity")
		}
		if c.recoverPostgresRecord(done) != nil {
			t.Fatal("a done context refused recovery of a proven-dead postmaster")
		}
		if lockDirectory(c.path("run")) != nil {
			t.Fatal("the recovery record parent is not a real directory")
		}
		if _, err := os.Lstat(c.path("run", "postgres.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("recovery published signaling authority for a dead postmaster")
		}
	})
	// LC-LEASE-2C2-F1 (G4): under a done context the active fence still refuses
	// an ordinary caller (generation 0); nil there would admit past it.
	t.Run("maintenanceFenceAllows_active", func(t *testing.T) {
		c, _ := unitCell(t)
		if leaseRecordPlant(t, c, "fence") != nil {
			t.Fatal("cannot prepare the unleased active fence row")
		}
		if !errors.Is(c.maintenanceFenceAllows(done, 0), errMaintenanceFenceActive) {
			t.Fatal("a done context let an ordinary caller past an active maintenance fence")
		}
	})
	// LC-LEASE-2D R5: commands, credentials, ports, reinstall, checkpoint internals.
	failed, listed := errors.New("unleased command row step failed"), func(i Inspection) bool { return len(i.Checkpoints) == 1 && i.Checkpoints[0].Complete }
	t.Run("prepareComponentPorts_postgres", func(t *testing.T) {
		c, m := unitCell(t)
		if err := selectedFixtureDataDirectory(c); err != nil {
			t.Fatal("cannot prepare the postgres port directory")
		}
		original := m.Ports.Postgres
		if err := c.prepareComponentPorts(done, m, "postgres", false, true); err != nil {
			t.Fatal("done context rejected an unleased postgres port reallocation")
		}
		if m.Ports.Postgres == original {
			t.Fatal("postgres reallocation retained the original port")
		}
		saved, err := readManifest(t.Context(), c.cfg.Root)
		if err != nil {
			t.Fatal("cannot read the manifest after postgres reallocation")
		}
		if saved.Ports.Postgres != m.Ports.Postgres {
			t.Fatal("manifest did not publish the reallocated postgres port")
		}
		config, err := os.ReadFile(c.path("pg", "postgresql.conf"))
		if err != nil {
			t.Fatal("cannot read the reallocated postgres configuration")
		}
		if !strings.Contains(string(config), "\nport = "+strconv.Itoa(saved.Ports.Postgres)+"\n") {
			t.Fatal("postgres configuration did not publish the manifest port")
		}
		data, err := os.ReadFile(c.path("journal", "operations.jsonl"))
		if err != nil {
			t.Fatal("cannot read the postgres reallocation journal")
		}
		entries, ok := journalDirectorySyncLines(data)
		if !ok || len(entries) != 1 {
			t.Fatal("postgres reallocation did not write one complete journal entry")
		}
		want := journalEntry{Operation: "start", Step: "postgres", State: "started", At: entries[0].At, Cause: "port_reallocated=postgres"}
		if entries[0] != want {
			t.Fatal("journal did not record the postgres port reallocation")
		}
	})
	journaled := func(c *Cell, cause string) bool {
		data, _ := os.ReadFile(c.path("journal", "operations.jsonl"))
		return strings.Contains(string(data), cause)
	}
	for _, row := range []struct {
		name, prepare string
		call          func(c *Cell, m *Manifest) (bool, error)
	}{
		{"password", "", func(c *Cell, _ *Manifest) (bool, error) { value, err := c.password(done); return value != "", err }},
		{"logFile", "", func(c *Cell, _ *Manifest) (bool, error) { return opened(c.logFile(done, "server")) }},
		{"childEnvironment", "", func(c *Cell, m *Manifest) (bool, error) {
			environment, err := c.childEnvironment(done, m, "server")
			return slices.ContainsFunc(environment, func(value string) bool { return strings.HasPrefix(value, "WITSELF_DATABASE_URL=postgres") }), err
		}},
		{"prepareComponentPorts", "", func(c *Cell, m *Manifest) (bool, error) {
			err := c.prepareComponentPorts(done, m, "server", false, true)
			return journaled(c, "port_reallocated=server"), err
		}},
		{"prepareComponentPortsFor", "", func(c *Cell, m *Manifest) (bool, error) {
			err := c.prepareComponentPortsFor(done, "reinstall", m, "worker", false, true)
			return journaled(c, "port_reallocated=worker"), err
		}},
		{"credentialPassword", "selected", func(c *Cell, m *Manifest) (bool, error) {
			value, err := c.credentialPassword(done, m, credentialServer)
			return len(value) == 43, err
		}},
		{"validatePrincipalPasswords", "selected", func(c *Cell, m *Manifest) (bool, error) { return true, c.validatePrincipalPasswords(done, m) }},
		{"makePrincipalPasswords", "fresh", func(c *Cell, m *Manifest) (bool, error) {
			err := c.makePrincipalPasswords(done, m)
			entries, _ := os.ReadDir(c.path("run", "credentials"))
			return len(entries) == len(principalCredentialPurposes), err
		}},
		{"principalDSN", "selected", func(c *Cell, m *Manifest) (bool, error) {
			dsn, err := c.principalDSN(done, m, credentialServer)
			return strings.HasPrefix(dsn, "postgres:"), err
		}},
		{"maintenanceDSN", "selected", func(c *Cell, m *Manifest) (bool, error) {
			dsn, err := c.maintenanceDSN(done, m)
			return strings.HasPrefix(dsn, "postgres:"), err
		}},
		{"lastValueFreeDiagnostic", "", func(c *Cell, _ *Manifest) (bool, error) {
			return c.lastValueFreeDiagnostic(done, "server", 0) == leaseCommandDiagnostic, nil
		}},
		{"admitReinstall", "retained", func(c *Cell, m *Manifest) (bool, error) {
			admitted, err := c.admitReinstall(done, ReinstallOptions{InstallationID: m.InstallationID})
			return admitted != nil && admitted.RemovalRetained, err
		}},
		{"reinstallAvailable", "retained", func(c *Cell, m *Manifest) (bool, error) { return true, c.reinstallAvailable(done, m, state(c)) }},
		{"finishReinstall", "reinstall", func(c *Cell, m *Manifest) (bool, error) {
			err := c.finishReinstall(done, m)
			return state(c).pending == nil && m.LastOperation == "reinstall", err
		}},
		{"checkpointOps", "", func(c *Cell, _ *Manifest) (bool, error) {
			return opened(c.checkpointOps(done).open(c.path("manifest.json")))
		}},
		{"checkpointSummaries", "", func(c *Cell, _ *Manifest) (bool, error) {
			inspection, lock, err := Inspection{}, (*operationLock)(nil), error(nil)
			if lock, err = acquireLock(context.Background(), c.cfg.Root, true); err == nil {
				defer func() { _ = lock.Close() }()
				err = c.checkpointSummaries(done, lock, &inspection)
			}
			return listed(inspection), err
		}},
		{"closeCheckpointFailure", "checkpoint", func(c *Cell, _ *Manifest) (bool, error) {
			return c.closeCheckpointFailure(done, "checkpoint", failed, true, nil) == failed && state(c).pending == nil, nil
		}},
		{"closeCheckpointLeftover", "checkpoint", func(c *Cell, _ *Manifest) (bool, error) {
			err := c.closeCheckpointLeftover(done)
			return state(c).pending == nil, err
		}},
		{"listCheckpoints", "", func(c *Cell, _ *Manifest) (bool, error) {
			summaries, unknown, err := c.listCheckpoints(done)
			return listed(Inspection{Checkpoints: summaries}) && unknown == 0, err
		}},
		{"openCheckpointEntry", "", func(c *Cell, _ *Manifest) (bool, error) {
			return opened(c.openCheckpointEntry(done, checkpointEntry{path: "manifest.json"}, nil))
		}},
		{"admitDeleteCheckpoint", "", func(c *Cell, _ *Manifest) (bool, error) {
			return true, c.admitDeleteCheckpoint(done, DeleteCheckpointOptions{CheckpointID: leaseLayerID})
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			normal, err := row.call(leaseCommandPlant(t, row.prepare))
			if err != nil {
				t.Fatal("a command declaration refused an unleased file under a done context")
			}
			if !normal {
				t.Fatal("a command declaration gave another result for an unleased file under a done context")
			}
		})
	}
	t.Run("closeCheckpointFailure_unreadable", func(t *testing.T) {
		c, _ := unitCell(t)
		if err := os.Mkdir(readerJournalPath(c), 0o700); err != nil {
			t.Fatal("cannot prepare the unreadable checkpoint journal")
		}
		if got := c.closeCheckpointFailure(done, "checkpoint", failed, true, nil); got != failed {
			t.Fatal("unreadable journal lost the original checkpoint failure")
		}
	})
}

// leaseCommandPlant prepares an LC-LEASE-2D row's cell: the selected layout's
// credentials (fresh: none yet), a retained cell, or a legacy cell holding its
// password, a value-free log diagnostic and a complete checkpoint, with the
// operation prepare names open.
func leaseCommandPlant(t *testing.T, prepare string) (*Cell, *Manifest) {
	c, m, err := (*Cell)(nil), (*Manifest)(nil), error(nil)
	switch prepare {
	case "selected", "fresh":
		if c, m, _ = selectedCredentialFixture(t); prepare == "fresh" {
			err = os.RemoveAll(c.path("run", "credentials"))
		}
	case "retained":
		f, _ := reinstallFixture(t, "legacy", false)
		c = f.cell
		m, err = readManifest(t.Context(), c.cfg.Root)
	default:
		c, m = unitCell(t)
		checkpointPlant(t, c, leaseLayerID, leaseLayerID)
		err = errors.Join(c.makePassword(), writeAtomicBytes(c.path("logs", "server.log"), []byte(leaseCommandDiagnostic+"\n"), nil))
		if prepare != "" {
			err = errors.Join(err, c.begin(context.Background(), prepare))
		}
	}
	if err != nil {
		t.Fatal("cannot prepare the command row cell")
	}
	return c, m
}

// LC-LEASE-2C2-F1 (G3): Inspect and DeleteCheckpoint started with a context
// already done end exactly as under context.Background() (the parent ignored
// their contexts): the same error and inspection, the same journal records,
// the named checkpoint deleted; one check per expectation, each with its own
// text (Windows and macOS run it in CI only).
func TestLeaseWaitReleasedDone(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	f, _ := checkpointFixture(t, "legacy", false)
	c := f.cell
	if errors.Join(c.Checkpoint(t.Context(), checkpointOptions(f)), c.Checkpoint(t.Context(), checkpointOptions(f))) != nil || len(checkpointIDs(t, c)) != 2 {
		t.Fatal("cannot prepare the two checkpoints the done context rows read and delete")
	}
	ids, contexts := checkpointIDs(t, c), []context.Context{context.Background(), done}
	t.Run("inspect", func(t *testing.T) {
		background, backgroundErr := c.Inspect(contexts[0])
		inspection, err := c.Inspect(contexts[1])
		if backgroundErr != nil || len(background.Checkpoints) != 2 {
			t.Fatal("the background Inspect row did not list both checkpoints")
		}
		if fmt.Sprint(err) != fmt.Sprint(backgroundErr) {
			t.Fatal("an already done context changed the Inspect error")
		}
		if !reflect.DeepEqual(inspection, background) {
			t.Fatal("an already done context changed the Inspect result")
		}
	})
	t.Run("delete", func(t *testing.T) {
		errs, records, kept := [2]error{}, [2][]journalEntry{}, [2]bool{}
		for index, ctx := range contexts {
			before := retainedRead(t, c, "journal/operations.jsonl")
			errs[index] = c.DeleteCheckpoint(ctx, DeleteCheckpointOptions{CheckpointID: ids[index]})
			for line := range bytes.Lines(bytes.TrimPrefix(retainedRead(t, c, "journal/operations.jsonl"), before)) {
				entry := journalEntry{}
				_ = json.Unmarshal(line, &entry)
				entry.At = time.Time{}
				records[index] = append(records[index], entry)
			}
			kept[index] = slices.Contains(checkpointIDs(t, c), ids[index])
		}
		if errs[0] != nil || kept[0] || len(records[0]) == 0 {
			t.Fatal("the background DeleteCheckpoint row did not delete its checkpoint")
		}
		if fmt.Sprint(errs[1]) != fmt.Sprint(errs[0]) {
			t.Fatal("an already done context changed the DeleteCheckpoint error")
		}
		if !slices.Equal(records[1], records[0]) {
			t.Fatal("an already done context changed the DeleteCheckpoint journal records")
		}
		if kept[1] != kept[0] {
			t.Fatal("an already done context changed whether DeleteCheckpoint removed its checkpoint")
		}
	})
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
