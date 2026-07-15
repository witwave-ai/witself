package memorycurator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type curatorServiceCommandCall struct {
	Name string
	Args []string
}

type curatorServiceFakeRunner struct {
	Calls []curatorServiceCommandCall
	Fail  map[string]error
}

func (r *curatorServiceFakeRunner) Run(_ context.Context, name string, args ...string) error {
	call := curatorServiceCommandCall{Name: name, Args: append([]string(nil), args...)}
	r.Calls = append(r.Calls, call)
	key := name + " " + strings.Join(args, " ")
	if err := r.Fail[key]; err != nil {
		return err
	}
	return nil
}

func TestCuratorServiceLaunchdLifecycleIsPrivateIdempotentAndRestartSafe(t *testing.T) {
	root := t.TempDir()
	userHome := filepath.Join(root, "user & home")
	witselfHome := filepath.Join(root, "state <private>")
	executable := filepath.Join(root, "Witself Tools", "witself")
	runner := &curatorServiceFakeRunner{}
	manager := CuratorServiceManager{
		Platform: CuratorServicePlatformDarwin, UserHome: userHome,
		WitselfHome: witselfHome, Executable: executable, UID: 501,
		RunCommand: runner.Run,
	}

	unrelated := filepath.Join(userHome, "Library", "LaunchAgents", "com.example.unrelated.plist")
	if err := os.MkdirAll(filepath.Dir(unrelated), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelated, []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := manager.Install(context.Background(), string(ProviderClaudeCode))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !status.Active || !status.Enabled || len(status.Paths) != 1 {
		t.Fatalf("install status = %#v", status)
	}
	path := manager.launchdPath(string(ProviderClaudeCode))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	definition := string(raw)
	for _, want := range []string{
		curatorServiceManagedMarker,
		"ai.witwave.witself.memory-curator.claude-code",
		"<string>" + strings.ReplaceAll(executable, "&", "&amp;") + "</string>",
		"<string>" + strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(witselfHome) + "</string>",
		"<string>--runtime</string>", "<string>claude-code</string>",
		"<string>--force</string>", "<string>--supervise</string>",
		"<key>RunAtLoad</key>", "<key>StartInterval</key>",
		"<key>SuccessfulExit</key>\n    <false/>",
		"<key>Umask</key>\n  <integer>63</integer>",
	} {
		if !strings.Contains(definition, want) {
			t.Fatalf("launchd definition missing %q:\n%s", want, definition)
		}
	}
	assertCuratorServiceDefinitionHasNoInferenceConfiguration(t, definition)
	assertPrivateCuratorServiceFile(t, path)
	first := append([]byte(nil), raw...)

	if _, err := manager.Install(context.Background(), string(ProviderClaudeCode)); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("idempotent launchd install changed the definition")
	}
	if _, err := manager.Start(context.Background(), string(ProviderClaudeCode)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Uninstall(context.Background(), string(ProviderClaudeCode)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed launchd file remains: %v", err)
	}
	if raw, err := os.ReadFile(unrelated); err != nil || string(raw) != "unrelated\n" {
		t.Fatalf("unrelated launchd file changed: %q / %v", raw, err)
	}
	if _, err := manager.Uninstall(context.Background(), string(ProviderClaudeCode)); err != nil {
		t.Fatalf("idempotent uninstall: %v", err)
	}

	joined := curatorServiceCalls(runner.Calls)
	for _, want := range []string{
		"launchctl bootout gui/501/ai.witwave.witself.memory-curator.claude-code",
		"launchctl enable gui/501/ai.witwave.witself.memory-curator.claude-code",
		"launchctl bootstrap gui/501 " + path,
		"launchctl kickstart -k gui/501/ai.witwave.witself.memory-curator.claude-code",
		"launchctl disable gui/501/ai.witwave.witself.memory-curator.claude-code",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("host calls missing %q:\n%s", want, joined)
		}
	}
}

