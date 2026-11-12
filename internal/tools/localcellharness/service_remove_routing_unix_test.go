//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The actual production route must persist stopped intent before calling Remove,
// while passing the descriptor value read before that write. The overlay replaces
// only the native factory, so the route and fixed error output remain real.
func TestServiceRemoveAdapterMaintenanceRouting(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	host := buildServiceRemovalHarness(t)
	adapter, err := serviceAdapterKind(runtime.GOOS)
	if err != nil {
		t.Fatal("Removal fixture adapter is unavailable")
	}
	for _, row := range []struct{ name, cause string }{
		{"fence", "local_maintenance_fence_active"},
		{"host", "localcell: service_host_already_active"},
	} {
		t.Run(row.name, func(t *testing.T) {
			root, cell := prepareServiceHarnessInstallation(t, host, adapter, nil)
			if cell.SetServiceDesiredState(t.Context(), "running") != nil {
				t.Fatal("Removal fixture running intent failed")
			}
			before, err := cell.ServiceDescriptor()
			if err != nil {
				t.Fatal("Removal fixture descriptor could not load")
			}
			manager := serviceHarnessManagerDirectory(t)
			if os.WriteFile(filepath.Join(manager, "cause"), []byte(row.name), 0600) != nil {
				t.Fatal("Removal fixture cause could not persist")
			}
			command := exec.CommandContext(t.Context(), host, "service", "remove", "--root", root)
			command.Env = serviceHarnessEnvironment(t, manager)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if command.Run() == nil {
				t.Fatal("Removal refusal became command success")
			}
			if stdout.Len() != 0 || stderr.String() != row.cause+"\n" {
				t.Fatal("Removal refusal output changed or exposed values")
			}
			calls, err := os.ReadFile(filepath.Join(manager, "calls"))
			if err != nil || string(calls) != "New\nRemoveChecked\n" {
				t.Fatal("Removal route missed the stale descriptor assertion")
			}
			after, err := cell.ServiceDescriptor()
			if err != nil || after.DesiredState != "stopped" || after.Revision != before.Revision+1 {
				t.Fatal("Removal refusal lost the persisted stopped intent")
			}
		})
	}
}

const serviceRemovalHarnessMethod = `func (harnessRecordingServiceAdapter) Remove(ctx context.Context, d ServiceIdentity) error {
 cell, err := New(Config{Root:d.Root})
 if err != nil { return errors.New("removal_fixture_cell_invalid") }
 fresh, err := cell.ServiceDescriptor()
 if err != nil || fresh.DesiredState != "stopped" || d.DesiredState != "running" || fresh.Revision != d.Revision+1 || fresh.InstallationID != d.InstallationID || fresh.Payload.Generation != d.Payload.Generation {
  return errors.New("removal_fixture_route_invalid")
 }
 if err := harnessRecordManagerCall("RemoveChecked"); err != nil { return err }
 cause, err := os.ReadFile(filepath.Join(os.Getenv("LC_S7A_TEST_MANAGER_DIR"), "cause"))
 if err != nil { return errors.New("removal_fixture_cause_invalid") }
 if string(cause) == "fence" { return errMaintenanceFenceActive }
 if string(cause) == "host" { return ErrServiceHostActive }
 return errors.New("removal_fixture_cause_unknown")
}`

func buildServiceRemovalHarness(t *testing.T) string {
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
	switch runtime.GOOS {
	case "linux":
		factory = "service_systemd_linux.go"
	case "darwin":
		factory = "service_launchd_darwin.go"
	}
	replaceFunctionName(factory, "NewServiceAdapter", "nativeServiceAdapterUnusedForHarnessFixture")
	bridge := strings.Replace(serviceHarnessRecordingAdapterSource,
		`func (harnessRecordingServiceAdapter) Remove(context.Context, ServiceIdentity) error { return harnessRecordManagerCall("Remove") }`,
		serviceRemovalHarnessMethod, 1)
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
		"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=-mod=readonly",
		"WITSELF_HOME=" + t.TempDir(), "DSH_HOME=" + t.TempDir(),
	}
	if output, err := command.CombinedOutput(); err != nil {
		// Compiler output may embed paths or source literals. Keep failures
		// value-free even if a future mutation introduces a token literal.
		t.Fatalf("Cannot build removal harness diagnostic bytes %d", len(output))
	}
	return host
}
