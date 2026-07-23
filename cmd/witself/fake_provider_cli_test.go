package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

const (
	fakeProviderLogEnv         = "WITSELF_FAKE_PROVIDER_LOG"
	fakeProviderStateEnv       = "WITSELF_FAKE_PROVIDER_STATE"
	fakeProviderFailNextAddEnv = "WITSELF_FAKE_PROVIDER_FAIL_NEXT_MCP_ADD"
)

type fakeProviderInvocation struct {
	Args []string `json:"args"`
}

type fakeProviderRegistry struct {
	Servers map[string][]string `json:"servers"`
}

type fakeProviderCLI struct {
	Path      string
	LogPath   string
	StatePath string
}

func buildFakeProviderCLI(t *testing.T, root string) fakeProviderCLI {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve fake provider test source")
	}
	executableName := "fake-provider"
	if runtime.GOOS == "windows" {
		executableName += ".exe"
	}
	fixture := fakeProviderCLI{
		Path:      filepath.Join(root, executableName),
		LogPath:   filepath.Join(root, "fake-provider-invocations.jsonl"),
		StatePath: filepath.Join(root, "fake-provider-registry.json"),
	}
	cmd := exec.Command("go", "build", "-o", fixture.Path, ".")
	cmd.Dir = filepath.Join(filepath.Dir(thisFile), "testdata", "fakeprovider")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build host-native fake provider CLI: %v\n%s", err, output)
	}
	t.Setenv(fakeProviderLogEnv, fixture.LogPath)
	t.Setenv(fakeProviderStateEnv, fixture.StatePath)
	return fixture
}

func (fixture fakeProviderCLI) writeRegistry(t *testing.T, servers map[string][]string) {
	t.Helper()
	raw, err := json.Marshal(fakeProviderRegistry{Servers: servers})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.StatePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture fakeProviderCLI) readRegistry(t *testing.T) fakeProviderRegistry {
	t.Helper()
	raw, err := os.ReadFile(fixture.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	var state fakeProviderRegistry
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func (fixture fakeProviderCLI) invocations(t *testing.T) []fakeProviderInvocation {
	t.Helper()
	file, err := os.Open(fixture.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var calls []fakeProviderInvocation
	decoder := json.NewDecoder(file)
	for {
		var call fakeProviderInvocation
		if err := decoder.Decode(&call); errors.Is(err, io.EOF) {
			return calls
		} else if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
}

func (fixture fakeProviderCLI) failNextMCPAdd(t *testing.T) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(fixture.StatePath), "fail-next-mcp-add")
	if err := os.WriteFile(path, []byte("fail once\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeProviderFailNextAddEnv, path)
	return path
}
