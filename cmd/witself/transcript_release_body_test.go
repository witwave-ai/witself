package main

import (
	"bytes"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestTranscriptReleaseSessionRedactsSystemBodyCopiesHTTP(t *testing.T) {
	const canary = "RELEASE_SYSTEM_BODY_CANARY"
	for _, test := range []struct {
		name  string
		input map[string]any
	}{
		{"permission_denied_reason", map[string]any{"hook_event_name": "PermissionDenied", "tool_name": "Bash", "tool_input": map[string]any{"command": "printf " + canary}, "reason": "Denied: printf " + canary}},
		{"tool_failure_error", map[string]any{"hook_event_name": "PostToolUseFailure", "tool_name": "Bash", "tool_input": map[string]any{"command": "printf " + canary}, "error": "Failed: " + canary}},
		{"stop_failure_error", map[string]any{"hook_event_name": "StopFailure", "error": "Failed: " + canary}},
		{"stop_failure_structured_error", map[string]any{"hook_event_name": "StopFailure", "error": map[string]any{"message": "Failed: " + canary}}},
		{"stop_failure_error_message", map[string]any{"hook_event_name": "StopFailure", "error_message": "Failed: " + canary}},
		{"stop_failure_reason", map[string]any{"hook_event_name": "StopFailure", "reason": "Failed: " + canary}},
		{"stop_reason", map[string]any{"hook_event_name": "Stop", "reason": "Stopped: " + canary}},
		{"stop_status", map[string]any{"hook_event_name": "Stop", "status": "Stopped: " + canary}},
		{"session_end_reason", map[string]any{"hook_event_name": "SessionEnd", "reason": "Ended: " + canary}},
		{"notification_reason", map[string]any{"hook_event_name": "Notification", "reason": "Notice: " + canary}},
		{"subagent_stop_reason", map[string]any{"hook_event_name": "SubagentStop", "reason": "Stopped: " + canary}},
		{"thought", map[string]any{"hook_event_name": "AgentThought", "last_assistant_message": "Internal: " + canary}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newTranscriptReleaseSessionFixture(t)
			const prompt = "Preserve this captured release prompt."
			const assistant = "Preserve this captured assistant response."
			f.enqueue(map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": prompt})
			if stderr, code := f.release(); code != 0 {
				t.Fatalf("release exit = %d: %s", code, stderr)
			}
			event := f.enqueue(test.input)
			f.enqueue(map[string]any{"hook_event_name": "Stop", "last_assistant_message": assistant})
			pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
			if err != nil {
				t.Fatal(err)
			}
			f.flush()
			if raw := f.assertUploads(prompt, canary); !bytes.Contains(raw, []byte(assistant)) {
				t.Error("release changed the captured assistant response")
			}
			// An operator marker alone cannot authorize a copied reason/error
			// body, including on a stale event snapshot presented to the uploader.
			for _, item := range pending {
				if item.Event.ID == event.ID {
					item.Event.Body = "Copied error: " + canary
					if transcriptcapture.PendingEventUploadReady(item, pending) {
						t.Error("release readiness accepted an unredacted system body")
					}
				}
			}
		})
	}
}
