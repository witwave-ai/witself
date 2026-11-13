package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

type retainedFixture struct {
	cell     *Cell
	manifest Manifest
	material map[string]retainedEntry
	prefix   []byte
	launches []string
}

type retainedEntry struct {
	mode fs.FileMode
	size int64
	data string
}

// All contents are synthetic byte-preservation fixtures, not database obligation
// proofs. No process or database runs; the existing hooks record external starts.
func retainedRemovalFixture(t *testing.T, layout string, service bool) *retainedFixture {
	t.Helper()
	c, m := unitCell(t)
	if layout == "selected" {
		m.PrincipalLayoutVersion, m.PrincipalRoles = PrincipalLayoutVersion, installerRoleNames()
	}
	f := seedRetainedFixture(t, c, m)
	if service {
		prepareSupervisorTest(t, c)
		if err := c.SetServiceDesiredState(t.Context(), "stopped"); err != nil {
			t.Fatal("cannot prepare stopped service fixture")
		}
		f.material = retainedSnapshot(t, c)
	}
	return f
}

func seedRetainedFixture(t *testing.T, c *Cell, m *Manifest) *retainedFixture {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	m.AccountID, m.LastVerifiedSchemaVersion = "retained-fixture-account", 7
	for _, name := range []string{"pg", "payload", "trust"} {
		if err := privateDir(c.path(name), true); err != nil {
			t.Fatal("cannot prepare private retained directory")
		}
	}
	for name, value := range map[string]string{
		"pg/PG_VERSION": "16\n", "pg/retained-data": "inert PGDATA bytes",
		"run/unknown-private": "unknown custody", "journal/private-recovery": "synthetic byte-preservation fixture: pending operation",
		"payload/sentinel": "payload bystander", "trust/sentinel": "trust bystander", "bystander": "root bystander",
		"spool/temporary": "temporary spool", "tmp/temporary": "temporary file", "logs/temporary": "temporary log",
	} {
		if err := writeAtomicBytes(c.path(filepath.FromSlash(name)), []byte(value), nil); err != nil {
			t.Fatal("cannot publish private retention sentinel")
		}
	}
	if c.makePassword() != nil || (m.PrincipalLayoutVersion != 0 && c.makePrincipalPasswords(m) != nil) || writeAtomic(c.path("manifest.json"), m, nil) != nil {
		t.Fatal("cannot prepare generated custody fixture")
	}
	if c.begin("stop") != nil || c.journal("stop", "stop", "started") != nil || c.journal("stop", "stop", "done") != nil || c.journal("stop", "operation", "done") != nil {
		t.Fatal("cannot prepare completed journal prefix")
	}
	held, err := c.own(false)
	if err != nil {
		t.Fatal("cannot prepare stable operation lock")
	}
	_ = held.Close()
	f := &retainedFixture{cell: c, manifest: *m, prefix: retainedRead(t, c, "journal/operations.jsonl")}
	admissionRecordStart(c, &f.launches, nil)
	f.material = retainedSnapshot(t, c)
	return f
}

func retainedRead(t *testing.T, c *Cell, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(c.path(filepath.FromSlash(name)))
	if err != nil {
		t.Fatal("retained fixture bytes missing")
	}
	return data
}

func retainedSnapshot(t *testing.T, c *Cell) map[string]retainedEntry {
	t.Helper()
	result := map[string]retainedEntry{}
	err := filepath.WalkDir(c.cfg.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(c.cfg.Root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := retainedEntry{mode: info.Mode()}
		if !entry.IsDir() {
			value.size = info.Size()
		}
		// Windows locks the empty lock byte range mandatorily. Its zero size
		// proves the complete bytes without reopening a held lock handle.
		if !entry.IsDir() && !strings.HasSuffix(name, ".lock") {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value.data = string(data)
		}
		result[filepath.ToSlash(name)] = value
		return nil
	})
	if err != nil {
		t.Fatal("cannot snapshot synthetic retained tree")
	}
	return result
}

