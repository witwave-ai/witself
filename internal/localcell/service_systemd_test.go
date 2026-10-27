package localcell

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func systemdFixture(t *testing.T) (*systemdUserAdapter, ServiceDescriptor, *systemdFixtureManager) {
	t.Helper()
	home := t.TempDir()
	runtimeDir := filepath.Join(home, "session")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	d := ServiceDescriptor{Version: 1, InstallationID: "fixture-install", UID: "1001", Root: filepath.Join(home, "cell's 日本 space"), Adapter: "systemd-user", Scope: "user", Payload: ServicePayload{Generation: "generation-1", Host: filepath.Join(home, "host's 日本 space"), Args: []string{"run", "--root", filepath.Join(home, "cell's 日本 space")}}, Runtime: "local", DesiredState: "running"}
	a := &systemdUserAdapter{uid: d.UID, home: home, runtimeDir: runtimeDir, unitDir: filepath.Join(home, ".config", "systemd", "user"), systemctl: "/fixture/systemctl", busctl: "/fixture/busctl", owned: func(_ string, _ os.FileInfo) bool { return true }}
	// This fixture models a Unix manager on every platform. Windows' native
	// adapter remains unavailable; its ACL semantics are not certified here.
	if runtime.GOOS == "windows" {
		a.inspectFile = func(path string) (os.FileInfo, error) {
			info, err := os.Lstat(path)
			if err != nil {
				return nil, err
			}
			return systemdUnixModeInfo{info}, nil
		}
	}
	m := &systemdFixtureManager{t: t, adapter: a, descriptor: d}
	a.command = m.command
	return a, d, m
}

type systemdUnixModeInfo struct{ os.FileInfo }

func (i systemdUnixModeInfo) Mode() os.FileMode {
	if i.IsDir() {
		return i.FileInfo.Mode()&^os.ModePerm | 0o700
	}
	return i.FileInfo.Mode()&^os.ModePerm | 0o600
}

type systemdFixtureManager struct {
	t                   *testing.T
	adapter             *systemdUserAdapter
	descriptor          ServiceDescriptor
	calls               [][]string
	loaded              bool
	unavailable         bool
	mismatch            string
	failEnable          bool
	disabled            bool
	wrappedBusProperty  bool
	mismatchAfterEnable string
}

func (m *systemdFixtureManager) command(_ context.Context, binary string, args ...string) ([]byte, error) {
	m.calls = append(m.calls, append([]string{binary}, args...))
	if m.unavailable {
		return nil, errors.New("fixture unavailable")
	}
	if binary == m.adapter.busctl {
		if args[2] == "call" {
			return json.Marshal(systemdBusResult{Type: "o", Data: json.RawMessage(`["/org/freedesktop/systemd1/unit/fixture"]`)})
		}
		path := m.descriptor.Payload.Host
		argv := append([]string{path}, m.descriptor.Payload.Args...)
		if m.mismatch == "executable" {
			path += "-foreign"
		}
		if m.mismatch == "arguments" {
			argv = append(argv, "foreign")
		}
		data, _ := json.Marshal([]any{[]any{path, argv, false, 0, 0, 0, 0, 0, 0, 0}})
		if m.wrappedBusProperty {
			data, _ = json.Marshal([]json.RawMessage{data})
		}
		return json.Marshal(systemdBusResult{Type: "a(sasbttttuii)", Data: data})
	}
	if len(args) > 1 && args[1] == "show" {
		if args[2] == "--property=Version" {
			return []byte("fixture\n"), nil
		}
		id, uid, fragment, load, dropins := serviceUnitLabel(m.descriptor), m.descriptor.UID, "", "not-found", ""
		if m.loaded {
			fragment = m.adapter.unitPath(m.descriptor)
			load = "loaded"
		}
		switch m.mismatch {
		case "label":
			id = "foreign.service"
		case "uid":
			uid = "1002"
		case "fragment":
			fragment += "-foreign"
		case "drop-in":
			dropins = "foreign.conf"
		}
		return []byte("Id=" + id + "\nUser=" + uid + "\nFragmentPath=" + fragment + "\nLoadState=" + load + "\nDropInPaths=" + dropins + "\n"), nil
	}
	if len(args) > 1 && args[1] == "daemon-reload" {
		_, err := os.Lstat(m.adapter.unitPath(m.descriptor))
		m.loaded = err == nil
		return nil, nil
	}
	if len(args) > 1 && args[1] == "enable" && m.failEnable {
		return nil, errors.New("fixture registration failure")
	}
	if len(args) > 1 && args[1] == "enable" && m.mismatchAfterEnable != "" {
		m.mismatch = m.mismatchAfterEnable
	}
	if len(args) > 1 && args[1] == "disable" {
		m.disabled = true
	}
	return nil, nil
}

