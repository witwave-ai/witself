package transcriptcapture

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type completedFence struct {
	OccurredAt           time.Time `json:"occurred_at"`
	SealedContentOmitted bool      `json:"sealed_content_omitted,omitempty"`
}

type readinessFenceKey struct {
	session readinessSessionKey
	runID   string
	turnID  string
}

// EnqueueFence closes the expected Codex run/turn through the same redaction
// and bookkeeping path as Stop. Callers must pin that identity when the job
// starts and reuse it on retries, never resolve the current turn at completion.
// The last completed fence remains a no-op after upload and subsequent prompts;
// other stale identities are refused without changing the current turn.
func EnqueueFence(runtime, sessionID, expectedRunID, expectedTurnID, reason string) (Event, bool, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil || runtime != RuntimeCodex {
		return Event{}, false, errors.New("transcript fence requires runtime codex")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Event{}, false, errors.New("transcript fence requires a session")
	}
	expectedRunID = strings.TrimSpace(expectedRunID)
	expectedTurnID = strings.TrimSpace(expectedTurnID)
	if expectedRunID == "" || expectedTurnID == "" {
		return Event{}, false, errors.New("transcript fence requires an expected run and turn")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "job-completed"
	}
	if len(reason) > 256 {
		return Event{}, false, errors.New("transcript fence reason exceeds 256 bytes")
	}
	cfg, err := LoadConfig(runtime)
	if err != nil {
		return Event{}, false, err
	}
	release, err := acquireSessionStateLock(runtime, sessionID)
	if err != nil {
		return Event{}, false, err
	}
	defer release()
	state, err := loadSessionState(runtime, sessionID)
	if err != nil {
		return Event{}, false, err
	}
	if state.RunID == "" {
		return Event{}, false, errors.New("transcript fence session has no local state")
	}
	if state.RunID != expectedRunID {
		return Event{}, false, errors.New("transcript fence does not match the current run")
	}
	if state.PendingFence != nil {
		event := *state.PendingFence
		if event.RunID != expectedRunID || event.TurnID != expectedTurnID {
			return Event{}, false, errors.New("transcript fence does not match the pending completion")
		}
		if err := EventBindingError(event, cfg); err != nil {
			return Event{}, false, err
		}
		if err := finishPendingFence(runtime, sessionID, &state); err != nil {
			return Event{}, false, err
		}
		return event, true, nil
	}
	if state.SyntheticFencedTurnID == expectedTurnID {
		// Completion readiness is durable outside the removable outbox, so a
		// partial upload does not require recreating this terminal event.
		return Event{}, false, nil
	}
	if state.TurnID == "" {
		return Event{}, false, errors.New("transcript fence session has no open turn")
	}
	if state.TurnID != expectedTurnID {
		return Event{}, false, errors.New("transcript fence does not match the open turn")
	}
	pending, err := Pending(runtime)
	if err != nil {
		return Event{}, false, err
	}
	// An open turn's events are held by the upload gate. Reuse their real
	// persisted-rollout path, including for state written before fence support;
	// never invent a path that would bypass Codex ephemeral exclusion.
	var sourcePath, cwd string
	for _, item := range pending {
		event := item.Event
		if event.SessionID != sessionID || event.RunID != state.RunID || event.TurnID != state.TurnID {
			continue
		}
		if err := EventBindingError(event, cfg); err != nil {
			return Event{}, false, err
		}
		switch event.HookEvent {
		case "AgentResponse", "Stop", "StopFailure", "SessionEnd":
			return Event{}, false, errors.New("transcript fence session has no open turn")
		}
		if strings.TrimSpace(event.SourceTranscriptPath) != "" {
			sourcePath, cwd = event.SourceTranscriptPath, event.CWD
		}
	}
	if sourcePath == "" || state.ResponseCaptured {
		return Event{}, false, errors.New("transcript fence session has no eligible open turn")
	}
	input := hookInput{
		SessionID: sessionID, TranscriptPath: sourcePath, CWD: cwd,
		HookEventName: "Stop", NativeHookEvent: "Stop", Reason: reason,
		SyntheticFence: true,
	}
	if err := normalizeHookInput(runtime, &input); err != nil {
		return Event{}, false, err
	}
	event, err := enqueueHook(cfg, input, nil)
	return event, err == nil, err
}

func finishPendingFence(runtime, sessionID string, state *sessionState) error {
	if state.PendingFence == nil {
		return nil
	}
	// enqueueHook has already completed the shared Stop redaction path and
	// saved this exact pending fence. Persist readiness before publishing the
	// terminal event, which the flush fallback may acknowledge independently.
	path, err := completedFencePath(*state.PendingFence)
	if err != nil {
		return err
	}
	if err := writeJSONAtomic(path, completedFence{
		OccurredAt:           state.PendingFence.OccurredAt,
		SealedContentOmitted: eventSealedContentOmitted(state.PendingFence.Data),
	}); err != nil {
		return err
	}
	if err := writeOutboxEvent(*state.PendingFence); err != nil {
		return err
	}
	state.PendingFence = nil
	return saveSessionState(runtime, sessionID, *state)
}

// Keep one bounded, value-free marker per fenced run/turn. SessionEnd and a
// later run may replace session state while rejected outbox events still exist.
func completedFencePath(event Event) (string, error) {
	path, err := sessionStatePath(event.Runtime, event.SessionID)
	if err != nil {
		return "", err
	}
	key := event.TranscriptExternalID() + "\x00" + event.RunID + "\x00" + event.TurnID
	return filepath.Join(filepath.Dir(path), "completed", sessionHash(event.SessionID), sessionHash(key)+".json"), nil
}

func loadCompletedFence(event Event) completedFence {
	if event.Runtime != RuntimeCodex || event.SessionID == "" || event.RunID == "" || event.TurnID == "" {
		return completedFence{}
	}
	path, err := completedFencePath(event)
	if err != nil {
		return completedFence{}
	}
	file, err := os.Open(path)
	if err != nil {
		return completedFence{}
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, 1025))
	var fence completedFence
	if err != nil || len(raw) > 1024 || json.Unmarshal(raw, &fence) != nil {
		return completedFence{}
	}
	return fence
}

// Kernel locks serialize hooks and companion fences across processes and are
// released automatically when a hook process exits. Keep the lock inode in
// place so a concurrent waiter cannot acquire a different file for the session.
func acquireSessionStateLock(runtime, sessionID string) (func(), error) {
	path, err := sessionStatePath(runtime, sessionID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockSessionFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = file.Close() }, nil
}
