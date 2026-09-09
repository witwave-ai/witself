package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Count the actual bytes consumed, independently of Content-Length. This
// catches a decoder which reads an arbitrarily large document before asking
// the store to enforce its decoded-field limits.
type supportRequestCountingReader struct {
	reader io.Reader
	read   int
}

func (r *supportRequestCountingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func TestSupportTicketRequestBodyBounds(t *testing.T) {
	for _, route := range []struct {
		name, method, path, valid string
		maximum                   int
	}{
		{"open", http.MethodPost, "/v1/support/tickets", `{"subject":"subject","body":"body"}`, 512 * 1024},
		{"reply", http.MethodPost, "/v1/support/tickets/tkt_test/messages", `{"body":"body"}`, 512 * 1024},
		{"state", http.MethodPatch, "/v1/support/tickets/tkt_test/state", `{"state":"closed"}`, 8 * 1024},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, input := range []struct {
				name, body string
			}{
				{"oversized known field", strings.Replace(route.valid, `"`+map[string]string{"open": "body", "reply": "body", "state": "closed"}[route.name]+`"}`, `"`+strings.Repeat("x", 2*route.maximum)+`"}`, 1)},
				{"oversized unknown field", route.valid[:len(route.valid)-1] + `,"unused":"` + strings.Repeat("x", 2*route.maximum) + `"}`},
				{"oversized trailing whitespace", route.valid + strings.Repeat(" ", 2*route.maximum)},
				{"second JSON value", route.valid + ` {"extra":true}`},
				{"trailing garbage", route.valid + ` not-json`},
			} {
				t.Run(input.name, func(t *testing.T) {
					calls := 0
					mux := supportRequestBoundsMux(&calls, nil)
					body := &supportRequestCountingReader{reader: strings.NewReader(input.body)}
					req := httptest.NewRequest(route.method, "http://support.test"+route.path, body)
					req.Header.Set("Authorization", "Bearer operator-token")
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, req)
					if w.Code != http.StatusBadRequest || calls != 0 {
						t.Errorf("status=%d, store calls=%d, consumed=%d; want 400 without a store call", w.Code, calls, body.read)
					}
					if body.read > route.maximum+1 {
						t.Errorf("consumed %d request bytes; ceiling is %d plus one overflow byte", body.read, route.maximum)
					}
					if strings.Contains(w.Body.String(), "unused") || strings.Contains(w.Body.String(), "not-json") {
						t.Errorf("error response contains request content: %s", w.Body.String())
					}
				})
			}
		})
	}
}

func TestSupportTicketRequestBodyPreservesValidInputs(t *testing.T) {
	// JSON permits every ASCII byte to use a six-byte escape. A transport cap
	// of 64 KiB would incorrectly reject an otherwise valid maximum-size body.
	decodedBody := strings.Repeat("a", 64*1024)
	escapedBody := strings.Repeat(`\u0061`, 64*1024)
	for _, input := range []struct {
		name, method, path, body, wantBody string
		wantStatus                         int
	}{
		{"open escaped maximum", http.MethodPost, "/v1/support/tickets", `{"subject":"` + strings.Repeat(`\u0061`, 200) + `","body":"` + escapedBody + `","category":"technical","priority":"normal"}`, decodedBody, http.StatusCreated},
		{"reply escaped maximum", http.MethodPost, "/v1/support/tickets/tkt_test/messages", `{"body":"` + escapedBody + `"}`, decodedBody, http.StatusCreated},
		{"open unknown field and whitespace", http.MethodPost, "/v1/support/tickets", " \n" + `{"subject":"subject","body":"body","unused":true}` + " \t", "body", http.StatusCreated},
		{"reply unknown field and whitespace", http.MethodPost, "/v1/support/tickets/tkt_test/messages", " \n" + `{"body":"body","unused":true}` + " \t", "body", http.StatusCreated},
		{"state unknown field and whitespace", http.MethodPatch, "/v1/support/tickets/tkt_test/state", " \n" + `{"state":"closed","unused":true}` + " \t", "", http.StatusOK},
	} {
		t.Run(input.name, func(t *testing.T) {
			calls := 0
			var receivedBody string
			mux := supportRequestBoundsMux(&calls, &receivedBody)
			req := httptest.NewRequest(input.method, "http://support.test"+input.path, strings.NewReader(input.body))
			req.Header.Set("Authorization", "Bearer operator-token")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != input.wantStatus || calls != 1 || receivedBody != input.wantBody {
				t.Fatalf("status=%d, store calls=%d, body bytes=%d; want status=%d, one call, body bytes=%d", w.Code, calls, len(receivedBody), input.wantStatus, len(input.wantBody))
			}
		})
	}
}

func supportRequestBoundsMux(calls *int, receivedBody *string) http.Handler {
	return apiMux(Config{
		Authenticate: func(context.Context, string) (string, string, string, bool, error) {
			return "operator", "account", "active", true, nil
		},
		OpenSupportTicket: func(_ context.Context, in OpenTicketRequest) (SupportTicket, SupportTicketMessage, error) {
			*calls++
			if receivedBody != nil {
				*receivedBody = in.Body
			}
			return SupportTicket{}, SupportTicketMessage{}, nil
		},
		ReplySupportTicket: func(_ context.Context, _, _, _, body string) (SupportTicketMessage, error) {
			*calls++
			if receivedBody != nil {
				*receivedBody = body
			}
			return SupportTicketMessage{}, nil
		},
		ChangeSupportTicketState: func(context.Context, ChangeTicketStateRequest) (SupportTicket, error) {
			*calls++
			return SupportTicket{}, nil
		},
	})
}
