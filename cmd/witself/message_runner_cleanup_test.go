package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/legacyrunnercleanup"
)

func TestMessageRunnerRemovedSubcommandsAreUnavailable(t *testing.T) {
	for _, subcommand := range []string{"enable", "status", "notifications", "run", "serve", "start"} {
		t.Run(subcommand, func(t *testing.T) {
			if code := messageRunnerCmd([]string{subcommand}); code != 2 {
				t.Fatalf("message runner %s exit code = %d, want 2", subcommand, code)
			}
		})
	}
}

func TestMessageRunnerCleanupRequiresExactlyOneSelector(t *testing.T) {
	for _, args := range [][]string{
		{"disable"},
		{"disable", "--all", "--runtime", "codex"},
		{"disable", "--runtime", "codex", "extra"},
	} {
		if code := messageRunnerCmd(args); code != 2 {
			t.Fatalf("message runner %v exit code = %d, want 2", args, code)
		}
	}
}

func TestMessageRunnerCleanupAllPurgesEveryFormerRuntime(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	witselfHome := filepath.Join(root, "witself")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("WITSELF_HOME", witselfHome)
	installTestLegacyCleaner(t, legacyrunnercleanup.Cleaner{
		Platform: "linux", UserHome: home, ConfigHome: filepath.Join(root, "config"),
		WitselfHome: witselfHome, UID: 501,
		Run: func(_ context.Context, _ string, args ...string) error {
			if len(args) >= 2 && args[1] == "is-active" {
				return legacyrunnercleanup.ErrServiceNotLoaded
			}
			return nil
		},
		RemoveAll: os.RemoveAll,
	})

	for _, runtimeName := range legacyrunnercleanup.Runtimes() {
		statePath := filepath.Join(witselfHome, "message-runners", runtimeName)
		if err := os.MkdirAll(statePath, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"config.json", "provider-credentials.json", "state.json"} {
			body := []byte("private")
			if name == "state.json" {
				body = []byte(`{"schema":"witself.message-runner-state.v1","notifications":[]}`)
			}
			if err := os.WriteFile(filepath.Join(statePath, name), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	if code := messageRunnerCmd([]string{"disable", "--all", "--json"}); code != 0 {
		t.Fatalf("message runner disable --all exit code = %d", code)
	}
	for _, runtimeName := range legacyrunnercleanup.Runtimes() {
		statePath := filepath.Join(witselfHome, "message-runners", runtimeName)
		if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy state %s still exists: %v", statePath, err)
		}
	}
}

func TestLegacyRunnerStartupMigrationRunsOnce(t *testing.T) {
	root := t.TempDir()
	cleaner := legacyrunnercleanup.Cleaner{
		Platform: "linux", UserHome: filepath.Join(root, "home"),
		ConfigHome: filepath.Join(root, "config"), WitselfHome: filepath.Join(root, "witself"),
		UID: 501,
		Run: func(ctx context.Context, _ string, args ...string) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) <= 0 {
				t.Fatalf("startup cleanup deadline = %v / %t", deadline, ok)
			}
			if len(args) >= 2 && args[1] == "is-active" {
				return legacyrunnercleanup.ErrServiceNotLoaded
			}
			return nil
		},
		RemoveAll: os.RemoveAll,
	}
	statePath := filepath.Join(cleaner.WitselfHome, "message-runners", "cursor", "state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(`{"schema":"witself.message-runner-state.v1","notifications":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	installTestLegacyCleaner(t, cleaner)

	for range 2 {
		if err := retireLegacyMessageRunnersOnStartup(); err != nil {
			t.Fatal(err)
		}
	}
	if complete, err := cleaner.Completed(); err != nil || !complete {
		t.Fatalf("startup cleanup completed = %t / %v", complete, err)
	}
}

func TestLegacyRunnerStartupWithoutArtifactsSkipsServiceManager(t *testing.T) {
	root := t.TempDir()
	cleaner := legacyrunnercleanup.Cleaner{
		Platform: "linux", UserHome: filepath.Join(root, "home"),
		ConfigHome: filepath.Join(root, "config"), WitselfHome: filepath.Join(root, "witself"),
		UID: 501,
		Run: func(context.Context, string, ...string) error {
			t.Fatal("artifact-free startup contacted service manager")
			return nil
		},
		RemoveAll: os.RemoveAll,
	}
	installTestLegacyCleaner(t, cleaner)
	if err := retireLegacyMessageRunnersOnStartup(); err != nil {
		t.Fatal(err)
	}
	if complete, err := cleaner.Completed(); err != nil || complete {
		t.Fatalf("artifact-free startup completion = %t / %v", complete, err)
	}
}

func TestMessageRunnerSingleRuntimeCleanupDoesNotMarkGlobalCompletion(t *testing.T) {
	root := t.TempDir()
	cleaner := legacyrunnercleanup.Cleaner{
		Platform: "linux", UserHome: filepath.Join(root, "home"),
		ConfigHome: filepath.Join(root, "config"), WitselfHome: filepath.Join(root, "witself"),
		UID: 501,
		Run: func(_ context.Context, _ string, args ...string) error {
			if len(args) >= 2 && args[1] == "is-active" {
				return legacyrunnercleanup.ErrServiceNotLoaded
			}
			return nil
		},
		RemoveAll: os.RemoveAll,
	}
	installTestLegacyCleaner(t, cleaner)
	if code := messageRunnerCmd([]string{"disable", "--runtime", "codex"}); code != 0 {
		t.Fatalf("single runtime cleanup exit code = %d", code)
	}
	if complete, err := cleaner.Completed(); err != nil || complete {
		t.Fatalf("single runtime cleanup completion = %t / %v", complete, err)
	}
}

func TestLegacyRunnerServeTombstoneRecognizesOnlyInstalledShape(t *testing.T) {
	runtimeName, ok := legacyRunnerServeRuntime([]string{"message", "runner", "serve", "--runtime", "claude"})
	if !ok || runtimeName != "claude-code" {
		t.Fatalf("serve tombstone = %q / %t", runtimeName, ok)
	}
	for _, args := range [][]string{
		{"message", "runner", "serve"},
		{"message", "runner", "serve", "--runtime", "unknown"},
		{"message", "runner", "serve", "--runtime", "codex", "extra"},
		{"message", "runner", "disable", "--runtime", "codex"},
	} {
		if got, matched := legacyRunnerServeRuntime(args); matched {
			t.Fatalf("unexpected tombstone match %v = %q", args, got)
		}
	}
}

func TestLegacyRunnerServeTombstoneAllowsMissingDefinition(t *testing.T) {
	root := t.TempDir()
	var stopped bool
	cleaner := legacyrunnercleanup.Cleaner{
		Platform: "linux", UserHome: filepath.Join(root, "home"),
		ConfigHome: filepath.Join(root, "config"), WitselfHome: filepath.Join(root, "witself"),
		UID: 501,
		Run: func(_ context.Context, _ string, args ...string) error {
			if len(args) >= 2 && args[1] == "stop" {
				stopped = true
			}
			return nil
		},
		RemoveAll: os.RemoveAll,
	}
	installTestLegacyCleaner(t, cleaner)
	if err := retireLegacyMessageRunnerServeTombstone("cursor"); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("serve tombstone did not stop loaded unit")
	}
}

func installTestLegacyCleaner(t *testing.T, cleaner legacyrunnercleanup.Cleaner) {
	t.Helper()
	previous := newLegacyRunnerCleaner
	newLegacyRunnerCleaner = func() (legacyrunnercleanup.Cleaner, error) { return cleaner, nil }
	t.Cleanup(func() { newLegacyRunnerCleaner = previous })
}
