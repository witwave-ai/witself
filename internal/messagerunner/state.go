package messagerunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"golang.org/x/sys/unix"
)

const (
	// RunnerStateSchemaV1 identifies private, content-free operational state.
	RunnerStateSchemaV1 = "witself.message-runner-state.v1"

	maximumRunnerNotifications  = 1024
	maximumRunnerStateBytes     = 4 * 1024 * 1024
	maximumOperationalIDLength  = 256
	maximumOperationalNameBytes = 256
)

// OperationalState is the trusted parent runner's content-free local
// notification boundary. Turn depth and processing attempts are backend-owned.
type OperationalState interface {
	RecordNotification(context.Context, client.Message) error
}

// Notification is a durable, content-free pointer to a terminal or otherwise
// non-actionable message that the background runner acknowledged. The message
// body remains only in Witself and can be read through the ordinary message
// read command.
type Notification struct {
	MessageID     string    `json:"message_id"`
	ThreadID      string    `json:"thread_id"`
	Kind          string    `json:"kind"`
	FromAgentID   string    `json:"from_agent_id"`
	FromAgentName string    `json:"from_agent_name"`
	CreatedAt     time.Time `json:"created_at"`
	RecordedAt    time.Time `json:"recorded_at"`
}

// Health is a content-free summary of the background runner's latest cycle.
// It never retains an error string, message body, subject, or payload.
type Health struct {
	LastCycleAt         time.Time `json:"last_cycle_at,omitempty"`
	LastSuccessAt       time.Time `json:"last_success_at,omitempty"`
	LastStatus          string    `json:"last_status,omitempty"`
	LastErrorClass      string    `json:"last_error_class,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
}

type persistedRunnerState struct {
	Schema        string         `json:"schema"`
	Revision      int64          `json:"revision"`
	Notifications []Notification `json:"notifications,omitempty"`
	Health        Health         `json:"health"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// RecordNotification persists only routing metadata, never subject, body, or
// payload. It is idempotent by message ID.
func (s ConfigStore) RecordNotification(ctx context.Context, message client.Message) error {
	if err := validateOperationalContext(ctx); err != nil {
		return err
	}
	for label, value := range map[string]string{
		"message": message.ID, "thread": message.ThreadID, "agent": message.From.AgentID,
	} {
		if err := validateOperationalID(label, value); err != nil {
			return err
		}
	}
	kind := normalizeNotificationKind(message.Kind)
	if kind == "" || len(kind) > 64 {
		return fmt.Errorf("%w: notification kind is invalid", ErrInvalidConfiguration)
	}
	release, err := s.lockOperationalState()
	if err != nil {
		return err
	}
	defer func() { _ = release() }()
	state, err := s.loadOperationalState()
	if err != nil {
		return err
	}
	for _, existing := range state.Notifications {
		if existing.MessageID == message.ID {
			return nil
		}
	}
	if len(state.Notifications) >= maximumRunnerNotifications {
		return errors.New("message runner notification ledger is full; inspect and clear notifications before continuing")
	}
	state.Notifications = append(state.Notifications, Notification{
		MessageID: message.ID, ThreadID: message.ThreadID, Kind: kind,
		FromAgentID:   message.From.AgentID,
		FromAgentName: normalizeNotificationAgentName(message.From.AgentName),
		CreatedAt:     message.CreatedAt.UTC(), RecordedAt: s.operationalNow(),
	})
	return s.saveOperationalState(&state)
}

// NotificationMatchesMessage verifies every canonical message field retained
// by a content-free notification pointer. Agent display names use the same
// bounded normalization as RecordNotification; the immutable agent ID remains
// the exact sender identity boundary.
func NotificationMatchesMessage(notification Notification, message client.Message) bool {
	return notification.MessageID == message.ID && notification.ThreadID == message.ThreadID &&
		notification.Kind == normalizeNotificationKind(message.Kind) &&
		notification.FromAgentID == message.From.AgentID &&
		notification.FromAgentName == normalizeNotificationAgentName(message.From.AgentName) &&
		notification.CreatedAt.Equal(message.CreatedAt)
}

func normalizeNotificationKind(kind string) string {
	return strings.ToLower(strings.TrimSpace(kind))
}

func normalizeNotificationAgentName(name string) string {
	return truncateUTF8(strings.TrimSpace(name), maximumOperationalNameBytes)
}

// Notifications returns the bounded content-free notification ledger newest
// first. Reading it does not delete or mutate entries.
func (s ConfigStore) Notifications(ctx context.Context) ([]Notification, error) {
	if err := validateOperationalContext(ctx); err != nil {
		return nil, err
	}
	release, err := s.lockOperationalState()
	if err != nil {
		return nil, err
	}
	defer func() { _ = release() }()
	state, err := s.loadOperationalState()
	if err != nil {
		return nil, err
	}
	result := append([]Notification(nil), state.Notifications...)
	sort.SliceStable(result, func(i, j int) bool { return result[i].RecordedAt.After(result[j].RecordedAt) })
	return result, nil
}

// RecordCycle updates content-free health after one runner cycle.
func (s ConfigStore) RecordCycle(ctx context.Context, result RunResult, cycleErr error) error {
	if err := validateOperationalContext(ctx); err != nil {
		return err
	}
	release, err := s.lockOperationalState()
	if err != nil {
		return err
	}
	defer func() { _ = release() }()
	state, err := s.loadOperationalState()
	if err != nil {
		return err
	}
	now := s.operationalNow()
	state.Health.LastCycleAt = now
	if cycleErr == nil {
		status := strings.TrimSpace(result.Status)
		if !validRunnerCycleStatus(status) {
			status = "ok"
		}
		state.Health.LastStatus = status
		state.Health.LastSuccessAt = now
		state.Health.LastErrorClass = ""
		state.Health.ConsecutiveFailures = 0
	} else {
		state.Health.LastStatus = "error"
		state.Health.LastErrorClass = classifyRunnerCycleError(cycleErr)
		state.Health.ConsecutiveFailures++
	}
	return s.saveOperationalState(&state)
}

// RunnerHealth returns the content-free cycle health snapshot.
func (s ConfigStore) RunnerHealth(ctx context.Context) (Health, error) {
	if err := validateOperationalContext(ctx); err != nil {
		return Health{}, err
	}
	release, err := s.lockOperationalState()
	if err != nil {
		return Health{}, err
	}
	defer func() { _ = release() }()
	state, err := s.loadOperationalState()
	if err != nil {
		return Health{}, err
	}
	return state.Health, nil
}

// ClearNotifications removes either the exact supplied message IDs or every
// recorded pointer when no IDs are supplied. The operation is idempotent and
// serialized with the background runner's notification writes.
func (s ConfigStore) ClearNotifications(ctx context.Context, messageIDs []string) (int, error) {
	if err := validateOperationalContext(ctx); err != nil {
		return 0, err
	}
	wanted := make(map[string]struct{}, len(messageIDs))
	for _, messageID := range messageIDs {
		messageID = strings.TrimSpace(messageID)
		if err := validateOperationalID("message", messageID); err != nil {
			return 0, err
		}
		wanted[messageID] = struct{}{}
	}
	release, err := s.lockOperationalState()
	if err != nil {
		return 0, err
	}
	defer func() { _ = release() }()
	state, err := s.loadOperationalState()
	if err != nil {
		return 0, err
	}
	before := len(state.Notifications)
	if len(wanted) == 0 {
		state.Notifications = nil
	} else {
		kept := state.Notifications[:0]
		for _, notification := range state.Notifications {
			if _, remove := wanted[notification.MessageID]; !remove {
				kept = append(kept, notification)
			}
		}
		state.Notifications = append([]Notification(nil), kept...)
	}
	removed := before - len(state.Notifications)
	if removed == 0 {
		return 0, nil
	}
	if err := s.saveOperationalState(&state); err != nil {
		return 0, err
	}
	return removed, nil
}

// ConsumeNotification removes one exact pointer under the same lock used by
// RecordNotification and binding replacement. Matching the complete
// content-free record, rather than only its message id, prevents a stale
// foreground reader from clearing a pointer written after a runtime rebind.
func (s ConfigStore) ConsumeNotification(ctx context.Context, expected Notification) (bool, error) {
	if err := validateOperationalContext(ctx); err != nil {
		return false, err
	}
	if err := validateNotification(expected); err != nil {
		return false, err
	}
	release, err := s.lockOperationalState()
	if err != nil {
		return false, err
	}
	defer func() { _ = release() }()
	state, err := s.loadOperationalState()
	if err != nil {
		return false, err
	}
	index := -1
	for i := range state.Notifications {
		if state.Notifications[i].MessageID != expected.MessageID {
			continue
		}
		if !sameNotification(state.Notifications[i], expected) {
			return false, errors.New("message runner notification changed before consumption")
		}
		index = i
		break
	}
	if index < 0 {
		return false, nil
	}
	state.Notifications = append(state.Notifications[:index:index], state.Notifications[index+1:]...)
	if err := s.saveOperationalState(&state); err != nil {
		return false, err
	}
	return true, nil
}

func (s ConfigStore) replaceBindingConfig(config PersistedConfig) error {
	release, err := s.lockOperationalState()
	if err != nil {
		return err
	}
	defer func() { _ = release() }()
	previous, err := s.loadOperationalState()
	if err != nil {
		return err
	}
	empty := persistedRunnerState{Schema: RunnerStateSchemaV1}
	if err := s.saveOperationalState(&empty); err != nil {
		return err
	}
	if err := writePrivateJSONAtomic(s.configPath(), config); err != nil {
		restoreErr := writePrivateJSONAtomic(s.statePath(), previous)
		return errors.Join(err, restoreErr)
	}
	return nil
}

func (s ConfigStore) statePath() string {
	return filepath.Join(s.Root, s.Runtime, "state.json")
}

func (s ConfigStore) stateLockPath() string {
	return filepath.Join(s.Root, s.Runtime, "state.lock")
}

func (s ConfigStore) lockOperationalState() (func() error, error) {
	path := s.stateLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() error {
		return errors.Join(unix.Flock(int(file.Fd()), unix.LOCK_UN), file.Close())
	}, nil
}

func (s ConfigStore) loadOperationalState() (persistedRunnerState, error) {
	path := s.statePath()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return persistedRunnerState{Schema: RunnerStateSchemaV1}, nil
	}
	if err != nil {
		return persistedRunnerState{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return persistedRunnerState{}, fmt.Errorf("message runner state %s must be a private regular file", path)
	}
	if info.Size() > maximumRunnerStateBytes {
		return persistedRunnerState{}, errors.New("message runner state exceeds its size limit")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return persistedRunnerState{}, err
	}
	var state persistedRunnerState
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return persistedRunnerState{}, fmt.Errorf("parse message runner state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return persistedRunnerState{}, errors.New("parse message runner state: trailing data")
	}
	if err := validateOperationalState(state); err != nil {
		return persistedRunnerState{}, err
	}
	return state, nil
}

func (s ConfigStore) saveOperationalState(state *persistedRunnerState) error {
	state.Schema = RunnerStateSchemaV1
	state.Revision++
	state.UpdatedAt = s.operationalNow()
	if err := validateOperationalState(*state); err != nil {
		return err
	}
	return writePrivateJSONAtomic(s.statePath(), state)
}

func (s ConfigStore) operationalNow() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func validateOperationalContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: operational state context is required", ErrInvalidConfiguration)
	}
	return ctx.Err()
}

