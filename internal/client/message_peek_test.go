package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPeekMessageClientContract(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != 0 || r.Method != http.MethodGet ||
			r.URL.EscapedPath() != "/v1/messages/msg_encoded%2Fpart%3Fvalue:peek" || r.URL.RawQuery != "" ||
			r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("peek request did not use an authenticated, escaped, body-free GET")
		}
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","message":{"id":"msg_1","body":"observe only","payload":{"task":42},"read_state":{"state":"unread"},"processing":{"state":"claimed","generation":3}}}`))
	}))
	defer srv.Close()
	msg, err := PeekMessage(context.Background(), srv.URL+"/", "synthetic-token", "msg_encoded/part?value")
	if err != nil || calls != 1 || msg.ID != "msg_1" || msg.Body != "observe only" ||
		string(msg.Payload) != `{"task":42}` || msg.ReadState.State != "unread" ||
		msg.Processing.State != "claimed" || msg.Processing.Generation != 3 {
		t.Fatalf("peek client response mismatch: calls=%d error=%v", calls, err)
	}
}

func TestPeekMessageClientDoesNotFallbackToRead(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/messages/msg_1:peek" {
			t.Error("peek attempted a mutating fallback")
		}
		http.Error(w, `{"error":"message not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	msg, err := PeekMessage(context.Background(), srv.URL, "synthetic-token", "msg_1")
	if err == nil || calls != 1 || msg.ID != "" || msg.Body != "" {
		t.Fatalf("missing observational API result: calls=%d error=%v", calls, err)
	}
}
