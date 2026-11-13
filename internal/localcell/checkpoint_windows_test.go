//go:build windows

package localcell

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/store"
)

// checkpointPGDATAFixture selects W1's restricted-token child; it is unset in
// every ordinary run.
const checkpointPGDATAFixture = "WITSELF_CHECKPOINT_PGDATA_FIXTURE"

// checkpointPGDATAObject is one synthetic PostgreSQL-created object below pg/
// by slash path; size -1 is a directory.
type checkpointPGDATAObject struct {
	name string
	size int
}

// checkpointPGDATA is W1's tree in creation order. base/1/2619 is far above the
// PGDATA record reader's 8,193-byte cap.
var checkpointPGDATA = []checkpointPGDATAObject{{"base", -1}, {"base/1", -1}, {"global", -1}, {"pg_notify", -1}, {"pg_tblspc", -1},
	{"PG_VERSION", 3}, {"base/1/1259", 24}, {"base/1/2619", 65536}, {"global/pg_control", 8192}}

func checkpointPGDATABytes(name string, size int) []byte {
	if name == "PG_VERSION" {
		return []byte("16\n")
	}
	return bytes.Repeat([]byte(name+" synthetic PGDATA bytes\n"), size)[:size]
}

// TestWindowsCheckpointPGDATAChild is W1's restricted-token child: it creates
// inherited-ACL PGDATA objects as PostgreSQL would, and never postmaster.pid.
func TestWindowsCheckpointPGDATAChild(t *testing.T) {
	if os.Getenv(checkpointPGDATAFixture) != "1" {
		return
	}
	pg := filepath.Join(os.Args[len(os.Args)-1], "pg")
	for _, object := range checkpointPGDATA {
		path := filepath.Join(pg, filepath.FromSlash(object.name))
		if object.size < 0 {
			if os.Mkdir(path, 0o700) != nil {
				t.Fatal("cannot create inherited PGDATA directory")
			}
			continue
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal("cannot create inherited PGDATA file")
		}
		_, err = file.Write(checkpointPGDATABytes(object.name, object.size))
		if errors.Join(err, file.Close()) != nil {
			t.Fatal("cannot write inherited PGDATA file")
		}
	}
}

// windowsCheckpointFixture is a cold, verified legacy cell at root. pg/ is the
// PGDATA boundary as Install prepares it, and a child launched exactly as
// windowsPostgresPrivateFixture launches its own creates every PGDATA object.
// The manifest and custody use the private helpers; the recorder answers the
// control and free boundaries.
func windowsCheckpointFixture(t *testing.T, root string) (*Cell, *checkpointRecorder, CheckpointOptions) {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	c, err := New(Config{Root: root, Timeout: time.Second})
	if err != nil || c.layout() != nil || c.preparePostgresDirectory() != nil {
		t.Fatal("cannot prepare the PGDATA checkpoint fixture")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate the PGDATA checkpoint child")
	}
	cmd := exec.CommandContext(t.Context(), self, "-test.run=^TestWindowsCheckpointPGDATAChild$", "--", c.cfg.Root)
	cmd.Dir = c.cfg.Root
	if cmd.Env, err = c.postgresEnvironment(); err != nil {
		t.Fatal(err)
	}
	cmd.Env = appendEnvironment(cmd.Env, checkpointPGDATAFixture+"=1")
	release, err := configurePostgresCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := cmd.Run(); err != nil {
		t.Fatal("restricted PGDATA checkpoint child failed")
	}
	m := &Manifest{LayoutVersion: LayoutVersion, InstallationID: "lc_test", CreatedAt: time.Now().UTC(), Database: localDatabaseName,
		Role: "localcell", Installed: true, AccountID: "acc_aaaaaaaaaaaaaaaa", LastVerifiedSchemaVersion: store.SchemaVersion(),
		Ports: Ports{Postgres: 21101, ServerAPI: 21102, ServerHealth: 21103, ServerMetrics: 21104, WorkerHealth: 21105, WorkerMetrics: 21106}}
	if writeAtomic(c.path("manifest.json"), m, nil) != nil || c.makePassword() != nil {
		t.Fatal("cannot publish the PGDATA checkpoint fixture custody")
	}
	held, err := c.own(false) // the stable locks exist, as in any used cell
	if err != nil {
		t.Fatal("cannot prepare the PGDATA checkpoint fixture locks")
	}
	_ = held.Close()
	c.cfg.Binaries.PostgresDir = t.TempDir() // no pg_controldata: the recorder answers
	r := &checkpointRecorder{calls: map[string]int{}, readings: []pgControlData{checkpointSyntheticControl}, after: func(string) {}, before: func() {},
		free: func(string) (uint64, uint64, error) { return 1 << 50, 1 << 51, nil }, nextID: checkpointNewID}
	r.install(c)
	return c, r, CheckpointOptions{InstallationID: m.InstallationID}
}

