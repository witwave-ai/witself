package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestProviderCLICommandsUseIsolatedWorkingDirectories(t *testing.T) {
	callerDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf(".witself-provider-side-effect-%d", time.Now().UnixNano())
	t.Setenv(fakeProviderCWDArtifactEnv, marker)
	t.Cleanup(func() { _ = os.Remove(filepath.Join(callerDirectory, marker)) })

	for _, runtimeName := range genericProviderTestRuntimes {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := newGenericProviderTestFixture(t, runtimeName)
			if _, err := runGenericProviderCLI(fixture.cli, fixture.cfg, time.Second, "--version"); err != nil {
				t.Fatalf("run generic provider CLI: %v", err)
			}
			if _, err := runLegacyProviderCLI(fixture.cli, time.Second, "--version"); err != nil {
				t.Fatalf("run legacy provider CLI: %v", err)
			}
			if version := detectRuntimeVersion(runtimeName, fixture.cli); version != "1.0.0" {
				t.Fatalf("detected version = %q, want 1.0.0", version)
			}
			if selected, err := findRuntimeCLI(runtimeName); err != nil || selected != fixture.cli {
				t.Fatalf("selected CLI = %q, %v; want %q", selected, err, fixture.cli)
			}
			if runtimeName == transcriptcapture.RuntimeGrokBuild {
				if _, err := runGrokJSONCLI(fixture.cli, time.Second, "inspect", "--json"); err != nil {
					t.Fatalf("run Grok JSON inspection: %v", err)
				}
			}
			invocations := fixture.invocations(t)
			directories := make([]string, 0, len(invocations))
			for _, invocation := range invocations {
				directories = append(directories, invocation.WorkingDirectory)
			}
			assertIsolatedProviderCLIWorkingDirectories(t, callerDirectory, marker, directories)
		})
	}

	t.Run("openclaw", func(t *testing.T) {
		provider := buildFakeProviderCLI(t, t.TempDir())
		provider.useKind(t, "openclaw")
		t.Setenv("OPENCLAW_CLI_PATH", provider.Path)
		t.Setenv(fakeProviderWorkspaceEnv, t.TempDir())
		if _, err := openClawCLIJSONWithEnvironment(provider.Path, nil, "agents", "list", "--json"); err != nil {
			t.Fatalf("run OpenClaw inspection: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := runOpenClawMutationCommand(ctx, provider.Path, nil, "--version"); err != nil {
			t.Fatalf("run OpenClaw mutation helper: %v", err)
		}
		if version := detectRuntimeVersion(transcriptcapture.RuntimeOpenClaw, provider.Path); version != "2026.7.1-2" {
			t.Fatalf("detected version = %q, want 2026.7.1-2", version)
		}
		if selected, err := findRuntimeCLI(transcriptcapture.RuntimeOpenClaw); err != nil || selected != provider.Path {
			t.Fatalf("selected CLI = %q, %v; want %q", selected, err, provider.Path)
		}
		assertFakeProviderCLIWorkingDirectories(t, provider, callerDirectory, marker)
	})

	t.Run("copilot", func(t *testing.T) {
		provider := buildFakeProviderCLI(t, t.TempDir())
		provider.useKind(t, "copilot")
		configRoot := filepath.Join(t.TempDir(), "copilot")
		t.Setenv("COPILOT_HOME", configRoot)
		t.Setenv("COPILOT_CLI_PATH", provider.Path)
		if _, err := runCopilotCLICommand(provider.Path, configRoot, time.Second, "--version"); err != nil {
			t.Fatalf("run Copilot CLI: %v", err)
		}
		if version := detectRuntimeVersion(transcriptcapture.RuntimeCopilot, provider.Path); version != "1.0.73" {
			t.Fatalf("detected version = %q, want 1.0.73", version)
		}
		if selected, err := findRuntimeCLI(transcriptcapture.RuntimeCopilot); err != nil || selected != provider.Path {
			t.Fatalf("selected CLI = %q, %v; want %q", selected, err, provider.Path)
		}
		assertFakeProviderCLIWorkingDirectories(t, provider, callerDirectory, marker)
	})

	t.Run("antigravity", func(t *testing.T) {
		provider := buildFakeProviderCLI(t, t.TempDir())
		provider.useKind(t, "antigravity")
		t.Setenv("ANTIGRAVITY_CLI_PATH", provider.Path)
		bundle := antigravityPluginBundle{files: map[string][]byte{
			"plugin.json":      []byte("{\"name\":\"witself-test\"}\n"),
			"rules/witself.md": []byte("Use Witself.\n"),
		}}
		if err := validateAntigravityPluginWithCLI(provider.Path, bundle); err != nil {
			t.Fatalf("validate Antigravity plugin: %v", err)
		}
		if version := detectRuntimeVersion(transcriptcapture.RuntimeAntigravity, provider.Path); version != "1.1.5" {
			t.Fatalf("detected version = %q, want 1.1.5", version)
		}
		if selected, err := findRuntimeCLI(transcriptcapture.RuntimeAntigravity); err != nil || selected != provider.Path {
			t.Fatalf("selected CLI = %q, %v; want %q", selected, err, provider.Path)
		}
		assertFakeProviderCLIWorkingDirectories(t, provider, callerDirectory, marker)
	})
}

func assertFakeProviderCLIWorkingDirectories(
	t *testing.T,
	provider fakeProviderCLI,
	callerDirectory string,
	marker string,
) {
	t.Helper()
	invocations := provider.invocations(t)
	directories := make([]string, 0, len(invocations))
	for _, invocation := range invocations {
		directories = append(directories, invocation.WorkingDirectory)
	}
	assertIsolatedProviderCLIWorkingDirectories(t, callerDirectory, marker, directories)
}

func assertIsolatedProviderCLIWorkingDirectories(
	t *testing.T,
	callerDirectory string,
	marker string,
	directories []string,
) {
	t.Helper()
	if len(directories) == 0 {
		t.Fatal("provider CLI recorded no invocations")
	}
	for _, directory := range directories {
		if directory == "" {
			t.Fatal("provider CLI recorded an empty working directory")
		}
		if filepath.Clean(directory) == filepath.Clean(callerDirectory) {
			t.Fatalf("provider CLI inherited caller working directory %q", directory)
		}
		if !strings.HasPrefix(filepath.Base(directory), "witself-provider-cli-") {
			t.Fatalf("provider CLI working directory %q is not a Witself-isolated directory", directory)
		}
		if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("isolated provider CLI directory %q was not removed: %v", directory, err)
		}
	}
	if _, err := os.Stat(filepath.Join(callerDirectory, marker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provider side effect escaped into caller directory: %v", err)
	}
}
