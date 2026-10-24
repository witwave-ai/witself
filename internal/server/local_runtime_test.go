package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/version"
)

func localVersionResponse(t *testing.T, cfg Config) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	apiMux(cfg).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/version", nil))
	return w
}

func versionResponseSnapshot(schema int) string {
	return fmt.Sprintf("{\"schema_version\":\"witself.v0\",\"version\":%q,\"commit\":%q,\"date\":%q,\"account_evacuation_protocol\":3,\"account_provision_protocol\":1,\"store_schema_version\":%d,\"backup_validation_protocol\":2}\n", version.Version, version.Commit, version.Date, schema)
}

func TestHostedRuntimeConfigAndEndpointSnapshots(t *testing.T) {
	for _, name := range []string{"WITSELF_RUNTIME", "WITSELF_API_ADDR", "WITSELF_HEALTH_ADDR", "WITSELF_METRICS_ADDR"} {
		t.Setenv(name, "")
	}
	if !reflect.DeepEqual(ConfigFromEnv(), Config{APIAddr: ":8080", HealthAddr: ":8081", MetricsAddr: ":9090"}) {
		t.Fatal("hosted resolved config changed")
	}
	w := localVersionResponse(t, Config{AppliedSchemaVersion: func(context.Context) (int, error) {
		t.Error("hosted version must not read the database")
		return 0, errors.New("unexpected read")
	}})
	if w.Code != http.StatusOK || w.Body.String() != versionResponseSnapshot(store.SchemaVersion()) || w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("hosted version response bytes changed")
	}
	for _, tc := range []struct {
		name   string
		ready  func(context.Context) error
		status int
		body   string
	}{
		{"no reader", nil, http.StatusOK, "ok\n"},
		{"ready", func(context.Context) error { return nil }, http.StatusOK, "ok\n"},
		{"unready", func(context.Context) error { return errors.New("db down") }, http.StatusServiceUnavailable, "not ready: db down\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			healthMux(tc.ready).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Fatal("hosted readiness response bytes changed")
			}
		})
	}
}

func TestLocalVersionUsesAppliedSchema(t *testing.T) {
	for _, schema := range []int{store.SchemaVersion(), store.SchemaVersion() - 1, store.SchemaVersion() + 1, 0} {
		t.Run(fmt.Sprintf("schema_%d", schema), func(t *testing.T) {
			w := localVersionResponse(t, Config{LocalRuntime: true, AppliedSchemaVersion: func(ctx context.Context) (int, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("database version read must be bounded")
				}
				return schema, nil
			}})
			if w.Code != http.StatusOK || w.Body.String() != versionResponseSnapshot(schema) {
				t.Fatal("local version response did not report applied database version")
			}
		})
	}
}

func TestLocalVersionFailsClosedWithoutAppliedSchema(t *testing.T) {
	for _, reader := range []func(context.Context) (int, error){nil, func(context.Context) (int, error) { return 0, errors.New("private database diagnostic") }} {
		w := localVersionResponse(t, Config{LocalRuntime: true, AppliedSchemaVersion: reader})
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "database schema version unavailable") || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "store_schema_version") {
			t.Fatal("local version failed to refuse unavailable database version safely")
		}
	}
}

func TestLocalSweepConfinedToConfiguredDirectory(t *testing.T) {
	ambient := t.TempDir()
	t.Setenv("TMPDIR", ambient)
	t.Setenv("TMP", ambient)
	t.Setenv("TEMP", ambient)
	spoolDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(spoolDir, 0700); err != nil {
		t.Fatal(err)
	}
	name := "witself-self-export-123.tar.gz"
	for _, dir := range []string{ambient, spoolDir} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var log bytes.Buffer
	root, err := runtimeconfig.OpenSpoolRoot(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := SweepLocalArchiveSpools(root, spoolDir, &log); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(spoolDir, name)); !os.IsNotExist(err) {
		t.Fatal("local spool survived sweep")
	}
	if _, err := os.Stat(filepath.Join(ambient, name)); err != nil {
		t.Fatal("ambient temp spool was touched")
	}
	if !strings.Contains(log.String(), "removed=1") {
		t.Fatal("sweep did not report removed local spool")
	}
	if _, err := runtimeconfig.OpenSpoolRoot(os.TempDir()); err == nil {
		t.Fatal("local sweep accepted ambient temp directory")
	}
}

type localSpoolFixture struct {
	dir            string
	root, observer *os.Root
	replacement    string
	names          []string
}

