package transcriptcapture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func releaseSessionFixture(t *testing.T, release bool) Event {
	t.Helper()
	cfg := setupFenceCapture(t, ModeRaw)
	cfg.Runtime = RuntimeClaudeCode
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	prompt := enqueueTestHook(t, cfg.Runtime, `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"preserve prompt"}`)
	enqueueTestHook(t, cfg.Runtime, `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":"original result"}`)
	if release {
		if count, err := ReleaseResidue(cfg.Runtime, "s", 0, true); err != nil || count != 1 {
			t.Fatalf("release = %d, %v", count, err)
		}
	}
	return prompt
}

func releaseSessionState(t *testing.T) sessionState {
	t.Helper()
	state, err := loadSessionState(RuntimeClaudeCode, "s")
	if err != nil || state.OperatorRelease == nil {
		t.Fatalf("missing durable session release: %v", err)
	}
	return state
}

func TestReleaseSessionHoldSurvivesRewriteFailureAndStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix file permission validation")
	}
	prompt := releaseSessionFixture(t, false)
	pending, err := Pending(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	var resultPath string
	for _, item := range pending {
		if item.Event.Kind == "tool.result" {
			resultPath = item.Path
		}
	}
	if err := os.Chmod(resultPath, 0666); err != nil {
		t.Fatal(err)
	}
	if count, err := ReleaseResidue(RuntimeClaudeCode, "s", 0, true); err == nil || count != 0 {
		t.Fatalf("unsafe rewrite = %d, %v", count, err)
	}
	state := releaseSessionState(t)
	if state.OperatorRelease.Status != "pending" || !state.OperatorRelease.active() ||
		!state.OperatorRelease.releasesTurn(prompt.TurnID) || state.OperatorRelease.Since.IsZero() {
		t.Fatal("rewrite failure did not persist the pending session hold")
	}
	since := state.OperatorRelease.Since
	if err := os.Chmod(resultPath, 0600); err != nil {
		t.Fatal(err)
	}
	enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"Stop"}`)
	state = releaseSessionState(t)
	if state.TurnID != "" || state.OperatorRelease.Status != "pending" || !state.OperatorRelease.Since.Equal(since) {
		t.Fatal("Stop lost or reset the interrupted release identity")
	}
	late := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":"LATE_CANARY"}`)
	if late.TurnID != "" || !operatorReleasedEventSafe(late) {
		t.Fatal("late turn-less result escaped pending session suppression")
	}
	if count, err := ReleaseResidue(RuntimeClaudeCode, "s", 0, true); err != nil || count != 1 {
		t.Fatalf("retry = %d, %v", count, err)
	}
	state = releaseSessionState(t)
	if state.OperatorRelease.Status != "completed" || !state.OperatorRelease.Since.Equal(since) ||
		!PendingEventUploadReady(PendingEvent{Event: late}, nil) {
		t.Fatal("retry did not complete the existing session hold")
	}
}

func TestReleaseSessionSuppressionRequiresMatchingFreshStop(t *testing.T) {
	for _, hook := range []string{"UserPromptSubmit", "AgentResponse", "StopFailure", "UnmatchedStop", "SessionRestart"} {
		t.Run(hook, func(t *testing.T) {
			old := releaseSessionFixture(t, true)
			enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"Stop"}`)
			if hook == "SessionRestart" {
				enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"SessionEnd"}`)
				enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"SessionStart"}`)
			}
			fresh := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"new prompt"}`)
			if fresh.TurnID == old.TurnID || eventOperatorReleased(fresh.Data) {
				t.Fatal("fresh prompt inherited the released turn")
			}
			switch hook {
			case "AgentResponse", "StopFailure":
				raw, _ := json.Marshal(map[string]string{"session_id": "s", "hook_event_name": hook})
				enqueueTestHook(t, RuntimeClaudeCode, string(raw))
			case "UnmatchedStop":
				enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"Stop","turn_id":"not-the-new-turn"}`)
			}
			if !releaseSessionState(t).OperatorRelease.active() {
				t.Fatal("release suppression ended without a matching fresh Stop")
			}
			permission := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"printf RELEASE_CANARY"}}`)
			if permission.TurnID != "" || !operatorReleasedEventSafe(permission) {
				t.Fatal("turn-less permission escaped active session suppression")
			}
			// A new prompt establishes an unambiguous open identity even after
			// StopFailure transitioned the preceding fresh turn.
			fresh = enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"fence this prompt"}`)
			raw, _ := json.Marshal(map[string]string{"session_id": "s", "hook_event_name": "Stop", "turn_id": fresh.TurnID})
			enqueueTestHook(t, RuntimeClaudeCode, string(raw))
			state := releaseSessionState(t)
			if state.OperatorRelease.active() || state.OperatorRelease.FencedAt.IsZero() {
				t.Fatal("matching fresh Stop did not end active suppression")
			}
			ordinary := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"normal post-fence command"}}`)
			if eventOperatorReleased(ordinary.Data) || !strings.Contains(string(ordinary.Data), "normal post-fence command") ||
				!PendingEventUploadReady(PendingEvent{Event: ordinary}, nil) {
				t.Fatal("genuinely fenced session remained suppressed")
			}
		})
	}
}

