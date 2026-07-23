// genericprovider is a host-native test double for the Codex, Claude, Grok,
// and Cursor provider CLIs. It implements only the MCP commands exercised by
// Witself's generic-provider contract tests and mutates the providers' native
// user configuration shapes without requiring an account or network access.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	runtimeEnv     = "WITSELF_FAKE_GENERIC_RUNTIME"
	logEnv         = "WITSELF_FAKE_GENERIC_LOG"
	stateEnv       = "WITSELF_FAKE_GENERIC_STATE"
	failAddEnv     = "WITSELF_FAKE_GENERIC_FAIL_NEXT_ADD"
	failRemoveEnv  = "WITSELF_FAKE_GENERIC_FAIL_AFTER_REMOVE"
	largeErrorEnv  = "WITSELF_FAKE_GENERIC_LARGE_ERROR_BYTES"
	largeOutputEnv = "WITSELF_FAKE_GENERIC_LARGE_OUTPUT_BYTES"
	cwdArtifactEnv = "WITSELF_FAKE_PROVIDER_CWD_ARTIFACT"
	emptyListEnv   = "WITSELF_FAKE_GENERIC_EMPTY_MCP_LIST"
	legacyStateEnv = "FAKE_MCP_STATE"
	legacyFailAdd  = "FAKE_FAIL_ADD_AGENT"
	legacyFailOnce = "FAKE_FAIL_REMOVE_ONCE"
	legacyFailLog  = "FAKE_REMOVE_FAILED"
	blockBegin     = "# BEGIN WITSELF GENERIC PROVIDER TEST DOUBLE"
	blockEnd       = "# END WITSELF GENERIC PROVIDER TEST DOUBLE"
)

type invocation struct {
	Args             []string `json:"args"`
	CodexHome        string   `json:"codex_home,omitempty"`
	ClaudeConfigDir  string   `json:"claude_config_dir,omitempty"`
	GrokHome         string   `json:"grok_home,omitempty"`
	CursorConfigDir  string   `json:"cursor_config_dir,omitempty"`
	WorkingDirectory string   `json:"working_directory"`
}

