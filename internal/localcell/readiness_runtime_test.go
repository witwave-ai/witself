package localcell

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

func TestReadinessTimeoutUsesPersistedRuntimeSettings(t *testing.T) {
	c, m := unitCell(t)
	if got := c.readinessTimeout("server", m); got != c.cfg.Timeout+runtimeconfig.DefaultMigrationWait {
		t.Fatal("legacy manifest did not receive the local migration default")
	}
	m.MigrationWait, m.JobRestartPolicy = 3*time.Second, runtimeconfig.JobRestartOnFailure
	if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(Config{Root: c.cfg.Root, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := readManifest(c.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.readinessTimeout("server", stored); got != 4*time.Second {
		t.Fatal("server readiness omitted the persisted migration wait")
	}
	if got := reopened.readinessTimeout("worker", stored); got != time.Second {
		t.Fatal("migration wait extended worker readiness")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	bounded, release := context.WithTimeout(ctx, reopened.readinessTimeout("server", stored))
	defer release()
	wantDeadline, _ := ctx.Deadline()
	gotDeadline, _ := bounded.Deadline()
	if !gotDeadline.Equal(wantDeadline) {
		t.Fatal("composed readiness ignored the caller's earlier deadline")
	}
	reopened.cfg.MigrationWait = time.Duration(1<<63 - 1)
	if reopened.readinessTimeout("server", stored) <= 0 {
		t.Fatal("large migration wait overflowed the readiness deadline")
	}
}

func TestRuntimeSettingsValidationAndChildEnvironment(t *testing.T) {
	c, m := unitCell(t)
	for _, cfg := range []Config{{Root: c.cfg.Root, MigrationWait: -time.Second}, {Root: c.cfg.Root, JobRestartPolicy: "unknown"}} {
		if _, err := New(cfg); err == nil {
			t.Fatal("invalid lifecycle runtime settings were accepted")
		}
	}
	for _, invalid := range []Manifest{{MigrationWait: -time.Second}, {JobRestartPolicy: "unknown"}} {
		candidate := *m
		candidate.MigrationWait, candidate.JobRestartPolicy = invalid.MigrationWait, invalid.JobRestartPolicy
		if err := writeAtomic(c.path("manifest.json"), &candidate, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := readManifest(c.cfg.Root); err == nil {
			t.Fatal("invalid persisted runtime settings were accepted")
		}
	}
	if err := c.makePassword(); err != nil {
		t.Fatal(err)
	}
	m.MigrationWait, m.JobRestartPolicy = 7*time.Second, runtimeconfig.JobRestartOnFailure
	for _, component := range []string{"server", "worker"} {
		environment, err := c.childEnvironment(m, component)
		if err != nil {
			t.Fatal(err)
		}
		lookup := func(key string) (string, bool) {
			for _, entry := range environment {
				name, value, _ := strings.Cut(entry, "=")
				if name == key {
					return value, true
				}
			}
			return "", false
		}
		config, err := runtimeconfig.FromEnv(component, lookup, environment)
		if err != nil || config.MigrationWait != m.MigrationWait || config.JobRestartPolicy != m.JobRestartPolicy {
			t.Fatal("child did not receive the persisted runtime settings")
		}
	}
}

func TestReadinessDiagnosticOnlyAcceptsValueFreeLines(t *testing.T) {
	const safe = "witself-server: migration wait exceeded 2m0s during advisory lock"
	for _, line := range []string{
		"postgres://synthetic-private-value@invalid/fixture",
		"prefix " + safe,
		safe + " synthetic-private-value",
		"witself-server: migration wait exceeded synthetic-private-value during verify",
		"witself-server: migration wait exceeded 1s during synthetic-private-value",
		"witself-server: migration wait exceeded 0s during verify",
		"witself-worker: worker job restart limit exceeded: synthetic-private-value",
	} {
		if valueFreeDiagnostic("server", line) != "" || valueFreeDiagnostic("worker", line) != "" {
			t.Fatal("untrusted component output crossed the diagnostic allowlist")
		}
	}
	for _, line := range []string{safe, "2026/10/25 12:00:00 " + safe} {
		if valueFreeDiagnostic("server", line) != safe {
			t.Fatal("value-free migration diagnostic was lost")
		}
	}
	const worker = "witself-worker: worker job restart limit exceeded: avatar_style_rollout"
	if valueFreeDiagnostic("worker", worker) != worker {
		t.Fatal("value-free worker restart diagnostic was lost")
	}
	c, _ := unitCell(t)
	log := c.path("logs", "server.log")
	stale := safe + "\n"
	if err := writeAtomicBytes(log, []byte(stale+"untrusted-current-output\n"), nil); err != nil {
		t.Fatal(err)
	}
	if c.lastValueFreeDiagnostic("server", int64(len(stale))) != "" {
		t.Fatal("readiness cause included a previous launch's diagnostic")
	}
	if err := writeAtomicBytes(log, []byte(strings.Repeat("private-output", 4096)+"\n"+safe+"\n"), nil); err != nil {
		t.Fatal(err)
	}
	if got := c.lastValueFreeDiagnostic("server", 0); got != safe || len(got) > maxReadinessDiagnostic {
		t.Fatal("bounded tail did not retain exactly the safe diagnostic")
	}
}

func TestStatusRechecksProcessAfterReadyProbe(t *testing.T) {
	c, m := unitCell(t)
	identity, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		t.Fatal("cannot identify test process")
	}
	record := ProcessRecord{PID: os.Getpid(), StartTime: identity, Root: c.cfg.Root, Component: "server"}
	if err := writeAtomic(c.path("run", "server.json"), &record, nil); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Invalidate the fence while the probe is in flight. No PID is signaled.
		if err := removePrivateFile(c.path("run", "server.json")); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	_, port, _ := strings.Cut(strings.TrimPrefix(server.URL, "http://"), ":")
	m.Ports.ServerHealth, err = strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
		t.Fatal(err)
	}
	status, err := c.Status(t.Context())
	if err != nil || status.ServerReady {
		t.Fatal("readiness survived its process fence disappearing during the probe")
	}
}

func TestReadinessExitRetainsCompletedStatusAfterCancellation(t *testing.T) {
	c, _ := unitCell(t)
	c.beginChildObservation("server", 0)
	observation := c.children["server"]
	observation.pid = 42
	observation.status = &os.ProcessState{}
	close(observation.done)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 100 {
		err := c.childReadinessExit(ctx, "server", 42)
		if strings.Contains(err.Error(), "exit status unavailable") {
			t.Fatal("cancellation discarded an already-published exit status")
		}
	}
}
