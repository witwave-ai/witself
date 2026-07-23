package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	runtimepkg "runtime"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const (
	genericProviderConfigReadLimit = 8 * 1024 * 1024
	genericProviderCLIOutputLimit  = 64 * 1024
	genericProviderCLIWaitDelay    = 2 * time.Second
)

type genericMCPBinding struct {
	Command     string
	Args        []string
	Environment map[string]string
}

type genericMCPConfigSnapshot struct {
	path       string
	existed    bool
	mode       os.FileMode
	raw        []byte
	nonTarget  string
	fileInfo   os.FileInfo
	rootInfo   os.FileInfo
	parentInfo os.FileInfo
	target     genericMCPBinding
	targetSet  bool
}

type genericProviderEffectiveProbeError struct {
	err error
}

func (e *genericProviderEffectiveProbeError) Error() string { return e.err.Error() }
func (e *genericProviderEffectiveProbeError) Unwrap() error { return e.err }

func isGenericProviderRuntime(runtimeName string) bool {
	switch runtimeName {
	case transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor:
		return true
	default:
		return false
	}
}

func genericProviderConfigPaths(runtimeName string) (string, string, error) {
	if !isGenericProviderRuntime(runtimeName) {
		return "", "", fmt.Errorf("runtime %q does not use the generic provider topology", runtimeName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve user home for %s: %w", runtimeName, err)
	}
	home, err = cleanCopilotAbsolutePath("user home", home)
	if err != nil {
		return "", "", err
	}

	selectorName := ""
	defaultDirectory := ""
	switch runtimeName {
	case transcriptcapture.RuntimeCodex:
		selectorName, defaultDirectory = "CODEX_HOME", ".codex"
	case transcriptcapture.RuntimeClaudeCode:
		selectorName, defaultDirectory = "CLAUDE_CONFIG_DIR", ".claude"
	case transcriptcapture.RuntimeGrokBuild:
		selectorName, defaultDirectory = "GROK_HOME", ".grok"
	case transcriptcapture.RuntimeCursor:
		selectorName, defaultDirectory = "CURSOR_CONFIG_DIR", ".cursor"
	}

	if runtimeName == transcriptcapture.RuntimeCursor {
		// cursor-agent 2026.07.16 resolves only project-local .cursor/mcp.json
		// and ~/.cursor/mcp.json; it does not honor CURSOR_CONFIG_DIR. Reject the
		// ineffective selector so Witself never records a namespace that the
		// provider silently ignores.
		root, err := cleanCopilotAbsolutePath("Cursor config root", filepath.Join(home, defaultDirectory))
		if err != nil {
			return "", "", err
		}
		if os.Getenv(selectorName) != "" {
			return "", "", fmt.Errorf("%s is not supported by cursor-agent; unset it so the native home config root %s is used", selectorName, root)
		}
		return root, filepath.Join(root, "mcp.json"), nil
	}

	selected := os.Getenv(selectorName)
	explicitSelector := selected != ""
	if !explicitSelector {
		selected = filepath.Join(home, defaultDirectory)
	}
	root, err := cleanCopilotAbsolutePath(selectorName, selected)
	if err != nil {
		return "", "", err
	}

	var mcpPath string
	switch runtimeName {
	case transcriptcapture.RuntimeCodex, transcriptcapture.RuntimeGrokBuild:
		mcpPath = filepath.Join(root, "config.toml")
	case transcriptcapture.RuntimeCursor:
		mcpPath = filepath.Join(root, "mcp.json")
	case transcriptcapture.RuntimeClaudeCode:
		if explicitSelector {
			mcpPath = filepath.Join(root, ".claude.json")
		} else {
			mcpPath = filepath.Join(home, ".claude.json")
		}
	}
	return root, filepath.Clean(mcpPath), nil
}

func configureGenericProviderBinding(cfg *transcriptcapture.Config, runtimeCLI, witselfExecutable string) error {
	if cfg == nil || !isGenericProviderRuntime(cfg.Runtime) {
		return errors.New("generic provider integration config is required")
	}
	var err error
	runtimeCLI, err = cleanGenericInvocationPath(cfg.Runtime+" CLI", runtimeCLI)
	if err != nil {
		return err
	}
	witselfExecutable, err = cleanGenericInvocationPath("Witself executable", witselfExecutable)
	if err != nil {
		return err
	}
	root, mcpPath, err := genericProviderConfigPaths(cfg.Runtime)
	if err != nil {
		return err
	}
	witselfHome, err := local.Home()
	if err != nil {
		return fmt.Errorf("resolve WITSELF_HOME: %w", err)
	}
	witselfHome, err = cleanCopilotAbsolutePath("WITSELF_HOME", witselfHome)
	if err != nil {
		return err
	}
	cfg.RuntimeCLICommand = runtimeCLI
	cfg.MCPCommand = witselfExecutable
	cfg.MCPEnvironment = map[string]string{"WITSELF_HOME": witselfHome}
	cfg.RuntimeConfigRoot = root
	cfg.RuntimeMCPConfigPath = mcpPath
	return nil
}

func cleanGenericInvocationPath(label, value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") || len(value) > 4096 {
		return "", fmt.Errorf("%s must be a non-empty path without surrounding whitespace", label)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s must resolve to a regular file", label)
	}
	if runtimepkg.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s must resolve to an executable file", label)
	}
	return absolute, nil
}

func genericProviderConfigIsPinned(cfg transcriptcapture.Config) bool {
	return cfg.RuntimeCLICommand != "" || cfg.MCPCommand != "" || cfg.RuntimeConfigRoot != "" ||
		cfg.RuntimeMCPConfigPath != "" || len(cfg.MCPEnvironment) != 0
}

