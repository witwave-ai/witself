package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestTranscriptReleaseLegacySessionEndLateResultFlush(t *testing.T) {
	f := newTranscriptReleaseSessionFixture(t)
	const promptText = "Preserve this mixed-version release prompt."
	prompt := f.enqueue(map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": promptText})
	f.enqueue(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_response": "original result"})
	if stderr, code := f.release(); code != 0 {
		t.Fatalf("release exit = %d: %s", code, stderr)
	}

	if executable := os.Getenv("WITSELF_TEST_LEGACY_HOOK_BINARY"); executable != "" {
		// A pre-release executable can exercise the actual version mismatch
		// without compiling an arbitrary moving HEAD during ordinary tests.
		for _, hook := range []map[string]any{
			{"session_id": f.session, "hook_event_name": "SessionEnd"},
			{"session_id": f.session, "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_response": "LATE_CANARY"},
		} {
			raw, err := json.Marshal(hook)
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(executable, "transcript", "hook", "--runtime", transcriptcapture.RuntimeClaudeCode)
			command.Stdin = bytes.NewReader(raw)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("legacy hook failed: %v: %s", err, output)
			}
		}
	} else {
		// Stable regression fixture for the legacy wire format: SessionEnd
		// deletes the state; the following hook writes a new run without a
		// turn ID or operator_release and emits an ordinary raw tool result.
		sum := sha256.Sum256([]byte(f.session))
		statePath := filepath.Join(os.Getenv("WITSELF_HOME"), "capture", "state", transcriptcapture.RuntimeClaudeCode, hex.EncodeToString(sum[:16])+".json")
		if err := os.Remove(statePath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, []byte(`{"run_id":"legacy-recreated-run"}`), 0600); err != nil {
			t.Fatal(err)
		}
		late := prompt
		late.ID = "evt_legacy_late"
		late.RunID, late.TurnID = "legacy-recreated-run", ""
		late.HookEvent, late.NativeHookEvent = "PostToolUse", "PostToolUse"
		late.Kind, late.Role, late.Body = "tool.result", "tool", "LATE_CANARY"
		late.Data = json.RawMessage(`{"tool":{"name":"Bash","output":"LATE_CANARY"}}`)
		late.Raw = json.RawMessage(`{"tool_response":"LATE_CANARY"}`)
		late.OccurredAt = time.Now().UTC()
		raw, err := json.Marshal(late)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(os.Getenv("WITSELF_HOME"), "capture", "outbox", late.Runtime, fmt.Sprintf("%020d-%s.json", late.OccurredAt.UnixNano(), late.ID))
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	var lateID string
	for _, item := range pending {
		if bytes.Contains([]byte(item.Event.Body), []byte("LATE_CANARY")) {
			if item.Event.TurnID != "" {
				t.Fatal("legacy fixture did not erase the released turn identity")
			}
			lateID = item.Event.ID
		}
	}
	if lateID == "" {
		t.Fatal("legacy hook did not queue the plaintext late result")
	}
	_, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode})
	})
	f.assertUploads(promptText, "LATE_CANARY")
	if code != 1 || !strings.Contains(stderr, "deferred") {
		t.Fatalf("flush exit = %d: %s", code, stderr)
	}
	pending, err = transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	legacyHeld := false
	for _, item := range pending {
		if item.Event.ID == lateID && !transcriptcapture.PendingEventUploadReady(item, pending) {
			legacyHeld = true
		}
	}
	if !legacyHeld {
		t.Fatal("legacy plaintext result was uploaded or lost instead of remaining gated")
	}
	// An old uploader provides no attempt history. Even a current Stop must
	// not rewrite those ambiguous external IDs: it cannot know whether the
	// server already accepted their original bytes through that old uploader.
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(`{"session_id":"release-session","hook_event_name":"UserPromptSubmit","prompt":"Seal a fresh turn after the legacy hook."}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(`{"session_id":"release-session","hook_event_name":"Stop"}`)); err == nil {
		t.Fatal("fresh Stop rewrote a legacy event with unknown submission history")
	}
	pending, err = transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pending {
		if item.Event.ID == lateID && transcriptcapture.PendingEventUploadReady(item, pending) {
			t.Fatal("fresh Stop admitted legacy plaintext after failed reconciliation")
		}
	}
	f.assertUploads(promptText, "LATE_CANARY")
}
