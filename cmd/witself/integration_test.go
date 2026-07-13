package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestVerifyInstallAgentIdentityPinsRequestedScope(t *testing.T) {
	identity := client.SelfIdentity{
		AccountID: "acc_1", RealmID: "rlm_1", RealmName: "default",
		AgentID: "agt_1", AgentName: "scott",
	}
	tests := []struct {
		name    string
		conn    agentConnection
		mutate  func(*client.SelfIdentity)
		wantErr string
	}{
		{name: "managed binding", conn: agentConnection{AccountID: "acc_1", RealmName: "default", AgentName: "scott"}},
		{name: "explicit token without managed account id", conn: agentConnection{RealmName: "default", AgentName: "scott"}},
		{name: "managed account mismatch", conn: agentConnection{AccountID: "acc_other", RealmName: "default", AgentName: "scott"}, wantErr: "local account"},
		{name: "requested realm mismatch", conn: agentConnection{AccountID: "acc_1", RealmName: "other", AgentName: "scott"}, wantErr: "requested realm"},
		{name: "requested agent mismatch", conn: agentConnection{AccountID: "acc_1", RealmName: "default", AgentName: "other"}, wantErr: "requested agent"},
		{name: "incomplete server identity", conn: agentConnection{AccountID: "acc_1", RealmName: "default", AgentName: "scott"}, mutate: func(identity *client.SelfIdentity) {
			identity.RealmID = ""
		}, wantErr: "incomplete agent identity"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotIdentity := identity
			if tc.mutate != nil {
				tc.mutate(&gotIdentity)
			}
			err := verifyInstallAgentIdentity(tc.conn, gotIdentity)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("identity error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestInstallCodexRegistersMCPAndHooksWithoutEmbeddingToken(t *testing.T) {
	home := t.TempDir()
	setInstallExecutableForTest(t)
	witselfHome := filepath.Join(home, ".witself")
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", witselfHome)
	t.Setenv("CODEX_HOME", codexHome)
	logPath := filepath.Join(home, "codex-args.log")
	t.Setenv("FAKE_CLI_LOG", logPath)
	fakeCLI := filepath.Join(home, "codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_CLI_LOG\"\nexit 0\n"
	if err := os.WriteFile(fakeCLI, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_CLI_PATH", fakeCLI)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer agent-token" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","realm_id":"realm_1","realm_name":"default","agent_id":"agent_1","agent_name":"scott"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`))
	}))
	defer srv.Close()
	tokenPath := filepath.Join(home, "agent.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := installCmd([]string{
		"codex", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--capture", "raw", "--endpoint", srv.URL,
		"--token-file", tokenPath, "--user-hooks",
	}); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccountID != "acc_1" || cfg.RealmID != "realm_1" ||
		cfg.AgentID != "agent_1" || cfg.AgentName != "scott" || cfg.Location.Name != "home" {
		t.Fatalf("config = %#v", cfg)
	}
	hooks, err := os.ReadFile(filepath.Join(codexHome, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	cliLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	combined := string(hooks) + string(cliLog)
	if strings.Contains(combined, "agent-token") {
		t.Fatal("agent token was embedded in runtime configuration")
	}
	if !strings.Contains(string(hooks), "transcript hook --runtime codex") ||
		!strings.Contains(string(hooks), "--account 'default' --realm 'default' --agent 'scott' --location 'home'") ||
		!strings.Contains(string(cliLog), "mcp add witself --") ||
		!strings.Contains(string(cliLog), "mcp serve --runtime codex --account default --realm default --agent scott --location home") {
		t.Fatalf("hooks/log = %s\n---\n%s", hooks, cliLog)
	}
}

func TestBareInstallReusesBindingAndDefaultsToManagedHooks(t *testing.T) {
	home := t.TempDir()
	setInstallExecutableForTest(t)
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv(managedHooksTestRootEnv, filepath.Join(home, "managed"))
	logPath := filepath.Join(home, "codex-args.log")
	t.Setenv("FAKE_CLI_LOG", logPath)
	fakeCLI := filepath.Join(home, "codex")
	if err := os.WriteFile(fakeCLI, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_CLI_LOG\"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_CLI_PATH", fakeCLI)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer agent-token" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","realm_id":"realm_1","realm_name":"default","agent_id":"agent_1","agent_name":"scott"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`))
	}))
	defer srv.Close()
	tokenPath := filepath.Join(home, "agent.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loc, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCodex, CaptureMode: transcriptcapture.ModeRaw,
		HookMode: transcriptcapture.HookModeUser, Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Endpoint: srv.URL, TokenFile: tokenPath, Location: loc,
	}); err != nil {
		t.Fatal(err)
	}
	if code := installCmd([]string{"codex"}); code != 0 {
		t.Fatalf("bare install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HookMode != transcriptcapture.HookModeManaged || cfg.Agent != "scott" || cfg.Location.Name != "home" {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestInferInstallAgentRequiresOnlyAmbiguousChoice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	if err := local.Save("default", local.Account{ID: "acc_1"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	writeAgent := func(name string) {
		t.Helper()
		path, err := local.AgentTokenPath("default", "default", name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAgent("scott")
	name, err := inferInstallAgent("", "")
	if err != nil || name != "scott" {
		t.Fatalf("inferred %q, err %v", name, err)
	}
	writeAgent("alex")
	if _, err := inferInstallAgent("", ""); err == nil || !strings.Contains(err.Error(), "multiple local agents") {
		t.Fatalf("ambiguous error = %v", err)
	}
}

func TestRuntimeTargetsNormalizeAliasesAndPreserveOrder(t *testing.T) {
	got, err := runtimeTargets("claude,codex,grok,cursor,claude-code")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	if _, err := runtimeTargets("codex,,cursor"); err == nil {
		t.Fatal("empty runtime item was accepted")
	}
}

func TestCommaSeparatedInstallConfiguresEveryRuntime(t *testing.T) {
	home := t.TempDir()
	setInstallExecutableForTest(t)
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	t.Setenv("CURSOR_CONFIG_DIR", filepath.Join(home, ".cursor"))
	managedRoot := filepath.Join(home, "managed")
	t.Setenv(managedHooksTestRootEnv, managedRoot)

	for envName, cliName := range map[string]string{
		"CODEX_CLI_PATH":  "codex",
		"CLAUDE_CLI_PATH": "claude",
		"GROK_CLI_PATH":   "grok",
		"CURSOR_CLI_PATH": "cursor",
	} {
		fakeCLI := filepath.Join(home, cliName)
		if err := os.WriteFile(fakeCLI, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(envName, fakeCLI)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer agent-token" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","realm_id":"realm_1","realm_name":"default","agent_id":"agent_1","agent_name":"scott"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`))
	}))
	defer srv.Close()
	tokenPath := filepath.Join(home, "agent.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := installCmd([]string{
		"claude,codex,grok,cursor", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--capture", "raw", "--endpoint", srv.URL, "--token-file", tokenPath,
	}); code != 0 {
		t.Fatalf("install code = %d", code)
	}

	for _, tc := range []struct {
		runtime  string
		hookMode string
	}{
		{transcriptcapture.RuntimeClaudeCode, transcriptcapture.HookModeManaged},
		{transcriptcapture.RuntimeCodex, transcriptcapture.HookModeManaged},
		{transcriptcapture.RuntimeGrokBuild, transcriptcapture.HookModeUser},
		{transcriptcapture.RuntimeCursor, transcriptcapture.HookModeUser},
	} {
		cfg, err := transcriptcapture.LoadConfig(tc.runtime)
		if err != nil {
			t.Fatalf("load %s config: %v", tc.runtime, err)
		}
		if cfg.HookMode != tc.hookMode || cfg.Agent != "scott" || cfg.Location.Name != "home" {
			t.Fatalf("%s config = %#v", tc.runtime, cfg)
		}
	}
	for _, path := range []string{
		filepath.Join(managedRoot, "claude-code", "managed-settings.d", "50-witself.json"),
		filepath.Join(managedRoot, "codex", "requirements.toml"),
		filepath.Join(home, ".grok", "hooks", "witself.json"),
		filepath.Join(home, ".cursor", "hooks.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected hook configuration %s: %v", path, err)
		}
	}
}

