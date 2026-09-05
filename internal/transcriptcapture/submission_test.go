package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestReleaseSubmissionProvenanceQueuedAttemptedAndLegacy(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "legacy"}[legacy], func(t *testing.T) {
			cfg := setupFenceCapture(t, ModeRaw)
			cfg.Runtime = RuntimeClaudeCode
			if err := SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			event := enqueueTestHook(t, cfg.Runtime, `{"session_id":"s","hook_event_name":"SessionStart","reason":"SUBMISSION_CONTENT_CANARY"}`)
			pending, err := Pending(cfg.Runtime)
			if err != nil || len(pending) != 1 {
				t.Fatalf("queued events = %d, %v", len(pending), err)
			}
			item := pending[0]
			queued, err := os.ReadFile(submissionPath(item.Path))
			if err != nil || bytes.Contains(queued, []byte("SUBMISSION_CONTENT_CANARY")) || !item.tracked || item.submission != nil {
				t.Fatalf("new event lacks value-free queued provenance: %v", err)
			}
			if legacy {
				if err := os.Remove(submissionPath(item.Path)); err != nil {
					t.Fatal(err)
				}
				// A retry writing the existing path cannot invent evidence that
				// this event was never attempted by an older uploader.
				if err := writeOutboxEvent(event); err != nil {
					t.Fatal(err)
				}
				pending, err = Pending(cfg.Runtime)
				if err != nil || pending[0].tracked || !errors.Is(operatorReleaseRewriteAllowed(pending[0]), errLegacySubmissionUnknown) {
					t.Fatalf("legacy event acquired invented queued provenance: %v", err)
				}
				item = pending[0]
			}
			if !PendingEventUploadReady(item, pending) {
				t.Fatal("ordinary ready legacy metadata cannot reconcile through normal flush")
			}
			wantEvent, _ := json.Marshal(item.Event)
			wantEntries, _ := json.Marshal(item.Entries())
			if err := MarkPendingSubmitted(item); err != nil {
				t.Fatal(err)
			}
			attempted, err := os.ReadFile(submissionPath(item.Path))
			if err != nil || !bytes.Contains(attempted, []byte("SUBMISSION_CONTENT_CANARY")) {
				t.Fatalf("attempt did not freeze the original envelope: %v", err)
			}
			changed := event
			changed.Body = "subsequent outbox rewrite"
			if err := writeOutboxEvent(changed); err != nil {
				t.Fatal(err)
			}
			pending, err = Pending(cfg.Runtime)
			if err != nil || len(pending) != 1 {
				t.Fatalf("attempted events = %d, %v", len(pending), err)
			}
			gotEvent, _ := json.Marshal(pending[0].Event)
			gotEntries, _ := json.Marshal(pending[0].Entries())
			if pending[0].submission == nil || !bytes.Equal(gotEvent, wantEvent) || !bytes.Equal(gotEntries, wantEntries) {
				t.Fatalf("outbox rewrite changed the immutable attempted envelope: %v", err)
			}
			if after, err := os.ReadFile(submissionPath(item.Path)); err != nil || !bytes.Equal(attempted, after) {
				t.Fatalf("outbox retry replaced attempted provenance: %v", err)
			}
			if err := RemovePending(item.Path); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(submissionPath(item.Path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("acknowledgement retained attempted content: %v", err)
			}
		})
	}
}

func TestReleaseRefusesLegacyHistoryBeforeMutation(t *testing.T) {
	for _, kind := range []string{"message.user", "tool.result", "session.started"} {
		t.Run(kind, func(t *testing.T) {
			prompt := releaseSessionFixture(t, false)
			if kind == "session.started" {
				legacy := prompt
				legacy.ID, legacy.TurnID = "evt_legacy_started", ""
				legacy.Kind, legacy.HookEvent, legacy.NativeHookEvent = kind, "SessionStart", "SessionStart"
				legacy.Role, legacy.Body = "system", "session started"
				legacy.OccurredAt = prompt.OccurredAt.Add(-time.Second)
				if err := writeOutboxEvent(legacy); err != nil {
					t.Fatal(err)
				}
			}
			pending, err := Pending(RuntimeClaudeCode)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range pending {
				if item.Event.Kind == kind {
					if err := os.Remove(submissionPath(item.Path)); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := releaseFileSnapshot(t)
			if count, err := ReleaseResidue(RuntimeClaudeCode, "s", 0, true); count != 0 || !errors.Is(err, errLegacySubmissionUnknown) {
				t.Fatalf("legacy release = %d, %v", count, err)
			}
			if after := releaseFileSnapshot(t); !reflect.DeepEqual(before, after) {
				t.Fatal("ambiguous legacy release changed files before refusing")
			}
			state, err := loadSessionState(RuntimeClaudeCode, "s")
			if err != nil || state.OperatorRelease != nil {
				t.Fatalf("ambiguous legacy release published a hold: %v", err)
			}
		})
	}
}

func TestReleaseRefusesLegacyAlreadyRedactedNoop(t *testing.T) {
	releaseSessionFixture(t, false)
	enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":"LEGACY_REDACTED_CANARY"}`)
	pending, err := Pending(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	var path string
	var before []byte
	for _, item := range pending {
		if item.Event.Kind != "tool.result" {
			continue
		}
		path = item.Path
		event := redactOperatorReleasedEvent(item.Event)
		if err := writeJSONAtomic(path, event); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(submissionPath(path)); err != nil {
			t.Fatal(err)
		}
		before, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if count, err := ReleaseResidue(RuntimeClaudeCode, "s", 0, true); count != 0 || !errors.Is(err, errLegacySubmissionUnknown) {
		t.Fatalf("already redacted legacy release = %d, %v; want refusal", count, err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("release changed an already redacted legacy envelope: %v", err)
	}
}

func TestReleaseSubmissionRejectsUnknownState(t *testing.T) {
	releaseSessionFixture(t, false)
	pending, err := Pending(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	item := pending[0]
	for _, state := range []string{"", "unexpected"} {
		snapshot := pendingSubmission{State: state, Event: &item.Event, Entries: item.Entries()}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(submissionPath(item.Path), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Pending(RuntimeClaudeCode); err == nil {
			t.Fatalf("accepted unrecognized submission state %q", state)
		}
	}
}
