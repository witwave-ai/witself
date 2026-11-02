//go:build darwin || linux

package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// Arm cancellation only after the artifact probes, then let Run cancel itself
// at its next idle select. A real ticker may run during the probes; cancelling
// directly from the test could legitimately produce a tick-phase receipt.
// Only Run receives this wrapper. Control requests retain the base context.
type serviceRestartIdleContext struct {
	context.Context
	cancel context.CancelFunc
	armed  atomic.Bool
}

func (c *serviceRestartIdleContext) Done() <-chan struct{} {
	if c.armed.Load() {
		c.cancel()
	}
	return c.Context.Done()
}

// Both generations use Run's production hooks. Only Darwin's boot reader is
// overlaid in a child test process when the sandbox denies kern.boottime; the
// child still runs both generations consecutively in one process on one root.
func TestServiceRunRestartAfterIdleShutdown(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	boot, err := serviceBootID()
	if err != nil {
		if runtime.GOOS == "darwin" {
			serviceRestartWithDarwinBootReader(t)
			return
		}
		t.Fatal("restart fixture could not read its boot identity")
	}
	prepared := time.Now()
	c := shortServiceTestCell(t)
	if c.SetServiceDesiredState(t.Context(), "stopped") != nil {
		t.Fatal("restart fixture could not establish stopped intent")
	}
	d, err := c.ServiceDescriptor()
	if err != nil {
		t.Fatal("restart fixture could not read its descriptor")
	}
	// An idle, freshly prepared installation does not create supervisor.json.
	// Seed a valid prior record to exercise the existing-record load on both
	// starts, without introducing a failed start or launching any components.
	record := supervisorRecord{Version: 1, RepairRevision: d.RepairRevision}
	if writeAtomic(c.path("service", "supervisor.json"), record, nil) != nil {
		t.Fatal("restart fixture could not seed its supervisor record")
	}
	supervisorBefore, err := os.ReadFile(c.path("service", "supervisor.json"))
	if err != nil {
		t.Fatal("restart fixture could not inspect its supervisor record")
	}
	// Scale the allowance with this fixture's own synchronous disk work. There
	// are no PostgreSQL processes, sleeps in hooks, or native manager calls.
	allowance := max(2*time.Second, 10*time.Since(prepared))
	var previousLease, stableLock os.FileInfo
	var previousJournal int64
	for generation := 0; generation < 2; generation++ {
		ctx, cancel := context.WithTimeout(t.Context(), allowance)
		runCtx := &serviceRestartIdleContext{Context: ctx, cancel: cancel}
		done := make(chan error, 1)
		stderr, err := os.CreateTemp(t.TempDir(), "restart-stderr")
		if err != nil {
			cancel()
			t.Fatal("restart fixture could not capture the shutdown receipt")
		}
		originalStderr := os.Stderr
		os.Stderr = stderr
		finished := false
		cleanup := func() {
			cancel()
			if !finished {
				select {
				case <-done:
				case <-time.After(allowance):
					t.Error("restart fixture host did not finish cleanup")
				}
			}
			os.Stderr = originalStderr
			_ = stderr.Close()
		}
		t.Cleanup(cleanup)
		go func() { done <- c.Run(runCtx, SupervisorPolicy{ShutdownTimeout: allowance}) }()
		for {
			requestCtx, requestCancel := context.WithTimeout(ctx, allowance/4)
			status, statusErr := c.Control(requestCtx, "status")
			requestCancel()
			if statusErr == nil {
				if status.DesiredState != "stopped" || status.State != "stopped" || status.Healthy || status.Generation != d.Payload.Generation || status.LastStepOutcome != "ok" {
					t.Fatal("restart fixture authenticated status did not describe the stopped installation")
				}
				break
			}
			select {
			case <-done:
				finished = true
				t.Fatal("restart fixture host exited before authenticated status")
			case <-ctx.Done():
				t.Fatal("restart fixture did not reach authenticated status within its allowance")
			case <-time.After(time.Millisecond):
			}
		}
		lease, leaseErr := c.acquireServiceLease(d)
		if lease != nil {
			_ = lease.Close()
		}
		if lease != nil || !errors.Is(leaseErr, ErrServiceHostActive) {
			t.Fatal("restart host did not reacquire the exclusive lifetime lease")
		}
		var leaseRecord serviceLeaseRecord
		identity, alive, identityErr := processIdentity(os.Getpid())
		if readJSON(c.path("service", "lease.json"), &leaseRecord) != nil || identityErr != nil || !alive || leaseRecord.Version != 1 || leaseRecord.PID != os.Getpid() || leaseRecord.StartIdentity != identity || leaseRecord.BootID != boot || leaseRecord.Owner != d.UID || leaseRecord.InstallationID != d.InstallationID {
			t.Fatal("restart host did not publish its fenced lease record")
		}
		currentLease, leaseStatErr := os.Stat(c.path("service", "lease.json"))
		currentLock, lockStatErr := os.Stat(c.path(".service-host.lock"))
		if leaseStatErr != nil || lockStatErr != nil || (previousLease != nil && os.SameFile(previousLease, currentLease)) || (stableLock != nil && !os.SameFile(stableLock, currentLock)) {
			t.Fatal("restart host did not rewrite the lease while retaining the stable lock inode")
		}
		previousLease, stableLock = currentLease, currentLock
		runCtx.armed.Store(true)
		// Wake the existing select without changing the stopped state. The next
		// select cancels after Run has restored its idle phase. Cancellation can
		// beat this response, so its result is deliberately immaterial.
		wakeCtx, wakeCancel := context.WithTimeout(ctx, allowance/4)
		_, _ = c.Control(wakeCtx, "observe")
		wakeCancel()
		select {
		case runErr := <-done:
			finished = true
			if runErr != nil {
				t.Fatal("restart fixture idle shutdown failed")
			}
		case <-time.After(allowance):
			t.Fatal("restart fixture shutdown exceeded its allowance")
		}
		os.Stderr = originalStderr
		receipt, readErr := os.ReadFile(stderr.Name())
		if readErr != nil || string(receipt) != "service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true\n" {
			t.Fatal("restart fixture did not emit exactly one clean idle shutdown receipt")
		}
		cleanup()
		serviceRestartStoppedArtifacts(t, c, d, boot, supervisorBefore, previousJournal)
		progress, _ := c.readJournal()
		previousJournal = progress.durableBytes
		// Test the released OS lock without overwriting the diagnostic lease.
		file, openErr := openLockFile(c.path(".service-host.lock"), false)
		if openErr != nil {
			t.Fatal("restart fixture could not reopen the stable lifetime lock")
		}
		probe := &operationLock{files: []*os.File{file}}
		lockErr := lockFile(file)
		closeErr := probe.Close()
		if lockErr != nil || closeErr != nil {
			t.Fatal("restart fixture shutdown retained its lifetime lock")
		}
	}
	t.Log("production restart proof completed")
}

