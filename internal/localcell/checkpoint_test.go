package localcell

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/store"
)

// checkpointSyntheticControl is a labelled synthetic pg_controldata reading of
// a cleanly stopped PostgreSQL 16 cluster; no real cluster is read.
var checkpointSyntheticControl = pgControlData{ControlVersion: "1300", CatalogVersion: "202307071",
	SystemIdentifier: "7300000000000000001", State: "shut down", CheckpointLSN: "0/1548F48"}

// checkpointUnallocatedPID is above every PID Linux (4194304) or Darwin
// (99999) allocates, and Windows reports no such process for it.
const checkpointUnallocatedPID = 2147483644

// checkpointRecorder replaces the control, free, afterCopy, beforeClose and
// newID boundaries and counts each call. production runs the real control.
type checkpointRecorder struct {
	calls      map[string]int
	readings   []pgControlData // successive control readings; the last repeats
	controlErr error
	production bool
	free       func(string) (uint64, uint64, error)
	probes     []string
	after      func(string)
	before     func()
	nextID     func() (string, error)
}

func (r *checkpointRecorder) install(c *Cell) {
	c.checkpointTest = &checkpointOperations{
		control: func(ctx context.Context) (pgControlData, error) {
			if r.calls["control"]++; r.production {
				return c.checkpointControl(ctx)
			} else if r.controlErr != nil {
				return pgControlData{}, r.controlErr
			}
			return r.readings[min(r.calls["control"], len(r.readings))-1], nil
		},
		free: func(probe string) (uint64, uint64, error) {
			r.calls["free"], r.probes = r.calls["free"]+1, append(r.probes, probe)
			return r.free(probe)
		},
		afterCopy:   func(dir string) { r.calls["afterCopy"]++; r.after(dir) },
		beforeClose: func() { r.before() },
		newID:       func() (string, error) { r.calls["newID"]++; return r.nextID() },
	}
}

func checkpointNewID() (string, error) { return id.New("ckpt") }

// checkpointFixture is a synthetic cold cell with a verified account at this
// build's schema and a small synthetic PGDATA, retained by a real ordinary
// Remove when asked. No process or database runs.
func checkpointFixture(t *testing.T, layout string, retained bool) (*retainedFixture, *checkpointRecorder) {
	t.Helper()
	f := retainedRemovalFixture(t, layout, false)
	c, m := f.cell, f.manifest
	m.AccountID, m.LastVerifiedSchemaVersion = "acc_aaaaaaaaaaaaaaaa", store.SchemaVersion()
	if writeAtomic(c.path("manifest.json"), &m, nil) != nil {
		t.Fatal("cannot prepare verified checkpoint fixture")
	}
	for _, dir := range []string{"pg/base", "pg/base/1", "pg/global", "pg/pg_notify", "pg/pg_tblspc"} {
		if privateDir(c.path(filepath.FromSlash(dir)), true) != nil {
			t.Fatal("cannot prepare synthetic PGDATA directory")
		}
	}
	checkpointWrite(t, c, "pg/base/1/1259", "synthetic relation bytes")
	checkpointWrite(t, c, "pg/global/pg_control", "synthetic control bytes")
	if retained && c.Remove(t.Context(), RemoveOptions{}) != nil {
		t.Fatal("cannot retain checkpoint fixture")
	}
	current, err := readManifest(c.cfg.Root)
	if err != nil {
		t.Fatal("cannot read checkpoint fixture manifest")
	}
	c.cfg.Binaries.PostgresDir = t.TempDir() // Absent executables: no native PostgreSQL can run.
	r := &checkpointRecorder{calls: map[string]int{}, readings: []pgControlData{checkpointSyntheticControl}, after: func(string) {}, before: func() {},
		free: func(string) (uint64, uint64, error) { return 1 << 50, 1 << 51, nil }, nextID: checkpointNewID}
	r.install(c)
	f.manifest, f.prefix, f.material = *current, retainedRead(t, c, "journal/operations.jsonl"), retainedSnapshot(t, c)
	return f, r
}

// checkpointRecord publishes a server process record of this test process.
func checkpointRecord(t *testing.T, c *Cell, start string) {
	if writeAtomic(c.path("run", "server.json"), ProcessRecord{PID: os.Getpid(), StartTime: start, Root: c.cfg.Root, Component: "server"}, nil) != nil {
		t.Fatal("cannot prepare process record")
	}
}

func checkpointOptions(f *retainedFixture) CheckpointOptions {
	return CheckpointOptions{InstallationID: f.manifest.InstallationID}
}

func checkpointWrite(t *testing.T, c *Cell, name, value string) {
	t.Helper()
	if writeAtomicBytes(c.path(filepath.FromSlash(name)), []byte(value), nil) != nil {
		t.Fatal("cannot write checkpoint fixture material")
	}
}

