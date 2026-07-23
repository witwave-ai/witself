package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestIntegrationCommandHelpIsSuccessfulAndSideEffectFree(t *testing.T) {
	witselfHome := filepath.Join(t.TempDir(), "witself-home")
	t.Setenv("WITSELF_HOME", witselfHome)

	previousProbe := probeRuntimeForIntegrationCatalog
	previousInstall := installOneIntegration
	previousUninstall := uninstallOneIntegration
	probeRuntimeForIntegrationCatalog = func(string) integrationDetection {
		t.Fatal("integration help performed runtime detection")
		return integrationDetection{}
	}
	installOneIntegration = func([]string) int {
		t.Fatal("integration help attempted installation")
		return 1
	}
	uninstallOneIntegration = func([]string) int {
		t.Fatal("integration help attempted uninstallation")
		return 1
	}
	t.Cleanup(func() {
		probeRuntimeForIntegrationCatalog = previousProbe
		installOneIntegration = previousInstall
		uninstallOneIntegration = previousUninstall
	})

	commands := [][]string{
		{"integrations", "--help"},
		{"integrations", "help"},
		{"install", "--help"},
		{"install", "codex", "--help"},
		{"install", "codex", "help"},
		{"install", "all", "--help"},
		{"uninstall", "--help"},
		{"uninstall", "codex", "--help"},
		{"uninstall", "codex", "help"},
		{"uninstall", "all", "--help"},
	}
	for _, args := range commands {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
			if code != 0 || !strings.Contains(strings.ToLower(stdout+stderr), "usage:") {
				t.Fatalf("run(%q) = %d stdout=%q stderr=%q", args, code, stdout, stderr)
			}
			if _, err := os.Lstat(witselfHome); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("help created integration state at %s: %v", witselfHome, err)
			}
		})
	}
}

func TestIntegrationsJSONUsesStableRuntimeOrder(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	runtimes := transcriptcapture.SupportedRuntimes()
	delays := make(map[string]time.Duration, len(runtimes))
	for index, runtimeName := range runtimes {
		delays[runtimeName] = time.Duration(len(runtimes)-index) * time.Millisecond
	}
	probeRuntimeForIntegrationCatalog = func(runtimeName string) integrationDetection {
		time.Sleep(delays[runtimeName])
		switch runtimeName {
		case transcriptcapture.RuntimeCodex, transcriptcapture.RuntimeCursor, transcriptcapture.RuntimeOpenClaw:
			return integrationDetection{
				State:      integrationDetectionAvailable,
				Executable: "/bin/" + runtimeName,
				Version:    runtimeName + "-version",
			}
		case transcriptcapture.RuntimeAntigravity:
			return integrationDetection{State: integrationDetectionError, Message: "probe failed"}
		default:
			return integrationDetection{State: integrationDetectionNotFound, Message: "not on PATH"}
		}
	}
	loadIntegrationForCatalog = func(runtimeName string) (transcriptcapture.Config, error) {
		switch runtimeName {
		case transcriptcapture.RuntimeCodex:
			return transcriptcapture.Config{
				Account:  "default",
				Realm:    "default",
				Agent:    "scott",
				HookMode: transcriptcapture.HookModeManaged,
				Location: transcriptcapture.Location{Name: "home"},
			}, nil
		case transcriptcapture.RuntimeCursor:
			return transcriptcapture.Config{}, errors.New("corrupt integration record")
		default:
			return transcriptcapture.Config{}, os.ErrNotExist
		}
	}

	stdout, stderr, code := captureIntegrationsCLI(t, func() int {
		return integrationsCmd([]string{"--json"})
	})
	if code != 0 {
		t.Fatalf("integrations --json code = %d, stderr = %q", code, stderr)
	}
	var report integrationsReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode integrations JSON: %v\noutput: %s", err, stdout)
	}
	if report.SchemaVersion != integrationsSchemaVersion {
		t.Fatalf("schema_version = %q, want %q", report.SchemaVersion, integrationsSchemaVersion)
	}
	gotOrder := make([]string, 0, len(report.Runtimes))
	for _, status := range report.Runtimes {
		gotOrder = append(gotOrder, status.Runtime)
		if !status.Capabilities.MCP || !status.Capabilities.ManagedRouting {
			t.Errorf("%s capabilities = %#v, want MCP and managed routing", status.Runtime, status.Capabilities)
		}
	}
	if !reflect.DeepEqual(gotOrder, runtimes) {
		t.Fatalf("runtime order = %v, want %v", gotOrder, runtimes)
	}
	wantSummary := integrationsSummary{Supported: 7, Detected: 3, Installed: 1, Attention: 2}
	if report.Summary != wantSummary {
		t.Fatalf("summary = %#v, want %#v", report.Summary, wantSummary)
	}
	if got := report.Runtimes[0].Integration; got.State != integrationStateInstalled || got.Agent != "scott" || got.Location != "home" || got.HookMode != transcriptcapture.HookModeManaged {
		t.Fatalf("codex integration = %#v", got)
	}
	if got := report.Runtimes[3].Integration; got.State != integrationStateError || !strings.Contains(got.Message, "corrupt") {
		t.Fatalf("cursor integration = %#v", got)
	}
	if report.VerificationSummary != nil {
		t.Fatalf("default inventory unexpectedly ran verification: %#v", report.VerificationSummary)
	}
	for _, runtimeStatus := range report.Runtimes {
		if runtimeStatus.Verification != nil {
			t.Fatalf("%s default inventory has verification = %#v", runtimeStatus.Runtime, runtimeStatus.Verification)
		}
	}
}

