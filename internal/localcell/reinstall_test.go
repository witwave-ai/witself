package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/store"
)

// reinstallContentionBound is an absolute ceiling for a refusal that never
// waits: every contender's lock attempt is non-blocking.
const reinstallContentionBound = 10 * time.Second

// reinstallRecorder replaces the postgres, query, stop and selected verify
// boundaries. Each call records the on-disk retained marker it observed.
type reinstallRecorder struct {
	cell            *Cell
	calls           []string
	fail            map[string]error
	account, schema string
	inside          func()
	preRemoval      []byte
}

func (r *reinstallRecorder) record(name string) error {
	state := ":unreadable"
	if m, err := readManifest(r.cell.cfg.Root); err == nil {
		state = map[bool]string{true: ":marked", false: ":clear"}[m.RemovalRetained]
	}
	r.calls = append(r.calls, name+state)
	return r.fail[name]
}

func (r *reinstallRecorder) postgres(context.Context, *Manifest) error {
	if r.inside != nil {
		r.inside()
	}
	return r.record("postgres")
}

func (r *reinstallRecorder) query(_ context.Context, _ *Manifest, query string) (string, error) {
	if err := r.record("query"); err != nil {
		return "", err
	}
	if strings.Contains(query, "FROM accounts") {
		return r.account, nil
	}
	return r.schema, nil
}

func (r *reinstallRecorder) stop(context.Context) error              { return r.record("stop") }
func (r *reinstallRecorder) verify(context.Context, *Manifest) error { return r.record("verify") }

// reinstallFixture is a synthetic byte-preservation fixture with a verified
// account and this build's schema. A real ordinary Remove publishes the marker.
// No process or database runs: the recorder replaces every external boundary.
func reinstallFixture(t *testing.T, layout string, service bool) (*retainedFixture, *reinstallRecorder) {
	t.Helper()
	f := retainedRemovalFixture(t, layout, service)
	c, m := f.cell, f.manifest
	m.AccountID, m.LastVerifiedSchemaVersion = "acc_aaaaaaaaaaaaaaaa", store.SchemaVersion()
	// A dynamically allocated saved port keeps byte-equality rows independent of
	// any fixed host port, including one that another lane's T5 holds.
	ports, err := c.allocateAvailablePorts(m.Ports, "postgres")
	if err != nil {
		t.Fatal("cannot allocate the fixture's saved PostgreSQL port")
	}
	m.Ports = ports
	// The managed configuration is rewritten byte-identically while its port is free.
	if writeAtomic(c.path("manifest.json"), &m, nil) != nil || c.configurePostgres(&m) != nil {
		t.Fatal("cannot prepare verified reinstall fixture")
	}
	r := &reinstallRecorder{cell: c, fail: map[string]error{}, account: m.AccountID,
		schema: strconv.Itoa(m.LastVerifiedSchemaVersion), preRemoval: retainedRead(t, c, "manifest.json")}
	if c.Remove(t.Context(), RemoveOptions{}) != nil {
		t.Fatal("cannot retain reinstall fixture")
	}
	c.cfg.Binaries.PostgresDir = t.TempDir() // Absent executables: no native PostgreSQL can run.
	c.reinstallTest = &reinstallOperations{postgres: r.postgres, query: r.query, stop: r.stop}
	ops := c.principalOperations()
	ops.verify = r.verify
	c.installerPrincipals = &ops
	f.manifest, f.prefix, f.material = m, retainedRead(t, c, "journal/operations.jsonl"), retainedSnapshot(t, c)
	return f, r
}

func reinstallOptions(f *retainedFixture) ReinstallOptions {
	return ReinstallOptions{InstallationID: f.manifest.InstallationID}
}

// reinstallEntries parses every record appended after the fixture's prefix.
func reinstallEntries(t *testing.T, f *retainedFixture) []journalEntry {
	t.Helper()
	data := retainedRead(t, f.cell, "journal/operations.jsonl")
	if !bytes.HasPrefix(data, f.prefix) {
		t.Fatal("prior journal prefix lost")
	}
	var entries []journalEntry
	for _, line := range bytes.SplitAfter(data[len(f.prefix):], []byte("\n")) {
		var entry journalEntry
		if len(line) == 0 {
			continue
		}
		if json.Unmarshal(line, &entry) != nil {
			t.Fatal("appended journal record is unreadable")
		}
		entries = append(entries, entry)
	}
	return entries
}

// reinstallChanged names every entry added, removed or changed between snapshots.
func reinstallChanged(before, after map[string]retainedEntry) []string {
	var changed []string
	for _, side := range []map[string]retainedEntry{before, after} {
		for name := range side {
			if before[name] != after[name] && !slices.Contains(changed, name) {
				changed = append(changed, name)
			}
		}
	}
	return changed
}

// reinstallAssertRemoved proves a settled cell is still removed, with no open
// operation and with custody and PGDATA bytes identical to the fixture.
func reinstallAssertRemoved(t *testing.T, f *retainedFixture) {
	t.Helper()
	p, err := f.cell.readJournal()
	m, manifestErr := readManifest(f.cell.cfg.Root)
	if err != nil || manifestErr != nil || p.pending != nil || p.tornTail || !m.RemovalRetained {
		t.Fatal("reinstall left an open operation or cleared the retained marker")
	}
	for _, name := range reinstallChanged(f.material, retainedSnapshot(t, f.cell)) {
		if strings.HasPrefix(name, "run/") || strings.HasPrefix(name, "pg/") {
			t.Fatal("reinstall changed retained custody or PGDATA bytes")
		}
	}
}

