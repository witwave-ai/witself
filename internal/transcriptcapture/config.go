// Package transcriptcapture adapts local agent-runtime hooks into the portable
// Witself transcript ledger without putting network latency on the hook path.
package transcriptcapture

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/local"
)

// SchemaVersion identifies the durable local capture envelope.
const SchemaVersion = "witself.capture.v1"

// Runtime and capture-mode names form the local integration contract.
const (
	RuntimeCodex       = "codex"
	RuntimeClaudeCode  = "claude-code"
	RuntimeGrokBuild   = "grok-build"
	RuntimeCursor      = "cursor"
	RuntimeOpenClaw    = "openclaw"
	RuntimeAntigravity = "antigravity"
	// HookEventCodexPermissionReview is Codex's normalized internal approval-review event.
	HookEventCodexPermissionReview = "PermissionReview"

	ModeMessages = "messages"
	ModeTrace    = "trace"
	ModeRaw      = "raw"

	HookModeNone    = "none"
	HookModeUser    = "user"
	HookModeManaged = "managed"
)

// SupportedRuntimes returns the canonical runtime names in stable display
// order. Callers may mutate the returned slice without affecting later calls.
func SupportedRuntimes() []string {
	return []string{
		RuntimeCodex,
		RuntimeClaudeCode,
		RuntimeGrokBuild,
		RuntimeCursor,
		RuntimeOpenClaw,
		RuntimeAntigravity,
	}
}

var (
	locationNamePattern            = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	antigravityPluginDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	antigravityPluginNamePattern   = regexp.MustCompile(`^witself-managed-[0-9a-f]{24}$`)
)

// Location identifies one local installation without relying on mutable host
// names, IP addresses, or filesystem paths.
type Location struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Config is the non-secret integration binding for one runtime.
type Config struct {
	SchemaVersion            string            `json:"schema_version"`
	Runtime                  string            `json:"runtime"`
	RuntimeVersion           string            `json:"runtime_version,omitempty"`
	RuntimeCLICommand        string            `json:"runtime_cli_command,omitempty"`
	MCPCommand               string            `json:"mcp_command,omitempty"`
	MCPEnvironment           map[string]string `json:"mcp_environment,omitempty"`
	MCPConnectTimeoutSeconds int               `json:"mcp_connect_timeout_seconds,omitempty"`
	RuntimeWorkspace         string            `json:"runtime_workspace,omitempty"`
	RuntimeAgentID           string            `json:"runtime_agent_id,omitempty"`
	RuntimeConfigRoot        string            `json:"runtime_config_root,omitempty"`
	RuntimeMCPConfigPath     string            `json:"runtime_mcp_config_path,omitempty"`
	RuntimePluginPath        string            `json:"runtime_plugin_path,omitempty"`
	RuntimePluginSource      string            `json:"runtime_plugin_source,omitempty"`
	RuntimePluginDigest      string            `json:"runtime_plugin_digest,omitempty"`
	CaptureMode              string            `json:"capture_mode"`
	HookMode                 string            `json:"hook_mode"`
	Account                  string            `json:"account"`
	AccountID                string            `json:"account_id,omitempty"`
	Realm                    string            `json:"realm"`
	RealmID                  string            `json:"realm_id,omitempty"`
	Agent                    string            `json:"agent"`
	AgentID                  string            `json:"agent_id"`
	AgentName                string            `json:"agent_name"`
	Endpoint                 string            `json:"endpoint,omitempty"`
	TokenFile                string            `json:"token_file,omitempty"`
	ManagedPermissions       []string          `json:"managed_permissions,omitempty"`
	Location                 Location          `json:"location"`
	InstalledAt              time.Time         `json:"installed_at"`
}