func TestIntegrationsVerifyClassifiesPersistedTopology(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	integrationCatalogGOOS = "windows"
	root := t.TempDir()
	availableCLI := filepath.Join(root, "runtime.exe")
	if err := os.WriteFile(availableCLI, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingCLI := filepath.Join(root, "missing.exe")

	probeRuntimeForIntegrationCatalog = func(runtimeName string) integrationDetection {
		return integrationDetection{
			State:      integrationDetectionAvailable,
			Executable: filepath.Join(root, runtimeName+".exe"),
			Version:    "test",
		}
	}
	loadIntegrationForCatalog = func(runtimeName string) (transcriptcapture.Config, error) {
		switch runtimeName {
		case transcriptcapture.RuntimeCodex:
			return integrationInventoryTestConfig(runtimeName, availableCLI, root), nil
		case transcriptcapture.RuntimeClaudeCode:
			return integrationInventoryTestConfig(runtimeName, missingCLI, root), nil
		case transcriptcapture.RuntimeGrokBuild:
			return transcriptcapture.Config{}, errors.New("corrupt persisted integration")
		case transcriptcapture.RuntimeCursor:
			return integrationInventoryTestConfig(runtimeName, availableCLI, root), nil
		case transcriptcapture.RuntimeOpenClaw:
			cfg := integrationInventoryTestConfig(runtimeName, availableCLI, root)
			cfg.MCPEnvironment["OPENCLAW_STATE_DIR"] = filepath.Join(root, "openclaw-state")
			cfg.MCPEnvironment["OPENCLAW_CONFIG_PATH"] = filepath.Join(root, "openclaw.json")
			cfg.RuntimeWorkspace = "main"
			cfg.RuntimeAgentID = "main"
			return cfg, nil
		case transcriptcapture.RuntimeAntigravity:
			return transcriptcapture.Config{}, os.ErrNotExist
		case transcriptcapture.RuntimeCopilot:
			return integrationInventoryTestConfig(runtimeName, availableCLI, root), nil
		default:
			return transcriptcapture.Config{}, os.ErrNotExist
		}
	}
	validationErrors := map[string]error{
		transcriptcapture.RuntimeOpenClaw: errors.New("default workspace changed"),
	}
	validateIntegrationForCatalog = func(runtimeName string, _ transcriptcapture.Config) error {
		return validationErrors[runtimeName]
	}

	stdout, stderr, code := captureIntegrationsCLI(t, func() int {
		return integrationsCmd([]string{"--verify", "--json"})
	})
	if code != 1 {
		t.Fatalf("integrations --verify --json code = %d, want 1 for unhealthy topology; stderr = %q", code, stderr)
	}
	var report integrationsReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode integrations verification JSON: %v\noutput: %s", err, stdout)
	}
	if report.SchemaVersion != integrationsSchemaVersion {
		t.Fatalf("schema_version = %q, want %q", report.SchemaVersion, integrationsSchemaVersion)
	}
	wantSummary := integrationsVerificationSummary{
		Healthy:      2,
		Drifted:      1,
		Incomplete:   1,
		Unavailable:  1,
		Unsupported:  1,
		NotInstalled: 1,
	}
	if report.VerificationSummary == nil || *report.VerificationSummary != wantSummary {
		t.Fatalf("verification summary = %#v, want %#v", report.VerificationSummary, wantSummary)
	}
	wantStates := map[string]string{
		transcriptcapture.RuntimeCodex:       integrationVerificationHealthy,
		transcriptcapture.RuntimeClaudeCode:  integrationVerificationUnavailable,
		transcriptcapture.RuntimeGrokBuild:   integrationVerificationIncomplete,
		transcriptcapture.RuntimeCursor:      integrationVerificationUnsupported,
		transcriptcapture.RuntimeOpenClaw:    integrationVerificationDrifted,
		transcriptcapture.RuntimeAntigravity: integrationVerificationNotInstalled,
		transcriptcapture.RuntimeCopilot:     integrationVerificationHealthy,
	}
	for _, runtimeStatus := range report.Runtimes {
		if runtimeStatus.Verification == nil || runtimeStatus.Verification.State != wantStates[runtimeStatus.Runtime] {
			t.Errorf("%s verification = %#v, want %q", runtimeStatus.Runtime, runtimeStatus.Verification, wantStates[runtimeStatus.Runtime])
		}
	}
	if report.Summary.Attention != 4 {
		t.Fatalf("attention = %d, want 4", report.Summary.Attention)
	}

	human, stderr, code := captureIntegrationsCLI(t, func() int {
		return integrationsCmd([]string{"--verify"})
	})
	if code != 1 {
		t.Fatalf("integrations --verify code = %d, want 1; stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"HEALTH",
		"openclaw verification drifted: default workspace changed",
		"2 healthy, 1 drifted, 1 incomplete, 1 unavailable, 1 unsupported, 1 not installed",
	} {
		if !strings.Contains(human, want) {
			t.Errorf("verification output missing %q:\n%s", want, human)
		}
	}
}

func TestDefaultIntegrationsInventoryDoesNotValidateTopology(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	probeRuntimeForIntegrationCatalog = func(string) integrationDetection {
		return integrationDetection{State: integrationDetectionNotFound}
	}
	loadIntegrationForCatalog = func(runtimeName string) (transcriptcapture.Config, error) {
		if runtimeName == transcriptcapture.RuntimeCodex {
			return transcriptcapture.Config{Runtime: runtimeName}, nil
		}
		return transcriptcapture.Config{}, os.ErrNotExist
	}
	validationCalls := make(chan string, 1)
	statCalls := make(chan string, 1)
	validateIntegrationForCatalog = func(runtimeName string, _ transcriptcapture.Config) error {
		validationCalls <- runtimeName
		return nil
	}
	statIntegrationCLIForCatalog = func(path string) (os.FileInfo, error) {
		statCalls <- path
		return nil, os.ErrNotExist
	}

	report := collectIntegrationsReport()
	if report.VerificationSummary != nil {
		t.Fatalf("default report verification summary = %#v", report.VerificationSummary)
	}
	select {
	case runtimeName := <-validationCalls:
		t.Fatalf("default inventory validated %s topology", runtimeName)
	default:
	}
	select {
	case path := <-statCalls:
		t.Fatalf("default inventory checked persisted CLI %s", path)
	default:
	}
}

func TestIntegrationsVerifyReportsPendingOpenClawTransactionWithoutMutation(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	fixture := setupOpenClawIntegrationFixture(t)
	desired := openClawTransactionTestConfig(t, fixture)
	journal, err := beginOpenClawTransaction(openClawTransactionInstall, nil, &desired)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := openClawTransactionPath(fixture.stateDir)
	journalBefore, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	pendingTransactionForCatalog = pendingIntegrationTransaction
	probeRuntimeForIntegrationCatalog = func(runtimeName string) integrationDetection {
		if runtimeName != transcriptcapture.RuntimeOpenClaw {
			t.Fatalf("unexpected runtime probe %q", runtimeName)
		}
		return integrationDetection{State: integrationDetectionAvailable, Executable: fixture.cli}
	}
	loadIntegrationForCatalog = func(string) (transcriptcapture.Config, error) {
		return transcriptcapture.Config{}, os.ErrNotExist
	}
	validateIntegrationForCatalog = func(string, transcriptcapture.Config) error {
		t.Fatal("pending verification attempted topology validation")
		return nil
	}

	status := inspectIntegrationRuntime(transcriptcapture.RuntimeOpenClaw, true)
	if status.Verification == nil || status.Verification.State != integrationVerificationIncomplete ||
		!strings.Contains(status.Verification.Message, "interrupted install") ||
		!strings.Contains(status.Verification.Message, "witself install openclaw") {
		t.Fatalf("pending OpenClaw verification = %#v", status)
	}
	journalAfter, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(journalAfter, journalBefore) {
		t.Fatal("verification changed the pending OpenClaw journal")
	}
	if _, err := os.Lstat(fixture.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification changed OpenClaw MCP state: %v", err)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification created an integration config: %v", err)
	}
	routingCurrent, err := runtimeMemoryRoutingCurrentAt(transcriptcapture.RuntimeOpenClaw, desired.RuntimeWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if routingCurrent {
		t.Fatal("verification installed OpenClaw routing")
	}
	if journal.Operation != openClawTransactionInstall {
		t.Fatalf("test journal operation = %q", journal.Operation)
	}
}

func TestIntegrationsVerifyFindsPendingOpenClawJournalFromPersistedRootAfterSelectorDrift(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	fixture := setupOpenClawIntegrationFixture(t)
	previous := openClawTransactionTestConfig(t, fixture)
	if err := transcriptcapture.SaveConfig(previous); err != nil {
		t.Fatal(err)
	}
	previous, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw)
	if err != nil {
		t.Fatal(err)
	}
	writeOpenClawTransactionTestBinding(t, fixture, previous)
	desired := previous
	if _, err := beginOpenClawTransaction(openClawTransactionInstall, &previous, &desired); err != nil {
		t.Fatal(err)
	}
	journalPath := openClawTransactionPath(fixture.stateDir)
	journalBefore, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("OPENCLAW_STATE_DIR", filepath.Join(fixture.home, "drifted-state"))
	t.Setenv("OPENCLAW_CONFIG_PATH", filepath.Join(fixture.home, "drifted-state", "openclaw.json"))
	t.Setenv("OPENCLAW_PROFILE", "drifted")
	pendingTransactionForCatalog = pendingIntegrationTransaction
	probeRuntimeForIntegrationCatalog = func(string) integrationDetection {
		return integrationDetection{State: integrationDetectionAvailable, Executable: fixture.cli}
	}
	loadIntegrationForCatalog = transcriptcapture.LoadConfig
	validateIntegrationForCatalog = func(string, transcriptcapture.Config) error {
		t.Fatal("pending verification attempted topology validation")
		return nil
	}

	status := inspectIntegrationRuntime(transcriptcapture.RuntimeOpenClaw, true)
	if status.Verification == nil || status.Verification.State != integrationVerificationIncomplete ||
		!strings.Contains(status.Verification.Message, "interrupted install") {
		t.Fatalf("selector-drift pending verification = %#v", status)
	}
	journalAfter, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(journalAfter, journalBefore) {
		t.Fatal("selector-drift verification changed the persisted-root journal")
	}
}

