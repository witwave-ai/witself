package memorycurator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const nativeTestSentinel = "NATIVE_UNTRUSTED_INPUT_SENTINEL"

type nativeHelperRecord struct {
	Args              []string `json:"args"`
	WorkingDirectory  string   `json:"working_directory"`
	WorkspaceArgument string   `json:"workspace_argument,omitempty"`
	PromptPath        string   `json:"prompt_path,omitempty"`
	ProviderHome      string   `json:"provider_home,omitempty"`
	PromptMode        uint32   `json:"prompt_mode,omitempty"`
	PromptHadSentinel bool     `json:"prompt_had_sentinel"`
	PromptHadBoundary bool     `json:"prompt_had_boundary"`
	Marker            string   `json:"marker"`
	WitselfVariables  []string `json:"witself_variables"`
	HomePreserved     bool     `json:"home_preserved"`
	PathPreserved     bool     `json:"path_preserved"`
	AuthPreserved     bool     `json:"auth_preserved"`
	ProviderAuthCopy  bool     `json:"provider_auth_copy"`
}

func TestMain(m *testing.M) {
	if provider := os.Getenv("NATIVE_TEST_PROVIDER"); provider != "" {
		os.Exit(runNativePlannerHelper(NativeProvider(provider)))
	}
	os.Exit(m.Run())
}

