package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func setupTranscriptFenceCLI(t *testing.T, sensitive bool) transcriptcapture.Event {
	t.Helper()
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCodex, CaptureMode: transcriptcapture.ModeRaw,
		Account: "default", Realm: "default", Agent: "scott", AgentID: "agent_1", AgentName: "scott", Location: location,
	}); err != nil {
		t.Fatal(err)
	}
	tool := "ordinary_tool"
	if sensitive {
		tool = "mcp__witself__witself_secret_reveal"
	}
	var prompt transcriptcapture.Event
	for _, input := range []map[string]any{
		{"hook_event_name": "UserPromptSubmit", "prompt": "prompt-cli-fence-canary"},
		{"hook_event_name": "PreToolUse", "tool_name": tool, "tool_use_id": "tool-1", "tool_input": map[string]any{"value": "tool-cli-fence-canary"}},
	} {
		input["session_id"] = "delegated"
		input["transcript_path"] = "/tmp/codex-delegated.jsonl"
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
	return prompt
}

func TestTranscriptFenceCommandReadyIdempotentAndSensitive(t *testing.T) {
	for _, sensitive := range []bool{false, true} {
		name := "ordinary"
		if sensitive {
			name = "sealed"
		}
		t.Run(name, func(t *testing.T) {
			prompt := setupTranscriptFenceCLI(t, sensitive)
			args := []string{"fence", "--runtime", "codex", "--session", "delegated", "--run", prompt.RunID, "--turn", prompt.TurnID, "--reason", "reason-cli-fence-canary"}
			for range 2 {
				if code := transcriptCmd(args); code != 0 {
					t.Fatalf("transcript fence exit = %d", code)
				}
			}
			pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
			if err != nil || len(pending) != 3 {
				t.Fatalf("fenced pending events = %d, %v", len(pending), err)
			}
			index := transcriptcapture.NewReadinessIndex(pending)
			for _, item := range pending {
				if !index.UploadReady(item) {
					t.Fatalf("command did not release %s", item.Event.HookEvent)
				}
				if sensitive {
					raw, err := os.ReadFile(item.Path)
					if err != nil || bytes.Contains(raw, []byte("cli-fence-canary")) {
						t.Fatalf("sealed command retained a canary, read error %v", err)
					}
				}
			}
			terminal := pending[len(pending)-1].Event
			var data map[string]any
			if err := json.Unmarshal(terminal.Data, &data); err != nil {
				t.Fatal(err)
			}
			if terminal.Kind != "turn.completed" || terminal.Role != "system" || data["synthetic_fence"] != true {
				t.Fatalf("command fence = %#v", terminal)
			}
		})
	}
}

func TestTranscriptFenceCommandRefusesUnknownAndInvalidArguments(t *testing.T) {
	prompt := setupTranscriptFenceCLI(t, false)
	for _, test := range []struct {
		args []string
		code int
	}{
		{[]string{"--runtime", "codex", "--session", "unknown", "--run", prompt.RunID, "--turn", prompt.TurnID}, 1},
		{[]string{"--runtime", "claude-code", "--session", "delegated", "--run", prompt.RunID, "--turn", prompt.TurnID}, 2},
		{[]string{"--runtime", "codex", "--session", "delegated"}, 2},
		{[]string{"--runtime", "codex", "--session", "delegated", "--run", prompt.RunID}, 2},
		{[]string{"--runtime", "codex", "--session", "delegated", "--turn", prompt.TurnID}, 2},
		{[]string{"--runtime", "codex", "--session", "delegated", "--run", " ", "--turn", prompt.TurnID}, 2},
		{[]string{"--runtime", "codex", "--session", "delegated", "--run", prompt.RunID, "--turn", " "}, 2},
		{[]string{"--runtime", "codex"}, 2},
		{[]string{"--session", "delegated"}, 2},
		{[]string{"--runtime", "codex", "--session", "delegated", "--run", prompt.RunID, "--turn", prompt.TurnID, "unexpected"}, 2},
	} {
		if code := transcriptFence(test.args); code != test.code {
			t.Fatalf("fence %q exit = %d, want %d", test.args, code, test.code)
		}
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || len(pending) != 2 {
		t.Fatalf("refused command changed pending events: %d, %v", len(pending), err)
	}
}

func TestTranscriptFenceCommandStartsNormalFlush(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell executable fixture requires Unix")
	}
	prompt := setupTranscriptFenceCLI(t, false)
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "")
	dir := t.TempDir()
	marker := filepath.Join(dir, "flush-args")
	executable := filepath.Join(dir, "witself")
	t.Setenv("WITSELF_FENCE_TEST_MARKER", marker)
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$WITSELF_FENCE_TEST_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(witselfExecutableTestEnv, executable)
	if code := transcriptFence([]string{"--runtime", "codex", "--session", "delegated", "--run", prompt.RunID, "--turn", prompt.TurnID}); code != 0 {
		t.Fatalf("fence exit = %d", code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		raw, err := os.ReadFile(marker)
		if err == nil && bytes.Equal(raw, []byte("transcript\nflush\n--runtime\ncodex\n")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("normal flush was not started: %q, %v", raw, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTranscriptFenceCommandRetryCannotCompleteResumedTurn(t *testing.T) {
	prompt := setupTranscriptFenceCLI(t, false)
	args := []string{"fence", "--runtime", "codex", "--session", prompt.SessionID, "--run", prompt.RunID, "--turn", prompt.TurnID}
	if code := transcriptCmd(args); code != 0 {
		t.Fatalf("initial fence exit = %d", code)
	}
	resumed, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, []byte(`{"hook_event_name":"UserPromptSubmit","session_id":"delegated","transcript_path":"/tmp/codex-delegated.jsonl","prompt":"SECRET_CANARY"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.TurnID == prompt.TurnID {
		t.Fatal("resumed prompt did not open a new turn")
	}
	if code := transcriptCmd(args); code != 0 {
		t.Fatalf("identical delayed fence exit = %d", code)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || len(pending) != 4 {
		t.Fatalf("delayed fence changed pending events: %d, %v", len(pending), err)
	}
	index := transcriptcapture.NewReadinessIndex(pending)
	foundPrompt := false
	for _, item := range pending {
		if item.Event.ID == resumed.ID {
			foundPrompt = true
			if index.UploadReady(item) {
				t.Fatal("delayed fence released the resumed prompt")
			}
		}
	}
	if !foundPrompt {
		t.Fatal("resumed prompt missing from outbox")
	}
	sealed, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, []byte(`{"hook_event_name":"PreToolUse","session_id":"delegated","transcript_path":"/tmp/codex-delegated.jsonl","tool_name":"mcp__witself__witself_secret_reveal","tool_use_id":"sealed-resumed"}`))
	if err != nil {
		t.Fatal(err)
	}
	if sealed.TurnID != resumed.TurnID {
		t.Fatal("delayed fence cleared the resumed turn before its sealed hook")
	}
	pending, err = transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || len(pending) != 5 {
		t.Fatalf("sealed pending events = %d, %v", len(pending), err)
	}
	index = transcriptcapture.NewReadinessIndex(pending)
	for _, item := range pending {
		raw, readErr := os.ReadFile(item.Path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Contains(raw, []byte("SECRET_CANARY")) {
			t.Fatal("sealed hook retained the resumed prompt canary")
		}
		if item.Event.TurnID == resumed.TurnID && index.UploadReady(item) {
			t.Fatal("resumed sensitive turn became upload-ready before its completion")
		}
	}
}
