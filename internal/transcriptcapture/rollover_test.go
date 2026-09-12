package transcriptcapture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Headless Cursor fix rounds resume a session id under a new run and never
// emit a prompt or a terminal hook, so the run's thoughts and tool calls carry
// provider turn ids that local state never opens.
const (
	rolloverSession      = "resumed-session"
	rolloverThoughtTurn  = "generation-thought"
	rolloverToolTurn     = "generation-tool"
	rolloverSessionStart = `{"conversation_id":"resumed-session","hook_event_name":"sessionStart","cursor_version":"3.15.6"}`
)

func setupRolloverCapture(t *testing.T, runtime string) {
	t.Helper()
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	saveTestCaptureConfig(t, runtime)
}

func enqueueRolloverRunEvents(t *testing.T) (thought, tool Event) {
	t.Helper()
	thought = enqueueTestHook(t, RuntimeCursor,
		`{"conversation_id":"resumed-session","generation_id":"`+rolloverThoughtTurn+`",`+
			`"hook_event_name":"afterAgentThought","text":"planning the next edit",`+
			`"transcript_path":"/tmp/cursor-resumed.jsonl"}`)
	tool = enqueueTestHook(t, RuntimeCursor,
		`{"conversation_id":"resumed-session","generation_id":"`+rolloverToolTurn+`",`+
			`"hook_event_name":"preToolUse","tool_name":"Shell","tool_use_id":"tool-1",`+
			`"transcript_path":"/tmp/cursor-resumed.jsonl"}`)
	return thought, tool
}

func assertAllPendingReady(t *testing.T, runtime string) []PendingEvent {
	t.Helper()
	pending, err := Pending(runtime)
	if err != nil {
		t.Fatal(err)
	}
	index := NewReadinessIndex(pending)
	for _, item := range pending {
		if !index.UploadReady(item) {
			t.Fatalf("%s %s event remained deferred", item.Event.RunID, item.Event.HookEvent)
		}
	}
	return pending
}

func fenceEventData(t *testing.T, event Event) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(event.Data, &data); err != nil {
		t.Fatalf("fence data = %s, %v", event.Data, err)
	}
	return data
}

func TestRunRolloverFencesPriorRunAndKeepsBothRunsInOrder(t *testing.T) {
	setupRolloverCapture(t, RuntimeCursor)
	start := enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	thought, tool := enqueueRolloverRunEvents(t)
	pending, err := Pending(RuntimeCursor)
	if err != nil || len(pending) != 3 {
		t.Fatalf("first run pending = %d, %v", len(pending), err)
	}
	index := NewReadinessIndex(pending)
	for _, item := range pending[1:] {
		if index.UploadReady(item) {
			t.Fatalf("unfenced %s event was upload-ready", item.Event.HookEvent)
		}
	}

	resumed := enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	if resumed.RunID == start.RunID {
		t.Fatal("resumed session did not bind a new run")
	}
	pending = assertAllPendingReady(t, RuntimeCursor)
	if len(pending) != 6 {
		t.Fatalf("rollover pending = %d, want 6", len(pending))
	}
	order := make([]string, 0, len(pending))
	fencedTurns := make(map[string]Event, 2)
	for _, item := range pending {
		order = append(order, item.Event.RunID)
		event := item.Event
		if event.Kind != "turn.completed" {
			continue
		}
		if event.HookEvent != "Stop" || event.RunID != start.RunID || event.Role != "system" ||
			event.Body != "run completed when the session restarted" ||
			event.SourceTranscriptPath != thought.SourceTranscriptPath {
			t.Fatalf("rollover fence = %#v", event)
		}
		data := fenceEventData(t, event)
		if data["synthetic_fence"] != true || data["reason"] != FenceReasonResumed {
			t.Fatalf("rollover fence data = %s", event.Data)
		}
		fencedTurns[event.TurnID] = event
	}
	if len(fencedTurns) != 2 || fencedTurns[thought.TurnID].ID == "" || fencedTurns[tool.TurnID].ID == "" {
		t.Fatalf("fenced turns = %#v, want one per held turn", fencedTurns)
	}
	want := []string{start.RunID, start.RunID, start.RunID, start.RunID, start.RunID, resumed.RunID}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("capture order = %v, want the prior run and its fences before the resumed run", order)
	}
	state, err := loadSessionState(RuntimeCursor, rolloverSession)
	if err != nil || state.RunID != resumed.RunID || state.SyntheticFencedTurnID != "" || state.PendingFence != nil {
		t.Fatalf("rollover state = %#v, %v", state, err)
	}
}

