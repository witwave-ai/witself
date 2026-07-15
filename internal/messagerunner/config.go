package messagerunner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/local"
	"golang.org/x/sys/unix"
)

// RunnerConfigSchemaV1 identifies the private value-free local configuration.
const (
	RunnerConfigSchemaV1     = "witself.message-runner.v1"
	maximumRunnerConfigBytes = 64 * 1024
)

var (
	// ErrRunnerNotConfigured reports that no local configuration exists.
	ErrRunnerNotConfigured = errors.New("message runner is not configured")
	// ErrRunnerBindingConflict prevents silently rebinding an existing runner.
	ErrRunnerBindingConflict = errors.New("message runner binding conflict")
	runnerRuntimePattern     = regexp.MustCompile(`^(codex|claude-code|grok-build|cursor)$`)
	runnerProviderPattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
)

// Settings contains value-free local runner policy. ProviderPath is an
// executable path, never a credential or prompt path.
type Settings struct {
	Runtime        string
	AccountID      string
	RealmID        string
	AgentID        string
	AgentName      string
	Provider       string
	ProviderPath   string
	Model          string
	MaximumTurns   int
	ReplaceBinding bool
}

// PersistedConfig is deliberately incapable of containing a Witself token,
// message content, provider output, or model credential.
type PersistedConfig struct {
	Schema       string    `json:"schema"`
	Enabled      bool      `json:"enabled"`
	Revision     int64     `json:"revision"`
	RunnerID     string    `json:"runner_id"`
	Runtime      string    `json:"runtime"`
	AccountID    string    `json:"account_id"`
	RealmID      string    `json:"realm_id"`
	AgentID      string    `json:"agent_id"`
	AgentName    string    `json:"agent_name"`
	Provider     string    `json:"provider"`
	ProviderPath string    `json:"provider_path,omitempty"`
	Model        string    `json:"model,omitempty"`
	MaximumTurns int       `json:"maximum_turns"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ConfigStore owns one runtime's private, value-free runner configuration and
// local single-instance lock.
type ConfigStore struct {
	Runtime string
	Root    string
	Now     func() time.Time
	NewID   func(string) (string, error)
}

// DefaultConfigStore resolves one runtime's store under WITSELF_HOME.
func DefaultConfigStore(runtimeName string) (ConfigStore, error) {
	home, err := local.Home()
	if err != nil {
		return ConfigStore{}, err
	}
	return NewConfigStore(filepath.Join(home, "message-runners"), runtimeName)
}

// NewConfigStore constructs a store rooted at an explicit directory.
func NewConfigStore(root, runtimeName string) (ConfigStore, error) {
	runtimeName = strings.ToLower(strings.TrimSpace(runtimeName))
	if !runnerRuntimePattern.MatchString(runtimeName) {
		return ConfigStore{}, fmt.Errorf("%w: unsupported runtime %q", ErrInvalidConfiguration, runtimeName)
	}
	if strings.TrimSpace(root) == "" {
		return ConfigStore{}, fmt.Errorf("%w: runner config root is required", ErrInvalidConfiguration)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return ConfigStore{}, err
	}
	return ConfigStore{Runtime: runtimeName, Root: root, Now: time.Now, NewID: id.New}, nil
}

// Enable validates and atomically persists an enabled identity-pinned policy.
func (s ConfigStore) Enable(settings Settings) (PersistedConfig, error) {
	settings.Runtime = strings.ToLower(strings.TrimSpace(settings.Runtime))
	if settings.Runtime == "" {
		settings.Runtime = s.Runtime
	}
	if settings.Runtime != s.Runtime {
		return PersistedConfig{}, fmt.Errorf("%w: settings runtime does not match store", ErrInvalidConfiguration)
	}
	settings.AccountID = strings.TrimSpace(settings.AccountID)
	settings.RealmID = strings.TrimSpace(settings.RealmID)
	settings.AgentID = strings.TrimSpace(settings.AgentID)
	settings.AgentName = strings.TrimSpace(settings.AgentName)
	settings.Provider = strings.ToLower(strings.TrimSpace(settings.Provider))
	settings.ProviderPath = strings.TrimSpace(settings.ProviderPath)
	settings.Model = strings.TrimSpace(settings.Model)
	if settings.MaximumTurns == 0 {
		settings.MaximumTurns = 12
	}
	if err := validateSettings(settings); err != nil {
		return PersistedConfig{}, err
	}

	existing, err := s.Load()
	switch {
	case err == nil:
	case errors.Is(err, ErrRunnerNotConfigured):
		existing = PersistedConfig{}
	default:
		return PersistedConfig{}, err
	}
	sameBinding := existing.AccountID == settings.AccountID && existing.RealmID == settings.RealmID &&
		existing.AgentID == settings.AgentID
	if existing.RunnerID != "" && !sameBinding && !settings.ReplaceBinding {
		return PersistedConfig{}, ErrRunnerBindingConflict
	}

	now := s.Now().UTC()
	runnerID := existing.RunnerID
	createdAt := existing.CreatedAt
	revision := existing.Revision + 1
	if runnerID == "" || !sameBinding {
		runnerID, err = s.NewID("mrn")
		if err != nil {
			return PersistedConfig{}, err
		}
		createdAt = now
		revision = 1
	}
	config := PersistedConfig{
		Schema: RunnerConfigSchemaV1, Enabled: true, Revision: revision,
		RunnerID: runnerID, Runtime: s.Runtime,
		AccountID: settings.AccountID, RealmID: settings.RealmID,
		AgentID: settings.AgentID, AgentName: settings.AgentName,
		Provider: settings.Provider, ProviderPath: settings.ProviderPath, Model: settings.Model,
		MaximumTurns: settings.MaximumTurns,
		CreatedAt:    createdAt, UpdatedAt: now,
	}
	if err := validatePersistedConfig(config); err != nil {
		return PersistedConfig{}, err
	}
	writeConfig := func() error { return writePrivateJSONAtomic(s.configPath(), config) }
	if existing.RunnerID != "" && !sameBinding {
		err = s.replaceBindingConfig(config)
	} else {
		err = writeConfig()
	}
	if err != nil {
		return PersistedConfig{}, err
	}
	return config, nil
}

// Disable atomically marks an existing runner configuration disabled.
func (s ConfigStore) Disable() (PersistedConfig, error) {
	config, err := s.Load()
	if err != nil {
		return PersistedConfig{}, err
	}
	if !config.Enabled {
		return config, nil
	}
	config.Enabled = false
	config.Revision++
	config.UpdatedAt = s.Now().UTC()
	if err := writePrivateJSONAtomic(s.configPath(), config); err != nil {
		return PersistedConfig{}, err
	}
	return config, nil
}

// Load reads and strictly validates one private runner configuration.
func (s ConfigStore) Load() (PersistedConfig, error) {
	path := s.configPath()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return PersistedConfig{}, ErrRunnerNotConfigured
	}
	if err != nil {
		return PersistedConfig{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return PersistedConfig{}, fmt.Errorf("message runner config %s must be a private regular file", path)
	}
	if info.Size() > maximumRunnerConfigBytes {
		return PersistedConfig{}, errors.New("message runner config exceeds its size limit")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return PersistedConfig{}, err
	}
	var config PersistedConfig
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return PersistedConfig{}, fmt.Errorf("parse message runner config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return PersistedConfig{}, errors.New("parse message runner config: trailing data")
	}
	if err := validatePersistedConfig(config); err != nil {
		return PersistedConfig{}, err
	}
	return config, nil
}

// Acquire obtains this runtime's non-blocking local singleton lock.
func (s ConfigStore) Acquire() (release func() error, acquired bool, err error) {
	path := s.lockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return func() error { return nil }, false, nil
		}
		return nil, false, err
	}
	release = func() error {
		unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
		return errors.Join(unlockErr, file.Close())
	}
	return release, true, nil
}

func (s ConfigStore) configPath() string {
	return filepath.Join(s.Root, s.Runtime, "config.json")
}

func (s ConfigStore) lockPath() string {
	return filepath.Join(s.Root, s.Runtime, "runner.lock")
}

func validateSettings(settings Settings) error {
	if !runnerRuntimePattern.MatchString(settings.Runtime) || settings.AccountID == "" || settings.RealmID == "" ||
		settings.AgentID == "" || settings.AgentName == "" || !runnerProviderPattern.MatchString(settings.Provider) {
		return fmt.Errorf("%w: runner binding and provider are required", ErrInvalidConfiguration)
	}
	for _, value := range []string{settings.AccountID, settings.RealmID, settings.AgentID, settings.AgentName, settings.Model} {
		if len(value) > 256 {
			return fmt.Errorf("%w: runner setting exceeds its size limit", ErrInvalidConfiguration)
		}
	}
	if settings.ProviderPath != "" && (!filepath.IsAbs(settings.ProviderPath) || len(settings.ProviderPath) > 4096) {
		return fmt.Errorf("%w: provider path must be absolute", ErrInvalidConfiguration)
	}
	if settings.MaximumTurns < 1 || settings.MaximumTurns > 64 {
		return fmt.Errorf("%w: maximum turns must be 1-64", ErrInvalidConfiguration)
	}
	return nil
}

func validatePersistedConfig(config PersistedConfig) error {
	if config.Schema != RunnerConfigSchemaV1 || config.Revision < 1 ||
		!strings.HasPrefix(config.RunnerID, "mrn_") || config.CreatedAt.IsZero() || config.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: persisted runner config is invalid", ErrInvalidConfiguration)
	}
	return validateSettings(Settings{
		Runtime: config.Runtime, AccountID: config.AccountID, RealmID: config.RealmID,
		AgentID: config.AgentID, AgentName: config.AgentName, Provider: config.Provider,
		ProviderPath: config.ProviderPath, Model: config.Model,
		MaximumTurns: config.MaximumTurns,
	})
}

func writePrivateJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".message-runner-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(raw, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