// checkpointIDs lists recovery/ by name; an absent recovery/ lists nothing.
func checkpointIDs(t *testing.T, c *Cell) []string {
	t.Helper()
	entries, err := os.ReadDir(c.recoveryDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cannot list checkpoints")
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// checkpointPlant publishes a synthetic checkpoint: a witness naming witnessID
// when it is not empty, and private files at the given slash paths.
func checkpointPlant(t *testing.T, c *Cell, checkpointID, witnessID string, files ...string) string {
	t.Helper()
	dir := c.path("recovery", checkpointID)
	directories := []string{c.recoveryDir(), dir}
	for _, name := range files {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			directories = append(directories, filepath.Join(dir, filepath.FromSlash(parent)))
		}
	}
	slices.Sort(directories[2:])
	for _, directory := range directories {
		if privateDir(directory, true) != nil {
			t.Fatal("cannot plant a synthetic checkpoint directory")
		}
	}
	for _, name := range files {
		checkpointWrite(t, c, path.Join("recovery", checkpointID, name), "synthetic checkpoint bytes")
	}
	witness := checkpointWitness{Format: 1, CheckpointID: witnessID, InstallationID: "lc_test", CreatedAt: time.Now().UTC(), Bytes: 26}
	if witnessID != "" && writeAtomic(filepath.Join(dir, checkpointWitnessName), witness, nil) != nil {
		t.Fatal("cannot plant synthetic witness")
	}
	return dir
}

// checkpointSameError requires a fixed cause by its exact text, independently
// of the production variable, or else the exact error text.
func checkpointSameError(err error, want string) bool {
	var cause fixedCause
	if strings.HasPrefix(want, "local_") {
		return errors.As(err, &cause) && string(cause) == want
	}
	return err != nil && err.Error() == want
}

// checkpointUnchanged requires a byte- and mode-identical tree, apart from the
// run/ entries the pinned lock prefix creates when a purge removed them.
func checkpointUnchanged(t *testing.T, c *Cell, before map[string]retainedEntry) {
	t.Helper()
	after := checkpointTreeState(t, c)
	for _, name := range []string{"run", "run/operation.lock"} {
		if _, existed := before[name]; !existed {
			delete(after, name)
		}
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refused checkpoint call changed the tree beyond its lock prefix")
	}
}

// checkpointTreeState is retainedSnapshot that reads only regular files: a
// link, FIFO or junction is recorded by its Lstat mode, never opened.
func checkpointTreeState(t *testing.T, c *Cell) map[string]retainedEntry {
	t.Helper()
	result := map[string]retainedEntry{}
	err := filepath.WalkDir(c.cfg.Root, func(name string, entry fs.DirEntry, err error) error {
		rel, relErr := filepath.Rel(c.cfg.Root, name)
		info, infoErr := entry.Info()
		if err = errors.Join(err, relErr, infoErr); err != nil {
			return err
		}
		value := retainedEntry{mode: info.Mode()}
		// Windows locks the empty lock byte range mandatorily.
		if entry.Type().IsRegular() && !strings.HasSuffix(rel, ".lock") {
			data, err := os.ReadFile(name)
			value.size, value.data = info.Size(), string(data)
			if err != nil {
				return err
			}
		}
		result[filepath.ToSlash(rel)] = value
		return nil
	})
	if err != nil {
		t.Fatal("cannot snapshot the checkpoint fixture tree")
	}
	return result
}

// checkpointCopySet is the fixture's live copy set by slash path: PGDATA, the
// manifest, run/ and the layout's custody.
func checkpointCopySet(f *retainedFixture) map[string]retainedEntry {
	set := map[string]retainedEntry{}
	for name, entry := range f.material {
		if name == "pg" || strings.HasPrefix(name, "pg/") || name == "manifest.json" || name == "run" || name == "run/postgres.password" ||
			name == "run/credentials" || strings.HasPrefix(name, "run/credentials/") {
			set[name] = entry
		}
	}
	return set
}

var checkpointRecords = []string{"checkpoint/operation/started", "checkpoint/copy/started", "checkpoint/copy/done",
	"checkpoint/inventory/started", "checkpoint/inventory/done", "checkpoint/witness/started", "checkpoint/witness/done",
	"checkpoint/operation/done"}

// checkpointCase is one admission row's fixture.
type checkpointCase struct {
	t       *testing.T
	f       *retainedFixture
	c       *Cell
	r       *checkpointRecorder
	options *CheckpointOptions
}

func TestCheckpointAdmissionRefusals(t *testing.T) {
	own, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		t.Fatal("cannot observe the test process identity")
	}
	manifest := func(edit func(*Manifest)) func(*checkpointCase) {
		return func(k *checkpointCase) {
			m := k.f.manifest
			if edit(&m); writeAtomic(k.c.path("manifest.json"), &m, nil) != nil {
				k.t.Fatal("cannot edit checkpoint fixture manifest")
			}
		}
	}
	write := func(name, value string) func(*checkpointCase) {
		return func(k *checkpointCase) { checkpointWrite(k.t, k.c, name, value) }
	}
	remove := func(name string) func(*checkpointCase) {
		return func(k *checkpointCase) { _ = os.RemoveAll(k.c.path(filepath.FromSlash(name))) }
	}
	record := func(start string) func(*checkpointCase) {
		return func(k *checkpointCase) { checkpointRecord(k.t, k.c, start) }
	}
	service := func(arm func(*testing.T, *Cell) error) func(*checkpointCase) {
		return func(k *checkpointCase) {
			if prepareSupervisorTest(k.t, k.c); arm(k.t, k.c) != nil {
				k.t.Fatal("cannot arm the service fixture")
			}
		}
	}
	plant := func(names ...string) func(*checkpointCase) {
		return func(k *checkpointCase) { checkpointPlant(k.t, k.c, names[0], names[1], names[2:]...) }
	}
	control := func(state, failure string) func(*checkpointCase) {
		return func(k *checkpointCase) {
			reading := checkpointSyntheticControl
			reading.State = state
			if k.r.readings = []pgControlData{reading}; failure != "" {
				k.r.controlErr = fixedCause(failure)
			}
		}
	}
	a, b, cc := "ckpt_aaaaaaaaaaaaaaaa", "ckpt_bbbbbbbbbbbbbbbb", "ckpt_cccccccccccccccc"
	both, legacy, selected := []string{"legacy", "selected"}, []string{"legacy"}, []string{"selected"}
	// position is the R4 predicate a row violates alone: control runs at 15,
	// free at 16 and identity allocation (17) follows both; 0 is the owner's fence.
	for _, row := range []struct {
		name     string
		layouts  []string
		position int
		setup    func(*checkpointCase)
		want     string
	}{
		{"empty-id", legacy, 1, func(k *checkpointCase) { k.options.InstallationID = "" }, "local_checkpoint_identity_mismatch"},
		{"wrong-id", legacy, 3, func(k *checkpointCase) { k.options.InstallationID += "-other" }, "local_checkpoint_identity_mismatch"},
		{"manifest-absent-purged", legacy, 2, func(k *checkpointCase) { _ = k.c.Remove(k.t.Context(), RemoveOptions{Purge: true}) }, "local_checkpoint_state_unsupported"},
		{"not-installed", legacy, 4, manifest(func(m *Manifest) { m.Installed = false }), "local_checkpoint_state_unsupported"},
		{"cleanup-pending", legacy, 4, manifest(func(m *Manifest) {
			m.Cleanup = &journalEntry{Operation: "remove", Step: "runtime_cleanup", State: "started", At: time.Now().UTC()}
		}), "local_checkpoint_state_unsupported"},
		{"account-empty", legacy, 5, manifest(func(m *Manifest) { m.AccountID = "" }), "local_checkpoint_state_unsupported"},
		{"schema-zero", legacy, 5, manifest(func(m *Manifest) { m.LastVerifiedSchemaVersion = 0 }), "local_checkpoint_state_unsupported"},
		{"pending-operation", legacy, 6, func(k *checkpointCase) { _ = k.c.begin("stop") }, "local_checkpoint_state_unsupported"},
		{"torn-tail", legacy, 6, func(k *checkpointCase) { write("journal/operations.jsonl", string(k.f.prefix)+`{"operation":`)(k) }, "local_checkpoint_state_unsupported"},
		{"valid-fence", legacy, 0, func(k *checkpointCase) { updateTestFence(k.t, k.c) }, "local_maintenance_fence_active"},
		{"invalid-fence", legacy, 0, func(k *checkpointCase) { updateTestFence(k.t, k.c); write("journal/maintenance.json", "{broken\n")(k) }, "local_maintenance_fence_active"},
		{"live-record", legacy, 7, record(own), "local_checkpoint_state_unsupported"},
		{"foreign-record", legacy, 7, record("foreign-incarnation"), "local cell process identity mismatch; refusing to signal"},
		{"service-maintenance", legacy, 8, service(func(t *testing.T, c *Cell) error { return c.SetServiceDesiredState(t.Context(), "maintenance") }), "local_checkpoint_state_unsupported"},
		{"transition-present", legacy, 8, service(func(_ *testing.T, c *Cell) error {
			d, err := c.ServiceDescriptor()
			return errors.Join(err, writeAtomic(c.path("service", "transition.json"), d, nil))
		}), "local_checkpoint_state_unsupported"},
		{"bootstrap-missing", both, 9, remove("run/postgres.password"), "local cell password file is unavailable or not private"},
		{"selected-five-of-six", selected, 9, remove("run/credentials/" + string(store.SyncPrincipalWorker)), "local_principal_credentials_refused"},
		{"relative-postgres-dir", legacy, 10, func(k *checkpointCase) { k.c.cfg.Binaries.PostgresDir = "relative-postgres" }, "explicit PostgreSQL binary directory is required"},
		{"pg-absent", legacy, 11, remove("pg"), "local_checkpoint_state_unsupported"},
		{"postmaster-pid", legacy, 11, func(k *checkpointCase) {
			write("pg/postmaster.pid", fmt.Sprintf("%d\n%s\n1\n", checkpointUnallocatedPID, k.c.path("pg")))(k)
		}, "local_checkpoint_state_unsupported"},
		{"pg-version-15", legacy, 11, write("pg/PG_VERSION", "15\n"), "local_checkpoint_source_unsupported"},
		{"tablespace", legacy, 14, write("pg/pg_tblspc/16384", "synthetic tablespace entry"), "local_checkpoint_source_unsupported"},
		// A private temporary at any depth, the cell's own crash residue, is never copied (XR1-02).
		{"pending-temporary", legacy, 14, write("pg/.pending-123456789", "synthetic temporary"), "local_checkpoint_source_unsupported"},
		{"pending-temporary-nested", legacy, 14, write("pg/base/1/.pending-x", "synthetic temporary"), "local_checkpoint_source_unsupported"},
		{"pending-directory", legacy, 14, func(k *checkpointCase) {
			if privateDir(k.c.path("pg", "base", ".pending-d"), true) != nil {
				k.t.Fatal("cannot prepare a pending directory")
			}
		}, "local_checkpoint_source_unsupported"},
		{"recovery-invalid-deep", legacy, 12, func(k *checkpointCase) {
			recoveryBoundaryNonPrivate(k.t, filepath.Join(checkpointPlant(k.t, k.c, a, a, "pg/base/1/1259"), "pg", "base", "1", "1259"), false)
		}, "local_checkpoint_state_unsupported"},
		{"unknown-entry", legacy, 13, func(k *checkpointCase) { plant(a, a)(k); write("recovery/notes", "synthetic note")(k) }, "local_checkpoint_state_unsupported"},
		{"incomplete-present", legacy, 13, plant(a, "", "pg/PG_VERSION"), "local_checkpoint_incomplete"},
		{"foreign-witness", legacy, 13, plant(a, b), "local_checkpoint_incomplete"},
		{"two-complete", legacy, 13, func(k *checkpointCase) { plant(a, a)(k); plant(b, b)(k) }, "local_checkpoint_limit_reached"},
		// R4.13's order: incomplete, then unknown, then the limit (MX13); an
		// identifier-shaped regular file is unknown, not incomplete (MX18).
		{"incomplete-before-limit", legacy, 13, func(k *checkpointCase) { plant(a, a)(k); plant(b, b)(k); plant(cc, "", "pg/PG_VERSION")(k) }, "local_checkpoint_incomplete"},
		{"unknown-before-limit", legacy, 13, func(k *checkpointCase) { plant(a, a)(k); plant(b, b)(k); write("recovery/notes", "synthetic note")(k) }, "local_checkpoint_state_unsupported"},
		{"id-shaped-file-unknown", legacy, 13, func(k *checkpointCase) { plant(b, b)(k); write("recovery/"+a, "synthetic file")(k) }, "local_checkpoint_state_unsupported"},
		{"control-error", legacy, 15, control("shut down", "PostgreSQL command timed out"), "PostgreSQL command timed out"},
		{"control-in-production", legacy, 15, control("in production", ""), "local_checkpoint_state_unsupported"},
		// The production runner maps a missing pg_controldata to the fixed start text.
		{"control-unavailable", legacy, 15, func(k *checkpointCase) { k.r.production = true }, "PostgreSQL command could not start"},
		{"free-error", legacy, 16, func(k *checkpointCase) {
			k.r.free = func(string) (uint64, uint64, error) { return 0, 0, errors.New("synthetic probe failure") }
		}, "local_checkpoint_state_unsupported"},
		{"generator-error", legacy, 17, func(k *checkpointCase) {
			k.r.nextID = func() (string, error) { return "", errors.New("synthetic generator failure") }
		}, "cannot allocate local cell checkpoint identity"},
		{"generator-existing-id", legacy, 17, func(k *checkpointCase) {
			plant(a, a)(k)
			k.r.nextID = func() (string, error) { return a, nil }
		}, "cannot allocate local cell checkpoint identity"},
	} {
		for _, layout := range row.layouts {
			t.Run(row.name+"/"+layout, func(t *testing.T) {
				f, r := checkpointFixture(t, layout, false)
				options := checkpointOptions(f)
				row.setup(&checkpointCase{t, f, f.cell, r, &options})
				before := checkpointTreeState(t, f.cell)
				err := f.cell.Checkpoint(t.Context(), options)
				if checkpointUnchanged(t, f.cell, before); !checkpointSameError(err, row.want) {
					t.Fatal("checkpoint admission returned a different refusal")
				}
				reached := func(position int) int { return map[bool]int{true: 1}[row.position >= position] }
				if r.calls["control"] != reached(15) || r.calls["free"] != reached(16) || r.calls["newID"] != reached(17) || r.calls["afterCopy"] != 0 {
					t.Fatal("a refused checkpoint reached a boundary out of admission order")
				}
			})
		}
	}
	// A Faulted service launches nothing, so it does not fence a checkpoint.
	t.Run("faulted-service-admitted", func(t *testing.T) {
		f, _ := checkpointFixture(t, "legacy", false)
		if prepareSupervisorTest(t, f.cell); f.cell.setServiceFaulted(t.Context(), true) != nil {
			t.Fatal("cannot fault the service")
		}
		if err := f.cell.Checkpoint(t.Context(), checkpointOptions(f)); err != nil || len(checkpointIDs(t, f.cell)) != 1 {
			t.Fatal("a Faulted service refused the checkpoint")
		}
	})
}

