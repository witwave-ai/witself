package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// These fixtures observe existing per-Cell boundaries. They do not replace the
// operation lock, maintenance reader, journal, or lifecycle orchestration.
type admissionFixture struct {
	cell         *Cell
	run          func(context.Context) error
	trace        *[]string
	controlError string
}

func admissionRecordStart(c *Cell, trace *[]string, probe func(string)) {
	record := func(name string) {
		*trace = append(*trace, name)
		if probe != nil {
			probe(name)
		}
	}
	c.installerStart = &installerStartOperations{
		postgres: func(context.Context, *Manifest) error { record("postgres"); return nil },
		verify:   func(context.Context, *Manifest) error { record("verify"); return nil },
		launch: func(_ context.Context, _ *Manifest, component string, prepare func() error) error {
			if err := prepare(); err != nil {
				return err
			}
			record(component)
			return nil
		},
		ready: func(_ context.Context, _ *Manifest, component string, _ bool) error {
			record(component + "_ready")
			return nil
		},
		account: func(context.Context, *Manifest) error { record("account"); return nil },
	}
}

func newAdmissionFixture(t *testing.T, name string) admissionFixture {
	t.Helper()
	var c *Cell
	var m *Manifest
	switch name {
	case "StartSelected":
		c, m, _ = selectedCredentialFixture(t)
	case "RepairSelectedResume":
		c, m = selectedJournalFixture(t)
	default:
		c, m = unitCell(t)
	}
	trace := []string{}
	f := admissionFixture{cell: c, trace: &trace}
	switch name {
	case "Install":
		if os.Remove(c.path("manifest.json")) != nil {
			t.Fatal("cannot prepare new installation")
		}
		c.afterPortAllocation = func(Ports) { trace = append(trace, "allocate") }
		f.run = func(ctx context.Context) error { return c.Install(ctx, InstallOptions{}) }
		f.controlError = "cannot read PostgreSQL major/minor version"
	case "StartLegacy", "StartSelected":
		if selectedFixtureDataDirectory(c) != nil {
			t.Fatal("cannot prepare startup data")
		}
		if name == "StartLegacy" && c.makePassword() != nil {
			t.Fatal("cannot prepare startup credential")
		}
		admissionRecordStart(c, &trace, nil)
		f.run = c.Start
	case "RepairSelectedResume":
		if selectedFixtureDataDirectory(c) != nil {
			t.Fatal("cannot prepare repair data")
		}
		ops := &installerPrincipalOperations{}
		for _, field := range []struct {
			name   string
			target *func(context.Context, *Manifest) error
		}{
			{"resume", &ops.resumePostgres}, {"credentials", &ops.credentials}, {"provision", &ops.provision},
			{"migrate", &ops.migrate}, {"grants", &ops.grants}, {"verify", &ops.verify},
		} {
			*field.target = func(context.Context, *Manifest) error { trace = append(trace, field.name); return nil }
		}
		c.installerPrincipals = ops
		f.run = c.Repair
	case "RepairCleanup":
		m.Cleanup = &journalEntry{Operation: "remove", Step: "runtime_cleanup", State: "started", At: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}
		if c.begin("remove") != nil || writeAtomic(c.path("manifest.json"), m, nil) != nil || c.journal("remove", "runtime_cleanup", "started") != nil {
			t.Fatal("cannot prepare cleanup recovery")
		}
		f.run = c.Repair
	case "RepairNoop", "RepairTornTail":
		if name == "RepairTornTail" && testWriteFile(c.path("journal", "operations.jsonl"), []byte(`{"operation":`), 0o600) != nil {
			t.Fatal("cannot prepare torn tail")
		}
		f.run = c.Repair
	case "RemoveRetain", "RemovePurge":
		if selectedFixtureDataDirectory(c) != nil || testWriteFile(c.path("pg", "retained-data"), []byte("retained"), 0o600) != nil {
			t.Fatal("cannot prepare removal data")
		}
		f.run = func(ctx context.Context) error { return c.Remove(ctx, RemoveOptions{Purge: name == "RemovePurge"}) }
	default:
		t.Fatal("unknown admission fixture")
	}
	c.cfg.Failpoint = func(operation, step string) error { trace = append(trace, operation+":"+step); return nil }
	// Create the stable lock before byte inventories, as production admission
	// may create this persistent inode even when the requested work is refused.
	lock, err := c.ownExisting()
	if err != nil {
		t.Fatal("cannot prepare fixture lock")
	}
	_ = lock.Close()
	return f
}