// NormalizeRuntime returns the stable runtime namespace used in transcript ids.
func NormalizeRuntime(runtime string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case RuntimeCodex:
		return RuntimeCodex, nil
	case "claude", RuntimeClaudeCode:
		return RuntimeClaudeCode, nil
	case "grok", RuntimeGrokBuild:
		return RuntimeGrokBuild, nil
	case RuntimeCursor:
		return RuntimeCursor, nil
	case RuntimeOpenClaw:
		return RuntimeOpenClaw, nil
	case "agy", RuntimeAntigravity:
		return RuntimeAntigravity, nil
	default:
		return "", fmt.Errorf("runtime must be %s, %s, %s, %s, %s, or %s", RuntimeCodex, RuntimeClaudeCode, RuntimeGrokBuild, RuntimeCursor, RuntimeOpenClaw, RuntimeAntigravity)
	}
}

// NormalizeMode validates a capture policy.
func NormalizeMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ModeMessages:
		return ModeMessages, nil
	case ModeTrace:
		return ModeTrace, nil
	case ModeRaw:
		return ModeRaw, nil
	default:
		return "", fmt.Errorf("capture mode must be %s, %s, or %s", ModeMessages, ModeTrace, ModeRaw)
	}
}

// NormalizeHookMode validates where runtime hooks are installed.
func NormalizeHookMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", HookModeUser:
		return HookModeUser, nil
	case HookModeNone:
		return HookModeNone, nil
	case HookModeManaged:
		return HookModeManaged, nil
	default:
		return "", fmt.Errorf("hook mode must be %s, %s, or %s", HookModeNone, HookModeUser, HookModeManaged)
	}
}

// EnsureLocation loads this installation's stable id and updates its label.
func EnsureLocation(name string) (Location, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name != "" && !locationNamePattern.MatchString(name) {
		return Location{}, fmt.Errorf("invalid location %q (use lowercase letters, digits, and hyphens)", name)
	}
	path, err := locationPath()
	if err != nil {
		return Location{}, err
	}
	var loc Location
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &loc); err != nil {
			return Location{}, fmt.Errorf("parse %s: %w", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return Location{}, err
	}
	if loc.ID == "" {
		loc.ID, err = id.New("loc")
		if err != nil {
			return Location{}, err
		}
	}
	if name != "" {
		loc.Name = name
	}
	if err := writeJSONAtomic(path, loc); err != nil {
		return Location{}, err
	}
	return loc, nil
}

// SaveConfig writes one runtime binding under the Witself home.
func SaveConfig(cfg Config) error {
	runtime, err := NormalizeRuntime(cfg.Runtime)
	if err != nil {
		return err
	}
	mode, err := NormalizeMode(cfg.CaptureMode)
	if err != nil {
		return err
	}
	hookMode, err := NormalizeHookMode(cfg.HookMode)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Agent) == "" || strings.TrimSpace(cfg.AgentID) == "" || strings.TrimSpace(cfg.AgentName) == "" {
		return errors.New("agent, agent_id, and agent_name are required")
	}
	if cfg.Location.ID == "" {
		return errors.New("location is required")
	}
	if cfg.Location.Name != "" && !locationNamePattern.MatchString(cfg.Location.Name) {
		return fmt.Errorf("invalid location %q (use lowercase letters, digits, and hyphens)", cfg.Location.Name)
	}
	cfg.SchemaVersion = SchemaVersion
	cfg.Runtime = runtime
	cfg.RuntimeVersion = strings.TrimSpace(cfg.RuntimeVersion)
	if len(cfg.RuntimeVersion) > 256 {
		return errors.New("runtime_version must be 256 bytes or fewer")
	}
	cfg.MCPCommand = strings.TrimSpace(cfg.MCPCommand)
	if cfg.MCPCommand != "" && (!filepath.IsAbs(cfg.MCPCommand) || filepath.Clean(cfg.MCPCommand) != cfg.MCPCommand) {
		return errors.New("mcp_command must be a clean absolute path")
	}
	cfg.RuntimeWorkspace = strings.TrimSpace(cfg.RuntimeWorkspace)
	cfg.RuntimeAgentID = strings.TrimSpace(cfg.RuntimeAgentID)
	cfg.RuntimeCLICommand = strings.TrimSpace(cfg.RuntimeCLICommand)
	cfg.RuntimeConfigRoot = strings.TrimSpace(cfg.RuntimeConfigRoot)
	cfg.RuntimeMCPConfigPath = strings.TrimSpace(cfg.RuntimeMCPConfigPath)
	cfg.RuntimePluginPath = strings.TrimSpace(cfg.RuntimePluginPath)
	cfg.RuntimePluginSource = strings.TrimSpace(cfg.RuntimePluginSource)
	cfg.RuntimePluginDigest = strings.TrimSpace(cfg.RuntimePluginDigest)
	if err := validateRuntimeIntegrationFields(runtime, hookMode, cfg.RuntimeCLICommand, cfg.MCPCommand, cfg.MCPEnvironment, cfg.MCPConnectTimeoutSeconds, cfg.RuntimeWorkspace, cfg.RuntimeAgentID, cfg.RuntimeConfigRoot, cfg.RuntimeMCPConfigPath, cfg.RuntimePluginPath, cfg.RuntimePluginSource, cfg.RuntimePluginDigest); err != nil {
		return err
	}
	cfg.CaptureMode = mode
	cfg.HookMode = hookMode
	if cfg.Account == "" {
		cfg.Account = "default"
	}
	if cfg.Realm == "" {
		cfg.Realm = "default"
	}
	if cfg.InstalledAt.IsZero() {
		cfg.InstalledAt = time.Now().UTC()
	}
	path, err := ConfigPath(runtime)
	if err != nil {
		return err
	}
	return writeJSONAtomic(path, cfg)
}

