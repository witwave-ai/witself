package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const identityHeldCanary = "foreign-identity canary"

func identityHeldFixture(cfg transcriptcapture.Config, id, session, body string) transcriptcapture.Event {
	return transcriptcapture.Event{
		SchemaVersion: transcriptcapture.SchemaVersion, ID: id, Runtime: cfg.Runtime,
		CaptureMode: cfg.CaptureMode, Account: cfg.Account, AccountID: cfg.AccountID,
		Realm: cfg.Realm, RealmID: cfg.RealmID, Agent: cfg.Agent, AgentID: cfg.AgentID,
		AgentName: cfg.AgentName, Location: cfg.Location, SessionID: session, RunID: "run_identity_held",
		HookEvent: "SessionStart", NativeHookEvent: "SessionStart", Kind: "session.started", Role: "system",
		Body: body, OccurredAt: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
	}
}

func identityHeldForeign(event transcriptcapture.Event) transcriptcapture.Event {
	event.Agent, event.AgentID, event.AgentName = "cc-accept-1", "agent_foreign", "cc-accept-1"
	return event
}

func writeIdentityHeldFixture(t *testing.T, home, runtime, basename string, event transcriptcapture.Event) (string, []byte) {
	t.Helper()
	path, raw, err := writeCapturePendingFixture(home, runtime, basename, event)
	if err != nil {
		t.Fatal(err)
	}
	return path, raw
}

func assertIdentityHeldBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("held file changed: read error %v, bytes equal %v", err, bytes.Equal(got, want))
	}
}

func assertIdentityHeldNoQuarantine(t *testing.T, home string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(home, "capture", "quarantine")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("holding identity created a quarantine directory: %v", err)
	}
}

// C1: the installed identity drains normally, and a held identity can return.
func TestTranscriptIdentityHeldNotFailedAndTransient(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	var appended []string
	srv := startCaptureLedgerServer(t, &appended)
	configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, srv.URL)
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	foreign := identityHeldForeign(identityHeldFixture(cfg, "evt_foreign_held", "identity-shared-session", identityHeldCanary))
	foreignPath, foreignRaw := writeIdentityHeldFixture(t, home, cfg.Runtime, "00000000000000000001-evt_foreign_held.json", foreign)
	for i, body := range []string{"matching first body", "matching second body"} {
		event := identityHeldFixture(cfg, fmt.Sprintf("evt_matching_%d", i), foreign.SessionID, body)
		writeIdentityHeldFixture(t, home, cfg.Runtime, fmt.Sprintf("0000000000000000000%d-%s.json", i+2, event.ID), event)
	}
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptStatus([]string{"--runtime", cfg.Runtime})
	})
	wantStatus := "claude-code capture: 3 queued event(s); deferred 1 (no-fence 0, run-mismatch 0, session-unbound 0, identity-mismatch 1)\n"
	if code != 0 || stdout != wantStatus || stderr != "" {
		t.Fatalf("identity status = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	for _, flushed := range []int{2, 0} {
		stdout, stderr, code = captureFactDeleteCLI(t, func() int {
			return transcriptFlush([]string{"--runtime", cfg.Runtime})
		})
		want := fmt.Sprintf("flushed %d claude-code transcript event(s); deferred 1 incomplete or mismatched event(s) (no-fence 0, run-mismatch 0, session-unbound 0, identity-mismatch 1)\n", flushed)
		if code != 0 || stdout != "" || stderr != want {
			t.Fatalf("held flush = code %d stdout %q stderr %q, want exit 0 and stderr %q", code, stdout, stderr, want)
		}
		assertIdentityHeldBytes(t, foreignPath, foreignRaw)
		assertOnlyCaptureEventPending(t, cfg.Runtime, foreign.ID)
		assertIdentityHeldNoQuarantine(t, home)
		if !slices.Equal(appended, []string{"matching first body", "matching second body"}) {
			t.Fatalf("held flush uploaded bodies = %#v", appended)
		}
	}
	cfg.Agent, cfg.AgentID, cfg.AgentName = foreign.Agent, foreign.AgentID, foreign.AgentName
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", cfg.Runtime})
	})
	if code != 0 || stdout != "" || stderr != "flushed 1 claude-code transcript event(s)\n" {
		t.Fatalf("restored binding flush = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if !slices.Equal(appended, []string{"matching first body", "matching second body", identityHeldCanary}) {
		t.Fatalf("restored binding uploaded bodies = %#v", appended)
	}
	pending, err := transcriptcapture.Pending(cfg.Runtime)
	if err != nil || len(pending) != 0 {
		t.Fatalf("restored binding pending count = %d, error %v", len(pending), err)
	}
	assertIdentityHeldNoQuarantine(t, home)
}

