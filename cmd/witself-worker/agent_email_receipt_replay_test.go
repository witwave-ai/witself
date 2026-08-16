package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemailoutbound"
	"github.com/witwave-ai/witself/internal/store"
)

func TestRunAgentEmailReceiptReplayEmitsOnlyClosedProof(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	acceptedAt := time.Date(2026, 8, 15, 16, 17, 18, 123000000, time.UTC)
	source := validAgentEmailOutboundClaim()
	source.Message.ID = "esnd_aaaaaaaaaaaaaaaa"
	source.Message.AccountID = "acc_aaaaaaaaaaaaaaaa"
	source.Message.RealmID = "realm_aaaaaaaaaaaaaaaa"
	source.Message.OwnerAgentID = "agent_aaaaaaaaaaaaaaaa"
	source.Message.ToAddress = "recipient-leak-marker@example.com"
	source.Message.Subject = "subject-leak-marker"
	source.Text = "body-leak-marker"
	source.Message.ProviderMessageID = "provider-message-leak-marker"
	source.Message.AcceptedAt = &acceptedAt
	source.Message.AttemptCount = 1
	st := &fakeAgentEmailReceiptReplayStore{source: source}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != agentemailoutbound.ReceiptReplayPath ||
			r.Header.Get(agentemailoutbound.HeaderAudience) != agentemailoutbound.ReceiptReplayAudience {
			t.Errorf("request path/audience = %q/%q", r.URL.Path, r.Header.Get(agentemailoutbound.HeaderAudience))
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Error(readErr)
		}
		if !bytes.Contains(body, []byte("body-leak-marker")) {
			t.Error("edge did not receive the immutable production dispatch")
		}
		_, _ = io.WriteString(w, `{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"accepted","digest_matched":true,"signer_matched":true,"provider_call_started_count":1,"verified_replay_count":1,"route_pending":false}`)
	}))
	defer srv.Close()
	env := map[string]string{
		"WITSELF_DATABASE_URL":                  "postgres://worker-only.example/witself",
		agentEmailOutboundDispatchEndpointEnv:   srv.URL + agentemailoutbound.DispatchPath,
		agentEmailOutboundDispatchAudienceEnv:   "wrong-normal-audience-must-be-ignored",
		agentEmailOutboundDispatchKeyIDEnv:      "cell-2026-08",
		agentEmailOutboundDispatchPrivateKeyEnv: base64.StdEncoding.EncodeToString(privateKey),
	}
	var stdout, stderr bytes.Buffer
	code := runAgentEmailReceiptReplayWith(
		context.Background(),
		[]string{
			"--account-id", source.Message.AccountID,
			"--send-id", source.Message.ID,
			"--expected-accepted-at", acceptedAt.Format(time.RFC3339Nano),
			"--expected-attempt-count", "1",
			"--json",
		},
		mapLookup(env),
		&stdout,
		&stderr,
		func(_ context.Context, dsn string) (agentEmailReceiptReplayStore, error) {
			if dsn != env["WITSELF_DATABASE_URL"] {
				t.Fatalf("dsn changed")
			}
			return st, nil
		},
		srv.Client(),
	)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if st.input.AccountID != source.Message.AccountID ||
		st.input.SendID != source.Message.ID ||
		!st.input.ExpectedAcceptedAt.Equal(acceptedAt) ||
		st.input.ExpectedAttemptCount != 1 || !st.pinged || !st.closed {
		t.Fatalf("store request/lifecycle = %#v pinged=%t closed=%t", st.input, st.pinged, st.closed)
	}
	result := stdout.String()
	for _, forbidden := range []string{
		"recipient-leak-marker", "subject-leak-marker", "body-leak-marker",
		"provider-message-leak-marker", "sha256", "cell-2026-08", "wrong-normal-audience",
		base64.StdEncoding.EncodeToString(privateKey),
	} {
		if strings.Contains(result, forbidden) {
			t.Fatalf("value-free output leaked %q: %s", forbidden, result)
		}
	}
	for _, required := range []string{
		`"schema_version":"witself.agent-email-dispatch-receipt-proof.v1"`,
		`"send_id":"esnd_aaaaaaaaaaaaaaaa"`,
		`"provider_call_started_count":1`,
		`"verified_replay_count":1`,
	} {
		if !strings.Contains(result, required) {
			t.Fatalf("proof missing %q: %s", required, result)
		}
	}
}