func TestIntegrationsVerifyRejectsMissingPersistedMCPCommandBinary(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	integrationCatalogGOOS = "windows"
	runtimeCLI := filepath.Join(t.TempDir(), "provider.exe")
	mcpCommand := filepath.Join(t.TempDir(), "witself.exe")
	statIntegrationCLIForCatalog = func(path string) (os.FileInfo, error) {
		if path == mcpCommand {
			return nil, os.ErrNotExist
		}
		return os.Stat(os.Args[0])
	}
	validationCalled := false
	validateIntegrationForCatalog = func(string, transcriptcapture.Config) error {
		validationCalled = true
		return nil
	}
	cfg := integrationInventoryTestConfig(transcriptcapture.RuntimeCodex, runtimeCLI, t.TempDir())
	cfg.MCPCommand = mcpCommand
	status := verifyInstalledIntegration(
		transcriptcapture.RuntimeCodex,
		integrationPlatformStatus{SupportedOnPlatform: true, SupportLevel: integrationPlatformNative},
		cfg,
	)
	if status.State != integrationVerificationUnavailable || !strings.Contains(status.Message, "Witself MCP command") {
		t.Fatalf("missing MCP command verification = %#v", status)
	}
	if validationCalled {
		t.Fatal("topology validator ran after the persisted MCP command was already unavailable")
	}
}