// reinstallSameError requires the identical fixed cause, or else the exact text.
func reinstallSameError(err, want error) bool {
	var cause fixedCause
	if errors.As(want, &cause) {
		return errors.Is(err, want)
	}
	return err != nil && err.Error() == want.Error()
}

func TestReinstallAdmissionRefusals(t *testing.T) {
	own, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		t.Fatal("cannot observe the test process identity")
	}
	type setup func(*testing.T, *Cell, *ReinstallOptions)
	manifest := func(edit func(*Manifest)) setup {
		return func(t *testing.T, c *Cell, _ *ReinstallOptions) {
			m, err := readManifest(c.cfg.Root)
			if err != nil {
				t.Fatal("cannot read reinstall fixture manifest")
			}
			edit(m)
			if writeAtomic(c.path("manifest.json"), m, nil) != nil {
				t.Fatal("cannot edit reinstall fixture manifest")
			}
		}
	}
	write := func(name, value string) setup {
		return func(t *testing.T, c *Cell, _ *ReinstallOptions) {
			if writeAtomicBytes(c.path(filepath.FromSlash(name)), []byte(value), nil) != nil {
				t.Fatal("cannot write reinstall fixture material")
			}
		}
	}
	remove := func(name string) setup {
		return func(t *testing.T, c *Cell, _ *ReinstallOptions) {
			if os.RemoveAll(c.path(filepath.FromSlash(name))) != nil {
				t.Fatal("cannot remove reinstall fixture material")
			}
		}
	}
	record := func(component, start string) setup {
		return func(t *testing.T, c *Cell, _ *ReinstallOptions) {
			if writeAtomic(c.path("run", component+".json"), ProcessRecord{PID: os.Getpid(), StartTime: start, Root: c.cfg.Root, Component: component}, nil) != nil {
				t.Fatal("cannot prepare process record")
			}
		}
	}
	// A live postmaster record passes verifyAll only with this PGDATA's pidfile.
	livePostgres := func(t *testing.T, c *Cell, o *ReinstallOptions) {
		record("postgres", own)(t, c, o)
		m, err := readManifest(c.cfg.Root)
		if err != nil {
			t.Fatal("cannot read reinstall fixture manifest")
		}
		write("pg/postmaster.pid", fmt.Sprintf("%d\n%s\n1\n%d\n\n127.0.0.1\n1\nready\n", os.Getpid(), c.path("pg"), m.Ports.Postgres))(t, c, o)
	}
	both, selected := []string{"legacy", "selected"}, []string{"selected"}
	for _, row := range []struct {
		name    string
		layouts []string
		setup   setup
		want    error
	}{
		{"empty-id", both, func(_ *testing.T, _ *Cell, o *ReinstallOptions) { o.InstallationID = "" }, errReinstallIdentity},
		{"wrong-id", both, func(_ *testing.T, _ *Cell, o *ReinstallOptions) { o.InstallationID += "-other" }, errReinstallIdentity},
		// Consent is exact: neither a case variant nor a strict prefix names it.
		{"id-case", both, func(_ *testing.T, _ *Cell, o *ReinstallOptions) { o.InstallationID = strings.ToUpper(o.InstallationID) }, errReinstallIdentity},
		{"id-prefix", both, func(_ *testing.T, _ *Cell, o *ReinstallOptions) {
			o.InstallationID = o.InstallationID[:len(o.InstallationID)-1]
		}, errReinstallIdentity},
		{"manifest-absent", both, remove("manifest.json"), errReinstallState},
		{"healthy-installed", both, manifest(func(m *Manifest) { m.RemovalRetained = false }), errReinstallState},
		{"interrupted-purge", both, manifest(func(m *Manifest) { m.Installed = false }), errReinstallState},
		{"cleanup-pending", both, manifest(func(m *Manifest) {
			m.Cleanup = &journalEntry{Operation: "remove", Step: "runtime_cleanup", State: "started", At: time.Now().UTC()}
		}), errReinstallState},
		{"account-empty", both, manifest(func(m *Manifest) { m.AccountID = "" }), errReinstallState},
		{"schema-zero", both, manifest(func(m *Manifest) { m.LastVerifiedSchemaVersion = 0 }), errReinstallState},
		{"schema-differs", both, manifest(func(m *Manifest) { m.LastVerifiedSchemaVersion = store.SchemaVersion() + 1 }), errReinstallVersion},
		// An older build's data stays retained: the next legacy Start would migrate it with no recovery point.
		{"schema-older", both, manifest(func(m *Manifest) { m.LastVerifiedSchemaVersion = store.SchemaVersion() - 1 }), errReinstallVersion},
		{"pending-operation", both, func(t *testing.T, c *Cell, _ *ReinstallOptions) {
			if c.begin("stop") != nil {
				t.Fatal("cannot prepare pending operation")
			}
		}, errReinstallState},
		{"torn-tail", both, func(t *testing.T, c *Cell, o *ReinstallOptions) {
			write("journal/operations.jsonl", string(retainedRead(t, c, "journal/operations.jsonl"))+`{"operation":`)(t, c, o)
		}, errReinstallState},
		{"valid-fence", both, func(t *testing.T, c *Cell, _ *ReinstallOptions) { updateTestFence(t, c) }, errMaintenanceFenceActive},
		{"invalid-fence", both, func(t *testing.T, c *Cell, o *ReinstallOptions) {
			updateTestFence(t, c)
			write("journal/maintenance.json", "{broken\n")(t, c, o)
		}, errMaintenanceFenceActive},
		{"live-matching-record", both, record("server", own), errReinstallState},
		{"live-postgres-record", both, livePostgres, errReinstallState},
		{"live-worker-record", both, record("worker", own), errReinstallState},
		{"foreign-record", both, record("server", "foreign-incarnation"), errors.New("local cell process identity mismatch; refusing to signal")},
		{"pg-absent", both, remove("pg"), errReinstallState},
		{"pg-version-15", both, write("pg/PG_VERSION", "15\n"), errReinstallVersion},
		// PG_VERSION is read only when Lstat shows a regular file of at most 64 bytes.
		{"pg-version-absent", both, remove("pg/PG_VERSION"), errReinstallState},
		{"pg-version-oversize", both, write("pg/PG_VERSION", "16\n"+strings.Repeat("#", 62)), errReinstallState},
		{"bootstrap-missing", both, remove("run/postgres.password"), errors.New("local cell password file is unavailable or not private")},
		{"selected-partial-custody", selected, remove("run/credentials/" + string(store.SyncPrincipalWorker)), ErrLocalPrincipalCredentialsRefused},
		{"selected-foreign-credential", selected, write("run/credentials/foreign", "synthetic foreign credential\n"), ErrLocalPrincipalCredentialsRefused},
		{"relative-postgres-dir", both, func(_ *testing.T, c *Cell, _ *ReinstallOptions) { c.cfg.Binaries.PostgresDir = "relative-postgres" },
			errors.New("explicit PostgreSQL binary directory is required")},
	} {
		for _, layout := range row.layouts {
			t.Run(row.name+"/"+layout, func(t *testing.T) {
				f, r := reinstallFixture(t, layout, false)
				options := reinstallOptions(f)
				row.setup(t, f.cell, &options)
				// The synthetic cluster reports the schema its manifest records, so a
				// root that slips past a weakened offline check is not stopped by the proof.
				if m, err := readManifest(f.cell.cfg.Root); err == nil {
					r.schema = strconv.Itoa(m.LastVerifiedSchemaVersion)
				}
				before := retainedSnapshot(t, f.cell)
				err := f.cell.Reinstall(t.Context(), options)
				after := retainedSnapshot(t, f.cell)
				if !reflect.DeepEqual(before, after) || len(r.calls) != 0 || strings.Contains(after["journal/operations.jsonl"].data, `"reinstall"`) {
					t.Fatal("refused reinstall wrote durable bytes or reached a boundary")
				}
				if !reinstallSameError(err, row.want) {
					t.Fatal("reinstall admission returned a different refusal")
				}
			})
		}
	}
}

