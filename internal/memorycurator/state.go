package memorycurator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
)

// LaunchStateSchemaV1 identifies the versioned value-free launch-state document.
const LaunchStateSchemaV1 = "witself.curator-launch.v1"

const (
	// PhaseStarting precedes creation or recovery of a server run.
	PhaseStarting = "starting"
	// PhaseStarted indicates that the server run is open.
	PhaseStarted = "started"
	// PhasePlanning indicates that the local planner is running.
	PhasePlanning = "planning"
	// PhasePlanned indicates that the server accepted a validated plan.
	PhasePlanned = "planned"
	// PhaseApplying indicates that an approved plan is being applied.
	PhaseApplying = "applying"
	// PhaseApplied indicates that the server applied the plan.
	PhaseApplied = "applied"
	// PhasePreviewAbandoning indicates that preview cleanup is in progress.
	PhasePreviewAbandoning = "preview_abandoning"
	// PhasePreviewed indicates that a preview plan was abandoned without mutation.
	PhasePreviewed = "previewed"
	// PhaseFailureAbandoning indicates that failure cleanup is in progress.
	PhaseFailureAbandoning = "failure_abandoning"
	// PhaseAbandoned indicates that the server run was abandoned after failure.
	PhaseAbandoned = "abandoned"
	// PhaseTerminal indicates a terminal server state that cannot be resumed.
	PhaseTerminal = "terminal"
)

