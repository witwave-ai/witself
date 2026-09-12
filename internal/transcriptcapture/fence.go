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

// FenceReasonResumed marks the synthetic terminal event that closes a run when
// the same provider session is resumed under a new run id.
const FenceReasonResumed = "resumed"

// A runtime may also restart a session's run by clearing it. That is a
// rollover too, but it is not a resume and is not reported as one.
const fenceReasonCleared = "cleared"

const maxFenceReasonBytes = 256

// rolloverFenceReason names why the prior run ended. Claude Code reports its
// own session-start source; runtimes that send none are resuming.
func rolloverFenceReason(source string) string {
	if strings.EqualFold(strings.TrimSpace(source), "clear") {
		return fenceReasonCleared
	}
	return FenceReasonResumed
}

// sessionStartKeepsRun reports a SessionStart that does not end the prior
// run. Claude Code emits one with source "compact" when it compacts context,
// which can happen in the middle of a turn that is still running; the turn
// and its run continue, so nothing is fenced or rebound.
func sessionStartKeepsRun(source string) bool {
	return strings.EqualFold(strings.TrimSpace(source), "compact")
}

type completedFence struct {
	OccurredAt           time.Time `json:"occurred_at"`
	SealedContentOmitted bool      `json:"sealed_content_omitted,omitempty"`
}

type readinessFenceKey struct {
	session readinessSessionKey
	runID   string
	turnID  string
}

// fenceKey identifies one closable run/turn pair inside a capture session.
type fenceKey struct {
	runID  string
	turnID string
}

// fenceTarget is the value-free projection of one run/turn's queued events.
// held is the only durable signal that a terminal event is still missing: it
// reports that the upload gate still holds at least one of those events.
// sealed carries that turn's own sealed-tool suppression, which the run-level
// session flag cannot answer once a fence closes a turn the runtime has left.
type fenceTarget struct {
	key        fenceKey
	sourcePath string
	cwd        string
	held       bool
	terminal   bool
	sealed     bool
}

// EnqueueFence closes the expected run/turn through the same redaction and
// bookkeeping path as Stop. Callers must pin that identity when the job
// starts and reuse it on retries, never resolve the current turn at completion;
// EnqueueLatestFence is the supported alternative for a launcher that cannot
// pin ids. The last completed fence remains a no-op after upload and subsequent
// prompts; other stale identities are refused without changing the current turn.
func EnqueueFence(runtime, sessionID, expectedRunID, expectedTurnID, reason string) (Event, bool, error) {
	runtime, sessionID, reason, err := normalizeFenceRequest(runtime, sessionID, reason)
	if err != nil {
		return Event{}, false, err
	}
	expectedRunID = strings.TrimSpace(expectedRunID)
	expectedTurnID = strings.TrimSpace(expectedTurnID)
	if expectedRunID == "" || expectedTurnID == "" {
		return Event{}, false, errors.New("transcript fence requires an expected run and turn")
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
	targets, err := fenceTargets(cfg, sessionID, state)
	if err != nil {
		return Event{}, false, err
	}
	target, found := findFenceTarget(targets, fenceKey{runID: expectedRunID, turnID: expectedTurnID})
	if state.TurnID == expectedTurnID {
		// The runtime's own open turn keeps the original completion rules: it
		// must still be open, and its events must reuse their real persisted
		// rollout path so Codex ephemeral exclusion is never bypassed.
		if !found || target.terminal || target.sourcePath == "" || state.ResponseCaptured {
			return Event{}, false, errors.New("transcript fence session has no eligible open turn")
		}
	} else {
		// A provider that carries its own turn ids (Cursor, Grok Build) never
		// opens a turn in local state, so the only safe generalization is the
		// upload gate itself: fence a turn exactly while it still holds events.
		if !found || !target.held {
			return Event{}, false, errors.New("transcript fence does not match a held turn in this run")
		}
		if err := fenceTargetPathError(cfg.Runtime, target); err != nil {
			return Event{}, false, err
		}
	}
	event, err := enqueueTurnFence(cfg, sessionID, target, reason, false)
	return event, err == nil, err
}

// EnqueueLatestFence derives the fence identity from local capture state and
// closes every run/turn of that session whose events the upload gate still
// holds, so a headless launcher can fence its job immediately after it exits
// without pinning ids in advance. A session with nothing held is a no-op.
func EnqueueLatestFence(runtime, sessionID, reason string) ([]Event, error) {
	runtime, sessionID, reason, err := normalizeFenceRequest(runtime, sessionID, reason)
	if err != nil {
		return nil, err
	}
	cfg, err := LoadConfig(runtime)
	if err != nil {
		return nil, err
	}
	release, err := acquireSessionStateLock(runtime, sessionID)
	if err != nil {
		return nil, err
	}
	defer release()
	state, err := loadSessionState(runtime, sessionID)
	if err != nil {
		return nil, err
	}
	if state.RunID == "" {
		// The runtime's own SessionEnd removes local state after publishing
		// its terminal, so a launcher that fences unconditionally after a
		// clean exit finds nothing held: that is the documented no-op. Only a
		// session that still holds events without any state is refused,
		// because no local record can prove which run those events belong to.
		targets, err := fenceTargets(cfg, sessionID, state)
		if err != nil {
			return nil, err
		}
		for _, target := range targets {
			if target.held {
				return nil, errors.New("transcript fence session has no local state")
			}
		}
		return nil, nil
	}
	if err := finishPendingFence(runtime, sessionID, &state); err != nil {
		return nil, err
	}
	return fenceHeldTurns(cfg, sessionID, state, reason, false)
}

func normalizeFenceRequest(runtime, sessionID, reason string) (string, string, string, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return "", "", "", errors.New("transcript fence requires a capture runtime")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", "", "", errors.New("transcript fence requires a session")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "job-completed"
	}
	if len(reason) > maxFenceReasonBytes {
		return "", "", "", errors.New("transcript fence reason exceeds 256 bytes")
	}
	return runtime, sessionID, reason, nil
}

