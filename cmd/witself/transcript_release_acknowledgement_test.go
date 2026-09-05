package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestTranscriptReleaseFlushAfterInterruptedSubmittedAcknowledgement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory unlink permissions require Unix modes")
	}
	const canary = "RELEASE_SUBMITTED_ACK_CANARY"
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, "http://release-acknowledgement.invalid")
	start, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(`{"session_id":"submitted-ack","hook_event_name":"SessionStart","reason":"`+canary+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil || len(pending) != 1 {
		t.Fatalf("initial pending = %d, %v", len(pending), err)
	}
	item := pending[0]
	if err := transcriptcapture.MarkPendingSubmitted(item); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(item.Path)
	if err != nil || !bytes.Contains(before, []byte(canary)) {
		t.Fatalf("initial SessionStart lacks canary: %v", err)
	}
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(`{"session_id":"submitted-ack","hook_event_name":"UserPromptSubmit","prompt":"keep this prompt"}`)); err != nil {
		t.Fatal(err)
	}
	if count, err := transcriptcapture.ReleaseResidue(transcriptcapture.RuntimeClaudeCode, "submitted-ack", 0, true); err != nil || count != 1 {
		t.Fatalf("release = %d, %v", count, err)
	}
	if after, err := os.ReadFile(item.Path); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("release changed the attempted SessionStart: %v", err)
	}
	outbox := filepath.Dir(item.Path)
	snapshot := filepath.Join(outbox, ".submissions", filepath.Base(item.Path))
	t.Cleanup(func() { _ = os.Chmod(outbox, 0o700) })
	if err := os.Chmod(outbox, 0o500); err != nil {
		t.Fatal(err)
	}
	// An acknowledged SessionStart must retain value-free provenance when only
	// its event unlink fails, even though release kept its original bytes.
	if err := transcriptcapture.RemovePending(item.Path); err == nil {
		t.Skip("environment does not enforce directory unlink permissions")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("interrupted acknowledgement = %v", err)
	}
	if _, err := os.Stat(snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plaintext snapshot survived event unlink failure: %v", err)
	}
	if after, err := os.ReadFile(item.Path); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("interrupted acknowledgement lost the discoverable event: %v", err)
	}
	if err := os.Chmod(outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	appended := 0
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	http.DefaultTransport = releaseUploaderRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		response := `{"entries":[]}`
		switch r.URL.Path {
		case "/v1/transcripts":
			response = `{"transcript":{"id":"trn_acknowledgement","metadata":{}}}`
		case "/v1/self/activity":
			response = `{"activity":{"last_activity_at":"2026-09-05T12:00:00Z"}}`
		case "/v1/transcripts/trn_acknowledgement/entries:batch":
			var body struct {
				Entries []client.AppendTranscriptEntryInput `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return nil, err
			}
			for _, entry := range body.Entries {
				if entry.ExternalID == start.Entries()[0].ExternalID {
					t.Error("flush retried an already acknowledged SessionStart")
				}
				encoded, _ := json.Marshal(entry)
				if bytes.Contains(encoded, []byte(canary)) {
					t.Error("flush uploaded an acknowledged plaintext canary")
				}
				appended++
			}
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
	})
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode})
	})
	if code != 0 || strings.Contains(stdout+stderr, canary) {
		t.Fatalf("next actual flush failed or leaked canary: exit %d, %q, %q", code, stdout, stderr)
	}
	pending, err = transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil || len(pending) != 0 || appended != 2 {
		t.Fatalf("next actual flush stranded release residue: pending=%d appended=%d err=%v", len(pending), appended, err)
	}
	records, err := os.ReadDir(filepath.Dir(snapshot))
	if err != nil || len(records) != 0 {
		t.Fatalf("successful flush retained acknowledgement records: %d, %v", len(records), err)
	}
}
