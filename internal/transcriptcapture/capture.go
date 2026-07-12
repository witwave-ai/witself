package transcriptcapture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	maxRawPayloadBytes           = 8 * 1024
	maxStructuredBytes           = 4 * 1024
	maxNativeTranscriptTailBytes = 16 * 1024 * 1024
	entryBodyChunkSize           = 60 * 1024
	staleFlushLockAge            = 5 * time.Minute
)

// Event is one durable, provider-neutral hook event in the local outbox.
type Event struct {
	SchemaVersion        string          `json:"schema_version"`
	ID                   string          `json:"id"`
	Runtime              string          `json:"runtime"`
	RuntimeVersion       string          `json:"runtime_version,omitempty"`
	RuntimeVersionSource string          `json:"runtime_version_source,omitempty"`
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
	NativeHookEvent      string          `json:"native_hook_event"`
	Kind                 string          `json:"kind"`
	Role                 string          `json:"role"`
	Body                 string          `json:"body,omitempty"`
	Data                 json.RawMessage `json:"data,omitempty"`
	Model                string          `json:"model,omitempty"`
	ModelSource          string          `json:"model_source,omitempty"`
	ModelProvider        string          `json:"model_provider,omitempty"`
	ModelProviderSource  string          `json:"model_provider_source,omitempty"`
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
	SessionIDCamel       string          `json:"sessionId"`
	ConversationID       string          `json:"conversation_id"`
	TranscriptPath       string          `json:"transcript_path"`
	TranscriptPathCamel  string          `json:"transcriptPath"`
	CWD                  string          `json:"cwd"`
	WorkspaceRoot        string          `json:"workspaceRoot"`
	HookEventName        string          `json:"hook_event_name"`
	HookEventNameCamel   string          `json:"hookEventName"`
	Model                string          `json:"model"`
	ModelSource          string          `json:"-"`
	ModelProvider        string          `json:"model_provider"`
	ModelProviderCamel   string          `json:"modelProvider"`
	RuntimeVersion       string          `json:"runtime_version"`
	RuntimeVersionCamel  string          `json:"runtimeVersion"`
	CursorVersion        string          `json:"cursor_version"`
	TurnID               string          `json:"turn_id"`
	GenerationID         string          `json:"generation_id"`
	PromptID             string          `json:"promptId"`
	PromptIDSnake        string          `json:"prompt_id"`
	Prompt               string          `json:"prompt"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	LastAssistantCamel   string          `json:"lastAssistantMessage"`
	Text                 string          `json:"text"`
	ToolName             string          `json:"tool_name"`
	ToolNameCamel        string          `json:"toolName"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolUseIDCamel       string          `json:"toolUseId"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolInputCamel       json.RawMessage `json:"toolInput"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	ToolOutput           json.RawMessage `json:"tool_output"`
	ToolOutputCamel      json.RawMessage `json:"toolOutput"`
	Source               string          `json:"source"`
	Reason               string          `json:"reason"`
	Status               string          `json:"status"`
	FailureType          string          `json:"failure_type"`
	ErrorMessage         string          `json:"error_message"`
	DurationMS           int64           `json:"duration_ms"`
	Duration             int64           `json:"duration"`
	InputTokens          int64           `json:"input_tokens"`
	OutputTokens         int64           `json:"output_tokens"`
	CacheReadTokens      int64           `json:"cache_read_tokens"`
	CacheWriteTokens     int64           `json:"cache_write_tokens"`
	AgentID              string          `json:"agent_id"`
	AgentType            string          `json:"agent_type"`
	SubagentType         string          `json:"subagent_type"`
	Error                json.RawMessage `json:"error"`
	NativeHookEvent      string          `json:"-"`
}

type sessionState struct {
	RunID                string `json:"run_id"`
	RuntimeVersion       string `json:"runtime_version,omitempty"`
	RuntimeVersionSource string `json:"runtime_version_source,omitempty"`
	TurnID               string `json:"turn_id,omitempty"`
	PromptEventID        string `json:"prompt_event_id,omitempty"`
}

