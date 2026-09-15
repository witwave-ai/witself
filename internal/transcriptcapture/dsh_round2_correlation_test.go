package transcriptcapture

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDSHRound2SealSurvivesStopSegments(t *testing.T) {
	for _, continuation := range []string{"plugin", "goal", "user"} {
		t.Run(continuation, func(t *testing.T) {
			dshTestCaptureConfig(t, ModeTrace)
			const session, cwd, canary = "round2-segments", "/tmp/project", "round2-sealed-copy"
			log := dshBridgeClaim(newDSHLog(session, cwd).event("turn/start", map[string]any{"turn": 1}), "next-turn", "first", "request", "user", 1)
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "request"})
			log.event("step/start", map[string]any{"turn": 1, "step": 1}).prompt("request").
				assistant(1, 1, "checking", "deepseek-flash", map[string]any{"inputTokens": 11}).
				toolCall(1, 1, "reveal", "witself.secret.reveal", `{}`).toolResult(1, 1, "reveal", canary).
				event("step/end", map[string]any{"turn": 1, "step": 1}).
				event("hook/invoked", map[string]any{"turn": 1, "point": "Stop", "dialect": "claude-code", "handlerId": "first-stop"})
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			first := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
			log.event("hook/result", map[string]any{"turn": 1, "point": "Stop", "handlerId": "first-stop"})
			dshBridgeClaim(log, "next-step", "continuation", "continue", continuation, 1)
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "continue"})
			log.event("step/start", map[string]any{"turn": 1, "step": 2}).
				event("user/message", dshBridgeMessage("continuation", "continue", continuation)).
				assistant(1, 2, canary, "deepseek-flash", map[string]any{"inputTokens": 17}).
				event("step/end", map[string]any{"turn": 1, "step": 2}).
				event("hook/invoked", map[string]any{"turn": 1, "point": "Stop", "dialect": "claude-code", "handlerId": "second-stop"})
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			second := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
			log.event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			for i, event := range []Event{first, second} {
				final, ready, err := finalizePendingWithin(findPendingDSHEvent(t, event.ID), 0, time.Millisecond)
				if err != nil || !ready {
					t.Fatal("completed Stop could not finalize")
				}
				encoded, err := json.Marshal(final.Event.Entries())
				wantVisible := i == 1 && continuation == "user"
				if err != nil || strings.Contains(string(encoded), canary) != wantVisible {
					t.Fatal("native seal did not persist until the next proven human boundary")
				}
			}
		})
	}
}

func TestDSHRound2RepeatedUnanchoredPromptsKeepOccurrences(t *testing.T) {
	dshTestCaptureConfig(t, ModeTrace)
	const session, cwd = "round2-repeat", "/tmp/project"
	log := newDSHLog(session, cwd)
	var stops []Event
	for turn := 1; turn <= 2; turn++ {
		dshBridgeClaim(log.event("turn/start", map[string]any{"turn": turn}), "next-turn", "prompt", "continue", "user", turn)
		writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
		enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "continue"})
		if turn == 1 {
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "SessionStart", "source": "startup"})
		}
		answer, model := "first answer", "first-model"
		if turn == 2 {
			answer, model = "second answer", "second-model"
		}
		log.event("step/start", map[string]any{"turn": turn, "step": 1}).prompt("continue").
			assistant(turn, 1, answer, model, map[string]any{"inputTokens": turn * 13}).
			event("step/end", map[string]any{"turn": turn, "step": 1}).
			event("hook/invoked", map[string]any{"turn": turn, "point": "Stop", "dialect": "claude-code", "handlerId": "stop"})
		writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
		stops = append(stops, enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"}))
		log.event("turn/end", map[string]any{"turn": turn, "reason": map[string]any{"kind": "completed"}})
	}
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	for index, stop := range stops {
		// Reload each persisted event after both turns finished, as a detached
		// process does. Session state now describes only the second prompt.
		final, ready, err := finalizePendingWithin(findPendingDSHEvent(t, stop.ID), 0, time.Millisecond)
		var data struct {
			Usage struct {
				Input int `json:"input_tokens"`
			} `json:"usage"`
		}
		answer, model := "first answer", "first-model"
		if index == 1 {
			answer, model = "second answer", "second-model"
		}
		if err != nil || !ready || final.Event.Body != answer || final.Event.Model != model ||
			json.Unmarshal(final.Event.Data, &data) != nil || data.Usage.Input != (index+1)*13 {
			t.Fatal("repeated unanchored Stop borrowed another occurrence's answer, model or usage")
		}
	}
}

