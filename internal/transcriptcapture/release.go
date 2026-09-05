package transcriptcapture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/id"
)

// OperatorReleaseKind identifies a privacy-restricted completion made by an
// operator because the provider never sealed the captured turn.
const OperatorReleaseKind = "operator_release"

// The session hold is durable before any event rewrite. FencedAt closes only
// the turn-less suppression window; its history still protects stale snapshots.
type operatorReleaseState struct {
	Status          string    `json:"status"`
	ReleasedTurnIDs []string  `json:"released_turn_ids"`
	Since           time.Time `json:"since"`
	FencedAt        time.Time `json:"fenced_at,omitzero"`
}

func (release *operatorReleaseState) active() bool {
	return release != nil && release.FencedAt.IsZero()
}

func (release *operatorReleaseState) releasesTurn(turnID string) bool {
	if release == nil || strings.TrimSpace(turnID) == "" {
		return false
	}
	for _, released := range release.ReleasedTurnIDs {
		if turnID == released {
			return true
		}
	}
	return false
}

func (release *operatorReleaseState) suppresses(event Event) bool {
	if release == nil {
		return false
	}
	if strings.TrimSpace(event.TurnID) != "" {
		return release.releasesTurn(event.TurnID)
	}
	return release.active() || !event.OccurredAt.After(release.FencedAt)
}

type operatorReleaseReadiness struct {
	state  *operatorReleaseState
	failed bool
}

func (index *ReadinessIndex) operatorSession(event Event) operatorReleaseReadiness {
	key := eventFenceKey(event).session
	if release, loaded := index.operatorSessions[key]; loaded {
		return release
	}
	state, err := loadSessionState(event.Runtime, event.SessionID)
	release := operatorReleaseReadiness{state: state.OperatorRelease, failed: err != nil}
	index.operatorSessions[key] = release
	return release
}

// ResidueSession is a value-free summary of still-gated local turn events.
type ResidueSession struct {
	SessionID     string
	EventCount    int
	FirstEventAt  time.Time
	LastEventAt   time.Time
	HasToolResult bool
}

type residueTurn struct {
	key    readinessFenceKey
	events []PendingEvent
	first  time.Time
	last   time.Time
}

func eventFenceKey(event Event) readinessFenceKey {
	return readinessFenceKey{
		session: readinessSessionKey{transcriptID: event.TranscriptExternalID(), sessionID: event.SessionID},
		runID:   event.RunID, turnID: event.TurnID,
	}
}

func residueTurns(pending []PendingEvent, sessionID string) []residueTurn {
	index := NewReadinessIndex(pending)
	grouped := make(map[readinessFenceKey]*residueTurn)
	fenced := make(map[readinessFenceKey]bool)
	ended := make(map[string]bool)
	for _, item := range pending {
		event := item.Event
		switch event.HookEvent {
		case "AgentResponse", "Stop", "StopFailure":
			fenced[eventFenceKey(event)] = true
		case "SessionEnd":
			ended[event.TranscriptExternalID()+"\x00"+event.RunID] = true
		}
	}
	for _, item := range pending {
		event := item.Event
		if (sessionID != "" && event.SessionID != sessionID) || event.SessionID == "" || event.RunID == "" || event.TurnID == "" ||
			event.Kind == OperatorReleaseKind || (event.Runtime == RuntimeCodex && strings.TrimSpace(event.SourceTranscriptPath) == "") {
			continue
		}
		key := eventFenceKey(event)
		turn := grouped[key]
		if turn == nil {
			turn = &residueTurn{key: key, first: event.OccurredAt, last: event.OccurredAt}
			grouped[key] = turn
		}
		turn.events = append(turn.events, item)
		if event.OccurredAt.Before(turn.first) {
			turn.first = event.OccurredAt
		}
		if event.OccurredAt.After(turn.last) {
			turn.last = event.OccurredAt
		}
	}
	turns := make([]residueTurn, 0, len(grouped))
	for _, turn := range grouped {
		// Never touch a runtime-fenced identity, including a late event whose
		// timestamp falls after that fence. Durable operator completion is also
		// final; future hooks use the persisted release suppression policy.
		if (!index.operatorReleases[turn.key] && (fenced[turn.key] || ended[turn.key.session.transcriptID+"\x00"+turn.key.runID])) ||
			!index.completedFences[turn.key].OccurredAt.IsZero() {
			continue
		}
		gated := false
		for _, item := range turn.events {
			gated = gated || !index.UploadReady(item)
		}
		if !gated {
			continue
		}
		turns = append(turns, *turn)
	}
	sort.Slice(turns, func(i, j int) bool {
		if turns[i].key.session.sessionID != turns[j].key.session.sessionID {
			return turns[i].key.session.sessionID < turns[j].key.session.sessionID
		}
		if !turns[i].first.Equal(turns[j].first) {
			return turns[i].first.Before(turns[j].first)
		}
		if turns[i].key.runID != turns[j].key.runID {
			return turns[i].key.runID < turns[j].key.runID
		}
		return turns[i].key.turnID < turns[j].key.turnID
	})
	return turns
}