type binding struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Enabled bool              `json:"enabled"`
	Name    string            `json:"name"`
	Scope   string            `json:"scope"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if err := appendInvocation(args); err != nil {
		return err
	}
	if value := os.Getenv(largeErrorEnv); value != "" {
		size, err := strconv.Atoi(value)
		if err != nil || size < 0 {
			return errors.New("invalid large error byte count")
		}
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("x", size))
		return errors.New("forced large provider error")
	}
	if value := os.Getenv(largeOutputEnv); value != "" {
		size, err := strconv.Atoi(value)
		if err != nil || size < 0 {
			return errors.New("invalid large output byte count")
		}
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", size))
		return nil
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("generic-provider 1.0.0")
		return nil
	}
	if len(args) == 2 && args[0] == "mcp" && args[1] == "--help" {
		fmt.Println("Manage MCP servers")
		return nil
	}
	if len(args) >= 3 && args[0] == "mcp" && (args[1] == "add" || args[1] == "add-json") && args[2] == "--help" {
		return nil
	}
	runtimeName := os.Getenv(runtimeEnv)
	if runtimeName == "" {
		name := strings.ToLower(filepath.Base(os.Args[0]))
		switch {
		case strings.Contains(name, "claude"):
			runtimeName = "claude-code"
		case strings.Contains(name, "grok"):
			runtimeName = "grok-build"
		case strings.Contains(name, "cursor"):
			runtimeName = "cursor"
		case strings.Contains(name, "codex"):
			runtimeName = "codex"
		}
	}
	if runtimeName == "grok-build" && len(args) == 2 && args[0] == "inspect" && args[1] == "--json" {
		fmt.Println(`{"grokVersion":"test","hooks":[],"mcpServers":[]}`)
		return nil
	}
	if runtimeName == "cursor" && len(args) == 3 && args[0] == "mcp" {
		if args[1] == "enable" {
			if failed, err := consumeFailNextAdd(); err != nil {
				return err
			} else if failed {
				return errors.New("forced generic provider add failure")
			}
		}
		return nil
	}
	if len(args) >= 3 && args[0] == "mcp" && (args[1] == "add" || args[1] == "add-json") {
		if shouldFailLegacyAdd(args) {
			return errors.New("forced MCP add failure")
		}
		if failed, err := consumeFailNextAdd(); err != nil {
			return err
		} else if failed {
			return errors.New("forced generic provider add failure")
		}
		current, err := bindingFromAdd(args)
		if err != nil {
			return err
		}
		if err := writeBinding(runtimeName, current); err != nil {
			return err
		}
		if err := writeLegacyState(args); err != nil {
			return err
		}
		if runtimeName == "grok-build" {
			return writeState(current)
		}
		return nil
	}
	if len(args) >= 3 && args[0] == "mcp" && args[1] == "remove" {
		if failed, err := consumeLegacyFailRemoveOnce(); err != nil {
			return err
		} else if failed {
			return errors.New("forced MCP removal failure")
		}
		if err := removeBinding(runtimeName); err != nil {
			return err
		}
		if path := os.Getenv(legacyStateEnv); path != "" {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if runtimeName == "grok-build" {
			if err := os.Remove(os.Getenv(stateEnv)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if failed, err := consumeMarker(failRemoveEnv); err != nil {
			return err
		} else if failed {
			return errors.New("forced generic provider error after removal")
		}
		return nil
	}
	if runtimeName == "grok-build" && len(args) == 3 && args[0] == "mcp" && args[1] == "list" && args[2] == "--json" {
		if os.Getenv(emptyListEnv) == "1" {
			fmt.Println("[]")
			return nil
		}
		current, err := readState()
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("[]")
			return nil
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode([]binding{current})
	}
	return nil
}

func shouldFailLegacyAdd(args []string) bool {
	want := os.Getenv(legacyFailAdd)
	if want == "" {
		return false
	}
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "--agent" && args[index+1] == want {
			return true
		}
	}
	return false
}

func consumeLegacyFailRemoveOnce() (bool, error) {
	if os.Getenv(legacyFailOnce) != "1" {
		return false, nil
	}
	marker := os.Getenv(legacyFailLog)
	if marker == "" {
		return false, errors.New(legacyFailLog + " is required")
	}
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		return true, file.Close()
	}
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	return false, err
}

func writeLegacyState(args []string) error {
	path := os.Getenv(legacyStateEnv)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(args, " ")+"\n"), 0o600)
}

func consumeFailNextAdd() (bool, error) {
	return consumeMarker(failAddEnv)
}

func consumeMarker(environmentName string) (bool, error) {
	marker := os.Getenv(environmentName)
	if marker == "" {
		return false, nil
	}
	if err := os.Remove(marker); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}

func bindingFromAdd(args []string) (binding, error) {
	if len(args) == 6 && args[0] == "mcp" && args[1] == "add-json" && args[2] == "--scope" && args[3] == "user" && args[4] == "witself" {
		var definition struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		}
		if err := json.Unmarshal([]byte(args[5]), &definition); err != nil {
			return binding{}, fmt.Errorf("parse add-json definition: %w", err)
		}
		if definition.Type != "stdio" || definition.Command == "" {
			return binding{}, errors.New("invalid add-json stdio definition")
		}
		return binding{
			Command: definition.Command, Args: definition.Args, Env: definition.Env,
			Enabled: true, Name: "witself", Scope: "user",
		}, nil
	}
	separator := -1
	environment := map[string]string{}
	for index, arg := range args {
		if arg == "--" {
			separator = index
		}
		if (arg == "--env" || arg == "-e") && index+1 < len(args) {
			key, value, ok := strings.Cut(args[index+1], "=")
			if !ok {
				return binding{}, errors.New("invalid --env assignment")
			}
			environment[key] = value
		}
	}
	if separator < 0 || separator+1 >= len(args) {
		return binding{}, errors.New("missing MCP command separator")
	}
	return binding{
		Command: args[separator+1], Args: append([]string(nil), args[separator+2:]...),
		Env: environment, Enabled: true, Name: "witself", Scope: "user",
	}, nil
}

func providerPath(runtimeName string) (string, error) {
	switch runtimeName {
	case "codex":
		return filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), nil
	case "claude-code":
		if root := os.Getenv("CLAUDE_CONFIG_DIR"); root != "" {
			return filepath.Join(root, ".claude.json"), nil
		}
		return filepath.Join(os.Getenv("HOME"), ".claude.json"), nil
	case "grok-build":
		return filepath.Join(os.Getenv("GROK_HOME"), "config.toml"), nil
	default:
		return "", fmt.Errorf("unsupported fake runtime %q", runtimeName)
	}
}

func writeBinding(runtimeName string, current binding) error {
	path, err := providerPath(runtimeName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if runtimeName == "claude-code" {
		root := map[string]any{}
		if raw, readErr := os.ReadFile(path); readErr == nil {
			if err := json.Unmarshal(raw, &root); err != nil {
				return err
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		servers, _ := root["mcpServers"].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
		}
		servers["witself"] = map[string]any{"type": "stdio", "command": current.Command, "args": current.Args, "env": current.Env}
		root["mcpServers"] = servers
		raw, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, append(raw, '\n'), 0o600)
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.Contains(string(raw), blockBegin) {
		return errors.New("fake provider binding already exists")
	}
	command, _ := json.Marshal(current.Command)
	arguments, _ := json.Marshal(current.Args)
	block := fmt.Sprintf("%s\n[mcp_servers.witself]\ncommand = %s\nargs = %s\n", blockBegin, command, arguments)
	if home := current.Env["WITSELF_HOME"]; home != "" {
		encodedHome, _ := json.Marshal(home)
		block += fmt.Sprintf("env = { WITSELF_HOME = %s }\n", encodedHome)
	}
	if runtimeName == "grok-build" {
		block += "enabled = true\n"
	}
	block += blockEnd + "\n"
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}
	return os.WriteFile(path, append(raw, []byte(block)...), 0o600)
}

func removeBinding(runtimeName string) error {
	path, err := providerPath(runtimeName)
	if err != nil {
		return err
	}
	if runtimeName == "claude-code" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		root := map[string]any{}
		if err := json.Unmarshal(raw, &root); err != nil {
			return err
		}
		servers, _ := root["mcpServers"].(map[string]any)
		delete(servers, "witself")
		if len(servers) == 0 {
			delete(root, "mcpServers")
		}
		updated, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, append(updated, '\n'), 0o600)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	start := strings.Index(string(raw), blockBegin)
	end := strings.Index(string(raw), blockEnd)
	if start < 0 || end < start {
		return errors.New("fake provider binding markers missing")
	}
	end += len(blockEnd)
	if end < len(raw) && raw[end] == '\n' {
		end++
	}
	return os.WriteFile(path, append(append([]byte(nil), raw[:start]...), raw[end:]...), 0o600)
}

func appendInvocation(args []string) error {
	path := os.Getenv(logEnv)
	if path == "" {
		return errors.New(logEnv + " is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	workingDirectory, err := os.Getwd()
	if err != nil {
		return err
	}
	if artifact := os.Getenv(cwdArtifactEnv); artifact != "" {
		if filepath.Base(artifact) != artifact {
			return errors.New(cwdArtifactEnv + " must be a base name")
		}
		if err := os.WriteFile(filepath.Join(workingDirectory, artifact), []byte("provider side effect\n"), 0o600); err != nil {
			return err
		}
	}
	if err := json.NewEncoder(file).Encode(invocation{
		Args: args, CodexHome: os.Getenv("CODEX_HOME"), ClaudeConfigDir: os.Getenv("CLAUDE_CONFIG_DIR"),
		GrokHome: os.Getenv("GROK_HOME"), CursorConfigDir: os.Getenv("CURSOR_CONFIG_DIR"),
		WorkingDirectory: workingDirectory,
	}); err != nil {
		return err
	}
	if plainPath := os.Getenv("FAKE_CLI_LOG"); plainPath != "" {
		plain, err := os.OpenFile(plainPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := fmt.Fprintln(plain, strings.Join(args, " "))
		closeErr := plain.Close()
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	return nil
}

func writeState(current binding) error {
	path := os.Getenv(stateEnv)
	if path == "" {
		return errors.New(stateEnv + " is required")
	}
	raw, err := json.Marshal(current)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func readState() (binding, error) {
	raw, err := os.ReadFile(os.Getenv(stateEnv))
	if err != nil {
		return binding{}, err
	}
	var current binding
	if err := json.Unmarshal(raw, &current); err != nil {
		return binding{}, err
	}
	return current, nil
}