// A purge removes run/ and keeps .owner.lock. Like Start's, Reinstall's pinned
// lock prefix then creates run/ and run/operation.lock before admission
// refuses; nothing else may change (review LC-S7D2D-XR1-8).
func TestReinstallPurgedRootRefusal(t *testing.T) {
	f, r := reinstallFixture(t, "legacy", false)
	c := f.cell
	if c.Remove(t.Context(), RemoveOptions{Purge: true}) != nil {
		t.Fatal("cannot purge the retained fixture")
	}
	before := retainedSnapshot(t, c)
	err := c.Reinstall(t.Context(), reinstallOptions(f))
	added := reinstallChanged(before, retainedSnapshot(t, c))
	sort.Strings(added)
	if _, kept := before["run"]; kept || !slices.Equal(added, []string{"run", "run/operation.lock"}) || len(r.calls) != 0 {
		t.Fatal("refused reinstall of a purged root changed more than its lock prefix's run/ entries")
	}
	if !errors.Is(err, errReinstallState) {
		t.Fatal("purged root was not refused as unsupported state")
	}
}

// A described cell keeps the service start fence: maintenance, Faulted, a
// pending transition and an unreadable or foreign descriptor refuse before any
// boundary, while stopped (which every Remove publishes) and running are
// admitted (review LC-S7D2D-XR1-4).
func TestReinstallServiceFence(t *testing.T) {
	desired := func(state string) func(*testing.T, *Cell) {
		return func(t *testing.T, c *Cell) {
			if c.SetServiceDesiredState(t.Context(), state) != nil {
				t.Fatal("cannot record service intent")
			}
		}
	}
	for _, row := range []struct {
		name  string
		arm   func(*testing.T, *Cell)
		admit bool
	}{
		{"stopped", func(*testing.T, *Cell) {}, true},
		{"running", desired("running"), true},
		{"maintenance", desired("maintenance"), false},
		{"faulted", func(t *testing.T, c *Cell) {
			if c.setServiceFaulted(t.Context(), true) != nil {
				t.Fatal("cannot fault the service")
			}
		}, false},
		{"transition-pending", func(t *testing.T, c *Cell) {
			d, err := c.ServiceDescriptor()
			if err != nil || writeAtomic(c.path("service", "transition.json"), d, nil) != nil {
				t.Fatal("cannot leave a service transition pending")
			}
		}, false},
		{"descriptor-invalid", func(t *testing.T, c *Cell) {
			if writeAtomicBytes(c.path("service", "descriptor.json"), []byte("{broken\n"), nil) != nil {
				t.Fatal("cannot corrupt the service descriptor")
			}
		}, false},
		{"descriptor-foreign", func(t *testing.T, c *Cell) {
			d, err := c.ServiceDescriptor()
			d.InstallationID += "-other"
			if err != nil || writeAtomic(c.path("service", "descriptor.json"), d, nil) != nil {
				t.Fatal("cannot publish a foreign service descriptor")
			}
		}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, r := reinstallFixture(t, "legacy", true)
			c := f.cell
			row.arm(t, c)
			before := retainedSnapshot(t, c)
			err := c.Reinstall(t.Context(), reinstallOptions(f))
			if row.admit {
				if err != nil || len(r.calls) == 0 {
					t.Fatal("service intent that permits launches refused reinstall")
				}
				return
			}
			after := retainedSnapshot(t, c)
			if !reflect.DeepEqual(before, after) || len(r.calls) != 0 || strings.Contains(after["journal/operations.jsonl"].data, `"reinstall"`) {
				t.Fatal("service-fenced reinstall wrote durable bytes or reached a boundary")
			}
			if !errors.Is(err, errReinstallState) {
				t.Fatal("service-fenced reinstall returned a different refusal")
			}
		})
	}
}

