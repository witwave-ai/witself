package messagerunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

const nativeMessageTestSentinel = "NATIVE_MESSAGE_PRIVATE_SENTINEL"

type nativeMessageHelperRecord struct {
	Args                 []string `json:"args"`
	WorkingDirectory     string   `json:"working_directory"`
	WorkspaceArgument    string   `json:"workspace_argument,omitempty"`
	PromptPath           string   `json:"prompt_path,omitempty"`
	ProviderHome         string   `json:"provider_home,omitempty"`
	PromptMode           uint32   `json:"prompt_mode,omitempty"`
	PromptHadSentinel    bool     `json:"prompt_had_sentinel"`
	PromptHadBoundary    bool     `json:"prompt_had_boundary"`
	PromptHadClaim       bool     `json:"prompt_had_claim"`
	Marker               string   `json:"marker"`
	WitselfVariables     []string `json:"witself_variables"`
	HomeIsolated         bool     `json:"home_isolated"`
	PathPreserved        bool     `json:"path_preserved"`
	AuthPreserved        bool     `json:"auth_preserved"`
	CrossAuthAbsent      bool     `json:"cross_auth_absent"`
	ProviderAuthCopy     bool     `json:"provider_auth_copy"`
	ProviderConfigSafe   bool     `json:"provider_config_safe"`
	ProviderHomeMode     uint32   `json:"provider_home_mode,omitempty"`
	ProviderAuthFileMode uint32   `json:"provider_auth_file_mode,omitempty"`
}

func TestMain(m *testing.M) {
	if provider := os.Getenv("MESSAGE_NATIVE_TEST_PROVIDER"); provider != "" {
		os.Exit(runNativeMessageProviderHelper(NativeProvider(provider)))
	}
	os.Exit(m.Run())
}