func serviceRestartStoppedArtifacts(t *testing.T, c *Cell, d ServiceDescriptor, boot string, supervisorBefore []byte, previousJournal int64) {
	t.Helper()
	for _, name := range []string{"manifest.json", ".owner.lock", "run/operation.lock", ".service-host.lock", "service/lease.json", "service/boot.json", "service/supervisor.json", "service/descriptor.json", "service/control.secret", "journal/operations.jsonl"} {
		if servicePrivateRecord(c.path(filepath.FromSlash(name))) != nil {
			t.Fatal("restart fixture retained a record without private ownership")
		}
	}
	operation, err := acquireLock(c.cfg.Root, false)
	if err != nil || operation == nil {
		t.Fatal("restart fixture shutdown retained an operation lock")
	}
	if operation.Close() != nil {
		t.Fatal("restart fixture could not release its operation lock probe")
	}
	for _, name := range []string{"service/control.sock", "service/control-endpoint.json", "run/postgres.json", "run/server.json", "run/worker.json", "pg/postmaster.pid"} {
		if _, err := os.Lstat(c.path(filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("restart fixture shutdown retained a control endpoint or child process record")
		}
	}
	var bootRecord serviceBootRecord
	if readJSON(c.path("service", "boot.json"), &bootRecord) != nil || bootRecord.InstallationID != d.InstallationID || bootRecord.BootID != boot {
		t.Fatal("restart fixture shutdown lost the same-boot installation fence")
	}
	progress, err := c.readJournal()
	if err != nil || progress.pending != nil || progress.tornTail || progress.durableBytes <= previousJournal {
		t.Fatal("restart fixture shutdown did not retain a completed advancing journal")
	}
	manifest, err := readManifest(c.cfg.Root)
	if err != nil || !manifest.Installed || manifest.LastOperation != "stop" {
		t.Fatal("restart fixture shutdown did not finish the production stop operation")
	}
	after, err := c.ServiceDescriptor()
	if err != nil || after.DesiredState != "stopped" || after.InstallationID != d.InstallationID || after.Payload.Generation != d.Payload.Generation || after.Revision != d.Revision {
		t.Fatal("restart fixture shutdown changed durable stopped intent or identity")
	}
	supervisorAfter, err := os.ReadFile(c.path("service", "supervisor.json"))
	if err != nil || !bytes.Equal(supervisorBefore, supervisorAfter) {
		t.Fatal("restart fixture changed the valid existing idle supervisor record")
	}
}

func serviceRestartWithDarwinBootReader(t *testing.T) {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal("restart fixture could not locate its source")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal("restart fixture could not canonicalize its source")
	}
	scratch := t.TempDir()
	reader := filepath.Join(scratch, "service_boot_darwin.go")
	if os.WriteFile(reader, []byte("//go:build darwin\n\npackage localcell\n\nfunc serviceBootID() (string, error) { return \"darwin:synthetic-restart\", nil }\n"), 0o600) != nil {
		t.Fatal("restart fixture could not write its isolated boot reader")
	}
	overlay, err := json.Marshal(struct{ Replace map[string]string }{map[string]string{filepath.Join(root, "internal/localcell/service_boot_darwin.go"): reader}})
	if err != nil {
		t.Fatal("restart fixture could not encode its isolated boot reader")
	}
	overlayPath := filepath.Join(scratch, "overlay.json")
	if os.WriteFile(overlayPath, overlay, 0o600) != nil {
		t.Fatal("restart fixture could not write its overlay")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("restart fixture could not locate Go")
	}
	// Query only Go's two public cache paths; never print inherited environment
	// or the child compiler/test output. The child has an explicit environment.
	cacheOutput, err := exec.CommandContext(t.Context(), goTool, "env", "-json", "GOCACHE", "GOMODCACHE").Output()
	var caches struct{ GOCACHE, GOMODCACHE string }
	if err != nil || json.Unmarshal(cacheOutput, &caches) != nil || !filepath.IsAbs(caches.GOCACHE) || !filepath.IsAbs(caches.GOMODCACHE) {
		t.Fatal("restart fixture could not locate Go caches")
	}
	buildCtx, buildCancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer buildCancel()
	command := exec.CommandContext(buildCtx, goTool, "test", "-overlay", overlayPath, "-run", "^TestServiceRunRestartAfterIdleShutdown$", "-count=1", "-v", "./internal/localcell")
	command.WaitDelay = time.Second
	command.Dir = root
	command.Env = []string{
		"HOME=" + t.TempDir(), "PATH=" + filepath.Dir(goTool) + ":/usr/bin:/bin",
		"GOCACHE=" + caches.GOCACHE, "GOMODCACHE=" + caches.GOMODCACHE,
		"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off",
		"WITSELF_HOME=" + t.TempDir(), "DSH_HOME=" + t.TempDir(),
	}
	output, err := command.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("production restart proof completed")) {
		t.Fatal("restart fixture with isolated Darwin boot reader failed")
	}
	t.Log("production restart proof completed with isolated Darwin boot reader")
}
