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