var admissionNames = []string{"Install", "StartLegacy", "StartSelected", "RepairSelectedResume", "RepairCleanup", "RepairNoop", "RepairTornTail", "RemoveRetain", "RemovePurge"}

func admissionSnapshot(t *testing.T, c *Cell) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, name := range []string{"manifest.json", "journal", "journal/operations.jsonl", "journal/maintenance.json", "pg", "pg/retained-data", "run", "spool", "tmp", "logs", "service/descriptor.json"} {
		info, err := os.Lstat(c.path(name))
		if errors.Is(err, os.ErrNotExist) {
			result[name] = "absent"
			continue
		}
		if err != nil {
			t.Fatal("cannot inspect admission fixture")
		}
		if info.IsDir() {
			result[name] = "directory"
			continue
		}
		data, err := os.ReadFile(c.path(name))
		if err != nil {
			t.Fatal("cannot snapshot admission fixture")
		}
		result[name] = "bytes:" + string(data)
	}
	return result
}

func assertAdmissionControl(t *testing.T, f admissionFixture, name string) {
	t.Helper()
	err := f.run(t.Context())
	if f.controlError != "" {
		if err == nil || err.Error() != f.controlError {
			t.Fatal("unfenced control did not reach its expected boundary")
		}
	} else if err != nil {
		t.Fatal("unfenced control refused eligible lifecycle")
	}
	if name != "RepairNoop" && name != "RepairTornTail" && len(*f.trace) == 0 {
		t.Fatal("unfenced control did not reach guarded effect")
	}
	switch name {
	case "RepairSelectedResume":
		if !slices.Contains(*f.trace, "migrate") || !slices.Contains(*f.trace, "resume") {
			t.Fatal("unfenced repair did not reach migration and resume")
		}
	case "RepairCleanup", "RemoveRetain", "RemovePurge":
		if _, err := os.Stat(f.cell.path("journal")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unfenced cleanup did not remove journal")
		}
	case "RepairTornTail":
		data, err := os.ReadFile(f.cell.path("journal", "operations.jsonl"))
		if err != nil || len(data) != 0 {
			t.Fatal("unfenced repair did not discard torn tail")
		}
	}
}

func TestMaintenanceFenceLifecycleAdmissions(t *testing.T) {
	for _, name := range admissionNames {
		t.Run(name, func(t *testing.T) {
			t.Run("Control", func(t *testing.T) { assertAdmissionControl(t, newAdmissionFixture(t, name), name) })
			for _, state := range []string{"Valid", "Corrupt", "Foreign"} {
				t.Run(state, func(t *testing.T) {
					f := newAdmissionFixture(t, name)
					updateTestFence(t, f.cell)
					if state == "Corrupt" && testWriteFile(f.cell.path("journal", "maintenance.json"), []byte(`{"generation":`), 0o600) != nil {
						t.Fatal("cannot prepare corrupt fence")
					}
					if state == "Foreign" && testWriteFile(f.cell.path("journal", "maintenance.json"), []byte(`{"generation":1,"operation_id":"foreign-owner","since":"2026-11-10T00:00:00Z","foreign":"retained"}`), 0o600) != nil {
						t.Fatal("cannot prepare foreign fence evidence")
					}
					before := admissionSnapshot(t, f.cell)
					err := f.run(t.Context())
					if err != errMaintenanceFenceActive || err.Error() != "local_maintenance_fence_active" {
						t.Fatal("eligible admission did not return exact maintenance cause")
					}
					if len(*f.trace) != 0 {
						t.Fatal("fenced admission reached guarded effect")
					}
					if !reflect.DeepEqual(before, admissionSnapshot(t, f.cell)) {
						t.Fatal("fenced admission changed manifest journal fence or cleanup targets")
					}
				})
			}
		})
	}
}

// Each fixture is constructed directly, so an erroneous absent-fence refusal
// in one API cannot prevent the other APIs from reaching their own admissions.
func TestMaintenanceFenceAbsentAdmissionControls(t *testing.T) {
	for _, name := range admissionNames {
		t.Run(name, func(t *testing.T) {
			assertAdmissionControl(t, newAdmissionFixture(t, name), name)
		})
	}
}