// checkpointSum is the SHA-256 of data, compared and never printed.
func checkpointSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// checkpointDecode strictly decodes one record.
func checkpointDecode(data string, value any) error {
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func TestCheckpointSequenceAndContents(t *testing.T) {
	for _, layout := range []string{"legacy", "selected"} {
		for _, retained := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retained=%t", layout, retained), func(t *testing.T) {
				f, r := checkpointFixture(t, layout, retained)
				c, m := f.cell, f.manifest
				started := time.Now().UTC()
				err := c.Checkpoint(t.Context(), checkpointOptions(f))
				ids := checkpointIDs(t, c)
				if err != nil || len(ids) != 1 || !checkpointIDPattern.MatchString(ids[0]) || r.calls["control"] != 2 || r.calls["free"] != 1 || r.calls["afterCopy"] != 1 {
					t.Fatal("checkpoint did not create exactly one identified directory through its boundaries")
				}
				retainedAssertRecords(t, f, checkpointRecords)
				tree := retainedSnapshot(t, &Cell{cfg: Config{Root: c.path("recovery", ids[0])}})
				// The pg/** mirror, empty directories included, the manifest, run/,
				// the layout's custody and the two records: nothing else.
				want := checkpointCopySet(f)
				files, directories, copied := 0, 0, int64(0)
				for _, entry := range want {
					if entry.mode.IsDir() {
						directories++
					} else {
						files, copied = files+1, copied+entry.size
					}
				}
				if _, ok := want["pg/pg_notify"]; !ok || len(tree) != len(want)+3 {
					t.Fatal("checkpoint tree is not exactly the copy set")
				}
				for name, source := range want {
					got := tree[name]
					if got.data != source.data || got.size != source.size || got.mode.IsDir() != source.mode.IsDir() ||
						(runtime.GOOS != "windows" && got.mode.Perm() != map[bool]os.FileMode{true: 0o700, false: 0o600}[got.mode.IsDir()]) {
						t.Fatal("checkpoint entry missing, not private or different from its source")
					}
				}
				var inventory checkpointInventory
				data := tree[checkpointInventoryName].data
				if checkpointDecode(data, &inventory) != nil || inventory.Format != 1 || inventory.CheckpointID != ids[0] || len(inventory.Entries) != len(want) {
					t.Fatal("inventory is not the format-1 record of this checkpoint")
				}
				for index, entry := range inventory.Entries {
					source, planned := want[entry.Path]
					if !planned || (index > 0 && inventory.Entries[index-1].Path >= entry.Path) || source.mode.IsDir() != (entry.Type == "dir") ||
						(entry.Type == "dir" && (entry.Size != nil || entry.SHA256 != "")) ||
						(entry.Type == "file" && (entry.Size == nil || *entry.Size != source.size || entry.SHA256 != checkpointSum([]byte(source.data)))) {
						t.Fatal("inventory entries are not the sorted copy set with its digests")
					}
				}
				var witness checkpointWitness
				if checkpointDecode(tree[checkpointWitnessName].data, &witness) != nil || witness.CreatedAt.Before(started) || witness.CreatedAt.After(time.Now()) ||
					witness != (checkpointWitness{Format: 1, CheckpointID: ids[0], InstallationID: m.InstallationID, CreatedAt: witness.CreatedAt,
						LayoutVersion: LayoutVersion, PrincipalLayoutVersion: m.PrincipalLayoutVersion, SchemaVersion: store.SchemaVersion(),
						PostgresMajor: "16", Control: checkpointSyntheticControl, Files: files, Directories: directories, Bytes: copied,
						InventorySHA256: checkpointSum([]byte(data))}) || c.validateRecovery() != nil || c.validateRoot(false) != nil {
					t.Fatal("witness fields differ or the checkpoint is not private")
				}
				live, material := retainedSnapshot(t, c), maps.Clone(f.material)
				maps.DeleteFunc(live, func(name string, _ retainedEntry) bool {
					return name == "recovery" || strings.HasPrefix(name, "recovery/") || name == "journal/operations.jsonl"
				})
				if delete(material, "journal/operations.jsonl"); !reflect.DeepEqual(live, material) {
					t.Fatal("checkpoint changed the live tree or the manifest")
				}
				inspection, err := c.Inspect(t.Context())
				if err != nil || !reflect.DeepEqual(inspection.Checkpoints, []CheckpointSummary{{ID: ids[0], InstallationID: m.InstallationID,
					CreatedAt: witness.CreatedAt, Bytes: copied, Complete: true}}) {
					t.Fatal("inspection does not list the complete checkpoint")
				}
				if !retained && (c.Start(t.Context()) != nil || !slices.Contains(f.launches, "server")) {
					t.Fatal("checkpointed cell was not admitted to Start")
				}
			})
		}
	}
}

