package transcriptcapture

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"unicode/utf16"
)

const hookConfigReadLimit = 8 * 1024 * 1024

const hookCommandMarker = " transcript hook --runtime "

// InstallHooks idempotently adds Witself capture handlers while preserving
// unrelated user and plugin hooks.
func InstallHooks(runtime, mode, executable, account, realm, agent, location string) (string, error) {
	return installHooksForPlatform(goruntime.GOOS, runtime, mode, executable, account, realm, agent, location)
}

// InstallHooksWithWitselfHome pins the Witself state root in the serialized
// hook command. An explicit argument is portable across POSIX shells and the
// Codex commandWindows path, unlike an inline environment assignment.
func InstallHooksWithWitselfHome(runtime, mode, executable, account, realm, agent, location, witselfHome string) (string, error) {
	return installHooksForPlatformWithWitselfHome(goruntime.GOOS, runtime, mode, executable, account, realm, agent, location, witselfHome)
}

// installHooksForPlatform keeps platform-specific command serialization
// testable without requiring the test process itself to run on that platform.
func installHooksForPlatform(platform, runtime, mode, executable, account, realm, agent, location string) (string, error) {
	return installHooksForPlatformWithWitselfHome(platform, runtime, mode, executable, account, realm, agent, location, "")
}

func installHooksForPlatformWithWitselfHome(platform, runtime, mode, executable, account, realm, agent, location, witselfHome string) (string, error) {
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
	witselfHome = strings.TrimSpace(witselfHome)
	if err := validateHookWitselfHome(platform, witselfHome); err != nil {
		return "", err
	}
	path, err := hookSettingsPath(runtime)
	if err != nil {
		return "", err
	}
	command := shellQuote(executable) + " transcript hook " + hookBindingArgsWithWitselfHome(runtime, account, realm, agent, location, witselfHome)
	commandWindows := ""
	if runtime == RuntimeCodex && platform == "windows" {
		commandWindows, err = codexWindowsHookCommandWithWitselfHome(executable, runtime, account, realm, agent, location, witselfHome)
		if err != nil {
			return "", err
		}
	}
	if runtime == RuntimeGrokBuild {
		hooks := map[string]any{}
		addWitselfHandlers(hooks, runtime, mode, command)
		snapshot, err := readHookFileSnapshot(path)
		if err != nil {
			return "", err
		}
		if snapshot.exists {
			root, err := parseHookConfigRoot(path, snapshot.raw)
			if err != nil {
				return "", err
			}
			if err := validateOwnedGrokHookConfig(root); err != nil {
				return "", fmt.Errorf("inspect %s: %w", path, err)
			}
		}
		if err := writeHookJSONAtomicCAS(path, map[string]any{"hooks": hooks}, snapshot); err != nil {
			return "", err
		}
		return path, nil
	}
	snapshot, err := readHookFileSnapshot(path)
	if err != nil {
		return "", err
	}
	root := map[string]any{}
	if snapshot.exists {
		root, err = parseHookConfigRoot(path, snapshot.raw)
		if err != nil {
			return "", err
		}
	}
	hooks := map[string]any{}
	if rawHooks, ok := root["hooks"]; ok {
		var valid bool
		hooks, valid = rawHooks.(map[string]any)
		if !valid || hooks == nil {
			return "", fmt.Errorf("parse %s: hooks must be a JSON object", path)
		}
	}
	for _, event := range hookEvents(runtime, mode) {
		if rawGroups, ok := hooks[event]; ok {
			if _, valid := rawGroups.([]any); !valid {
				return "", fmt.Errorf("parse %s: hook event %q must be a JSON array", path, event)
			}
		}
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
	if err := writeHookJSONAtomicCAS(path, root, snapshot); err != nil {
		return "", err
	}
	return path, nil
}

func validateHookWitselfHome(platform, value string) error {
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("hook WITSELF_HOME must be a clean absolute path")
	}
	if platform == "windows" {
		if !windowsPathIsAbs(value) {
			return errors.New("hook WITSELF_HOME must be a clean absolute path")
		}
		for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '\\' || r == '/' }) {
			if part == "." || part == ".." {
				return errors.New("hook WITSELF_HOME must be a clean absolute path")
			}
		}
		return nil
	}
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return errors.New("hook WITSELF_HOME must be a clean absolute path")
	}
	return nil
}

func hookBindingArgs(runtime, account, realm, agent, location string) string {
	return hookBindingArgsWithWitselfHome(runtime, account, realm, agent, location, "")
}

