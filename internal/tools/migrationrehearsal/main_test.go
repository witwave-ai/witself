package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestTimingOutput(t *testing.T) {
	var out bytes.Buffer
	if err := writeTiming(&out, migrationFile{Name: "0098_index_account_export_order.sql"}, 1234567*time.Microsecond); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "FILE 0098_index_account_export_order.sql 1234\n"; got != want {
		t.Fatalf("unexpected timing protocol: %q", got)
	}
}

func TestVersionRange(t *testing.T) {
	files := []migrationFile{{Version: 1}, {Version: 3}, {Version: 4}}
	cases := []struct {
		name            string
		current, target int64
		want            []int64
		fail            bool
	}{
		{"empty to latest", 0, 0, []int64{1, 3, 4}, false},
		{"bounded", 1, 3, []int64{3}, false},
		{"resume latest", 3, 0, []int64{4}, false},
		{"already applied", 4, 4, nil, false},
		{"downgrade", 4, 3, nil, true},
		{"unknown target", 1, 2, nil, true},
		{"unknown current", 2, 4, nil, true},
		{"ahead", 1, 5, nil, true},
		{"negative", -1, 4, nil, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := versionRange(files, tt.current, tt.target)
			if (err != nil) != tt.fail {
				t.Fatalf("error presence=%v, want=%v", err != nil, tt.fail)
			}
			if tt.fail {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("file count=%d, want=%d", len(got), len(tt.want))
			}
			for i, file := range got {
				if file.Version != tt.want[i] {
					t.Fatal("wrong version order")
				}
			}
		})
	}
}

func TestLocalDSN(t *testing.T) {
	for _, dsn := range []string{
		"postgres://postgres:test@127.0.0.1:55432/rehearsal?sslmode=disable",
		"postgresql://postgres@[::1]:55432/rehearsal",
		"postgres://postgres@127.0.0.2:55432/rehearsal",
	} {
		if _, err := localConfig(dsn); err != nil {
			t.Fatal("literal loopback DSN rejected")
		}
	}
	for name, dsn := range map[string]string{
		"remote":               "postgres://postgres@192.0.2.2:5432/rehearsal",
		"dns":                  "postgres://postgres@localhost:5432/rehearsal",
		"reserved":             "postgres://postgres@127.0.0.1:5599/rehearsal",
		"port unspecified":     "postgres://postgres@127.0.0.1/rehearsal",
		"port zero":            "postgres://postgres@127.0.0.1:0/rehearsal",
		"socket":               "postgres:///rehearsal?host=/tmp",
		"query host":           "postgres://postgres@127.0.0.1:5432/rehearsal?host=192.0.2.2",
		"query port":           "postgres://postgres@127.0.0.1:5432/rehearsal?port=5599",
		"multi host":           "postgres://postgres@127.0.0.1:5432,192.0.2.2:5432/rehearsal",
		"all local multi host": "postgres://postgres@127.0.0.1:5432,127.0.0.1:5433/rehearsal",
		"service":              "postgres://postgres@127.0.0.1:5432/rehearsal?servicefile=/tmp/ignored",
		"passfile":             "postgres://postgres@127.0.0.1:5432/rehearsal?passfile=/tmp/ignored",
		"options":              "postgres://postgres@127.0.0.1:5432/rehearsal?options=-c+search_path=other",
		"tls":                  "postgres://postgres@127.0.0.1:5432/rehearsal?sslmode=require",
		"repeated query":       "postgres://postgres@127.0.0.1:5432/rehearsal?sslmode=disable&sslmode=require",
		"keyword":              "host=127.0.0.1 port=5432 dbname=rehearsal user=postgres",
		"missing database":     "postgres://postgres@127.0.0.1:5432/",
		"missing user":         "postgres://127.0.0.1:5432/rehearsal",
		"missing":              "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := localConfig(dsn); err == nil {
				t.Fatal("unsafe DSN accepted")
			}
		})
	}
}

