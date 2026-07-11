package transcriptcapture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/local"
)

const (
	maxRawPayloadBytes = 8 * 1024
	entryBodyChunkSize = 60 * 1024
	staleFlushLockAge  = 5 * time.Minute
)

// Event is one durable, provider-neutral hook event in the local outbox.
type Event struct {
	SchemaVersion        string          `json:"schema_version"`
	ID                   string          `json:"id"`
	Runtime              string          `json:"runtime"`
	CaptureMode          string          `json:"capture_mode"`
	Account              string          `json:"account"`
	Realm                string          `json:"realm"`
	Agent                string          `json:"agent"`
	AgentID              string          `json:"agent_id"`
	AgentName            string          `json:"agent_name"`
	Location             Location        `json:"location"`
	SessionID            string          `json:"session_id"`
	RunID                string          `json:"run_id"`
	TurnID               string          `json:"turn_id,omitempty"`
	HookEvent            string          `json:"hook_event"`
	Kind                 string          `json:"kind"`
	Role                 string          `json:"role"`
	Body                 string          `json:"body,omitempty"`
	Model                string          `json:"model,omitempty"`
	CWD                  string          `json:"cwd,omitempty"`
	SourceTranscriptPath string          `json:"source_transcript_path,omitempty"`
	ReplyToEventID       string          `json:"reply_to_event_id,omitempty"`
	OccurredAt           time.Time       `json:"occurred_at"`
	Raw                  json.RawMessage `json:"raw,omitempty"`
}

// Entry is one server append generated from an event. Large visible bodies are
// chunked without truncation; all chunks retain the same event id in payload.
type Entry struct {
	ExternalID        string
	Role              string
	Body              string
	Payload           json.RawMessage
	Model             string
	ReplyToExternalID string
}

// PendingEvent ties an event to its outbox file.
type PendingEvent struct {
	Path  string
	Event Event
}

type hookInput struct {
	SessionID            string          `json:"session_id"`
	TranscriptPath       string          `json:"transcript_path"`
	CWD                  string          `json:"cwd"`
	HookEventName        string          `json:"hook_event_name"`
	Model                string          `json:"model"`
	TurnID               string          `json:"turn_id"`
	Prompt               string          `json:"prompt"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	ToolName             string          `json:"tool_name"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	Source               string          `json:"source"`
	Reason               string          `json:"reason"`
	Error                json.RawMessage `json:"error"`
}

type sessionState struct {
	RunID         string `json:"run_id"`
	TurnID        string `json:"turn_id,omitempty"`
	PromptEventID string `json:"prompt_event_id,omitempty"`
}

// EnqueueHook converts stdin from Codex or Claude into one local outbox event.
func EnqueueHook(runtime string, raw []byte) (Event, error) {
	cfg, err := LoadConfig(runtime)
	if err != nil {
		return Event{}, err
	}
	var input hookInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Event{}, fmt.Errorf("parse hook JSON: %w", err)
	}
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.HookEventName = strings.TrimSpace(input.HookEventName)
	if input.SessionID == "" || input.HookEventName == "" {
		return Event{}, errors.New("hook input requires session_id and hook_event_name")
	}

	eventID, err := id.New("evt")
	if err != nil {
		return Event{}, err
	}
	state, err := loadSessionState(cfg.Runtime, input.SessionID)
	if err != nil {
		return Event{}, err
	}
	if input.HookEventName == "SessionStart" || state.RunID == "" {
		state.RunID, err = id.New("run")
		if err != nil {
			return Event{}, err
		}
		state.TurnID = ""
		state.PromptEventID = ""
	}

	turnID := strings.TrimSpace(input.TurnID)
	switch input.HookEventName {
	case "UserPromptSubmit":
		if turnID == "" {
			turnID, err = id.New("turn")
			if err != nil {
				return Event{}, err
			}
		}
		state.TurnID = turnID
		state.PromptEventID = eventID
	case "Stop", "StopFailure":
		if turnID == "" {
			turnID = state.TurnID
		}
	case "PreToolUse", "PostToolUse", "PostToolUseFailure":
		if turnID == "" {
			turnID = state.TurnID
		}
	}

	event := Event{
		SchemaVersion:        SchemaVersion,
		ID:                   eventID,
		Runtime:              cfg.Runtime,
		CaptureMode:          cfg.CaptureMode,
		Account:              cfg.Account,
		Realm:                cfg.Realm,
		Agent:                cfg.Agent,
		AgentID:              cfg.AgentID,
		AgentName:            cfg.AgentName,
		Location:             cfg.Location,
		SessionID:            input.SessionID,
		RunID:                state.RunID,
		TurnID:               turnID,
		HookEvent:            input.HookEventName,
		Model:                strings.TrimSpace(input.Model),
		CWD:                  input.CWD,
		SourceTranscriptPath: input.TranscriptPath,
		OccurredAt:           time.Now().UTC(),
	}
	setEventContent(&event, input, raw)
	if input.HookEventName == "Stop" || input.HookEventName == "StopFailure" {
		event.ReplyToEventID = state.PromptEventID
		state.TurnID = ""
		state.PromptEventID = ""
	}
	if err := saveSessionState(cfg.Runtime, input.SessionID, state); err != nil {
		return Event{}, err
	}
	if err := writeOutboxEvent(event); err != nil {
		return Event{}, err
	}
	if input.HookEventName == "SessionEnd" {
		_ = removeSessionState(cfg.Runtime, input.SessionID)
	}
	return event, nil
}

