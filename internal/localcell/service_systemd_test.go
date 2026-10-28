package localcell

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
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

type systemdFixtureModeInfo struct {
	os.FileInfo
	mode os.FileMode
}

func (i systemdFixtureModeInfo) Mode() os.FileMode { return i.mode }

func systemdFixtureMode(t *testing.T, a *systemdUserAdapter, target string, mode os.FileMode) {
	t.Helper()
	inspect := a.inspectFile
	if inspect == nil {
		inspect = os.Lstat
	}
	a.inspectFile = func(path string) (os.FileInfo, error) {
		info, err := inspect(path)
		if err == nil && path == target {
			return systemdFixtureModeInfo{FileInfo: info, mode: info.Mode()&^os.ModePerm | mode}, nil
		}
		return info, err
	}
}

type systemdFixtureManager struct {
	t                   *testing.T
	adapter             *systemdUserAdapter
	descriptor          ServiceDescriptor
	calls               [][]string
	loaded              bool
	active              bool
	unavailable         bool
	mismatch            string
	failEnable          bool
	disabled            bool
	wrappedBusProperty  bool
	mismatchAfterEnable string
	emptyUserProperty   bool
	failBus             bool
	failExecBus         bool
	failReload          bool
	busLostAfterReload  bool
	busLostAfterEnable  bool
	stayUnloaded        bool
}

