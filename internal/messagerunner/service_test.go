package messagerunner

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

type messageRunnerServiceCommandCall struct {
	Name string
	Args []string
}

type messageRunnerServiceFakeRunner struct {
	Calls  []messageRunnerServiceCommandCall
	Fail   map[string]error
	Output map[string]string
}

func (r *messageRunnerServiceFakeRunner) Inspect(ctx context.Context, name string, args ...string) (string, error) {
	if err := r.Run(ctx, name, args...); err != nil {
		return "", err
	}
	key := name + " " + strings.Join(args, " ")
	if output, exists := r.Output[key]; exists {
		return output, nil
	}
	return "state = running\n", nil
}

func (r *messageRunnerServiceFakeRunner) Run(_ context.Context, name string, args ...string) error {
	call := messageRunnerServiceCommandCall{Name: name, Args: append([]string(nil), args...)}
	r.Calls = append(r.Calls, call)
	key := name + " " + strings.Join(args, " ")
	if err := r.Fail[key]; err != nil {
		return err
	}
	return nil
}

func TestMessageRunnerServiceLaunchdLifecycleIsPrivateIdempotentAndPersistent(t *testing.T) {
	root := t.TempDir()
	userHome := filepath.Join(root, "user & home")
	witselfHome := filepath.Join(root, "state <private>")
	executable := filepath.Join(root, "Witself Tools", "witself")
	runner := &messageRunnerServiceFakeRunner{}
	manager := ServiceManager{
		Platform: ServicePlatformDarwin, UserHome: userHome,
		WitselfHome: witselfHome, Executable: executable, UID: 501,
		RunCommand: runner.Run, InspectCommand: runner.Inspect,
	}

	unrelated := filepath.Join(userHome, "Library", "LaunchAgents", "com.example.unrelated.plist")
	if err := os.MkdirAll(filepath.Dir(unrelated), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelated, []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := manager.Install(context.Background(), "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !status.Active || !status.Enabled || len(status.Paths) != 1 {
		t.Fatalf("install status = %#v", status)
	}
	path := manager.launchdPath("claude-code")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	definition := string(raw)
	for _, want := range []string{
		messageRunnerServiceManagedMarker,
		"ai.witwave.witself.message-runner.claude-code",
		"<string>" + strings.ReplaceAll(executable, "&", "&amp;") + "</string>",
		"<string>" + strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(witselfHome) + "</string>",
		"<string>message</string>", "<string>runner</string>", "<string>serve</string>",
		"<string>--runtime</string>", "<string>claude-code</string>",
		"<key>RunAtLoad</key>\n  <true/>", "<key>KeepAlive</key>\n  <true/>",
		"<key>Umask</key>\n  <integer>63</integer>",
	} {
		if !strings.Contains(definition, want) {
			t.Fatalf("launchd definition missing %q:\n%s", want, definition)
		}
	}
	assertMessageRunnerServiceDefinitionIsValueFree(t, definition)
	assertPrivateMessageRunnerServiceFile(t, path)
	first := append([]byte(nil), raw...)

	if _, err := manager.Install(context.Background(), "claude-code"); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("idempotent launchd install changed the definition")
	}
	if _, err := manager.Start(context.Background(), "claude-code"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Uninstall(context.Background(), "claude-code"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed launchd file remains: %v", err)
	}
	if got, err := os.ReadFile(unrelated); err != nil || string(got) != "unrelated\n" {
		t.Fatalf("unrelated launchd file changed: %q / %v", got, err)
	}
	if _, err := manager.Uninstall(context.Background(), "claude-code"); err != nil {
		t.Fatalf("idempotent uninstall: %v", err)
	}

	joined := messageRunnerServiceCalls(runner.Calls)
	for _, want := range []string{
		"launchctl bootout gui/501/ai.witwave.witself.message-runner.claude-code",
		"launchctl enable gui/501/ai.witwave.witself.message-runner.claude-code",
		"launchctl bootstrap gui/501 " + path,
		"launchctl kickstart -k gui/501/ai.witwave.witself.message-runner.claude-code",
		"launchctl disable gui/501/ai.witwave.witself.message-runner.claude-code",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("host calls missing %q:\n%s", want, joined)
		}
	}
}

