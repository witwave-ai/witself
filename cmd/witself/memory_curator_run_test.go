package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

const curatorDriverTokenCanary = "witself_agt_curator_driver_secret_canary"

func TestMemoryCuratorRunNoWorkDoesNotLaunchPlanner(t *testing.T) {
	fixture := newCuratorDriverFixture(t)
	fixture.noWork = true
	server := httptest.NewServer(fixture)
	defer server.Close()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("CURATOR_HELPER_MUST_NOT_RUN", "1")

	stdout, stderr, code := runMemoryCuratorCLI(t, append(
		curatorDriverConnectionArgs(t, server.URL), curatorPlannerCommand("must-not-run")...,
	))
	if code != 0 || stderr != "" {
		t.Fatalf("run = %d stderr=%q", code, stderr)
	}
	var summary curatorRunSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Schema != curatorRunResultSchemaV1 || summary.Status != "no_work" || summary.LaunchID != "" {
		t.Fatalf("summary = %#v", summary)
	}
	if fixture.count("requests") != 1 || fixture.count("start") != 0 {
		t.Fatalf("calls = %#v", fixture.calls)
	}
}

func TestMemoryCuratorRunPreviewRequeuesAndKeepsCredentialOutOfPlanner(t *testing.T) {
	fixture := newCuratorDriverFixture(t)
	server := httptest.NewServer(fixture)
	defer server.Close()
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_DRIVER_TOKEN", curatorDriverTokenCanary)
	injectionPath := filepath.Join(t.TempDir(), "shell-was-used")
	dangerous := "$(touch " + injectionPath + ")"

	args := curatorDriverConnectionArgs(t, server.URL)
	args = append(args, "--client-runtime", "test-runtime", "--client-model", "test-model", "--client-recipe", "test-recipe", "--client-recipe-version", "v1")
	args = append(args, curatorPlannerCommand("valid", dangerous)...)
	stdout, stderr, code := runMemoryCuratorCLI(t, args)
	if code != 0 || stderr != "" {
		t.Fatalf("run = %d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(injectionPath); !os.IsNotExist(err) {
		t.Fatalf("planner argument was interpreted by a shell: %v", err)
	}
	var summary curatorRunSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Status != "previewed" || !summary.PreviewRequeued || summary.LaunchID == "" ||
		summary.RequestID != fixture.request.ID || summary.RunID != fixture.run.ID || summary.PlanRevision != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	if fixture.count("preflight") != 1 || fixture.count("plan") != 1 || fixture.count("abandon") != 1 || fixture.count("apply") != 0 {
		t.Fatalf("calls = %#v", fixture.calls)
	}
	if strings.Contains(stdout+stderr, curatorDriverTokenCanary) || strings.Contains(stdout+stderr, fixture.inputContent) {
		t.Fatalf("CLI output exposed credential or input: stdout=%q stderr=%q", stdout, stderr)
	}
	stateFiles, err := filepath.Glob(filepath.Join(home, "curation", fixture.agentID, "*.json"))
	if err != nil || len(stateFiles) != 1 {
		t.Fatalf("state files = %v, %v", stateFiles, err)
	}
	stateRaw, err := os.ReadFile(stateFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateRaw), curatorDriverTokenCanary) || strings.Contains(string(stateRaw), fixture.inputContent) {
		t.Fatalf("launch state exposed credential or input: %s", stateRaw)
	}
	info, err := os.Stat(stateFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state mode = %v", info.Mode().Perm())
	}
}