func TestLocalDSNIgnoresAmbientRoutingAndFiles(t *testing.T) {
	t.Setenv("PGHOST", "192.0.2.2")
	t.Setenv("PGPORT", "5599")
	t.Setenv("PGSERVICE", "not_a_service")
	t.Setenv("PGSERVICEFILE", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("PGPASSWORD", "ambient_not_used")
	t.Setenv("PGSSLMODE", "require")
	t.Setenv("PGSSLKEY", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("PGOPTIONS", "-c search_path=other")
	config, err := localConfig("postgres://postgres@127.0.0.1:55432/rehearsal")
	if err != nil {
		t.Fatal("explicit local config rejected under ambient libpq settings")
	}
	if config.Password != "" || config.Host != "127.0.0.1" || config.Port != 55432 || config.TLSConfig != nil || len(config.Fallbacks) != 0 {
		t.Fatal("ambient settings escaped canonical config")
	}
	if len(config.RuntimeParams) != 1 || config.RuntimeParams["application_name"] != "witself_rehearsal_helper" {
		t.Fatal("ambient runtime options survived")
	}
}

func TestPasswordFileRejectsUnsafeInputs(t *testing.T) {
	const diagnostic = "migration rehearsal helper failed: helper_error password_file"
	for _, tt := range []struct {
		name string
		mode os.FileMode
	}{
		{"public", 0644},
		{"group-readable", 0640},
		{"other-readable", 0604},
		{"symlink", 0},
		{"oversize", 0},
		{"directory", 0},
		{"missing", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "password")
			switch tt.name {
			case "public", "group-readable", "other-readable":
				if err := os.WriteFile(filename, []byte("private contents"), tt.mode); err != nil {
					t.Fatal("create test password failed")
				}
				if err := os.Chmod(filename, tt.mode); err != nil {
					t.Fatal("set public password permissions failed")
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, []byte("private contents"), 0600); err != nil {
					t.Fatal("create test password failed")
				}
				if err := os.Symlink(target, filename); err != nil {
					t.Fatal("create test symlink failed")
				}
			case "oversize":
				if err := os.WriteFile(filename, bytes.Repeat([]byte("x"), 257), 0600); err != nil {
					t.Fatal("create oversized password failed")
				}
			case "directory":
				if err := os.Mkdir(filename, 0700); err != nil {
					t.Fatal("create test directory failed")
				}
			case "missing":
			}
			if _, err := passwordConfig("postgres://postgres@127.0.0.1:55432/rehearsal", filename); err == nil || safeDiagnostic(err) != diagnostic {
				t.Fatal("unsafe password file accepted before opening a connection")
			}
			var out bytes.Buffer
			err := run(context.Background(), []string{"--mode", "inspect", "--dsn", "postgres://postgres@127.0.0.1:55432/rehearsal", "--password-file", filename}, &out)
			if err == nil || safeDiagnostic(err) != diagnostic || out.Len() != 0 {
				t.Fatal("unsafe password file did not fail with the fixed value-free diagnostic")
			}
		})
	}
	for _, mode := range []string{"migrate", "observe", "append", "long-tx", "verify", "inspect", "seed", "measure"} {
		err := run(context.Background(), []string{"--mode", mode, "--dsn", "postgres://postgres@127.0.0.1:55432/rehearsal"}, io.Discard)
		if err == nil || safeDiagnostic(err) != diagnostic {
			t.Fatal("DSN operation accepted a missing password file")
		}
	}
}

func TestPasswordFileConfig(t *testing.T) {
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("PGPASSWORD", "ambient_not_used")
	for _, password := range []string{"private:p@ss/?#&", "private\n", "private\n\n", strings.Repeat("x", 256)} {
		filename := filepath.Join(t.TempDir(), "password")
		if err := os.WriteFile(filename, []byte(password), 0600); err != nil {
			t.Fatal("create test password failed")
		}
		config, err := passwordConfig("postgres://postgres@127.0.0.1:55432/rehearsal", filename)
		if err != nil {
			t.Fatal("private password file rejected")
		}
		if config.Password != strings.TrimSuffix(password, "\n") {
			t.Fatal("file password was not installed in the connection config")
		}
		u, err := url.Parse(config.ConnString())
		if err != nil || u.Query().Get("passfile") != "/dev/null" {
			t.Fatal("connection config enabled ambient password-file discovery")
		}
	}
}