func TestMaintenanceFenceLegacyOrdering(t *testing.T) {
	for _, name := range []string{"Install", "StartLegacy", "RepairNoop", "RemoveRetain"} {
		t.Run(name, func(t *testing.T) {
			t.Run("Lock", func(t *testing.T) {
				f := newAdmissionFixture(t, name)
				updateTestFence(t, f.cell)
				lock, err := f.cell.ownExisting()
				if err != nil {
					t.Fatal("cannot hold contender lock")
				}
				defer func() { _ = lock.Close() }()
				before := admissionSnapshot(t, f.cell)
				if !errors.Is(f.run(t.Context()), ErrLocked) {
					t.Fatal("maintenance displaced lock refusal")
				}
				if !reflect.DeepEqual(before, admissionSnapshot(t, f.cell)) {
					t.Fatal("lock refusal mutated fixture")
				}
			})
			t.Run("MalformedManifest", func(t *testing.T) {
				f := newAdmissionFixture(t, name)
				if testWriteFile(f.cell.path("manifest.json"), []byte(`{"layout_version":`), 0o600) != nil {
					t.Fatal("cannot corrupt manifest")
				}
				err := f.run(t.Context())
				if err == nil || err.Error() != "invalid local cell record" {
					t.Fatal("malformed manifest control missed legacy refusal")
				}
				updateTestFence(t, f.cell)
				before := admissionSnapshot(t, f.cell)
				err = f.run(t.Context())
				if name == "RemoveRetain" {
					if err != errMaintenanceFenceActive {
						t.Fatal("no-service Remove did not put admission before manifest")
					}
				} else if err == nil || err.Error() != "invalid local cell record" {
					t.Fatal("maintenance displaced earlier malformed manifest refusal")
				}
				if !reflect.DeepEqual(before, admissionSnapshot(t, f.cell)) {
					t.Fatal("manifest refusal mutated fixture")
				}
			})
			t.Run("InvalidJournalParent", func(t *testing.T) {
				f := newAdmissionFixture(t, name)
				if os.Remove(f.cell.path("journal")) != nil || testWriteFile(f.cell.path("journal"), []byte("invalid-parent"), 0o600) != nil {
					t.Fatal("cannot prepare invalid journal parent")
				}
				want := "local cell directory is missing or not a real directory"
				if name == "RemoveRetain" {
					want = hostExitRefused("envelope").Error()
				}
				if err := f.run(t.Context()); err == nil || err.Error() != want {
					t.Fatal("invalid parent lost legacy root refusal")
				}
			})
			t.Run("CorruptJournal", func(t *testing.T) {
				f := newAdmissionFixture(t, name)
				if testWriteFile(f.cell.path("journal", "operations.jsonl"), []byte("invalid\n"), 0o600) != nil {
					t.Fatal("cannot corrupt journal")
				}
				want := "invalid local cell journal; manual recovery required"
				if name == "Install" {
					want = "cannot read PostgreSQL major/minor version"
				}
				if err := f.run(t.Context()); err == nil || err.Error() != want {
					t.Fatal("corrupt journal control changed first legacy refusal")
				}
				*f.trace = nil
				updateTestFence(t, f.cell)
				before := admissionSnapshot(t, f.cell)
				if f.run(t.Context()) != errMaintenanceFenceActive {
					t.Fatal("later corrupt journal displaced maintenance refusal")
				}
				if !reflect.DeepEqual(before, admissionSnapshot(t, f.cell)) {
					t.Fatal("corrupt-journal refusal mutated fixture")
				}
			})
			t.Run("PreCanceled", func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				control := newAdmissionFixture(t, name)
				want := ""
				switch name {
				case "Install":
					want = "cannot read PostgreSQL major/minor version"
				case "StartLegacy":
					want = "local cell start: postgres failed"
				case "RemoveRetain":
					if runtime.GOOS == "windows" {
						want = "local cell remove: stop failed"
					}
				}
				controlErr := control.run(ctx)
				if (want == "" && controlErr != nil) || (want != "" && (controlErr == nil || controlErr.Error() != want)) {
					t.Fatal("pre-canceled control changed API-specific legacy ordering")
				}
				f := newAdmissionFixture(t, name)
				updateTestFence(t, f.cell)
				if f.run(ctx) != errMaintenanceFenceActive {
					t.Fatal("pre-cancellation displaced admission without service predrain")
				}
			})
		})
	}
	t.Run("InstallExistingManifest", func(t *testing.T) {
		c, _ := unitCell(t)
		updateTestFence(t, c)
		if err := c.Install(t.Context(), InstallOptions{}); err == nil || err.Error() != "local cell is already installed" {
			t.Fatal("maintenance displaced existing installation refusal")
		}
	})
	t.Run("InstallExistingPGDATA", func(t *testing.T) {
		f := newAdmissionFixture(t, "Install")
		if selectedFixtureDataDirectory(f.cell) != nil || testWriteFile(f.cell.path("pg", "retained-data"), []byte("retained"), 0o600) != nil {
			t.Fatal("cannot prepare existing data")
		}
		updateTestFence(t, f.cell)
		if err := f.run(t.Context()); err == nil || err.Error() != "refusing existing PostgreSQL data; repair required" {
			t.Fatal("maintenance displaced existing PGDATA refusal")
		}
	})
	t.Run("StartIncomplete", func(t *testing.T) {
		c, m := unitCell(t)
		m.Installed = false
		if writeAtomic(c.path("manifest.json"), m, nil) != nil {
			t.Fatal("cannot prepare incomplete manifest")
		}
		updateTestFence(t, c)
		if err := c.Start(t.Context()); err == nil || err.Error() != "local cell installation is incomplete" {
			t.Fatal("maintenance displaced incomplete installation refusal")
		}
	})
	t.Run("StartDesiredState", func(t *testing.T) {
		c, _ := unitCell(t)
		prepareSupervisorTest(t, c)
		if c.SetServiceDesiredState(t.Context(), "stopped") != nil {
			t.Fatal("cannot stop service intent")
		}
		updateTestFence(t, c)
		if c.Start(t.Context()) != ErrServiceDesiredState {
			t.Fatal("maintenance displaced service intent refusal")
		}
	})
	t.Run("StartMissingCredential", func(t *testing.T) {
		c, _ := unitCell(t)
		control := c.Start(t.Context())
		if control == nil || control.Error() != "local cell password file is unavailable or not private" {
			t.Fatal("missing credential control unexpectedly started")
		}
		updateTestFence(t, c)
		if c.Start(t.Context()) != errMaintenanceFenceActive {
			t.Fatal("later credential refusal displaced maintenance")
		}
	})
	t.Run("RemoveUnrecordedPGDATA", func(t *testing.T) {
		f := newAdmissionFixture(t, "RemoveRetain")
		if os.Remove(f.cell.path("manifest.json")) != nil {
			t.Fatal("cannot remove manifest")
		}
		if err := f.run(t.Context()); err == nil || err.Error() != "refusing removal of unrecorded PostgreSQL data" {
			t.Fatal("unrecorded data control missed refusal")
		}
		updateTestFence(t, f.cell)
		if f.run(t.Context()) != errMaintenanceFenceActive {
			t.Fatal("later unrecorded data refusal displaced maintenance")
		}
	})
	t.Run("RemoveServiceMalformedManifest", func(t *testing.T) {
		c, _ := unitCell(t)
		prepareSupervisorTest(t, c)
		if testWriteFile(c.path("manifest.json"), []byte("invalid"), 0o600) != nil {
			t.Fatal("cannot corrupt service manifest")
		}
		control := c.Remove(t.Context(), RemoveOptions{})
		if control == nil || control.Error() != hostExitRefused("envelope").Error() {
			t.Fatal("service predrain control unexpectedly succeeded")
		}
		updateTestFence(t, c)
		before := admissionSnapshot(t, c)
		err := c.Remove(t.Context(), RemoveOptions{})
		if err == nil || err.Error() != control.Error() || err == errMaintenanceFenceActive {
			t.Fatal("maintenance displaced earlier service predrain refusal")
		}
		if !reflect.DeepEqual(before, admissionSnapshot(t, c)) {
			t.Fatal("failed predrain mutated fixture")
		}
	})
}

