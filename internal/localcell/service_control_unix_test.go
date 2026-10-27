//go:build darwin || linux

package localcell

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func shortServiceTestCell(t *testing.T) *Cell {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "lcs7-")
	if err != nil {
		t.Fatal("cannot create private socket fixture")
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	_, m := unitCell(t)
	c, err := New(Config{Root: root, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.layout(); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
		t.Fatal(err)
	}
	prepareSupervisorTest(t, c)
	return c
}

func TestServiceHostLeaseIndependentOfOperationLock(t *testing.T) {
	c := shortServiceTestCell(t)
	d, err := c.ServiceDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	boot := func() (string, error) { return "synthetic-boot", nil }
	lease, err := c.acquireServiceLeaseWithBoot(d, boot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	second, err := c.acquireServiceLeaseWithBoot(d, boot)
	if second != nil || !errors.Is(err, ErrServiceHostActive) {
		t.Fatal("second host acquired same-root lifetime lease")
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatalf("public Stop blocked behind lifetime lease: %v", err)
	}
	var record serviceLeaseRecord
	if err := readJSON(c.path("service", "lease.json"), &record); err != nil {
		t.Fatal(err)
	}
	if record.PID != os.Getpid() || record.BootID == "" || record.StartIdentity == "" || record.Owner != d.UID || record.InstallationID != d.InstallationID {
		t.Fatal("lifetime lease omitted process or installation fence")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := c.acquireServiceLeaseWithBoot(d, boot)
	if err != nil {
		t.Fatal("released host lease did not permit successor")
	}
	_ = after.Close()
}

func TestServiceControlAuthenticationPermissionsAndActions(t *testing.T) {
	c := shortServiceTestCell(t)
	commands := make(chan serviceCommand)
	closeControl, err := c.serveServiceControl(commands)
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	for _, item := range []struct {
		name string
		mode os.FileMode
	}{{"", 0o700}, {"control.secret", 0o600}, {"control.sock", 0o600}} {
		info, err := os.Lstat(c.path("service", item.name))
		if err != nil || info.Mode().Perm() != item.mode {
			t.Fatal("control object is not owner-private")
		}
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.path("service", "control.sock"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	for _, token := range []string{"", "wrong"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://localcell/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", token)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("cannot probe synthetic control socket")
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 256))
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusUnauthorized || string(body) != "service_control_refused\n" {
			t.Fatal("wrong or missing token did not produce fixed refusal")
		}
		select {
		case <-commands:
			t.Fatal("unauthenticated request reached supervisor")
		default:
		}
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		command := <-commands
		command.reply <- serviceReply{status: ServiceStatus{DesiredState: "running", State: "running", Healthy: true}}
	}()
	status, err := c.Control(t.Context(), "status")
	if err != nil || !status.Healthy {
		t.Fatal("authenticated private socket request failed")
	}
	<-finished
	if _, err := c.Control(t.Context(), "unknown"); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("unknown action accepted")
	}
	if err := os.Chmod(c.path("service", "control.sock"), 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Control(t.Context(), "status"); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("non-private socket accepted")
	}
	if err := os.Chmod(c.path("service", "control.sock"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRunExplicitForegroundLeaseAndGracefulStop(t *testing.T) {
	c := shortServiceTestCell(t)
	manager, _, _ := systemdFixture(t)
	if err := manager.inspectDirectories(true); err != nil {
		t.Fatal("cannot prepare isolated manager directory")
	}
	// Observe the same private user-unit directory as the adapter fixtures.
	// The harness regression separately injects a recording adapter into dispatch.
	rootBefore := foregroundInventory(t, c.cfg.Root)
	managerBefore := foregroundInventory(t, manager.unitDir)
	assertUnregistered := func() {
		t.Helper()
		assertForegroundInventory(t, rootBefore, foregroundInventory(t, c.cfg.Root), map[string]bool{
			".service-host.lock":      true,
			"service/descriptor.json": true,
			"service/lease.json":      true,
			"service/supervisor.json": true,
			"service/control.sock":    true,
		})
		assertForegroundInventory(t, managerBefore, foregroundInventory(t, manager.unitDir), nil)
	}
	started := make(chan struct{})
	var starts, stops atomic.Int32
	h := supervisorHooks{start: func(context.Context) error {
		if starts.Add(1) == 1 {
			close(started)
		}
		return nil
	}, stop: func(context.Context) error { stops.Add(1); return nil }, repair: func(context.Context) error { return nil }, health: func(context.Context) error { return nil }, reconcile: func() error { return nil }, now: time.Now, ticks: make(chan time.Time), initialBackoff: time.Second, maxBackoff: time.Minute, failureWindow: 10 * time.Minute}
	h.boot = func() (string, error) { return "synthetic-boot", nil }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, SupervisorPolicy{hooks: &h}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("foreground supervisor did not exit")
		}
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("foreground supervisor did not start")
	}
	assertUnregistered()
	if err := c.Run(t.Context(), SupervisorPolicy{hooks: &h}); !errors.Is(err, ErrServiceHostActive) {
		t.Fatal("second foreground host was not refused")
	}
	status, err := c.Control(t.Context(), "stop")
	if err != nil || status.DesiredState != "stopped" || status.Healthy {
		t.Fatal("authenticated stop did not establish fence")
	}
	status, err = c.Control(t.Context(), "start")
	if err != nil || !status.Healthy || starts.Load() != 2 {
		t.Fatal("authenticated start did not launch single tree")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		done <- nil
	case <-time.After(2 * time.Second):
		t.Fatal("graceful shutdown exceeded fixture deadline")
	}
	d, err := c.ServiceDescriptor()
	if err != nil || d.DesiredState != "running" || stops.Load() != 3 {
		t.Fatal("host shutdown did not stop children while preserving running intent")
	}
	if _, err := os.Lstat(c.path("service", "control.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("foreground host left control socket behind")
	}
	assertUnregistered()
	// The stable lock inode intentionally remains; cleanup means releasing its
	// OS lock so a successor can acquire the same lifetime lease.
	lease, err := c.acquireServiceLeaseWithBoot(d, h.boot)
	if err != nil {
		t.Fatal("foreground shutdown retained the lifetime lease")
	}
	if err := lease.Close(); err != nil {
		t.Fatal("cannot close successor fixture lease")
	}
}

type foregroundFile struct {
	mode   os.FileMode
	digest [sha256.Size]byte
}

// Only synthetic fixture trees are inspected. Digests and contents are never
// included in diagnostics, including the synthetic private control credential.
func foregroundInventory(t *testing.T, root string) map[string]foregroundFile {
	t.Helper()
	files := make(map[string]foregroundFile)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if strings.HasSuffix(name, ".service") || info.Mode()&os.ModeSymlink != 0 {
			t.Error("foreground host created a service unit or enablement link")
		}
		file := foregroundFile{mode: info.Mode()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			file.digest = sha256.Sum256(data)
		}
		files[filepath.ToSlash(name)] = file
		return nil
	})
	if err != nil {
		t.Fatal("cannot inventory synthetic foreground fixture")
	}
	return files
}

func assertForegroundInventory(t *testing.T, before, after map[string]foregroundFile, mutable map[string]bool) {
	t.Helper()
	for name, file := range after {
		if mutable[name] {
			wantMode := os.FileMode(0o600)
			if name == "service/control.sock" {
				wantMode |= os.ModeSocket
			}
			if file.mode != wantMode {
				t.Error("foreground state has an unexpected type or mode")
			}
			continue
		}
		if old, exists := before[name]; !exists || old != file {
			t.Error("foreground host changed a file outside expected service state")
		}
	}
	for name := range before {
		if _, exists := after[name]; !exists {
			t.Error("foreground host removed a pre-existing fixture object")
		}
	}
}

func TestServiceRunRejectsHostedRuntime(t *testing.T) {
	c := shortServiceTestCell(t)
	t.Setenv("WITSELF_RUNTIME", "")
	if err := c.Run(t.Context(), SupervisorPolicy{}); err == nil {
		t.Fatal("empty hosted runtime started supervisor")
	}
	if _, err := os.Lstat(c.path(".service-host.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("hosted runtime created lifetime lease")
	}
}

func TestServiceRunRejectsDifferentImmutableHost(t *testing.T) {
	c := shortServiceTestCell(t)
	d, err := c.ServiceDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	d.Payload.Host = c.path("different-host")
	if err := testWriteFile(d.Payload.Host, nil, 0o700); err != nil {
		t.Fatal("cannot create host ownership fixture")
	}
	if err := c.writeServiceDescriptor(d); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(t.Context(), SupervisorPolicy{}); !errors.Is(err, ErrServiceOwnershipConflict) {
		t.Fatal("different immutable host acquired service ownership")
	}
	if _, err := os.Lstat(c.path(".service-host.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("foreign host created lifetime lease")
	}
}
