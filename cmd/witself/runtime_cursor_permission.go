package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const cursorWitselfMCPPermission = "Mcp(witself:*)"

type cursorCLIConfigState struct {
	path    string
	existed bool
	raw     []byte
	mode    os.FileMode
	root    map[string]any
}

type cursorPermissionMutationKind uint8

const (
	cursorPermissionAdded cursorPermissionMutationKind = iota + 1
	cursorPermissionRemoved
)

// cursorPermissionMutation records only the value Witself changed. That lets a
// failed install or uninstall undo its own change without replacing unrelated
// settings that Cursor or the user wrote in the meantime.
type cursorPermissionMutation struct {
	kind               cursorPermissionMutationKind
	path               string
	before             cursorCLIConfigState
	afterRaw           []byte
	afterMode          os.FileMode
	permissionIndex    int
	permissionCount    int
	permissionsExisted bool
	allowExisted       bool
}

// cursorCLIConfigSnapshot captures the config used by install ownership
// decisions. Mutations re-read the file immediately before merging; restore
// either restores the captured bytes when our output is still untouched or
// reverses only our permission-list mutation when the file changed afterward.
type cursorCLIConfigSnapshot struct {
	configuredPath string
	mutation       *cursorPermissionMutation
}

func snapshotCursorCLIConfig() (cursorCLIConfigSnapshot, error) {
	root, err := cursorConfigRoot()
	if err != nil {
		return cursorCLIConfigSnapshot{}, err
	}
	configuredPath := filepath.Join(root, "cli-config.json")
	state, err := readCursorCLIConfig(configuredPath)
	if err != nil {
		return cursorCLIConfigSnapshot{}, err
	}
	if _, err := cursorPermissionAllowList(state.path, state.root, false); err != nil {
		return cursorCLIConfigSnapshot{}, err
	}
	return cursorCLIConfigSnapshot{
		configuredPath: configuredPath,
	}, nil
}

func (s *cursorCLIConfigSnapshot) ensureWitselfMCPPermission() (bool, error) {
	state, err := readCursorCLIConfig(s.configuredPath)
	if err != nil {
		return false, err
	}
	allow, err := cursorPermissionAllowList(state.path, state.root, false)
	if err != nil {
		return false, err
	}
	if cursorPermissionCount(allow) != 0 {
		return false, nil
	}

	permissionsValue, permissionsExisted := state.root["permissions"]
	allowExisted := false
	if permissions, ok := permissionsValue.(map[string]any); ok {
		_, allowExisted = permissions["allow"]
	}
	allow, err = cursorPermissionAllowList(state.path, state.root, true)
	if err != nil {
		return false, err
	}
	permissions := state.root["permissions"].(map[string]any)
	permissionIndex := len(allow)
	permissions["allow"] = append(allow, cursorWitselfMCPPermission)
	afterRaw, err := marshalCursorCLIConfig(state.root)
	if err != nil {
		return false, err
	}
	afterMode := cursorCLIConfigWriteMode(state)
	if err := writeFileAtomic(state.path, afterRaw, afterMode); err != nil {
		return false, err
	}
	s.mutation = &cursorPermissionMutation{
		kind:               cursorPermissionAdded,
		path:               state.path,
		before:             state,
		afterRaw:           bytes.Clone(afterRaw),
		afterMode:          afterMode,
		permissionIndex:    permissionIndex,
		permissionCount:    1,
		permissionsExisted: permissionsExisted,
		allowExisted:       allowExisted,
	}
	return true, nil
}

func (s *cursorCLIConfigSnapshot) removeWitselfMCPPermission() (bool, error) {
	state, err := readCursorCLIConfig(s.configuredPath)
	if err != nil {
		return false, err
	}
	allow, err := cursorPermissionAllowList(state.path, state.root, false)
	if err != nil {
		return false, err
	}
	filtered := make([]any, 0, len(allow))
	firstIndex := -1
	for index, value := range allow {
		if value == cursorWitselfMCPPermission && firstIndex == -1 {
			firstIndex = index
			continue
		}
		filtered = append(filtered, value)
	}
	if firstIndex == -1 {
		return false, nil
	}
	permissionCountBefore := cursorPermissionCount(allow)
	permissions := state.root["permissions"].(map[string]any)
	if len(filtered) == 0 {
		delete(permissions, "allow")
	} else {
		permissions["allow"] = filtered
	}
	if len(permissions) == 0 {
		delete(state.root, "permissions")
	}
	afterRaw, err := marshalCursorCLIConfig(state.root)
	if err != nil {
		return false, err
	}
	afterMode := cursorCLIConfigWriteMode(state)
	if err := writeFileAtomic(state.path, afterRaw, afterMode); err != nil {
		return false, err
	}
	s.mutation = &cursorPermissionMutation{
		kind:            cursorPermissionRemoved,
		path:            state.path,
		before:          state,
		afterRaw:        bytes.Clone(afterRaw),
		afterMode:       afterMode,
		permissionIndex: firstIndex,
		permissionCount: permissionCountBefore,
	}
	return true, nil
}