func TestMaintenanceFenceAdmissionLockLifetime(t *testing.T) {
	for _, name := range []string{"Install", "StartLegacy", "StartSelected", "StartPortReallocation", "Stop", "RepairSelectedResume", "RemoveRetain", "RemovePurge"} {
		t.Run(name, func(t *testing.T) {
			fixtureName := name
			if name == "StartPortReallocation" || name == "Stop" {
				fixtureName = "StartLegacy"
			}
			f := newAdmissionFixture(t, fixtureName)
			if name == "Stop" {
				f.run = f.cell.Stop
			}
			other, err := New(f.cell.cfg)
			if err != nil {
				t.Fatal("cannot construct independent contender")
			}
			var probes []string
			probe := func(site string) {
				probes = append(probes, site)
				lock, err := other.ownExisting()
				if lock != nil {
					_ = lock.Close()
				}
				if !errors.Is(err, ErrLocked) {
					t.Fatal("guarded callback did not retain operation lock")
				}
			}
			if name == "StartPortReallocation" {
				thief, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal("cannot occupy saved startup port")
				}
				defer func() { _ = thief.Close() }()
				m, err := readManifest(f.cell.cfg.Root)
				if err != nil {
					t.Fatal("cannot read reallocation manifest")
				}
				m.Ports.ServerAPI = thief.Addr().(*net.TCPAddr).Port
				if writeAtomic(f.cell.path("manifest.json"), m, nil) != nil {
					t.Fatal("cannot publish occupied startup port")
				}
				f.cell.afterPortAllocation = func(Ports) { probe("reallocate") }
			}
			originalFailpoint := f.cell.cfg.Failpoint
			f.cell.cfg.Failpoint = func(operation, step string) error {
				probe(operation + ":" + step)
				return originalFailpoint(operation, step)
			}
			if name == "Install" {
				f.cell.afterPortAllocation = func(Ports) { *f.trace = append(*f.trace, "allocate"); probe("allocate") }
			}
			if strings.HasPrefix(name, "Start") {
				admissionRecordStart(f.cell, f.trace, probe)
			}
			if name == "RepairSelectedResume" {
				original := f.cell.installerPrincipals.resumePostgres
				f.cell.installerPrincipals.resumePostgres = func(ctx context.Context, m *Manifest) error { probe("resume"); return original(ctx, m) }
			}
			assertAdmissionControl(t, f, fixtureName)
			if len(probes) == 0 {
				t.Fatal("lock fixture missed callback")
			}
			if strings.HasPrefix(name, "Remove") && !slices.Contains(probes, "remove:cleanup_run") {
				t.Fatal("lock fixture missed run unlink")
			}
			if strings.HasPrefix(name, "Start") && (!slices.Contains(probes, "server_ready") || !slices.Contains(probes, "worker_ready")) {
				t.Fatal("lock fixture missed later readiness")
			}
			if name == "StartPortReallocation" && !slices.Contains(probes, "reallocate") {
				t.Fatal("lock fixture missed occupied-port reallocation")
			}
			lock, err := other.ownExisting()
			if err != nil || lock == nil {
				t.Fatal("operation lock was not released after return")
			}
			_ = lock.Close()
		})
	}
}