func TestMessageRunnerServiceSystemdLifecycleIsLongRunningAndRestarting(t *testing.T) {
	root := t.TempDir()
	runner := &messageRunnerServiceFakeRunner{}
	manager := ServiceManager{
		Platform: ServicePlatformLinux,
		UserHome: filepath.Join(root, "home"), ConfigHome: filepath.Join(root, "config with space"),
		WitselfHome: filepath.Join(root, "wit$self%state"), Executable: filepath.Join(root, "bin with space", "wit$self%"),
		UID: 1000, RunCommand: runner.Run,
	}

	status, err := manager.Install(context.Background(), "grok-build")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !status.Active || !status.Enabled || len(status.Paths) != 1 {
		t.Fatalf("install status = %#v", status)
	}
	servicePath := manager.systemdServicePath("grok-build")
	raw, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	definition := string(raw)
	for _, want := range []string{
		messageRunnerServiceManagedMarker,
		`Environment="WITSELF_HOME=`,
		`ExecStart="`, `"message" "runner" "serve" "--runtime" "grok-build"`,
		"Type=simple", "UMask=0077", "Restart=always", "RestartSec=10s",
		"[Install]", "WantedBy=default.target",
	} {
		if !strings.Contains(definition, want) {
			t.Fatalf("systemd service missing %q:\n%s", want, definition)
		}
	}
	if !strings.Contains(definition, `Environment="WITSELF_HOME=`+filepath.Join(root, `wit$self%%state`)+`"`) ||
		!strings.Contains(definition, filepath.Join(root, `bin with space`, `wit$$self%%`)) {
		t.Fatalf("systemd values did not apply distinct dollar/percent escaping: %s", definition)
	}
	if strings.Contains(definition, "[Timer]") || strings.Contains(definition, "OnUnit") {
		t.Fatalf("long-running runner unexpectedly uses a timer:\n%s", definition)
	}
	assertMessageRunnerServiceDefinitionIsValueFree(t, definition)
	assertPrivateMessageRunnerServiceFile(t, servicePath)

	if _, err := manager.Install(context.Background(), "grok-build"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(context.Background(), "grok-build"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Uninstall(context.Background(), "grok-build"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(servicePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed systemd file remains: %v", err)
	}
	joined := messageRunnerServiceCalls(runner.Calls)
	for _, want := range []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now witself-message-runner-grok-build.service",
		"systemctl --user start --no-block witself-message-runner-grok-build.service",
		"systemctl --user disable --now witself-message-runner-grok-build.service",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("host calls missing %q:\n%s", want, joined)
		}
	}
}

func TestMessageRunnerServiceDefinitionsPassNativeSyntaxValidation(t *testing.T) {
	root := t.TempDir()
	manager := nativeSyntaxMessageRunnerServiceManager(t, root, runtime.GOOS)
	var validator string
	var args func(string) []string
	switch runtime.GOOS {
	case ServicePlatformDarwin:
		validator = "plutil"
		args = func(path string) []string { return []string{"-lint", path} }
	case ServicePlatformLinux:
		validator = "systemd-analyze"
		args = func(path string) []string { return []string{"verify", path} }
	default:
		t.Skipf("native service syntax validation unsupported on %s", runtime.GOOS)
	}
	validatorPath, err := exec.LookPath(validator)
	if err != nil {
		t.Skipf("%s validation unavailable: %v", validator, err)
	}

	for _, runtimeName := range messageRunnerServiceRuntimeNames() {
		definitions, err := manager.definitions(runtimeName)
		if err != nil {
			t.Fatalf("generate %s definition: %v", runtimeName, err)
		}
		if len(definitions) != 1 {
			t.Fatalf("%s definition count = %d, want 1", runtimeName, len(definitions))
		}
		definition := definitions[0]
		if err := writeMessageRunnerServiceFile(definition.Path, definition.Body); err != nil {
			t.Fatalf("write %s definition: %v", runtimeName, err)
		}
		if output, err := exec.Command(validatorPath, args(definition.Path)...).CombinedOutput(); err != nil {
			t.Fatalf("%s rejected %s definition: %v\n%s", validator, runtimeName, err, output)
		}
	}
}

func TestMessageRunnerServiceRefusesUnownedFilesBeforeChangingAnything(t *testing.T) {
	for _, platform := range []string{ServicePlatformDarwin, ServicePlatformLinux} {
		t.Run(platform, func(t *testing.T) {
			root := t.TempDir()
			runner := &messageRunnerServiceFakeRunner{}
			manager := ServiceManager{
				Platform: platform, UserHome: filepath.Join(root, "home"), ConfigHome: filepath.Join(root, "config"),
				WitselfHome: filepath.Join(root, "state"), Executable: filepath.Join(root, "bin", "witself"),
				UID: 501, RunCommand: runner.Run, InspectCommand: runner.Inspect,
			}
			definitions, err := manager.definitions("codex")
			if err != nil {
				t.Fatal(err)
			}
			collision := definitions[0].Path
			if err := os.MkdirAll(filepath.Dir(collision), 0o700); err != nil {
				t.Fatal(err)
			}
			// Mentioning the marker alone is not ownership. The exact header,
			// filename, and managed command contract must all match.
			original := "user-owned definition mentioning " + messageRunnerServiceManagedMarker + "\n"
			if err := os.WriteFile(collision, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := manager.Install(context.Background(), "codex"); err == nil || !strings.Contains(err.Error(), "unowned") {
				t.Fatalf("Install() error = %v", err)
			}
			if len(runner.Calls) != 0 {
				t.Fatalf("host manager called before ownership preflight: %#v", runner.Calls)
			}
			if got, err := os.ReadFile(collision); err != nil || string(got) != original {
				t.Fatalf("collision changed: %q / %v", got, err)
			}
			if _, err := manager.Uninstall(context.Background(), "codex"); err == nil || !strings.Contains(err.Error(), "unowned") {
				t.Fatalf("Uninstall() error = %v", err)
			}
			if got, err := os.ReadFile(collision); err != nil || string(got) != original {
				t.Fatalf("uninstall changed collision: %q / %v", got, err)
			}
		})
	}
}

func TestMessageRunnerServiceUninstallPreservesDefinitionOnDeactivationFailure(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		platform   string
		failedCall string
	}{
		{
			name: "launchd", platform: ServicePlatformDarwin,
			failedCall: "launchctl bootout gui/501/ai.witwave.witself.message-runner.cursor",
		},
		{
			name: "systemd", platform: ServicePlatformLinux,
			failedCall: "systemctl --user disable --now witself-message-runner-cursor.service",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			runner := &messageRunnerServiceFakeRunner{Fail: map[string]error{testCase.failedCall: errors.New("manager refused")}}
			manager := ServiceManager{
				Platform: testCase.platform, UserHome: filepath.Join(root, "home"), ConfigHome: filepath.Join(root, "config"),
				WitselfHome: filepath.Join(root, "state"), Executable: filepath.Join(root, "bin", "witself"),
				UID: 501, RunCommand: runner.Run, InspectCommand: runner.Inspect,
			}
			definitions, err := manager.definitions("cursor")
			if err != nil {
				t.Fatal(err)
			}
			if err := writeMessageRunnerServiceFile(definitions[0].Path, definitions[0].Body); err != nil {
				t.Fatal(err)
			}

			if _, err := manager.Uninstall(context.Background(), "cursor"); err == nil ||
				!strings.Contains(err.Error(), "deactivate message runner service") {
				t.Fatalf("Uninstall() error = %v", err)
			}
			if _, err := os.Stat(definitions[0].Path); err != nil {
				t.Fatalf("definition removed after failed deactivation: %v", err)
			}
		})
	}
}

