// fakeprovider is a host-native test double for provider CLIs.
//
// It intentionally has no shell dependency so integration lifecycle tests can
// run unchanged on Windows, macOS, and Linux. Every invocation is appended as
// one JSON record. The small state file models a provider-owned MCP registry so
// tests can verify that Witself removes and replaces only its own entry.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	logEnv         = "WITSELF_FAKE_PROVIDER_LOG"
	stateEnv       = "WITSELF_FAKE_PROVIDER_STATE"
	failNextAddEnv = "WITSELF_FAKE_PROVIDER_FAIL_NEXT_MCP_ADD"
	kindEnv        = "WITSELF_FAKE_PROVIDER_KIND"
	workspaceEnv   = "WITSELF_FAKE_PROVIDER_WORKSPACE"
	cwdArtifactEnv = "WITSELF_FAKE_PROVIDER_CWD_ARTIFACT"
)

type invocation struct {
	Args             []string `json:"args"`
	WorkingDirectory string   `json:"working_directory"`
}

type registry struct {
	Servers     map[string][]string          `json:"servers"`
	Environment map[string]map[string]string `json:"environment,omitempty"`
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
	switch os.Getenv(kindEnv) {
	case "openclaw":
		return runOpenClaw(args)
	case "antigravity":
		return runAntigravity(args)
	case "copilot":
		return runCopilot(args)
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("codex-cli 1.2.3")
		return nil
	}
	if equalArgs(args, "mcp", "--help") {
		fmt.Println("Manage MCP servers")
		return nil
	}
	if len(args) == 3 && args[0] == "mcp" && (args[1] == "enable" || args[1] == "disable") {
		return nil
	}
	if len(args) >= 3 && args[0] == "mcp" && args[1] == "add" && args[2] == "--help" {
		return nil
	}
	if len(args) == 3 && args[0] == "mcp" && args[1] == "remove" {
		return updateRegistry(func(state *registry) {
			delete(state.Servers, args[2])
			delete(state.Environment, args[2])
		})
	}
	if len(args) >= 5 && args[0] == "mcp" && args[1] == "add" {
		separator := -1
		environment := map[string]string{}
		for index, arg := range args {
			if arg == "--" {
				separator = index
			}
			if arg == "--env" && index+1 < len(args) {
				key, value, ok := strings.Cut(args[index+1], "=")
				if !ok {
					return errors.New("invalid --env assignment")
				}
				environment[key] = value
			}
		}
		if separator < 0 || separator+1 >= len(args) {
			return errors.New("missing MCP command separator")
		}
		failed, err := consumeFailNextAdd()
		if err != nil {
			return err
		}
		if failed {
			return errors.New("forced one-shot MCP add failure")
		}
		return updateRegistry(func(state *registry) {
			state.Servers[args[2]] = append([]string(nil), args[separator+1:]...)
			state.Environment[args[2]] = environment
		})
	}
	return nil
}

