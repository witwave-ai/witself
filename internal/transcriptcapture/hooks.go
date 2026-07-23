package transcriptcapture

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"unicode/utf16"
)

const hookCommandMarker = " transcript hook --runtime "

// InstallHooks idempotently adds Witself capture handlers while preserving
// unrelated user and plugin hooks.
func InstallHooks(runtime, mode, executable, account, realm, agent, location string) (string, error) {
	return installHooksForPlatform(goruntime.GOOS, runtime, mode, executable, account, realm, agent, location)
}

// installHooksForPlatform keeps platform-specific command serialization
// testable without requiring the test process itself to run on that platform.
func installHooksForPlatform(platform, runtime, mode, executable, account, realm, agent, location string) (string, error) {
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
	command := shellQuote(executable) + " transcript hook " + hookBindingArgs(runtime, account, realm, agent, location)
	commandWindows := ""
	if runtime == RuntimeCodex && platform == "windows" {
		commandWindows, err = codexWindowsHookCommand(executable, runtime, account, realm, agent, location)
		if err != nil {
			return "", err
		}
	}
	if runtime == RuntimeGrokBuild {
		hooks := map[string]any{}
		addWitselfHandlers(hooks, runtime, mode, command)
		if err := writeJSONAtomic(path, map[string]any{"hooks": hooks}); err != nil {
			return "", err
		}
		return path, nil
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

	switch runtime {
	case RuntimeCursor:
		addCursorWitselfHandlers(hooks, mode, command)
		if _, ok := root["version"]; !ok {
			root["version"] = 1
		}
	case RuntimeCodex:
		addWitselfHandlersWithWindowsCommand(hooks, runtime, mode, command, commandWindows)
	default:
		addWitselfHandlers(hooks, runtime, mode, command)
	}
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
	if runtime == RuntimeGrokBuild {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		_ = os.Remove(filepath.Dir(path))
		return path, nil
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
	if len(root) == 0 || (runtime == RuntimeCursor && len(root) == 1 && root["version"] != nil) {
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

// HooksInstalled reports whether the runtime's user-scoped settings currently
// contain at least one Witself transcript hook. It parses the same settings
// shape used by InstallHooks and RemoveHooks so integration rollback can
// snapshot actual tier presence without treating unrelated hooks as Witself's.
func HooksInstalled(runtime string) (bool, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return false, err
	}
	path, err := hookSettingsPath(runtime)
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	root := map[string]any{}
	if err := json.Unmarshal(raw, &root); err != nil {
		// Preserve the existing install/remove behavior for malformed settings:
		// the mutating operation will report the parse error at its established
		// point in the transaction. A bounded raw marker check is sufficient for
		// rollback to remember whether Witself handlers may have been present.
		return strings.Contains(string(raw), hookCommandMarker), nil
	}
	hooks, _ := root["hooks"].(map[string]any)
	return hasWitselfHandlers(hooks), nil
}

func addWitselfHandlers(hooks map[string]any, runtime, mode, command string) {
	addWitselfHandlersWithWindowsCommand(hooks, runtime, mode, command, "")
}

func addWitselfHandlersWithWindowsCommand(hooks map[string]any, runtime, mode, command, commandWindows string) {
	for _, event := range hookEvents(runtime, mode) {
		handler := map[string]any{
			"type":    "command",
			"command": command,
			"timeout": 10,
		}
		if commandWindows != "" {
			handler["commandWindows"] = commandWindows
		}
		group := map[string]any{
			"hooks": []any{handler},
		}
		if eventNeedsToolMatcher(event) {
			group["matcher"] = "*"
		}
		groups, _ := hooks[event].([]any)
		hooks[event] = append(groups, group)
	}
}

func addCursorWitselfHandlers(hooks map[string]any, mode, command string) {
	for _, event := range hookEvents(RuntimeCursor, mode) {
		handler := map[string]any{
			"command": command,
			"timeout": 10,
		}
		handlers, _ := hooks[event].([]any)
		hooks[event] = append(handlers, handler)
	}
}

func hookEvents(runtime, mode string) []string {
	var events []string
	switch runtime {
	case RuntimeCodex:
		events = []string{
			"SessionStart", "UserPromptSubmit", "Stop",
			"SubagentStart", "SubagentStop", "PreCompact", "PostCompact",
		}
	case RuntimeClaudeCode, RuntimeGrokBuild:
		events = []string{
			"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd",
			"SubagentStart", "SubagentStop", "PreCompact", "PostCompact",
		}
	case RuntimeCursor:
		events = []string{
			"sessionStart", "beforeSubmitPrompt", "afterAgentResponse", "stop", "sessionEnd",
			"subagentStart", "subagentStop", "preCompact",
		}
	}
	// Messages mode still observes tool hooks as a privacy fence. Those hooks
	// are not persisted as ordinary transcript entries, but they must be seen so
	// a sealed tool can synchronously redact the queued prompt and suppress the
	// rest of its turn. Trace/raw additionally retain ordinary tool/thought
	// events.
	trace := mode == ModeTrace || mode == ModeRaw
	switch runtime {
	case RuntimeCodex:
		return append(events, "PreToolUse", "PermissionRequest", "PostToolUse")
	case RuntimeClaudeCode:
		events = append(events,
			"PreToolUse", "PermissionRequest", "PermissionDenied",
			"PostToolUse", "PostToolUseFailure",
		)
		if trace {
			events = append(events, "Notification")
		}
		return events
	case RuntimeGrokBuild:
		events = append(events, "PreToolUse", "PermissionDenied", "PostToolUse", "PostToolUseFailure")
		if trace {
			events = append(events, "Notification")
		}
		return events
	case RuntimeCursor:
		if trace {
			events = append(events, "afterAgentThought")
		}
		return append(events, "preToolUse", "postToolUse", "postToolUseFailure")
	default:
		return events
	}
}

func eventNeedsToolMatcher(event string) bool {
	return event == "PreToolUse" || event == "PermissionRequest" || event == "PermissionDenied" ||
		event == "PostToolUse" || event == "PostToolUseFailure"
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
			if command, _ := group["command"].(string); strings.Contains(command, hookCommandMarker) {
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

func hasWitselfHandlers(hooks map[string]any) bool {
	for _, rawGroups := range hooks {
		groups, ok := rawGroups.([]any)
		if !ok {
			continue
		}
		for _, rawGroup := range groups {
			group, ok := rawGroup.(map[string]any)
			if !ok {
				continue
			}
			if command, _ := group["command"].(string); strings.Contains(command, hookCommandMarker) {
				return true
			}
			handlers, _ := group["hooks"].([]any)
			for _, rawHandler := range handlers {
				handler, ok := rawHandler.(map[string]any)
				if !ok {
					continue
				}
				if command, _ := handler["command"].(string); strings.Contains(command, hookCommandMarker) {
					return true
				}
			}
		}
	}
	return false
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
	case RuntimeGrokBuild:
		root := strings.TrimSpace(os.Getenv("GROK_HOME"))
		if root == "" {
			root = filepath.Join(home, ".grok")
		}
		return filepath.Join(root, "hooks", "witself.json"), nil
	case RuntimeCursor:
		root := strings.TrimSpace(os.Getenv("CURSOR_CONFIG_DIR"))
		if root == "" {
			root = filepath.Join(home, ".cursor")
		}
		return filepath.Join(root, "hooks.json"), nil
	default:
		return "", fmt.Errorf("unsupported runtime %q", runtime)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func codexWindowsHookCommand(executable, runtime, account, realm, agent, location string) (string, error) {
	if err := validateWindowsHookExecutable(executable); err != nil {
		return "", err
	}
	powerShellExecutable, err := codexWindowsPowerShellExecutable()
	if err != nil {
		return "", fmt.Errorf("resolve trusted Windows PowerShell executable: %w", err)
	}
	if err := validateWindowsHookExecutable(powerShellExecutable); err != nil {
		return "", fmt.Errorf("validate trusted Windows PowerShell executable: %w", err)
	}
	args := []string{
		executable, "transcript", "hook", "--runtime", runtime,
		"--account", account, "--realm", realm, "--agent", agent,
	}
	if location != "" {
		args = append(args, "--location", location)
	}

	// Codex executes commandWindows through cmd.exe. Encoding the PowerShell
	// program keeps cmd from expanding %, !, carets, or command separators from
	// authenticated binding names while still presenting every value to
	// witself.exe as a literal argument.
	var script strings.Builder
	script.WriteString("$ErrorActionPreference = 'Stop'; & ")
	for index, arg := range args {
		if index > 0 {
			script.WriteByte(' ')
		}
		script.WriteString(powerShellSingleQuotedLiteral(arg))
	}
	script.WriteString("; exit $LASTEXITCODE")
	encoded := base64.StdEncoding.EncodeToString(utf16LEBytes(script.String()))
	// Pin the kernel-reported system PowerShell path. A bare powershell.exe
	// would let a checked-out project shadow the interpreter when Codex runs a
	// hook from that project's working directory.
	return `"` + powerShellExecutable + `" -NoLogo -NoProfile -NonInteractive -EncodedCommand ` + encoded, nil
}

func validateWindowsHookExecutable(executable string) error {
	if executable == "" || executable != strings.TrimSpace(executable) || strings.ContainsAny(executable, "\x00\r\n") {
		return errors.New("codex Windows hook executable must be a clean absolute .exe path")
	}
	lower := strings.ToLower(executable)
	if !strings.HasSuffix(lower, ".exe") || !windowsPathIsAbs(executable) {
		return errors.New("codex Windows hook executable must be a clean absolute .exe path")
	}
	return nil
}

func windowsPathIsAbs(path string) bool {
	if len(path) >= 3 && isASCIIAlpha(path[0]) && path[1] == ':' && isWindowsPathSeparator(path[2]) {
		return true
	}
	if len(path) < 5 || !isWindowsPathSeparator(path[0]) || !isWindowsPathSeparator(path[1]) {
		return false
	}
	parts := strings.FieldsFunc(path[2:], func(r rune) bool { return r == '\\' || r == '/' })
	return len(parts) >= 3 && parts[0] != "" && parts[1] != ""
}

func isASCIIAlpha(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isWindowsPathSeparator(value byte) bool {
	return value == '\\' || value == '/'
}

func powerShellSingleQuotedLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func utf16LEBytes(value string) []byte {
	encoded := utf16.Encode([]rune(value))
	out := make([]byte, 0, len(encoded)*2)
	for _, unit := range encoded {
		out = append(out, byte(unit), byte(unit>>8))
	}
	return out
}