// fenceHeldTurns publishes one terminal event for every run/turn of the session
// whose queued events remain held. Runs are fenced in capture order, so a
// resumed session's runs still reach the ledger in the order they happened.
func fenceHeldTurns(cfg Config, sessionID string, state sessionState, reason string, rollover bool) ([]Event, error) {
	targets, err := fenceTargets(cfg, sessionID, state)
	if err != nil {
		return nil, err
	}
	var events []Event
	for _, target := range targets {
		if !target.held || fenceTargetPathError(cfg.Runtime, target) != nil {
			continue
		}
		event, err := enqueueTurnFence(cfg, sessionID, target, reason, rollover)
		if err != nil {
			return events, err
		}
		events = append(events, event)
	}
	return events, nil
}

// fenceTargets projects one session's queued events into closable run/turn
// pairs, in capture order. Events captured under a different binding refuse the
// whole session: a fence must never publish an identity this install cannot own.
//
// A target is sealed exactly when the hook path sealed that turn: one of its
// own queued events already carries the sealed marker, or the session state
// recorded the turn as sealed before a redaction that then failed. The
// session's run-level flag is never projected onto other turns, because the
// hook path clears it at the next real prompt and a fence has no way to tell
// which held turns came before or after that reset.
func fenceTargets(cfg Config, sessionID string, state sessionState) ([]fenceTarget, error) {
	pending, err := Pending(cfg.Runtime)
	if err != nil {
		return nil, err
	}
	index := NewReadinessIndex(pending)
	targets := make([]fenceTarget, 0, 4)
	positions := make(map[fenceKey]int, 4)
	for _, item := range pending {
		event := item.Event
		if event.SessionID != sessionID {
			continue
		}
		if err := EventBindingError(event, cfg); err != nil {
			return nil, err
		}
		if event.TurnID == "" {
			continue
		}
		key := fenceKey{runID: event.RunID, turnID: event.TurnID}
		position, exists := positions[key]
		if !exists {
			position = len(targets)
			positions[key] = position
			targets = append(targets, fenceTarget{key: key})
		}
		target := &targets[position]
		if strings.TrimSpace(event.SourceTranscriptPath) != "" {
			target.sourcePath, target.cwd = event.SourceTranscriptPath, event.CWD
		}
		if eventSealedContentOmitted(event.Data) {
			target.sealed = true
		}
		switch event.HookEvent {
		case "AgentResponse", "Stop", "StopFailure", "SessionEnd":
			target.terminal = true
		}
		if !index.UploadReady(item) {
			target.held = true
		}
	}
	for i := range targets {
		if state.sealedTurnRecorded(targets[i].key.runID, targets[i].key.turnID) {
			targets[i].sealed = true
		}
	}
	return targets, nil
}

func findFenceTarget(targets []fenceTarget, key fenceKey) (fenceTarget, bool) {
	for _, target := range targets {
		if target.key == key {
			return target, true
		}
	}
	return fenceTarget{}, false
}

// fenceTargetPathError keeps Codex's persistence boundary: a run with no
// rollout path is an ephemeral session that capture deliberately excludes, so
// it is never closed by a synthetic terminal event.
func fenceTargetPathError(runtime string, target fenceTarget) error {
	if runtime == RuntimeCodex && target.sourcePath == "" {
		return errors.New("transcript fence session has no eligible open turn")
	}
	return nil
}

// enqueueTurnFence publishes one value-free terminal event for target through
// the shared Stop path, including sealed-turn redaction. The fenced run may
// already be unbound and the fenced turn may no longer be the runtime's own,
// so the run id and that turn's sealed state travel with the request instead
// of being read from the session's current binding.
func enqueueTurnFence(cfg Config, sessionID string, target fenceTarget, reason string, rollover bool) (Event, error) {
	input := hookInput{
		SessionID: sessionID, ConversationID: sessionID,
		TranscriptPath: target.sourcePath, CWD: target.cwd,
		HookEventName: "Stop", NativeHookEvent: "Stop", Reason: reason,
		TurnID: target.key.turnID, SyntheticFence: true, FenceRunID: target.key.runID,
		FenceSealedTurn: target.sealed, FenceRunRollover: rollover,
	}
	if err := normalizeHookInput(cfg.Runtime, &input); err != nil {
		return Event{}, err
	}
	return enqueueHook(cfg, input, nil)
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
	if event.SessionID == "" || event.RunID == "" || event.TurnID == "" {
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