// ListResidue reads the outbox without creating locks, changing state, or
// releasing anything. An empty sessionID lists all sessions for the runtime.
func ListResidue(runtime, sessionID string) ([]ResidueSession, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return nil, err
	}
	pending, err := Pending(runtime)
	if err != nil {
		return nil, err
	}
	var sessions []ResidueSession
	for _, turn := range residueTurns(pending, strings.TrimSpace(sessionID)) {
		if len(sessions) == 0 || sessions[len(sessions)-1].SessionID != turn.key.session.sessionID {
			sessions = append(sessions, ResidueSession{SessionID: turn.key.session.sessionID, FirstEventAt: turn.first, LastEventAt: turn.last})
		}
		session := &sessions[len(sessions)-1]
		session.EventCount += len(turn.events)
		if turn.first.Before(session.FirstEventAt) {
			session.FirstEventAt = turn.first
		}
		if turn.last.After(session.LastEventAt) {
			session.LastEventAt = turn.last
		}
		for _, item := range turn.events {
			session.HasToolResult = session.HasToolResult || item.Event.Kind == "tool.result"
		}
	}
	return sessions, nil
}

// ReleaseResidue releases eligible turns, returning their count. An empty
// sessionID selects all sessions and skips fresh turns; an explicit session
// refuses the entire session if any residue turn is fresh. Every apply rereads
// under the flush and session locks, so a preceding listing grants no authority
// over a turn that has since resumed or received its runtime fence.
func ReleaseResidue(runtime, sessionID string, olderThan time.Duration, force bool) (int, error) {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return 0, err
	}
	if olderThan < 0 {
		return 0, errors.New("transcript release older-than must not be negative")
	}
	sessionID = strings.TrimSpace(sessionID)
	if err := VerifyReleaseUpgradeBarrier(runtime); err != nil {
		return 0, err
	}
	release, acquired, err := AcquireFlushLock(runtime)
	if err != nil {
		return 0, err
	}
	if !acquired {
		return 0, &ReleaseUpgradeBarrierError{Runtime: runtime, Reason: "a runtime transcript flush is active"}
	}
	defer release()
	if err := SweepOrphanSubmissions(runtime); err != nil {
		fmt.Fprintf(os.Stderr, "witself: clean orphaned capture submissions: %v\n", err)
	}
	if err := VerifyReleaseSubmissionProvenance(runtime); err != nil {
		return 0, err
	}
	sessions, err := ListResidue(runtime, sessionID)
	if err != nil {
		return 0, err
	}
	if sessionID != "" && len(sessions) == 0 {
		// A crash after the last completion marker may leave only the session
		// status pending. An explicit retry can reconcile it without reupload.
		sessions = append(sessions, ResidueSession{SessionID: sessionID})
	} else if sessionID == "" {
		pending, err := Pending(runtime)
		if err != nil {
			return 0, err
		}
		seen := make(map[string]bool)
		for _, session := range sessions {
			seen[session.SessionID] = true
		}
		for _, item := range pending {
			if seen[item.Event.SessionID] {
				continue
			}
			seen[item.Event.SessionID] = true
			state, err := loadSessionState(runtime, item.Event.SessionID)
			if err != nil {
				return 0, err
			}
			if state.OperatorRelease != nil && state.OperatorRelease.Status == "pending" {
				sessions = append(sessions, ResidueSession{SessionID: item.Event.SessionID})
			}
		}
	}
	cutoff := time.Now().UTC().Add(-olderThan)
	count := 0
	var failures []error
	for _, session := range sessions {
		n, err := releaseResidueSession(runtime, session.SessionID, cutoff, force, sessionID != "")
		count += n
		if err != nil {
			failures = append(failures, fmt.Errorf("session %q: %w", session.SessionID, err))
		}
	}
	return count, errors.Join(failures...)
}