func TestCommaSeparatedUninstallRemovesEveryRuntimeBinding(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	t.Setenv("CURSOR_CONFIG_DIR", filepath.Join(home, ".cursor"))
	loc, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	for _, runtimeName := range []string{
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor,
	} {
		if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
			Runtime: runtimeName, CaptureMode: transcriptcapture.ModeRaw, HookMode: transcriptcapture.HookModeUser,
			Account: "default", Realm: "default", Agent: "scott",
			AgentID: "agent_1", AgentName: "scott", Location: loc,
		}); err != nil {
			t.Fatal(err)
		}
	}
	fakeCLI := filepath.Join(home, "runtime-cli")
	if err := os.WriteFile(fakeCLI, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, envName := range []string{"CLAUDE_CLI_PATH", "CODEX_CLI_PATH", "GROK_CLI_PATH", "CURSOR_CLI_PATH"} {
		t.Setenv(envName, fakeCLI)
	}
	if code := uninstallCmd([]string{"claude,codex,grok,cursor"}); code != 0 {
		t.Fatalf("uninstall code = %d", code)
	}
	for _, runtimeName := range []string{
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor,
	} {
		path, err := transcriptcapture.ConfigPath(runtimeName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s binding remains: %v", runtimeName, err)
		}
	}
}

func TestManagedInstallAndUninstallAcrossRuntimes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		installName   string
		runtime       string
		cliEnv        string
		configRootEnv string
		configDir     string
		policyPath    func(string) string
		routingPath   func(string) string
		routingText   string
		removeRouting bool
	}{
		{
			name: "codex", installName: "codex", runtime: transcriptcapture.RuntimeCodex,
			cliEnv: "CODEX_CLI_PATH", configRootEnv: "CODEX_HOME", configDir: ".codex",
			policyPath:  func(root string) string { return filepath.Join(root, "codex", "requirements.toml") },
			routingPath: func(root string) string { return filepath.Join(root, "AGENTS.md") },
			routingText: codexMemoryRoutingInstructions,
		},
		{
			name: "claude", installName: "claude", runtime: transcriptcapture.RuntimeClaudeCode,
			cliEnv: "CLAUDE_CLI_PATH", configRootEnv: "CLAUDE_CONFIG_DIR", configDir: ".claude",
			policyPath: func(root string) string {
				return filepath.Join(root, "claude-code", "managed-settings.d", "50-witself.json")
			},
			routingPath:   func(root string) string { return filepath.Join(root, "rules", claudeMemoryRoutingRuleFile) },
			routingText:   runtimeNeutralMemoryRoutingInstructions,
			removeRouting: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setInstallExecutableForTest(t)
			witselfHome := filepath.Join(home, ".witself")
			runtimeRoot := filepath.Join(home, tc.configDir)
			managedRoot := filepath.Join(home, "managed")
			t.Setenv("HOME", home)
			t.Setenv("WITSELF_HOME", witselfHome)
			t.Setenv(tc.configRootEnv, runtimeRoot)
			t.Setenv(managedHooksTestRootEnv, managedRoot)

			logPath := filepath.Join(home, tc.name+"-args.log")
			t.Setenv("FAKE_CLI_LOG", logPath)
			fakeCLI := filepath.Join(home, tc.name)
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_CLI_LOG\"\nexit 0\n"
			if err := os.WriteFile(fakeCLI, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv(tc.cliEnv, fakeCLI)

			settingsPath := filepath.Join(runtimeRoot, "settings.json")
			if tc.runtime == transcriptcapture.RuntimeCodex {
				settingsPath = filepath.Join(runtimeRoot, "hooks.json")
			}
			if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
				t.Fatal(err)
			}
			unrelated := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"custom-check"}]}]}}`
			if err := os.WriteFile(settingsPath, []byte(unrelated), 0o600); err != nil {
				t.Fatal(err)
			}

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer agent-token" {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","realm_id":"realm_1","realm_name":"default","agent_id":"agent_1","agent_name":"scott"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`))
			}))
			defer srv.Close()
			tokenPath := filepath.Join(home, "agent.token")
			if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			if code := installCmd([]string{
				tc.installName, "--account", "default", "--realm", "default", "--agent", "scott",
				"--location", "home", "--capture", "raw", "--endpoint", srv.URL,
				"--token-file", tokenPath, "--managed-hooks",
			}); code != 0 {
				t.Fatalf("install code = %d", code)
			}
			cfg, err := transcriptcapture.LoadConfig(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.HookMode != transcriptcapture.HookModeManaged {
				t.Fatalf("hook mode = %q", cfg.HookMode)
			}
			policy, err := os.ReadFile(tc.policyPath(managedRoot))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(policy), "--account 'default' --realm 'default' --agent 'scott' --location 'home'") {
				t.Fatalf("managed hook does not pin verified binding:\n%s", policy)
			}
			raw, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "custom-check") || strings.Contains(string(raw), "transcript hook --runtime") {
				t.Fatalf("user settings = %s", raw)
			}
			routing, err := os.ReadFile(tc.routingPath(runtimeRoot))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(routing), tc.routingText) {
				t.Fatalf("managed memory routing = %s", routing)
			}

			if code := uninstallCmd([]string{tc.installName}); code != 0 {
				t.Fatalf("uninstall code = %d", code)
			}
			if _, err := os.Stat(tc.policyPath(managedRoot)); !os.IsNotExist(err) {
				t.Fatalf("managed policy still exists: %v", err)
			}
			configPath, err := transcriptcapture.ConfigPath(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Fatalf("integration config still exists: %v", err)
			}
			if _, err := os.Stat(tokenPath); err != nil {
				t.Fatalf("token was removed: %v", err)
			}
			if tc.removeRouting {
				if _, err := os.Stat(tc.routingPath(runtimeRoot)); !os.IsNotExist(err) {
					t.Fatalf("dedicated memory-routing rule still exists: %v", err)
				}
			} else {
				routing, err := os.ReadFile(tc.routingPath(runtimeRoot))
				if err != nil {
					t.Fatal(err)
				}
				if len(routing) != 0 {
					t.Fatalf("shared instruction file retained managed bytes: %q", routing)
				}
			}
		})
	}
}

