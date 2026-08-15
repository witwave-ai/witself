// Package agentemailoutbound defines the provider-neutral, signed boundary
// between a cell worker and the managed outbound-email adapter. It contains no
// provider credential and performs no mailbox or plan lookup; those remain at
// the cell before a dispatch is signed.
package agentemailoutbound

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DispatchSchemaVersion identifies the immutable cell-to-adapter request.
	DispatchSchemaVersion = "witself.agent-email-dispatch.v1"
	// ResponseSchemaVersion identifies the adapter's closed response envelope.
	ResponseSchemaVersion = "witself.agent-email-dispatch-response.v1"
	// ReceiptReplayProofSchemaVersion identifies the edge's value-free proof.
	ReceiptReplayProofSchemaVersion = "witself.agent-email-dispatch-receipt-proof.v1"
	// ReceiptReplayAudience is reserved for operator receipt verification.
	ReceiptReplayAudience = "witself-agent-email-send-receipt-replay"
	// DispatchPath is the only provider-call path.
	DispatchPath = "/v1/dispatch"
	// ReceiptReplayPath is the read-only durable-receipt proof path.
	ReceiptReplayPath = "/v1/dispatch:receipt-replay"

	// HeaderVersion carries DispatchSchemaVersion on a signed request.
	HeaderVersion = "X-Witself-Email-Dispatch-Version"
	// HeaderTimestamp carries the signed request creation time.
	HeaderTimestamp = "X-Witself-Email-Dispatch-Timestamp"
	// HeaderKeyID identifies the trusted cell signing key.
	HeaderKeyID = "X-Witself-Email-Dispatch-Key-Id"
	// HeaderAudience binds a request to the managed adapter.
	HeaderAudience = "X-Witself-Email-Dispatch-Audience"
	// HeaderDigest carries the signed lowercase SHA-256 body digest.
	HeaderDigest = "X-Witself-Email-Dispatch-SHA256"
	// HeaderSignature carries the base64 Ed25519 signature.
	HeaderSignature = "X-Witself-Email-Dispatch-Signature"

	// StateAccepted records known provider acceptance.
	StateAccepted = "accepted"
	// StateDelivered records synchronous known delivery.
	StateDelivered = "delivered"
	// StateQueued records synchronous provider queueing.
	StateQueued = "queued"
	// StatePermanentBounce records a proven permanent recipient bounce.
	StatePermanentBounce = "permanent_bounce"
	// StateRejected records known non-delivery without retry.
	StateRejected = "rejected"
	// StateRetryable records known non-acceptance that may be retried.
	StateRetryable = "retryable"
	// StateAmbiguous records an uncertain provider boundary.
	StateAmbiguous = "ambiguous"

	maxSubjectBytes    = 4 * 1024
	maxTextBytes       = 256 * 1024
	maxReferences      = 16
	maxResponseBytes   = 64 * 1024
	defaultHTTPTimeout = 20 * time.Second
)

var (
	// ErrInvalidDispatch reports a malformed outbound dispatch envelope.
	ErrInvalidDispatch = errors.New("invalid agent-email dispatch")
	// ErrInvalidResponse reports a malformed or mismatched adapter response.
	ErrInvalidResponse = errors.New("invalid agent-email dispatch response")

	generatedIDPattern = regexp.MustCompile(`^(?:esnd|acc|realm|agent)_[A-Za-z0-9_-]{1,128}$`)
	keyIDPattern       = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	messageIDPattern   = regexp.MustCompile(`^<[^<>\r\n]{1,996}>$`)
)

// Dispatch is one immutable, single-recipient, plain-text provider request.
// From and ReplyTo are derived by the cell; clients never supply them.
type Dispatch struct {
	SchemaVersion string   `json:"schema_version"`
	SendID        string   `json:"send_id"`
	AccountID     string   `json:"account_id"`
	RealmID       string   `json:"realm_id"`
	AgentID       string   `json:"agent_id"`
	From          string   `json:"from"`
	ReplyTo       string   `json:"reply_to"`
	To            string   `json:"to"`
	Subject       string   `json:"subject"`
	Text          string   `json:"text"`
	InReplyTo     string   `json:"in_reply_to,omitempty"`
	References    []string `json:"references,omitempty"`
}

// Response is the adapter's closed, content-free result. ErrorCode is bounded
// provider-neutral vocabulary; raw provider responses never cross this seam.
type Response struct {
	SchemaVersion     string `json:"schema_version"`
	SendID            string `json:"send_id"`
	State             string `json:"state"`
	Provider          string `json:"provider"`
	ProviderMessageID string `json:"provider_message_id,omitempty"`
	ErrorCode         string `json:"error_code,omitempty"`
	RetryAfterSeconds int64  `json:"retry_after_seconds,omitempty"`
}

