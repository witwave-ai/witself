package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTranscriptBatchAndPageContracts(t *testing.T) {
	var sawBatch, sawPage, sawObservationalPage bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			sawBatch = true
			var body struct {
				Entries []AppendTranscriptEntryInput `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Entries) != 1 || body.Entries[0].ReplyToExternalID != "evt_prompt:0" {
				t.Fatalf("batch body = %#v", body)
			}
			_, _ = w.Write([]byte(`{"entries":[{"id":"ent_2","transcript_id":"trn_1","sequence":2,"role":"assistant","artifacts":[]}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/transcripts/trn_1":
			if r.URL.Query().Get("observational") == "true" {
				sawObservationalPage = true
			} else {
				sawPage = true
			}
			if r.URL.Query().Get("after_sequence") != "5" || r.URL.Query().Get("limit") != "25" || r.URL.Query().Get("tail") != "" {
				t.Fatalf("page query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}},"entries":[],"next_after_sequence":12}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	entries, err := AppendTranscriptEntries(context.Background(), srv.URL, "token", "trn_1", []AppendTranscriptEntryInput{{
		ExternalID: "evt_reply:0", Role: "assistant", Body: "done", ReplyToExternalID: "evt_prompt:0",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "ent_2" {
		t.Fatalf("entries = %#v", entries)
	}
	page, err := GetTranscriptPage(context.Background(), srv.URL, "token", "trn_1", TranscriptPageOptions{AfterSequence: 5, Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextAfterSequence != 12 || !sawBatch || !sawPage {
		t.Fatalf("page/requests = %#v / %v / %v", page, sawBatch, sawPage)
	}
	if _, err := GetTranscriptPage(context.Background(), srv.URL, "token", "trn_1", TranscriptPageOptions{AfterSequence: 5, Limit: 25, Observational: true}); err != nil {
		t.Fatal(err)
	}
	if !sawObservationalPage {
		t.Fatal("observational transcript page query was not sent")
	}
}

func TestGetTranscriptPageRedactionView(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/transcripts/trn_redacted" {
			t.Error("unexpected transcript page request")
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"transcript":{"id":"trn_redacted","metadata":{}},"entries":[{"id":"ent_redacted","transcript_id":"trn_redacted","sequence":1,"role":"tool","artifacts":[],"redaction":{"receipt_id":"trd_0123456789abcdef0123456789abcdef","redacted_at":"2026-10-22T18:04:05.123456Z","reason_code":"owner_request"}}]}`))
	}))
	defer srv.Close()

	page, err := GetTranscriptPage(context.Background(), srv.URL, "test-token", "trn_redacted", TranscriptPageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("transcript page entry count = %d, want 1", len(page.Entries))
	}
	redaction := page.Entries[0].Redaction
	if redaction == nil {
		t.Fatal("transcript page omitted the redaction view")
	}
	if redaction.ReceiptID != "trd_0123456789abcdef0123456789abcdef" ||
		!redaction.RedactedAt.Equal(time.Date(2026, 10, 22, 18, 4, 5, 123456000, time.UTC)) ||
		redaction.ReasonCode != "owner_request" {
		t.Fatalf("transcript page redaction view = %#v", redaction)
	}
}
