package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestTranscriptReleasePreservesSubmittedEntries(t *testing.T) {
	for _, failure := range []string{"activity-failure", "lost-append-ack", "legacy-activity-failure", "legacy-lost-append-ack"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
			t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
			var mu sync.Mutex
			accepted := map[string][]byte{}
			attempts := map[string]int{}
			firstActivity, firstAppend := true, true
			conflicts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.Method + " " + r.URL.Path {
				case "POST /v1/self/activity":
					if firstActivity && strings.HasSuffix(failure, "activity-failure") {
						firstActivity = false
						http.Error(w, "activity unavailable", http.StatusInternalServerError)
						return
					}
					_, _ = io.WriteString(w, `{"activity":{"last_activity_at":"2026-09-05T12:00:00Z"}}`)
				case "POST /v1/transcripts":
					_, _ = io.WriteString(w, `{"transcript":{"id":"trn_submitted","metadata":{}}}`)
				case "POST /v1/transcripts/trn_submitted/entries:batch":
					var body struct {
						Entries []client.AppendTranscriptEntryInput `json:"entries"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode append: %v", err)
						http.Error(w, "invalid append", http.StatusBadRequest)
						return
					}
					pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
					if err != nil {
						t.Errorf("read pending during append: %v", err)
					}
					frozen := map[string]bool{}
					for _, item := range pending {
						path := filepath.Join(filepath.Dir(item.Path), ".submissions", filepath.Base(item.Path))
						raw, err := os.ReadFile(path)
						var record struct {
							State string `json:"state"`
						}
						if err == nil && json.Unmarshal(raw, &record) == nil && record.State == "attempted" {
							for _, entry := range item.Entries() {
								frozen[entry.ExternalID] = true
							}
						}
					}
					for _, entry := range body.Entries {
						if !frozen[entry.ExternalID] {
							t.Error("append reached the server before its immutable attempt was persisted")
						}
						encoded, _ := json.Marshal(entry)
						attempts[entry.ExternalID]++
						if previous, exists := accepted[entry.ExternalID]; exists && !bytes.Equal(previous, encoded) {
							conflicts++
							// Match the server's immutable external-ID contract.
							http.Error(w, "transcript entry already exists", http.StatusConflict)
							return
						}
						accepted[entry.ExternalID] = encoded
					}
					if firstAppend && strings.HasSuffix(failure, "lost-append-ack") {
						firstAppend = false
						// The server committed the entries, but its acknowledgement
						// never reached the client as a successful response.
						http.Error(w, "acknowledgement lost", http.StatusInternalServerError)
						return
					}
					_, _ = io.WriteString(w, `{"entries":[]}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, server.URL)
			start, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(`{"session_id":"submitted","hook_event_name":"SessionStart"}`))
			if err != nil {
				t.Fatal(err)
			}
			flush := func() int {
				_, _, code := captureFactDeleteCLI(t, func() int {
					return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode})
				})
				return code
			}
			if code := flush(); code != 1 {
				t.Fatalf("initial flush exit = %d, want transient failure", code)
			}
			pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
			if err != nil || len(pending) != 1 {
				t.Fatalf("accepted event must remain pending: %d, %v", len(pending), err)
			}
			before, err := os.ReadFile(pending[0].Path)
			if err != nil {
				t.Fatal(err)
			}
			legacy := strings.HasPrefix(failure, "legacy-")
			if legacy {
				// Old uploaders leave only the outbox event after a lost ack.
				// Missing history must not be mistaken for proof of no attempt.
				path := filepath.Join(filepath.Dir(pending[0].Path), ".submissions", filepath.Base(pending[0].Path))
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(`{"session_id":"submitted","hook_event_name":"UserPromptSubmit","prompt":"p"}`)); err != nil {
				t.Fatal(err)
			}
			count, releaseErr := transcriptcapture.ReleaseResidue(transcriptcapture.RuntimeClaudeCode, "submitted", 0, true)
			if legacy {
				if count != 0 || releaseErr == nil {
					t.Fatalf("ambiguous legacy release = %d, %v; want refusal", count, releaseErr)
				}
			} else if releaseErr != nil || count != 1 {
				t.Fatalf("release = %d, %v", count, releaseErr)
			}
			after, err := os.ReadFile(pending[0].Path)
			if err != nil || !bytes.Equal(before, after) {
				t.Errorf("release rewrote an already submitted event: %v", err)
			}
			if legacy {
				// Reconcile the ready original through the normal uploader before
				// retrying release. The unrelated open prompt remains turn-gated.
				_ = flush()
				if count, err := transcriptcapture.ReleaseResidue(transcriptcapture.RuntimeClaudeCode, "submitted", 0, true); err != nil || count != 1 {
					t.Fatalf("release after legacy reconciliation = %d, %v", count, err)
				}
			}
			for range 2 {
				if code := flush(); code != 0 {
					t.Errorf("retry flush exit = %d, want successful immutable retry", code)
				}
			}
			pending, err = transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
			if err != nil || len(pending) != 0 {
				t.Errorf("retry left pending events: %d, %v", len(pending), err)
			}
			records, err := filepath.Glob(filepath.Join(os.Getenv("WITSELF_HOME"), "capture", "outbox", transcriptcapture.RuntimeClaudeCode, ".submissions", "*.json"))
			if err != nil || len(records) != 0 {
				t.Errorf("acknowledged events retained submission records: %d, %v", len(records), err)
			}
			mu.Lock()
			defer mu.Unlock()
			if conflicts != 0 || len(accepted) != 3 || attempts[start.Entries()[0].ExternalID] != 2 {
				t.Errorf("conflicts=%d accepted=%d start attempts=%d, want 0/3/2", conflicts, len(accepted), attempts[start.Entries()[0].ExternalID])
			}
		})
	}
}
