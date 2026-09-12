package transcriptcapture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// dshLogBuilder assembles a synthetic dsh session log. Fixtures are generated
// here rather than committed so the container encoding stays inspectable and
// no binary blob enters the repository.
type dshLogBuilder struct {
	lines []string
	seq   int
}

func newDSHLog(sessionID, cwd string) *dshLogBuilder {
	builder := &dshLogBuilder{}
	builder.lines = append(builder.lines, mustDSHJSON(map[string]any{
		"type": "session", "version": 3, "id": sessionID, "createdAt": 0,
		"delegationDepth": 0, "cwd": cwd, "isSeeded": false,
	}))
	return builder
}

func (b *dshLogBuilder) event(recordType string, data map[string]any) *dshLogBuilder {
	b.seq++
	b.lines = append(b.lines, mustDSHJSON(map[string]any{
		"type": recordType, "seq": b.seq, "time": 1700000000000 + b.seq, "data": data,
	}))
	return b
}

func (b *dshLogBuilder) prompt(text string) *dshLogBuilder {
	return b.event("user/message", map[string]any{
		"id": "msg-user", "role": "user", "source": map[string]any{"kind": "user"},
		"content": []any{map[string]any{"type": "text", "text": text}},
	})
}

// injected is the plugin-sourced context dsh injects into a step. It shares
// the `user/message` record type with a real prompt and must never be counted
// as one.
func (b *dshLogBuilder) injected(text string) *dshLogBuilder {
	return b.event("user/message", map[string]any{
		"id": "msg-plugin", "role": "user",
		"source":  map[string]any{"kind": "plugin", "plugin": "hooks-claude-code"},
		"content": []any{map[string]any{"type": "text", "text": text}},
	})
}

func (b *dshLogBuilder) assistant(turn, step int, text, model string, usage map[string]any) *dshLogBuilder {
	data := map[string]any{
		"turn": turn, "step": step, "stream": []any{},
		"message": map[string]any{
			"id": "msg-assistant", "role": "assistant",
			"content": []any{
				map[string]any{"type": "reasoning", "text": "private model reasoning"},
				map[string]any{"type": "text", "text": text},
			},
			"source": map[string]any{
				"kind": "model", "provider": "deepseek-official", "model": model,
			},
		},
	}
	if usage != nil {
		counts := make(map[string]any, len(usage)+2)
		for key, value := range usage {
			counts[key] = value
		}
		count := func(value any) int64 {
			switch value := value.(type) {
			case int:
				return int64(value)
			case int64:
				return value
			default:
				return 0
			}
		}
		counts["reasoningTokens"] = 0
		counts["totalTokens"] = count(usage["inputTokens"]) + count(usage["outputTokens"])
		data["usage"] = counts
	}
	return b.event("assistant/message", data)
}

func (b *dshLogBuilder) toolCall(turn, step int, callID, name, arguments string) *dshLogBuilder {
	return b.event("tool/call", map[string]any{
		"turn": turn, "step": step, "callId": callID, "name": name, "arguments": arguments,
	})
}

func (b *dshLogBuilder) toolResult(turn, step int, callID, text string) *dshLogBuilder {
	return b.event("tool/result", map[string]any{
		"turn": turn, "step": step,
		"message": map[string]any{
			"id": "msg-tool", "role": "user",
			"source": map[string]any{"kind": "tool", "callId": callID},
			"content": []any{map[string]any{
				"type": "tool-result", "toolCallId": callID,
				"content": []any{map[string]any{"type": "text", "text": text}},
			}},
		},
	})
}

func (b *dshLogBuilder) plain() []byte {
	return []byte(strings.Join(b.lines, "\n") + "\n")
}

// frames encodes every line as its own complete Zstandard frame, matching the
// concatenated-frame container the harness appends to.
func (b *dshLogBuilder) frames(t *testing.T) [][]byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = encoder.Close() }()
	out := make([][]byte, 0, len(b.lines))
	for _, line := range b.lines {
		out = append(out, encoder.EncodeAll([]byte(line+"\n"), nil))
	}
	return out
}

func (b *dshLogBuilder) zstd(t *testing.T) []byte {
	t.Helper()
	var out []byte
	for _, frame := range b.frames(t) {
		out = append(out, frame...)
	}
	return out
}