var reinstallRecords = []string{"reinstall/operation/started", "reinstall/postgres/started", "reinstall/postgres/done",
	"reinstall/verified/started", "reinstall/verified/done", "reinstall/stop/started", "reinstall/stop/done",
	"reinstall/readmitted/started", "reinstall/readmitted/done", "reinstall/operation/done"}

func TestReinstallSequence(t *testing.T) {
	for _, layout := range []string{"legacy", "selected"} {
		t.Run(layout, func(t *testing.T) {
			f, r := reinstallFixture(t, layout, false)
			c := f.cell
			removed, err := readManifest(c.cfg.Root)
			if err != nil {
				t.Fatal("cannot read retained manifest")
			}
			if err := c.Reinstall(t.Context(), reinstallOptions(f)); err != nil {
				t.Fatal(err)
			}
			want := []string{"postgres:marked", "query:marked", "query:marked", "stop:marked"}
			if layout == "selected" {
				want = []string{"postgres:marked", "verify:marked", "stop:marked"}
			}
			if !slices.Equal(r.calls, want) {
				t.Fatal("boundary order or observed retained marker differs")
			}
			retainedAssertRecords(t, f, reinstallRecords)
			m, err := readManifest(c.cfg.Root)
			removed.RemovalRetained, removed.LastOperation = false, "reinstall"
			if err != nil || !reflect.DeepEqual(m, removed) || bytes.Contains(retainedRead(t, c, "manifest.json"), []byte("removal_retained")) {
				t.Fatal("readmission changed more than the marker and last operation")
			}
			for _, name := range reinstallChanged(f.material, retainedSnapshot(t, c)) {
				if name != "manifest.json" && name != "journal/operations.jsonl" && name != "pg/postgresql.conf" {
					t.Fatal("reinstall changed material outside its allowlist")
				}
			}
			if err := c.Start(t.Context()); err != nil || !slices.Contains(f.launches, "postgres") || !slices.Contains(f.launches, "server") || !slices.Contains(f.launches, "worker") {
				t.Fatal("readmitted cell did not start through the recorded boundaries")
			}
			if inspection, err := c.Inspect(t.Context()); err != nil || strings.Contains(inspection.InstallState, "removed") {
				t.Fatal("readmitted cell still reports removal")
			}
		})
	}
}

