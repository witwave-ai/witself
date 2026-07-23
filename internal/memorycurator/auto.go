package memorycurator

// This file owns only value-free, client-local automation state. It does not
// connect to Witself, invoke inference, or store credentials, memory content,
// transcript content, evidence, prompts, plans, or provider output. Callers
// supply one bounded work function after the state engine has debounced and
// acquired the agent-scoped singleflight lock.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/local"
)

// AutoConfigSchemaV1 and the related schema constants identify persisted
// client-local automation documents.
const (
	AutoConfigSchemaV1 = "witself.curator-auto.v1"
	AutoStatusSchemaV1 = "witself.curator-auto-status.v1"
	AutoWakeSchemaV1   = "witself.curator-auto-wake.v1"
	AutoLockSchemaV1   = "witself.curator-auto-lock.v1"
)

// AutoStateDisabled and the related state constants describe automation
// lifecycle states.
const (
	AutoStateDisabled = "disabled"
	AutoStateIdle     = "idle"
	AutoStateRunning  = "running"
	AutoStateBackoff  = "backoff"
)

// AutoOutcomeNoWork and the related outcome constants classify a work result.
const (
	AutoOutcomeNoWork    AutoWorkOutcome = "no_work"
	AutoOutcomePreviewed AutoWorkOutcome = "previewed"
	AutoOutcomeApplied   AutoWorkOutcome = "applied"
)

// AutoWakeTerminalFlush and the related wake constants classify wake signals.
const (
	AutoWakeTerminalFlush AutoWakeReason = "terminal_flush"
	AutoWakeScheduledPoll AutoWakeReason = "scheduled_poll"
	AutoWakeManualPoll    AutoWakeReason = "manual_poll"
)

// AutoFailureWorker and the related failure constants are safe persisted
// failure codes.
const (
	AutoFailureWorker        = "worker_failed"
	AutoFailureContract      = "worker_contract"
	AutoFailurePreflight     = "preflight_failed"
	AutoFailureProviderProbe = "provider_probe_failed"
	AutoFailureCredential    = "credential_expired"
	AutoFailureIdentity      = "identity_mismatch"
	AutoFailureCuration      = "curation_failed"
)

// DefaultAutoDebounce and the related timing and run-count constants bound
// automatic curator scheduling.
const (
	DefaultAutoDebounce        = 30 * time.Second
	DefaultAutoMinimumInterval = 10 * time.Minute
	DefaultAutoMaxRuns         = 1

	MinAutoDebounce        = time.Second
	MaxAutoDebounce        = 10 * time.Minute
	MaxAutoMinimumInterval = 24 * time.Hour
	MaxAutoRuns            = 16
)

const (
	defaultAutoFailureBackoff = time.Minute
	maxAutoFailureBackoff     = time.Hour
)

// ErrAutoDisabled and ErrAutoInvalidResult report automation state and worker
// contract failures.
var (
	ErrAutoDisabled      = errors.New("memory curator automation is disabled")
	ErrAutoInvalidResult = errors.New("memory curator automation worker returned an invalid result")
)

// AutoSettings is the caller-authored, value-free policy saved by Enable.
// AccountID and RealmID are optional as a pair for older/local-only bindings;
// when provided they let the command layer pin authenticated preflight to the
// same durable identity before inference is invoked.
type AutoSettings struct {
	AccountID              string
	RealmID                string
	AgentID                string
	Provider               NativeProvider
	ProviderPath           string
	Model                  string
	ApplyPolicy            ApplyPolicy
	AllowTranscriptContent bool
	Debounce               time.Duration
	MinimumInterval        time.Duration
	MaxRuns                int
}

// AutoConfig is deliberately incapable of holding credentials or source
// material. Durations are persisted as whole seconds so the local contract is
// stable across Go versions and readable by other clients.
type AutoConfig struct {
	Schema                 string         `json:"schema"`
	Enabled                bool           `json:"enabled"`
	Revision               int64          `json:"revision"`
	AccountID              string         `json:"account_id,omitempty"`
	RealmID                string         `json:"realm_id,omitempty"`
	AgentID                string         `json:"agent_id"`
	Provider               NativeProvider `json:"provider"`
	ProviderPath           string         `json:"provider_path,omitempty"`
	Model                  string         `json:"model,omitempty"`
	ApplyPolicy            ApplyPolicy    `json:"apply_policy"`
	AllowTranscriptContent bool           `json:"allow_transcript_content"`
	DebounceSeconds        int64          `json:"debounce_seconds"`
	MinimumIntervalSeconds int64          `json:"minimum_interval_seconds"`
	MaxRuns                int            `json:"max_runs"`
	CreatedAt              time.Time      `json:"created_at"`
	UpdatedAt              time.Time      `json:"updated_at"`
}