func integrationInventoryTestConfig(runtimeName, runtimeCLI, root string) transcriptcapture.Config {
	configPath := filepath.Join(root, runtimeName, "config.toml")
	switch runtimeName {
	case transcriptcapture.RuntimeClaudeCode:
		configPath = filepath.Join(root, runtimeName, ".claude.json")
	case transcriptcapture.RuntimeCursor:
		configPath = filepath.Join(root, runtimeName, "mcp.json")
	}
	return transcriptcapture.Config{
		Runtime:              runtimeName,
		RuntimeCLICommand:    runtimeCLI,
		MCPCommand:           runtimeCLI,
		MCPEnvironment:       map[string]string{"WITSELF_HOME": root},
		RuntimeConfigRoot:    filepath.Dir(configPath),
		RuntimeMCPConfigPath: configPath,
	}
}

func TestIntegrationPlatformSupportSeparatesNativeWindowsFromWSL(t *testing.T) {
	for _, runtimeName := range transcriptcapture.SupportedRuntimes() {
		support := integrationPlatformSupport(runtimeName, "windows")
		if runtimeName == transcriptcapture.RuntimeCursor {
			if support.SupportedOnPlatform || support.SupportLevel != integrationPlatformWSLOnly ||
				!strings.Contains(support.Reason, "same WSL") {
				t.Fatalf("Cursor Windows support = %#v", support)
			}
			continue
		}
		if !support.SupportedOnPlatform || support.SupportLevel != integrationPlatformNative {
			t.Errorf("%s Windows support = %#v, want native", runtimeName, support)
		}
	}
	if support := integrationPlatformSupport(transcriptcapture.RuntimeCodex, "freebsd"); support.SupportedOnPlatform || support.SupportLevel != integrationPlatformUnsupported {
		t.Fatalf("FreeBSD support = %#v", support)
	}
}

