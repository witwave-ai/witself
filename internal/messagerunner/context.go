package messagerunner

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/client"
)

const (
	// TurnContextSchemaV1 identifies bounded advisory continuation context.
	TurnContextSchemaV1           = "witself.message-thread-context.v1"
	runnerContextPayloadKey       = "_witself_runner"
	maximumHistoryEntries         = 6
	maximumHistoryBodyBytes       = 2048
	maximumProviderPayloadBytes   = 4096
	maximumCompletionPayloadBytes = 15 * 1024
)

// TurnHistoryEntry is bounded advisory conversation context. It is untrusted
// message content, never identity or authorization evidence.
type TurnHistoryEntry struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	Kind      string `json:"kind"`
	Subject   string `json:"subject,omitempty"`
	Body      string `json:"body"`
}

type turnContext struct {
	Schema    string             `json:"schema"`
	TurnsUsed int                `json:"turns_used"`
	History   []TurnHistoryEntry `json:"history,omitempty"`
}

// decodeTurnPayload removes the runner-reserved context key from an otherwise
// unchanged provider payload. Malformed context is ignored, never trusted.
func decodeTurnPayload(payload json.RawMessage) (json.RawMessage, []TurnHistoryEntry, int) {
	if len(payload) == 0 {
		return nil, nil, 1
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(payload, &object) != nil || object == nil {
		return payload, nil, 1
	}
	rawContext, exists := object[runnerContextPayloadKey]
	if !exists {
		return payload, nil, 1
	}
	delete(object, runnerContextPayloadKey)
	providerPayload, _ := marshalOptionalObject(object)

	var context turnContext
	if json.Unmarshal(rawContext, &context) != nil || context.Schema != TurnContextSchemaV1 ||
		context.TurnsUsed < 1 || context.TurnsUsed > 64 || len(context.History) > maximumHistoryEntries {
		return providerPayload, nil, 1
	}
	for i := range context.History {
		context.History[i] = normalizeHistoryEntry(context.History[i])
	}
	return providerPayload, context.History, context.TurnsUsed + 1
}

func encodeTurnPayload(providerPayload json.RawMessage, turnsUsed int, history []TurnHistoryEntry) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if len(providerPayload) != 0 {
		if len(providerPayload) > maximumProviderPayloadBytes || json.Unmarshal(providerPayload, &object) != nil || object == nil {
			return nil, errors.New("message provider payload exceeds the runner continuation limit")
		}
	} else {
		object = map[string]json.RawMessage{}
	}
	delete(object, runnerContextPayloadKey)
	history = compactHistory(history)
	context := turnContext{Schema: TurnContextSchemaV1, TurnsUsed: turnsUsed, History: history}

	for {
		rawContext, err := json.Marshal(context)
		if err != nil {
			return nil, err
		}
		object[runnerContextPayloadKey] = rawContext
		payload, err := json.Marshal(object)
		if err != nil {
			return nil, err
		}
		if len(payload) <= maximumCompletionPayloadBytes {
			return payload, nil
		}
		if !shrinkHistory(&context.History) {
			return nil, errors.New("message continuation payload exceeds its limit")
		}
	}
}

func historyWithCurrent(history []TurnHistoryEntry, message client.Message) []TurnHistoryEntry {
	result := append([]TurnHistoryEntry(nil), history...)
	result = append(result, normalizeHistoryEntry(TurnHistoryEntry{
		AgentID: message.From.AgentID, AgentName: message.From.AgentName,
		Kind: message.Kind, Subject: message.Subject, Body: message.Body,
	}))
	return compactHistory(result)
}

func compactHistory(history []TurnHistoryEntry) []TurnHistoryEntry {
	if len(history) > maximumHistoryEntries {
		// Preserve the initiating objective and the newest conversational turns.
		first := history[0]
		history = append([]TurnHistoryEntry{first}, history[len(history)-(maximumHistoryEntries-1):]...)
	} else {
		history = append([]TurnHistoryEntry(nil), history...)
	}
	for i := range history {
		history[i] = normalizeHistoryEntry(history[i])
	}
	return history
}

func normalizeHistoryEntry(entry TurnHistoryEntry) TurnHistoryEntry {
	entry.AgentID = truncateUTF8(strings.TrimSpace(entry.AgentID), 256)
	entry.AgentName = truncateUTF8(strings.TrimSpace(entry.AgentName), 256)
	entry.Kind = truncateUTF8(strings.TrimSpace(entry.Kind), 64)
	entry.Subject = truncateUTF8(strings.TrimSpace(entry.Subject), 256)
	entry.Body = truncateUTF8(entry.Body, maximumHistoryBodyBytes)
	return entry
}

func shrinkHistory(history *[]TurnHistoryEntry) bool {
	entries := *history
	largest := -1
	for i := range entries {
		if largest == -1 || len(entries[i].Body) > len(entries[largest].Body) {
			largest = i
		}
	}
	if largest >= 0 && len(entries[largest].Body) > 128 {
		entries[largest].Body = truncateUTF8(entries[largest].Body, len(entries[largest].Body)/2)
		*history = entries
		return true
	}
	if len(entries) > 1 {
		*history = append(entries[:1:1], entries[2:]...)
		return true
	}
	return false
}

func truncateUTF8(value string, maximumBytes int) string {
	if len(value) <= maximumBytes {
		return value
	}
	value = value[:maximumBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func marshalOptionalObject(object map[string]json.RawMessage) (json.RawMessage, error) {
	if len(object) == 0 {
		return nil, nil
	}
	return json.Marshal(object)
}