func (s cursorCLIConfigSnapshot) restore() error {
	mutation := s.mutation
	if mutation == nil {
		return nil
	}
	current, err := readCursorCLIConfigTarget(mutation.path)
	if err != nil {
		return err
	}
	if current.existed && bytes.Equal(current.raw, mutation.afterRaw) && current.mode == mutation.afterMode {
		if mutation.before.existed {
			return writeFileAtomic(mutation.path, mutation.before.raw, mutation.before.mode)
		}
		if err := os.Remove(mutation.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}

	switch mutation.kind {
	case cursorPermissionAdded:
		return restoreAddedCursorPermission(current, mutation)
	case cursorPermissionRemoved:
		return restoreRemovedCursorPermission(current, mutation)
	default:
		return errors.New("restore Cursor CLI permissions: unknown mutation")
	}
}

func restoreAddedCursorPermission(current cursorCLIConfigState, mutation *cursorPermissionMutation) error {
	if !current.existed {
		// The permission no longer exists, so our addition is already undone.
		return nil
	}
	allow, err := cursorPermissionAllowList(current.path, current.root, false)
	if err != nil {
		return err
	}
	indexes := cursorPermissionIndexes(allow)
	if len(indexes) == 0 {
		return nil
	}
	if len(indexes) > 1 {
		return fmt.Errorf("refuse to roll back %s: Witself MCP permission became ambiguous", current.path)
	}
	removeIndex := indexes[0]
	filtered := append([]any(nil), allow[:removeIndex]...)
	filtered = append(filtered, allow[removeIndex+1:]...)
	permissions, _ := current.root["permissions"].(map[string]any)
	if len(filtered) == 0 && !mutation.allowExisted {
		delete(permissions, "allow")
	} else {
		permissions["allow"] = filtered
	}
	if len(permissions) == 0 && !mutation.permissionsExisted {
		delete(current.root, "permissions")
	}
	if len(current.root) == 0 && !mutation.before.existed {
		if err := os.Remove(current.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeCursorCLIConfig(current)
}

func restoreRemovedCursorPermission(current cursorCLIConfigState, mutation *cursorPermissionMutation) error {
	if !current.existed {
		return fmt.Errorf("refuse to roll back %s: Cursor CLI config was removed after Witself updated it", current.path)
	}
	allow, err := cursorPermissionAllowList(current.path, current.root, true)
	if err != nil {
		return err
	}
	missing := mutation.permissionCount - cursorPermissionCount(allow)
	if missing <= 0 {
		return nil
	}
	insertAt := mutation.permissionIndex
	if insertAt < 0 || insertAt > len(allow) {
		insertAt = len(allow)
	}
	restored := make([]any, 0, len(allow)+missing)
	restored = append(restored, allow[:insertAt]...)
	for range missing {
		restored = append(restored, cursorWitselfMCPPermission)
	}
	restored = append(restored, allow[insertAt:]...)
	permissions := current.root["permissions"].(map[string]any)
	permissions["allow"] = restored
	return writeCursorCLIConfig(current)
}

func readCursorCLIConfig(configuredPath string) (cursorCLIConfigState, error) {
	targetPath, err := resolveCursorCLIConfigTarget(configuredPath)
	if err != nil {
		return cursorCLIConfigState{}, err
	}
	return readCursorCLIConfigTarget(targetPath)
}

func resolveCursorCLIConfigTarget(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	return target, nil
}

func readCursorCLIConfigTarget(path string) (cursorCLIConfigState, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cursorCLIConfigState{path: path, mode: 0o600, root: map[string]any{}}, nil
	}
	if err != nil {
		return cursorCLIConfigState{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return cursorCLIConfigState{}, err
	}
	root, err := parseJSONObject(path, raw)
	if err != nil {
		return cursorCLIConfigState{}, err
	}
	return cursorCLIConfigState{
		path: path, existed: true, raw: raw, mode: info.Mode().Perm(), root: root,
	}, nil
}

func marshalCursorCLIConfig(root map[string]any) ([]byte, error) {
	raw, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func writeCursorCLIConfig(state cursorCLIConfigState) error {
	raw, err := marshalCursorCLIConfig(state.root)
	if err != nil {
		return err
	}
	return writeFileAtomic(state.path, raw, cursorCLIConfigWriteMode(state))
}

func cursorCLIConfigWriteMode(state cursorCLIConfigState) os.FileMode {
	if state.existed && state.mode != 0 {
		return state.mode
	}
	return 0o600
}

func cursorPermissionIndexes(allow []any) []int {
	indexes := make([]int, 0, 1)
	for index, value := range allow {
		if value == cursorWitselfMCPPermission {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

func cursorPermissionCount(allow []any) int {
	return len(cursorPermissionIndexes(allow))
}

func cursorPermissionAllowList(path string, root map[string]any, create bool) ([]any, error) {
	permissionsValue, exists := root["permissions"]
	if !exists {
		if !create {
			return nil, nil
		}
		permissions := map[string]any{}
		root["permissions"] = permissions
		return nil, nil
	}
	permissions, ok := permissionsValue.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parse %s: permissions must be an object", path)
	}
	allowValue, exists := permissions["allow"]
	if !exists {
		return nil, nil
	}
	allow, ok := allowValue.([]any)
	if !ok {
		return nil, fmt.Errorf("parse %s: permissions.allow must be an array", path)
	}
	for _, value := range allow {
		if _, ok := value.(string); !ok {
			return nil, fmt.Errorf("parse %s: permissions.allow must contain only strings", path)
		}
	}
	return allow, nil
}

func cursorConfigManagesWitselfMCPPermission(managed []string) bool {
	for _, permission := range managed {
		if permission == cursorWitselfMCPPermission {
			return true
		}
	}
	return false
}

func addManagedCursorMCPPermission(managed []string) []string {
	if cursorConfigManagesWitselfMCPPermission(managed) {
		return managed
	}
	return append(managed, cursorWitselfMCPPermission)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".witself-config-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if mode == 0 {
		mode = 0o600
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