// checkpointNeed is an independent reading of R5 for the fixture: S over the
// live copy set, creating recovery/ when it is absent.
func checkpointNeed(f *retainedFixture, recoveryPresent bool) uint64 {
	size, directories := uint64(1<<20), uint64(2)
	if recoveryPresent {
		directories = 1
	}
	for _, entry := range checkpointCopySet(f) {
		if size += 256; entry.mode.IsDir() {
			directories++
		} else {
			size += uint64((entry.size + 4095) / 4096 * 4096)
		}
	}
	return 2*(size+directories*4096) + 1<<30
}

func TestCheckpointStorageRule(t *testing.T) {
	t.Run("boundary", func(t *testing.T) {
		f, r := checkpointFixture(t, "legacy", false)
		c := f.cell
		for _, row := range []struct {
			present bool
			slack   uint64
			probe   string
		}{{false, 0, c.cfg.Root}, {false, 1, c.cfg.Root}, {true, 0, c.recoveryDir()}, {true, 1, c.recoveryDir()}} {
			need, before := checkpointNeed(f, row.present), checkpointTreeState(t, c)
			r.free = func(string) (uint64, uint64, error) { return need - 1 + row.slack, need, nil }
			err := c.Checkpoint(t.Context(), checkpointOptions(f))
			if row.slack == 0 {
				checkpointUnchanged(t, c, before)
			}
			if (row.slack == 1) != (err == nil) || (err != nil && !checkpointSameError(err, "local_checkpoint_storage_insufficient")) || r.probes[len(r.probes)-1] != row.probe {
				t.Fatal("the storage rule was not exact or probed the wrong path")
			}
		}
	})
	t.Run("copy-sync-failure", func(t *testing.T) {
		f, r := checkpointFixture(t, "legacy", false)
		c := f.cell
		if err := c.Checkpoint(t.Context(), checkpointOptions(f)); err != nil {
			t.Fatal(err)
		}
		target, err := id.New("ckpt")
		r.nextID = func() (string, error) { return target, err }
		dir := c.path("recovery", target)
		copies, failedLineSeen, dirAtFailedLine := 0, false, true
		original := syncFile
		t.Cleanup(func() { syncFile = original })
		syncFile = func(file *os.File) error {
			if strings.HasPrefix(file.Name(), dir+string(filepath.Separator)) {
				if copies++; copies == 3 {
					return errors.New("synthetic sync failure")
				}
			}
			if data, err := os.ReadFile(file.Name()); err == nil && filepath.Base(file.Name()) == "operations.jsonl" &&
				bytes.HasSuffix(data, []byte(`"cause":"local_checkpoint_copy_failed"}`+"\n")) {
				_, statErr := os.Lstat(dir)
				failedLineSeen, dirAtFailedLine = true, !errors.Is(statErr, os.ErrNotExist)
			}
			return original(file)
		}
		live := checkpointTreeState(t, c)
		f.prefix = retainedRead(t, c, "journal/operations.jsonl")
		err = c.Checkpoint(t.Context(), checkpointOptions(f))
		if syncFile = original; !errors.Is(err, errCheckpointCopy) || !checkpointSameError(err, "local_checkpoint_copy_failed") || !failedLineSeen || dirAtFailedLine {
			t.Fatal("copy failure was not journaled after its directory was released")
		}
		retainedAssertRecords(t, f, []string{"checkpoint/operation/started", "checkpoint/copy/started", "checkpoint/copy/failed", "checkpoint/operation/done"})
		after := checkpointTreeState(t, c)
		delete(after, "journal/operations.jsonl")
		if delete(live, "journal/operations.jsonl"); !reflect.DeepEqual(live, after) {
			t.Fatal("copy failure changed the live tree or the bystander checkpoint")
		}
		if r.nextID = checkpointNewID; c.Checkpoint(t.Context(), checkpointOptions(f)) != nil {
			t.Fatal("checkpoint after a released copy failure did not succeed")
		}
	})
	t.Run("torn-close", func(t *testing.T) {
		f, r := checkpointFixture(t, "legacy", false)
		c := f.cell
		r.after = func(dir string) { checkpointRewrite(t, filepath.Join(dir, "pg", "base", "1", "1259")) }
		r.before = func() {
			checkpointWrite(t, c, "journal/operations.jsonl", string(retainedRead(t, c, "journal/operations.jsonl"))+`{"operation":`)
		}
		err := c.Checkpoint(t.Context(), checkpointOptions(f))
		if !checkpointSameError(err, "local_checkpoint_witness_mismatch") || !bytes.HasSuffix(retainedRead(t, c, "journal/operations.jsonl"), []byte("}\n"+`{"operation":`)) {
			t.Fatal("a closing line followed the torn journal tail")
		}
		if r.before = func() {}; c.Repair(t.Context()) != nil {
			t.Fatal("Repair did not settle the torn checkpoint operation")
		}
		if p, err := c.readJournal(); err != nil || p.pending != nil || p.tornTail || len(checkpointIDs(t, c)) != 0 {
			t.Fatal("torn checkpoint operation remained after Repair")
		}
	})
}

// checkpointRewrite replaces a file with different bytes of the same size.
func checkpointRewrite(t *testing.T, name string) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil || len(data) == 0 {
		t.Fatal("cannot read a file to rewrite")
	}
	data[0] ^= 0x20
	if writeAtomicBytes(name, data, nil) != nil {
		t.Fatal("cannot rewrite a file")
	}
}

// checkpointFailpoint crashes one boundary of one operation.
func checkpointFailpoint(operation, cut string) func(string, string) error {
	return func(got, step string) error {
		if got == operation && step == cut {
			return errors.New("synthetic crash")
		}
		return nil
	}
}

func checkpointSummaryOf(t *testing.T, c *Cell, checkpointID string) CheckpointSummary {
	t.Helper()
	inspection, err := c.Inspect(t.Context())
	if index := slices.IndexFunc(inspection.Checkpoints, func(s CheckpointSummary) bool { return s.ID == checkpointID }); err == nil && index >= 0 {
		return inspection.Checkpoints[index]
	}
	t.Fatal("inspection does not list the checkpoint")
	return CheckpointSummary{}
}

