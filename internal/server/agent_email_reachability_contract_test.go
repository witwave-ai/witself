package server

import (
	"bytes"
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
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
)

type reachabilityContract struct {
	Schema           string                     `json:"schema"`
	Audience         string                     `json:"audience"`
	ControlAudience  string                     `json:"control_audience"`
	ExampleRecipient string                     `json:"example_recipient"`
	Cases            []reachabilityContractCase `json:"cases"`
}

type reachabilityContractCase struct {
	Name         string  `json:"name"`
	Method       string  `json:"method"`
	Path         string  `json:"path"`
	Route        string  `json:"route"`
	Relay        string  `json:"relay"`
	Status       int     `json:"status"`
	Verdict      *string `json:"verdict"`
	Code         *string `json:"code"`
	CacheControl *string `json:"cache_control"`
}

func TestAgentEmailCellReachabilityContract(t *testing.T) {
	data, err := os.ReadFile("../../infra/cloudflare/agent-email/test/cell-reachability-contract.json")
	if err != nil {
		t.Fatal("reachability contract")
	}
	var contract reachabilityContract
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil {
		t.Fatal("reachability contract")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF ||
		contract.Schema != "witself.agent-email.cell-reachability-contract.v1" || len(contract.Cases) != 6 {
		t.Fatal("reachability contract")
	}
	seen := make(map[string]bool)
	for _, test := range contract.Cases {
		if test.Name == "" || seen[test.Name] {
			t.Fatal("reachability contract")
		}
		seen[test.Name] = true
	}
	if !validAgentEmailAudience(contract.Audience) || !validAgentEmailAudience(contract.ControlAudience) ||
		contract.Audience == contract.ControlAudience {
		t.Fatal("reachability contract audiences")
	}
	parts, err := agentemail.ParseRecipient(contract.ExampleRecipient, "")
	if err != nil || parts.AgentSegment != "reachability-probe" || parts.Domain != "witmail.net" ||
		!agentemail.IsCanonicalRealmLabel(parts.RealmLabel) {
		t.Fatal("reachability contract recipient")
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("reachability contract signing key")
	}
	now := time.Date(2026, time.September, 29, 18, 0, 0, 0, time.UTC)
	receive := AgentEmailReceiveConfig{
		Enabled:           true,
		Mode:              AgentEmailReceiveModeProduction,
		Domain:            "witmail.net",
		Audience:          contract.Audience,
		AccountIDs:        map[string]bool{"acc_aaaaaaaaaaaaaaaa": true},
		RelayPublicKeys:   map[string]ed25519.PublicKey{"relay-2026-08": publicKey},
		RelayReplayWindow: 5 * time.Minute,
		Now:               func() time.Time { return now },
	}
	if err := ValidateAgentEmailReceiveConfig(receive); err != nil {
		t.Fatal("reachability contract receive configuration")
	}
	var ingestCalls, providerEventCalls int
	var gotAudience, gotRecipient string
	ingest := func(_ context.Context, metadata agentemail.RelayMetadata, _ []byte) error {
		ingestCalls++
		gotAudience = metadata.Audience
		gotRecipient = metadata.EnvelopeRecipient
		return ErrAgentEmailUnknownRecipient
	}
	providerEvent := func(context.Context, AgentEmailOutboundProviderEvent) error {
		providerEventCalls++
		return nil
	}
	on := apiMux(Config{
		AgentEmailReceive:                    receive,
		IngestAgentEmailPilot:                ingest,
		AgentEmailProviderEventToken:         strings.Repeat("p", 40),
		ApplyAgentEmailOutboundProviderEvent: providerEvent,
	})
	off := apiMux(Config{
		AgentEmailReceive:                    AgentEmailReceiveConfig{Enabled: false},
		AgentEmailPilot:                      AgentEmailPilotConfig{Enabled: false},
		IngestAgentEmailPilot:                ingest,
		AgentEmailProviderEventToken:         "",
		ApplyAgentEmailOutboundProviderEvent: providerEvent,
	})

	for _, test := range contract.Cases {
		t.Run(test.Name, func(t *testing.T) {
			var handler http.Handler
			switch test.Route {
			case "on":
				handler = on
			case "off":
				handler = off
			default:
				t.Fatal(test.Name)
			}
			var request *http.Request
			switch test.Relay {
			case "none":
				body, contentType := "", "message/rfc822"
				switch test.Path {
				case "/v1/internal/agent-email:ingest":
				case "/v1/internal/agent-email-send:provider-event":
					body, contentType = "{}", "application/json"
				default:
					t.Fatal(test.Name)
				}
				request = httptest.NewRequest(http.MethodPost, test.Path, strings.NewReader(body))
				request.Header.Set("Content-Type", contentType)
			case "control_audience", "audience":
				audience := contract.Audience
				if test.Relay == "control_audience" {
					audience = contract.ControlAudience
				}
				raw := []byte("From: witself-reachability-probe@probe.invalid\r\nTo: " +
					contract.ExampleRecipient + "\r\nSubject: Witself cell reachability probe\r\n\r\nSynthetic probe.\r\n")
				digest := sha256.Sum256(raw)
				metadata := agentemail.RelayMetadata{
					Version:           agentemail.RelaySignatureVersionV2,
					Timestamp:         now.Unix(),
					KeyID:             "relay-2026-08",
					Audience:          audience,
					EnvelopeSender:    "witself-reachability-probe@probe.invalid",
					EnvelopeRecipient: contract.ExampleRecipient,
					RawSize:           int64(len(raw)),
					RawSHA256:         hex.EncodeToString(digest[:]),
					SPFResult:         "unknown",
					DKIMResult:        "unknown",
					DMARCResult:       "unknown",
				}
				request = reachabilityRelayRequest(t, test.Name, raw, metadata, privateKey)
			default:
				t.Fatal(test.Name)
			}
			if request.Method != test.Method || request.URL.Path != test.Path {
				t.Fatal(test.Name)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.Status ||
				(test.CacheControl != nil && response.Header().Get("Cache-Control") != *test.CacheControl) {
				t.Fatal(test.Name)
			}
			// Check value-free predicates before calling the shared response helpers,
			// whose diagnostic messages otherwise include complete response bodies.
			if test.Verdict != nil {
				var body map[string]any
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body) != 1 ||
					body["verdict"] != *test.Verdict || response.Header().Get("Content-Type") != "application/json" {
					t.Fatal(test.Name)
				}
				assertAgentEmailVerdict(t, response, test.Status, *test.Verdict)
			}
			if test.Code != nil {
				var body map[string]any
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body) != 4 ||
					body["schema_version"] != "witself.v0" || body["code"] != *test.Code || body["retryable"] != false {
					t.Fatal(test.Name)
				}
				message, ok := body["error"].(string)
				if !ok || strings.TrimSpace(message) == "" || len(message) > 256 ||
					response.Header().Get("Cache-Control") != "private, no-store" {
					t.Fatal(test.Name)
				}
				assertAgentEmailOutboundCodedError(t, response, test.Status, *test.Code, false)
			}
		})
	}
	if ingestCalls != 1 || gotAudience != contract.Audience || gotRecipient != contract.ExampleRecipient {
		t.Fatal("ingest_unknown_recipient")
	}
	if providerEventCalls != 0 {
		t.Fatal("provider_event_unauthenticated_on")
	}
}

