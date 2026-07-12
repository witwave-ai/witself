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
	root := strings.TrimSpace(os.Getenv("CURSOR_CONFIG_DIR"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".cursor")
	}
	return filepath.Join(root, "mcp.json"), nil
}

func readJSONObject(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	root := map[string]any{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return root, nil
}

func writeJSONObjectAtomic(path string, root map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".witself-mcp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
