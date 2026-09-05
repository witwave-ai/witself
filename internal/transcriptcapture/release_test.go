package transcriptcapture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func releaseTestEvent(t *testing.T, cfg Config, session, run, turn, hook, kind, body string, data json.RawMessage, at time.Time) Event {
	t.Helper()
	event := Event{
		SchemaVersion: SchemaVersion, ID: "evt_" + session + "_" + turn + "_" + hook,
		Runtime: cfg.Runtime, CaptureMode: cfg.CaptureMode, Account: cfg.Account, AccountID: cfg.AccountID,
		Realm: cfg.Realm, RealmID: cfg.RealmID, Agent: cfg.Agent, AgentID: cfg.AgentID, AgentName: cfg.AgentName,
		Location: cfg.Location, SessionID: session, RunID: run, TurnID: turn, HookEvent: hook,
		NativeHookEvent: hook, Kind: kind, Role: "tool", Body: body, Data: data,
		SourceTranscriptPath: "/tmp/operator-release-rollout.jsonl", OccurredAt: at,
		Raw: json.RawMessage(`{"tool_response":"release-tool-canary"}`),
	}
	if kind == "message.user" {
		event.Role = "user"
	} else if kind == "message.assistant" {
		event.Role = "assistant"
	}
	if err := writeOutboxEvent(event); err != nil {
		t.Fatal(err)
	}
	return event
}