func TestMemoryCuratorRunNativeProviderUsesSafePromptTransportAndModel(t *testing.T) {
	fixture := newCuratorDriverFixture(t)
	fixture.wantClientRuntime = "claude-code"
	fixture.wantClientModel = "claude-model_1"
	server := httptest.NewServer(fixture)
	defer server.Close()
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_DRIVER_TOKEN", curatorDriverTokenCanary)
	nativeDir := t.TempDir()
	argsLog := filepath.Join(nativeDir, "args.log")
	promptLog := filepath.Join(nativeDir, "prompt.log")
	t.Setenv("NATIVE_CLI_ARGS_LOG", argsLog)
	t.Setenv("NATIVE_CLI_PROMPT_LOG", promptLog)
	providerPath := writeFakeNativeProvider(t, nativeDir, `
if [ "$#" -eq 1 ] && [ "$1" = "--version" ]; then
  printf '%s\n' '99.0.0 (Claude Code)'
  exit 0
fi
if [ "$#" -eq 1 ] && [ "$1" = "--help" ]; then
  printf '%s\n' '--print --input-format --output-format --safe-mode --no-session-persistence --disable-slash-commands --strict-mcp-config --mcp-config --tools --permission-mode --no-chrome --model'
  exit 0
fi
: > "$NATIVE_CLI_ARGS_LOG"
for argument in "$@"; do
  printf '%s\n' "$argument" >> "$NATIVE_CLI_ARGS_LOG"
done
cat > "$NATIVE_CLI_PROMPT_LOG"
printf '%s' '{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}'
`)

	args := curatorDriverConnectionArgs(t, server.URL)
	args = append(args, "--provider", "claude-code", "--provider-path", providerPath, "--client-model", "claude-model_1")
	stdout, stderr, code := runMemoryCuratorCLI(t, args)
	if code != 0 || stderr != "" {
		t.Fatalf("native run = %d stderr=%q", code, stderr)
	}
	var summary curatorRunSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Status != "previewed" || fixture.count("plan") != 1 || fixture.count("abandon") != 1 {
		t.Fatalf("summary=%#v calls=%#v", summary, fixture.calls)
	}
	argsRaw, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	promptRaw, err := os.ReadFile(promptLog)
	if err != nil {
		t.Fatal(err)
	}
	arguments := string(argsRaw)
	prompt := string(promptRaw)
	if !strings.Contains(arguments, "--model\nclaude-model_1\n") || !strings.Contains(arguments, "--safe-mode\n") {
		t.Fatalf("native provider arguments = %q", arguments)
	}
	for _, private := range []string{curatorDriverTokenCanary, fixture.inputContent, fixture.request.ID, "BEGIN_UNTRUSTED_PLANNER_ENVELOPE"} {
		if strings.Contains(arguments, private) {
			t.Fatalf("planner content appeared in argv: %q", arguments)
		}
	}
	if !strings.Contains(prompt, "BEGIN_UNTRUSTED_PLANNER_ENVELOPE") || !strings.Contains(prompt, fixture.inputContent) {
		t.Fatalf("native provider prompt did not contain the bounded envelope")
	}
	if strings.Contains(prompt, curatorDriverTokenCanary) || strings.Contains(stdout+stderr, curatorDriverTokenCanary) {
		t.Fatal("native provider path exposed the curator credential")
	}
}

