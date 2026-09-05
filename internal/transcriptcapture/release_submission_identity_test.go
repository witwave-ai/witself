package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestReleaseSubmissionIdentitySurvivesInterruptedAcknowledgement(t *testing.T) {
	releaseSessionFixture(t, false)
	enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":"RELEASE_ACK_CRASH_CANARY"}`)
	original, err := Pending(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	canary := false
	oldIDs := map[string]bool{}
	for _, item := range original {
		for _, entry := range item.Entries() {
			oldIDs[entry.ExternalID] = true
			canary = canary || bytes.Contains([]byte(entry.Body), []byte("RELEASE_ACK_CRASH_CANARY"))
		}
	}
	if !canary {
		t.Fatal("original queue lacks canary")
	}
	if n, err := ReleaseResidue(RuntimeClaudeCode, "s", 0, true); err != nil || n != 1 {
		t.Fatalf("release: %d, %v", n, err)
	}
	pending, err := Pending(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string][]byte{}
	for _, item := range pending {
		for _, entry := range item.Entries() {
			if oldIDs[entry.ExternalID] {
				t.Error("release reused an original external ID")
			}
			if bytes.Contains([]byte(entry.Body), []byte("RELEASE_ACK_CRASH_CANARY")) {
				t.Fatal("release retained canary")
			}
		}
		expected[item.Path], err = json.Marshal(item.Entries())
		if err != nil {
			t.Fatal(err)
		}
		if err := MarkPendingSubmitted(item); err != nil {
			t.Fatal(err)
		}
		// Simulate the exact crash between snapshot-first and event-second unlink.
		if err := os.Remove(submissionPath(item.Path)); err != nil {
			t.Fatal(err)
		}
	}
	pending, err = Pending(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pending {
		got, _ := json.Marshal(item.Entries())
		if !bytes.Equal(got, expected[item.Path]) {
			t.Error("snapshot unlink lost replacement identity; next flush would conflict")
		}
		if err := MarkPendingSubmitted(item); err != nil {
			t.Fatal(err)
		}
		if err := RemovePending(item.Path); err != nil {
			t.Fatal(err)
		}
	}
	if err := SweepOrphanSubmissions(RuntimeClaudeCode); err != nil {
		t.Fatal(err)
	}
}