// EnqueueHook converts stdin from Codex or Claude into one local outbox event.
func EnqueueHook(runtime string, raw []byte) (Event, error) {
	return EnqueueHookForBinding(runtime, "", "", "", "", raw)
}

// EnqueueHookForAgent also verifies that the hook's pinned agent matches the
// installed runtime binding.
func EnqueueHookForAgent(runtime, expectedAgent string, raw []byte) (Event, error) {
	return EnqueueHookForBinding(runtime, "", "", expectedAgent, "", raw)
}

// EnqueueHookForBinding verifies the hook's pinned identity and optional
// location against the installed runtime binding.
func EnqueueHookForBinding(runtime, expectedAccount, expectedRealm, expectedAgent, expectedLocation string, raw []byte) (Event, error) {
	cfg, err := LoadConfig(runtime)
	if err != nil {
		return Event{}, err
	}
	expectedAccount = strings.TrimSpace(expectedAccount)
	if expectedAccount != "" && expectedAccount != cfg.Account {
		return Event{}, fmt.Errorf("hook account %q does not match installed account %q", expectedAccount, cfg.Account)
	}
	expectedRealm = strings.TrimSpace(expectedRealm)
	if expectedRealm != "" && expectedRealm != cfg.Realm {
		return Event{}, fmt.Errorf("hook realm %q does not match installed realm %q", expectedRealm, cfg.Realm)
	}
	expectedAgent = strings.TrimSpace(expectedAgent)
	if expectedAgent != "" && expectedAgent != cfg.Agent {
		return Event{}, fmt.Errorf("hook agent %q does not match installed agent %q", expectedAgent, cfg.Agent)
	}
	expectedLocation = strings.TrimSpace(expectedLocation)
	if expectedLocation != "" && expectedLocation != cfg.Location.Name {
		return Event{}, fmt.Errorf("hook location %q does not match installed location %q", expectedLocation, cfg.Location.Name)
	}
	var input hookInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Event{}, fmt.Errorf("parse hook JSON: %w", err)
	}
	if err := validateNativeHookInput(cfg.Runtime, input); err != nil {
		return Event{}, err
	}
	if err := normalizeHookInput(cfg.Runtime, &input); err != nil {
		return Event{}, err
	}
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
		state.RuntimeVersion = ""
		state.RuntimeVersionSource = ""
	}
	pinRunRuntimeVersion(&state, input.RuntimeVersion, cfg.RuntimeVersion)

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
	case "AgentResponse", "AgentThought", "Stop", "StopFailure":
		if turnID == "" {
			turnID = state.TurnID
		}
	case "PreToolUse", "PostToolUse", "PostToolUseFailure", "SubagentStart", "SubagentStop", "PreCompact", "PostCompact":
		if turnID == "" {
			turnID = state.TurnID
		}
	}

	model := strings.TrimSpace(input.Model)
	modelSource := strings.TrimSpace(input.ModelSource)
	if model == "" {
		modelSource = ""
	} else if modelSource == "" {
		modelSource = "hook"
	}
	modelProvider := strings.TrimSpace(input.ModelProvider)
	modelProviderSource := "hook"
	if modelProvider == "" {
		modelProviderSource = ""
	}
	event := Event{
		SchemaVersion:        SchemaVersion,
		ID:                   eventID,
		Runtime:              cfg.Runtime,
		RuntimeVersion:       state.RuntimeVersion,
		RuntimeVersionSource: state.RuntimeVersionSource,
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
		NativeHookEvent:      input.NativeHookEvent,
		Model:                model,
		ModelSource:          modelSource,
		ModelProvider:        modelProvider,
		ModelProviderSource:  modelProviderSource,
		CWD:                  input.CWD,
		SourceTranscriptPath: input.TranscriptPath,
		OccurredAt:           time.Now().UTC(),
	}
	setEventContent(&event, input, raw)
	if input.HookEventName == "AgentResponse" {
		event.ReplyToEventID = state.PromptEventID
	}
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