func releaseResidueSession(runtime, sessionID string, cutoff time.Time, force, refuseFresh bool) (int, error) {
	release, err := acquireSessionStateLock(runtime, sessionID)
	if err != nil {
		return 0, err
	}
	defer release()
	pending, err := Pending(runtime)
	if err != nil {
		return 0, err
	}
	turns := residueTurns(pending, sessionID)
	if refuseFresh && !force {
		for _, turn := range turns {
			if turn.last.After(cutoff) {
				return 0, errors.New("residue contains a turn newer than --older-than; use --force to override")
			}
		}
	}
	cfg, err := LoadConfig(runtime)
	if err != nil {
		return 0, err
	}
	state, err := loadSessionState(runtime, sessionID)
	if err != nil {
		return 0, err
	}
	var selected []residueTurn
	for _, turn := range turns {
		if !force && turn.last.After(cutoff) {
			continue
		}
		for _, item := range turn.events {
			if err := EventBindingError(item.Event, cfg); err != nil {
				return 0, err
			}
			if err := operatorReleaseRewriteAllowed(item); err != nil {
				return 0, err
			}
			if state.SensitiveTurn && state.RunID == turn.key.runID && state.TurnID == turn.key.turnID {
				// A sealed hook can persist sensitivity after a failed rewrite.
				// Refuse before installing the release hold so the original hook
				// or runtime fence can still finish suppressing this exact turn.
				// Release preserves captured messages, while queued tool/system
				// payloads are suppressed by its rewrite. Attempted envelopes are
				// preserved wholesale and must already be fully suppressed.
				preservesBody := item.submission != nil || item.Event.Kind == "message.user" || item.Event.Kind == "message.assistant"
				if (preservesBody && !eventSealedContentOmitted(item.Event.Data)) || len(item.Event.RecoveredMessages) != 0 ||
					(item.submission != nil && len(item.Event.Raw) != 0) {
					return 0, errors.New("turn has pending sealed-content suppression; retry the sealed hook or runtime fence before release")
				}
			}
		}
		if state.PendingFence != nil && eventFenceKey(*state.PendingFence) == turn.key {
			return 0, errors.New("turn has a pending runtime fence; retry transcript fence to finish it")
		}
		selected = append(selected, turn)
	}
	if len(selected) == 0 {
		return 0, reconcileOperatorRelease(cfg, sessionID, state, pending)
	}
	for _, item := range pending {
		if item.Event.SessionID == sessionID && strings.TrimSpace(item.Event.TurnID) == "" {
			if err := EventBindingError(item.Event, cfg); err != nil {
				return 0, err
			}
			if err := operatorReleaseRewriteAllowed(item); err != nil {
				return 0, err
			}
		}
	}
	if state.OperatorRelease == nil {
		state.OperatorRelease = &operatorReleaseState{Since: time.Now().UTC()}
	}
	state.OperatorRelease.Status = "pending"
	for _, turn := range selected {
		if !state.OperatorRelease.releasesTurn(turn.key.turnID) {
			// Retrying an old release must not reopen a session already ended
			// by a genuinely new fence. Only newly requested turns reopen it.
			if !state.OperatorRelease.active() {
				state.OperatorRelease.Since = time.Now().UTC()
				state.OperatorRelease.FencedAt = time.Time{}
			}
			state.OperatorRelease.ReleasedTurnIDs = append(state.OperatorRelease.ReleasedTurnIDs, turn.key.turnID)
		}
	}
	// Save every requested identity before the first rewrite (or hold event).
	// A crash here must not let a later Stop or empty-ID hook bypass release.
	if err := saveSessionState(runtime, sessionID, state); err != nil {
		return 0, err
	}
	count := 0
	var failures []error
	for _, turn := range selected {
		if err := releaseResidueTurn(cfg, turn, pending); err != nil {
			failures = append(failures, err)
			continue
		}
		count++
	}
	if len(failures) == 0 {
		state, err := loadSessionState(runtime, sessionID)
		if err != nil {
			return count, err
		}
		state.OperatorRelease.Status = "completed"
		if err := saveSessionState(runtime, sessionID, state); err != nil {
			return count, err
		}
	}
	return count, errors.Join(failures...)
}