func TestCuratorServiceSystemdLifecycleUsesPersistentTimerAndCrashRecovery(t *testing.T) {
	root := t.TempDir()
	runner := &curatorServiceFakeRunner{}
	manager := CuratorServiceManager{
		Platform: CuratorServicePlatformLinux,
		UserHome: filepath.Join(root, "home"), ConfigHome: filepath.Join(root, "config with space"),
		WitselfHome: filepath.Join(root, "wit$self%state"), Executable: filepath.Join(root, "bin with space", "wit$self%"),
		UID: 1000, RunCommand: runner.Run,
	}

	status, err := manager.Install(context.Background(), string(ProviderGrokBuild))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !status.Active || !status.Enabled || len(status.Paths) != 2 {
		t.Fatalf("install status = %#v", status)
	}
	servicePath := manager.systemdServicePath(string(ProviderGrokBuild))
	timerPath := manager.systemdTimerPath(string(ProviderGrokBuild))
	serviceRaw, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	timerRaw, err := os.ReadFile(timerPath)
	if err != nil {
		t.Fatal(err)
	}
	service, timer := string(serviceRaw), string(timerRaw)
	for _, want := range []string{
		curatorServiceManagedMarker,
		`Environment="WITSELF_HOME=`,
		`ExecStart="`, `"memory" "curate" "auto" "run" "--runtime" "grok-build" "--force" "--supervise"`,
		"UMask=0077", "Restart=on-failure", "RestartSec=60s", "TimeoutStartSec=50min",
	} {
		if !strings.Contains(service, want) {
			t.Fatalf("systemd service missing %q:\n%s", want, service)
		}
	}
	if !strings.Contains(service, `Environment="WITSELF_HOME=`+filepath.Join(root, `wit$self%%state`)+`"`) ||
		!strings.Contains(service, filepath.Join(root, `bin with space`, `wit$$self%%`)) {
		t.Fatalf("systemd environment/exec values did not apply distinct dollar/percent escaping: %s", service)
	}
	for _, want := range []string{
		curatorServiceManagedMarker, "OnBootSec=2min", "OnUnitInactiveSec=300s",
		"Persistent=true", "Unit=witself-memory-curator-grok-build.service",
	} {
		if !strings.Contains(timer, want) {
			t.Fatalf("systemd timer missing %q:\n%s", want, timer)
		}
	}
	assertCuratorServiceDefinitionHasNoInferenceConfiguration(t, service+timer)
	assertPrivateCuratorServiceFile(t, servicePath)
	assertPrivateCuratorServiceFile(t, timerPath)

	if _, err := manager.Install(context.Background(), string(ProviderGrokBuild)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(context.Background(), string(ProviderGrokBuild)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Uninstall(context.Background(), string(ProviderGrokBuild)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{servicePath, timerPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("managed systemd file %s remains: %v", path, err)
		}
	}
	joined := curatorServiceCalls(runner.Calls)
	for _, want := range []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now witself-memory-curator-grok-build.timer",
		"systemctl --user start --no-block witself-memory-curator-grok-build.service",
		"systemctl --user disable --now witself-memory-curator-grok-build.timer",
		"systemctl --user stop witself-memory-curator-grok-build.service",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("host calls missing %q:\n%s", want, joined)
		}
	}
}

func TestCuratorServiceLaunchdDefinitionsPassNativeSyntaxValidation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("plutil launchd validation requires Darwin; host is %s", runtime.GOOS)
	}
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skipf("plutil launchd validation unavailable: %v", err)
	}

	root := t.TempDir()
	manager := nativeSyntaxCuratorServiceManager(t, root, CuratorServicePlatformDarwin)
	for _, runtimeName := range curatorServiceRuntimeNames() {
		definitions, err := manager.definitions(runtimeName)
		if err != nil {
			t.Fatalf("generate %s launchd definition: %v", runtimeName, err)
		}
		if len(definitions) != 1 {
			t.Fatalf("%s launchd definition count = %d, want 1", runtimeName, len(definitions))
		}
		definition := definitions[0]
		if err := writeCuratorServiceFile(definition.Path, definition.Body); err != nil {
			t.Fatalf("write %s launchd definition: %v", runtimeName, err)
		}
		if output, err := exec.Command(plutil, "-lint", definition.Path).CombinedOutput(); err != nil {
			t.Fatalf("plutil -lint rejected %s launchd definition: %v\n%s", runtimeName, err, output)
		}
	}
}

