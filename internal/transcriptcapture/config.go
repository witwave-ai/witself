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

const SchemaVersion = "witself.capture.v1"

const (
	RuntimeCodex      = "codex"
	RuntimeClaudeCode = "claude-code"

	ModeMessages = "messages"
	ModeTrace    = "trace"
	ModeRaw      = "raw"
)

var locationNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Location identifies one local installation without relying on mutable host
// names, IP addresses, or filesystem paths.
type Location struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Config is the non-secret integration binding for one runtime.
type Config struct {
	SchemaVersion string    `json:"schema_version"`
	Runtime       string    `json:"runtime"`
	CaptureMode   string    `json:"capture_mode"`
	Account       string    `json:"account"`
	Realm         string    `json:"realm"`
	Agent         string    `json:"agent"`
	AgentID       string    `json:"agent_id"`
	AgentName     string    `json:"agent_name"`
	Endpoint      string    `json:"endpoint,omitempty"`
	TokenFile     string    `json:"token_file,omitempty"`
	Location      Location  `json:"location"`
	InstalledAt   time.Time `json:"installed_at"`
}

// NormalizeRuntime returns the stable runtime namespace used in transcript ids.
func NormalizeRuntime(runtime string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case RuntimeCodex:
		return RuntimeCodex, nil
	case "claude", RuntimeClaudeCode:
		return RuntimeClaudeCode, nil
	default:
		return "", fmt.Errorf("runtime must be %s or %s", RuntimeCodex, RuntimeClaudeCode)
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

// EnsureLocation loads this installation's stable id and updates its label.
func EnsureLocation(name string) (Location, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = "default"
	}
	if !locationNamePattern.MatchString(name) {
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
	loc.Name = name
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
	if strings.TrimSpace(cfg.Agent) == "" || strings.TrimSpace(cfg.AgentID) == "" || strings.TrimSpace(cfg.AgentName) == "" {
		return errors.New("agent, agent_id, and agent_name are required")
	}
	if cfg.Location.ID == "" || cfg.Location.Name == "" {
		return errors.New("location is required")
	}
	cfg.SchemaVersion = SchemaVersion
	cfg.Runtime = runtime
	cfg.CaptureMode = mode
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
	return cfg, nil
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
