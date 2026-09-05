package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestTranscriptReleaseSubsequentFlushUploadsExactlyRedactedEvents(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	const canary = "RELEASE_TOOL_RESULT_CANARY"
	const prompt = "Keep this captured prompt intact."
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
			t.Error("tool result canary reached the HTTP server")
		}
		mu.Lock()
		defer mu.Unlock()
		requests++
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/self/activity":
			_, _ = w.Write([]byte(`{"activity":{"last_activity_at":"2026-09-05T12:00:00Z"}}`))
		case "POST /v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_release","metadata":{}}}`))
		case "POST /v1/transcripts/trn_release/entries:batch":
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
	enqueue := func(input map[string]any) transcriptcapture.Event {
		t.Helper()
		input["session_id"] = "release-flush"
		input["transcript_path"] = "/tmp/codex-release-flush.jsonl"
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		event, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, raw)
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	enqueue(map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": prompt})
	// Exercise small structured results and results beyond both structured-data
	// and raw-envelope budgets: every persisted copy must be suppressed.
	for i, output := range []string{canary, strings.Repeat(canary+"\n", 800)} {
		useID := fmt.Sprintf("call-%d", i)
		enqueue(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "ordinary_tool", "tool_use_id": useID, "tool_input": map[string]any{"query": "public"}})
		enqueue(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "ordinary_tool", "tool_use_id": useID, "tool_response": map[string]any{"output": output}})
	}
	if _, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptCmd([]string{"release", "--runtime", "codex", "--session", "release-flush", "--yes", "--force"})
	}); code != 0 {
		t.Fatalf("release exit %d: %s", code, stderr)
	}
	mu.Lock()
	requestCount := requests
	mu.Unlock()
	if requestCount != 0 {
		t.Fatal("release performed network requests before the explicit flush")
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || len(pending) != 6 {
		t.Fatalf("release must retain all five events and add one fence: %d, %v", len(pending), err)
	}
	var expected []client.AppendTranscriptEntryInput
	index := transcriptcapture.NewReadinessIndex(pending)
	markers, results, prompts := 0, 0, 0
	for _, item := range pending {
		raw, err := os.ReadFile(item.Path)
		if err != nil || bytes.Contains(raw, []byte(canary)) {
			t.Fatalf("released event retained canary or was unreadable: %v", err)
		}
		if !index.UploadReady(item) {
			t.Fatalf("released event %s is still gated", item.Event.Kind)
		}
		switch item.Event.Kind {
		case "message.user":
			prompts++
			if item.Event.Body != prompt {
				t.Fatal("release changed the prompt")
			}
		case "tool.call":
			if !strings.Contains(item.Event.Body, "ordinary_tool") {
				t.Fatal("release lost the tool call name")
			}
		case "tool.result":
			results++
			if !bytes.Contains(raw, []byte("released_without_fence")) {
				t.Fatal("released result is missing its placeholder")
			}
		}
		if bytes.Contains(item.Event.Data, []byte("operator_release")) {
			markers++
		}
		batches, err := captureAppendBatches(item.Event.Entries())
		if err != nil {
			t.Fatal(err)
		}
		for _, batch := range batches {
			expected = append(expected, batch...)
		}
	}
	if prompts != 1 || results != 2 || markers == 0 {
		t.Fatalf("missing prompt/results/operator marker: %d/%d/%d", prompts, results, markers)
	}
	for range 2 {
		if _, stderr, code := captureFactDeleteCLI(t, func() int {
			return transcriptFlush([]string{"--runtime", "codex"})
		}); code != 0 {
			t.Fatalf("flush exit %d: %s", code, stderr)
		}
	}
	mu.Lock()
	actual := append([]client.AppendTranscriptEntryInput(nil), received...)
	mu.Unlock()
	// Compare the complete uploaded envelope, including payload and reply IDs,
	// independent of JSON whitespace introduced by HTTP encoding.
	canonical := func(value any) any {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	if !reflect.DeepEqual(canonical(actual), canonical(expected)) {
		t.Fatalf("uploaded entries differ from the redacted outbox: got %d, want %d", len(actual), len(expected))
	}
	pending, err = transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || len(pending) != 0 {
		t.Fatalf("flush left pending events: %d, %v", len(pending), err)
	}
}

func TestTranscriptReleaseGrokLateStopFlushesAfterReleaseUpload(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	const canary = "RELEASE_GROK_LATE_RESULT_CANARY"
	var mu sync.Mutex
	var received []client.AppendTranscriptEntryInput
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		if bytes.Contains(raw, []byte(canary)) {
			t.Error("late released result reached the HTTP server")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/self/activity":
			_, _ = w.Write([]byte(`{"activity":{"last_activity_at":"2026-09-05T12:00:00Z"}}`))
		case "POST /v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_grok_release","metadata":{}}}`))
		case "POST /v1/transcripts/trn_grok_release/entries:batch":
			var body struct {
				Entries []client.AppendTranscriptEntryInput `json:"entries"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode append: %v", err)
				http.Error(w, "invalid append", http.StatusBadRequest)
				return
			}
			mu.Lock()
			received = append(received, body.Entries...)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configureCaptureFlushTest(t, transcriptcapture.RuntimeGrokBuild, server.URL)
	enqueue := func(input map[string]any) transcriptcapture.Event {
		t.Helper()
		input["sessionId"] = "grok-release-flush"
		input["promptId"] = "prompt-1"
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		event, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeGrokBuild, raw)
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	flush := func() {
		t.Helper()
		if _, stderr, code := captureFactDeleteCLI(t, func() int {
			return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeGrokBuild})
		}); code != 0 {
			t.Fatalf("flush exit %d: %s", code, stderr)
		}
		if pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeGrokBuild); err != nil || len(pending) != 0 {
			t.Fatalf("flush left pending events: %d, %v", len(pending), err)
		}
	}
	prompt := enqueue(map[string]any{"hookEventName": "user_prompt_submit", "prompt": "preserve the Grok prompt"})
	if count, err := transcriptcapture.ReleaseResidue(transcriptcapture.RuntimeGrokBuild, prompt.SessionID, 0, true); err != nil || count != 1 {
		t.Fatalf("release = %d, %v", count, err)
	}
	// Acknowledgement removes the operator event; its durable policy must
	// still finalize a native Stop arriving later, without native rehydration.
	flush()
	stop := enqueue(map[string]any{
		"hookEventName": "stop", "transcriptPath": filepath.Join(t.TempDir(), "missing-updates.jsonl"),
	})
	result := enqueue(map[string]any{"hookEventName": "post_tool_use", "toolName": "ordinary_tool", "toolOutput": canary})
	if stop.TurnID != prompt.TurnID || result.TurnID != prompt.TurnID || result.Kind != "tool.result" {
		t.Fatal("late native hooks lost their released turn identity or result kind")
	}
	flush()
	flush()
	mu.Lock()
	actual := append([]client.AppendTranscriptEntryInput(nil), received...)
	mu.Unlock()
	if len(actual) != 4 {
		t.Fatalf("uploaded %d entries, want prompt, operator release, late Stop, and late result", len(actual))
	}
	for i, want := range []string{"message.user", transcriptcapture.OperatorReleaseKind, "turn.completed", "tool.result"} {
		var payload struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(actual[i].Payload, &payload); err != nil || payload.Kind != want {
			t.Fatalf("uploaded entry %d kind = %q, %v; want %q", i, payload.Kind, err, want)
		}
	}
	if actual[0].Body != prompt.Body || actual[2].Body != stop.Body ||
		!strings.Contains(actual[3].Body, "released_without_fence") {
		t.Fatal("flush changed the captured prompt/Stop or lost the redacted late result")
	}
}