func TestCuratorServiceSystemdDefinitionsPassNativeSyntaxValidation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("systemd-analyze validation requires Linux; host is %s", runtime.GOOS)
	}
	systemdAnalyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skipf("systemd-analyze validation unavailable: %v", err)
	}

	root := t.TempDir()
	manager := nativeSyntaxCuratorServiceManager(t, root, CuratorServicePlatformLinux)
	var paths []string
	for _, runtimeName := range curatorServiceRuntimeNames() {
		definitions, err := manager.definitions(runtimeName)
		if err != nil {
			t.Fatalf("generate %s systemd definitions: %v", runtimeName, err)
		}
		if len(definitions) != 2 {
			t.Fatalf("%s systemd definition count = %d, want 2", runtimeName, len(definitions))
		}
		for _, definition := range definitions {
			if err := writeCuratorServiceFile(definition.Path, definition.Body); err != nil {
				t.Fatalf("write %s systemd definition: %v", runtimeName, err)
			}
			paths = append(paths, definition.Path)
		}
	}
	args := append([]string{"verify"}, paths...)
	if output, err := exec.Command(systemdAnalyze, args...).CombinedOutput(); err != nil {
		t.Fatalf("systemd-analyze verify rejected curator definitions: %v\n%s", err, output)
	}
}

func TestCuratorServiceUninstallPreservesDefinitionsWhenDeactivationFails(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		platform   string
		runtime    string
		failedCall string
	}{
		{
			name: "launchd", platform: CuratorServicePlatformDarwin, runtime: string(ProviderCodex),
			failedCall: "launchctl bootout gui/501/ai.witwave.witself.memory-curator.codex",
		},
		{
			name: "systemd", platform: CuratorServicePlatformLinux, runtime: string(ProviderClaudeCode),
			failedCall: "systemctl --user disable --now witself-memory-curator-claude-code.timer",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			runner := &curatorServiceFakeRunner{Fail: map[string]error{testCase.failedCall: errors.New("manager refused deactivation")}}
			manager := CuratorServiceManager{
				Platform: testCase.platform, UserHome: filepath.Join(root, "home"), ConfigHome: filepath.Join(root, "config"),
				WitselfHome: filepath.Join(root, "state"), Executable: filepath.Join(root, "bin", "witself"),
				UID: 501, RunCommand: runner.Run,
			}
			definitions, err := manager.definitions(testCase.runtime)
			if err != nil {
				t.Fatal(err)
			}
			for _, definition := range definitions {
				if err := writeCuratorServiceFile(definition.Path, definition.Body); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := manager.Uninstall(context.Background(), testCase.runtime); err == nil ||
				!strings.Contains(err.Error(), "deactivate automatic curator service") {
				t.Fatalf("Uninstall() error = %v", err)
			}
			for _, definition := range definitions {
				if _, err := os.Stat(definition.Path); err != nil {
					t.Fatalf("definition removed after failed deactivation: %v", err)
				}
			}
		})
	}
}

func TestCuratorServiceLaunchdUninstallRemovesOwnedUnloadedDefinition(t *testing.T) {
	root := t.TempDir()
	runtimeName := string(ProviderCursor)
	printCall := "launchctl print gui/501/ai.witwave.witself.memory-curator.cursor"
	runner := &curatorServiceFakeRunner{Fail: map[string]error{printCall: errCuratorServiceNotLoaded}}
	manager := CuratorServiceManager{
		Platform: CuratorServicePlatformDarwin, UserHome: filepath.Join(root, "home"),
		WitselfHome: filepath.Join(root, "state"), Executable: filepath.Join(root, "bin", "witself"),
		UID: 501, RunCommand: runner.Run,
	}
	definitions, err := manager.definitions(runtimeName)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCuratorServiceFile(definitions[0].Path, definitions[0].Body); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Uninstall(context.Background(), runtimeName); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(definitions[0].Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unloaded owned definition remains: %v", err)
	}
	joined := curatorServiceCalls(runner.Calls)
	if strings.Contains(joined, "launchctl bootout") || !strings.Contains(joined, "launchctl disable") {
		t.Fatalf("unloaded uninstall calls = %s", joined)
	}
}