var (
	stateComponentPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	planHashPattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// LaunchState deliberately contains no materialized input, memory content,
// transcript body, evidence artifact, or plan JSON. It is sufficient to retry
// mutation boundaries without becoming a second memory store.
type LaunchState struct {
	Schema                string                         `json:"schema"`
	LaunchID              string                         `json:"launch_id"`
	Phase                 string                         `json:"phase"`
	ApplyPolicy           ApplyPolicy                    `json:"apply_policy"`
	RequestID             string                         `json:"request_id"`
	RequestGeneration     int64                          `json:"request_generation,omitempty"`
	IncludesSensitive     bool                           `json:"includes_sensitive,omitempty"`
	RunID                 string                         `json:"run_id,omitempty"`
	FencingGeneration     int64                          `json:"fencing_generation,omitempty"`
	LeaseExpiresAt        *time.Time                     `json:"lease_expires_at,omitempty"`
	Caps                  client.MemoryCurationInputCaps `json:"caps"`
	PageSize              int                            `json:"page_size"`
	LeaseSeconds          int64                          `json:"lease_seconds"`
	PlannerTimeoutSeconds int64                          `json:"planner_timeout_seconds"`
	RenewBeforeSeconds    int64                          `json:"renew_before_seconds"`
	MaximumActions        int                            `json:"maximum_actions"`
	InputCount            int                            `json:"input_count,omitempty"`
	StartKey              string                         `json:"start_key"`
	PlanKey               string                         `json:"plan_key"`
	ApplyKey              string                         `json:"apply_key"`
	AbandonKey            string                         `json:"abandon_key"`
	AbandonReason         string                         `json:"abandon_reason,omitempty"`
	LastRenewKey          string                         `json:"last_renew_key,omitempty"`
	PlanAttempt           int                            `json:"plan_attempt"`
	RenewalCount          int                            `json:"renewal_count,omitempty"`
	PlanRevision          int64                          `json:"plan_revision,omitempty"`
	PlanHash              string                         `json:"plan_hash,omitempty"`
	PlanReceiptID         string                         `json:"plan_receipt_id,omitempty"`
	ApplyReceiptID        string                         `json:"apply_receipt_id,omitempty"`
	Client                client.MemoryClientProvenance  `json:"client"`
	CreatedAt             time.Time                      `json:"created_at"`
	UpdatedAt             time.Time                      `json:"updated_at"`
}

// StateStore persists and retrieves value-free curation launch state.
type StateStore interface {
	Save(LaunchState) error
	Load(string) (LaunchState, error)
}

// FileStateStore stores value-free launch receipts under one already-scoped
// root. Files and temporary files are mode 0600; directories are mode 0700.
type FileStateStore struct {
	Root string
}

// DefaultFileStateStore returns the private launch-state store for one agent.
func DefaultFileStateStore(agentID string) (FileStateStore, error) {
	agentID = strings.TrimSpace(agentID)
	if !stateComponentPattern.MatchString(agentID) {
		return FileStateStore{}, fmt.Errorf("invalid curator state agent id %q", agentID)
	}
	home, err := local.Home()
	if err != nil {
		return FileStateStore{}, err
	}
	return FileStateStore{Root: filepath.Join(home, "curation", agentID)}, nil
}

// Save atomically validates and persists one launch state with private permissions.
func (s FileStateStore) Save(state LaunchState) error {
	if err := validateLaunchState(state); err != nil {
		return err
	}
	path, err := s.path(state.LaunchID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".launch-*.tmp")
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
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

// Load reads and validates one private launch-state document.
func (s FileStateStore) Load(launchID string) (LaunchState, error) {
	path, err := s.path(launchID)
	if err != nil {
		return LaunchState{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return LaunchState{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return LaunchState{}, fmt.Errorf("curator state %s is accessible by group or other users", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return LaunchState{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var state LaunchState
	if err := decoder.Decode(&state); err != nil {
		return LaunchState{}, fmt.Errorf("parse curator state: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return LaunchState{}, fmt.Errorf("parse curator state: %w", err)
	}
	if err := validateLaunchState(state); err != nil {
		return LaunchState{}, err
	}
	return state, nil
}

func (s FileStateStore) path(launchID string) (string, error) {
	if strings.TrimSpace(s.Root) == "" {
		return "", errors.New("curator state root is required")
	}
	if !stateComponentPattern.MatchString(launchID) {
		return "", fmt.Errorf("invalid curator launch id %q", launchID)
	}
	return filepath.Join(s.Root, launchID+".json"), nil
}

func validateLaunchState(state LaunchState) error {
	if state.Schema != LaunchStateSchemaV1 {
		return fmt.Errorf("unsupported curator state schema %q", state.Schema)
	}
	if !stateComponentPattern.MatchString(state.LaunchID) || strings.TrimSpace(state.Phase) == "" || strings.TrimSpace(state.RequestID) == "" {
		return errors.New("curator state requires launch_id, phase, and request_id")
	}
	if state.PlanHash != "" && !planHashPattern.MatchString(state.PlanHash) {
		return errors.New("curator state plan_hash must be lowercase SHA-256")
	}
	if state.PlanRevision < 0 || state.FencingGeneration < 0 || state.RequestGeneration < 0 || state.InputCount < 0 || state.PlanAttempt < 1 || state.RenewalCount < 0 {
		return errors.New("curator state contains an invalid negative counter")
	}
	if state.ApplyPolicy != ApplyPolicyPreview && state.ApplyPolicy != ApplyPolicyApply {
		return errors.New("curator state contains an invalid apply policy")
	}
	validPhase := map[string]bool{
		PhaseStarting: true, PhaseStarted: true, PhasePlanning: true,
		PhasePlanned: true, PhaseApplying: true, PhaseApplied: true,
		PhasePreviewAbandoning: true, PhasePreviewed: true,
		PhaseFailureAbandoning: true, PhaseAbandoned: true, PhaseTerminal: true,
	}
	if !validPhase[state.Phase] {
		return fmt.Errorf("curator state contains invalid phase %q", state.Phase)
	}
	if state.Caps.MaxMemories < 1 || state.Caps.MaxEvidence < 1 || state.Caps.MaxTranscriptEntries < 1 ||
		state.PageSize < 1 || state.PageSize > 200 || state.MaximumActions < 1 || state.MaximumActions > 128 ||
		state.LeaseSeconds < 1 || state.PlannerTimeoutSeconds < 1 || state.RenewBeforeSeconds < 0 || state.RenewBeforeSeconds >= state.LeaseSeconds {
		return errors.New("curator state contains invalid persisted policy limits")
	}
	if (state.RunID == "") != (state.FencingGeneration == 0) {
		return errors.New("curator state run id and fencing generation must appear together")
	}
	if (state.PlanRevision == 0) != (state.PlanHash == "") {
		return errors.New("curator state plan revision and hash must appear together")
	}
	if state.AbandonReason != "" && !stateComponentPattern.MatchString(state.AbandonReason) {
		return errors.New("curator state abandon reason must be a value-free reason code")
	}
	if strings.TrimSpace(state.StartKey) == "" || strings.TrimSpace(state.PlanKey) == "" || strings.TrimSpace(state.ApplyKey) == "" || strings.TrimSpace(state.AbandonKey) == "" {
		return errors.New("curator state requires all mutation idempotency keys")
	}
	if state.CreatedAt.IsZero() || state.UpdatedAt.IsZero() {
		return errors.New("curator state requires timestamps")
	}
	return nil
}