func validateGenericProviderPreviousSelection(desired, previous transcriptcapture.Config) error {
	if !genericProviderConfigIsPinned(previous) {
		return nil
	}
	switch {
	case previous.RuntimeCLICommand != desired.RuntimeCLICommand:
		return fmt.Errorf("%s CLI changed from %s to %s; restore the installed CLI selection before reinstalling", previous.Runtime, previous.RuntimeCLICommand, desired.RuntimeCLICommand)
	case previous.RuntimeConfigRoot != desired.RuntimeConfigRoot:
		return fmt.Errorf("%s config root changed from %s to %s; restore the installed selector before reinstalling", previous.Runtime, previous.RuntimeConfigRoot, desired.RuntimeConfigRoot)
	case previous.RuntimeMCPConfigPath != desired.RuntimeMCPConfigPath:
		return fmt.Errorf("%s MCP config path changed; restore the installed selector before reinstalling", previous.Runtime)
	case previous.MCPEnvironment["WITSELF_HOME"] != desired.MCPEnvironment["WITSELF_HOME"]:
		return errors.New("WITSELF_HOME changed since installation; restore it before reinstalling")
	}
	return nil
}

func validateGenericProviderCurrentRoots(cfg transcriptcapture.Config) error {
	if !isGenericProviderRuntime(cfg.Runtime) || !genericProviderConfigIsPinned(cfg) {
		return nil
	}
	root, mcpPath, err := genericProviderConfigPaths(cfg.Runtime)
	if err != nil {
		return err
	}
	if root != cfg.RuntimeConfigRoot || mcpPath != cfg.RuntimeMCPConfigPath {
		return fmt.Errorf("%s provider selector changed from installed config root %s; restore it before continuing", cfg.Runtime, cfg.RuntimeConfigRoot)
	}
	if len(cfg.MCPEnvironment) != 0 {
		witselfHome, err := local.Home()
		if err != nil {
			return err
		}
		witselfHome, err = cleanCopilotAbsolutePath("WITSELF_HOME", witselfHome)
		if err != nil {
			return err
		}
		if witselfHome != cfg.MCPEnvironment["WITSELF_HOME"] {
			return errors.New("WITSELF_HOME changed from the installed provider binding; restore it before continuing")
		}
	}
	return nil
}

func validateGenericProviderCurrentSelection(cfg transcriptcapture.Config) (string, error) {
	if err := validateGenericProviderCurrentRoots(cfg); err != nil {
		return "", err
	}
	currentCLI, err := findRuntimeCLI(cfg.Runtime)
	if err != nil {
		return "", err
	}
	currentCLI, err = cleanGenericInvocationPath(cfg.Runtime+" CLI", currentCLI)
	if err != nil {
		return "", err
	}
	if genericProviderConfigIsPinned(cfg) && currentCLI != cfg.RuntimeCLICommand {
		return "", fmt.Errorf("%s CLI selection changed from installed %s to %s; restore it before continuing", cfg.Runtime, cfg.RuntimeCLICommand, currentCLI)
	}
	return currentCLI, nil
}

func hydrateLegacyGenericProviderConfig(cfg transcriptcapture.Config, runtimeCLI, witselfExecutable string) (transcriptcapture.Config, error) {
	if genericProviderConfigIsPinned(cfg) {
		return cfg, nil
	}
	if err := configureGenericProviderBinding(&cfg, runtimeCLI, witselfExecutable); err != nil {
		return transcriptcapture.Config{}, err
	}
	// Releases before generic-provider ownership pinning wrote the same exact
	// command and arguments but did not serialize an MCP environment. Keep that
	// empty environment on this transient expected binding so reinstall can
	// transactionally migrate it and uninstall can remove it without claiming a
	// foreign registration. The newly persisted desired config still requires
	// and injects WITSELF_HOME.
	cfg.MCPEnvironment = nil
	return cfg, nil
}

func genericMCPBindingFromConfig(cfg transcriptcapture.Config) (genericMCPBinding, error) {
	if !isGenericProviderRuntime(cfg.Runtime) {
		return genericMCPBinding{}, fmt.Errorf("runtime %q does not use a generic MCP binding", cfg.Runtime)
	}
	if cfg.MCPCommand == "" || !filepath.IsAbs(cfg.MCPCommand) {
		return genericMCPBinding{}, errors.New("installed generic provider integration has no absolute MCP command")
	}
	agent := strings.TrimSpace(cfg.Agent)
	if agent == "" {
		agent = strings.TrimSpace(cfg.AgentName)
	}
	if agent == "" {
		return genericMCPBinding{}, errors.New("installed generic provider integration has no agent name")
	}
	serveArgs := runtimeMCPServeArgs(
		cfg.Runtime,
		cfg.MCPCommand,
		defaultString(cfg.Account, "default"),
		defaultString(cfg.Realm, "default"),
		agent,
		cfg.Location.Name,
	)
	if len(serveArgs) < 2 {
		return genericMCPBinding{}, errors.New("build generic provider MCP command")
	}
	environment := cloneStringMap(cfg.MCPEnvironment)
	if len(environment) > 1 || (len(environment) == 1 && environment["WITSELF_HOME"] == "") {
		return genericMCPBinding{}, errors.New("generic provider MCP environment must be empty for a legacy binding or contain only WITSELF_HOME")
	}
	return genericMCPBinding{Command: serveArgs[0], Args: append([]string(nil), serveArgs[1:]...), Environment: environment}, nil
}

func equalGenericMCPBinding(left, right genericMCPBinding) bool {
	if left.Command != right.Command || !slices.Equal(left.Args, right.Args) || len(left.Environment) != len(right.Environment) {
		return false
	}
	for key, value := range left.Environment {
		if right.Environment[key] != value {
			return false
		}
	}
	return true
}

func inspectGenericMCP(cfg transcriptcapture.Config) (genericMCPBinding, bool, genericMCPConfigSnapshot, error) {
	snapshot, root, err := readGenericMCPConfig(cfg)
	if err != nil {
		return genericMCPBinding{}, false, genericMCPConfigSnapshot{}, err
	}
	binding, exists, err := genericMCPBindingFromRoot(cfg, root)
	if err != nil {
		return genericMCPBinding{}, false, genericMCPConfigSnapshot{}, err
	}
	if exists {
		snapshot.target = binding
		snapshot.targetSet = true
	}
	return binding, exists, snapshot, nil
}

