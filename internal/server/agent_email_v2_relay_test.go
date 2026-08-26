package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/witwave-ai/witself/internal/agentemail"
)

func testAgentEmailV2IngestRequest(t *testing.T, body []byte, metadata agentemail.RelayMetadata, privateKey ed25519.PrivateKey) *http.Request {
	t.Helper()
	canonical, err := agentemail.CanonicalSignatureInput(metadata)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, canonical)
	request := httptest.NewRequest(http.MethodPost, "/v1/internal/agent-email:ingest", bytes.NewReader(body))
	request.Header.Set("Content-Type", "message/rfc822")
	request.Header.Set(AgentEmailRelayHeaderVersion, agentemail.RelaySignatureVersionV2)
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

func testV2RelayMetadata(t *testing.T, raw []byte, pilot AgentEmailPilotConfig) agentemail.RelayMetadata {
	t.Helper()
	metadata := testAgentEmailRelayMetadata(raw, pilot, "pilot-key")
	metadata.Version = agentemail.RelaySignatureVersionV2
	metadata.SPFResult, metadata.DKIMResult, metadata.DMARCResult = "pass", "none", "fail"
	return metadata
}

func TestAgentEmailIngestAcceptsV2RelayVerdicts(t *testing.T) {
	pilot, privateKey := testAgentEmailPilotConfig(t)
	raw := []byte("Subject: v2 verdicts\r\n\r\nbody")
	metadata := testV2RelayMetadata(t, raw, pilot)
	var got agentemail.RelayMetadata
	handler := agentEmailIngestHandler(pilot, func(_ context.Context, m agentemail.RelayMetadata, _ []byte) error {
		got = m
		return nil
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, testAgentEmailV2IngestRequest(t, raw, metadata, privateKey))
	assertAgentEmailVerdict(t, response, http.StatusOK, "accepted")
	if got.Version != agentemail.RelaySignatureVersionV2 ||
		got.SPFResult != "pass" || got.DKIMResult != "none" || got.DMARCResult != "fail" {
		t.Fatalf("ingest metadata = %#v, want v2 verdicts", got)
	}
}

func TestAgentEmailIngestRejectsV2MissingVerdictHeader(t *testing.T) {
	pilot, privateKey := testAgentEmailPilotConfig(t)
	raw := []byte("Subject: v2 missing verdict\r\n\r\nbody")
	metadata := testV2RelayMetadata(t, raw, pilot)
	request := testAgentEmailV2IngestRequest(t, raw, metadata, privateKey)
	request.Header.Del(AgentEmailRelayHeaderDMARCResult)
	handler := agentEmailIngestHandler(pilot, func(context.Context, agentemail.RelayMetadata, []byte) error {
		t.Fatal("ingest reached without a complete verdict set")
		return nil
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertAgentEmailVerdict(t, response, http.StatusUnauthorized, "invalid_relay")
}

func TestAgentEmailIngestRejectsNonCanonicalV2Verdict(t *testing.T) {
	pilot, privateKey := testAgentEmailPilotConfig(t)
	raw := []byte("Subject: v2 non-canonical\r\n\r\nbody")
	metadata := testV2RelayMetadata(t, raw, pilot)
	request := testAgentEmailV2IngestRequest(t, raw, metadata, privateKey)
	request.Header.Set(AgentEmailRelayHeaderSPFResult, "PASS")
	handler := agentEmailIngestHandler(pilot, func(context.Context, agentemail.RelayMetadata, []byte) error {
		t.Fatal("ingest reached with a non-canonical verdict")
		return nil
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertAgentEmailVerdict(t, response, http.StatusUnauthorized, "invalid_relay")
}

func TestAgentEmailIngestIgnoresStrayVerdictHeadersOnV1(t *testing.T) {
	pilot, privateKey := testAgentEmailPilotConfig(t)
	raw := []byte("Subject: v1 stray headers\r\n\r\nbody")
	metadata := testAgentEmailRelayMetadata(raw, pilot, "pilot-key")
	request := testAgentEmailIngestRequest(t, raw, metadata, privateKey)
	// Unsigned stray verdict headers on a v1 envelope are never read.
	request.Header.Set(AgentEmailRelayHeaderSPFResult, "pass")
	request.Header.Set(AgentEmailRelayHeaderDMARCResult, "pass")
	var got agentemail.RelayMetadata
	handler := agentEmailIngestHandler(pilot, func(_ context.Context, m agentemail.RelayMetadata, _ []byte) error {
		got = m
		return nil
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertAgentEmailVerdict(t, response, http.StatusOK, "accepted")
	if got.SPFResult != "" || got.DKIMResult != "" || got.DMARCResult != "" || got.Version != agentemail.RelaySignatureVersion {
		t.Fatalf("v1 ingest metadata = %#v, want empty verdicts", got)
	}
}