func (m *systemdFixtureManager) command(_ context.Context, binary string, args ...string) ([]byte, error) {
	m.calls = append(m.calls, append([]string{binary}, args...))
	if m.unavailable {
		return nil, errors.New("fixture unavailable")
	}
	if binary == m.adapter.busctl {
		if m.failBus || (m.failExecBus && args[len(args)-1] != "Version") {
			return nil, errors.New("fixture bus unavailable")
		}
		if args[len(args)-1] == "Version" {
			return []byte(`{"type":"s","data":"fixture"}`), nil
		}
		if args[2] == "call" {
			switch args[6] {
			case "GetUnit":
				if !m.active {
					return nil, &exec.ExitError{Stderr: []byte("org.freedesktop.systemd1.NoSuchUnit: Unit not loaded")}
				}
			case "LoadUnit":
				info, err := os.Lstat(filepath.Join(m.adapter.unitDir, args[8]))
				if err != nil || !info.Mode().IsRegular() {
					return nil, &exec.ExitError{Stderr: []byte("org.freedesktop.systemd1.NoSuchUnit: Unit not loaded")}
				}
			default:
				m.t.Fatal("unexpected fixture manager method")
			}
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
		id, fragment, load, dropins := serviceUnitLabel(m.descriptor), "", "not-found", ""
		if m.loaded {
			fragment = m.adapter.unitPath(m.descriptor)
			load = "loaded"
		}
		switch m.mismatch {
		case "label":
			id = "foreign.service"
		case "fragment":
			fragment += "-foreign"
		case "drop-in":
			dropins = "foreign.conf"
		}
		properties := "Id=" + id + "\nFragmentPath=" + fragment + "\nLoadState=" + load + "\nDropInPaths=" + dropins + "\n"
		if m.emptyUserProperty {
			properties += "User=\n"
		}
		return []byte(properties), nil
	}
	if len(args) > 1 && args[1] == "daemon-reload" {
		if m.failReload {
			return nil, errors.New("fixture reload denied")
		}
		_, err := os.Lstat(m.adapter.unitPath(m.descriptor))
		m.loaded = err == nil && !m.stayUnloaded
		m.failBus = m.busLostAfterReload
		return nil, nil
	}
	if len(args) > 1 && args[1] == "enable" && m.failEnable {
		return nil, errors.New("fixture registration failure")
	}
	if len(args) > 1 && args[1] == "enable" && m.mismatchAfterEnable != "" {
		m.mismatch = m.mismatchAfterEnable
	}
	if len(args) > 1 && args[1] == "enable" && m.busLostAfterEnable {
		m.failBus = true
	}
	if len(args) > 1 && args[1] == "enable" {
		m.active = true
	}
	if len(args) > 1 && args[1] == "disable" {
		m.disabled = true
		m.active = false
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

func systemdInactiveReloadedFixture(t *testing.T) (*systemdUserAdapter, ServiceDescriptor, *systemdFixtureManager) {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	a, d, m := systemdFixture(t)
	if err := a.inspectDirectories(true); err != nil {
		t.Fatal(err)
	}
	unit, err := RenderSystemdUserUnit(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.unitPath(d), unit, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.command(t.Context(), a.systemctl, "--user", "daemon-reload"); err != nil {
		t.Fatal(err)
	}
	return a, d, m
}

func TestServiceSystemdInactiveUnitUsesLoadUnit(t *testing.T) {
	a, d, m := systemdInactiveReloadedFixture(t)
	if !m.loaded || m.active {
		t.Fatal("fixture must have a freshly reloaded, inactive unit")
	}
	before, err := os.Stat(a.unitPath(d))
	if err != nil {
		t.Fatal(err)
	}
	// Pin the negative branch that the old always-successful fake omitted.
	_, err = m.command(t.Context(), a.busctl, "--user", "--json=short", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "GetUnit", "s", serviceUnitLabel(d))
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || !strings.Contains(string(exitErr.Stderr), "org.freedesktop.systemd1.NoSuchUnit") {
		t.Fatal("GetUnit incorrectly found an inactive unit in the fixture")
	}
	start := len(m.calls)
	observation, err := a.Inspect(t.Context(), d)
	if err != nil || !observation.Available || !observation.Registered || !observation.Owned {
		t.Fatal("LoadUnit did not inspect the freshly reloaded inactive unit")
	}
	loads, reads := 0, 0
	for _, call := range m.calls[start:] {
		if call[0] == a.busctl && call[3] == "call" {
			if call[7] != "LoadUnit" || call[8] != "s" || call[9] != serviceUnitLabel(d) {
				t.Fatal("inspection used the wrong unit-loading method or signature")
			}
			loads++
		}
		if call[0] == a.busctl && call[len(call)-1] == "ExecStart" {
			reads++
		}
		if call[0] == a.systemctl && call[2] != "show" {
			t.Fatal("inspection mutated manager state")
		}
	}
	after, err := os.Stat(a.unitPath(d))
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) || m.active || loads != 1 || reads != 1 {
		t.Fatal("inspection changed the unit or did not load/read it exactly once")
	}
	unit, err := os.ReadFile(a.unitPath(d))
	want, renderErr := RenderSystemdUserUnit(d)
	if err != nil || renderErr != nil || !reflect.DeepEqual(unit, want) {
		t.Fatal("inspection changed the unit bytes")
	}
	_, err = m.command(t.Context(), a.busctl, "--user", "--json=short", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "LoadUnit", "s", "absent.service")
	if !errors.As(err, &exitErr) || !strings.Contains(string(exitErr.Stderr), "org.freedesktop.systemd1.NoSuchUnit") {
		t.Fatal("LoadUnit incorrectly found an absent unit in the fixture")
	}
}

func TestServiceSystemdExecStartReloadRetry(t *testing.T) {
	for _, tc := range []struct {
		name, stderr, step string
		reloadResponse     string
		failures           int
		failReload         bool
		mismatch           bool
		loads, reads       int
		wantErr            error
	}{
		{name: "dbus-error-name", stderr: "org.freedesktop.systemd1.NoSuchUnit", failures: 1, loads: 2, reads: 2},
		{name: "not-loaded-text", stderr: "Unit fixture.service not loaded", failures: 1, loads: 2, reads: 2},
		{name: "retry-exhausted", stderr: "org.freedesktop.systemd1.NoSuchUnit: Unit not loaded", failures: 2, loads: 2, reads: 2, wantErr: ErrServiceManagerUnavailable, step: "inspect:busctl_exec_start"},
		{name: "unrelated-error", stderr: "Access denied", failures: 1, loads: 1, reads: 1, wantErr: ErrServiceManagerUnavailable, step: "inspect:busctl_exec_start"},
		{name: "sanitized-error", stderr: "FIXTURE_SECRET=not loaded", failures: 1, loads: 1, reads: 1, wantErr: ErrServiceManagerUnavailable, step: "inspect:busctl_exec_start"},
		{name: "reload-failure", stderr: "Unit not loaded", failures: 1, failReload: true, loads: 2, reads: 1, wantErr: ErrServiceManagerUnavailable, step: "inspect:busctl_load_unit"},
		{name: "reload-missing-type", stderr: "Unit not loaded", failures: 1, reloadResponse: `{}`, loads: 2, reads: 1, wantErr: ErrServiceOwnershipConflict, step: "inspect:busctl_load_unit"},
		{name: "reload-missing-object", stderr: "Unit not loaded", failures: 1, reloadResponse: `{"type":"o"}`, loads: 2, reads: 1, wantErr: ErrServiceOwnershipConflict, step: "inspect:busctl_load_unit"},
		{name: "retry-argv-mismatch", stderr: "Unit not loaded", failures: 1, mismatch: true, loads: 2, reads: 2, wantErr: ErrServiceOwnershipConflict, step: "inspect:exec_start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, d, m := systemdInactiveReloadedFixture(t)
			var steps []string
			loads, reads := 0, 0
			a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
				if binary == a.busctl && args[2] == "call" {
					if args[6] != "LoadUnit" {
						t.Fatal("retry did not use LoadUnit")
					}
					loads++
					steps = append(steps, "load")
					if loads == 2 {
						if tc.failReload {
							return nil, &exec.ExitError{Stderr: []byte("fixture reload denied")}
						}
						if tc.reloadResponse != "" {
							return []byte(tc.reloadResponse), nil
						}
						// The retry must use the newly returned object, not the old one.
						return []byte(`{"type":"o","data":["/org/freedesktop/systemd1/unit/reloaded"]}`), nil
					}
				}
				if binary == a.busctl && args[len(args)-1] == "ExecStart" {
					reads++
					steps = append(steps, "read")
					if reads <= tc.failures {
						return []byte("not loaded"), &exec.ExitError{Stderr: []byte(tc.stderr)}
					}
					if reads == 2 && args[4] != "/org/freedesktop/systemd1/unit/reloaded" {
						t.Fatal("retry used the stale object path")
					}
					if tc.mismatch {
						m.mismatch = "arguments"
					}
				}
				return m.command(ctx, binary, args...)
			}
			err := a.inspectExecStart(t.Context(), d)
			if !errors.Is(err, tc.wantErr) || (err != nil && !strings.Contains(err.Error(), tc.step)) {
				t.Fatal("ExecStart retry lost its expected outcome, sentinel, or command step")
			}
			wantSteps := []string{"load", "read", "load", "read"}
			if loads != tc.loads || reads != tc.reads || !reflect.DeepEqual(steps, wantSteps[:tc.loads+tc.reads]) {
				t.Fatal("ExecStart did not perform the exact bounded load/read sequence")
			}
		})
	}
}

func TestServiceSystemdLoadUnitFailuresPreserveSanitizedCause(t *testing.T) {
	for _, tc := range []struct {
		name         string
		loads, reads int
		steps        []string
	}{
		{name: "initial-load", loads: 1, reads: 0, steps: []string{"load"}},
		{name: "retry-load", loads: 2, reads: 1, steps: []string{"load", "read", "load"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, d, m := systemdInactiveReloadedFixture(t)
			privateHex := strings.Repeat("a", 64)
			const suffix = "load unit refusal tail"
			stderr := "denied " + d.Root + "\n" + a.home + "\n" + privateHex + "\nFIXTURE_SECRET=private-fixture-value\n" + suffix
			var steps []string
			loads, reads := 0, 0
			a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
				if binary == a.busctl && args[2] == "call" {
					if args[6] != "LoadUnit" {
						t.Fatal("inspection did not use LoadUnit")
					}
					loads++
					steps = append(steps, "load")
					if loads == tc.loads {
						return nil, &exec.ExitError{Stderr: []byte(stderr)}
					}
				}
				if binary == a.busctl && args[len(args)-1] == "ExecStart" {
					reads++
					steps = append(steps, "read")
					return nil, &exec.ExitError{Stderr: []byte("org.freedesktop.systemd1.NoSuchUnit: Unit not loaded")}
				}
				return m.command(ctx, binary, args...)
			}
			err := a.inspectExecStart(t.Context(), d)
			if !errors.Is(err, ErrServiceManagerUnavailable) || !strings.Contains(err.Error(), "inspect:busctl_load_unit") {
				t.Fatal("LoadUnit refusal lost its unavailable sentinel or command step")
			}
			for _, private := range []string{d.Root, a.home, privateHex, "FIXTURE_SECRET", "private-fixture-value"} {
				if strings.Contains(err.Error(), private) {
					t.Fatal("LoadUnit refusal exposed an unsanitized diagnostic")
				}
			}
			for _, want := range []string{"<root>", "<home>", "<hex>", "<environment>"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatal("LoadUnit refusal discarded sanitized stderr detail")
				}
			}
			if !strings.HasSuffix(err.Error(), suffix) {
				t.Fatal("LoadUnit refusal discarded its harmless diagnostic suffix")
			}
			if loads != tc.loads || reads != tc.reads || !reflect.DeepEqual(steps, tc.steps) {
				t.Fatal("LoadUnit refusal did not stop at the exact expected load/read sequence")
			}
		})
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
	for _, want := range []string{"Type=simple\n", "Environment=WITSELF_RUNTIME=local\n", "Restart=no\n", "KillMode=mixed\n", "TimeoutStopSec=90s\n", `"literal%%value$$dollar\"quoted\\slash"`, "cell's 日本 space"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing rendered invariant %q", want)
		}
	}
	if strings.Contains(text, "/bin/sh") || strings.Contains(text, "ExecStop=") || strings.Contains(text, "\nUser=") {
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
		if len(call) < 3 || (call[0] == a.systemctl && call[2] != "show") || (call[0] == a.busctl && (call[3] != "get-property" || call[len(call)-1] != "Version")) {
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

func TestServiceSystemdUnitDirectoryResolution(t *testing.T) {
	home, config := t.TempDir(), t.TempDir()
	config, err := filepath.EvalSymlinks(config)
	if err != nil {
		t.Fatal("cannot canonicalize external config fixture")
	}
	missingHome := func() (string, error) { return "", errors.New("fixture HOME absent") }
	passwdHome := func() (string, error) { return home, nil }
	for _, test := range []struct {
		name, config, wantBase, wantUnit string
	}{
		{"passwd-fallback", "", home, filepath.Join(home, ".config", "systemd", "user")},
		{"xdg-home", filepath.Join(home, "config"), home, filepath.Join(home, "config", "systemd", "user")},
		{"xdg-external", filepath.Join(config, "missing"), config, filepath.Join(config, "missing", "systemd", "user")},
		{"relative-xdg-refused", "relative-config", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			gotHome, base, unit := systemdUserDirectories(missingHome, passwdHome, test.config)
			if gotHome != home || base != test.wantBase || unit != test.wantUnit {
				t.Fatal("unit directory resolution differed from explicit home/config contract")
			}
		})
	}
	if gotHome, base, unit := systemdUserDirectories(missingHome, missingHome, ""); gotHome != "" || base != "" || unit != "" {
		t.Fatal("unresolvable home accepted")
	}
}

func TestServiceSystemdCapabilityRequiresUsableStorageAndBus(t *testing.T) {
	for _, failure := range []string{"home-unresolved", "home-missing", "unit-unresolved", "parent-unwritable", "unit-unwritable", "parent-unsearchable", "busctl-unreachable"} {
		t.Run(failure, func(t *testing.T) {
			a, d, m := systemdFixture(t)
			switch failure {
			case "home-unresolved":
				a.home = ""
			case "home-missing":
				a.home = filepath.Join(a.home, "missing-home")
				a.unitDir = filepath.Join(a.home, ".config", "systemd", "user")
			case "unit-unresolved":
				a.unitDir = ""
			case "unit-unwritable":
				if err := a.inspectDirectories(true); err != nil {
					t.Fatal(err)
				}
				a.accessible = func(path string, write bool) bool { return path != a.unitDir || !write }
			case "parent-unwritable":
				a.accessible = func(_ string, write bool) bool { return !write }
			case "parent-unsearchable":
				a.accessible = func(string, bool) bool { return false }
			case "busctl-unreachable":
				m.failBus = true
			}
			if err := a.capability(t.Context()); !errors.Is(err, ErrServiceManagerUnavailable) {
				t.Fatal("direct capability accepted an unavailable manager")
			}
			plan, err := a.Plan(t.Context(), d)
			if !errors.Is(err, ErrServiceManagerUnavailable) || plan.Observation.Available || plan.Observation.Reason != "service_manager_unavailable" || plan.Observation.Fallback != serviceForegroundFallback {
				t.Fatal("unusable capability did not select the explicit foreground fallback")
			}
			if err := a.Apply(t.Context(), plan); !errors.Is(err, ErrServiceManagerUnavailable) {
				t.Fatal("Apply did not repeat capability refusal")
			}
			for _, call := range m.calls {
				if call[0] == a.systemctl && call[2] != "show" {
					t.Fatal("unavailable capability mutated manager state")
				}
			}
			if a.unitDir != "" {
				if _, err := os.Lstat(a.unitPath(d)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unavailable capability wrote a registration")
				}
			}
		})
	}
}

func TestServiceSystemdDirectoryWalkRefusalsAgree(t *testing.T) {
	for _, refusal := range []struct{ name, step string }{
		{"group-writable", "unit_dir_walk:3:mode"},
		{"foreign-owner", "unit_dir_walk:3:owner"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			a, _, _ := systemdFixture(t)
			if err := a.inspectDirectories(true); err != nil {
				t.Fatal(err)
			}
			if refusal.name == "group-writable" {
				systemdFixtureMode(t, a, a.unitDir, 0o770)
			} else {
				a.owned = func(path string, _ os.FileInfo) bool { return path != a.unitDir }
			}
			createErr, recheckErr := a.inspectDirectories(true), a.inspectDirectories(false)
			for _, err := range []error{createErr, recheckErr, a.capability(t.Context())} {
				if !errors.Is(err, ErrServiceOwnershipConflict) || !strings.Contains(err.Error(), refusal.step) {
					t.Fatal("unsafe unit directory lost its ownership sentinel or exact refusal step")
				}
			}
			if createErr.Error() != recheckErr.Error() {
				t.Fatal("create and recheck disagree about the same unsafe unit directory")
			}
			if refusal.name == "group-writable" && !strings.Contains(createErr.Error(), "0770") {
				t.Fatal("directory mode refusal omitted the offending mode bits")
			}
		})
	}
}