func TestMemoryCuratorRunNativeProviderSelectionValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "provider and command",
			args: []string{"--provider", "claude-code", "--", "/bin/true"},
			want: "--provider and a planner command are mutually exclusive",
		},
		{
			name: "path without provider",
			args: []string{"--provider-path", "/bin/true", "--", "/bin/true"},
			want: "--provider-path requires --provider",
		},
		{
			name: "unknown provider",
			args: []string{"--provider", "unknown"},
			want: "--provider must be codex, claude-code, grok-build, or cursor",
		},
		{
			name: "provider and planner dir",
			args: []string{"--provider", "claude-code", "--planner-dir", t.TempDir()},
			want: "--planner-dir is valid only with an explicit planner command",
		},
		{
			name: "unsafe model argument",
			args: []string{"--provider", "claude-code", "--client-model=--permission-mode"},
			want: "--client-model is not a valid native provider model name",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, code := runMemoryCuratorCLI(t, test.args)
			if code != 2 || stdout != "" || !strings.Contains(stderr, test.want) {
				t.Fatalf("run = %d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestValidateCuratorRunExpectedBindingRequiresExactFullIdentity(t *testing.T) {
	preflight := &client.MemoryCurationPreflight{}
	preflight.Principal.AccountID = "acc_1"
	preflight.Principal.RealmID = "rlm_1"
	preflight.Principal.AgentID = "agt_1"
	preflight.Credential.AccessProfile = "full"
	want := &curatorRunExpectedBinding{
		AccountID: "acc_1", RealmID: "rlm_1", AgentID: "agt_1", AccessProfile: "full",
	}
	if err := validateCuratorRunExpectedBinding(preflight, want); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*client.MemoryCurationPreflight){
		func(value *client.MemoryCurationPreflight) { value.Principal.AccountID = "acc_other" },
		func(value *client.MemoryCurationPreflight) { value.Principal.RealmID = "rlm_other" },
		func(value *client.MemoryCurationPreflight) { value.Principal.AgentID = "agt_other" },
		func(value *client.MemoryCurationPreflight) { value.Credential.AccessProfile = "curator-apply" },
	} {
		candidate := *preflight
		mutate(&candidate)
		if err := validateCuratorRunExpectedBinding(&candidate, want); err == nil {
			t.Fatalf("binding unexpectedly accepted: %#v", candidate)
		}
	}
	if err := validateCuratorRunExpectedBinding(preflight, nil); err != nil {
		t.Fatalf("manual runner binding = %v", err)
	}
}

func TestMemoryCuratorRunNativeProviderFailsClosedBeforeClaimingWork(t *testing.T) {
	fixture := newCuratorDriverFixture(t)
	server := httptest.NewServer(fixture)
	defer server.Close()
	t.Setenv("WITSELF_HOME", t.TempDir())
	nativeDir := t.TempDir()
	providerPath := writeFakeNativeProvider(t, nativeDir, `
if [ "$#" -eq 1 ] && [ "$1" = "--version" ]; then
  printf '%s\n' '2099.01.01'
  exit 0
fi
if [ "$#" -eq 1 ] && [ "$1" = "--help" ]; then
  printf '%s\n' '--print --mode plan'
  exit 0
fi
exit 91
`)

	args := curatorDriverConnectionArgs(t, server.URL)
	args = append(args, "--provider", "cursor", "--provider-path", providerPath)
	stdout, stderr, code := runMemoryCuratorCLI(t, args)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "native provider cursor is unavailable") {
		t.Fatalf("run = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if fixture.count("preflight") != 1 || fixture.count("requests") != 0 || fixture.count("start") != 0 {
		t.Fatalf("unsupported native provider claimed work: %#v", fixture.calls)
	}
	if strings.Contains(stderr, curatorDriverTokenCanary) || strings.Contains(stderr, fixture.inputContent) {
		t.Fatalf("unsupported provider error exposed private data: %q", stderr)
	}
}

func TestMemoryCuratorRunApplyRequiresConfirmationAndPermission(t *testing.T) {
	fixture := newCuratorDriverFixture(t)
	fixture.allowApply = true
	server := httptest.NewServer(fixture)
	defer server.Close()
	t.Setenv("WITSELF_HOME", t.TempDir())

	base := curatorDriverConnectionArgs(t, server.URL)
	stdout, stderr, code := runMemoryCuratorCLI(t, append(append([]string{}, base...), append([]string{"--apply"}, curatorPlannerCommand("valid")...)...))
	if code != 2 || stdout != "" || !strings.Contains(stderr, "--apply and --yes") || fixture.count("preflight") != 0 {
		t.Fatalf("unguarded apply = %d stdout=%q stderr=%q calls=%#v", code, stdout, stderr, fixture.calls)
	}

	args := append(append([]string{}, base...), "--apply", "--yes")
	args = append(args, curatorPlannerCommand("valid")...)
	stdout, stderr, code = runMemoryCuratorCLI(t, args)
	if code != 0 || stderr != "" {
		t.Fatalf("apply = %d stderr=%q", code, stderr)
	}
	var summary curatorRunSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Status != "applied" || summary.ApplyReceiptID != "mrec_apply" || fixture.count("get-plan") != 1 || fixture.count("apply") != 1 || fixture.count("abandon") != 0 {
		t.Fatalf("summary=%#v calls=%#v", summary, fixture.calls)
	}
}

func TestMemoryCuratorRunPreflightRejectsPermissionAndProtocolMismatches(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*curatorDriverFixture)
		apply  bool
	}{
		{name: "missing list", mutate: func(f *curatorDriverFixture) { f.allowList = false }},
		{name: "missing plan review", apply: true, mutate: func(f *curatorDriverFixture) { f.allowGetPlan = false }},
		{name: "missing apply", apply: true, mutate: func(f *curatorDriverFixture) { f.allowApply = false }},
		{name: "wrong schema", mutate: func(f *curatorDriverFixture) { f.planSchema = "witself.memory-plan.v2" }},
		{name: "backend inference", mutate: func(f *curatorDriverFixture) { f.backendInference = true }},
		{name: "client inference optional", mutate: func(f *curatorDriverFixture) { f.clientInferenceRequired = false }},
		{name: "primitive mismatch", mutate: func(f *curatorDriverFixture) { f.primitives = []string{"create"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCuratorDriverFixture(t)
			fixture.allowApply = true
			test.mutate(fixture)
			server := httptest.NewServer(fixture)
			defer server.Close()
			t.Setenv("WITSELF_HOME", t.TempDir())
			args := curatorDriverConnectionArgs(t, server.URL)
			if test.apply {
				args = append(args, "--apply", "--yes")
			}
			args = append(args, curatorPlannerCommand("must-not-run")...)
			stdout, stderr, code := runMemoryCuratorCLI(t, args)
			if code != 1 || stdout != "" || !strings.Contains(stderr, "preflight memory curator") || fixture.count("requests") != 0 {
				t.Fatalf("run = %d stdout=%q stderr=%q calls=%#v", code, stdout, stderr, fixture.calls)
			}
		})
	}
}