// ReceiptReplayProof is the closed, value-free evidence returned by the edge.
// It deliberately contains no provider identifier, dispatch digest, signer key,
// recipient, subject, or body.
type ReceiptReplayProof struct {
	SchemaVersion            string `json:"schema_version"`
	SendID                   string `json:"send_id"`
	ReceiptState             string `json:"receipt_state"`
	DigestMatched            bool   `json:"digest_matched"`
	SignerMatched            bool   `json:"signer_matched"`
	ProviderCallStartedCount int64  `json:"provider_call_started_count"`
	VerifiedReplayCount      int64  `json:"verified_replay_count"`
	RoutePending             bool   `json:"route_pending"`
}

// Validate checks the complete immutable provider envelope.
func (d Dispatch) Validate() error {
	if d.SchemaVersion != DispatchSchemaVersion ||
		!validGeneratedID(d.SendID, "esnd") ||
		!validGeneratedID(d.AccountID, "acc") ||
		!validGeneratedID(d.RealmID, "realm") ||
		!validGeneratedID(d.AgentID, "agent") {
		return ErrInvalidDispatch
	}
	from, err := canonicalMailbox(d.From)
	if err != nil || from != d.From || !strings.HasSuffix(from, "@send.witmail.net") {
		return ErrInvalidDispatch
	}
	replyTo, err := canonicalMailbox(d.ReplyTo)
	if err != nil || replyTo != d.ReplyTo || !strings.HasSuffix(replyTo, "@witmail.net") {
		return ErrInvalidDispatch
	}
	fromLocal, _, _ := strings.Cut(from, "@")
	replyLocal, _, _ := strings.Cut(replyTo, "@")
	if fromLocal != replyLocal {
		return ErrInvalidDispatch
	}
	to, err := canonicalMailbox(d.To)
	if err != nil || to != d.To {
		return ErrInvalidDispatch
	}
	if !utf8.ValidString(d.Subject) || len(d.Subject) > maxSubjectBytes ||
		strings.ContainsAny(d.Subject, "\r\n") || hasForbiddenControl(d.Subject) {
		return ErrInvalidDispatch
	}
	if !utf8.ValidString(d.Text) || len(d.Text) < 1 || len(d.Text) > maxTextBytes ||
		strings.IndexByte(d.Text, 0) >= 0 {
		return ErrInvalidDispatch
	}
	if d.InReplyTo != "" && !messageIDPattern.MatchString(d.InReplyTo) {
		return ErrInvalidDispatch
	}
	if len(d.References) > maxReferences {
		return ErrInvalidDispatch
	}
	for _, reference := range d.References {
		if !messageIDPattern.MatchString(reference) {
			return ErrInvalidDispatch
		}
	}
	return nil
}

func validGeneratedID(value, prefix string) bool {
	return generatedIDPattern.MatchString(value) && strings.HasPrefix(value, prefix+"_")
}

func canonicalMailbox(value string) (string, error) {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 320 ||
		strings.ContainsAny(value, "\r\n") {
		return "", ErrInvalidDispatch
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || parsed.Address != value {
		return "", ErrInvalidDispatch
	}
	local, domain, ok := strings.Cut(value, "@")
	if !ok || local == "" || domain == "" || domain != strings.ToLower(domain) {
		return "", ErrInvalidDispatch
	}
	return local + "@" + domain, nil
}

func hasForbiddenControl(value string) bool {
	for _, r := range value {
		if r < 0x20 && r != '\t' || r == 0x7f {
			return true
		}
	}
	return false
}

// Marshal validates and emits the deterministic JSON body covered by the
// detached signature. Struct field order is stable under encoding/json.
func (d Dispatch) Marshal() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("marshal dispatch: %w", err)
	}
	return body, nil
}

// SignatureInput is shared with the edge adapter and deliberately binds only
// closed headers plus the complete JSON digest.
func SignatureInput(version, timestamp, keyID, audience, digest string) ([]byte, error) {
	if version != DispatchSchemaVersion || !keyIDPattern.MatchString(keyID) ||
		audience == "" || audience != strings.TrimSpace(audience) ||
		strings.ContainsAny(audience, "\r\n") {
		return nil, ErrInvalidDispatch
	}
	if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
		return nil, ErrInvalidDispatch
	}
	if len(digest) != sha256.Size*2 {
		return nil, ErrInvalidDispatch
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return nil, ErrInvalidDispatch
	}
	return []byte(strings.Join([]string{
		"witself.agent-email-dispatch-signature.v1",
		version, timestamp, keyID, audience, digest,
	}, "\n")), nil
}