func retainedAssertMaterial(t *testing.T, f *retainedFixture) {
	t.Helper()
	after := retainedSnapshot(t, f.cell)
	custody := []string{"run/postgres.password"}
	for _, role := range f.manifest.PrincipalRoles {
		custody = append(custody, "run/credentials/"+role)
	}
	for _, name := range custody {
		if before, exists := f.material[name]; exists && after[name] != before {
			t.Fatal("original bootstrap/selected credential bytes missing or changed")
		}
	}
	if after["journal/private-recovery"] != f.material["journal/private-recovery"] ||
		!bytes.HasPrefix([]byte(after["journal/operations.jsonl"].data), f.prefix) {
		t.Fatal("prior journal prefix or extra recovery record missing")
	}
	for name, before := range f.material {
		if name == "manifest.json" || name == "journal/operations.jsonl" || name == "service/descriptor.json" {
			continue
		}
		if got, ok := after[name]; !ok || got != before {
			t.Logf("retained material class=%s", retainedArea(name))
			t.Fatal("retained original bytes or private mode missing")
		}
	}
	if !bytes.HasPrefix(retainedRead(t, f.cell, "journal/operations.jsonl"), f.prefix) {
		t.Fatal("prior journal prefix lost")
	}
	if f.cell.validateRoot(false) != nil {
		t.Fatal("retained tree lost private ownership or ACL validity")
	}
	m, err := readManifest(f.cell.cfg.Root)
	if err != nil || !m.RemovalRetained {
		t.Fatal("retained marker missing")
	}
	identity := *m
	identity.RemovalRetained, identity.LastOperation, identity.Cleanup = f.manifest.RemovalRetained, f.manifest.LastOperation, f.manifest.Cleanup
	if !reflect.DeepEqual(identity, f.manifest) {
		t.Fatal("removal changed initialized installation or principal identity")
	}
	for _, component := range []string{"postgres", "server", "worker"} {
		state, err := f.cell.processState(component)
		if err != nil || state.Alive {
			t.Fatal("retained cell has a running process")
		}
	}
	if len(f.launches) != 0 {
		t.Fatal("retained cell launched a writer")
	}
}

func retainedAssertSettled(t *testing.T, c *Cell) {
	t.Helper()
	p, err := c.readJournal()
	m, manifestErr := readManifest(c.cfg.Root)
	if err != nil || manifestErr != nil || p.pending != nil || p.tornTail || m.Cleanup != nil || !m.RemovalRetained {
		t.Fatal("retained cleanup falsely settled or left unresolved operation")
	}
	before := retainedSnapshot(t, c)
	if c.Repair(t.Context()) != nil || !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
		t.Fatal("second Repair changed settled bytes or appended duplicate completion")
	}
}

func TestRemovalRetainsDurableMaterial(t *testing.T) {
	for _, layout := range []string{"legacy", "selected"} {
		for _, service := range []bool{false, true} {
			name := layout + "/bare"
			if service {
				name = layout + "/service"
			}
			t.Run(name, func(t *testing.T) {
				f := retainedRemovalFixture(t, layout, service)
				err := f.cell.Remove(t.Context(), RemoveOptions{})
				retainedAssertMaterial(t, f)
				if err != nil {
					t.Fatal(err)
				}
				retainedAssertSettled(t, f.cell)
			})
		}
	}
}

func TestRetainedRemovalDisablesLaunch(t *testing.T) {
	for _, route := range []string{"direct", "supervisor", "postgres_helper"} {
		t.Run(route, func(t *testing.T) {
			f := retainedRemovalFixture(t, "legacy", route == "supervisor")
			c := f.cell
			if c.Remove(t.Context(), RemoveOptions{}) != nil {
				t.Fatal("cannot retain launch fixture")
			}
			start := c.Start
			switch route {
			case "supervisor":
				if c.SetServiceDesiredState(t.Context(), "running") != nil {
					t.Fatal("running intent attempt was not admitted")
				}
				// The actual default hook is production Start. Only external process
				// boundaries are recorded; no supervisor start hook is substituted.
				start = newSupervisor(c, SupervisorPolicy{}).hooks.start
			case "postgres_helper":
				c.cfg.Binaries.PostgresDir = t.TempDir() // Absent executable: no native PostgreSQL can run.
				m, err := readManifest(c.cfg.Root)
				if err != nil {
					t.Fatal(err)
				}
				start = func(ctx context.Context) error { return c.startServicePostgres(ctx, m) }
			default:
				// A removed Start guard must be caught at begin("start") even
				// while the independent PostgreSQL guard still prevents launch.
				c.installerStart.postgres = c.startServicePostgres
			}
			before := retainedSnapshot(t, c)
			err := start(t.Context())
			if len(f.launches) != 0 {
				t.Fatal("retained running intent launched PostgreSQL/server/worker")
			}
			if !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
				t.Fatal("retained launch had journal-begin, manifest, or postgres effects")
			}
			if err == nil || err.Error() != "local cell installation is incomplete" {
				t.Fatal("retained launch did not return incomplete-install refusal")
			}
		})
	}
}