// LoadConfig reads one installed runtime binding.
func LoadConfig(runtime string) (Config, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return Config{}, err
	}
	path, err := ConfigPath(runtime)
	if err != nil {
		return Config{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read integration config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse integration config %s: %w", path, err)
	}
	storedRuntime, err := NormalizeRuntime(cfg.Runtime)
	if err != nil {
		return Config{}, fmt.Errorf("parse integration config %s: %w", path, err)
	}
	if storedRuntime != runtime {
		return Config{}, fmt.Errorf("parse integration config %s: stored runtime %q does not match requested runtime %q", path, storedRuntime, runtime)
	}
	cfg.Runtime = storedRuntime
	if cfg.SchemaVersion != SchemaVersion {
		return Config{}, fmt.Errorf("unsupported integration config schema %q", cfg.SchemaVersion)
	}
	cfg.HookMode, err = NormalizeHookMode(cfg.HookMode)
	if err != nil {
		return Config{}, fmt.Errorf("parse integration config %s: %w", path, err)
	}
	cfg.MCPCommand = strings.TrimSpace(cfg.MCPCommand)
	cfg.RuntimeWorkspace = strings.TrimSpace(cfg.RuntimeWorkspace)
	cfg.RuntimeAgentID = strings.TrimSpace(cfg.RuntimeAgentID)
	cfg.RuntimeCLICommand = strings.TrimSpace(cfg.RuntimeCLICommand)
	cfg.RuntimeConfigRoot = strings.TrimSpace(cfg.RuntimeConfigRoot)
	cfg.RuntimeMCPConfigPath = strings.TrimSpace(cfg.RuntimeMCPConfigPath)
	cfg.RuntimePluginPath = strings.TrimSpace(cfg.RuntimePluginPath)
	cfg.RuntimePluginSource = strings.TrimSpace(cfg.RuntimePluginSource)
	cfg.RuntimePluginDigest = strings.TrimSpace(cfg.RuntimePluginDigest)
	if err := validateRuntimeIntegrationFields(runtime, cfg.HookMode, cfg.RuntimeCLICommand, cfg.MCPCommand, cfg.MCPEnvironment, cfg.MCPConnectTimeoutSeconds, cfg.RuntimeWorkspace, cfg.RuntimeAgentID, cfg.RuntimeConfigRoot, cfg.RuntimeMCPConfigPath, cfg.RuntimePluginPath, cfg.RuntimePluginSource, cfg.RuntimePluginDigest); err != nil {
		return Config{}, fmt.Errorf("parse integration config %s: %w", path, err)
	}
	return cfg, nil
}