func TestMaintenanceFenceRemovePredrainParity(t *testing.T) {
	t.Run("PredrainOnly", func(t *testing.T) {
		f := newAdmissionFixture(t, "RemoveRetain")
		d := prepareSupervisorTest(t, f.cell)
		updateTestFence(t, f.cell)
		before := admissionSnapshot(t, f.cell)
		if f.cell.exitServiceHostForRemoval(t.Context()) != nil {
			t.Fatal("no-host predrain refused eligible fixture")
		}
		after, err := f.cell.ServiceDescriptor()
		if err != nil || after.DesiredState != "stopped" || after.Revision != d.Revision+1 {
			t.Fatal("predrain did not publish exactly one stopped intent")
		}
		delete(before, "service/descriptor.json")
		current := admissionSnapshot(t, f.cell)
		delete(current, "service/descriptor.json")
		if !reflect.DeepEqual(before, current) || len(*f.trace) != 0 {
			t.Fatal("no-host predrain changed final cleanup targets")
		}
	})
	for _, fenced := range []bool{false, true} {
		name := "Absent"
		if fenced {
			name = "Fenced"
		}
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t, "RemoveRetain")
			d := prepareSupervisorTest(t, f.cell)
			if fenced {
				updateTestFence(t, f.cell)
			}
			before := admissionSnapshot(t, f.cell)
			err := f.run(t.Context())
			after, readErr := f.cell.ServiceDescriptor()
			if readErr != nil || after.DesiredState != "stopped" {
				t.Fatal("Remove did not preserve no-host stopped predrain")
			}
			wantRevision := d.Revision + 2
			if fenced {
				wantRevision = d.Revision + 1
			}
			if after.Revision != wantRevision {
				t.Fatal("Remove predrain or final-lock desired-state revision changed")
			}
			if fenced {
				if err != errMaintenanceFenceActive || len(*f.trace) != 0 {
					t.Fatal("fenced Remove crossed final mutation boundary")
				}
				delete(before, "service/descriptor.json")
				current := admissionSnapshot(t, f.cell)
				delete(current, "service/descriptor.json")
				if !reflect.DeepEqual(before, current) {
					t.Fatal("fenced Remove changed final removal targets after predrain")
				}
			} else if err != nil || !slices.Contains(*f.trace, "remove:cleanup_run") {
				t.Fatal("absent Remove did not finish after predrain")
			}
		})
	}
}