func setEventContent(event *Event, input hookInput, raw []byte) {
	switch input.HookEventName {
	case "SessionStart":
		event.Kind, event.Role, event.Body = "session.started", "system", "session started"
	case "UserPromptSubmit":
		event.Kind, event.Role, event.Body = "message.user", "user", input.Prompt
	case "Stop":
		event.Kind, event.Role, event.Body = "message.assistant", "assistant", input.LastAssistantMessage
	case "StopFailure":
		event.Kind, event.Role, event.Body = "turn.failed", "system", firstNonempty(errorText(input.Error), input.Reason, "turn failed")
	case "SessionEnd":
		event.Kind, event.Role, event.Body = "session.ended", "system", firstNonempty(input.Reason, "session ended")
	case "PreToolUse":
		event.Kind, event.Role = "tool.call", "tool"
		event.Body = toolBody(input.ToolName, input.ToolUseID, input.ToolInput)
	case "PostToolUse":
		event.Kind, event.Role = "tool.result", "tool"
		event.Body = toolBody(input.ToolName, input.ToolUseID, input.ToolResponse)
	case "PostToolUseFailure":
		event.Kind, event.Role = "tool.error", "tool"
		event.Body = toolErrorBody(input.ToolName, input.ToolUseID, input.ToolInput,
			firstNonempty(errorText(input.Error), input.Reason, "tool failed"))
	default:
		event.Kind, event.Role, event.Body = "runtime.event", "system", input.HookEventName
	}
	if event.CaptureMode == ModeRaw {
		event.Raw = append(json.RawMessage(nil), raw...)
	}
}

func toolBody(name, useID string, value json.RawMessage) string {
	var data any
	if len(value) > 0 && json.Unmarshal(value, &data) == nil {
		raw, _ := json.Marshal(map[string]any{"tool_name": name, "tool_use_id": useID, "value": data})
		return string(raw)
	}
	raw, _ := json.Marshal(map[string]any{"tool_name": name, "tool_use_id": useID})
	return string(raw)
}

func toolErrorBody(name, useID string, input json.RawMessage, message string) string {
	value := map[string]any{
		"tool_name":   name,
		"tool_use_id": useID,
		"error":       message,
	}
	var decoded any
	if len(input) > 0 && json.Unmarshal(input, &decoded) == nil {
		value["input"] = decoded
	}
	raw, _ := json.Marshal(value)
	return string(raw)
}