func mustDSHJSON(value map[string]any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// writeDSHSessionLog lays out `$DSH_HOME/sessions/<projectKey>/<id>/<name>`
// and pins DSH_HOME to a temp tree.
func writeDSHSessionLog(t *testing.T, cwd, sessionID, name string, content []byte) string {
	t.Helper()
	root := os.Getenv("DSH_HOME")
	if root == "" {
		root = filepath.Join(t.TempDir(), "dsh")
		t.Setenv("DSH_HOME", root)
	}
	dir := filepath.Join(root, "sessions", dshProjectKey(cwd), dshEncodeSegment(sessionID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func pinDSHReaderHomes(t *testing.T) {
	t.Helper()
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh"))
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), "witself"))
}

func completeDSHTurnFixture() *dshLogBuilder {
	return newDSHLog("session-log-1", "/tmp/project").
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("rename the helper").
		assistant(1, 1, "Let me look at the file.", "deepseek-v3",
			map[string]any{"inputTokens": 100, "outputTokens": 10, "cacheReadTokens": 5}).
		toolCall(1, 1, "call-1", "str_replace_editor", `{"path":"helper.go"}`).
		toolResult(1, 1, "call-1", "edited helper.go").
		event("step/end", map[string]any{"turn": 1, "step": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 2}).
		injected("a file changed on disk").
		assistant(1, 2, "Renamed the helper.", "deepseek-v3",
			map[string]any{"inputTokens": 200, "outputTokens": 20}).
		event("step/end", map[string]any{"turn": 1, "step": 2}).
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
}

func TestDSHSessionLogReadsPlainAndZstandardContainers(t *testing.T) {
	pinDSHReaderHomes(t)
	fixture := completeDSHTurnFixture()
	for _, tc := range []struct {
		name    string
		file    string
		content []byte
	}{
		{"plain jsonl", "session.v3.jsonl", fixture.plain()},
		{"zstandard frames", "session.v3.jsonl.zstd", fixture.zstd(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh"))
			writeDSHSessionLog(t, "/tmp/project", "session-log-1", tc.file, tc.content)

			turn, err := readDSHSessionTurn("session-log-1", "/tmp/project", 1)
			if err != nil {
				t.Fatal(err)
			}
			if !turn.Complete {
				t.Fatal("a turn with turn/end must read as complete")
			}
			if turn.Body != "Renamed the helper." {
				t.Fatalf("turn body = %q", turn.Body)
			}
			if turn.Model != "deepseek-v3" || turn.Provider != "deepseek-official" {
				t.Fatalf("turn model/provider = %q/%q", turn.Model, turn.Provider)
			}
			want := dshTokenUsage{InputTokens: 300, OutputTokens: 30, CacheReadTokens: 5}
			if turn.Usage != want {
				t.Fatalf("turn usage = %#v, want %#v", turn.Usage, want)
			}
			if len(turn.Steps) != 2 {
				t.Fatalf("turn steps = %#v", turn.Steps)
			}
			if turn.Steps[0].Text != "Let me look at the file." {
				t.Fatalf("earlier step text = %q", turn.Steps[0].Text)
			}
			if len(turn.Steps[0].ToolCalls) != 1 || turn.Steps[0].ToolCalls[0].CallID != "call-1" {
				t.Fatalf("step tool calls = %#v", turn.Steps[0].ToolCalls)
			}
			if len(turn.Steps[0].ToolResults) != 1 ||
				turn.Steps[0].ToolResults[0].Body != "edited helper.go" {
				t.Fatalf("step tool results = %#v", turn.Steps[0].ToolResults)
			}
		})
	}
}

// TestDSHSessionLogTruncatedTrailingFrameIsIncomplete proves a write in flight
// reads as "not yet", never as an empty assistant response.
func TestDSHSessionLogTruncatedTrailingFrameIsIncomplete(t *testing.T) {
	pinDSHReaderHomes(t)
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh"))
	frames := completeDSHTurnFixture().frames(t)
	var torn []byte
	for index, frame := range frames {
		if index == len(frames)-1 {
			// The turn/end frame is only half written.
			torn = append(torn, frame[:len(frame)/2]...)
			break
		}
		torn = append(torn, frame...)
	}
	writeDSHSessionLog(t, "/tmp/project", "session-log-1", "session.v3.jsonl.zstd", torn)

	turn, err := readDSHSessionTurn("session-log-1", "/tmp/project", 1)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Complete {
		t.Fatal("a turn whose turn/end frame is torn must not read as complete")
	}
	// Everything that landed in whole frames is still readable, so the retry
	// costs nothing once the tail settles.
	if turn.Body != "Renamed the helper." {
		t.Fatalf("torn-tail turn body = %q", turn.Body)
	}
}

// TestDSHSessionLogSelectsTurnByPromptOrdinal covers both correlation paths:
// the additive ordinal a Stop event carries, and the latest-turn
// fallback for a session whose state predates that field.
func TestDSHSessionLogSelectsTurnByPromptOrdinal(t *testing.T) {
	pinDSHReaderHomes(t)
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh"))
	log := newDSHLog("session-log-2", "/tmp/project").
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("first").
		assistant(1, 1, "first answer", "deepseek-v3", nil).
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}).
		event("turn/start", map[string]any{"turn": 2}).
		event("step/start", map[string]any{"turn": 2, "step": 1}).
		prompt("second").
		// A plugin context enters the step beside the human prompt.
		injected("workspace notice").
		assistant(2, 1, "second answer", "deepseek-v3", nil).
		event("turn/end", map[string]any{"turn": 2, "reason": map[string]any{"kind": "completed"}})
	writeDSHSessionLog(t, "/tmp/project", "session-log-2", "session.v3.jsonl", log.plain())

	for _, tc := range []struct {
		name    string
		ordinal int
		want    string
	}{
		{"first prompt", 1, "first answer"},
		{"second prompt", 2, "second answer"},
		{"missing state follows the latest turn", 0, "second answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turn, err := readDSHSessionTurn("session-log-2", "/tmp/project", tc.ordinal)
			if err != nil {
				t.Fatal(err)
			}
			if !turn.Complete || turn.Body != tc.want {
				t.Fatalf("turn = %#v, want body %q", turn, tc.want)
			}
		})
	}

	// An ordinal the log has not reached yet is pending, not empty.
	turn, err := readDSHSessionTurn("session-log-2", "/tmp/project", 3)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Complete || turn.Body != "" {
		t.Fatalf("unwritten turn = %#v", turn)
	}
}

