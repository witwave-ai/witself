package transcriptcapture

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Match the existing bounded native-completion budget. Grok Stops without any
// native correlation cannot be polled; their detached retry is due at expiry.
const grokNativeRetryWindow = dshNativeRetryWindow

type grokNativeRetryState struct {
	Deadline time.Time `json:"grok_native_retry_deadline"`
}

func grokStopWithoutNativeIdentity(event Event) bool {
	if event.Runtime != RuntimeGrokBuild || event.HookEvent != "Stop" ||
		event.Kind != "turn.completed" || event.Role != "system" || event.NativeTurnFinalized ||
		strings.TrimSpace(event.TurnID) != "" || strings.TrimSpace(event.SourceTranscriptPath) != "" ||
		eventSyntheticFence(event.Data) || eventSealedContentOmitted(event.Data) {
		return false
	}
	var data struct {
		PromptID string `json:"prompt_id"`
	}
	return (len(event.Data) == 0 || json.Unmarshal(event.Data, &data) == nil) && strings.TrimSpace(data.PromptID) == ""
}

func finalizeGrokOrphanStop(pending PendingEvent) (PendingEvent, bool, error) {
	event := pending.Event
	var state grokNativeRetryState
	if len(event.Data) != 0 && json.Unmarshal(event.Data, &state) != nil {
		return pending, false, errors.New("invalid grok native retry state")
	}
	now := time.Now().UTC()
	scheduled := !state.Deadline.IsZero()
	if !scheduled {
		// An orphan has nothing to poll. Start its budget at the durable hook
		// time so a backlog of Stops from one session does not serially consume
		// a fresh window per Stop and outlive the detached flush process.
		started := event.OccurredAt
		if started.IsZero() || started.After(now) {
			started = now
		}
		state.Deadline = started.Add(grokNativeRetryWindow)
	}
	if now.Before(state.Deadline) {
		if scheduled {
			return pending, false, nil
		}
		// Persist the deadline before deferring, including for legacy events
		// without a usable timestamp. A restart must not reset their budget.
		var data map[string]any
		if len(event.Data) != 0 && json.Unmarshal(event.Data, &data) != nil {
			return pending, false, errors.New("invalid grok native retry data")
		}
		if data == nil {
			data = make(map[string]any)
		}
		data["grok_native_retry_deadline"] = state.Deadline
		event.Data, _ = json.Marshal(data)
	} else {
		event.NativeTurnFinalized = true
		event.Body, event.Model, event.ModelProvider = "", "", ""
		event.ModelSource, event.ModelProviderSource = "", ""
		event.Raw, event.RecoveredMessages = nil, nil
		event.Data = json.RawMessage(`{"grok_native_finalization":"unresolved_prompt_without_transcript"}`)
	}
	if err := validatePendingRewritePath(pending.Path, event); err != nil {
		return pending, false, err
	}
	if err := writeJSONAtomic(pending.Path, event); err != nil {
		return pending, false, err
	}
	return PendingEvent{Path: pending.Path, Event: event}, event.NativeTurnFinalized, nil
}

// GrokNativeRetryAt returns the durable terminal retry for only a Stop with no
// turn, prompt, or transcript. Stops carrying a real transcript keep their
// existing native-completion behavior.
func GrokNativeRetryAt(pending PendingEvent) (time.Time, bool) {
	if !grokStopWithoutNativeIdentity(pending.Event) {
		return time.Time{}, false
	}
	var state grokNativeRetryState
	if json.Unmarshal(pending.Event.Data, &state) != nil || state.Deadline.IsZero() {
		return time.Time{}, false
	}
	return state.Deadline, true
}

func withoutGrokNativeRetry(raw json.RawMessage) json.RawMessage {
	var data map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &data) != nil {
		return raw
	}
	if _, ok := data["grok_native_retry_deadline"]; !ok {
		return raw
	}
	delete(data, "grok_native_retry_deadline")
	result, err := json.Marshal(data)
	if err != nil {
		return raw
	}
	return result
}
