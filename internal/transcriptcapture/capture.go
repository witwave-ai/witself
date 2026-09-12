package transcriptcapture

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/local"
)

const (
	maxRawPayloadBytes = 8 * 1024
	// maxEntryPayloadBytes mirrors the server's transcript entry payload cap
	// (store maxTranscriptJSONBytes). An entry whose payload exceeds it is
	// rejected on append, which blocks every later event of that transcript.
	maxEntryPayloadBytes         = 16 * 1024
	maxStructuredBytes           = 4 * 1024
	maxNativeTranscriptTailBytes = 16 * 1024 * 1024
	maxNativeTranscriptScanBytes = 64 * 1024 * 1024
	maxNativeTranscriptLineBytes = 2 * 1024 * 1024
	maxNativeTranscriptRecords   = 10_000
	maxSessionStateBytes         = 256 * 1024
	maxSensitiveToolUseFences    = 256
	entryBodyChunkSize           = 60 * 1024
	staleFlushLockAge            = 5 * time.Minute
	grokTranscriptPollInterval   = 50 * time.Millisecond
	grokTranscriptMaxWait        = 2 * time.Second
	codexAutoReviewModel         = "codex-auto-review"
	// maxDSHTurnToolUseIDs bounds the dedup key one dsh turn's Stop event
	// carries into session-log recovery. It is a session-state budget, not a
	// limit on how many tools a turn may run: a turn that exceeds it records an
	// overflow instead of forgetting ids.
	maxDSHTurnToolUseIDs = 512
)

// ErrEphemeralSessionSkipped identifies a Codex hook whose session has no
// persistent Codex rollout and therefore is not part of transcript capture.
var ErrEphemeralSessionSkipped = errors.New("codex ephemeral session is not captured")

// Event is one durable, provider-neutral hook event in the local outbox.
type Event struct {
	SchemaVersion        string             `json:"schema_version"`
	ID                   string             `json:"id"`
	Runtime              string             `json:"runtime"`
	RuntimeVersion       string             `json:"runtime_version,omitempty"`
	RuntimeVersionSource string             `json:"runtime_version_source,omitempty"`
	CaptureMode          string             `json:"capture_mode"`
	Account              string             `json:"account"`
	AccountID            string             `json:"account_id,omitempty"`
	Realm                string             `json:"realm"`
	RealmID              string             `json:"realm_id,omitempty"`
	Agent                string             `json:"agent"`
	AgentID              string             `json:"agent_id"`
	AgentName            string             `json:"agent_name"`
	Location             Location           `json:"location"`
	SessionID            string             `json:"session_id"`
	RunID                string             `json:"run_id"`
	TurnID               string             `json:"turn_id,omitempty"`
	HookEvent            string             `json:"hook_event"`
	NativeHookEvent      string             `json:"native_hook_event"`
	Kind                 string             `json:"kind"`
	Role                 string             `json:"role"`
	Body                 string             `json:"body,omitempty"`
	Data                 json.RawMessage    `json:"data,omitempty"`
	Model                string             `json:"model,omitempty"`
	ModelSource          string             `json:"model_source,omitempty"`
	ModelProvider        string             `json:"model_provider,omitempty"`
	ModelProviderSource  string             `json:"model_provider_source,omitempty"`
	CWD                  string             `json:"cwd,omitempty"`
	SourceTranscriptPath string             `json:"source_transcript_path,omitempty"`
	ReplyToEventID       string             `json:"reply_to_event_id,omitempty"`
	OccurredAt           time.Time          `json:"occurred_at"`
	Raw                  json.RawMessage    `json:"raw,omitempty"`
	RecoveredMessages    []RecoveredMessage `json:"recovered_messages,omitempty"`
	NativeTurnFinalized  bool               `json:"native_turn_finalized,omitempty"`
}

// RecoveredMessage is a visible user or assistant message atomically attached
// to one terminal provider hook when that provider omitted its message hooks.
// Its ids are content-derived so retrying the same terminal hook remains
// idempotent in the provider session's transcript.
type RecoveredMessage struct {
	ID              string          `json:"id"`
	TurnID          string          `json:"turn_id"`
	HookEvent       string          `json:"hook_event"`
	NativeHookEvent string          `json:"native_hook_event"`
	Kind            string          `json:"kind"`
	Role            string          `json:"role"`
	Body            string          `json:"body"`
	Data            json.RawMessage `json:"data,omitempty"`
	ReplyToEventID  string          `json:"reply_to_event_id,omitempty"`
}

// ActivityObservation is the deliberately narrow hook projection sent to the
// activity endpoint. Transcript content, paths, models, raw provider payloads,
// and session identifiers never cross this boundary.
type ActivityObservation struct {
	Runtime         string
	LocationID      string
	Location        string
	Event           string
	EventID         string
	EventOccurredAt time.Time
}

