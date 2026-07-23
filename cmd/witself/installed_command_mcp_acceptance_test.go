package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderIntegrationContractCodexInstalledCommandMCPStdio(t *testing.T) {
	root := t.TempDir()
	witselfExecutable := buildInstalledCommandTestWitself(t, root)
	provider := buildFakeProviderCLI(t, root)
	provider.writeRegistry(t, map[string][]string{
		"sibling": {"sibling-mcp", "serve"},
	})

	home := filepath.Join(root, "home")
	witselfHome := filepath.Join(home, ".witself")
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	// os.UserHomeDir uses USERPROFILE on Windows. The explicit Witself and
	// Codex roots below remain authoritative on every platform, while setting
	// both home variables keeps any vendor-side fallback equally isolated.
	t.Setenv("USERPROFILE", home)
	t.Setenv("WITSELF_HOME", witselfHome)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_CLI_PATH", provider.Path)
	t.Setenv(witselfExecutableTestEnv, "")

	const tokenValue = "synthetic-installed-command-token"
	var selfRequests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer "+tokenValue {
			http.NotFound(w, r)
			return
		}
		selfRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"schema_version":"witself.v0","identity":{"account_id":"acc_acceptance","realm_id":"realm_acceptance","realm_name":"default","agent_id":"agent_acceptance","agent_name":"codex-test-bot"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`)
	}))
	t.Cleanup(backend.Close)
	tokenPath := filepath.Join(root, "agent.token")
	if err := os.WriteFile(tokenPath, []byte(tokenValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	install := exec.Command(
		witselfExecutable,
		"install", "codex",
		"--account", "default",
		"--realm", "default",
		"--agent", "codex-test-bot",
		"--location", "acceptance",
		"--capture", "raw",
		"--endpoint", backend.URL,
		"--token-file", tokenPath,
		"--user-hooks",
	)
	install.Env = os.Environ()
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("run installed Witself integration: %v\n%s", err, output)
	}
	requestsAfterInstall := selfRequests.Load()
	if requestsAfterInstall == 0 {
		t.Fatal("real install path did not verify the synthetic backend identity")
	}

	state := provider.readRegistry(t)
	registered := state.Servers["witself"]
	if len(registered) < 2 {
		t.Fatalf("Codex provider registration = %q", registered)
	}
	if filepath.Clean(registered[0]) != filepath.Clean(witselfExecutable) {
		t.Fatalf("registered executable = %q, want built CLI %q", registered[0], witselfExecutable)
	}
	wantArgs := []string{
		"mcp", "serve", "--runtime", "codex",
		"--account", "default", "--realm", "default",
		"--agent", "codex-test-bot", "--location", "acceptance",
	}
	if !slices.Equal(registered[1:], wantArgs) {
		t.Fatalf("registered MCP args = %q, want %q", registered[1:], wantArgs)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, registered[0], registered[1:]...)
	command.Env = os.Environ()
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr installedCommandSafeBuffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("launch exact registered MCP command: %v", err)
	}
	reader := bufio.NewReader(stdout)
	writeInstalledCommandMCPMessage(t, stdin, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name": "witself-installed-command-acceptance", "version": "1",
			},
		},
	})
	initialize := readInstalledCommandMCPResponse(t, reader, 1, &stderr)
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(initialize.Result, &initialized); err != nil {
		t.Fatal(err)
	}
	if initialized.ProtocolVersion == "" || initialized.ServerInfo.Name != "witself" {
		t.Fatalf("initialize result = %s", initialize.Result)
	}
	writeInstalledCommandMCPMessage(t, stdin, map[string]any{
		"jsonrpc": "2.0", "method": "notifications/initialized", "params": map[string]any{},
	})
	writeInstalledCommandMCPMessage(t, stdin, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{},
	})
	list := readInstalledCommandMCPResponse(t, reader, 2, &stderr)
	var tools struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(list.Result, &tools); err != nil {
		t.Fatal(err)
	}
	foundSelf := false
	for _, tool := range tools.Tools {
		if tool.Name == "witself.self.show" {
			foundSelf = true
			break
		}
	}
	if !foundSelf {
		t.Fatalf("installed MCP server advertised %d tools without witself.self.show", len(tools.Tools))
	}
	writeInstalledCommandMCPMessage(t, stdin, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "witself.self.show", "arguments": map[string]any{},
		},
	})
	selfCall := readInstalledCommandMCPResponse(t, reader, 3, &stderr)
	var selfResult struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(selfCall.Result, &selfResult); err != nil {
		t.Fatal(err)
	}
	if selfResult.IsError {
		t.Fatalf("witself.self.show returned a tool error: %s", selfCall.Result)
	}
	var self struct {
		Identity struct {
			AgentName string `json:"agent_name"`
		} `json:"identity"`
	}
	if err := json.Unmarshal(selfResult.StructuredContent, &self); err != nil {
		t.Fatal(err)
	}
	if self.Identity.AgentName != "codex-test-bot" {
		t.Fatalf("witself.self.show identity = %s", selfResult.StructuredContent)
	}
	if selfRequests.Load() <= requestsAfterInstall {
		t.Fatal("registered MCP command did not reach the configured backend")
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("registered MCP command shutdown: %v\nstderr:\n%s", err, stderr.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("registered MCP command timed out: %v\nstderr:\n%s", ctx.Err(), stderr.String())
	}
	if strings.Contains(stderr.String(), tokenValue) {
		t.Fatal("registered MCP process wrote the synthetic token to stderr")
	}
}

func buildInstalledCommandTestWitself(t *testing.T, root string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve cmd/witself source directory")
	}
	name := "witself"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(root, name)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-o", path, ".")
	command.Dir = filepath.Dir(thisFile)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build installed-command Witself binary: %v\n%s", err, output)
	}
	return path
}

func writeInstalledCommandMCPMessage(t *testing.T, writer io.Writer, message any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(message); err != nil {
		t.Fatal(err)
	}
}

type installedCommandMCPResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func readInstalledCommandMCPResponse(
	t *testing.T,
	reader *bufio.Reader,
	wantID int,
	stderr *installedCommandSafeBuffer,
) installedCommandMCPResponse {
	t.Helper()
	type readResult struct {
		line []byte
		err  error
	}
	for {
		result := make(chan readResult, 1)
		go func() {
			line, err := reader.ReadBytes('\n')
			result <- readResult{line: line, err: err}
		}()
		select {
		case read := <-result:
			if read.err != nil {
				t.Fatalf("read MCP response %d: %v\nstderr:\n%s", wantID, read.err, stderr.String())
			}
			var response installedCommandMCPResponse
			if err := json.Unmarshal(read.line, &response); err != nil {
				t.Fatalf("decode MCP response %q: %v", read.line, err)
			}
			if strings.TrimSpace(string(response.ID)) != fmt.Sprint(wantID) {
				continue
			}
			if response.Error != nil {
				t.Fatalf("MCP response %d error %d: %s", wantID, response.Error.Code, response.Error.Message)
			}
			if len(response.Result) == 0 || bytes.Equal(bytes.TrimSpace(response.Result), []byte("null")) {
				t.Fatalf("MCP response %d has no result", wantID)
			}
			return response
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out reading MCP response %d\nstderr:\n%s", wantID, stderr.String())
		}
	}
}

type installedCommandSafeBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *installedCommandSafeBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(value)
}

func (buffer *installedCommandSafeBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}
