package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type transcriptReleaseSessionFixture struct {
	t       *testing.T
	session string
	mu      sync.Mutex
	uploads [][]byte
	entries []client.AppendTranscriptEntryInput
	events  []transcriptcapture.Event
}

func newTranscriptReleaseSessionFixture(t *testing.T) *transcriptReleaseSessionFixture {
	t.Helper()
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	f := &transcriptReleaseSessionFixture{t: t, session: "release-session"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upload: %v", err)
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.uploads = append(f.uploads, raw)
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/self/activity":
			_, _ = w.Write([]byte(`{"activity":{"last_activity_at":"2026-09-05T12:00:00Z"}}`))
		case "POST /v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_session_release","metadata":{}}}`))
		case "POST /v1/transcripts/trn_session_release/entries:batch":
			var body struct {
				Entries []client.AppendTranscriptEntryInput `json:"entries"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode append: %v", err)
				http.Error(w, "invalid append", http.StatusBadRequest)
				return
			}
			f.entries = append(f.entries, body.Entries...)
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, server.URL)
	installRuntimeReleaseUploaderTestHooks(t, transcriptcapture.RuntimeClaudeCode, compatibleReleaseUploaderTestExecutable(t))
	return f
}

func (f *transcriptReleaseSessionFixture) enqueue(input map[string]any) transcriptcapture.Event {
	f.t.Helper()
	input["session_id"] = f.session
	raw, err := json.Marshal(input)
	if err != nil {
		f.t.Fatal(err)
	}
	event, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, raw)
	if err != nil {
		f.t.Fatal(err)
	}
	f.events = append(f.events, event)
	return event
}

func (f *transcriptReleaseSessionFixture) release() (string, int) {
	f.t.Helper()
	_, stderr, code := captureFactDeleteCLI(f.t, func() int {
		return transcriptCmd([]string{"release", "--runtime", transcriptcapture.RuntimeClaudeCode, "--session", f.session, "--yes", "--force"})
	})
	if code == 0 {
		pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
		if err != nil {
			f.t.Fatal(err)
		}
		// A release assigns replacement upload IDs to queued events while
		// preserving local event identity. Assert every captured event uploads
		// using its persisted replacement projection after a successful release.
		byID := make(map[string]transcriptcapture.Event, len(pending))
		for _, item := range pending {
			byID[item.Event.ID] = item.Event
		}
		for i, event := range f.events {
			if released, ok := byID[event.ID]; ok {
				f.events[i] = released
			}
		}
	}
	return stderr, code
}

func (f *transcriptReleaseSessionFixture) flush() {
	f.t.Helper()
	if _, stderr, code := captureFactDeleteCLI(f.t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode})
	}); code != 0 {
		f.t.Fatalf("flush exit = %d: %s", code, stderr)
	}
	if pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode); err != nil || len(pending) != 0 {
		f.t.Fatalf("flush left pending events = %d, %v", len(pending), err)
	}
}

func (f *transcriptReleaseSessionFixture) assertUploads(prompt string, forbidden ...string) []byte {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) == 0 {
		f.t.Fatal("no transcript entries reached the HTTP server")
	}
	for i, raw := range f.uploads {
		for _, canary := range forbidden {
			if bytes.Contains(raw, []byte(canary)) {
				f.t.Errorf("HTTP upload %d leaked %s", i, canary)
			}
		}
	}
	raw, err := json.Marshal(f.entries)
	if err != nil {
		f.t.Fatal(err)
	}
	uploadedIDs := make(map[string]bool, len(f.entries))
	promptPreserved := false
	for _, entry := range f.entries {
		uploadedIDs[entry.ExternalID] = true
		promptPreserved = promptPreserved || (entry.Role == "user" && entry.Body == prompt)
	}
	if !promptPreserved {
		f.t.Fatal("release lost the captured prompt")
	}
	for _, event := range f.events {
		for _, entry := range event.Entries() {
			if !uploadedIDs[entry.ExternalID] {
				f.t.Errorf("captured %s event did not reach the HTTP server", event.HookEvent)
			}
		}
	}
	return raw
}

func TestTranscriptReleaseSessionPermissionRequestFlush(t *testing.T) {
	f := newTranscriptReleaseSessionFixture(t)
	const prompt = "Preserve the permission release prompt."
	f.enqueue(map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": prompt})
	f.enqueue(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_response": "earlier result"})
	if stderr, code := f.release(); code != 0 {
		t.Fatalf("release exit = %d: %s", code, stderr)
	}
	f.enqueue(map[string]any{
		"hook_event_name": "PermissionRequest", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "printf RELEASE_CANARY"},
	})
	f.flush()
	raw := f.assertUploads(prompt, "RELEASE_CANARY")
	if !bytes.Contains(raw, []byte("operator_release")) || !bytes.Contains(raw, []byte("released_without_fence")) {
		t.Fatal("released upload lost its operator marker or payload placeholder")
	}
}

func TestTranscriptReleaseSessionInterruptedStopLateResultFlush(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("queued-file permission validation requires Unix")
	}
	f := newTranscriptReleaseSessionFixture(t)
	const prompt = "Preserve the interrupted session release prompt."
	f.enqueue(map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": prompt})
	result := f.enqueue(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_response": "earlier result"})
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	var resultPath string
	for _, item := range pending {
		if item.Event.ID == result.ID {
			resultPath = item.Path
		}
	}
	if resultPath == "" {
		t.Fatal("original result is missing from the outbox")
	}
	if err := os.Chmod(resultPath, 0o666); err != nil {
		t.Fatal(err)
	}
	stderr, code := f.release()
	if err := os.Chmod(resultPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if code != 1 || !strings.Contains(stderr, "not a trusted regular file") {
		t.Fatalf("interrupted release exit = %d: %s", code, stderr)
	}
	f.enqueue(map[string]any{"hook_event_name": "Stop", "last_assistant_message": "Preserve this captured assistant response."})
	f.enqueue(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_response": "LATE_CANARY"})
	if stderr, code := f.release(); code != 0 {
		t.Fatalf("release retry exit = %d: %s", code, stderr)
	}
	f.flush()
	raw := f.assertUploads(prompt, "LATE_CANARY")
	if !bytes.Contains(raw, []byte("Preserve this captured assistant response.")) {
		t.Fatal("release changed the captured assistant response")
	}
}

func TestTranscriptReleaseSessionNewFencedTurnFlushesNormally(t *testing.T) {
	f := newTranscriptReleaseSessionFixture(t)
	f.enqueue(map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "The old released prompt."})
	if stderr, code := f.release(); code != 0 {
		t.Fatalf("release exit = %d: %s", code, stderr)
	}
	f.flush()
	const prompt = "Preserve the genuinely new fenced prompt."
	f.enqueue(map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": prompt})
	f.enqueue(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "printf new-fenced-tool-input"}})
	f.enqueue(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_response": "new-fenced-tool-result"})
	f.enqueue(map[string]any{"hook_event_name": "Stop", "last_assistant_message": "The new turn has its genuine fence."})
	// A turn-less hook after the new turn's genuine fence is ordinary capture.
	f.enqueue(map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": map[string]any{"command": "printf after-new-fence-input"}})
	f.flush()
	raw := f.assertUploads(prompt)
	for _, want := range []string{"new-fenced-tool-input", "new-fenced-tool-result", "after-new-fence-input"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("new fenced turn was over-suppressed: missing %q", want)
		}
	}
}
