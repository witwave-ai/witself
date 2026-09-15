//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

// This helper runs the actual detached flush in a separate process, just as a
// Stop hook does. The only token it can access is the generated fixture token.
func TestDSHNativeRetryFlushHelper(t *testing.T) {
	directory := os.Getenv("WITSELF_DSH_RETRY_HELPER_DIRECTORY")
	if directory == "" {
		t.Setenv("DSH_HOME", t.TempDir())
		t.Setenv("WITSELF_HOME", t.TempDir())
		return
	}
	if os.Getenv("DSH_HOME") == "" || os.Getenv("WITSELF_HOME") == "" || os.Getenv(captureDetachedFlushEnv) != "1" {
		t.Fatal("isolated detached dsh homes are required")
	}
	if err := os.WriteFile(filepath.Join(directory, "started"), []byte("yes"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeDSH})
	if err := os.WriteFile(filepath.Join(directory, "finished"), []byte{byte('0' + code)}, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDSHDetachedFlushRetriesSlowFollowingStopHook(t *testing.T) {
	var appended []string
	srv := startCaptureLedgerServer(t, &appended)
	dshHome := configureDSHCaptureFlushTest(t, srv.URL)
	const cwd = "/tmp/project"
	const session = "dsh-slow-stop"
	directory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITSELF_DSH_RETRY_HELPER_DIRECTORY", directory)
	t.Setenv("WITSELF_DSH_RETRY_TEST_BINARY", executable)
	wrapper := filepath.Join(directory, "flush-helper")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec \"$WITSELF_DSH_RETRY_TEST_BINARY\" -test.run='^TestDSHNativeRetryFlushHelper$' -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(witselfExecutableTestEnv, wrapper)

	// The prompt, a failed model attempt, nested tool result, and final answer
	// are already durable; only turn/end is held by the following Stop hook.
	lines := []string{
		mustDSHTestJSON(map[string]any{"type": "session", "version": 3, "id": session, "cwd": cwd, "delegationDepth": 0, "isSeeded": false}),
		mustDSHTestEvent(1, "turn/start", map[string]any{"turn": 1}),
		mustDSHTestEvent(2, "step/start", map[string]any{"turn": 1, "step": 1}),
		mustDSHTestEvent(3, "user/message", map[string]any{"id": "prompt", "role": "user", "source": map[string]any{"kind": "user"}, "content": []any{map[string]any{"type": "text", "text": "finish the task"}}}),
		mustDSHTestEvent(4, "assistant/attempt", map[string]any{"turn": 1, "step": 1, "error": map[string]any{"name": "provider failure"}}),
		mustDSHTestEvent(5, "tool/call", map[string]any{"turn": 1, "step": 1, "callId": "tool-1", "name": "read_file", "arguments": `{"path":"fixture.txt"}`}),
		mustDSHTestEvent(6, "tool/result", map[string]any{
			"turn": 1, "step": 1,
			"message": map[string]any{
				"role": "tool", "source": map[string]any{"kind": "tool", "callId": "tool-1"},
				"content": []any{map[string]any{
					"type": "tool-result", "content": []any{map[string]any{"type": "text", "text": "fixture contents"}},
				}},
			},
		}),
		mustDSHTestEvent(7, "assistant/message", map[string]any{"turn": 1, "step": 1, "message": map[string]any{"id": "answer", "role": "assistant", "source": map[string]any{"kind": "model", "provider": "deepseek-official", "model": "deepseek-flash"}, "content": []any{map[string]any{"type": "reasoning", "text": "private synthetic reasoning"}, map[string]any{"type": "text", "text": "The delayed answer is complete."}}}}),
		mustDSHTestEvent(8, "step/end", map[string]any{"turn": 1, "step": 1}),
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = encoder.Close() }()
	var content []byte
	for _, line := range lines {
		content = append(content, encoder.EncodeAll([]byte(line+"\n"), nil)...)
	}
	logDir := filepath.Join(dshHome, "sessions", dshTestProjectKey(cwd), session)
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "session.v3.jsonl.zstd")
	if err := os.WriteFile(logPath, encoder.EncodeAll([]byte(lines[0]+"\n"), nil), 0o600); err != nil {
		t.Fatal(err)
	}
	enqueueDSHFlushHook(t, map[string]any{"session_id": session, "hook_event_name": "UserPromptSubmit", "cwd": cwd, "prompt": "finish the task"})
	// UserPromptSubmit is synchronous and precedes the native user/message.
	// Publish the already completed work only after that prompt hook returns.
	if err := os.WriteFile(logPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	enqueueDSHFlushHook(t, map[string]any{"session_id": session, "hook_event_name": "Stop", "cwd": cwd})
	if err := startBackgroundFlush(transcriptcapture.RuntimeDSH); err != nil {
		t.Fatal(err)
	}
	readCaptureFlushTestFile(t, filepath.Join(directory, "started"))
	// dsh runs a three-second Stop command after Witself's hook. No later
	// event enters the outbox, and the session is idle once this fence lands.
	time.Sleep(3 * time.Second)
	file, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fence := mustDSHTestEvent(9, "turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	_, writeErr := file.Write(encoder.EncodeAll([]byte(fence+"\n"), nil))
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("append synthetic native fence failed")
	}
	if got := string(readCaptureFlushTestFile(t, filepath.Join(directory, "finished"))); got != "0" {
		t.Fatal("detached flush stopped before the delayed native completion")
	}
	srv.Close()
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeDSH)
	if err != nil || len(pending) != 0 {
		t.Fatal("idle session retained its completed Stop instead of retrying")
	}
	if !slices.Contains(appended, "The delayed answer is complete.") {
		t.Fatal("detached retry did not upload the answer")
	}
}

func TestDSHStopDefersFlushUntilSynchronousHookReturns(t *testing.T) {
	directory := t.TempDir()
	configureDSHCaptureFlushTest(t, "http://127.0.0.1:1")
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "")
	t.Setenv("WITSELF_CURATOR_SESSION", "")
	observation := filepath.Join(directory, "flush-observed")
	t.Setenv("DSH_FLUSH_TEST_FILE", observation)
	wrapper := filepath.Join(directory, "witself-flush-recorder")
	script := "#!/bin/sh\nif [ \"$WITSELF_CAPTURE_DETACHED_FLUSH\" = \"1\" ]; then\n  printf 'detached\\n' >> \"$DSH_FLUSH_TEST_FILE\"\nelse\n  printf 'foreground\\n' >> \"$DSH_FLUSH_TEST_FILE\"\nfi\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(witselfExecutableTestEnv, wrapper)
	input, err := os.CreateTemp(directory, "dsh-stop-input")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	if _, err := input.WriteString(`{"session_id":"dsh-detached-stop","cwd":"/tmp/project","hook_event_name":"Stop"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	priorStdin := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = priorStdin })
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptHook([]string{"--runtime", transcriptcapture.RuntimeDSH})
	})
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatal("dsh Stop hook failed")
	}
	if got := string(readCaptureFlushTestFile(t, observation)); got != "detached\n" {
		t.Fatal("dsh Stop started a foreground flusher before returning")
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeDSH)
	if err != nil || len(pending) != 1 || pending[0].Event.NativeTurnFinalized || pending[0].Event.Kind != "turn.completed" {
		t.Fatal("dsh Stop was lost or finalized before the native fence")
	}
}
