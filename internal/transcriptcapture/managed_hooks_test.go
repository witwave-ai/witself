package transcriptcapture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexManagedHooksPreservePolicyAndAreIdempotent(t *testing.T) {
	opts := managedHooksTestOptions(t, RuntimeCodex, ModeRaw)
	initial := "# existing administrator policy\nallowed_approval_policies = [\"on-request\"]\n"
	if err := os.MkdirAll(filepath.Dir(opts.CodexRequirementsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opts.CodexRequirementsPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := InstallManagedHooks(opts); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(opts.CodexRequirementsPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Count(text, codexManagedBlockBegin) != 1 || !strings.Contains(text, initial) {
		t.Fatalf("managed requirements were not idempotent or lost policy:\n%s", text)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "PreToolUse", "PostToolUse"} {
		if !strings.Contains(text, "[[hooks."+event+"]]") {
			t.Errorf("missing %s", event)
		}
	}
	for _, event := range []string{"SessionEnd", "StopFailure", "PostToolUseFailure"} {
		if strings.Contains(text, "[[hooks."+event+"]]") {
			t.Errorf("unexpected Codex event %s", event)
		}
	}
	runner := filepath.Join(opts.CodexManagedDir, managedRunnerName)
	assertManagedRunner(t, runner, opts.Executable)
	assertPinnedManagedScope(t, text, opts.Account, opts.Realm)
	assertPinnedManagedAgent(t, text, opts.Agent)
	assertPinnedManagedLocation(t, text, opts.Location)

	opts.Mode = ModeMessages
	if _, err := InstallManagedHooks(opts); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(opts.CodexRequirementsPath)
	if err != nil {
		t.Fatal(err)
	}
	text = string(raw)
	if strings.Contains(text, "[[hooks.PreToolUse]]") || strings.Count(text, codexManagedBlockBegin) != 1 {
		t.Fatalf("mode update left stale or duplicate hooks:\n%s", text)
	}

	if _, err := RemoveManagedHooks(opts); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(opts.CodexRequirementsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != initial {
		t.Fatalf("existing policy changed after removal:\n%s", raw)
	}
	if _, err := os.Stat(runner); !os.IsNotExist(err) {
		t.Fatalf("runner still exists: %v", err)
	}
}

func TestCodexManagedHooksReuseExistingManagedDirectory(t *testing.T) {
	opts := managedHooksTestOptions(t, RuntimeCodex, ModeMessages)
	existingDir := filepath.Join(t.TempDir(), "enterprise-hooks")
	initial := "[features]\nhooks = true\n\n[hooks]\nmanaged_dir = " + tomlTestQuote(existingDir) + "\n"
	if err := os.MkdirAll(filepath.Dir(opts.CodexRequirementsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opts.CodexRequirementsPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallManagedHooks(opts); err != nil {
		t.Fatal(err)
	}
	assertManagedRunner(t, filepath.Join(existingDir, managedRunnerName), opts.Executable)
	raw, err := os.ReadFile(opts.CodexRequirementsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "[hooks]") != 1 || !strings.Contains(string(raw), codexManagedBlockBegin) {
		t.Fatalf("existing hooks table was not reused:\n%s", raw)
	}
}

func TestCodexManagedHooksRespectDisabledPolicy(t *testing.T) {
	opts := managedHooksTestOptions(t, RuntimeCodex, ModeRaw)
	if err := os.MkdirAll(filepath.Dir(opts.CodexRequirementsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opts.CodexRequirementsPath, []byte("[features]\nhooks = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallManagedHooks(opts); err == nil || !strings.Contains(err.Error(), "disables hooks") {
		t.Fatalf("error = %v", err)
	}
}

func TestClaudeManagedHooksUseIsolatedDropIn(t *testing.T) {
	opts := managedHooksTestOptions(t, RuntimeClaudeCode, ModeRaw)
	if _, err := InstallManagedHooks(opts); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(opts.ClaudeSettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok || len(hooks) != 15 {
		t.Fatalf("hooks = %#v", root["hooks"])
	}
	for _, event := range []string{
		"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd",
		"SubagentStart", "SubagentStop", "PreCompact", "PostCompact",
		"PreToolUse", "PermissionRequest", "PermissionDenied",
		"PostToolUse", "PostToolUseFailure", "Notification",
	} {
		if _, ok := hooks[event]; !ok {
			t.Errorf("missing %s", event)
		}
	}
	runner := filepath.Join(opts.ClaudeManagedDir, managedRunnerName)
	assertManagedRunner(t, runner, opts.Executable)
	assertPinnedManagedScope(t, string(raw), opts.Account, opts.Realm)
	assertPinnedManagedAgent(t, string(raw), opts.Agent)
	assertPinnedManagedLocation(t, string(raw), opts.Location)

	opts.Mode = ModeMessages
	if _, err := InstallManagedHooks(opts); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(opts.ClaudeSettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PreToolUse") {
		t.Fatalf("mode update left trace hooks:\n%s", raw)
	}
	if _, err := RemoveManagedHooks(opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(opts.ClaudeSettingsPath); !os.IsNotExist(err) {
		t.Fatalf("drop-in still exists: %v", err)
	}
	if _, err := os.Stat(runner); !os.IsNotExist(err) {
		t.Fatalf("runner still exists: %v", err)
	}
}

func TestRemoveHooksPreservesUnrelatedUserSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{"env":{"EXISTING":"yes"},"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"custom-check"}]}]}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallHooks(RuntimeClaudeCode, ModeRaw, "/usr/local/bin/witself", "default", "default", "scott", "home"); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveHooks(RuntimeClaudeCode); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), hookCommandMarker) || !strings.Contains(string(raw), "custom-check") || !strings.Contains(string(raw), "EXISTING") {
		t.Fatalf("unexpected settings after removal:\n%s", raw)
	}
}

func managedHooksTestOptions(t *testing.T, runtimeName, mode string) ManagedHooksOptions {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	return ManagedHooksOptions{
		Runtime:               runtimeName,
		Mode:                  mode,
		Executable:            executable,
		Account:               "account-under-test",
		Realm:                 "realm-under-test",
		Agent:                 "agent-under-test",
		Location:              "home",
		CodexRequirementsPath: filepath.Join(root, "codex", "requirements.toml"),
		CodexManagedDir:       filepath.Join(root, "codex", "witself-hooks"),
		ClaudeSettingsPath:    filepath.Join(root, "claude-code", "managed-settings.d", "50-witself.json"),
		ClaudeManagedDir:      filepath.Join(root, "claude-code", "witself-hooks"),
	}
}

func assertManagedRunner(t *testing.T, path, executable string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), executable) || !strings.Contains(string(raw), `"$@"`) {
		t.Fatalf("runner = %q", raw)
	}
	if strings.Contains(string(raw), "--runtime") || strings.Contains(string(raw), "--agent") {
		t.Fatalf("runner should forward the policy binding: %q", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("runner mode = %o", info.Mode().Perm())
	}
}

func assertPinnedManagedAgent(t *testing.T, policy, agent string) {
	t.Helper()
	if !strings.Contains(policy, "--agent '"+agent+"'") {
		t.Fatalf("managed policy does not pin agent %q:\n%s", agent, policy)
	}
}

func assertPinnedManagedScope(t *testing.T, policy, account, realm string) {
	t.Helper()
	if !strings.Contains(policy, "--account '"+account+"' --realm '"+realm+"'") {
		t.Fatalf("managed policy does not pin account %q and realm %q:\n%s", account, realm, policy)
	}
}

func assertPinnedManagedLocation(t *testing.T, policy, location string) {
	t.Helper()
	if !strings.Contains(policy, "--location '"+location+"'") {
		t.Fatalf("managed policy does not pin location %q:\n%s", location, policy)
	}
}

func tomlTestQuote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}