// Client sends signed dispatches to the managed provider adapter.
type Client struct {
	Endpoint   string
	Audience   string
	KeyID      string
	PrivateKey ed25519.PrivateKey
	HTTPClient *http.Client
	Now        func() time.Time
}

// Validate checks the complete adapter endpoint and signing configuration.
func (c *Client) Validate() error {
	return c.validateFor(DispatchPath, "")
}

// ValidateReceiptReplay checks the dedicated operator proof endpoint and
// audience. A normal dispatch client cannot be reused accidentally.
func (c *Client) ValidateReceiptReplay() error {
	return c.validateFor(ReceiptReplayPath, ReceiptReplayAudience)
}

func (c *Client) validateFor(path, exactAudience string) error {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.Fragment != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Path != path || u.RawPath != "" {
		return fmt.Errorf("agent-email dispatch endpoint must be an exact HTTPS %s URL", path)
	}
	if !keyIDPattern.MatchString(c.KeyID) || c.Audience == "" ||
		c.Audience != strings.TrimSpace(c.Audience) || strings.ContainsAny(c.Audience, "\r\n") {
		return errors.New("agent-email dispatch identity is invalid")
	}
	if exactAudience != "" && c.Audience != exactAudience {
		return errors.New("agent-email receipt replay audience is invalid")
	}
	if len(c.PrivateKey) != ed25519.PrivateKeySize {
		return errors.New("agent-email dispatch private key is invalid")
	}
	return nil
}

// Send submits one dispatch. Transport failures are reported as ambiguous:
// the caller may resolve by replaying the same send id against the adapter's
// durable receipt, but must never create a fresh logical send automatically.
func (c *Client) Send(ctx context.Context, dispatch Dispatch) (Response, error) {
	if err := c.Validate(); err != nil {
		return Response{}, err
	}
	status, responseBody, err := c.signedPost(ctx, dispatch)
	if err != nil {
		return Response{SchemaVersion: ResponseSchemaVersion, SendID: dispatch.SendID, State: StateAmbiguous, Provider: "managed"}, err
	}
	var out Response
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil || out.Validate(dispatch.SendID) != nil {
		return Response{SchemaVersion: ResponseSchemaVersion, SendID: dispatch.SendID, State: StateAmbiguous, Provider: "managed"}, ErrInvalidResponse
	}
	if status < 200 || status >= 300 {
		if out.State != StateRetryable && out.State != StateRejected && out.State != StateAmbiguous {
			return Response{SchemaVersion: ResponseSchemaVersion, SendID: dispatch.SendID, State: StateAmbiguous, Provider: "managed"}, ErrInvalidResponse
		}
		return out, fmt.Errorf("agent-email adapter returned HTTP %d", status)
	}
	return out, nil
}

// ReplayReceipt asks the adapter to prove one exact accepted durable receipt.
// This operation never authorizes a provider call. Only an exact 200 proof with
// the closed schema and a single provider-start fence is accepted.
func (c *Client) ReplayReceipt(
	ctx context.Context,
	dispatch Dispatch,
) (ReceiptReplayProof, error) {
	if err := c.ValidateReceiptReplay(); err != nil {
		return ReceiptReplayProof{}, err
	}
	status, responseBody, err := c.signedPost(ctx, dispatch)
	if err != nil {
		return ReceiptReplayProof{}, err
	}
	if status != http.StatusOK {
		return ReceiptReplayProof{}, fmt.Errorf(
			"agent-email receipt replay returned HTTP %d", status,
		)
	}
	proof, err := decodeReceiptReplayProof(responseBody, dispatch.SendID)
	if err != nil {
		return ReceiptReplayProof{}, err
	}
	return proof, nil
}

