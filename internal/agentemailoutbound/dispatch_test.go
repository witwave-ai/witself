package agentemailoutbound

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func validDispatch() Dispatch {
	return Dispatch{
		SchemaVersion: DispatchSchemaVersion,
		SendID:        "esnd_aaaaaaaaaaaaaaaa", AccountID: "acc_aaaaaaaaaaaaaaaa",
		RealmID: "realm_aaaaaaaaaaaaaaaa", AgentID: "agent_aaaaaaaaaaaaaaaa",
		From:    "scott.aaaaaaaaaaaaaaaa@send.witmail.net",
		ReplyTo: "scott.aaaaaaaaaaaaaaaa@witmail.net",
		To:      "person@example.com", Subject: "Hello", Text: "Plain text only.",
	}
}

func TestDispatchValidate(t *testing.T) {
	if err := validDispatch().Validate(); err != nil {
		t.Fatalf("valid dispatch: %v", err)
	}
	tests := []func(*Dispatch){
		func(d *Dispatch) { d.From = "other.aaaaaaaaaaaaaaaa@witmail.net" },
		func(d *Dispatch) { d.ReplyTo = "other.aaaaaaaaaaaaaaaa@witmail.net" },
		func(d *Dispatch) { d.To = "Person <person@example.com>" },
		func(d *Dispatch) { d.Subject = "bad\nheader" },
		func(d *Dispatch) { d.Text = "" },
		func(d *Dispatch) { d.InReplyTo = "not-a-message-id" },
	}
	for i, mutate := range tests {
		d := validDispatch()
		mutate(&d)
		if err := d.Validate(); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestClientSignsCompleteBodyAndParsesReceipt(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 14, 12, 0, 0, 123, time.UTC)
	var serverURL string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Error(readErr)
		}
		digestBytes := sha256Bytes(body)
		digest := hex.EncodeToString(digestBytes)
		if got := r.Header.Get(HeaderDigest); got != digest {
			t.Errorf("digest = %q, want %q", got, digest)
		}
		input, inputErr := SignatureInput(
			r.Header.Get(HeaderVersion), r.Header.Get(HeaderTimestamp),
			r.Header.Get(HeaderKeyID), r.Header.Get(HeaderAudience), digest,
		)
		if inputErr != nil {
			t.Error(inputErr)
		}
		signature, decodeErr := base64.StdEncoding.DecodeString(r.Header.Get(HeaderSignature))
		if decodeErr != nil || !ed25519.Verify(publicKey, input, signature) {
			t.Error("signature did not verify")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{
			SchemaVersion: ResponseSchemaVersion, SendID: validDispatch().SendID,
			State: StateQueued, Provider: "cloudflare_email_sending",
			ProviderMessageID: "provider-message-1",
		})
	}))
	defer srv.Close()
	serverURL = srv.URL + "/v1/dispatch"
	client := Client{
		Endpoint: serverURL, Audience: "witself-agent-email-send",
		KeyID: "cell-2026-08", PrivateKey: privateKey,
		HTTPClient: srv.Client(), Now: func() time.Time { return now },
	}
	// TLS test URLs are HTTPS and exercise the same validation.
	got, err := client.Send(context.Background(), validDispatch())
	if err != nil || got.State != StateQueued || got.ProviderMessageID != "provider-message-1" {
		t.Fatalf("Send = %#v, %v", got, err)
	}
	_ = serverURL
}

