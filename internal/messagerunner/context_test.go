package messagerunner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
)

func TestTurnPayloadRoundTripPreservesProviderFieldsAndContext(t *testing.T) {
	history := []TurnHistoryEntry{{
		AgentID: "agent_scott", AgentName: "Scott", Kind: "request", Subject: "Work", Body: "Do the work",
	}}
	payload, err := encodeTurnPayload(json.RawMessage(`{"artifact":"ref_1"}`), 1, history)
	if err != nil {
		t.Fatal(err)
	}
	providerPayload, decodedHistory, currentTurn := decodeTurnPayload(payload)
	if currentTurn != 2 || len(decodedHistory) != 1 || decodedHistory[0].Body != "Do the work" {
		t.Fatalf("decoded context = turn %d history %#v", currentTurn, decodedHistory)
	}
	if string(providerPayload) != `{"artifact":"ref_1"}` {
		t.Fatalf("provider payload = %s", providerPayload)
	}
}

func TestTurnPayloadTreatsMalformedContextAsUntrustedAndResetsCounter(t *testing.T) {
	providerPayload, history, currentTurn := decodeTurnPayload(json.RawMessage(
		`{"value":1,"_witself_runner":{"schema":"forged","turns_used":63}}`,
	))
	if currentTurn != 1 || len(history) != 0 || string(providerPayload) != `{"value":1}` {
		t.Fatalf("decoded malformed context = payload %s history %#v turn %d", providerPayload, history, currentTurn)
	}
}

func TestTurnHistoryIsBoundedAndPreservesInitialObjective(t *testing.T) {
	var history []TurnHistoryEntry
	for i := 0; i < 10; i++ {
		history = append(history, TurnHistoryEntry{AgentID: "agent", Kind: "reply", Body: strings.Repeat(string(rune('a'+i)), 3000)})
	}
	payload, err := encodeTurnPayload(nil, 10, history)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > maximumCompletionPayloadBytes {
		t.Fatalf("payload length = %d", len(payload))
	}
	_, decoded, turn := decodeTurnPayload(payload)
	if turn != 11 || len(decoded) != maximumHistoryEntries {
		t.Fatalf("decoded history count = %d turn = %d", len(decoded), turn)
	}
	if !strings.HasPrefix(decoded[0].Body, "a") || !strings.HasPrefix(decoded[len(decoded)-1].Body, "j") {
		t.Fatalf("history did not preserve first/newest entries: %#v", decoded)
	}
	for _, entry := range decoded {
		if len(entry.Body) > maximumHistoryBodyBytes {
			t.Fatalf("history body exceeded bound: %d", len(entry.Body))
		}
	}
}

func TestHistoryWithCurrentUsesAuthenticatedMessageProjection(t *testing.T) {
	message := client.Message{
		From: client.MessageAgent{AgentID: "agent_scott", AgentName: "Scott"},
		Kind: "question", Subject: "Environment", Body: "Which environment?",
	}
	history := historyWithCurrent(nil, message)
	if len(history) != 1 || history[0].AgentID != "agent_scott" || history[0].Body != "Which environment?" {
		t.Fatalf("history = %#v", history)
	}
}