func newLocalSpoolFixture(t *testing.T, replacement string) *localSpoolFixture {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "spool")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := runtimeconfig.OpenSpoolRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	observer, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	f := &localSpoolFixture{dir: dir, root: root, observer: observer}
	if replacement == "" {
		return f
	}
	if err := os.Rename(dir, filepath.Join(parent, "original")); err != nil {
		t.Fatal(err)
	}
	switch replacement {
	case "directory":
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		f.replacement = dir
	case "temp-symlink":
		// A disposable process temp directory makes a mistaken sweep harmless.
		ambient := t.TempDir()
		t.Setenv("TMPDIR", ambient)
		t.Setenv("TEMP", ambient)
		t.Setenv("TMP", ambient)
		if err := os.Symlink(os.TempDir(), dir); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("symlink creation unavailable: %v", err)
			}
			t.Fatal(err)
		}
		f.replacement = os.TempDir()
	default:
		t.Fatal("unknown replacement fixture")
	}
	return f
}

func (f *localSpoolFixture) requireFile(t *testing.T, value any) {
	t.Helper()
	file, ok := value.(*os.File)
	if !ok || file == nil {
		t.Error("archive callback did not receive a file")
		return
	}
	name := filepath.Base(file.Name())
	if !archiveSpoolName.MatchString(name) {
		t.Error("archive name shape changed")
	}
	info, err := file.Stat()
	if err != nil {
		t.Error(err)
		return
	}
	original, err := f.observer.Stat(name)
	if err != nil || !os.SameFile(info, original) {
		t.Error("archive escaped the original directory handle")
		return
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Error("archive is not private")
	}
	if f.replacement != "" {
		path := filepath.Join(f.replacement, name)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("archive created in replacement directory")
		}
		// Cleanup must also use the original root, preserving this foreign file.
		if err := os.WriteFile(path, []byte("replacement sentinel"), 0600); err != nil {
			t.Error(err)
		}
		f.names = append(f.names, name)
	}
}

func (f *localSpoolFixture) requireClean(t *testing.T) {
	t.Helper()
	entries, err := fs.ReadDir(f.observer.FS(), ".")
	if err != nil || len(entries) != 0 {
		t.Error("original archive spool was not cleaned up")
	}
	for _, name := range f.names {
		data, err := os.ReadFile(filepath.Join(f.replacement, name))
		if err != nil || string(data) != "replacement sentinel" {
			t.Error("cleanup touched replacement directory")
		}
	}
}

func TestLocalSelfExportSpoolDirectory(t *testing.T) {
	for _, replacement := range []string{"", "directory", "temp-symlink"} {
		t.Run("replacement_"+replacement, func(t *testing.T) {
			spool := newLocalSpoolFixture(t, replacement)
			called := false
			cfg := Config{LocalRuntime: true, SpoolDir: spool.dir, SpoolRoot: spool.root,
				Authenticate: func(context.Context, string) (string, string, string, bool, error) {
					return "opr_local", "acc_local", "active", true, nil
				},
				StreamAccountSelf: func(_ context.Context, _ string, w io.Writer) error {
					called = true
					spool.requireFile(t, w)
					_, err := io.WriteString(w, "local archive")
					return err
				},
			}
			r := httptest.NewRequest(http.MethodGet, "/v1/export", nil)
			r.Header.Set("Authorization", "Bearer local-fixture")
			w := httptest.NewRecorder()
			apiMux(cfg).ServeHTTP(w, r)
			if !called || w.Code != http.StatusOK || w.Body.String() != "local archive" {
				t.Fatal("local self export failed")
			}
			spool.requireClean(t)
		})
	}
}

func TestLocalImportSpoolDirectory(t *testing.T) {
	for _, replacement := range []string{"", "directory", "temp-symlink"} {
		for _, async := range []bool{false, true} {
			t.Run(fmt.Sprintf("replacement_%s/async_%t", replacement, async), func(t *testing.T) {
				f := newImportJobFixture(t)
				spool := newLocalSpoolFixture(t, replacement)
				f.cfg.LocalRuntime, f.cfg.SpoolDir, f.cfg.SpoolRoot = true, spool.dir, spool.root
				called := make(chan struct{}, 1)
				read := func(_ context.Context, r io.Reader) (ImportSummary, error) {
					spool.requireFile(t, r)
					called <- struct{}{}
					return importTestSummary(), nil
				}
				f.lease.importFn = read
				r := f.request("start-import-evacuation", "300")
				if !async {
					f.cfg.BeginAccountImport = nil
					f.cfg.ImportAccountArchive = func(ctx context.Context, _, _ string, r io.Reader) (ImportSummary, error) { return read(ctx, r) }
					r.URL.Path = "/v1/accounts/acc_job:import-evacuation"
				}
				w := f.serve(r)
				f.drained(t)
				if w.Code != http.StatusOK || len(called) != 1 {
					t.Fatal("local import did not consume spool")
				}
				spool.requireClean(t)
			})
		}
	}
}