func TestCheckpointFailureCrashRepair(t *testing.T) {
	plantFile := func(t *testing.T, f *retainedFixture, name func(string) string) func(string) {
		return func(dir string) { checkpointWrite(t, f.cell, name(dir), "synthetic record\n") }
	}
	const mismatch = "local_checkpoint_witness_mismatch"
	for _, row := range []struct {
		name string
		arm  func(*testing.T, *retainedFixture, *checkpointRecorder)
		want string // the fixed cause; empty when the checkpoint is complete
	}{
		{"byte-flip", func(t *testing.T, _ *retainedFixture, r *checkpointRecorder) {
			r.after = func(dir string) { checkpointRewrite(t, filepath.Join(dir, "pg", "base", "1", "1259")) }
		}, mismatch},
		{"extra-copied-entry", func(t *testing.T, f *retainedFixture, r *checkpointRecorder) {
			r.after = plantFile(t, f, func(dir string) string { return path.Join("recovery", filepath.Base(dir), "pg", "recovery.signal") })
		}, mismatch},
		// The exact-tree count: a planned empty directory missing from the copy (XR1-03, MX10).
		{"missing-planned-directory", func(t *testing.T, _ *retainedFixture, r *checkpointRecorder) {
			r.after = func(dir string) {
				if os.Remove(filepath.Join(dir, "pg", "pg_notify")) != nil {
					t.Fatal("cannot remove a copied empty directory")
				}
			}
		}, mismatch},
		{"postmaster-pid", func(t *testing.T, f *retainedFixture, r *checkpointRecorder) {
			r.after = plantFile(t, f, func(string) string { return "pg/postmaster.pid" })
		}, mismatch},
		{"pg-control-rewritten", func(t *testing.T, f *retainedFixture, r *checkpointRecorder) {
			r.after = func(string) { checkpointRewrite(t, f.cell.path("pg", "global", "pg_control")) }
		}, mismatch},
		{"control-lsn-moved", func(_ *testing.T, _ *retainedFixture, r *checkpointRecorder) {
			moved := checkpointSyntheticControl
			moved.CheckpointLSN = "0/1548FC0"
			r.readings = []pgControlData{checkpointSyntheticControl, moved}
		}, mismatch},
		// Every field of the second reading is compared, the state too (MX11).
		{"control-state-differs", func(_ *testing.T, _ *retainedFixture, r *checkpointRecorder) {
			moved := checkpointSyntheticControl
			moved.State = "shut down in recovery"
			r.readings = []pgControlData{checkpointSyntheticControl, moved}
		}, mismatch},
		// A second control run that itself fails read nothing: a copy failure (XR1-11).
		{"second-control-fails", func(_ *testing.T, _ *retainedFixture, r *checkpointRecorder) {
			r.after = func(string) { r.controlErr = fixedCause("PostgreSQL command could not start") }
		}, "local_checkpoint_copy_failed"},
		{"witness-done-sync-fails", func(t *testing.T, _ *retainedFixture, _ *checkpointRecorder) {
			original, failed := syncFile, false
			t.Cleanup(func() { syncFile = original })
			syncFile = func(file *os.File) error {
				data, _ := os.ReadFile(file.Name())
				last := data[bytes.LastIndexByte(bytes.TrimSuffix(data, []byte("\n")), '\n')+1:]
				if !failed && filepath.Base(file.Name()) == "operations.jsonl" && bytes.Contains(last, []byte(`"step":"witness","state":"done"`)) {
					failed = true
					return errors.New("synthetic journal sync failure")
				}
				return original(file)
			}
		}, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			c := f.cell
			row.arm(t, f, r)
			err := c.Checkpoint(t.Context(), checkpointOptions(f))
			if err == nil || errors.Is(err, errInterrupted) || (row.want != "" && !checkpointSameError(err, row.want)) {
				t.Fatal("checkpoint failure was not reported with its cause")
			}
			p, err := c.readJournal()
			if ids := checkpointIDs(t, c); err != nil || p.pending != nil || p.tornTail || len(ids) != map[bool]int{true: 1}[row.want == ""] ||
				(row.want == "" && !checkpointSummaryOf(t, c, ids[0]).Complete) {
				t.Fatal("a failure left the operation open or its directory, or discarded a witnessed checkpoint")
			}
		})
	}
	t.Run("foreign-witness-listed-incomplete", func(t *testing.T) {
		f, _ := checkpointFixture(t, "legacy", false)
		checkpointPlant(t, f.cell, "ckpt_aaaaaaaaaaaaaaaa", "ckpt_bbbbbbbbbbbbbbbb")
		if summary := checkpointSummaryOf(t, f.cell, "ckpt_aaaaaaaaaaaaaaaa"); summary != (CheckpointSummary{ID: "ckpt_aaaaaaaaaaaaaaaa"}) {
			t.Fatal("a witness naming another checkpoint was listed complete")
		}
	})
	// A witness of another format names its own checkpoint and is not complete (MX6).
	t.Run("other-format-witness-listed-incomplete", func(t *testing.T) {
		f, _ := checkpointFixture(t, "legacy", false)
		dir := checkpointPlant(t, f.cell, "ckpt_cccccccccccccccc", "")
		if writeAtomic(filepath.Join(dir, checkpointWitnessName), checkpointWitness{Format: 2, CheckpointID: "ckpt_cccccccccccccccc"}, nil) != nil {
			t.Fatal("cannot plant a witness of another format")
		}
		if summary := checkpointSummaryOf(t, f.cell, "ckpt_cccccccccccccccc"); summary != (CheckpointSummary{ID: "ckpt_cccccccccccccccc"}) {
			t.Fatal("a witness of another format was listed complete")
		}
	})
	// The copy step refuses an identifier that appeared after allocation: it
	// neither writes into it nor removes it, witnessed or not (XR1-01: MX1, MX2).
	for _, witness := range []string{"ckpt_cccccccccccccccc", ""} {
		t.Run("copy-step-existing-id/witness="+strconv.FormatBool(witness != ""), func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			c, planted := f.cell, map[string]retainedEntry(nil)
			tree := func() map[string]retainedEntry { return retainedSnapshot(t, &Cell{cfg: Config{Root: c.recoveryDir()}}) }
			r.nextID = func() (string, error) { return "ckpt_cccccccccccccccc", nil }
			c.cfg.Failpoint = func(operation, step string) error {
				if operation == "checkpoint" && step == "operation_started" {
					checkpointPlant(t, c, "ckpt_cccccccccccccccc", witness, "pg/PG_VERSION", "manifest.json")
					planted = tree()
				}
				return nil
			}
			err := c.Checkpoint(t.Context(), checkpointOptions(f))
			retainedAssertRecords(t, f, []string{"checkpoint/operation/started", "checkpoint/copy/started", "checkpoint/copy/failed", "checkpoint/operation/done"})
			if !checkpointSameError(err, "local_checkpoint_copy_failed") || planted == nil || !reflect.DeepEqual(planted, tree()) {
				t.Fatal("the copy step wrote into or removed an identifier that appeared after allocation")
			}
		})
	}
	// Cancellation after admission is a copy failure: the directory is
	// discarded, the operation closed and nothing complete remains (MX7).
	t.Run("cancelled-after-admission", func(t *testing.T) {
		f, r := checkpointFixture(t, "legacy", false)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		r.after = func(string) { cancel() }
		err := f.cell.Checkpoint(ctx, checkpointOptions(f))
		if p, journalErr := f.cell.readJournal(); !checkpointSameError(err, "local_checkpoint_copy_failed") || journalErr != nil || p.pending != nil ||
			len(checkpointIDs(t, f.cell)) != 0 {
			t.Fatal("a cancelled checkpoint was not discarded and closed as a copy failure")
		}
	})
	for _, layout := range []string{"legacy", "selected"} {
		for _, cut := range []string{"operation_started", "copy", "inventory", "witness"} {
			t.Run("crash-"+cut+"/"+layout, func(t *testing.T) {
				f, r := checkpointFixture(t, layout, false)
				c := f.cell
				if c.cfg.Failpoint = checkpointFailpoint("checkpoint", cut); !errors.Is(c.Checkpoint(t.Context(), checkpointOptions(f)), errInterrupted) {
					t.Fatal("checkpoint crash was not injected")
				}
				c.cfg.Failpoint = nil
				ids := checkpointIDs(t, c)
				_, err := os.Lstat(filepath.Join(c.recoveryDir(), strings.Join(ids, ""), checkpointWitnessName))
				if len(ids) != map[bool]int{false: 1}[cut == "operation_started"] || ((cut == "copy" || cut == "inventory") && !errors.Is(err, os.ErrNotExist)) {
					t.Fatal("crash left a different set of checkpoint directories, or a witness before its step")
				}
				calls, repaired := maps.Clone(r.calls), c.Repair(t.Context())
				settled := retainedSnapshot(t, c)
				if repaired != nil || c.Repair(t.Context()) != nil || !reflect.DeepEqual(settled, retainedSnapshot(t, c)) || !maps.Equal(calls, r.calls) {
					t.Fatal("Repair reached a checkpoint boundary or a second Repair changed bytes")
				}
				if p, err := c.readJournal(); err != nil || p.pending != nil || !slices.Equal(checkpointIDs(t, c), ids) {
					t.Fatal("Repair left the operation open or changed recovery")
				}
				if cut != "operation_started" && checkpointSummaryOf(t, c, ids[0]).Complete != (cut == "witness") {
					t.Fatal("a crashed checkpoint is listed with the wrong completeness")
				}
				if cut == "operation_started" || cut == "witness" {
					return
				}
				refused := c.Checkpoint(t.Context(), checkpointOptions(f))
				if !checkpointSameError(refused, "local_checkpoint_incomplete") || c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: ids[0]}) != nil ||
					c.Checkpoint(t.Context(), checkpointOptions(f)) != nil {
					t.Fatal("an incomplete checkpoint did not refuse the next one until deleted by its identifier")
				}
			})
		}
	}
}

