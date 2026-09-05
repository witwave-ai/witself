package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
)

func TestReleaseReplyMetadataGrowsLinearly(t *testing.T) {
	var persistedBytes []int
	for _, count := range []int{32, 64} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			cfg := setupFenceCapture(t, ModeRaw)
			cfg.Runtime = RuntimeClaudeCode
			if err := SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			enqueueTestHook(t, cfg.Runtime, `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"preserve prompt"}`)
			for i := 1; i < count; i++ {
				enqueueTestHook(t, cfg.Runtime, `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":"RELEASE_LINEAR_METADATA_CANARY"}`)
			}
			if n, err := ReleaseResidue(cfg.Runtime, "s", 0, true); err != nil || n != 1 {
				t.Fatalf("release = %d, %v", n, err)
			}
			pending, err := Pending(cfg.Runtime)
			if err != nil || len(pending) != count+1 {
				t.Fatalf("pending = %d, %v", len(pending), err)
			}
			index := NewReadinessIndex(pending)
			size, mappings := 0, 0
			for _, item := range pending {
				if !index.UploadReady(item) {
					t.Fatal("released event is not upload-ready")
				}
				mappings += len(item.Event.SubmissionReplyEventIDs)
				if item.queuedSubmission != nil {
					mappings += len(item.queuedSubmission.ReplyEventIDs)
				}
				for _, path := range []string{item.Path, submissionPath(item.Path)} {
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(raw, []byte("RELEASE_LINEAR_METADATA_CANARY")) {
						t.Fatal("release retained tool content")
					}
					size += len(raw)
				}
			}
			persistedBytes = append(persistedBytes, size)
			t.Logf("%d events: %d persisted bytes, %d reply mappings", count, size, mappings)
			if mappings != 0 {
				t.Errorf("events without reply references persisted %d reply mappings", mappings)
			}
		})
	}
	if len(persistedBytes) == 2 && persistedBytes[1]*4 > persistedBytes[0]*9 {
		t.Fatalf("doubling events increased persisted bytes from %d to %d; want linear growth", persistedBytes[0], persistedBytes[1])
	}
}

func TestReleaseReplyMetadataTargetsAndRetry(t *testing.T) {
	prompt := releaseSessionFixture(t, false)
	recovered := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":"RELEASE_REPLY_METADATA_CANARY"}`)
	recovered.ReplyToEventID = prompt.ID
	recovered.RecoveredMessages = []RecoveredMessage{
		{ID: "evt_recovered_prompt", Kind: "message.user", Role: "user", Body: "preserve recovered prompt"},
		{ID: "evt_recovered_reply", Kind: "message.assistant", Role: "assistant", Body: "preserve recovered reply", ReplyToEventID: "evt_recovered_prompt"},
	}
	if err := writeOutboxEvent(recovered); err != nil {
		t.Fatal(err)
	}
	reply := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":"RELEASE_REPLY_METADATA_CANARY"}`)
	reply.ReplyToEventID = "evt_recovered_reply"
	reply.RecoveredMessages = []RecoveredMessage{
		{ID: "evt_external_reply", Kind: "message.assistant", Role: "assistant", Body: "preserve external reply", ReplyToEventID: "evt_already_uploaded"},
	}
	if err := writeOutboxEvent(reply); err != nil {
		t.Fatal(err)
	}
	stale, err := Pending(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := ReleaseResidue(RuntimeClaudeCode, "s", 0, true); err != nil || n != 1 {
		t.Fatalf("release = %d, %v", n, err)
	}
	pending, err := Pending(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]PendingEvent)
	for _, item := range pending {
		byID[item.Event.ID] = item
		for _, entry := range item.Entries() {
			if bytes.Contains([]byte(entry.Body), []byte("RELEASE_REPLY_METADATA_CANARY")) {
				t.Fatal("release retained tool content")
			}
		}
	}
	want := map[string]map[string]string{
		recovered.ID: {
			prompt.ID:              releasedSubmissionEventID(prompt.ID, byID[prompt.ID].Event.SubmissionReleaseID),
			"evt_recovered_prompt": releasedSubmissionEventID("evt_recovered_prompt", byID[recovered.ID].Event.SubmissionReleaseID),
		},
		reply.ID: {
			"evt_recovered_reply": releasedSubmissionEventID("evt_recovered_reply", byID[recovered.ID].Event.SubmissionReleaseID),
		},
	}
	for eventID, mappings := range want {
		item := byID[eventID]
		if !reflect.DeepEqual(item.Event.SubmissionReplyEventIDs, mappings) ||
			!reflect.DeepEqual(item.queuedSubmission.ReplyEventIDs, mappings) {
			t.Errorf("event %s persisted unrelated or missing reply targets: got %#v, want %#v", eventID, item.Event.SubmissionReplyEventIDs, mappings)
		}
		for _, original := range stale {
			if original.Event.ID != eventID {
				continue
			}
			retry, err := prepareOperatorReleaseSubmission(original, stale, func(Event) bool { return true }, "different_retry_release")
			if err != nil || retry.SubmissionReleaseID != item.Event.SubmissionReleaseID || !reflect.DeepEqual(retry.SubmissionReplyEventIDs, item.Event.SubmissionReplyEventIDs) {
				t.Fatalf("retry changed durable release namespace or reply mappings: %v", err)
			}
		}
	}
	entries := byID[recovered.ID].Entries()
	if len(entries) != 3 || entries[1].ReplyToExternalID != entries[0].ExternalID ||
		entries[2].ReplyToExternalID != byID[prompt.ID].Entries()[0].ExternalID {
		t.Fatalf("recovered reply chain differs from released entry identities: %#v", entries)
	}
	replies := byID[reply.ID].Entries()
	if len(replies) != 2 || replies[0].ReplyToExternalID != entryExternalID("evt_already_uploaded", 0) ||
		replies[1].ReplyToExternalID != entries[1].ExternalID {
		t.Fatalf("cross-event or already-uploaded reply target changed: %#v", replies)
	}
	// The event copy alone must keep these reply identities durable if its
	// queued record is unavailable after an interrupted acknowledgement.
	for _, eventID := range []string{recovered.ID, reply.ID} {
		raw, err := os.ReadFile(byID[eventID].Path)
		if err != nil {
			t.Fatal(err)
		}
		var event Event
		if err := json.Unmarshal(raw, &event); err != nil || !reflect.DeepEqual(event.Entries(), byID[eventID].Entries()) {
			t.Fatalf("outbox projection lost durable reply identities: %v", err)
		}
	}
}
