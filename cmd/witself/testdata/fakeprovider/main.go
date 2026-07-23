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
)

const (
	logEnv         = "WITSELF_FAKE_PROVIDER_LOG"
	stateEnv       = "WITSELF_FAKE_PROVIDER_STATE"
	failNextAddEnv = "WITSELF_FAKE_PROVIDER_FAIL_NEXT_MCP_ADD"
)

type invocation struct {
	Args []string `json:"args"`
}

type registry struct {
	Servers map[string][]string `json:"servers"`
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
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("codex-cli 1.2.3")
		return nil
	}
	if len(args) >= 3 && args[0] == "mcp" && args[1] == "add" && args[2] == "--help" {
		return nil
	}
	if len(args) == 3 && args[0] == "mcp" && args[1] == "remove" {
		return updateRegistry(func(state *registry) {
			delete(state.Servers, args[2])
		})
	}
	if len(args) >= 5 && args[0] == "mcp" && args[1] == "add" && args[3] == "--" {
		failed, err := consumeFailNextAdd()
		if err != nil {
			return err
		}
		if failed {
			return errors.New("forced one-shot MCP add failure")
		}
		return updateRegistry(func(state *registry) {
			state.Servers[args[2]] = append([]string(nil), args[4:]...)
		})
	}
	return nil
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
	return json.NewEncoder(file).Encode(invocation{Args: append([]string(nil), args...)})
}

func updateRegistry(update func(*registry)) error {
	path := os.Getenv(stateEnv)
	if path == "" {
		return errors.New(stateEnv + " is required")
	}
	state := registry{Servers: map[string][]string{}}
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
	update(&state)
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}
