package transcriptcapture

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseResidueInterruptedRewriteSurvivesCompanionFence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("queued-file permission validation requires Unix")
	}
	setupFenceCapture(t, ModeRaw)
	prompt := fenceTestPromptAndTool(t, false)
	result := enqueueTestHook(t, RuntimeCodex, `{"session_id":"delegated","hook_event_name":"PostToolUse","tool_name":"ordinary_browser_tool","tool_use_id":"tool-1","tool_response":"release-recovery-canary","transcript_path":"/tmp/delegated-rollout.jsonl"}`)
	pending, err := Pending(RuntimeCodex)
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
		t.Fatal("tool result was not queued")
	}
	if err := os.Chmod(resultPath, 0o666); err != nil {
		t.Fatal(err)
	}
	if count, err := ReleaseResidue(RuntimeCodex, "delegated", 0, true); err == nil || count != 0 || !strings.Contains(err.Error(), "not a trusted regular file") {
		t.Fatalf("interrupted release = %d, %v", count, err)
	}
	afterFailure := snapshotFenceRetry(t, "delegated")
	state, err := loadSessionState(RuntimeCodex, "delegated")
	if err != nil || state.OperatorRelease == nil || state.OperatorRelease.Status != "pending" ||
		!state.OperatorRelease.releasesTurn(prompt.TurnID) || state.OperatorRelease.Since.IsZero() {
		t.Fatalf("release did not persist its session hold before the failed rewrite: %v", err)
	}
	pending, err = Pending(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	var operatorFenceID string
	index := NewReadinessIndex(pending)
	for _, item := range pending {
		if index.UploadReady(item) {
			t.Fatal("interrupted release admitted a queued event")
		}
		if item.Event.Kind == OperatorReleaseKind {
			operatorFenceID = item.Event.ID
		}
	}
	if operatorFenceID == "" {
		t.Fatal("release failed before persisting its operator hold marker")
	}
	if err := os.Chmod(resultPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "job-completed"); err == nil || created || !strings.Contains(err.Error(), "retry transcript release") {
		t.Errorf("companion fence must preserve pending release: created=%t, error=%v", created, err)
	} else {
		assertFenceRetryUnchanged(t, "delegated", afterFailure)
	}
	if count, err := ReleaseResidue(RuntimeCodex, "delegated", 0, true); err != nil || count != 1 {
		t.Fatalf("release retry = %d, %v; want one recovered turn", count, err)
	}
	fence := loadCompletedFence(prompt)
	if fence.Kind != OperatorReleaseKind || fence.OccurredAt.IsZero() {
		t.Fatalf("recovered completion = %#v", fence)
	}
	pending, err = Pending(RuntimeCodex)
	if err != nil || len(pending) != 4 {
		t.Fatalf("recovered pending events = %d, %v", len(pending), err)
	}
	index = NewReadinessIndex(pending)
	for _, item := range pending {
		raw, err := os.ReadFile(item.Path)
		if err != nil || bytes.Contains(raw, []byte("release-recovery-canary")) || !operatorReleasedEventSafe(item.Event) || !index.UploadReady(item) {
			t.Fatalf("recovered event is unsafe or held: %s, %v", item.Event.Kind, err)
		}
		if item.Event.Kind == OperatorReleaseKind && item.Event.ID != operatorFenceID {
			t.Fatal("release retry replaced the durable operator hold marker")
		}
		if item.Event.ID == prompt.ID && item.Event.Body != prompt.Body {
			t.Fatal("release retry changed the captured prompt")
		}
		if err := RemovePending(item.Path); err != nil {
			t.Fatal(err)
		}
	}
	if _, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "job-completed"); err != nil || created {
		t.Fatalf("completed release fence retry = %t, %v", created, err)
	}
	if count, err := ReleaseResidue(RuntimeCodex, "delegated", 0, true); err != nil || count != 0 {
		t.Fatalf("completed release retry = %d, %v", count, err)
	}
}
