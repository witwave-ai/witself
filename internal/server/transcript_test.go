package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTranscriptAgentWritesAndOperatorReads(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		switch token {
		case "agent-token":
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AgentName: "scott", AccountStatus: "active"}, true, nil
		case "operator-token":
			return DomainPrincipal{Kind: PrincipalKindOperator, ID: "opr_1", AccountID: "acc_1", AccountStatus: "active"}, true, nil
		case "suspended-token":
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "suspended"}, true, nil
		default:
			return DomainPrincipal{}, false, nil
		}
	}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	createCalls := 0
	appendCalls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		CreateTranscript: func(_ context.Context, p DomainPrincipal, in CreateTranscriptRequest) (Transcript, error) {
			createCalls++
			if p.Kind != PrincipalKindAgent || p.ID != "agent_1" || in.Title != "Deployment" {
				t.Fatalf("create input = %#v / %#v", p, in)
			}
			var metadata map[string]any
			if err := json.Unmarshal(in.Metadata, &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata["agent_id"] != "agent_1" || metadata["agent_name"] != "scott" {
				t.Fatalf("metadata = %#v", metadata)
			}
			return Transcript{ID: "trn_1", AccountID: p.AccountID, RealmID: p.RealmID, OwnerAgentID: p.ID, Title: in.Title, Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now}, nil
		},
		AppendTranscriptEntry: func(_ context.Context, p DomainPrincipal, transcriptID string, in AppendTranscriptEntryRequest) (TranscriptEntry, error) {
			appendCalls++
			if in.ExternalID == "duplicate" {
				return TranscriptEntry{}, ErrConflict
			}
			if p.ID != "agent_1" || transcriptID != "trn_1" || in.ExternalID != "vendor-message-2" || in.Role != "assistant" || in.ReplyToEntryID != "ent_prompt" {
				t.Fatalf("append input = %#v / %q / %#v", p, transcriptID, in)
			}
			return TranscriptEntry{ID: "ent_reply", AccountID: p.AccountID, TranscriptID: transcriptID, RealmID: p.RealmID, RecordedByAgentID: p.ID, Sequence: 2, Role: in.Role, Body: in.Body, ReplyToEntryID: in.ReplyToEntryID, Artifacts: json.RawMessage(`[]`), CreatedAt: now}, nil
		},
		ListTranscripts: func(_ context.Context, p DomainPrincipal) ([]Transcript, error) {
			if p.Kind != PrincipalKindOperator {
				t.Fatalf("list principal = %#v", p)
			}
			return []Transcript{{ID: "trn_1", AccountID: p.AccountID, OwnerAgentID: "agent_1", Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now}}, nil
		},
		GetTranscript: func(_ context.Context, p DomainPrincipal, transcriptID string) (Transcript, []TranscriptEntry, error) {
			if p.Kind != PrincipalKindOperator || transcriptID != "trn_1" {
				t.Fatalf("get principal/id = %#v / %s", p, transcriptID)
			}
			return Transcript{ID: transcriptID, AccountID: p.AccountID, OwnerAgentID: "agent_1", Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now}, []TranscriptEntry{}, nil
		},
	}))
	defer srv.Close()

	resp := transcriptRequest(t, srv.URL, http.MethodPost, "/v1/transcripts", "agent-token", `{"title":"Deployment"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("agent create = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	resp = transcriptRequest(t, srv.URL, http.MethodPost, "/v1/transcripts/trn_1/entries", "agent-token", `{"external_id":"vendor-message-2","role":"assistant","body":"Done","reply_to_entry_id":"ent_prompt"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("agent append = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	resp = transcriptRequest(t, srv.URL, http.MethodPost, "/v1/transcripts/trn_1/entries", "agent-token", `{"external_id":"duplicate","role":"assistant","body":"Done"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate append = %d, want 409", resp.StatusCode)
	}
	closeBody(t, resp)

	resp = transcriptRequest(t, srv.URL, http.MethodGet, "/v1/transcripts", "operator-token", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("operator list = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	resp = transcriptRequest(t, srv.URL, http.MethodGet, "/v1/transcripts/trn_1", "operator-token", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("operator show = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	resp = transcriptRequest(t, srv.URL, http.MethodPost, "/v1/transcripts", "operator-token", `{}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator create = %d, want 403", resp.StatusCode)
	}
	closeBody(t, resp)
	if createCalls != 1 {
		t.Fatalf("create hook calls = %d, operator write reached hook", createCalls)
	}

	resp = transcriptRequest(t, srv.URL, http.MethodPost, "/v1/transcripts/trn_1/entries", "operator-token", `{"role":"user","body":"forged"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator append = %d, want 403", resp.StatusCode)
	}
	closeBody(t, resp)
	if appendCalls != 2 {
		t.Fatalf("append hook calls = %d, operator write reached hook", appendCalls)
	}

	resp = transcriptRequest(t, srv.URL, http.MethodGet, "/v1/transcripts", "suspended-token", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("suspended list = %d, want 403", resp.StatusCode)
	}
	closeBody(t, resp)
}

func TestTranscriptBatchAndPagedRead(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token", nil
	}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		AppendTranscriptEntries: func(_ context.Context, _ DomainPrincipal, transcriptID string, inputs []AppendTranscriptEntryRequest) ([]TranscriptEntry, error) {
			if transcriptID != "trn_1" || len(inputs) != 2 || inputs[1].ReplyToExternalID != "evt_1:0" {
				t.Fatalf("batch = %q / %#v", transcriptID, inputs)
			}
			return []TranscriptEntry{{ID: "ent_1", Sequence: 1}, {ID: "ent_2", Sequence: 2}}, nil
		},
		GetTranscriptPage: func(_ context.Context, _ DomainPrincipal, transcriptID string, opts TranscriptPageOptions) (TranscriptPage, error) {
			if transcriptID != "trn_1" || opts.AfterSequence != 2 || opts.Limit != 25 || opts.Tail {
				t.Fatalf("page = %q / %#v", transcriptID, opts)
			}
			return TranscriptPage{
				Transcript:        Transcript{ID: transcriptID, Metadata: json.RawMessage(`{}`)},
				Entries:           []TranscriptEntry{{ID: "ent_3", Sequence: 3}},
				NextAfterSequence: 3,
			}, nil
		},
	}))
	defer srv.Close()

	resp := transcriptRequest(t, srv.URL, http.MethodPost, "/v1/transcripts/trn_1/entries:batch", "agent-token", `{"entries":[{"external_id":"evt_1:0","role":"user","body":"hello"},{"external_id":"evt_2:0","role":"assistant","body":"hi","reply_to_external_id":"evt_1:0"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("batch status = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	resp = transcriptRequest(t, srv.URL, http.MethodGet, "/v1/transcripts/trn_1?after_sequence=2&limit=25", "agent-token", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d", resp.StatusCode)
	}
	var page struct {
		Next int64 `json:"next_after_sequence"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if page.Next != 3 {
		t.Fatalf("next = %d", page.Next)
	}
}

func transcriptRequest(t *testing.T, base, method, path, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