func TestInstallAllSkipsUnsupportedPlatformWithoutProbing(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	integrationCatalogGOOS = "windows"
	probeRuntimeForIntegrationCatalog = func(runtimeName string) integrationDetection {
		if runtimeName == transcriptcapture.RuntimeCursor {
			t.Fatal("unsupported native Cursor must not be probed")
		}
		return integrationDetection{State: integrationDetectionNotFound}
	}
	loadIntegrationForCatalog = func(string) (transcriptcapture.Config, error) {
		return transcriptcapture.Config{}, os.ErrNotExist
	}
	stdout, _, code := captureIntegrationsCLI(t, func() int { return installAllCmd([]string{"--dry-run"}) })
	if code != 1 {
		t.Fatalf("install all code = %d, want no-supported-runtime failure\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "cursor") || !strings.Contains(stdout, "unsupported") ||
		!strings.Contains(stdout, "same WSL") {
		t.Fatalf("unsupported Cursor result missing:\n%s", stdout)
	}
}

func TestProbeIntegrationRuntimeEnforcesCopilotMinimumVersion(t *testing.T) {
	home := t.TempDir()
	cli := filepath.Join(home, "copilot")
	t.Setenv("COPILOT_HOME", filepath.Join(home, ".copilot"))
	t.Setenv("COPILOT_CLI_PATH", cli)

	writeCLI := func(version string) {
		t.Helper()
		script := "#!/bin/sh\n" +
			"if [ \"$1\" = \"--version\" ]; then printf '%s\\n' 'GitHub Copilot CLI " + version + "'; exit 0; fi\n" +
			"if [ \"$1 $2 $3\" = \"mcp add --help\" ]; then exit 0; fi\n" +
			"exit 2\n"
		if err := os.WriteFile(cli, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	writeCLI("1.0.72")
	old := probeIntegrationRuntime(transcriptcapture.RuntimeCopilot)
	if old.State != integrationDetectionError || old.Version != "1.0.72" ||
		!strings.Contains(old.Message, "1.0.73 or newer") {
		t.Fatalf("old Copilot detection = %#v", old)
	}

	writeCLI("1.0.73")
	current := probeIntegrationRuntime(transcriptcapture.RuntimeCopilot)
	if current.State != integrationDetectionAvailable || current.Version != "1.0.73" || current.Executable != cli {
		t.Fatalf("current Copilot detection = %#v", current)
	}
}

func TestBulkInstallPreviewCoversFullIdentitySelection(t *testing.T) {
	t.Setenv("WITSELF_ACCOUNT", "")
	t.Setenv("WITSELF_REALM", "")
	t.Setenv("WITSELF_AGENT", "")
	status := integrationRuntimeStatus{
		Integration: integrationBindingStatus{
			State:   integrationStateInstalled,
			Account: "default",
			Realm:   "default",
			Agent:   "scott",
		},
	}
	for _, test := range []struct {
		name      string
		flags     map[string]bool
		account   string
		realm     string
		agent     string
		endpoint  string
		tokenFile string
		location  string
		want      string
	}{
		{name: "no selector", flags: map[string]bool{}, want: "would refresh"},
		{name: "same tuple", flags: map[string]bool{"account": true, "realm": true, "agent": true}, account: "default", realm: "default", agent: "scott", want: "would refresh"},
		{name: "account change", flags: map[string]bool{"account": true}, account: "work", want: "would rebind"},
		{name: "realm change", flags: map[string]bool{"realm": true}, realm: "other", want: "would rebind"},
		{name: "agent change", flags: map[string]bool{"agent": true}, agent: "other", want: "would rebind"},
		{name: "location change", flags: map[string]bool{"location": true}, location: "work", want: "would rebind"},
		{name: "external token", flags: map[string]bool{"endpoint": true, "token-file": true}, endpoint: "https://example.test", tokenFile: "/tmp/token", want: "would refresh/rebind"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, _ := bulkInstallPreview(status, test.flags, test.account, test.realm, test.agent, test.location, test.endpoint, test.tokenFile)
			if got != test.want {
				t.Fatalf("action = %q, want %q", got, test.want)
			}
		})
	}

	t.Setenv("WITSELF_AGENT", "env-agent")
	if got, _ := bulkInstallPreview(status, map[string]bool{}, "", "", "", "", "", ""); got != "would rebind" {
		t.Fatalf("environment-selected agent action = %q, want would rebind", got)
	}
}

func TestBulkInstallHookPolicyPreservesOrOverrides(t *testing.T) {
	status := integrationRuntimeStatus{
		Runtime: transcriptcapture.RuntimeCodex,
		Integration: integrationBindingStatus{
			State:    integrationStateInstalled,
			HookMode: transcriptcapture.HookModeUser,
		},
		Capabilities: integrationCapabilities{TranscriptHooks: true, AdministratorHooks: true},
	}
	if got, _ := bulkInstallHookArgs(status, map[string]bool{}, false, false); !reflect.DeepEqual(got, []string{"--user-hooks"}) {
		t.Fatalf("preserved hook args = %v, want --user-hooks", got)
	}
	if got, _ := bulkInstallHookArgs(status, map[string]bool{"managed-hooks": true}, true, false); !reflect.DeepEqual(got, []string{"--managed-hooks"}) {
		t.Fatalf("managed hook args = %v, want --managed-hooks", got)
	}
	if got, _ := bulkInstallHookArgs(status, map[string]bool{"user-hooks": true}, false, true); !reflect.DeepEqual(got, []string{"--user-hooks"}) {
		t.Fatalf("user hook args = %v, want --user-hooks", got)
	}
}

func TestInstallAllDryRunDoesNotMutateAndReportsRebind(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	probeRuntimeForIntegrationCatalog = func(runtimeName string) integrationDetection {
		if runtimeName == transcriptcapture.RuntimeCodex || runtimeName == transcriptcapture.RuntimeOpenClaw {
			return integrationDetection{State: integrationDetectionAvailable, Executable: "/bin/" + runtimeName}
		}
		return integrationDetection{State: integrationDetectionNotFound}
	}
	loadIntegrationForCatalog = func(runtimeName string) (transcriptcapture.Config, error) {
		if runtimeName == transcriptcapture.RuntimeCodex {
			return transcriptcapture.Config{Agent: "old-agent", Location: transcriptcapture.Location{Name: "home"}}, nil
		}
		return transcriptcapture.Config{}, os.ErrNotExist
	}
	mutations := 0
	installOneIntegration = func([]string) int {
		mutations++
		return 0
	}

	stdout, stderr, code := captureIntegrationsCLI(t, func() int {
		return installAllCmd([]string{"--agent", "new-agent", "--location", "home", "--dry-run"})
	})
	if code != 0 {
		t.Fatalf("install all --dry-run code = %d, stderr = %q", code, stderr)
	}
	if mutations != 0 {
		t.Fatalf("dry run performed %d install mutations", mutations)
	}
	for _, want := range []string{"codex", "would rebind", "openclaw", "would install", "2 planned, 0 failed, 5 skipped"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, stdout)
		}
	}
}

func TestBulkIntegrationJSONIsStableAndContainsNoProgressText(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	probeRuntimeForIntegrationCatalog = func(runtimeName string) integrationDetection {
		if runtimeName == transcriptcapture.RuntimeCodex {
			return integrationDetection{State: integrationDetectionAvailable, Executable: "/bin/codex"}
		}
		return integrationDetection{State: integrationDetectionNotFound}
	}
	loadIntegrationForCatalog = func(string) (transcriptcapture.Config, error) {
		return transcriptcapture.Config{}, os.ErrNotExist
	}
	stdout, stderr, code := captureIntegrationsCLI(t, func() int {
		return installAllCmd([]string{"--agent", "test-bot", "--location", "home", "--dry-run", "--json"})
	})
	if code != 0 {
		t.Fatalf("install all JSON code = %d, stderr = %q", code, stderr)
	}
	var report bulkIntegrationReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode JSON output: %v\n%s", err, stdout)
	}
	if report.SchemaVersion != bulkIntegrationSchemaVersion || report.Operation != "install" || !report.DryRun {
		t.Fatalf("bulk report metadata = %#v", report)
	}
	if report.Summary != (bulkIntegrationSummary{Succeeded: 1, Skipped: 6}) {
		t.Fatalf("bulk report summary = %#v", report.Summary)
	}
	if strings.Contains(stdout, "installing ") || strings.Contains(stdout, "RUNTIME\t") {
		t.Fatalf("JSON output contains human progress text:\n%s", stdout)
	}
}

