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

	"github.com/pelletier/go-toml/v2"
)

var providerCLIPathOverrideEnvironment = []string{
	"CODEX_CLI_PATH",
	"CLAUDE_CLI_PATH",
	"GROK_CLI_PATH",
	"CURSOR_CLI_PATH",
	"OPENCLAW_CLI_PATH",
	"ANTIGRAVITY_CLI_PATH",
	"COPILOT_CLI_PATH",
}

func clearProviderCLIPathOverridesForTest(t *testing.T) {
	t.Helper()
	for _, key := range providerCLIPathOverrideEnvironment {
		t.Setenv(key, "")
	}
}

// isolateProviderDiscoveryPATHForTest is called only after every fake CLI and
// installed Witself artifact needed by the test has been built. Provider
// discovery must not fall through to a developer's real clients while a
// portable contract inventories runtimes other than its explicit target.
func isolateProviderDiscoveryPATHForTest(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

const (
	fakeProviderLogEnv         = "WITSELF_FAKE_PROVIDER_LOG"
	fakeProviderStateEnv       = "WITSELF_FAKE_PROVIDER_STATE"
	fakeProviderFailNextAddEnv = "WITSELF_FAKE_PROVIDER_FAIL_NEXT_MCP_ADD"
	fakeProviderKindEnv        = "WITSELF_FAKE_PROVIDER_KIND"
	fakeProviderWorkspaceEnv   = "WITSELF_FAKE_PROVIDER_WORKSPACE"
)

type fakeProviderInvocation struct {
	Args             []string `json:"args"`
	WorkingDirectory string   `json:"working_directory"`
}

type fakeProviderRegistry struct {
	Servers map[string][]string `json:"servers"`
}

type fakeProviderCLI struct {
	Path      string
	LogPath   string
	StatePath string
}

func copyGenericProviderFixture(t *testing.T, destination string) string {
	t.Helper()
	raw, err := os.ReadFile(genericProviderFixtureExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	return destination
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
	fixture.writeCodexConfig(t, servers)
}

func (fixture fakeProviderCLI) readRegistry(t *testing.T) fakeProviderRegistry {
	t.Helper()
	if root := os.Getenv("CODEX_HOME"); root != "" {
		path := filepath.Join(root, "config.toml")
		if raw, err := os.ReadFile(path); err == nil {
			var document struct {
				Servers map[string]struct {
					Command string   `toml:"command"`
					Args    []string `toml:"args"`
				} `toml:"mcp_servers"`
			}
			if err := toml.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			result := fakeProviderRegistry{Servers: map[string][]string{}}
			for name, server := range document.Servers {
				result.Servers[name] = append([]string{server.Command}, server.Args...)
			}
			return result
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
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

func (fixture fakeProviderCLI) writeCodexConfig(t *testing.T, servers map[string][]string) {
	t.Helper()
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		return
	}
	document := map[string]any{"mcp_servers": map[string]any{}}
	target := document["mcp_servers"].(map[string]any)
	for name, command := range servers {
		if len(command) == 0 {
			continue
		}
		target[name] = map[string]any{"command": command[0], "args": command[1:]}
	}
	raw, err := toml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
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

func (fixture fakeProviderCLI) useKind(t *testing.T, kind string) {
	t.Helper()
	t.Setenv(fakeProviderKindEnv, kind)
}

func (fixture fakeProviderCLI) writeRawState(t *testing.T, state map[string]any) {
	t.Helper()
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.StatePath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture fakeProviderCLI) readRawState(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(fixture.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}