func TestCuratorServiceLaunchdUninstallPreservesDefinitionWhenLoadStateIsUnknown(t *testing.T) {
	root := t.TempDir()
	runtimeName := string(ProviderCursor)
	printCall := "launchctl print gui/501/ai.witwave.witself.memory-curator.cursor"
	runner := &curatorServiceFakeRunner{Fail: map[string]error{printCall: errors.New("launchd unavailable")}}
	manager := CuratorServiceManager{
		Platform: CuratorServicePlatformDarwin, UserHome: filepath.Join(root, "home"),
		WitselfHome: filepath.Join(root, "state"), Executable: filepath.Join(root, "bin", "witself"),
		UID: 501, RunCommand: runner.Run,
	}
	definitions, err := manager.definitions(runtimeName)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCuratorServiceFile(definitions[0].Path, definitions[0].Body); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Uninstall(context.Background(), runtimeName); err == nil ||
		!strings.Contains(err.Error(), "inspect automatic curator service") {
		t.Fatalf("Uninstall() error = %v", err)
	}
	if _, err := os.Stat(definitions[0].Path); err != nil {
		t.Fatalf("definition removed after unknown load state: %v", err)
	}
}

func TestCuratorServiceRefusesUnownedFilesBeforeChangingAnything(t *testing.T) {
	for _, platform := range []string{CuratorServicePlatformDarwin, CuratorServicePlatformLinux} {
		t.Run(platform, func(t *testing.T) {
			root := t.TempDir()
			runner := &curatorServiceFakeRunner{}
			manager := CuratorServiceManager{
				Platform: platform, UserHome: filepath.Join(root, "home"), ConfigHome: filepath.Join(root, "config"),
				WitselfHome: filepath.Join(root, "state"), Executable: filepath.Join(root, "bin", "witself"),
				UID: 501, RunCommand: runner.Run,
			}
			definitions, err := manager.definitions(string(ProviderCodex))
			if err != nil {
				t.Fatal(err)
			}
			collision := definitions[len(definitions)-1].Path
			if err := os.MkdirAll(filepath.Dir(collision), 0o700); err != nil {
				t.Fatal(err)
			}
			// Merely mentioning Witself's marker is not ownership authority. Only
			// the exact generated header plus kind/runtime contract may be managed.
			original := "user-owned service definition mentioning " + curatorServiceManagedMarker + "\n"
			if err := os.WriteFile(collision, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := manager.Install(context.Background(), string(ProviderCodex)); err == nil || !strings.Contains(err.Error(), "unowned") {
				t.Fatalf("Install() error = %v", err)
			}
			if len(runner.Calls) != 0 {
				t.Fatalf("host service manager called before ownership preflight: %#v", runner.Calls)
			}
			if raw, err := os.ReadFile(collision); err != nil || string(raw) != original {
				t.Fatalf("collision changed: %q / %v", raw, err)
			}
			if platform == CuratorServicePlatformLinux {
				if _, err := os.Stat(definitions[0].Path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("first systemd file changed before second-file preflight: %v", err)
				}
			}
			if _, err := manager.Uninstall(context.Background(), string(ProviderCodex)); err == nil || !strings.Contains(err.Error(), "unowned") {
				t.Fatalf("Uninstall() error = %v", err)
			}
			if raw, err := os.ReadFile(collision); err != nil || string(raw) != original {
				t.Fatalf("uninstall changed collision: %q / %v", raw, err)
			}
		})
	}
}

func TestCuratorServiceStatusIsValueFreeAndInactiveIsNotAnError(t *testing.T) {
	root := t.TempDir()
	runner := &curatorServiceFakeRunner{Fail: map[string]error{
		"systemctl --user is-enabled --quiet witself-memory-curator-cursor.timer": errors.New("disabled"),
		"systemctl --user is-active --quiet witself-memory-curator-cursor.timer":  errors.New("inactive"),
	}}
	manager := CuratorServiceManager{
		Platform: CuratorServicePlatformLinux,
		UserHome: filepath.Join(root, "home"), ConfigHome: filepath.Join(root, "config"),
		WitselfHome: filepath.Join(root, "state"), Executable: filepath.Join(root, "bin", "witself"),
		UID: 1000, RunCommand: runner.Run,
	}
	definitions, err := manager.definitions(string(ProviderCursor))
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		if err := writeCuratorServiceFile(definition.Path, definition.Body); err != nil {
			t.Fatal(err)
		}
	}
	status, err := manager.Status(context.Background(), string(ProviderCursor))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || status.Enabled || status.Active || status.Runtime != string(ProviderCursor) {
		t.Fatalf("status = %#v", status)
	}
	serialized := fmt.Sprintf("%#v", status)
	for _, forbidden := range []string{"token", "provider", "model", "agent_"} {
		if strings.Contains(strings.ToLower(serialized), forbidden) {
			t.Fatalf("status contains forbidden field %q: %s", forbidden, serialized)
		}
	}
}