func TestServiceSystemdCapabilityCauseSurvivesPlan(t *testing.T) {
	for _, failure := range []struct{ name, step string }{
		{"runtime-mode", "runtime_dir_mode"},
		{"unit-access", "unit_dir_walk:3:access"},
		{"systemctl-unreachable", "bus:systemctl_show"},
		{"busctl-unreachable", "bus:busctl_version"},
	} {
		t.Run(failure.name, func(t *testing.T) {
			a, d, m := systemdFixture(t)
			switch failure.name {
			case "runtime-mode":
				systemdFixtureMode(t, a, a.runtimeDir, 0o755)
			case "unit-access":
				if err := a.inspectDirectories(true); err != nil {
					t.Fatal(err)
				}
				a.accessible = func(path string, write bool) bool { return path != a.unitDir || !write }
			case "systemctl-unreachable":
				m.unavailable = true
			case "busctl-unreachable":
				m.failBus = true
			}
			plan, err := a.Plan(t.Context(), d)
			if !errors.Is(err, ErrServiceManagerUnavailable) || !strings.Contains(err.Error(), failure.step) {
				t.Fatal("Plan discarded the exact capability refusal step")
			}
			if plan.Observation.Reason != "service_manager_unavailable" || plan.Observation.Fallback != serviceForegroundFallback || plan.Observation.Available {
				t.Fatal("capability detail changed the closed observation reason or foreground fallback")
			}
			if failure.name == "runtime-mode" && !strings.Contains(err.Error(), "0755") {
				t.Fatal("runtime mode refusal omitted the offending mode bits")
			}
		})
	}
}

