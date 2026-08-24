package supportrunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFromEnvDefaults(t *testing.T) {
	cfg, err := FromEnv(mapLookup(nil))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("Enabled = true, want dark default")
	}
	if cfg.ControlPlane != defaultControlPlane || cfg.Model != defaultModel {
		t.Fatalf("default endpoint/model = %q/%q", cfg.ControlPlane, cfg.Model)
	}
	if cfg.Interval != defaultInterval ||
		cfg.MaxTicketsPerTick != defaultMaxTicketsPerTick ||
		cfg.LLMTimeout != defaultLLMTimeout ||
		cfg.MaxAssistantReplies != defaultMaxAssistantReplies ||
		cfg.Lookback != defaultLookback {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestFromEnvOverridesAndCredentialFiles(t *testing.T) {
	dir := t.TempDir()
	adminFile := filepath.Join(dir, "admin.token")
	apiKeyFile := filepath.Join(dir, "anthropic.key")
	if err := os.WriteFile(adminFile, []byte(" file-admin \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(apiKeyFile, []byte(" file-api-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := FromEnv(mapLookup(map[string]string{
		enabledEnv:             " TRUE ",
		controlPlaneEnv:        " http://127.0.0.1:8080 ",
		adminTokenFileEnv:      adminFile,
		adminTokenEnv:          "inline-must-not-win",
		anthropicAPIKeyFileEnv: apiKeyFile,
		anthropicAPIKeyEnv:     "inline-must-not-win",
		modelEnv:               " test-model ",
		intervalEnv:            "2m",
		maxTicketsPerTickEnv:   "9",
		llmTimeoutEnv:          "45s",
		maxAssistantRepliesEnv: "4",
		lookbackEnv:            "48h",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.Enabled || cfg.ControlPlane != "http://127.0.0.1:8080" || cfg.Model != "test-model" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if cfg.Interval != 2*time.Minute || cfg.MaxTicketsPerTick != 9 ||
		cfg.LLMTimeout != 45*time.Second || cfg.MaxAssistantReplies != 4 ||
		cfg.Lookback != 48*time.Hour {
		t.Fatalf("numeric overrides not applied: %+v", cfg)
	}
	if cfg.adminToken != "file-admin" || cfg.anthropicAPIKey != "file-api-key" {
		t.Fatal("credential files did not take precedence over inline values")
	}
}

func TestFromEnvInlineCredentials(t *testing.T) {
	cfg, err := FromEnv(mapLookup(map[string]string{
		enabledEnv:         "true",
		adminTokenEnv:      " admin ",
		anthropicAPIKeyEnv: " key ",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.adminToken != "admin" || cfg.anthropicAPIKey != "key" {
		t.Fatalf("inline credentials = %q/%q", cfg.adminToken, cfg.anthropicAPIKey)
	}
}

func TestFromEnvDisabledDoesNotReadCredentialFiles(t *testing.T) {
	cfg, err := FromEnv(mapLookup(map[string]string{
		enabledEnv:             "false",
		adminTokenFileEnv:      "/definitely/not/present",
		anthropicAPIKeyFileEnv: "/also/not/present",
	}))
	if err != nil {
		t.Fatalf("disabled FromEnv touched a credential file: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("disabled config became enabled")
	}
}

func TestFromEnvRejectsInvalidValues(t *testing.T) {
	emptyFile := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(emptyFile, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	validSecrets := map[string]string{
		enabledEnv:         "true",
		adminTokenEnv:      "admin",
		anthropicAPIKeyEnv: "key",
	}
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "enablement", env: map[string]string{enabledEnv: "1"}, want: enabledEnv},
		{name: "endpoint", env: map[string]string{controlPlaneEnv: "localhost"}, want: controlPlaneEnv},
		{name: "model", env: map[string]string{modelEnv: ""}, want: modelEnv},
		{name: "interval parse", env: map[string]string{intervalEnv: "soon"}, want: intervalEnv},
		{name: "interval bound", env: map[string]string{intervalEnv: "0s"}, want: intervalEnv},
		{name: "ticket cap parse", env: map[string]string{maxTicketsPerTickEnv: "many"}, want: maxTicketsPerTickEnv},
		{name: "ticket cap bound", env: map[string]string{maxTicketsPerTickEnv: "0"}, want: maxTicketsPerTickEnv},
		{name: "llm timeout", env: map[string]string{llmTimeoutEnv: "-1s"}, want: llmTimeoutEnv},
		{name: "reply cap", env: map[string]string{maxAssistantRepliesEnv: "-1"}, want: maxAssistantRepliesEnv},
		{name: "lookback", env: map[string]string{lookbackEnv: "0s"}, want: lookbackEnv},
		{name: "missing admin", env: map[string]string{enabledEnv: "true", anthropicAPIKeyEnv: "key"}, want: adminTokenEnv},
		{name: "missing api key", env: map[string]string{enabledEnv: "true", adminTokenEnv: "admin"}, want: anthropicAPIKeyEnv},
		{name: "empty admin file", env: mergeMaps(validSecrets, map[string]string{adminTokenFileEnv: emptyFile}), want: adminTokenFileEnv},
		{name: "missing api key file", env: mergeMaps(validSecrets, map[string]string{anthropicAPIKeyFileEnv: "/not/present"}), want: anthropicAPIKeyFileEnv},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := FromEnv(mapLookup(test.env))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want name %q", err, test.want)
			}
		})
	}
	if _, err := FromEnv(nil); err == nil {
		t.Fatal("nil lookup accepted")
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func mergeMaps(left, right map[string]string) map[string]string {
	result := make(map[string]string, len(left)+len(right))
	for key, value := range left {
		result[key] = value
	}
	for key, value := range right {
		result[key] = value
	}
	return result
}
