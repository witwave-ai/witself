package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
	"github.com/witwave-ai/witself/internal/version"
)

func releaseUploaderTestExecutable(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uploader executable fixture uses a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "witself")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func compatibleReleaseUploaderTestExecutable(t *testing.T) string {
	t.Helper()
	return releaseUploaderTestExecutable(t, `if test "$#" = 1 && test "$1" = --version; then
printf '%s\n' '`+version.String("witself")+`'
exit 0
fi
test "$#" = 3 && test "$1" = transcript && test "$2" = release && test "$3" = --uploader-capability || exit 2
printf '%s\n' '`+transcriptReleaseUploaderCapability+`'`)
}

func installReleaseUploaderTestHooks(t *testing.T, executable string) transcriptcapture.Config {
	t.Helper()
	return installRuntimeReleaseUploaderTestHooks(t, transcriptcapture.RuntimeCodex, executable)
}

func installRuntimeReleaseUploaderTestHooks(t *testing.T, runtimeName, executable string) transcriptcapture.Config {
	t.Helper()
	cfg, err := transcriptcapture.LoadConfig(runtimeName)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MCPCommand = executable
	cfg.RuntimeCLICommand = executable
	cfg.RuntimeConfigRoot = t.TempDir()
	cfg.RuntimeMCPConfigPath = filepath.Join(cfg.RuntimeConfigRoot, "config.toml")
	if cfg.Runtime == transcriptcapture.RuntimeClaudeCode {
		cfg.RuntimeMCPConfigPath = filepath.Join(cfg.RuntimeConfigRoot, ".claude.json")
	}
	cfg.MCPEnvironment = map[string]string{"WITSELF_HOME": os.Getenv("WITSELF_HOME")}
	cfg.HookMode = transcriptcapture.HookModeManaged
	managedRoot := t.TempDir()
	opts := transcriptcapture.ManagedHooksOptions{
		Runtime: cfg.Runtime, Mode: cfg.CaptureMode, Executable: executable,
		Account: cfg.Account, Realm: cfg.Realm, Agent: cfg.Agent, Location: cfg.Location.Name,
		WitselfHome:           os.Getenv("WITSELF_HOME"),
		CodexRequirementsPath: filepath.Join(managedRoot, "requirements.toml"),
		CodexManagedDir:       filepath.Join(managedRoot, "hooks"),
		ClaudeSettingsPath:    filepath.Join(managedRoot, "managed-settings.d", "50-witself.json"),
		ClaudeManagedDir:      filepath.Join(managedRoot, "hooks"),
	}
	ownership, _, err := transcriptcapture.InstallManagedHooksOwned(opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	setManagedHookOwnership(&cfg, ownership)
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestTranscriptReleaseUploaderCapabilityIsReadOnly(t *testing.T) {
	setupTranscriptReleaseCLI(t)
	before := transcriptReleaseFiles(t)
	stdout, stderr, code := captureIntegrationsCLI(t, func() int {
		return transcriptCmd([]string{"release", transcriptReleaseUploaderCapabilityFlag})
	})
	if code != 0 || stderr != "" || stdout != transcriptReleaseUploaderCapability+"\n" {
		t.Fatalf("capability = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if !reflect.DeepEqual(before, transcriptReleaseFiles(t)) {
		t.Fatal("capability probe changed capture state")
	}
}

func TestTranscriptReleaseRejectsOlderInstalledUploaderBeforeMutation(t *testing.T) {
	setupTranscriptReleaseCLI(t)
	// A successful read-only command does not prove the installed version is
	// the binary performing this release.
	executable := releaseUploaderTestExecutable(t, "printf 'witself old-build\\n'")
	installReleaseUploaderTestHooks(t, executable)
	before := transcriptReleaseFiles(t)
	stdout, stderr, code := captureIntegrationsCLI(t, func() int {
		return transcriptRelease([]string{"--runtime", "codex", "--session", "delegated", "--yes", "--force"})
	})
	if code != 1 || !strings.Contains(stderr, "witself install --runtime codex") || !strings.Contains(stderr, "does not match this release") || strings.Contains(stdout, "released 1") {
		t.Fatalf("old uploader release = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if !reflect.DeepEqual(before, transcriptReleaseFiles(t)) {
		t.Fatal("failed compatibility check changed capture state")
	}
}

func TestTranscriptReleaseDryRunDoesNotProbeInstalledUploader(t *testing.T) {
	setupTranscriptReleaseCLI(t)
	marker := filepath.Join(t.TempDir(), "probe-ran")
	quotedMarker := "'" + strings.ReplaceAll(marker, "'", "'\\''") + "'"
	executable := releaseUploaderTestExecutable(t, "touch "+quotedMarker+"\nexit 1")
	installReleaseUploaderTestHooks(t, executable)
	before := transcriptReleaseFiles(t)
	_, stderr, code := captureIntegrationsCLI(t, func() int {
		return transcriptRelease([]string{"--runtime", "codex", "--all", "--yes", "--dry-run"})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("dry-run = %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("dry-run executed uploader: %v", err)
	}
	if !reflect.DeepEqual(before, transcriptReleaseFiles(t)) {
		t.Fatal("dry-run changed capture state")
	}
}

func TestTranscriptReleaseAcceptsCompatibleInstalledUploader(t *testing.T) {
	setupTranscriptReleaseCLI(t)
	installReleaseUploaderTestHooks(t, compatibleReleaseUploaderTestExecutable(t))
	stdout, stderr, code := captureIntegrationsCLI(t, func() int {
		return transcriptRelease([]string{"--runtime", "codex", "--session", "delegated", "--yes", "--force"})
	})
	if code != 0 || !strings.Contains(stdout, "released 1 turn(s)") {
		t.Fatalf("compatible uploader release = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	assertTranscriptReleaseCLIReady(t, "delegated")
}

func TestTranscriptReleaseChecksOtherInstalledRuntimeUploaders(t *testing.T) {
	setupTranscriptReleaseCLI(t)
	cfg := installReleaseUploaderTestHooks(t, compatibleReleaseUploaderTestExecutable(t))
	cfg.Runtime = transcriptcapture.RuntimeCopilot
	cfg.RuntimeMCPConfigPath = filepath.Join(cfg.RuntimeConfigRoot, "mcp-config.json")
	cfg.HookMode = transcriptcapture.HookModeNone
	clearHookOwnership(&cfg)
	cfg.MCPCommand = releaseUploaderTestExecutable(t, "exit 2")
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	before := transcriptReleaseFiles(t)
	_, stderr, code := captureIntegrationsCLI(t, func() int {
		return transcriptRelease([]string{"--runtime", "codex", "--all", "--yes", "--force"})
	})
	if code != 1 || !strings.Contains(stderr, "copilot installed uploader") {
		t.Fatalf("other runtime uploader release = %d, stderr %q", code, stderr)
	}
	if !reflect.DeepEqual(before, transcriptReleaseFiles(t)) {
		t.Fatal("incompatible shared-home uploader allowed release mutation")
	}
}

func TestTranscriptReleaseRejectsHookExecutableDifferentFromRegistration(t *testing.T) {
	setupTranscriptReleaseCLI(t)
	compatible := compatibleReleaseUploaderTestExecutable(t)
	cfg := installReleaseUploaderTestHooks(t, compatible)
	old := releaseUploaderTestExecutable(t, "exit 2")
	raw, err := os.ReadFile(cfg.HookRunnerPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a hook pinned to an older executable despite the refreshed MCP
	// registration; probing only MCPCommand would incorrectly authorize this.
	raw = []byte(strings.ReplaceAll(string(raw), compatible, old))
	if err := os.WriteFile(cfg.HookRunnerPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before := transcriptReleaseFiles(t)
	_, stderr, code := captureIntegrationsCLI(t, func() int {
		return transcriptRelease([]string{"--runtime", "codex", "--all", "--yes", "--force"})
	})
	if code != 1 || !strings.Contains(stderr, "witself install --runtime codex") {
		t.Fatalf("mixed hook release = %d, stderr %q", code, stderr)
	}
	if !reflect.DeepEqual(before, transcriptReleaseFiles(t)) {
		t.Fatal("mixed hook executable allowed release mutation")
	}
}

func TestTranscriptReleaseUploaderProbeIsBoundedAndIsolated(t *testing.T) {
	t.Run("isolated home", func(t *testing.T) {
		liveHome := t.TempDir()
		t.Setenv("WITSELF_HOME", liveHome)
		t.Setenv("HOME", liveHome)
		t.Setenv("USERPROFILE", liveHome)
		t.Setenv("XDG_CONFIG_HOME", liveHome)
		executable := releaseUploaderTestExecutable(t, `touch "$WITSELF_HOME/probe-maintenance"
touch "$HOME/probe-user-home" "$USERPROFILE/probe-profile" "$XDG_CONFIG_HOME/probe-service-config"
if test "$#" = 1 && test "$1" = --version; then
printf '%s\n' '`+version.String("witself")+`'
exit 0
fi
printf '%s\n' '`+transcriptReleaseUploaderCapability+`'`)
		if err := probeTranscriptReleaseUploader(context.Background(), executable); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(liveHome)
		if err != nil || len(entries) != 0 {
			t.Fatalf("probe touched live home: %v, %v", entries, err)
		}
	})
	t.Run("output limit", func(t *testing.T) {
		executable := releaseUploaderTestExecutable(t, "printf '%0300d' 1")
		if err := probeTranscriptReleaseUploader(context.Background(), executable); err == nil || !strings.Contains(err.Error(), "256 bytes") {
			t.Fatalf("oversized probe response = %v", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		executable := releaseUploaderTestExecutable(t, "exec sleep 30")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		started := time.Now()
		if err := probeTranscriptReleaseUploader(ctx, executable); err == nil {
			t.Fatal("probe ignored its deadline")
		}
		if time.Since(started) > 3*time.Second {
			t.Fatal("probe did not terminate promptly after its deadline")
		}
	})
}