func TestServiceSystemdBusPropertyMessageEnvelope(t *testing.T) {
	a, d, m := systemdFixture(t)
	m.wrappedBusProperty = true
	plan, err := a.Plan(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
}

func TestServiceSystemdUnitArgumentEscaping(t *testing.T) {
	_, d, _ := systemdFixture(t)
	d.Payload.Args = append(d.Payload.Args, `literal%value$dollar"quoted\slash`)
	unit, err := RenderSystemdUserUnit(d)
	if err != nil {
		t.Fatal(err)
	}
	text := string(unit)
	for _, want := range []string{"Type=simple\n", "User=1001\n", "Environment=WITSELF_RUNTIME=local\n", "Restart=no\n", "KillMode=mixed\n", "TimeoutStopSec=90s\n", `"literal%%value$$dollar\"quoted\\slash"`, "cell's 日本 space"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing rendered invariant %q", want)
		}
	}
	if strings.Contains(text, "/bin/sh") || strings.Contains(text, "ExecStop=") {
		t.Fatal("unit delegates a shell or competing lifecycle")
	}
	d.Payload.Args = append(d.Payload.Args, "bad\nExecStart=foreign")
	if _, err := RenderSystemdUserUnit(d); !errors.Is(err, ErrServiceOwnershipConflict) {
		t.Fatal("newline injection accepted")
	}
}

