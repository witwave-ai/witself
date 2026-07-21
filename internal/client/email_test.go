package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentEmailClientRoutes(t *testing.T) {
	type seenRequest struct {
		method, path, query, key string
		body                     map[string]any
	}
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := seenRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, key: r.Header.Get("Idempotency-Key")}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&entry.body)
		}
		seen = append(seen, entry)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/email/address":
			_, _ = w.Write([]byte(`{"address":{"id":"eaddr_aaaaaaaaaaaaaaaa","address":"owner.realm@example.com"}}`))
		case r.URL.Path == "/v1/email":
			_, _ = w.Write([]byte(`{"messages":[],"next_cursor":"next"}`))
		case r.URL.Path == "/v1/email:listen":
			_, _ = w.Write([]byte(`{"messages":[],"timed_out":true}`))
		case strings.HasSuffix(r.URL.Path, ":read") || strings.HasSuffix(r.URL.Path, ":ack") || strings.HasSuffix(r.URL.Path, ":code-consumed"):
			_, _ = w.Write([]byte(`{"message":{"id":"emsg_aaaaaaaaaaaaaaaa","sender_verification_state":"unverified"}}`))
		default:
			_, _ = w.Write([]byte(`{"processing":{"state":"claimed","claim_id":"ecl_aaaaaaaaaaaaaaaa","generation":1}}`))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if _, err := ShowAgentEmailAddress(ctx, srv.URL, "token"); err != nil {
		t.Fatal(err)
	}
	if page, err := ListAgentEmails(ctx, srv.URL, "token", AgentEmailListOptions{Unread: true, Unacked: true, Limit: 7, Cursor: "cursor"}); err != nil || page.Messages == nil {
		t.Fatalf("list = %#v / %v", page, err)
	}
	wait := 0
	if result, err := ListenAgentEmails(ctx, srv.URL, "token", AgentEmailListenOptions{WaitSeconds: &wait, Limit: 3}); err != nil || !result.TimedOut || result.Messages == nil {
		t.Fatalf("listen = %#v / %v", result, err)
	}
	for _, action := range []func() error{
		func() error { _, err := ReadAgentEmail(ctx, srv.URL, "token", "emsg_aaaaaaaaaaaaaaaa"); return err },
		func() error { _, err := AckAgentEmail(ctx, srv.URL, "token", "emsg_aaaaaaaaaaaaaaaa"); return err },
		func() error {
			_, err := MarkAgentEmailCodeConsumed(ctx, srv.URL, "token", "emsg_aaaaaaaaaaaaaaaa")
			return err
		},
		func() error {
			_, err := ClaimAgentEmail(ctx, srv.URL, "token", "emsg_aaaaaaaaaaaaaaaa", ClaimAgentEmailInput{LeaseSeconds: 60, IdempotencyKey: "claim-key"})
			return err
		},
		func() error {
			_, err := RenewAgentEmailClaim(ctx, srv.URL, "token", "emsg_aaaaaaaaaaaaaaaa", RenewAgentEmailClaimInput{ClaimID: "ecl_aaaaaaaaaaaaaaaa", Generation: 1, LeaseSeconds: 90})
			return err
		},
		func() error {
			_, err := ReleaseAgentEmailClaim(ctx, srv.URL, "token", "emsg_aaaaaaaaaaaaaaaa", AgentEmailClaimInput{ClaimID: "ecl_aaaaaaaaaaaaaaaa", Generation: 1, DeterministicFailure: true})
			return err
		},
		func() error {
			_, err := CompleteAgentEmail(ctx, srv.URL, "token", "emsg_aaaaaaaaaaaaaaaa", CompleteAgentEmailInput{ClaimID: "ecl_aaaaaaaaaaaaaaaa", Generation: 1, IdempotencyKey: "complete-key"})
			return err
		},
	} {
		if err := action(); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 10 {
		t.Fatalf("requests = %d, want 10", len(seen))
	}
	if seen[1].query != "cursor=cursor&limit=7&unacked=true&unread=true" {
		t.Fatalf("list query = %q", seen[1].query)
	}
	if seen[3].path != "/v1/email/emsg_aaaaaaaaaaaaaaaa:read" || seen[6].key != "claim-key" || seen[9].key != "complete-key" {
		t.Fatalf("action routes/keys = %#v", seen)
	}
	if seen[7].body["lease_seconds"] != float64(90) || seen[8].body["deterministic_failure"] != true {
		t.Fatalf("lifecycle bodies = %#v / %#v", seen[7].body, seen[8].body)
	}
}