func TestDSHSessionLogRequiresTurnEnd(t *testing.T) {
	pinDSHReaderHomes(t)
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh"))
	log := newDSHLog("session-log-3", "/tmp/project").
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("hello").
		assistant(1, 1, "still working", "deepseek-v3", nil)
	writeDSHSessionLog(t, "/tmp/project", "session-log-3", "session.v3.jsonl", log.plain())

	turn, err := readDSHSessionTurn("session-log-3", "/tmp/project", 1)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Complete {
		t.Fatal("a turn without turn/end must not read as complete")
	}
}

// TestDSHSessionLogDropsSealedToolResultBodies proves the harness log can
// never reintroduce a revealed secret the hook plane suppressed.
func TestDSHSessionLogDropsSealedToolResultBodies(t *testing.T) {
	pinDSHReaderHomes(t)
	const canary = "dsh-session-log-canary-4471"
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh"))
	log := newDSHLog("session-log-4", "/tmp/project").
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("read my token").
		assistant(1, 1, "reading", "deepseek-v3", nil).
		toolCall(1, 1, "call-sealed", "mcp__witself__witself_secret_reveal_0564d00a4f41",
			`{"subject":"`+canary+`"}`).
		toolResult(1, 1, "call-sealed", canary).
		event("step/start", map[string]any{"turn": 1, "step": 2}).
		assistant(1, 2, "done", "deepseek-v3", nil).
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	writeDSHSessionLog(t, "/tmp/project", "session-log-4", "session.v3.jsonl", log.plain())

	turn, err := readDSHSessionTurn("session-log-4", "/tmp/project", 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), canary) {
		t.Fatalf("sealed tool payload survived the session-log read: %s", raw)
	}
	call := turn.Steps[0].ToolCalls[0]
	result := turn.Steps[0].ToolResults[0]
	if !call.Sealed || call.Arguments != "" || !result.Sealed || result.Body != "" {
		t.Fatalf("sealed tool records = %#v / %#v", call, result)
	}
}