func TestMessageRunnerServiceLaunchdUninstallHandlesKnownAndUnknownLoadState(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		printError error
		wantError  bool
	}{
		{name: "not loaded", printError: errMessageRunnerServiceNotLoaded},
		{name: "unknown", printError: errors.New("launchd unavailable"), wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			printCall := "launchctl print gui/501/ai.witwave.witself.message-runner.cursor"
			runner := &messageRunnerServiceFakeRunner{Fail: map[string]error{printCall: testCase.printError}}
			manager := ServiceManager{
				Platform: ServicePlatformDarwin, UserHome: filepath.Join(root, "home"),
				WitselfHome: filepath.Join(root, "state"), Executable: filepath.Join(root, "bin", "witself"),
				UID: 501, RunCommand: runner.Run, InspectCommand: runner.Inspect,
			}
			definitions, err := manager.definitions("cursor")
			if err != nil {
				t.Fatal(err)
			}
			if err := writeMessageRunnerServiceFile(definitions[0].Path, definitions[0].Body); err != nil {
				t.Fatal(err)
			}

			_, err = manager.Uninstall(context.Background(), "cursor")
			if (err != nil) != testCase.wantError {
				t.Fatalf("Uninstall() error = %v, wantError %v", err, testCase.wantError)
			}
			_, statErr := os.Stat(definitions[0].Path)
			if testCase.wantError && statErr != nil {
				t.Fatalf("definition removed with unknown load state: %v", statErr)
			}
			if !testCase.wantError && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("known-unloaded definition remains: %v", statErr)
			}
			if strings.Contains(messageRunnerServiceCalls(runner.Calls), "launchctl bootout") {
				t.Fatal("uninstall attempted bootout without a known loaded service")
			}
		})
	}
}

