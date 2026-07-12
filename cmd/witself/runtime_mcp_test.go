package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCursorMCPRegistrationPreservesUnrelatedServers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CURSOR_CONFIG_DIR", filepath.Join(home, ".cursor"))
	path, err := cursorMCPPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"existing":{"command":"existing-mcp"}},"other":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	command := []string{
		"/usr/local/bin/witself", "mcp", "serve", "--runtime", "cursor",
		"--account", "default", "--realm", "default", "--agent", "scott", "--location", "home",
	}
	if err := registerCursorMCP(command); err != nil {
		t.Fatal(err)
	}
	root := readTestJSONObject(t, path)
	servers := root["mcpServers"].(map[string]any)
	if _, ok := servers["existing"]; !ok {
		t.Fatal("existing MCP server was removed")
	}
	witself := servers["witself"].(map[string]any)
	if witself["command"] != command[0] {
		t.Fatalf("command = %#v", witself["command"])
	}
	if err := unregisterCursorMCP(); err != nil {
		t.Fatal(err)
	}
	root = readTestJSONObject(t, path)
	servers = root["mcpServers"].(map[string]any)
	if _, ok := servers["witself"]; ok {
		t.Fatal("Witself MCP server remains after uninstall")
	}
	if _, ok := servers["existing"]; !ok || root["other"] != true {
		t.Fatalf("unrelated Cursor config changed: %#v", root)
	}
}

func readTestJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	root := map[string]any{}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	return root
}