func TestRetainedRemovalMarkerSurvivesOperations(t *testing.T) {
	f := retainedRemovalFixture(t, "selected", true)
	c := f.cell
	if c.Remove(t.Context(), RemoveOptions{}) != nil {
		t.Fatal("cannot prepare retained fixture")
	}
	for _, action := range []struct {
		name string
		run  func(context.Context) error
	}{{"Stop", c.Stop}, {"Repair", c.Repair}, {"RepairAgain", c.Repair}, {"Remove", func(ctx context.Context) error { return c.Remove(ctx, RemoveOptions{}) }}, {"Observe", func(ctx context.Context) error {
		before := retainedSnapshot(t, c)
		inspection, err := c.Inspect(ctx)
		if err == nil && inspection.InstallState != "removed; data retained; reinstall unsupported" {
			t.Fatal("retained inspection advised adoption or purge")
		}
		if !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
			t.Fatal("inspection wrote retained material")
		}
		return err
	}}} {
		t.Run(action.name, func(t *testing.T) {
			if action.run(t.Context()) != nil {
				t.Fatal("retained operation failed")
			}
			retainedAssertMaterial(t, f)
			if c.SetServiceDesiredState(t.Context(), "running") != nil {
				t.Fatal("cannot force a running intent retry")
			}
			before := retainedSnapshot(t, c)
			if c.Start(t.Context()) == nil || !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
				t.Fatal("marker/no-launch assertion failed after " + action.name)
			}
			if c.SetServiceDesiredState(t.Context(), "stopped") != nil {
				t.Fatal("cannot restore stopped intent")
			}
		})
	}
}

func TestRetainedRemovalCrashRepair(t *testing.T) {
	for _, cut := range []string{"stop", "removal_retained", "cleanup_checkpoint", "runtime_cleanup", "operation_done", "before_cleanup_clear", "cleanup_clear"} {
		t.Run(cut, func(t *testing.T) {
			f := retainedRemovalFixture(t, "selected", false)
			c := f.cell
			fired := false
			c.cfg.Failpoint = func(op, step string) error {
				if op == "remove" && step == cut {
					fired = true
					if cut == "before_cleanup_clear" {
						m, err := readManifest(c.cfg.Root)
						if err != nil || m.Cleanup == nil {
							t.Fatal("Cleanup cleared before recorded completion boundary")
						}
					}
					return errors.New("synthetic crash")
				}
				return nil
			}
			if err := c.Remove(t.Context(), RemoveOptions{}); !fired || !errors.Is(err, errInterrupted) {
				t.Fatal("retained crash boundary was not exercised")
			}
			prefix := retainedRead(t, c, "journal/operations.jsonl")
			c.cfg.Failpoint = nil
			if err := c.Repair(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(retainedRead(t, c, "journal/operations.jsonl"), prefix) {
				t.Fatal("repair lost acknowledged journal prefix")
			}
			retainedAssertMaterial(t, f)
			retainedAssertSettled(t, c)
		})
	}
	for _, boundary := range []string{"cleanup_checkpoint", "runtime_cleanup"} {
		t.Run("SyncFailure_"+boundary, func(t *testing.T) {
			f := retainedRemovalFixture(t, "legacy", false)
			c := f.cell
			original := syncFile
			t.Cleanup(func() { syncFile = original })
			armed, fired := false, false
			c.cfg.Failpoint = func(op, step string) error {
				if op == "remove" && step == boundary {
					armed = true
				}
				return nil
			}
			syncFile = func(file *os.File) error {
				if armed && !fired && filepath.Base(file.Name()) == "operations.jsonl" {
					fired = true
					return errors.New("synthetic sync failure")
				}
				return original(file)
			}
			if err := c.Remove(t.Context(), RemoveOptions{}); !fired || err == nil || err.Error() != "cannot sync local cell journal" {
				t.Fatal("first-run sync cause was lost or cleanup fell back destructively")
			}
			if boundary == "runtime_cleanup" {
				// Physical operation-done is present, but a returned sync failure
				// never authorizes clearing Cleanup without a successful retry sync.
				before := retainedSnapshot(t, c)
				fired = false
				if err := c.Repair(t.Context()); !fired || err == nil || err.Error() != "cannot sync local cell journal" {
					t.Fatal("completion retry ignored journal sync failure")
				}
				m, err := readManifest(c.cfg.Root)
				if err != nil || m.Cleanup == nil || !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
					t.Fatal("completion sync failure lost checkpoint or duplicated history")
				}
			}
			syncFile, c.cfg.Failpoint = original, nil
			retainedAssertMaterial(t, f)
			if c.Repair(t.Context()) != nil {
				t.Fatal("sync interruption could not settle")
			}
			retainedAssertSettled(t, c)
		})
	}
}