func TestMessageRunnerServiceStatusIsValueFreeAndInactiveIsNotAnError(t *testing.T) {
	root := t.TempDir()
	runner := &messageRunnerServiceFakeRunner{Fail: map[string]error{
		"systemctl --user is-enabled --quiet witself-message-runner-cursor.service": errMessageRunnerServiceDisabled,
		"systemctl --user is-active --quiet witself-message-runner-cursor.service":  errMessageRunnerServiceInactive,
	}}
	manager := ServiceManager{
		Platform: ServicePlatformLinux,
		UserHome: filepath.Join(root, "home"), ConfigHome: filepath.Join(root, "config"),
		WitselfHome: filepath.Join(root, "state"), Executable: filepath.Join(root, "bin", "witself"),
		UID: 1000, RunCommand: runner.Run,
	}
	definitions, err := manager.definitions("cursor")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMessageRunnerServiceFile(definitions[0].Path, definitions[0].Body); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(context.Background(), "cursor")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || status.Enabled || status.Active || status.Runtime != "cursor" {
		t.Fatalf("status = %#v", status)
	}
	serialized := fmt.Sprintf("%#v", status)
	for _, forbidden := range []string{"token", "provider", "model", "agent_", "account_", "realm_"} {
		if strings.Contains(strings.ToLower(serialized), forbidden) {
			t.Fatalf("status contains forbidden field %q: %s", forbidden, serialized)
		}
	}
}

func TestMessageRunnerServiceLaunchdLoadedButExitedIsNotActive(t *testing.T) {
	root := t.TempDir()
	printCall := "launchctl print gui/501/ai.witwave.witself.message-runner.codex"
	runner := &messageRunnerServiceFakeRunner{Output: map[string]string{
		printCall: "gui/501/ai.witwave.witself.message-runner.codex = {\n\tstate = exited\n\tlast exit code = 1\n}\n",
	}}
	manager := ServiceManager{
		Platform: ServicePlatformDarwin, UserHome: filepath.Join(root, "home"),
		WitselfHome: filepath.Join(root, "state"), Executable: filepath.Join(root, "bin", "witself"),
		UID: 501, RunCommand: runner.Run, InspectCommand: runner.Inspect,
	}
	definitions, err := manager.definitions("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMessageRunnerServiceFile(definitions[0].Path, definitions[0].Body); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !status.Enabled || status.Active {
		t.Fatalf("loaded exited launchd status = %#v", status)
	}
}