func reconcileOperatorRelease(cfg Config, sessionID string, state sessionState, pending []PendingEvent) error {
	if state.OperatorRelease == nil || state.OperatorRelease.Status != "pending" {
		return nil
	}
	for _, item := range pending {
		event := item.Event
		if event.SessionID != sessionID || !state.OperatorRelease.suppresses(event) {
			continue
		}
		if err := EventBindingError(event, cfg); err != nil {
			return err
		}
		if item.submission != nil {
			continue
		}
		if !operatorReleasedEventSafe(event) {
			return nil
		}
		if strings.TrimSpace(event.TurnID) != "" {
			fence := loadCompletedFence(event)
			if fence.Kind != OperatorReleaseKind || fence.OccurredAt.Before(event.OccurredAt) {
				return nil
			}
		}
	}
	state.OperatorRelease.Status = "completed"
	return saveSessionState(cfg.Runtime, sessionID, state)
}

func releaseResidueTurn(cfg Config, turn residueTurn, pending []PendingEvent) error {
	for _, item := range turn.events {
		if err := EventBindingError(item.Event, cfg); err != nil {
			return err
		}
	}
	state, err := loadSessionState(cfg.Runtime, turn.key.session.sessionID)
	if err != nil {
		return err
	}
	if state.PendingFence != nil && eventFenceKey(*state.PendingFence) == turn.key {
		return errors.New("turn has a pending runtime fence; retry transcript fence to finish it")
	}
	var fence Event
	for _, item := range pending {
		if item.Event.Kind == OperatorReleaseKind && eventFenceKey(item.Event) == turn.key {
			fence = item.Event
			break
		}
	}
	if fence.ID == "" {
		fence = turn.events[0].Event
		fence.ID, err = id.New("evt")
		if err != nil {
			return err
		}
		fence.SubmissionReleaseID, fence.SubmissionReplyEventIDs = "", nil
		fence.HookEvent, fence.NativeHookEvent = "OperatorRelease", "OperatorRelease"
		fence.Kind, fence.Role = OperatorReleaseKind, "system"
		fence.Body = "turn released by operator without runtime seal"
		fence.Data = json.RawMessage(`{"operator_release":true,"synthetic_fence":true,"fence_kind":"operator_release"}`)
		fence.Raw, fence.RecoveredMessages = nil, nil
		fence.ReplyToEventID = ""
		fence.NativeTurnFinalized = true
		fence.OccurredAt = time.Now().UTC()
		if !fence.OccurredAt.After(turn.last) {
			fence.OccurredAt = turn.last.Add(time.Nanosecond)
		}
		// This distinct hook is a hold fence until the completed marker exists.
		// Persist it first so a crash cannot bypass unfinished suppression by
		// adding a normal terminal hook to a partially rewritten turn.
		if err := writeOutboxEvent(fence); err != nil {
			return err
		}
	}
	// Future terminal hooks must reply to the replacement prompt even after
	// its outbox event and submission sidecar have been acknowledged.
	for _, item := range turn.events {
		if state.PromptEventID == item.Event.ID && item.submission == nil && !operatorReleasedEventSafe(item.Event) {
			releaseID := fence.ID
			if item.queuedSubmission != nil && item.queuedSubmission.ReleaseID != "" {
				releaseID = item.queuedSubmission.ReleaseID
			}
			state.PromptEventID = releasedSubmissionEventID(item.Event.ID, releaseID)
			if err := saveSessionState(cfg.Runtime, turn.key.session.sessionID, state); err != nil {
				return err
			}
		}
	}
	if err := redactPendingEvents(pending, func(event Event) bool {
		return eventFenceKey(event) == turn.key ||
			(event.SessionID == turn.key.session.sessionID && strings.TrimSpace(event.TurnID) == "" && state.OperatorRelease.suppresses(event))
	}, OperatorReleaseKind, fence.ID); err != nil {
		return err
	}
	if state.RunID == turn.key.runID && state.TurnID == turn.key.turnID {
		// Keep this identity until the next real prompt. A forced release can
		// race a still-live runtime; subsequent tools must inherit this turn's
		// release policy instead of escaping the gate with an empty turn ID.
		state.SyntheticFencedTurnID = turn.key.turnID
		if err := saveSessionState(cfg.Runtime, turn.key.session.sessionID, state); err != nil {
			return err
		}
	}
	path, err := completedFencePath(fence)
	if err != nil {
		return err
	}
	completedAt := fence.OccurredAt
	if turn.last.After(completedAt) {
		completedAt = turn.last
	}
	return writeJSONAtomic(path, completedFence{OccurredAt: completedAt, Kind: OperatorReleaseKind})
}

