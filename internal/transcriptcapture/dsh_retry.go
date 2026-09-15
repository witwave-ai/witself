package transcriptcapture

import (
	"encoding/json"
	"errors"
	"time"
)

const (
	dshNativeRetryWindow = 30 * time.Second
	dshNativeRetryDelay  = 250 * time.Millisecond
)

type dshNativeRetryState struct {
	Deadline time.Time `json:"dsh_native_retry_deadline"`
	At       time.Time `json:"dsh_native_retry_at"`
}

// initDSHNativeRetry starts one durable native-read budget. Restarting a
// detached flusher must retain the first attempt's deadline, otherwise a log
// that never completes could keep spawning work indefinitely.
func initDSHNativeRetry(pending PendingEvent) (PendingEvent, error) {
	var state dshNativeRetryState
	if len(pending.Event.Data) != 0 && json.Unmarshal(pending.Event.Data, &state) != nil {
		return pending, errors.New("invalid dsh native retry state")
	}
	if !state.Deadline.IsZero() {
		if state.At.IsZero() || state.At.After(state.Deadline) {
			return pending, errors.New("invalid dsh native retry schedule")
		}
		return pending, nil
	}
	if !state.At.IsZero() {
		return pending, errors.New("invalid dsh native retry deadline")
	}
	now := time.Now().UTC()
	return persistDSHNativeRetry(pending, dshNativeRetryState{Deadline: now.Add(dshNativeRetryWindow), At: now})
}

// finishDSHNativeRetry records the next bounded detached attempt. A foreground
// flush also leaves this schedule durable, but never waits for it itself.
func finishDSHNativeRetry(pending PendingEvent) (PendingEvent, bool, error) {
	var state dshNativeRetryState
	if json.Unmarshal(pending.Event.Data, &state) != nil || state.Deadline.IsZero() {
		return pending, false, errors.New("missing dsh native retry deadline")
	}
	now := time.Now().UTC()
	if !now.Before(state.Deadline) {
		return pending, true, nil
	}
	state.At = now.Add(dshNativeRetryDelay)
	if state.At.After(state.Deadline) {
		state.At = state.Deadline
	}
	updated, err := persistDSHNativeRetry(pending, state)
	return updated, false, err
}

func persistDSHNativeRetry(pending PendingEvent, state dshNativeRetryState) (PendingEvent, error) {
	var data map[string]any
	if len(pending.Event.Data) != 0 && json.Unmarshal(pending.Event.Data, &data) != nil {
		return pending, errors.New("invalid dsh native retry data")
	}
	if data == nil {
		data = make(map[string]any)
	}
	data["dsh_native_retry_deadline"] = state.Deadline
	data["dsh_native_retry_at"] = state.At
	raw, err := json.Marshal(data)
	if err != nil {
		return pending, err
	}
	pending.Event.Data = raw
	if err := validatePendingRewritePath(pending.Path, pending.Event); err != nil {
		return pending, err
	}
	if err := writeJSONAtomic(pending.Path, pending.Event); err != nil {
		return pending, err
	}
	return pending, nil
}

// DSHNativeRetryAt returns a value-free local retry schedule for an unfinished
// dsh Stop. It intentionally includes expired deadlines: the final attempt
// records a distinguishable terminal outcome before the event can upload.
func DSHNativeRetryAt(pending PendingEvent) (time.Time, bool) {
	event := pending.Event
	if event.Runtime != RuntimeDSH || event.HookEvent != "Stop" || event.NativeTurnFinalized ||
		event.Kind != "turn.completed" || event.Role != "system" ||
		eventSyntheticFence(event.Data) || eventSealedContentOmitted(event.Data) {
		return time.Time{}, false
	}
	var state dshNativeRetryState
	if json.Unmarshal(event.Data, &state) != nil || state.Deadline.IsZero() ||
		state.At.IsZero() || state.At.After(state.Deadline) {
		return time.Time{}, false
	}
	return state.At, true
}

func withoutDSHNativeRetry(raw json.RawMessage) json.RawMessage {
	var data map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &data) != nil {
		return raw
	}
	delete(data, "dsh_native_retry_deadline")
	delete(data, "dsh_native_retry_at")
	result, err := json.Marshal(data)
	if err != nil {
		return raw
	}
	return result
}
