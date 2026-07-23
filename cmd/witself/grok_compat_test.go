package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestGrokCompatibilityPreflightAcceptsFencedHooksAndNativeOverride(t *testing.T) {
	executable := "/opt/homebrew/bin/witself"
	report, err := parseGrokCompatibilityInspection([]byte(`{
  "grokVersion": "test-1",
  "hooks": [
    {
      "target": "'/opt/homebrew/bin/witself' transcript hook --runtime claude-code --account 'default' --realm 'default' --agent 'claude-test-bot'",
      "source": {"type": "user", "path": "/home/test/.claude"},
      "vendor": "claude",
      "compatibilityStatus": "enabled"
    },
    {
      "target": "'/opt/homebrew/bin/witself' transcript hook --runtime grok-build --account 'default' --realm 'default' --agent 'grok-test-bot'",
      "source": {"type": "user", "path": "/home/test/.grok/hooks"}
    }
  ],
  "mcpServers": [
    {
      "name": "witself",
      "target": "/opt/homebrew/bin/witself",
      "source": {"type": "mcpJson", "path": "/home/test/.cursor/mcp.json"},
      "vendor": "cursor",
      "compatibilityStatus": "enabled"
    }
  ]
}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Hooks) != 1 || len(report.MCPServers) != 1 {
		t.Fatalf("report = %#v", report)
	}
	if err := validateGrokCompatibilityReport(report, executable); err != nil {
		t.Fatalf("safe compatibility report rejected: %v", err)
	}
	var warnings bytes.Buffer
	writeGrokCompatibilityWarnings(&warnings, report)
	for _, want := range []string{
		"imports 1 foreign Witself claude hook(s)",
		"Grok-originated events are fenced to the grok-build binding",
		"native Grok user registration will override it",
		"set mcps=false under [compat.cursor]",
	} {
		if !strings.Contains(warnings.String(), want) {
			t.Errorf("warning does not contain %q:\n%s", want, warnings.String())
		}
	}
}

func TestGrokCompatibilityPreflightRejectsUnfencedAndAliasedBindings(t *testing.T) {
	report := grokCompatibilityReport{
		Inspected: true,
		Hooks: []grokCompatibilityFinding{{
			Vendor: "claude", Target: "'/old/bin/witself' transcript hook --runtime claude-code", Source: "/home/test/.claude",
		}},
		MCPServers: []grokCompatibilityFinding{{
			Vendor: "cursor", Name: "witself-cursor", Target: "/opt/homebrew/bin/witself", Source: "/home/test/.cursor/mcp.json",
		}},
	}
	err := validateGrokCompatibilityReport(report, "/opt/homebrew/bin/witself")
	if err == nil {
		t.Fatal("unsafe foreign bindings were accepted")
	}
	for _, want := range []string{
		"cannot be isolated",
		"claude hooks",
		`cursor MCP server "witself-cursor"`,
		"Witself did not change those broad compatibility settings",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q: %v", want, err)
		}
	}
}

func TestGrokHookExecutableValidationParsesRuntimeFormsAndFailsClosed(t *testing.T) {
	executable := "/opt/homebrew/bin/witself"
	for _, test := range []struct {
		name, target string
		candidate    bool
		valid        bool
	}{
		{name: "separate runtime", target: "'/opt/homebrew/bin/witself' transcript hook --runtime claude-code --agent test", candidate: true, valid: true},
		{name: "equals runtime", target: "'/opt/homebrew/bin/witself'   transcript  hook --runtime=claude-code --agent test", candidate: true, valid: true},
		{name: "old executable equals runtime", target: "'/old/bin/witself' transcript hook --runtime=claude-code", candidate: true},
		{name: "missing runtime value", target: "'/opt/homebrew/bin/witself' transcript hook --runtime", candidate: true},
		{name: "ambiguous runtime", target: "'/opt/homebrew/bin/witself' transcript hook --runtime claude-code --runtime=cursor", candidate: true},
		{name: "shell prefix rejected", target: "env MODE=test '/opt/homebrew/bin/witself' transcript hook --runtime claude-code", candidate: true},
		{name: "shell wrapper rejected", target: "sh -c '/old/bin/witself transcript hook --runtime claude-code'", candidate: true},
		{name: "malformed quoting rejected", target: "'/opt/homebrew/bin/witself transcript hook --runtime claude-code", candidate: true},
		{name: "unrelated hook", target: "custom-check --event transcript", candidate: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := grokTranscriptHookCandidate(test.target); got != test.candidate {
				t.Fatalf("candidate = %t, want %t", got, test.candidate)
			}
			if got := grokHookUsesExecutable(test.target, executable); got != test.valid {
				t.Fatalf("valid = %t, want %t", got, test.valid)
			}
		})
	}
}

func TestGrokCompatibilityDetectsShellWrappedWitselfMCP(t *testing.T) {
	for _, target := range []string{
		"sh -c '/old/bin/witself mcp serve --runtime cursor'",
		"sh -c '/old/bin/ws mcp serve --runtime claude-code'",
	} {
		if !looksLikeWitselfMCPServer("foreign-helper", target) {
			t.Fatalf("shell-wrapped Witself MCP target was ignored: %q", target)
		}
	}
}

func TestGrokCompatibilityInspectionRejectsEmptyJSON(t *testing.T) {
	cli := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectGrokCompatibility(cli); err == nil || !strings.Contains(err.Error(), "returned no JSON") {
		t.Fatalf("empty Grok inspection error = %v", err)
	}
}

func TestGrokCompatibilityInspectionRejectsMissingNullAndDriftedSchema(t *testing.T) {
	for name, raw := range map[string]string{
		"empty object":        `{}`,
		"missing version":     `{"hooks":[],"mcpServers":[]}`,
		"null fields":         `{"grokVersion":"test","hooks":null,"mcpServers":null}`,
		"missing MCP servers": `{"grokVersion":"test","hooks":[]}`,
		"missing hooks":       `{"grokVersion":"test","mcpServers":[]}`,
		"error object":        `{"grokVersion":"test","hooks":[],"mcpServers":[],"error":"inspection unavailable"}`,
		"wrong hooks type":    `{"grokVersion":"test","hooks":{},"mcpServers":[]}`,
		"wrong MCP type":      `{"grokVersion":"test","hooks":[],"mcpServers":{}}`,
		"renamed hook target": `{"grokVersion":"test","hooks":[{"command":"witself transcript hook","source":{"type":"user","path":"/tmp/hooks"}}],"mcpServers":[]}`,
		"hook source drift":   `{"grokVersion":"test","hooks":[{"target":"witself transcript hook","source":{"kind":"user","file":"/tmp/hooks"}}],"mcpServers":[]}`,
		"renamed MCP target":  `{"grokVersion":"test","hooks":[],"mcpServers":[{"name":"witself","command":"witself","source":{"type":"user","path":"/tmp/mcp"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if report, err := parseGrokCompatibilityInspection([]byte(raw)); err == nil || report.Inspected {
				t.Fatalf("unsafe inspection accepted: report=%#v err=%v", report, err)
			}
		})
	}
}

func TestForeignGrokCompatibilityHookUsesRuntimeFence(t *testing.T) {
	for _, tc := range []struct {
		name, runtimeName, event string
		want                     bool
	}{
		{name: "Claude imported by Grok", runtimeName: transcriptcapture.RuntimeClaudeCode, event: "SessionStart", want: true},
		{name: "Cursor imported by Grok", runtimeName: transcriptcapture.RuntimeCursor, event: "SessionStart", want: true},
		{name: "native Grok", runtimeName: transcriptcapture.RuntimeGrokBuild, event: "SessionStart"},
		{name: "Claude native invocation", runtimeName: transcriptcapture.RuntimeClaudeCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := foreignGrokCompatibilityHook(tc.runtimeName, tc.event); got != tc.want {
				t.Fatalf("foreignGrokCompatibilityHook(%q, %q) = %t, want %t", tc.runtimeName, tc.event, got, tc.want)
			}
		})
	}
}

func TestTranscriptHookDoesNotQueueForeignBindingInsideGrok(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	t.Setenv(grokHookEventEnv, "Stop")
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeClaudeCode, CaptureMode: transcriptcapture.ModeRaw,
		HookMode: transcriptcapture.HookModeUser, Account: "default", Realm: "default",
		Agent: "claude-test-bot", AgentID: "agent_claude", AgentName: "claude-test-bot", Location: location,
	}); err != nil {
		t.Fatal(err)
	}

	input, err := os.CreateTemp(t.TempDir(), "grok-compat-hook-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	if _, err := input.WriteString(`{"session_id":"grok-session","hook_event_name":"Stop"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	previousStdin := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = previousStdin })

	if code := transcriptHook([]string{
		"--runtime", transcriptcapture.RuntimeClaudeCode,
		"--account", "default", "--realm", "default", "--agent", "claude-test-bot", "--location", "home",
	}); code != 0 {
		t.Fatalf("hook code = %d", code)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("Grok compatibility hook queued %d events against the Claude binding", len(pending))
	}
}

func TestValidateGrokMCPListRequiresExactNativeUserBinding(t *testing.T) {
	serveArgs := []string{
		"/opt/homebrew/bin/witself", "mcp", "serve", "--runtime", "grok-build",
		"--account", "default", "--realm", "default", "--agent", "grok-test-bot", "--location", "home",
	}
	valid := []byte(`[{
  "command": "/opt/homebrew/bin/witself",
  "args": ["mcp", "serve", "--runtime", "grok-build", "--account", "default", "--realm", "default", "--agent", "grok-test-bot", "--location", "home"],
  "enabled": true,
  "name": "witself",
  "scope": "user"
}]`)
	if err := validateGrokMCPList(valid, serveArgs); err != nil {
		t.Fatalf("valid native Grok binding rejected: %v", err)
	}
	missingEnabled := bytes.Replace(valid, []byte("  \"enabled\": true,\n"), nil, 1)
	if bytes.Equal(missingEnabled, valid) {
		t.Fatal("missing-enabled fixture did not remove the enabled field")
	}
	uppercaseAlias := bytes.Replace(valid, []byte(`"name": "witself"`), []byte(`"name": "WITSELF"`), 1)
	twoCaseVariants := []byte(strings.TrimSuffix(string(valid), "]") + "," + strings.TrimPrefix(string(uppercaseAlias), "["))
	envWrappedAlias := []byte(strings.TrimSuffix(string(valid), "]") + `,{
  "command": "env",
  "args": ["MODE=x", "/old/bin/witself", "mcp", "serve", "--runtime", "claude-code"],
  "enabled": true,
  "name": "memory",
  "scope": "user"
}]`)
	shellWrappedAlias := []byte(strings.TrimSuffix(string(valid), "]") + `,{
  "command": "sh",
  "args": ["-c", "/old/bin/witself mcp serve --runtime cursor"],
  "enabled": true,
  "name": "memory",
  "scope": "user"
}]`)
	for name, raw := range map[string][]byte{
		"foreign runtime":        bytes.Replace(valid, []byte(`"grok-build"`), []byte(`"cursor"`), 1),
		"project shadow":         bytes.Replace(valid, []byte(`"scope": "user"`), []byte(`"scope": "project"`), 1),
		"disabled":               bytes.Replace(valid, []byte(`"enabled": true`), []byte(`"enabled": false`), 1),
		"missing enabled":        missingEnabled,
		"case alias plus native": twoCaseVariants,
		"env wrapper alias":      envWrappedAlias,
		"shell wrapper alias":    shellWrappedAlias,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateGrokMCPList(raw, serveArgs); err == nil {
				t.Fatal("unsafe effective Grok MCP binding was accepted")
			}
		})
	}
}

func TestGrokNativeMCPVerificationRejectsEmptyJSON(t *testing.T) {
	cli := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	serveArgs := []string{"/opt/homebrew/bin/witself", "mcp", "serve", "--runtime", "grok-build"}
	if verified, err := verifyGrokNativeMCPBinding(cli, serveArgs); err == nil || verified || !strings.Contains(err.Error(), "returned no JSON") {
		t.Fatalf("empty Grok MCP verification = verified %t, err %v", verified, err)
	}
}
