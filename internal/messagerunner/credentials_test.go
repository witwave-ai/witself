package messagerunner

import (
	"os"
	"strings"
	"testing"
)

func TestConfigStoreCapturesOnlyProviderScopedCredentials(t *testing.T) {
	store := testConfigStore(t, "claude-code")
	environment := []string{
		"CLAUDE_CODE_OAUTH_TOKEN=claude-oauth-secret",
		"ANTHROPIC_BASE_URL=https://provider.example",
		"XAI_API_KEY=must-not-cross",
		"AWS_SECRET_ACCESS_KEY=must-not-enter",
		"WITSELF_TOKEN=must-not-enter",
	}
	if err := store.CaptureProviderCredentials("claude-code", environment); err != nil {
		t.Fatal(err)
	}
	values, err := store.ProviderEnvironment("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(values, "\n")
	if !strings.Contains(joined, "CLAUDE_CODE_OAUTH_TOKEN=claude-oauth-secret") ||
		!strings.Contains(joined, "ANTHROPIC_BASE_URL=https://provider.example") ||
		strings.Contains(joined, "XAI") || strings.Contains(joined, "AWS") || strings.Contains(joined, "WITSELF") {
		t.Fatalf("captured provider environment = %q", joined)
	}
	info, err := os.Stat(store.credentialsPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file = %v / %v", info, err)
	}
	if _, err := store.ProviderEnvironment("grok-build"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("cross-provider load error = %v", err)
	}
}

func TestConfigStorePreservesSameProviderCredentialsWhenShellValueIsAbsent(t *testing.T) {
	store := testConfigStore(t, "claude-code")
	if err := store.CaptureProviderCredentials("claude-code", []string{"ANTHROPIC_API_KEY=secret"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CaptureProviderCredentials("claude-code", nil); err != nil {
		t.Fatal(err)
	}
	values, err := store.ProviderEnvironment("claude-code")
	if err != nil || len(values) != 1 || values[0] != "ANTHROPIC_API_KEY=secret" {
		t.Fatalf("preserved values = %#v / %v", values, err)
	}
	if err := store.CaptureProviderCredentials("grok-build", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.credentialsPath()); !os.IsNotExist(err) {
		t.Fatalf("old provider credential file remains: %v", err)
	}
}