func TestInstallAllDoesNotInvokeRuntimeWithUnreadableIntegrationRecord(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	probeRuntimeForIntegrationCatalog = func(runtimeName string) integrationDetection {
		if runtimeName == transcriptcapture.RuntimeCodex {
			return integrationDetection{State: integrationDetectionAvailable, Executable: "/bin/codex"}
		}
		return integrationDetection{State: integrationDetectionNotFound}
	}
	loadIntegrationForCatalog = func(runtimeName string) (transcriptcapture.Config, error) {
		if runtimeName == transcriptcapture.RuntimeCodex {
			return transcriptcapture.Config{}, errors.New("corrupt integration record")
		}
		return transcriptcapture.Config{}, os.ErrNotExist
	}
	installOneIntegration = func([]string) int {
		t.Fatal("blocked runtime install was invoked")
		return 0
	}
	stdout, _, code := captureIntegrationsCLI(t, func() int { return installAllCmd([]string{"--agent", "test-bot"}) })
	if code != 1 || !strings.Contains(stdout, "blocked") || !strings.Contains(stdout, "corrupt integration record") {
		t.Fatalf("blocked install code = %d\n%s", code, stdout)
	}
}

func TestInstallAllContinuesAfterFailureAndForwardsCommonFlags(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	available := map[string]bool{
		transcriptcapture.RuntimeCodex:       true,
		transcriptcapture.RuntimeCursor:      true,
		transcriptcapture.RuntimeAntigravity: true,
	}
	probeRuntimeForIntegrationCatalog = func(runtimeName string) integrationDetection {
		if available[runtimeName] {
			return integrationDetection{State: integrationDetectionAvailable, Executable: "/bin/" + runtimeName}
		}
		return integrationDetection{State: integrationDetectionNotFound}
	}
	loadIntegrationForCatalog = func(runtimeName string) (transcriptcapture.Config, error) {
		if runtimeName == transcriptcapture.RuntimeCodex {
			return transcriptcapture.Config{Agent: "existing-agent"}, nil
		}
		return transcriptcapture.Config{}, os.ErrNotExist
	}
	var calls [][]string
	installOneIntegration = func(args []string) int {
		if !suppressIntegrationSuccessOutput {
			t.Errorf("individual integration output was not suppressed for %q", args[0])
		}
		calls = append(calls, append([]string(nil), args...))
		if args[0] == transcriptcapture.RuntimeCursor {
			return 1
		}
		return 0
	}

	commonFlags := []string{
		"--account", "work",
		"--realm", "engineering",
		"--agent", "scott",
		"--location", "home",
		"--endpoint", "https://witself.example",
		"--token-file", "/tmp/witself-token",
	}
	stdout, _, code := captureIntegrationsCLI(t, func() int {
		return installAllCmd(commonFlags)
	})
	if code != 1 {
		t.Fatalf("install all code = %d, want 1 for partial failure\n%s", code, stdout)
	}
	wantRuntimes := []string{
		transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeCursor,
		transcriptcapture.RuntimeAntigravity,
	}
	if len(calls) != len(wantRuntimes) {
		t.Fatalf("install calls = %v, want runtimes %v", calls, wantRuntimes)
	}
	for index, runtimeName := range wantRuntimes {
		want := append([]string{runtimeName}, commonFlags...)
		if !reflect.DeepEqual(calls[index], want) {
			t.Errorf("install call %d = %v, want %v", index, calls[index], want)
		}
	}
	if suppressIntegrationSuccessOutput {
		t.Fatal("bulk install did not restore individual-output suppression")
	}
	for _, want := range []string{
		"codex", "rebound",
		"cursor", "failed",
		"antigravity", "installed",
		"2 succeeded, 1 failed, 4 skipped",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("install output missing %q:\n%s", want, stdout)
		}
	}
}