func validateRuntimeIntegrationFields(runtime, hookMode, runtimeCLICommand, mcpCommand string, mcpEnvironment map[string]string, mcpConnectTimeoutSeconds int, workspace, runtimeAgentID, configRoot, mcpConfigPath, pluginPath, pluginSource, pluginDigest string) error {
	if runtimeCLICommand != "" && (!filepath.IsAbs(runtimeCLICommand) || filepath.Clean(runtimeCLICommand) != runtimeCLICommand) {
		return errors.New("runtime_cli_command must be a clean absolute path")
	}
	if mcpCommand != "" && (!filepath.IsAbs(mcpCommand) || filepath.Clean(mcpCommand) != mcpCommand) {
		return errors.New("mcp_command must be a clean absolute path")
	}
	if workspace != "" && (!filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace) {
		return errors.New("runtime_workspace must be a clean absolute path")
	}
	for field, value := range map[string]string{
		"runtime_config_root":     configRoot,
		"runtime_mcp_config_path": mcpConfigPath,
		"runtime_plugin_path":     pluginPath,
		"runtime_plugin_source":   pluginSource,
	} {
		if value != "" && (len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") || !filepath.IsAbs(value) || filepath.Clean(value) != value) {
			return fmt.Errorf("%s must be a clean absolute path", field)
		}
	}
	if runtime == RuntimeOpenClaw {
		if configRoot != "" || mcpConfigPath != "" || pluginPath != "" || pluginSource != "" || pluginDigest != "" {
			return errors.New("runtime plugin fields are not supported for OpenClaw")
		}
		if hookMode != HookModeNone {
			return errors.New("openclaw hook_mode must be none")
		}
		if mcpCommand == "" {
			return errors.New("mcp_command is required for OpenClaw")
		}
		if runtimeCLICommand == "" {
			return errors.New("runtime_cli_command is required for OpenClaw")
		}
		if workspace == "" {
			return errors.New("runtime_workspace is required for OpenClaw")
		}
		if strings.TrimSpace(runtimeAgentID) == "" {
			return errors.New("runtime_agent_id is required for OpenClaw")
		}
		if err := validateOpenClawMCPEnvironment(mcpEnvironment); err != nil {
			return err
		}
		if mcpConnectTimeoutSeconds <= 0 || mcpConnectTimeoutSeconds > 3600 {
			return errors.New("mcp_connect_timeout_seconds must be between 1 and 3600 for OpenClaw")
		}
		return nil
	}
	if runtime == RuntimeAntigravity {
		if hookMode != HookModeNone {
			return errors.New("antigravity hook_mode must be none")
		}
		if runtimeCLICommand == "" {
			return errors.New("runtime_cli_command is required for Antigravity")
		}
		if mcpCommand == "" {
			return errors.New("mcp_command is required for Antigravity")
		}
		if configRoot == "" || pluginPath == "" || pluginSource == "" {
			return errors.New("runtime_config_root, runtime_plugin_path, and runtime_plugin_source are required for Antigravity")
		}
		if mcpConfigPath != "" && mcpConfigPath != filepath.Join(configRoot, "mcp_config.json") {
			return errors.New("runtime_mcp_config_path must be the canonical Antigravity MCP config under runtime_config_root")
		}
		if filepath.Dir(pluginPath) != filepath.Join(configRoot, "plugins") || !antigravityPluginNamePattern.MatchString(filepath.Base(pluginPath)) {
			return errors.New("runtime_plugin_path must be a collision-resistant Witself-managed plugin under runtime_config_root")
		}
		if !antigravityPluginDigestPattern.MatchString(pluginDigest) {
			return errors.New("runtime_plugin_digest must be a lowercase SHA-256 digest for Antigravity")
		}
		if workspace != "" || strings.TrimSpace(runtimeAgentID) != "" {
			return errors.New("runtime_workspace and runtime_agent_id are not supported for Antigravity")
		}
		if mcpConnectTimeoutSeconds != 0 {
			return errors.New("mcp_connect_timeout_seconds is not supported for Antigravity")
		}
		if err := validateAntigravityMCPEnvironment(mcpEnvironment); err != nil {
			return err
		}
		expectedSource := filepath.Join(mcpEnvironment["WITSELF_HOME"], "integrations", RuntimeAntigravity, "bundles", pluginDigest)
		if pluginSource != expectedSource {
			return errors.New("runtime_plugin_source must be the digest-addressed Antigravity bundle under WITSELF_HOME")
		}
		return nil
	}
	if configRoot != "" || mcpConfigPath != "" || pluginPath != "" || pluginSource != "" || pluginDigest != "" {
		return fmt.Errorf("runtime plugin fields are not supported for %s", runtime)
	}
	if len(mcpEnvironment) != 0 {
		return fmt.Errorf("mcp_environment is not supported for %s", runtime)
	}
	if mcpConnectTimeoutSeconds != 0 {
		return fmt.Errorf("mcp_connect_timeout_seconds is not supported for %s", runtime)
	}
	if hookMode == HookModeNone {
		return fmt.Errorf("hook_mode none is not supported for %s", runtime)
	}
	return nil
}

