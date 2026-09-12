package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const statusTestSession = "headless-cursor-session"

func enqueueHeadlessCursorRun(t *testing.T) {
	t.Helper()
	for _, raw := range []string{
		`{"conversation_id":"headless-cursor-session","hook_event_name":"sessionStart","cursor_version":"3.15.6"}`,
		`{"conversation_id":"headless-cursor-session","generation_id":"generation-thought",` +
			`"hook_event_name":"afterAgentThought","text":"planning","transcript_path":"/tmp/cursor-headless.jsonl"}`,
		`{"conversation_id":"headless-cursor-session","generation_id":"generation-tool",` +
			`"hook_event_name":"preToolUse","tool_name":"Shell","tool_use_id":"tool-1",` +
			`"transcript_path":"/tmp/cursor-headless.jsonl"}`,
	} {
		if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCursor, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
}

// startCaptureLedgerServer records the visible body of every appended entry in
// arrival order, which is the delivery order a resumed session must preserve.
func startCaptureLedgerServer(t *testing.T, appended *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/self/activity":
			_, _ = w.Write([]byte(`{"activity":{"last_activity_at":"2026-09-11T12:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			var body struct {
				Entries []struct {
					Body string `json:"body"`
				} `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode append: %v", err)
				http.Error(w, "bad append", http.StatusBadRequest)
				return
			}
			for _, entry := range body.Entries {
				*appended = append(*appended, entry.Body)
			}
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A headless lane that never emits a terminal hook must show its backlog shape
// on the console, and the launcher's completion fence must clear it.
func TestTranscriptFlushAndStatusReportDeferredBuckets(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	var appended []string
	srv := startCaptureLedgerServer(t, &appended)
	configureCaptureFlushTest(t, transcriptcapture.RuntimeCursor, srv.URL)
	enqueueHeadlessCursorRun(t)

	_, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeCursor})
	})
	if code != 1 || stderr != "flushed 1 cursor transcript event(s); deferred 2 incomplete or mismatched event(s) (no-fence 2, run-mismatch 0, session-unbound 0)\n" {
		t.Fatalf("unfenced flush = code %d stderr %q", code, stderr)
	}
	stdout, _, code := captureFactDeleteCLI(t, func() int {
		return transcriptStatus([]string{"--runtime", transcriptcapture.RuntimeCursor})
	})
	if code != 0 || stdout != "cursor capture: 2 queued event(s); deferred 2 (no-fence 2, run-mismatch 0, session-unbound 0)\n" {
		t.Fatalf("unfenced status = code %d stdout %q", code, stdout)
	}

	_, stderr, code = captureFactDeleteCLI(t, func() int {
		return transcriptCmd([]string{
			"fence", "--runtime", "cursor", "--session", statusTestSession,
			"--latest", "--reason", "job-completed",
		})
	})
	if code != 0 || stderr != "fenced 2 held cursor turn(s)\n" {
		t.Fatalf("launcher fence = code %d stderr %q", code, stderr)
	}
	_, stderr, code = captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeCursor})
	})
	if code != 0 || !strings.Contains(stderr, "flushed 4 cursor transcript event(s)") {
		t.Fatalf("fenced flush = code %d stderr %q", code, stderr)
	}
	want := []string{
		"session started", "planning",
		`{"tool_name":"Shell","tool_use_id":"tool-1"}`,
		"delegation job completed", "delegation job completed",
	}
	if !slices.Equal(appended, want) {
		t.Fatalf("uploaded bodies = %#v, want %#v in capture order", appended, want)
	}
	stdout, _, code = captureFactDeleteCLI(t, func() int {
		return transcriptStatus([]string{"--runtime", transcriptcapture.RuntimeCursor})
	})
	if code != 0 || stdout != "cursor capture: 0 queued event(s); deferred 0 (no-fence 0, run-mismatch 0, session-unbound 0)\n" {
		t.Fatalf("fenced status = code %d stdout %q", code, stdout)
	}
}

// A resumed fix round reuses the session id under a new run. Both runs must
// reach the ledger, in capture order, with the prior run closed in between.
func TestResumedCursorSessionFlushesBothRunsInOrder(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	var appended []string
	srv := startCaptureLedgerServer(t, &appended)
	configureCaptureFlushTest(t, transcriptcapture.RuntimeCursor, srv.URL)
	enqueueHeadlessCursorRun(t)
	enqueueHeadlessCursorRun(t)
	if code := transcriptCmd([]string{
		"fence", "--runtime", "cursor", "--session", statusTestSession, "--latest",
	}); code != 0 {
		t.Fatalf("launcher fence exit = %d", code)
	}
	_, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeCursor})
	})
	if code != 0 || !strings.Contains(stderr, "flushed 10 cursor transcript event(s)") {
		t.Fatalf("resumed flush = code %d stderr %q", code, stderr)
	}
	want := []string{
		"session started", "planning", `{"tool_name":"Shell","tool_use_id":"tool-1"}`,
		"run completed when the session restarted",
		"run completed when the session restarted",
		"session started", "planning", `{"tool_name":"Shell","tool_use_id":"tool-1"}`,
		"delegation job completed", "delegation job completed",
	}
	if !slices.Equal(appended, want) {
		t.Fatalf("uploaded bodies = %#v, want %#v", appended, want)
	}
}

func TestTranscriptStatusReportsRunMismatchAndRefusesUnknownRuntime(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	configureCaptureFlushTest(t, transcriptcapture.RuntimeCursor, "http://127.0.0.1:1")
	enqueueHeadlessCursorRun(t)
	// Rebinding the run without a terminal event is what an older build did on
	// resume, and it is exactly the orphaned-run bucket an operator must see.
	states, err := filepath.Glob(filepath.Join(home, "capture", "state", "cursor", "*.json"))
	if err != nil || len(states) != 1 {
		t.Fatalf("session state files = %v, %v", states, err)
	}
	raw, err := os.ReadFile(states[0])
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	state["run_id"] = "run_orphaned_canary"
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(states[0], raw, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, code := captureFactDeleteCLI(t, func() int {
		return transcriptStatus([]string{"--runtime", transcriptcapture.RuntimeCursor})
	})
	if code != 0 || stdout != "cursor capture: 3 queued event(s); deferred 2 (no-fence 0, run-mismatch 2, session-unbound 0)\n" {
		t.Fatalf("orphaned-run status = code %d stdout %q", code, stdout)
	}
	if code := transcriptCmd([]string{
		"fence", "--runtime", "cursor", "--session", statusTestSession, "--latest",
	}); code != 0 {
		t.Fatalf("recovery fence exit = %d", code)
	}
	stdout, _, code = captureFactDeleteCLI(t, func() int {
		return transcriptStatus([]string{"--runtime", transcriptcapture.RuntimeCursor})
	})
	if code != 0 || stdout != "cursor capture: 5 queued event(s); deferred 0 (no-fence 0, run-mismatch 0, session-unbound 0)\n" {
		t.Fatalf("recovered status = code %d stdout %q", code, stdout)
	}
	for _, args := range [][]string{
		{"--runtime", "not-a-runtime"},
		{"--runtime", "cursor", "unexpected"},
		{},
	} {
		if code := transcriptStatus(args); code != 2 {
			t.Fatalf("status %q exit = %d, want 2", args, code)
		}
	}
}