var checkpointDeleteRecords = []string{"checkpoint_delete/operation/started", "checkpoint_delete/unseal/started", "checkpoint_delete/unseal/done",
	"checkpoint_delete/remove/started", "checkpoint_delete/remove/done", "checkpoint_delete/operation/done"}

// checkpointWithout drops one checkpoint's entries and the journal record.
func checkpointWithout(tree map[string]retainedEntry, checkpointID string) map[string]retainedEntry {
	maps.DeleteFunc(tree, func(name string, _ retainedEntry) bool {
		return name == "journal/operations.jsonl" || name == "recovery/"+checkpointID || strings.HasPrefix(name, "recovery/"+checkpointID+"/")
	})
	return tree
}

func TestDeleteCheckpoint(t *testing.T) {
	own, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		t.Fatal("cannot observe the test process identity")
	}
	// twoComplete makes two real checkpoints and plants a live PGDATA sentinel.
	twoComplete := func(t *testing.T) (*retainedFixture, *Cell, []string) {
		f, _ := checkpointFixture(t, "legacy", false)
		if errors.Join(f.cell.Checkpoint(t.Context(), checkpointOptions(f)), f.cell.Checkpoint(t.Context(), checkpointOptions(f))) != nil {
			t.Fatal("cannot prepare the checkpoints")
		}
		checkpointWrite(t, f.cell, "pg/sentinel", "live sentinel")
		f.prefix = retainedRead(t, f.cell, "journal/operations.jsonl")
		return f, f.cell, checkpointIDs(t, f.cell)
	}
	for _, row := range []struct {
		name  string
		setup func(*testing.T, *Cell, string) string
		want  string
	}{
		{"bad-shape", func(*testing.T, *Cell, string) string { return "ckpt_AAAAAAAAAAAAAAAA" }, "local_checkpoint_identity_mismatch"},
		{"unknown-id", func(*testing.T, *Cell, string) string { return "ckpt_zzzzzzzzzzzzzzzz" }, "local_checkpoint_identity_mismatch"},
		{"dot-dot", func(*testing.T, *Cell, string) string { return ".." }, "local_checkpoint_identity_mismatch"},
		{"live-pgdata", func(*testing.T, *Cell, string) string { return "../pg" }, "local_checkpoint_identity_mismatch"},
		{"escaping-id", func(*testing.T, *Cell, string) string { return "ckpt_aaaaaaaaaaaaaaaa/../../pg" }, "local_checkpoint_identity_mismatch"},
		{"pending-operation", func(_ *testing.T, c *Cell, named string) string { _ = c.begin("stop"); return named }, "local_checkpoint_state_unsupported"},
		{"valid-fence", func(t *testing.T, c *Cell, named string) string { updateTestFence(t, c); return named }, "local_maintenance_fence_active"},
		{"live-record", func(t *testing.T, c *Cell, named string) string { checkpointRecord(t, c, own); return named }, "local_checkpoint_state_unsupported"},
		// Another spelling a case-insensitive volume resolves is never deleted (XR1-06).
		{"case-alias", func(t *testing.T, c *Cell, _ string) string {
			checkpointPlant(t, c, "CKPT_AAAAAAAAAAAAAAAA", "ckpt_aaaaaaaaaaaaaaaa", "pg/base/1/1259")
			return "ckpt_aaaaaaaaaaaaaaaa"
		}, "local_checkpoint_identity_mismatch"},
		{"recovery-invalid-inside", func(t *testing.T, c *Cell, named string) string {
			recoveryBoundaryNonPrivate(t, c.path("recovery", named, "pg", "base", "1", "1259"), false)
			return named
		}, "local_checkpoint_state_unsupported"},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, c, ids := twoComplete(t)
			named := row.setup(t, c, ids[0])
			before := checkpointTreeState(t, c)
			err := c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: named})
			if checkpointUnchanged(t, c, before); !checkpointSameError(err, row.want) {
				t.Fatal("refused deletion returned a different refusal")
			}
		})
	}
	t.Run("exact-id", func(t *testing.T) {
		f, c, ids := twoComplete(t)
		before := checkpointTreeState(t, c)
		err := c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: ids[0]})
		if retainedAssertRecords(t, f, checkpointDeleteRecords); err != nil || !slices.Equal(checkpointIDs(t, c), ids[1:]) ||
			!reflect.DeepEqual(checkpointWithout(before, ids[0]), checkpointWithout(checkpointTreeState(t, c), ids[0])) {
			t.Fatal("deletion removed more or less than the named checkpoint")
		}
	})
	t.Run("unseal-crash", func(t *testing.T) {
		_, c, ids := twoComplete(t)
		if c.cfg.Failpoint = checkpointFailpoint("checkpoint_delete", "unseal"); !errors.Is(c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: ids[0]}), errInterrupted) {
			t.Fatal("deletion crash was not injected at unseal")
		}
		c.cfg.Failpoint = nil
		_, witnessErr := os.Lstat(c.path("recovery", ids[0], checkpointWitnessName))
		if _, err := os.Lstat(c.path("recovery", ids[0], checkpointInventoryName)); err != nil || !errors.Is(witnessErr, os.ErrNotExist) {
			t.Fatal("unseal did not remove exactly the witness")
		}
		if c.Repair(t.Context()) != nil || checkpointSummaryOf(t, c, ids[0]).Complete ||
			c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: ids[0]}) != nil || !slices.Equal(checkpointIDs(t, c), ids[1:]) {
			t.Fatal("Repair changed an unsealed checkpoint, or a second exact-id call did not finish the deletion")
		}
	})
	t.Run("purged-root", func(t *testing.T) {
		_, c, ids := twoComplete(t)
		if c.Remove(t.Context(), RemoveOptions{Purge: true}) != nil {
			t.Fatal("cannot purge the checkpointed fixture")
		}
		before := checkpointTreeState(t, c)
		err := c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: ids[0]})
		changed := reinstallChanged(checkpointWithout(before, ids[0]), checkpointWithout(checkpointTreeState(t, c), ids[0]))
		if slices.Sort(changed); err != nil || !slices.Equal(changed, []string{"journal", "run", "run/operation.lock"}) || !slices.Equal(checkpointIDs(t, c), ids[1:]) {
			t.Fatal("deletion on a purged root recreated more than journal/ and the lock prefix")
		}
	})
}