func TestMigrationFilesUsesOnlyUpLocks(t *testing.T) {
	source := fstest.MapFS{
		"migrations/0003_lock.sql":     {Data: []byte("-- +goose Up\nLOCK TABLE a IN SHARE MODE;\n-- +goose Down\nSELECT 1;")},
		"migrations/0004_optional.sql": {Data: []byte("-- +goose Up\nLOCK a IN SHARE MODE;")},
		"migrations/0001_no_lock.sql":  {Data: []byte("-- +goose Up\nSELECT 1;\n-- +goose Down\nLOCK TABLE a;")},
	}
	got, err := migrationFiles(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got[2].ExplicitLocks || got[0].Version != 1 || got[0].ExplicitLocks || got[1].Version != 3 || !got[1].ExplicitLocks {
		t.Fatal("migration inventory order/lock classification incorrect")
	}
}

func TestPinnedImageUsesExistingValuesReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "values.yaml")
	digest := strings.Repeat("a", 64)
	body := "apps:\n  civoPostgres:\n    image:\n      registry: registry.example\n      repository: mirror/postgresql\n      digest: sha256:" + digest + "\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--mode", "image", "--values", path}, &out); err != nil {
		t.Fatal("pinned image read failed")
	}
	var result struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal("image protocol invalid")
	}
	if result.Image != "registry.example/mirror/postgresql@sha256:"+digest {
		t.Fatal("wrong selected image")
	}
	if err := os.WriteFile(path, []byte("apps:\n  civoPostgres:\n    image:\n      registry: registry.example\n      repository: mirror/postgresql\n      tag: latest\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(context.Background(), []string{"--mode", "image", "--values", path}, &out); err == nil {
		t.Fatal("unpinned image accepted")
	}
}