func reachabilityRelayRequest(t *testing.T, name string, body []byte, metadata agentemail.RelayMetadata, privateKey ed25519.PrivateKey) *http.Request {
	t.Helper()
	canonical, err := agentemail.CanonicalSignatureInput(metadata)
	if err != nil {
		t.Fatal(name)
	}
	signature := ed25519.Sign(privateKey, canonical)
	request := httptest.NewRequest(http.MethodPost, "/v1/internal/agent-email:ingest", bytes.NewReader(body))
	request.Header.Set("Content-Type", "message/rfc822")
	request.Header.Set(AgentEmailRelayHeaderVersion, metadata.Version)
	request.Header.Set(AgentEmailRelayHeaderTimestamp, strconv.FormatInt(metadata.Timestamp, 10))
	request.Header.Set(AgentEmailRelayHeaderKeyID, metadata.KeyID)
	request.Header.Set(AgentEmailRelayHeaderAudience, metadata.Audience)
	request.Header.Set(AgentEmailRelayHeaderEnvelopeFrom, base64.RawURLEncoding.EncodeToString([]byte(metadata.EnvelopeSender)))
	request.Header.Set(AgentEmailRelayHeaderEnvelopeTo, base64.RawURLEncoding.EncodeToString([]byte(metadata.EnvelopeRecipient)))
	request.Header.Set(AgentEmailRelayHeaderRawSize, strconv.FormatInt(metadata.RawSize, 10))
	request.Header.Set(AgentEmailRelayHeaderRawSHA256, "sha256:"+metadata.RawSHA256)
	request.Header.Set(AgentEmailRelayHeaderSignature, base64.StdEncoding.EncodeToString(signature))
	request.Header.Set(AgentEmailRelayHeaderSPFResult, metadata.SPFResult)
	request.Header.Set(AgentEmailRelayHeaderDKIMResult, metadata.DKIMResult)
	request.Header.Set(AgentEmailRelayHeaderDMARCResult, metadata.DMARCResult)
	return request
}