// windowsCheckpointListed is Inspect's checkpoint view, id to completeness.
func windowsCheckpointListed(t *testing.T, c *Cell) map[string]bool {
	t.Helper()
	inspection, err := c.Inspect(t.Context())
	if err != nil {
		t.Fatal("inspect failed after a checkpoint operation")
	}
	listed := map[string]bool{}
	for _, summary := range inspection.Checkpoints {
		listed[summary.ID] = summary.Complete
	}
	return listed
}

// TestWindowsCheckpointPrivateCopy (W1) copies real inherited-ACL PGDATA
// objects into strict-private, byte-equal copies without changing the sources;
// a junction is never followed and a destination of 260 UTF-16 units refuses
// before any write. A new identifier comes from Inspect before and after the
// call, never from its error. Paths and bytes are compared, never printed.
func TestWindowsCheckpointPrivateCopy(t *testing.T) {
	c, _, options := windowsCheckpointFixture(t, testPrivateRoot(t))
	before := windowsCheckpointListed(t, c)
	err := c.Checkpoint(t.Context(), options)
	after := windowsCheckpointListed(t, c)
	var created []string
	for id := range after {
		if _, existed := before[id]; !existed {
			created = append(created, id)
		}
	}
	// Row 1: the call succeeds and Inspect lists exactly one new, complete checkpoint.
	if err != nil || len(before) != 0 || len(created) != 1 || !after[created[0]] {
		t.Fatalf("checkpoint of inherited PGDATA did not complete: %v", err)
	}
	dir := c.path("recovery", created[0])
	// Row 2: every object under recovery/ is strict-private and both validators pass.
	objects := 0
	if filepath.WalkDir(c.recoveryDir(), func(path string, _ fs.DirEntry, err error) error {
		if err == nil {
			assertWindowsPrivate(t, path)
			objects++
		}
		return err
	}) != nil || objects != len(checkpointPGDATA)+8 {
		t.Fatal("the checkpoint tree is not exactly the copy set")
	}
	if c.validateRecovery() != nil || c.validateRoot(false) != nil {
		t.Fatal("the checkpoint or the root failed privacy validation")
	}
	// Row 3: every copy equals its source; each PGDATA source holds the child's bytes.
	for _, name := range []string{"manifest.json", "run/postgres.password"} {
		source, sourceErr := os.ReadFile(c.path(filepath.FromSlash(name)))
		copied, copyErr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if sourceErr != nil || copyErr != nil || !bytes.Equal(source, copied) {
			t.Fatal("a copied control file differs from its source")
		}
	}
	for _, object := range checkpointPGDATA {
		if object.size < 0 {
			continue
		}
		source, sourceErr := os.ReadFile(c.path("pg", filepath.FromSlash(object.name)))
		copied, copyErr := os.ReadFile(filepath.Join(dir, "pg", filepath.FromSlash(object.name)))
		if sourceErr != nil || copyErr != nil || !bytes.Equal(source, copied) || !bytes.Equal(source, checkpointPGDATABytes(object.name, object.size)) {
			t.Fatal("a copied PGDATA file differs from its source")
		}
	}
	// Row 4: pg/ and every source are still inherited PGDATA objects, which the
	// strict private rule refuses: only the copies were re-secured.
	for _, object := range append([]checkpointPGDATAObject{{"", -1}}, checkpointPGDATA...) {
		path, directory := filepath.Join(c.path("pg"), filepath.FromSlash(object.name)), object.size < 0
		if runtimeconfig.WindowsPrivatePostgresValidate(c.path("pg"), path, directory) != nil || validatePrivate(path, directory) == nil {
			t.Fatal("a PGDATA source lost its inherited PostgreSQL privacy")
		}
	}
	// Row 5: the PGDATA opener itself reads the whole 64 KiB file.
	file, err := runtimeconfig.WindowsPrivatePostgresOpen(c.path("pg"), c.path("pg", "base", "1", "2619"))
	if err != nil {
		t.Fatal("the PGDATA opener refused an inherited source")
	}
	data, err := io.ReadAll(file)
	if errors.Join(err, file.Close()) != nil || !bytes.Equal(data, checkpointPGDATABytes("base/1/2619", 65536)) {
		t.Fatal("the PGDATA opener did not read the whole 64 KiB source")
	}
	// Row 6: a junction in pg/ is refused with zero writes. Every own() walk
	// refuses it first, exactly as validateRoot does, so the call returns that
	// refusal; the plan walk behind it refuses the junction as source unsupported.
	listed := windowsCheckpointListed(t, c)
	junction := c.path("pg", "pg_junction")
	createTestJunction(t, junction, testPrivateRoot(t))
	snapshot, refusal := checkpointTreeState(t, c), c.validateRoot(false)
	if err := c.Checkpoint(t.Context(), options); refusal == nil || err == nil || err.Error() != refusal.Error() {
		t.Fatal("a PGDATA junction was not refused before the checkpoint admission")
	}
	checkpointUnchanged(t, c, snapshot)
	m, err := readManifest(c.cfg.Root)
	if err != nil {
		t.Fatal("cannot read the PGDATA checkpoint fixture manifest")
	}
	if _, err := c.planCheckpoint(m); !checkpointSameError(err, "local_checkpoint_source_unsupported") {
		t.Fatal("the checkpoint plan walk did not refuse a PGDATA junction")
	}
	if os.Remove(junction) != nil || !maps.Equal(listed, windowsCheckpointListed(t, c)) {
		t.Fatal("a refused checkpoint changed Inspect's checkpoints")
	}
	// Row 7: the directory rule decides the refusal for this tree. The 207-unit
	// root's 260-unit file arithmetic (root + 53) is asserted as a property of
	// the root, not the deciding rule. Every live path stays below 260 (the
	// longest transient, a run/ temporary, is root + 38).
	units := func(path string) int { return len(utf16.Encode([]rune(path))) }
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil || units(parent) > 205 {
		t.Fatal("cannot place the long checkpoint root")
	}
	root := filepath.Join(parent, strings.Repeat("r", 206-units(parent)))
	if privateDir(root, true) != nil {
		t.Fatal("cannot create the long checkpoint root")
	}
	long, r, options := windowsCheckpointFixture(t, root)
	longest := 0
	if filepath.WalkDir(long.cfg.Root, func(path string, _ fs.DirEntry, err error) error {
		longest = max(longest, units(path))
		return err
	}) != nil || units(long.cfg.Root) != 207 || longest >= 260 ||
		units(filepath.Join(long.recoveryDir(), "ckpt_"+strings.Repeat("a", 16), "run", "postgres.password")) != 260 {
		t.Fatalf("long checkpoint root has %d units and a %d-unit live path", units(long.cfg.Root), longest)
	}
	listed, snapshot = windowsCheckpointListed(t, long), checkpointTreeState(t, long)
	if err := long.Checkpoint(t.Context(), options); !checkpointSameError(err, "local_checkpoint_source_unsupported") {
		t.Fatal("a checkpoint destination of 260 UTF-16 units was not refused as source unsupported")
	}
	checkpointUnchanged(t, long, snapshot)
	if r.calls["control"] != 0 || r.calls["free"] != 0 || len(listed) != 0 || len(windowsCheckpointListed(t, long)) != 0 {
		t.Fatal("the long-root refusal reached a boundary or listed a checkpoint")
	}
}