func TestRetainedRemovalLegacyCleanup(t *testing.T) {
	for _, state := range []string{"neither", "run_only", "journal_only", "partial_custody", "completed_journal"} {
		t.Run(state, func(t *testing.T) {
			f := retainedRemovalFixture(t, "selected", false)
			c := f.cell
			m := f.manifest
			if state == "completed_journal" {
				if c.begin("remove") != nil || c.journal("remove", "runtime_cleanup", "started") != nil || c.journal("remove", "runtime_cleanup", "done") != nil || c.journal("remove", "operation", "done") != nil {
					t.Fatal("cannot prepare completed physical removal")
				}
			} else if state != "journal_only" && state != "partial_custody" {
				if os.RemoveAll(c.path("journal")) != nil {
					t.Fatal("cannot prepare predecessor journal absence")
				}
			}
			switch state {
			case "neither", "journal_only":
				if os.RemoveAll(c.path("run")) != nil {
					t.Fatal("cannot prepare predecessor custody absence")
				}
			case "partial_custody":
				path, _ := c.credentialPath(credentialWorker)
				if removePrivateFile(path) != nil {
					t.Fatal("cannot prepare partial selected custody")
				}
			}
			m.Cleanup = &journalEntry{Operation: "remove", Step: "runtime_cleanup", State: "started", At: time.Now().UTC()}
			if writeAtomic(c.path("manifest.json"), &m, nil) != nil {
				t.Fatal("cannot prepare predecessor checkpoint")
			}
			before := retainedSnapshot(t, c)
			original := beforePasswordWrite
			minted := 0
			t.Cleanup(func() { beforePasswordWrite = original })
			beforePasswordWrite = func(*os.File) { minted++ }
			if c.Repair(t.Context()) != nil {
				t.Fatal("predecessor cleanup repair failed")
			}
			after := retainedSnapshot(t, c)
			delete(before, "manifest.json")
			delete(after, "manifest.json")
			if minted != 0 || !reflect.DeepEqual(before, after) {
				t.Fatal("predecessor repair invented custody/history or changed surviving bytes")
			}
			retainedAssertSettled(t, c)
			if c.Start(t.Context()) == nil {
				t.Fatal("missing/partial predecessor custody became startable")
			}
		})
	}
	t.Run("foreign_pending_with_torn_tail", func(t *testing.T) {
		f := retainedRemovalFixture(t, "selected", false)
		c := f.cell
		if c.begin("start") != nil {
			t.Fatal("cannot prepare foreign pending operation")
		}
		prefix := retainedRead(t, c, "journal/operations.jsonl")
		if writeAtomicBytes(c.path("journal", "operations.jsonl"), append(prefix, []byte(`{"operation":`)...), nil) != nil {
			t.Fatal("cannot prepare foreign torn tail")
		}
		m := f.manifest
		m.Cleanup = &journalEntry{Operation: "remove", Step: "runtime_cleanup", State: "started", At: time.Now().UTC()}
		if writeAtomic(c.path("manifest.json"), &m, nil) != nil {
			t.Fatal("cannot prepare conflicting manifest checkpoint")
		}
		before := retainedSnapshot(t, c)
		if c.Repair(t.Context()) == nil || !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
			t.Fatal("foreign cleanup repair changed evidence or truncated unowned tail")
		}
	})
}