func TestReleaseSessionReadinessRejectsStaleAndMutatedTurnlessSnapshots(t *testing.T) {
	releaseSessionFixture(t, false)
	stale := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"printf RELEASE_CANARY"}}`)
	if _, err := ReleaseResidue(RuntimeClaudeCode, "s", 0, true); err != nil {
		t.Fatal(err)
	}
	permission := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"safe placeholder source"}}`)
	if !operatorReleasedEventSafe(permission) || !PendingEventUploadReady(PendingEvent{Event: permission}, nil) {
		t.Fatal("safe turn-less release event was held")
	}
	for _, event := range []Event{stale, func() Event {
		event := permission
		event.Raw = json.RawMessage(`{"tool_input":"RELEASE_CANARY"}`)
		return event
	}(), func() Event {
		event := permission
		event.Data = json.RawMessage(`{"operator_release":true,"tool":{"input":"RELEASE_CANARY"}}`)
		return event
	}(), func() Event {
		event := permission
		event.Data = json.RawMessage(`{"operator_release":true,"tool":{"output":"LATE_CANARY"}}`)
		return event
	}()} {
		item := PendingEvent{Event: event}
		if NewReadinessIndex([]PendingEvent{item}).UploadReady(item) || PendingEventUploadReady(item, nil) {
			t.Fatal("empty-turn upload gate accepted an unredacted snapshot")
		}
	}
	enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"fresh turn"}`)
	enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"Stop"}`)
	if PendingEventUploadReady(PendingEvent{Event: stale}, nil) {
		t.Fatal("fresh Stop admitted an old unredacted empty-turn snapshot")
	}
}

func TestReleasePermissionRequestPreservesOriginalPayloadDigest(t *testing.T) {
	releaseSessionFixture(t, true)
	payload, _ := json.Marshal(map[string]string{"command": "printf RELEASE_CANARY" + strings.Repeat("z", 16384)})
	raw, _ := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": json.RawMessage(payload)})
	event := enqueueTestHook(t, RuntimeClaudeCode, string(raw))
	var data struct {
		Tool struct {
			Input releasedPayload `json:"input"`
		} `json:"tool"`
	}
	sum := sha256.Sum256(payload)
	if err := json.Unmarshal(event.Data, &data); err != nil || data.Tool.Input.Redacted != "released_without_fence" ||
		data.Tool.Input.OriginalBytes != len(payload) || data.Tool.Input.SHA256 != hex.EncodeToString(sum[:]) ||
		len(event.Raw) != 0 || !operatorReleasedEventSafe(event) {
		t.Fatal("permission redaction lost the original input digest or retained payload")
	}
}

func TestReleaseSessionReconcilesCompletedMarkerWithoutRewriting(t *testing.T) {
	for _, session := range []string{"s", ""} {
		t.Run("session="+session, func(t *testing.T) {
			releaseSessionFixture(t, true)
			state := releaseSessionState(t)
			state.OperatorRelease.Status = "pending"
			if err := saveSessionState(RuntimeClaudeCode, "s", state); err != nil {
				t.Fatal(err)
			}
			before, err := Pending(RuntimeClaudeCode)
			if err != nil {
				t.Fatal(err)
			}
			original := make(map[string][]byte, len(before))
			for _, item := range before {
				original[item.Path], err = os.ReadFile(item.Path)
				if err != nil {
					t.Fatal(err)
				}
			}
			if count, err := ReleaseResidue(RuntimeClaudeCode, session, 0, true); err != nil || count != 0 {
				t.Fatalf("completion reconciliation = %d, %v", count, err)
			}
			after, err := Pending(RuntimeClaudeCode)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("completion reconciliation changed pending events")
			}
			for path, raw := range original {
				current, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(raw, current) {
					t.Fatal("completion reconciliation rewrote event bytes")
				}
			}
			completed := releaseSessionState(t)
			state.OperatorRelease.Status = "completed"
			if !reflect.DeepEqual(state.OperatorRelease, completed.OperatorRelease) {
				t.Fatal("completion reconciliation did not preserve the release identity")
			}
		})
	}
}