func releaseFileSnapshot(t *testing.T) map[string]string {
	t.Helper()
	out := make(map[string]string)
	root := os.Getenv("WITSELF_HOME")
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReleaseResidueListsWithoutMutationAndRedactsEveryResult(t *testing.T) {
	for _, runtime := range []string{RuntimeCodex, RuntimeClaudeCode, RuntimeGrokBuild, RuntimeCursor} {
		t.Run(runtime, func(t *testing.T) {
			cfg := setupFenceCapture(t, ModeRaw)
			cfg.Runtime = runtime
			if err := SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Add(-48 * time.Hour)
			payload, _ := json.Marshal(map[string]string{"result": "release-tool-canary" + strings.Repeat("z", 8192)})
			data, _ := json.Marshal(map[string]any{"tool": map[string]any{"name": "browser", "use_id": "call", "output": boundedJSON(payload)}})
			releaseTestEvent(t, cfg, "residue", "run", "turn", "UserPromptSubmit", "message.user", "preserve this prompt", nil, at)
			releaseTestEvent(t, cfg, "residue", "run", "turn", "PreToolUse", "tool.call", toolBody("browser", "call", payload), data, at.Add(time.Second))
			releaseTestEvent(t, cfg, "residue", "run", "turn", "PostToolUse", "tool.result", toolBody("browser", "call", payload), data, at.Add(2*time.Second))
			releaseTestEvent(t, cfg, "residue", "run", "turn", "PartialMessage", "message.assistant", "preserve this assistant message", nil, at.Add(3*time.Second))
			before := releaseFileSnapshot(t)
			sessions, err := ListResidue(runtime, "")
			want := []ResidueSession{{SessionID: "residue", EventCount: 4, FirstEventAt: at, LastEventAt: at.Add(3 * time.Second), HasToolResult: true}}
			if err != nil || !reflect.DeepEqual(sessions, want) {
				t.Fatalf("residue = %#v, %v", sessions, err)
			}
			if after := releaseFileSnapshot(t); !reflect.DeepEqual(before, after) {
				t.Fatal("residue listing mutated local files")
			}
			stale, _ := Pending(runtime)
			if count, err := ReleaseResidue(runtime, "residue", 24*time.Hour, false); err != nil || count != 1 {
				t.Fatalf("release = %d, %v", count, err)
			}
			pending, err := Pending(runtime)
			if err != nil || len(pending) != 5 {
				t.Fatalf("released pending = %d, %v", len(pending), err)
			}
			sum := sha256.Sum256(payload)
			index := NewReadinessIndex(pending)
			for _, item := range pending {
				raw, _ := os.ReadFile(item.Path)
				if bytes.Contains(raw, []byte("release-tool-canary")) || !index.UploadReady(item) {
					t.Fatalf("unsafe/unready released event %s", item.Event.Kind)
				}
				switch item.Event.Kind {
				case "message.user":
					if item.Event.Body != "preserve this prompt" {
						t.Fatal("prompt changed")
					}
				case "message.assistant":
					if item.Event.Body != "preserve this assistant message" {
						t.Fatal("assistant changed")
					}
				case "tool.call":
					if item.Event.Body != toolBody("browser", "call", nil) {
						t.Fatal("tool call name lost")
					}
				case "tool.result":
					var placeholder releasedPayload
					if json.Unmarshal([]byte(item.Event.Body), &placeholder) != nil || placeholder.Redacted != "released_without_fence" || placeholder.OriginalBytes != len(payload) || placeholder.SHA256 != hex.EncodeToString(sum[:]) {
						t.Fatalf("wrong original result digest/placeholder: %s", item.Event.Body)
					}
				case OperatorReleaseKind:
					if item.Event.HookEvent != "OperatorRelease" || !eventOperatorReleased(item.Event.Data) {
						t.Fatal("missing operator marker")
					}
					if err := RemovePending(item.Path); err != nil {
						t.Fatal(err)
					}
				}
			}
			// Durable completion survives the terminal's acknowledgement, while
			// snapshots captured before redaction remain held.
			for _, item := range stale {
				if PendingEventUploadReady(item, stale) {
					t.Fatal("operator release admitted stale plaintext")
				}
			}
			if sessions, err := ListResidue(runtime, ""); err != nil || len(sessions) != 0 {
				t.Fatalf("completed residue = %#v, %v", sessions, err)
			}
			before = releaseFileSnapshot(t)
			if count, err := ReleaseResidue(runtime, "residue", 0, true); err != nil || count != 0 {
				t.Fatalf("repeat = %d, %v", count, err)
			}
			if !reflect.DeepEqual(before, releaseFileSnapshot(t)) {
				t.Fatal("repeat changed released events")
			}
		})
	}
}

func TestReleaseResidueAgeForceAndFencedTurnsUntouched(t *testing.T) {
	cfg := setupFenceCapture(t, ModeRaw)
	at := time.Now().UTC()
	releaseTestEvent(t, cfg, "fresh", "run", "open", "UserPromptSubmit", "message.user", "fresh", nil, at)
	releaseTestEvent(t, cfg, "fenced", "run", "closed", "UserPromptSubmit", "message.user", "fenced prompt", nil, at.Add(-48*time.Hour))
	releaseTestEvent(t, cfg, "fenced", "run", "closed", "Stop", "turn.completed", "closed", nil, at.Add(-47*time.Hour))
	// A late result on a runtime-fenced identity is not release residue.
	releaseTestEvent(t, cfg, "fenced", "run", "closed", "PostToolUse", "tool.result", "fenced late result", nil, at.Add(-46*time.Hour))
	before := releaseFileSnapshot(t)
	if count, err := ReleaseResidue(RuntimeCodex, "fresh", 24*time.Hour, false); err == nil || count != 0 {
		t.Fatalf("fresh release = %d, %v", count, err)
	}
	// Session-lock creation is harmless; outbox content must stay unchanged.
	for path, raw := range before {
		if after, err := os.ReadFile(path); err != nil || string(after) != raw {
			t.Fatalf("refusal changed %s", path)
		}
	}
	if count, err := ReleaseResidue(RuntimeCodex, "", 24*time.Hour, false); err != nil || count != 0 {
		t.Fatalf("all fresh = %d, %v", count, err)
	}
	if count, err := ReleaseResidue(RuntimeCodex, "fresh", 24*time.Hour, true); err != nil || count != 1 {
		t.Fatalf("forced release = %d, %v", count, err)
	}
	for path, raw := range before {
		if strings.Contains(filepath.Base(path), "evt_fenced_") {
			if after, err := os.ReadFile(path); err != nil || string(after) != raw {
				t.Fatalf("release touched fenced event %s", path)
			}
		}
	}
}

func TestReleaseResidueRetriesInterruptedCompletionAfterLateRuntimeFence(t *testing.T) {
	cfg := setupFenceCapture(t, ModeRaw)
	at := time.Now().UTC().Add(-48 * time.Hour)
	prompt := releaseTestEvent(t, cfg, "retry", "run", "turn", "UserPromptSubmit", "message.user", "retry prompt", nil, at)
	marker, err := completedFencePath(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(marker, 0700); err != nil {
		t.Fatal(err)
	}
	if count, err := ReleaseResidue(RuntimeCodex, "retry", 0, true); err == nil || count != 0 {
		t.Fatalf("blocked completion = %d, %v", count, err)
	}
	pending, _ := Pending(RuntimeCodex)
	for _, item := range pending {
		if NewReadinessIndex(pending).UploadReady(item) {
			t.Fatal("unfinished release became ready")
		}
	}
	var fenceID string
	for _, item := range pending {
		if item.Event.Kind == OperatorReleaseKind {
			fenceID = item.Event.ID
		}
	}
	late := time.Now().UTC().Add(time.Hour)
	releaseTestEvent(t, cfg, "retry", "run", "turn", "PostToolUse", "tool.result", toolBody("browser", "call", json.RawMessage(`"release-tool-canary"`)), nil, late)
	releaseTestEvent(t, cfg, "retry", "run", "turn", "Stop", "turn.completed", "finished", nil, late.Add(time.Second))
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if count, err := ReleaseResidue(RuntimeCodex, "retry", 0, true); err != nil || count != 1 {
		t.Fatalf("retry = %d, %v", count, err)
	}
	pending, _ = Pending(RuntimeCodex)
	fences := 0
	for _, item := range pending {
		if !NewReadinessIndex(pending).UploadReady(item) {
			t.Fatalf("retry left event held: %s", item.Event.Kind)
		}
		if item.Event.Kind == OperatorReleaseKind {
			fences++
			if item.Event.ID != fenceID {
				t.Fatal("retry changed fence ID")
			}
		}
		if item.Event.Kind == "tool.result" && strings.Contains(item.Event.Body, "release-tool-canary") {
			t.Fatal("retry leaked late result")
		}
	}
	if fences != 1 {
		t.Fatalf("fence count = %d", fences)
	}
}

func TestReleaseResidueLateHooksStayRedactedAndReleasedBytesStayStable(t *testing.T) {
	setupFenceCapture(t, ModeRaw)
	prompt := fenceTestPromptAndTool(t, false)
	if count, err := ReleaseResidue(RuntimeCodex, "delegated", 0, true); err != nil || count != 1 {
		t.Fatalf("release = %d, %v", count, err)
	}
	pending, _ := Pending(RuntimeCodex)
	original := make(map[string]string)
	for _, item := range pending {
		raw, _ := os.ReadFile(item.Path)
		original[item.Path] = string(raw)
	}
	for i, hook := range []string{
		`{"session_id":"delegated","hook_event_name":"PostToolUse","tool_name":"browser","tool_response":"late-release-canary","transcript_path":"/tmp/delegated-rollout.jsonl"}`,
		`{"session_id":"delegated","hook_event_name":"PreToolUse","tool_name":"mcp__witself__witself_secret_reveal","tool_input":{"secret":"sealed-release-canary"},"transcript_path":"/tmp/delegated-rollout.jsonl"}`,
		`{"session_id":"delegated","hook_event_name":"Stop","transcript_path":"/tmp/delegated-rollout.jsonl"}`,
		`{"session_id":"delegated","hook_event_name":"PostToolUse","tool_name":"browser","tool_response":"after-stop-canary","transcript_path":"/tmp/delegated-rollout.jsonl"}`,
	} {
		event := enqueueTestHook(t, RuntimeCodex, hook)
		wantTurn := prompt.TurnID
		if i == 3 {
			wantTurn = "" // Stop transitions the turn; session suppression remains.
		}
		if event.TurnID != wantTurn || !operatorReleasedEventSafe(event) {
			t.Fatalf("late hook escaped released turn: %s / %s", event.HookEvent, event.TurnID)
		}
	}
	for path, raw := range original {
		after, err := os.ReadFile(path)
		if err != nil || string(after) != raw {
			t.Fatalf("late sealed hook changed upload-ready bytes %s", path)
		}
	}
	pending, _ = Pending(RuntimeCodex)
	for _, item := range pending {
		raw, _ := os.ReadFile(item.Path)
		if bytes.Contains(raw, []byte("late-release-canary")) || bytes.Contains(raw, []byte("sealed-release-canary")) || bytes.Contains(raw, []byte("after-stop-canary")) || !NewReadinessIndex(pending).UploadReady(item) {
			t.Fatalf("unsafe late event %s", item.Event.HookEvent)
		}
	}
	next := enqueueTestHook(t, RuntimeCodex, `{"session_id":"delegated","hook_event_name":"UserPromptSubmit","prompt":"next prompt","transcript_path":"/tmp/delegated-rollout.jsonl"}`)
	if next.TurnID == prompt.TurnID || eventOperatorReleased(next.Data) || PendingEventUploadReady(PendingEvent{Event: next}, nil) {
		t.Fatal("release leaked into next turn")
	}
}

func TestReleaseReadinessRejectsPayloadCopiesAndFlushContention(t *testing.T) {
	cfg := setupFenceCapture(t, ModeRaw)
	releaseTestEvent(t, cfg, "s", "run", "turn", "PostToolUse", "tool.result", toolBody("browser", "call", json.RawMessage(`"value"`)), nil, time.Now().UTC())
	unlock, acquired, err := AcquireFlushLock(RuntimeCodex)
	if err != nil || !acquired {
		t.Fatalf("lock = %t, %v", acquired, err)
	}
	if count, err := ReleaseResidue(RuntimeCodex, "s", 0, true); err == nil || count != 0 {
		t.Fatalf("concurrent flush release = %d, %v", count, err)
	}
	unlock()
	if _, err := ReleaseResidue(RuntimeCodex, "s", 0, true); err != nil {
		t.Fatal(err)
	}
	pending, _ := Pending(RuntimeCodex)
	for _, item := range pending {
		if item.Event.Kind != "tool.result" {
			continue
		}
		for _, mutation := range []func(*Event){
			func(event *Event) { event.Raw = json.RawMessage(`{"value":"canary"}`) },
			func(event *Event) { event.Body = strings.TrimSuffix(event.Body, "}") + `,"value":"canary"}` },
			func(event *Event) {
				event.Data = append(append(json.RawMessage(nil), event.Data[:len(event.Data)-1]...), []byte(`,"extra":"canary"}`)...)
			},
			func(event *Event) {
				event.Data = append(append(json.RawMessage(nil), event.Data[:len(event.Data)-1]...), []byte(`,"sealed_content_omitted":"canary"}`)...)
			},
		} {
			unsafe := item
			mutation(&unsafe.Event)
			if PendingEventUploadReady(unsafe, pending) {
				t.Fatal("release marker admitted an unredacted payload copy")
			}
		}
	}
}