func validateAntigravityMCPEnvironment(environment map[string]string) error {
	if len(environment) != 1 {
		return errors.New("mcp_environment must contain only WITSELF_HOME for Antigravity")
	}
	home := environment["WITSELF_HOME"]
	if home == "" || len(home) > 4096 || strings.ContainsAny(home, "\x00\r\n") || !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return errors.New("mcp_environment WITSELF_HOME must be a clean absolute path for Antigravity")
	}
	return nil
}

func validateOpenClawMCPEnvironment(environment map[string]string) error {
	if strings.TrimSpace(environment["WITSELF_HOME"]) == "" {
		return errors.New("mcp_environment WITSELF_HOME is required for OpenClaw")
	}
	if environment["OPENCLAW_PROFILE"] != "" && (environment["OPENCLAW_CONFIG_PATH"] == "" || environment["OPENCLAW_STATE_DIR"] == "") {
		return errors.New("mcp_environment OPENCLAW_PROFILE requires OPENCLAW_CONFIG_PATH and OPENCLAW_STATE_DIR")
	}
	pathKeys := map[string]bool{
		"WITSELF_HOME":         true,
		"OPENCLAW_STATE_DIR":   true,
		"OPENCLAW_CONFIG_PATH": true,
	}
	for key, value := range environment {
		if !pathKeys[key] && key != "OPENCLAW_PROFILE" {
			return fmt.Errorf("mcp_environment key %q is not allowed for OpenClaw", key)
		}
		if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("mcp_environment value for %s is invalid", key)
		}
		if pathKeys[key] && (!filepath.IsAbs(value) || filepath.Clean(value) != value) {
			return fmt.Errorf("mcp_environment value for %s must be a clean absolute path", key)
		}
		if key == "OPENCLAW_PROFILE" {
			if len(value) > 64 || !isOpenClawProfileLetterOrDigit(value[0]) {
				return errors.New("mcp_environment value for OPENCLAW_PROFILE is invalid")
			}
			for _, character := range []byte(value) {
				if !isOpenClawProfileLetterOrDigit(character) && character != '-' && character != '_' {
					return errors.New("mcp_environment value for OPENCLAW_PROFILE is invalid")
				}
			}
		}
	}
	return nil
}

func isOpenClawProfileLetterOrDigit(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9')
}

// RemoveConfig removes one runtime binding without touching tokens or pending
// transcript events.
func RemoveConfig(runtime string) error {
	path, err := ConfigPath(runtime)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Dir(path))
	return nil
}

// ConfigPath returns one runtime's non-secret binding path.
func ConfigPath(runtime string) (string, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return "", err
	}
	home, err := local.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "integrations", runtime, "config.json"), nil
}

func locationPath() (string, error) {
	home, err := local.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "location.json"), nil
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