// retainedServiceAdapter gives this test's own installed cell a descriptor that
// matches its root, owner and installation identity, and wires it to the
// family's existing fake manager. Nothing registers until install runs.
func retainedServiceAdapter(t *testing.T, family, owner string) (*serviceRemovalFixture, func()) {
	t.Helper()
	if family == "Launchd" {
		a, d, manager := launchdFixture(t)
		a.uid, d.UID = owner, owner
		manager.d = d
		manager.enabled[a.target(d)] = false
		unixServicePersistDescriptor(t, manager.cell, d)
		return &serviceRemovalFixture{t: t, cell: manager.cell, d: d, launch: a, lm: manager}, func() { launchdInstallFixture(t, a, d) }
	}
	// The fake's own unit cell is installed with a private, canonical root that
	// the Windows validators accept, and the fake's lock seam already guards it.
	win := newWindowsTaskFake(t)
	c := win.cell
	m, err := readManifest(c.cfg.Root)
	if err != nil {
		t.Fatal("cannot read Windows task fixture manifest")
	}
	// A Windows installation allocates the identity shape a task name accepts.
	m.InstallationID, err = newLocalCellInstallationID("windows")
	if err != nil || writeAtomic(c.path("manifest.json"), m, nil) != nil {
		t.Fatal("cannot publish Windows task fixture identity")
	}
	d := ServiceDescriptor{Version: 1, InstallationID: m.InstallationID, UID: owner, Root: c.cfg.Root, Adapter: "windows-logon-task", Scope: "user",
		Runtime: "local", DesiredState: "running", ResourceProfile: "unconfigured", Revision: 1,
		Payload: ServicePayload{Host: filepath.Join(filepath.Dir(c.cfg.Root), "witself.exe"), Generation: "generation-1", Args: []string{"run", "--root", c.cfg.Root}}}
	unixServicePersistDescriptor(t, c, d)
	win.d, win.current, win.a.uid = d, d, owner
	// The descriptor-file seam of newServiceRemovalFixture: the adapter reads
	// this cell's durable descriptor, never the fake's in-memory copy.
	win.a.readDescriptor = func(ServiceDescriptor) (ServiceDescriptor, error) {
		var fresh ServiceDescriptor
		err := readJSON(c.path("service", "descriptor.json"), &fresh)
		return fresh, err
	}
	return &serviceRemovalFixture{t: t, cell: c, d: d, win: win}, win.apply
}

