package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

// TestCaptureAppendBatchesSanitizesNUL pins the client-side half of the NUL
// defense: sanitizing at batch assembly repairs events already sitting in the
// outbox, so a backlog wedged behind a NUL-bearing entry flushes after a CLI
// upgrade without any server change.
func TestCaptureAppendBatchesSanitizesNUL(t *testing.T) {
	nulEscapeJSON := "\\" + "u0000"
	entries := []transcriptcapture.Entry{{
		ExternalID: "evt_1:0",
		Role:       "tool",
		Body:       "out\x00put",
		Payload:    json.RawMessage(`{"data":"a` + nulEscapeJSON + `b"}`),
	}}
	batches, err := captureAppendBatches(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Fatalf("unexpected batch shape: %d", len(batches))
	}
	input := batches[0][0]
	if input.Body != "out�put" {
		t.Fatalf("body = %q, want NUL replaced", input.Body)
	}
	if strings.Contains(string(input.Payload), nulEscapeJSON) {
		t.Fatalf("payload still contains NUL escape: %s", input.Payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal(input.Payload, &decoded); err != nil {
		t.Fatalf("sanitized payload not JSON: %v", err)
	}
	if decoded["data"] != "a�b" {
		t.Fatalf("payload data = %q, want sanitized", decoded["data"])
	}
}
