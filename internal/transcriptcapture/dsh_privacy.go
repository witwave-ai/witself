package transcriptcapture

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

// dshDelegationToolName identifies the native delegation tools in dsh-base.
// Their results contain only the child's text: the bridge does not carry the
// child's session identity or its sealed-content provenance back to the parent.
// A delegation therefore makes the parent's turn value-free before the child
// can return any sealed material. This deliberately also omits safe delegated
// answers; ordinary tools and non-delegating turns remain capturable.
func dshDelegationToolName(name string) bool {
	switch strings.TrimSpace(name) {
	case "subagent", "subagent_fork":
		return true
	default:
		return false
	}
}

// protectDSHDelegation is called only on the dsh hook path. Keep this separate
// from the shared sealed-tool name matcher: other runtimes have their own
// delegation protocols and must not inherit a dsh-specific omission rule.
func protectDSHDelegation(input *hookInput, state *sessionState) {
	if input == nil || state == nil {
		return
	}
	if !state.DSHSessionProvenRoot && !state.DSHSessionCaptureSuppressed {
		path, err := resolveDSHSessionLogPath(input.SessionID, input.CWD)
		if err == nil && path != "" {
			records, readErr := readDSHSessionLogRecords(path, input.SessionID)
			if readErr == nil && len(records) != 0 {
				if dshSessionHeaderProvesRoot(records) {
					state.DSHSessionProvenRoot = true
				} else if dshSessionHeaderIsDelegated(records) {
					state.DSHSessionCaptureSuppressed = true
				}
			}
		}
	}
	// The header is immutable session provenance. Unknown provenance omits
	// this turn without latching the entire session; a later proven human
	// prompt can reopen a verified root session. A delegated/seeded session
	// never reopens, because its source:user inputs can all come from a parent.
	if !state.DSHSessionProvenRoot || state.DSHSessionCaptureSuppressed ||
		(isToolHookEvent(input.HookEventName) && dshDelegationToolName(input.ToolName)) {
		state.SensitiveTurn = true
	}
}

func dshSessionHeaderProvesRoot(records []dshSessionRecord) bool {
	if len(records) == 0 || records[0].Type != "session" {
		return false
	}
	header := records[0]
	return header.DelegationDepth != nil && *header.DelegationDepth == 0 &&
		header.IsSeeded != nil && !*header.IsSeeded
}

func dshSessionHeaderIsDelegated(records []dshSessionRecord) bool {
	if len(records) == 0 || records[0].Type != "session" {
		return false
	}
	header := records[0]
	return (header.DelegationDepth != nil && *header.DelegationDepth > 0) ||
		(header.IsSeeded != nil && *header.IsSeeded)
}

// dshPromptProvenance recovers source information the bridge removes from
// UserPromptSubmit. The loop claims its inbox before calling the bridge, but
// persists user/message only after that hook returns. Claim splice records and
// the current unpaired hook/invoked record are therefore the evidence available
// at this boundary. Persistence batches for 200 ms, so a sealed caller may wait
// boundedly for that evidence. Unknown provenance must never clear a seal.
// Optional bounds are the last observed invocation sequence and the native
// clock cutoff of a locally observed sealed operation, respectively.
func dshPromptProvenance(sessionID, cwd, prompt string, maxWait time.Duration, previousSeq ...int64) (userPrompt string, known bool, hookSeq int64) {
	deadline := time.Now().Add(maxWait)
	var cachedPath string
	var cachedInfo os.FileInfo
	for {
		path, err := resolveDSHSessionLogPath(sessionID, cwd)
		if err != nil {
			return "", false, 0
		}
		if path != "" {
			info, err := os.Stat(path)
			if err != nil {
				return "", false, 0
			}
			if path != cachedPath || cachedInfo == nil || !os.SameFile(info, cachedInfo) || info.Size() != cachedInfo.Size() || !info.ModTime().Equal(cachedInfo.ModTime()) {
				records, err := readDSHSessionLogRecords(path, sessionID)
				if err != nil {
					return "", false, 0
				}
				if userPrompt, known, hookSeq = projectDSHPromptProvenance(records, prompt, previousSeq...); known {
					return userPrompt, true, hookSeq
				}
				cachedPath, cachedInfo = path, info
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", false, 0
		}
		time.Sleep(min(dshSessionLogPollInterval, remaining))
	}
}

func projectDSHPromptProvenance(records []dshSessionRecord, prompt string, previousSeq ...int64) (string, bool, int64) {
	queues := map[string][]dshMessageRecord{"next-turn": nil, "next-step": nil}
	var claimed []dshMessageRecord
	var current []dshMessageRecord
	currentHandler := ""
	currentPending := false
	var currentSeq, currentTime int64
	for _, record := range records {
		switch record.Type {
		case "turn/start", "step/start":
			claimed, current = nil, nil
			currentHandler, currentPending = "", false
		case "agent/inbox/spliced":
			var splice struct {
				Target       string             `json:"target"`
				Start        int                `json:"start"`
				RemovedCount int                `json:"removedCount"`
				Inserted     []dshMessageRecord `json:"inserted"`
				Outcome      string             `json:"outcome"`
			}
			if json.Unmarshal(record.Data, &splice) != nil {
				return "", false, 0
			}
			queue, exists := queues[splice.Target]
			if !exists || splice.Start < 0 || splice.Start > len(queue) || splice.RemovedCount < 0 || splice.RemovedCount > len(queue)-splice.Start {
				return "", false, 0
			}
			end := splice.Start + splice.RemovedCount
			if splice.Outcome == "" && splice.RemovedCount > 0 && len(splice.Inserted) == 0 {
				// claim() removes next-step before next-turn; splice order
				// preserves exactly the batch the bridge flattens.
				claimed = append(claimed, queue[splice.Start:end]...)
			}
			next := append([]dshMessageRecord{}, queue[:splice.Start]...)
			next = append(next, splice.Inserted...)
			queues[splice.Target] = append(next, queue[end:]...)
		case "hook/invoked", "hook/result":
			var hook struct {
				Point     string `json:"point"`
				Dialect   string `json:"dialect"`
				HandlerID string `json:"handlerId"`
			}
			if json.Unmarshal(record.Data, &hook) != nil {
				return "", false, 0
			}
			if hook.Point != "UserPromptSubmit" {
				continue
			}
			if record.Type == "hook/invoked" {
				if hook.Dialect != "claude-code" || hook.HandlerID == "" {
					return "", false, 0
				}
				current = append([]dshMessageRecord{}, claimed...)
				currentHandler, currentPending = hook.HandlerID, true
				currentSeq = record.Seq
				currentTime = record.Time
			} else if hook.HandlerID == currentHandler {
				currentPending = false
			}
		}
	}
	if !currentPending || len(current) == 0 || (len(previousSeq) != 0 && currentSeq <= previousSeq[0]) {
		return "", false, 0
	}
	var all, user strings.Builder
	for _, message := range current {
		if message.Source.Kind == "" {
			return "", false, 0
		}
		text := dshVisibleText(message.Content)
		all.WriteString(text)
		if message.Source.Kind == "user" {
			user.WriteString(text)
		}
	}
	if all.String() != prompt {
		return "", false, 0
	}
	if user.Len() != 0 && len(previousSeq) > 1 && currentTime <= previousSeq[1] {
		// A source:user invocation that predates the local sealed operation
		// is an older persisted prefix, even if its text and previously
		// unobserved sequence happen to match this hook. Equal timestamps
		// cannot prove ordering at native millisecond precision either.
		return "", false, 0
	}
	return user.String(), true, currentSeq
}