// Expected journal operation/step/state sequences are explicit legacy fixtures.
// Only the timestamp field of each observed line is substituted; all punctuation,
// field order, line count, and omitted fields remain byte-exact comparisons.
func admissionAssertJournalBytes(t *testing.T, c *Cell, operation string, steps []string) {
	t.Helper()
	var pairs [][2]string
	pairs = append(pairs, [2]string{"operation", "started"})
	for _, step := range steps {
		pairs = append(pairs, [2]string{step, "started"}, [2]string{step, "done"})
	}
	pairs = append(pairs, [2]string{"operation", "done"})
	data, err := os.ReadFile(c.path("journal", "operations.jsonl"))
	if err != nil {
		t.Fatal("cannot read selected parity journal")
	}
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) != len(pairs)+1 || len(lines[len(lines)-1]) != 0 {
		t.Fatal("selected parity journal changed line count or termination")
	}
	for index, pair := range pairs {
		var timestamp struct {
			At string `json:"at"`
		}
		if json.Unmarshal(lines[index], &timestamp) != nil {
			t.Fatal("selected parity journal is invalid")
		}
		if _, err := time.Parse(time.RFC3339Nano, timestamp.At); err != nil {
			t.Fatal("selected parity journal timestamp is invalid")
		}
		want := fmt.Sprintf(`{"operation":%q,"step":%q,"state":%q,"at":%q}`, operation, pair[0], pair[1], timestamp.At)
		if string(lines[index]) != want {
			t.Fatal("selected parity journal bytes changed outside timestamp field")
		}
	}
}

func TestMaintenanceFenceAbsentSelectedParity(t *testing.T) {
	for _, name := range []string{"StartSelected", "RepairSelectedResume"} {
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t, name)
			m, err := readManifest(f.cell.cfg.Root)
			if err != nil {
				t.Fatal("cannot read selected parity input")
			}
			m.CreatedAt = time.Date(2026, 1, 2, 3, 4, 5, 6000000, time.UTC)
			m.MigrationWait = time.Second
			m.JobRestartPolicy = "never"
			if writeAtomic(f.cell.path("manifest.json"), m, nil) != nil {
				t.Fatal("cannot fix selected parity inputs")
			}
			before, err := os.ReadFile(f.cell.path("manifest.json"))
			if err != nil {
				t.Fatal("cannot snapshot selected parity manifest")
			}
			assertAdmissionControl(t, f, name)
			operation := "start"
			steps := []string{"postgres", "server", "server_ready", "account", "worker", "worker_ready"}
			wantTrace := []string{"postgres", "start:postgres", "verify", "server", "start:server", "server_ready", "start:server_ready", "account", "start:account", "worker", "start:worker", "worker_ready", "start:worker_ready"}
			lastOperation := "start"
			if name == "RepairSelectedResume" {
				operation = "install"
				lastOperation = "repair"
				steps = []string{"manifest", "password", "initdb", "configure", "postgres", "database", "principal_credentials", "principals", "principal_migrations", "principal_grants", "principal_verified", "revert_stop"}
				wantTrace = []string{"resume", "credentials", "install:principal_credentials", "provision", "install:principals", "migrate", "install:principal_migrations", "grants", "install:principal_grants", "verify", "install:principal_verified", "verify", "install:revert_stop"}
			}
			if !slices.Equal(*f.trace, wantTrace) {
				t.Fatal("selected absent lifecycle changed ordered effect trace")
			}
			admissionAssertJournalBytes(t, f.cell, operation, steps)
			want := bytes.Replace(before, []byte(`"last_operation": ""`), []byte(`"last_operation": "`+lastOperation+`"`), 1)
			if name == "RepairSelectedResume" {
				want = bytes.Replace(want, []byte(`"installed": false`), []byte(`"installed": true`), 1)
			}
			after, err := os.ReadFile(f.cell.path("manifest.json"))
			if err != nil || !bytes.Equal(after, want) {
				t.Fatal("selected absent lifecycle changed expected manifest bytes")
			}
		})
	}
}

