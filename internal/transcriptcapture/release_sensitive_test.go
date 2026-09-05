package transcriptcapture

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseResidueRefusesFailedSensitiveRedactionBeforeHoldingTurn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix file permission validation")
	}
	for _, recovery := range []string{"queued-fence", "queued-hook", "attempted"} {
		t.Run(recovery, func(t *testing.T) {
			setupFenceCapture(t, ModeRaw)
			const canary = "SEALED_RELEASE_CANARY"
			prompt := enqueueTestHook(t, RuntimeCodex, `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"SEALED_RELEASE_CANARY","transcript_path":"/tmp/s.jsonl"}`)
			pending, err := Pending(RuntimeCodex)
			if err != nil || len(pending) != 1 {
				t.Fatalf("prompt pending = %d, %v", len(pending), err)
			}
			blockedPath := pending[0].Path
			if recovery == "attempted" {
				if err := MarkPendingSubmitted(pending[0]); err != nil {
					t.Fatal(err)
				}
				tool := enqueueTestHook(t, RuntimeCodex, `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"ordinary_tool","tool_response":"SEALED_RELEASE_CANARY","transcript_path":"/tmp/s.jsonl"}`)
				pending, err = Pending(RuntimeCodex)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range pending {
					if item.Event.ID == tool.ID {
						blockedPath = item.Path
					}
				}
			}
			if err := os.Chmod(blockedPath, 0666); err != nil {
				t.Fatal(err)
			}
			_, err = EnqueueHook(RuntimeCodex, []byte(`{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"mcp__witself__witself_secret_create","transcript_path":"/tmp/s.jsonl"}`))
			if err == nil {
				t.Fatal("sealed hook unexpectedly completed redaction")
			}
			state, err := loadSessionState(RuntimeCodex, "s")
			if err != nil || !state.SensitiveTurn || state.RunID != prompt.RunID || state.TurnID != prompt.TurnID {
				t.Fatalf("failed redaction lost sensitive identity: %v", err)
			}
			if err := os.Chmod(blockedPath, 0600); err != nil {
				t.Fatal(err)
			}
			if count, err := ReleaseResidue(RuntimeCodex, "s", 0, true); count != 0 || err == nil || !strings.Contains(err.Error(), "pending sealed-content suppression") {
				t.Fatalf("release of known-sensitive plaintext = %d, %v", count, err)
			}
			state, err = loadSessionState(RuntimeCodex, "s")
			if err != nil || state.OperatorRelease != nil {
				t.Fatalf("refusal persisted a release hold that would block fence recovery: %v", err)
			}
			if fence := loadCompletedFence(prompt); !fence.OccurredAt.IsZero() {
				t.Fatal("refused release published completion")
			}
			pending, err = Pending(RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			index := NewReadinessIndex(pending)
			for _, item := range pending {
				if item.submission == nil && index.UploadReady(item) {
					t.Fatal("refused release made unsubmitted canary content upload-ready")
				}
			}
			if recovery == "attempted" {
				// The attempted envelope may already exist remotely. Refusal must
				// preserve its immutable provenance, not claim it was suppressed.
				for _, item := range pending {
					if item.Event.ID == prompt.ID && (item.submission == nil || !strings.Contains(item.Event.Body, canary)) {
						t.Fatal("refused release changed the attempted envelope")
					}
				}
				return
			}
			if recovery == "queued-hook" {
				enqueueTestHook(t, RuntimeCodex, `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"mcp__witself__witself_secret_create","transcript_path":"/tmp/s.jsonl"}`)
				if count, err := ReleaseResidue(RuntimeCodex, "s", 0, true); count != 1 || err != nil {
					t.Fatalf("release after completed suppression = %d, %v", count, err)
				}
			} else if _, created, err := EnqueueFence(RuntimeCodex, "s", prompt.RunID, prompt.TurnID, "retry sealed suppression"); err != nil || !created {
				t.Fatalf("runtime fence recovery = %t, %v", created, err)
			}
			pending, err = Pending(RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			index = NewReadinessIndex(pending)
			for _, item := range pending {
				raw, err := os.ReadFile(item.Path)
				if err != nil || bytes.Contains(raw, []byte(canary)) ||
					(item.Event.ID == prompt.ID && !eventSealedContentOmitted(item.Event.Data)) || !index.UploadReady(item) {
					t.Fatalf("recovery retained canary content or failed to release suppressed event: %s, %v", item.Event.HookEvent, err)
				}
			}
			if recovery != "queued-hook" {
				return
			}
			// A sensitive current identity must not suppress an unrelated
			// residue turn, including when only its run or turn differs.
			for _, mismatch := range []string{"run", "turn"} {
				other := enqueueTestHook(t, RuntimeCodex, `{"session_id":"other-`+mismatch+`","hook_event_name":"UserPromptSubmit","prompt":"SEALED_RELEASE_CANARY","transcript_path":"/tmp/s.jsonl"}`)
				otherState, err := loadSessionState(RuntimeCodex, other.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				otherState.SensitiveTurn = true
				if mismatch == "run" {
					otherState.RunID = "different-run"
				} else {
					otherState.TurnID = "different-turn"
				}
				if err := saveSessionState(RuntimeCodex, other.SessionID, otherState); err != nil {
					t.Fatal(err)
				}
				if count, err := ReleaseResidue(RuntimeCodex, other.SessionID, 0, true); count != 1 || err != nil {
					t.Fatalf("release with unrelated sensitive %s = %d, %v", mismatch, count, err)
				}
				pending, err := Pending(RuntimeCodex)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range pending {
					if item.Event.ID == other.ID && (item.Event.Body != canary || !NewReadinessIndex(pending).UploadReady(item)) {
						t.Fatalf("unrelated sensitive %s changed residue prompt or held release", mismatch)
					}
				}
			}
		})
	}
}