func TestLocalBackupValidationSpoolDirectory(t *testing.T) {
	for _, replacement := range []string{"", "directory", "temp-symlink"} {
		for _, async := range []bool{false, true} {
			t.Run(fmt.Sprintf("replacement_%s/async_%t", replacement, async), func(t *testing.T) {
				f := newValidationJobFixture(t, 1)
				spool := newLocalSpoolFixture(t, replacement)
				f.cfg.LocalRuntime, f.cfg.SpoolDir, f.cfg.SpoolRoot = true, spool.dir, spool.root
				called := make(chan struct{}, 1)
				read := func(_ context.Context, r io.Reader) (ImportSummary, error) {
					spool.requireFile(t, r)
					called <- struct{}{}
					return ImportSummary{AccountID: "acc_job", Status: "active", SchemaVersion: 98, BackupID: "backup_20261003T000000Z"}, nil
				}
				f.validate = read
				r := f.start("acc_job", "validation_local", "300")
				if !async {
					f.cfg.BeginBackupValidation = nil
					f.cfg.ValidateAccountBackup = func(ctx context.Context, _, _ string, r io.Reader) (ImportSummary, error) { return read(ctx, r) }
					r.URL.Path = "/v1/accounts/acc_job:validate-backup"
				}
				w := f.serve(r)
				if w.Code != http.StatusOK || len(called) != 1 {
					t.Fatal("local validation did not consume spool")
				}
				spool.requireClean(t)
			})
		}
	}
}

func TestLocalSweepUsesOriginalDirectoryHandle(t *testing.T) {
	for _, replacement := range []string{"directory", "temp-symlink"} {
		t.Run(replacement, func(t *testing.T) {
			f := newLocalSpoolFixture(t, replacement)
			name := "witself-self-export-123.tar.gz"
			for _, name := range []string{name, "unrelated-file"} {
				if err := f.observer.WriteFile(name, []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.replacement, name), []byte("replacement"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Matching non-regular entries must survive.
			const directory = "witself-account-import-456.tar.gz"
			if err := f.observer.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			if err := SweepLocalArchiveSpools(f.root, f.dir, &log); err != nil {
				t.Fatal(err)
			}
			if _, err := f.observer.Stat(name); !os.IsNotExist(err) {
				t.Error("original spool survived handle sweep")
			}
			for _, name := range []string{name, "unrelated-file"} {
				data, err := os.ReadFile(filepath.Join(f.replacement, name))
				if err != nil || string(data) != "replacement" {
					t.Error("sweep touched replacement directory")
				}
			}
			if _, err := f.observer.Stat("unrelated-file"); err != nil {
				t.Error("sweep removed unmatched original file")
			}
			if _, err := f.observer.Stat(directory); err != nil {
				t.Error("sweep removed matching directory")
			}
			if !strings.Contains(log.String(), "removed=1") {
				t.Error("sweep count changed")
			}
		})
	}
}

type localSchemaOverride struct {
	*store.Store
	schema int
}

func (s localSchemaOverride) AppliedSchemaVersion(context.Context) (int, error) { return s.schema, nil }

func TestLocalReadinessAndVersionPostgres(t *testing.T) {
	st, _ := exportWriteDeadlineStore(t)
	ctx := context.Background()
	accountID, err := st.EnsureDefaultAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("mismatch_%t", mismatch), func(t *testing.T) {
			var source runtimeconfig.BootstrapStore = st
			wantVersion := store.SchemaVersion()
			wantReady := http.StatusOK
			if mismatch {
				wantVersion--
				wantReady = http.StatusServiceUnavailable
				source = localSchemaOverride{Store: st, schema: wantVersion}
			}
			cfg := Config{LocalRuntime: true, AppliedSchemaVersion: source.AppliedSchemaVersion,
				Ready: func(ctx context.Context) error {
					return runtimeconfig.CheckReady(ctx, source, store.SchemaVersion(), accountID)
				},
			}
			ready := httptest.NewRecorder()
			healthMux(cfg.Ready).ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if ready.Code != wantReady {
				t.Fatalf("ready status = %d, want %d", ready.Code, wantReady)
			}
			response := localVersionResponse(t, cfg)
			if response.Code != http.StatusOK || response.Body.String() != versionResponseSnapshot(wantVersion) {
				t.Fatal("version did not report real database reader result")
			}
		})
	}
}
