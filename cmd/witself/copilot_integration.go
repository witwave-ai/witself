package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const (
	copilotMCPServerNamePrefix = "witself-managed-"
	copilotMCPServerHashLength = 24
	copilotCLIOutputLimit      = 1024 * 1024
	copilotMCPConfigReadLimit  = 1024 * 1024
	copilotCLIReadTimeout      = 30 * time.Second
	copilotCLIMutationTimeout  = 45 * time.Second
	copilotCLIWaitDelay        = 2 * time.Second
)

type copilotMCPBinding struct {
	Tools   []string          `json:"tools"`
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Source  string            `json:"source,omitempty"`
	Enabled *bool             `json:"enabled,omitempty"`
}

type copilotMCPList struct {
	Servers map[string]json.RawMessage `json:"mcpServers"`
}

// copilotMCPConfigSnapshot is a semantic projection of every persisted field
// the current operation does not own. The exact target binding is kept
// separately so add/remove may change only that one collision-resistant key.
type copilotMCPConfigSnapshot struct {
	path         string
	targetName   string
	rootFields   map[string]string
	siblings     map[string]string
	target       copilotMCPBinding
	targetExists bool
	exists       bool
	mode         os.FileMode
	raw          []byte
	fileInfo     os.FileInfo
}

// The indirection keeps command construction and JSON validation production
// code while allowing focused tests to model the official Copilot CLI without
// writing to a real user profile.
var runCopilotCLI = runCopilotCLICommand

func captureCopilotMCPEnvironment() (map[string]string, error) {
	witselfHome, err := local.Home()
	if err != nil {
		return nil, fmt.Errorf("resolve WITSELF_HOME: %w", err)
	}
	witselfHome, err = cleanCopilotAbsolutePath("WITSELF_HOME", witselfHome)
	if err != nil {
		return nil, err
	}
	return map[string]string{"WITSELF_HOME": witselfHome}, nil
}

func configureCopilotBinding(cfg *transcriptcapture.Config, runtimeCLI, witselfExecutable string) error {
	if cfg == nil {
		return errors.New("copilot integration config is required")
	}
	if err := validateCopilotRuntimeVersion(cfg.RuntimeVersion); err != nil {
		return err
	}
	var err error
	runtimeCLI, err = cleanCopilotInvocationPath("GitHub Copilot CLI", runtimeCLI)
	if err != nil {
		return err
	}
	witselfExecutable, err = cleanCopilotInvocationPath("Witself executable", witselfExecutable)
	if err != nil {
		return err
	}
	root, err := currentCopilotConfigRoot()
	if err != nil {
		return err
	}
	environment, err := captureCopilotMCPEnvironment()
	if err != nil {
		return err
	}

	cfg.RuntimeCLICommand = runtimeCLI
	cfg.MCPCommand = witselfExecutable
	cfg.MCPEnvironment = environment
	cfg.RuntimeConfigRoot = root
	cfg.RuntimeMCPConfigPath = filepath.Join(root, "mcp-config.json")
	return nil
}

func validateCopilotRuntimeVersion(version string) error {
	const minimum = "1.0.73"
	match := semanticVersionPattern.FindStringSubmatch(strings.TrimSpace(version))
	if len(match) != 2 {
		return fmt.Errorf("GitHub Copilot CLI %s or newer is required; could not parse version %q", minimum, version)
	}
	core := match[1]
	prerelease := ""
	if index := strings.IndexAny(core, "-+"); index >= 0 {
		if core[index] == '-' {
			prerelease = core[index+1:]
		}
		core = core[:index]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return fmt.Errorf("GitHub Copilot CLI %s or newer is required; found %q", minimum, version)
	}
	numbers := make([]int, 3)
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return fmt.Errorf("GitHub Copilot CLI %s or newer is required; found %q", minimum, version)
		}
		numbers[index] = value
	}
	minimumNumbers := [...]int{1, 0, 73}
	for index := range numbers {
		if numbers[index] > minimumNumbers[index] {
			return nil
		}
		if numbers[index] < minimumNumbers[index] {
			return fmt.Errorf("GitHub Copilot CLI %s or newer is required; found %q", minimum, version)
		}
	}
	if prerelease != "" {
		return fmt.Errorf("GitHub Copilot CLI %s or newer is required; found prerelease %q", minimum, version)
	}
	return nil
}

func copilotMCPServerName(cfg transcriptcapture.Config) (string, error) {
	parts := []struct {
		label string
		value string
	}{
		{"account id", cfg.AccountID},
		{"realm id", cfg.RealmID},
		{"agent id", cfg.AgentID},
		{"location id", cfg.Location.ID},
	}
	var material strings.Builder
	material.WriteString("witself-copilot-mcp-binding-v1\x00")
	for _, part := range parts {
		if part.value == "" || strings.TrimSpace(part.value) != part.value ||
			len(part.value) > 1024 || strings.ContainsAny(part.value, "\x00\r\n") {
			return "", fmt.Errorf("copilot MCP server name requires a stable %s", part.label)
		}
		material.WriteString(part.label)
		material.WriteByte(0)
		material.WriteString(part.value)
		material.WriteByte(0)
	}
	digest := sha256.Sum256([]byte(material.String()))
	return copilotMCPServerNamePrefix + hex.EncodeToString(digest[:])[:copilotMCPServerHashLength], nil
}