// C2: a binding hold does not excuse the installed identity's open turn.
func TestTranscriptIdentityOtherDeferralStillFails(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, "http://127.0.0.1:1")
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	foreign := identityHeldForeign(identityHeldFixture(cfg, "evt_foreign_open_neighbor", "foreign-held-session", identityHeldCanary))
	writeIdentityHeldFixture(t, home, cfg.Runtime, "00000000000000000001-evt_foreign_open_neighbor.json", foreign)
	if _, err := transcriptcapture.EnqueueHook(cfg.Runtime, []byte(
		`{"session_id":"installed-open-session","hook_event_name":"UserPromptSubmit","prompt":"matching open prompt"}`,
	)); err != nil {
		t.Fatal(err)
	}
	pending, err := transcriptcapture.Pending(cfg.Runtime)
	if err != nil || len(pending) != 2 {
		t.Fatalf("open-turn fixture count = %d, error %v", len(pending), err)
	}
	before := make(map[string][]byte, len(pending))
	for _, item := range pending {
		raw, err := os.ReadFile(item.Path)
		if err != nil {
			t.Fatal(err)
		}
		before[item.Path] = raw
	}
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", cfg.Runtime})
	})
	want := "flushed 0 claude-code transcript event(s); deferred 2 incomplete or mismatched event(s) (no-fence 1, run-mismatch 0, session-unbound 0, identity-mismatch 1)\n"
	if code != 1 || stdout != "" || stderr != want {
		t.Fatalf("open-turn flush = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	for path, raw := range before {
		assertIdentityHeldBytes(t, path, raw)
	}
	assertIdentityHeldNoQuarantine(t, home)
}

// C3: an event misplaced in another runtime's outbox remains an error.
func TestTranscriptIdentityRuntimeMismatchStillFails(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, "http://127.0.0.1:1")
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	event := identityHeldFixture(cfg, "evt_wrong_runtime", "wrong-runtime-session", "wrong runtime body")
	event.Runtime = transcriptcapture.RuntimeCursor
	path, raw := writeIdentityHeldFixture(t, home, cfg.Runtime, "00000000000000000001-evt_wrong_runtime.json", event)
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptStatus([]string{"--runtime", cfg.Runtime})
	})
	wantStatus := "claude-code capture: 1 queued event(s); deferred 0 (no-fence 0, run-mismatch 0, session-unbound 0, identity-mismatch 0)\n"
	if code != 0 || stdout != wantStatus || stderr != "" {
		t.Fatalf("runtime mismatch status = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	stdout, stderr, code = captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", cfg.Runtime})
	})
	want := "witself: finalize capture event: queued transcript identity does not match the installed runtime binding\n" +
		"flushed 0 claude-code transcript event(s); deferred 1 incomplete or mismatched event(s) (no-fence 0, run-mismatch 0, session-unbound 0, identity-mismatch 0, other 1)\n"
	if code != 1 || stdout != "" || stderr != want {
		t.Fatalf("runtime mismatch flush = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	assertIdentityHeldBytes(t, path, raw)
	assertIdentityHeldNoQuarantine(t, home)
}

// C5: the earlier ephemeral-move failure owns the hold even if identity differs.
func TestTranscriptIdentityFailedEphemeralMoveStillFails(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	var appended []string
	srv := startCaptureLedgerServer(t, &appended)
	configureCaptureFlushTest(t, transcriptcapture.RuntimeCodex, srv.URL)
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	ephemeral := identityHeldForeign(identityHeldFixture(cfg, "evt_ephemeral_identity_collision", "ephemeral-collision-session", identityHeldCanary))
	const basename = "00000000000000000001-evt_ephemeral_identity_collision.json"
	source, sourceRaw := writeIdentityHeldFixture(t, home, cfg.Runtime, basename, ephemeral)
	quarantine := filepath.Join(home, "capture", "quarantine", cfg.Runtime)
	if err := os.MkdirAll(quarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	const collision = "existing-quarantine-file-canary\n"
	destination := filepath.Join(quarantine, basename)
	if err := os.WriteFile(destination, []byte(collision), 0o600); err != nil {
		t.Fatal(err)
	}
	// Retain ordinary work so this exercises the main-path exit exemption.
	matching := identityHeldFixture(cfg, "evt_sourced_identity_neighbor", "sourced-matching-session", "matching sourced body")
	matching.SourceTranscriptPath = filepath.Join(t.TempDir(), "rollout.jsonl")
	writeIdentityHeldFixture(t, home, cfg.Runtime, "00000000000000000002-evt_sourced_identity_neighbor.json", matching)
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", cfg.Runtime})
	})
	if code != 1 {
		t.Fatalf("failed ephemeral identity flush exit = %d, want 1; stderr %q", code, stderr)
	}
	if stdout != "" || strings.Count(stderr, "destination already exists") != 1 ||
		!strings.Contains(stderr, "flushed 1 codex transcript event(s); deferred 1 incomplete or mismatched event(s) (no-fence 0, run-mismatch 0, session-unbound 0, identity-mismatch 0, other 1)\n") ||
		strings.Contains(stderr, "finalize capture event") {
		t.Fatalf("failed ephemeral identity flush stdout %q stderr %q", stdout, stderr)
	}
	if !slices.Equal(appended, []string{"matching sourced body"}) {
		t.Fatalf("collision main-path uploads = %#v", appended)
	}
	assertIdentityHeldBytes(t, source, sourceRaw)
	assertIdentityHeldBytes(t, destination, []byte(collision))
	assertOnlyCaptureEventPending(t, cfg.Runtime, ephemeral.ID)
	skipped, err := transcriptcapture.SkippedSessions(cfg.Runtime)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("failed collision skipped count = %d, error %v", len(skipped), err)
	}
}