func TestDSHRound2AmbiguousDigestStaysPending(t *testing.T) {
	dshTestCaptureConfig(t, ModeMessages)
	const session, cwd = "round2-ambiguous", "/tmp/project"
	log := newDSHLog(session, cwd)
	appendDSHCompletedPrompt(log, 1, "continue", "first answer", 13)
	appendDSHCompletedPrompt(log, 2, "continue", "second answer", 26)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	turn, err := readDSHSessionTurn(session, cwd, 0, dshTurnCorrelation{PromptSHA256: dshPromptSHA256("continue")})
	if err != nil || turn.Complete || turn.Body != "" || turn.Model != "" || !turn.Usage.empty() {
		t.Fatal("ambiguous digest guessed an occurrence instead of remaining pending")
	}
}

func TestDSHRound2UnpublishedPromptWaitsForNativeBatch(t *testing.T) {
	dshTestCaptureConfig(t, ModeMessages)
	const session, cwd = "round2-unpublished", "/tmp/project"
	log := newDSHLog(session, cwd)
	appendDSHCompletedPrompt(log, 1, "old prompt", "old answer", 13)
	path := writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "new prompt"})
	stop := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
	pending := findPendingDSHEvent(t, stop.ID)
	if _, ready, err := finalizePendingWithin(pending, 0, time.Millisecond); err != nil || ready {
		t.Fatal("a stale native prefix permanently finalized an unpublished prompt")
	}
	appendDSHCompletedPrompt(log, 2, "new prompt", "new answer", 29)
	raw := log.plain()
	appended := make(chan error, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		appended <- os.WriteFile(path, raw, 0o600)
	}()
	final, ready, err := finalizePendingWithin(findPendingDSHEvent(t, stop.ID), time.Second, time.Millisecond)
	appendErr := <-appended
	if err != nil || appendErr != nil || !ready || final.Event.Body != "new answer" {
		t.Fatal("polling did not recover the newly published prompt and answer")
	}
}

func TestDSHRound2RepeatedPromptCannotUseAlreadyDurablePredecessor(t *testing.T) {
	dshTestCaptureConfig(t, ModeMessages)
	const session, cwd = "round2-stale-repeat", "/tmp/project"
	log := newDSHLog(session, cwd)
	appendDSHCompletedPrompt(log, 1, "continue", "old answer", 13)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "continue"})
	stop := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
	if _, ready, err := finalizePendingWithin(findPendingDSHEvent(t, stop.ID), 0, time.Millisecond); err != nil || ready {
		t.Fatal("new repeated prompt selected a native occurrence durable before its hook")
	}
	appendDSHCompletedPrompt(log, 2, "continue", "new answer", 29)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	final, ready, err := finalizePendingWithin(findPendingDSHEvent(t, stop.ID), 0, time.Millisecond)
	if err != nil || !ready || final.Event.Body != "new answer" {
		t.Fatal("native record watermark did not select the newly published occurrence")
	}
}

func TestDSHRound2UnresolvedTerminalRequiresExpiredDurableWindow(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		name := "unpublished"
		if ambiguous {
			name = "ambiguous"
		}
		t.Run(name, func(t *testing.T) {
			dshTestCaptureConfig(t, ModeMessages)
			const session, cwd = "round2-terminal", "/tmp/project"
			log := newDSHLog(session, cwd)
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "continue"})
			stop := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
			if ambiguous {
				appendDSHCompletedPrompt(log, 1, "continue", "first answer", 13)
				appendDSHCompletedPrompt(log, 2, "continue", "second answer", 26)
				writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			}
			pending, ready, err := finalizePendingWithin(findPendingDSHEvent(t, stop.ID), 0, time.Millisecond)
			if err != nil || ready || pending.Event.NativeTurnFinalized {
				t.Fatal("unresolved Stop settled before waiting")
			}
			// Shorten only the test's durable budget, then let the actual reader
			// consume it. Zero-wait reads and process reloads cannot expire it.
			deadline := time.Now().UTC().Add(50 * time.Millisecond)
			pending, err = persistDSHNativeRetry(pending, dshNativeRetryState{Deadline: deadline, At: time.Now().UTC()})
			if err != nil {
				t.Fatal("persist test retry budget")
			}
			final, ready, err := finalizePendingWithin(findPendingDSHEvent(t, stop.ID), time.Second, time.Millisecond)
			var details map[string]any
			if err != nil || !ready || time.Now().Before(deadline) || !final.Event.NativeTurnFinalized ||
				json.Unmarshal(final.Event.Data, &details) != nil || details["dsh_native_finalization"] != "unresolved_prompt" ||
				eventSealedContentOmitted(final.Event.Data) || final.Event.Body != "" || final.Event.Model != "" ||
				len(final.Event.RecoveredMessages) != 0 || details["usage"] != nil {
				t.Fatal("unresolved terminal did not reflect an expired, value-free native publication failure")
			}
			encoded, err := json.Marshal(final.Event.Entries())
			if err != nil || strings.Contains(string(encoded), "dsh_native_retry") || strings.Contains(string(encoded), "dsh_prompt_") {
				t.Fatal("private native correlation or retry state reached upload entries")
			}
		})
	}
}