func validateOperationalID(label, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maximumOperationalIDLength {
		return fmt.Errorf("%w: %s id is invalid", ErrInvalidConfiguration, label)
	}
	return nil
}

func validateOperationalState(state persistedRunnerState) error {
	if state.Schema != RunnerStateSchemaV1 || state.Revision < 0 ||
		len(state.Notifications) > maximumRunnerNotifications {
		return fmt.Errorf("%w: persisted runner state is invalid", ErrInvalidConfiguration)
	}
	if state.Revision > 0 && state.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: persisted runner state timestamp is invalid", ErrInvalidConfiguration)
	}
	if state.Health.ConsecutiveFailures < 0 || state.Health.ConsecutiveFailures > 1_000_000 ||
		(state.Health.LastStatus != "" && !validRunnerCycleStatus(state.Health.LastStatus)) ||
		!validRunnerErrorClass(state.Health.LastErrorClass) {
		return fmt.Errorf("%w: persisted runner health is invalid", ErrInvalidConfiguration)
	}
	seen := map[string]struct{}{}
	for _, notification := range state.Notifications {
		if err := validateNotification(notification); err != nil {
			return err
		}
		if _, exists := seen[notification.MessageID]; exists {
			return fmt.Errorf("%w: persisted runner notification is duplicated", ErrInvalidConfiguration)
		}
		seen[notification.MessageID] = struct{}{}
	}
	return nil
}