func validateNativeHookInput(runtime string, input hookInput) error {
	switch runtime {
	case RuntimeGrokBuild:
		if strings.TrimSpace(input.SessionIDCamel) == "" || strings.TrimSpace(input.HookEventNameCamel) == "" {
			return fmt.Errorf("hook input does not match a native %s payload", runtime)
		}
	case RuntimeCursor:
		if strings.TrimSpace(input.ConversationID) == "" {
			return fmt.Errorf("hook input does not match a native %s payload", runtime)
		}
	}
	return nil
}

func normalizeHookInput(runtime string, input *hookInput) error {
	input.SessionID = strings.TrimSpace(firstNonempty(input.SessionID, input.SessionIDCamel, input.ConversationID))
	input.TranscriptPath = firstNonempty(input.TranscriptPath, input.TranscriptPathCamel)
	input.CWD = firstNonempty(input.CWD, input.WorkspaceRoot)
	input.NativeHookEvent = strings.TrimSpace(firstNonempty(input.HookEventName, input.HookEventNameCamel))
	input.HookEventName = canonicalHookEvent(input.NativeHookEvent)
	input.RuntimeVersion = truncateUTF8(strings.TrimSpace(firstNonempty(input.RuntimeVersion, input.RuntimeVersionCamel, input.CursorVersion)), 256)
	input.Model = truncateUTF8(strings.TrimSpace(input.Model), 256)
	input.ModelProvider = truncateUTF8(strings.TrimSpace(firstNonempty(input.ModelProvider, input.ModelProviderCamel)), 256)
	input.TurnID = strings.TrimSpace(firstNonempty(input.TurnID, input.GenerationID, input.PromptID, input.PromptIDSnake))
	input.PromptID = strings.TrimSpace(firstNonempty(input.PromptID, input.PromptIDSnake, input.GenerationID))
	input.LastAssistantMessage = firstNonempty(input.LastAssistantMessage, input.LastAssistantCamel)
	input.ToolName = firstNonempty(input.ToolName, input.ToolNameCamel)
	input.ToolUseID = firstNonempty(input.ToolUseID, input.ToolUseIDCamel)
	if len(input.ToolInput) == 0 {
		input.ToolInput = input.ToolInputCamel
	}
	if len(input.ToolResponse) == 0 {
		input.ToolResponse = firstRaw(input.ToolOutput, input.ToolOutputCamel)
	}
	if input.DurationMS == 0 {
		input.DurationMS = input.Duration
	}
	if input.ErrorMessage != "" && len(input.Error) == 0 {
		input.Error, _ = json.Marshal(input.ErrorMessage)
	}
	if input.HookEventName == "AgentResponse" || input.HookEventName == "AgentThought" {
		input.LastAssistantMessage = firstNonempty(input.LastAssistantMessage, input.Text)
	}
	if runtime == RuntimeGrokBuild {
		if input.HookEventName == "UserPromptSubmit" {
			input.Prompt = unwrapGrokPrompt(input.Prompt)
		}
		if input.HookEventName == "Stop" && input.TranscriptPath != "" {
			body, model, err := readGrokAssistantMessage(input.TranscriptPath, input.PromptID)
			if err == nil {
				if input.LastAssistantMessage == "" {
					input.LastAssistantMessage = body
				}
				if input.Model == "" && model != "" {
					input.Model = model
					input.ModelSource = "native_transcript"
				}
			}
		}
	}
	if runtime == RuntimeClaudeCode && input.HookEventName == "Stop" && input.Model == "" && input.TranscriptPath != "" {
		model, err := readClaudeModel(input.TranscriptPath)
		if err == nil && model != "" {
			input.Model = model
			input.ModelSource = "native_transcript"
		}
	}
	return nil
}