func TestGlobalUserInstallAndUninstallAcrossNativeRuntimes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		installName   string
		runtime       string
		cliEnv        string
		configRootEnv string
		hookPath      func(string) string
		routingPath   func(string) string
		routingText   string
	}{
		{
			name: "grok", installName: "grok", runtime: transcriptcapture.RuntimeGrokBuild,
			cliEnv: "GROK_CLI_PATH", configRootEnv: "GROK_HOME",
			hookPath:    func(root string) string { return filepath.Join(root, "hooks", "witself.json") },
			routingPath: func(root string) string { return filepath.Join(root, "AGENTS.md") },
			routingText: grokPortableMemoryRoutingInstructions,
		},
		{
			name: "cursor", installName: "cursor", runtime: transcriptcapture.RuntimeCursor,
			cliEnv: "CURSOR_CLI_PATH", configRootEnv: "CURSOR_CONFIG_DIR",
			hookPath: func(root string) string { return filepath.Join(root, "hooks.json") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setInstallExecutableForTest(t)
			runtimeRoot := filepath.Join(home, "."+tc.name)
			t.Setenv("HOME", home)
			t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
			t.Setenv(tc.configRootEnv, runtimeRoot)

			logPath := filepath.Join(home, tc.name+"-args.log")
			t.Setenv("FAKE_CLI_LOG", logPath)
			fakeCLI := filepath.Join(home, tc.name)
			if err := os.WriteFile(fakeCLI, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_CLI_LOG\"\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv(tc.cliEnv, fakeCLI)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer agent-token" {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","realm_id":"realm_1","realm_name":"default","agent_id":"agent_1","agent_name":"scott"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`))
			}))
			defer srv.Close()
			tokenPath := filepath.Join(home, "agent.token")
			if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			if code := installCmd([]string{
				tc.installName, "--account", "default", "--realm", "default", "--agent", "scott",
				"--location", "home", "--capture", "raw", "--endpoint", srv.URL, "--token-file", tokenPath,
			}); code != 0 {
				t.Fatalf("install code = %d", code)
			}
			cfg, err := transcriptcapture.LoadConfig(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.HookMode != transcriptcapture.HookModeUser || cfg.Agent != "scott" || cfg.Location.Name != "home" {
				t.Fatalf("config = %#v", cfg)
			}
			hooks, err := os.ReadFile(tc.hookPath(runtimeRoot))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(hooks), "--runtime "+tc.runtime) || !strings.Contains(string(hooks), "--agent 'scott' --location 'home'") {
				t.Fatalf("hooks = %s", hooks)
			}
			if tc.routingPath != nil {
				routing, err := os.ReadFile(tc.routingPath(runtimeRoot))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(routing), tc.routingText) {
					t.Fatalf("managed memory routing = %s", routing)
				}
			}
			if tc.runtime == transcriptcapture.RuntimeGrokBuild {
				log, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(log), "mcp add --scope user witself --") || !strings.Contains(string(log), "mcp serve --runtime grok-build") {
					t.Fatalf("Grok CLI log = %s", log)
				}
			} else {
				mcpConfig, err := os.ReadFile(filepath.Join(runtimeRoot, "mcp.json"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(mcpConfig), `"witself"`) || !strings.Contains(string(mcpConfig), `"cursor"`) || !strings.Contains(string(mcpConfig), `"scott"`) {
					t.Fatalf("Cursor MCP config = %s", mcpConfig)
				}
				log, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(log), "agent mcp enable witself") {
					t.Fatalf("Cursor CLI log = %s", log)
				}
			}

			if code := uninstallCmd([]string{tc.installName}); code != 0 {
				t.Fatalf("uninstall code = %d", code)
			}
			if _, err := os.Stat(tc.hookPath(runtimeRoot)); !os.IsNotExist(err) {
				t.Fatalf("hook file still exists: %v", err)
			}
			configPath, err := transcriptcapture.ConfigPath(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Fatalf("integration config still exists: %v", err)
			}
			if _, err := os.Stat(tokenPath); err != nil {
				t.Fatalf("token was removed: %v", err)
			}
			if tc.routingPath != nil {
				routing, err := os.ReadFile(tc.routingPath(runtimeRoot))
				if err != nil {
					t.Fatal(err)
				}
				if len(routing) != 0 {
					t.Fatalf("shared instruction file retained managed bytes: %q", routing)
				}
			}
			if tc.runtime == transcriptcapture.RuntimeCursor {
				log, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(log), "agent mcp disable witself") {
					t.Fatalf("Cursor uninstall log = %s", log)
				}
			}
		})
	}
}

