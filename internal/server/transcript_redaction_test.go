package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedactTranscriptEntriesHandler(t *testing.T) {
	const selector = `{"from_sequence":3,"until_sequence":4}`
	withRevision := `{"from_sequence":3,"until_sequence":4,"expected_redaction_set_revision":" ` + strings.Repeat("a", 64) + ` "}`
	tests := []struct {
		name, token, query, body, key, legacyHeader string
		wantStatus                                  int
		wantMessage                                 string
		wantCallback, wantApply                     bool
	}{
		{name: "agent", token: "agent", query: "dry_run=true", body: selector, wantStatus: 403, wantMessage: "only the account owner's operator token may redact transcript entries"},
		{name: "curator preview", token: "curator-preview", query: "dry_run=true", body: selector, wantStatus: 403, wantMessage: "credential profile is not authorized"},
		{name: "curator apply", token: "curator-apply", body: withRevision, key: "key", wantStatus: 403, wantMessage: "credential profile is not authorized"},
		{name: "pending", token: "pending", query: "dry_run=true", body: selector, wantStatus: 403, wantMessage: "account is pending — this action requires an active or suspended account"},
		{name: "closed", token: "closed", query: "dry_run=true", body: selector, wantStatus: 403, wantMessage: "account is closed — this action requires an active or suspended account"},
		{name: "missing bearer", query: "dry_run=true", body: selector, wantStatus: 401, wantMessage: "missing bearer token"},
		{name: "invalid bearer", token: "invalid", query: "dry_run=true", body: selector, wantStatus: 401, wantMessage: "invalid token"},
		{name: "authentication error", token: "auth-error", query: "dry_run=true", body: selector, wantStatus: 500, wantMessage: "internal error"},
		{name: "preview key", token: "operator", query: "dry_run=true", body: selector, key: "key", wantStatus: 400, wantMessage: "redaction preview accepts no mutation guards"},
		{name: "preview blank key", token: "operator", query: "dry_run=true", body: selector, key: " ", wantStatus: 400, wantMessage: "redaction preview accepts no mutation guards"},
		{name: "preview revision", token: "operator", query: "dry_run=true", body: withRevision, wantStatus: 400, wantMessage: "redaction preview accepts no mutation guards"},
		{name: "preview empty revision", token: "operator", query: "dry_run=true", body: `{"from_sequence":3,"until_sequence":4,"expected_redaction_set_revision":""}`, wantStatus: 400, wantMessage: "redaction preview accepts no mutation guards"},
		{name: "unknown field", token: "operator", query: "dry_run=true", body: `{"from_sequence":3,"until_sequence":4,"extra":true}`, wantStatus: 400, wantMessage: "invalid transcript redaction request"},
		{name: "reason code", token: "operator", query: "dry_run=true", body: `{"from_sequence":3,"until_sequence":4,"reason_code":"owner_request"}`, wantStatus: 400, wantMessage: "invalid transcript redaction request"},
		{name: "trailing value", token: "operator", query: "dry_run=true", body: selector + `{}`, wantStatus: 400, wantMessage: "invalid transcript redaction request"},
		{name: "oversized body", token: "operator", query: "dry_run=true", body: strings.Repeat(" ", 4096) + selector, wantStatus: 400, wantMessage: "invalid transcript redaction request"},
		{name: "null body", token: "operator", query: "dry_run=true", body: "null", wantStatus: 400, wantMessage: "invalid transcript redaction request"},
		{name: "empty body", token: "operator", query: "dry_run=true", wantStatus: 400, wantMessage: "invalid transcript redaction request"},
		{name: "bad dry run", token: "operator", query: "dry_run=maybe", body: selector, wantStatus: 400, wantMessage: "dry_run must be true or false"},
		{name: "empty dry run", token: "operator", query: "dry_run=", body: selector, wantStatus: 400, wantMessage: "dry_run must be true or false"},
		{name: "apply missing key", token: "operator", body: withRevision, wantStatus: 400, wantMessage: "expected_redaction_set_revision and Idempotency-Key are required"},
		{name: "apply missing revision", token: "operator", body: selector, key: "key", wantStatus: 400, wantMessage: "expected_redaction_set_revision and Idempotency-Key are required"},
		{name: "preview", token: "operator", query: "dry_run=true", body: selector, wantStatus: 200, wantCallback: true},
		{name: "suspended preview", token: "suspended", query: "dry_run=true", body: selector, wantStatus: 200, wantCallback: true},
		{name: "suspended apply", token: "suspended", body: withRevision, key: " key ", wantStatus: 200, wantCallback: true, wantApply: true},
		{name: "apply has no direct user header", token: "operator", body: withRevision, key: " key ", wantStatus: 200, wantCallback: true, wantApply: true},
		{name: "preview ignores legacy header", token: "operator", query: "dry_run=true", body: selector, legacyHeader: "yes", wantStatus: 200, wantCallback: true},
		{name: "apply ignores legacy header", token: "operator", body: withRevision, key: " key ", legacyHeader: "false", wantStatus: 200, wantCallback: true, wantApply: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			mux := apiMux(Config{
				AuthenticatePrincipal: transcriptRedactionTestAuth,
				RedactTranscriptEntries: func(_ context.Context, p DomainPrincipal, in RedactTranscriptEntriesRequest) (TranscriptRedactionReceipt, error) {
					calls++
					if p.Kind != PrincipalKindOperator || p.ID != "opr_owner" || p.AccountID != "acc_test" {
						t.Fatal("callback received incorrect authenticated principal")
					}
					if in.TranscriptID != "trn_test" || in.FromSequence != 3 || in.UntilSequence != 4 || in.Apply != tt.wantApply {
						t.Fatal("callback received incorrect selector or mode")
					}
					if in.Apply {
						if in.IdempotencyKey != "key" || in.ExpectedRedactionSetRevision != strings.Repeat("a", 64) {
							t.Fatal("apply callback guards were not preserved and trimmed")
						}
					} else if in.IdempotencyKey != "" || in.ExpectedRedactionSetRevision != "" {
						t.Fatal("preview callback received mutation guards")
					}
					return TranscriptRedactionReceipt{TranscriptID: in.TranscriptID, Applied: in.Apply, Entries: []TranscriptRedactionEntry{}, EvidenceMemoryIDs: []string{}}, nil
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/transcripts/trn_test/entries:redact?"+tt.query, strings.NewReader(tt.body))
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			if tt.key != "" {
				req.Header.Set("Idempotency-Key", tt.key)
			}
			if tt.legacyHeader != "" {
				req.Header.Set("X-Witself-Direct-User-Authorized", tt.legacyHeader)
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, req)
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, tt.wantStatus)
			}
			if (calls == 1) != tt.wantCallback {
				t.Fatalf("callback calls = %d, want called %t", calls, tt.wantCallback)
			}
			if tt.wantMessage != "" && !strings.Contains(response.Body.String(), tt.wantMessage) {
				t.Fatalf("response omitted expected message %q", tt.wantMessage)
			}
			if strings.Contains(response.Body.String(), "private-auth-error") {
				t.Fatal("authentication failure exposed internal error")
			}
			if response.Code == http.StatusOK {
				if response.Header().Get("Cache-Control") != "private, no-store" {
					t.Fatal("success omitted private no-store policy")
				}
				var envelope struct {
					SchemaVersion string                     `json:"schema_version"`
					Redaction     map[string]json.RawMessage `json:"redaction"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.SchemaVersion != "witself.v0" {
					t.Fatal("invalid response envelope")
				}
				for _, field := range []string{"curation_runs_to_cancel", "cancelled_curation_run_count"} {
					if string(envelope.Redaction[field]) != "0" {
						t.Fatalf("receipt omitted mandatory zero count %s", field)
					}
				}
			} else if !strings.Contains(response.Header().Get("Cache-Control"), "no-store") {
				t.Fatal("refusal omitted authenticated no-store policy")
			}
		})
	}
}

func transcriptRedactionTestAuth(_ context.Context, token string) (DomainPrincipal, bool, error) {
	p := DomainPrincipal{Kind: PrincipalKindOperator, ID: "opr_owner", AccountID: "acc_test", AccountStatus: "active"}
	switch token {
	case "operator":
	case "agent":
		p.Kind, p.ID, p.RealmID = PrincipalKindAgent, "agt_test", "rlm_test"
	case "curator-preview", "curator-apply":
		p.Kind, p.AccessProfile = PrincipalKindAgent, token
	case "suspended", "pending", "closed":
		p.AccountStatus = token
	case "auth-error":
		return DomainPrincipal{}, false, errors.New("private-auth-error")
	default:
		return DomainPrincipal{}, false, nil
	}
	return p, true, nil
}

func TestRedactTranscriptEntriesHandlerErrors(t *testing.T) {
	for _, tt := range []struct {
		name, message string
		err           error
		status        int
	}{
		{"bad input", "invalid transcript redaction request: invalid sequence range", fmt.Errorf("%w: invalid sequence range", ErrBadInput), 400},
		{"forbidden", "transcript redaction forbidden", ErrForbidden, 403},
		{"not found", "transcript not found", ErrNotFound, 404},
		{"revision conflict", "transcript entries changed since redaction preview", ErrConflict, 409},
		{"evacuation", "account move in progress", ErrAccountMoveInProgress, 409},
		{"already redacted", "every entry in the range is already redacted", ErrTranscriptEntriesRedacted, 410},
		{"internal error", "could not redact transcript entries", errors.New("private-callback-error"), 500},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mux := apiMux(Config{
				AuthenticatePrincipal: transcriptRedactionTestAuth,
				RedactTranscriptEntries: func(context.Context, DomainPrincipal, RedactTranscriptEntriesRequest) (TranscriptRedactionReceipt, error) {
					return TranscriptRedactionReceipt{}, tt.err
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/transcripts/trn_test/entries:redact?dry_run=true", strings.NewReader(`{"from_sequence":1,"until_sequence":1}`))
			req.Header.Set("Authorization", "Bearer operator")
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, req)
			if response.Code != tt.status || !strings.Contains(response.Body.String(), tt.message) {
				t.Fatalf("status = %d, want %d with bounded message %q", response.Code, tt.status, tt.message)
			}
			if strings.Contains(response.Body.String(), "private-callback-error") {
				t.Fatal("internal failure exposed callback error")
			}
		})
	}
}