func TestMemoryCuratorRunMalformedPlannerIsAbandoned(t *testing.T) {
	fixture := newCuratorDriverFixture(t)
	server := httptest.NewServer(fixture)
	defer server.Close()
	t.Setenv("WITSELF_HOME", t.TempDir())
	stdout, stderr, code := runMemoryCuratorCLI(t, append(
		curatorDriverConnectionArgs(t, server.URL), curatorPlannerCommand("malformed")...,
	))
	if code != 1 || stdout != "" || !strings.Contains(stderr, "invalid curator planner output") || fixture.count("abandon") != 1 {
		t.Fatalf("run = %d stdout=%q stderr=%q calls=%#v", code, stdout, stderr, fixture.calls)
	}
}

func TestMemoryCuratorRunRefusesSensitiveRequests(t *testing.T) {
	fixture := newCuratorDriverFixture(t)
	fixture.request.Scope.IncludeSensitive = true
	server := httptest.NewServer(fixture)
	defer server.Close()
	t.Setenv("WITSELF_HOME", t.TempDir())
	stdout, stderr, code := runMemoryCuratorCLI(t, append(
		curatorDriverConnectionArgs(t, server.URL), curatorPlannerCommand("must-not-run")...,
	))
	if code != 1 || stdout != "" || !strings.Contains(stderr, "includes sensitive inputs") ||
		fixture.count("start") != 0 || fixture.count("plan") != 0 {
		t.Fatalf("run = %d stdout=%q stderr=%q calls=%#v", code, stdout, stderr, fixture.calls)
	}
}

