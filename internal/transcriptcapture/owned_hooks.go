package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
)

// UserHooksOptions is the complete, durable ownership identity for one
// user-scoped hook installation. ConfigPath is deliberately explicit: refresh,
// verification, and removal must not follow a later CODEX_HOME,
// CLAUDE_CONFIG_DIR, GROK_HOME, or CURSOR_CONFIG_DIR value.
type UserHooksOptions struct {
	Platform    string
	Runtime     string
	Mode        string
	Executable  string
	Account     string
	Realm       string
	Agent       string
	Location    string
	WitselfHome string
	ConfigPath  string
}

// HookMutation reports the exact path and whether this invocation committed a
// filesystem mutation. Callers use Touched to avoid rolling back state they did
// not create.
type HookMutation struct {
	Path    string
	Touched bool
}

// DefaultUserHooksOptions resolves the current provider selector once. The
// returned ConfigPath must be persisted and reused for every later operation.
func DefaultUserHooksOptions(runtimeName, mode, executable, account, realm, agent, location, witselfHome string) (UserHooksOptions, error) {
	path, err := hookSettingsPath(runtimeName)
	if err != nil {
		return UserHooksOptions{}, err
	}
	opts := UserHooksOptions{
		Platform:    goruntime.GOOS,
		Runtime:     runtimeName,
		Mode:        mode,
		Executable:  executable,
		Account:     account,
		Realm:       realm,
		Agent:       agent,
		Location:    location,
		WitselfHome: witselfHome,
		ConfigPath:  path,
	}
	return normalizeUserHooksOptions(opts)
}

// InstallOwnedHooks installs desired and, when previous is non-nil, replaces
// only the exact handler set reconstructed from that durable prior binding.
// Supplying previous is also the sole legacy migration path: marker-shaped
// handlers are adopted only when every command, event, matcher, timeout, and
// platform-specific command field exactly matches the prior integration.
func InstallOwnedHooks(desired UserHooksOptions, previous *UserHooksOptions) (HookMutation, error) {
	var err error
	desired, err = normalizeUserHooksOptions(desired)
	if err != nil {
		return HookMutation{}, err
	}
	if previous != nil {
		normalized, normalizeErr := normalizeUserHooksOptions(*previous)
		if normalizeErr != nil {
			return HookMutation{}, fmt.Errorf("normalize previous hook ownership: %w", normalizeErr)
		}
		previous = &normalized
		if previous.Runtime != desired.Runtime || previous.ConfigPath != desired.ConfigPath {
			return HookMutation{}, errors.New("previous and desired hooks must use the same persisted runtime and config path")
		}
	}

	snapshot, err := readHookFileSnapshot(desired.ConfigPath)
	if err != nil {
		return HookMutation{}, err
	}
	if desired.Runtime == RuntimeGrokBuild {
		return installOwnedGrokHooks(desired, previous, snapshot)
	}

	root := map[string]any{}
	if snapshot.exists {
		root, err = parseHookConfigRoot(desired.ConfigPath, snapshot.raw)
		if err != nil {
			return HookMutation{}, err
		}
	}
	before := cloneHookDocument(root)
	hooks, err := sharedHookMap(root, desired.ConfigPath)
	if err != nil {
		return HookMutation{}, err
	}
	if previous == nil {
		if hasHookCommandMarker(hooks) {
			return HookMutation{}, fmt.Errorf("hook config %s contains marker-shaped handlers without a durable Witself ownership record; refusing to claim or replace them", desired.ConfigPath)
		}
	} else {
		found, err := removeExactOwnedHookSet(hooks, *previous, true)
		if err != nil {
			return HookMutation{}, fmt.Errorf("inspect prior hooks in %s: %w", desired.ConfigPath, err)
		}
		if found != 0 && found != len(hookEvents(previous.Runtime, previous.Mode)) {
			return HookMutation{}, fmt.Errorf("hook config %s contains only part of the exact prior Witself handler set", desired.ConfigPath)
		}
	}
	if hasHookCommandMarker(hooks) {
		return HookMutation{}, fmt.Errorf("hook config %s contains a foreign or mixed marker-shaped handler; refusing to overwrite it", desired.ConfigPath)
	}
	if err := addExactOwnedHookSet(hooks, desired); err != nil {
		return HookMutation{}, err
	}
	root["hooks"] = hooks
	if reflect.DeepEqual(before, root) {
		return HookMutation{Path: desired.ConfigPath}, nil
	}
	if err := writeHookJSONAtomicCAS(desired.ConfigPath, root, snapshot); err != nil {
		return HookMutation{}, err
	}
	return HookMutation{Path: desired.ConfigPath, Touched: true}, nil
}

