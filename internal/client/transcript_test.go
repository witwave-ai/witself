package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTranscriptBatchAndPageContracts(t *testing.T) {
	var sawBatch, sawPage bool
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
			sawPage = true
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
}