func TestClientReceiptReplayUsesExactPathAudienceAndClosedProof(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wantDispatchBody, err := validDispatch().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != ReceiptReplayPath ||
			r.Header.Get(HeaderAudience) != ReceiptReplayAudience {
			t.Errorf("request = %s %s audience=%q", r.Method, r.URL.Path, r.Header.Get(HeaderAudience))
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Error(readErr)
		}
		if string(body) != string(wantDispatchBody) {
			t.Fatalf("replay body changed immutable dispatch")
		}
		digestBytes := sha256.Sum256(body)
		digest := hex.EncodeToString(digestBytes[:])
		input, inputErr := SignatureInput(
			r.Header.Get(HeaderVersion), r.Header.Get(HeaderTimestamp),
			r.Header.Get(HeaderKeyID), r.Header.Get(HeaderAudience), digest,
		)
		if inputErr != nil {
			t.Error(inputErr)
		}
		signature, decodeErr := base64.StdEncoding.DecodeString(r.Header.Get(HeaderSignature))
		if decodeErr != nil || !ed25519.Verify(publicKey, input, signature) {
			t.Error("receipt replay signature did not verify")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"accepted","digest_matched":true,"signer_matched":true,"provider_call_started_count":1,"verified_replay_count":2,"route_pending":false}`)
	}))
	defer srv.Close()
	client := Client{
		Endpoint: srv.URL + ReceiptReplayPath, Audience: ReceiptReplayAudience,
		KeyID: "cell-2026-08", PrivateKey: privateKey, HTTPClient: srv.Client(),
	}
	proof, err := client.ReplayReceipt(context.Background(), validDispatch())
	if err != nil || proof.ProviderCallStartedCount != 1 ||
		proof.VerifiedReplayCount != 2 || proof.RoutePending {
		t.Fatalf("ReplayReceipt = %#v, %v", proof, err)
	}
}

func TestClientReceiptReplayRejectsNonExactProofs(t *testing.T) {
	_, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{
			name: "unknown field", status: 200,
			body: `{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"accepted","digest_matched":true,"signer_matched":true,"provider_call_started_count":1,"verified_replay_count":1,"route_pending":false,"provider_message_id":"forbidden"}`,
		},
		{
			name: "missing route pending", status: 200,
			body: `{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"accepted","digest_matched":true,"signer_matched":true,"provider_call_started_count":1,"verified_replay_count":1}`,
		},
		{
			name: "null route pending", status: 200,
			body: `{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"accepted","digest_matched":true,"signer_matched":true,"provider_call_started_count":1,"verified_replay_count":1,"route_pending":null}`,
		},
		{
			name: "duplicate field", status: 200,
			body: `{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"accepted","digest_matched":true,"digest_matched":true,"signer_matched":true,"provider_call_started_count":1,"verified_replay_count":1,"route_pending":false}`,
		},
		{
			name: "provider called twice", status: 200,
			body: `{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"accepted","digest_matched":true,"signer_matched":true,"provider_call_started_count":2,"verified_replay_count":1,"route_pending":false}`,
		},
		{
			name: "replay count out of bounds", status: 200,
			body: `{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"accepted","digest_matched":true,"signer_matched":true,"provider_call_started_count":1,"verified_replay_count":1000001,"route_pending":false}`,
		},
		{
			name: "digest mismatch", status: 200,
			body: `{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"accepted","digest_matched":false,"signer_matched":true,"provider_call_started_count":1,"verified_replay_count":1,"route_pending":false}`,
		},
		{
			name: "signer mismatch", status: 200,
			body: `{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"accepted","digest_matched":true,"signer_matched":false,"provider_call_started_count":1,"verified_replay_count":1,"route_pending":false}`,
		},
		{
			name: "unresolved state", status: 200,
			body: `{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"provider_started","digest_matched":true,"signer_matched":true,"provider_call_started_count":1,"verified_replay_count":1,"route_pending":false}`,
		},
		{
			name: "bounded missing receipt", status: 404,
			body: `{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"missing","error_code":"receipt_missing"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer srv.Close()
			client := Client{
				Endpoint: srv.URL + ReceiptReplayPath, Audience: ReceiptReplayAudience,
				KeyID: "cell-2026-08", PrivateKey: privateKey, HTTPClient: srv.Client(),
			}
			if _, err := client.ReplayReceipt(context.Background(), validDispatch()); err == nil {
				t.Fatal("non-exact receipt proof accepted")
			}
		})
	}
}