func TestMessageRunnerServiceStatusSurfacesInspectionFailures(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		platform string
		failure  string
	}{
		{
			name: "launchd", platform: ServicePlatformDarwin,
			failure: "launchctl print gui/501/ai.witwave.witself.message-runner.codex",
		},
		{
			name: "systemd", platform: ServicePlatformLinux,
			failure: "systemctl --user is-active --quiet witself-message-runner-codex.service",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			runner := &messageRunnerServiceFakeRunner{Fail: map[string]error{
				testCase.failure: errors.New("manager unavailable"),
			}}
			manager := ServiceManager{
				Platform: testCase.platform, UserHome: filepath.Join(root, "home"),
				ConfigHome: filepath.Join(root, "config"), WitselfHome: filepath.Join(root, "state"),
				Executable: filepath.Join(root, "bin", "witself"), UID: 501,
				RunCommand: runner.Run, InspectCommand: runner.Inspect,
			}
			definitions, err := manager.definitions("codex")
			if err != nil {
				t.Fatal(err)
			}
			if err := writeMessageRunnerServiceFile(definitions[0].Path, definitions[0].Body); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Status(context.Background(), "codex"); err == nil ||
				!strings.Contains(err.Error(), "manager unavailable") {
				t.Fatalf("Status() error = %v, want inspection failure", err)
			}
		})
	}
}

func TestMessageRunnerServiceValidatesOpaqueRuntimeSlugsPlatformAndPaths(t *testing.T) {
	base := ServiceManager{
		Platform: ServicePlatformLinux, UserHome: "/home/scott", ConfigHome: "/home/scott/.config",
		WitselfHome: "/home/scott/.witself", Executable: "/usr/local/bin/witself",
		UID: 1000, RunCommand: func(context.Context, string, ...string) error { return nil },
	}
	for _, runtimeName := range messageRunnerServiceRuntimeNames() {
		if _, err := base.Status(context.Background(), runtimeName); err != nil {
			t.Fatalf("supported runtime %q: %v", runtimeName, err)
		}
	}
	for _, runtimeName := range []string{"gemini", "Claude-Code", "../codex", "codex.service", ""} {
		if _, err := base.Status(context.Background(), runtimeName); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("runtime %q error = %v", runtimeName, err)
		}
	}
	unsupported := base
	unsupported.Platform = "windows"
	if _, err := unsupported.Status(context.Background(), "codex"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported platform error = %v", err)
	}
	unsafe := base
	unsafe.WitselfHome = "relative/state"
	if _, err := unsafe.Status(context.Background(), "codex"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("unsafe path error = %v", err)
	}
}

func assertMessageRunnerServiceDefinitionIsValueFree(t *testing.T, definition string) {
	t.Helper()
	lower := strings.ToLower(definition)
	for _, forbidden := range []string{
		"--token", "token_file", "token-path", "bearer", "--provider", "provider_path", "--model",
		"agent_id", "account_id", "realm_id", "message_body", "claim_id",
	} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("service definition contains forbidden configuration %q:\n%s", forbidden, definition)
		}
	}
}

func assertPrivateMessageRunnerServiceFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %#o, want 0600", path, got)
	}
}

func nativeSyntaxMessageRunnerServiceManager(t *testing.T, root, platform string) ServiceManager {
	t.Helper()
	executable := filepath.Join(root, "bin", "witself")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return ServiceManager{
		Platform: platform, UserHome: filepath.Join(root, "home"), ConfigHome: filepath.Join(root, "config"),
		WitselfHome: filepath.Join(root, "state"), Executable: executable, UID: 501,
		RunCommand: func(context.Context, string, ...string) error { return nil },
	}
}

func messageRunnerServiceRuntimeNames() []string {
	return []string{"codex", "claude-code", "grok-build", "cursor"}
}

func messageRunnerServiceCalls(calls []messageRunnerServiceCommandCall) string {
	lines := make([]string, 0, len(calls))
	for _, call := range calls {
		lines = append(lines, call.Name+" "+strings.Join(call.Args, " "))
	}
	return strings.Join(lines, "\n")
}