func TestReleaseSessionRetryPreservesGenuineFreshFence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix file permission validation")
	}
	releaseSessionFixture(t, false)
	pending, err := Pending(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	var resultPath string
	for _, item := range pending {
		if item.Event.Kind == "tool.result" {
			resultPath = item.Path
		}
	}
	if err := os.Chmod(resultPath, 0666); err != nil {
		t.Fatal(err)
	}
	if count, err := ReleaseResidue(RuntimeClaudeCode, "s", 0, true); err == nil || count != 0 {
		t.Fatalf("unsafe rewrite = %d, %v", count, err)
	}
	if err := os.Chmod(resultPath, 0600); err != nil {
		t.Fatal(err)
	}
	enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"fresh normal prompt"}`)
	enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"Stop"}`)
	fenced := releaseSessionState(t)
	if fenced.OperatorRelease.FencedAt.IsZero() {
		t.Fatal("fresh Stop did not end active release suppression")
	}
	permission := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"normal post-fence command"}}`)
	if eventOperatorReleased(permission.Data) || !strings.Contains(string(permission.Data), "normal post-fence command") {
		t.Fatal("post-fence permission was suppressed before retry")
	}
	pending, err = Pending(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	var permissionBefore PendingEvent
	var permissionBytes []byte
	for _, item := range pending {
		if item.Event.ID == permission.ID {
			permissionBefore = item
			permissionBytes, err = os.ReadFile(item.Path)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if permissionBefore.Path == "" {
		t.Fatal("post-fence permission was not persisted")
	}
	if count, err := ReleaseResidue(RuntimeClaudeCode, "s", 0, true); err != nil || count != 1 {
		t.Fatalf("retry = %d, %v", count, err)
	}
	completed := releaseSessionState(t)
	fenced.OperatorRelease.Status = "completed"
	if !reflect.DeepEqual(fenced.OperatorRelease, completed.OperatorRelease) {
		t.Fatal("retry reset the original release interval or genuine fence")
	}
	pending, err = Pending(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pending {
		if item.Event.ID == permission.ID {
			currentBytes, err := os.ReadFile(item.Path)
			if err != nil || !bytes.Equal(permissionBytes, currentBytes) || !reflect.DeepEqual(item, permissionBefore) {
				t.Fatal("retry changed ordinary post-fence permission bytes")
			}
			if !PendingEventUploadReady(item, pending) {
				t.Fatal("retry held ordinary post-fence permission")
			}
			return
		}
	}
	t.Fatal("retry removed the post-fence permission")
}

func TestReleaseSessionFreshStopWriteFailurePreservesRetryIdentity(t *testing.T) {
	releaseSessionFixture(t, true)
	prompt := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"fresh normal prompt"}`)
	dir, err := outboxDir(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnqueueHook(RuntimeClaudeCode, []byte(`{"session_id":"s","hook_event_name":"Stop"}`)); err == nil {
		t.Fatal("Stop unexpectedly wrote through the blocked outbox")
	}
	state := releaseSessionState(t)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir+".saved", dir); err != nil {
		t.Fatal(err)
	}
	if state.TurnID != prompt.TurnID || state.PromptEventID != prompt.ID || !state.OperatorRelease.active() {
		t.Fatal("failed Stop lost the fresh identity needed for retry")
	}
	stop := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"Stop"}`)
	state = releaseSessionState(t)
	if stop.TurnID != prompt.TurnID || state.OperatorRelease.active() || state.OperatorRelease.FencedAt.IsZero() {
		t.Fatal("retried genuine Stop did not end session suppression")
	}
}
