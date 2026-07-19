package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallRoutingOnlyRefreshesCodexAndClaudeWithoutRuntimeAccess(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex")
	claudeHome := filepath.Join(root, "claude")
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("PATH", t.TempDir())

	if code := installCmd([]string{"codex,claude", "--routing-only"}); code != 0 {
		t.Fatalf("install routing-only code = %d, want 0", code)
	}
	checks := []struct {
		path, begin, end string
	}{
		{filepath.Join(codexHome, "AGENTS.md"), codexMemoryRoutingBeginMarker, codexMemoryRoutingEndMarker},
		{filepath.Join(claudeHome, "rules", claudeMemoryRoutingRuleFile), claudeMemoryRoutingBeginMarker, claudeMemoryRoutingEndMarker},
	}
	for _, check := range checks {
		raw, err := os.ReadFile(check.path)
		if err != nil {
			t.Fatalf("read refreshed routing %s: %v", check.path, err)
		}
		text := string(raw)
		for _, want := range []string{check.begin, check.end, "## Witself agent avatar", "pending avatar_checkpoint"} {
			if !strings.Contains(text, want) {
				t.Errorf("routing %s omitted %q", check.path, want)
			}
		}
	}

	// The early routing-only path must not need or create a runtime binding,
	// MCP registration, or hook installation. An empty PATH above proves no
	// provider CLI was executed; a second pass proves the refresh is idempotent.
	if code := installCmd([]string{"codex,claude", "--routing-only"}); code != 0 {
		t.Fatalf("second install routing-only code = %d, want 0", code)
	}
}

func TestInstallRoutingOnlyRejectsBindingAndHookFlags(t *testing.T) {
	for _, args := range [][]string{
		{"codex", "--routing-only", "--agent", "scott"},
		{"claude", "--routing-only", "--user-hooks"},
		{"codex", "--routing-only", "--capture", "messages"},
	} {
		if code := installCmd(args); code != 2 {
			t.Errorf("install %v code = %d, want 2", args, code)
		}
	}
}
