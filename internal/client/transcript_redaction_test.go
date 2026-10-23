package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTranscriptRedactionClientContract(t *testing.T) {
	const transcriptID = "trn_contract/part?query=value"
	revision := strings.Repeat("a", 64)
	createdAt := time.Date(2026, 10, 22, 18, 4, 5, 123456000, time.UTC)
	for _, apply := range []bool{false, true} {
		name := "preview"
		if apply {
			name = "apply"
		}
		t.Run(name, func(t *testing.T) {
			want := TranscriptRedactionReceipt{
				TranscriptID: transcriptID, FromSequence: 3, UntilSequence: 4,
				RedactionSetRevision: revision, EntryCount: 2, RedactableCount: 1,
				AlreadyRedactedCount: 1, PartialCaptureEvent: true,
				MemoryEvidenceCount: 1, EvidenceMemoryIDs: []string{"mem_one"},
				CurationRunsToCancel: 2,
				Entries: []TranscriptRedactionEntry{
					{EntryID: "ent_three", Sequence: 3, Role: "tool", Kind: "tool.result", CreatedAt: createdAt},
					{EntryID: "ent_four", Sequence: 4, Role: "assistant", Redacted: true, CreatedAt: createdAt},
				},
			}
			wantBody := `{"from_sequence":3,"until_sequence":4}`
			wantQuery, wantKey := "dry_run=true", ""
			if apply {
				wantBody = `{"from_sequence":3,"until_sequence":4,"expected_redaction_set_revision":"` + revision + `"}`
				wantQuery, wantKey = "", "redaction-contract-key"
				want.Applied, want.Replayed = true, true
				want.ReceiptID = "trd_0123456789abcdef0123456789abcdef"
				want.RedactedCount, want.CancelledCurationRunCount = 1, 2
				want.RedactedAt = &createdAt
				want.Entries[0].Redacted = true
			}
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want POST", r.Method)
				}
				if r.URL.EscapedPath() != "/v1/transcripts/trn_contract%2Fpart%3Fquery=value/entries:redact" {
					t.Error("transcript id was not escaped as one path segment")
				}
				if r.URL.RawQuery != wantQuery {
					t.Errorf("query = %q, want %q", r.URL.RawQuery, wantQuery)
				}
				if r.Header.Get("Authorization") != "Bearer operator-test-token" {
					t.Error("operator bearer token missing or changed")
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Error("request must declare JSON content")
				}
				if r.Header.Get("Idempotency-Key") != wantKey {
					t.Error("Idempotency-Key must appear only on apply and match the input")
				}
				if !apply {
					if _, exists := r.Header["Idempotency-Key"]; exists {
						t.Error("preview must not send even an empty Idempotency-Key")
					}
				}
				if _, exists := r.Header["X-Witself-Direct-User-Authorized"]; exists {
					t.Error("owner redaction must never send a direct-user header")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error("could not read request body")
				} else if string(body) != wantBody {
					t.Error("request body differs from the exact redaction contract")
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(struct {
					SchemaVersion string                     `json:"schema_version"`
					Redaction     TranscriptRedactionReceipt `json:"redaction"`
				}{"witself.v0", want}); err != nil {
					t.Error("could not encode test receipt")
				}
			}))
			defer srv.Close()
			var got *TranscriptRedactionReceipt
			var err error
			if apply {
				got, err = RedactTranscriptEntries(context.Background(), srv.URL+"/", "operator-test-token", RedactTranscriptEntriesInput{
					TranscriptID: transcriptID, FromSequence: 3, UntilSequence: 4,
					ExpectedRedactionSetRevision: revision, IdempotencyKey: wantKey,
				})
			} else {
				got, err = PreviewRedactTranscriptEntries(context.Background(), srv.URL+"/", "operator-test-token", transcriptID, 3, 4)
			}
			if err != nil {
				t.Fatalf("redaction request failed: %v", err)
			}
			if !reflect.DeepEqual(got, &want) {
				t.Error("decoded receipt differs from the response")
			}
			if requests != 1 {
				t.Errorf("requests = %d, want 1", requests)
			}
		})
	}
}

func TestTranscriptRedactionReceiptCurationCounts(t *testing.T) {
	encoded, err := json.Marshal(TranscriptRedactionReceipt{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"curation_runs_to_cancel", "cancelled_curation_run_count"} {
		if string(fields[name]) != "0" {
			t.Errorf("%s must be present as an integer, including zero", name)
		}
	}
	if _, exists := fields["active_curation_run_count"]; exists {
		t.Error("receipt must not expose the superseded active-curation field")
	}
	var receipt TranscriptRedactionReceipt
	if err := json.Unmarshal([]byte(`{"curation_runs_to_cancel":2147483648,"cancelled_curation_run_count":2147483649}`), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.CurationRunsToCancel != 2147483648 || receipt.CancelledCurationRunCount != 2147483649 {
		t.Error("curation counts must preserve int64 values")
	}
}
