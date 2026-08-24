// Package supportrunner implements the dark, operator-run AI support first
// responder. It talks only to the fleet-admin HTTP surface and never imports
// cell storage or billing implementation packages.
package supportrunner

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	enabledEnv             = "WITSELF_SUPPORT_RUNNER_ENABLED"
	controlPlaneEnv        = "WITSELF_SUPPORT_RUNNER_CONTROL_PLANE"
	adminTokenFileEnv      = "WITSELF_SUPPORT_RUNNER_ADMIN_TOKEN_FILE"
	adminTokenEnv          = "WITSELF_SUPPORT_RUNNER_ADMIN_TOKEN"
	anthropicAPIKeyFileEnv = "WITSELF_SUPPORT_RUNNER_ANTHROPIC_API_KEY_FILE"
	anthropicAPIKeyEnv     = "WITSELF_SUPPORT_RUNNER_ANTHROPIC_API_KEY"
	modelEnv               = "WITSELF_SUPPORT_RUNNER_MODEL"
	intervalEnv            = "WITSELF_SUPPORT_RUNNER_INTERVAL"
	maxTicketsPerTickEnv   = "WITSELF_SUPPORT_RUNNER_MAX_TICKETS_PER_TICK"
	llmTimeoutEnv          = "WITSELF_SUPPORT_RUNNER_LLM_TIMEOUT"
	maxAssistantRepliesEnv = "WITSELF_SUPPORT_RUNNER_MAX_ASSISTANT_REPLIES"
	lookbackEnv            = "WITSELF_SUPPORT_RUNNER_LOOKBACK"

	defaultControlPlane        = "https://self.witwave.ai"
	defaultModel               = "claude-opus-5"
	defaultInterval            = 60 * time.Second
	defaultMaxTicketsPerTick   = 5
	defaultLLMTimeout          = 120 * time.Second
	defaultMaxAssistantReplies = 3
	defaultLookback            = 720 * time.Hour
)

// ErrDisabled is returned by New when the dark enablement gate is not set.
// Callers must not construct alternate dependencies after receiving it.
var ErrDisabled = errors.New("support runner is disabled")

// Config controls the support-runner polling and inference bounds. Credential
// values are deliberately private and are consumed only by New.
type Config struct {
	Enabled             bool
	ControlPlane        string
	Model               string
	Interval            time.Duration
	MaxTicketsPerTick   int
	LLMTimeout          time.Duration
	MaxAssistantReplies int
	Lookback            time.Duration

	adminToken      string
	anthropicAPIKey string
}

// FromEnv parses support-runner configuration using lookup. Credential files
// take precedence over inline values and are read only after the explicit dark
// gate is enabled.
func FromEnv(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("support runner environment lookup is nil")
	}

	cfg := Config{
		ControlPlane:        defaultControlPlane,
		Model:               defaultModel,
		Interval:            defaultInterval,
		MaxTicketsPerTick:   defaultMaxTicketsPerTick,
		LLMTimeout:          defaultLLMTimeout,
		MaxAssistantReplies: defaultMaxAssistantReplies,
		Lookback:            defaultLookback,
	}

	var err error
	cfg.Enabled, err = explicitBool(lookup, enabledEnv)
	if err != nil {
		return Config{}, err
	}
	cfg.ControlPlane = envOr(lookup, controlPlaneEnv, cfg.ControlPlane)
	cfg.Model = envOr(lookup, modelEnv, cfg.Model)
	if cfg.Interval, err = durationEnv(lookup, intervalEnv, cfg.Interval); err != nil {
		return Config{}, err
	}
	if cfg.MaxTicketsPerTick, err = intEnv(lookup, maxTicketsPerTickEnv, cfg.MaxTicketsPerTick); err != nil {
		return Config{}, err
	}
	if cfg.LLMTimeout, err = durationEnv(lookup, llmTimeoutEnv, cfg.LLMTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxAssistantReplies, err = intEnv(lookup, maxAssistantRepliesEnv, cfg.MaxAssistantReplies); err != nil {
		return Config{}, err
	}
	if cfg.Lookback, err = durationEnv(lookup, lookbackEnv, cfg.Lookback); err != nil {
		return Config{}, err
	}

	// Keeping the dark path free of secret-file I/O makes the gate an actual
	// construction boundary: shipping mounts or stale paths cannot activate or
	// otherwise initialize the runner.
	if cfg.Enabled {
		if cfg.adminToken, err = credentialFromEnv(lookup, adminTokenFileEnv, adminTokenEnv); err != nil {
			return Config{}, err
		}
		if cfg.anthropicAPIKey, err = credentialFromEnv(lookup, anthropicAPIKeyFileEnv, anthropicAPIKeyEnv); err != nil {
			return Config{}, err
		}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks every support-runner bound and, when enabled, requires both
// credentials. Disabled configuration remains valid without secret material.
func (c Config) Validate() error {
	endpoint, err := url.Parse(strings.TrimSpace(c.ControlPlane))
	if err != nil || endpoint.Host == "" ||
		(endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil {
		return fmt.Errorf("%s must be an absolute HTTP(S) URL", controlPlaneEnv)
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("%s must not be empty", modelEnv)
	}
	if c.Interval <= 0 {
		return fmt.Errorf("%s must be positive", intervalEnv)
	}
	if c.MaxTicketsPerTick <= 0 {
		return fmt.Errorf("%s must be positive", maxTicketsPerTickEnv)
	}
	if c.LLMTimeout <= 0 {
		return fmt.Errorf("%s must be positive", llmTimeoutEnv)
	}
	if c.MaxAssistantReplies <= 0 {
		return fmt.Errorf("%s must be positive", maxAssistantRepliesEnv)
	}
	if c.Lookback <= 0 {
		return fmt.Errorf("%s must be positive", lookbackEnv)
	}
	if c.Enabled && strings.TrimSpace(c.adminToken) == "" {
		return fmt.Errorf("%s or %s is required", adminTokenFileEnv, adminTokenEnv)
	}
	if c.Enabled && strings.TrimSpace(c.anthropicAPIKey) == "" {
		return fmt.Errorf("%s or %s is required", anthropicAPIKeyFileEnv, anthropicAPIKeyEnv)
	}
	return nil
}

func explicitBool(lookup func(string) (string, bool), key string) (bool, error) {
	raw, _ := lookup(key)
	value := strings.TrimSpace(raw)
	switch {
	case value == "", strings.EqualFold(value, "false"):
		return false, nil
	case strings.EqualFold(value, "true"):
		return true, nil
	default:
		return false, fmt.Errorf("%s must be true or false", key)
	}
}

func durationEnv(lookup func(string) (string, bool), key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", key, err)
	}
	return value, nil
}

func intEnv(lookup func(string) (string, bool), key string, fallback int) (int, error) {
	raw, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return value, nil
}

func envOr(lookup func(string) (string, bool), key, fallback string) string {
	raw, ok := lookup(key)
	if !ok {
		return fallback
	}
	return strings.TrimSpace(raw)
}

func credentialFromEnv(lookup func(string) (string, bool), fileKey, inlineKey string) (string, error) {
	if rawPath, ok := lookup(fileKey); ok && strings.TrimSpace(rawPath) != "" {
		path := strings.TrimSpace(rawPath)
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", fileKey, err)
		}
		value := strings.TrimSpace(string(raw))
		if value == "" {
			return "", fmt.Errorf("%s is empty", fileKey)
		}
		return value, nil
	}
	if raw, ok := lookup(inlineKey); ok {
		return strings.TrimSpace(raw), nil
	}
	return "", nil
}
