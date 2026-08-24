package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSupportContactMatchAdminRoute(t *testing.T) {
	const provisionToken = "witself_prv_support_email"
	var (
		calls    int
		gotEmail string
	)
	handler := apiMux(Config{
		ProvisionToken: provisionToken,
		MatchSupportContact: func(_ context.Context, email string) ([]SupportContactMatch, error) {
			calls++
			gotEmail = email
			return []SupportContactMatch{{AccountID: "acc_match", Status: "active"}}, nil
		},
	})

	request := func(token, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/support/admin:match-contact", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	if recorder := request("wrong", `{"email":"owner@example.test"}`); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	if calls != 0 {
		t.Fatalf("wrong-token callback calls = %d, want 0", calls)
	}
	if recorder := request(provisionToken, `{}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing-email status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	recorder := request(provisionToken, `{"email":"  Owner@Example.Test  "}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("success status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if gotEmail != "Owner@Example.Test" {
		t.Fatalf("matched email = %q", gotEmail)
	}
	var response struct {
		SchemaVersion string                `json:"schema_version"`
		Matches       []SupportContactMatch `json:"matches"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.SchemaVersion != "witself.v0" || len(response.Matches) != 1 ||
		response.Matches[0] != (SupportContactMatch{AccountID: "acc_match", Status: "active"}) {
		t.Fatalf("match response = %#v", response)
	}
}

func TestSupportEmailTicketAdminRoutesAreValueFree(t *testing.T) {
	const (
		provisionToken = "witself_prv_support_email"
		sender         = "private-sender@example.test"
		subject        = "private support subject"
		openingBody    = "private opening body"
		replyBody      = "private reply body"
	)
	var (
		gotOpen  OpenEmailTicketRequest
		gotReply ReplyEmailTicketRequest
	)
	cfg := supportEmailAdminTestConfig(provisionToken)
	cfg.OpenEmailTicket = func(_ context.Context, in OpenEmailTicketRequest) (SupportTicket, SupportTicketMessage, error) {
		gotOpen = in
		return SupportTicket{ID: "tkt_open", Subject: subject},
			SupportTicketMessage{ID: "tkm_open", Body: openingBody}, nil
	}
	cfg.ReplyEmailTicket = func(_ context.Context, in ReplyEmailTicketRequest) (SupportTicketMessage, error) {
		gotReply = in
		return SupportTicketMessage{ID: "tkm_reply", Body: replyBody}, nil
	}
	handler := apiMux(cfg)

	openBody := `{"email":"` + sender + `","subject":"` + subject + `","body":"` + openingBody + `","email_message_id":"<open@example.test>"}`
	open := supportEmailAdminRequest(t, handler, provisionToken,
		"/v1/accounts/acc_target/admin:open-email-ticket", openBody)
	if open.Code != http.StatusCreated {
		t.Fatalf("open status = %d, want %d: %s", open.Code, http.StatusCreated, open.Body.String())
	}
	if gotOpen != (OpenEmailTicketRequest{
		AccountID: "acc_target", Email: sender, Subject: subject,
		Body: openingBody, EmailMessageID: "<open@example.test>",
	}) {
		t.Fatalf("open request = %#v", gotOpen)
	}
	assertSupportEmailResponseValueFree(t, open.Body.String(), sender, subject, openingBody)
	var openResponse map[string]string
	if err := json.Unmarshal(open.Body.Bytes(), &openResponse); err != nil {
		t.Fatal(err)
	}
	if openResponse["schema_version"] != "witself.v0" || openResponse["ticket_id"] != "tkt_open" ||
		openResponse["message_id"] != "tkm_open" {
		t.Fatalf("open response = %#v", openResponse)
	}

	replyRequestBody := `{"email":"` + sender + `","ticket_id":"tkt_open","body":"` + replyBody + `","email_message_id":"<reply@example.test>"}`
	reply := supportEmailAdminRequest(t, handler, provisionToken,
		"/v1/accounts/acc_target/admin:reply-email-ticket", replyRequestBody)
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply status = %d, want %d: %s", reply.Code, http.StatusCreated, reply.Body.String())
	}
	if gotReply != (ReplyEmailTicketRequest{
		AccountID: "acc_target", Email: sender, TicketID: "tkt_open",
		Body: replyBody, EmailMessageID: "<reply@example.test>",
	}) {
		t.Fatalf("reply request = %#v", gotReply)
	}
	assertSupportEmailResponseValueFree(t, reply.Body.String(), sender, replyBody)
	var replyResponse map[string]string
	if err := json.Unmarshal(reply.Body.Bytes(), &replyResponse); err != nil {
		t.Fatal(err)
	}
	if replyResponse["schema_version"] != "witself.v0" || replyResponse["message_id"] != "tkm_reply" {
		t.Fatalf("reply response = %#v", replyResponse)
	}
}

func TestSupportEmailTicketAdminErrorMapping(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		reply bool
		want  int
	}{
		{name: "input", err: ErrTicketInputInvalid, want: http.StatusBadRequest},
		{name: "sender mismatch", err: ErrSupportSenderMismatch, want: http.StatusForbidden},
		{name: "entitlement", err: ErrSupportNotIncluded, want: http.StatusForbidden},
		{name: "support disabled", err: ErrSupportDisabled, want: http.StatusConflict},
		{name: "account missing", err: ErrNotFound, want: http.StatusNotFound},
		{name: "ticket missing", err: ErrTicketNotFound, reply: true, want: http.StatusNotFound},
		{name: "closed", err: ErrTicketStateInvalid, reply: true, want: http.StatusConflict},
		{name: "internal", err: errors.New("database unavailable"), reply: true, want: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const token = "witself_prv_support_email"
			cfg := supportEmailAdminTestConfig(token)
			cfg.OpenEmailTicket = func(context.Context, OpenEmailTicketRequest) (SupportTicket, SupportTicketMessage, error) {
				return SupportTicket{}, SupportTicketMessage{}, test.err
			}
			cfg.ReplyEmailTicket = func(context.Context, ReplyEmailTicketRequest) (SupportTicketMessage, error) {
				return SupportTicketMessage{}, test.err
			}
			path := "/v1/accounts/acc_target/admin:open-email-ticket"
			body := `{"email":"private@example.test","subject":"private subject","body":"private body"}`
			if test.reply {
				path = "/v1/accounts/acc_target/admin:reply-email-ticket"
				body = `{"email":"private@example.test","ticket_id":"tkt_target","body":"private body"}`
			}
			recorder := supportEmailAdminRequest(t, apiMux(cfg), token, path, body)
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.want, recorder.Body.String())
			}
			assertSupportEmailResponseValueFree(t, recorder.Body.String(),
				"private@example.test", "private subject", "private body")
		})
	}
}

func supportEmailAdminTestConfig(provisionToken string) Config {
	return Config{
		ProvisionToken: provisionToken,
		ProvisionAccountExact: func(context.Context, string, string, string) (ProvisionedAccount, error) {
			return ProvisionedAccount{}, errors.New("unused")
		},
	}
}

func supportEmailAdminRequest(t *testing.T, handler http.Handler, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func assertSupportEmailResponseValueFree(t *testing.T, response string, values ...string) {
	t.Helper()
	for _, value := range values {
		if value != "" && strings.Contains(response, value) {
			t.Fatalf("support email response leaked %q: %s", value, response)
		}
	}
}
