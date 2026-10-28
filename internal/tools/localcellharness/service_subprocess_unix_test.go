//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

// This builds the actual main and run dispatch once. Only the manager factory
// and Darwin's sandbox-denied boot sysctl are replaced in the disposable Go
// overlay. Run, the lease/control server, reconciliation, Stop, and the real
// signal.NotifyContext handler remain production code. A stopped synthetic
// installation exercises those paths without PostgreSQL or a native manager.
func TestServiceRunDispatchSIGINTNoRegistration(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	host := buildServiceHarnessObserver(t)
	token := []byte(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xa7}, 32)))

	t.Run("recording_adapter_and_receipt_positive_control", func(t *testing.T) {
		root, _ := prepareServiceHarnessInstallation(t, host, "systemd-user", token)
		manager := serviceHarnessManagerDirectory(t)
		for _, action := range []string{"install", "remove"} {
			output := invokeServiceHarness(t, host, manager, token, "service", action, "--root", root)
			var receipt struct {
				Operation string `json:"operation"`
				State     string `json:"state"`
			}
			if json.Unmarshal(output, &receipt) != nil || receipt.Operation != "service "+action || receipt.State != "done" {
				t.Fatal("service command did not emit its completed receipt")
			}
		}
		calls, err := os.ReadFile(filepath.Join(manager, "calls"))
		if err != nil || string(calls) != "New\nPlan\nApply\nNew\nRemove\n" {
			t.Fatal("injected manager recorder did not observe the positive control")
		}
		assertServiceHarnessTokenAbsent(t, "manager recording", calls, token)
	})

	t.Run("adapter_errors_reach_stderr", func(t *testing.T) {
		for _, test := range []struct {
			method, action, reason, calls string
		}{
			{"Plan", "install", localcell.ErrServiceManagerUnavailable.Error(), "New\nPlan\n"},
			{"Apply", "install", "service_registration_failed", "New\nPlan\nApply\n"},
			{"Remove", "remove", localcell.ErrServiceOwnershipConflict.Error(), "New\nRemove\n"},
		} {
			t.Run(test.method, func(t *testing.T) {
				root, _ := prepareServiceHarnessInstallation(t, host, "systemd-user", token)
				manager := serviceHarnessManagerDirectory(t)
				if os.WriteFile(filepath.Join(manager, "fail-"+test.method), nil, 0o600) != nil {
					t.Fatal("cannot prepare manager failure fixture")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				command := exec.CommandContext(ctx, host, "service", test.action, "--root", root)
				command.Env = serviceHarnessEnvironment(t, manager)
				var stdout, stderr bytes.Buffer
				command.Stdout, command.Stderr = &stdout, &stderr
				var exitErr *exec.ExitError
				if err := command.Run(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
					t.Fatal("adapter failure did not produce the harness failure exit")
				}
				assertServiceHarnessTokenAbsent(t, "failed service stdout", stdout.Bytes(), token)
				assertServiceHarnessTokenAbsent(t, "failed service stderr", stderr.Bytes(), token)
				if stdout.Len() != 0 || stderr.String() != test.reason+"\n" {
					t.Fatal("adapter failure did not emit only its fixed reason on stderr")
				}
				calls, err := os.ReadFile(filepath.Join(manager, "calls"))
				if err != nil || string(calls) != test.calls {
					t.Fatal("failed adapter command did not stop at its injected failure")
				}
			})
		}
	})

	t.Run("foreground_interrupt_and_status", func(t *testing.T) {
		root, cell := prepareServiceHarnessInstallation(t, host, "foreground", token)
		manager := serviceHarnessManagerDirectory(t)
		rootBefore, managerBefore := snapshotServiceHarnessTree(t, root), snapshotServiceHarnessTree(t, manager)
		// A second successful host after SIGINT proves the stable lease file's
		// OS lock is released; deleting the lease inode would be incorrect.
		for generation := 0; generation < 2; generation++ {
			hostProcess := startServiceHarnessForeground(t, host, root, manager)
			waitServiceHarnessStatus(t, cell, hostProcess)
			assertServiceHarnessForegroundTree(t, rootBefore, snapshotServiceHarnessTree(t, root))
			if !reflect.DeepEqual(managerBefore, snapshotServiceHarnessTree(t, manager)) {
				t.Fatal("foreground run called or wrote to the service manager")
			}
			// Status dispatch itself creates an adapter today. Keep its recorder
			// separate so foreground zero-call evidence remains exact.
			statusOutput := invokeServiceHarness(t, host, serviceHarnessManagerDirectory(t), token, "service", "status", "--root", root)
			var status localcell.ServiceStatus
			if json.Unmarshal(statusOutput, &status) != nil || status.DesiredState != "stopped" || status.State != "stopped" || status.Healthy {
				t.Fatal("service status did not serialize the supervisor's stopped state")
			}
			hostProcess.interrupt(t)
			assertServiceHarnessTokenAbsent(t, "foreground stdout", hostProcess.stdout.Bytes(), token)
			assertServiceHarnessTokenAbsent(t, "foreground stderr", hostProcess.stderr.Bytes(), token)
			if _, err := os.Lstat(filepath.Join(root, "service", "control.sock")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("SIGINT left the foreground control socket behind")
			}
			assertServiceHarnessForegroundTree(t, rootBefore, snapshotServiceHarnessTree(t, root))
			if !reflect.DeepEqual(managerBefore, snapshotServiceHarnessTree(t, manager)) {
				t.Fatal("foreground shutdown called or wrote to the service manager")
			}
		}
	})
}

