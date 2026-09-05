package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestReleaseHoldSurvivesLegacyHookSessionWrites(t *testing.T) {
	for _, operation := range []string{"rewrite", "session_end", "session_end_then_late_result"} {
		t.Run(operation, func(t *testing.T) {
			prompt := releaseSessionFixture(t, true)
			want := releaseSessionState(t).OperatorRelease
			path, err := sessionStatePath(RuntimeClaudeCode, "s")
			if err != nil {
				t.Fatal(err)
			}
			// Older hooks decode a sessionState struct without operator_release;
			// every write drops that unknown field. SessionEnd removes the file
			// outright, and a late hook recreates it with a new run and no turn.
			// Use the legacy disk schema directly so this regression remains
			// valid after HEAD itself gains release support.
			if operation != "rewrite" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if operation != "session_end" {
				legacy := struct {
					RunID string `json:"run_id"`
				}{RunID: "legacy-recreated-run"}
				if err := writeJSONAtomic(path, legacy); err != nil {
					t.Fatal(err)
				}
			}
			mutable, err := loadMutableSessionState(RuntimeClaudeCode, "s")
			if err != nil || mutable.OperatorRelease != nil {
				t.Fatalf("legacy state retained release metadata: %v", err)
			}
			state := releaseSessionState(t)
			if !reflect.DeepEqual(state.OperatorRelease, want) {
				t.Fatal("legacy hook erased or changed the authoritative release hold")
			}

			// A result emitted by that legacy executable has no release markers
			// and no turn ID. Both uploader entry points must refuse its bytes.
			late := prompt
			late.ID = "evt_legacy_late"
			late.RunID, late.TurnID = "legacy-recreated-run", ""
			late.HookEvent, late.NativeHookEvent = "PostToolUse", "PostToolUse"
			late.Kind, late.Role = "tool.result", "tool"
			late.Body = "LATE_CANARY"
			late.Data = json.RawMessage(`{"tool":{"name":"Bash","output":"LATE_CANARY"}}`)
			late.Raw = json.RawMessage(`{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":"LATE_CANARY"}`)
			late.OccurredAt = time.Now().UTC()
			if err := writeOutboxEvent(late); err != nil {
				t.Fatal(err)
			}
			pending, err := Pending(RuntimeClaudeCode)
			if err != nil {
				t.Fatal(err)
			}
			item := PendingEvent{Event: late}
			if NewReadinessIndex(pending).UploadReady(item) || PendingEventUploadReady(item, pending) {
				t.Fatal("legacy empty-turn result bypassed the durable release hold")
			}
			current := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":"CURRENT_CANARY"}`)
			raw, err := json.Marshal(current)
			if err != nil || bytes.Contains(raw, []byte("CURRENT_CANARY")) || !operatorReleasedEventSafe(current) ||
				!PendingEventUploadReady(PendingEvent{Event: current}, nil) {
				t.Fatal("current hook failed to recover and enforce the release hold")
			}
		})
	}
}

func TestReleaseCorruptDurableHoldFailsClosedAfterLegacyRewrite(t *testing.T) {
	prompt := releaseSessionFixture(t, true)
	path, err := operatorReleaseStatePath(RuntimeClaudeCode, "s")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"status":"completed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if PendingEventUploadReady(PendingEvent{Event: prompt}, nil) {
		t.Fatal("invalid durable release state fell back to mutable session state")
	}
	if _, err := EnqueueHook(RuntimeClaudeCode, []byte(`{"session_id":"s","hook_event_name":"PostToolUse","tool_response":"LATE_CANARY"}`)); err == nil {
		t.Fatal("hook ignored invalid durable release state")
	}
}