func genericMCPBindingFromRoot(cfg transcriptcapture.Config, root map[string]any) (genericMCPBinding, bool, error) {
	containerName := "mcp_servers"
	if cfg.Runtime == transcriptcapture.RuntimeClaudeCode || cfg.Runtime == transcriptcapture.RuntimeCursor {
		containerName = "mcpServers"
	}
	containerValue, exists := root[containerName]
	if !exists {
		return genericMCPBinding{}, false, nil
	}
	container, ok := containerValue.(map[string]any)
	if !ok || container == nil {
		return genericMCPBinding{}, false, fmt.Errorf("parse %s: %s must be an object", cfg.RuntimeMCPConfigPath, containerName)
	}
	for name := range container {
		if name != "witself" && strings.EqualFold(name, "witself") {
			return genericMCPBinding{}, false, fmt.Errorf("%s MCP server %q collides case-insensitively with witself", cfg.Runtime, name)
		}
	}
	rawBinding, exists := container["witself"]
	if !exists {
		return genericMCPBinding{}, false, nil
	}
	binding, err := parseGenericMCPBinding(cfg.Runtime, rawBinding)
	if err != nil {
		return genericMCPBinding{}, false, fmt.Errorf("parse %s MCP server witself: %w", cfg.Runtime, err)
	}
	return binding, true, nil
}

func readGenericMCPConfig(cfg transcriptcapture.Config) (genericMCPConfigSnapshot, map[string]any, error) {
	path := cfg.RuntimeMCPConfigPath
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return genericMCPConfigSnapshot{}, nil, errors.New("generic provider MCP config path must be a clean absolute path")
	}
	rootInfo, err := inspectGenericProviderDirectory(cfg.RuntimeConfigRoot, "provider config root")
	if err != nil {
		return genericMCPConfigSnapshot{}, nil, err
	}
	parent := filepath.Dir(path)
	parentInfo, err := inspectGenericProviderDirectory(parent, "provider MCP config parent")
	if err != nil {
		return genericMCPConfigSnapshot{}, nil, err
	}
	snapshot := genericMCPConfigSnapshot{path: path, rootInfo: rootInfo, parentInfo: parentInfo}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := revalidateGenericProviderDirectories(cfg, snapshot); err != nil {
			return genericMCPConfigSnapshot{}, nil, err
		}
		root := map[string]any{}
		snapshot.nonTarget, _ = canonicalGenericMCPNonTarget(cfg.Runtime, root)
		return snapshot, root, nil
	}
	if err != nil {
		return genericMCPConfigSnapshot{}, nil, fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return genericMCPConfigSnapshot{}, nil, fmt.Errorf("generic provider MCP config %s must be a regular non-symlink file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return genericMCPConfigSnapshot{}, nil, err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return genericMCPConfigSnapshot{}, nil, fmt.Errorf("inspect opened %s: %w", path, err)
	}
	linkedInfo, err := os.Lstat(path)
	if err != nil || !linkedInfo.Mode().IsRegular() || linkedInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, openedInfo) || !os.SameFile(openedInfo, linkedInfo) {
		if err == nil {
			err = errors.New("path identity changed while it was being opened")
		}
		return genericMCPConfigSnapshot{}, nil, fmt.Errorf("inspect %s: %w", path, err)
	}
	raw, err := io.ReadAll(io.LimitReader(file, genericProviderConfigReadLimit+1))
	if err != nil {
		return genericMCPConfigSnapshot{}, nil, err
	}
	if len(raw) > genericProviderConfigReadLimit {
		return genericMCPConfigSnapshot{}, nil, fmt.Errorf("generic provider MCP config %s exceeds %d bytes", path, genericProviderConfigReadLimit)
	}
	linkedAfterRead, err := os.Lstat(path)
	if err != nil || !linkedAfterRead.Mode().IsRegular() || linkedAfterRead.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, linkedAfterRead) {
		if err == nil {
			err = errors.New("path identity changed while it was being read")
		}
		return genericMCPConfigSnapshot{}, nil, fmt.Errorf("inspect %s: %w", path, err)
	}
	if err := revalidateGenericProviderDirectories(cfg, snapshot); err != nil {
		return genericMCPConfigSnapshot{}, nil, err
	}
	snapshot.existed = true
	snapshot.mode = info.Mode().Perm()
	snapshot.raw = append([]byte(nil), raw...)
	snapshot.fileInfo = openedInfo

	root, err := parseGenericMCPConfigBytes(cfg.Runtime, path, raw)
	if err != nil {
		return genericMCPConfigSnapshot{}, nil, err
	}
	snapshot.nonTarget, err = canonicalGenericMCPNonTarget(cfg.Runtime, root)
	if err != nil {
		return genericMCPConfigSnapshot{}, nil, fmt.Errorf("canonicalize non-target provider config %s: %w", path, err)
	}
	return snapshot, root, nil
}

// parseGenericMCPConfigBytes is shared by live inspection and transaction
// journal validation. Keeping one strict parser prevents recovery metadata from
// describing a preimage that the ordinary ownership checks would reject.
func parseGenericMCPConfigBytes(runtimeName, path string, raw []byte) (map[string]any, error) {
	root := map[string]any{}
	if runtimeName == transcriptcapture.RuntimeClaudeCode || runtimeName == transcriptcapture.RuntimeCursor {
		if err := rejectDuplicateJSONKeys(raw); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&root); err != nil || root == nil {
			if err == nil {
				err = errors.New("top-level value must be an object")
			}
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			if err == nil {
				err = errors.New("multiple JSON values are not allowed")
			}
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if len(bytes.TrimSpace(raw)) != 0 {
		if err := toml.Unmarshal(raw, &root); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	return root, nil
}

func inspectGenericProviderDirectory(path, label string) (os.FileInfo, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("%s must be a clean absolute path", label)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s %s: %w", label, path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s %s must be a real non-symlink directory", label, path)
	}
	return info, nil
}

func revalidateGenericProviderDirectories(cfg transcriptcapture.Config, snapshot genericMCPConfigSnapshot) error {
	root, err := inspectGenericProviderDirectory(cfg.RuntimeConfigRoot, "provider config root")
	if err != nil {
		return err
	}
	parent, err := inspectGenericProviderDirectory(filepath.Dir(snapshot.path), "provider MCP config parent")
	if err != nil {
		return err
	}
	if !os.SameFile(root, snapshot.rootInfo) || !os.SameFile(parent, snapshot.parentInfo) {
		return errors.New("provider configuration directory identity changed during inspection")
	}
	return nil
}