func pinRunRuntimeVersion(state *sessionState, nativeVersion, configuredVersion string) {
	if state.RuntimeVersion != "" {
		return
	}
	if nativeVersion != "" {
		state.RuntimeVersion = nativeVersion
		state.RuntimeVersionSource = "hook"
		return
	}
	if configuredVersion != "" {
		state.RuntimeVersion = configuredVersion
		state.RuntimeVersionSource = "cli"
	}
}

func canonicalHookEvent(event string) string {
	switch strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(event), "_", ""), "-", "")) {
	case "sessionstart":
		return "SessionStart"
	case "sessionend":
		return "SessionEnd"
	case "userpromptsubmit", "beforesubmitprompt":
		return "UserPromptSubmit"
	case "afteragentresponse", "agentresponse":
		return "AgentResponse"
	case "afteragentthought", "agentthought":
		return "AgentThought"
	case "stop":
		return "Stop"
	case "stopfailure":
		return "StopFailure"
	case "pretooluse":
		return "PreToolUse"
	case "posttooluse":
		return "PostToolUse"
	case "posttoolusefailure":
		return "PostToolUseFailure"
	case "subagentstart":
		return "SubagentStart"
	case "subagentstop":
		return "SubagentStop"
	case "precompact":
		return "PreCompact"
	case "postcompact":
		return "PostCompact"
	case "permissionrequest":
		return "PermissionRequest"
	case "permissiondenied":
		return "PermissionDenied"
	case "notification":
		return "Notification"
	default:
		return strings.TrimSpace(event)
	}
}

func firstRaw(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) != 0 {
			return value
		}
	}
	return nil
}

func unwrapGrokPrompt(prompt string) string {
	const prefix, suffix = "<user_query>\n", "\n</user_query>"
	if strings.HasPrefix(prompt, prefix) && strings.HasSuffix(prompt, suffix) {
		return strings.TrimSuffix(strings.TrimPrefix(prompt, prefix), suffix)
	}
	return prompt
}

type grokSessionUpdate struct {
	Params struct {
		Meta struct {
			PromptID string `json:"promptId"`
		} `json:"_meta"`
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Meta          struct {
				ModelID string `json:"modelId"`
			} `json:"_meta"`
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	} `json:"params"`
}

func readGrokAssistantMessage(path, promptID string) (string, string, error) {
	home := strings.TrimSpace(os.Getenv("GROK_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		home = filepath.Join(userHome, ".grok")
	}
	sessionsRoot, err := filepath.EvalSymlinks(filepath.Join(home, "sessions"))
	if err != nil {
		return "", "", err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(sessionsRoot, resolvedPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Base(resolvedPath) != "updates.jsonl" {
		return "", "", errors.New("grok transcript path is outside the session store")
	}

	file, err := os.Open(resolvedPath)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", "", err
	}
	start := max(info.Size()-maxNativeTranscriptTailBytes, 0)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", "", err
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxNativeTranscriptTailBytes))
	if err != nil {
		return "", "", err
	}
	if start > 0 {
		if newline := bytes.IndexByte(raw, '\n'); newline >= 0 {
			raw = raw[newline+1:]
		}
	}

	var chunks []string
	model := ""
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var update grokSessionUpdate
		if len(line) == 0 || json.Unmarshal(line, &update) != nil {
			continue
		}
		if value := strings.TrimSpace(update.Params.Update.Meta.ModelID); value != "" {
			model = truncateUTF8(value, 256)
		}
		kind := update.Params.Update.SessionUpdate
		if promptID == "" && kind == "user_message_chunk" {
			chunks = nil
			continue
		}
		if kind != "agent_message_chunk" || (promptID != "" && update.Params.Meta.PromptID != promptID) {
			continue
		}
		if text := update.Params.Update.Content.Text; text != "" {
			chunks = append(chunks, text)
		}
	}
	return strings.Join(chunks, "\n\n"), model, nil
}