func TestCurrentExecutablePathRejectsGoRunBinary(t *testing.T) {
	t.Setenv(witselfExecutableTestEnv, "")
	original := os.Args[0]
	os.Args[0] = filepath.Join(t.TempDir(), "go-build123", "b001", "exe", "witself")
	t.Cleanup(func() { os.Args[0] = original })
	if _, err := currentExecutablePath(); err == nil || !strings.Contains(err.Error(), "temporary executable") {
		t.Fatalf("error = %v", err)
	}
}

func TestDetectRuntimeVersion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   string
	}{
		{"codex", "codex-cli 0.30.0", "0.30.0"},
		{"claude", "2.1.197 (Claude Code)", "2.1.197"},
		{"grok", "grok 0.2.93 (f00f96316d4b) [stable]", "0.2.93"},
		{"cursor", "3.11.13\ncommit\narm64", "3.11.13"},
		{"fallback", "development-build", "development-build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtime")
			script := "#!/bin/sh\nprintf '%s\\n' '" + tc.output + "'\n"
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if got := detectRuntimeVersion(path); got != tc.want {
				t.Fatalf("version = %q, want %q", got, tc.want)
			}
		})
	}
}

func setInstallExecutableForTest(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(witselfExecutableTestEnv, executable)
}

func TestRegisterMCPClaudeUsesUserScopedStdio(t *testing.T) {
	home := t.TempDir()
	logPath := filepath.Join(home, "claude-args.log")
	t.Setenv("FAKE_CLI_LOG", logPath)
	fakeCLI := filepath.Join(home, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_CLI_LOG\"\nexit 0\n"
	if err := os.WriteFile(fakeCLI, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := registerMCP(transcriptcapture.RuntimeClaudeCode, fakeCLI, "/usr/local/bin/witself", "account-under-test", "realm-under-test", "agent-under-test", "home"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	for _, want := range []string{
		"mcp remove --scope user witself",
		"mcp add --scope user --transport stdio witself -- /usr/local/bin/witself mcp serve --runtime claude-code --account account-under-test --realm realm-under-test --agent agent-under-test --location home",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("Claude registration log %q does not contain %q", log, want)
		}
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := registerMCP(transcriptcapture.RuntimeClaudeCode, fakeCLI, "/usr/local/bin/witself", "account-under-test", "realm-under-test", "agent-under-test", ""); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "--account account-under-test --realm realm-under-test --agent agent-under-test") || strings.Contains(string(raw), "--location") {
		t.Fatalf("unlabeled MCP registration = %q", raw)
	}
}

func TestCaptureOutboxFlushesWholeVisibleTurn(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	var mu sync.Mutex
	var appended []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			var body struct {
				Entries []map[string]any `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode batch: %v", err)
				http.Error(w, "bad batch", http.StatusBadRequest)
				return
			}
			mu.Lock()
			appended = append(appended, body.Entries...)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loc, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeClaudeCode, CaptureMode: transcriptcapture.ModeRaw,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: loc,
		Endpoint: srv.URL, TokenFile: tokenPath,
	}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"session_id":"session-1","hook_event_name":"SessionStart","cwd":"/src/witself"}`,
		`{"session_id":"session-1","hook_event_name":"UserPromptSubmit","prompt":"the full original prompt"}`,
		`{"session_id":"session-1","hook_event_name":"Stop","last_assistant_message":"the full final response"}`,
	} {
		if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if code := transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode}); code != 0 {
		t.Fatalf("flush code = %d", code)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %d", len(pending))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(appended) != 3 {
		t.Fatalf("appended = %#v", appended)
	}
	if appended[1]["body"] != "the full original prompt" || appended[2]["body"] != "the full final response" {
		t.Fatalf("visible turn = %#v", appended)
	}
	if appended[2]["reply_to_external_id"] != appended[1]["external_id"] {
		t.Fatalf("reply link = %#v", appended[2])
	}
}

func TestCaptureFlushDrainsEventsQueuedWhileRunning(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	started := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	appended := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			once.Do(func() {
				close(started)
				<-resume
			})
			mu.Lock()
			appended++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loc, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCodex, CaptureMode: transcriptcapture.ModeMessages,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: loc,
		Endpoint: srv.URL, TokenFile: tokenPath,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, []byte(`{"session_id":"session-1","hook_event_name":"SessionStart"}`)); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeCodex}) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("flush did not start")
	}
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, []byte(`{"session_id":"session-1","hook_event_name":"UserPromptSubmit","prompt":"queued during flush"}`)); err != nil {
		t.Fatal(err)
	}
	close(resume)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("flush code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("flush did not finish")
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pending) != 0 || appended != 2 {
		t.Fatalf("pending/appended = %d/%d", len(pending), appended)
	}
}
