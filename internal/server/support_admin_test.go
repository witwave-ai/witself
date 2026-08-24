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

func TestAdminReplyTicketPassesAssistantAttribution(t *testing.T) {
	var got ReplyAdminTicketRequest
	handler := accountLifecycleHandler(Config{
		ProvisionToken: "witself_prv_test",
		ReplyAdminTicket: func(_ context.Context, in ReplyAdminTicketRequest) (SupportTicketMessage, error) {
			got = in
			return SupportTicketMessage{ID: "msg_1", TicketID: in.TicketID}, nil
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/accounts/acct_1/admin:reply-ticket",
		strings.NewReader(`{"admin_handle":"support-bot","ticket_id":"tkt_1","body":"hello","as_assistant":true}`),
	)
	req.Header.Set("Authorization", "Bearer witself_prv_test")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusCreated)
	}
	if got.AccountID != "acct_1" || got.AdminHandle != "support-bot" ||
		got.TicketID != "tkt_1" || got.Body != "hello" || !got.AsAssistant {
		t.Fatalf("reply input = %#v", got)
	}
}

func TestAdminRetriageTicketRoute(t *testing.T) {
	var (
		got       RetriageAdminTicketRequest
		returnErr error
		calls     int
	)
	handler := accountLifecycleHandler(Config{
		ProvisionToken: "witself_prv_test",
		RetriageAdminTicket: func(_ context.Context, in RetriageAdminTicketRequest) (SupportTicket, error) {
			calls++
			got = in
			return SupportTicket{
				ID:        in.TicketID,
				AccountID: in.AccountID,
				Category:  in.Category,
				Priority:  in.Priority,
			}, returnErr
		},
	})

	post := func(t *testing.T, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(
			http.MethodPost,
			"/v1/accounts/acct_1/admin:retriage-ticket",
			strings.NewReader(`{"admin_handle":"support-bot","ticket_id":"tkt_1","category":"security","priority":"urgent"}`),
		)
		req.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	recorder := post(t, "wrong")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	if calls != 0 {
		t.Fatalf("wrong-token calls = %d, want 0", calls)
	}

	recorder = post(t, "witself_prv_test")
	if recorder.Code != http.StatusOK {
		t.Fatalf("success status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got != (RetriageAdminTicketRequest{
		AccountID:   "acct_1",
		AdminHandle: "support-bot",
		TicketID:    "tkt_1",
		Category:    "security",
		Priority:    "urgent",
	}) {
		t.Fatalf("retriage input = %#v", got)
	}
	var out struct {
		Ticket SupportTicket `json:"ticket"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Ticket.ID != "tkt_1" || out.Ticket.Category != "security" ||
		out.Ticket.Priority != "urgent" {
		t.Fatalf("ticket response = %#v", out.Ticket)
	}
}

func TestAdminRetriageTicketErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "invalid input", err: ErrTicketInputInvalid, want: http.StatusBadRequest},
		{name: "ticket missing", err: ErrTicketNotFound, want: http.StatusNotFound},
		{name: "account missing", err: ErrNotFound, want: http.StatusNotFound},
		{name: "closed ticket", err: ErrTicketStateInvalid, want: http.StatusConflict},
		{name: "internal error", err: errors.New("database unavailable"), want: http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := accountLifecycleHandler(Config{
				ProvisionToken: "witself_prv_test",
				RetriageAdminTicket: func(context.Context, RetriageAdminTicketRequest) (SupportTicket, error) {
					return SupportTicket{}, tc.err
				},
			})

			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/accounts/acct_1/admin:retriage-ticket",
				strings.NewReader(`{"admin_handle":"support-bot","ticket_id":"tkt_1","priority":"urgent"}`),
			)
			req.Header.Set("Authorization", "Bearer witself_prv_test")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.want)
			}
		})
	}
}
