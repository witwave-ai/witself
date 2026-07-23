//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHookWitselfHomeAppliesBeforeStartupCleanupSubprocess(t *testing.T) {
	defaultUserHome := t.TempDir()
	selectedHome := filepath.Join(t.TempDir(), "selected-witself")
	if canonical, err := filepath.EvalSymlinks(filepath.Dir(selectedHome)); err == nil {
		selectedHome = filepath.Join(canonical, filepath.Base(selectedHome))
	}
	defaultWitselfHome := filepath.Join(defaultUserHome, ".witself")
	defaultState := writeLegacyRunnerStateFixture(t, defaultWitselfHome)
	selectedState := writeLegacyRunnerStateFixture(t, selectedHome)

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve package directory")
	}
	binary := filepath.Join(t.TempDir(), "witself-startup-home-test")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = filepath.Dir(thisFile)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Witself subprocess: %v\n%s", err, output)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary,
		"transcript", "hook",
		"--runtime", "codex",
		"--account", "default",
		"--realm", "default",
		"--agent", "startup-home-test",
		"--witself-home", selectedHome,
	)
	command.Stdin = strings.NewReader("{}")
	command.Env = append(environmentWithoutKeys(os.Environ(), "WITSELF_HOME", "HOME", "USERPROFILE", "XDG_CONFIG_HOME"),
		"HOME="+defaultUserHome,
		"USERPROFILE="+defaultUserHome,
		"XDG_CONFIG_HOME="+filepath.Join(defaultUserHome, ".config"),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("hook subprocess failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(selectedState); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selected WITSELF_HOME legacy state was not cleaned: %v", err)
	}
	if _, err := os.Stat(defaultState); err != nil {
		t.Fatalf("startup cleanup touched the default home before applying the hook binding: %v", err)
	}
}

func writeLegacyRunnerStateFixture(t *testing.T, witselfHome string) string {
	t.Helper()
	stateRoot := filepath.Join(witselfHome, "message-runners", "codex")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateRoot, "state.json")
	if err := os.WriteFile(path, []byte(`{"schema":"witself.message-runner-state.v1","notifications":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func environmentWithoutKeys(environment []string, keys ...string) []string {
	result := make([]string, 0, len(environment))
	prefixes := make([]string, len(keys))
	for index, key := range keys {
		prefixes[index] = key + "="
	}
	for _, entry := range environment {
		excluded := false
		for _, prefix := range prefixes {
			if len(entry) >= len(prefix) && entry[:len(prefix)] == prefix {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		result = append(result, entry)
	}
	return result
}
