package transcriptcapture

import (
	"errors"
	"path/filepath"
	"strings"
)

// DSHDedicatedHooksFilename names the exclusive Witself hook document under
// canonical DSH_HOME. The default hooks.json remains a shared operator document.
const DSHDedicatedHooksFilename = "witself-hooks.json"

func isDSHDedicatedHookConfig(opts UserHooksOptions) bool {
	return opts.Runtime == RuntimeDSH && filepath.Base(opts.ConfigPath) == DSHDedicatedHooksFilename
}

// InspectDSHLegacyHooks reports whether foreign hook entries remain after
// subtracting an optional exact prior DSH binding from a wrapped document in
// memory. Bare event maps are supported only without marker-bearing handlers,
// since RemoveOwnedHooks cannot remove bare handlers. A completely absent
// prior-owned set is allowed for interrupted migration recovery. It never writes
// the document or returns its content. This snapshot is not a write lock: callers
// must re-check ownership and content before mutation.
func InspectDSHLegacyHooks(legacyPath string, previous *UserHooksOptions) (bool, error) {
	legacyPath = strings.TrimSpace(legacyPath)
	if !filepath.IsAbs(legacyPath) || filepath.Clean(legacyPath) != legacyPath || strings.ContainsAny(legacyPath, "\x00\r\n") {
		return false, errors.New("legacy DSH hook config path must be a clean absolute path")
	}
	if previous != nil {
		normalized, err := normalizeUserHooksOptions(*previous)
		if err != nil {
			return false, errors.New("inspect legacy DSH hooks: invalid prior ownership binding")
		}
		if normalized.Runtime != RuntimeDSH || normalized.ConfigPath != legacyPath {
			return false, errors.New("inspect legacy DSH hooks: prior ownership must use the same DSH config path")
		}
		previous = &normalized
	}
	snapshot, err := readHookFileSnapshot(legacyPath)
	if err != nil {
		return false, errors.New("inspect legacy DSH hooks: unable to read safe hook snapshot")
	}
	if !snapshot.exists {
		return false, nil
	}
	root, err := parseHookConfigRoot(legacyPath, snapshot.raw)
	if err != nil {
		return false, errors.New("inspect legacy DSH hooks: invalid JSON object")
	}
	if _, wrapped := root["hooks"]; !wrapped {
		return inspectDSHBareLegacyHooks(root)
	}
	hooks, err := sharedHookMap(root, legacyPath)
	if err != nil {
		return false, errors.New("inspect legacy DSH hooks: invalid hook map")
	}
	if previous != nil {
		found, err := removeExactOwnedHookSet(hooks, *previous, true)
		if err != nil {
			return false, errors.New("inspect legacy DSH hooks: handlers differ from the exact prior ownership binding")
		}
		if found != 0 && found != len(hookEvents(previous.Runtime, previous.Mode)) {
			return false, errors.New("inspect legacy DSH hooks: incomplete prior-owned handler set")
		}
	}
	if hasHookCommandMarker(hooks) {
		return false, errors.New("inspect legacy DSH hooks: unclaimed marker-shaped handlers")
	}
	for _, entries := range hooks {
		if len(entries.([]any)) != 0 {
			return true, nil
		}
	}
	return false, nil
}

func inspectDSHBareLegacyHooks(root map[string]any) (bool, error) {
	remaining := false
	// These are the bare event keys supported by dsh-hooks-claude-code.
	// Other root fields are metadata, even when their values are arrays.
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "SubagentStart", "SubagentStop"} {
		value, exists := root[event]
		if !exists {
			continue
		}
		entries, ok := value.([]any)
		if !ok {
			return false, errors.New("inspect legacy DSH hooks: invalid bare hook event")
		}
		// Never subtract even an exact prior set: generic removal leaves bare
		// handlers intact, so a compatibility bridge could execute them twice.
		if hookValueHasCommandMarker(entries) {
			return false, errors.New("inspect legacy DSH hooks: marker-shaped bare handlers cannot be migrated")
		}
		remaining = remaining || len(entries) != 0
	}
	return remaining, nil
}