// RemoveOwnedHooks removes only the exact event/handler set represented by the
// persisted binding. Missing complete state is an idempotent no-op; partial,
// mixed, or differently bound marker handlers are drift and remain untouched.
func RemoveOwnedHooks(installed UserHooksOptions) (HookMutation, error) {
	installed, err := normalizeUserHooksOptions(installed)
	if err != nil {
		return HookMutation{}, err
	}
	snapshot, err := readHookFileSnapshot(installed.ConfigPath)
	if err != nil {
		return HookMutation{}, err
	}
	if !snapshot.exists {
		return HookMutation{Path: installed.ConfigPath}, nil
	}
	if installed.Runtime == RuntimeGrokBuild {
		root, err := parseHookConfigRoot(installed.ConfigPath, snapshot.raw)
		if err != nil {
			return HookMutation{}, err
		}
		exact, exactErr := exactGrokHookDocument(root, installed)
		if exactErr != nil {
			return HookMutation{}, exactErr
		}
		if !exact {
			return HookMutation{}, fmt.Errorf("hook config %s is not the exact persisted Grok hook binding; refusing to remove it", installed.ConfigPath)
		}
		if err := removeHookFileCAS(installed.ConfigPath, snapshot); err != nil {
			return HookMutation{}, err
		}
		_ = os.Remove(filepath.Dir(installed.ConfigPath))
		return HookMutation{Path: installed.ConfigPath, Touched: true}, nil
	}

	root, err := parseHookConfigRoot(installed.ConfigPath, snapshot.raw)
	if err != nil {
		return HookMutation{}, err
	}
	before := cloneHookDocument(root)
	hooks, err := sharedHookMap(root, installed.ConfigPath)
	if err != nil {
		return HookMutation{}, err
	}
	found, err := removeExactOwnedHookSet(hooks, installed, true)
	if err != nil {
		return HookMutation{}, fmt.Errorf("inspect hooks in %s: %w", installed.ConfigPath, err)
	}
	if found == 0 {
		if hasHookCommandMarker(hooks) {
			return HookMutation{}, fmt.Errorf("hook config %s contains marker-shaped handlers that do not match the persisted binding", installed.ConfigPath)
		}
		return HookMutation{Path: installed.ConfigPath}, nil
	}
	if found != len(hookEvents(installed.Runtime, installed.Mode)) {
		return HookMutation{}, fmt.Errorf("hook config %s contains only part of the persisted Witself handler set", installed.ConfigPath)
	}
	if hasHookCommandMarker(hooks) {
		return HookMutation{}, fmt.Errorf("hook config %s contains a foreign or mixed marker-shaped handler; refusing to modify it", installed.ConfigPath)
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}
	if reflect.DeepEqual(before, root) {
		return HookMutation{Path: installed.ConfigPath}, nil
	}
	if len(root) == 0 || (installed.Runtime == RuntimeCursor && len(root) == 1 && root["version"] != nil) {
		if err := removeHookFileCAS(installed.ConfigPath, snapshot); err != nil {
			return HookMutation{}, err
		}
		return HookMutation{Path: installed.ConfigPath, Touched: true}, nil
	}
	if err := writeHookJSONAtomicCAS(installed.ConfigPath, root, snapshot); err != nil {
		return HookMutation{}, err
	}
	return HookMutation{Path: installed.ConfigPath, Touched: true}, nil
}

// VerifyOwnedHooks verifies the complete exact owned handler set without
// accepting extra marker-shaped handlers.
func VerifyOwnedHooks(installed UserHooksOptions) error {
	installed, err := normalizeUserHooksOptions(installed)
	if err != nil {
		return err
	}
	snapshot, err := readHookFileSnapshot(installed.ConfigPath)
	if err != nil {
		return err
	}
	if !snapshot.exists {
		return fmt.Errorf("owned hook config %s is missing", installed.ConfigPath)
	}
	root, err := parseHookConfigRoot(installed.ConfigPath, snapshot.raw)
	if err != nil {
		return err
	}
	if installed.Runtime == RuntimeGrokBuild {
		exact, exactErr := exactGrokHookDocument(root, installed)
		if exactErr != nil {
			return exactErr
		}
		if !exact {
			return fmt.Errorf("the Grok hook config %s differs from the exact persisted binding", installed.ConfigPath)
		}
		return nil
	}
	hooks, err := sharedHookMap(root, installed.ConfigPath)
	if err != nil {
		return err
	}
	copyHooks := cloneHookMap(hooks)
	found, err := removeExactOwnedHookSet(copyHooks, installed, true)
	if err != nil {
		return err
	}
	if found != len(hookEvents(installed.Runtime, installed.Mode)) {
		return fmt.Errorf("hook config %s does not contain the complete persisted Witself handler set", installed.ConfigPath)
	}
	if hasHookCommandMarker(copyHooks) {
		return fmt.Errorf("hook config %s contains a foreign or mixed marker-shaped handler", installed.ConfigPath)
	}
	return nil
}