func runOpenClaw(args []string) error {
	switch {
	case equalArgs(args, "--version"):
		fmt.Println("OpenClaw 2026.7.1-2")
		return nil
	case equalArgs(args, "mcp", "add", "--help"):
		return nil
	case equalArgs(args, "agents", "list", "--json"):
		workspace := os.Getenv(workspaceEnv)
		if workspace == "" {
			return errors.New(workspaceEnv + " is required")
		}
		return json.NewEncoder(os.Stdout).Encode([]map[string]any{{
			"id": "main", "workspace": workspace, "isDefault": true,
		}})
	case equalArgs(args, "mcp", "list", "--json"):
		state, err := readRawState()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(state)
	case len(args) >= 3 && args[0] == "mcp" && args[1] == "add" && args[2] == "witself":
		state, err := readRawState()
		if err != nil {
			return err
		}
		if _, exists := state["witself"]; exists {
			return errors.New("witself already exists")
		}
		binding := map[string]any{"args": []string{}, "env": map[string]string{}}
		arguments := binding["args"].([]string)
		environment := binding["env"].(map[string]string)
		for index := 3; index < len(args); {
			switch args[index] {
			case "--no-probe":
				index++
			case "--command":
				if index+1 >= len(args) {
					return errors.New("missing --command value")
				}
				binding["command"] = args[index+1]
				index += 2
			case "--connect-timeout":
				if index+1 >= len(args) {
					return errors.New("missing --connect-timeout value")
				}
				value, err := strconv.Atoi(args[index+1])
				if err != nil {
					return err
				}
				binding["connectTimeout"] = value
				index += 2
			case "--env":
				if index+1 >= len(args) {
					return errors.New("missing --env value")
				}
				key, value, ok := strings.Cut(args[index+1], "=")
				if !ok {
					return errors.New("invalid --env assignment")
				}
				environment[key] = value
				index += 2
			case "--arg":
				if index+1 >= len(args) {
					return errors.New("missing --arg value")
				}
				arguments = append(arguments, args[index+1])
				index += 2
			default:
				return fmt.Errorf("unexpected OpenClaw add argument %q", args[index])
			}
		}
		if binding["command"] == nil {
			return errors.New("missing OpenClaw MCP command")
		}
		binding["args"] = arguments
		if len(environment) == 0 {
			delete(binding, "env")
		}
		state["witself"] = binding
		return writeRawState(state)
	case equalArgs(args, "mcp", "unset", "witself"):
		state, err := readRawState()
		if err != nil {
			return err
		}
		if _, exists := state["witself"]; !exists {
			return errors.New(`No MCP server named "witself"`)
		}
		delete(state, "witself")
		return writeRawState(state)
	default:
		return fmt.Errorf("unexpected OpenClaw command: %v", args)
	}
}

func runAntigravity(args []string) error {
	switch {
	case equalArgs(args, "--version"):
		fmt.Println("agy version 1.1.5")
		return nil
	case len(args) == 3 && args[0] == "plugin" && args[1] == "validate":
		for _, relative := range []string{"plugin.json", filepath.Join("rules", "witself.md")} {
			info, err := os.Stat(filepath.Join(args[2], relative))
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("missing plugin file %s", relative)
			}
		}
		if _, err := os.Lstat(filepath.Join(args[2], "mcp_config.json")); !errors.Is(err, os.ErrNotExist) {
			return errors.New("rules-only plugin unexpectedly contains mcp_config.json")
		}
		fmt.Println("Plugin validation passed")
		return nil
	default:
		return fmt.Errorf("unexpected Antigravity command: %v", args)
	}
}

func runCopilot(args []string) error {
	root := os.Getenv("COPILOT_HOME")
	if root == "" {
		return errors.New("COPILOT_HOME is required")
	}
	configPath := filepath.Join(root, "mcp-config.json")
	switch {
	case equalArgs(args, "--version"):
		fmt.Println("GitHub Copilot CLI 1.0.73")
		return nil
	case equalArgs(args, "mcp", "add", "--help"):
		return nil
	case equalArgs(args, "mcp", "list", "--json"):
		document, err := readCopilotDocument(configPath)
		if err != nil {
			return err
		}
		servers := document["mcpServers"].(map[string]any)
		for name, raw := range servers {
			binding, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			decorated := cloneAnyMap(binding)
			decorated["source"] = "user"
			decorated["enabled"] = true
			servers[name] = decorated
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"mcpServers": servers})
	case len(args) >= 3 && args[0] == "mcp" && args[1] == "add":
		document, err := readCopilotDocument(configPath)
		if err != nil {
			return err
		}
		servers := document["mcpServers"].(map[string]any)
		name := args[2]
		if _, exists := servers[name]; exists {
			return fmt.Errorf("server %s already exists", name)
		}
		binding := map[string]any{
			"type": "local", "tools": []string{}, "env": map[string]string{},
		}
		environment := binding["env"].(map[string]string)
		index := 3
		for index < len(args) && args[index] != "--" {
			if index+1 >= len(args) {
				return errors.New("missing Copilot add option value")
			}
			switch args[index] {
			case "--tools":
				binding["tools"] = []string{args[index+1]}
			case "--env":
				key, value, ok := strings.Cut(args[index+1], "=")
				if !ok {
					return errors.New("invalid Copilot --env assignment")
				}
				environment[key] = value
			default:
				return fmt.Errorf("unexpected Copilot add option %q", args[index])
			}
			index += 2
		}
		if index+1 >= len(args) || args[index] != "--" {
			return errors.New("missing Copilot command separator")
		}
		binding["command"] = args[index+1]
		binding["args"] = append([]string(nil), args[index+2:]...)
		servers[name] = binding
		return writeCopilotDocument(configPath, document)
	case len(args) == 3 && args[0] == "mcp" && args[1] == "remove":
		document, err := readCopilotDocument(configPath)
		if err != nil {
			return err
		}
		servers := document["mcpServers"].(map[string]any)
		if _, exists := servers[args[2]]; !exists {
			return fmt.Errorf("no MCP server named %s", args[2])
		}
		delete(servers, args[2])
		return writeCopilotDocument(configPath, document)
	default:
		return fmt.Errorf("unexpected Copilot command: %v", args)
	}
}