func buildServiceHarnessObserver(t *testing.T) string {
	t.Helper()
	source, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal("cannot locate harness source")
	}
	// Go resolves its working directory without the parent's logical PWD.
	// Overlay keys must name that same tree when a checkout is reached through
	// an alias such as macOS /tmp -> /private/tmp.
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal("cannot canonicalize harness source")
	}
	buildRoot := t.TempDir()
	replacements := make(map[string]string)
	replaceFunctionName := func(name, from, to string) {
		t.Helper()
		original := filepath.Join(source, "internal", "localcell", name)
		data, err := os.ReadFile(original)
		if err != nil || bytes.Count(data, []byte("func "+from+"(")) != 1 {
			t.Fatal("cannot locate test overlay injection seam")
		}
		data = bytes.Replace(data, []byte("func "+from+"("), []byte("func "+to+"("), 1)
		replacement := filepath.Join(buildRoot, name)
		if os.WriteFile(replacement, data, 0o600) != nil {
			t.Fatal("cannot write test overlay injection")
		}
		replacements[original] = replacement
	}
	factory := "service_unsupported.go"
	if runtime.GOOS == "linux" {
		factory = "service_systemd_linux.go"
	}
	replaceFunctionName(factory, "NewServiceAdapter", "nativeServiceAdapterUnusedForHarnessFixture")
	bridge := serviceHarnessRecordingAdapterSource
	if runtime.GOOS == "darwin" {
		replaceFunctionName("service_boot_darwin.go", "serviceBootID", "nativeServiceBootIDUnusedForHarnessFixture")
		bridge += "\nfunc serviceBootID() (string, error) { return \"harness-synthetic-boot\", nil }\n"
	}
	bridgePath := filepath.Join(buildRoot, "service_harness_observer.go")
	if os.WriteFile(bridgePath, []byte(bridge), 0o600) != nil {
		t.Fatal("cannot write test recording adapter")
	}
	replacements[filepath.Join(source, "internal", "localcell", "service_harness_observer.go")] = bridgePath
	overlay, err := json.Marshal(struct{ Replace map[string]string }{replacements})
	if err != nil {
		t.Fatal("cannot encode test overlay")
	}
	overlayPath := filepath.Join(buildRoot, "overlay.json")
	if os.WriteFile(overlayPath, overlay, 0o600) != nil {
		t.Fatal("cannot write test overlay")
	}
	host := filepath.Join(buildRoot, "localcellharness")
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("cannot locate Go tool")
	}
	// Read only Go's two non-secret cache locations. No process environment or
	// child output is printed, and the build receives an explicit allowlist.
	query := exec.CommandContext(t.Context(), goTool, "env", "-json", "GOCACHE", "GOMODCACHE")
	cacheJSON, err := query.Output()
	var caches struct{ GOCACHE, GOMODCACHE string }
	if err != nil || json.Unmarshal(cacheJSON, &caches) != nil || !filepath.IsAbs(caches.GOCACHE) || !filepath.IsAbs(caches.GOMODCACHE) {
		t.Fatal("cannot locate Go caches")
	}
	command := exec.CommandContext(t.Context(), goTool, "build", "-overlay", overlayPath, "-o", host, "./internal/tools/localcellharness")
	command.Dir = source
	command.Env = []string{
		"HOME=" + t.TempDir(), "PATH=" + filepath.Dir(goTool) + ":/usr/bin:/bin",
		"GOCACHE=" + caches.GOCACHE, "GOMODCACHE=" + caches.GOMODCACHE,
		"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off",
		"WITSELF_HOME=" + t.TempDir(), "DSH_HOME=" + t.TempDir(),
	}
	if output, err := command.CombinedOutput(); err != nil {
		// Compiler output may embed paths or source literals. Keep failures
		// value-free even if a future mutation introduces a token literal.
		t.Fatalf("cannot build observed harness (diagnostic bytes=%d)", len(output))
	}
	return host
}