func TestRunAgentEmailReceiptReplayRequiresStrictAssertionsBeforeOpeningStore(t *testing.T) {
	for name, args := range map[string][]string{
		"attempt count": {
			"--account-id", "acc_aaaaaaaaaaaaaaaa", "--send-id", "esnd_aaaaaaaaaaaaaaaa",
			"--expected-accepted-at", "2026-08-15T00:00:00Z",
			"--expected-attempt-count", "2", "--json",
		},
		"json": {
			"--account-id", "acc_aaaaaaaaaaaaaaaa", "--send-id", "esnd_aaaaaaaaaaaaaaaa",
			"--expected-accepted-at", "2026-08-15T00:00:00Z",
			"--expected-attempt-count", "1",
		},
		"accepted time": {
			"--account-id", "acc_aaaaaaaaaaaaaaaa", "--send-id", "esnd_aaaaaaaaaaaaaaaa",
			"--expected-accepted-at", "yesterday",
			"--expected-attempt-count", "1", "--json",
		},
		"whitespace account": {
			"--account-id", " acc_aaaaaaaaaaaaaaaa", "--send-id", "esnd_aaaaaaaaaaaaaaaa",
			"--expected-accepted-at", "2026-08-15T00:00:00Z",
			"--expected-attempt-count", "1", "--json",
		},
		"malformed count stays private": {
			"--account-id", "acc_aaaaaaaaaaaaaaaa", "--send-id", "esnd_aaaaaaaaaaaaaaaa",
			"--expected-accepted-at", "2026-08-15T00:00:00Z",
			"--expected-attempt-count", "private-paste-marker", "--json",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			opened := false
			code := runAgentEmailReceiptReplayWith(
				context.Background(), args, mapLookup(nil), &stdout, &stderr,
				func(context.Context, string) (agentEmailReceiptReplayStore, error) {
					opened = true
					return nil, nil
				}, nil,
			)
			if code != 2 || opened || stdout.Len() != 0 ||
				!strings.Contains(stderr.String(), "invalid agent-email receipt-replay command") {
				t.Fatalf("code=%d opened=%t stdout=%q stderr=%q", code, opened, stdout.String(), stderr.String())
			}
			if strings.Contains(stderr.String(), "private-paste-marker") {
				t.Fatalf("malformed flag value leaked: %q", stderr.String())
			}
		})
	}
}

func TestRunAgentEmailReceiptReplayRejectsPendingProviderRoute(t *testing.T) {
	_, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	acceptedAt := time.Date(2026, 8, 15, 16, 17, 18, 0, time.UTC)
	source := validAgentEmailOutboundClaim()
	source.Message.ID = "esnd_aaaaaaaaaaaaaaaa"
	source.Message.AccountID = "acc_aaaaaaaaaaaaaaaa"
	source.Message.RealmID = "realm_aaaaaaaaaaaaaaaa"
	source.Message.OwnerAgentID = "agent_aaaaaaaaaaaaaaaa"
	st := &fakeAgentEmailReceiptReplayStore{source: source}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"accepted","digest_matched":true,"signer_matched":true,"provider_call_started_count":1,"verified_replay_count":1,"route_pending":true}`)
	}))
	defer srv.Close()
	env := map[string]string{
		"WITSELF_DATABASE_URL":                  "postgres://worker-only.example/witself",
		agentEmailOutboundDispatchEndpointEnv:   srv.URL + agentemailoutbound.DispatchPath,
		agentEmailOutboundDispatchKeyIDEnv:      "cell-2026-08",
		agentEmailOutboundDispatchPrivateKeyEnv: base64.StdEncoding.EncodeToString(privateKey),
	}
	var stdout, stderr bytes.Buffer
	code := runAgentEmailReceiptReplayWith(
		context.Background(),
		[]string{
			"--account-id", source.Message.AccountID,
			"--send-id", source.Message.ID,
			"--expected-accepted-at", acceptedAt.Format(time.RFC3339Nano),
			"--expected-attempt-count", "1", "--json",
		},
		mapLookup(env), &stdout, &stderr,
		func(context.Context, string) (agentEmailReceiptReplayStore, error) {
			return st, nil
		},
		srv.Client(),
	)
	if code != 1 || stdout.Len() != 0 ||
		!strings.Contains(stderr.String(), "edge receipt route is not settled") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

type fakeAgentEmailReceiptReplayStore struct {
	source store.AgentEmailOutboundDispatch
	input  store.AgentEmailOutboundReceiptReplayInput
	pinged bool
	closed bool
	err    error
}

func (s *fakeAgentEmailReceiptReplayStore) Ping(context.Context) error {
	s.pinged = true
	return nil
}

func (s *fakeAgentEmailReceiptReplayStore) LoadAgentEmailOutboundReceiptReplay(
	_ context.Context,
	in store.AgentEmailOutboundReceiptReplayInput,
) (store.AgentEmailOutboundDispatch, error) {
	s.input = in
	return s.source, s.err
}

func (s *fakeAgentEmailReceiptReplayStore) Close() { s.closed = true }