func TestInstallAllPropagatesUsageExitAfterContinuing(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	probeRuntimeForIntegrationCatalog = func(runtimeName string) integrationDetection {
		if runtimeName == transcriptcapture.RuntimeCodex || runtimeName == transcriptcapture.RuntimeCursor {
			return integrationDetection{State: integrationDetectionAvailable}
		}
		if runtimeName == transcriptcapture.RuntimeAntigravity {
			return integrationDetection{State: integrationDetectionError, Message: "later detection error"}
		}
		return integrationDetection{State: integrationDetectionNotFound}
	}
	loadIntegrationForCatalog = func(string) (transcriptcapture.Config, error) {
		return transcriptcapture.Config{}, os.ErrNotExist
	}
	var calls []string
	installOneIntegration = func(args []string) int {
		calls = append(calls, args[0])
		if args[0] == transcriptcapture.RuntimeCodex {
			return 2
		}
		return 0
	}

	_, _, code := captureIntegrationsCLI(t, func() int { return installAllCmd(nil) })
	if code != 2 {
		t.Fatalf("install all code = %d, want 2", code)
	}
	if want := []string{transcriptcapture.RuntimeCodex, transcriptcapture.RuntimeCursor}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestUninstallAllPreservesUsageExitAcrossLaterReadError(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	loadIntegrationForCatalog = func(runtimeName string) (transcriptcapture.Config, error) {
		switch runtimeName {
		case transcriptcapture.RuntimeCodex:
			return transcriptcapture.Config{Runtime: runtimeName}, nil
		case transcriptcapture.RuntimeClaudeCode:
			return transcriptcapture.Config{}, errors.New("later read error")
		default:
			return transcriptcapture.Config{}, os.ErrNotExist
		}
	}
	uninstallOneIntegration = func([]string) int { return 2 }

	_, _, code := captureIntegrationsCLI(t, func() int { return uninstallAllCmd(nil) })
	if code != 2 {
		t.Fatalf("uninstall all code = %d, want 2", code)
	}
}

func TestIndividualIntegrationCommandsRejectTrailingShellExpansion(t *testing.T) {
	for _, test := range []struct {
		name    string
		command func([]string) int
	}{
		{name: "install", command: installCmd},
		{name: "uninstall", command: uninstallCmd},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, stderr, code := captureIntegrationsCLI(t, func() int {
				return test.command([]string{transcriptcapture.RuntimeCodex, "shell-expanded-file"})
			})
			if code != 2 {
				t.Fatalf("code = %d, want 2", code)
			}
			if !strings.Contains(stderr, "usage: witself "+test.name) {
				t.Fatalf("stderr = %q, want %s usage", stderr, test.name)
			}
		})
	}
}

func TestInstallAllRejectsProviderSpecificFlagsBeforeDiscovery(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	probeRuntimeForIntegrationCatalog = func(string) integrationDetection {
		t.Fatal("runtime discovery should not run for rejected flags")
		return integrationDetection{}
	}
	installOneIntegration = func([]string) int {
		t.Fatal("install should not run for rejected flags")
		return 0
	}

	_, stderr, code := captureIntegrationsCLI(t, func() int {
		return installAllCmd([]string{"--capture", transcriptcapture.ModeRaw})
	})
	if code != 2 {
		t.Fatalf("install all --capture code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "provider-specific") {
		t.Fatalf("stderr = %q, want provider-specific flag guidance", stderr)
	}
}

func TestUninstallAllUsesInstalledRecordsAndContinuesAfterFailure(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	probeRuntimeForIntegrationCatalog = func(string) integrationDetection {
		t.Fatal("uninstall all must not depend on runtime CLI detection")
		return integrationDetection{}
	}
	loadIntegrationForCatalog = func(runtimeName string) (transcriptcapture.Config, error) {
		switch runtimeName {
		case transcriptcapture.RuntimeCodex, transcriptcapture.RuntimeOpenClaw:
			return transcriptcapture.Config{Runtime: runtimeName}, nil
		case transcriptcapture.RuntimeCursor:
			return transcriptcapture.Config{}, errors.New("cannot read integration record")
		default:
			return transcriptcapture.Config{}, os.ErrNotExist
		}
	}
	var calls []string
	uninstallOneIntegration = func(args []string) int {
		if !suppressIntegrationSuccessOutput {
			t.Errorf("individual integration output was not suppressed for %q", args[0])
		}
		calls = append(calls, args[0])
		if args[0] == transcriptcapture.RuntimeCodex {
			return 1
		}
		return 0
	}

	stdout, _, code := captureIntegrationsCLI(t, func() int {
		return uninstallAllCmd(nil)
	})
	if code != 1 {
		t.Fatalf("uninstall all code = %d, want 1 for partial failure\n%s", code, stdout)
	}
	wantCalls := []string{transcriptcapture.RuntimeCodex, transcriptcapture.RuntimeOpenClaw}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("uninstall calls = %v, want %v", calls, wantCalls)
	}
	if suppressIntegrationSuccessOutput {
		t.Fatal("bulk uninstall did not restore individual-output suppression")
	}
	for _, want := range []string{
		"codex", "failed",
		"cursor", "cannot read integration record",
		"openclaw", "uninstalled",
		"1 succeeded, 2 failed, 4 skipped",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("uninstall output missing %q:\n%s", want, stdout)
		}
	}
}

func TestUninstallAllRecoversPendingUninstallWithoutIntegrationRecord(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	loadIntegrationForCatalog = func(string) (transcriptcapture.Config, error) {
		return transcriptcapture.Config{}, os.ErrNotExist
	}
	pendingTransactionForCatalog = func(runtimeName string, persisted *transcriptcapture.Config) (string, bool, error) {
		if persisted != nil {
			t.Fatalf("pending transaction inspection for %s received absent persisted config %#v", runtimeName, persisted)
		}
		if runtimeName == transcriptcapture.RuntimeOpenClaw {
			return openClawTransactionUninstall, true, nil
		}
		return "", false, nil
	}
	var calls []string
	uninstallOneIntegration = func(args []string) int {
		if !suppressIntegrationSuccessOutput {
			t.Errorf("individual integration output was not suppressed for %q", args[0])
		}
		calls = append(calls, args...)
		return 0
	}

	stdout, stderr, code := captureIntegrationsCLI(t, func() int {
		return uninstallAllCmd(nil)
	})
	if code != 0 {
		t.Fatalf("uninstall all code = %d, stderr = %q\n%s", code, stderr, stdout)
	}
	if want := []string{transcriptcapture.RuntimeOpenClaw}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("uninstall calls = %v, want %v", calls, want)
	}
	if !strings.Contains(stdout, "openclaw") || !strings.Contains(stdout, "uninstalled") ||
		!strings.Contains(stdout, "1 succeeded, 0 failed, 6 skipped") {
		t.Fatalf("unexpected pending-uninstall output:\n%s", stdout)
	}
}

