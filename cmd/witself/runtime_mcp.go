package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func registerCursorMCP(command []string) error {
	if len(command) == 0 || !filepath.IsAbs(command[0]) {
		return errors.New("cursor MCP command must use an absolute executable path")
	}
	path, err := cursorMCPPath()
	if err != nil {
		return err
	}
	root, err := readJSONObject(path)
	if err != nil {
		return err
	}
	servers := map[string]any{}
	if existing, ok := root["mcpServers"]; ok {
		var valid bool
		servers, valid = existing.(map[string]any)
		if !valid {
			return fmt.Errorf("parse %s: mcpServers must be an object", path)
		}
	}
	if servers == nil {
		servers = map[string]any{}
	}
	servers["witself"] = map[string]any{
		"command": command[0],
		"args":    command[1:],
	}
	root["mcpServers"] = servers
	return writeJSONObjectAtomic(path, root)
}

func unregisterCursorMCP() error {
	path, err := cursorMCPPath()
	if err != nil {
		return err
	}
	root, err := readJSONObject(path)
	if err != nil {
		return err
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers != nil {
		delete(servers, "witself")
		if len(servers) == 0 {
			delete(root, "mcpServers")
		} else {
			root["mcpServers"] = servers
		}
	}
	if len(root) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeJSONObjectAtomic(path, root)
}

func cursorMCPPath() (string, error) {
	root, err := cursorConfigRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "mcp.json"), nil
}

func cursorConfigRoot() (string, error) {
	root := strings.TrimSpace(os.Getenv("CURSOR_CONFIG_DIR"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".cursor")
	}
	return root, nil
}

func readJSONObject(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseJSONObject(path, raw)
}

func parseJSONObject(path string, raw []byte) (map[string]any, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	root, ok := value.(map[string]any)
	if !ok || root == nil {
		return nil, fmt.Errorf("parse %s: top-level value must be an object", path)
	}
	return root, nil
}

func writeJSONObjectAtomic(path string, root map[string]any) error {
	raw, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(raw, '\n'), 0o600)
}