// Debounce returns the configured wake debounce interval.
func (c AutoConfig) Debounce() time.Duration {
	return time.Duration(c.DebounceSeconds) * time.Second
}

// MinimumInterval returns the configured minimum interval between runs.
func (c AutoConfig) MinimumInterval() time.Duration {
	return time.Duration(c.MinimumIntervalSeconds) * time.Second
}

// AutoStatus is a value-free operational projection. FailureCode is a bounded
// machine code supplied by AutoWorkError; an underlying error string is never
// persisted because provider errors may contain source material.
type AutoStatus struct {
	Schema              string          `json:"schema"`
	AgentID             string          `json:"agent_id"`
	State               string          `json:"state"`
	LastAttemptAt       *time.Time      `json:"last_attempt_at,omitempty"`
	LastSuccessAt       *time.Time      `json:"last_success_at,omitempty"`
	LastFailureAt       *time.Time      `json:"last_failure_at,omitempty"`
	LastOutcome         AutoWorkOutcome `json:"last_outcome,omitempty"`
	LastFailureCode     string          `json:"last_failure_code,omitempty"`
	ConsecutiveFailures int             `json:"consecutive_failures"`
	TotalRuns           int64           `json:"total_runs"`
	RetryNotBefore      *time.Time      `json:"retry_not_before,omitempty"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// AutoWakeMarker is one persisted, value-free automation wake signal.
type AutoWakeMarker struct {
	Schema    string         `json:"schema"`
	ID        string         `json:"id"`
	AgentID   string         `json:"agent_id"`
	Reason    AutoWakeReason `json:"reason"`
	CreatedAt time.Time      `json:"created_at"`
}

// AutoInspection is suitable for a CLI status projection. It contains no
// wake identifiers or filesystem paths.
type AutoInspection struct {
	Configured       bool       `json:"configured"`
	Config           AutoConfig `json:"config,omitempty"`
	Status           AutoStatus `json:"status"`
	PendingWakeCount int        `json:"pending_wake_count"`
}

// AutoWorkOutcome identifies the result of one bounded automation callback.
type AutoWorkOutcome string

// AutoWakeReason identifies why automatic curation was signaled.
type AutoWakeReason string

// AutoWorkResult tells the engine whether another bounded queue poll is useful.
// MoreWork may be true after applied/previewed, but never with no_work.
type AutoWorkResult struct {
	Outcome  AutoWorkOutcome
	MoreWork bool
}

// AutoWorkFunc performs one bounded automatic curation poll.
type AutoWorkFunc func(context.Context, AutoConfig) (AutoWorkResult, error)

// AutoWorkError carries a safe machine code separately from an error that is
// returned to the caller but never serialized.
type AutoWorkError struct {
	Code string
	Err  error
}

func (e *AutoWorkError) Error() string {
	if e == nil || e.Err == nil {
		return "memory curator automatic work failed"
	}
	return e.Err.Error()
}

func (e *AutoWorkError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NewAutoWorkError attaches a safe persisted failure code to a callback error.
func NewAutoWorkError(code string, err error) error {
	if err == nil {
		err = errors.New("memory curator automatic work failed")
	}
	return &AutoWorkError{Code: normalizeAutoFailureCode(code), Err: err}
}

// AutoRunResult summarizes one attempt to service pending wake signals.
type AutoRunResult struct {
	Acquired         bool            `json:"acquired"`
	Attempted        bool            `json:"attempted"`
	Runs             int             `json:"runs"`
	Outcome          AutoWorkOutcome `json:"outcome,omitempty"`
	PendingWakeCount int             `json:"pending_wake_count"`
	MoreWork         bool            `json:"more_work"`
	NextEligibleAt   *time.Time      `json:"next_eligible_at,omitempty"`
}

// AutoStore persists value-free automation state for one agent. Its injectable
// functions make waits, IDs, and state-write failures deterministic under test.
type AutoStore struct {
	Root        string
	AgentID     string
	Now         func() time.Time
	Sleep       func(context.Context, time.Duration) error
	NewID       func(string) (string, error)
	BeforeWrite func(path string) error
}

var autoProcessLocks sync.Map

// DefaultAutoStore constructs an agent-scoped store under the Witself home.
func DefaultAutoStore(agentID string) (AutoStore, error) {
	agentID = strings.TrimSpace(agentID)
	if !stateComponentPattern.MatchString(agentID) {
		return AutoStore{}, fmt.Errorf("invalid curator automation agent id %q", agentID)
	}
	home, err := local.Home()
	if err != nil {
		return AutoStore{}, err
	}
	return NewAutoStore(filepath.Join(home, "curation", agentID), agentID)
}

// NewAutoStore constructs an agent-scoped store at an explicit local root.
func NewAutoStore(root, agentID string) (AutoStore, error) {
	root = strings.TrimSpace(root)
	agentID = strings.TrimSpace(agentID)
	if root == "" {
		return AutoStore{}, errors.New("curator automation root is required")
	}
	if !stateComponentPattern.MatchString(agentID) {
		return AutoStore{}, fmt.Errorf("invalid curator automation agent id %q", agentID)
	}
	return AutoStore{Root: root, AgentID: agentID}, nil
}

// Enable validates and atomically activates one explicit automation policy.
// Status is prepared first and the enabled config is the final activation
// commit. If either write reports an error, the previous files are restored so
// a failed Enable never leaves the newly requested policy active.
func (s AutoStore) Enable(settings AutoSettings) (AutoConfig, error) {
	if err := s.validate(); err != nil {
		return AutoConfig{}, err
	}
	settings.AgentID = strings.TrimSpace(settings.AgentID)
	if settings.AgentID == "" {
		settings.AgentID = s.AgentID
	}
	if settings.Debounce%time.Second != 0 || settings.MinimumInterval%time.Second != 0 {
		return AutoConfig{}, errors.New("curator automation timing must use whole seconds")
	}
	now := s.now()
	createdAt := now
	revision := int64(1)
	if previous, err := s.LoadConfig(); err == nil {
		createdAt = previous.CreatedAt
		revision = previous.Revision + 1
	} else if !errors.Is(err, os.ErrNotExist) {
		return AutoConfig{}, err
	}
	config := AutoConfig{
		Schema: AutoConfigSchemaV1, Enabled: true, Revision: revision,
		AccountID: strings.TrimSpace(settings.AccountID), RealmID: strings.TrimSpace(settings.RealmID),
		AgentID: settings.AgentID, Provider: settings.Provider,
		ProviderPath: strings.TrimSpace(settings.ProviderPath), Model: strings.TrimSpace(settings.Model),
		ApplyPolicy: settings.ApplyPolicy, AllowTranscriptContent: settings.AllowTranscriptContent,
		DebounceSeconds:        int64(settings.Debounce / time.Second),
		MinimumIntervalSeconds: int64(settings.MinimumInterval / time.Second),
		MaxRuns:                settings.MaxRuns, CreatedAt: createdAt, UpdatedAt: now,
	}
	if err := validateAutoConfig(config, s.AgentID); err != nil {
		return AutoConfig{}, err
	}
	status, err := s.loadStatusOrDefault(AutoStateIdle)
	if err != nil {
		return AutoConfig{}, err
	}
	configSnapshot, err := snapshotAutoFile(s.configPath())
	if err != nil {
		return AutoConfig{}, err
	}
	statusSnapshot, err := snapshotAutoFile(s.statusPath())
	if err != nil {
		return AutoConfig{}, err
	}
	status.State, status.ConsecutiveFailures, status.RetryNotBefore = AutoStateIdle, 0, nil
	status.LastFailureCode = ""
	status.UpdatedAt = now
	if err := s.saveStatus(status); err != nil {
		rollbackErr := s.restoreAutoFile(s.statusPath(), statusSnapshot)
		if rollbackErr != nil {
			rollbackErr = fmt.Errorf("restore curator automation status: %w", rollbackErr)
		}
		return AutoConfig{}, errors.Join(err, rollbackErr)
	}
	if err := s.writeJSON(s.configPath(), config); err != nil {
		configRollbackErr := s.restoreAutoFile(s.configPath(), configSnapshot)
		if configRollbackErr != nil {
			configRollbackErr = fmt.Errorf("restore curator automation config: %w", configRollbackErr)
		}
		statusRollbackErr := s.restoreAutoFile(s.statusPath(), statusSnapshot)
		if statusRollbackErr != nil {
			statusRollbackErr = fmt.Errorf("restore curator automation status: %w", statusRollbackErr)
		}
		return AutoConfig{}, errors.Join(err, configRollbackErr, statusRollbackErr)
	}
	return config, nil
}

// Disable preserves settings and pending wake markers so re-enabling cannot
// lose server-durable due work. It only prevents RunPending from invoking work.
func (s AutoStore) Disable() error {
	config, err := s.LoadConfig()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	now := s.now()
	config.Enabled = false
	config.Revision++
	config.UpdatedAt = now
	if err := validateAutoConfig(config, s.AgentID); err != nil {
		return err
	}
	if err := s.writeJSON(s.configPath(), config); err != nil {
		return err
	}
	status, err := s.loadStatusOrDefault(AutoStateDisabled)
	if err != nil {
		return err
	}
	status.State, status.RetryNotBefore, status.UpdatedAt = AutoStateDisabled, nil, now
	return s.saveStatus(status)
}

// LoadConfig reads and validates the persisted automation configuration.
func (s AutoStore) LoadConfig() (AutoConfig, error) {
	if err := s.validate(); err != nil {
		return AutoConfig{}, err
	}
	var config AutoConfig
	if err := s.readJSON(s.configPath(), &config); err != nil {
		return AutoConfig{}, err
	}
	if err := validateAutoConfig(config, s.AgentID); err != nil {
		return AutoConfig{}, err
	}
	return config, nil
}

// LoadStatus reads and validates the persisted automation status.
func (s AutoStore) LoadStatus() (AutoStatus, error) {
	if err := s.validate(); err != nil {
		return AutoStatus{}, err
	}
	var status AutoStatus
	if err := s.readJSON(s.statusPath(), &status); err != nil {
		return AutoStatus{}, err
	}
	if err := validateAutoStatus(status, s.AgentID); err != nil {
		return AutoStatus{}, err
	}
	return status, nil
}

// Inspect returns a value-free projection of configuration, status, and wakes.
func (s AutoStore) Inspect() (AutoInspection, error) {
	inspection := AutoInspection{}
	config, err := s.LoadConfig()
	switch {
	case err == nil:
		inspection.Configured, inspection.Config = true, config
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return AutoInspection{}, err
	}
	defaultState := AutoStateDisabled
	if inspection.Configured && inspection.Config.Enabled {
		defaultState = AutoStateIdle
	}
	status, err := s.loadStatusOrDefault(defaultState)
	if err != nil {
		return AutoInspection{}, err
	}
	markers, err := s.PendingWakes()
	if err != nil {
		return AutoInspection{}, err
	}
	inspection.Status, inspection.PendingWakeCount = status, len(markers)
	return inspection, nil
}

// RecordWake writes one unique value-free signal. Reason must be a stable
// machine code such as terminal_flush or scheduled_poll.
func (s AutoStore) RecordWake(reason AutoWakeReason) (AutoWakeMarker, error) {
	if err := s.validate(); err != nil {
		return AutoWakeMarker{}, err
	}
	if !validAutoWakeReason(reason) {
		return AutoWakeMarker{}, fmt.Errorf("invalid curator automation wake reason %q", reason)
	}
	for attempts := 0; attempts < 3; attempts++ {
		markerID, err := s.newID("wake")
		if err != nil {
			return AutoWakeMarker{}, fmt.Errorf("create curator automation wake id: %w", err)
		}
		marker := AutoWakeMarker{
			Schema: AutoWakeSchemaV1, ID: markerID, AgentID: s.AgentID,
			Reason: reason, CreatedAt: s.now(),
		}
		if err := validateAutoWake(marker, s.AgentID); err != nil {
			return AutoWakeMarker{}, err
		}
		err = s.writeWakeExclusive(marker)
		if err == nil {
			return marker, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return AutoWakeMarker{}, err
		}
	}
	return AutoWakeMarker{}, errors.New("could not allocate a unique curator automation wake marker")
}

// PendingWakes reads and validates all pending wake markers in creation order.
func (s AutoStore) PendingWakes() ([]AutoWakeMarker, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(s.wakeDir(), "wake_*.json"))
	if err != nil {
		return nil, err
	}
	sortStrings(paths)
	markers := make([]AutoWakeMarker, 0, len(paths))
	for _, path := range paths {
		var marker AutoWakeMarker
		if err := s.readJSON(path, &marker); err != nil {
			return nil, err
		}
		if err := validateAutoWake(marker, s.AgentID); err != nil {
			return nil, fmt.Errorf("parse curator automation wake %s: %w", path, err)
		}
		if filepath.Base(path) != marker.ID+".json" {
			return nil, fmt.Errorf("curator automation wake filename does not match marker id")
		}
		markers = append(markers, marker)
	}
	return markers, nil
}

// RunPending debounces pending signals, runs at most config.MaxRuns callbacks,
// records only value-free health, and acknowledges exactly the marker snapshot
// that was serviced. Signals written during work therefore remain pending.
func (s AutoStore) RunPending(ctx context.Context, work AutoWorkFunc) (AutoRunResult, error) {
	if ctx == nil {
		return AutoRunResult{}, errors.New("curator automation context is required")
	}
	if work == nil {
		return AutoRunResult{}, errors.New("curator automation work function is required")
	}
	if err := s.validate(); err != nil {
		return AutoRunResult{}, err
	}
	markers, err := s.PendingWakes()
	if err != nil {
		return AutoRunResult{}, err
	}
	if len(markers) == 0 {
		return AutoRunResult{}, nil
	}
	release, acquired, err := s.acquire()
	if err != nil {
		return AutoRunResult{}, err
	}
	if !acquired {
		return AutoRunResult{PendingWakeCount: len(markers)}, nil
	}
	defer release()
	result := AutoRunResult{Acquired: true}

	var config AutoConfig
	var status AutoStatus
	for {
		config, err = s.LoadConfig()
		if errors.Is(err, os.ErrNotExist) {
			return result, ErrAutoDisabled
		}
		if err != nil {
			return result, err
		}
		if !config.Enabled {
			return result, ErrAutoDisabled
		}
		markers, err = s.PendingWakes()
		if err != nil {
			return result, err
		}
		result.PendingWakeCount = len(markers)
		if len(markers) == 0 {
			return result, nil
		}
		status, err = s.loadStatusOrDefault(AutoStateIdle)
		if err != nil {
			return result, err
		}
		now := s.now()
		eligible := latestWakeTime(markers).Add(config.Debounce())
		if status.LastAttemptAt != nil {
			eligible = laterTime(eligible, status.LastAttemptAt.Add(config.MinimumInterval()))
		}
		if status.RetryNotBefore != nil {
			eligible = laterTime(eligible, *status.RetryNotBefore)
		}
		if eligible.After(now) {
			next := eligible
			result.NextEligibleAt = &next
			if err := s.sleep(ctx, eligible.Sub(now)); err != nil {
				return result, err
			}
			continue
		}
		result.NextEligibleAt = nil
		break
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	// This immutable slice is the acknowledgement boundary. PendingWakes sorts
	// by marker path, while acknowledgements name exact ids and cannot remove a
	// wake created after this point.
	snapshot := append([]AutoWakeMarker(nil), markers...)
	now := s.now()
	status.State, status.LastAttemptAt, status.RetryNotBefore, status.UpdatedAt = AutoStateRunning, timePointer(now), nil, now
	if err := s.saveStatus(status); err != nil {
		return result, err
	}
	result.Attempted = true

	moreWork := false
	for result.Runs < config.MaxRuns {
		workResult, workErr := work(ctx, config)
		if workErr != nil {
			failureCode := AutoFailureWorker
			var coded *AutoWorkError
			if errors.As(workErr, &coded) {
				failureCode = normalizeAutoFailureCode(coded.Code)
			}
			if statusErr := s.recordFailure(status, failureCode, int64(result.Runs)); statusErr != nil {
				return result, errors.Join(workErr, statusErr)
			}
			pending, pendingErr := s.PendingWakes()
			if pendingErr == nil {
				result.PendingWakeCount = len(pending)
			}
			return result, workErr
		}
		if err := validateAutoWorkResult(workResult); err != nil {
			contractErr := fmt.Errorf("%w: %v", ErrAutoInvalidResult, err)
			if statusErr := s.recordFailure(status, AutoFailureContract, int64(result.Runs)); statusErr != nil {
				return result, errors.Join(contractErr, statusErr)
			}
			return result, contractErr
		}
		result.Runs++
		result.Outcome, moreWork = workResult.Outcome, workResult.MoreWork
		if !moreWork {
			break
		}
		latestConfig, configErr := s.LoadConfig()
		if configErr != nil && !errors.Is(configErr, os.ErrNotExist) {
			return result, configErr
		}
		if errors.Is(configErr, os.ErrNotExist) || !latestConfig.Enabled || latestConfig.Revision != config.Revision {
			// A concurrent disable or policy replacement applies before another
			// callback. Preserve the serviced marker so the replacement policy can
			// make a fresh bounded decision.
			moreWork = true
			break
		}
	}
	result.MoreWork = moreWork
	if err := s.recordSuccess(status, result.Outcome, int64(result.Runs)); err != nil {
		return result, err
	}
	if !moreWork {
		if err := s.acknowledge(snapshot); err != nil {
			return result, err
		}
	}
	pending, err := s.PendingWakes()
	if err != nil {
		return result, err
	}
	result.PendingWakeCount = len(pending)
	return result, nil
}

func (s AutoStore) recordSuccess(status AutoStatus, outcome AutoWorkOutcome, runs int64) error {
	now := s.now()
	state, err := s.currentAutoState(AutoStateIdle)
	if err != nil {
		return err
	}
	status.State, status.LastSuccessAt, status.LastOutcome = state, timePointer(now), outcome
	status.LastFailureCode, status.ConsecutiveFailures, status.RetryNotBefore = "", 0, nil
	status.TotalRuns += runs
	status.UpdatedAt = now
	return s.saveStatus(status)
}

func (s AutoStore) recordFailure(status AutoStatus, code string, completedRuns int64) error {
	now := s.now()
	state, err := s.currentAutoState(AutoStateBackoff)
	if err != nil {
		return err
	}
	status.ConsecutiveFailures++
	retry := now.Add(autoFailureBackoff(status.ConsecutiveFailures))
	status.State, status.LastFailureAt = state, timePointer(now)
	status.LastFailureCode = normalizeAutoFailureCode(code)
	if status.State == AutoStateBackoff {
		status.RetryNotBefore = &retry
	} else {
		status.RetryNotBefore = nil
	}
	status.TotalRuns += completedRuns
	status.UpdatedAt = now
	return s.saveStatus(status)
}

func (s AutoStore) currentAutoState(enabledState string) (string, error) {
	config, err := s.LoadConfig()
	if errors.Is(err, os.ErrNotExist) || err == nil && !config.Enabled {
		return AutoStateDisabled, nil
	}
	if err != nil {
		return "", err
	}
	return enabledState, nil
}

func (s AutoStore) acknowledge(markers []AutoWakeMarker) error {
	for _, marker := range markers {
		if err := validateAutoWake(marker, s.AgentID); err != nil {
			return err
		}
		path := filepath.Join(s.wakeDir(), marker.ID+".json")
		var current AutoWakeMarker
		if err := s.readJSON(path, &current); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if current.ID != marker.ID || current.CreatedAt != marker.CreatedAt {
			return errors.New("curator automation wake changed before acknowledgement")
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return syncDirectory(s.wakeDir())
}

func (s AutoStore) acquire() (release func(), acquired bool, err error) {
	if err := s.ensureRoot(); err != nil {
		return nil, false, err
	}
	path, err := filepath.Abs(s.lockPath())
	if err != nil {
		return nil, false, err
	}
	processLockValue, _ := autoProcessLocks.LoadOrStore(path, &sync.Mutex{})
	processLock := processLockValue.(*sync.Mutex)
	if !processLock.TryLock() {
		return func() {}, false, nil
	}
	processLocked := true
	defer func() {
		if processLocked {
			processLock.Unlock()
		}
	}()

	// Keep one persistent inode and hold the descriptor open for the complete
	// lease. Unlinking here or on release could let contenders lock different
	// inodes for the same path. The file contents are deliberately irrelevant to
	// ownership, so malformed bytes or a crashed predecessor never delay work.
	file, err := openAutoLockFileNoFollow(path)
	if err != nil {
		return nil, false, err
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()
	if _, err := validateAutoLockFileIdentity(path, file); err != nil {
		return nil, false, err
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, false, err
	}
	info, err := validateAutoLockFileIdentity(path, file)
	if err != nil {
		return nil, false, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, false, fmt.Errorf("curator automation lock %s is accessible by group or other users", path)
	}
	lockAcquired, err := tryLockAutoFile(file)
	if err != nil {
		return nil, false, err
	}
	if !lockAcquired {
		return func() {}, false, nil
	}
	locked := true
	defer func() {
		if locked {
			_ = unlockAutoFile(file)
		}
	}()
	info, err = validateAutoLockFileIdentity(path, file)
	if err != nil {
		return nil, false, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, false, fmt.Errorf("curator automation lock %s is accessible by group or other users", path)
	}
	if err := file.Truncate(0); err != nil {
		return nil, false, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, false, err
	}
	if _, err := io.WriteString(file, AutoLockSchemaV1+"\n"); err != nil {
		return nil, false, err
	}
	if err := file.Sync(); err != nil {
		return nil, false, err
	}

	var releaseOnce sync.Once
	processLocked, closeFile, locked = false, false, false
	return func() {
		releaseOnce.Do(func() {
			_ = unlockAutoFile(file)
			_ = file.Close()
			processLock.Unlock()
		})
	}, true, nil
}

func (s AutoStore) saveStatus(status AutoStatus) error {
	if err := validateAutoStatus(status, s.AgentID); err != nil {
		return err
	}
	return s.writeJSON(s.statusPath(), status)
}

func (s AutoStore) loadStatusOrDefault(state string) (AutoStatus, error) {
	status, err := s.LoadStatus()
	if err == nil {
		return status, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return AutoStatus{}, err
	}
	now := s.now()
	status = AutoStatus{
		Schema: AutoStatusSchemaV1, AgentID: s.AgentID,
		State: state, UpdatedAt: now,
	}
	return status, nil
}

func validateAutoConfig(config AutoConfig, expectedAgentID string) error {
	if config.Schema != AutoConfigSchemaV1 {
		return fmt.Errorf("unsupported curator automation config schema %q", config.Schema)
	}
	if config.AgentID != expectedAgentID || !stateComponentPattern.MatchString(config.AgentID) {
		return errors.New("curator automation config agent identity does not match its state root")
	}
	if config.Revision < 1 {
		return errors.New("curator automation config revision must be positive")
	}
	if (config.AccountID == "") != (config.RealmID == "") ||
		(config.AccountID != "" && (!stateComponentPattern.MatchString(config.AccountID) || !stateComponentPattern.MatchString(config.RealmID))) {
		return errors.New("curator automation account and realm identities must be valid and appear together")
	}
	switch config.Provider {
	case ProviderCodex, ProviderClaudeCode, ProviderGrokBuild, ProviderCursor:
	default:
		return errors.New("curator automation requires an explicit supported provider name")
	}
	if config.ProviderPath != "" {
		if len(config.ProviderPath) > 4096 || !filepath.IsAbs(config.ProviderPath) || filepath.Clean(config.ProviderPath) != config.ProviderPath {
			return errors.New("curator automation provider path must be a clean absolute path")
		}
	}
	if config.Model != "" && !nativeModelPattern.MatchString(config.Model) {
		return errors.New("curator automation model name is invalid")
	}
	if config.ApplyPolicy != ApplyPolicyPreview && config.ApplyPolicy != ApplyPolicyApply {
		return errors.New("curator automation apply policy is invalid")
	}
	if config.Enabled && !config.AllowTranscriptContent {
		return errors.New("enabled curator automation requires explicit transcript content authorization")
	}
	debounce, minimumInterval := config.Debounce(), config.MinimumInterval()
	if debounce < MinAutoDebounce || debounce > MaxAutoDebounce || config.DebounceSeconds <= 0 {
		return fmt.Errorf("curator automation debounce must be between %s and %s", MinAutoDebounce, MaxAutoDebounce)
	}
	if minimumInterval < 0 || minimumInterval > MaxAutoMinimumInterval || config.MinimumIntervalSeconds < 0 {
		return fmt.Errorf("curator automation minimum interval must be between 0 and %s", MaxAutoMinimumInterval)
	}
	if config.MaxRuns < 1 || config.MaxRuns > MaxAutoRuns {
		return fmt.Errorf("curator automation max runs must be between 1 and %d", MaxAutoRuns)
	}
	if config.CreatedAt.IsZero() || config.UpdatedAt.IsZero() || config.UpdatedAt.Before(config.CreatedAt) {
		return errors.New("curator automation config timestamps are invalid")
	}
	return nil
}

func validateAutoStatus(status AutoStatus, expectedAgentID string) error {
	if status.Schema != AutoStatusSchemaV1 || status.AgentID != expectedAgentID || !stateComponentPattern.MatchString(status.AgentID) {
		return errors.New("curator automation status schema or agent identity is invalid")
	}
	switch status.State {
	case AutoStateDisabled, AutoStateIdle, AutoStateRunning, AutoStateBackoff:
	default:
		return errors.New("curator automation status state is invalid")
	}
	if status.LastOutcome != "" && !validAutoOutcome(status.LastOutcome) {
		return errors.New("curator automation status outcome is invalid")
	}
	if status.LastFailureCode != "" && !validAutoFailureCode(status.LastFailureCode) {
		return errors.New("curator automation status failure code is invalid")
	}
	if status.ConsecutiveFailures < 0 || status.TotalRuns < 0 || status.UpdatedAt.IsZero() {
		return errors.New("curator automation status counters or timestamp are invalid")
	}
	if status.State == AutoStateRunning && status.LastAttemptAt == nil {
		return errors.New("running curator automation status requires an attempt timestamp")
	}
	if status.State == AutoStateBackoff && (status.LastFailureAt == nil || status.RetryNotBefore == nil ||
		status.ConsecutiveFailures < 1 || status.LastFailureCode == "") {
		return errors.New("backoff curator automation status requires value-free failure coordinates")
	}
	if status.RetryNotBefore != nil && status.State != AutoStateBackoff {
		return errors.New("curator automation retry timestamp requires backoff state")
	}
	if (status.LastSuccessAt == nil) != (status.LastOutcome == "") {
		return errors.New("curator automation success timestamp and outcome must appear together")
	}
	return nil
}

func validateAutoWake(marker AutoWakeMarker, expectedAgentID string) error {
	if marker.Schema != AutoWakeSchemaV1 || marker.AgentID != expectedAgentID ||
		!stateComponentPattern.MatchString(marker.ID) || !validAutoWakeReason(marker.Reason) || marker.CreatedAt.IsZero() {
		return errors.New("curator automation wake marker is invalid")
	}
	return nil
}

func validateAutoWorkResult(result AutoWorkResult) error {
	if !validAutoOutcome(result.Outcome) {
		return errors.New("outcome is invalid")
	}
	if result.Outcome == AutoOutcomeNoWork && result.MoreWork {
		return errors.New("no_work cannot report more work")
	}
	return nil
}

func validAutoOutcome(outcome AutoWorkOutcome) bool {
	return outcome == AutoOutcomeNoWork || outcome == AutoOutcomePreviewed || outcome == AutoOutcomeApplied
}

func autoFailureBackoff(failures int) time.Duration {
	if failures <= 1 {
		return defaultAutoFailureBackoff
	}
	backoff := defaultAutoFailureBackoff
	for i := 1; i < failures && backoff < maxAutoFailureBackoff; i++ {
		backoff *= 2
		if backoff > maxAutoFailureBackoff {
			return maxAutoFailureBackoff
		}
	}
	return backoff
}

func latestWakeTime(markers []AutoWakeMarker) time.Time {
	var latest time.Time
	for _, marker := range markers {
		if marker.CreatedAt.After(latest) {
			latest = marker.CreatedAt
		}
	}
	return latest
}

func laterTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

func timePointer(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}

func normalizeAutoFailureCode(code string) string {
	code = strings.TrimSpace(code)
	if !validAutoFailureCode(code) {
		return AutoFailureWorker
	}
	return code
}

func validAutoFailureCode(code string) bool {
	switch code {
	case AutoFailureWorker, AutoFailureContract, AutoFailurePreflight,
		AutoFailureProviderProbe, AutoFailureCredential, AutoFailureIdentity,
		AutoFailureCuration:
		return true
	default:
		return false
	}
}

func validAutoWakeReason(reason AutoWakeReason) bool {
	switch reason {
	case AutoWakeTerminalFlush, AutoWakeScheduledPoll, AutoWakeManualPoll:
		return true
	default:
		return false
	}
}

func (s AutoStore) validate() error {
	if strings.TrimSpace(s.Root) == "" {
		return errors.New("curator automation root is required")
	}
	if !stateComponentPattern.MatchString(s.AgentID) {
		return fmt.Errorf("invalid curator automation agent id %q", s.AgentID)
	}
	return nil
}

func (s AutoStore) ensureRoot() error {
	if err := s.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return err
	}
	return os.Chmod(s.Root, 0o700)
}

type autoFileSnapshot struct {
	exists bool
	raw    []byte
}

func snapshotAutoFile(path string) (autoFileSnapshot, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return autoFileSnapshot{}, nil
	}
	if err != nil {
		return autoFileSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return autoFileSnapshot{}, fmt.Errorf("curator automation state %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return autoFileSnapshot{}, fmt.Errorf("curator automation state %s is accessible by group or other users", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return autoFileSnapshot{}, err
	}
	return autoFileSnapshot{exists: true, raw: raw}, nil
}

func (s AutoStore) restoreAutoFile(path string, snapshot autoFileSnapshot) error {
	if snapshot.exists {
		return s.writeBytesAtomic(path, snapshot.raw)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (s AutoStore) writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if s.BeforeWrite != nil {
		if err := s.BeforeWrite(path); err != nil {
			return err
		}
	}
	return s.writeBytesAtomic(path, append(raw, '\n'))
}

func (s AutoStore) writeBytesAtomic(path string, raw []byte) error {
	if err := s.ensureRoot(); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(directory, ".auto-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
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
	return syncDirectory(directory)
}

func (s AutoStore) writeWakeExclusive(marker AutoWakeMarker) error {
	if err := s.ensureRoot(); err != nil {
		return err
	}
	if err := validateAutoWake(marker, s.AgentID); err != nil {
		return err
	}
	directory := s.wakeDir()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(directory, marker.ID+".json")
	file, err := os.CreateTemp(directory, ".wake-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// A hard link publishes the fully-written inode atomically while preserving
	// O_EXCL semantics: an existing marker id returns os.ErrExist and is never
	// overwritten. Temporary names are outside the PendingWakes glob.
	if err := os.Link(tmpPath, path); err != nil {
		return err
	}
	if err := syncDirectory(directory); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func (s AutoStore) readJSON(path string, target any) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("curator automation state %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("curator automation state %s is accessible by group or other users", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("parse curator automation state %s: %w", path, err)
	}
	if err := requireAutoJSONEOF(decoder); err != nil {
		return fmt.Errorf("parse curator automation state %s: %w", path, err)
	}
	return nil
}

func requireAutoJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("unexpected trailing JSON value")
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func (s AutoStore) configPath() string { return filepath.Join(s.Root, "auto.json") }
func (s AutoStore) statusPath() string { return filepath.Join(s.Root, "auto-status.json") }
func (s AutoStore) wakeDir() string    { return filepath.Join(s.Root, "wake") }
func (s AutoStore) lockPath() string   { return filepath.Join(s.Root, ".auto.lock") }

func (s AutoStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s AutoStore) sleep(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	if s.Sleep != nil {
		return s.Sleep(ctx, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s AutoStore) newID(prefix string) (string, error) {
	if s.NewID != nil {
		return s.NewID(prefix)
	}
	return id.New(prefix)
}
