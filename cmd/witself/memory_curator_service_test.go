package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/memorycurator"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type fakeAutomaticCuratorServiceLifecycle struct {
	calls []string
}

func (f *fakeAutomaticCuratorServiceLifecycle) Install(_ context.Context, runtimeName string) (memorycurator.CuratorServiceStatus, error) {
	f.calls = append(f.calls, "install:"+runtimeName)
	return fakeAutomaticCuratorServiceStatus(runtimeName, true), nil
}

func (f *fakeAutomaticCuratorServiceLifecycle) Status(_ context.Context, runtimeName string) (memorycurator.CuratorServiceStatus, error) {
	f.calls = append(f.calls, "status:"+runtimeName)
	return fakeAutomaticCuratorServiceStatus(runtimeName, true), nil
}

func (f *fakeAutomaticCuratorServiceLifecycle) Start(_ context.Context, runtimeName string) (memorycurator.CuratorServiceStatus, error) {
	f.calls = append(f.calls, "start:"+runtimeName)
	return fakeAutomaticCuratorServiceStatus(runtimeName, true), nil
}

func (f *fakeAutomaticCuratorServiceLifecycle) Uninstall(_ context.Context, runtimeName string) (memorycurator.CuratorServiceStatus, error) {
	f.calls = append(f.calls, "uninstall:"+runtimeName)
	return fakeAutomaticCuratorServiceStatus(runtimeName, false), nil
}

func fakeAutomaticCuratorServiceStatus(runtimeName string, installed bool) memorycurator.CuratorServiceStatus {
	return memorycurator.CuratorServiceStatus{
		Schema:   memorycurator.CuratorServiceStatusSchemaV1,
		Platform: memorycurator.CuratorServicePlatformDarwin, Runtime: runtimeName,
		Installed: installed, Enabled: installed, Active: installed,
		Paths: []string{"/Users/test/Library/LaunchAgents/managed.plist"},
	}
}

func TestMemoryCuratorAutoServiceLifecycleUsesEnabledBindingAndNeverNeedsHostServices(t *testing.T) {
	binding := setupAutomaticCuratorServiceBinding(t, true)
	fake := &fakeAutomaticCuratorServiceLifecycle{}
	oldFactory := automaticCuratorServiceFactory
	t.Cleanup(func() { automaticCuratorServiceFactory = oldFactory })
	var factoryExecutable string
	automaticCuratorServiceFactory = func(executable string) (automaticCuratorServiceLifecycle, error) {
		factoryExecutable = executable
		return fake, nil
	}

	stdout, stderr, code := runMemoryCuratorAutoCLI(t, []string{
		"service", "install", "--runtime", binding.Runtime, "--json",
	})
	if code != 0 || stderr != "" {
		t.Fatalf("install = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var status memorycurator.CuratorServiceStatus
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !status.Enabled || !status.Active || status.Runtime != binding.Runtime {
		t.Fatalf("install status = %#v", status)
	}
	if want := filepath.Join(os.Getenv("WITSELF_HOME"), "bin", "witself"); factoryExecutable != want {
		t.Fatalf("factory executable = %q, want %q", factoryExecutable, want)
	}

	stdout, stderr, code = runMemoryCuratorAutoCLI(t, []string{"service", "start", "--runtime", binding.Runtime})
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "installed\t") {
		t.Fatalf("start = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	// Status and uninstall must remain available after an integration binding
	// is removed so stale user services can always be cleaned up safely.
	if err := transcriptcapture.RemoveConfig(binding.Runtime); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = runMemoryCuratorAutoCLI(t, []string{"service", "status", "--runtime", binding.Runtime})
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "installed\t") {
		t.Fatalf("status = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runMemoryCuratorAutoCLI(t, []string{"service", "uninstall", "--runtime", binding.Runtime})
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "uninstalled\t") {
		t.Fatalf("uninstall = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if got, want := strings.Join(fake.calls, ","),
		"install:claude-code,start:claude-code,status:claude-code,uninstall:claude-code"; got != want {
		t.Fatalf("lifecycle calls = %q, want %q", got, want)
	}
}

func TestMemoryCuratorAutoServiceInstallAndStartRequireEnabledExactBinding(t *testing.T) {
	binding := setupAutomaticCuratorServiceBinding(t, false)
	fake := &fakeAutomaticCuratorServiceLifecycle{}
	oldFactory := automaticCuratorServiceFactory
	t.Cleanup(func() { automaticCuratorServiceFactory = oldFactory })
	automaticCuratorServiceFactory = func(string) (automaticCuratorServiceLifecycle, error) { return fake, nil }

	for _, command := range []string{"install", "start"} {
		stdout, stderr, code := runMemoryCuratorAutoCLI(t, []string{"service", command, "--runtime", binding.Runtime})
		if code != 1 || stdout != "" || !strings.Contains(stderr, "enable automatic curation") {
			t.Fatalf("%s = %d stdout=%q stderr=%q", command, code, stdout, stderr)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("host lifecycle called without enabled binding: %#v", fake.calls)
	}
}

func TestMemoryCuratorAutoServiceRejectsUnknownLifecycleCommand(t *testing.T) {
	stdout, stderr, code := runMemoryCuratorAutoCLI(t, []string{"service", "restart", "--runtime", "claude-code"})
	if code != 2 || stdout != "" || !strings.Contains(stderr, "unknown subcommand") {
		t.Fatalf("unknown = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func setupAutomaticCuratorServiceBinding(t *testing.T, enabled bool) transcriptcapture.Config {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	executable := filepath.Join(home, "bin", "witself")
	t.Setenv(witselfExecutableTestEnv, executable)
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	binding := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeClaudeCode, CaptureMode: transcriptcapture.ModeRaw,
		Account: "default", AccountID: "acc_service", Realm: "default", RealmID: "realm_service",
		Agent: "service", AgentID: "agent_service", AgentName: "service", Location: location,
	}
	if err := transcriptcapture.SaveConfig(binding); err != nil {
		t.Fatal(err)
	}
	if enabled {
		store, err := memorycurator.DefaultAutoStore(binding.AgentID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Enable(memorycurator.AutoSettings{
			AccountID: binding.AccountID, RealmID: binding.RealmID, AgentID: binding.AgentID,
			Provider: memorycurator.ProviderClaudeCode, ProviderPath: filepath.Join(home, "bin", "claude"),
			ApplyPolicy: memorycurator.ApplyPolicyPreview, AllowTranscriptContent: true,
			Debounce: time.Second, MinimumInterval: 0, MaxRuns: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return binding
}