type claudeTranscriptRecord struct {
	Type    string `json:"type"`
	Message struct {
		Role  string `json:"role"`
		Model string `json:"model"`
	} `json:"message"`
}

func readClaudeModel(path string) (string, error) {
	home := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".claude")
	}
	projectsRoot, err := filepath.EvalSymlinks(filepath.Join(home, "projects"))
	if err != nil {
		return "", err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(projectsRoot, resolvedPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Ext(resolvedPath) != ".jsonl" {
		return "", errors.New("claude transcript path is outside the project session store")
	}

	file, err := os.Open(resolvedPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	start := max(info.Size()-maxNativeTranscriptTailBytes, 0)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxNativeTranscriptTailBytes))
	if err != nil {
		return "", err
	}
	if start > 0 {
		if newline := bytes.IndexByte(raw, '\n'); newline >= 0 {
			raw = raw[newline+1:]
		}
	}

	model := ""
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var record claudeTranscriptRecord
		if len(line) == 0 || json.Unmarshal(line, &record) != nil {
			continue
		}
		if record.Type == "assistant" && record.Message.Role == "assistant" && strings.TrimSpace(record.Message.Model) != "" {
			model = truncateUTF8(strings.TrimSpace(record.Message.Model), 256)
		}
	}
	return model, nil
}