func genericSnapshotStillCurrent(cfg transcriptcapture.Config, expected genericMCPConfigSnapshot) error {
	current, _, err := readGenericMCPConfig(cfg)
	if err != nil {
		return err
	}
	if current.existed != expected.existed || current.mode != expected.mode || !bytes.Equal(current.raw, expected.raw) ||
		!os.SameFile(current.rootInfo, expected.rootInfo) || !os.SameFile(current.parentInfo, expected.parentInfo) {
		return errors.New("provider configuration changed before mutation")
	}
	if current.existed && (current.fileInfo == nil || expected.fileInfo == nil || !os.SameFile(current.fileInfo, expected.fileInfo)) {
		return errors.New("provider configuration file identity changed before mutation")
	}
	return nil
}

func parseGenericMCPBinding(runtimeName string, raw any) (genericMCPBinding, error) {
	fields, ok := raw.(map[string]any)
	if !ok || fields == nil {
		return genericMCPBinding{}, errors.New("registration must be an object")
	}
	for field := range fields {
		allowed := field == "command" || field == "args" || field == "env" || field == "enabled"
		if runtimeName == transcriptcapture.RuntimeClaudeCode || runtimeName == transcriptcapture.RuntimeCursor {
			allowed = allowed || field == "type"
		}
		if !allowed {
			return genericMCPBinding{}, fmt.Errorf("non-standard field %q; refusing to modify it", field)
		}
	}
	command, ok := fields["command"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return genericMCPBinding{}, errors.New("command must be a non-empty string")
	}
	args, err := stringSliceFromGenericValue(fields["args"])
	if err != nil {
		return genericMCPBinding{}, fmt.Errorf("args: %w", err)
	}
	environment, err := stringMapFromGenericValue(fields["env"])
	if err != nil {
		return genericMCPBinding{}, fmt.Errorf("env: %w", err)
	}
	if enabled, exists := fields["enabled"]; exists {
		value, ok := enabled.(bool)
		if !ok || !value {
			return genericMCPBinding{}, errors.New("MCP server must be enabled")
		}
	} else if runtimeName == transcriptcapture.RuntimeGrokBuild {
		return genericMCPBinding{}, errors.New("the Grok MCP server must explicitly be enabled")
	}
	if kind, exists := fields["type"]; exists {
		value, ok := kind.(string)
		if !ok || value != "stdio" {
			return genericMCPBinding{}, errors.New("MCP server type must be stdio")
		}
	} else if runtimeName == transcriptcapture.RuntimeClaudeCode {
		return genericMCPBinding{}, errors.New("the Claude MCP server type must be stdio")
	}
	return genericMCPBinding{Command: command, Args: args, Environment: environment}, nil
}

func stringSliceFromGenericValue(raw any) ([]string, error) {
	values, ok := raw.([]any)
	if !ok {
		if strings, ok := raw.([]string); ok {
			return append([]string(nil), strings...), nil
		}
		return nil, errors.New("must be an array of strings")
	}
	result := make([]string, len(values))
	for index, value := range values {
		item, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("item %d must be a string", index)
		}
		result[index] = item
	}
	return result, nil
}

func stringMapFromGenericValue(raw any) (map[string]string, error) {
	if raw == nil {
		return map[string]string{}, nil
	}
	values, ok := raw.(map[string]any)
	if !ok {
		if strings, ok := raw.(map[string]string); ok {
			return cloneStringMap(strings), nil
		}
		return nil, errors.New("must be an object of strings")
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		item, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("value for %s must be a string", key)
		}
		result[key] = item
	}
	return result, nil
}

