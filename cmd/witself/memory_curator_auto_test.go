package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/memorycurator"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestMemoryCuratorAutoEnableStatusAndDisable(t *testing.T) {
	fixture := newCuratorDriverFixture(t)
	fixture.accessProfile = "full"
	server := httptest.NewServer(fixture)
	defer server.Close()
	binding, providerPath := setupAutomaticCuratorCLI(t, server.URL)

	stdout, stderr, code := runMemoryCuratorAutoCLI(t, []string{
		"enable", "--runtime", binding.Runtime, "--provider", "claude-code",
		"--provider-path", providerPath, "--allow-transcript-content",
		"--debounce", "1s", "--minimum-interval", "0s", "--max-runs", "2", "--json",
	})
	if code != 0 || stderr != "" {
		t.Fatalf("enable = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var config memorycurator.AutoConfig
	if err := json.Unmarshal([]byte(stdout), &config); err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.AgentID != binding.AgentID || config.Provider != memorycurator.ProviderClaudeCode ||
		config.ProviderPath != providerPath || config.ApplyPolicy != memorycurator.ApplyPolicyPreview ||
		!config.AllowTranscriptContent || config.MaxRuns != 2 {
		t.Fatalf("config = %#v", config)
	}
	if fixture.count("preflight") != 1 {
		t.Fatalf("preflight calls = %#v", fixture.calls)
	}

	stdout, stderr, code = runMemoryCuratorAutoCLI(t, []string{"status", "--runtime", binding.Runtime, "--json"})
	if code != 0 || stderr != "" {
		t.Fatalf("status = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var inspection memorycurator.AutoInspection
	if err := json.Unmarshal([]byte(stdout), &inspection); err != nil {
		t.Fatal(err)
	}
	if !inspection.Configured || !inspection.Config.Enabled || inspection.Status.State != memorycurator.AutoStateIdle {
		t.Fatalf("inspection = %#v", inspection)
	}

	stdout, stderr, code = runMemoryCuratorAutoCLI(t, []string{"disable", "--runtime", binding.Runtime, "--json"})
	if code != 0 || stderr != "" {
		t.Fatalf("disable = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.Config.Enabled || inspection.Status.State != memorycurator.AutoStateDisabled {
		t.Fatalf("disabled inspection = %#v", inspection)
	}
}

func TestMemoryCuratorAutoRequiresExplicitTranscriptAndApplyAuthority(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "transcript authorization",
			args: []string{"enable", "--runtime", "claude-code", "--provider", "claude-code"},
			want: "--allow-transcript-content is required",
		},
		{
			name: "apply confirmation",
			args: []string{"enable", "--runtime", "claude-code", "--provider", "claude-code", "--allow-transcript-content", "--policy", "apply"},
			want: "--policy apply and --yes",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, code := runMemoryCuratorAutoCLI(t, test.args)
			if code != 2 || stdout != "" || !strings.Contains(stderr, test.want) {
				t.Fatalf("run = %d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestMemoryCuratorAutoRequiresFullInstalledCredential(t *testing.T) {
	fixture := newCuratorDriverFixture(t)
	server := httptest.NewServer(fixture)
	defer server.Close()
	binding, providerPath := setupAutomaticCuratorCLI(t, server.URL)
	stdout, stderr, code := runMemoryCuratorAutoCLI(t, []string{
		"enable", "--runtime", binding.Runtime, "--provider", "claude-code",
		"--provider-path", providerPath, "--allow-transcript-content",
	})
	if code != 1 || stdout != "" || !strings.Contains(stderr, "requires full credential profile") {
		t.Fatalf("enable = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	store, err := memorycurator.DefaultAutoStore(binding.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection, err := store.Inspect(); err != nil || inspection.Configured {
		t.Fatalf("restricted enable persisted config: %#v / %v", inspection, err)
	}
}

func TestMemoryCuratorAutoRunNoWorkAcknowledgesWakeWithoutPlannerInput(t *testing.T) {
	fixture := newCuratorDriverFixture(t)
	fixture.accessProfile = "full"
	fixture.noWork = true
	server := httptest.NewServer(fixture)
	defer server.Close()
	binding, providerPath := setupAutomaticCuratorCLI(t, server.URL)
	store, err := memorycurator.DefaultAutoStore(binding.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enable(memorycurator.AutoSettings{
		AccountID: binding.AccountID, RealmID: binding.RealmID, AgentID: binding.AgentID,
		Provider: memorycurator.ProviderClaudeCode, ProviderPath: providerPath,
		ApplyPolicy: memorycurator.ApplyPolicyPreview, AllowTranscriptContent: true,
		Debounce: time.Second, MinimumInterval: 0, MaxRuns: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordWake(memorycurator.AutoWakeManualPoll); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runMemoryCuratorAutoCLI(t, []string{"run", "--runtime", binding.Runtime, "--json"})
	if code != 0 || stderr != "" {
		t.Fatalf("run = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var result memorycurator.AutoRunResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Acquired || !result.Attempted || result.Outcome != memorycurator.AutoOutcomeNoWork || result.PendingWakeCount != 0 {
		t.Fatalf("result = %#v", result)
	}
	if fixture.count("preflight") != 1 || fixture.count("requests") != 1 || fixture.count("start") != 0 {
		t.Fatalf("calls = %#v", fixture.calls)
	}
	if strings.Contains(stdout+stderr, curatorDriverTokenCanary) {
		t.Fatal("automatic run exposed the installed credential")
	}
}

func TestMemoryCuratorAutoWakeRecordsWorkBeforeRunning(t *testing.T) {
	fixture := newCuratorDriverFixture(t)
	fixture.accessProfile = "full"
	fixture.noWork = true
	server := httptest.NewServer(fixture)
	defer server.Close()
	binding, providerPath := setupAutomaticCuratorCLI(t, server.URL)
	store, err := memorycurator.DefaultAutoStore(binding.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enable(memorycurator.AutoSettings{
		AccountID: binding.AccountID, RealmID: binding.RealmID, AgentID: binding.AgentID,
		Provider: memorycurator.ProviderClaudeCode, ProviderPath: providerPath,
		ApplyPolicy: memorycurator.ApplyPolicyPreview, AllowTranscriptContent: true,
		Debounce: time.Second, MinimumInterval: 0, MaxRuns: 1,
	}); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runMemoryCuratorAutoCLI(t, []string{"wake", "--runtime", binding.Runtime, "--json"})
	if code != 0 || stderr != "" {
		t.Fatalf("wake = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var result memorycurator.AutoRunResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Attempted || result.Outcome != memorycurator.AutoOutcomeNoWork || fixture.count("requests") != 1 {
		t.Fatalf("wake did not record and service work: result=%#v calls=%#v", result, fixture.calls)
	}
	if markers, err := store.PendingWakes(); err != nil || len(markers) != 0 {
		t.Fatalf("wake markers = %#v / %v", markers, err)
	}
}

func TestAutomaticCuratorSupervisorRetriesThenDrainsTwoRequests(t *testing.T) {
	clock := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	store, err := memorycurator.NewAutoStore(t.TempDir(), "agent_auto_supervisor")
	if err != nil {
		t.Fatal(err)
	}
	store.Now = func() time.Time { return clock }
	var sleeps []time.Duration
	store.Sleep = func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		sleeps = append(sleeps, delay)
		clock = clock.Add(delay)
		return nil
	}
	if _, err := store.Enable(memorycurator.AutoSettings{
		AgentID: "agent_auto_supervisor", Provider: memorycurator.ProviderClaudeCode,
		ProviderPath: filepath.Join(t.TempDir(), "claude"), ApplyPolicy: memorycurator.ApplyPolicyPreview,
		AllowTranscriptContent: true, Debounce: time.Second, MinimumInterval: 0, MaxRuns: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordWake(memorycurator.AutoWakeTerminalFlush); err != nil {
		t.Fatal(err)
	}

	calls := 0
	result, err := runAutomaticCuratorSupervisor(context.Background(), store, func(context.Context, memorycurator.AutoConfig) (memorycurator.AutoWorkResult, error) {
		calls++
		switch calls {
		case 1:
			return memorycurator.AutoWorkResult{}, memorycurator.NewAutoWorkError(
				memorycurator.AutoFailureWorker, errors.New("transient fake failure"))
		case 2, 3:
			return memorycurator.AutoWorkResult{Outcome: memorycurator.AutoOutcomePreviewed, MoreWork: true}, nil
		default:
			return memorycurator.AutoWorkResult{Outcome: memorycurator.AutoOutcomeNoWork}, nil
		}
	})
	if err != nil {
		t.Fatalf("supervisor error = %v", err)
	}
	if calls != 4 || result.Runs != 3 || result.Outcome != memorycurator.AutoOutcomePreviewed ||
		result.MoreWork || result.PendingWakeCount != 0 {
		t.Fatalf("result=%#v calls=%d", result, calls)
	}
	if len(sleeps) != 2 || sleeps[0] != time.Second || sleeps[1] != time.Minute {
		t.Fatalf("persisted timing sleeps = %v", sleeps)
	}
	inspection, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.PendingWakeCount != 0 || inspection.Status.State != memorycurator.AutoStateIdle ||
		inspection.Status.ConsecutiveFailures != 0 || inspection.Status.RetryNotBefore != nil ||
		inspection.Status.TotalRuns != 3 {
		t.Fatalf("inspection = %#v", inspection)
	}
}

func TestAutomaticCuratorSupervisorBoundsFailuresAndPreservesWake(t *testing.T) {
	clock := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	store, err := memorycurator.NewAutoStore(t.TempDir(), "agent_auto_bounded")
	if err != nil {
		t.Fatal(err)
	}
	store.Now = func() time.Time { return clock }
	store.Sleep = func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		clock = clock.Add(delay)
		return nil
	}
	if _, err := store.Enable(memorycurator.AutoSettings{
		AgentID: "agent_auto_bounded", Provider: memorycurator.ProviderClaudeCode,
		ProviderPath: filepath.Join(t.TempDir(), "claude"), ApplyPolicy: memorycurator.ApplyPolicyPreview,
		AllowTranscriptContent: true, Debounce: time.Second, MinimumInterval: 0, MaxRuns: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordWake(memorycurator.AutoWakeTerminalFlush); err != nil {
		t.Fatal(err)
	}
	calls := 0
	result, err := runAutomaticCuratorSupervisor(context.Background(), store, func(context.Context, memorycurator.AutoConfig) (memorycurator.AutoWorkResult, error) {
		calls++
		return memorycurator.AutoWorkResult{}, memorycurator.NewAutoWorkError(
			memorycurator.AutoFailureWorker, errors.New("persistent fake failure"))
	})
	if err == nil || calls != automaticCuratorSupervisorMaxPasses || result.PendingWakeCount != 1 {
		t.Fatalf("bounded result=%#v calls=%d err=%v", result, calls, err)
	}
	inspection, inspectErr := store.Inspect()
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if inspection.PendingWakeCount != 1 || inspection.Status.ConsecutiveFailures != automaticCuratorSupervisorMaxPasses ||
		inspection.Status.RetryNotBefore == nil {
		t.Fatalf("inspection = %#v", inspection)
	}
}

func TestAutomaticCuratorAcceptsValidatedRecoveredApply(t *testing.T) {
	config := memorycurator.AutoConfig{ApplyPolicy: memorycurator.ApplyPolicyApply}
	result, err := automaticCuratorWorkResult(memorycurator.Result{
		Run: &client.MemoryCurationRun{State: "applied", ApplyReceiptID: "mrec_recovered"},
	}, config)
	if err != nil || result.Outcome != memorycurator.AutoOutcomeApplied || !result.MoreWork {
		t.Fatalf("recovered apply = %#v / %v", result, err)
	}
	if _, err := automaticCuratorWorkResult(memorycurator.Result{
		Run: &client.MemoryCurationRun{State: "applied"},
	}, config); err == nil {
		t.Fatal("applied run without receipt was accepted")
	}
}

func setupAutomaticCuratorCLI(t *testing.T, endpoint string) (transcriptcapture.Config, string) {
	t.Helper()
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	tokenPath := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenPath, []byte(curatorDriverTokenCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	binding := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeClaudeCode, CaptureMode: transcriptcapture.ModeRaw,
		Account: "default", AccountID: "acc_driver", Realm: "default", RealmID: "realm_driver",
		Agent: "driver", AgentID: "agent_driver", AgentName: "driver",
		Endpoint: endpoint, TokenFile: tokenPath, Location: location,
	}
	if err := transcriptcapture.SaveConfig(binding); err != nil {
		t.Fatal(err)
	}
	providerDir := t.TempDir()
	providerPath := writeFakeNativeProvider(t, providerDir, `
if [ "$#" -eq 1 ] && [ "$1" = "--version" ]; then
  printf '%s\n' '99.0.0 (Claude Code)'
  exit 0
fi
if [ "$#" -eq 1 ] && [ "$1" = "--help" ]; then
  printf '%s\n' '--print --input-format --output-format --safe-mode --no-session-persistence --disable-slash-commands --strict-mcp-config --mcp-config --tools --permission-mode --no-chrome --model'
  exit 0
fi
printf '%s' '{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}'
`)
	return binding, providerPath
}

func runMemoryCuratorAutoCLI(t *testing.T, args []string) (stdout, stderr string, code int) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code = memoryCurateAuto(args)
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()
	outRaw, outReadErr := io.ReadAll(outR)
	errRaw, errReadErr := io.ReadAll(errR)
	_ = outR.Close()
	_ = errR.Close()
	if outReadErr != nil || errReadErr != nil {
		t.Fatalf("capture output: %v, %v", outReadErr, errReadErr)
	}
	return string(outRaw), string(errRaw), code
}