type releasedPayload struct {
	Redacted      string `json:"redacted"`
	OriginalBytes int    `json:"original_bytes"`
	SHA256        string `json:"sha256"`
}

func redactOperatorReleasedEvent(event Event) Event {
	if operatorReleasedEventSafe(event) {
		return event
	}
	var body struct {
		Name  string          `json:"tool_name"`
		UseID string          `json:"tool_use_id"`
		Value json.RawMessage `json:"value"`
		Input json.RawMessage `json:"input"`
	}
	_ = json.Unmarshal([]byte(event.Body), &body)
	var existing struct {
		Tool struct {
			Name   string          `json:"name"`
			UseID  string          `json:"use_id"`
			Input  json.RawMessage `json:"input"`
			Output json.RawMessage `json:"output"`
		} `json:"tool"`
		Sealed bool `json:"sealed_content_omitted"`
	}
	_ = json.Unmarshal(event.Data, &existing)
	data := map[string]any{"operator_release": true}
	if existing.Sealed {
		data["sealed_content_omitted"] = true
	}
	name, useID := firstNonempty(body.Name, existing.Tool.Name), firstNonempty(body.UseID, existing.Tool.UseID)
	var envelope hookInput
	_ = json.Unmarshal(event.Raw, &envelope)
	input := firstRaw(envelope.ToolInput, envelope.ToolInputCamel, body.Input)
	if event.Kind == "tool.call" {
		input = firstRaw(body.Value, input)
	}
	output := firstRaw(envelope.ToolResponse, envelope.ToolOutput, envelope.ToolOutputCamel)
	tool := compactMap(map[string]any{"name": name, "use_id": useID})
	if len(input) != 0 || len(existing.Tool.Input) != 0 {
		tool["input"] = releasePayload(input, existing.Tool.Input)
	}
	if len(output) != 0 || len(existing.Tool.Output) != 0 {
		tool["output"] = releasePayload(output, existing.Tool.Output)
	}
	switch event.Kind {
	case "tool.result":
		// Body.value is the complete canonical JSON result; Data.tool.output
		// can already be a bounded hash marker for large provider results.
		payload := firstRaw(body.Value, output)
		if len(payload) == 0 && len(existing.Tool.Output) == 0 {
			payload = []byte(event.Body)
		}
		placeholder := releasePayload(payload, existing.Tool.Output)
		raw, _ := json.Marshal(placeholder)
		event.Body = string(raw)
		tool["output"] = placeholder
	case "tool.call", "tool.error":
		// Arguments can repeat earlier result values. Retain tool identity,
		// without preserving either argument or error-message copies.
		event.Body = toolBody(name, useID, nil)
	case "message.user", "message.assistant":
		// Only captured visible messages may preserve free-form bodies.
	case OperatorReleaseKind:
		event.Body = "turn released by operator without runtime seal"
		data["synthetic_fence"] = true
		data["fence_kind"] = OperatorReleaseKind
	default:
		// Permission denials, terminal reasons, notifications, and other
		// system bodies can repeat tool input or output outside Data and Raw.
		placeholder, _ := json.Marshal(releasePayload([]byte(event.Body), nil))
		event.Body = string(placeholder)
	}
	if len(tool) != 0 {
		data["tool"] = tool
	}
	event.Raw = nil
	// Recovered messages contain only captured visible messages, so retain
	// their bodies while discarding ancillary payload copies.
	for i := range event.RecoveredMessages {
		event.RecoveredMessages[i].Data = json.RawMessage(`{"operator_release":true}`)
	}
	event.Data, _ = json.Marshal(data)
	return event
}