func canonicalGenericMCPNonTarget(runtimeName string, root map[string]any) (string, error) {
	cloneRaw, err := json.Marshal(root)
	if err != nil {
		return "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(cloneRaw))
	decoder.UseNumber()
	clone := map[string]any{}
	if err := decoder.Decode(&clone); err != nil {
		return "", err
	}
	containerName := "mcp_servers"
	if runtimeName == transcriptcapture.RuntimeClaudeCode || runtimeName == transcriptcapture.RuntimeCursor {
		containerName = "mcpServers"
	}
	if container, ok := clone[containerName].(map[string]any); ok {
		delete(container, "witself")
		if len(container) == 0 {
			delete(clone, containerName)
		} else {
			clone[containerName] = container
		}
	}
	var canonical strings.Builder
	if err := writeCopilotSemanticJSON(&canonical, clone); err != nil {
		return "", err
	}
	return canonical.String(), nil
}

func prepareGenericMCPInstall(runtimeCLI string, desired transcriptcapture.Config, previous *transcriptcapture.Config) error {
	_, err := prepareGenericMCPInstallSnapshot(runtimeCLI, desired, previous)
	return err
}

func prepareGenericMCPInstallSnapshot(runtimeCLI string, desired transcriptcapture.Config, previous *transcriptcapture.Config) (genericMCPConfigSnapshot, error) {
	current, exists, snapshot, err := inspectGenericMCP(desired)
	if err != nil {
		return genericMCPConfigSnapshot{}, err
	}
	if previous == nil {
		if exists {
			return genericMCPConfigSnapshot{}, fmt.Errorf("%s MCP server witself exists without a Witself integration record; refusing to claim or replace it", desired.Runtime)
		}
		return snapshot, nil
	}
	expected, err := genericMCPBindingFromConfig(*previous)
	if err != nil {
		return genericMCPConfigSnapshot{}, err
	}
	if !exists {
		return genericMCPConfigSnapshot{}, fmt.Errorf("installed %s MCP server witself is missing; refusing to reinstall over drift", desired.Runtime)
	}
	if !equalGenericMCPBinding(current, expected) {
		return genericMCPConfigSnapshot{}, fmt.Errorf("installed %s MCP server witself changed since installation; refusing to replace it", desired.Runtime)
	}
	if runtimeCLI != desired.RuntimeCLICommand {
		return genericMCPConfigSnapshot{}, fmt.Errorf("%s CLI does not match the persisted provider command", desired.Runtime)
	}
	return snapshot, nil
}

func registerGenericMCP(runtimeCLI string, desired transcriptcapture.Config, previous *transcriptcapture.Config) error {
	_, err := registerGenericMCPWithMutation(runtimeCLI, desired, previous)
	return err
}

func registerGenericMCPWithMutation(runtimeCLI string, desired transcriptcapture.Config, previous *transcriptcapture.Config) (bool, error) {
	desiredBinding, err := genericMCPBindingFromConfig(desired)
	if err != nil {
		return false, err
	}
	current, exists, before, err := inspectGenericMCP(desired)
	if err != nil {
		return false, err
	}
	if previous == nil {
		if exists {
			return false, fmt.Errorf("%s MCP server witself appeared before registration; refusing to replace it", desired.Runtime)
		}
	} else {
		previousBinding, bindingErr := genericMCPBindingFromConfig(*previous)
		if bindingErr != nil {
			return false, bindingErr
		}
		if !exists || !equalGenericMCPBinding(current, previousBinding) {
			return false, fmt.Errorf("installed %s MCP server witself changed before registration; refusing to replace it", desired.Runtime)
		}
		if equalGenericMCPBinding(current, desiredBinding) {
			return false, nil
		}
		if err := genericSnapshotStillCurrent(desired, before); err != nil {
			return false, fmt.Errorf("%s provider configuration changed before removing the prior binding: %w", desired.Runtime, err)
		}
		if err := removeGenericMCPUnchecked(runtimeCLI, desired); err != nil {
			return true, restoreGenericSnapshotAfterFailure(runtimeCLI, desired, before, fmt.Errorf("remove prior %s MCP registration: %w", desired.Runtime, err))
		}
		if _, stillExists, afterRemove, inspectErr := inspectGenericMCP(desired); inspectErr != nil || stillExists || before.nonTarget != afterRemove.nonTarget {
			return true, restoreGenericSnapshotAfterFailure(runtimeCLI, desired, before, fmt.Errorf("%s did not remove only the expected MCP registration", desired.Runtime))
		} else if err := genericSnapshotStillCurrent(desired, afterRemove); err != nil {
			return true, restoreGenericSnapshotAfterFailure(runtimeCLI, desired, before, fmt.Errorf("%s provider configuration changed before adding the replacement binding: %w", desired.Runtime, err))
		}
	}
	if previous == nil {
		if err := genericSnapshotStillCurrent(desired, before); err != nil {
			return false, fmt.Errorf("%s provider configuration changed before registration: %w", desired.Runtime, err)
		}
	}

	if err := addGenericMCPUnchecked(runtimeCLI, desired, desiredBinding); err != nil {
		return true, restoreGenericSnapshotAfterFailure(runtimeCLI, desired, before, fmt.Errorf("add %s MCP registration: %w", desired.Runtime, err))
	}
	after, afterExists, afterSnapshot, err := inspectGenericMCP(desired)
	if err != nil || !afterExists || !equalGenericMCPBinding(after, desiredBinding) || before.nonTarget != afterSnapshot.nonTarget {
		if err == nil {
			err = errors.New("provider did not persist the exact command, arguments, environment, and non-target configuration")
		}
		return true, restoreGenericSnapshotAfterFailure(runtimeCLI, desired, before, fmt.Errorf("verify %s MCP registration: %w", desired.Runtime, err))
	}
	if desired.Runtime == transcriptcapture.RuntimeGrokBuild {
		if _, err := verifyGrokNativeMCPBindingForConfig(runtimeCLI, desired, desiredBinding); err != nil {
			return true, restoreGenericSnapshotAfterFailure(runtimeCLI, desired, before, fmt.Errorf("verify native Grok MCP registration: %w", err))
		}
	}
	return true, nil
}

func unregisterGenericMCP(runtimeCLI string, expected transcriptcapture.Config) error {
	expectedBinding, err := genericMCPBindingFromConfig(expected)
	if err != nil {
		return err
	}
	current, exists, before, err := inspectGenericMCP(expected)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("installed %s MCP server witself is missing; refusing to discard the integration record", expected.Runtime)
	}
	if !equalGenericMCPBinding(current, expectedBinding) {
		return fmt.Errorf("installed %s MCP server witself changed since installation; refusing to remove it", expected.Runtime)
	}
	if err := removeGenericMCPUnchecked(runtimeCLI, expected); err != nil {
		return restoreGenericSnapshotAfterFailure(runtimeCLI, expected, before, fmt.Errorf("remove %s MCP registration: %w", expected.Runtime, err))
	}
	_, exists, after, err := inspectGenericMCP(expected)
	if err != nil || exists || before.nonTarget != after.nonTarget {
		if err == nil {
			err = errors.New("provider did not remove only the expected MCP registration")
		}
		return restoreGenericSnapshotAfterFailure(runtimeCLI, expected, before, fmt.Errorf("verify %s MCP removal: %w", expected.Runtime, err))
	}
	return nil
}

func restoreGenericMCPBinding(runtimeCLI string, previous, attempted *transcriptcapture.Config) error {
	if attempted == nil {
		return errors.New("attempted generic provider binding is required for safe MCP rollback")
	}
	current, exists, _, err := inspectGenericMCP(*attempted)
	if err != nil {
		return err
	}
	if previous == nil {
		if !exists {
			return nil
		}
		attemptedBinding, err := genericMCPBindingFromConfig(*attempted)
		if err != nil {
			return err
		}
		if !equalGenericMCPBinding(current, attemptedBinding) {
			return fmt.Errorf("%s MCP server witself changed during rollback; refusing to modify it", attempted.Runtime)
		}
		return unregisterGenericMCP(runtimeCLI, *attempted)
	}
	previousBinding, err := genericMCPBindingFromConfig(*previous)
	if err != nil {
		return err
	}
	if exists && equalGenericMCPBinding(current, previousBinding) {
		return nil
	}
	if exists {
		attemptedBinding, err := genericMCPBindingFromConfig(*attempted)
		if err != nil {
			return err
		}
		if !equalGenericMCPBinding(current, attemptedBinding) {
			return fmt.Errorf("%s MCP server witself changed during rollback; refusing to replace it", attempted.Runtime)
		}
		return registerGenericMCP(runtimeCLI, *previous, attempted)
	}
	// The failed operation removed the exact-owned target. Reinstall from the
	// now-absent state: passing the attempted binding as "previous" would demand
	// that it still exist and reject the rollback it is meant to perform.
	return registerGenericMCP(runtimeCLI, *previous, nil)
}

