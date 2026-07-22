package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

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
	wantSummary := integrationsSummary{Supported: 6, Detected: 3, Installed: 1, Attention: 2}
	if report.Summary != wantSummary {
		t.Fatalf("summary = %#v, want %#v", report.Summary, wantSummary)
	}
	if got := report.Runtimes[0].Integration; got.State != integrationStateInstalled || got.Agent != "scott" || got.Location != "home" || got.HookMode != transcriptcapture.HookModeManaged {
		t.Fatalf("codex integration = %#v", got)
	}
	if got := report.Runtimes[3].Integration; got.State != integrationStateError || !strings.Contains(got.Message, "corrupt") {
		t.Fatalf("cursor integration = %#v", got)
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
		want      string
	}{
		{name: "no selector", flags: map[string]bool{}, want: "would refresh"},
		{name: "same tuple", flags: map[string]bool{"account": true, "realm": true, "agent": true}, account: "default", realm: "default", agent: "scott", want: "would refresh"},
		{name: "account change", flags: map[string]bool{"account": true}, account: "work", want: "would rebind"},
		{name: "realm change", flags: map[string]bool{"realm": true}, realm: "other", want: "would rebind"},
		{name: "agent change", flags: map[string]bool{"agent": true}, agent: "other", want: "would rebind"},
		{name: "external token", flags: map[string]bool{"endpoint": true, "token-file": true}, endpoint: "https://example.test", tokenFile: "/tmp/token", want: "would refresh/rebind"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, _ := bulkInstallPreview(status, test.flags, test.account, test.realm, test.agent, test.endpoint, test.tokenFile)
			if got != test.want {
				t.Fatalf("action = %q, want %q", got, test.want)
			}
		})
	}

	t.Setenv("WITSELF_AGENT", "env-agent")
	if got, _ := bulkInstallPreview(status, map[string]bool{}, "", "", "", "", ""); got != "would rebind" {
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
	for _, want := range []string{"codex", "would rebind", "openclaw", "would install", "2 planned, 0 failed, 4 skipped"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, stdout)
		}
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
		"2 succeeded, 1 failed, 3 skipped",
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
		"1 succeeded, 2 failed, 3 skipped",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("uninstall output missing %q:\n%s", want, stdout)
		}
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
		!strings.Contains(stdout, "1 planned, 0 failed, 5 skipped") {
		t.Fatalf("unexpected uninstall dry-run output:\n%s", stdout)
	}
}

func restoreIntegrationCatalogHooks(t *testing.T) {
	t.Helper()
	oldProbe := probeRuntimeForIntegrationCatalog
	oldLoad := loadIntegrationForCatalog
	oldInstall := installOneIntegration
	oldUninstall := uninstallOneIntegration
	oldSuppress := suppressIntegrationSuccessOutput
	oldOutput := integrationsOutput
	t.Cleanup(func() {
		probeRuntimeForIntegrationCatalog = oldProbe
		loadIntegrationForCatalog = oldLoad
		installOneIntegration = oldInstall
		uninstallOneIntegration = oldUninstall
		suppressIntegrationSuccessOutput = oldSuppress
		integrationsOutput = oldOutput
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
