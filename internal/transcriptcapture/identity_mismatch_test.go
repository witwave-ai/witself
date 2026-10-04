package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// L1 distinguishes identity holds from runtime mismatches with identical text.
func TestIdentityMismatchSentinel(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	saveTestCaptureConfig(t, RuntimeClaudeCode)
	cfg, err := LoadConfig(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	matching := Event{
		Runtime: cfg.Runtime, Account: cfg.Account, AccountID: cfg.AccountID,
		Realm: cfg.Realm, RealmID: cfg.RealmID, Agent: cfg.Agent,
		AgentID: cfg.AgentID, AgentName: cfg.AgentName, Location: cfg.Location,
	}
	if err := EventBindingError(matching, cfg); err != nil || HeldForBinding(RuntimeClaudeCode, matching, cfg) {
		t.Fatalf("matching event was held: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Event)
	}{
		{"account", func(event *Event) { event.Account = "foreign-account" }},
		{"realm", func(event *Event) { event.Realm = "foreign-realm" }},
		{"agent", func(event *Event) {
			event.Agent, event.AgentID, event.AgentName = "cc-accept-1", "agent_foreign", "cc-accept-1"
		}},
		{"location", func(event *Event) { event.Location.ID = "foreign-location" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := matching
			tc.change(&event)
			err := EventBindingError(event, cfg)
			if !errors.Is(err, ErrIdentityMismatch) || err.Error() != "queued transcript identity does not match the installed runtime binding" {
				t.Fatalf("identity mismatch error = %v; want sentinel and unchanged text", err)
			}
			if !HeldForBinding(" CLAUDE ", event, cfg) {
				t.Fatal("normalized runtime did not hold the mismatched identity")
			}
			if HeldForBinding(RuntimeCursor, event, cfg) || HeldForBinding("unknown-runtime", event, cfg) {
				t.Fatal("runtime filter held an event outside the requested runtime")
			}
		})
	}
	foreignRuntime := matching
	foreignRuntime.Runtime = RuntimeCursor
	err = EventBindingError(foreignRuntime, cfg)
	if err == nil || errors.Is(err, ErrIdentityMismatch) || err.Error() != "queued transcript identity does not match the installed runtime binding" {
		t.Fatalf("runtime mismatch error = %v; want non-sentinel and unchanged text", err)
	}
	if HeldForBinding(RuntimeClaudeCode, foreignRuntime, cfg) || HeldForBinding(RuntimeCursor, foreignRuntime, cfg) {
		t.Fatal("runtime mismatch was held for a binding")
	}
}

// L2 retains the measured base buckets without a config while classifying
// both ready and incomplete mismatched events before session-state reads.
func TestIdentityMismatchDeferredSummary(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	saveTestCaptureConfig(t, RuntimeClaudeCode)
	matching := enqueueTestHook(t, RuntimeClaudeCode,
		`{"session_id":"matching-open-session","hook_event_name":"UserPromptSubmit","prompt":"matching open prompt"}`)
	cfg, err := LoadConfig(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if err := EventBindingError(matching, cfg); err != nil {
		t.Fatalf("matching prompt binding: %v", err)
	}
	state, err := loadSessionState(RuntimeClaudeCode, matching.SessionID)
	if err != nil || state.RunID != matching.RunID {
		t.Fatalf("matching prompt run is not locally bound: %v", err)
	}
	foreignReady := matching
	foreignReady.ID = "foreign_ready"
	foreignReady.SessionID = "foreign-ready-session"
	foreignReady.TurnID = ""
	foreignReady.Agent, foreignReady.AgentID, foreignReady.AgentName = "cc-accept-1", "agent_foreign", "cc-accept-1"
	foreignReady.Body = "foreign-identity canary ready"
	writeIdentityMismatchFixture(t, RuntimeClaudeCode, foreignReady)
	foreignOpen := foreignReady
	foreignOpen.ID = "foreign_open"
	foreignOpen.SessionID = "foreign-open-session"
	foreignOpen.TurnID = "foreign-open-turn"
	foreignOpen.Body = "foreign-identity canary open"
	writeIdentityMismatchFixture(t, RuntimeClaudeCode, foreignOpen)
	otherRuntime := matching
	otherRuntime.ID = "other_runtime"
	otherRuntime.Runtime = RuntimeCursor
	otherRuntime.SessionID = "other-runtime-session"
	otherRuntime.TurnID = "other-runtime-turn"
	writeIdentityMismatchFixture(t, RuntimeClaudeCode, otherRuntime)
	pending, err := Pending(RuntimeClaudeCode)
	if err != nil || len(pending) != 4 {
		t.Fatalf("pending count = %d, want 4: %v", len(pending), err)
	}
	index := NewReadinessIndex(pending)
	for _, item := range pending {
		if got, want := index.UploadReady(item), item.Event.ID == foreignReady.ID; got != want {
			t.Fatalf("fixture upload readiness = %v, want %v", got, want)
		}
	}
	summary, err := SummarizeDeferred(RuntimeClaudeCode, pending, nil)
	if err != nil || summary.NoFence != 1 || summary.RunMismatch != 0 || summary.SessionUnbound != 1 || summary.IdentityMismatch != 0 || summary.Total() != 2 {
		t.Fatalf("legacy deferred summary = %#v, %v; want no-fence 1, run-mismatch 0, session-unbound 1, total 2", summary, err)
	}
	t.Logf("L2 nil-config summary: no-fence %d, run-mismatch %d, session-unbound %d, total %d", summary.NoFence, summary.RunMismatch, summary.SessionUnbound, summary.Total())
	// Classification must not read local state for a mismatched identity.
	foreignStatePath, err := sessionStatePath(RuntimeClaudeCode, foreignOpen.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreignStatePath, []byte("unreadable-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err = SummarizeDeferred(RuntimeClaudeCode, pending, &cfg)
	if err != nil || summary.NoFence != 1 || summary.RunMismatch != 0 || summary.SessionUnbound != 0 || summary.IdentityMismatch != 2 || summary.Total() != 3 {
		t.Fatalf("binding deferred summary = %#v, %v; want no-fence 1, identity-mismatch 2, total 3", summary, err)
	}
}

// R3 leaves pathless Codex events to the existing ephemeral partition.
func TestIdentityMismatchSummaryExcludesPathlessCodex(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	saveTestCaptureConfig(t, RuntimeCodex)
	cfg, err := LoadConfig(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	foreign := Event{
		Runtime: cfg.Runtime, Account: cfg.Account, Realm: cfg.Realm,
		Agent: "cc-accept-1", AgentID: "agent_foreign", AgentName: "cc-accept-1",
		Location: cfg.Location, SessionID: "codex-session", Body: "foreign-identity canary",
	}
	for _, sourcePath := range []string{"", " \t", "/tmp/persisted-rollout.jsonl"} {
		for _, turnID := range []string{"", "open-turn"} {
			foreign.SourceTranscriptPath = sourcePath
			foreign.TurnID = turnID
			pending := []PendingEvent{{Event: foreign}}
			summary, err := SummarizeDeferred(RuntimeCodex, pending, &cfg)
			wantMismatch, wantUnbound := 0, 0
			if sourcePath == "/tmp/persisted-rollout.jsonl" {
				wantMismatch = 1
			} else if turnID != "" {
				wantUnbound = 1
			}
			if err != nil || summary.IdentityMismatch != wantMismatch || summary.SessionUnbound != wantUnbound || summary.Total() != wantMismatch+wantUnbound {
				t.Fatalf("Codex source %q turn %q summary = %#v, %v; want identity %d, unbound %d", sourcePath, turnID, summary, err, wantMismatch, wantUnbound)
			}
			summary, err = SummarizeDeferred(RuntimeCodex, pending, nil)
			wantUnbound = 0
			if turnID != "" {
				wantUnbound = 1
			}
			if err != nil || summary.IdentityMismatch != 0 || summary.SessionUnbound != wantUnbound || summary.Total() != wantUnbound {
				t.Fatalf("nil-config Codex summary = %#v, %v; want unbound %d", summary, err, wantUnbound)
			}
		}
	}
}

// L5 pins the privacy reason held events must remain in the ordinary outbox.
func TestIdentityMismatchQueuedTurnStillRedacts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	saveTestCaptureConfig(t, RuntimeClaudeCode)
	event := enqueueTestHook(t, RuntimeClaudeCode,
		`{"session_id":"redacted-foreign-session","hook_event_name":"UserPromptSubmit","prompt":"foreign-identity canary"}`)
	cfg, err := LoadConfig(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	event.Agent, event.AgentID, event.AgentName = "cc-accept-1", "agent_foreign", "cc-accept-1"
	event.Raw = json.RawMessage(`{"value":"foreign-identity canary raw"}`)
	event.RecoveredMessages = []RecoveredMessage{{Body: "foreign-identity canary recovered"}}
	writeIdentityMismatchFixture(t, RuntimeClaudeCode, event)
	if !HeldForBinding(RuntimeClaudeCode, event, cfg) {
		t.Fatal("redaction fixture was not mismatched")
	}
	if err := RedactPendingTurn(RuntimeClaudeCode, event.SessionID, event.TurnID); err != nil {
		t.Fatal(err)
	}
	pending, err := Pending(RuntimeClaudeCode)
	if err != nil || len(pending) != 1 {
		t.Fatalf("redacted pending count = %d, want 1: %v", len(pending), err)
	}
	redacted := pending[0].Event
	var data map[string]any
	if err := json.Unmarshal(redacted.Data, &data); err != nil || len(data) != 1 || data["sealed_content_omitted"] != true {
		t.Fatalf("redacted marker does not contain only sealed_content_omitted=true: %v", err)
	}
	if redacted.Body != "prompt omitted from portable transcript because this turn used sealed secrets" || len(redacted.Raw) != 0 || len(redacted.RecoveredMessages) != 0 {
		t.Fatal("mismatched queued prompt was not fully redacted")
	}
	if !HeldForBinding(RuntimeClaudeCode, redacted, cfg) || redacted.ID != event.ID || redacted.SessionID != event.SessionID || redacted.TurnID != event.TurnID {
		t.Fatal("redaction changed the queued event identity")
	}
	raw, err := os.ReadFile(pending[0].Path)
	if err != nil || bytes.Contains(raw, []byte("foreign-identity canary")) {
		t.Fatalf("redacted outbox retained canary content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "capture", "quarantine")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("redaction created quarantine: %v", err)
	}
}

func writeIdentityMismatchFixture(t *testing.T, outboxRuntime string, event Event) {
	t.Helper()
	dir, err := outboxDir(outboxRuntime)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%020d-%s.json", event.OccurredAt.UnixNano(), event.ID))
	if err := writeJSONAtomic(path, event); err != nil {
		t.Fatal(err)
	}
}