func normalizeUserHooksOptions(opts UserHooksOptions) (UserHooksOptions, error) {
	var err error
	opts.Runtime, err = NormalizeRuntime(opts.Runtime)
	if err != nil {
		return UserHooksOptions{}, err
	}
	opts.Mode, err = NormalizeMode(opts.Mode)
	if err != nil {
		return UserHooksOptions{}, err
	}
	opts.Platform = strings.TrimSpace(opts.Platform)
	if opts.Platform == "" {
		opts.Platform = goruntime.GOOS
	}
	opts.Executable = strings.TrimSpace(opts.Executable)
	opts.Account = strings.TrimSpace(opts.Account)
	opts.Realm = strings.TrimSpace(opts.Realm)
	opts.Agent = strings.TrimSpace(opts.Agent)
	opts.Location = strings.TrimSpace(opts.Location)
	opts.WitselfHome = strings.TrimSpace(opts.WitselfHome)
	opts.ConfigPath = strings.TrimSpace(opts.ConfigPath)
	if opts.Account == "" || opts.Realm == "" || opts.Agent == "" {
		return UserHooksOptions{}, errors.New("hook account, realm, and agent are required")
	}
	if opts.Location != "" && !locationNamePattern.MatchString(opts.Location) {
		return UserHooksOptions{}, fmt.Errorf("invalid hook location %q", opts.Location)
	}
	if err := validateHookWitselfHome(opts.Platform, opts.WitselfHome); err != nil {
		return UserHooksOptions{}, err
	}
	if strings.ContainsAny(opts.Executable, "\x00\r\n") {
		return UserHooksOptions{}, errors.New("hook executable must be a clean absolute path")
	}
	if opts.Platform == "windows" {
		if !windowsPathIsAbs(opts.Executable) || !windowsPathIsAbs(opts.ConfigPath) {
			return UserHooksOptions{}, errors.New("hook executable and config path must be clean absolute paths")
		}
	} else if !filepath.IsAbs(opts.Executable) || filepath.Clean(opts.Executable) != opts.Executable ||
		!filepath.IsAbs(opts.ConfigPath) || filepath.Clean(opts.ConfigPath) != opts.ConfigPath {
		return UserHooksOptions{}, errors.New("hook executable and config path must be clean absolute paths")
	}
	if strings.ContainsAny(opts.ConfigPath, "\x00\r\n") {
		return UserHooksOptions{}, errors.New("hook config path must be a clean absolute path")
	}
	return opts, nil
}

func installOwnedGrokHooks(desired UserHooksOptions, previous *UserHooksOptions, snapshot hookFileSnapshot) (HookMutation, error) {
	desiredRoot, err := exactHookDocument(desired)
	if err != nil {
		return HookMutation{}, err
	}
	if !snapshot.exists {
		if err := writeHookJSONAtomicCAS(desired.ConfigPath, desiredRoot, snapshot); err != nil {
			return HookMutation{}, err
		}
		return HookMutation{Path: desired.ConfigPath, Touched: true}, nil
	}
	current, err := parseHookConfigRoot(desired.ConfigPath, snapshot.raw)
	if err != nil {
		return HookMutation{}, err
	}
	if previous == nil {
		return HookMutation{}, fmt.Errorf("dedicated Grok hook file %s already exists without a durable Witself ownership record; refusing to claim it", desired.ConfigPath)
	}
	exactPrevious, err := exactGrokHookDocument(current, *previous)
	if err != nil {
		return HookMutation{}, err
	}
	if !exactPrevious {
		return HookMutation{}, fmt.Errorf("dedicated Grok hook file %s does not match the exact prior persisted binding", desired.ConfigPath)
	}
	if hookJSONEquivalent(current, desiredRoot) {
		return HookMutation{Path: desired.ConfigPath}, nil
	}
	if err := writeHookJSONAtomicCAS(desired.ConfigPath, desiredRoot, snapshot); err != nil {
		return HookMutation{}, err
	}
	return HookMutation{Path: desired.ConfigPath, Touched: true}, nil
}

