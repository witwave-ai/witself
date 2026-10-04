package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

// C4 pins the existing cross-identity readiness behavior before changing the
// error classification. The terminal still participates in the whole snapshot.
func TestTranscriptIdentityReadinessUnchanged(t *testing.T) {
	for _, terminal := range []bool{true, false} {
		name := "foreign-stop"
		if !terminal {
			name = "prompt-alone"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("WITSELF_HOME", home)
			t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
			var appended []string
			srv := startCaptureLedgerServer(t, &appended)
			configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, srv.URL)
			cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeClaudeCode)
			if err != nil {
				t.Fatal(err)
			}
			prompt, err := transcriptcapture.EnqueueHook(cfg.Runtime, []byte(`{"session_id":"identity-readiness-session","hook_event_name":"UserPromptSubmit","prompt":"matching readiness prompt"}`))
			if err != nil {
				t.Fatal(err)
			}
			if terminal {
				foreign := cfg
				foreign.Agent, foreign.AgentID, foreign.AgentName = "cc-accept-1", "agent_foreign", "cc-accept-1"
				if err := transcriptcapture.SaveConfig(foreign); err != nil {
					t.Fatal(err)
				}
				stop, err := transcriptcapture.EnqueueHook(cfg.Runtime, []byte(`{"session_id":"identity-readiness-session","hook_event_name":"Stop","last_assistant_message":"foreign-identity canary"}`))
				if err != nil {
					t.Fatal(err)
				}
				if prompt.SessionID != stop.SessionID || prompt.TurnID == "" || prompt.TurnID != stop.TurnID || prompt.TranscriptExternalID() != stop.TranscriptExternalID() || !stop.OccurredAt.After(prompt.OccurredAt) {
					t.Fatal("prompt and later Stop must share session, turn and transcript")
				}
				if err := transcriptcapture.SaveConfig(cfg); err != nil {
					t.Fatal(err)
				}
			}
			// Stable basenames make the before/after outbox evidence comparable.
			pending, err := transcriptcapture.Pending(cfg.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			original := make(map[string][]byte)
			for _, item := range pending {
				basename := "01-prompt.json"
				if item.Event.HookEvent == "Stop" {
					basename = "02-stop.json"
				}
				raw, err := os.ReadFile(item.Path)
				if err != nil {
					t.Fatal(err)
				}
				original[basename] = raw
				if err := os.Rename(item.Path, filepath.Join(filepath.Dir(item.Path), basename)); err != nil {
					t.Fatal(err)
				}
			}
			stdout, stderr, code := captureFactDeleteCLI(t, func() int {
				return transcriptFlush([]string{"--runtime", cfg.Runtime})
			})
			wantCode := 1
			wantStderr := "flushed 0 claude-code transcript event(s); deferred 1 incomplete or mismatched event(s) (no-fence 1, run-mismatch 0, session-unbound 0, identity-mismatch 0)\n"
			wantBodies := []string(nil)
			wantBasenames := []string{"01-prompt.json"}
			if terminal {
				wantCode = 0
				wantStderr = "flushed 1 claude-code transcript event(s); deferred 1 incomplete or mismatched event(s) (no-fence 0, run-mismatch 0, session-unbound 0, identity-mismatch 1)\n"
				wantBodies = []string{"matching readiness prompt"}
				wantBasenames = []string{"02-stop.json"}
			}
			remaining, err := transcriptcapture.Pending(cfg.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			var basenames []string
			for _, item := range remaining {
				basename := filepath.Base(item.Path)
				basenames = append(basenames, basename)
				raw, err := os.ReadFile(item.Path)
				if err != nil || !slices.Equal(raw, original[basename]) {
					t.Fatal("held outbox file changed")
				}
			}
			t.Logf("C4 code=%d stdout=%q stderr=%q uploaded=%q remaining=%q", code, stdout, stderr, appended, basenames)
			if !slices.Equal(appended, wantBodies) {
				t.Fatalf("uploaded bodies = %q, want %q", appended, wantBodies)
			}
			if code != wantCode || stdout != "" || stderr != wantStderr {
				t.Fatalf("readiness flush = code %d stdout %q stderr %q", code, stdout, stderr)
			}
			if !slices.Equal(basenames, wantBasenames) {
				t.Fatalf("remaining basenames = %q, want %q", basenames, wantBasenames)
			}
		})
	}
}

func TestPrepareTranscriptFlushRuntimeMismatchStillFails(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	cfg := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeClaudeCode, Account: "default", Realm: "default",
		Agent: "scott", AgentID: "agent_1", AgentName: "scott",
		Location: transcriptcapture.Location{ID: "loc_1", Name: "home"},
	}
	event := transcriptcapture.Event{
		Runtime: transcriptcapture.RuntimeCursor, Account: cfg.Account, Realm: cfg.Realm,
		Agent: cfg.Agent, AgentID: cfg.AgentID, AgentName: cfg.AgentName, Location: cfg.Location,
		SessionID: "runtime-mismatch-session", HookEvent: "SessionStart",
	}
	pending := []transcriptcapture.PendingEvent{{Path: "/unused/runtime.json", Event: event}}
	held, blocked := map[string]error{}, map[string]error{}
	ready, err := prepareTranscriptFlushEvents(pending, cfg, blocked, held)
	const want = "queued transcript identity does not match the installed runtime binding"
	if err == nil || err.Error() != want || errors.Is(err, transcriptcapture.ErrIdentityMismatch) {
		t.Fatalf("runtime mismatch error = %v, want fresh binding error", err)
	}
	if len(ready) != 0 || len(blocked) != 0 || len(held) != 1 || held[pending[0].Path] != err {
		t.Fatal("runtime mismatch must stay held and select no upload")
	}
	if countHeldForBindingCaptureEvents(pending, held) != 0 || countBlockedCaptureEvents(pending, blocked, held) != 1 {
		t.Fatal("runtime mismatch was exempted from flush failure")
	}
}