const serviceHarnessRecordingAdapterSource = `package localcell
import (
 "context"
 "errors"
 "os"
 "path/filepath"
)
type harnessRecordingServiceAdapter struct{}
func harnessRecordManagerCall(method string) error {
 f, err := os.OpenFile(filepath.Join(os.Getenv("LC_S7A_TEST_MANAGER_DIR"), "calls"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
 if err != nil { return errors.New("harness manager recorder unavailable") }
 _, writeErr := f.WriteString(method + "\n")
 closeErr := f.Close()
 if writeErr != nil || closeErr != nil { return errors.New("harness manager recorder failed") }
 if _, err := os.Stat(filepath.Join(os.Getenv("LC_S7A_TEST_MANAGER_DIR"), "fail-"+method)); err == nil {
  switch method {
  case "Plan": return ErrServiceManagerUnavailable
  case "Apply": return errors.New("service_registration_failed")
  case "Remove": return ErrServiceOwnershipConflict
  }
 }
 return nil
}
func NewServiceAdapter() ServiceAdapter {
 if harnessRecordManagerCall("New") != nil { panic("harness manager recorder unavailable") }
 return harnessRecordingServiceAdapter{}
}
func (harnessRecordingServiceAdapter) Inspect(context.Context, ServiceIdentity) (ServiceObservation, error) {
 return ServiceObservation{Available:true, Owned:true}, harnessRecordManagerCall("Inspect")
}
func (harnessRecordingServiceAdapter) Plan(_ context.Context, spec ServiceSpec) (ServicePlan, error) {
 return ServicePlan{Spec:spec}, harnessRecordManagerCall("Plan")
}
func (harnessRecordingServiceAdapter) Apply(context.Context, ServicePlan) error { return harnessRecordManagerCall("Apply") }
func (harnessRecordingServiceAdapter) Remove(context.Context, ServiceIdentity) error { return harnessRecordManagerCall("Remove") }
`

func prepareServiceHarnessInstallation(t *testing.T, host, adapter string, token []byte) (string, *localcell.Cell) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "lcs7h-")
	if err != nil {
		t.Fatal("cannot create private harness socket fixture")
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal("cannot canonicalize harness fixture")
	}
	for _, directory := range []string{"spool", "run", "journal", "logs", "tmp", "home"} {
		if os.Mkdir(filepath.Join(root, directory), 0o700) != nil {
			t.Fatal("cannot create synthetic installation layout")
		}
	}
	manifest := localcell.Manifest{LayoutVersion: localcell.LayoutVersion, InstallationID: "lc_harness_fixture", CreatedAt: time.Now().UTC(), Database: "witself", Role: "localcell", Installed: true,
		Ports: localcell.Ports{Postgres: 21101, ServerAPI: 21102, ServerHealth: 21103, ServerMetrics: 21104, WorkerHealth: 21105, WorkerMetrics: 21106}}
	data, err := json.Marshal(manifest)
	if err != nil || os.WriteFile(filepath.Join(root, "manifest.json"), data, 0o600) != nil {
		t.Fatal("cannot write synthetic installation manifest")
	}
	cell, err := localcell.New(localcell.Config{Root: root, Timeout: time.Second})
	if err != nil {
		t.Fatal("cannot open synthetic installation")
	}
	args := serviceFlags{root: root, timeout: time.Second}.hostArgs()
	descriptor := localcell.ServiceDescriptor{Adapter: adapter, DesiredState: "stopped", Payload: localcell.ServicePayload{Host: host, Args: args}}
	if cell.PrepareService(t.Context(), descriptor) != nil {
		t.Fatal("cannot prepare synthetic service descriptor")
	}
	if os.WriteFile(filepath.Join(root, "service", "control.secret"), token, 0o600) != nil {
		t.Fatal("cannot install known synthetic control token")
	}
	return root, cell
}

func serviceHarnessEnvironment(t *testing.T, manager string) []string {
	t.Helper()
	home := filepath.Dir(filepath.Dir(filepath.Dir(manager)))
	return []string{"HOME=" + home, "WITSELF_HOME=" + t.TempDir(), "DSH_HOME=" + t.TempDir(), "WITSELF_RUNTIME=local", "LC_S7A_TEST_MANAGER_DIR=" + manager}
}

