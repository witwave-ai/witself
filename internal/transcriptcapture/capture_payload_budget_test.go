package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func payloadBudgetEvent(t *testing.T, rawBytes, dataBytes int) Event {
	t.Helper()
	filler := func(n int) json.RawMessage {
		// {"v":"xxxx..."} sized to exactly n bytes.
		body := strings.Repeat("x", n-len(`{"v":""}`))
		raw := json.RawMessage(`{"v":"` + body + `"}`)
		if len(raw) != n {
			t.Fatalf("filler size = %d, want %d", len(raw), n)
		}
		return raw
	}
	event := Event{
		ID: "evt_budget", Runtime: RuntimeCodex, HookEvent: "PostToolUse", NativeHookEvent: "PostToolUse",
		Kind: "tool.result", Role: "tool", CaptureMode: ModeRaw, SessionID: "session-budget",
		TurnID: "turn-budget", RunID: "run_budget", OccurredAt: time.Date(2026, 9, 2, 5, 33, 5, 0, time.UTC),
		CWD: "/Users/example/project", Body: "tool output",
	}
	if rawBytes > 0 {
		event.Raw = filler(rawBytes)
	}
	if dataBytes > 0 {
		event.Data = filler(dataBytes)
	}
	return event
}

func decodePayload(t *testing.T, entry Entry) map[string]json.RawMessage {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// The live codex outbox held one tool.result whose payload was 16,402 bytes:
// raw 7,960 (embedded, under the raw cap) plus data 7,566 (never budgeted).
// The server rejected it and every later event of that transcript was skipped.
// Both copies here are individually under the raw cap but cannot share the
// entry payload cap with the envelope.
func TestEntryPayloadBudgetKeepsDataAndOmitsRawWhenBothCannotFit(t *testing.T) {
	entries := payloadBudgetEvent(t, 8100, 8100).Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if len(entries[0].Payload) > maxEntryPayloadBytes {
		t.Fatalf("payload = %d bytes, exceeds cap %d", len(entries[0].Payload), maxEntryPayloadBytes)
	}
	payload := decodePayload(t, entries[0])
	if _, ok := payload["data"]; !ok {
		t.Fatal("data was not embedded although it fits alone")
	}
	if _, ok := payload["raw"]; ok {
		t.Fatal("raw was embedded although it pushes the payload over the cap")
	}
	if string(payload["raw_omitted"]) != "true" || string(payload["raw_bytes"]) != "8100" || len(payload["raw_sha256"]) != 66 {
		t.Fatalf("raw omission markers = %s/%s/%s", payload["raw_omitted"], payload["raw_bytes"], payload["raw_sha256"])
	}
}

func TestEntryPayloadBudgetEmbedsBothWhenTheyFit(t *testing.T) {
	entries := payloadBudgetEvent(t, 2048, 2048).Entries()
	payload := decodePayload(t, entries[0])
	for _, key := range []string{"data", "raw"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("%s was not embedded", key)
		}
	}
	for _, key := range []string{"data_omitted", "raw_omitted"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("%s present although everything fits", key)
		}
	}
}

func TestEntryPayloadBudgetOmitsOversizedDataWithDigest(t *testing.T) {
	entries := payloadBudgetEvent(t, 1024, 20*1024).Entries()
	if len(entries[0].Payload) > maxEntryPayloadBytes {
		t.Fatalf("payload = %d bytes, exceeds cap", len(entries[0].Payload))
	}
	payload := decodePayload(t, entries[0])
	if _, ok := payload["data"]; ok {
		t.Fatal("oversized data was embedded")
	}
	if string(payload["data_omitted"]) != "true" || string(payload["data_bytes"]) != "20480" || len(payload["data_sha256"]) != 66 {
		t.Fatalf("data omission markers = %s/%s/%s", payload["data_omitted"], payload["data_bytes"], payload["data_sha256"])
	}
	if _, ok := payload["raw"]; !ok {
		t.Fatal("small raw was not embedded after data was omitted")
	}
}

func TestEntryPayloadBudgetKeepsLargeRawOmissionAndIsDeterministic(t *testing.T) {
	event := payloadBudgetEvent(t, maxRawPayloadBytes+1, 512)
	first := event.Entries()[0].Payload
	second := event.Entries()[0].Payload
	if !bytes.Equal(first, second) {
		t.Fatal("entry payload is not byte-identical across calls")
	}
	payload := decodePayload(t, event.Entries()[0])
	if string(payload["raw_omitted"]) != "true" {
		t.Fatal("raw over maxRawPayloadBytes was not marked omitted")
	}
	if _, ok := payload["data"]; !ok {
		t.Fatal("small data was not embedded")
	}
}
