package transcriptcapture

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const hookCommandMarker = " transcript hook --runtime "

// InstallHooks idempotently adds Witself capture handlers while preserving
// unrelated user and plugin hooks.
func InstallHooks(runtime, mode, executable, account, realm, agent, location string) (string, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return "", err
	}
	mode, err = NormalizeMode(mode)
	if err != nil {
		return "", err
	}
	account = strings.TrimSpace(account)
	realm = strings.TrimSpace(realm)
	agent = strings.TrimSpace(agent)
	if account == "" || realm == "" || agent == "" {
		return "", errors.New("hook account, realm, and agent are required")
	}
	location = strings.TrimSpace(location)
	path, err := hookSettingsPath(runtime)
	if err != nil {
		return "", err
	}
	root := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &root); err != nil {
			return "", fmt.Errorf("parse %s: %w", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	removeWitselfHandlers(hooks)

	command := shellQuote(executable) + " transcript hook " + hookBindingArgs(runtime, account, realm, agent, location)
	addWitselfHandlers(hooks, runtime, mode, command)
	root["hooks"] = hooks
	if err := writeJSONAtomic(path, root); err != nil {
		return "", err
	}
	return path, nil
}

func hookBindingArgs(runtime, account, realm, agent, location string) string {
	args := "--runtime " + runtime +
		" --account " + shellQuote(account) +
		" --realm " + shellQuote(realm) +
		" --agent " + shellQuote(agent)
	if location != "" {
		args += " --location " + shellQuote(location)
	}
	return args
}

// RemoveHooks removes Witself handlers while preserving unrelated runtime
// settings and hooks.
func RemoveHooks(runtime string) (string, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return "", err
	}
	path, err := hookSettingsPath(runtime)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	root := map[string]any{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks != nil {
		removeWitselfHandlers(hooks)
		if len(hooks) == 0 {
			delete(root, "hooks")
		} else {
			root["hooks"] = hooks
		}
	}
	if len(root) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return path, nil
	}
	if err := writeJSONAtomic(path, root); err != nil {
		return "", err
	}
	return path, nil
}

func addWitselfHandlers(hooks map[string]any, runtime, mode, command string) {
	for _, event := range hookEvents(runtime, mode) {
		group := map[string]any{
			"hooks": []any{map[string]any{
				"type":    "command",
				"command": command,
				"timeout": 10,
			}},
		}
		if event == "PreToolUse" || event == "PostToolUse" || event == "PostToolUseFailure" {
			group["matcher"] = "*"
		}
		groups, _ := hooks[event].([]any)
		hooks[event] = append(groups, group)
	}
}

func hookEvents(runtime, mode string) []string {
	events := []string{"SessionStart", "UserPromptSubmit", "Stop"}
	if runtime == RuntimeClaudeCode {
		events = append(events, "StopFailure", "SessionEnd")
	}
	if mode == ModeTrace || mode == ModeRaw {
		events = append(events, "PreToolUse", "PostToolUse")
		if runtime == RuntimeClaudeCode {
			events = append(events, "PostToolUseFailure")
		}
	}
	return events
}

func removeWitselfHandlers(hooks map[string]any) {
	for event, rawGroups := range hooks {
		groups, ok := rawGroups.([]any)
		if !ok {
			continue
		}
		keptGroups := make([]any, 0, len(groups))
		for _, rawGroup := range groups {
			group, ok := rawGroup.(map[string]any)
			if !ok {
				keptGroups = append(keptGroups, rawGroup)
				continue
			}
			handlers, ok := group["hooks"].([]any)
			if !ok {
				keptGroups = append(keptGroups, rawGroup)
				continue
			}
			keptHandlers := make([]any, 0, len(handlers))
			for _, rawHandler := range handlers {
				handler, ok := rawHandler.(map[string]any)
				command, _ := handler["command"].(string)
				if ok && strings.Contains(command, hookCommandMarker) {
					continue
				}
				keptHandlers = append(keptHandlers, rawHandler)
			}
			if len(keptHandlers) == 0 {
				continue
			}
			group["hooks"] = keptHandlers
			keptGroups = append(keptGroups, group)
		}
		if len(keptGroups) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = keptGroups
		}
	}
}

func hookSettingsPath(runtime string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime {
	case RuntimeCodex:
		root := strings.TrimSpace(os.Getenv("CODEX_HOME"))
		if root == "" {
			root = filepath.Join(home, ".codex")
		}
		return filepath.Join(root, "hooks.json"), nil
	case RuntimeClaudeCode:
		root := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
		if root == "" {
			root = filepath.Join(home, ".claude")
		}
		return filepath.Join(root, "settings.json"), nil
	default:
		return "", fmt.Errorf("unsupported runtime %q", runtime)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