func TestRetainedRemovalServiceRetryAndPurge(t *testing.T) {
	// Each phase logs a closed class first, so a CI failure from a helper's raw
	// error still shows how far the test got.
	phase := func(name string) { t.Logf("retained service phase class=%s", name) }
	phase("setup")
	// Windows ownership uses SIDs rather than launchd numeric user IDs, so the
	// Windows row registers this cell as a logon task. Every phase is shared.
	family := "Launchd"
	if runtime.GOOS == "windows" {
		family = "Windows"
	}
	owner, err := serviceOwner()
	if err != nil {
		t.Fatal("cannot resolve fixture service owner")
	}
	adapter, install := retainedServiceAdapter(t, family, owner)
	adapter.setDrain(func(context.Context, ServiceDescriptor) error {
		fresh, err := adapter.cell.ServiceDescriptor()
		if err != nil || fresh.DesiredState != "stopped" {
			return ErrServiceDesiredState
		}
		return nil
	})
	phase("install")
	install()
	serviceRemovalStopped(t, adapter.cell, adapter.d)
	phase("hold_host")
	release := serviceRemovalHoldHost(t, adapter.cell)
	release()
	c := adapter.cell
	phase("manifest")
	m, err := readManifest(c.cfg.Root)
	if err != nil {
		t.Fatal("cannot read retained service manifest")
	}
	phase("seed")
	f := seedRetainedFixture(t, c, m)
	steps := []string{"remove_first", "stop", "repair", "remove_again"}
	for i, action := range []func(context.Context) error{
		func(ctx context.Context) error { return c.Remove(ctx, RemoveOptions{}) }, c.Stop, c.Repair,
		func(ctx context.Context) error { return c.Remove(ctx, RemoveOptions{}) },
	} {
		phase(steps[i])
		if err := action(t.Context()); err != nil {
			t.Logf("retained service retry class=%s", steps[i])
			t.Fatal("service retained lifecycle retry failed")
		}
		if _, err := c.ServiceDescriptor(); err != nil {
			t.Fatal("retained removal invalidated ordinary descriptor identity")
		}
		retainedAssertMaterial(t, f)
	}
	phase("adapter_remove")
	before := retainedSnapshot(t, c)
	if adapter.remove() != nil || adapter.registered() {
		t.Fatal("fake manager unregister did not complete")
	}
	if !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
		t.Fatal("adapter removal mutated lifecycle custody")
	}
	phase("purge")
	if c.Remove(t.Context(), RemoveOptions{Purge: true}) != nil {
		t.Fatal("uninterrupted explicit service purge failed")
	}
	phase("verify")
	for _, name := range []string{"pg", "run", "journal", "manifest.json"} {
		if _, err := os.Lstat(c.path(name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("explicit purge left an established deletion target")
		}
	}
	if string(retainedRead(t, c, "payload/sentinel")) != "payload bystander" || string(retainedRead(t, c, "trust/sentinel")) != "trust bystander" {
		t.Fatal("purge expanded into payload/trust bystanders")
	}
}

func TestRetainedRemovalPurgeRequiresCurrentCall(t *testing.T) {
	for _, service := range []bool{false, true} {
		name := "bare"
		if service {
			name = "service_baseline_refusal"
		}
		t.Run(name, func(t *testing.T) {
			f := retainedRemovalFixture(t, "legacy", service)
			c := f.cell
			c.cfg.Failpoint = func(op, step string) error {
				if op == "remove" && step == "purge" {
					// Model a residual owned page after an interrupted destructive
					// phase; this does not represent a usable database.
					if privateDir(c.path("pg"), true) != nil || writeAtomicBytes(c.path("pg", "residual"), []byte("residual PG evidence"), nil) != nil {
						t.Fatal("cannot prepare residual purge evidence")
					}
					return errors.New("synthetic interrupted purge")
				}
				return nil
			}
			if !errors.Is(c.Remove(t.Context(), RemoveOptions{Purge: true}), errInterrupted) {
				t.Fatal("purge interruption not reached")
			}
			c.cfg.Failpoint = nil
			if c.Repair(t.Context()) != nil || string(retainedRead(t, c, "pg/residual")) != "residual PG evidence" {
				t.Fatal("Repair acquired purge authority without current consent")
			}
			retainedAssertSettled(t, c)
			before := retainedSnapshot(t, c)
			err := c.Remove(t.Context(), RemoveOptions{Purge: true})
			if service {
				if err == nil || !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
					t.Fatal("post-Installed=false service retry baseline was weakened or erased evidence")
				}
			} else if _, statErr := os.Lstat(c.path("pg")); err != nil || !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("fresh explicit bare-cell purge did not remove residual evidence")
			}
		})
	}
}

