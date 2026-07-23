package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestProviderIntegrationContractClaude(t *testing.T) {
	runGenericProviderPortableLifecycleContract(t, transcriptcapture.RuntimeClaudeCode)
}

func TestProviderIntegrationContractGrok(t *testing.T) {
	runGenericProviderPortableLifecycleContract(t, transcriptcapture.RuntimeGrokBuild)
}

func TestProviderIntegrationContractCursor(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Cursor's supported Windows contract is WSL-as-Linux, not native Windows")
	}
	runGenericProviderPortableLifecycleContract(t, transcriptcapture.RuntimeCursor)
}

func runGenericProviderPortableLifecycleContract(t *testing.T, runtimeName string) {
	t.Helper()
	clearProviderCLIPathOverridesForTest(t)
	fixture := newGenericProviderTestFixture(t, runtimeName)
	isolateProviderDiscoveryPATHForTest(t)
	before := fixture.seedNonTargetConfig(t)
	witselfExecutable := genericProviderContractWitselfExecutable(t)

	tokenPath := filepath.Join(fixture.root, "agent.token")
	if err := os.WriteFile(tokenPath, []byte("generic-contract-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/self" || request.Header.Get("Authorization") != "Bearer generic-contract-token" {
			http.NotFound(writer, request)
			return
		}
		_, _ = fmt.Fprint(writer, `{"schema_version":"witself.v0","identity":{"account_id":"acc_contract","realm_id":"realm_contract","realm_name":"default","agent_id":"agent_provider_test","agent_name":"provider-test-bot"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`)
	}))
	t.Cleanup(backend.Close)

	installArgs := []string{
		runtimeName,
		"--account", "default",
		"--realm", "default",
		"--agent", "provider-test-bot",
		"--location", "home",
		"--endpoint", backend.URL,
		"--token-file", tokenPath,
	}
	if supportsTranscriptHooksForPlatform(runtimeName, runtime.GOOS) {
		installArgs = append(installArgs, "--capture", transcriptcapture.ModeRaw, "--user-hooks")
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if code := runGenericProviderContractCLI(t, witselfExecutable, "install", installArgs...); code != 0 {
			t.Fatalf("install attempt %d code = %d", attempt, code)
		}
		cfg, err := transcriptcapture.LoadConfig(runtimeName)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RuntimeCLICommand != fixture.cli || filepath.Clean(cfg.MCPCommand) != filepath.Clean(witselfExecutable) ||
			cfg.RuntimeConfigRoot != fixture.selectorRoot || cfg.RuntimeMCPConfigPath != fixture.cfg.RuntimeMCPConfigPath {
			t.Fatalf("persisted %s exact binding = %#v", runtimeName, cfg)
		}
		wantHookMode := transcriptcapture.HookModeNone
		if supportsTranscriptHooksForPlatform(runtimeName, runtime.GOOS) {
			wantHookMode = transcriptcapture.HookModeUser
		}
		if cfg.HookMode != wantHookMode {
			t.Fatalf("%s hook mode = %q, want %q", runtimeName, cfg.HookMode, wantHookMode)
		}
		if err := validateGenericInstalledTopology(cfg); err != nil {
			t.Fatalf("validate %s topology after install %d: %v", runtimeName, attempt, err)
		}
		if err := verifyRuntimeHooksOwned(cfg); err != nil {
			t.Fatalf("verify %s hooks after install %d: %v", runtimeName, attempt, err)
		}
		_, exists, current, err := inspectGenericMCP(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !exists || current.nonTarget != before.nonTarget {
			t.Fatalf("%s install %d lost target or changed sibling semantics", runtimeName, attempt)
		}
		assertGenericProviderContractVerification(
			t,
			witselfExecutable,
			runtimeName,
			integrationVerificationHealthy,
		)
	}
	if code := runGenericProviderContractCLI(t, witselfExecutable, "uninstall", runtimeName); code != 0 {
		t.Fatalf("uninstall code = %d", code)
	}
	_, exists, after, err := inspectGenericMCP(fixture.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if exists || after.nonTarget != before.nonTarget {
		t.Fatalf("%s uninstall retained target or changed sibling semantics", runtimeName)
	}
	if _, err := transcriptcapture.LoadConfig(runtimeName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s integration config remains: %v", runtimeName, err)
	}
	assertGenericProviderContractVerification(
		t,
		witselfExecutable,
		runtimeName,
		integrationVerificationNotInstalled,
	)
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("%s lifecycle removed the token fixture: %v", runtimeName, err)
	}
}

func assertGenericProviderContractVerification(
	t *testing.T,
	witselfExecutable string,
	runtimeName string,
	wantState string,
) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(installedCommandAcceptanceBinaryEnv)) == "" {
		status := inspectIntegrationRuntime(runtimeName, true)
		if status.Verification == nil || status.Verification.State != wantState {
			t.Fatalf("%s verification = %#v, want %s", runtimeName, status, wantState)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, witselfExecutable, "integrations", "--verify", "--json")
	process.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = &stderr
	if err := process.Run(); err != nil {
		if ctx.Err() != nil {
			t.Fatalf("installed Witself integrations --verify timed out: %v\n%s", ctx.Err(), stderr.Bytes())
		}
		t.Fatalf("installed Witself integrations --verify failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.Bytes(), stderr.Bytes())
	}
	var report integrationsReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode installed integrations --verify report: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.Bytes(), stderr.Bytes())
	}
	if report.SchemaVersion != integrationsSchemaVersion {
		t.Fatalf("installed integrations schema = %q, want %q", report.SchemaVersion, integrationsSchemaVersion)
	}
	for _, status := range report.Runtimes {
		if status.Runtime != runtimeName {
			continue
		}
		if status.Verification == nil || status.Verification.State != wantState {
			t.Fatalf("installed %s verification = %#v, want %s", runtimeName, status, wantState)
		}
		return
	}
	t.Fatalf("installed integrations --verify report omitted runtime %s", runtimeName)
}

func runGenericProviderContractCLI(t *testing.T, witselfExecutable, command string, args ...string) int {
	t.Helper()
	if strings.TrimSpace(os.Getenv(installedCommandAcceptanceBinaryEnv)) == "" {
		switch command {
		case "install":
			return installCmd(args)
		case "uninstall":
			return uninstallCmd(args)
		default:
			t.Fatalf("unsupported generic-provider contract command %q", command)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	commandArgs := append([]string{command}, args...)
	process := exec.CommandContext(ctx, witselfExecutable, commandArgs...)
	process.Env = os.Environ()
	output, err := process.CombinedOutput()
	if err == nil {
		return 0
	}
	if ctx.Err() != nil {
		t.Fatalf("installed Witself %s timed out: %v\n%s", command, ctx.Err(), output)
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		t.Logf("installed Witself %s exited %d:\n%s", command, exitError.ExitCode(), output)
		return exitError.ExitCode()
	}
	// Failure to launch the supplied artifact is not a provider contract exit;
	// surface it directly instead of mapping it to a synthetic command code.
	t.Fatalf("launch installed Witself %s: %v\n%s", command, err, output)
	return -1
}

func genericProviderContractWitselfExecutable(t *testing.T) string {
	t.Helper()
	path := os.Getenv(installedCommandAcceptanceBinaryEnv)
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			t.Fatal(err)
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		t.Fatalf("inspect lifecycle Witself command %s: %v", absolute, err)
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		t.Fatalf("lifecycle Witself command %s is not an executable regular file", absolute)
	}
	t.Setenv(witselfExecutableTestEnv, absolute)
	return absolute
}