func TestNativeTextProviderContracts(t *testing.T) {
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
			temp := t.TempDir()
			logPath := filepath.Join(temp, "record.json")
			providerSourceHome := filepath.Join(temp, "provider-source")
			if err := os.MkdirAll(providerSourceHome, 0o700); err != nil {
				t.Fatal(err)
			}
			authFile := "auth.json"
			if test.provider == ProviderClaudeCode {
				authFile = ".credentials.json"
			}
			if err := os.WriteFile(filepath.Join(providerSourceHome, authFile), []byte(`{"test":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			// Model the persistent service process: its ambient environment has no
			// provider credential, so authentication must come from the separately
			// captured provider-bound Env slice.
			t.Setenv(test.authEnv, "")
			crossAuthName := "XAI_API_KEY"
			if test.provider == ProviderGrokBuild {
				crossAuthName = "ANTHROPIC_API_KEY"
			}
			t.Setenv(crossAuthName, "must-not-cross-provider-boundary")
			t.Setenv("WITSELF_TOKEN", "must-not-reach-provider")
			t.Setenv("WITSELF_TOKEN_FILE", "/private/must-not-reach-provider")
			t.Setenv("WITSELF_ENDPOINT", "https://must-not-reach-provider.invalid")
			environment := []string{
				"MESSAGE_NATIVE_TEST_PROVIDER=" + string(test.provider),
				test.authEnv + "=test-provider-auth",
				"MESSAGE_NATIVE_TEST_RECORD=" + logPath,
				"MESSAGE_NATIVE_TEST_EXPECT_HOME=" + os.Getenv("HOME"),
				"MESSAGE_NATIVE_TEST_EXPECT_PATH=" + os.Getenv("PATH"),
				"MESSAGE_NATIVE_TEST_AUTH_NAME=" + test.authEnv,
				"MESSAGE_NATIVE_TEST_AUTH_VALUE=test-provider-auth",
				"MESSAGE_NATIVE_TEST_CROSS_AUTH_NAME=" + crossAuthName,
			}
			if test.provider == ProviderClaudeCode {
				environment = append(environment, "CLAUDE_CONFIG_DIR="+providerSourceHome)
			} else {
				environment = append(environment, "GROK_HOME="+providerSourceHome)
			}
			provider := NativeTextProvider{
				Provider: test.provider,
				Path:     executable,
				Model:    "native-model_1",
				Env:      environment,
				TempDir:  temp,
				Timeout:  5 * time.Second,
			}
			envelope := validTurnEnvelope()
			envelope.Message.Body = nativeMessageTestSentinel
			envelope.Message.Processing = client.MessageProcessing{
				State: "claimed", ClaimID: "claim-secret", Generation: 9,
			}
			result, err := provider.Invoke(context.Background(), envelope)
			if err != nil {
				t.Fatalf("Invoke() error = %v", err)
			}
			if result.Schema != TurnResultSchemaV1 || result.Outcome != OutcomeResult || result.Body != "native result" {
				t.Fatalf("Invoke() = %#v", result)
			}
			record := readNativeMessageHelperRecord(t, logPath)
			if !record.PromptHadSentinel || !record.PromptHadBoundary || record.PromptHadClaim {
				t.Fatalf("unsafe native provider prompt: %#v", record)
			}
			if record.Marker != "1" || len(record.WitselfVariables) != 0 {
				t.Fatalf("unsafe native provider environment: %#v", record)
			}
			if !record.HomeIsolated || !record.PathPreserved || !record.AuthPreserved || !record.CrossAuthAbsent {
				t.Fatalf("provider runtime environment was not safely isolated: %#v", record)
			}
			for _, argument := range record.Args {
				if strings.Contains(argument, nativeMessageTestSentinel) || strings.Contains(argument, "claim-secret") {
					t.Fatalf("private turn content leaked into argv: %#v", record.Args)
				}
			}
			assertNativeMessageArgs(t, test.provider, record)
			if test.provider == ProviderGrokBuild &&
				(filepath.Dir(record.PromptPath) != record.WorkspaceArgument || filepath.Ext(record.PromptPath) != ".txt") {
				t.Fatalf("Grok prompt must be readable inside its strict sandbox as plain text: %#v", record)
			}
			if !record.ProviderAuthCopy || record.ProviderHomeMode != 0o700 || record.ProviderAuthFileMode != 0o600 {
				t.Fatalf("unsafe isolated provider home: %#v", record)
			}
			if test.provider == ProviderGrokBuild && !record.ProviderConfigSafe {
				t.Fatalf("unsafe isolated Grok configuration: %#v", record)
			}
			if record.ProviderHome == "" {
				t.Fatal("isolated provider home was not recorded")
			}
			if _, err := os.Stat(record.ProviderHome); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("isolated provider home was not removed: %v", err)
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

func TestNativeTextProviderFailsClosedForUnsupportedCLIs(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		provider NativeProvider
		reason   string
	}{
		{provider: ProviderCodex, reason: "no-tools"},
		{provider: ProviderCursor, reason: "stdin"},
	}
	for _, test := range tests {
		t.Run(string(test.provider), func(t *testing.T) {
			provider := NativeTextProvider{
				Provider: test.provider,
				Path:     executable,
				Env:      []string{"MESSAGE_NATIVE_TEST_PROVIDER=" + string(test.provider)},
			}
			capability, err := provider.Probe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if capability.Supported || capability.PromptTransport != "unsupported" ||
				!strings.Contains(capability.UnsupportedReason, test.reason) {
				t.Fatalf("capability = %#v", capability)
			}
			_, err = provider.Invoke(context.Background(), validTurnEnvelope())
			if !errors.Is(err, ErrNativeProviderUnsupported) {
				t.Fatalf("Invoke() error = %v", err)
			}
		})
	}
}

func TestNativeTextProviderClassifiesHostFailuresAsUnavailable(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("missing executable", func(t *testing.T) {
		provider := NativeTextProvider{Provider: ProviderClaudeCode, Path: filepath.Join(t.TempDir(), "missing")}
		if _, err := provider.Invoke(context.Background(), validTurnEnvelope()); !errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("Invoke() error = %v, want ErrProviderUnavailable", err)
		}
	})
	t.Run("scratch root unavailable", func(t *testing.T) {
		blocked := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(blocked, []byte("blocked"), 0o600); err != nil {
			t.Fatal(err)
		}
		provider := NativeTextProvider{
			Provider: ProviderClaudeCode, Path: executable, TempDir: blocked,
			Env: []string{"MESSAGE_NATIVE_TEST_PROVIDER=" + string(ProviderClaudeCode)},
		}
		if _, err := provider.Invoke(context.Background(), validTurnEnvelope()); !errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("Invoke() error = %v, want ErrProviderUnavailable", err)
		}
	})
	t.Run("invalid static model", func(t *testing.T) {
		provider := NativeTextProvider{Provider: ProviderClaudeCode, Path: executable, Model: "bad model"}
		if _, err := provider.Invoke(context.Background(), validTurnEnvelope()); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("Invoke() error = %v, want ErrInvalidConfiguration", err)
		}
	})
}

func TestNativeTextProviderRequiresAdvertisedSafetyControls(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := NativeTextProvider{
		Provider: ProviderClaudeCode,
		Path:     executable,
		Env: []string{
			"MESSAGE_NATIVE_TEST_PROVIDER=" + string(ProviderClaudeCode),
			"MESSAGE_NATIVE_TEST_HELP_VARIANT=missing-safety-controls",
		},
	}
	capability, err := provider.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capability.Supported || len(capability.MissingControls) == 0 ||
		!strings.Contains(capability.UnsupportedReason, "required headless safety") {
		t.Fatalf("capability = %#v", capability)
	}
	_, err = provider.Invoke(context.Background(), validTurnEnvelope())
	if !errors.Is(err, ErrNativeProviderUnsupported) {
		t.Fatalf("Invoke() error = %v", err)
	}
}

func TestNativeTextProviderErrorsAreBoundedStrictAndValueFree(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		mode      string
		maxOutput int
		maxStderr int
		deadline  bool
		want      error
		contains  string
	}{
		{name: "process error", mode: "error", want: ErrNativeProviderCommand},
		{name: "malformed output", mode: "malformed", want: ErrProviderOutputInvalid},
		{name: "unknown field", mode: "unknown", want: ErrProviderOutputInvalid},
		{name: "trailing data", mode: "trailing", want: ErrProviderOutputInvalid},
		{name: "invalid result model", mode: "invalid-model", want: ErrProviderResultInvalid},
		{name: "output limit", mode: "large-output", maxOutput: 32, contains: "output exceeded"},
		{name: "stderr limit", mode: "large-stderr", maxStderr: 32, contains: "stderr exceeded"},
		{name: "cancellation", mode: "sleep", deadline: true, want: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			provider := NativeTextProvider{
				Provider: ProviderClaudeCode,
				Path:     executable,
				Timeout:  5 * time.Second,
				Env: []string{
					"MESSAGE_NATIVE_TEST_PROVIDER=" + string(ProviderClaudeCode),
					"MESSAGE_NATIVE_TEST_MODE=" + test.mode,
				},
				MaxOutputBytes: test.maxOutput,
				MaxStderrBytes: test.maxStderr,
			}
			envelope := validTurnEnvelope()
			envelope.Message.Body = nativeMessageTestSentinel
			_, err := provider.Invoke(ctx, envelope)
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("Invoke() error = %v, want %v", err, test.want)
			}
			if test.contains != "" && (err == nil || !strings.Contains(err.Error(), test.contains)) {
				t.Fatalf("Invoke() error = %v, want text %q", err, test.contains)
			}
			if strings.Contains(fmt.Sprint(err), nativeMessageTestSentinel) || strings.Contains(fmt.Sprint(err), "PROVIDER_ECHOED_PRIVATE_MESSAGE") {
				t.Fatalf("provider error disclosed private content: %v", err)
			}
		})
	}
}

func TestNativeMessagePromptCapsInputAndStripsFence(t *testing.T) {
	envelope := validTurnEnvelope()
	envelope.Message.Processing = client.MessageProcessing{State: "claimed", ClaimID: "claim-secret", Generation: 3}
	envelope.History = []TurnHistoryEntry{{
		AgentID: "scott", AgentName: "Scott", Kind: "question", Body: "bounded prior turn",
	}}
	prompt, err := nativeMessagePrompt(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prompt), "claim-secret") ||
		!strings.Contains(string(prompt), "BEGIN_UNTRUSTED_MESSAGE_TURN") ||
		!strings.Contains(string(prompt), "bounded prior turn") ||
		!strings.Contains(string(prompt), `"current_turn":1`) ||
		!strings.Contains(string(prompt), `"maximum_turns":12`) {
		t.Fatalf("unsafe prompt: %s", prompt)
	}
	envelope.Message.Payload = json.RawMessage(`{"large":"` + strings.Repeat("x", DefaultMaxNativeTurnPromptBytes) + `"}`)
	if _, err := nativeMessagePrompt(envelope); err == nil || !strings.Contains(err.Error(), "prompt exceeds") {
		t.Fatalf("oversize prompt error = %v", err)
	}
}

func assertNativeMessageArgs(t *testing.T, provider NativeProvider, record nativeMessageHelperRecord) {
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

func readNativeMessageHelperRecord(t *testing.T, path string) nativeMessageHelperRecord {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record nativeMessageHelperRecord
	if err := json.Unmarshal(value, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func runNativeMessageProviderHelper(provider NativeProvider) int {
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
			_, _ = fmt.Fprintln(os.Stdout, "cursor-agent 99.0.0")
		}
		return 0
	}
	if reflect.DeepEqual(args, []string{"--help"}) || reflect.DeepEqual(args, []string{"exec", "--help"}) {
		_, _ = fmt.Fprintln(os.Stdout, nativeMessageHelperHelp(provider, os.Getenv("MESSAGE_NATIVE_TEST_HELP_VARIANT")))
		return 0
	}

	promptPath := nativeMessageArgumentAfter(args, "--prompt-file")
	var prompt []byte
	var err error
	var promptMode uint32
	if promptPath != "" {
		prompt, err = os.ReadFile(promptPath)
		if info, statErr := os.Stat(promptPath); statErr == nil {
			promptMode = uint32(info.Mode().Perm())
		}
	} else {
		prompt, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return 41
	}
	workingDirectory, _ := os.Getwd()
	providerHome := ""
	providerAuthCopy := false
	providerConfigSafe := false
	var providerHomeMode, providerAuthMode uint32
	switch provider {
	case ProviderClaudeCode:
		providerHome = os.Getenv("CLAUDE_CONFIG_DIR")
		if info, statErr := os.Stat(providerHome); statErr == nil {
			providerHomeMode = uint32(info.Mode().Perm())
		}
		if info, statErr := os.Stat(filepath.Join(providerHome, ".credentials.json")); statErr == nil {
			providerAuthCopy = true
			providerAuthMode = uint32(info.Mode().Perm())
		}
	case ProviderGrokBuild:
		providerHome = os.Getenv("GROK_HOME")
		if info, statErr := os.Stat(providerHome); statErr == nil {
			providerHomeMode = uint32(info.Mode().Perm())
		}
		if info, statErr := os.Stat(filepath.Join(providerHome, "auth.json")); statErr == nil {
			providerAuthCopy = true
			providerAuthMode = uint32(info.Mode().Perm())
		}
		if config, readErr := os.ReadFile(filepath.Join(providerHome, "config.toml")); readErr == nil {
			configText := string(config)
			providerConfigSafe = strings.Contains(configText, "disable_plugins = true") &&
				strings.Count(configText, "mcps = false") == 2 && strings.Count(configText, "hooks = false") == 2
		}
	}
	witselfVariables := make([]string, 0)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(name), "WITSELF_") && name != "WITSELF_MESSAGE_RUNNER_SESSION" {
			witselfVariables = append(witselfVariables, name)
		}
	}
	record := nativeMessageHelperRecord{
		Args: args, WorkingDirectory: workingDirectory,
		WorkspaceArgument: nativeMessageArgumentAfter(args, "--cwd"),
		PromptPath:        promptPath, ProviderHome: providerHome, PromptMode: promptMode,
		PromptHadSentinel: strings.Contains(string(prompt), nativeMessageTestSentinel),
		PromptHadBoundary: strings.Contains(string(prompt), "BEGIN_UNTRUSTED_MESSAGE_TURN"),
		PromptHadClaim:    strings.Contains(string(prompt), "claim-secret"),
		Marker:            os.Getenv("WITSELF_MESSAGE_RUNNER_SESSION"), WitselfVariables: witselfVariables,
		HomeIsolated:     os.Getenv("HOME") != os.Getenv("MESSAGE_NATIVE_TEST_EXPECT_HOME") && strings.Contains(os.Getenv("HOME"), ".witself-native-message-"),
		PathPreserved:    os.Getenv("PATH") == os.Getenv("MESSAGE_NATIVE_TEST_EXPECT_PATH"),
		AuthPreserved:    os.Getenv(os.Getenv("MESSAGE_NATIVE_TEST_AUTH_NAME")) == os.Getenv("MESSAGE_NATIVE_TEST_AUTH_VALUE"),
		CrossAuthAbsent:  os.Getenv(os.Getenv("MESSAGE_NATIVE_TEST_CROSS_AUTH_NAME")) == "",
		ProviderAuthCopy: providerAuthCopy, ProviderConfigSafe: providerConfigSafe,
		ProviderHomeMode: providerHomeMode, ProviderAuthFileMode: providerAuthMode,
	}
	if recordPath := os.Getenv("MESSAGE_NATIVE_TEST_RECORD"); recordPath != "" {
		value, _ := json.Marshal(record)
		if err := os.WriteFile(recordPath, value, 0o600); err != nil {
			return 43
		}
	}
	switch os.Getenv("MESSAGE_NATIVE_TEST_MODE") {
	case "error":
		_, _ = fmt.Fprintln(os.Stderr, "PROVIDER_ECHOED_PRIVATE_MESSAGE "+nativeMessageTestSentinel)
		return 42
	case "malformed":
		_, _ = fmt.Fprintln(os.Stdout, "not JSON")
	case "unknown":
		_, _ = fmt.Fprint(os.Stdout, `{"schema":"witself.message-turn-result.v1","outcome":"result","body":"native result","claim_id":"forged"}`)
	case "trailing":
		_, _ = fmt.Fprint(os.Stdout, `{"schema":"witself.message-turn-result.v1","outcome":"result","body":"native result"} trailing`)
	case "invalid-model":
		_, _ = fmt.Fprint(os.Stdout, `{"schema":"witself.message-turn-result.v1","outcome":"result","body":"native result","model":"invalid model name"}`)
	case "large-output":
		for range 100 {
			_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", 16))
			time.Sleep(time.Millisecond)
		}
	case "large-stderr":
		for range 100 {
			_, _ = fmt.Fprint(os.Stderr, strings.Repeat("x", 16))
			time.Sleep(time.Millisecond)
		}
	case "sleep":
		time.Sleep(5 * time.Second)
	default:
		_, _ = fmt.Fprint(os.Stdout, `{"schema":"witself.message-turn-result.v1","outcome":"result","body":"native result"}`)
	}
	return 0
}

func nativeMessageHelperHelp(provider NativeProvider, variant string) string {
	if variant == "missing-safety-controls" {
		return "--print --input-format --output-format"
	}
	switch provider {
	case ProviderCodex:
		return "--ephemeral --sandbox read-only --skip-git-repo-check --cd"
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

func nativeMessageArgumentAfter(args []string, name string) string {
	for index := range args {
		if args[index] == name && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}