func TestReinstallVerificationFailure(t *testing.T) {
	exited := fixedCause("PostgreSQL command exited with failure")
	for _, row := range []struct {
		name, layout, step string
		arm                func(*reinstallRecorder)
		want               error
	}{
		{"legacy-foreign-account", "legacy", "verified", func(r *reinstallRecorder) { r.account = "acc_bbbbbbbbbbbbbbbb" }, errReinstallIdentity},
		{"legacy-online-schema", "legacy", "verified", func(r *reinstallRecorder) { r.schema = strconv.Itoa(store.SchemaVersion() + 1) }, errReinstallVersion},
		{"legacy-online-older-schema", "legacy", "verified", func(r *reinstallRecorder) { r.schema = strconv.Itoa(store.SchemaVersion() - 1) }, errReinstallVersion},
		{"legacy-query-error", "legacy", "verified", func(r *reinstallRecorder) { r.fail["query"] = errors.New("synthetic query failure") }, errReinstallIdentity},
		{"selected-verify-refused", "selected", "verified", func(r *reinstallRecorder) { r.fail["verify"] = ErrLocalPrincipalCredentialsRefused }, ErrLocalPrincipalCredentialsRefused},
		{"postgres-boundary-error", "legacy", "postgres", func(r *reinstallRecorder) { r.fail["postgres"] = exited }, exited},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, r := reinstallFixture(t, row.layout, false)
			c := f.cell
			row.arm(r)
			err := c.Reinstall(t.Context(), reinstallOptions(f))
			if !slices.ContainsFunc(reinstallEntries(t, f), func(e journalEntry) bool {
				return e.Step == row.step && e.State == "failed" && e.Cause == row.want.Error()
			}) {
				t.Fatal("failed step did not journal its fixed cause")
			}
			stops := 0
			for _, call := range r.calls {
				if strings.HasPrefix(call, "stop:") {
					stops++
				}
			}
			if !errors.Is(err, row.want) || stops != 1 || r.calls[len(r.calls)-1] != "stop:marked" {
				t.Fatal("failed proof was not stopped exactly once after the failure")
			}
			reinstallAssertRemoved(t, f)
			r.account, r.schema, r.fail = f.manifest.AccountID, strconv.Itoa(store.SchemaVersion()), map[string]error{}
			if c.Reinstall(t.Context(), reinstallOptions(f)) != nil {
				t.Fatal("healthy retry after a failed proof was refused")
			}
		})
	}
	// These failures leave the operation open; Repair settles each still removed.
	for _, row := range []string{"stop-failure", "readmitted-publish-failure", "rollback-incomplete"} {
		t.Run(row, func(t *testing.T) {
			f, r := reinstallFixture(t, "legacy", false)
			c := f.cell
			original := syncFile
			t.Cleanup(func() { syncFile = original })
			step, want := "stop", "local cell reinstall: stop failed"
			switch row {
			case "stop-failure":
				r.fail["stop"] = errors.New("synthetic stop failure")
			case "rollback-incomplete":
				step, want = "verified", "local cell reinstall failed; rollback incomplete, repair required"
				r.account, r.fail["stop"] = "acc_bbbbbbbbbbbbbbbb", errors.New("synthetic stop failure")
			default:
				step, want = "readmitted", "local cell reinstall: readmitted failed"
				armed := false
				c.cfg.Failpoint = func(operation, name string) error {
					armed = armed || operation == "reinstall" && name == "stop"
					return nil
				}
				// Fail only the readmitted manifest publication's temporary file.
				syncFile = func(file *os.File) error {
					if armed && filepath.Dir(file.Name()) == c.cfg.Root {
						armed = false
						return errors.New("synthetic manifest sync failure")
					}
					return original(file)
				}
			}
			err := c.Reinstall(t.Context(), reinstallOptions(f))
			syncFile, c.cfg.Failpoint = original, nil
			p, journalErr := c.readJournal()
			if err == nil || err.Error() != want || journalErr != nil || p.pending == nil || p.pending.Operation != "reinstall" || p.pending.Step != step {
				t.Fatal("failed reinstall did not leave its operation open for Repair")
			}
			r.account, r.fail = f.manifest.AccountID, map[string]error{}
			calls := len(r.calls)
			if c.Repair(t.Context()) != nil || len(r.calls) != calls {
				t.Fatal("Repair could not settle the open reinstall or reached a boundary")
			}
			reinstallAssertRemoved(t, f)
			if c.Reinstall(t.Context(), reinstallOptions(f)) != nil {
				t.Fatal("reinstall retry after Repair was refused")
			}
		})
	}
}

func TestReinstallCrashRepair(t *testing.T) {
	for _, layout := range []string{"legacy", "selected"} {
		for index, cut := range []string{"operation_started", "postgres", "verified", "stop", "readmitted", "operation_manifest"} {
			t.Run(fmt.Sprintf("%s/C%d_%s", layout, index, cut), func(t *testing.T) {
				f, r := reinstallFixture(t, layout, false)
				c := f.cell
				fired := false
				c.cfg.Failpoint = func(operation, step string) error {
					if operation == "reinstall" && step == cut {
						fired = true
						return errors.New("synthetic crash")
					}
					return nil
				}
				if err := c.Reinstall(t.Context(), reinstallOptions(f)); !fired || !errors.Is(err, errInterrupted) {
					t.Fatal("reinstall crash boundary was not exercised")
				}
				c.cfg.Failpoint = nil
				calls := len(r.calls)
				if c.Repair(t.Context()) != nil {
					t.Fatal("Repair could not settle the interrupted reinstall")
				}
				settled := retainedSnapshot(t, c)
				if c.Repair(t.Context()) != nil || !reflect.DeepEqual(settled, retainedSnapshot(t, c)) || len(r.calls) != calls {
					t.Fatal("second Repair changed settled bytes or Repair reached a boundary")
				}
				var reverts []string
				for _, entry := range reinstallEntries(t, f) {
					if entry.Step == "revert_stop" {
						reverts = append(reverts, entry.Operation+"/"+entry.State)
					}
				}
				p, err := c.readJournal()
				m, manifestErr := readManifest(c.cfg.Root)
				marked := index < 4 // The readmitted publication is durable from C4 on.
				if err != nil || manifestErr != nil || p.pending != nil || m.RemovalRetained != marked || !slices.Equal(reverts, []string{"reinstall/started", "reinstall/done"}) {
					t.Fatal("Repair left the operation open, repeated its stop or changed the marker")
				}
				for _, name := range reinstallChanged(f.material, settled) {
					if strings.HasPrefix(name, "run/") {
						t.Fatal("Repair changed retained custody")
					}
				}
				if marked {
					err = c.Reinstall(t.Context(), reinstallOptions(f))
				} else {
					err = c.Start(t.Context())
				}
				if err != nil {
					t.Fatal("settled reinstall did not admit its next operation")
				}
			})
		}
	}
}