func errorText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var value map[string]any
	if json.Unmarshal(raw, &value) == nil {
		for _, key := range []string{"message", "error", "type"} {
			if s, ok := value[key].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// TranscriptExternalID is the stable namespace for one local runtime session.
func (e Event) TranscriptExternalID() string {
	value := e.Runtime + ":" + e.Location.ID + ":" + e.SessionID
	if len(value) <= 512 {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return e.Runtime + ":" + e.Location.ID + ":sha256:" + hex.EncodeToString(sum[:])
}

// TranscriptTitle is intentionally descriptive rather than identity-bearing.
func (e Event) TranscriptTitle() string {
	workspace := filepath.Base(e.CWD)
	if workspace == "." || workspace == string(filepath.Separator) || workspace == "" {
		workspace = "session"
	}
	title := strings.Join([]string{e.AgentName, e.Runtime, e.Location.Name, workspace}, " / ")
	return truncateUTF8(title, 256)
}

// TranscriptMetadata returns bounded source/session metadata for creation.
func (e Event) TranscriptMetadata() json.RawMessage {
	value := map[string]any{
		"capture_schema":    SchemaVersion,
		"capture_mode":      e.CaptureMode,
		"runtime":           e.Runtime,
		"location":          e.Location,
		"source_session_id": e.SessionID,
		"agent_id":          e.AgentID,
		"agent_name":        e.AgentName,
	}
	if e.CWD != "" {
		value["initial_cwd"] = e.CWD
	}
	raw, _ := json.Marshal(value)
	return raw
}

// Entries converts an event into immutable server entries without truncating
// a visible body. Raw hook JSON is embedded only while it fits the bounded
// payload; larger raw envelopes retain a digest and byte count.
func (e Event) Entries() []Entry {
	chunks := splitUTF8(e.Body, entryBodyChunkSize)
	if len(chunks) == 0 {
		chunks = []string{""}
	}
	entries := make([]Entry, 0, len(chunks))
	for i, chunk := range chunks {
		payload := map[string]any{
			"capture_schema": SchemaVersion,
			"event_id":       e.ID,
			"runtime":        e.Runtime,
			"hook_event":     e.HookEvent,
			"kind":           e.Kind,
			"capture_mode":   e.CaptureMode,
			"location":       e.Location,
			"session_id":     e.SessionID,
			"run_id":         e.RunID,
			"occurred_at":    e.OccurredAt,
			"chunk_index":    i,
			"chunk_count":    len(chunks),
		}
		if e.TurnID != "" {
			payload["turn_id"] = e.TurnID
		}
		if e.CWD != "" {
			payload["cwd"] = e.CWD
		}
		if e.CaptureMode == ModeRaw && e.SourceTranscriptPath != "" {
			payload["source_transcript_path"] = e.SourceTranscriptPath
		}
		if i == 0 && len(e.Raw) > 0 {
			if len(e.Raw) <= maxRawPayloadBytes {
				var value any
				if json.Unmarshal(e.Raw, &value) == nil {
					payload["raw"] = value
				}
			} else {
				sum := sha256.Sum256(e.Raw)
				payload["raw_omitted"] = true
				payload["raw_bytes"] = len(e.Raw)
				payload["raw_sha256"] = hex.EncodeToString(sum[:])
			}
		}
		raw, _ := json.Marshal(payload)
		reply := ""
		if i == 0 && e.ReplyToEventID != "" {
			reply = entryExternalID(e.ReplyToEventID, 0)
		}
		entries = append(entries, Entry{
			ExternalID:        entryExternalID(e.ID, i),
			Role:              e.Role,
			Body:              chunk,
			Payload:           raw,
			Model:             e.Model,
			ReplyToExternalID: reply,
		})
	}
	return entries
}

func entryExternalID(eventID string, index int) string {
	return fmt.Sprintf("%s:%d", eventID, index)
}

func splitUTF8(value string, maxBytes int) []string {
	if value == "" {
		return nil
	}
	chunks := make([]string, 0, len(value)/maxBytes+1)
	for len(value) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(value[cut]) {
			cut--
		}
		if cut == 0 {
			cut = maxBytes
		}
		chunks = append(chunks, value[:cut])
		value = value[cut:]
	}
	if value != "" {
		chunks = append(chunks, value)
	}
	return chunks
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func writeOutboxEvent(event Event) error {
	dir, err := outboxDir(event.Runtime)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%020d-%s.json", event.OccurredAt.UnixNano(), event.ID)
	return writeJSONAtomic(filepath.Join(dir, name), event)
}

// Pending returns queued events in capture order.
func Pending(runtime string) ([]PendingEvent, error) {
	dir, err := outboxDir(runtime)
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	out := make([]PendingEvent, 0, len(files))
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var event Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, fmt.Errorf("parse outbox event %s: %w", path, err)
		}
		out = append(out, PendingEvent{Path: path, Event: event})
	}
	return out, nil
}

// RemovePending acknowledges one uploaded event.
func RemovePending(path string) error {
	return os.Remove(path)
}

// AcquireFlushLock keeps background flushers from uploading the same files.
func AcquireFlushLock(runtime string) (release func(), acquired bool, err error) {
	dir, err := outboxDir(runtime)
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	path := filepath.Join(dir, ".flush.lock")
	for attempts := 0; attempts < 2; attempts++ {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
			_ = file.Close()
			return func() { _ = os.Remove(path) }, true, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, false, err
		}
		info, statErr := os.Stat(path)
		if statErr != nil || time.Since(info.ModTime()) <= staleFlushLockAge {
			return func() {}, false, nil
		}
		_ = os.Remove(path)
	}
	return func() {}, false, nil
}

func outboxDir(runtime string) (string, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return "", err
	}
	home, err := local.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "capture", "outbox", runtime), nil
}

func sessionStatePath(runtime, sessionID string) (string, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return "", err
	}
	home, err := local.Home()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(home, "capture", "state", runtime, hex.EncodeToString(sum[:16])+".json"), nil
}

func loadSessionState(runtime, sessionID string) (sessionState, error) {
	path, err := sessionStatePath(runtime, sessionID)
	if err != nil {
		return sessionState{}, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return sessionState{}, nil
	}
	if err != nil {
		return sessionState{}, err
	}
	var state sessionState
	if err := json.Unmarshal(raw, &state); err != nil {
		return sessionState{}, fmt.Errorf("parse session state: %w", err)
	}
	return state, nil
}

func saveSessionState(runtime, sessionID string, state sessionState) error {
	path, err := sessionStatePath(runtime, sessionID)
	if err != nil {
		return err
	}
	return writeJSONAtomic(path, state)
}

func removeSessionState(runtime, sessionID string) error {
	path, err := sessionStatePath(runtime, sessionID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