func TestCuratorServiceRejectsUnsupportedPlatformRuntimeAndUnsafePaths(t *testing.T) {
	base := CuratorServiceManager{
		Platform: CuratorServicePlatformLinux, UserHome: "/home/scott", ConfigHome: "/home/scott/.config",
		WitselfHome: "/home/scott/.witself", Executable: "/usr/local/bin/witself",
		UID: 1000, RunCommand: func(context.Context, string, ...string) error { return nil },
	}
	if _, err := base.Status(context.Background(), "gemini"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported runtime error = %v", err)
	}
	unsupported := base
	unsupported.Platform = "windows"
	if _, err := unsupported.Status(context.Background(), string(ProviderCodex)); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported platform error = %v", err)
	}
	unsafe := base
	unsafe.WitselfHome = "relative/state"
	if _, err := unsafe.Status(context.Background(), string(ProviderCodex)); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("unsafe path error = %v", err)
	}
}

func assertCuratorServiceDefinitionHasNoInferenceConfiguration(t *testing.T, definition string) {
	t.Helper()
	lower := strings.ToLower(definition)
	for _, forbidden := range []string{"--token", "token_file", "bearer", "--provider", "provider_path", "--model", "agent_id", "account_id", "realm_id"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("service definition contains forbidden configuration %q:\n%s", forbidden, definition)
		}
	}
}

func assertPrivateCuratorServiceFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %#o, want 0600", path, got)
	}
}

func nativeSyntaxCuratorServiceManager(t *testing.T, root, platform string) CuratorServiceManager {
	t.Helper()
	executable := filepath.Join(root, "bin", "witself")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return CuratorServiceManager{
		Platform: platform, UserHome: filepath.Join(root, "home"), ConfigHome: filepath.Join(root, "config"),
		WitselfHome: filepath.Join(root, "state"), Executable: executable, UID: 501,
		RunCommand: func(context.Context, string, ...string) error { return nil },
	}
}

func curatorServiceRuntimeNames() []string {
	return []string{
		string(ProviderCodex),
		string(ProviderClaudeCode),
		string(ProviderGrokBuild),
		string(ProviderCursor),
	}
}

func curatorServiceCalls(calls []curatorServiceCommandCall) string {
	var lines []string
	for _, call := range calls {
		lines = append(lines, call.Name+" "+strings.Join(call.Args, " "))
	}
	return strings.Join(lines, "\n")
}