func validateGenericInstalledTopology(cfg transcriptcapture.Config) error {
	expected, err := genericMCPBindingFromConfig(cfg)
	if err != nil {
		return err
	}
	current, exists, _, err := inspectGenericMCP(cfg)
	if err != nil {
		return err
	}
	if !exists || !equalGenericMCPBinding(current, expected) {
		return fmt.Errorf("installed %s MCP server witself does not match the persisted exact binding", cfg.Runtime)
	}
	if cfg.Runtime == transcriptcapture.RuntimeGrokBuild {
		if cfg.RuntimeCLICommand == "" {
			return incompleteIntegrationTopology(errors.New("installed Grok binding does not pin its provider CLI"))
		}
		if _, err := verifyGrokNativeMCPBindingForConfig(cfg.RuntimeCLICommand, cfg, expected); err != nil {
			var unavailable *genericProviderEffectiveProbeError
			if errors.As(err, &unavailable) {
				return unavailableIntegrationTopology(fmt.Errorf("verify effective Grok MCP registration: %w", err))
			}
			return fmt.Errorf("verify effective Grok MCP registration: %w", err)
		}
	}
	return nil
}

func addGenericMCPUnchecked(runtimeCLI string, cfg transcriptcapture.Config, binding genericMCPBinding) error {
	if cfg.Runtime == transcriptcapture.RuntimeCursor {
		if err := writeCursorMCPBinding(cfg, binding); err != nil {
			return err
		}
		_, err := runGenericProviderCLI(runtimeCLI, cfg, 15*time.Second, "mcp", "enable", "witself")
		return err
	}
	homeArgs := []string{}
	if home := binding.Environment["WITSELF_HOME"]; home != "" {
		homeArgs = []string{"--env", "WITSELF_HOME=" + home}
	}
	var args []string
	switch cfg.Runtime {
	case transcriptcapture.RuntimeCodex:
		args = append([]string{"mcp", "add", "witself"}, homeArgs...)
		args = append(args, "--", binding.Command)
		args = append(args, binding.Args...)
	case transcriptcapture.RuntimeClaudeCode:
		// Use Claude's structured registration surface so the variadic -e/--env
		// parser cannot consume the server name or command arguments. This also
		// gives us one exact payload to compare against the native config file.
		definition, err := json.Marshal(struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		}{
			Type:    "stdio",
			Command: binding.Command,
			Args:    append([]string(nil), binding.Args...),
			Env:     cloneStringMap(binding.Environment),
		})
		if err != nil {
			return fmt.Errorf("encode Claude MCP registration: %w", err)
		}
		args = []string{"mcp", "add-json", "--scope", "user", "witself", string(definition)}
	case transcriptcapture.RuntimeGrokBuild:
		args = append([]string{"mcp", "add", "--scope", "user"}, homeArgs...)
		args = append(args, "witself", "--", binding.Command)
		args = append(args, binding.Args...)
	default:
		return fmt.Errorf("unsupported generic provider runtime %q", cfg.Runtime)
	}
	_, err := runGenericProviderCLI(runtimeCLI, cfg, 15*time.Second, args...)
	return err
}

func removeGenericMCPUnchecked(runtimeCLI string, cfg transcriptcapture.Config) error {
	if cfg.Runtime == transcriptcapture.RuntimeCursor {
		_, _ = runGenericProviderCLI(runtimeCLI, cfg, 15*time.Second, "mcp", "disable", "witself")
		return deleteCursorMCPBinding(cfg)
	}
	args := []string{"mcp", "remove", "witself"}
	if cfg.Runtime == transcriptcapture.RuntimeClaudeCode || cfg.Runtime == transcriptcapture.RuntimeGrokBuild {
		args = []string{"mcp", "remove", "--scope", "user", "witself"}
	}
	output, err := runGenericProviderCLI(runtimeCLI, cfg, 15*time.Second, args...)
	if err != nil && mcpRegistrationAlreadyMissing(output) {
		return errors.New("provider reported that the exact-owned MCP registration disappeared before removal")
	}
	return err
}

func runGenericProviderCLI(runtimeCLI string, cfg transcriptcapture.Config, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtimeCLI, args...)
	cmd.Env = genericProviderCommandEnvironment(os.Environ(), cfg)
	// Avoid project-local provider configuration shadowing the exact
	// home-scoped binding that Witself owns and has locked. This also contains
	// provider-created session and documentation artifacts.
	cleanup, err := isolateProviderCLIWorkingDirectory(cmd, cfg.Runtime)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	cmd.WaitDelay = genericProviderCLIWaitDelay
	output := &antigravityValidationOutput{limit: genericProviderCLIOutputLimit}
	cmd.Stdout = output
	cmd.Stderr = output
	err = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return []byte(output.String()), fmt.Errorf("%s CLI timed out after %s", cfg.Runtime, timeout)
	}
	if err != nil {
		message := strings.TrimSpace(output.String())
		if message == "" {
			message = err.Error()
		}
		return []byte(output.String()), errors.New(message)
	}
	return []byte(output.String()), nil
}

