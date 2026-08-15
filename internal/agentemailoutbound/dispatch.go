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
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.Fragment != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Path != "/v1/dispatch" || u.RawPath != "" {
		return errors.New("agent-email dispatch endpoint must be an exact HTTPS /v1/dispatch URL")
	}
	if !keyIDPattern.MatchString(c.KeyID) || c.Audience == "" ||
		c.Audience != strings.TrimSpace(c.Audience) || strings.ContainsAny(c.Audience, "\r\n") {
		return errors.New("agent-email dispatch identity is invalid")
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
	body, err := dispatch.Marshal()
	if err != nil {
		return Response{}, err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	timestamp := now().UTC().Format(time.RFC3339Nano)
	digestBytes := sha256.Sum256(body)
	digest := hex.EncodeToString(digestBytes[:])
	input, err := SignatureInput(DispatchSchemaVersion, timestamp, c.KeyID, c.Audience, digest)
	if err != nil {
		return Response{}, err
	}
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(c.PrivateKey, input))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("create agent-email dispatch request: %w", err)
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
	// the configured client so this safety rule cannot mutate shared state or
	// be weakened by a caller-supplied redirect policy.
	noRedirectClient := *httpClient
	noRedirectClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	res, err := noRedirectClient.Do(req)
	if err != nil {
		return Response{SchemaVersion: ResponseSchemaVersion, SendID: dispatch.SendID, State: StateAmbiguous, Provider: "managed"}, err
	}
	defer func() { _ = res.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil || len(responseBody) > maxResponseBytes {
		return Response{SchemaVersion: ResponseSchemaVersion, SendID: dispatch.SendID, State: StateAmbiguous, Provider: "managed"}, ErrInvalidResponse
	}
	var out Response
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil || out.Validate(dispatch.SendID) != nil {
		return Response{SchemaVersion: ResponseSchemaVersion, SendID: dispatch.SendID, State: StateAmbiguous, Provider: "managed"}, ErrInvalidResponse
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if out.State != StateRetryable && out.State != StateRejected && out.State != StateAmbiguous {
			return Response{SchemaVersion: ResponseSchemaVersion, SendID: dispatch.SendID, State: StateAmbiguous, Provider: "managed"}, ErrInvalidResponse
		}
		return out, fmt.Errorf("agent-email adapter returned HTTP %d", res.StatusCode)
	}
	return out, nil
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