func TestReinstallPortReallocation(t *testing.T) {
	f, _ := reinstallFixture(t, "legacy", false)
	c := f.cell
	listener, err := net.Listen("tcp4", addr(f.manifest.Ports.Postgres))
	if err != nil {
		t.Fatal("cannot hold the saved PostgreSQL port")
	}
	defer func() { _ = listener.Close() }()
	if err := c.Reinstall(t.Context(), reinstallOptions(f)); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(c.cfg.Root)
	config := string(retainedRead(t, c, "pg/postgresql.conf"))
	if err != nil || m.Ports.Postgres == f.manifest.Ports.Postgres || !strings.Contains(config, fmt.Sprintf("\nport = %d\n", m.Ports.Postgres)) {
		t.Fatal("reinstall did not move PostgreSQL to a reallocated port")
	}
	if !slices.ContainsFunc(reinstallEntries(t, f), func(e journalEntry) bool {
		return e.Operation == "reinstall" && e.Step == "postgres" && e.State == "started" && e.Cause == "port_reallocated=postgres"
	}) {
		t.Fatal("port reallocation was not journaled under reinstall")
	}
	if _, err := c.Inspect(t.Context()); err != nil || c.Repair(t.Context()) != nil {
		t.Fatal("journal with a reinstall reallocation became unreadable")
	}
}

func TestReinstallFenceAndContention(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		f, r := reinstallFixture(t, "legacy", false)
		updateTestFence(t, f.cell)
		if corrupt && writeAtomicBytes(f.cell.path("journal", "maintenance.json"), []byte("{broken\n"), nil) != nil {
			t.Fatal("cannot prepare corrupt fence")
		}
		before := retainedSnapshot(t, f.cell)
		err := f.cell.Reinstall(t.Context(), reinstallOptions(f))
		if !errors.Is(err, errMaintenanceFenceActive) || !reflect.DeepEqual(before, retainedSnapshot(t, f.cell)) || len(r.calls) != 0 {
			t.Fatal("fenced reinstall was admitted or wrote durable bytes")
		}
	}
	f, r := reinstallFixture(t, "legacy", true)
	c := f.cell
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	r.inside = func() { close(entered); <-release }
	result := StartJoinedTestRunForTest(t, unblock, reinstallContentionBound, func() error {
		return c.Reinstall(context.Background(), reinstallOptions(f))
	})
	select {
	case <-entered:
	case <-result:
		t.Fatal("reinstall returned before its postgres boundary")
	case <-time.After(reinstallContentionBound):
		t.Fatal("reinstall did not reach its postgres boundary")
	}
	held := retainedSnapshot(t, c)
	for _, contender := range []struct {
		name string
		run  func(context.Context) error
		want error
	}{
		{"Start", c.Start, ErrLocked},
		{"Stop", c.Stop, ErrLocked},
		{"Repair", c.Repair, ErrLocked},
		{"Remove", func(ctx context.Context) error { return c.Remove(ctx, RemoveOptions{}) }, ErrLocked},
		{"RemovePurge", func(ctx context.Context) error { return c.Remove(ctx, RemoveOptions{Purge: true}) }, ErrLocked},
		{"Install", func(ctx context.Context) error { return c.Install(ctx, InstallOptions{}) }, ErrLocked},
		// Selection refuses any existing manifest before it takes the lock.
		{"InstallManagedPrincipals", func(ctx context.Context) error { return c.Install(ctx, InstallOptions{ManagedPrincipals: true}) }, errPrincipalLayout},
		{"Reinstall", func(ctx context.Context) error { return c.Reinstall(ctx, reinstallOptions(f)) }, ErrLocked},
		{"SetServiceDesiredState", func(ctx context.Context) error { return c.SetServiceDesiredState(ctx, "running") }, ErrLocked},
	} {
		t.Run(contender.name, func(t *testing.T) {
			started := time.Now()
			err := contender.run(t.Context())
			if !errors.Is(err, contender.want) || time.Since(started) > reinstallContentionBound || !reflect.DeepEqual(held, retainedSnapshot(t, c)) {
				t.Fatal("concurrent operation was not refused promptly without a record")
			}
		})
	}
	unblock()
	if err := <-result; err != nil {
		t.Fatal("blocked reinstall did not complete after release")
	}
}

