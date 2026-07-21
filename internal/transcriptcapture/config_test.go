package transcriptcapture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