// The resumed run's own events must also reach the server. Its terminal event
// comes from the launcher, not the runtime, so this is the same fence path.
func TestRunRolloverKeepsCapturingTheResumedRun(t *testing.T) {
	setupRolloverCapture(t, RuntimeCursor)
	enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	enqueueRolloverRunEvents(t)
	resumed := enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	resumedThought, resumedTool := enqueueRolloverRunEvents(t)
	if resumedThought.RunID != resumed.RunID || resumedTool.RunID != resumed.RunID {
		t.Fatalf("resumed run events = %s/%s, want %s", resumedThought.RunID, resumedTool.RunID, resumed.RunID)
	}
	pending, err := Pending(RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	index := NewReadinessIndex(pending)
	deferred := 0
	for _, item := range pending {
		if !index.UploadReady(item) {
			deferred++
			if item.Event.RunID != resumed.RunID {
				t.Fatalf("prior run %s remained deferred after rollover", item.Event.RunID)
			}
		}
	}
	if deferred != 2 {
		t.Fatalf("deferred resumed-run events = %d, want 2", deferred)
	}
	events, err := EnqueueLatestFence(RuntimeCursor, rolloverSession, "job-completed")
	if err != nil || len(events) != 2 {
		t.Fatalf("launcher fence = %d event(s), %v", len(events), err)
	}
	for _, event := range events {
		if event.RunID != resumed.RunID || fenceEventData(t, event)["reason"] != "job-completed" {
			t.Fatalf("launcher fence = %#v", event)
		}
	}
	assertAllPendingReady(t, RuntimeCursor)
}

// Canary for the reported defect: a build that rebinds the run without fencing
// leaves the prior run's events unreachable, and they must flush afterwards.
func TestResumedRunWithoutRolloverFenceStaysDeferredUntilFenced(t *testing.T) {
	setupRolloverCapture(t, RuntimeCursor)
	enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	thought, tool := enqueueRolloverRunEvents(t)
	state, err := loadSessionState(RuntimeCursor, rolloverSession)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly what the old rollover did: bind the resumed run and drop the
	// prior run's turn bookkeeping without publishing a terminal event.
	state.RunID = "run_unfenced_canary"
	state.TurnID = ""
	state.PromptEventID = ""
	if err := saveSessionState(RuntimeCursor, rolloverSession, state); err != nil {
		t.Fatal(err)
	}
	pending, err := Pending(RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	index := NewReadinessIndex(pending)
	for _, item := range pending {
		if item.Event.TurnID != "" && index.UploadReady(item) {
			t.Fatalf("orphaned %s event was upload-ready without a fence", item.Event.HookEvent)
		}
	}
	summary, err := SummarizeDeferred(RuntimeCursor, pending)
	if err != nil || summary.RunMismatch != 2 || summary.NoFence != 0 || summary.SessionUnbound != 0 {
		t.Fatalf("deferred summary = %#v, %v", summary, err)
	}
	// The fence the launcher runs at job completion also recovers a backlog
	// that an older build orphaned.
	events, err := EnqueueLatestFence(RuntimeCursor, rolloverSession, "job-completed")
	if err != nil || len(events) != 2 {
		t.Fatalf("recovery fence = %d event(s), %v", len(events), err)
	}
	for _, event := range events {
		if event.RunID != thought.RunID || event.RunID != tool.RunID {
			t.Fatalf("recovery fence run = %s, want the orphaned run %s", event.RunID, thought.RunID)
		}
	}
	pending = assertAllPendingReady(t, RuntimeCursor)
	summary, err = SummarizeDeferred(RuntimeCursor, pending)
	if err != nil || summary.Total() != 0 {
		t.Fatalf("summary after fencing = %#v, %v", summary, err)
	}
}

func TestEnqueueLatestFenceDerivesIdentityAndIsIdempotent(t *testing.T) {
	setupRolloverCapture(t, RuntimeCursor)
	start := enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	thought, tool := enqueueRolloverRunEvents(t)
	events, err := EnqueueLatestFence(RuntimeCursor, rolloverSession, "job-completed")
	if err != nil || len(events) != 2 {
		t.Fatalf("latest fence = %d event(s), %v", len(events), err)
	}
	turns := map[string]bool{}
	for _, event := range events {
		if event.RunID != start.RunID {
			t.Fatalf("latest fence run = %s, want the bound run %s", event.RunID, start.RunID)
		}
		turns[event.TurnID] = true
	}
	if !turns[thought.TurnID] || !turns[tool.TurnID] {
		t.Fatalf("latest fence turns = %v", turns)
	}
	pending := assertAllPendingReady(t, RuntimeCursor)
	if len(pending) != 5 {
		t.Fatalf("latest fence pending = %d, want 5", len(pending))
	}
	// Nothing is held, so a launcher may call the fence again safely.
	repeat, err := EnqueueLatestFence(RuntimeCursor, rolloverSession, "job-completed")
	if err != nil || len(repeat) != 0 {
		t.Fatalf("repeated latest fence = %d event(s), %v", len(repeat), err)
	}
	// Durable completion outlives the outbox, so an acknowledged upload does
	// not make the launcher fence the same turns again.
	for _, item := range pending {
		if err := RemovePending(item.Path); err != nil {
			t.Fatal(err)
		}
	}
	if again, err := EnqueueLatestFence(RuntimeCursor, rolloverSession, "job-completed"); err != nil || len(again) != 0 {
		t.Fatalf("latest fence after upload = %d event(s), %v", len(again), err)
	}
}

func TestEnqueueFenceExplicitIdentityStillWorksForCursor(t *testing.T) {
	setupRolloverCapture(t, RuntimeCursor)
	start := enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	thought, _ := enqueueRolloverRunEvents(t)
	event, created, err := EnqueueFence(RuntimeCursor, rolloverSession, start.RunID, thought.TurnID, "job-completed")
	if err != nil || !created || event.RunID != start.RunID || event.TurnID != thought.TurnID ||
		event.Kind != "turn.completed" {
		t.Fatalf("explicit cursor fence = %#v, %t, %v", event, created, err)
	}
	pending, err := Pending(RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	index := NewReadinessIndex(pending)
	for _, item := range pending {
		ready := index.UploadReady(item)
		if (item.Event.TurnID == thought.TurnID || item.Event.TurnID == "") != ready {
			t.Fatalf("explicit fence released the wrong turn: %s ready=%t", item.Event.TurnID, ready)
		}
	}
	for _, test := range []struct{ name, run, turn string }{
		{"stale run", "run_other", thought.TurnID},
		{"unheld turn", start.RunID, "generation-unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, created, err := EnqueueFence(RuntimeCursor, rolloverSession, test.run, test.turn, ""); err == nil || created {
				t.Fatalf("refused fence = %t, %v", created, err)
			}
		})
	}
}

func TestFenceRefusesUnknownRuntime(t *testing.T) {
	setupRolloverCapture(t, RuntimeCursor)
	enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	enqueueRolloverRunEvents(t)
	if _, _, err := EnqueueFence("mystery-runtime", rolloverSession, "run_1", "turn_1", ""); err == nil {
		t.Fatal("unknown runtime was fenced")
	}
	if _, err := EnqueueLatestFence("mystery-runtime", rolloverSession, ""); err == nil {
		t.Fatal("unknown runtime was fenced by --latest")
	}
	pending, err := Pending(RuntimeCursor)
	if err != nil || len(pending) != 3 {
		t.Fatalf("refused runtime changed the outbox: %d, %v", len(pending), err)
	}
}

func TestEnqueueLatestFenceClosesGrokBuildWithoutNativeRehydration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	saveTestCaptureConfig(t, RuntimeGrokBuild)
	sessionDir := filepath.Join(home, ".grok", "sessions", "workspace", "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
	if err := os.WriteFile(transcriptPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := enqueueTestHook(t, RuntimeGrokBuild,
		`{"sessionId":"session-1","hookEventName":"user_prompt_submit","promptId":"prompt-1",`+
			`"prompt":"<user_query>\nhello\n</user_query>","transcriptPath":"`+transcriptPath+`"}`)
	events, err := EnqueueLatestFence(RuntimeGrokBuild, "session-1", "job-completed")
	if err != nil || len(events) != 1 || events[0].TurnID != prompt.TurnID || events[0].RunID != prompt.RunID {
		t.Fatalf("grok latest fence = %#v, %v", events, err)
	}
	pending := assertAllPendingReady(t, RuntimeGrokBuild)
	if len(pending) != 2 {
		t.Fatalf("grok pending = %d, want 2", len(pending))
	}
	for _, item := range pending {
		// The companion fence is a completion marker, never an unresolved Grok
		// turn waiting for its native assistant chunks.
		finalized, ready, err := FinalizePending(item)
		if err != nil || !ready || finalized.Event.Body != item.Event.Body {
			t.Fatalf("finalize %s = ready %t, %v", item.Event.HookEvent, ready, err)
		}
	}
}

// The sealed fence is forward-only within a run. Fencing a turn that ran
// before any sealed tool must not rewrite it, and fencing a sealed turn must
// still suppress it even though the runtime has moved on.
func TestRolloverFenceRedactsOnlySealedTurns(t *testing.T) {
	const canary = "rollover-sealed-canary-714"
	setupRolloverCapture(t, RuntimeCursor)
	enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	open := enqueueTestHook(t, RuntimeCursor,
		`{"conversation_id":"resumed-session","generation_id":"generation-open",`+
			`"hook_event_name":"afterAgentThought","text":"planning the next edit",`+
			`"transcript_path":"/tmp/cursor-resumed.jsonl"}`)
	enqueueTestHook(t, RuntimeCursor,
		`{"conversation_id":"resumed-session","generation_id":"generation-sealed",`+
			`"hook_event_name":"preToolUse","tool_name":"witself.secret.reveal",`+
			`"tool_use_id":"tool-secret","tool_input":{"value":"`+canary+`"},`+
			`"transcript_path":"/tmp/cursor-resumed.jsonl"}`)
	sealed := enqueueTestHook(t, RuntimeCursor,
		`{"conversation_id":"resumed-session","generation_id":"generation-sealed",`+
			`"hook_event_name":"afterAgentThought","text":"`+canary+`",`+
			`"transcript_path":"/tmp/cursor-resumed.jsonl"}`)

	enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	pending := assertAllPendingReady(t, RuntimeCursor)
	fences := 0
	for _, item := range pending {
		event := item.Event
		switch {
		case event.Kind == "turn.completed":
			fences++
			if event.Body != "run completed when the session restarted" {
				t.Fatalf("rollover fence body = %q", event.Body)
			}
			wantSealed := event.TurnID == sealed.TurnID
			if eventSealedContentOmitted(event.Data) != wantSealed {
				t.Fatalf("fence for %s sealed = %s", event.TurnID, event.Data)
			}
		case event.ID == open.ID:
			if event.Body != "planning the next edit" || eventSealedContentOmitted(event.Data) {
				t.Fatalf("fencing a later sealed turn rewrote an earlier turn: %#v", event)
			}
		case event.ID == sealed.ID:
			if event.Body == canary || !eventSealedContentOmitted(event.Data) {
				t.Fatalf("sealed turn was released: %#v", event)
			}
		}
	}
	if fences != 2 {
		t.Fatalf("rollover fences = %d, want one per held turn", fences)
	}
}

// A runtime also rebinds the run when it clears a session. That run must be
// closed too, and must not be reported as a resume.
func TestRolloverFenceNamesTheSessionStartSource(t *testing.T) {
	for _, test := range []struct{ source, reason string }{
		{"resume", FenceReasonResumed},
		{"clear", fenceReasonCleared},
	} {
		t.Run(test.source, func(t *testing.T) {
			setupRolloverCapture(t, RuntimeClaudeCode)
			enqueueTestHook(t, RuntimeClaudeCode,
				`{"session_id":"session-1","hook_event_name":"SessionStart","source":"startup",`+
					`"transcript_path":"/tmp/claude-session.jsonl"}`)
			prompt := enqueueTestHook(t, RuntimeClaudeCode,
				`{"session_id":"session-1","hook_event_name":"UserPromptSubmit","prompt":"hello",`+
					`"transcript_path":"/tmp/claude-session.jsonl"}`)
			restart := enqueueTestHook(t, RuntimeClaudeCode,
				`{"session_id":"session-1","hook_event_name":"SessionStart","source":"`+test.source+`",`+
					`"transcript_path":"/tmp/claude-session.jsonl"}`)
			if restart.RunID == prompt.RunID {
				t.Fatal("session start did not rebind the run")
			}
			fenced := 0
			for _, item := range assertAllPendingReady(t, RuntimeClaudeCode) {
				if item.Event.Kind != "turn.completed" {
					continue
				}
				fenced++
				if item.Event.RunID != prompt.RunID || item.Event.TurnID != prompt.TurnID ||
					fenceEventData(t, item.Event)["reason"] != test.reason {
					t.Fatalf("%s fence = %#v", test.source, item.Event)
				}
			}
			if fenced != 1 {
				t.Fatalf("%s fences = %d, want 1", test.source, fenced)
			}
		})
	}
}

// The rollover fence reads the outbox, which flushing already treats as
// best-effort. One unreadable queued file must never stop capture.
func TestRolloverKeepsCapturingWhenTheOutboxCannotBeProjected(t *testing.T) {
	setupRolloverCapture(t, RuntimeCursor)
	start := enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	enqueueRolloverRunEvents(t)
	dir, err := outboxDir(RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(dir, "9999999999999999999-truncated.json")
	if err := os.WriteFile(unreadable, []byte("{\"id\":"), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed := enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	if resumed.RunID == start.RunID || resumed.ID == "" {
		t.Fatalf("hook was dropped by an unreadable outbox file: %#v", resumed)
	}
	if err := os.Remove(unreadable); err != nil {
		t.Fatal(err)
	}
	pending, err := Pending(RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pending {
		if item.Event.Kind == "turn.completed" {
			t.Fatalf("a fence was published from an unprojectable outbox: %#v", item.Event)
		}
	}
	// The prior run is recoverable once the outbox is readable again.
	events, err := EnqueueLatestFence(RuntimeCursor, rolloverSession, "job-completed")
	if err != nil || len(events) != 2 {
		t.Fatalf("recovery fence = %d event(s), %v", len(events), err)
	}
	assertAllPendingReady(t, RuntimeCursor)
}

func TestSummarizeDeferredSeparatesUnboundSessionsFromOpenRuns(t *testing.T) {
	setupRolloverCapture(t, RuntimeCursor)
	enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	enqueueRolloverRunEvents(t)
	pending, err := Pending(RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := SummarizeDeferred(RuntimeCursor, pending)
	if err != nil || summary.NoFence != 2 || summary.RunMismatch != 0 || summary.SessionUnbound != 0 {
		t.Fatalf("open-run summary = %#v, %v", summary, err)
	}
	if err := removeSessionState(RuntimeCursor, rolloverSession); err != nil {
		t.Fatal(err)
	}
	summary, err = SummarizeDeferred(RuntimeCursor, pending)
	if err != nil || summary.SessionUnbound != 2 || summary.NoFence != 0 || summary.RunMismatch != 0 {
		t.Fatalf("unbound summary = %#v, %v", summary, err)
	}
	if _, err := EnqueueLatestFence(RuntimeCursor, rolloverSession, ""); err == nil {
		t.Fatal("a session with no local state was fenced")
	}
}

// Claude Code reports context compaction as a SessionStart in the middle of a
// turn that is still running. Compaction must not close that turn or rebind
// the run: the prompt stays held, so a sealed tool later in the same turn can
// still suppress it before anything is uploaded.
func TestCompactionKeepsTheRunAndSealedTurnStillRedactsThePrompt(t *testing.T) {
	const canary = "compaction-sealed-canary-311"
	setupRolloverCapture(t, RuntimeClaudeCode)
	enqueueTestHook(t, RuntimeClaudeCode,
		`{"session_id":"session-1","hook_event_name":"SessionStart","source":"startup",`+
			`"transcript_path":"/tmp/claude-session.jsonl"}`)
	prompt := enqueueTestHook(t, RuntimeClaudeCode,
		`{"session_id":"session-1","hook_event_name":"UserPromptSubmit","prompt":"`+canary+`",`+
			`"transcript_path":"/tmp/claude-session.jsonl"}`)
	compact := enqueueTestHook(t, RuntimeClaudeCode,
		`{"session_id":"session-1","hook_event_name":"SessionStart","source":"compact",`+
			`"transcript_path":"/tmp/claude-session.jsonl"}`)
	if compact.RunID != prompt.RunID || compact.TurnID != "" {
		t.Fatalf("compaction rebound the run: %#v", compact)
	}
	state, err := loadSessionState(RuntimeClaudeCode, "session-1")
	if err != nil || state.RunID != prompt.RunID || state.TurnID != prompt.TurnID {
		t.Fatalf("compaction changed the open turn: %#v, %v", state, err)
	}
	pending, err := Pending(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	index := NewReadinessIndex(pending)
	for _, item := range pending {
		if item.Event.Kind == "turn.completed" {
			t.Fatalf("compaction fenced the open turn: %#v", item.Event)
		}
		if item.Event.ID == prompt.ID && index.UploadReady(item) {
			t.Fatal("compaction released the open turn's prompt")
		}
	}
	enqueueTestHook(t, RuntimeClaudeCode,
		`{"session_id":"session-1","hook_event_name":"PreToolUse","tool_name":"witself.secret.reveal",`+
			`"tool_use_id":"tool-secret","tool_input":{"value":"`+canary+`"},`+
			`"transcript_path":"/tmp/claude-session.jsonl"}`)
	stop := enqueueTestHook(t, RuntimeClaudeCode,
		`{"session_id":"session-1","hook_event_name":"Stop","last_assistant_message":"`+canary+`",`+
			`"transcript_path":"/tmp/claude-session.jsonl"}`)
	if stop.RunID != prompt.RunID || stop.TurnID != prompt.TurnID {
		t.Fatalf("stop closed a different turn: %#v", stop)
	}
	for _, item := range assertAllPendingReady(t, RuntimeClaudeCode) {
		if strings.Contains(item.Event.Body, canary) || strings.Contains(string(item.Event.Data), canary) {
			t.Fatalf("sealed turn leaked across compaction: %#v", item.Event)
		}
		if item.Event.ID == prompt.ID && !eventSealedContentOmitted(item.Event.Data) {
			t.Fatalf("pre-compaction prompt was not redacted: %#v", item.Event)
		}
	}
}

// Pending never fails because a concurrent flush acknowledged and removed a
// queued file between the outbox listing and the read.
func TestPendingSkipsAFileRemovedDuringTheScan(t *testing.T) {
	setupRolloverCapture(t, RuntimeCursor)
	enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	enqueueRolloverRunEvents(t)
	dir, err := outboxDir(RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	// A dangling symlink is exactly what the reader sees when a file vanishes
	// after the glob: the name is listed and the read reports ErrNotExist.
	if err := os.Symlink(filepath.Join(dir, "gone.json.removed"), filepath.Join(dir, "0000000000000000000-gone.json")); err != nil {
		t.Fatal(err)
	}
	pending, err := Pending(RuntimeCursor)
	if err != nil || len(pending) != 3 {
		t.Fatalf("pending = %d, %v; want the three real events", len(pending), err)
	}
}

// The hook path clears the session's sealed flag at the next real prompt. A
// fence must follow that per-turn decision: a turn captured after the reset is
// closed unsealed and its content is never rewritten, even though an earlier
// sealed turn of the same run still sits in the outbox.
func TestFenceKeepsUnsealedTurnsCapturedAfterAPromptReset(t *testing.T) {
	const canary = "prompt-reset-canary-522"
	setupRolloverCapture(t, RuntimeCursor)
	enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	sealedPrompt := enqueueTestHook(t, RuntimeCursor,
		`{"conversation_id":"resumed-session","generation_id":"generation-two",`+
			`"hook_event_name":"beforeSubmitPrompt","prompt":"store `+canary+`",`+
			`"transcript_path":"/tmp/cursor-resumed.jsonl"}`)
	enqueueTestHook(t, RuntimeCursor,
		`{"conversation_id":"resumed-session","generation_id":"generation-two",`+
			`"hook_event_name":"preToolUse","tool_name":"witself.secret.create",`+
			`"tool_use_id":"tool-secret","tool_input":{"value":"`+canary+`"},`+
			`"transcript_path":"/tmp/cursor-resumed.jsonl"}`)
	enqueueTestHook(t, RuntimeCursor,
		`{"conversation_id":"resumed-session","generation_id":"generation-two",`+
			`"hook_event_name":"afterAgentResponse","text":"stored",`+
			`"transcript_path":"/tmp/cursor-resumed.jsonl"}`)
	plainPrompt := enqueueTestHook(t, RuntimeCursor,
		`{"conversation_id":"resumed-session","generation_id":"generation-three",`+
			`"hook_event_name":"beforeSubmitPrompt","prompt":"plain three",`+
			`"transcript_path":"/tmp/cursor-resumed.jsonl"}`)
	plainTool := enqueueTestHook(t, RuntimeCursor,
		`{"conversation_id":"resumed-session","generation_id":"generation-four",`+
			`"hook_event_name":"preToolUse","tool_name":"Shell","tool_use_id":"tool-ls",`+
			`"tool_input":{"command":"ls -la"},"transcript_path":"/tmp/cursor-resumed.jsonl"}`)
	if eventSealedContentOmitted(plainTool.Data) || plainTool.Body == "" {
		t.Fatalf("tool captured after the reset was not plain: %#v", plainTool)
	}
	events, err := EnqueueLatestFence(RuntimeCursor, rolloverSession, "job-completed")
	if err != nil || len(events) != 2 {
		t.Fatalf("latest fence = %d event(s), %v", len(events), err)
	}
	for _, fence := range events {
		if fence.TurnID == plainTool.TurnID && eventSealedContentOmitted(fence.Data) {
			t.Fatalf("fence sealed a turn captured after the prompt reset: %#v", fence)
		}
	}
	for _, item := range assertAllPendingReady(t, RuntimeCursor) {
		event := item.Event
		switch event.ID {
		case plainTool.ID:
			if event.Body != plainTool.Body || eventSealedContentOmitted(event.Data) {
				t.Fatalf("fence rewrote a turn no sealed tool touched: %#v", event)
			}
		case plainPrompt.ID:
			if event.Body != "plain three" || eventSealedContentOmitted(event.Data) {
				t.Fatalf("fence rewrote the prompt that reset the seal: %#v", event)
			}
		case sealedPrompt.ID:
			if strings.Contains(event.Body, canary) || !eventSealedContentOmitted(event.Data) {
				t.Fatalf("sealed turn was released: %#v", event)
			}
		}
	}
}

// A sealed hook whose redaction fails leaves the turn's earlier plaintext in
// the outbox with no sealed marker. The fence must still close that turn
// sealed and retry the redaction, never release the plaintext.
func TestFenceRetriesASealedRedactionThatFailedAtCaptureTime(t *testing.T) {
	const canary = "failed-redaction-canary-808"
	setupRolloverCapture(t, RuntimeCursor)
	enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	thought := enqueueTestHook(t, RuntimeCursor,
		`{"conversation_id":"resumed-session","generation_id":"generation-a",`+
			`"hook_event_name":"afterAgentThought","text":"`+canary+`",`+
			`"transcript_path":"/tmp/cursor-resumed.jsonl"}`)
	pending, err := Pending(RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	var thoughtPath string
	for _, item := range pending {
		if item.Event.ID == thought.ID {
			thoughtPath = item.Path
		}
	}
	if thoughtPath == "" {
		t.Fatal("thought event not queued")
	}
	// Make the queued file untrusted for a rewrite, so the sealed hook's
	// redaction fails exactly as a local I/O problem would make it fail.
	if err := os.Rename(thoughtPath, thoughtPath+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(thoughtPath+".real", thoughtPath); err != nil {
		t.Fatal(err)
	}
	if _, err := EnqueueHook(RuntimeCursor,
		[]byte(`{"conversation_id":"resumed-session","generation_id":"generation-a",`+
			`"hook_event_name":"preToolUse","tool_name":"witself.secret.reveal",`+
			`"tool_use_id":"tool-secret","tool_input":{"value":"`+canary+`"},`+
			`"transcript_path":"/tmp/cursor-resumed.jsonl"}`)); err == nil {
		t.Fatal("sealed hook succeeded although its redaction could not be written")
	}
	if err := os.Remove(thoughtPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(thoughtPath+".real", thoughtPath); err != nil {
		t.Fatal(err)
	}
	state, err := loadSessionState(RuntimeCursor, rolloverSession)
	if err != nil || !state.SensitiveTurn || !state.sealedTurnRecorded(thought.RunID, thought.TurnID) {
		t.Fatalf("failed redaction did not record the sealed turn: %#v, %v", state, err)
	}
	later := enqueueTestHook(t, RuntimeCursor,
		`{"conversation_id":"resumed-session","generation_id":"generation-b",`+
			`"hook_event_name":"afterAgentThought","text":"after the seal",`+
			`"transcript_path":"/tmp/cursor-resumed.jsonl"}`)
	if !eventSealedContentOmitted(later.Data) {
		t.Fatalf("hook after the seal was not suppressed: %#v", later)
	}
	events, err := EnqueueLatestFence(RuntimeCursor, rolloverSession, "job-completed")
	if err != nil || len(events) != 2 {
		t.Fatalf("latest fence = %d event(s), %v", len(events), err)
	}
	for _, fence := range events {
		if !eventSealedContentOmitted(fence.Data) {
			t.Fatalf("fence closed a sealed turn unsealed: %#v", fence)
		}
	}
	for _, item := range assertAllPendingReady(t, RuntimeCursor) {
		if strings.Contains(item.Event.Body, canary) || strings.Contains(string(item.Event.Raw), canary) {
			t.Fatalf("plaintext of the sealing turn was released: %#v", item.Event)
		}
		if item.Event.ID == thought.ID && !eventSealedContentOmitted(item.Event.Data) {
			t.Fatalf("fence did not retry the failed redaction: %#v", item.Event)
		}
	}
}

// A runtime's own SessionEnd publishes its terminal and removes local state.
// The launcher's unconditional fence after a clean exit must then be a no-op.
func TestEnqueueLatestFenceIsANoOpAfterSessionEnd(t *testing.T) {
	setupRolloverCapture(t, RuntimeCursor)
	enqueueTestHook(t, RuntimeCursor, rolloverSessionStart)
	enqueueRolloverRunEvents(t)
	enqueueTestHook(t, RuntimeCursor,
		`{"conversation_id":"resumed-session","hook_event_name":"sessionEnd"}`)
	if _, err := os.Stat(mustSessionStatePath(t, RuntimeCursor, rolloverSession)); !os.IsNotExist(err) {
		t.Fatalf("session end kept local state: %v", err)
	}
	assertAllPendingReady(t, RuntimeCursor)
	events, err := EnqueueLatestFence(RuntimeCursor, rolloverSession, "job-completed")
	if err != nil || len(events) != 0 {
		t.Fatalf("fence after session end = %d event(s), %v; want a no-op", len(events), err)
	}
}

func mustSessionStatePath(t *testing.T, runtime, sessionID string) string {
	t.Helper()
	path, err := sessionStatePath(runtime, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