func setEventContent(event *Event, input hookInput, raw []byte) {
	switch input.HookEventName {
	case "SessionStart":
		event.Kind, event.Role, event.Body = "session.started", "system", "session started"
	case "UserPromptSubmit":
		event.Kind, event.Role, event.Body = "message.user", "user", input.Prompt
	case "AgentResponse":
		event.Kind, event.Role, event.Body = "message.assistant", "assistant", input.LastAssistantMessage
	case "AgentThought":
		event.Kind, event.Role, event.Body = "agent.thought", "system", input.LastAssistantMessage
	case "Stop":
		if input.LastAssistantMessage != "" {
			event.Kind, event.Role, event.Body = "message.assistant", "assistant", input.LastAssistantMessage
		} else {
			event.Kind, event.Role, event.Body = "turn.completed", "system", firstNonempty(input.Status, input.Reason, "turn completed")
		}
	case "StopFailure":
		event.Kind, event.Role, event.Body = "turn.failed", "system", firstNonempty(errorText(input.Error), input.ErrorMessage, input.Reason, "turn failed")
	case "SessionEnd":
		event.Kind, event.Role, event.Body = "session.ended", "system", firstNonempty(input.Reason, "session ended")
	case "SubagentStart":
		event.Kind, event.Role, event.Body = "subagent.started", "system", firstNonempty(input.AgentType, input.SubagentType, "subagent started")
	case "SubagentStop":
		event.Kind, event.Role, event.Body = "subagent.stopped", "system", firstNonempty(input.AgentType, input.SubagentType, input.Reason, "subagent stopped")
	case "PreCompact":
		event.Kind, event.Role, event.Body = "compaction.started", "system", "conversation compaction started"
	case "PostCompact":
		event.Kind, event.Role, event.Body = "compaction.completed", "system", "conversation compaction completed"
	case "PermissionRequest":
		event.Kind, event.Role, event.Body = "permission.requested", "system", firstNonempty(input.ToolName, "permission requested")
	case "PermissionDenied":
		event.Kind, event.Role, event.Body = "permission.denied", "system", firstNonempty(input.Reason, input.ToolName, "permission denied")
	case "Notification":
		event.Kind, event.Role, event.Body = "runtime.notification", "system", firstNonempty(input.Reason, "runtime notification")
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
	event.Data = structuredEventData(input)
	if event.CaptureMode == ModeRaw {
		event.Raw = append(json.RawMessage(nil), raw...)
	}
}

func structuredEventData(input hookInput) json.RawMessage {
	data := map[string]any{}
	if input.PromptID != "" {
		data["prompt_id"] = input.PromptID
	}
	if input.Status != "" {
		data["status"] = input.Status
	}
	if input.Reason != "" {
		data["reason"] = input.Reason
	}
	if input.FailureType != "" {
		data["failure_type"] = input.FailureType
	}
	if input.DurationMS != 0 {
		data["duration_ms"] = input.DurationMS
	}
	usage := map[string]any{}
	if input.InputTokens != 0 {
		usage["input_tokens"] = input.InputTokens
	}
	if input.OutputTokens != 0 {
		usage["output_tokens"] = input.OutputTokens
	}
	if input.CacheReadTokens != 0 {
		usage["cache_read_tokens"] = input.CacheReadTokens
	}
	if input.CacheWriteTokens != 0 {
		usage["cache_write_tokens"] = input.CacheWriteTokens
	}
	if len(usage) != 0 {
		data["usage"] = usage
	}
	if input.AgentID != "" || input.AgentType != "" || input.SubagentType != "" {
		data["subagent"] = compactMap(map[string]any{
			"id":   input.AgentID,
			"type": firstNonempty(input.AgentType, input.SubagentType),
		})
	}
	if input.ToolName != "" || input.ToolUseID != "" || len(input.ToolInput) != 0 || len(input.ToolResponse) != 0 {
		tool := map[string]any{
			"name":   input.ToolName,
			"use_id": input.ToolUseID,
		}
		if len(input.ToolInput) != 0 {
			tool["input"] = boundedJSON(input.ToolInput)
		}
		if len(input.ToolResponse) != 0 {
			tool["output"] = boundedJSON(input.ToolResponse)
		}
		if message := firstNonempty(input.ErrorMessage, errorText(input.Error)); message != "" {
			tool["error"] = message
		}
		data["tool"] = compactMap(tool)
	}
	if len(data) == 0 {
		return nil
	}
	raw, _ := json.Marshal(data)
	return raw
}

func compactMap(value map[string]any) map[string]any {
	for key, item := range value {
		switch typed := item.(type) {
		case string:
			if typed == "" {
				delete(value, key)
			}
		case nil:
			delete(value, key)
		}
	}
	return value
}

func (e Event) provenancePayload() map[string]any {
	return map[string]any{
		"runtime":         e.Runtime,
		"runtime_version": nullableString(e.RuntimeVersion),
		"model_provider":  nullableString(e.ModelProvider),
		"model":           nullableString(e.Model),
		"sources": map[string]any{
			"runtime":         "integration",
			"runtime_version": nullableString(e.RuntimeVersionSource),
			"model_provider":  nullableString(e.ModelProviderSource),
			"model":           nullableString(e.ModelSource),
		},
	}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func boundedJSON(raw json.RawMessage) any {
	if len(raw) <= maxStructuredBytes {
		var value any
		if json.Unmarshal(raw, &value) == nil {
			return value
		}
	}
	sum := sha256.Sum256(raw)
	return map[string]any{
		"omitted": true,
		"bytes":   len(raw),
		"sha256":  hex.EncodeToString(sum[:]),
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
	parts := make([]string, 0, 4)
	for _, value := range []string{e.AgentName, e.Runtime, e.Location.Name, workspace} {
		if value != "" {
			parts = append(parts, value)
		}
	}
	title := strings.Join(parts, " / ")
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
			"native_event":   e.NativeHookEvent,
			"kind":           e.Kind,
			"capture_mode":   e.CaptureMode,
			"location":       e.Location,
			"session_id":     e.SessionID,
			"run_id":         e.RunID,
			"occurred_at":    e.OccurredAt,
			"chunk_index":    i,
			"chunk_count":    len(chunks),
			"provenance":     e.provenancePayload(),
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
		if i == 0 && len(e.Data) > 0 {
			var value any
			if json.Unmarshal(e.Data, &value) == nil {
				payload["data"] = value
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