func TestTranscriptIdentityStatusNoReadableBinding(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%t", corrupt), func(t *testing.T) {
			home := filepath.Join(t.TempDir(), ".witself")
			t.Setenv("WITSELF_HOME", home)
			t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
			const runtime = transcriptcapture.RuntimeClaudeCode
			if corrupt {
				path, err := transcriptcapture.ConfigPath(runtime)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("private-invalid-binding-canary"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			stdout, stderr, code := captureFactDeleteCLI(t, func() int {
				return transcriptStatus([]string{"--runtime", runtime})
			})
			want := "claude-code capture: 0 queued event(s); deferred 0 (no-fence 0, run-mismatch 0, session-unbound 0)\n"
			if code != 0 || stdout != want || stderr != "" {
				t.Fatalf("uninstalled empty status = code %d stdout %q stderr %q", code, stdout, stderr)
			}
			event := identityHeldForeign(identityHeldFixture(transcriptcapture.Config{Runtime: runtime}, "evt_no_binding", "no-binding-session", identityHeldCanary))
			event.TurnID = "turn_no_binding"
			path, raw := writeIdentityHeldFixture(t, home, runtime, "00000000000000000001-evt_no_binding.json", event)
			stdout, stderr, code = captureFactDeleteCLI(t, func() int {
				return transcriptStatus([]string{"--runtime", runtime})
			})
			want = "claude-code capture: 1 queued event(s); deferred 1 (no-fence 0, run-mismatch 0, session-unbound 1)\n"
			if code != 0 || stdout != want || stderr != "witself: identity-mismatch not classified: no readable claude-code binding\n" {
				t.Fatalf("uninstalled queued status = code %d stdout %q stderr %q", code, stdout, stderr)
			}
			assertIdentityHeldBytes(t, path, raw)
			assertIdentityHeldNoQuarantine(t, home)
		})
	}
}

func TestTranscriptIdentityStatusOmitsPathlessCodexFromIdentityBucket(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	configureCaptureFlushTest(t, transcriptcapture.RuntimeCodex, "http://127.0.0.1:1")
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	pathless := identityHeldForeign(identityHeldFixture(cfg, "evt_pathless_status", "pathless-status-session", identityHeldCanary))
	path, raw := writeIdentityHeldFixture(t, home, cfg.Runtime, "00000000000000000001-evt_pathless_status.json", pathless)
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptStatus([]string{"--runtime", cfg.Runtime})
	})
	want := "codex capture: 1 queued event(s); deferred 0 (no-fence 0, run-mismatch 0, session-unbound 0, identity-mismatch 0)\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("pathless Codex status = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	assertIdentityHeldBytes(t, path, raw)
	assertIdentityHeldNoQuarantine(t, home)
}

func TestCountHeldForBindingCaptureEventsUsesHeldReason(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	foreign := identityHeldForeign(identityHeldFixture(transcriptcapture.Config{Runtime: transcriptcapture.RuntimeClaudeCode}, "evt_count_held", "held-count-session", identityHeldCanary))
	pending := []transcriptcapture.PendingEvent{
		{Path: "direct", Event: foreign},
		{Path: "wrapped", Event: foreign},
		{Path: "same-text-other-error", Event: foreign},
		{Path: "failed-move", Event: foreign},
		{Path: "nil-reason", Event: foreign},
		{Path: "not-held", Event: foreign},
	}
	held := map[string]error{
		"direct":                transcriptcapture.ErrIdentityMismatch,
		"wrapped":               fmt.Errorf("held: %w", transcriptcapture.ErrIdentityMismatch),
		"same-text-other-error": errors.New(transcriptcapture.ErrIdentityMismatch.Error()),
		"failed-move":           errors.New("ephemeral Codex capture event could not be quarantined"),
		"nil-reason":            nil,
		"absent-from-remaining": transcriptcapture.ErrIdentityMismatch,
	}
	if got := countHeldForBindingCaptureEvents(pending, held); got != 2 {
		t.Fatalf("held-for-binding count = %d, want 2 from the remaining held reasons only", got)
	}
	if deferred := countBlockedCaptureEvents(pending, nil, held); deferred != 5 {
		t.Fatalf("total deferred = %d, want 5 including every other held reason", deferred)
	}
}
