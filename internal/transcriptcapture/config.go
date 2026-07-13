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
	RuntimeCodex      = "codex"
	RuntimeClaudeCode = "claude-code"
	RuntimeGrokBuild  = "grok-build"
	RuntimeCursor     = "cursor"

	ModeMessages = "messages"
	ModeTrace    = "trace"
	ModeRaw      = "raw"

	HookModeUser    = "user"
	HookModeManaged = "managed"
)

var locationNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Location identifies one local installation without relying on mutable host
// names, IP addresses, or filesystem paths.
type Location struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Config is the non-secret integration binding for one runtime.
type Config struct {
	SchemaVersion  string    `json:"schema_version"`
	Runtime        string    `json:"runtime"`
	RuntimeVersion string    `json:"runtime_version,omitempty"`
	CaptureMode    string    `json:"capture_mode"`
	HookMode       string    `json:"hook_mode"`
	Account        string    `json:"account"`
	AccountID      string    `json:"account_id,omitempty"`
	Realm          string    `json:"realm"`
	RealmID        string    `json:"realm_id,omitempty"`
	Agent          string    `json:"agent"`
	AgentID        string    `json:"agent_id"`
	AgentName      string    `json:"agent_name"`
	Endpoint       string    `json:"endpoint,omitempty"`
	TokenFile      string    `json:"token_file,omitempty"`
	Location       Location  `json:"location"`
	InstalledAt    time.Time `json:"installed_at"`
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
	default:
		return "", fmt.Errorf("runtime must be %s, %s, %s, or %s", RuntimeCodex, RuntimeClaudeCode, RuntimeGrokBuild, RuntimeCursor)
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
	case HookModeManaged:
		return HookModeManaged, nil
	default:
		return "", fmt.Errorf("hook mode must be %s or %s", HookModeUser, HookModeManaged)
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
	if cfg.SchemaVersion != SchemaVersion {
		return Config{}, fmt.Errorf("unsupported integration config schema %q", cfg.SchemaVersion)
	}
	cfg.HookMode, err = NormalizeHookMode(cfg.HookMode)
	if err != nil {
		return Config{}, fmt.Errorf("parse integration config %s: %w", path, err)
	}
	return cfg, nil
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