func TestReinstallServiceDescribedCell(t *testing.T) {
	f, _ := reinstallFixture(t, "legacy", true)
	c := f.cell
	descriptor := retainedRead(t, c, "service/descriptor.json")
	host, err := os.Executable()
	if err != nil {
		t.Fatal("test executable unavailable")
	}
	prepare := func() error {
		return c.PrepareService(t.Context(), ServiceDescriptor{Adapter: "foreground", Payload: ServicePayload{Host: host, Args: []string{"run", "--root", c.cfg.Root}}})
	}
	if err := prepare(); err == nil || err.Error() != "service_installation_required" {
		t.Fatal("removed cell accepted service preparation")
	}
	if err := c.Reinstall(t.Context(), reinstallOptions(f)); err != nil {
		t.Fatal(err)
	}
	if d, err := c.ServiceDescriptor(); err != nil || d.DesiredState != "stopped" || !bytes.Equal(descriptor, retainedRead(t, c, "service/descriptor.json")) {
		t.Fatal("reinstall changed the service descriptor or desired state")
	}
	if prepare() != nil || c.SetServiceDesiredState(t.Context(), "running") != nil {
		t.Fatal("readmitted cell refused service preparation or running intent")
	}
	if err := newSupervisor(c, SupervisorPolicy{}).hooks.start(t.Context()); err != nil || !slices.Contains(f.launches, "server") {
		t.Fatal("supervisor start hook did not launch the readmitted cell")
	}
}

// reinstallPredecessorJournalStep is copied literally from the pre-slice validJournalStep.
func reinstallPredecessorJournalStep(operation, step string) bool {
	steps := map[string]map[string]bool{
		"install": {"manifest": true, "password": true, "initdb": true, "configure": true, "postgres": true, "database": true, "stop": true, "principal_credentials": true, "principals": true, "principal_migrations": true, "principal_grants": true, "principal_verified": true},
		"start":   {"ports": true, "postgres": true, "server": true, "server_ready": true, "account": true, "worker": true, "worker_ready": true, "principal_verified": true},
		"stop":    {"stop": true},
		"remove":  {"stop": true, "spool": true, "tmp": true, "logs": true, "purge": true, "runtime_cleanup": true},
	}
	allowed, ok := steps[operation]
	return ok && (step == "operation" || step == "revert_stop" || allowed[step])
}

func TestReinstallCompatibility(t *testing.T) {
	f, r := reinstallFixture(t, "legacy", false)
	c := f.cell
	if err := c.Reinstall(t.Context(), reinstallOptions(f)); err != nil {
		t.Fatal(err)
	}
	reinstallLines := 0
	for _, line := range bytes.SplitAfter(retainedRead(t, c, "journal/operations.jsonl"), []byte("\n")) {
		var entry journalEntry
		if len(line) == 0 {
			continue
		}
		if json.Unmarshal(line, &entry) != nil || !validJournalStep(entry.Operation, entry.Step) {
			t.Fatal("current journal vocabulary refused a written record")
		}
		if reinstallPredecessorJournalStep(entry.Operation, entry.Step) == (entry.Operation == "reinstall") {
			t.Fatal("predecessor step table accepted a reinstall record or refused an older one")
		}
		if entry.Operation == "reinstall" {
			reinstallLines++
		}
	}
	if _, err := c.readJournal(); err != nil || reinstallLines != len(reinstallRecords) {
		t.Fatal("current reader refused the reinstall records")
	}
	want := bytes.Replace(r.preRemoval, []byte(`"last_operation": ""`), []byte(`"last_operation": "reinstall"`), 1)
	if bytes.Equal(want, r.preRemoval) || !bytes.Equal(want, retainedRead(t, c, "manifest.json")) {
		t.Fatal("readmitted manifest bytes differ from the pre-removal bytes")
	}
}

var reinstallForbidden = []string{"Remove", "RemoveAll", "Rename", "Truncate", "WriteFile", "Mkdir", "MkdirAll", "Create", "OpenFile",
	"removePrivateFile", "publishPrivateFile", "makePassword", "makePrincipalPasswords", "pgCommand", "databaseCommand",
	"setServiceDesiredStateLocked", "writeServiceDescriptor", "retainRemoval", "finishRemoval", "Purge"}

// reinstallSourceContract pins the single marker-clearing site and seam reach
// across all production text, independent of GOOS and build tags.
func reinstallSourceContract(sources map[string]string) []string {
	var failures []string
	clears, references := 0, map[string]int{}
	for name, source := range sources {
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, strings.ReplaceAll(source, "\r\n", "\n"), 0)
		if err != nil {
			failures = append(failures, name+"_parse")
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.AssignStmt:
				for index, left := range node.Lhs {
					if selector, ok := left.(*ast.SelectorExpr); !ok || selector.Sel.Name != "RemovalRetained" {
						continue
					}
					var value *ast.Ident
					if len(node.Rhs) == len(node.Lhs) {
						value, _ = node.Rhs[index].(*ast.Ident)
					}
					if value != nil && value.Name == "false" && name == "reinstall.go" {
						clears++
					} else if value == nil || value.Name != "true" {
						failures = append(failures, name+"_marker_assignment")
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := node.Key.(*ast.Ident); ok && key.Name == "RemovalRetained" {
					if value, ok := node.Value.(*ast.Ident); !ok || value.Name != "true" {
						failures = append(failures, name+"_marker_literal")
					}
				}
			case *ast.SelectorExpr:
				references[node.Sel.Name]++
			case *ast.Ident:
				if name == "reinstall.go" && slices.Contains(reinstallForbidden, node.Name) {
					failures = append(failures, "reinstall.go_forbidden_"+node.Name)
				}
			}
			return true
		})
	}
	if clears != 1 {
		failures = append(failures, "marker_clear_count")
	}
	for _, seam := range []string{"startRetainedPostgres", "reinstallLocked"} {
		if references[seam] != 1 {
			failures = append(failures, seam+"_reference_count")
		}
	}
	sort.Strings(failures)
	return failures
}

