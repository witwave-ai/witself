package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/url"

	"github.com/witwave-ai/witself/internal/agentemailoutbound"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/store"
)

// The proof invokes every existing production registration. Ordinary local job
// eligibility and hosted enable switches are untouched; only this explicit
// registerJobs argument selects the complete set and its write modes.
func captureProofJobs(jobs jobConfig) jobConfig {
	jobs.avatarEnabled = true
	jobs.messageRateBucketCleanupEnabled = true
	jobs.agentEmailRateBucketCleanupEnabled = true
	jobs.transcriptStorageReconcileEnabled = true
	jobs.agentEmailOutboundEnabled = true
	jobs.retentionEnabled = true
	jobs.messageRetentionEnabled = true
	jobs.agentEmailRetentionEnabled = true
	jobs.accountPurgeEnabled = true
	jobs.supportTicketAgeOutEnabled = true
	jobs.retention.Mode = store.TranscriptRetentionModeEnforce
	jobs.messageRetention.Mode = store.MessageRetentionModeEnforce
	jobs.agentEmailRetention.Mode = store.AgentEmailRetentionModeEnforce
	jobs.accountPurge.Mode = store.AccountPurgeModeEnforce
	return jobs
}

func captureProofDispatchClient(endpoint string) (agentemailoutbound.Client, error) {
	if !runtimeconfig.CaptureProofLoopbackEndpoint(endpoint) {
		return agentemailoutbound.Client{}, errors.New("capture_proof_stub_invalid")
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return agentemailoutbound.Client{}, errors.New("capture_proof_stub_unavailable")
	}
	target, _ := url.Parse(endpoint)
	signedEndpoint := *target
	signedEndpoint.Scheme = "https"
	return agentemailoutbound.Client{
		Endpoint: signedEndpoint.String(), Audience: "witself-capture-proof", KeyID: "capture-proof", PrivateKey: key,
		HTTPClient: &http.Client{Transport: &captureProofTransport{target: *target, transport: &http.Transport{Proxy: nil}}},
	}, nil
}

// The unchanged signed dispatch client requires HTTPS. This private profile
// transport maps exactly one signed origin to the driver's plain HTTP loopback
// stub. No DNS, proxy or redirect can send a request to an external provider.
type captureProofTransport struct {
	target    url.URL
	transport *http.Transport
}

func (t *captureProofTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != t.target.Host || request.URL.Path != t.target.Path ||
		request.URL.User != nil || request.URL.RawPath != "" || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Fragment != "" {
		return nil, errors.New("capture_proof_stub_invalid")
	}
	forwarded := request.Clone(request.Context())
	forwarded.URL = &url.URL{Scheme: "http", Host: t.target.Host, Path: t.target.Path}
	response, err := t.transport.RoundTrip(forwarded)
	if err != nil {
		return nil, errors.New("capture_proof_stub_unavailable")
	}
	return response, nil
}