func TestServiceSystemdPlanIsSideEffectFree(t *testing.T) {
	a, d, m := systemdFixture(t)
	plan, err := a.Plan(context.Background(), d)
	if err != nil || !plan.Observation.Available || plan.Observation.Registered || plan.RequiredPrivilege != "user" {
		t.Fatal("unexpected plan")
	}
	if _, err := os.Lstat(filepath.Join(a.home, ".config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("plan created registration directory")
	}
	for _, call := range m.calls {
		if len(call) < 3 || call[2] != "show" {
			t.Fatal("plan issued mutating command")
		}
	}
}

func TestServiceSystemdUnavailableNamesForegroundFallback(t *testing.T) {
	a, d, m := systemdFixture(t)
	m.unavailable = true
	plan, err := a.Plan(context.Background(), d)
	if !errors.Is(err, ErrServiceManagerUnavailable) || plan.Observation.Reason != "service_manager_unavailable" || plan.Observation.Fallback != "localcellharness run" {
		t.Fatal("missing explicit capability outcome")
	}
	if _, err := os.Lstat(filepath.Join(a.home, ".config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unavailable manager mutated home")
	}
}

func TestServiceUnsupportedAdapterCapability(t *testing.T) {
	adapter := unavailableServiceAdapter{}
	observation, err := adapter.Inspect(context.Background(), ServiceDescriptor{})
	if !errors.Is(err, ErrServiceManagerUnavailable) || observation.Available || observation.Fallback != "localcellharness run" {
		t.Fatal("unsupported adapter omitted capability")
	}
	if runtime.GOOS != "linux" {
		if _, err := NewServiceAdapter().Plan(context.Background(), ServiceDescriptor{}); !errors.Is(err, ErrServiceManagerUnavailable) {
			t.Fatal("unsupported platform selected native manager")
		}
	}
}

func TestServiceSystemdExactRegistrationAndRemovalRetainsData(t *testing.T) {
	a, d, m := systemdFixture(t)
	if err := os.Mkdir(d.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(d.Root, "retained-data")
	if err := os.WriteFile(dataPath, []byte("persistent fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := a.Plan(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(a.unitPath(d))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal("registration is not private")
	}
	observation, err := a.Inspect(context.Background(), d)
	if err != nil || !observation.Registered || !observation.Owned {
		t.Fatal("loaded ownership not verified")
	}
	if err := a.Remove(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if !m.disabled {
		t.Fatal("manager registration not disabled")
	}
	if _, err := os.Stat(a.unitPath(d)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unit retained")
	}
	if data, err := os.ReadFile(dataPath); err != nil || string(data) != "persistent fixture" {
		t.Fatal("removal changed durable data")
	}
}

func TestServiceSystemdLoadedOwnershipConflicts(t *testing.T) {
	for _, mismatch := range []string{"label", "uid", "fragment", "drop-in", "executable", "arguments"} {
		t.Run(mismatch, func(t *testing.T) {
			a, d, m := systemdFixture(t)
			plan, err := a.Plan(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Apply(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			m.mismatch = mismatch
			before := len(m.calls)
			if _, err := a.Inspect(context.Background(), d); !errors.Is(err, ErrServiceOwnershipConflict) {
				t.Fatal("foreign registration accepted")
			}
			if err := a.Remove(context.Background(), d); !errors.Is(err, ErrServiceOwnershipConflict) {
				t.Fatal("foreign registration removed")
			}
			if err := a.Apply(context.Background(), plan); !errors.Is(err, ErrServiceOwnershipConflict) {
				t.Fatal("stale plan took over foreign registration")
			}
			for _, call := range m.calls[before:] {
				if len(call) > 2 && (call[2] == "disable" || call[2] == "enable" || call[2] == "daemon-reload") {
					t.Fatal("ownership conflict caused manager mutation")
				}
			}
		})
	}
}

func TestServiceSystemdForeignFileRefusedBeforeManagerMutation(t *testing.T) {
	a, d, m := systemdFixture(t)
	plan, err := a.Plan(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.inspectDirectories(true); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("[Service]\nExecStart=/foreign\n")
	if err := os.WriteFile(a.unitPath(d), foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), plan); !errors.Is(err, ErrServiceOwnershipConflict) {
		t.Fatal("foreign file overwritten")
	}
	if err := a.Remove(context.Background(), d); !errors.Is(err, ErrServiceOwnershipConflict) {
		t.Fatal("foreign file removed")
	}
	got, err := os.ReadFile(a.unitPath(d))
	if err != nil || !reflect.DeepEqual(got, foreign) {
		t.Fatal("foreign file changed")
	}
	if m.disabled {
		t.Fatal("foreign manager registration disabled")
	}
}

func TestServiceSystemdFailedRegistrationCleansOnlyOwnedUnit(t *testing.T) {
	a, d, m := systemdFixture(t)
	m.failEnable = true
	plan, err := a.Plan(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), plan); err == nil {
		t.Fatal("enable failure accepted")
	}
	if _, err := os.Stat(a.unitPath(d)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned partial registration retained")
	}
	if !m.disabled {
		t.Fatal("partial enabled state not disabled")
	}
}

func TestServiceSystemdFailedUnitWriteRemovesOnlyCreatedInode(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		name := "partial"
		if replacement {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			a, d, _ := systemdFixture(t)
			a.writeUnit = func(file *os.File, _ []byte) error {
				if _, err := file.Write([]byte("partial")); err != nil {
					t.Fatal(err)
				}
				if replacement {
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(file.Name(), file.Name()+".fixture-original"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(file.Name(), []byte("foreign replacement"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("fixture write failure")
			}
			plan, err := a.Plan(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Apply(context.Background(), plan); err == nil {
				t.Fatal("write failure accepted")
			}
			data, err := os.ReadFile(a.unitPath(d))
			if replacement {
				if err != nil || string(data) != "foreign replacement" {
					t.Fatal("foreign replacement removed")
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("owned partial file retained")
			}
		})
	}
}

func TestServiceSystemdForeignEnablementRefused(t *testing.T) {
	a, d, m := systemdFixture(t)
	plan, err := a.Plan(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.inspectDirectories(true); err != nil {
		t.Fatal(err)
	}
	wants := filepath.Join(a.unitDir, "default.target.wants")
	if err := os.Mkdir(wants, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wants, serviceUnitLabel(d)), []byte("foreign enablement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), plan); !errors.Is(err, ErrServiceOwnershipConflict) {
		t.Fatal("foreign enablement accepted")
	}
	if m.disabled {
		t.Fatal("foreign enablement removed")
	}
}

func TestServiceSystemdRechecksOwnershipAfterEnable(t *testing.T) {
	a, d, m := systemdFixture(t)
	m.mismatchAfterEnable = "arguments"
	plan, err := a.Plan(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), plan); !errors.Is(err, ErrServiceOwnershipConflict) {
		t.Fatal("post-enable ownership drift accepted")
	}
	if m.disabled {
		t.Fatal("foreign service disabled during cleanup")
	}
}

func TestServiceSystemdRejectsRootAndNonlocalSelection(t *testing.T) {
	_, d, _ := systemdFixture(t)
	for _, mutate := range []func(*ServiceDescriptor){func(d *ServiceDescriptor) { d.UID = "0" }, func(d *ServiceDescriptor) { d.Runtime = "" }, func(d *ServiceDescriptor) { d.Scope = "system" }, func(d *ServiceDescriptor) { d.Payload.Host = "relative-host" }, func(d *ServiceDescriptor) { d.InstallationID = "../foreign" }} {
		candidate := d
		mutate(&candidate)
		if _, err := RenderSystemdUserUnit(candidate); !errors.Is(err, ErrServiceOwnershipConflict) {
			t.Fatal("unsafe scope accepted")
		}
	}
}