func TestReinstallSourceContract(t *testing.T) {
	sources := admissionReadSources(t)
	for _, ending := range []string{"LF", "CRLF"} {
		t.Run(ending, func(t *testing.T) {
			input := serviceAdmissionClone(sources)
			if ending == "CRLF" {
				for name, source := range input {
					input[name] = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\n", "\r\n")
				}
			}
			if failures := reinstallSourceContract(input); len(failures) != 0 {
				t.Fatalf("reinstall source contract: %v", failures)
			}
		})
	}
	t.Run("UnmarkedPostgresRefused", func(t *testing.T) {
		c, m := unitCell(t)
		c.cfg.Binaries.PostgresDir = t.TempDir() // Absent executables: no native PostgreSQL can run.
		if !errors.Is(c.startRetainedPostgres(t.Context(), m), errReinstallState) {
			t.Fatal("retained PostgreSQL seam admitted an unmarked manifest")
		}
		for _, name := range []string{"run/postgres.json", "pg/postmaster.pid", "logs/postgres.log"} {
			if _, err := os.Lstat(c.path(filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refused retained PostgreSQL seam left a process record or launch effect")
			}
		}
	})
}

// Inspect offers reinstall only where admission's own offline predicate holds;
// every other retained root keeps the pre-slice text (review LC-S7D2D-XR1-3).
// Inspect writes nothing, and Reinstall agrees with it on every row.
func TestReinstallInspectState(t *testing.T) {
	available, unsupported := "removed; data retained; explicit reinstall available", "removed; data retained; reinstall unsupported"
	edit := func(change func(*Manifest)) func(*testing.T, *retainedFixture) {
		return func(t *testing.T, f *retainedFixture) {
			m, err := readManifest(f.cell.cfg.Root)
			if err != nil {
				t.Fatal("cannot read reinstall fixture manifest")
			}
			change(m)
			if writeAtomic(f.cell.path("manifest.json"), m, nil) != nil {
				t.Fatal("cannot edit reinstall fixture manifest")
			}
		}
	}
	for _, row := range []struct {
		name string
		arm  func(*testing.T, *retainedFixture)
		want string
	}{
		{"healthy", func(*testing.T, *retainedFixture) {}, available},
		{"pg-absent", func(t *testing.T, f *retainedFixture) {
			if os.RemoveAll(f.cell.path("pg")) != nil {
				t.Fatal("cannot remove retained PGDATA")
			}
		}, "removed; data purged or missing; reinstall unsupported"},
		// Repair settles an interrupted purge this way; purge consent was given.
		{"interrupted-purge", edit(func(m *Manifest) { m.Installed = false }), unsupported},
		{"other-build-schema", edit(func(m *Manifest) { m.LastVerifiedSchemaVersion = store.SchemaVersion() + 1 }), unsupported},
		{"older-build-schema", edit(func(m *Manifest) { m.LastVerifiedSchemaVersion = store.SchemaVersion() - 1 }), unsupported},
		{"never-verified", edit(func(m *Manifest) { m.AccountID, m.LastVerifiedSchemaVersion = "", 0 }), unsupported},
		{"open-reinstall", func(t *testing.T, f *retainedFixture) {
			f.cell.cfg.Failpoint = func(operation, step string) error {
				if operation == "reinstall" && step == "operation_started" {
					return errors.New("synthetic crash")
				}
				return nil
			}
			if !errors.Is(f.cell.Reinstall(t.Context(), reinstallOptions(f)), errInterrupted) {
				t.Fatal("cannot leave a reinstall open")
			}
			f.cell.cfg.Failpoint = nil
		}, unsupported},
		{"open-removal", func(t *testing.T, f *retainedFixture) {
			f.cell.cfg.Failpoint = retainedCrashAt("removal_retained", nil)
			if !errors.Is(f.cell.Remove(t.Context(), RemoveOptions{}), errInterrupted) {
				t.Fatal("cannot leave a removal open")
			}
			f.cell.cfg.Failpoint = nil
		}, unsupported},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, r := reinstallFixture(t, "legacy", false)
			c := f.cell
			row.arm(t, f)
			before := retainedSnapshot(t, c)
			inspection, err := c.Inspect(t.Context())
			if err != nil || inspection.InstallState != row.want || !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
				t.Fatal("retained inspection reported a different state or wrote bytes")
			}
			calls := len(r.calls)
			if err := c.Reinstall(t.Context(), reinstallOptions(f)); (err == nil) != (row.want == available) || (err != nil && len(r.calls) != calls) {
				t.Fatal("reinstall admission disagreed with inspection")
			}
			if row.want != available {
				return
			}
			if inspection, err := c.Inspect(t.Context()); err != nil || inspection.InstallState != "" {
				t.Fatal("readmitted inspection kept the removal text")
			}
		})
	}
}