func TestMemoryCuratorRunResumeUsesPersistedLaunchAndValidatesPolicy(t *testing.T) {
	fixture := newCuratorDriverFixture(t)
	server := httptest.NewServer(fixture)
	defer server.Close()
	t.Setenv("WITSELF_HOME", t.TempDir())
	base := curatorDriverConnectionArgs(t, server.URL)
	stdout, stderr, code := runMemoryCuratorCLI(t, append(append([]string{}, base...), curatorPlannerCommand("valid")...))
	if code != 0 || stderr != "" {
		t.Fatalf("initial preview = %d stderr=%q", code, stderr)
	}
	var first curatorRunSummary
	if err := json.Unmarshal([]byte(stdout), &first); err != nil {
		t.Fatal(err)
	}

	args := append(append([]string{}, base...), "--resume", first.LaunchID)
	args = append(args, curatorPlannerCommand("must-not-run")...)
	stdout, stderr, code = runMemoryCuratorCLI(t, args)
	if code != 0 || stderr != "" {
		t.Fatalf("resume = %d stderr=%q", code, stderr)
	}
	var resumed curatorRunSummary
	if err := json.Unmarshal([]byte(stdout), &resumed); err != nil {
		t.Fatal(err)
	}
	if !resumed.Recovered || resumed.Status != "previewed" || fixture.count("status") != 1 || fixture.count("get-run") != 1 || fixture.count("plan") != 1 {
		t.Fatalf("summary=%#v calls=%#v", resumed, fixture.calls)
	}

	fixture.allowApply = true
	args = append(append([]string{}, base...), "--resume", first.LaunchID, "--apply", "--yes")
	args = append(args, curatorPlannerCommand("must-not-run")...)
	stdout, stderr, code = runMemoryCuratorCLI(t, args)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "launch uses preview policy") {
		t.Fatalf("policy change = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

// TestMemoryCuratorPlannerHelperProcess is executed as the planner child by
// the tests above. It validates the credential boundary before returning a
// plan; no shell participates in the invocation.
func TestMemoryCuratorPlannerHelperProcess(_ *testing.T) {
	if os.Getenv("CURATOR_DRIVER_HELPER") != "1" {
		return
	}
	if os.Getenv("CURATOR_HELPER_MUST_NOT_RUN") == "1" {
		fmt.Fprintln(os.Stderr, "planner unexpectedly launched")
		os.Exit(41)
	}
	for _, entry := range os.Environ() {
		if strings.Contains(entry, curatorDriverTokenCanary) || strings.HasPrefix(strings.ToUpper(entry), "WITSELF_") && entry != "WITSELF_CURATOR_SESSION=1" {
			fmt.Fprintln(os.Stderr, "planner received Witself credential environment")
			os.Exit(42)
		}
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil || strings.Contains(string(raw), curatorDriverTokenCanary) {
		fmt.Fprintln(os.Stderr, "planner stdin contained credential")
		os.Exit(43)
	}
	separator := -1
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		fmt.Fprintln(os.Stderr, "planner helper mode missing")
		os.Exit(44)
	}
	for _, arg := range os.Args {
		if strings.Contains(arg, curatorDriverTokenCanary) {
			fmt.Fprintln(os.Stderr, "planner argv contained credential")
			os.Exit(45)
		}
	}
	switch os.Args[separator+1] {
	case "valid":
		if separator+2 < len(os.Args) && !strings.HasPrefix(os.Args[separator+2], "$(touch ") {
			fmt.Fprintln(os.Stderr, "planner argv was changed")
			os.Exit(46)
		}
		fmt.Print(`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}`)
		os.Exit(0)
	case "malformed":
		fmt.Print(`{"schema":`)
		os.Exit(0)
	case "must-not-run":
		fmt.Fprintln(os.Stderr, "planner unexpectedly launched")
		os.Exit(47)
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(48)
	}
}

func curatorPlannerCommand(mode string, extra ...string) []string {
	result := []string{"--", os.Args[0], "-test.run=^TestMemoryCuratorPlannerHelperProcess$", "--", mode}
	return append(result, extra...)
}

func writeFakeNativeProvider(t *testing.T, directory, body string) string {
	t.Helper()
	path := filepath.Join(directory, "fake-native-provider")
	script := "#!/bin/sh\nset -eu\n" + body
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func curatorDriverConnectionArgs(t *testing.T, endpoint string) []string {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "curator.token")
	if err := os.WriteFile(tokenFile, []byte(curatorDriverTokenCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"--endpoint", endpoint, "--token-file", tokenFile, "--json"}
}

func runMemoryCuratorCLI(t *testing.T, args []string) (stdout, stderr string, code int) {
	t.Helper()
	t.Setenv("CURATOR_DRIVER_HELPER", "1")
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
	code = memoryCurateRun(args)
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

type curatorDriverFixture struct {
	t *testing.T

	mu                      sync.Mutex
	calls                   map[string]int
	agentID                 string
	request                 client.MemoryCurationRequest
	run                     client.MemoryCurationRun
	inputContent            string
	noWork                  bool
	allowList               bool
	allowGetPlan            bool
	allowApply              bool
	accessProfile           string
	planSchema              string
	primitives              []string
	backendInference        bool
	clientInferenceRequired bool
	wantClientRuntime       string
	wantClientModel         string
}

func newCuratorDriverFixture(t *testing.T) *curatorDriverFixture {
	t.Helper()
	expires := time.Now().UTC().Add(20 * time.Minute)
	request := client.MemoryCurationRequest{
		ID: "mcrq_driver", State: "queued", RequestGeneration: 1, DueAt: time.Now().UTC(), MaxAttempts: 5,
		Scope: client.MemoryCurationScope{Sources: []string{"memory"}, IncludeSensitive: false},
	}
	return &curatorDriverFixture{
		t: t, calls: make(map[string]int), agentID: "agent_driver",
		request: request,
		run: client.MemoryCurationRun{
			ID: "mrun_driver", RequestID: request.ID, RequestGeneration: 1,
			FencingGeneration: 1, State: "open", LeaseExpiresAt: &expires, InputCount: 1,
		},
		inputContent: "nonsensitive-input-content-that-must-not-enter-state-or-output",
		allowList:    true, allowGetPlan: true, planSchema: "witself.memory-plan.v1",
		accessProfile:           "curator-apply",
		primitives:              []string{"create", "replace", "supersede", "relate", "propose_fact"},
		clientInferenceRequired: true,
	}
}

func (f *curatorDriverFixture) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *curatorDriverFixture) hit(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
}

func (f *curatorDriverFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+curatorDriverTokenCanary {
		f.t.Errorf("authorization = %q", got)
	}
	planHash := strings.Repeat("a", 64)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/memory-curation-preflight":
		f.hit("preflight")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"principal":      map[string]any{"account_id": "acc_driver", "realm_id": "realm_driver", "agent_id": f.agentID, "agent_name": "driver"},
			"credential":     map[string]any{"token_id": "tok_driver", "access_profile": f.accessProfile},
			"protocol": map[string]any{
				"plan_schema": f.planSchema, "allowed_primitives": f.primitives,
				"backend_inference": f.backendInference, "client_inference_required": f.clientInferenceRequired,
			},
			"permissions": map[string]any{
				"list_requests": f.allowList, "get_request": true, "start": true, "get_run": true,
				"get_inputs": true, "get_plan": f.allowGetPlan, "renew": true, "plan": true, "abandon": true, "apply": f.allowApply,
			},
			"limits": map[string]any{
				"max_page_size": 200, "max_memories": 500, "max_evidence": 1000,
				"max_transcript_entries": 2000, "min_lease_seconds": 30, "max_lease_seconds": 1800,
				"max_plan_actions": 128, "max_plan_bytes": 32 << 20,
			},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/memory-curation-requests":
		f.hit("requests")
		requests := []client.MemoryCurationRequest{f.request}
		if f.noWork {
			requests = []client.MemoryCurationRequest{}
		}
		_ = json.NewEncoder(w).Encode(client.MemoryCurationRequestPage{Requests: requests})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/memory-curation-requests/"+f.request.ID+"/start":
		f.hit("start")
		var body struct {
			Client client.MemoryClientProvenance `json:"client"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode start request: %v", err)
		}
		if f.wantClientRuntime != "" && body.Client.Runtime != f.wantClientRuntime {
			f.t.Errorf("start client runtime = %q, want %q", body.Client.Runtime, f.wantClientRuntime)
		}
		if f.wantClientModel != "" && body.Client.Model != f.wantClientModel {
			f.t.Errorf("start client model = %q, want %q", body.Client.Model, f.wantClientModel)
		}
		_ = json.NewEncoder(w).Encode(client.StartMemoryCurationResult{Run: f.run, Request: f.request})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/memory-curation-runs/"+f.run.ID+"/renew":
		f.hit("renew")
		if got := r.Header.Get("Idempotency-Key"); got == "" {
			f.t.Error("renew idempotency key is empty")
		}
		var body struct {
			FencingGeneration int64 `json:"fencing_generation"`
			ExtensionSeconds  int64 `json:"extension_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode renew request: %v", err)
		}
		if body.FencingGeneration != f.run.FencingGeneration {
			f.t.Errorf("renew fencing_generation = %d, want %d", body.FencingGeneration, f.run.FencingGeneration)
		}
		if body.ExtensionSeconds <= 0 {
			f.t.Errorf("renew extension_seconds = %d, want positive", body.ExtensionSeconds)
		}
		expires := time.Now().UTC().Add(time.Duration(body.ExtensionSeconds) * time.Second)
		f.run.LeaseExpiresAt = &expires
		_ = json.NewEncoder(w).Encode(client.RenewMemoryCurationResult{
			Run: f.run,
			Receipt: client.MemoryCurationMutationReceipt{
				Operation:         "renew",
				ActorID:           f.agentID,
				IdempotencyKey:    r.Header.Get("Idempotency-Key"),
				RequestID:         f.run.RequestID,
				RunID:             f.run.ID,
				RequestGeneration: f.run.RequestGeneration,
				FencingGeneration: f.run.FencingGeneration,
				LeaseExpiresAt:    &expires,
				ResultState:       f.run.State,
			},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/memory-curation-runs/"+f.run.ID+"/inputs":
		f.hit("inputs")
		_ = json.NewEncoder(w).Encode(client.MemoryCurationRunInputPage{
			Run: f.run,
			Inputs: []client.MemoryCurationRunInput{{
				RunID: f.run.ID, Ordinal: 1, Kind: "memory", MemoryID: "mem_driver", MemoryVersion: 1,
				Memory: &client.Memory{ID: "mem_driver", Version: 1, Content: f.inputContent},
			}},
		})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/memory-curation-runs/"+f.run.ID+"/plan":
		f.hit("plan")
		planned := f.run
		planned.State, planned.PlanSchema, planned.PlanRevision, planned.PlanHash = "planned", "witself.memory-plan.v1", 1, planHash
		f.run = planned
		_ = json.NewEncoder(w).Encode(client.PlanMemoryCurationResult{
			Run:  planned,
			Plan: json.RawMessage(`{"schema":"witself.memory-plan.v1","plan_revision":1,"actions":[]}`),
			Receipt: client.MemoryCurationPlanReceipt{
				ID: "mrec_plan", Operation: "plan", RequestID: planned.RequestID,
				RunID: planned.ID, RequestGeneration: planned.RequestGeneration,
				FencingGeneration: planned.FencingGeneration, PlanSchema: "witself.memory-plan.v1",
				PlanRevision: 1, PlanHash: planHash, ResultState: "planned",
			},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/memory-curation-runs/"+f.run.ID+"/plan":
		f.hit("get-plan")
		if got := r.URL.Query().Get("fencing_generation"); got != "1" {
			f.t.Errorf("get plan fencing_generation = %q", got)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "" {
			f.t.Errorf("get plan idempotency key = %q", got)
		}
		_ = json.NewEncoder(w).Encode(client.GetMemoryCurationPlanResult{
			Run:  f.run,
			Plan: json.RawMessage(`{"schema":"witself.memory-plan.v1","plan_revision":1,"actions":[]}`),
		})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/memory-curation-runs/"+f.run.ID+"/abandon":
		f.hit("abandon")
		abandoned := f.run
		abandoned.State = "abandoned"
		f.run = abandoned
		_ = json.NewEncoder(w).Encode(client.FinishMemoryCurationResult{Run: abandoned})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/memory-curation-runs/"+f.run.ID+"/apply":
		f.hit("apply")
		applied := f.run
		applied.State, applied.ApplyReceiptID = "applied", "mrec_apply"
		f.run = applied
		_ = json.NewEncoder(w).Encode(client.ApplyMemoryCurationResult{
			Run: applied,
			Receipt: client.MemoryCurationApplyReceipt{
				ID: "mrec_apply", RunID: applied.ID, FencingGeneration: applied.FencingGeneration,
				PlanRevision: 1, PlanHash: planHash,
			},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/memory-curation-status":
		f.hit("status")
		_ = json.NewEncoder(w).Encode(client.MemoryCurationStatus{Request: &f.request, Run: &f.run})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/memory-curation-runs/"+f.run.ID:
		f.hit("get-run")
		_ = json.NewEncoder(w).Encode(map[string]any{"run": f.run})
	default:
		http.NotFound(w, r)
	}
}