func TestNativePlannerProviderContracts(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		provider NativeProvider
		authEnv  string
	}{
		{provider: ProviderClaudeCode, authEnv: "ANTHROPIC_API_KEY"},
		{provider: ProviderGrokBuild, authEnv: "XAI_API_KEY"},
	}
	for _, test := range tests {
		t.Run(string(test.provider), func(t *testing.T) {
			t.Parallel()
			temp := t.TempDir()
			logPath := filepath.Join(temp, "record.json")
			home := filepath.Join(temp, "home")
			providerSourceHome := filepath.Join(temp, "provider-source")
			if err := os.MkdirAll(providerSourceHome, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(providerSourceHome, "auth.json"), []byte(`{"test":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			environment := []string{
				"NATIVE_TEST_PROVIDER=" + string(test.provider),
				"NATIVE_TEST_RECORD=" + logPath,
				"NATIVE_TEST_EXPECT_HOME=" + home,
				"NATIVE_TEST_EXPECT_PATH=" + os.Getenv("PATH"),
				"NATIVE_TEST_AUTH_NAME=" + test.authEnv,
				"NATIVE_TEST_AUTH_VALUE=test-provider-auth",
				"HOME=" + home,
				"PATH=" + os.Getenv("PATH"),
				test.authEnv + "=test-provider-auth",
				"WITSELF_TOKEN=must-not-reach-provider",
				"WITSELF_ENDPOINT=https://must-not-reach-provider.invalid",
				"WITSELF_CURATOR_SESSION=0",
			}
			switch test.provider {
			case ProviderCodex:
				environment = append(environment, "CODEX_HOME="+providerSourceHome)
			case ProviderGrokBuild:
				environment = append(environment, "GROK_HOME="+providerSourceHome)
			}
			planner := NativePlanner{
				Provider: test.provider, Path: executable, Model: "native-model_1",
				Env: environment, TempDir: temp,
			}
			plan, err := planner.Plan(context.Background(), nativeTestEnvelope())
			if err != nil {
				t.Fatalf("Plan() error = %v", err)
			}
			if string(plan) != `{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}` {
				t.Fatalf("Plan() = %s", plan)
			}
			record := readNativeHelperRecord(t, logPath)
			if !record.PromptHadSentinel || !record.PromptHadBoundary {
				t.Fatalf("provider did not receive the protected prompt: %#v", record)
			}
			if record.Marker != "1" || len(record.WitselfVariables) != 0 {
				t.Fatalf("unsafe provider environment: %#v", record)
			}
			if !record.HomePreserved || !record.PathPreserved || !record.AuthPreserved {
				t.Fatalf("provider runtime environment was not preserved: %#v", record)
			}
			for _, argument := range record.Args {
				if strings.Contains(argument, nativeTestSentinel) || strings.Contains(argument, "mcrq_native") || strings.Contains(argument, "mrun_native") {
					t.Fatalf("planner content leaked into argv: %#v", record.Args)
				}
			}
			assertNativeArgs(t, test.provider, record)
			if test.provider == ProviderGrokBuild {
				if !record.ProviderAuthCopy {
					t.Fatalf("provider authentication was not available in isolated home: %#v", record)
				}
				if record.ProviderHome == "" {
					t.Fatal("isolated provider home was not recorded")
				}
				if _, err := os.Stat(record.ProviderHome); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("isolated provider home was not removed: %v", err)
				}
			}
			if record.PromptPath != "" {
				if record.PromptMode != 0o600 {
					t.Fatalf("prompt mode = %#o", record.PromptMode)
				}
				if _, err := os.Stat(record.PromptPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("private prompt file was not removed: %v", err)
				}
			}
			if _, err := os.Stat(record.WorkingDirectory); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("private workspace was not removed: %v", err)
			}
		})
	}
}

func TestNativePlannerFailsClosedForUnsupportedCLIs(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("codex lacks no-tools contract", func(t *testing.T) {
		planner := NativePlanner{Provider: ProviderCodex, Path: executable, Env: []string{
			"NATIVE_TEST_PROVIDER=" + string(ProviderCodex),
		}}
		capability, err := planner.Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if capability.Supported || capability.PromptTransport != "unsupported" ||
			!strings.Contains(capability.UnsupportedReason, "no-tools") {
			t.Fatalf("capability = %#v", capability)
		}
		_, err = planner.Plan(context.Background(), nativeTestEnvelope())
		if !errors.Is(err, ErrNativeProviderUnsupported) {
			t.Fatalf("Plan() error = %v", err)
		}
	})
	t.Run("current cursor contract", func(t *testing.T) {
		planner := NativePlanner{Provider: ProviderCursor, Path: executable, Env: []string{
			"NATIVE_TEST_PROVIDER=" + string(ProviderCursor),
		}}
		capability, err := planner.Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if capability.Supported || capability.PromptTransport != "unsupported" || !strings.Contains(capability.UnsupportedReason, "stdin") {
			t.Fatalf("capability = %#v", capability)
		}
		_, err = planner.Plan(context.Background(), nativeTestEnvelope())
		if !errors.Is(err, ErrNativeProviderUnsupported) {
			t.Fatalf("Plan() error = %v", err)
		}
	})
}

func TestNativePlannerErrorsAreBoundedAndValueFree(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		mode      string
		maxOutput int
		deadline  bool
		want      error
	}{
		{name: "process error", mode: "error", want: ErrNativeProviderCommand},
		{name: "invalid output", mode: "malformed", want: ErrInvalidPlannerOutput},
		{name: "output limit", mode: "large", maxOutput: 32, want: ErrPlannerOutputLimit},
		{name: "cancellation", mode: "sleep", deadline: true, want: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if test.deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 40*time.Millisecond)
				defer cancel()
			}
			planner := NativePlanner{
				Provider: ProviderClaudeCode, Path: executable, MaxOutputBytes: test.maxOutput,
				Env: []string{"NATIVE_TEST_PROVIDER=" + string(ProviderClaudeCode), "NATIVE_TEST_MODE=" + test.mode},
			}
			_, err := planner.Plan(ctx, nativeTestEnvelope())
			if !errors.Is(err, test.want) {
				t.Fatalf("Plan() error = %v, want %v", err, test.want)
			}
			if strings.Contains(fmt.Sprint(err), nativeTestSentinel) || strings.Contains(fmt.Sprint(err), "PROVIDER_ECHOED_PRIVATE_INPUT") {
				t.Fatalf("error leaked planner content: %v", err)
			}
		})
	}
}

func nativeTestEnvelope() PlannerEnvelope {
	envelope := testPlannerEnvelope()
	envelope.RequestID = "mcrq_native"
	envelope.RunID = "mrun_native"
	envelope.MaterializedInputs[0].CursorStreamID = nativeTestSentinel
	return envelope
}

func assertNativeArgs(t *testing.T, provider NativeProvider, record nativeHelperRecord) {
	t.Helper()
	var want []string
	switch provider {
	case ProviderClaudeCode:
		want = []string{
			"--print", "--input-format", "text", "--output-format", "text",
			"--safe-mode", "--no-session-persistence", "--disable-slash-commands",
			"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
			"--tools", "", "--permission-mode", "plan", "--no-chrome",
			"--model", "native-model_1",
		}
	case ProviderGrokBuild:
		want = []string{
			"--prompt-file", record.PromptPath, "--verbatim", "--output-format", "plain",
			"--permission-mode", "plan", "--sandbox", "strict", "--cwd", record.WorkspaceArgument,
			"--disable-web-search", "--no-memory", "--no-subagents", "--max-turns", "1",
			"--tools", "", "--disallowed-tools", "Agent", "--deny", "MCPTool",
			"--model", "native-model_1",
		}
	default:
		t.Fatalf("unexpected provider %q", provider)
	}
	if !reflect.DeepEqual(record.Args, want) {
		t.Fatalf("argv:\n got: %#v\nwant: %#v", record.Args, want)
	}
}

func readNativeHelperRecord(t *testing.T, path string) nativeHelperRecord {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record nativeHelperRecord
	if err := json.Unmarshal(value, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func runNativePlannerHelper(provider NativeProvider) int {
	args := os.Args[1:]
	if reflect.DeepEqual(args, []string{"--version"}) {
		switch provider {
		case ProviderCodex:
			_, _ = fmt.Fprintln(os.Stdout, "codex-cli 99.0.0")
		case ProviderClaudeCode:
			_, _ = fmt.Fprintln(os.Stdout, "99.0.0 (Claude Code)")
		case ProviderGrokBuild:
			_, _ = fmt.Fprintln(os.Stdout, "grok 99.0.0")
		case ProviderCursor:
			_, _ = fmt.Fprintln(os.Stdout, "2099.01.01")
		}
		return 0
	}
	if reflect.DeepEqual(args, []string{"--help"}) || reflect.DeepEqual(args, []string{"exec", "--help"}) {
		_, _ = fmt.Fprintln(os.Stdout, nativeHelperHelp(provider, os.Getenv("NATIVE_TEST_HELP_VARIANT")))
		return 0
	}

	promptPath := argumentAfter(args, "--prompt-file")
	var prompt []byte
	var err error
	var promptMode uint32
	if promptPath != "" {
		prompt, err = os.ReadFile(promptPath)
		if info, statErr := os.Stat(promptPath); statErr == nil {
			promptMode = uint32(info.Mode().Perm())
		}
	} else {
		prompt, err = os.ReadFile("/dev/stdin")
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 41
	}
	workingDirectory, _ := os.Getwd()
	providerHome := ""
	switch provider {
	case ProviderCodex:
		providerHome = os.Getenv("CODEX_HOME")
	case ProviderGrokBuild:
		providerHome = os.Getenv("GROK_HOME")
	}
	witselfVariables := make([]string, 0)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "WITSELF_") && name != "WITSELF_CURATOR_SESSION" {
			witselfVariables = append(witselfVariables, name)
		}
	}
	record := nativeHelperRecord{
		Args: args, WorkingDirectory: workingDirectory, PromptPath: promptPath,
		WorkspaceArgument: firstNonempty(argumentAfter(args, "--cd"), argumentAfter(args, "--cwd")),
		ProviderHome:      providerHome, PromptMode: promptMode,
		PromptHadSentinel: bytesContains(prompt, nativeTestSentinel),
		PromptHadBoundary: bytesContains(prompt, "BEGIN_UNTRUSTED_PLANNER_ENVELOPE"),
		Marker:            os.Getenv("WITSELF_CURATOR_SESSION"), WitselfVariables: witselfVariables,
		HomePreserved: os.Getenv("HOME") == os.Getenv("NATIVE_TEST_EXPECT_HOME"),
		PathPreserved: os.Getenv("PATH") == os.Getenv("NATIVE_TEST_EXPECT_PATH"),
		AuthPreserved: os.Getenv(os.Getenv("NATIVE_TEST_AUTH_NAME")) == os.Getenv("NATIVE_TEST_AUTH_VALUE"),
	}
	if providerHome != "" {
		_, err := os.Stat(filepath.Join(providerHome, "auth.json"))
		record.ProviderAuthCopy = err == nil
	}
	value, _ := json.Marshal(record)
	if err := os.WriteFile(os.Getenv("NATIVE_TEST_RECORD"), value, 0o600); err != nil && os.Getenv("NATIVE_TEST_RECORD") != "" {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 43
	}
	switch os.Getenv("NATIVE_TEST_MODE") {
	case "error":
		_, _ = fmt.Fprintln(os.Stderr, "PROVIDER_ECHOED_PRIVATE_INPUT "+nativeTestSentinel)
		return 42
	case "malformed":
		_, _ = fmt.Fprintln(os.Stdout, "not a memory plan")
	case "large":
		_, _ = fmt.Fprintln(os.Stdout, strings.Repeat("x", 512))
	case "sleep":
		time.Sleep(5 * time.Second)
	default:
		_, _ = fmt.Fprint(os.Stdout, `{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}`)
	}
	return 0
}

func nativeHelperHelp(provider NativeProvider, variant string) string {
	if variant == "missing-safety-controls" {
		return "Usage: codex exec --sandbox read-only --skip-git-repo-check --cd"
	}
	switch provider {
	case ProviderCodex:
		return "--ephemeral --ignore-user-config --ignore-rules --sandbox read-only --skip-git-repo-check --cd"
	case ProviderClaudeCode:
		return "--print --input-format --output-format --safe-mode --no-session-persistence --disable-slash-commands --strict-mcp-config --mcp-config --tools --permission-mode --no-chrome --model"
	case ProviderGrokBuild:
		return "--prompt-file --verbatim --output-format --permission-mode --sandbox --cwd --disable-web-search --no-memory --no-subagents --max-turns --tools --disallowed-tools --deny --model"
	case ProviderCursor:
		return "--print --mode plan"
	default:
		return ""
	}
}

func argumentAfter(args []string, name string) string {
	for index := range args {
		if args[index] == name && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func bytesContains(value []byte, needle string) bool {
	return strings.Contains(string(value), needle)
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