func sharedHookMap(root map[string]any, path string) (map[string]any, error) {
	rawHooks, ok := root["hooks"]
	if !ok {
		return map[string]any{}, nil
	}
	hooks, ok := rawHooks.(map[string]any)
	if !ok || hooks == nil {
		return nil, fmt.Errorf("parse %s: hooks must be a JSON object", path)
	}
	for event, value := range hooks {
		if _, ok := value.([]any); !ok {
			return nil, fmt.Errorf("parse %s: hook event %q must be a JSON array", path, event)
		}
	}
	return hooks, nil
}

func exactHookDocument(opts UserHooksOptions) (map[string]any, error) {
	hooks := map[string]any{}
	if err := addExactOwnedHookSet(hooks, opts); err != nil {
		return nil, err
	}
	return map[string]any{"hooks": hooks}, nil
}

func exactGrokHookDocument(root map[string]any, opts UserHooksOptions) (bool, error) {
	expected, err := exactHookDocument(opts)
	if err != nil {
		return false, err
	}
	return hookJSONEquivalent(root, expected), nil
}

func addExactOwnedHookSet(hooks map[string]any, opts UserHooksOptions) error {
	command, commandWindows, err := ownedHookCommands(opts)
	if err != nil {
		return err
	}
	switch opts.Runtime {
	case RuntimeCursor:
		addCursorWitselfHandlers(hooks, opts.Mode, command)
	case RuntimeCodex:
		addWitselfHandlersWithWindowsCommand(hooks, opts.Runtime, opts.Mode, command, commandWindows)
	default:
		addWitselfHandlers(hooks, opts.Runtime, opts.Mode, command)
	}
	return nil
}

func ownedHookCommands(opts UserHooksOptions) (string, string, error) {
	command := shellQuote(opts.Executable) + " transcript hook " +
		hookBindingArgsWithWitselfHome(opts.Runtime, opts.Account, opts.Realm, opts.Agent, opts.Location, opts.WitselfHome)
	if opts.Runtime != RuntimeCodex || opts.Platform != "windows" {
		return command, "", nil
	}
	commandWindows, err := codexWindowsHookCommandWithWitselfHome(
		opts.Executable,
		opts.Runtime,
		opts.Account,
		opts.Realm,
		opts.Agent,
		opts.Location,
		opts.WitselfHome,
	)
	if err != nil {
		return "", "", err
	}
	return command, commandWindows, nil
}

// removeExactOwnedHookSet removes exact top-level entries generated for opts.
// It rejects every marker-bearing entry that is not byte-equivalent JSON to
// the expected event shape. The returned count is the number of exact events
// found and removed.
func removeExactOwnedHookSet(hooks map[string]any, opts UserHooksOptions, rejectForeignMarkers bool) (int, error) {
	expected := map[string]any{}
	if err := addExactOwnedHookSet(expected, opts); err != nil {
		return 0, err
	}
	found := 0
	for event, rawEntries := range hooks {
		entries, ok := rawEntries.([]any)
		if !ok {
			return 0, fmt.Errorf("hook event %q must be a JSON array", event)
		}
		wantEntries, expectedEvent := expected[event].([]any)
		if expectedEvent && len(wantEntries) != 1 {
			return 0, fmt.Errorf("internal hook ownership for event %q is invalid", event)
		}
		kept := make([]any, 0, len(entries))
		eventFound := false
		for _, entry := range entries {
			if expectedEvent && hookJSONEquivalent(entry, wantEntries[0]) {
				if eventFound {
					return 0, fmt.Errorf("hook event %q contains duplicate exact Witself handlers", event)
				}
				eventFound = true
				found++
				continue
			}
			if rejectForeignMarkers && hookValueHasCommandMarker(entry) {
				return 0, fmt.Errorf("hook event %q contains a marker-shaped handler that differs from the persisted executable, identity, home, or event shape", event)
			}
			kept = append(kept, entry)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	return found, nil
}

func hasHookCommandMarker(hooks map[string]any) bool {
	for _, value := range hooks {
		if hookValueHasCommandMarker(value) {
			return true
		}
	}
	return false
}

func hookValueHasCommandMarker(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "command" || key == "commandWindows" {
				if command, ok := child.(string); ok && strings.Contains(command, hookCommandMarker) {
					return true
				}
			}
			if hookValueHasCommandMarker(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if hookValueHasCommandMarker(child) {
				return true
			}
		}
	}
	return false
}

func hookJSONEquivalent(left, right any) bool {
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
}

func cloneHookDocument(root map[string]any) map[string]any {
	raw, _ := json.Marshal(root)
	var clone map[string]any
	_ = json.Unmarshal(raw, &clone)
	if clone == nil {
		clone = map[string]any{}
	}
	return clone
}

func cloneHookMap(hooks map[string]any) map[string]any {
	return cloneHookDocument(hooks)
}