func copilotMCPBindingFromConfig(executable string, cfg transcriptcapture.Config) (string, copilotMCPBinding, error) {
	name, err := copilotMCPServerName(cfg)
	if err != nil {
		return "", copilotMCPBinding{}, err
	}
	if strings.TrimSpace(cfg.MCPCommand) != "" {
		executable = cfg.MCPCommand
	}
	executable, err = cleanCopilotCommandPath("Copilot MCP command", executable)
	if err != nil {
		return "", copilotMCPBinding{}, err
	}
	if err := validateCopilotBindingEnvironment(cfg.MCPEnvironment); err != nil {
		return "", copilotMCPBinding{}, err
	}
	account := strings.TrimSpace(cfg.Account)
	if account == "" {
		account = "default"
	}
	realm := strings.TrimSpace(cfg.Realm)
	if realm == "" {
		realm = "default"
	}
	agent := strings.TrimSpace(cfg.Agent)
	if agent == "" {
		agent = strings.TrimSpace(cfg.AgentName)
	}
	if agent == "" {
		return "", copilotMCPBinding{}, errors.New("installed Copilot integration has no agent name")
	}
	serveArgs := runtimeMCPServeArgs(
		transcriptcapture.RuntimeCopilot,
		executable,
		account,
		realm,
		agent,
		cfg.Location.Name,
	)
	return name, copilotMCPBinding{
		Tools:   []string{"*"},
		Type:    "local",
		Command: serveArgs[0],
		Args:    append([]string(nil), serveArgs[1:]...),
		Env:     cloneCopilotEnvironment(cfg.MCPEnvironment),
	}, nil
}

func validateCopilotBindingEnvironment(environment map[string]string) error {
	if len(environment) != 1 {
		return errors.New("copilot MCP environment must contain only WITSELF_HOME")
	}
	home, exists := environment["WITSELF_HOME"]
	if !exists || home == "" || strings.TrimSpace(home) != home ||
		len(home) > 4096 || strings.ContainsAny(home, "\x00\r\n") ||
		!filepath.IsAbs(home) || filepath.Clean(home) != home {
		return errors.New("copilot MCP environment WITSELF_HOME must be a clean absolute path")
	}
	return nil
}

func cloneCopilotEnvironment(environment map[string]string) map[string]string {
	cloned := make(map[string]string, len(environment))
	for key, value := range environment {
		cloned[key] = value
	}
	return cloned
}

func equalCopilotMCPBinding(left, right copilotMCPBinding) bool {
	if !copilotMCPTypeEquivalent(left.Type, right.Type) ||
		left.Command != right.Command ||
		!equalCopilotStrings(left.Tools, right.Tools) ||
		!equalCopilotStrings(left.Args, right.Args) ||
		!equalCopilotEnvironment(left.Env, right.Env) {
		return false
	}
	return true
}

func copilotMCPTypeEquivalent(left, right string) bool {
	left = strings.ToLower(strings.TrimSpace(left))
	right = strings.ToLower(strings.TrimSpace(right))
	if left != "local" && left != "stdio" {
		return false
	}
	if right != "local" && right != "stdio" {
		return false
	}
	return true
}

func equalCopilotStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalCopilotEnvironment(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func inspectCopilotMCP(runtimeCLI string, cfg transcriptcapture.Config) (copilotMCPBinding, bool, error) {
	binding, exists, _, err := inspectCopilotMCPState(runtimeCLI, cfg)
	return binding, exists, err
}

func inspectCopilotMCPState(runtimeCLI string, cfg transcriptcapture.Config) (copilotMCPBinding, bool, copilotMCPConfigSnapshot, error) {
	name, _, err := copilotMCPBindingFromConfig(cfg.MCPCommand, cfg)
	if err != nil {
		return copilotMCPBinding{}, false, copilotMCPConfigSnapshot{}, err
	}
	persisted, err := readCopilotMCPConfigSnapshot(cfg.RuntimeMCPConfigPath, name)
	if err != nil {
		return copilotMCPBinding{}, false, copilotMCPConfigSnapshot{}, err
	}
	raw, err := runCopilotCLI(runtimeCLI, cfg.RuntimeConfigRoot, copilotCLIReadTimeout, "mcp", "list", "--json")
	if err != nil {
		return copilotMCPBinding{}, false, copilotMCPConfigSnapshot{},
			unavailableIntegrationTopology(fmt.Errorf("list GitHub Copilot MCP servers: %w", err))
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return copilotMCPBinding{}, false, copilotMCPConfigSnapshot{}, fmt.Errorf("parse GitHub Copilot MCP server list: %w", err)
	}
	var list copilotMCPList
	if err := json.Unmarshal(raw, &list); err != nil {
		return copilotMCPBinding{}, false, copilotMCPConfigSnapshot{}, fmt.Errorf("parse GitHub Copilot MCP server list: %w", err)
	}
	if list.Servers == nil {
		return copilotMCPBinding{}, false, copilotMCPConfigSnapshot{}, errors.New("GitHub Copilot MCP server list omitted mcpServers")
	}
	for candidate := range list.Servers {
		if candidate != name && strings.EqualFold(candidate, name) {
			return copilotMCPBinding{}, false, copilotMCPConfigSnapshot{}, fmt.Errorf("GitHub Copilot MCP server %q collides case-insensitively with Witself-managed name %q", candidate, name)
		}
	}
	definition, exists := list.Servers[name]
	if exists != persisted.targetExists {
		return copilotMCPBinding{}, false, copilotMCPConfigSnapshot{}, fmt.Errorf(
			"GitHub Copilot CLI and persisted %s disagree about MCP server %s",
			filepath.Base(cfg.RuntimeMCPConfigPath), name,
		)
	}
	if !exists {
		return copilotMCPBinding{}, false, persisted, nil
	}
	binding, err := parseCopilotMCPBinding(definition)
	if err != nil {
		return copilotMCPBinding{}, false, copilotMCPConfigSnapshot{}, fmt.Errorf("parse GitHub Copilot MCP server %s: %w", name, err)
	}
	if !equalCopilotMCPBinding(binding, persisted.target) {
		return copilotMCPBinding{}, false, copilotMCPConfigSnapshot{}, fmt.Errorf(
			"GitHub Copilot CLI and persisted %s disagree about the exact MCP server %s binding",
			filepath.Base(cfg.RuntimeMCPConfigPath), name,
		)
	}
	return binding, true, persisted, nil
}

func readCopilotMCPConfigSnapshot(path, targetName string) (copilotMCPConfigSnapshot, error) {
	snapshot := copilotMCPConfigSnapshot{
		path: path, targetName: targetName,
		rootFields: map[string]string{}, siblings: map[string]string{},
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return copilotMCPConfigSnapshot{}, fmt.Errorf("inspect GitHub Copilot MCP config %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return copilotMCPConfigSnapshot{}, fmt.Errorf("GitHub Copilot MCP config %s is a symlink; refusing an ambiguous registry", path)
	}
	if !info.Mode().IsRegular() {
		return copilotMCPConfigSnapshot{}, fmt.Errorf("GitHub Copilot MCP config %s is not a regular file", path)
	}
	if info.Size() > copilotMCPConfigReadLimit {
		return copilotMCPConfigSnapshot{}, fmt.Errorf("GitHub Copilot MCP config %s exceeds %d bytes", path, copilotMCPConfigReadLimit)
	}
	snapshot.exists = true
	snapshot.mode = info.Mode()
	file, err := os.Open(path)
	if err != nil {
		return copilotMCPConfigSnapshot{}, fmt.Errorf("open GitHub Copilot MCP config %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return copilotMCPConfigSnapshot{}, fmt.Errorf("inspect open GitHub Copilot MCP config %s: %w", path, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return copilotMCPConfigSnapshot{}, fmt.Errorf("GitHub Copilot MCP config %s changed while it was opened", path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, copilotMCPConfigReadLimit+1))
	if err != nil {
		return copilotMCPConfigSnapshot{}, fmt.Errorf("read GitHub Copilot MCP config %s: %w", path, err)
	}
	if len(raw) > copilotMCPConfigReadLimit {
		return copilotMCPConfigSnapshot{}, fmt.Errorf("GitHub Copilot MCP config %s exceeds %d bytes", path, copilotMCPConfigReadLimit)
	}
	readInfo, err := file.Stat()
	if err != nil {
		return copilotMCPConfigSnapshot{}, fmt.Errorf("reinspect open GitHub Copilot MCP config %s: %w", path, err)
	}
	currentInfo, err := os.Lstat(path)
	if err != nil || currentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, currentInfo) ||
		!os.SameFile(openedInfo, readInfo) || openedInfo.Size() != readInfo.Size() ||
		!openedInfo.ModTime().Equal(readInfo.ModTime()) {
		if err == nil {
			err = errors.New("file identity or contents changed")
		}
		return copilotMCPConfigSnapshot{}, fmt.Errorf("GitHub Copilot MCP config %s changed while it was read: %w", path, err)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return copilotMCPConfigSnapshot{}, fmt.Errorf("parse GitHub Copilot MCP config %s: %w", path, err)
	}
	snapshot.raw = bytes.Clone(raw)
	snapshot.fileInfo = openedInfo
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		if err == nil {
			err = errors.New("root must be a JSON object")
		}
		return copilotMCPConfigSnapshot{}, fmt.Errorf("parse GitHub Copilot MCP config %s: %w", path, err)
	}
	for field, value := range root {
		if field == "mcpServers" {
			continue
		}
		canonical, err := canonicalCopilotJSON(value)
		if err != nil {
			return copilotMCPConfigSnapshot{}, fmt.Errorf("parse GitHub Copilot MCP config field %s: %w", field, err)
		}
		snapshot.rootFields[field] = canonical
	}
	serversRaw, exists := root["mcpServers"]
	if !exists {
		return snapshot, nil
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(serversRaw, &servers); err != nil || servers == nil {
		if err == nil {
			err = errors.New("mcpServers must be a JSON object")
		}
		return copilotMCPConfigSnapshot{}, fmt.Errorf("parse GitHub Copilot MCP config %s: %w", path, err)
	}
	for name, definition := range servers {
		if name != targetName && strings.EqualFold(name, targetName) {
			return copilotMCPConfigSnapshot{}, fmt.Errorf("persisted GitHub Copilot MCP server %q collides case-insensitively with Witself-managed name %q", name, targetName)
		}
		if name == targetName {
			binding, err := parseCopilotMCPBinding(definition)
			if err != nil {
				return copilotMCPConfigSnapshot{}, fmt.Errorf("parse persisted GitHub Copilot MCP server %s: %w", name, err)
			}
			snapshot.target = binding
			snapshot.targetExists = true
			continue
		}
		canonical, err := canonicalCopilotJSON(definition)
		if err != nil {
			return copilotMCPConfigSnapshot{}, fmt.Errorf("parse persisted GitHub Copilot MCP sibling %s: %w", name, err)
		}
		snapshot.siblings[name] = canonical
	}
	return snapshot, nil
}

func canonicalCopilotJSON(raw []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", errors.New("multiple JSON values are not allowed")
		}
		return "", err
	}
	var canonical strings.Builder
	if err := writeCopilotSemanticJSON(&canonical, value); err != nil {
		return "", err
	}
	return canonical.String(), nil
}

func writeCopilotSemanticJSON(output *strings.Builder, value any) error {
	switch typed := value.(type) {
	case nil:
		output.WriteByte('n')
	case bool:
		if typed {
			output.WriteString("b1")
		} else {
			output.WriteString("b0")
		}
	case string:
		output.WriteByte('s')
		output.WriteString(strconv.Itoa(len(typed)))
		output.WriteByte(':')
		output.WriteString(typed)
	case json.Number:
		rational, ok := new(big.Rat).SetString(string(typed))
		if !ok {
			return fmt.Errorf("invalid JSON number %q", typed)
		}
		number := rational.RatString()
		output.WriteByte('d')
		output.WriteString(strconv.Itoa(len(number)))
		output.WriteByte(':')
		output.WriteString(number)
	case []any:
		output.WriteByte('a')
		output.WriteString(strconv.Itoa(len(typed)))
		output.WriteByte(':')
		for _, item := range typed {
			if err := writeCopilotSemanticJSON(output, item); err != nil {
				return err
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('o')
		output.WriteString(strconv.Itoa(len(keys)))
		output.WriteByte(':')
		for _, key := range keys {
			if err := writeCopilotSemanticJSON(output, key); err != nil {
				return err
			}
			if err := writeCopilotSemanticJSON(output, typed[key]); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported decoded JSON value %T", value)
	}
	return nil
}

func verifyCopilotNonTargetPreservation(before, after copilotMCPConfigSnapshot) error {
	if before.targetName != after.targetName || before.path != after.path ||
		!equalCopilotSemanticFields(before.rootFields, after.rootFields) ||
		!equalCopilotSemanticFields(before.siblings, after.siblings) {
		return errors.New("GitHub Copilot changed non-target root fields or sibling MCP servers during the Witself mutation")
	}
	return nil
}

func copilotMCPConfigSnapshotMatchesExact(left, right copilotMCPConfigSnapshot) bool {
	if left.path != right.path || left.targetName != right.targetName ||
		left.exists != right.exists || left.targetExists != right.targetExists ||
		left.mode.Perm() != right.mode.Perm() || !bytes.Equal(left.raw, right.raw) ||
		!equalCopilotSemanticFields(left.rootFields, right.rootFields) ||
		!equalCopilotSemanticFields(left.siblings, right.siblings) {
		return false
	}
	if left.exists &&
		(left.fileInfo == nil || right.fileInfo == nil || !os.SameFile(left.fileInfo, right.fileInfo)) {
		return false
	}
	if left.targetExists && !equalCopilotMCPBinding(left.target, right.target) {
		return false
	}
	return true
}

func verifyCopilotMutationPermissions(after copilotMCPConfigSnapshot) error {
	if after.exists && !integrationFileModeMatches(after.mode, 0o600) {
		return fmt.Errorf("GitHub Copilot MCP config permissions are %04o after mutation; want 0600", after.mode.Perm())
	}
	return nil
}

func equalCopilotSemanticFields(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func parseCopilotMCPBinding(raw []byte) (copilotMCPBinding, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return copilotMCPBinding{}, err
	}
	for field := range fields {
		switch field {
		case "tools", "type", "command", "args", "env", "source", "enabled":
		default:
			return copilotMCPBinding{}, fmt.Errorf("non-standard field %q; refusing to modify it", field)
		}
	}
	for _, required := range []string{"tools", "type", "command", "args", "env"} {
		if fields[required] == nil {
			return copilotMCPBinding{}, fmt.Errorf("required field %q is missing", required)
		}
	}
	var binding copilotMCPBinding
	if err := json.Unmarshal(raw, &binding); err != nil {
		return copilotMCPBinding{}, err
	}
	if binding.Type != "local" && binding.Type != "stdio" {
		return copilotMCPBinding{}, fmt.Errorf("unsupported MCP type %q", binding.Type)
	}
	if binding.Command == "" || !filepath.IsAbs(binding.Command) || filepath.Clean(binding.Command) != binding.Command {
		return copilotMCPBinding{}, errors.New("command must be a clean absolute path")
	}
	if binding.Args == nil || !equalCopilotStrings(binding.Tools, []string{"*"}) {
		return copilotMCPBinding{}, errors.New("tools or args do not match a Witself-managed local server")
	}
	if err := validateCopilotBindingEnvironment(binding.Env); err != nil {
		return copilotMCPBinding{}, err
	}
	if binding.Source != "" && binding.Source != "user" {
		return copilotMCPBinding{}, fmt.Errorf("MCP source %q is not the user registry", binding.Source)
	}
	if binding.Enabled != nil && !*binding.Enabled {
		return copilotMCPBinding{}, errors.New("MCP server is disabled")
	}
	return binding, nil
}

type copilotMCPInstallPlan struct {
	desired          transcriptcapture.Config
	desiredName      string
	desiredBinding   copilotMCPBinding
	expected         copilotMCPConfigSnapshot
	registerRequired bool
}

// prepareCopilotMCPInstallPlan permits replacement only when the live
// definition exactly matches the prior durable integration. It returns an
// exact persisted-registry snapshot that register must revalidate immediately
// before invoking the provider CLI.
func prepareCopilotMCPInstallPlan(runtimeCLI, executable string, desired transcriptcapture.Config, previous *transcriptcapture.Config) (copilotMCPInstallPlan, bool, error) {
	if err := validateCopilotCLISelection(runtimeCLI, desired); err != nil {
		return copilotMCPInstallPlan{}, false, err
	}
	desiredName, desiredBinding, err := copilotMCPBindingFromConfig(executable, desired)
	if err != nil {
		return copilotMCPInstallPlan{}, false, err
	}
	desiredCurrent, desiredExists, desiredSnapshot, err := inspectCopilotMCPState(runtimeCLI, desired)
	if err != nil {
		return copilotMCPInstallPlan{}, false, err
	}
	plan := copilotMCPInstallPlan{
		desired: desired, desiredName: desiredName, desiredBinding: desiredBinding,
		expected: desiredSnapshot, registerRequired: true,
	}
	if previous == nil {
		if desiredExists {
			return copilotMCPInstallPlan{}, false, fmt.Errorf("GitHub Copilot MCP server %s exists without a Witself integration record; refusing to claim or replace it", desiredName)
		}
		return plan, false, nil
	}
	if err := validateCopilotPreviousSelection(runtimeCLI, desired, *previous); err != nil {
		return copilotMCPInstallPlan{}, false, err
	}
	previousName, previousBinding, err := copilotMCPBindingFromConfig(previous.MCPCommand, *previous)
	if err != nil {
		return copilotMCPInstallPlan{}, false, err
	}
	if previousName == desiredName {
		if !desiredExists {
			return plan, false, nil
		}
		if equalCopilotMCPBinding(desiredCurrent, desiredBinding) {
			if !equalCopilotMCPBinding(previousBinding, desiredBinding) {
				return copilotMCPInstallPlan{}, false, fmt.Errorf("GitHub Copilot MCP server %s matches the requested binding but not the prior durable integration; refusing to claim an unjournaled interrupted rebind", desiredName)
			}
			plan.registerRequired = false
			return plan, false, nil
		}
		if !equalCopilotMCPBinding(desiredCurrent, previousBinding) {
			return copilotMCPInstallPlan{}, false, fmt.Errorf("GitHub Copilot MCP server %s differs from both the prior and requested bindings; refusing to replace it", desiredName)
		}
		touched, err := unregisterCopilotMCPWithSnapshot(runtimeCLI, previous, &desiredSnapshot)
		if err != nil {
			return copilotMCPInstallPlan{}, touched, err
		}
		_, exists, expected, err := inspectCopilotMCPState(runtimeCLI, desired)
		if err != nil {
			return copilotMCPInstallPlan{}, touched, err
		}
		if exists {
			return copilotMCPInstallPlan{}, touched, &providerMutationUncertainError{err: fmt.Errorf("GitHub Copilot MCP server %s reappeared after removal; refusing unsafe recovery", desiredName)}
		}
		plan.expected = expected
		return plan, touched, nil
	}

	previousCurrent, previousExists, previousSnapshot, err := inspectCopilotMCPState(runtimeCLI, *previous)
	if err != nil {
		return copilotMCPInstallPlan{}, false, err
	}
	if desiredExists {
		if previousExists {
			return copilotMCPInstallPlan{}, false, fmt.Errorf("GitHub Copilot MCP servers %s and %s both exist during rebind recovery; refusing to choose or remove either", previousName, desiredName)
		}
		if equalCopilotMCPBinding(desiredCurrent, desiredBinding) {
			return copilotMCPInstallPlan{}, false, fmt.Errorf("GitHub Copilot MCP server %s matches the requested binding while prior server %s is absent, but no active recovery journal authorizes claiming it", desiredName, previousName)
		}
		return copilotMCPInstallPlan{}, false, fmt.Errorf("GitHub Copilot MCP server %s differs from the interrupted requested binding; refusing to claim or replace it", desiredName)
	}
	if !previousExists {
		return copilotMCPInstallPlan{}, false, fmt.Errorf("prior GitHub Copilot MCP server %s is missing and requested server %s is absent; refusing an unjournaled rebind", previousName, desiredName)
	}
	if !equalCopilotMCPBinding(previousCurrent, previousBinding) {
		return copilotMCPInstallPlan{}, false, fmt.Errorf("GitHub Copilot MCP server %s no longer matches the prior durable binding; refusing to replace it", previousName)
	}
	touched, err := unregisterCopilotMCPWithSnapshot(runtimeCLI, previous, &previousSnapshot)
	if err != nil {
		return copilotMCPInstallPlan{}, touched, err
	}
	_, exists, expected, err := inspectCopilotMCPState(runtimeCLI, desired)
	if err != nil {
		return copilotMCPInstallPlan{}, touched, err
	}
	if exists {
		return copilotMCPInstallPlan{}, touched, &providerMutationUncertainError{err: fmt.Errorf("GitHub Copilot MCP server %s appeared after prior binding removal; refusing unsafe recovery", desiredName)}
	}
	plan.expected = expected
	return plan, touched, nil
}

func prepareCopilotMCPInstall(runtimeCLI, executable string, desired transcriptcapture.Config, previous *transcriptcapture.Config) (bool, error) {
	_, touched, err := prepareCopilotMCPInstallPlan(runtimeCLI, executable, desired, previous)
	return touched, err
}

func registerCopilotMCP(runtimeCLI string, cfg transcriptcapture.Config) error {
	current, exists, snapshot, err := inspectCopilotMCPState(runtimeCLI, cfg)
	if err != nil {
		return err
	}
	name, desired, err := copilotMCPBindingFromConfig(cfg.MCPCommand, cfg)
	if err != nil {
		return err
	}
	if exists {
		if equalCopilotMCPBinding(current, desired) {
			return nil
		}
		return fmt.Errorf("GitHub Copilot MCP server %s has a foreign registration; refusing to replace it", name)
	}
	_, err = registerCopilotMCPWithPlan(runtimeCLI, copilotMCPInstallPlan{
		desired: cfg, desiredName: name, desiredBinding: desired,
		expected: snapshot, registerRequired: true,
	})
	return err
}

func registerCopilotMCPWithPlan(runtimeCLI string, plan copilotMCPInstallPlan) (bool, error) {
	cfg := plan.desired
	if err := validateCopilotCLISelection(runtimeCLI, cfg); err != nil {
		return false, err
	}
	name, desired := plan.desiredName, plan.desiredBinding
	current, exists, before, err := inspectCopilotMCPState(runtimeCLI, cfg)
	if err != nil {
		return false, err
	}
	if !copilotMCPConfigSnapshotMatchesExact(plan.expected, before) {
		return false, &providerPreflightChangedError{err: fmt.Errorf("GitHub Copilot MCP registry changed after preflight; refusing to claim, replace, or remove server %s", name)}
	}
	if !plan.registerRequired {
		if !exists || !equalCopilotMCPBinding(current, desired) {
			return false, &providerPreflightChangedError{err: fmt.Errorf("GitHub Copilot MCP server %s changed after preflight; refusing to claim, replace, or remove it", name)}
		}
		return false, nil
	}
	if exists {
		return false, &providerPreflightChangedError{err: fmt.Errorf("GitHub Copilot MCP server %s appeared after preflight; refusing to claim, replace, or remove it", name)}
	}

	args := []string{"mcp", "add", name, "--tools", "*"}
	keys := make([]string, 0, len(desired.Env))
	for key := range desired.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--env", key+"="+desired.Env[key])
	}
	args = append(args, "--", desired.Command)
	args = append(args, desired.Args...)
	if _, err := runCopilotCLI(runtimeCLI, cfg.RuntimeConfigRoot, copilotCLIMutationTimeout, args...); err != nil {
		_, _, afterSnapshot, inspectErr := inspectCopilotMCPState(runtimeCLI, cfg)
		if inspectErr == nil && copilotMCPConfigSnapshotMatchesExact(before, afterSnapshot) {
			return false, fmt.Errorf("add GitHub Copilot MCP server %s: %w", name, err)
		}
		return false, &providerMutationUncertainError{err: fmt.Errorf("add GitHub Copilot MCP server %s returned an error after provider state changed; refusing unsafe attribution: %w", name, err)}
	}
	after, afterExists, afterSnapshot, err := inspectCopilotMCPState(runtimeCLI, cfg)
	if err != nil {
		return true, &providerMutationUncertainError{err: fmt.Errorf("verify GitHub Copilot MCP server %s: %w", name, err)}
	}
	if !afterExists || !equalCopilotMCPBinding(after, desired) {
		return true, &providerMutationUncertainError{err: fmt.Errorf("GitHub Copilot did not retain the exact Witself MCP server %s after a successful add", name)}
	}
	if err := verifyCopilotNonTargetPreservation(before, afterSnapshot); err != nil {
		return true, &providerMutationUncertainError{err: err}
	}
	if err := verifyCopilotMutationPermissions(afterSnapshot); err != nil {
		return true, &providerMutationUncertainError{err: err}
	}
	return true, nil
}

func unregisterCopilotMCP(runtimeCLI string, expected *transcriptcapture.Config) error {
	_, err := unregisterCopilotMCPWithSnapshot(runtimeCLI, expected, nil)
	return err
}

func unregisterCopilotMCPWithSnapshot(runtimeCLI string, expected *transcriptcapture.Config, expectedSnapshot *copilotMCPConfigSnapshot) (bool, error) {
	if expected == nil {
		return false, errors.New("expected Copilot integration binding is required for safe MCP removal")
	}
	if err := validateCopilotCLISelection(runtimeCLI, *expected); err != nil {
		return false, err
	}
	name, expectedBinding, err := copilotMCPBindingFromConfig(expected.MCPCommand, *expected)
	if err != nil {
		return false, err
	}
	current, exists, before, err := inspectCopilotMCPState(runtimeCLI, *expected)
	if err != nil {
		return false, err
	}
	if expectedSnapshot != nil && !copilotMCPConfigSnapshotMatchesExact(*expectedSnapshot, before) {
		return false, &providerPreflightChangedError{err: fmt.Errorf("GitHub Copilot MCP registry changed after preflight; refusing to remove server %s", name)}
	}
	if !exists {
		return false, nil
	}
	if !equalCopilotMCPBinding(current, expectedBinding) {
		return false, fmt.Errorf("GitHub Copilot MCP server %s does not match the installed binding; refusing to remove it", name)
	}
	if _, err := runCopilotCLI(runtimeCLI, expected.RuntimeConfigRoot, copilotCLIMutationTimeout, "mcp", "remove", name); err != nil {
		_, _, afterSnapshot, inspectErr := inspectCopilotMCPState(runtimeCLI, *expected)
		if inspectErr == nil && copilotMCPConfigSnapshotMatchesExact(before, afterSnapshot) {
			return false, fmt.Errorf("remove GitHub Copilot MCP server %s: %w", name, err)
		}
		return false, &providerMutationUncertainError{err: fmt.Errorf("remove GitHub Copilot MCP server %s returned an error after provider state changed; refusing unsafe attribution: %w", name, err)}
	}
	if _, afterExists, afterSnapshot, err := inspectCopilotMCPState(runtimeCLI, *expected); err != nil {
		return true, &providerMutationUncertainError{err: fmt.Errorf("verify GitHub Copilot MCP server %s removal: %w", name, err)}
	} else if afterExists {
		return true, &providerMutationUncertainError{err: fmt.Errorf("GitHub Copilot retained or replaced MCP server %s after a successful removal", name)}
	} else if err := verifyCopilotNonTargetPreservation(before, afterSnapshot); err != nil {
		return true, &providerMutationUncertainError{err: err}
	} else if err := verifyCopilotMutationPermissions(afterSnapshot); err != nil {
		return true, &providerMutationUncertainError{err: err}
	}
	return true, nil
}

func validateCopilotInstalledTopology(cfg transcriptcapture.Config) error {
	root, err := currentCopilotConfigRoot()
	if err != nil {
		return err
	}
	if root != cfg.RuntimeConfigRoot || cfg.RuntimeMCPConfigPath != filepath.Join(root, "mcp-config.json") {
		return fmt.Errorf("GitHub Copilot config root changed from installed %s to %s; refusing to expose the credential-bound MCP server", cfg.RuntimeConfigRoot, root)
	}
	environment, err := captureCopilotMCPEnvironment()
	if err != nil {
		return err
	}
	if !equalCopilotEnvironment(environment, cfg.MCPEnvironment) {
		return errors.New("WITSELF_HOME changed from the installed GitHub Copilot binding; refusing to expose the credential-bound MCP server")
	}
	return validateCopilotPersistedTopology(cfg)
}

func validateCopilotPersistedTopology(cfg transcriptcapture.Config) error {
	if err := validateCopilotCLISelection(cfg.RuntimeCLICommand, cfg); err != nil {
		return err
	}
	_, expected, err := copilotMCPBindingFromConfig(cfg.MCPCommand, cfg)
	if err != nil {
		return err
	}
	current, exists, persisted, err := inspectCopilotMCPState(cfg.RuntimeCLICommand, cfg)
	if err != nil {
		return fmt.Errorf("inspect installed GitHub Copilot MCP topology: %w", err)
	}
	if !exists || !equalCopilotMCPBinding(current, expected) {
		if !exists {
			return incompleteIntegrationTopology(errors.New("GitHub Copilot MCP topology is missing the installed Witself binding"))
		}
		return errors.New("GitHub Copilot MCP topology no longer matches the installed Witself binding")
	}
	if err := verifyCopilotMutationPermissions(persisted); err != nil {
		return err
	}
	if err := validateCopilotManagedInstructionsAt(cfg.RuntimeConfigRoot); err != nil {
		return fmt.Errorf("GitHub Copilot managed instructions no longer match the installed policy: %w", err)
	}
	return nil
}

func validateCopilotManagedInstructionsAt(configRoot string) error {
	spec, err := copilotManagedInstructionsSpecAt(configRoot)
	if err != nil {
		return err
	}
	spec, err = normalizeManagedInstructionsSpec(spec)
	if err != nil {
		return err
	}
	snapshot, err := readManagedInstructionsSnapshot(spec)
	if err != nil {
		return err
	}
	if !snapshot.existed {
		return incompleteIntegrationTopology(errors.New("managed instruction file is missing"))
	}
	if err := validateExclusiveManagedInstructionsContent(snapshot.data, spec, true); err != nil {
		return err
	}
	if _, changed, err := upsertManagedInstructionsBlock(snapshot.data, spec); err != nil {
		return err
	} else if changed {
		return errors.New("managed instruction file is stale")
	}
	return nil
}

func validateCopilotCLISelection(runtimeCLI string, cfg transcriptcapture.Config) error {
	clean, err := cleanCopilotInvocationPath("GitHub Copilot CLI", runtimeCLI)
	if err != nil {
		return err
	}
	if clean != cfg.RuntimeCLICommand {
		return fmt.Errorf("GitHub Copilot CLI %s does not match installed command %s", clean, cfg.RuntimeCLICommand)
	}
	if cfg.RuntimeConfigRoot == "" || !filepath.IsAbs(cfg.RuntimeConfigRoot) || filepath.Clean(cfg.RuntimeConfigRoot) != cfg.RuntimeConfigRoot {
		return errors.New("GitHub Copilot config root must be a clean absolute path")
	}
	if cfg.RuntimeMCPConfigPath != filepath.Join(cfg.RuntimeConfigRoot, "mcp-config.json") {
		return errors.New("GitHub Copilot MCP config path is not canonical for the installed config root")
	}
	return nil
}

// cleanCopilotCommandPath validates persisted command syntax without resolving
// the final symlink or requiring the target to still exist. Missing legacy
// Cellar paths must remain reconstructable so repair and uninstall can match
// and remove the exact prior Copilot registration after package cleanup.
func cleanCopilotCommandPath(label, value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value ||
		strings.ContainsAny(value, "\x00\r\n") || len(value) > 4096 {
		return "", fmt.Errorf("%s must be a non-empty path without surrounding whitespace", label)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	absolute = filepath.Clean(absolute)
	if !filepath.IsAbs(absolute) {
		return "", fmt.Errorf("%s must resolve to an absolute path", label)
	}
	return absolute, nil
}

// cleanCopilotInvocationPath deliberately preserves the final executable
// symlink while verifying the current target. Package managers such as
// Homebrew keep stable entrypoints while replacing versioned targets.
func cleanCopilotInvocationPath(label, value string) (string, error) {
	absolute, err := cleanCopilotCommandPath(label, value)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", label, err)
	}
	if !integrationExecutableModeIsUsable(info) {
		return "", fmt.Errorf("%s must resolve to an executable regular file", label)
	}
	return absolute, nil
}

func validateCopilotPreviousSelection(runtimeCLI string, desired, previous transcriptcapture.Config) error {
	if previous.RuntimeCLICommand != runtimeCLI {
		return errors.New("GitHub Copilot CLI changed since installation; uninstall the existing integration before reinstalling")
	}
	if previous.RuntimeConfigRoot != desired.RuntimeConfigRoot {
		return errors.New("COPILOT_HOME changed since installation; restore it before reinstalling or uninstall the existing integration first")
	}
	if previous.RuntimeMCPConfigPath != desired.RuntimeMCPConfigPath {
		return errors.New("GitHub Copilot MCP config path changed since installation")
	}
	return nil
}

func runCopilotCLICommand(runtimeCLI, configRoot string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtimeCLI, args...)
	cleanup, err := isolateProviderCLIWorkingDirectory(cmd, "GitHub Copilot")
	if err != nil {
		return nil, err
	}
	defer cleanup()
	cmd.WaitDelay = copilotCLIWaitDelay
	cmd.Env = copilotCommandEnvironment(os.Environ(), configRoot)
	output := &copilotBoundedBuffer{limit: copilotCLIOutputLimit}
	cmd.Stdout = output
	cmd.Stderr = output
	err = cmd.Run()
	raw, truncated := output.snapshot()
	if truncated {
		return nil, fmt.Errorf("GitHub Copilot CLI output exceeds %d bytes", copilotCLIOutputLimit)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("GitHub Copilot CLI timed out after %s: %w", timeout, ctx.Err())
	}
	if err != nil {
		message := strings.TrimSpace(string(raw))
		if message == "" {
			message = err.Error()
		}
		return nil, errors.New(message)
	}
	return raw, nil
}

func copilotCommandEnvironment(inherited []string, configRoot string) []string {
	values := make(map[string]string, len(inherited))
	for _, entry := range inherited {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	for key := range values {
		if strings.EqualFold(key, "COPILOT_HOME") {
			delete(values, key)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys)+1)
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return append(result, "COPILOT_HOME="+configRoot)
}

type copilotBoundedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *copilotBoundedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		write := len(value)
		if write > remaining {
			write = remaining
		}
		_, _ = buffer.buffer.Write(value[:write])
	}
	if len(value) > remaining {
		buffer.truncated = true
	}
	return len(value), nil
}

func (buffer *copilotBoundedBuffer) snapshot() ([]byte, bool) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return bytes.Clone(buffer.buffer.Bytes()), buffer.truncated
}
