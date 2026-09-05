package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type releaseUploaderRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn releaseUploaderRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func assertTranscriptReleaseUploaderRefusesUnchanged(t *testing.T, reason string) {
	t.Helper()
	before := transcriptReleaseFiles(t)
	foundCanary := false
	for _, raw := range before {
		foundCanary = foundCanary || strings.Contains(raw, "release-cli-result-canary")
	}
	if !foundCanary {
		t.Fatal("fixture lacks the plaintext release-cli-result-canary")
	}
	requests := 0
	previousTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	http.DefaultTransport = releaseUploaderRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("release-uploader-http-canary: no network is permitted")
	})
	var typed *transcriptReleaseUploaderVerificationError
	err := verifyTranscriptReleaseUploaders()
	if !errors.As(err, &typed) || typed.Runtime != transcriptcapture.RuntimeCodex || !strings.Contains(err.Error(), reason) {
		t.Errorf("uploader verification = %v, want typed codex refusal containing %q", err, reason)
	}
	for _, force := range []bool{false, true} {
		args := []string{"--runtime", "codex", "--session", "delegated", "--older-than", "0", "--yes"}
		if force {
			args = append(args, "--force")
		}
		stdout, stderr, code := captureIntegrationsCLI(t, func() int { return transcriptRelease(args) })
		if code != 1 || !strings.Contains(stderr, "witself install --runtime codex") || !strings.Contains(stderr, reason) {
			t.Errorf("release force=%t = %d, stdout %q, stderr %q", force, code, stdout, stderr)
		}
		if strings.Contains(stdout+stderr, "release-cli-result-canary") {
			t.Error("refusal exposed the plaintext canary")
		}
		if !reflect.DeepEqual(before, transcriptReleaseFiles(t)) {
			t.Errorf("release force=%t rewrote state containing the plaintext canary", force)
		}
	}
	if requests != 0 {
		t.Errorf("release issued %d HTTP requests", requests)
	}
}

func TestTranscriptReleaseLegacy2b04c0dRegistrationRefusesWithoutMutation(t *testing.T) {
	for _, hookMode := range []string{transcriptcapture.HookModeUser, transcriptcapture.HookModeManaged} {
		t.Run(hookMode, func(t *testing.T) {
			setupTranscriptReleaseCLI(t)
			cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			// Exact persisted field set and ordering from Config/SaveConfig and
			// install in 2b04c0d for Codex: that installer did not persist the
			// executable, hook config path, or hook runner path. Do not marshal
			// today's Config: it must remain possible to detect this old schema.
			legacy := fmt.Sprintf(`{
  "schema_version": "witself.capture.v1",
  "runtime": "codex",
  "runtime_version": "0.106.0",
  "capture_mode": "raw",
  "hook_mode": %q,
  "account": "default",
  "realm": "default",
  "agent": "scott",
  "agent_id": "agent_1",
  "agent_name": "scott",
  "location": {
    "id": %q,
    "name": "home"
  },
  "installed_at": "2026-09-01T00:00:00Z"
}
`, hookMode, cfg.Location.ID)
			path, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
				t.Fatal(err)
			}
			parsed, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
			if err != nil || parsed.MCPCommand != "" || parsed.HookConfigPath != "" || parsed.HookRunnerPath != "" {
				t.Fatalf("legacy fixture = %#v, %v", parsed, err)
			}
			assertTranscriptReleaseUploaderRefusesUnchanged(t, "installed hook executable is missing")
		})
	}
}

func TestTranscriptReleaseUserRegistrationMissingRunnerRefusesWithoutMutation(t *testing.T) {
	setupTranscriptReleaseCLI(t)
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	// This is a valid current user-hook registration, with a canary in the
	// queued result. The explicit release policy still requires a runner.
	clearHookOwnership(&cfg)
	cfg.HookMode = transcriptcapture.HookModeUser
	cfg.HookConfigPath = filepath.Join(t.TempDir(), "hooks.json")
	opts, err := userHooksOptionsFromConfig(cfg, cfg.MCPCommand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcriptcapture.InstallOwnedHooks(opts, nil); err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	assertTranscriptReleaseUploaderRefusesUnchanged(t, "hook_runner_path are required")
}

func TestTranscriptReleaseCapabilityWithDifferentVersionRefusesWithoutMutation(t *testing.T) {
	setupTranscriptReleaseCLI(t)
	// The binary advertises the release capability, but its --version is from
	// an older build. The queued release-cli-result-canary must remain held.
	executable := releaseUploaderTestExecutable(t, `if test "$1" = --version; then
printf '%s\n' 'witself old-release-canary (commit old, built old)'
else
printf '%s\n' '`+transcriptReleaseUploaderCapability+`'
fi`)
	installReleaseUploaderTestHooks(t, executable)
	assertTranscriptReleaseUploaderRefusesUnchanged(t, "uploader --version does not match this release")
}