func genericProviderCommandEnvironment(inherited []string, cfg transcriptcapture.Config) []string {
	selector := map[string]string{
		transcriptcapture.RuntimeCodex:      "CODEX_HOME",
		transcriptcapture.RuntimeClaudeCode: "CLAUDE_CONFIG_DIR",
		transcriptcapture.RuntimeGrokBuild:  "GROK_HOME",
		transcriptcapture.RuntimeCursor:     "CURSOR_CONFIG_DIR",
	}[cfg.Runtime]
	result := make([]string, 0, len(inherited)+3)
	for _, entry := range inherited {
		key, _, ok := strings.Cut(entry, "=")
		if ok && (strings.EqualFold(key, selector) ||
			(cfg.Runtime == transcriptcapture.RuntimeCursor && strings.EqualFold(key, "CURSOR_CONFIG_DIR")) ||
			(cfg.Runtime == transcriptcapture.RuntimeClaudeCode && (strings.EqualFold(key, "HOME") || strings.EqualFold(key, "USERPROFILE")))) {
			continue
		}
		result = append(result, entry)
	}
	if cfg.Runtime == transcriptcapture.RuntimeClaudeCode && cfg.RuntimeMCPConfigPath == filepath.Join(filepath.Dir(cfg.RuntimeConfigRoot), ".claude.json") {
		home := filepath.Dir(cfg.RuntimeMCPConfigPath)
		result = append(result, "HOME="+home, "USERPROFILE="+home)
	} else if cfg.Runtime != transcriptcapture.RuntimeCursor {
		result = append(result, selector+"="+cfg.RuntimeConfigRoot)
	}
	return result
}

func writeCursorMCPBinding(cfg transcriptcapture.Config, binding genericMCPBinding) error {
	snapshot, root, err := readGenericMCPConfig(cfg)
	if err != nil {
		return err
	}
	path := cfg.RuntimeMCPConfigPath
	servers := map[string]any{}
	if existing, ok := root["mcpServers"]; ok {
		var valid bool
		servers, valid = existing.(map[string]any)
		if !valid {
			return fmt.Errorf("parse %s: mcpServers must be an object", path)
		}
	}
	if servers == nil {
		servers = map[string]any{}
	}
	if _, exists := servers["witself"]; exists {
		return errors.New("the Cursor MCP server witself already exists before unchecked add")
	}
	servers["witself"] = map[string]any{
		"command": binding.Command,
		"args":    append([]string(nil), binding.Args...),
		"env":     cloneStringMap(binding.Environment),
	}
	root["mcpServers"] = servers
	raw, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := genericSnapshotStillCurrent(cfg, snapshot); err != nil {
		return fmt.Errorf("refuse Cursor MCP add: %w", err)
	}
	return writeFileAtomic(path, append(raw, '\n'), 0o600)
}

func deleteCursorMCPBinding(cfg transcriptcapture.Config) error {
	snapshot, root, err := readGenericMCPConfig(cfg)
	if err != nil {
		return err
	}
	path := cfg.RuntimeMCPConfigPath
	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		return errors.New("the Cursor MCP server witself disappeared before unchecked removal")
	}
	if _, exists := servers["witself"]; !exists {
		return errors.New("the Cursor MCP server witself disappeared before unchecked removal")
	}
	delete(servers, "witself")
	if len(servers) == 0 {
		delete(root, "mcpServers")
	} else {
		root["mcpServers"] = servers
	}
	if len(root) == 0 {
		if err := genericSnapshotStillCurrent(cfg, snapshot); err != nil {
			return fmt.Errorf("refuse Cursor MCP removal: %w", err)
		}
		linked, err := os.Lstat(path)
		if err != nil || linked.Mode()&os.ModeSymlink != 0 || !linked.Mode().IsRegular() || snapshot.fileInfo == nil || !os.SameFile(linked, snapshot.fileInfo) {
			if err == nil {
				err = errors.New("provider configuration file identity changed before removal")
			}
			return fmt.Errorf("refuse Cursor MCP removal: %w", err)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	raw, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := genericSnapshotStillCurrent(cfg, snapshot); err != nil {
		return fmt.Errorf("refuse Cursor MCP removal: %w", err)
	}
	return writeFileAtomic(path, append(raw, '\n'), snapshot.mode)
}

func restoreGenericSnapshotAfterFailure(runtimeCLI string, cfg transcriptcapture.Config, before genericMCPConfigSnapshot, cause error) error {
	currentBinding, currentExists, current, err := inspectGenericMCP(cfg)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("inspect failed %s mutation for exact rollback: %w", cfg.Runtime, err))
	}
	if current.nonTarget != before.nonTarget {
		return errors.Join(cause, errors.New("provider changed non-target configuration; refusing to overwrite concurrent or foreign edits"))
	}
	attempted, err := genericMCPBindingFromConfig(cfg)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("reconstruct attempted %s binding for exact rollback: %w", cfg.Runtime, err))
	}
	if currentExists && !equalGenericMCPBinding(currentBinding, attempted) &&
		(!before.targetSet || !equalGenericMCPBinding(currentBinding, before.target)) {
		return errors.Join(cause, errors.New("provider MCP server witself changed to a foreign or concurrent binding; refusing exact-byte rollback"))
	}
	if cfg.Runtime == transcriptcapture.RuntimeGrokBuild {
		if err := convergeGrokEffectiveMCP(runtimeCLI, cfg, before.target, before.targetSet, before.nonTarget); err != nil {
			return errors.Join(cause, fmt.Errorf("restore effective Grok MCP state: %w", err))
		}
		currentBinding, currentExists, current, err = inspectGenericMCP(cfg)
		if err != nil {
			return errors.Join(cause, err)
		}
		if current.nonTarget != before.nonTarget || currentExists != before.targetSet ||
			(currentExists && !equalGenericMCPBinding(currentBinding, before.target)) {
			return errors.Join(cause, errors.New("effective Grok rollback did not restore the exact target and non-target state"))
		}
	}
	// Re-read immediately before the exact-byte restore and require the same
	// snapshot we just inspected. This CAS-style fence catches a last-moment
	// provider or user edit instead of overwriting it with stale bytes.
	if err := genericSnapshotStillCurrent(cfg, current); err != nil {
		return errors.Join(cause, fmt.Errorf("provider configuration changed immediately before rollback; refusing to overwrite concurrent or foreign edits: %w", err))
	}
	if before.existed {
		if err := writeFileAtomic(before.path, before.raw, before.mode); err != nil {
			return errors.Join(cause, fmt.Errorf("restore exact provider config bytes: %w", err))
		}
	} else if err := os.Remove(before.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(cause, fmt.Errorf("remove failed provider config: %w", err))
	}
	if cfg.Runtime == transcriptcapture.RuntimeCursor {
		_, beforeHadBinding, _, inspectErr := inspectGenericMCPWithSnapshot(cfg, before)
		if inspectErr == nil {
			verb := "disable"
			if beforeHadBinding {
				verb = "enable"
			}
			_, _ = runGenericProviderCLI(runtimeCLI, cfg, 15*time.Second, "mcp", verb, "witself")
		}
	}
	return cause
}