func releasePayload(payload, projected json.RawMessage) releasedPayload {
	if len(payload) == 0 {
		// Bounded capture metadata already records the complete payload's
		// digest. Preserve it rather than hashing the omission record itself.
		var bounded struct {
			Omitted bool   `json:"omitted"`
			Bytes   int    `json:"bytes"`
			SHA256  string `json:"sha256"`
		}
		if json.Unmarshal(projected, &bounded) == nil && bounded.Omitted && bounded.Bytes >= 0 && len(bounded.SHA256) == sha256.Size*2 {
			if _, err := hex.DecodeString(bounded.SHA256); err == nil {
				return releasedPayload{Redacted: "released_without_fence", OriginalBytes: bounded.Bytes, SHA256: bounded.SHA256}
			}
		}
		payload = projected
	}
	sum := sha256.Sum256(payload)
	return releasedPayload{Redacted: "released_without_fence", OriginalBytes: len(payload), SHA256: hex.EncodeToString(sum[:])}
}

func operatorReleasedEventSafe(event Event) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(event.Data, &fields) != nil {
		return false
	}
	for key := range fields {
		if key != "operator_release" && key != "sealed_content_omitted" &&
			key != "tool" &&
			!((key == "synthetic_fence" || key == "fence_kind") && event.Kind == OperatorReleaseKind) {
			return false
		}
	}
	var data struct {
		Released       bool   `json:"operator_release"`
		Sealed         bool   `json:"sealed_content_omitted"`
		SyntheticFence bool   `json:"synthetic_fence"`
		FenceKind      string `json:"fence_kind"`
		Tool           struct {
			Input  json.RawMessage `json:"input"`
			Output json.RawMessage `json:"output"`
		} `json:"tool"`
	}
	if len(event.Raw) != 0 || json.Unmarshal(event.Data, &data) != nil || !data.Released {
		return false
	}
	if event.Kind == OperatorReleaseKind && (!data.SyntheticFence || data.FenceKind != OperatorReleaseKind) {
		return false
	}
	for _, message := range event.RecoveredMessages {
		if !bytes.Equal(message.Data, []byte(`{"operator_release":true}`)) || (message.Kind != "message.user" && message.Kind != "message.assistant") {
			return false
		}
	}
	var tool map[string]json.RawMessage
	if raw := fields["tool"]; len(raw) != 0 {
		if json.Unmarshal(raw, &tool) != nil {
			return false
		}
		for key := range tool {
			if key == "name" || key == "use_id" {
				var identity string
				if json.Unmarshal(tool[key], &identity) != nil {
					return false
				}
			}
			if key != "name" && key != "use_id" && key != "input" && key != "output" {
				return false
			}
			if (key == "input" || key == "output") && !releasedPayloadSafe(tool[key]) {
				return false
			}
		}
	}
	if event.Kind == "tool.call" || event.Kind == "tool.error" {
		var name, useID string
		_ = json.Unmarshal(tool["name"], &name)
		_ = json.Unmarshal(tool["use_id"], &useID)
		return event.Body == toolBody(name, useID, nil)
	}
	if event.Kind == "message.user" || event.Kind == "message.assistant" {
		return true
	}
	if event.Kind == OperatorReleaseKind {
		return event.Body == "turn released by operator without runtime seal"
	}
	if event.Kind != "tool.result" {
		return releasedPayloadSafe([]byte(event.Body))
	}
	var body, output releasedPayload
	if json.Unmarshal([]byte(event.Body), &body) != nil || json.Unmarshal(data.Tool.Output, &output) != nil || body != output ||
		body.Redacted != "released_without_fence" || body.OriginalBytes < 0 || len(body.SHA256) != sha256.Size*2 {
		return false
	}
	canonical, _ := json.Marshal(body)
	var normalized bytes.Buffer
	if json.Compact(&normalized, data.Tool.Output) != nil || !bytes.Equal(canonical, normalized.Bytes()) || event.Body != string(canonical) {
		return false
	}
	_, err := hex.DecodeString(body.SHA256)
	return err == nil
}

func releasedPayloadSafe(raw json.RawMessage) bool {
	var payload releasedPayload
	if json.Unmarshal(raw, &payload) != nil || payload.Redacted != "released_without_fence" ||
		payload.OriginalBytes < 0 || len(payload.SHA256) != sha256.Size*2 {
		return false
	}
	if _, err := hex.DecodeString(payload.SHA256); err != nil {
		return false
	}
	canonical, _ := json.Marshal(payload)
	var normalized bytes.Buffer
	return json.Compact(&normalized, raw) == nil && bytes.Equal(canonical, normalized.Bytes())
}

func eventOperatorReleased(raw json.RawMessage) bool {
	var data struct {
		Released bool `json:"operator_release"`
	}
	return json.Unmarshal(raw, &data) == nil && data.Released
}