func equalArgs(actual []string, expected ...string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range actual {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

func readRawState() (map[string]any, error) {
	path := os.Getenv(stateEnv)
	if path == "" {
		return nil, errors.New(stateEnv + " is required")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	if state == nil {
		state = map[string]any{}
	}
	return state, nil
}

func writeRawState(state map[string]any) error {
	path := os.Getenv(stateEnv)
	if path == "" {
		return errors.New(stateEnv + " is required")
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func readCopilotDocument(path string) (map[string]any, error) {
	document := map[string]any{}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &document); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	servers, ok := document["mcpServers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
		document["mcpServers"] = servers
	}
	return document, nil
}

func writeCopilotDocument(path string, document map[string]any) error {
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func cloneAnyMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func consumeFailNextAdd() (bool, error) {
	path := os.Getenv(failNextAddEnv)
	if path == "" {
		return false, nil
	}
	if err := os.Remove(path); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}

func appendInvocation(args []string) error {
	path := os.Getenv(logEnv)
	if path == "" {
		return errors.New(logEnv + " is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
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
	return json.NewEncoder(file).Encode(invocation{
		Args:             append([]string(nil), args...),
		WorkingDirectory: workingDirectory,
	})
}

func updateRegistry(update func(*registry)) error {
	path := os.Getenv(stateEnv)
	if path == "" {
		return errors.New(stateEnv + " is required")
	}
	state := registry{Servers: map[string][]string{}, Environment: map[string]map[string]string{}}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &state); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if state.Servers == nil {
		state.Servers = map[string][]string{}
	}
	if state.Environment == nil {
		state.Environment = map[string]map[string]string{}
	}
	update(&state)
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return writeCodexConfig(state)
}

func writeCodexConfig(state registry) error {
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		return nil
	}
	path := filepath.Join(root, "config.toml")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	names := make([]string, 0, len(state.Servers))
	for name := range state.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	var out strings.Builder
	for _, name := range names {
		command := state.Servers[name]
		if len(command) == 0 {
			continue
		}
		fmt.Fprintf(&out, "[mcp_servers.%s]\n", name)
		fmt.Fprintf(&out, "command = %s\n", strconv.Quote(command[0]))
		out.WriteString("args = [")
		for index, arg := range command[1:] {
			if index != 0 {
				out.WriteString(", ")
			}
			out.WriteString(strconv.Quote(arg))
		}
		out.WriteString("]\n")
		if environment := state.Environment[name]; len(environment) != 0 {
			keys := make([]string, 0, len(environment))
			for key := range environment {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			out.WriteString("env = { ")
			for index, key := range keys {
				if index != 0 {
					out.WriteString(", ")
				}
				fmt.Fprintf(&out, "%s = %s", key, strconv.Quote(environment[key]))
			}
			out.WriteString(" }\n")
		}
		out.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(out.String()), 0o600)
}
