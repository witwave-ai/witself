package transcriptcapture

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSupportedRuntimesOrderAndMutationIsolation(t *testing.T) {
	want := []string{
		RuntimeCodex,
		RuntimeClaudeCode,
		RuntimeGrokBuild,
		RuntimeCursor,
		RuntimeOpenClaw,
		RuntimeAntigravity,
	}

	got := SupportedRuntimes()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SupportedRuntimes() = %v, want %v", got, want)
	}

	got[0] = "mutated"
	got = append(got, "extra")
	if len(got) != len(want)+1 {
		t.Fatalf("caller append length = %d, want %d", len(got), len(want)+1)
	}
	if fresh := SupportedRuntimes(); !reflect.DeepEqual(fresh, want) {
		t.Fatalf("SupportedRuntimes() after caller mutation = %v, want %v", fresh, want)
	}
}

func TestConfigScopeIDsAreOptionalAndRoundTrip(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	loc, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	legacy := Config{
		Runtime: RuntimeCodex, CaptureMode: ModeRaw,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agt_1", AgentName: "scott", Location: loc,
	}
	if err := SaveConfig(legacy); err != nil {
		t.Fatal(err)
	}
	path, err := ConfigPath(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"account_id"`) || strings.Contains(string(raw), `"realm_id"`) {
		t.Fatalf("optional ids unexpectedly appeared in legacy-compatible config: %s", raw)
	}
	loaded, err := LoadConfig(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AccountID != "" || loaded.RealmID != "" || loaded.AgentID != "agt_1" {
		t.Fatalf("legacy-compatible config = %#v", loaded)
	}

	loaded.AccountID = "acc_1"
	loaded.RealmID = "rlm_1"
	if err := SaveConfig(loaded); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadConfig(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AccountID != "acc_1" || loaded.RealmID != "rlm_1" || loaded.AgentID != "agt_1" {
		t.Fatalf("scoped config = %#v", loaded)
	}
}

func TestOpenClawConfigRequiresAndRoundTripsNoHookBinding(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	loc, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Runtime: RuntimeOpenClaw, RuntimeCLICommand: "/usr/local/bin/openclaw",
		MCPCommand: "/usr/local/bin/witself",
		MCPEnvironment: map[string]string{
			"WITSELF_HOME":         "/Users/test/.witself",
			"OPENCLAW_CONFIG_PATH": "/Users/test/.openclaw/openclaw.json",
			"OPENCLAW_STATE_DIR":   "/Users/test/.openclaw",
			"OPENCLAW_PROFILE":     "work",
		},
		MCPConnectTimeoutSeconds: 60,
		RuntimeWorkspace:         "/Users/test/.openclaw/workspace", RuntimeAgentID: "main",
		CaptureMode: ModeMessages, HookMode: HookModeNone,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agt_1", AgentName: "scott", Location: loc,
	}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(RuntimeOpenClaw)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Runtime != RuntimeOpenClaw || loaded.HookMode != HookModeNone || loaded.RuntimeCLICommand != cfg.RuntimeCLICommand || loaded.MCPCommand != cfg.MCPCommand || loaded.RuntimeWorkspace != cfg.RuntimeWorkspace || loaded.RuntimeAgentID != cfg.RuntimeAgentID ||
		len(loaded.MCPEnvironment) != len(cfg.MCPEnvironment) || loaded.MCPEnvironment["WITSELF_HOME"] != cfg.MCPEnvironment["WITSELF_HOME"] ||
		loaded.MCPEnvironment["OPENCLAW_CONFIG_PATH"] != cfg.MCPEnvironment["OPENCLAW_CONFIG_PATH"] || loaded.MCPEnvironment["OPENCLAW_STATE_DIR"] != cfg.MCPEnvironment["OPENCLAW_STATE_DIR"] ||
		loaded.MCPEnvironment["OPENCLAW_PROFILE"] != cfg.MCPEnvironment["OPENCLAW_PROFILE"] || loaded.MCPConnectTimeoutSeconds != cfg.MCPConnectTimeoutSeconds {
		t.Fatalf("OpenClaw config = %#v", loaded)
	}
	cfg.MCPCommand = ""
	if err := SaveConfig(cfg); err == nil || !strings.Contains(err.Error(), "mcp_command is required") {
		t.Fatalf("missing MCP command error = %v", err)
	}
	cfg.MCPCommand = "relative/witself"
	if err := SaveConfig(cfg); err == nil || !strings.Contains(err.Error(), "clean absolute") {
		t.Fatalf("relative MCP command error = %v", err)
	}
	cfg.MCPCommand = "/usr/local/bin/witself"
	cfg.RuntimeWorkspace = ""
	if err := SaveConfig(cfg); err == nil || !strings.Contains(err.Error(), "runtime_workspace is required") {
		t.Fatalf("missing runtime workspace error = %v", err)
	}
	cfg.RuntimeWorkspace = "/Users/test/.openclaw/workspace"
	cfg.HookMode = HookModeUser
	if err := SaveConfig(cfg); err == nil || !strings.Contains(err.Error(), "hook_mode must be none") {
		t.Fatalf("OpenClaw user hook mode error = %v", err)
	}
	cfg.HookMode = HookModeNone
	cfg.MCPEnvironment = map[string]string{"OPENCLAW_STATE_DIR": "/Users/test/.openclaw"}
	if err := SaveConfig(cfg); err == nil || !strings.Contains(err.Error(), "WITSELF_HOME is required") {
		t.Fatalf("missing WITSELF_HOME environment error = %v", err)
	}
	cfg.MCPEnvironment = map[string]string{"WITSELF_HOME": "/Users/test/.witself", "OPENAI_API_KEY": "secret"}
	if err := SaveConfig(cfg); err == nil || !strings.Contains(err.Error(), "is not allowed") {
		t.Fatalf("foreign environment key error = %v", err)
	}
	cfg.MCPEnvironment = map[string]string{"WITSELF_HOME": "relative/.witself"}
	if err := SaveConfig(cfg); err == nil || !strings.Contains(err.Error(), "clean absolute") {
		t.Fatalf("relative WITSELF_HOME error = %v", err)
	}
	cfg.MCPEnvironment = map[string]string{"WITSELF_HOME": "/Users/test/.witself"}
	cfg.MCPConnectTimeoutSeconds = 0
	if err := SaveConfig(cfg); err == nil || !strings.Contains(err.Error(), "mcp_connect_timeout_seconds") {
		t.Fatalf("missing MCP connect timeout error = %v", err)
	}
	cfg.MCPConnectTimeoutSeconds = 60
	for _, profile := range []string{"-work", "work.profile", "work profile", strings.Repeat("a", 65)} {
		cfg.MCPEnvironment = map[string]string{
			"WITSELF_HOME": "/Users/test/.witself", "OPENCLAW_STATE_DIR": "/Users/test/.openclaw-work",
			"OPENCLAW_CONFIG_PATH": "/Users/test/.openclaw-work/openclaw.json", "OPENCLAW_PROFILE": profile,
		}
		if err := SaveConfig(cfg); err == nil || !strings.Contains(err.Error(), "OPENCLAW_PROFILE is invalid") {
			t.Errorf("profile %q validation error = %v", profile, err)
		}
	}
}

func TestAntigravityConfigRequiresAndRoundTripsOwnedPluginBinding(t *testing.T) {
	witselfHome := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", witselfHome)
	loc, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	configRoot := "/Users/test/.gemini/config"
	cfg := Config{
		Runtime: RuntimeAntigravity, RuntimeVersion: "1.1.1",
		RuntimeCLICommand:    "/Users/test/.local/bin/agy",
		MCPCommand:           "/usr/local/bin/witself",
		MCPEnvironment:       map[string]string{"WITSELF_HOME": witselfHome},
		RuntimeConfigRoot:    configRoot,
		RuntimeMCPConfigPath: filepath.Join(configRoot, "mcp_config.json"),
		RuntimePluginPath:    filepath.Join(configRoot, "plugins", "witself-managed-0123456789abcdef01234567"),
		RuntimePluginSource: filepath.Join(
			witselfHome, "integrations", RuntimeAntigravity, "bundles", digest,
		),
		RuntimePluginDigest: digest,
		CaptureMode:         ModeRaw,
		HookMode:            HookModeNone,
		Account:             "default", Realm: "default", Agent: "scott",
		AgentID: "agt_1", AgentName: "scott", Location: loc,
	}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig("agy")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Runtime != RuntimeAntigravity || loaded.HookMode != HookModeNone ||
		loaded.RuntimeCLICommand != cfg.RuntimeCLICommand || loaded.MCPCommand != cfg.MCPCommand ||
		loaded.MCPEnvironment["WITSELF_HOME"] != witselfHome || loaded.RuntimeConfigRoot != configRoot ||
		loaded.RuntimeMCPConfigPath != cfg.RuntimeMCPConfigPath ||
		loaded.RuntimePluginPath != cfg.RuntimePluginPath || loaded.RuntimePluginSource != cfg.RuntimePluginSource ||
		loaded.RuntimePluginDigest != digest {
		t.Fatalf("Antigravity config = %#v", loaded)
	}
	configPath, err := ConfigPath(RuntimeAntigravity)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	aliasRaw := strings.Replace(string(raw), `"runtime": "antigravity"`, `"runtime": "agy"`, 1)
	if aliasRaw == string(raw) {
		t.Fatal("could not rewrite stored runtime alias")
	}
	if err := os.WriteFile(configPath, []byte(aliasRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	aliased, err := LoadConfig(RuntimeAntigravity)
	if err != nil || aliased.Runtime != RuntimeAntigravity {
		t.Fatalf("stored alias did not canonicalize: runtime=%q err=%v", aliased.Runtime, err)
	}
	mismatchRaw := strings.Replace(aliasRaw, `"runtime": "agy"`, `"runtime": "codex"`, 1)
	if err := os.WriteFile(configPath, []byte(mismatchRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(RuntimeAntigravity); err == nil || !strings.Contains(err.Error(), "does not match requested runtime") {
		t.Fatalf("stored runtime mismatch error = %v", err)
	}

	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"hooks", func(value *Config) { value.HookMode = HookModeUser }, "hook_mode must be none"},
		{"relative CLI", func(value *Config) { value.RuntimeCLICommand = "agy" }, "clean absolute"},
		{"extra env", func(value *Config) { value.MCPEnvironment["PATH"] = "/bin" }, "only WITSELF_HOME"},
		{"MCP config path", func(value *Config) { value.RuntimeMCPConfigPath = filepath.Join(configRoot, "other.json") }, "canonical Antigravity MCP config"},
		{"plugin path", func(value *Config) { value.RuntimePluginPath = filepath.Join(configRoot, "plugins", "other") }, "collision-resistant Witself-managed plugin"},
		{"digest", func(value *Config) { value.RuntimePluginDigest = "ABC" }, "lowercase SHA-256"},
		{"source", func(value *Config) { value.RuntimePluginSource = filepath.Join(witselfHome, "other") }, "digest-addressed"},
		{"workspace", func(value *Config) { value.RuntimeWorkspace = "/tmp/work" }, "not supported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cfg
			candidate.MCPEnvironment = map[string]string{"WITSELF_HOME": witselfHome}
			test.edit(&candidate)
			if err := SaveConfig(candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}