func TestServiceSystemdEmptyUserPropertyDoesNotConferOwnership(t *testing.T) {
	a, d, m := systemdFixture(t)
	m.emptyUserProperty = true
	plan, err := a.Plan(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(t.Context(), plan); err != nil {
		t.Fatal("empty manager User property prevented exact current-user registration")
	}
	for _, call := range m.calls {
		for _, arg := range call {
			if strings.HasPrefix(arg, "--property=") && strings.Contains(arg, "User") {
				t.Fatal("ownership inspection still requests the manager User property")
			}
		}
	}
	foreign := d
	foreign.UID = "1002"
	if _, err := a.Inspect(t.Context(), foreign); !errors.Is(err, ErrServiceOwnershipConflict) {
		t.Fatal("empty User property permitted a foreign process UID")
	}
	if err := a.Remove(t.Context(), d); err != nil {
		t.Fatal("empty User property prevented exact registration cleanup")
	}
}

func TestServiceSystemdRegistrationFailuresAreNotOwnershipConflicts(t *testing.T) {
	for _, failure := range []string{"reload-denied", "enable-denied", "not-found-enable-denied", "bus-after-reload", "exec-bus-unavailable", "bus-after-enable"} {
		t.Run(failure, func(t *testing.T) {
			a, d, m := systemdFixture(t)
			switch failure {
			case "reload-denied":
				m.failReload = true
			case "enable-denied":
				m.failEnable = true
			case "not-found-enable-denied":
				m.stayUnloaded, m.failEnable = true, true
			case "bus-after-reload":
				m.busLostAfterReload = true
			case "exec-bus-unavailable":
				m.failExecBus = true
			case "bus-after-enable":
				m.busLostAfterEnable = true
			}
			plan, err := a.Plan(t.Context(), d)
			if err != nil {
				t.Fatal("capability incorrectly requires registration permission")
			}
			if err := a.Apply(t.Context(), plan); !errors.Is(err, ErrServiceRegistrationFailed) || errors.Is(err, ErrServiceOwnershipConflict) {
				t.Fatal("registration failure was lost or mislabeled as ownership conflict")
			}
		})
	}
}

func TestServiceSystemdLoadedUnitBusFailurePreventsEnableAndRemove(t *testing.T) {
	a, d, m := systemdFixture(t)
	// The Version probe is usable, but the loaded-unit LoadUnit request fails.
	// This reproduces a loaded, never-enabled unit without a failed reload or
	// any disagreement between the create and capability directory walks.
	m.failExecBus = true
	plan, err := a.Plan(t.Context(), d)
	if err != nil {
		t.Fatal("read-only Version probe incorrectly required loaded-unit access")
	}
	err = a.Apply(t.Context(), plan)
	if !errors.Is(err, ErrServiceRegistrationFailed) || !errors.Is(err, ErrServiceManagerUnavailable) ||
		!strings.Contains(err.Error(), "post_reload") || !strings.Contains(err.Error(), "inspect:busctl_load_unit") {
		t.Fatal("post-reload failure lost its registration prefix or loaded-unit bus cause")
	}
	if !m.loaded {
		t.Fatal("fixture manager did not successfully load the written unit")
	}
	if info, err := os.Lstat(a.unitPath(d)); err != nil || !info.Mode().IsRegular() {
		t.Fatal("failed loaded-unit inspection did not retain the unit for diagnostics")
	}
	if err := a.capability(t.Context()); err != nil {
		t.Fatal("the same capability and directory recheck no longer accepts the created tree")
	}
	if err := a.Remove(t.Context(), d); !errors.Is(err, ErrServiceManagerUnavailable) ||
		errors.Is(err, ErrServiceRegistrationFailed) || !strings.Contains(err.Error(), "inspect:busctl_load_unit") {
		t.Fatal("Remove lost the same loaded-unit bus refusal")
	}
	reloads := 0
	for _, call := range m.calls {
		if call[0] != a.systemctl || len(call) < 3 {
			continue
		}
		switch call[2] {
		case "daemon-reload":
			reloads++
		case "enable", "disable":
			t.Fatal("loaded-unit inspection failure enabled or disabled an unverified unit")
		}
	}
	if reloads != 1 {
		t.Fatal("fixture did not complete exactly the initial daemon-reload")
	}
}

func TestServiceSystemdCommandFailuresPreserveSanitizedCause(t *testing.T) {
	for _, failure := range []struct{ command, step string }{
		{"daemon-reload", "daemon_reload"},
		{"enable", "enable_now"},
	} {
		t.Run(failure.command, func(t *testing.T) {
			a, d, m := systemdFixture(t)
			privateHex := strings.Repeat("a", 64)
			stderr := "denied " + d.Root + "\n" + a.home + "\n" + privateHex + "\nFIXTURE_SECRET=private-fixture-value\nmanager failure tail"
			a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
				if binary == a.systemctl && len(args) >= 2 && args[1] == failure.command {
					return nil, &exec.ExitError{Stderr: []byte(stderr)}
				}
				return m.command(ctx, binary, args...)
			}
			plan, err := a.Plan(t.Context(), d)
			if err != nil {
				t.Fatal("read-only capability unexpectedly needs registration permission")
			}
			err = a.Apply(t.Context(), plan)
			prefix := "service_registration_failed: " + failure.step + ": "
			if !errors.Is(err, ErrServiceRegistrationFailed) || !strings.HasPrefix(err.Error(), prefix) {
				t.Fatal("registration command error lost its sentinel or command step")
			}
			detail := strings.TrimPrefix(err.Error(), prefix)
			for _, private := range []string{d.Root, a.home, privateHex, "private-fixture-value"} {
				if strings.Contains(detail, private) {
					t.Fatal("registration command error exposed an unsanitized diagnostic")
				}
			}
			for _, want := range []string{"<root>", "<home>", "<hex>", "manager failure tail"} {
				if !strings.Contains(detail, want) {
					t.Fatal("registration command error omitted its sanitized stderr detail")
				}
			}
			if len(detail) > 400 {
				t.Fatal("registration command error exceeded the diagnostic byte limit")
			}
		})
	}
}