func validateNotification(notification Notification) error {
	if validateOperationalID("message", notification.MessageID) != nil ||
		validateOperationalID("thread", notification.ThreadID) != nil ||
		validateOperationalID("agent", notification.FromAgentID) != nil ||
		notification.Kind == "" || len(notification.Kind) > 64 ||
		len(notification.FromAgentName) > maximumOperationalNameBytes || notification.RecordedAt.IsZero() {
		return fmt.Errorf("%w: persisted runner notification is invalid", ErrInvalidConfiguration)
	}
	return nil
}

func sameNotification(left, right Notification) bool {
	return left.MessageID == right.MessageID && left.ThreadID == right.ThreadID &&
		left.Kind == right.Kind && left.FromAgentID == right.FromAgentID &&
		left.FromAgentName == right.FromAgentName && left.CreatedAt.Equal(right.CreatedAt) &&
		left.RecordedAt.Equal(right.RecordedAt)
}

func validRunnerCycleStatus(status string) bool {
	switch status {
	case "", "ok", "error", RunStatusIdle, RunStatusNotified, RunStatusCompleted, RunStatusRecovered:
		return true
	default:
		return false
	}
}

func validRunnerErrorClass(class string) bool {
	switch class {
	case "", "configuration", "identity", "provider", "cancelled", "cycle":
		return true
	default:
		return false
	}
}

func classifyRunnerCycleError(err error) string {
	switch {
	case errors.Is(err, ErrInvalidConfiguration):
		return "configuration"
	case errors.Is(err, ErrIdentityMismatch):
		return "identity"
	case errors.Is(err, ErrProviderOutputInvalid), errors.Is(err, ErrProviderResultInvalid),
		errors.Is(err, ErrProviderUnavailable), errors.Is(err, ErrNativeProviderUnsupported),
		errors.Is(err, ErrNativeProviderCommand):
		return "provider"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	default:
		return "cycle"
	}
}