func TestClientReceiptReplayRequiresExactEndpointAndAudience(t *testing.T) {
	_, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	for name, mutate := range map[string]func(*Client){
		"normal path": func(client *Client) { client.Endpoint = "https://send.example/v1/dispatch" },
		"suffix":      func(client *Client) { client.Endpoint += "/" },
		"query":       func(client *Client) { client.Endpoint += "?mode=proof" },
		"normal audience": func(client *Client) {
			client.Audience = "witself-agent-email-send"
		},
	} {
		t.Run(name, func(t *testing.T) {
			client := Client{
				Endpoint: "https://send.example" + ReceiptReplayPath,
				Audience: ReceiptReplayAudience, KeyID: "cell-key", PrivateKey: privateKey,
			}
			mutate(&client)
			if err := client.ValidateReceiptReplay(); err == nil {
				t.Fatal("non-exact replay client accepted")
			}
		})
	}
}

func TestClientReceiptReplayRejectsRedirect(t *testing.T) {
	_, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	var forwarded atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		forwarded.Add(1)
	}))
	defer target.Close()
	adapter := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+ReceiptReplayPath, http.StatusTemporaryRedirect)
	}))
	defer adapter.Close()
	client := Client{
		Endpoint: adapter.URL + ReceiptReplayPath, Audience: ReceiptReplayAudience,
		KeyID: "cell-2026-08", PrivateKey: privateKey, HTTPClient: adapter.Client(),
	}
	if _, err := client.ReplayReceipt(context.Background(), validDispatch()); err == nil {
		t.Fatal("redirect accepted")
	}
	if forwarded.Load() != 0 {
		t.Fatalf("redirect target received %d signed dispatches", forwarded.Load())
	}
}

func TestClientTreatsTransportAndMalformedResponsesAsAmbiguous(t *testing.T) {
	_, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	client := Client{
		Endpoint: "https://127.0.0.1:1/v1/dispatch", Audience: "audience",
		KeyID: "cell-key", PrivateKey: privateKey,
		HTTPClient: &http.Client{Timeout: 50 * time.Millisecond},
	}
	got, err := client.Send(context.Background(), validDispatch())
	if err == nil || got.State != StateAmbiguous {
		t.Fatalf("transport result = %#v, %v", got, err)
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"accepted"}`))
	}))
	defer srv.Close()
	client.Endpoint, client.HTTPClient = srv.URL+"/v1/dispatch", srv.Client()
	got, err = client.Send(context.Background(), validDispatch())
	if err == nil || got.State != StateAmbiguous {
		t.Fatalf("malformed result = %#v, %v", got, err)
	}
}

func TestClientRejectsRedirectWithoutForwardingDispatch(t *testing.T) {
	_, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	var forwarded atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		forwarded.Add(1)
	}))
	defer target.Close()
	adapter := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer adapter.Close()

	client := Client{
		Endpoint: adapter.URL + "/v1/dispatch", Audience: "audience",
		KeyID: "cell-key", PrivateKey: privateKey, HTTPClient: adapter.Client(),
	}
	got, err := client.Send(context.Background(), validDispatch())
	if err == nil || got.State != StateAmbiguous {
		t.Fatalf("redirect result = %#v, %v", got, err)
	}
	if forwarded.Load() != 0 {
		t.Fatalf("redirect target received %d signed dispatches", forwarded.Load())
	}
}

func TestClientRequiresExactDispatchEndpoint(t *testing.T) {
	_, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	for _, endpoint := range []string{
		"http://send.example/v1/dispatch",
		"https://send.example/dispatch",
		"https://send.example/v1/dispatch/",
		"https://send.example/v1/dispatch?target=other",
		"https://user@send.example/v1/dispatch",
		"https://send.example/%76%31/dispatch",
	} {
		client := Client{
			Endpoint: endpoint, Audience: "audience", KeyID: "cell-key",
			PrivateKey: privateKey,
		}
		if err := client.Validate(); err == nil {
			t.Errorf("endpoint %q accepted", endpoint)
		}
	}
}

func TestParsePrivateKey(t *testing.T) {
	_, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	encoded := base64.StdEncoding.EncodeToString(privateKey)
	got, err := ParsePrivateKey(encoded)
	if err != nil || !got.Equal(privateKey) {
		t.Fatalf("ParsePrivateKey = %v, %v", got, err)
	}
	if _, err := ParsePrivateKey("bad"); err == nil {
		t.Fatal("bad private key accepted")
	}
}

func sha256Bytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}