// ActivityObservation returns only privacy-safe activity metadata. HookEvent
// has already been normalized from each provider's native event vocabulary.
func (e Event) ActivityObservation() ActivityObservation {
	return ActivityObservation{
		Runtime: e.Runtime, LocationID: e.Location.ID, Location: e.Location.Name,
		Event: e.HookEvent, EventID: e.ID, EventOccurredAt: e.OccurredAt,
	}
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

// ReadinessIndex answers pending-event upload gates without rescanning the
// complete outbox for every event.
type ReadinessIndex struct {
	sessionEnds     map[readinessSessionKey]time.Time
	terminals       map[readinessTurnKey]time.Time
	promptTimelines map[readinessSessionKey]readinessPromptTimeline
	completedFences map[readinessFenceKey]completedFence
}

type readinessSessionKey struct {
	transcriptID string
	sessionID    string
}

type readinessTurnKey struct {
	session readinessSessionKey
	turnID  string
}

type readinessPrompt struct {
	occurredAt time.Time
	turnID     string
}

type readinessPromptTimeline struct {
	prompts            []readinessPrompt
	suffixFirstTurn    []string
	suffixHasOtherTurn []bool
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
	DSHTurnOrdinal       int             `json:"-"`
	DSHPromptSHA256      string          `json:"-"`
	DSHPromptHookSeq     int64           `json:"-"`
	DSHPromptAfterSeq    int64           `json:"-"`
	DSHStopOrdinal       int             `json:"-"`
	DSHToolUseIDs        []string        `json:"-"`
	DSHToolUseOverflow   bool            `json:"-"`
	SensitiveToolEvent   bool            `json:"-"`
	SensitiveTurnContent bool            `json:"-"`
	SyntheticFence       bool            `json:"-"`
	FenceRunID           string          `json:"-"`
	FenceSealedTurn      bool            `json:"-"`
	FenceRunRollover     bool            `json:"-"`
}

type sessionState struct {
	RunID                 string          `json:"run_id"`
	RuntimeVersion        string          `json:"runtime_version,omitempty"`
	RuntimeVersionSource  string          `json:"runtime_version_source,omitempty"`
	TurnID                string          `json:"turn_id,omitempty"`
	PromptEventID         string          `json:"prompt_event_id,omitempty"`
	PromptCaptured        bool            `json:"prompt_captured,omitempty"`
	ResponseCaptured      bool            `json:"response_captured,omitempty"`
	SensitiveToolUseIDs   map[string]bool `json:"sensitive_tool_use_ids,omitempty"`
	RedactAllToolPayload  bool            `json:"redact_all_tool_payload,omitempty"`
	SensitiveTurn         bool            `json:"sensitive_turn,omitempty"`
	SyntheticFencedTurnID string          `json:"synthetic_fenced_turn_id,omitempty"`
	PendingFence          *Event          `json:"pending_fence,omitempty"`
	// DSHTurnToolUseIDs lists the tool calls of the open dsh turn that its
	// PreToolUse and PostToolUse hooks already captured. The Stop event carries
	// the list so finalization can replay only the tool records the hook plane
	// missed, long after this state may have moved on.
	DSHTurnToolUseIDs []string `json:"dsh_turn_tool_use_ids,omitempty"`
	// DSHTurnToolUseOverflow reports that the open dsh turn ran more tools than
	// that list may hold. The list is the only dedup key finalization has, so an
	// id it no longer holds would be replayed as a recovered child of an already
	// captured tool event. Recovery of this turn's tool records is dropped
	// instead of duplicated.
	DSHTurnToolUseOverflow bool `json:"dsh_turn_tool_use_overflow,omitempty"`
	// DSHTurnOrdinal counts native human messages since an observed start.
	// Prompt hook sequence and record watermark also identify the occurrence
	// when detached SessionStart arrived too late to establish an ordinal.
	DSHTurnOrdinal int `json:"dsh_turn_ordinal,omitempty"`
	// An ordinal is usable only after SessionStart anchored it to the native
	// log. Without an occurrence anchor, digest recovery must be unambiguous.
	DSHTurnOrdinalAnchored bool   `json:"dsh_turn_ordinal_anchored,omitempty"`
	DSHPromptSHA256        string `json:"dsh_prompt_sha256,omitempty"`
	DSHPromptHookSeq       int64  `json:"dsh_prompt_hook_seq,omitempty"`
	DSHPromptAfterSeq      int64  `json:"dsh_prompt_after_seq,omitempty"`
	DSHSessionStartSeen    bool   `json:"dsh_session_start_seen,omitempty"`
	DSHStopCaptured        bool   `json:"dsh_stop_captured,omitempty"`
	DSHLastPromptHookSeq   int64  `json:"dsh_last_prompt_hook_seq,omitempty"`
	DSHStopOrdinal         int    `json:"dsh_stop_ordinal,omitempty"`
	DSHSealedAtUnixMilli   int64  `json:"dsh_sealed_at_unix_milli,omitempty"`
	// dsh's immutable header, unlike source:user, distinguishes delegated
	// sessions. Root provenance is cached; inherited sessions remain omitted.
	DSHSessionProvenRoot        bool `json:"dsh_session_proven_root,omitempty"`
	DSHSessionCaptureSuppressed bool `json:"dsh_session_capture_suppressed,omitempty"`
	// SealedTurns lists run/turn pairs that handled sealed material, recorded
	// before their redaction is attempted. A fence consults it so a turn whose
	// queued events never received the sealed marker, because that redaction
	// failed, is still closed sealed and redacted. It survives run rollover.
	SealedTurns []string `json:"sealed_turns,omitempty"`
}

const maxSealedTurnRecords = 256

func sealedTurnKey(runID, turnID string) string {
	return runID + "\x00" + turnID
}

func (s *sessionState) rememberSealedTurn(runID, turnID string) {
	if runID == "" || turnID == "" {
		return
	}
	key := sealedTurnKey(runID, turnID)
	for _, existing := range s.SealedTurns {
		if existing == key {
			return
		}
	}
	s.SealedTurns = append(s.SealedTurns, key)
	if len(s.SealedTurns) > maxSealedTurnRecords {
		s.SealedTurns = s.SealedTurns[len(s.SealedTurns)-maxSealedTurnRecords:]
	}
}

func (s sessionState) sealedTurnRecorded(runID, turnID string) bool {
	key := sealedTurnKey(runID, turnID)
	for _, existing := range s.SealedTurns {
		if existing == key {
			return true
		}
	}
	return false
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
	if cfg.Runtime == RuntimeCodex && strings.TrimSpace(input.TranscriptPath) == "" {
		// Witself captures the Codex threads that Codex itself persists. A
		// session with no rollout (transcript_path null) is not the agent's
		// narrative record, which excludes desktop background agents without
		// prompt-signature matching. Deliberately ephemeral sessions such as
		// codex exec --ephemeral and non-persisted app-server threads are
		// excluded by the same structural rule, by design.
		_ = recordSkippedSession(
			cfg.Runtime,
			input.SessionID,
			input.HookEventName,
			input.RuntimeVersion,
			input.Model,
			time.Now().UTC(),
		)
		return Event{}, ErrEphemeralSessionSkipped
	}

	// Every runtime now reads and rewrites session state across a hook: run
	// rollover fences the prior run from the same critical section, so the lock
	// can no longer be Codex-only.
	release, err := acquireSessionStateLock(cfg.Runtime, input.SessionID)
	if err != nil {
		return Event{}, err
	}
	defer release()
	return enqueueHook(cfg, input, raw)
}

// enqueueHook is the common hook and companion-fence path. Codex callers hold
// the session lock across validation, sealed-turn redaction, and persistence.
func enqueueHook(cfg Config, input hookInput, raw []byte) (Event, error) {
	eventID, err := id.New("evt")
	if err != nil {
		return Event{}, err
	}
	state, err := loadSessionState(cfg.Runtime, input.SessionID)
	if err != nil {
		return Event{}, err
	}
	if err := finishPendingFence(cfg.Runtime, input.SessionID, &state); err != nil {
		return Event{}, err
	}
	freshState := state.RunID == ""
	// dsh runs SessionStart detached; the first prompt can bind the run first.
	// That first lifecycle hook acknowledges this binding instead of fencing it.
	lateDSHStart := cfg.Runtime == RuntimeDSH && input.HookEventName == "SessionStart" &&
		!freshState && (!state.DSHSessionStartSeen || (state.TurnID != "" && !state.DSHStopCaptured))
	// Compaction restarts nothing: Claude Code reports it as a SessionStart
	// in the middle of a still-running turn, and closing that turn early
	// would release its queued prompt before a later sealed tool could
	// suppress it. The session keeps its run and its open turn.
	rebind := state.RunID == "" ||
		(input.HookEventName == "SessionStart" && !sessionStartKeepsRun(input.Source) && !lateDSHStart)
	if rebind {
		if state.RunID != "" {
			// A restarted provider session keeps its session id and starts a
			// new run, whether it was resumed or cleared. Close the prior run
			// first: nothing else will ever emit its terminal event, so
			// without this its queued events would stay in the outbox under
			// a run the session no longer binds.
			//
			// Capture never stops because the outbox cannot be projected. An
			// unreadable or foreign queued file is a flush problem, and
			// `transcript fence --latest` still closes the prior run once it is
			// resolved, so a failed rollover fence skips rather than dropping
			// this hook.
			_, _ = fenceHeldTurns(cfg, input.SessionID, state, rolloverFenceReason(input.Source), true)
			// Each fence saved the session state it observed. Reread it so this
			// hook's rollover cannot overwrite that durable bookkeeping.
			state, err = loadSessionState(cfg.Runtime, input.SessionID)
			if err != nil {
				return Event{}, err
			}
		}
		state.RunID, err = id.New("run")
		if err != nil {
			return Event{}, err
		}
		state.TurnID = ""
		state.PromptEventID = ""
		state.PromptCaptured = false
		state.ResponseCaptured = false
		state.RuntimeVersion = ""
		state.RuntimeVersionSource = ""
		state.SensitiveToolUseIDs = nil
		state.RedactAllToolPayload = false
		state.SensitiveTurn = false
		state.SyntheticFencedTurnID = ""
		if cfg.Runtime == RuntimeDSH {
			state.DSHPromptSHA256 = ""
			state.DSHPromptHookSeq, state.DSHPromptAfterSeq = 0, 0
			state.DSHSealedAtUnixMilli = 0
			state.DSHTurnToolUseIDs = nil
			state.DSHTurnToolUseOverflow = false
		}
	}
	dshContinuation := false
	dshUserPrompt := input.Prompt
	dshUserProven := false
	var dshPromptHookSeq, dshPromptAfterSeq int64
	if cfg.Runtime == RuntimeDSH && state.SensitiveTurn && state.DSHSealedAtUnixMilli == 0 {
		// Legacy sealed state has no native ordering bound. Establish one
		// conservatively instead of accepting an older buffered hook record.
		state.DSHSealedAtUnixMilli = time.Now().UnixMilli()
	}
	if cfg.Runtime == RuntimeDSH && input.HookEventName == "UserPromptSubmit" {
		wait := time.Duration(0)
		if state.PromptCaptured || state.SensitiveTurn {
			wait = dshSessionLogMaxWait
		}
		userPrompt, known, hookSeq := dshPromptProvenance(input.SessionID, input.CWD, input.Prompt, wait,
			state.DSHLastPromptHookSeq, state.DSHSealedAtUnixMilli)
		if known {
			state.DSHLastPromptHookSeq = hookSeq
			dshPromptHookSeq = hookSeq
		}
		dshPromptAfterSeq = dshPromptRecordWatermark(input.SessionID, input.CWD)
		dshUserProven = known && userPrompt != ""
		dshContinuation = (known && userPrompt == "") || (!known && state.SensitiveTurn)
		if dshUserProven {
			dshUserPrompt = userPrompt
		}
	}
	// A new real user prompt is the only reliable cross-provider fence for a
	// new turn. Clear the prior turn's sealed-content suppression before any
	// tools for this turn can mark it again. Codex's nested approval review is
	// normalized to a different event and therefore cannot reset this fence.
	if input.HookEventName == "UserPromptSubmit" {
		// The dsh bridge loses source.kind. An unresolved inbox cannot prove
		// a user authorized the next turn, including after a native turn/end.
		if cfg.Runtime != RuntimeDSH || dshUserProven {
			state.SensitiveTurn = false
			if cfg.Runtime == RuntimeDSH {
				state.DSHSealedAtUnixMilli = 0
			}
		}
	}
	if cfg.Runtime == RuntimeDSH {
		protectDSHDelegation(&input, &state)
		protectDSHSensitiveToolPayload(&input, &state)
	}
	protectSensitiveToolPayload(&input, &state)
	if cfg.Runtime == RuntimeDSH && state.SensitiveTurn && state.DSHSealedAtUnixMilli == 0 {
		state.DSHSealedAtUnixMilli = time.Now().UnixMilli()
	}
	// The session's sealed flag describes the turn the runtime is on and every
	// turn after it. A fence may close a turn the runtime has already left, so
	// that turn's own recorded suppression decides instead: redacting an
	// earlier turn would destroy content no sealed tool ever touched. Decide
	// here, after this hook can still seal the open turn and before the switch
	// below closes it.
	sealedTurn := state.SensitiveTurn
	if input.SyntheticFence && input.TurnID != "" && input.TurnID != state.TurnID {
		sealedTurn = input.FenceSealedTurn
	}
	protectSensitiveTurnContent(&input, sealedTurn)
	pinRunRuntimeVersion(&state, input.RuntimeVersion, cfg.RuntimeVersion)

	turnID := strings.TrimSpace(input.TurnID)
	if cfg.Runtime == RuntimeDSH && !input.SyntheticFence {
		// The prompt ordinal and the turn's captured tool ids advance on the
		// real hooks and travel with the turn's Stop event, because flush runs
		// long after this session state may have moved on or been removed.
		switch {
		case input.HookEventName == "SessionStart":
			// SessionStart precedes persistence of the next prompt. Missing or
			// unavailable native history must never make the hook fail.
			if !lateDSHStart && (freshState || input.Source == "resume") {
				state.DSHTurnOrdinal, _ = countDSHSessionPrompts(input.SessionID, input.CWD)
				state.DSHTurnOrdinalAnchored = true
			}
			state.DSHSessionStartSeen = true
		case input.HookEventName == "UserPromptSubmit" && !dshContinuation:
			state.DSHStopCaptured = false
			state.DSHStopOrdinal = 0
			if state.DSHTurnOrdinalAnchored {
				// One bridge batch may contain several user records. The
				// current native invocation proves the previous batch durable;
				// refresh its count before naming this batch's first record.
				if dshUserProven {
					if count, countErr := countDSHSessionPrompts(input.SessionID, input.CWD); countErr == nil {
						state.DSHTurnOrdinal = count
					}
				}
				state.DSHTurnOrdinal++
			}
			state.DSHPromptSHA256 = ""
			state.DSHPromptHookSeq, state.DSHPromptAfterSeq = dshPromptHookSeq, dshPromptAfterSeq
			if !sealedTurn {
				state.DSHPromptSHA256 = dshPromptSHA256(dshUserPrompt)
			}
			state.DSHTurnToolUseIDs = nil
			state.DSHTurnToolUseOverflow = false
		case isToolHookEvent(input.HookEventName) && input.ToolUseID != "":
			toolKey := dshToolPhaseKey(input.ToolUseID, input.HookEventName)
			switch {
			case slices.Contains(state.DSHTurnToolUseIDs, toolKey):
			case len(state.DSHTurnToolUseIDs) < maxDSHTurnToolUseIDs:
				state.DSHTurnToolUseIDs = append(state.DSHTurnToolUseIDs, toolKey)
			default:
				// The local state file cannot grow without bound, and a turn
				// this long has plainly not lost its tool events to a missing
				// hook plane. Record the overflow rather than dropping ids that
				// finalization would then replay as duplicates.
				state.DSHTurnToolUseOverflow = true
			}
		case input.HookEventName == "Stop" || input.HookEventName == "StopFailure":
			state.DSHStopCaptured = true
			state.DSHStopOrdinal++
			input.DSHStopOrdinal = state.DSHStopOrdinal
			if state.DSHTurnOrdinalAnchored {
				input.DSHTurnOrdinal = state.DSHTurnOrdinal
			}
			input.DSHPromptSHA256 = state.DSHPromptSHA256
			input.DSHPromptHookSeq, input.DSHPromptAfterSeq = state.DSHPromptHookSeq, state.DSHPromptAfterSeq
			input.DSHToolUseIDs = state.DSHTurnToolUseIDs
			input.DSHToolUseOverflow = state.DSHTurnToolUseOverflow
		}
		if sealedTurn {
			state.DSHPromptSHA256 = ""
			input.DSHPromptSHA256 = ""
			state.DSHPromptHookSeq, state.DSHPromptAfterSeq = 0, 0
			input.DSHPromptHookSeq, input.DSHPromptAfterSeq = 0, 0
		}
	}
	switch input.HookEventName {
	case "UserPromptSubmit":
		if cfg.Runtime == RuntimeDSH && dshContinuation {
			turnID = state.TurnID
			break
		}
		if turnID == "" {
			turnID, err = id.New("turn")
			if err != nil {
				return Event{}, err
			}
		}
		state.TurnID = turnID
		state.PromptEventID = eventID
		state.PromptCaptured = strings.TrimSpace(input.Prompt) != ""
		state.ResponseCaptured = false
	case "AgentResponse":
		if turnID == "" {
			turnID = state.TurnID
		}
		state.ResponseCaptured = strings.TrimSpace(input.LastAssistantMessage) != ""
	case "AgentThought", "Stop", "StopFailure":
		if turnID == "" {
			turnID = state.TurnID
		}
		if (input.HookEventName == "Stop" || input.HookEventName == "StopFailure") && input.LastAssistantMessage != "" {
			state.ResponseCaptured = true
		}
	case "PreToolUse", "PostToolUse", "PostToolUseFailure", "SubagentStart", "SubagentStop", "PreCompact", "PostCompact":
		if turnID == "" {
			turnID = state.TurnID
		}
	}
	runID := state.RunID
	if input.SyntheticFence && input.FenceRunID != "" {
		// A launcher fence and a run rollover both close a run that the session
		// may no longer bind. The terminal event belongs to that run, not to
		// whichever run is current when the fence is written.
		runID = input.FenceRunID
	}
	if sealedTurn && turnID != "" {
		// Record the sealed turn before touching the outbox: if the redaction
		// below fails, no queued event carries the sealed marker, and this
		// record is what lets a later fence still close the turn sealed.
		state.rememberSealedTurn(runID, turnID)
		if err := RedactPendingTurn(cfg.Runtime, input.SessionID, turnID); err != nil {
			// Preserve the fail-closed fence even if a local I/O problem prevents
			// this hook from being queued. The next hook retries redaction, while
			// turn-gated flushing keeps the unredacted prompt local.
			_ = saveSessionState(cfg.Runtime, input.SessionID, state)
			return Event{}, err
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
		AccountID:            cfg.AccountID,
		Realm:                cfg.Realm,
		RealmID:              cfg.RealmID,
		Agent:                cfg.Agent,
		AgentID:              cfg.AgentID,
		AgentName:            cfg.AgentName,
		Location:             cfg.Location,
		SessionID:            input.SessionID,
		RunID:                runID,
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
	if cfg.Runtime == RuntimeCursor && cfg.CaptureMode == ModeRaw && input.HookEventName == "SessionEnd" &&
		!state.SensitiveTurn &&
		!state.PromptCaptured && !state.ResponseCaptured && input.TranscriptPath != "" {
		fallback, fallbackErr := cursorNativeTranscriptMessages(event, input.TranscriptPath)
		if fallbackErr == nil {
			event.RecoveredMessages = fallback
		}
	}
	if input.HookEventName == "AgentResponse" {
		event.ReplyToEventID = state.PromptEventID
	}
	if input.HookEventName == "Stop" || input.HookEventName == "StopFailure" {
		// A fence for a turn the session no longer holds open must not adopt
		// another turn's prompt as its parent, or close the turn still running.
		if !input.SyntheticFence || turnID == state.TurnID {
			event.ReplyToEventID = state.PromptEventID
			state.TurnID = ""
			state.PromptEventID = ""
		}
	}
	if input.SyntheticFence {
		// Persist the exact event before closing the turn. A failed outbox write
		// can then be retried with the same event identity and timestamp.
		state.PendingFence = &event
		if runID == state.RunID {
			state.SyntheticFencedTurnID = turnID
		}
	}
	if err := saveSessionState(cfg.Runtime, input.SessionID, state); err != nil {
		return Event{}, err
	}
	if input.SyntheticFence {
		if err := finishPendingFence(cfg.Runtime, input.SessionID, &state); err != nil {
			return Event{}, err
		}
		return event, nil
	}
	if cfg.CaptureMode == ModeMessages && isToolHookEvent(input.HookEventName) {
		// Messages capture observes tool hooks only to maintain the sealed-turn
		// privacy fence. Ordinary tool traffic remains excluded from this mode,
		// and a sealed hook has already redacted queued content above.
		return event, nil
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
	if runtime == RuntimeCursor {
		// conversation_id is the native Cursor session identity and is also
		// the directory name in its trusted transcript store. Never allow a
		// conflicting generic session_id field to redirect fallback recovery.
		input.SessionID = strings.TrimSpace(input.ConversationID)
	} else {
		input.SessionID = strings.TrimSpace(firstNonempty(input.SessionID, input.SessionIDCamel))
	}
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
	if runtime == RuntimeCodex && input.HookEventName == "UserPromptSubmit" && input.Model == codexAutoReviewModel {
		// Codex runs its automatic approval reviewer as a nested model turn in
		// the parent session and emits a UserPromptSubmit hook for the review
		// envelope. It is neither user-authored content nor a new parent turn.
		// Normalize it before session state and hydration consume the event.
		input.HookEventName = HookEventCodexPermissionReview
		input.Prompt = ""
	}
	if input.HookEventName == "UserPromptSubmit" {
		switch runtime {
		case RuntimeGrokBuild:
			input.Prompt = unwrapGrokPrompt(input.Prompt)
		case RuntimeCursor:
			input.Prompt = NormalizeUserPromptBody(runtime, input.Prompt)
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

// sensitiveToolEvent identifies the sealed-plane tool calls whose hook payloads
// may contain plaintext credentials, generated passwords, or one-time codes.
// Tool names vary by provider, so matching is deliberately suffix-based over
// normalized name tokens. Wrapper tools and shell tools are inspected only in
// their recognized tool-name namespaces.
func sensitiveToolEvent(input hookInput) bool {
	if sensitiveSealedToolName(input.ToolName) {
		return true
	}
	if mcpWrapperToolName(input.ToolName) &&
		(rawContainsSensitiveToolName(input.ToolInput) || rawContainsSensitiveToolName(input.ToolResponse)) {
		return true
	}
	return shellToolName(input.ToolName) &&
		(rawContainsSensitiveCLICommand(input.ToolInput) || rawContainsSensitiveCLICommand(input.ToolResponse))
}

func protectSensitiveToolPayload(input *hookInput, state *sessionState) {
	if input == nil || state == nil || !isToolHookEvent(input.HookEventName) {
		return
	}
	if len(state.SensitiveToolUseIDs) > maxSensitiveToolUseFences {
		state.SensitiveToolUseIDs = nil
		state.RedactAllToolPayload = true
	}
	// After any sealed value-bearing operation, every later tool in the same
	// turn is a possible exfiltration sink (browser typing, shell arguments,
	// HTTP clients, and so on). Preserve only value-free tool identity until the
	// next real user prompt resets SensitiveTurn.
	input.SensitiveToolEvent = state.RedactAllToolPayload || state.SensitiveTurn || sensitiveToolEvent(*input)
	if !input.SensitiveToolEvent && input.ToolUseID != "" && state.SensitiveToolUseIDs[input.ToolUseID] {
		input.SensitiveToolEvent = true
	}
	if !input.SensitiveToolEvent {
		return
	}
	state.SensitiveTurn = true
	if input.ToolUseID != "" && !state.RedactAllToolPayload {
		if !state.SensitiveToolUseIDs[input.ToolUseID] && len(state.SensitiveToolUseIDs) >= maxSensitiveToolUseFences {
			// A pathological session must not grow the local state file without
			// bound or lose the correlation that keeps terminal hook output safe.
			// Fail closed for later tool payloads until SessionEnd resets state.
			state.SensitiveToolUseIDs = nil
			state.RedactAllToolPayload = true
		} else {
			if state.SensitiveToolUseIDs == nil {
				state.SensitiveToolUseIDs = make(map[string]bool)
			}
			state.SensitiveToolUseIDs[input.ToolUseID] = true
		}
	}
	redactSensitiveToolPayload(input)
}

// protectSensitiveTurnContent prevents an authorized revealed value, generated
// password, TOTP code, or create input from being copied back into Witself's
// portable transcript by a later provider response hook. The user still sees
// the provider's native response; only Witself capture is made value-free.
// The fence is reset by the next real UserPromptSubmit in EnqueueHookForBinding.
func protectSensitiveTurnContent(input *hookInput, sealedTurn bool) {
	if input == nil || !sealedTurn || isToolHookEvent(input.HookEventName) {
		return
	}
	// Provider hook schemas evolve. Once a turn has handled sealed material,
	// suppress every later non-tool hook instead of maintaining an allowlist
	// that could let a newly introduced response-shaped event retain plaintext.
	// SessionStart and the next real UserPromptSubmit reset the fence before this
	// function runs; tool hooks take the correlated path above.
	input.SensitiveTurnContent = true
	input.Prompt = ""
	input.LastAssistantMessage = ""
	input.Text = ""
	input.Reason = ""
	input.ErrorMessage = ""
	redactSensitiveToolPayload(input)
}

func isToolHookEvent(event string) bool {
	switch event {
	case "PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionRequest", "PermissionDenied":
		return true
	default:
		return false
	}
}

func sensitiveSealedToolName(name string) bool {
	return toolNameHasSuffix(name,
		"witselfsecretcreate",
		"witselfsecretreveal",
		"witselfpasswordgenerate",
		"witselftotpcode",
	)
}

func sensitiveWrappedToolName(name string) bool {
	if sensitiveSealedToolName(name) {
		return true
	}
	return toolNameHasSuffix(name, "secretcreate", "secretreveal", "passwordgenerate", "totpcode")
}

// toolNameHasSuffix matches normalized sealed-tool tokens without interpreting
// runtime-specific identity suffixes. The dsh boundary handles its bridge's
// rewritten spelling before entering these shared matchers.
func toolNameHasSuffix(name string, suffixes ...string) bool {
	compact := compactName(name)
	for _, suffix := range suffixes {
		if strings.HasSuffix(compact, suffix) {
			return true
		}
	}
	return false
}

func mcpWrapperToolName(name string) bool {
	compact := compactName(name)
	return strings.HasSuffix(compact, "callmcptool") ||
		strings.HasSuffix(compact, "mcpcalltool") ||
		strings.HasSuffix(compact, "invokemcptool")
}

func shellToolName(name string) bool {
	compact := compactName(name)
	for _, suffix := range []string{
		"bash", "shell", "terminal", "execcommand", "runcommand", "shellcommand", "executecommand",
	} {
		if strings.HasSuffix(compact, suffix) {
			return true
		}
	}
	return false
}

func compactName(value string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func rawContainsSensitiveToolName(raw json.RawMessage) bool {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return false
	}
	return valueContainsSensitiveToolName(value)
}

func valueContainsSensitiveToolName(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			switch compactName(key) {
			case "tool", "toolname", "mcptool", "mcptoolname", "function", "functionname", "method", "name":
				if name, ok := item.(string); ok && sensitiveWrappedToolName(name) {
					return true
				}
			}
			if valueContainsSensitiveToolName(item) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if valueContainsSensitiveToolName(item) {
				return true
			}
		}
	}
	return false
}

func rawContainsSensitiveCLICommand(raw json.RawMessage) bool {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return false
	}
	return valueContainsSensitiveCLICommand(value)
}

func valueContainsSensitiveCLICommand(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			switch compactName(key) {
			case "command", "cmd", "script", "shellcommand":
				switch command := item.(type) {
				case string:
					if sensitiveCLICommand(command, 0) {
						return true
					}
				case []any:
					parts := make([]string, 0, len(command))
					for _, part := range command {
						text, ok := part.(string)
						if !ok {
							parts = nil
							break
						}
						parts = append(parts, text)
					}
					if len(parts) != 0 && sensitiveCLICommand(strings.Join(parts, " "), 0) {
						return true
					}
				}
			}
			if valueContainsSensitiveCLICommand(item) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if valueContainsSensitiveCLICommand(item) {
				return true
			}
		}
	}
	return false
}

func sensitiveCLICommand(command string, depth int) bool {
	if depth > 3 {
		return false
	}
	for _, segment := range shellCommandSegments(command) {
		if segmentInvokesSensitiveCLI(segment, depth) {
			return true
		}
	}
	return false
}

func segmentInvokesSensitiveCLI(words []string, depth int) bool {
	i := 0
	for i < len(words) && shellAssignment(words[i]) {
		i++
	}
	for i < len(words) {
		command := strings.ToLower(filepath.Base(words[i]))
		switch command {
		case "env":
			i++
			for i < len(words) {
				if shellAssignment(words[i]) {
					i++
					continue
				}
				if words[i] == "-u" || words[i] == "--unset" || words[i] == "-C" || words[i] == "--chdir" {
					i += min(2, len(words)-i)
					continue
				}
				if strings.HasPrefix(words[i], "-") {
					i++
					continue
				}
				break
			}
		case "command", "builtin", "exec", "nohup":
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				i++
			}
		case "sudo":
			i++
			i = skipSudoOptions(words, i)
		case "sh", "bash", "zsh", "dash", "ksh":
			for option := i + 1; option+1 < len(words); option++ {
				if strings.HasPrefix(words[option], "-") && strings.Contains(words[option], "c") {
					return sensitiveCLICommand(words[option+1], depth+1)
				}
				if !strings.HasPrefix(words[option], "-") {
					break
				}
			}
			return false
		default:
			return sensitiveCLIAt(words, i)
		}
	}
	return false
}

func skipSudoOptions(words []string, i int) int {
	for i < len(words) && strings.HasPrefix(words[i], "-") {
		option := words[i]
		i++
		switch option {
		case "-u", "--user", "-g", "--group", "-h", "--host", "-p", "--prompt", "-C", "--chdir", "-r", "--role", "-t", "--type":
			if i < len(words) {
				i++
			}
		}
	}
	return i
}

func sensitiveCLIAt(words []string, i int) bool {
	if i+2 >= len(words) {
		return false
	}
	command := strings.ToLower(filepath.Base(words[i]))
	if command != "witself" && command != "ws" {
		return false
	}
	group := strings.ToLower(words[i+1])
	action := strings.ToLower(words[i+2])
	return (group == "secret" && (action == "create" || action == "reveal")) ||
		(group == "password" && action == "generate") ||
		(group == "totp" && action == "code")
}

func shellAssignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	if !ok || name == "" {
		return false
	}
	for index, r := range name {
		if r != '_' && !unicode.IsLetter(r) && (index == 0 || !unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}

// shellCommandSegments is a deliberately small shell lexer. It recognizes the
// control operators that start a new command, strips quotes, and preserves a
// quoted `sh -c` body as one word for bounded recursive inspection.
func shellCommandSegments(command string) [][]string {
	var segments [][]string
	var words []string
	var word strings.Builder
	var quote rune
	escaped := false
	flushWord := func() {
		if word.Len() != 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	flushSegment := func() {
		flushWord()
		if len(words) != 0 {
			segments = append(segments, words)
			words = nil
		}
	}
	for _, r := range command {
		if escaped {
			word.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		switch r {
		case ' ', '\t', '\r':
			flushWord()
		case '\n', ';', '|', '&', '(', ')':
			flushSegment()
		default:
			word.WriteRune(r)
		}
	}
	if escaped {
		word.WriteRune('\\')
	}
	flushSegment()
	return segments
}

func redactSensitiveToolPayload(input *hookInput) {
	for _, raw := range []*json.RawMessage{
		&input.ToolInput, &input.ToolInputCamel, &input.ToolResponse,
		&input.ToolOutput, &input.ToolOutputCamel, &input.Error,
	} {
		clear(*raw)
		*raw = nil
	}
	input.ErrorMessage = ""
	input.Reason = ""
}

// readCompleteGrokAssistantTurn accounts for Grok's Stop-hook ordering: the
// provider persists the final assistant chunk only after the synchronous Stop
// hook returns. A one-shot transcript flusher may already be waiting outside
// that hook, so poll the trusted native file for the provider's exact
// turn_completed fence. Quiet time alone is not proof that a response is
// complete.
func readCompleteGrokAssistantTurn(path, promptID, expectedSessionID string) (string, string, bool, error) {
	return readCompleteGrokAssistantTurnWithin(path, promptID, expectedSessionID,
		grokTranscriptMaxWait, grokTranscriptPollInterval)
}

func readCompleteGrokAssistantTurnWithin(
	path, promptID, expectedSessionID string,
	maxWait, pollInterval time.Duration,
) (string, string, bool, error) {
	deadline := time.Now().Add(maxWait)
	body, model, complete, err := readGrokAssistantTurn(path, promptID, expectedSessionID)
	if err != nil {
		return "", "", false, err
	}
	if complete {
		return body, model, true, nil
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", false, err
	}
	// Force one re-read after establishing the polling loop. This closes the
	// window where the provider appends between the initial parse and the first
	// file stat, which would otherwise make the new size look like the baseline.
	lastSize := int64(-1)
	lastModTime := time.Time{}
	completeLine := false
	lastBody := body
	lastModel := model

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return lastBody, lastModel, false, nil
		}
		time.Sleep(min(pollInterval, remaining))

		size, modTime, complete, err := grokTranscriptFileState(resolvedPath)
		if err != nil {
			return "", "", false, err
		}
		if size != lastSize || !modTime.Equal(lastModTime) || complete != completeLine {
			lastSize, lastModTime, completeLine = size, modTime, complete

			body, model, complete, err = readGrokAssistantTurn(resolvedPath, promptID, expectedSessionID)
			if err != nil {
				return "", "", false, err
			}
			lastBody = body
			if model != "" {
				lastModel = model
			}
			if complete {
				return lastBody, lastModel, true, nil
			}
		}
	}
}

func grokTranscriptFileState(path string) (int64, time.Time, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, time.Time{}, false, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return 0, time.Time{}, false, err
	}
	if !info.Mode().IsRegular() {
		return 0, time.Time{}, false, errors.New("grok transcript is not a regular file")
	}
	if info.Size() == 0 {
		return 0, info.ModTime(), false, nil
	}
	last := []byte{0}
	if _, err := file.ReadAt(last, info.Size()-1); err != nil {
		return 0, time.Time{}, false, err
	}
	return info.Size(), info.ModTime(), last[0] == '\n', nil
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

// NormalizeUserPromptBody removes only a provider-generated wrapper whose
// exact grammar is known for the selected runtime. Unknown, malformed, or
// ambiguous input is preserved byte-for-byte so capture never guesses at the
// user's text.
func NormalizeUserPromptBody(runtime, prompt string) string {
	if runtime != RuntimeCursor {
		return prompt
	}
	if body, ok := unwrapCursorPrompt(prompt); ok {
		return body
	}
	return prompt
}

func unwrapCursorPrompt(prompt string) (string, bool) {
	const (
		timestampOpen     = "<timestamp>"
		timestampClose    = "</timestamp>"
		queryOpen         = "<user_query>"
		queryClose        = "</user_query>"
		queryPrefix       = "\n" + queryOpen + "\n"
		querySuffix       = "\n" + queryClose
		maxTimestampBytes = 256
	)
	for _, tag := range []string{timestampOpen, timestampClose, queryOpen, queryClose} {
		if strings.Count(prompt, tag) != 1 {
			return "", false
		}
	}
	if !strings.HasPrefix(prompt, timestampOpen) {
		return "", false
	}
	timestampEnd := strings.Index(prompt, timestampClose)
	if timestampEnd < len(timestampOpen) {
		return "", false
	}
	timestamp := prompt[len(timestampOpen):timestampEnd]
	if len(timestamp) > maxTimestampBytes || strings.TrimSpace(timestamp) == "" || cursorTimestampHasForbiddenRune(timestamp) {
		return "", false
	}
	remainder := prompt[timestampEnd+len(timestampClose):]
	if !strings.HasPrefix(remainder, queryPrefix) || !strings.HasSuffix(remainder, querySuffix) {
		return "", false
	}
	body := remainder[len(queryPrefix) : len(remainder)-len(querySuffix)]
	// Wrapper-like markup inside the candidate body is ambiguous. Preserve the
	// original envelope even when the outer bytes happen to be well formed.
	lowerBody := strings.ToLower(body)
	for _, marker := range []string{"<timestamp", "</timestamp", "<user_query", "</user_query"} {
		if strings.Contains(lowerBody, marker) {
			return "", false
		}
	}
	return body, true
}

func cursorTimestampHasForbiddenRune(timestamp string) bool {
	for _, r := range timestamp {
		switch r {
		case '<', '>', '\r', '\n', '\u0085', '\u2028', '\u2029':
			return true
		}
	}
	return false
}

type cursorTranscriptRecord struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Role    string `json:"role"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type cursorTranscriptContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func cursorNativeTranscriptMessages(base Event, path string) ([]RecoveredMessage, error) {
	promptBody, assistantBody, err := readCursorVisibleMessages(path, base.SessionID)
	if err != nil {
		return nil, err
	}
	turnID := cursorRecoveredID("turn", base.SessionID, "prompt", promptBody)
	promptID := cursorRecoveredID("evt", base.SessionID, "prompt", promptBody)
	responseID := cursorRecoveredID("evt", base.SessionID, "response", promptBody, assistantBody)
	data, _ := json.Marshal(map[string]any{
		"source":   "cursor_native_transcript",
		"fallback": true,
	})
	return []RecoveredMessage{
		{
			ID: promptID, TurnID: turnID,
			HookEvent: "UserPromptSubmit", NativeHookEvent: "nativeTranscriptUser",
			Kind: "message.user", Role: "user", Body: promptBody, Data: data,
		},
		{
			ID: responseID, TurnID: turnID,
			HookEvent: "AgentResponse", NativeHookEvent: "nativeTranscriptAssistant",
			Kind: "message.assistant", Role: "assistant", Body: assistantBody, Data: data,
			ReplyToEventID: promptID,
		},
	}, nil
}

func cursorRecoveredID(prefix, sessionID string, parts ...string) string {
	value := "cursor\x00" + sessionID + "\x00" + strings.Join(parts, "\x00")
	sum := sha256.Sum256([]byte(value))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func readCursorVisibleMessages(path, expectedSessionID string) (string, string, error) {
	home := strings.TrimSpace(os.Getenv("CURSOR_DATA_DIR"))
	if home == "" {
		home = strings.TrimSpace(os.Getenv("CURSOR_CONFIG_DIR"))
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		home = filepath.Join(userHome, ".cursor")
	}
	projectsRoot, err := filepath.EvalSymlinks(filepath.Join(home, "projects"))
	if err != nil {
		return "", "", err
	}
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return "", "", err
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("cursor transcript path must not be a symlink")
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(projectsRoot, resolvedPath)
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		filepath.Ext(resolvedPath) != ".jsonl" || len(parts) != 4 ||
		parts[len(parts)-3] != "agent-transcripts" ||
		strings.TrimSuffix(parts[len(parts)-1], ".jsonl") != parts[len(parts)-2] ||
		strings.TrimSpace(expectedSessionID) == "" || parts[len(parts)-2] != expectedSessionID {
		return "", "", errors.New("cursor transcript path is outside the agent transcript store")
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
	if !info.Mode().IsRegular() || !os.SameFile(linkInfo, info) ||
		!trustedPathIdentity(resolvedPath, info) ||
		info.Size() > maxNativeTranscriptTailBytes || !trustedNativeDirectoryChain(projectsRoot, resolvedPath) {
		return "", "", errors.New("cursor transcript is not a bounded trusted regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxNativeTranscriptTailBytes+1))
	if err != nil {
		return "", "", err
	}
	if len(raw) > maxNativeTranscriptTailBytes {
		return "", "", errors.New("cursor transcript exceeds the bounded read limit")
	}

	userCount := 0
	prompt := ""
	assistant := ""
	recordCount := 0
	terminalCount := 0
	lastRecordKind := ""
	terminalSuccess := false
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		recordCount++
		if recordCount > maxNativeTranscriptRecords || len(line) > maxNativeTranscriptLineBytes {
			return "", "", errors.New("cursor transcript exceeds the bounded record limit")
		}
		var record cursorTranscriptRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return "", "", fmt.Errorf("parse cursor transcript: %w", err)
		}
		if strings.TrimSpace(record.Role) == "" {
			if record.Type != "turn_ended" {
				return "", "", errors.New("cursor transcript contains an unsupported record")
			}
			terminalCount++
			terminalSuccess = record.Status == "success"
			lastRecordKind = "terminal"
			continue
		}
		if record.Type != "" {
			return "", "", errors.New("cursor transcript message record has an unexpected type")
		}
		lastRecordKind = "message"
		body, err := cursorVisibleText(record.Message.Content)
		if err != nil {
			return "", "", err
		}
		switch strings.ToLower(strings.TrimSpace(record.Role)) {
		case "user":
			if strings.TrimSpace(body) == "" {
				continue
			}
			userCount++
			prompt = NormalizeUserPromptBody(RuntimeCursor, body)
		case "assistant":
			if userCount == 1 && strings.TrimSpace(body) != "" {
				// Cursor records tool-turn status text and the final answer as
				// separate assistant messages. Retain only the last visible text
				// message so the fallback never promotes intermediate reasoning.
				assistant = body
			}
		default:
			return "", "", errors.New("cursor transcript contains an unsupported message role")
		}
	}
	if userCount != 1 || strings.TrimSpace(prompt) == "" || strings.TrimSpace(assistant) == "" ||
		terminalCount != 1 || lastRecordKind != "terminal" || !terminalSuccess {
		return "", "", errors.New("cursor headless transcript requires exactly one visible user prompt and one assistant response")
	}
	return prompt, assistant, nil
}

func trustedNativeDirectoryChain(projectsRoot, transcriptPath string) bool {
	dir := filepath.Dir(transcriptPath)
	for {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || !trustedPathIdentity(dir, info) {
			return false
		}
		if dir == projectsRoot {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func cursorVisibleText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var blocks []cursorTranscriptContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errors.New("cursor transcript message content has an unsupported shape")
	}
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n\n"), nil
}

type grokSessionUpdateEnvelope struct {
	Params struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
		} `json:"update"`
	} `json:"params"`
}

type grokSessionUpdate struct {
	Params struct {
		Meta struct {
			PromptID string `json:"promptId"`
		} `json:"_meta"`
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			PromptID      string `json:"prompt_id"`
			EventName     string `json:"event_name"`
			Meta          struct {
				ModelID string `json:"modelId"`
			} `json:"_meta"`
			Content json.RawMessage `json:"content"`
		} `json:"update"`
	} `json:"params"`
}

func grokAssistantChunkText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("grok agent message chunk content must contain a string text field")
	}
	var content map[string]json.RawMessage
	if err := json.Unmarshal(raw, &content); err != nil || content == nil {
		return "", errors.New("grok agent message chunk content must contain a string text field")
	}
	rawText, ok := content["text"]
	if !ok {
		return "", errors.New("grok agent message chunk content must contain a string text field")
	}
	var value any
	if err := json.Unmarshal(rawText, &value); err != nil {
		return "", errors.New("grok agent message chunk content must contain a string text field")
	}
	text, ok := value.(string)
	if !ok {
		return "", errors.New("grok agent message chunk content must contain a string text field")
	}
	return text, nil
}

func readGrokAssistantTurn(path, promptID, expectedSessionID string) (string, string, bool, error) {
	if strings.TrimSpace(promptID) == "" || strings.TrimSpace(expectedSessionID) == "" {
		return "", "", false, errors.New("grok transcript lookup requires prompt and session ids")
	}
	home := strings.TrimSpace(os.Getenv("GROK_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", "", false, err
		}
		home = filepath.Join(userHome, ".grok")
	}
	sessionsRoot, err := filepath.EvalSymlinks(filepath.Join(home, "sessions"))
	if err != nil {
		return "", "", false, err
	}
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return "", "", false, err
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", false, errors.New("grok transcript path must not be a symlink")
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", false, err
	}
	rel, err := filepath.Rel(sessionsRoot, resolvedPath)
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		len(parts) != 3 || parts[2] != "updates.jsonl" || parts[1] != expectedSessionID {
		return "", "", false, errors.New("grok transcript path is outside the session store")
	}

	file, err := os.Open(resolvedPath)
	if err != nil {
		return "", "", false, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", "", false, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(linkInfo, info) ||
		!trustedPathIdentity(resolvedPath, info) ||
		!trustedNativeDirectoryChain(sessionsRoot, resolvedPath) {
		return "", "", false, errors.New("grok transcript is not a trusted regular file")
	}
	if info.Size() == 0 {
		return "", "", false, nil
	}
	last := []byte{0}
	if _, err := file.ReadAt(last, info.Size()-1); err != nil {
		return "", "", false, err
	}
	scanStart, err := grokTranscriptTailStart(file, info.Size(), maxNativeTranscriptScanBytes)
	if err != nil {
		return "", "", false, err
	}
	if scanStart >= info.Size() {
		return "", "", false, nil
	}

	var chunks []string
	model := ""
	stopSeen := false
	complete := false
	lineIndex := 0
	recordsAfterStop := 0
	bodyBytes := 0
	malformedAfterStopAt := 0
	unsupportedAfterStopAt := 0
	// Scan exactly the same append-only prefix whose final byte was inspected
	// above. Bytes appended concurrently belong to the next poll; mixing them
	// into this scan would misclassify a normal partial append as corruption.
	scanner := bufio.NewScanner(io.NewSectionReader(file, scanStart, info.Size()-scanStart))
	scanner.Buffer(make([]byte, 64*1024), maxNativeTranscriptLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		lineIndex++
		if stopSeen {
			recordsAfterStop++
		}
		if recordsAfterStop > maxNativeTranscriptRecords {
			return "", "", false, errors.New("grok transcript exceeds recovery bounds")
		}
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			if stopSeen && malformedAfterStopAt == 0 {
				malformedAfterStopAt = lineIndex
			}
			continue
		}
		var envelope grokSessionUpdateEnvelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			if stopSeen && unsupportedAfterStopAt == 0 {
				unsupportedAfterStopAt = lineIndex
			}
			continue
		}
		kind := envelope.Params.Update.SessionUpdate
		switch kind {
		case "hook_execution", "turn_completed", "agent_message_chunk":
		default:
			continue
		}
		var update grokSessionUpdate
		if err := json.Unmarshal(line, &update); err != nil {
			if stopSeen && unsupportedAfterStopAt == 0 {
				unsupportedAfterStopAt = lineIndex
			}
			continue
		}
		if kind == "hook_execution" && update.Params.Update.EventName == "stop" &&
			update.Params.Update.PromptID == promptID {
			stopSeen = true
			complete = false
			chunks = nil
			model = ""
			bodyBytes = 0
			recordsAfterStop = 1
			continue
		}
		if !stopSeen {
			continue
		}
		if kind == "turn_completed" && update.Params.Update.PromptID == promptID {
			complete = true
			continue
		}
		if kind != "agent_message_chunk" || update.Params.Meta.PromptID != promptID {
			continue
		}
		// Any exact-prompt assistant chunk after an apparent terminal fence
		// means the earlier fence was not the end of this turn.
		complete = false
		if value := strings.TrimSpace(update.Params.Update.Meta.ModelID); value != "" {
			model = truncateUTF8(value, 256)
		}
		text, err := grokAssistantChunkText(update.Params.Update.Content)
		if err != nil {
			if unsupportedAfterStopAt == 0 {
				unsupportedAfterStopAt = lineIndex
			}
			continue
		}
		if text != "" {
			bodyBytes += len(text)
			if bodyBytes > maxNativeTranscriptTailBytes {
				return "", "", false, errors.New("grok assistant response exceeds recovery bounds")
			}
			chunks = append(chunks, text)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", false, fmt.Errorf("scan grok transcript: %w", err)
	}
	// A parseable final line without a newline can still be in the middle of
	// an append. Never promote it into the portable transcript.
	if last[0] != '\n' {
		complete = false
		if malformedAfterStopAt == lineIndex {
			malformedAfterStopAt = 0
		}
		if unsupportedAfterStopAt == lineIndex {
			unsupportedAfterStopAt = 0
		}
	}
	if malformedAfterStopAt != 0 {
		return "", "", false, errors.New("grok transcript contains a malformed record after the Stop fence")
	}
	if unsupportedAfterStopAt != 0 {
		return "", "", false, errors.New("grok transcript contains an unsupported relevant record after the Stop fence")
	}
	return strings.Join(chunks, "\n\n"), model, complete, nil
}

func grokTranscriptTailStart(file *os.File, size int64, window int) (int64, error) {
	scanStart := max(size-int64(window), 0)
	if scanStart == 0 {
		return 0, nil
	}
	previous := []byte{0}
	if _, err := file.ReadAt(previous, scanStart-1); err != nil {
		return 0, err
	}
	if previous[0] == '\n' {
		return scanStart, nil
	}

	// Drop only a partial record at the bounded-tail boundary. Recovery is
	// complete only when the exact Stop anchor is present after it, so no part
	// of the selected turn can be silently omitted.
	boundaryStart := scanStart
	probe := make([]byte, 64*1024)
	for scanStart < size {
		remaining := size - scanStart
		chunk := probe
		if remaining < int64(len(chunk)) {
			chunk = chunk[:remaining]
		}
		n, readErr := file.ReadAt(chunk, scanStart)
		if n > 0 {
			if newline := bytes.IndexByte(chunk[:n], '\n'); newline >= 0 {
				aligned := scanStart + int64(newline+1)
				if aligned-boundaryStart-1 > maxNativeTranscriptLineBytes {
					return 0, errors.New("grok transcript boundary record exceeds recovery bounds")
				}
				return aligned, nil
			}
			scanStart += int64(n)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return 0, readErr
		}
		if scanStart-boundaryStart > maxNativeTranscriptLineBytes {
			return 0, errors.New("grok transcript boundary record exceeds recovery bounds")
		}
	}
	return scanStart, nil
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
		event.Kind, event.Role = "message.assistant", "assistant"
		if input.SensitiveTurnContent {
			event.Body = "response omitted from portable transcript because this turn used sealed secrets"
		} else {
			event.Body = input.LastAssistantMessage
		}
	case "AgentThought":
		event.Kind, event.Role = "agent.thought", "system"
		if input.SensitiveTurnContent {
			event.Body = "thought omitted from portable transcript because this turn used sealed secrets"
		} else {
			event.Body = input.LastAssistantMessage
		}
	case "Stop":
		if input.SyntheticFence {
			event.Kind, event.Role, event.Body = "turn.completed", "system", "delegation job completed"
			// A sealed turn clears the reason before this runs, so the body of
			// a rollover fence cannot be derived from it.
			if input.FenceRunRollover {
				event.Body = "run completed when the session restarted"
			}
		} else if input.LastAssistantMessage != "" {
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
	case HookEventCodexPermissionReview:
		event.Kind, event.Role, event.Body = "permission.review.started", "system", "automatic permission review started"
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
	// The internal review envelope may serialize parent context and tool input.
	// Its normalized audit metadata is sufficient; never persist the envelope
	// or make it available to later transcript curation in any capture mode.
	if input.HookEventName == HookEventCodexPermissionReview {
		return
	}
	event.Data = structuredEventData(input)
	if event.CaptureMode == ModeRaw && !input.SyntheticFence && !input.SensitiveToolEvent && !input.SensitiveTurnContent {
		event.Raw = append(json.RawMessage(nil), raw...)
	}
}

func structuredEventData(input hookInput) json.RawMessage {
	if input.SensitiveTurnContent {
		data := map[string]any{"sealed_content_omitted": true}
		if input.SyntheticFence {
			data["synthetic_fence"] = true
		}
		if status := valueFreeToolStatus(input.Status); status != "" {
			data["status"] = status
		}
		raw, _ := json.Marshal(data)
		return raw
	}
	if input.SensitiveToolEvent {
		data := map[string]any{}
		if status := valueFreeToolStatus(input.Status); status != "" {
			data["status"] = status
		}
		data["tool"] = compactMap(map[string]any{
			"name":   input.ToolName,
			"use_id": input.ToolUseID,
		})
		raw, _ := json.Marshal(data)
		return raw
	}
	data := map[string]any{}
	if input.SyntheticFence {
		data["synthetic_fence"] = true
	}
	if input.PromptID != "" {
		data["prompt_id"] = input.PromptID
	}
	if input.DSHTurnOrdinal > 0 {
		data["dsh_turn_ordinal"] = input.DSHTurnOrdinal
	}
	if input.DSHPromptSHA256 != "" {
		data["dsh_prompt_sha256"] = input.DSHPromptSHA256
	}
	if input.DSHPromptHookSeq > 0 {
		data["dsh_prompt_hook_seq"] = input.DSHPromptHookSeq
	}
	if input.DSHPromptAfterSeq > 0 {
		data["dsh_prompt_after_seq"] = input.DSHPromptAfterSeq
	}
	if input.DSHStopOrdinal > 0 {
		data["dsh_stop_ordinal"] = input.DSHStopOrdinal
	}
	if len(input.DSHToolUseIDs) != 0 {
		data["dsh_hooked_tool_use_ids"] = input.DSHToolUseIDs
	}
	if input.DSHToolUseOverflow {
		data["dsh_hooked_tool_use_overflow"] = true
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

func valueFreeToolStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok", "success", "succeeded", "complete", "completed":
		return "completed"
	case "error", "failure", "failed":
		return "failed"
	case "cancelled", "canceled":
		return "cancelled"
	case "denied":
		return "denied"
	case "timeout", "timed_out", "timedout":
		return "timed_out"
	case "pending", "running", "interrupted":
		return strings.ToLower(strings.TrimSpace(status))
	default:
		return ""
	}
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
	if len(e.RecoveredMessages) == 0 {
		return e.entriesWithoutRecovered()
	}

	// Recovery is persisted atomically with the terminal event in one outbox
	// file. Expand it only at flush time, preserving visible turn order while
	// keeping the provider's SessionEnd entry last.
	recovered := append([]RecoveredMessage(nil), e.RecoveredMessages...)
	e.RecoveredMessages = nil
	entries := make([]Entry, 0, len(recovered)+1)
	for _, message := range recovered {
		child := e
		child.ID = message.ID
		child.TurnID = message.TurnID
		child.HookEvent = message.HookEvent
		child.NativeHookEvent = message.NativeHookEvent
		child.Kind = message.Kind
		child.Role = message.Role
		child.Body = message.Body
		child.Data = message.Data
		child.ReplyToEventID = message.ReplyToEventID
		child.Raw = nil
		// The native transcript has no trustworthy per-message timestamp or
		// Witself run id. Omitting the terminal hook's changing values keeps a
		// duplicate SessionEnd retry byte-identical at the server idempotency
		// boundary. Transcript sequence still preserves prompt/response order.
		child.RunID = ""
		child.OccurredAt = time.Time{}
		entries = append(entries, child.entriesWithoutRecovered()...)
	}
	return append(entries, e.entriesWithoutRecovered()...)
}

// entryPayloadFits adds key/value to payload only when the encoded payload
// stays within maxEntryPayloadBytes; otherwise payload is left unchanged.
func entryPayloadFits(payload map[string]any, key string, value any) bool {
	payload[key] = value
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) > maxEntryPayloadBytes {
		delete(payload, key)
		return false
	}
	return true
}

func (e Event) entriesWithoutRecovered() []Entry {
	if e.Runtime == RuntimeDSH {
		// Prompt correlation is private local state, including for unresolved
		// or operator-fenced events projected directly by a caller.
		e.Data = withoutDSHNativeRetry(withoutDSHPromptDigest(e.Data))
	}
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
			"chunk_index":    i,
			"chunk_count":    len(chunks),
			"provenance":     e.provenancePayload(),
		}
		if e.RunID != "" {
			payload["run_id"] = e.RunID
		}
		if !e.OccurredAt.IsZero() {
			payload["occurred_at"] = e.OccurredAt
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
		if i == 0 {
			// Budget the optional data and raw copies against the server's
			// entry payload cap. Data (the normalized capture content) is
			// admitted first; the raw hook payload only if it still fits.
			// Whatever does not fit is replaced by a digest marker so the
			// entry stays value-free and byte-identical on every retry.
			if len(e.Data) > 0 {
				var value any
				if json.Unmarshal(e.Data, &value) == nil && !entryPayloadFits(payload, "data", value) {
					sum := sha256.Sum256(e.Data)
					payload["data_omitted"] = true
					payload["data_bytes"] = len(e.Data)
					payload["data_sha256"] = hex.EncodeToString(sum[:])
				}
			}
			if len(e.Raw) > 0 {
				embedded := false
				if len(e.Raw) <= maxRawPayloadBytes {
					var value any
					if json.Unmarshal(e.Raw, &value) == nil {
						embedded = entryPayloadFits(payload, "raw", value)
					}
				}
				if !embedded && (len(e.Raw) > maxRawPayloadBytes || json.Valid(e.Raw)) {
					sum := sha256.Sum256(e.Raw)
					payload["raw_omitted"] = true
					payload["raw_bytes"] = len(e.Raw)
					payload["raw_sha256"] = hex.EncodeToString(sum[:])
				}
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
		if errors.Is(err, os.ErrNotExist) {
			// A concurrent flush acknowledged and removed this event between
			// the glob and the read. It is no longer pending.
			continue
		}
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

// RedactPendingTurn atomically replaces every still-local event body for one
// provider turn with a value-free marker. Flushing is turn-gated, so a prompt
// cannot be uploaded before a later sealed tool call has had this opportunity
// to suppress plaintext that the user placed in that prompt.
func RedactPendingTurn(runtime, sessionID, turnID string) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(turnID) == "" {
		return errors.New("pending transcript turn identity is invalid")
	}
	pending, err := Pending(runtime)
	if err != nil {
		return err
	}
	for _, item := range pending {
		event := item.Event
		if event.Runtime != runtime || event.SessionID != sessionID || event.TurnID != turnID {
			continue
		}
		switch event.Kind {
		case "message.user":
			event.Body = "prompt omitted from portable transcript because this turn used sealed secrets"
		case "message.assistant":
			event.Body = "response omitted from portable transcript because this turn used sealed secrets"
		case "agent.thought":
			event.Body = "thought omitted from portable transcript because this turn used sealed secrets"
		case "tool.call", "tool.result", "tool.error":
			event.Body = "tool payload omitted from portable transcript because this turn used sealed secrets"
		default:
			event.Body = "event content omitted from portable transcript because this turn used sealed secrets"
		}
		event.Raw = nil
		event.RecoveredMessages = nil
		event.Data = json.RawMessage(`{"sealed_content_omitted":true}`)
		if err := validatePendingRewritePath(item.Path, event); err != nil {
			return err
		}
		if err := writeJSONAtomic(item.Path, event); err != nil {
			return fmt.Errorf("redact pending sealed transcript turn: %w", err)
		}
	}
	return nil
}

// NewReadinessIndex builds the per-flush lookup used by UploadReady. Building
// it scans the pending slice once, loads each turn's durable fence once, then
// sorts each session's prompt timeline. Missing or unreadable markers grant no
// additional readiness.
func NewReadinessIndex(all []PendingEvent) *ReadinessIndex {
	index := &ReadinessIndex{
		sessionEnds:     make(map[readinessSessionKey]time.Time),
		terminals:       make(map[readinessTurnKey]time.Time),
		promptTimelines: make(map[readinessSessionKey]readinessPromptTimeline),
		completedFences: make(map[readinessFenceKey]completedFence),
	}
	promptsBySession := make(map[readinessSessionKey][]readinessPrompt)
	for _, pending := range all {
		event := pending.Event
		session := readinessSessionKey{
			transcriptID: event.TranscriptExternalID(),
			sessionID:    event.SessionID,
		}
		fenceKey := readinessFenceKey{session: session, runID: event.RunID, turnID: event.TurnID}
		if _, loaded := index.completedFences[fenceKey]; !loaded {
			index.completedFences[fenceKey] = loadCompletedFence(event)
		}
		switch event.HookEvent {
		case "SessionEnd":
			if latest, exists := index.sessionEnds[session]; !exists || event.OccurredAt.After(latest) {
				index.sessionEnds[session] = event.OccurredAt
			}
		case "AgentResponse", "Stop", "StopFailure":
			turn := readinessTurnKey{session: session, turnID: event.TurnID}
			if latest, exists := index.terminals[turn]; !exists || event.OccurredAt.After(latest) {
				index.terminals[turn] = event.OccurredAt
			}
		case "UserPromptSubmit":
			promptsBySession[session] = append(promptsBySession[session], readinessPrompt{
				occurredAt: event.OccurredAt,
				turnID:     event.TurnID,
			})
		}
	}
	for session, prompts := range promptsBySession {
		sort.SliceStable(prompts, func(i, j int) bool {
			return prompts[i].occurredAt.Before(prompts[j].occurredAt)
		})
		timeline := readinessPromptTimeline{
			prompts:            prompts,
			suffixFirstTurn:    make([]string, len(prompts)),
			suffixHasOtherTurn: make([]bool, len(prompts)),
		}
		for i := len(prompts) - 1; i >= 0; i-- {
			timeline.suffixFirstTurn[i] = prompts[i].turnID
			if i+1 < len(prompts) {
				timeline.suffixFirstTurn[i] = timeline.suffixFirstTurn[i+1]
				timeline.suffixHasOtherTurn[i] = timeline.suffixHasOtherTurn[i+1] ||
					prompts[i].turnID != timeline.suffixFirstTurn[i+1]
			}
		}
		index.promptTimelines[session] = timeline
	}
	return index
}

// UploadReady reports whether enough of the local provider turn is present to
// know whether its content must be suppressed. Turn content stays local until
// AgentResponse, Stop/StopFailure, SessionEnd, or the next real user prompt.
// This closes the prompt-enqueue versus later secret-tool race.
func (index *ReadinessIndex) UploadReady(current PendingEvent) bool {
	event := current.Event
	if strings.TrimSpace(event.TurnID) == "" {
		return true
	}
	session := readinessSessionKey{
		transcriptID: event.TranscriptExternalID(),
		sessionID:    event.SessionID,
	}
	fence := index.completedFences[readinessFenceKey{session: session, runID: event.RunID, turnID: event.TurnID}]
	if !fence.OccurredAt.IsZero() && !fence.OccurredAt.Before(event.OccurredAt) {
		// Pending may have read this snapshot before a sealed hook rewrote the
		// outbox, while the marker became visible afterward. Only the redacted
		// snapshot may use sealed completion; a fresh flush rereads the file.
		return !fence.SealedContentOmitted ||
			(eventSealedContentOmitted(event.Data) && len(event.Raw) == 0 && len(event.RecoveredMessages) == 0)
	}
	if endedAt, exists := index.sessionEnds[session]; exists && !endedAt.Before(event.OccurredAt) {
		return true
	}
	turn := readinessTurnKey{session: session, turnID: event.TurnID}
	terminalAt, terminalExists := index.terminals[turn]
	if terminalExists && !terminalAt.Before(event.OccurredAt) {
		return true
	}
	if timeline, exists := index.promptTimelines[session]; exists &&
		timeline.hasDifferentTurnAfter(event.OccurredAt, event.TurnID) {
		return true
	}
	return false
}

func (timeline readinessPromptTimeline) hasDifferentTurnAfter(occurredAt time.Time, turnID string) bool {
	firstLater := sort.Search(len(timeline.prompts), func(i int) bool {
		return timeline.prompts[i].occurredAt.After(occurredAt)
	})
	if firstLater == len(timeline.prompts) {
		return false
	}
	return timeline.suffixFirstTurn[firstLater] != turnID || timeline.suffixHasOtherTurn[firstLater]
}

// PendingEventUploadReady retains the original convenience API. Flush callers
// should build one ReadinessIndex and reuse it for the complete pending slice.
func PendingEventUploadReady(current PendingEvent, all []PendingEvent) bool {
	return NewReadinessIndex(all).UploadReady(current)
}

// FinalizePending prepares one locally durable event for upload. Grok writes
// its final response only after the synchronous Stop hook returns, so an
// unresolved Stop remains in the outbox until the trusted native transcript
// contains the matching completed turn. The original event is rewritten
// atomically before any network operation, preserving byte-identical retry
// behavior at the server idempotency boundary.
//
// The caller must hold the runtime flush lock. ready=false is a normal,
// retryable state; a later hook or an explicit transcript flush will try again.
func FinalizePending(pending PendingEvent) (finalized PendingEvent, ready bool, err error) {
	if pending.Event.Runtime == RuntimeDSH {
		return finalizePendingWithin(pending, dshSessionLogMaxWait, dshSessionLogPollInterval)
	}
	return finalizePendingWithin(pending, grokTranscriptMaxWait, grokTranscriptPollInterval)
}

func finalizePendingWithin(
	pending PendingEvent,
	maxWait, pollInterval time.Duration,
) (finalized PendingEvent, ready bool, err error) {
	event := pending.Event
	if event.Runtime != RuntimeGrokBuild && event.Runtime != RuntimeDSH {
		return pending, true, nil
	}
	if event.HookEvent != "Stop" || event.Kind != "turn.completed" || event.Role != "system" {
		return pending, true, nil
	}
	if event.NativeTurnFinalized {
		return pending, true, nil
	}
	if event.Runtime == RuntimeDSH {
		return finalizePendingDSH(pending, event, maxWait, pollInterval)
	}
	if eventSyntheticFence(event.Data) {
		// A companion fence is a value-free completion marker, not an
		// unresolved provider turn. It has no prompt id to correlate and must
		// never be rehydrated from the native transcript.
		return pending, true, nil
	}
	if eventSealedContentOmitted(event.Data) {
		// Never rehydrate a sealed turn from Grok's native transcript: its final
		// assistant chunk may contain the authorized revealed value that the
		// value-free Stop hook deliberately suppressed.
		event.NativeTurnFinalized = true
		if err := validatePendingRewritePath(pending.Path, event); err != nil {
			return pending, false, err
		}
		if err := writeJSONAtomic(pending.Path, event); err != nil {
			return pending, false, fmt.Errorf("persist suppressed grok Stop event: %w", err)
		}
		return PendingEvent{Path: pending.Path, Event: event}, true, nil
	}
	if strings.TrimSpace(event.SourceTranscriptPath) == "" {
		return pending, false, errors.New("unresolved grok Stop event has no native transcript path")
	}
	var data struct {
		PromptID string `json:"prompt_id"`
	}
	if len(event.Data) == 0 || json.Unmarshal(event.Data, &data) != nil || strings.TrimSpace(data.PromptID) == "" {
		return pending, false, errors.New("unresolved grok Stop event has no durable prompt id")
	}

	body, model, complete, err := readCompleteGrokAssistantTurnWithin(
		event.SourceTranscriptPath, strings.TrimSpace(data.PromptID), event.SessionID,
		maxWait, pollInterval,
	)
	if err != nil {
		return pending, false, err
	}
	if !complete {
		return pending, false, nil
	}
	event.NativeTurnFinalized = true
	if body == "" {
		// A provider-fenced turn with no assistant body is a legitimate
		// content-free completion, not a partially captured message. Persist
		// the finalization marker before upload so a retry never depends on
		// mutable or pruned native session storage.
		if err := validatePendingRewritePath(pending.Path, event); err != nil {
			return pending, false, err
		}
		if err := writeJSONAtomic(pending.Path, event); err != nil {
			return pending, false, fmt.Errorf("persist finalized grok Stop event: %w", err)
		}
		return PendingEvent{Path: pending.Path, Event: event}, true, nil
	}

	event.Kind = "message.assistant"
	event.Role = "assistant"
	event.Body = body
	if event.Model == "" && model != "" {
		event.Model = model
		event.ModelSource = "native_transcript"
	}
	if err := validatePendingRewritePath(pending.Path, event); err != nil {
		return pending, false, err
	}
	if err := writeJSONAtomic(pending.Path, event); err != nil {
		return pending, false, fmt.Errorf("persist finalized grok Stop event: %w", err)
	}
	return PendingEvent{Path: pending.Path, Event: event}, true, nil
}

// finalizePendingDSH resolves one DeepSeek Harness Stop event. The
// claude-code hook bridge sends a Stop payload carrying no assistant text and
// an empty transcript_path, so the turn's visible content exists only in the
// session log the harness writes under its own session store. The log is
// located from the session id, the hook's cwd, and the prompt ordinal the
// event carries.
func finalizePendingDSH(
	pending PendingEvent, event Event, maxWait, pollInterval time.Duration,
) (PendingEvent, bool, error) {
	if eventSyntheticFence(event.Data) {
		// A fence is a value-free completion marker for a turn the session no
		// longer holds. It has no prompt of its own to rehydrate.
		return pending, true, nil
	}
	if eventSealedContentOmitted(event.Data) {
		// Never rehydrate a sealed turn: the harness log holds the same
		// authorized revealed value the value-free Stop hook suppressed.
		event.NativeTurnFinalized = true
		return persistFinalizedDSHEvent(pending.Path, event)
	}
	var data struct {
		Ordinal            int      `json:"dsh_turn_ordinal"`
		PromptSHA256       string   `json:"dsh_prompt_sha256"`
		PromptHookSeq      int64    `json:"dsh_prompt_hook_seq"`
		PromptAfterSeq     int64    `json:"dsh_prompt_after_seq"`
		StopOrdinal        int      `json:"dsh_stop_ordinal"`
		HookedTool         []string `json:"dsh_hooked_tool_use_ids"`
		HookedToolOverflow bool     `json:"dsh_hooked_tool_use_overflow"`
	}
	if len(event.Data) != 0 {
		_ = json.Unmarshal(event.Data, &data)
	}
	pending, err := initDSHNativeRetry(pending)
	if err != nil {
		return pending, false, err
	}
	event = pending.Event
	var retry dshNativeRetryState
	_ = json.Unmarshal(event.Data, &retry)
	maxWait = min(maxWait, max(time.Until(retry.Deadline), 0))
	turn, err := readCompleteDSHTurnWithin(event.SessionID, event.CWD, data.Ordinal, maxWait, pollInterval,
		dshTurnCorrelation{PromptSHA256: data.PromptSHA256, PromptHookSeq: data.PromptHookSeq, PromptAfterSeq: data.PromptAfterSeq, StopOrdinal: data.StopOrdinal})
	if err != nil {
		if !dshSessionLogBoundExceeded(err) {
			return pending, false, err
		}
		// The log has already outgrown a bound this reader will not cross, and
		// it only grows. Returning the error would block every later event of
		// this session's transcript on a read that can never succeed, so settle
		// this Stop event value-free and let the rest of the session upload.
		event.NativeTurnFinalized = true
		var details map[string]any
		_ = json.Unmarshal(event.Data, &details)
		if details == nil {
			details = make(map[string]any)
		}
		details["dsh_native_finalization"] = "bound_exceeded"
		event.Data, _ = json.Marshal(details)
		return persistFinalizedDSHEvent(pending.Path, event)
	}
	if !turn.Complete {
		pending, expired, err := finishDSHNativeRetry(pending)
		if err != nil || !expired {
			return pending, false, err
		}
		event = pending.Event
		event.NativeTurnFinalized = true
		event.Body, event.Model, event.ModelProvider = "", "", ""
		event.ModelSource, event.ModelProviderSource = "", ""
		event.RecoveredMessages, event.Raw = nil, nil
		var details map[string]any
		_ = json.Unmarshal(event.Data, &details)
		if details == nil {
			details = make(map[string]any)
		}
		delete(details, "usage")
		details["dsh_native_finalization"] = "native_completion_timeout"
		if turn.Unresolved {
			details["dsh_native_finalization"] = "unresolved_prompt"
		}
		event.Data, _ = json.Marshal(details)
		return persistFinalizedDSHEvent(pending.Path, event)
	}

	event.NativeTurnFinalized = true
	if turn.Sealed {
		event.Body, event.Raw, event.RecoveredMessages = "", nil, nil
		event.Data = json.RawMessage(`{"sealed_content_omitted":true}`)
		return persistFinalizedDSHEvent(pending.Path, event)
	}
	if event.CaptureMode == ModeTrace || event.CaptureMode == ModeRaw {
		// The captured set travels on the event, not in the outbox: by the time
		// this Stop event is resolvable its turn's tool events have usually
		// already been uploaded and removed.
		captured := make(map[string]bool, len(data.HookedTool))
		for _, useID := range data.HookedTool {
			captured[useID] = true
		}
		event.RecoveredMessages = dshRecoveredTurnSteps(event, turn, captured, data.HookedToolOverflow)
	}
	if body := turn.Body; body != "" {
		event.Kind, event.Role, event.Body = "message.assistant", "assistant", body
	}
	if event.Model == "" && turn.Model != "" {
		event.Model, event.ModelSource = turn.Model, "native_transcript"
	}
	if event.ModelProvider == "" && turn.Provider != "" {
		event.ModelProvider, event.ModelProviderSource = turn.Provider, "native_transcript"
	}
	event.Data = mergeDSHTurnUsage(event.Data, turn.Usage)
	return persistFinalizedDSHEvent(pending.Path, event)
}

func persistFinalizedDSHEvent(path string, event Event) (PendingEvent, bool, error) {
	event.Data = withoutDSHNativeRetry(withoutDSHPromptDigest(event.Data))
	if err := validatePendingRewritePath(path, event); err != nil {
		return PendingEvent{Path: path, Event: event}, false, err
	}
	if err := writeJSONAtomic(path, event); err != nil {
		return PendingEvent{Path: path, Event: event}, false,
			fmt.Errorf("persist finalized dsh Stop event: %w", err)
	}
	return PendingEvent{Path: path, Event: event}, true, nil
}

func mergeDSHTurnUsage(raw json.RawMessage, totals dshTokenUsage) json.RawMessage {
	if totals.empty() {
		return raw
	}
	data := map[string]any{}
	if len(raw) != 0 && json.Unmarshal(raw, &data) != nil {
		return raw
	}
	if _, exists := data["usage"]; exists {
		return raw
	}
	data["usage"] = compactMap(map[string]any{
		"input_tokens":       totals.InputTokens,
		"output_tokens":      totals.OutputTokens,
		"cache_read_tokens":  totals.CacheReadTokens,
		"cache_write_tokens": totals.CacheWriteTokens,
	})
	merged, err := json.Marshal(data)
	if err != nil {
		return raw
	}
	return merged
}

// dshRecoveredTurnSteps projects the turn's intermediate work as children of
// the Stop event. Entries() emits them before the parent, so the ledger keeps
// the order the harness ran them in.
// capturedOverflowed reports that the dedup key is incomplete, so no tool record
// of this turn can be told apart from one the hook plane already uploaded.
func dshRecoveredTurnSteps(
	event Event, turn dshSessionTurn, captured map[string]bool, capturedOverflowed bool,
) []RecoveredMessage {
	var recovered []RecoveredMessage
	for index, step := range turn.Steps {
		last := index == len(turn.Steps)-1
		if !last && strings.TrimSpace(step.Text) != "" {
			recovered = append(recovered, RecoveredMessage{
				ID:        dshRecoveredID("evt", event.SessionID, event.TurnID, "thought", strconv.Itoa(step.Step)),
				TurnID:    event.TurnID,
				HookEvent: "AgentThought", NativeHookEvent: "sessionLogAssistantStep",
				Kind: "agent.thought", Role: "system", Body: step.Text,
				Data: dshRecoveredData(),
			})
		}
		for _, call := range step.ToolCalls {
			if capturedOverflowed || captured[call.CallID] || captured[dshToolPhaseKey(call.CallID, "PreToolUse")] {
				continue
			}
			recovered = append(recovered, RecoveredMessage{
				ID:        dshRecoveredID("evt", event.SessionID, event.TurnID, "tool-call", call.CallID),
				TurnID:    event.TurnID,
				HookEvent: "PreToolUse", NativeHookEvent: "sessionLogToolCall",
				Kind: "tool.call", Role: "tool",
				Body: toolBody(call.Name, call.CallID, dshJSONText(call.Arguments)),
				Data: dshRecoveredData(),
			})
		}
		for _, result := range step.ToolResults {
			if capturedOverflowed || captured[result.CallID] || captured[dshToolPhaseKey(result.CallID, "PostToolUse")] {
				continue
			}
			recovered = append(recovered, RecoveredMessage{
				ID:        dshRecoveredID("evt", event.SessionID, event.TurnID, "tool-result", result.CallID),
				TurnID:    event.TurnID,
				HookEvent: "PostToolUse", NativeHookEvent: "sessionLogToolResult",
				Kind: "tool.result", Role: "tool",
				Body: toolBody(result.Name, result.CallID, dshJSONText(result.Body)),
				Data: dshRecoveredData(),
			})
		}
	}
	return recovered
}

func dshRecoveredData() json.RawMessage {
	return json.RawMessage(`{"source":"dsh_session_log","fallback":true}`)
}

func dshToolPhaseKey(useID, hook string) string {
	return hook + "\x00" + useID
}

func withoutDSHPromptDigest(raw json.RawMessage) json.RawMessage {
	var data map[string]json.RawMessage
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	delete(data, "dsh_prompt_sha256")
	delete(data, "dsh_prompt_hook_seq")
	delete(data, "dsh_prompt_after_seq")
	clean, _ := json.Marshal(data)
	return clean
}

func dshJSONText(value string) json.RawMessage {
	if value == "" {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}

func dshRecoveredID(prefix, sessionID string, parts ...string) string {
	value := "dsh\x00" + sessionID + "\x00" + strings.Join(parts, "\x00")
	sum := sha256.Sum256([]byte(value))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func eventSealedContentOmitted(raw json.RawMessage) bool {
	var data struct {
		Omitted bool `json:"sealed_content_omitted"`
	}
	return len(raw) != 0 && json.Unmarshal(raw, &data) == nil && data.Omitted
}

func eventSyntheticFence(raw json.RawMessage) bool {
	var data struct {
		SyntheticFence bool `json:"synthetic_fence"`
	}
	return len(raw) != 0 && json.Unmarshal(raw, &data) == nil && data.SyntheticFence
}

func validatePendingRewritePath(path string, event Event) error {
	dir, err := outboxDir(event.Runtime)
	if err != nil {
		return err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || filepath.Dir(rel) != "." || filepath.Ext(rel) != ".json" ||
		!strings.HasSuffix(filepath.Base(rel), "-"+event.ID+".json") {
		return errors.New("pending event path does not match its outbox identity")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		!trustedPathIdentity(path, info) {
		return errors.New("pending event is not a trusted regular file")
	}
	return nil
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
		if running, known := flushLockOwnerRunning(path); known {
			if running {
				return func() {}, false, nil
			}
			_ = os.Remove(path)
			continue
		}
		info, statErr := os.Stat(path)
		if statErr != nil || time.Since(info.ModTime()) <= staleFlushLockAge {
			return func() {}, false, nil
		}
		_ = os.Remove(path)
	}
	return func() {}, false, nil
}

func flushLockOwnerRunning(path string) (running, known bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return false, false
	}
	return processRunning(pid)
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
	return filepath.Join(home, "capture", "state", runtime, sessionHash(sessionID)+".json"), nil
}

func sessionHash(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:16])
}

func loadSessionState(runtime, sessionID string) (sessionState, error) {
	path, err := sessionStatePath(runtime, sessionID)
	if err != nil {
		return sessionState{}, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return sessionState{}, nil
	}
	if err != nil {
		return sessionState{}, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxSessionStateBytes+1))
	if err != nil {
		return sessionState{}, err
	}
	if len(raw) == 0 || len(raw) > maxSessionStateBytes {
		return sessionState{}, errors.New("session state is invalid")
	}
	var state sessionState
	if err := json.Unmarshal(raw, &state); err != nil {
		return sessionState{}, fmt.Errorf("parse session state: %w", err)
	}
	if len(state.SensitiveToolUseIDs) > maxSensitiveToolUseFences {
		state.SensitiveToolUseIDs = nil
		state.RedactAllToolPayload = true
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