func inspectGenericMCPWithSnapshot(cfg transcriptcapture.Config, _ genericMCPConfigSnapshot) (genericMCPBinding, bool, genericMCPConfigSnapshot, error) {
	// The exact bytes are already back on disk. Reuse the ordinary parser so
	// Cursor approval repair follows the same strict target interpretation.
	return inspectGenericMCP(cfg)
}

func verifyGrokNativeMCPBindingForConfig(runtimeCLI string, cfg transcriptcapture.Config, expected genericMCPBinding) (bool, error) {
	output, err := runGenericProviderCLI(runtimeCLI, cfg, 10*time.Second, "mcp", "list", "--json")
	if err != nil {
		return false, &genericProviderEffectiveProbeError{err: fmt.Errorf("run grok mcp list --json: %w", err)}
	}
	if len(strings.TrimSpace(string(output))) == 0 {
		return false, errors.New("grok mcp list --json returned no JSON")
	}
	var entries []struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
		Enabled *bool             `json:"enabled"`
		Name    string            `json:"name"`
		Scope   string            `json:"scope"`
	}
	if err := json.Unmarshal(output, &entries); err != nil {
		return true, fmt.Errorf("parse grok mcp list --json: %w", err)
	}
	matches := 0
	for _, entry := range entries {
		if entry.Name != "witself" {
			continue
		}
		matches++
		current := genericMCPBinding{Command: entry.Command, Args: entry.Args, Environment: entry.Env}
		if entry.Scope != "user" || entry.Enabled == nil || !*entry.Enabled || !equalGenericMCPBinding(current, expected) {
			return true, errors.New("effective Grok MCP registration does not match the requested native user binding")
		}
	}
	if matches != 1 {
		return true, fmt.Errorf("effective Grok MCP list contains %d native witself registrations; want exactly one", matches)
	}
	return true, nil
}

func verifyGrokNativeMCPAbsenceForConfig(runtimeCLI string, cfg transcriptcapture.Config) error {
	output, err := runGenericProviderCLI(runtimeCLI, cfg, 10*time.Second, "mcp", "list", "--json")
	if err != nil {
		return &genericProviderEffectiveProbeError{err: fmt.Errorf("run grok mcp list --json: %w", err)}
	}
	if len(strings.TrimSpace(string(output))) == 0 {
		return errors.New("grok mcp list --json returned no JSON")
	}
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(output, &entries); err != nil {
		return fmt.Errorf("parse grok mcp list --json: %w", err)
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name, "witself") {
			return errors.New("effective Grok MCP registration still contains witself")
		}
	}
	return nil
}

func convergeGrokEffectiveMCP(
	runtimeCLI string,
	cfg transcriptcapture.Config,
	expected genericMCPBinding,
	expectedSet bool,
	expectedNonTarget string,
) error {
	currentBinding, currentSet, current, err := inspectGenericMCP(cfg)
	if err != nil {
		return err
	}
	if current.nonTarget != expectedNonTarget {
		return errors.New("the Grok non-target configuration changed during effective-state recovery")
	}
	attempted, err := genericMCPBindingFromConfig(cfg)
	if err != nil {
		return err
	}
	if currentSet && !equalGenericMCPBinding(currentBinding, attempted) &&
		(!expectedSet || !equalGenericMCPBinding(currentBinding, expected)) {
		return errors.New("the Grok MCP server witself changed to a foreign binding during effective-state recovery")
	}
	if expectedSet && currentSet && equalGenericMCPBinding(currentBinding, expected) {
		if _, err := verifyGrokNativeMCPBindingForConfig(runtimeCLI, cfg, expected); err == nil {
			return nil
		}
	}
	if currentSet {
		if err := genericSnapshotStillCurrent(cfg, current); err != nil {
			return err
		}
		if err := removeGenericMCPUnchecked(runtimeCLI, cfg); err != nil {
			return err
		}
		_, stillSet, afterRemove, err := inspectGenericMCP(cfg)
		if err != nil {
			return err
		}
		if stillSet || afterRemove.nonTarget != expectedNonTarget {
			return errors.New("the Grok MCP removal changed non-target state during effective-state recovery")
		}
		current = afterRemove
	}
	if !expectedSet {
		if err := verifyGrokNativeMCPAbsenceForConfig(runtimeCLI, cfg); err != nil {
			output, removeErr := runGenericProviderCLI(runtimeCLI, cfg, 15*time.Second, "mcp", "remove", "--scope", "user", "witself")
			if removeErr != nil {
				return errors.Join(err, fmt.Errorf("remove stale effective Grok MCP registration: %w: %s", removeErr, strings.TrimSpace(string(output))))
			}
			_, stillSet, afterRemove, inspectErr := inspectGenericMCP(cfg)
			if inspectErr != nil {
				return inspectErr
			}
			if stillSet || afterRemove.nonTarget != expectedNonTarget {
				return errors.New("removing stale effective Grok MCP state changed provider config")
			}
		}
		return verifyGrokNativeMCPAbsenceForConfig(runtimeCLI, cfg)
	}
	if err := genericSnapshotStillCurrent(cfg, current); err != nil {
		return err
	}
	if err := addGenericMCPUnchecked(runtimeCLI, cfg, expected); err != nil {
		return err
	}
	restored, restoredSet, afterAdd, err := inspectGenericMCP(cfg)
	if err != nil {
		return err
	}
	if !restoredSet || !equalGenericMCPBinding(restored, expected) || afterAdd.nonTarget != expectedNonTarget {
		return errors.New("the Grok MCP add did not restore the exact effective-state binding")
	}
	_, err = verifyGrokNativeMCPBindingForConfig(runtimeCLI, cfg, expected)
	return err
}
