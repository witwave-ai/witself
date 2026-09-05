package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestTranscriptReleaseOldFlusherQueuedLostAckUsesFreshExternalIDs(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, "http://release-lost-ack.invalid")
	start, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(`{"session_id":"lost-ack","hook_event_name":"SessionStart","reason":"OLD_FLUSHER_LOST_ACK_CANARY"}`))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %d, %v", len(pending), err)
	}
	path := pending[0].Path
	sidecar := filepath.Join(filepath.Dir(path), ".submissions", filepath.Base(path))
	queued, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	originalID := start.Entries()[0].ExternalID
	accepted := map[string][]byte{}
	attempts := map[string]int{}
	conflicts, appends := 0, 0
	firstAppend := true
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	http.DefaultTransport = releaseUploaderRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, response := http.StatusOK, `{"entries":[]}`
		switch r.URL.Path {
		case "/v1/transcripts":
			response = `{"transcript":{"id":"trn_lostack","metadata":{}}}`
		case "/v1/self/activity":
			response = `{"activity":{"last_activity_at":"2026-09-05T12:00:00Z"}}`
		case "/v1/transcripts/trn_lostack/entries:batch":
			appends++
			var body struct {
				Entries []client.AppendTranscriptEntryInput `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return nil, err
			}
			for _, entry := range body.Entries {
				raw, _ := json.Marshal(entry)
				attempts[entry.ExternalID]++
				if previous, exists := accepted[entry.ExternalID]; exists && !bytes.Equal(previous, raw) {
					conflicts++
					status, response = http.StatusConflict, `{"error":"transcript entry already exists"}`
					break
				}
				if !firstAppend && bytes.Contains(raw, []byte("CANARY")) {
					t.Error("release uploaded plaintext canary")
				}
				if entry.ReplyToExternalID != "" && accepted[entry.ReplyToExternalID] == nil {
					t.Error("replacement reply target missing")
				}
				accepted[entry.ExternalID] = raw
			}
			if firstAppend {
				firstAppend = false
				// Simulate the pre-upgrade flusher: it accepted the new-format event,
				// ignored queued provenance, and never delivered an acknowledgement.
				if !bytes.Contains(accepted[originalID], []byte("OLD_FLUSHER_LOST_ACK_CANARY")) {
					t.Error("accepted original lacks canary")
				}
				var barrier *transcriptcapture.ReleaseUpgradeBarrierError
				if n, err := transcriptcapture.ReleaseResidue(transcriptcapture.RuntimeClaudeCode, "lost-ack", 0, true); n != 0 || !errors.As(err, &barrier) {
					t.Errorf("in-flight release: %d, %v", n, err)
				}
				status, response = http.StatusInternalServerError, `{"error":"acknowledgement lost"}`
			}
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
	})
	flush := func() int {
		_, _, code := captureFactDeleteCLI(t, func() int { return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode}) })
		return code
	}
	if code := flush(); code != 1 {
		t.Fatalf("lost ack exit = %d", code)
	}
	if err := os.WriteFile(sidecar, queued, 0600); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"session_id":"lost-ack","hook_event_name":"UserPromptSubmit","prompt":"visible prompt"}`,
		`{"session_id":"lost-ack","hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":"QUEUED_RESULT_CANARY"}`,
	} {
		if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := transcriptcapture.ReleaseResidue(transcriptcapture.RuntimeClaudeCode, "lost-ack", 0, true); err != nil || n != 1 {
		t.Fatalf("release: %d, %v", n, err)
	}
	pending, err = transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pending {
		if item.Path != path {
			continue
		}
		raw, err := os.ReadFile(sidecar)
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			ReleaseID  string   `json:"release_id"`
			Superseded []string `json:"superseded_external_ids"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		if record.ReleaseID == "" || len(record.Superseded) != 1 || record.Superseded[0] != originalID {
			t.Error("queued rewrite did not supersede original external ID")
		}
		if item.Entries()[0].ExternalID == originalID {
			t.Error("queued rewrite reuses accepted original external ID")
		}
	}
	// Idempotent release retry must preserve every replacement ID.
	before := transcriptReleaseFiles(t)
	if n, err := transcriptcapture.ReleaseResidue(transcriptcapture.RuntimeClaudeCode, "lost-ack", 0, true); err != nil || n != 0 {
		t.Fatalf("release retry: %d, %v", n, err)
	}
	for name, raw := range before {
		if after, err := os.ReadFile(name); err != nil || string(after) != raw {
			t.Errorf("retry changed %s: %v", filepath.Base(name), err)
		}
	}
	if code := flush(); code != 0 {
		t.Errorf("replacement flush exit = %d", code)
	}
	if code := flush(); code != 0 {
		t.Errorf("empty retry flush exit = %d", code)
	}
	if conflicts != 0 || attempts[originalID] != 1 || len(accepted) != 5 || appends != 2 {
		t.Errorf("conflicts=%d original attempts=%d accepted=%d appends=%d; want 0/1/5/2", conflicts, attempts[originalID], len(accepted), appends)
	}
	pending, err = transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil || len(pending) != 0 {
		t.Fatalf("unflushed events = %d, %v", len(pending), err)
	}
}