func serviceHarnessManagerDirectory(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(privateHarnessRoot(t), ".config", "systemd", "user")
	if os.MkdirAll(directory, 0o700) != nil {
		t.Fatal("cannot create isolated systemd user-unit directory")
	}
	return directory
}

func invokeServiceHarness(t *testing.T, host, manager string, token []byte, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, host, args...)
	command.Env = serviceHarnessEnvironment(t, manager)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	assertServiceHarnessTokenAbsent(t, "service stdout", stdout.Bytes(), token)
	assertServiceHarnessTokenAbsent(t, "service stderr", stderr.Bytes(), token)
	if err != nil {
		t.Fatal("observed harness service command failed")
	}
	return stdout.Bytes()
}

type serviceHarnessProcess struct {
	command        *exec.Cmd
	stdout, stderr bytes.Buffer
	done           chan struct{}
	err            error
}

func startServiceHarnessForeground(t *testing.T, host, root, manager string) *serviceHarnessProcess {
	t.Helper()
	p := &serviceHarnessProcess{command: exec.CommandContext(t.Context(), host, "run", "--root", root), done: make(chan struct{})}
	p.command.Env = serviceHarnessEnvironment(t, manager)
	p.command.Stdout, p.command.Stderr = &p.stdout, &p.stderr
	if p.command.Start() != nil {
		t.Fatal("cannot start observed foreground harness")
	}
	go func() { p.err = p.command.Wait(); close(p.done) }()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.command.Process.Kill()
			select {
			case <-p.done:
			case <-time.After(3 * time.Second):
				t.Error("observed harness cleanup exceeded deadline")
			}
		}
	})
	return p
}

func waitServiceHarnessStatus(t *testing.T, cell *localcell.Cell, p *serviceHarnessProcess) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		status, err := cell.Control(ctx, "status")
		cancel()
		if err == nil && status.State == "stopped" {
			return
		}
		select {
		case <-p.done:
			t.Fatal("observed foreground harness exited before authenticated status")
		case <-deadline.C:
			t.Fatal("observed foreground harness missed its status deadline")
		case <-ticker.C:
		}
	}
}

func (p *serviceHarnessProcess) interrupt(t *testing.T) {
	t.Helper()
	if p.command.Process.Signal(os.Interrupt) != nil {
		t.Fatal("cannot send foreground SIGINT")
	}
	select {
	case <-p.done:
		if p.err != nil {
			t.Fatal("foreground SIGINT did not exit gracefully")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreground SIGINT exceeded graceful shutdown deadline")
	}
}

type serviceHarnessTreeEntry struct {
	mode os.FileMode
	data string
}

func snapshotServiceHarnessTree(t *testing.T, root string) map[string]serviceHarnessTreeEntry {
	t.Helper()
	entries := make(map[string]serviceHarnessTreeEntry)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || strings.HasSuffix(entry.Name(), ".service") {
			t.Fatal("foreground fixture contains a service unit or enablement link")
		}
		value := serviceHarnessTreeEntry{mode: info.Mode()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value.data = string(data)
		}
		entries[relative] = value
		return nil
	})
	if err != nil {
		t.Fatal("cannot inventory foreground filesystem state")
	}
	return entries
}

func assertServiceHarnessForegroundTree(t *testing.T, before, after map[string]serviceHarnessTreeEntry) {
	t.Helper()
	mutable := map[string]bool{
		"manifest.json": true, "journal/operations.jsonl": true,
		".service-host.lock": true, "service/lease.json": true,
		"service/boot.json": true, "service/supervisor.json": true,
		"service/control.sock": true,
	}
	for path, previous := range before {
		current, ok := after[path]
		if !ok || current.mode != previous.mode || (!mutable[filepath.ToSlash(path)] && current.data != previous.data) {
			t.Fatalf("foreground altered an unapproved existing entry %q", path)
		}
	}
	for path := range after {
		if _, existed := before[path]; !existed && !mutable[filepath.ToSlash(path)] {
			t.Fatalf("foreground created an unapproved entry %q", path)
		}
	}
}

func assertServiceHarnessTokenAbsent(t *testing.T, label string, data, token []byte) {
	t.Helper()
	if offset := bytes.Index(data, token); offset >= 0 {
		t.Fatalf("%s contains synthetic control token (length=%d, position=%d)", label, len(token), offset)
	}
}