func TestMaintenanceFenceCauseIsValueFree(t *testing.T) {
	for _, name := range admissionNames {
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t, name)
			private := `{"generation":1,"operation_id":"private-sentinel@example.invalid","since":"private-invalid-time"}`
			if testWriteFile(f.cell.path("journal", "maintenance.json"), []byte(private), 0o600) != nil {
				t.Fatal("cannot prepare private-looking fence")
			}
			before := admissionSnapshot(t, f.cell)
			err := f.run(t.Context())
			if err != errMaintenanceFenceActive || err.Error() != "local_maintenance_fence_active" {
				t.Fatal("boundary diagnostic was not exact value-free cause")
			}
			if !reflect.DeepEqual(before, admissionSnapshot(t, f.cell)) {
				t.Fatal("private-looking refusal wrote diagnostic or changed evidence")
			}
		})
	}
}

func TestMaintenanceFenceAbsentServiceDelegation(t *testing.T) {
	for _, action := range []string{"Initial", "status", "health", "start", "Retry", "PostShutdownRepair"} {
		t.Run(action, func(t *testing.T) {
			if action == "PostShutdownRepair" {
				admissionRunShutdownParity(t)
				return
			}
			f := newAdmissionFixture(t, "StartLegacy")
			d := prepareSupervisorTest(t, f.cell)
			hooks, now, _, _, _ := supervisorTestHooks()
			starts, stops, repairs := 0, 0, 0
			hooks.start = func(ctx context.Context) error { starts++; return f.cell.Start(ctx) }
			hooks.stop = func(ctx context.Context) error { stops++; return f.cell.stopServiceChildren(ctx) }
			hooks.repair = func(ctx context.Context) error { repairs++; return f.cell.Repair(ctx) }
			s := newSupervisor(f.cell, SupervisorPolicy{hooks: &hooks})
			if s.load(d) != nil {
				t.Fatal("cannot load recording supervisor")
			}
			switch action {
			case "Initial":
				if s.step(t.Context()) != nil {
					t.Fatal("initial supervisor delegation failed")
				}
			case "Retry":
				healthy := false
				s.hooks.health = func(context.Context) error {
					if healthy {
						return nil
					}
					return errors.New("fixture health refused")
				}
				if s.step(t.Context()) != nil || starts != 1 || stops != 1 || s.record.NextStart.IsZero() {
					t.Fatal("failed health did not retain retry policy")
				}
				if s.step(t.Context()) != nil || starts != 1 {
					t.Fatal("backoff repeated start")
				}
				*now = s.record.NextStart
				healthy = true
				if s.step(t.Context()) != nil || starts != 2 || !s.status.Healthy {
					t.Fatal("retry did not delegate real lifecycle start")
				}
			default:
				if _, err := s.command(t.Context(), action); err != nil {
					t.Fatal("supervisor command delegation failed")
				}
			}
			wantStarts := 1
			if action == "Retry" {
				wantStarts = 2
			}
			if starts != wantStarts || !s.status.Healthy || !slices.Contains(*f.trace, "postgres") || !slices.Contains(*f.trace, "worker_ready") {
				t.Fatal("supervisor stopped invoking full lifecycle startup")
			}
		})
	}
}