func TestRetainedRemovalRefusalsPreserveBytes(t *testing.T) {
	for _, kind := range []string{"fence", "corrupt_fence", "contention", "unsafe_tree", "foreign_process", "corrupt_journal", "foreign_operation"} {
		t.Run(kind, func(t *testing.T) {
			f := retainedRemovalFixture(t, "legacy", false)
			c := f.cell
			switch kind {
			case "fence", "corrupt_fence":
				updateTestFence(t, c)
				if kind == "corrupt_fence" && writeAtomicBytes(c.path("journal", "maintenance.json"), []byte("{broken\n"), nil) != nil {
					t.Fatal("cannot prepare corrupt fence")
				}
			case "contention":
				held, err := c.own(false)
				if err != nil {
					t.Fatal("cannot hold operation lock")
				}
				t.Cleanup(func() { _ = held.Close() })
			case "unsafe_tree":
				if os.Rename(c.path("run"), c.path("saved-run")) != nil || writeAtomicBytes(c.path("run"), []byte("wrong object type"), nil) != nil {
					t.Fatal("cannot prepare unsafe private tree")
				}
			case "foreign_process":
				if writeAtomic(c.path("run", "server.json"), ProcessRecord{PID: os.Getpid(), StartTime: "foreign-incarnation", Root: c.cfg.Root, Component: "server"}, nil) != nil {
					t.Fatal("cannot prepare foreign identity")
				}
			case "corrupt_journal":
				if writeAtomicBytes(c.path("journal", "operations.jsonl"), append(f.prefix, []byte("corrupt interior\n{}\n")...), nil) != nil {
					t.Fatal("cannot prepare corrupt journal")
				}
			case "foreign_operation":
				if c.begin("start") != nil {
					t.Fatal("cannot prepare unrelated pending operation")
				}
			}
			before := retainedSnapshot(t, c)
			if c.Remove(t.Context(), RemoveOptions{}) == nil || !reflect.DeepEqual(before, retainedSnapshot(t, c)) || len(f.launches) != 0 {
				t.Fatal("fenced refusal changed durable bytes or performed cleanup")
			}
		})
	}
}

func TestRetainedRemovalDoesNotAdopt(t *testing.T) {
	for _, selected := range []bool{false, true} {
		f := retainedRemovalFixture(t, "selected", false)
		c := f.cell
		if c.Remove(t.Context(), RemoveOptions{}) != nil {
			t.Fatal("cannot prepare retained adoption refusal")
		}
		before := retainedSnapshot(t, c)
		if c.Install(t.Context(), InstallOptions{ManagedPrincipals: selected}) == nil || !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
			t.Fatal("Install adopted retained data or minted replacement custody")
		}
	}
}

func TestRetainedRemovalManifestCompatibility(t *testing.T) {
	f := retainedRemovalFixture(t, "legacy", false)
	m := f.manifest
	// Construct the exact predecessor wire type, omitting only the new field.
	current := reflect.TypeOf(m)
	var fields []reflect.StructField
	for i := 0; i < current.NumField(); i++ {
		if current.Field(i).Name != "RemovalRetained" {
			fields = append(fields, current.Field(i))
		}
	}
	old := reflect.New(reflect.StructOf(fields))
	value := reflect.ValueOf(m)
	for i := 0; i < old.Elem().NumField(); i++ {
		old.Elem().Field(i).Set(value.FieldByName(fields[i].Name))
	}
	legacy, err := json.MarshalIndent(old.Interface(), "", "  ")
	if err != nil {
		t.Fatal("cannot encode predecessor fixture")
	}
	currentBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil || !bytes.Equal(currentBytes, legacy) || bytes.Contains(currentBytes, []byte("removal_retained")) {
		t.Fatal("false retained marker changed predecessor manifest bytes")
	}
	m.RemovalRetained = true
	for _, installed := range []bool{true, false} {
		m.Installed = installed
		if writeAtomic(f.cell.path("manifest.json"), &m, nil) != nil {
			t.Fatal("cannot publish marked compatibility fixture")
		}
		got, err := readManifest(f.cell.cfg.Root)
		if err != nil || !reflect.DeepEqual(got, &m) {
			t.Fatal("current reader rejected marked/interrupted-purge manifest")
		}
		decoder := json.NewDecoder(bytes.NewReader(retainedRead(t, f.cell, "manifest.json")))
		decoder.DisallowUnknownFields()
		if decoder.Decode(old.Interface()) == nil {
			t.Fatal("predecessor strict reader ignored retained launch refusal")
		}
	}
}

// retainedArea names the top-level area of a retained entry with a closed,
// value-free word so a CI failure says where the tree changed.
func retainedArea(name string) string {
	top, _, _ := strings.Cut(name, "/")
	for _, area := range []string{"pg", "run", "journal", "service", "payload", "trust", "spool", "tmp", "logs"} {
		if top == area {
			return area
		}
	}
	return "root"
}