func TestCheckpointRetentionAcrossLifecycle(t *testing.T) {
	f, _ := checkpointFixture(t, "legacy", false)
	c := f.cell
	if errors.Join(c.Checkpoint(t.Context(), checkpointOptions(f)), c.Checkpoint(t.Context(), checkpointOptions(f))) != nil {
		t.Fatal("cannot prepare two complete checkpoints")
	}
	recovery := func() map[string]retainedEntry { return retainedSnapshot(t, &Cell{cfg: Config{Root: c.recoveryDir()}}) }
	held := recovery()
	rr := &reinstallRecorder{cell: c, fail: map[string]error{}, account: f.manifest.AccountID, schema: strconv.Itoa(f.manifest.LastVerifiedSchemaVersion)}
	c.reinstallTest = &reinstallOperations{postgres: rr.postgres, query: rr.query, stop: rr.stop}
	reinstall := ReinstallOptions{InstallationID: f.manifest.InstallationID}
	// Stop, Repair, Remove{}, Reinstall and Remove{Purge: true}, in order.
	for _, operation := range []func(context.Context) error{c.Stop, c.Repair,
		func(ctx context.Context) error { return c.Remove(ctx, RemoveOptions{}) },
		func(ctx context.Context) error { return c.Reinstall(ctx, reinstall) },
		func(ctx context.Context) error { return c.Remove(ctx, RemoveOptions{Purge: true}) }} {
		if err := operation(t.Context()); err != nil || !reflect.DeepEqual(held, recovery()) {
			t.Fatal("a lifecycle operation changed recovery")
		}
	}
	inspection, err := c.Inspect(t.Context())
	if err != nil || len(inspection.Checkpoints) != 2 || !inspection.Checkpoints[0].Complete || !inspection.Checkpoints[1].Complete {
		t.Fatal("purged root does not still list both checkpoints")
	}
}

func TestCheckpointControlDataParse(t *testing.T) {
	// A labelled synthetic sample; the nonce placeholder is deliberately not hex.
	const sample = "pg_control version number:            1300\n" +
		"Catalog version number:               202307071\n" +
		"Database system identifier:           7300000000000000001\n" +
		"Database cluster state:               shut down\n" +
		"pg_control last modified:             Fri Nov 13 10:00:00 2026\n" +
		"Latest checkpoint location:           0/1548F48\n" +
		"Mock authentication nonce:            synthetic-nonce-placeholder\n"
	for _, input := range []string{sample, strings.ReplaceAll(sample, "\n", "\r\n"), sample + "Unknown synthetic field:  value\n"} {
		if data, err := parseCheckpointControl([]byte(input)); err != nil || data != checkpointSyntheticControl {
			t.Fatal("the labelled control sample, its CRLF copy or an unknown line did not parse exactly")
		}
	}
	replace := func(old, replacement string) string { return strings.Replace(sample, old, replacement, 1) }
	for _, row := range []struct{ name, input string }{
		{"crc-warning-before-fields", "WARNING: Calculated CRC checksum does not match value stored in file.\n\n" + sample},
		{"warning-after-version", replace("Catalog version number", "WARNING: synthetic warning\nCatalog version number")},
		{"stray-carriage-return", replace("shut down\n", "shut down\r\r\n")},
		{"state-outside-set", replace("shut down\n", "shut down cleanly\n")},
		{"missing-key", replace("Latest checkpoint location:           0/1548F48\n", "")},
		{"duplicate-key", replace("Latest checkpoint location", "Latest checkpoint location: 0/1\nLatest checkpoint location")},
		{"non-digit-identifier", replace("7300000000000000001", "73000000000000000x1")},
		{"bad-lsn", replace("0/1548F48", "0/1548f48")},
		{"oversized", sample + strings.Repeat("Synthetic padding line\n", 1024)},
	} {
		if _, err := parseCheckpointControl([]byte(row.input)); !checkpointSameError(err, "local_checkpoint_state_unsupported") {
			t.Fatalf("control output row %s was not refused", row.name)
		}
	}
}

func TestCheckpointFenceAndContention(t *testing.T) {
	f, r := checkpointFixture(t, "legacy", false)
	c := f.cell
	entered, release := make(chan string, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	r.after = func(dir string) { entered <- filepath.Base(dir); <-release }
	result := StartJoinedTestRunForTest(t, unblock, reinstallContentionBound, func() error {
		return c.Checkpoint(context.Background(), checkpointOptions(f))
	})
	var inFlight string
	select {
	case inFlight = <-entered:
	case <-result:
		t.Fatal("checkpoint returned before its copy finished")
	case <-time.After(reinstallContentionBound):
		t.Fatal("checkpoint did not reach its afterCopy boundary")
	}
	held := retainedSnapshot(t, c)
	for _, contender := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"Start", c.Start},
		{"RemovePurge", func(ctx context.Context) error { return c.Remove(ctx, RemoveOptions{Purge: true}) }},
		{"Reinstall", func(ctx context.Context) error {
			return c.Reinstall(ctx, ReinstallOptions{InstallationID: f.manifest.InstallationID})
		}},
		{"Checkpoint", func(ctx context.Context) error { return c.Checkpoint(ctx, checkpointOptions(f)) }},
		{"DeleteCheckpoint", func(ctx context.Context) error {
			return c.DeleteCheckpoint(ctx, DeleteCheckpointOptions{CheckpointID: inFlight})
		}},
	} {
		t.Run(contender.name, func(t *testing.T) {
			started := time.Now()
			err := contender.run(t.Context())
			if !errors.Is(err, ErrLocked) || time.Since(started) > reinstallContentionBound || !reflect.DeepEqual(held, retainedSnapshot(t, c)) {
				t.Fatal("concurrent operation was not refused promptly without a record")
			}
		})
	}
	inspection, err := c.Inspect(t.Context())
	if err != nil || !inspection.Locked || !reflect.DeepEqual(inspection.Checkpoints, []CheckpointSummary{{ID: inFlight}}) {
		t.Fatal("locked inspection did not list the in-flight checkpoint as incomplete")
	}
	if unblock(); <-result != nil || !checkpointSummaryOf(t, c, inFlight).Complete {
		t.Fatal("blocked checkpoint did not complete after release")
	}
}

func TestCheckpointDirShape(t *testing.T) {
	c, _ := unitCell(t)
	for _, value := range []string{"x", "ckpt_AAAAAAAAAAAAAAAA", "ckpt_" + strings.Repeat("a", 15), "ckpt_" + strings.Repeat("a", 17), "..", ".", "", "../pg",
		"ckpt_aaaaaaaaaaaaaaaa/..", "ckpt_aaaaaaaaaaaaaaaa/../../pg", "ckpt_aaaaaaaaaaaaaaaa\n", filepath.Join(c.cfg.Root, "ckpt_aaaaaaaaaaaaaaaa")} {
		if dir, err := c.checkpointDir(value); dir != "" || !checkpointSameError(err, "local_checkpoint_identity_mismatch") {
			t.Fatalf("checkpoint directory shape accepted row %q", value)
		}
	}
	if dir, err := c.checkpointDir("ckpt_abcdefghijklmn27"); err != nil || dir != c.path("recovery", "ckpt_abcdefghijklmn27") {
		t.Fatal("a valid checkpoint identifier did not name its directory")
	}
	// The Windows bound: 259 units fit a file; a directory keeps 34 more.
	if !checkpointPathFits(strings.Repeat("a", 259), false) || checkpointPathFits(strings.Repeat("a", 260), false) ||
		!checkpointPathFits(strings.Repeat("a", 225), true) || checkpointPathFits(strings.Repeat("a", 226), true) {
		t.Fatal("the Windows checkpoint path bound moved")
	}
}