func TestUninstallAllDryRunPlansPendingUninstallWithoutIntegrationRecord(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	loadIntegrationForCatalog = func(string) (transcriptcapture.Config, error) {
		return transcriptcapture.Config{}, os.ErrNotExist
	}
	pendingTransactionForCatalog = func(runtimeName string, persisted *transcriptcapture.Config) (string, bool, error) {
		if persisted != nil {
			t.Fatalf("pending transaction inspection for %s received absent persisted config %#v", runtimeName, persisted)
		}
		if runtimeName == transcriptcapture.RuntimeCodex {
			return genericProviderTransactionUninstall, true, nil
		}
		return "", false, nil
	}
	uninstallOneIntegration = func(args []string) int {
		t.Fatalf("dry run attempted pending-uninstall recovery for %v", args)
		return 1
	}

	stdout, stderr, code := captureIntegrationsCLI(t, func() int {
		return uninstallAllCmd([]string{"--dry-run"})
	})
	if code != 0 {
		t.Fatalf("uninstall all --dry-run code = %d, stderr = %q\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "codex") || !strings.Contains(stdout, "would uninstall") ||
		!strings.Contains(stdout, "1 planned, 0 failed, 6 skipped") {
		t.Fatalf("unexpected pending-uninstall dry-run output:\n%s", stdout)
	}
}

func TestUninstallAllDryRunDoesNotMutate(t *testing.T) {
	restoreIntegrationCatalogHooks(t)
	loadIntegrationForCatalog = func(runtimeName string) (transcriptcapture.Config, error) {
		if runtimeName == transcriptcapture.RuntimeOpenClaw {
			return transcriptcapture.Config{Runtime: runtimeName}, nil
		}
		return transcriptcapture.Config{}, os.ErrNotExist
	}
	mutations := 0
	uninstallOneIntegration = func([]string) int {
		mutations++
		return 0
	}

	stdout, stderr, code := captureIntegrationsCLI(t, func() int {
		return uninstallAllCmd([]string{"--dry-run"})
	})
	if code != 0 {
		t.Fatalf("uninstall all --dry-run code = %d, stderr = %q", code, stderr)
	}
	if mutations != 0 {
		t.Fatalf("dry run performed %d uninstall mutations", mutations)
	}
	if !strings.Contains(stdout, "openclaw") || !strings.Contains(stdout, "would uninstall") ||
		!strings.Contains(stdout, "1 planned, 0 failed, 6 skipped") {
		t.Fatalf("unexpected uninstall dry-run output:\n%s", stdout)
	}
}

func restoreIntegrationCatalogHooks(t *testing.T) {
	t.Helper()
	oldProbe := probeRuntimeForIntegrationCatalog
	oldLoad := loadIntegrationForCatalog
	oldValidate := validateIntegrationForCatalog
	oldStat := statIntegrationCLIForCatalog
	oldPending := pendingTransactionForCatalog
	oldInstall := installOneIntegration
	oldUninstall := uninstallOneIntegration
	oldSuppress := suppressIntegrationSuccessOutput
	oldOutput := integrationsOutput
	oldGOOS := integrationCatalogGOOS
	integrationCatalogGOOS = "darwin"
	pendingTransactionForCatalog = func(string, *transcriptcapture.Config) (string, bool, error) {
		return "", false, nil
	}
	t.Cleanup(func() {
		probeRuntimeForIntegrationCatalog = oldProbe
		loadIntegrationForCatalog = oldLoad
		validateIntegrationForCatalog = oldValidate
		statIntegrationCLIForCatalog = oldStat
		pendingTransactionForCatalog = oldPending
		installOneIntegration = oldInstall
		uninstallOneIntegration = oldUninstall
		suppressIntegrationSuccessOutput = oldSuppress
		integrationsOutput = oldOutput
		integrationCatalogGOOS = oldGOOS
	})
}

func captureIntegrationsCLI(t *testing.T, command func() int) (stdout, stderr string, code int) {
	t.Helper()
	outReader, outWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errReader, errWriter, err := os.Pipe()
	if err != nil {
		_ = outReader.Close()
		_ = outWriter.Close()
		t.Fatal(err)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outWriter, errWriter
	code = command()
	os.Stdout, os.Stderr = oldStdout, oldStderr
	_ = outWriter.Close()
	_ = errWriter.Close()
	outBytes, outErr := io.ReadAll(outReader)
	errBytes, errErr := io.ReadAll(errReader)
	_ = outReader.Close()
	_ = errReader.Close()
	if outErr != nil || errErr != nil {
		t.Fatalf("read captured output: stdout=%v stderr=%v", outErr, errErr)
	}
	return string(outBytes), string(errBytes), code
}