func (c *Client) signedPost(
	ctx context.Context,
	dispatch Dispatch,
) (int, []byte, error) {
	body, err := dispatch.Marshal()
	if err != nil {
		return 0, nil, err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	timestamp := now().UTC().Format(time.RFC3339Nano)
	digestBytes := sha256.Sum256(body)
	digest := hex.EncodeToString(digestBytes[:])
	input, err := SignatureInput(
		DispatchSchemaVersion, timestamp, c.KeyID, c.Audience, digest,
	)
	if err != nil {
		return 0, nil, err
	}
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(c.PrivateKey, input))
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body),
	)
	if err != nil {
		return 0, nil, errors.New("create signed agent-email request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderVersion, DispatchSchemaVersion)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderKeyID, c.KeyID)
	req.Header.Set(HeaderAudience, c.Audience)
	req.Header.Set(HeaderDigest, digest)
	req.Header.Set(HeaderSignature, signature)

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	// A dispatch carries the message body and cell signature. Never allow an
	// adapter redirect to forward either to a different origin or path. Copy
	// the configured client so this rule cannot mutate shared state or be
	// weakened by a caller-supplied redirect policy.
	noRedirectClient := *httpClient
	noRedirectClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	res, err := noRedirectClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("signed agent-email request failed: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil || len(responseBody) > maxResponseBytes {
		return 0, nil, ErrInvalidResponse
	}
	return res.StatusCode, responseBody, nil
}

func decodeReceiptReplayProof(body []byte, sendID string) (ReceiptReplayProof, error) {
	allowed := map[string]bool{
		"schema_version": true, "send_id": true, "receipt_state": true,
		"digest_matched": true, "signer_matched": true,
		"provider_call_started_count": true, "verified_replay_count": true,
		"route_pending": true,
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return ReceiptReplayProof{}, ErrInvalidResponse
	}
	seen := make(map[string]bool, len(allowed))
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] || seen[key] {
			return ReceiptReplayProof{}, ErrInvalidResponse
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return ReceiptReplayProof{}, ErrInvalidResponse
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(seen) != len(allowed) {
		return ReceiptReplayProof{}, ErrInvalidResponse
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ReceiptReplayProof{}, ErrInvalidResponse
	}
	var wire struct {
		SchemaVersion            *string `json:"schema_version"`
		SendID                   *string `json:"send_id"`
		ReceiptState             *string `json:"receipt_state"`
		DigestMatched            *bool   `json:"digest_matched"`
		SignerMatched            *bool   `json:"signer_matched"`
		ProviderCallStartedCount *int64  `json:"provider_call_started_count"`
		VerifiedReplayCount      *int64  `json:"verified_replay_count"`
		RoutePending             *bool   `json:"route_pending"`
	}
	if err := json.Unmarshal(body, &wire); err != nil ||
		wire.SchemaVersion == nil || wire.SendID == nil ||
		wire.ReceiptState == nil || wire.DigestMatched == nil ||
		wire.SignerMatched == nil || wire.ProviderCallStartedCount == nil ||
		wire.VerifiedReplayCount == nil || wire.RoutePending == nil ||
		*wire.SchemaVersion != ReceiptReplayProofSchemaVersion ||
		*wire.SendID != sendID || *wire.ReceiptState != StateAccepted ||
		!*wire.DigestMatched || !*wire.SignerMatched ||
		*wire.ProviderCallStartedCount != 1 ||
		*wire.VerifiedReplayCount < 1 || *wire.VerifiedReplayCount > 1_000_000 {
		return ReceiptReplayProof{}, ErrInvalidResponse
	}
	return ReceiptReplayProof{
		SchemaVersion:            *wire.SchemaVersion,
		SendID:                   *wire.SendID,
		ReceiptState:             *wire.ReceiptState,
		DigestMatched:            *wire.DigestMatched,
		SignerMatched:            *wire.SignerMatched,
		ProviderCallStartedCount: *wire.ProviderCallStartedCount,
		VerifiedReplayCount:      *wire.VerifiedReplayCount,
		RoutePending:             *wire.RoutePending,
	}, nil
}

// Validate checks a closed adapter response against the logical send ID.
func (r Response) Validate(sendID string) error {
	if r.SchemaVersion != ResponseSchemaVersion || r.SendID != sendID ||
		!validGeneratedID(r.SendID, "esnd") || !keyIDPattern.MatchString(r.Provider) {
		return ErrInvalidResponse
	}
	switch r.State {
	case StateAccepted, StateDelivered, StateQueued:
		if r.ProviderMessageID == "" || len(r.ProviderMessageID) > 512 || r.ErrorCode != "" {
			return ErrInvalidResponse
		}
	case StatePermanentBounce, StateRejected, StateRetryable, StateAmbiguous:
		if r.ErrorCode == "" || !keyIDPattern.MatchString(r.ErrorCode) || r.ProviderMessageID != "" && len(r.ProviderMessageID) > 512 {
			return ErrInvalidResponse
		}
	default:
		return ErrInvalidResponse
	}
	if r.RetryAfterSeconds < 0 || r.RetryAfterSeconds > 24*60*60 ||
		r.State != StateRetryable && r.RetryAfterSeconds != 0 {
		return ErrInvalidResponse
	}
	return nil
}

// ParsePrivateKey decodes the base64 raw Ed25519 private key used only by a
// cell worker. It never invents replacement key material.
func ParsePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New("agent-email dispatch private key must be base64 raw Ed25519")
	}
	return ed25519.PrivateKey(raw), nil
}