func hookBindingArgsWithWitselfHome(runtime, account, realm, agent, location, witselfHome string) string {
	args := "--runtime " + runtime +
		" --account " + shellQuote(account) +
		" --realm " + shellQuote(realm) +
		" --agent " + shellQuote(agent)
	if location != "" {
		args += " --location " + shellQuote(location)
	}
	if witselfHome != "" {
		args += " --witself-home " + shellQuote(witselfHome)
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
	snapshot, err := readHookFileSnapshot(path)
	if err != nil {
		return "", err
	}
	if !snapshot.exists {
		return path, nil
	}
	if runtime == RuntimeGrokBuild {
		root, err := parseHookConfigRoot(path, snapshot.raw)
		if err != nil {
			return "", err
		}
		if err := validateOwnedGrokHookConfig(root); err != nil {
			return "", fmt.Errorf("inspect %s: %w", path, err)
		}
		if err := removeHookFileCAS(path, snapshot); err != nil {
			return "", err
		}
		_ = os.Remove(filepath.Dir(path))
		return path, nil
	}
	root, err := parseHookConfigRoot(path, snapshot.raw)
	if err != nil {
		return "", err
	}
	rawHooks, ok := root["hooks"]
	if !ok {
		return path, nil
	}
	hooks, ok := rawHooks.(map[string]any)
	if !ok || hooks == nil {
		return "", fmt.Errorf("parse %s: hooks must be a JSON object", path)
	}
	if !hasWitselfHandlers(hooks) {
		return path, nil
	}
	removeWitselfHandlers(hooks)
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}
	if len(root) == 0 || (runtime == RuntimeCursor && len(root) == 1 && root["version"] != nil) {
		if err := removeHookFileCAS(path, snapshot); err != nil {
			return "", err
		}
		return path, nil
	}
	if err := writeHookJSONAtomicCAS(path, root, snapshot); err != nil {
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
	snapshot, err := readHookFileSnapshot(path)
	if err != nil {
		return false, err
	}
	if !snapshot.exists {
		return false, nil
	}
	root := map[string]any{}
	if err := json.Unmarshal(snapshot.raw, &root); err != nil {
		// Preserve the existing install/remove behavior for malformed settings:
		// the mutating operation will report the parse error at its established
		// point in the transaction. A bounded raw marker check is sufficient for
		// rollback to remember whether Witself handlers may have been present.
		return strings.Contains(string(snapshot.raw), hookCommandMarker), nil
	}
	hooks, _ := root["hooks"].(map[string]any)
	return hasWitselfHandlers(hooks), nil
}

type hookFileSnapshot struct {
	exists bool
	raw    []byte
	info   os.FileInfo
}

func readHookFileSnapshot(path string) (snapshot hookFileSnapshot, returnErr error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, fmt.Errorf("inspect hook config %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return snapshot, fmt.Errorf("hook config %s must be a real regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return snapshot, fmt.Errorf("open hook config %s: %w", path, err)
	}
	defer func() {
		if err := file.Close(); returnErr == nil && err != nil {
			returnErr = fmt.Errorf("close hook config %s: %w", path, err)
		}
	}()
	openedInfo, err := file.Stat()
	if err != nil {
		return snapshot, fmt.Errorf("inspect open hook config %s: %w", path, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return snapshot, fmt.Errorf("hook config %s changed while opening", path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, hookConfigReadLimit+1))
	if err != nil {
		return snapshot, fmt.Errorf("read hook config %s: %w", path, err)
	}
	if len(raw) > hookConfigReadLimit {
		return snapshot, fmt.Errorf("hook config %s exceeds %d bytes", path, hookConfigReadLimit)
	}
	afterInfo, err := os.Lstat(path)
	if err != nil || afterInfo.Mode()&os.ModeSymlink != 0 || !afterInfo.Mode().IsRegular() || !os.SameFile(info, afterInfo) {
		if err == nil {
			err = errors.New("file identity changed")
		}
		return snapshot, fmt.Errorf("hook config %s changed while reading: %w", path, err)
	}
	snapshot.exists = true
	snapshot.raw = raw
	snapshot.info = info
	return snapshot, nil
}

func parseHookConfigRoot(path string, raw []byte) (map[string]any, error) {
	if err := rejectDuplicateHookJSONKeys(raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	root := map[string]any{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if root == nil {
		return nil, fmt.Errorf("parse %s: root must be a JSON object", path)
	}
	return root, nil
}

func rejectDuplicateHookJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := consumeUniqueHookJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func consumeUniqueHookJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			seen[key] = true
			if err := consumeUniqueHookJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueHookJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func writeHookJSONAtomicCAS(path string, value any, expected hookFileSnapshot) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("inspect hook config directory %s: %w", filepath.Dir(path), err)
	}
	if !parentInfo.IsDir() {
		return fmt.Errorf("hook config directory %s must be a directory", filepath.Dir(path))
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".witself-hooks-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	stagedInfo, err := os.Lstat(tmpPath)
	if err != nil {
		return fmt.Errorf("inspect staged hook config %s: %w", tmpPath, err)
	}
	if !stagedInfo.Mode().IsRegular() {
		return fmt.Errorf("staged hook config %s must be a regular file", tmpPath)
	}
	runOwnedHookBeforeMutationForTest(path)
	if err := verifyHookFileSnapshot(path, expected); err != nil {
		return err
	}
	if err := replaceFileAtomic(tmpPath, path); err != nil {
		return err
	}
	committed, err := readHookFileSnapshot(path)
	if err != nil {
		return fmt.Errorf("verify hook config commit %s: %w", path, err)
	}
	if !committed.exists || !os.SameFile(committed.info, stagedInfo) || !bytes.Equal(committed.raw, raw) {
		return fmt.Errorf("hook config %s changed during commit; refusing to overwrite the later value", path)
	}
	return nil
}

func removeHookFileCAS(path string, expected hookFileSnapshot) error {
	runOwnedHookBeforeMutationForTest(path)
	if err := verifyHookFileSnapshot(path, expected); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("hook config %s still exists after removal", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("verify hook config removal %s: %w", path, err)
	}
	return nil
}

// ownedHookBeforeMutationForTest lets deterministic security tests replace a
// hook document in the final interval before the snapshot is revalidated.
// Production code leaves it nil.
var ownedHookBeforeMutationForTest func(string)

func runOwnedHookBeforeMutationForTest(path string) {
	if ownedHookBeforeMutationForTest != nil {
		ownedHookBeforeMutationForTest(path)
	}
}

func verifyHookFileSnapshot(path string, expected hookFileSnapshot) error {
	current, err := readHookFileSnapshot(path)
	if err != nil {
		return err
	}
	if current.exists != expected.exists {
		return fmt.Errorf("hook config %s changed concurrently; refusing to overwrite it", path)
	}
	if !current.exists {
		return nil
	}
	if !os.SameFile(current.info, expected.info) || !bytes.Equal(current.raw, expected.raw) {
		return fmt.Errorf("hook config %s changed concurrently; refusing to overwrite it", path)
	}
	return nil
}

func validateOwnedGrokHookConfig(root map[string]any) error {
	if len(root) != 1 {
		return errors.New("the Grok dedicated hook file contains non-Witself settings; refusing to overwrite or remove it")
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok || hooks == nil {
		return errors.New("the Grok dedicated hook file is not an exact Witself-owned hook document")
	}
	if !grokHookEventsAreOwned(hooks, hookEvents(RuntimeGrokBuild, ModeMessages)) &&
		!grokHookEventsAreOwned(hooks, hookEvents(RuntimeGrokBuild, ModeRaw)) {
		return errors.New("the Grok dedicated hook file has drifted from the exact Witself-owned hook shape")
	}
	return nil
}

func grokHookEventsAreOwned(hooks map[string]any, expectedEvents []string) bool {
	if len(hooks) != len(expectedEvents) {
		return false
	}
	expected := make(map[string]bool, len(expectedEvents))
	for _, event := range expectedEvents {
		expected[event] = true
	}
	commonCommand := ""
	for event, rawGroups := range hooks {
		if !expected[event] {
			return false
		}
		groups, ok := rawGroups.([]any)
		if !ok || len(groups) != 1 {
			return false
		}
		group, ok := groups[0].(map[string]any)
		if !ok {
			return false
		}
		wantGroupFields := 1
		if eventNeedsToolMatcher(event) {
			wantGroupFields = 2
			if group["matcher"] != "*" {
				return false
			}
		}
		if len(group) != wantGroupFields {
			return false
		}
		handlers, ok := group["hooks"].([]any)
		if !ok || len(handlers) != 1 {
			return false
		}
		handler, ok := handlers[0].(map[string]any)
		if !ok || len(handler) != 3 || handler["type"] != "command" || handler["timeout"] != float64(10) {
			return false
		}
		command, ok := handler["command"].(string)
		if !ok || !strings.Contains(command, hookCommandMarker+RuntimeGrokBuild+" ") {
			return false
		}
		if commonCommand == "" {
			commonCommand = command
		} else if command != commonCommand {
			return false
		}
	}
	return commonCommand != ""
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
	return codexWindowsHookCommandWithWitselfHome(executable, runtime, account, realm, agent, location, "")
}

func codexWindowsHookCommandWithWitselfHome(executable, runtime, account, realm, agent, location, witselfHome string) (string, error) {
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
	if witselfHome != "" {
		args = append(args, "--witself-home", witselfHome)
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