func TestServiceSystemdDiagnosticTailBound(t *testing.T) {
	root, home := "/fixture/home/cell", "/fixture/home"
	privateHex := strings.Repeat("b", 64)
	text := strings.Repeat("prefix ", 100) + root + " " + home + " " + privateHex + "\nFIXTURE_SECRET=private-fixture-value\nfinal diagnostic tail"
	detail := sanitizeServiceSystemdDetail(text, root, home)
	if len(detail) > 400 || !strings.HasSuffix(detail, "final diagnostic tail") {
		t.Fatal("diagnostic did not retain its bounded stderr tail")
	}
	for _, private := range []string{root, home, privateHex, "private-fixture-value"} {
		if strings.Contains(detail, private) {
			t.Fatal("bounded diagnostic exposed private fixture data")
		}
	}
	for _, marker := range []string{"<root>", "<home>", "<hex>"} {
		if !strings.Contains(detail, marker) {
			t.Fatal("bounded diagnostic lost a redaction marker from its tail")
		}
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
	for _, mismatch := range []string{"label", "process-uid", "fragment", "drop-in", "executable", "arguments"} {
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
			if mismatch == "process-uid" {
				d.UID = "1002"
				plan.Spec = d
			}
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
	if err := a.Apply(context.Background(), plan); !errors.Is(err, ErrServiceRegistrationFailed) || !strings.Contains(err.Error(), "enable_now") {
		t.Fatal("enable failure did not retain its fixed registration reason")
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