func admissionRunShutdownParity(t *testing.T) {
	t.Helper()
	c, m := unitCell(t)
	if runtime.GOOS != "windows" {
		// Match the existing embedded Run fixtures: native Unix socket names
		// must fit their platform limit, independent of the full subtest name.
		root, err := os.MkdirTemp("/tmp", "lc-admit-")
		if err != nil {
			t.Fatal("cannot prepare short supervisor root")
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		c, err = New(Config{Root: root, Timeout: time.Second})
		if err != nil || c.layout() != nil || writeAtomic(c.path("manifest.json"), m, nil) != nil {
			t.Fatal("cannot prepare short supervisor fixture")
		}
	}
	if selectedFixtureDataDirectory(c) != nil || c.makePassword() != nil {
		t.Fatal("cannot prepare supervisor startup fixture")
	}
	prepareSupervisorTest(t, c)
	var effects, calls []string
	admissionRecordStart(c, &effects, nil)
	c.cfg.Failpoint = func(operation, step string) error { effects = append(effects, operation+":"+step); return nil }
	hooks, _, _, _, _ := supervisorTestHooks()
	if runtime.GOOS != "windows" {
		hooks.boot = func() (string, error) { return "synthetic-boot", nil }
	}
	// Windows publishes an endpoint bound to the native boot identifier.
	hooks.ticks = make(chan time.Time)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	starts, repairs, stops := 0, 0, 0
	hooks.reconcile = func() error { calls = append(calls, "reconcile"); return nil }
	hooks.start = func(opCtx context.Context) error { calls = append(calls, "start"); starts++; return c.Start(opCtx) }
	hooks.repair = func(opCtx context.Context) error { calls = append(calls, "repair"); repairs++; return c.Repair(opCtx) }
	hooks.stop = func(opCtx context.Context) error {
		calls = append(calls, "stop")
		stops++
		return c.stopServiceChildren(opCtx)
	}
	hooks.health = func(opCtx context.Context) error {
		calls = append(calls, "health")
		// Model the unfinished operation that Run's deferred Repair must settle.
		// Cancellation occurs in this existing callback, with no timer race.
		lock, err := c.ownExisting()
		if err != nil {
			return err
		}
		err = c.begin("start")
		_ = lock.Close()
		if err != nil {
			return err
		}
		cancel()
		return opCtx.Err()
	}
	if err := c.Run(ctx, SupervisorPolicy{hooks: &hooks}); err != nil {
		t.Fatal("embedded supervisor did not complete canceled shutdown")
	}
	if starts != 1 || repairs != 2 || stops != 2 || !slices.Equal(calls, []string{"reconcile", "repair", "stop", "start", "health", "repair", "stop"}) {
		t.Fatal("Run changed initial or deferred lifecycle order and counts")
	}
	if !slices.Contains(effects, "worker_ready") || !slices.Contains(effects, "start:revert_stop") {
		t.Fatal("Run did not reach actual startup and interrupted-journal repair")
	}
	progress, err := c.readJournal()
	if err != nil || progress.pending != nil || progress.tornTail {
		t.Fatal("Run shutdown did not settle the interrupted lifecycle journal")
	}
	d, err := c.ServiceDescriptor()
	if err != nil || d.DesiredState != "running" {
		t.Fatal("host cancellation changed durable running intent")
	}
	if held, err := c.serviceHostLeaseHeld(); err != nil || held {
		t.Fatal("Run did not release the host lease after shutdown")
	}
}

// This negative control deliberately writes its fixture directly. It remains
// runnable when a mutant incorrectly refuses every absent-fence admission.
func TestMaintenanceFenceStopAndInspectionRemainUsableWithoutAdmissionSetup(t *testing.T) {
	c, _ := unitCell(t)
	inspection, err := c.Inspect(t.Context())
	if err != nil {
		t.Fatal("unfenced inspection control failed")
	}
	status, err := c.Status(t.Context())
	if err != nil {
		t.Fatal("unfenced status control failed")
	}
	var stops []string
	c.cfg.Failpoint = func(operation, step string) error { stops = append(stops, operation+":"+step); return nil }
	updateTestFence(t, c)
	before := admissionSnapshot(t, c)
	observedInspection, inspectionErr := c.Inspect(t.Context())
	observedStatus, statusErr := c.Status(t.Context())
	if inspectionErr != nil || statusErr != nil || !reflect.DeepEqual(inspection, observedInspection) || !reflect.DeepEqual(status, observedStatus) {
		t.Fatal("maintenance changed stopped inspection or status semantics")
	}
	if !reflect.DeepEqual(before, admissionSnapshot(t, c)) {
		t.Fatal("inspection or status mutated the fenced fixture")
	}
	if err := c.Stop(t.Context()); err != nil || !slices.Equal(stops, []string{"stop:stop"}) {
		t.Fatal("maintenance blocked safety stop or changed its trace")
	}
	after := admissionSnapshot(t, c)
	if before["journal/maintenance.json"] != after["journal/maintenance.json"] {
		t.Fatal("safety stop changed maintenance fence bytes")
	}
}
