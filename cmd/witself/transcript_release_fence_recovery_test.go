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

func TestTranscriptReleaseInterruptedRewriteCompanionFenceRetryFlush(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("queued-file permission validation requires Unix")
	}
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	const canary = "INTERRUPTED_RELEASE_RESULT_CANARY"
	var mu sync.Mutex
	var received []client.AppendTranscriptEntryInput
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		if bytes.Contains(raw, []byte(canary)) {
			t.Error("interrupted release tool result reached the HTTP server")
		}
		mu.Lock()
		defer mu.Unlock()
		requests++
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/self/activity":
			_, _ = w.Write([]byte(`{"activity":{"last_activity_at":"2026-09-05T12:00:00Z"}}`))
		case "POST /v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_recovery","metadata":{}}}`))
		case "POST /v1/transcripts/trn_recovery/entries:batch":
			var body struct {
				Entries []client.AppendTranscriptEntryInput `json:"entries"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode append: %v", err)
				http.Error(w, "invalid append", http.StatusBadRequest)
				return
			}
			received = append(received, body.Entries...)
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configureCaptureFlushTest(t, transcriptcapture.RuntimeCodex, server.URL)
	installReleaseUploaderTestHooks(t, compatibleReleaseUploaderTestExecutable(t))
	var prompt transcriptcapture.Event
	for _, input := range []map[string]any{
		{"hook_event_name": "UserPromptSubmit", "prompt": "Preserve the interrupted release prompt."},
		{"hook_event_name": "PostToolUse", "tool_name": "ordinary_tool", "tool_use_id": "call-1", "tool_response": canary},
	} {
		input["session_id"] = "release-recovery"
		input["transcript_path"] = "/tmp/codex-release-recovery.jsonl"
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		event, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, raw)
		if err != nil {
			t.Fatal(err)
		}
		if event.HookEvent == "UserPromptSubmit" {
			prompt = event
		}
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || len(pending) != 2 {
		t.Fatalf("initial pending events = %d, %v", len(pending), err)
	}
	var resultPath string
	for _, item := range pending {
		if item.Event.Kind == "tool.result" {
			resultPath = item.Path
		}
	}
	if err := os.Chmod(resultPath, 0o666); err != nil {
		t.Fatal(err)
	}
	releaseArgs := []string{"release", "--runtime", "codex", "--session", "release-recovery", "--yes", "--force"}
	if _, stderr, code := captureFactDeleteCLI(t, func() int { return transcriptCmd(releaseArgs) }); code != 1 || !strings.Contains(stderr, "not a trusted regular file") {
		t.Fatalf("interrupted release exit = %d: %s", code, stderr)
	}
	if err := os.Chmod(resultPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptCmd([]string{"fence", "--runtime", "codex", "--session", "release-recovery", "--run", prompt.RunID, "--turn", prompt.TurnID})
	}); code != 1 || !strings.Contains(stderr, "retry transcript release") {
		t.Fatalf("companion fence must defer to the interrupted release, exit = %d: %s", code, stderr)
	}
	if _, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", "codex"})
	}); code != 1 {
		t.Fatalf("unfinished release flush exit = %d: %s", code, stderr)
	}
	mu.Lock()
	requestCount := requests
	mu.Unlock()
	if requestCount != 0 {
		t.Fatal("unfinished release made HTTP requests")
	}
	if _, stderr, code := captureFactDeleteCLI(t, func() int { return transcriptCmd(releaseArgs) }); code != 0 {
		t.Fatalf("release retry exit = %d: %s", code, stderr)
	}
	pending, err = transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || len(pending) != 3 {
		t.Fatalf("recovered pending events = %d, %v", len(pending), err)
	}
	wantEntries := 0
	for _, item := range pending {
		wantEntries += len(item.Event.Entries())
	}
	for range 2 {
		if _, stderr, code := captureFactDeleteCLI(t, func() int {
			return transcriptFlush([]string{"--runtime", "codex"})
		}); code != 0 {
			t.Fatalf("recovered release flush exit = %d: %s", code, stderr)
		}
	}
	mu.Lock()
	actual := append([]client.AppendTranscriptEntryInput(nil), received...)
	mu.Unlock()
	if len(actual) != wantEntries {
		t.Fatalf("uploaded entries = %d, want exactly %d", len(actual), wantEntries)
	}
	raw, err := json.Marshal(actual)
	if err != nil || !bytes.Contains(raw, []byte(prompt.Body)) || !bytes.Contains(raw, []byte("released_without_fence")) || !bytes.Contains(raw, []byte("operator_release")) {
		t.Fatalf("recovered upload lost prompt, result suppression, or operator completion: %v", err)
	}
	pending, err = transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || len(pending) != 0 {
		t.Fatalf("recovered flush left pending events: %d, %v", len(pending), err)
	}
}