func TestTinyRehearsalIntegration(t *testing.T) {
	if os.Getenv("WITSELF_TEST_REHEARSAL") != "1" {
		t.Skip("requires explicit disposable Docker rehearsal opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	image := os.Getenv("WITSELF_REHEARSAL_IMAGE")
	if image == "" {
		image = "postgres:18-alpine3.23"
	}
	output := t.TempDir()
	root := filepath.Join("..", "..", "..")
	cmd := exec.CommandContext(ctx, "bash", "scripts/rehearse-store-migrations.sh", "--from", "97", "--to", "98", "--synthetic", "--entries", "2000", "--transcripts", "20", "--mutations", "200", "--run-inputs", "100", "--other-rows", "30", "--size-profile", "smoke", "--scenario", "quiet", "--image", image, "--out", output, "--verify-sql", syntheticIdentifierWidthVerification(t))
	cmd.Dir = root
	// Allow the wrapper's EXIT trap to remove its container on test timeout.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second

	runErr := cmd.Run()
	raw, err := os.ReadFile(filepath.Join(output, "report.json"))
	if err != nil {
		t.Fatal("report.json missing")
	}
	var report struct {
		Status           string `json:"status"`
		ServingReadiness bool   `json:"serving_readiness"`
		From             int    `json:"from"`
		To               int    `json:"to"`
		Failure          string `json:"failure"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal("report.json invalid")
	}
	if runErr != nil {
		if regexp.MustCompile(`^[a-z_A-Z0-9]{1,128}$`).MatchString(report.Failure) {
			t.Fatalf("tiny rehearsal failed: %s", report.Failure)
		}
		t.Fatal("tiny rehearsal failed without a static diagnostic")
	}
	if report.Status != "go" || report.ServingReadiness || report.From != 97 || report.To != 98 {
		t.Fatal("tiny rehearsal report does not prove the requested smoke transition")
	}

}

// The recorded tiny run took about 192 seconds. Twelve minutes rounds up beyond
// three times that baseline and leaves cancellation time within go test's 30m.
const tinyRestoreRehearsalTimeout = 12 * time.Minute

func rehearsalCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	return cmd
}

type rehearsalDocker struct {
	env []string
}

func newRehearsalDocker(t *testing.T) rehearsalDocker {
	t.Helper()
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "DOCKER_CONTEXT", "DOCKER_HOST", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_API_VERSION", "DOCKER_CONFIG":
			continue
		}
		env = append(env, entry)
	}
	return rehearsalDocker{env: append(env, "DOCKER_CONFIG="+t.TempDir())}
}

func (d rehearsalDocker) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := rehearsalCommand(ctx, "docker", append([]string{"--host", "unix:///var/run/docker.sock"}, args...)...)
	cmd.Env = d.env
	return cmd
}

func (d rehearsalDocker) resources(ctx context.Context, t *testing.T) ([]string, []string) {
	t.Helper()
	containers, err := d.command(ctx, "ps", "-a", "--filter", "name=witself-rehearsal-", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatal("cannot snapshot rehearsal containers")
	}
	volumes, err := d.command(ctx, "volume", "ls", "--filter", "name=witself-rehearsal-", "--format", "{{.Name}}").Output()
	if err != nil {
		t.Fatal("cannot snapshot rehearsal volumes")
	}
	containerSet, volumeSet := strings.Fields(string(containers)), strings.Fields(string(volumes))
	slices.Sort(containerSet)
	slices.Sort(volumeSet)
	return containerSet, volumeSet
}

func TestTinyRestoreRehearsalIntegration(t *testing.T) {
	if os.Getenv("WITSELF_TEST_REHEARSAL") != "1" {
		t.Skip("requires explicit disposable Docker rehearsal opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), tinyRestoreRehearsalTimeout)
	t.Cleanup(cancel)
	docker := newRehearsalDocker(t)
	beforeContainers, beforeVolumes := docker.resources(ctx, t)
	if len(beforeContainers) != 0 || len(beforeVolumes) != 0 {
		t.Fatal("pre-existing rehearsal resources; refusing the restore integration test")
	}
	image := os.Getenv("WITSELF_REHEARSAL_IMAGE")
	if image == "" {
		image = "postgres:18-alpine3.23"
	}
	var nameRandom [8]byte
	if _, err := rand.Read(nameRandom[:]); err != nil {
		t.Fatal("cannot generate source container name")
	}
	source := "witself-rehearsal-src-" + hex.EncodeToString(nameRandom[:])
	// Cleanup must still reach the exact source after the workload context expires.
	// It never removes the wrapper's container or volume, or resources by prefix.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cleanupCancel()
		_ = docker.command(cleanupCtx, "rm", "-f", "-v", source).Run()
	})
	private := t.TempDir()
	passwordFile := filepath.Join(private, "pg-password")
	var passwordRandom [32]byte
	if _, err := rand.Read(passwordRandom[:]); err != nil {
		t.Fatal("cannot generate source password")
	}
	if err := os.WriteFile(passwordFile, []byte(hex.EncodeToString(passwordRandom[:])), 0600); err != nil {
		t.Fatal("cannot create private source password file")
	}
	const database = "rehearsal_source"
	if err := docker.command(ctx, "run", "-d", "--pull=never", "--name", source,
		"--memory", "1536m", "--memory-swap", "1536m", "-p", "127.0.0.1::5432",
		"--mount", "type=bind,source="+passwordFile+",target=/run/rehearsal-password,readonly",
		"-e", "POSTGRES_PASSWORD_FILE=/run/rehearsal-password", "-e", "POSTGRES_DB="+database,
		"-e", "POSTGRES_INITDB_ARGS=--auth-local=trust --auth-host=scram-sha-256", image).Run(); err != nil {
		t.Fatal("cannot start isolated source container without pulling")
	}
	for {
		if err := docker.command(ctx, "exec", source, "pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-d", database).Run(); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("source PostgreSQL readiness timed out")
		case <-time.After(100 * time.Millisecond):
		}
	}
	rawPorts, err := docker.command(ctx, "inspect", "--format", `{{json (index .NetworkSettings.Ports "5432/tcp")}}`, source).Output()
	if err != nil {
		t.Fatal("cannot inspect isolated source port")
	}
	var ports []struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal(rawPorts, &ports); err != nil || len(ports) != 1 || ports[0].HostIP != "127.0.0.1" {
		t.Fatal("source PostgreSQL is not published only on loopback")
	}
	port, err := strconv.ParseUint(ports[0].HostPort, 10, 16)
	if err != nil || port == 0 || port == 5599 {
		t.Fatal("source PostgreSQL has an invalid or reserved port")
	}
	dsn := "postgres://postgres@" + net.JoinHostPort("127.0.0.1", ports[0].HostPort) + "/" + database
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal("cannot resolve rehearsal repository")
	}
	helper := filepath.Join(private, "migrationrehearsal")
	goBinary := ""
	if goRoot := os.Getenv("GOROOT"); goRoot != "" {
		goBinary = filepath.Join(goRoot, "bin", "go")
	} else {
		goBinary, err = exec.LookPath("go")
		if err != nil {
			t.Fatal("cannot locate source helper Go compiler")
		}
	}
	build := rehearsalCommand(ctx, goBinary, "build", "-o", helper, "./internal/tools/migrationrehearsal")
	build.Dir = root
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	if err := build.Run(); err != nil {
		t.Fatal("cannot build source migration helper")
	}
	if err := rehearsalCommand(ctx, helper, "--mode", "migrate", "--dsn", dsn, "--password-file", passwordFile, "--to", "97").Run(); err != nil {
		t.Fatal("source migration to 97 failed")
	}
	if err := rehearsalCommand(ctx, helper, "--mode", "seed", "--dsn", dsn, "--password-file", passwordFile,
		"--entries", "2000", "--transcripts", "20", "--mutations", "200", "--inputs", "100", "--other-rows", "30").Run(); err != nil {
		t.Fatal("source tiny seed failed")
	}
	archive, err := os.OpenFile(filepath.Join(private, "tiny.dump"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("cannot create private synthetic dump")
	}
	dump := docker.command(ctx, "exec", source, "pg_dump", "-Fc", "-U", "postgres", database)
	dump.Stdout = archive
	dumpErr := dump.Run()
	closeErr := archive.Close()
	if dumpErr != nil || closeErr != nil {
		t.Fatal("source custom-format dump failed")
	}
	if err := docker.command(ctx, "rm", "-f", "-v", source).Run(); err != nil {
		t.Fatal("cannot remove source container and its data")
	}
	input, err := os.Open(archive.Name())
	if err != nil {
		t.Fatal("cannot open private synthetic dump")
	}
	defer func() { _ = input.Close() }()
	output := t.TempDir()
	cmd := rehearsalCommand(ctx, "bash", "scripts/rehearse-store-migrations.sh", "--from", "97", "--to", "98", "--restore", "-", "--size-profile", "restore", "--scenario", "quiet", "--image", image, "--out", output)
	cmd.Dir = root
	cmd.Stdin = input
	runErr := cmd.Run()
	raw, err := os.ReadFile(filepath.Join(output, "report.json"))
	if err != nil {
		t.Fatal("restore report.json missing")
	}
	var report struct {
		Status            string `json:"status"`
		DataSource        string `json:"data_source"`
		RestoredDataReady bool   `json:"restored_data_ready"`
		ServingReadiness  bool   `json:"serving_readiness"`
		From              int    `json:"from"`
		To                int    `json:"to"`
		Failure           string `json:"failure"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal("restore report.json invalid")
	}
	if runErr != nil {
		if regexp.MustCompile(`^[a-z_A-Z0-9]{1,128}$`).MatchString(report.Failure) {
			t.Fatalf("tiny restore rehearsal failed: %s", report.Failure)
		}
		t.Fatal("tiny restore rehearsal failed without a static diagnostic")
	}
	if report.Status != "go" || report.DataSource != "restore" || !report.RestoredDataReady || report.ServingReadiness || report.From != 97 || report.To != 98 {
		t.Fatal("tiny restore report does not prove the restored transition")
	}
	afterContainers, afterVolumes := docker.resources(ctx, t)
	if !slices.Equal(beforeContainers, afterContainers) || !slices.Equal(beforeVolumes, afterVolumes) || slices.Contains(afterContainers, source) {
		t.Fatal("restore rehearsal left Docker resources behind")
	}
}

func TestDiagnosticExcludesServerValues(t *testing.T) {
	diagnostic := safeDiagnostic(&pgconn.PgError{Code: "23503", Message: "private row", Detail: "private details"})
	if diagnostic != "migration rehearsal helper failed: helper_error sqlstate=23503" {
		t.Fatal("diagnostic exposed a server value")
	}
	diagnostic = safeDiagnostic(&pgconn.PgError{Code: "unsafe-code", Message: "private row"})
	if diagnostic != "migration rehearsal helper failed: helper_error" {
		t.Fatal("invalid SQLSTATE escaped diagnostic")
	}
}

func TestSyntheticDiagnosticRetainsOnlyPhaseAndSQLState(t *testing.T) {
	diagnostic := safeDiagnostic(errors.New("synthetic generation failed: transcript entries (SQLSTATE 23503)"))
	if diagnostic != "migration rehearsal helper failed: helper_error seed_phase=transcript_entries sqlstate=23503" {
		t.Fatal("static seed diagnostic missing")
	}
	diagnostic = safeDiagnostic(errors.New("synthetic generation failed: private@example.test"))
	if diagnostic != "migration rehearsal helper failed: helper_error" {
		t.Fatal("untrusted seed error escaped")
	}
}