// TestDSHSessionLogSealsWrappedAndShellSealedToolPayloads covers the two fences
// the hook plane applies beyond the sealed tool name. The log projection stands
// in for hook events that were never captured, so a `bash` call that runs the
// sealed CLI and an MCP wrapper that dispatches to a sealed tool must seal here
// too, and everything the turn does after one of them must seal with it.
func TestDSHSessionLogSealsWrappedAndShellSealedToolPayloads(t *testing.T) {
	pinDSHReaderHomes(t)
	for _, tc := range []struct {
		name      string
		tool      string
		arguments string
	}{
		{"shell invocation", "bash", `{"command":"witself secret reveal github --json"}`},
		{
			"shell invocation behind env and sh -c",
			"Shell",
			`{"command":"env FOO=1 sudo -u me sh -c 'witself totp code github'"}`,
		},
		{
			"mcp wrapper dispatch",
			"call_mcp_tool",
			`{"server":"witself","toolName":"witself.secret.reveal","arguments":{}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const canary = "dsh-wrapped-canary-9902"
			t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh"))
			log := newDSHLog("session-log-7", "/tmp/project").
				event("turn/start", map[string]any{"turn": 1}).
				event("step/start", map[string]any{"turn": 1, "step": 1}).
				prompt("read my token").
				assistant(1, 1, "reading", "deepseek-v3", nil).
				toolCall(1, 1, "call-wrapped", tc.tool, tc.arguments).
				toolResult(1, 1, "call-wrapped", canary).
				// A later unrelated tool is an exfiltration sink for the value
				// the sealed one just produced, and so is the assistant's own
				// summary of the turn.
				event("step/start", map[string]any{"turn": 1, "step": 2}).
				assistant(1, 2, "Your token is "+canary, "deepseek-v3", nil).
				toolCall(1, 2, "call-later", "write_file", `{"text":"`+canary+`"}`).
				toolResult(1, 2, "call-later", "wrote "+canary).
				event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
			writeDSHSessionLog(t, "/tmp/project", "session-log-7", "session.v3.jsonl", log.plain())

			turn, err := readDSHSessionTurn("session-log-7", "/tmp/project", 1)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(turn)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), canary) {
				t.Fatalf("sealed payload survived the session-log read: %s", raw)
			}
			if !turn.Steps[0].ToolCalls[0].Sealed || !turn.Steps[0].ToolResults[0].Sealed {
				t.Fatalf("sealed tool records = %#v", turn.Steps[0])
			}
			if !turn.Steps[1].ToolCalls[0].Sealed || !turn.Steps[1].ToolResults[0].Sealed {
				t.Fatalf("later tool records of a sealed turn = %#v", turn.Steps[1])
			}
			if turn.Body != "" {
				t.Fatalf("sealed turn body = %q", turn.Body)
			}
		})
	}
}

// TestDSHSessionLogUnsealedShellCallsStayCapturable keeps the broadened fence
// from swallowing ordinary shell and wrapper work.
func TestDSHSessionLogUnsealedShellCallsStayCapturable(t *testing.T) {
	pinDSHReaderHomes(t)
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh"))
	log := newDSHLog("session-log-8", "/tmp/project").
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("build it").
		assistant(1, 1, "building", "deepseek-v3", nil).
		toolCall(1, 1, "call-build", "bash", `{"command":"go build ./..."}`).
		toolResult(1, 1, "call-build", "ok").
		toolCall(1, 1, "call-recall", "call_mcp_tool",
			`{"server":"witself","toolName":"witself.memory.recall"}`).
		toolResult(1, 1, "call-recall", "two memories").
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	writeDSHSessionLog(t, "/tmp/project", "session-log-8", "session.v3.jsonl", log.plain())

	turn, err := readDSHSessionTurn("session-log-8", "/tmp/project", 1)
	if err != nil {
		t.Fatal(err)
	}
	step := turn.Steps[0]
	for index, call := range step.ToolCalls {
		if call.Sealed || call.Arguments == "" {
			t.Fatalf("unsealed tool call %d = %#v", index, call)
		}
	}
	for index, result := range step.ToolResults {
		if result.Sealed || result.Body == "" {
			t.Fatalf("unsealed tool result %d = %#v", index, result)
		}
	}
	if turn.Body != "building" {
		t.Fatalf("unsealed turn body = %q", turn.Body)
	}
}

func TestDSHSessionLogRefusesUntrustedArtifacts(t *testing.T) {
	pinDSHReaderHomes(t)
	fixture := completeDSHTurnFixture().plain()

	t.Run("symlinked log", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "dsh")
		t.Setenv("DSH_HOME", root)
		dir := filepath.Join(root, "sessions", dshProjectKey("/tmp/project"), "session-log-1")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "elsewhere.jsonl")
		if err := os.WriteFile(target, fixture, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "session.v3.jsonl")); err != nil {
			t.Fatal(err)
		}
		if _, err := readDSHSessionTurn("session-log-1", "/tmp/project", 1); err == nil ||
			!strings.Contains(err.Error(), "regular file") {
			t.Fatalf("symlinked session log error = %v", err)
		}
	})

	t.Run("world-writable project directory", func(t *testing.T) {
		t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh"))
		path := writeDSHSessionLog(t, "/tmp/project", "session-log-1", "session.v3.jsonl", fixture)
		if err := os.Chmod(filepath.Dir(filepath.Dir(path)), 0o777); err != nil {
			t.Fatal(err)
		}
		if _, err := readDSHSessionTurn("session-log-1", "/tmp/project", 1); err == nil ||
			!strings.Contains(err.Error(), "trusted session store") {
			t.Fatalf("world-writable directory error = %v", err)
		}
	})

	t.Run("oversize log", func(t *testing.T) {
		t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh"))
		path := writeDSHSessionLog(t, "/tmp/project", "session-log-1", "session.v3.jsonl", fixture)
		// A sparse truncate proves the cap is enforced from the stat, before
		// any of those bytes are read.
		if err := os.Truncate(path, dshSessionLogMaxBytes+1); err != nil {
			t.Fatal(err)
		}
		_, err := readDSHSessionTurn("session-log-1", "/tmp/project", 1)
		if err == nil || !strings.Contains(err.Error(), "bounded read limit") {
			t.Fatalf("oversize session log error = %v", err)
		}
		// A bound a growing log has already crossed is reported as its own
		// sentinel, so the finalizer can settle the affected Stop event instead
		// of blocking the session's whole transcript on a read that can only
		// keep failing.
		if !dshSessionLogBoundExceeded(err) {
			t.Fatalf("oversize session log is not reported as a crossed bound: %v", err)
		}
	})
}

// TestDSHSessionLogAmbiguousProjectDirectoriesStayPending proves capture never
// guesses which project produced a session when the cwd does not disambiguate.
func TestDSHSessionLogAmbiguousProjectDirectoriesStayPending(t *testing.T) {
	pinDSHReaderHomes(t)
	root := filepath.Join(t.TempDir(), "dsh")
	t.Setenv("DSH_HOME", root)
	fixture := completeDSHTurnFixture().plain()
	writeDSHSessionLog(t, "/tmp/project-a", "session-log-1", "session.v3.jsonl", fixture)
	writeDSHSessionLog(t, "/tmp/project-b", "session-log-1", "session.v3.jsonl", fixture)

	turn, err := readDSHSessionTurn("session-log-1", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Complete {
		t.Fatal("an ambiguous session id must stay pending")
	}
	// The hook's own cwd resolves it.
	turn, err = readDSHSessionTurn("session-log-1", "/tmp/project-b", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !turn.Complete {
		t.Fatal("the cwd-matching project directory must resolve")
	}
}

// TestDSHSessionLogPrefersHighestGeneration pins the generation rule: the
// harness migrates forward and leaves older immutable generations in place.
func TestDSHSessionLogPrefersHighestGeneration(t *testing.T) {
	pinDSHReaderHomes(t)
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh"))
	stale := newDSHLog("session-log-5", "/tmp/project").
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("hello").
		assistant(1, 1, "stale generation", "deepseek-v2", nil).
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	writeDSHSessionLog(t, "/tmp/project", "session-log-5", "session.v2.jsonl", stale.plain())
	current := newDSHLog("session-log-5", "/tmp/project").
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("hello").
		assistant(1, 1, "current generation", "deepseek-v3", nil).
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	writeDSHSessionLog(t, "/tmp/project", "session-log-5", "session.v3.jsonl", current.plain())

	turn, err := readDSHSessionTurn("session-log-5", "/tmp/project", 1)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Body != "current generation" {
		t.Fatalf("resolved generation body = %q", turn.Body)
	}
}

// TestDSHSessionLogHeaderMustMatchTheSession keeps one session's Stop event
// from being finalized out of another session's log.
func TestDSHSessionLogHeaderMustMatchTheSession(t *testing.T) {
	pinDSHReaderHomes(t)
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh"))
	log := newDSHLog("some-other-session", "/tmp/project").
		event("turn/start", map[string]any{"turn": 1}).
		prompt("hello").
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	writeDSHSessionLog(t, "/tmp/project", "session-log-6", "session.v3.jsonl", log.plain())

	if _, err := readDSHSessionTurn("session-log-6", "/tmp/project", 1); err == nil ||
		!strings.Contains(err.Error(), "different session") {
		t.Fatalf("mismatched header error = %v", err)
	}

	// The binding is on the log's first record, not its first line: a blank
	// prefix must not demote the header to an ordinary record and let this
	// session's Stop be finalized from another session's log.
	writeDSHSessionLog(t, "/tmp/project", "session-log-6", "session.v3.jsonl",
		append([]byte("\n"), log.plain()...))
	if _, err := readDSHSessionTurn("session-log-6", "/tmp/project", 1); err == nil ||
		!strings.Contains(err.Error(), "different session") {
		t.Fatalf("blank-prefixed mismatched header error = %v", err)
	}
}

// TestDSHProjectKeyMatchesTheHarnessEncoding pins the two lossy conventions
// the session store's directory names depend on.
func TestDSHProjectKeyMatchesTheHarnessEncoding(t *testing.T) {
	pinDSHReaderHomes(t)
	for _, tc := range []struct{ cwd, want string }{
		{"/Users/scott/proj", "--Users-scott-proj--"},
		{"/", "--root--"},
		{`C:\work\repo`, "--C-work-repo--"},
		{"/tmp/a b", "--tmp-a~0020b--"},
		{"/tmp/~home", "--tmp-~007Ehome--"},
	} {
		if got := dshProjectKey(tc.cwd); got != tc.want {
			t.Errorf("dshProjectKey(%q) = %q, want %q", tc.cwd, got, tc.want)
		}
	}
	for _, tc := range []struct{ id, want string }{
		{"01JF8Z0000", "01JF8Z0000"},
		{"..", "~002E~002E"},
		{"a/b", "a~002Fb"},
	} {
		if got := dshEncodeSegment(tc.id); got != tc.want {
			t.Errorf("dshEncodeSegment(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestDSHSessionLogPromptValidationAndOpenFallback(t *testing.T) {
	pinDSHReaderHomes(t)
	log := newDSHLog("resumed", "/tmp/project").
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("old request").
		assistant(1, 1, "old answer", "old-model", map[string]any{"inputTokens": 99}).
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}).
		event("turn/start", map[string]any{"turn": 2}).
		event("step/start", map[string]any{"turn": 2, "step": 1}).
		prompt("new request").
		injected("plugin context").
		assistant(2, 1, "new answer", "new-model", map[string]any{"inputTokens": 7})
	path := writeDSHSessionLog(t, "/tmp/project", "resumed", "session.v3.jsonl", log.plain())
	for _, ordinal := range []int{0, 1, 2, 3} {
		turn, err := readDSHSessionTurn("resumed", "/tmp/project", ordinal, dshTurnCorrelation{PromptSHA256: dshPromptSHA256("new request")})
		if err != nil {
			t.Fatal(err)
		}
		if turn.Complete || turn.Body != "new answer" || turn.Model != "new-model" || turn.Usage.InputTokens != 7 {
			t.Fatalf("ordinal %d did not select the open matching turn", ordinal)
		}
	}
	legacy, err := readDSHSessionTurn("resumed", "/tmp/project", 0)
	if err != nil || legacy.Complete || legacy.Body != "new answer" {
		t.Fatal("an unanchored Stop selected the completed predecessor")
	}
	unmatched, err := readDSHSessionTurn("resumed", "/tmp/project", 1, dshTurnCorrelation{PromptSHA256: dshPromptSHA256("unrelated request")})
	if err != nil || unmatched.Complete || !unmatched.Unresolved || unmatched.Body != "" || unmatched.Model != "" || !unmatched.Usage.empty() || len(unmatched.Steps) != 0 {
		t.Fatal("an unmatched prompt borrowed content or accounting")
	}
	log.event("turn/end", map[string]any{"turn": 2, "reason": map[string]any{"kind": "completed"}})
	if err := os.WriteFile(path, log.plain(), 0o600); err != nil {
		t.Fatal(err)
	}
	finalized, err := readDSHSessionTurn("resumed", "/tmp/project", 1, dshTurnCorrelation{PromptSHA256: dshPromptSHA256("new request")})
	if err != nil || !finalized.Complete || finalized.Body != "new answer" || finalized.Usage.InputTokens != 7 {
		t.Fatal("the matching provider fence did not finalize the new answer")
	}
}

func TestDSHSessionLogPromptCountIncludesSeededHistoryOnlyUsers(t *testing.T) {
	pinDSHReaderHomes(t)
	log := newDSHLog("seeded", "/tmp/project")
	log.lines[0] = mustDSHJSON(map[string]any{
		"type": "session", "version": 3, "id": "seeded", "cwd": "/tmp/project",
		"createdAt": 0, "delegationDepth": 1, "isSeeded": true,
	})
	for turn := 1; turn <= 3; turn++ {
		log.event("turn/start", map[string]any{"turn": turn}).
			event("step/start", map[string]any{"turn": turn, "step": 1}).
			prompt("inherited request").injected("plugin context").
			event("user/message", map[string]any{"source": map[string]any{"kind": "agent-instructions"}}).
			event("agent/inbox/spliced", map[string]any{"turn": turn}).
			event("turn/end", map[string]any{"turn": turn, "reason": map[string]any{"kind": "completed"}})
	}
	writeDSHSessionLog(t, "/tmp/project", "seeded", "session.v3.jsonl", log.plain())
	count, err := countDSHSessionPrompts("seeded", "/tmp/project")
	if err != nil || count != 3 {
		t.Fatalf("seeded prompt count = %d, error = %v", count, err)
	}
	count, err = countDSHSessionPrompts("not-created", "/tmp/project")
	if err != nil || count != 0 {
		t.Fatalf("missing log prompt count = %d, error = %v", count, err)
	}
}

func TestDSHSessionLogFailedFinalModelCallDoesNotPromoteIntermediateText(t *testing.T) {
	pinDSHReaderHomes(t)
	log := newDSHLog("failed-call", "/tmp/project").
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("inspect the project").
		assistant(1, 1, "Let me inspect the project.", "deepseek-v3", map[string]any{"inputTokens": 5}).
		toolCall(1, 1, "call-inspect", "read_file", `{"path":"main.go"}`).
		toolResult(1, 1, "call-inspect", "package main").
		event("step/end", map[string]any{"turn": 1, "step": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 2}).
		event("assistant/attempt", map[string]any{"turn": 1, "step": 2, "error": map[string]any{"name": "provider failure"}}).
		event("step/end", map[string]any{"turn": 1, "step": 2}).
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "error"}})
	writeDSHSessionLog(t, "/tmp/project", "failed-call", "session.v3.jsonl", log.plain())
	turn, err := readDSHSessionTurn("failed-call", "/tmp/project", 1)
	if err != nil || !turn.Complete || turn.Body != "" || len(turn.Steps) != 2 || turn.Steps[0].Text == "" {
		t.Fatal("a failed final model call promoted preceding intermediate text")
	}
}

func TestDSHSessionLogForeignAndOrdinaryToolsStayCapturable(t *testing.T) {
	pinDSHReaderHomes(t)
	for _, name := range []string{
		"mcp__other__secretcreate_0564d00a4f41",
		"mcp__other__foreign_secretcreate_0564d00a4f41",
		"mcp__witself__witself_memory_recall_0564d00a4f41",
		"mcp__witself__witself_fact_get_0564d00a4f41",
	} {
		if dshSealedToolPayload(name, `{}`) {
			t.Errorf("ordinary native tool was sealed: %s", name)
		}
	}
}

func TestDSHSessionLogUsesBindingWhenHookEnvironmentDiffers(t *testing.T) {
	for _, scrubbed := range []bool{false, true} {
		t.Run(map[bool]string{false: "different home", true: "scrubbed home"}[scrubbed], func(t *testing.T) {
			cfg := dshTestCaptureConfig(t, ModeTrace)
			t.Setenv("HOME", t.TempDir())
			writeDSHSessionLog(t, "/tmp/project", "session-log-1", "session.v3.jsonl", completeDSHTurnFixture().plain())
			if scrubbed {
				t.Setenv("DSH_HOME", "")
			} else {
				t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "different-dsh"))
			}
			root, err := dshSessionsRoot()
			if err != nil || root != filepath.Join(cfg.RuntimeConfigRoot, "sessions") {
				t.Fatal("session lookup did not use the installed binding root")
			}
			turn, err := readDSHSessionTurn("session-log-1", "/tmp/project", 1)
			if err != nil || !turn.Complete || turn.Body != "Renamed the helper." {
				t.Fatal("hook environment stranded the bound session log")
			}
		})
	}
}

func TestDSHSessionLogPollCacheObservesAppendedFence(t *testing.T) {
	pinDSHReaderHomes(t)
	log := completeDSHTurnFixture()
	log.lines = log.lines[:len(log.lines)-1]
	path := writeDSHSessionLog(t, "/tmp/project", "session-log-1", "session.v3.jsonl", log.plain())
	const appendDelay = 20 * time.Millisecond
	writeDone := make(chan error, 1)
	go func() {
		time.Sleep(appendDelay)
		log.event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			writeDone <- err
			return
		}
		_, err = file.WriteString(log.lines[len(log.lines)-1] + "\n")
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		writeDone <- err
	}()
	turn, err := readCompleteDSHTurnWithin("session-log-1", "/tmp/project", 1, 50*appendDelay, appendDelay/4)
	if writeErr := <-writeDone; writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil || !turn.Complete || turn.Body != "Renamed the helper." {
		t.Fatal("poll cache hid an appended turn fence")
	}
}

func TestDSHSessionLogDecodedLimitCannotRecoverLaterAnswer(t *testing.T) {
	pinDSHReaderHomes(t)
	log := newDSHLog("long-session", "/tmp/project").
		event("request/context", map[string]any{"context": strings.Repeat("x", dshSessionLogMaxBytes)}).
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("latest request").
		assistant(1, 1, "latest answer", "deepseek-v3", nil).
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	encoded := log.zstd(t)
	if len(encoded) >= dshSessionLogMaxBytes {
		t.Fatal("fixture must stay within the compressed artifact cap")
	}
	writeDSHSessionLog(t, "/tmp/project", "long-session", "session.v3.jsonl.zstd", encoded)
	_, err := readDSHSessionTurn("long-session", "/tmp/project", 1)
	if !dshSessionLogBoundExceeded(err) {
		t.Fatalf("decoded cap error = %v", err)
	}
}

func TestDSHSessionLogGroupsSameStepUserInboxBatch(t *testing.T) {
	pinDSHReaderHomes(t)
	log := newDSHLog("batched-users", "/tmp/project").
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("first user text").
		injected("plugin context is not the prompt").
		prompt("second user text").
		assistant(1, 1, "one answer for the whole batch", "deepseek-v3", map[string]any{"inputTokens": 21}).
		event("step/end", map[string]any{"turn": 1, "step": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 2}).
		prompt("later steering").
		assistant(1, 2, "answer for later steering", "deepseek-v3", map[string]any{"inputTokens": 34}).
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	writeDSHSessionLog(t, "/tmp/project", "batched-users", "session.v3.jsonl", log.plain())
	for _, ordinal := range []int{0, 1, 2, 9} {
		turn, err := readDSHSessionTurn("batched-users", "/tmp/project", ordinal, dshTurnCorrelation{PromptSHA256: dshPromptSHA256("first user textsecond user text")})
		if err != nil || !turn.Complete || turn.Body != "one answer for the whole batch" || turn.Usage.InputTokens != 21 || len(turn.Steps) != 1 {
			t.Fatalf("same-step user batch at ordinal %d lost or duplicated its answer", ordinal)
		}
	}
	turn, err := readDSHSessionTurn("batched-users", "/tmp/project", 3, dshTurnCorrelation{PromptSHA256: dshPromptSHA256("later steering")})
	if err != nil || !turn.Complete || turn.Body != "answer for later steering" || turn.Usage.InputTokens != 34 || len(turn.Steps) != 1 {
		t.Fatal("a later user step did not retain its own answer and accounting")
	}
	count, err := countDSHSessionPrompts("batched-users", "/tmp/project")
	if err != nil || count != 3 {
		t.Fatal("batch grouping changed the user-record ordinal baseline")
	}
}

func TestDSHSessionLogStopOrdinalCoalescesHandlerMarkers(t *testing.T) {
	pinDSHReaderHomes(t)
	log := newDSHLog("plugin-continuation", "/tmp/project").
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("original request").
		assistant(1, 1, "first stopping answer", "deepseek-v3", map[string]any{"inputTokens": 13}).
		event("step/end", map[string]any{"turn": 1, "step": 1}).
		event("hook/invoked", map[string]any{"turn": 1, "point": "Stop", "handlerId": "operator"}).
		event("hook/invoked", map[string]any{"turn": 1, "point": "Stop", "handlerId": "witself"}).
		event("step/start", map[string]any{"turn": 1, "step": 2}).
		injected("continue after tool job completion").
		assistant(1, 2, "second stopping answer", "deepseek-v3", map[string]any{"inputTokens": 29}).
		event("step/end", map[string]any{"turn": 1, "step": 2}).
		event("hook/invoked", map[string]any{"turn": 1, "point": "Stop", "handlerId": "operator"}).
		event("hook/invoked", map[string]any{"turn": 1, "point": "Stop", "handlerId": "witself"})
	path := writeDSHSessionLog(t, "/tmp/project", "plugin-continuation", "session.v3.jsonl", log.plain())
	for _, completed := range []bool{false, true} {
		if completed {
			log.event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
			if err := os.WriteFile(path, log.plain(), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for _, item := range []struct {
			ordinal int
			body    string
			usage   int64
		}{{1, "first stopping answer", 13}, {2, "second stopping answer", 29}} {
			turn, err := readDSHSessionTurn("plugin-continuation", "/tmp/project", 1,
				dshTurnCorrelation{PromptSHA256: dshPromptSHA256("original request"), StopOrdinal: item.ordinal})
			if err != nil || turn.Complete != completed || turn.Body != item.body || turn.Usage.InputTokens != item.usage || len(turn.Steps) != 1 {
				t.Fatalf("Stop %d duplicated another stopping attempt or bypassed turn/end", item.ordinal)
			}
		}
	}
}