func TestCheckpointRecoveryValidator(t *testing.T) {
	for _, row := range []struct {
		names []string
		want  bool
	}{{[]string{"Recovery"}, false}, {[]string{"RECOVERY"}, false}, {[]string{"recovery2"}, false}, {[]string{}, false}, {[]string{"pg", "recovery"}, true}} {
		if recoveryEntryCanonical(row.names, "recovery") != row.want {
			t.Fatal("canonical recovery spelling decided differently")
		}
	}
	t.Run("absent-and-deep-valid", func(t *testing.T) {
		f, _ := checkpointFixture(t, "legacy", false)
		valid := f.cell.validateRecovery() == nil
		checkpointPlant(t, f.cell, "ckpt_aaaaaaaaaaaaaaaa", "", "pg/base/1/1259", "run/credentials/one")
		if !valid || f.cell.validateRecovery() != nil {
			t.Fatal("an absent or valid private recovery tree was refused")
		}
	})
	// On a case-sensitive volume another spelling is an ordinary directory;
	// where it also resolves as recovery, RV refuses it.
	t.Run("non-canonical-spelling", func(t *testing.T) {
		f, _ := checkpointFixture(t, "legacy", false)
		if privateDir(filepath.Join(f.cell.cfg.Root, "Recovery"), true) != nil {
			t.Fatal("cannot prepare a differently spelled recovery directory")
		}
		if _, err := os.Lstat(f.cell.recoveryDir()); !errors.Is(err, os.ErrNotExist) {
			checkpointRefusedEverywhere(t, f)
		} else if f.cell.validateRecovery() != nil {
			t.Fatal("a differently spelled directory on a case-sensitive volume was refused")
		}
	})
	t.Run("nested-recovery-non-private", func(t *testing.T) {
		f, _ := checkpointFixture(t, "legacy", false)
		checkpointPlant(t, f.cell, "ckpt_aaaaaaaaaaaaaaaa", "ckpt_aaaaaaaaaaaaaaaa")
		if privateDir(filepath.Join(f.cell.recoveryDir(), "recovery"), true) != nil {
			t.Fatal("cannot prepare a nested recovery directory")
		}
		checkpointWrite(t, f.cell, "recovery/recovery/x", "synthetic")
		recoveryBoundaryNonPrivate(t, filepath.Join(f.cell.recoveryDir(), "recovery", "x"), false)
		checkpointRefusedEverywhere(t, f)
	})
}

// checkpointRefusedEverywhere requires RV, Checkpoint, DeleteCheckpoint and
// Inspect to refuse one bad recovery tree: the operations with zero writes,
// Inspect without listing anything.
func checkpointRefusedEverywhere(t *testing.T, f *retainedFixture) {
	t.Helper()
	c, want := f.cell, "local_checkpoint_state_unsupported"
	before := checkpointTreeState(t, c)
	if !checkpointSameError(c.validateRecovery(), want) || !checkpointSameError(c.Checkpoint(t.Context(), checkpointOptions(f)), want) ||
		!checkpointSameError(c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: "ckpt_aaaaaaaaaaaaaaaa"}), want) {
		t.Fatal("the validator, checkpoint or deletion admitted a bad recovery tree")
	}
	checkpointUnchanged(t, c, before)
	if inspection, err := c.Inspect(t.Context()); !checkpointSameError(err, want) || len(inspection.Checkpoints) != 0 {
		t.Fatal("inspection listed a bad recovery tree")
	}
}

// The copy plan admits only a name the inventory spells exactly and no private
// temporary at any depth (XR1-02, XR1-04). The decision is pure, so these rows
// run on every OS; the non-UTF-8 file itself is a Linux row.
func TestCheckpointNameSupported(t *testing.T) {
	for name, want := range map[string]bool{"pg": true, "pg/base/1/1259": true, "pg/base/1/x.pending-1": true,
		"pg/.pending-123456789": false, "pg/base/1/.pending-x": false, "pg/base/.pending-d": false, "pg/base/1/x\xff": false,
		"pg/base/1/x\xed\xa0\x80": false} {
		if checkpointNameSupported(name) != want {
			t.Fatalf("checkpoint name row %q decided differently", name)
		}
	}
}

// The checkpoint directory syncs keep their durable order (XR1-09): the copy
// syncs every planned directory deepest first, then its own, after its last
// file and before its inventory (MX3); the discard syncs recovery/ once the
// directory is gone (MX5); the unseal syncs the directory once the witness is
// gone and before anything else is (MX4), and a failed unseal sync is returned.
func TestCheckpointDirectorySyncOrder(t *testing.T) {
	if reflect.ValueOf(syncCheckpointDirectory).Pointer() != reflect.ValueOf(syncDirectory).Pointer() {
		t.Fatal("checkpoint directory sync no longer defaults to syncDirectory")
	}
	const target = "ckpt_cccccccccccccccc"
	// fixture logs each sync as its slash path below recovery/ plus probe of the
	// target directory, and fails the sync of *fail.
	fixture := func(t *testing.T, fail *string, probe func(string) string) (*retainedFixture, *checkpointRecorder, *[]string) {
		f, r := checkpointFixture(t, "legacy", false)
		dir, synced, original := f.cell.path("recovery", target), &[]string{}, syncCheckpointDirectory
		r.nextID = func() (string, error) { return target, nil }
		t.Cleanup(func() { syncCheckpointDirectory = original })
		syncCheckpointDirectory = func(name string) error {
			rel, err := filepath.Rel(f.cell.recoveryDir(), name)
			if *synced = append(*synced, filepath.ToSlash(rel)+probe(dir)); err != nil || filepath.ToSlash(rel) == *fail {
				return errors.New("synthetic directory sync failure")
			}
			return original(name)
		}
		return f, r, synced
	}
	present := func(name string) string {
		_, err := os.Lstat(name)
		return map[bool]string{true: "+", false: "-"}[err == nil]
	}
	t.Run("copy", func(t *testing.T) {
		f, _, synced := fixture(t, new(string), func(dir string) string {
			return present(filepath.Join(dir, "run", "postgres.password")) + present(filepath.Join(dir, checkpointInventoryName))
		})
		var want []string
		for _, name := range slices.Sorted(maps.Keys(checkpointCopySet(f))) {
			if f.material[name].mode.IsDir() {
				want = append([]string{path.Join(target, name) + "+-"}, want...)
			}
		}
		if f.cell.Checkpoint(t.Context(), checkpointOptions(f)) != nil || !slices.Equal(*synced, append(want, target+"+-")) {
			t.Fatal("the copy did not sync every planned directory deepest first, then its own, after its last file and before its inventory")
		}
	})
	t.Run("discard", func(t *testing.T) {
		f, r, synced := fixture(t, new(string), present)
		r.after = func(dir string) { checkpointRewrite(t, filepath.Join(dir, "pg", "base", "1", "1259")) }
		if err := f.cell.Checkpoint(t.Context(), checkpointOptions(f)); !checkpointSameError(err, "local_checkpoint_witness_mismatch") ||
			!slices.Contains(*synced, ".-") || slices.Contains(*synced, ".+") {
			t.Fatal("the discard did not sync recovery/ after removing its directory")
		}
	})
	for _, fail := range []string{"", target} {
		t.Run("unseal/sync-fails="+strconv.FormatBool(fail != ""), func(t *testing.T) {
			var unsealed map[string]retainedEntry
			armed := ""
			tree := func(dir string) map[string]retainedEntry { return retainedSnapshot(t, &Cell{cfg: Config{Root: dir}}) }
			f, _, synced := fixture(t, &armed, func(dir string) string {
				if present(dir) == "-" {
					return " gone"
				}
				return map[bool]string{true: " unsealed", false: " other"}[reflect.DeepEqual(unsealed, tree(dir))]
			})
			c, dir := f.cell, f.cell.path("recovery", target)
			if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil {
				t.Fatal("cannot prepare the checkpoint")
			}
			unsealed, *synced, armed = tree(dir), nil, fail
			delete(unsealed, checkpointWitnessName)
			err := c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: target})
			if fail == "" && (err != nil || !slices.Equal(*synced, []string{target + " unsealed", ". gone"})) {
				t.Fatal("deletion did not sync its directory once the witness was gone and before anything else was, then recovery/ once it was removed")
			}
			// A failed unseal sync is returned, closes the operation and leaves the data in place without its witness.
			if p, journalErr := c.readJournal(); fail != "" && (!checkpointSameError(err, "local cell checkpoint_delete: unseal failed") ||
				journalErr != nil || p.pending != nil || !reflect.DeepEqual(unsealed, tree(dir))) {
				t.Fatal("a failed unseal sync was not returned, or did not leave the data in place without its witness")
			}
		})
	}
}
