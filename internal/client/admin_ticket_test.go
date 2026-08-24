package client

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type adminTicketRoundTripFunc func(*http.Request) (*http.Response, error)

func (f adminTicketRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func adminTicketResponse(r *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

func TestReplyAdminTicketAsAssistant(t *testing.T) {
	var got map[string]any
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	http.DefaultTransport = adminTicketRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost ||
			r.URL.Path != "/v1/admin/accounts/acct_1/tickets/tkt_1/messages" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer witself_adm_test" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return adminTicketResponse(r, `{
			"message": {
				"id": "msg_1",
				"ticket_id": "tkt_1",
				"account_id": "acct_1",
				"author_kind": "assistant",
				"author_id": "assistant",
				"body": "hello"
			}
		}`), nil
	})

	msg, err := ReplyAdminTicketAsAssistant(
		t.Context(), "https://cp.example/", "witself_adm_test", "acct_1", "tkt_1", "hello",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["body"] != "hello" || got["as_assistant"] != true {
		t.Fatalf("request body = %#v", got)
	}
	if msg.ID != "msg_1" || msg.AuthorKind != "assistant" || msg.AuthorID != "assistant" {
		t.Fatalf("message = %#v", msg)
	}
}

func TestRetriageAdminTicket(t *testing.T) {
	var got map[string]any
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	http.DefaultTransport = adminTicketRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPatch ||
			r.URL.Path != "/v1/admin/accounts/acct_1/tickets/tkt_1/retriage" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer witself_adm_test" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return adminTicketResponse(r, `{
			"ticket": {
				"id": "tkt_1",
				"account_id": "acct_1",
				"category": "security",
				"priority": "urgent"
			}
		}`), nil
	})

	ticket, err := RetriageAdminTicket(
		t.Context(), "https://cp.example/", "witself_adm_test", "acct_1", "tkt_1",
		"security", "urgent",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["category"] != "security" || got["priority"] != "urgent" {
		t.Fatalf("request body = %#v", got)
	}
	if ticket.ID != "tkt_1" || ticket.Category != "security" || ticket.Priority != "urgent" {
		t.Fatalf("ticket = %#v", ticket)
	}
}
