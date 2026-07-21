package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/agentemail"
)

// AgentEmailRelayHeaderVersion and the companion relay-header constants name
// the complete signed Cloudflare-to-Witself HTTP envelope.
const (
	AgentEmailRelayHeaderVersion           = "X-Witself-Email-Version"
	AgentEmailRelayHeaderTimestamp         = "X-Witself-Email-Timestamp"
	AgentEmailRelayHeaderKeyID             = "X-Witself-Email-Key-Id"
	AgentEmailRelayHeaderAudience          = "X-Witself-Email-Audience"
	AgentEmailRelayHeaderEnvelopeFrom      = "X-Witself-Email-Envelope-From"
	AgentEmailRelayHeaderEnvelopeTo        = "X-Witself-Email-Envelope-To"
	AgentEmailRelayHeaderRawSize           = "X-Witself-Email-Raw-Size"
	AgentEmailRelayHeaderRawSHA256         = "X-Witself-Email-Raw-SHA256"
	AgentEmailRelayHeaderSignature         = "X-Witself-Email-Signature"
	defaultAgentEmailListenWaitSeconds     = 20
	maxAgentEmailListenWaitSeconds         = 20
	agentEmailListenPollInterval           = time.Second
	maxConcurrentAgentEmailListens         = 128
	maxConcurrentAgentEmailListensPerAgent = 2
	minAgentEmailProcessingLeaseSeconds    = 30
	maxAgentEmailProcessingLeaseSeconds    = 15 * 60
	minAgentEmailRelayReplayWindow         = time.Second
	maxAgentEmailRelayReplayWindow         = 15 * time.Minute
)

var (
	// ErrAgentEmailUnknownRecipient reports that the signed envelope recipient
	// does not resolve to an enabled pilot mailbox.
	ErrAgentEmailUnknownRecipient = errors.New("unknown agent-email recipient")
	// ErrAgentEmailReceiveDisabled reports a known mailbox whose receive state is disabled.
	ErrAgentEmailReceiveDisabled = errors.New("agent-email receive is disabled")
	// ErrAgentEmailPilotUnavailable reports a transient pilot-wide ingestion failure.
	ErrAgentEmailPilotUnavailable = errors.New("agent-email pilot is unavailable")
	// ErrAgentEmailCodeConsumed reports a repeated single-use code-consumption attempt.
	ErrAgentEmailCodeConsumed = errors.New("agent-email code was already consumed")
)

// AgentEmailPilotConfig is a process-lifetime, default-off capability fence.
// Exactly one realm and 5-10 agents must be enabled. RelayPublicKeys supports
// bounded dual-key rotation; the signed key id selects one exact key.
type AgentEmailPilotConfig struct {
	Enabled           bool
	Domain            string
	Audience          string
	RealmIDs          map[string]bool
	AgentIDs          map[string]bool
	RelayPublicKeys   map[string]ed25519.PublicKey
	RelayReplayWindow time.Duration
	Now               func() time.Time
}

// ValidateAgentEmailPilotConfig fails closed on an enabled pilot whose scope
// or relay trust material exceeds the explicitly authorized pilot boundary.
func ValidateAgentEmailPilotConfig(cfg AgentEmailPilotConfig) error {
	if !cfg.Enabled {
		return nil
	}
	domain, err := agentemail.ValidateDomain(cfg.Domain)
	if err != nil || domain != cfg.Domain {
		return errors.New("agent-email pilot domain is invalid")
	}
	audience := strings.TrimSpace(cfg.Audience)
	if !validAgentEmailAudience(audience) || audience != strings.ToLower(audience) {
		return errors.New("agent-email pilot audience is invalid")
	}
	if cfg.RelayReplayWindow < minAgentEmailRelayReplayWindow || cfg.RelayReplayWindow > maxAgentEmailRelayReplayWindow {
		return errors.New("agent-email relay replay window must be between 1s and 15m")
	}
	if len(cfg.RelayPublicKeys) == 0 {
		return errors.New("agent-email relay public keys are required")
	}
	for keyID, key := range cfg.RelayPublicKeys {
		if !validAgentEmailRelayKeyID(keyID) || keyID != strings.ToLower(strings.TrimSpace(keyID)) || len(key) != ed25519.PublicKeySize {
			return errors.New("agent-email relay public key entry is invalid")
		}
	}
	if countEnabledAgentEmailIDs(cfg.RealmIDs, "realm") != 1 {
		return errors.New("agent-email pilot requires exactly one enabled realm")
	}
	agents := countEnabledAgentEmailIDs(cfg.AgentIDs, "agent")
	if agents < 5 || agents > 10 {
		return errors.New("agent-email pilot requires 5-10 enabled agents")
	}
	return nil
}

func countEnabledAgentEmailIDs(values map[string]bool, prefix string) int {
	count := 0
	for value, enabled := range values {
		if !enabled {
			continue
		}
		if !validAgentEmailGeneratedID(value, prefix) {
			return -1
		}
		count++
	}
	return count
}

func validAgentEmailGeneratedID(value, prefix string) bool {
	body := strings.TrimPrefix(value, prefix+"_")
	if body == value || len(body) != 16 {
		return false
	}
	for _, c := range []byte(body) {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

func validAgentEmailAudience(value string) bool {
	if len(value) < 1 || len(value) > 128 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, c := range []byte(value) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return value[len(value)-1] != '-'
}

func validAgentEmailRelayKeyID(value string) bool {
	if len(value) < 1 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, c := range []byte(value) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func (cfg AgentEmailPilotConfig) allows(p DomainPrincipal) bool {
	return cfg.Enabled && p.Kind == PrincipalKindAgent && cfg.RealmIDs[p.RealmID] && cfg.AgentIDs[p.ID]
}

// AgentEmailIngestFunc receives only a byte-identical body and metadata that
// the HTTP boundary has already verified against the configured relay key.
type AgentEmailIngestFunc func(context.Context, agentemail.RelayMetadata, []byte) error

// AgentEmailAddress is the owner-visible mailbox address and lifecycle state.
type AgentEmailAddress struct {
	ID               string     `json:"id"`
	MailboxID        string     `json:"mailbox_id"`
	AccountID        string     `json:"account_id"`
	RealmID          string     `json:"realm_id"`
	OwnerAgentID     string     `json:"owner_agent_id"`
	Address          string     `json:"address"`
	Domain           string     `json:"domain"`
	LocalPart        string     `json:"local_part"`
	AgentSegment     string     `json:"agent_segment"`
	RealmLabel       string     `json:"realm_label"`
	ProvisioningKind string     `json:"provisioning_kind"`
	ReceiveState     string     `json:"receive_state"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	DisabledAt       *time.Time `json:"disabled_at,omitempty"`
	RetiredAt        *time.Time `json:"retired_at,omitempty"`
}

// AgentEmailReadState records explicit content reads, acknowledgements, and
// the single-use code-consumption marker.
type AgentEmailReadState struct {
	State          string     `json:"state"`
	ReadAt         *time.Time `json:"read_at,omitempty"`
	AckedAt        *time.Time `json:"acked_at,omitempty"`
	CodeConsumedAt *time.Time `json:"code_consumed_at,omitempty"`
}

// AgentEmailProcessing is value-free. ClaimID and LeaseExpiresAt appear only
// in direct processing-transition results, never in list/read metadata.
type AgentEmailProcessing struct {
	State          string     `json:"state"`
	Generation     int64      `json:"generation"`
	FailureCount   int64      `json:"failure_count"`
	ClaimID        string     `json:"claim_id,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// AgentEmailMessage deliberately has no raw-MIME or attachment-content field.
// Text is populated only by the explicit owner read operation and is untrusted.
type AgentEmailMessage struct {
	ID                         string               `json:"id"`
	AccountID                  string               `json:"account_id"`
	RealmID                    string               `json:"realm_id"`
	MailboxID                  string               `json:"mailbox_id"`
	OwnerAgentID               string               `json:"owner_agent_id"`
	AddressID                  string               `json:"address_id"`
	Provider                   string               `json:"provider"`
	EnvelopeSender             string               `json:"envelope_sender"`
	EnvelopeRecipient          string               `json:"envelope_recipient"`
	AgentSegment               string               `json:"agent_segment"`
	RealmLabel                 string               `json:"realm_label"`
	SubaddressTag              string               `json:"subaddress_tag,omitempty"`
	RawSizeBytes               int64                `json:"raw_size_bytes"`
	ParseState                 string               `json:"parse_state"`
	ParseErrorCode             string               `json:"parse_error_code,omitempty"`
	HeaderFrom                 string               `json:"header_from,omitempty"`
	HeaderTo                   string               `json:"header_to,omitempty"`
	Subject                    string               `json:"subject,omitempty"`
	MIMEMessageID              string               `json:"mime_message_id,omitempty"`
	MessageDate                *time.Time           `json:"message_date,omitempty"`
	AttachmentCount            int64                `json:"attachment_count"`
	SPFResult                  string               `json:"spf_result"`
	DKIMResult                 string               `json:"dkim_result"`
	DMARCResult                string               `json:"dmarc_result"`
	SpamVerdict                string               `json:"spam_verdict"`
	SenderVerificationState    string               `json:"sender_verification_state"`
	PossibleDuplicate          bool                 `json:"possible_duplicate"`
	PossibleDuplicateOfMessage string               `json:"possible_duplicate_of_message_id,omitempty"`
	ReceivedAt                 time.Time            `json:"received_at"`
	CreatedAt                  time.Time            `json:"created_at"`
	Folder                     string               `json:"folder"`
	DeliveredAt                time.Time            `json:"delivered_at"`
	ReadState                  AgentEmailReadState  `json:"read_state"`
	Processing                 AgentEmailProcessing `json:"processing"`
	Text                       string               `json:"text,omitempty"`
	TextKind                   string               `json:"text_kind,omitempty"`
}

// AgentEmailListOptions contains the bounded owner-mailbox list filters.
type AgentEmailListOptions struct {
	Unread      bool
	Unacked     bool
	OldestFirst bool
	Limit       int
	Cursor      string
}

// AgentEmailPage is one metadata-only page from the owner mailbox.
type AgentEmailPage struct {
	Messages   []AgentEmailMessage
	NextCursor string
}

// AgentEmailListenRequest configures one bounded foreground mailbox poll.
type AgentEmailListenRequest struct {
	WaitSeconds *int `json:"wait_seconds,omitempty"`
	Limit       int  `json:"limit,omitempty"`
}

// ClaimAgentEmailRequest starts one fenced processing lease.
type ClaimAgentEmailRequest struct {
	LeaseSeconds   int    `json:"lease_seconds"`
	IdempotencyKey string `json:"-"`
}

// RenewAgentEmailClaimRequest renews an exact active processing fence.
type RenewAgentEmailClaimRequest struct {
	ClaimID      string `json:"claim_id"`
	Generation   int64  `json:"generation"`
	LeaseSeconds int    `json:"lease_seconds"`
}

// ReleaseAgentEmailClaimRequest releases an exact processing fence.
type ReleaseAgentEmailClaimRequest struct {
	ClaimID              string `json:"claim_id"`
	Generation           int64  `json:"generation"`
	DeterministicFailure bool   `json:"deterministic_failure"`
}

// CompleteAgentEmailRequest completes an exact processing fence idempotently.
type CompleteAgentEmailRequest struct {
	ClaimID        string `json:"claim_id"`
	Generation     int64  `json:"generation"`
	IdempotencyKey string `json:"-"`
}

// AgentEmailCheckpoint is a bounded, value-free foreground-work hint.
type AgentEmailCheckpoint struct {
	Pending        bool `json:"pending"`
	Unavailable    bool `json:"unavailable,omitempty"`
	MailboxPending bool `json:"mailbox_pending,omitempty"`
}

func agentEmailIngestHandler(cfg AgentEmailPilotConfig, ingest AgentEmailIngestFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		metadata, signature, ok := parseAgentEmailRelayHeaders(r)
		if !ok {
			writeAgentEmailVerdict(w, http.StatusUnauthorized, "invalid_relay")
			return
		}
		if metadata.RawSize > agentemail.PilotMaximumRawBytes || r.ContentLength > agentemail.PilotMaximumRawBytes {
			writeAgentEmailVerdict(w, http.StatusRequestEntityTooLarge, "permanent")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, agentemail.PilotMaximumRawBytes)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeAgentEmailVerdict(w, http.StatusRequestEntityTooLarge, "permanent")
			return
		}
		key := cfg.RelayPublicKeys[metadata.KeyID]
		now := time.Now()
		if cfg.Now != nil {
			now = cfg.Now()
		}
		verified, err := agentemail.VerifyRelay(now, cfg.RelayReplayWindow, key, metadata, raw, signature)
		if err != nil || verified.Audience != cfg.Audience {
			writeAgentEmailVerdict(w, http.StatusUnauthorized, "invalid_relay")
			return
		}
		err = ingest(r.Context(), verified, raw)
		switch {
		case err == nil:
			writeAgentEmailVerdict(w, http.StatusOK, "accepted")
		case errors.Is(err, ErrAgentEmailUnknownRecipient), errors.Is(err, ErrNotFound):
			writeAgentEmailVerdict(w, http.StatusNotFound, "unknown_recipient")
		case errors.Is(err, ErrAgentEmailReceiveDisabled):
			writeAgentEmailVerdict(w, http.StatusServiceUnavailable, "receive_disabled")
		case errors.Is(err, ErrAgentEmailPilotUnavailable), errors.Is(err, ErrForbidden):
			writeAgentEmailVerdict(w, http.StatusServiceUnavailable, "temporary")
		default:
			writeAgentEmailVerdict(w, http.StatusServiceUnavailable, "temporary")
		}
	}
}

func parseAgentEmailRelayHeaders(r *http.Request) (agentemail.RelayMetadata, []byte, bool) {
	version, ok := singleAgentEmailHeader(r.Header, AgentEmailRelayHeaderVersion)
	if !ok || version != agentemail.RelaySignatureVersion {
		return agentemail.RelayMetadata{}, nil, false
	}
	timestampRaw, ok := singleAgentEmailHeader(r.Header, AgentEmailRelayHeaderTimestamp)
	if !ok {
		return agentemail.RelayMetadata{}, nil, false
	}
	timestamp, err := strconv.ParseInt(timestampRaw, 10, 64)
	if err != nil || strconv.FormatInt(timestamp, 10) != timestampRaw {
		return agentemail.RelayMetadata{}, nil, false
	}
	rawSizeText, ok := singleAgentEmailHeader(r.Header, AgentEmailRelayHeaderRawSize)
	if !ok {
		return agentemail.RelayMetadata{}, nil, false
	}
	rawSize, err := strconv.ParseInt(rawSizeText, 10, 64)
	if err != nil || strconv.FormatInt(rawSize, 10) != rawSizeText {
		return agentemail.RelayMetadata{}, nil, false
	}
	keyID, keyOK := singleAgentEmailHeader(r.Header, AgentEmailRelayHeaderKeyID)
	audience, audienceOK := singleAgentEmailHeader(r.Header, AgentEmailRelayHeaderAudience)
	fromEncoded, fromOK := singleAgentEmailHeader(r.Header, AgentEmailRelayHeaderEnvelopeFrom)
	toEncoded, toOK := singleAgentEmailHeader(r.Header, AgentEmailRelayHeaderEnvelopeTo)
	digestHeader, digestOK := singleAgentEmailHeader(r.Header, AgentEmailRelayHeaderRawSHA256)
	signatureText, signatureOK := singleAgentEmailHeader(r.Header, AgentEmailRelayHeaderSignature)
	if !keyOK || !audienceOK || !fromOK || !toOK || !digestOK || !signatureOK ||
		len(fromEncoded) > 512 || len(toEncoded) > 512 || !strings.HasPrefix(digestHeader, "sha256:") {
		return agentemail.RelayMetadata{}, nil, false
	}
	from, err := decodeAgentEmailEnvelopeHeader(fromEncoded)
	if err != nil {
		return agentemail.RelayMetadata{}, nil, false
	}
	to, err := decodeAgentEmailEnvelopeHeader(toEncoded)
	if err != nil {
		return agentemail.RelayMetadata{}, nil, false
	}
	signature, err := agentemail.ParseSignature(signatureText)
	if err != nil {
		return agentemail.RelayMetadata{}, nil, false
	}
	metadata := agentemail.RelayMetadata{
		Timestamp: timestamp, KeyID: keyID, Audience: audience,
		EnvelopeSender: from, EnvelopeRecipient: to, RawSize: rawSize,
		RawSHA256: strings.TrimPrefix(digestHeader, "sha256:"),
	}
	normalized, err := metadata.Normalize()
	if err != nil || normalized != metadata || digestHeader != "sha256:"+metadata.RawSHA256 {
		return agentemail.RelayMetadata{}, nil, false
	}
	return metadata, signature, true
}

func singleAgentEmailHeader(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	returnValue := ""
	if len(values) == 1 {
		returnValue = values[0]
	}
	return returnValue, len(values) == 1
}

func decodeAgentEmailEnvelopeHeader(encoded string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded || !utf8.Valid(raw) {
		return "", errors.New("invalid envelope header")
	}
	return string(raw), nil
}

func writeAgentEmailVerdict(w http.ResponseWriter, status int, verdict string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"verdict": verdict})
}

func getAgentEmailAddressHandler(
	auth PrincipalAuthFunc,
	pilot AgentEmailPilotConfig,
	get func(context.Context, DomainPrincipal) (AgentEmailAddress, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailReadPrincipal(auth, pilot, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		address, err := get(r.Context(), p)
		if writeAgentEmailOwnerError(w, err, "could not get agent email address") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "address": address})
	}))
}

func listAgentEmailsHandler(
	auth PrincipalAuthFunc,
	pilot AgentEmailPilotConfig,
	list func(context.Context, DomainPrincipal, AgentEmailListOptions) (AgentEmailPage, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailReadPrincipal(auth, pilot, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		q := r.URL.Query()
		opts := AgentEmailListOptions{Cursor: q.Get("cursor")}
		for name, destination := range map[string]*bool{
			"unread": &opts.Unread, "unacked": &opts.Unacked, "oldest_first": &opts.OldestFirst,
		} {
			if raw := q.Get(name); raw != "" {
				value, err := strconv.ParseBool(raw)
				if err != nil {
					writeJSONError(w, http.StatusBadRequest, name+" must be true or false")
					return
				}
				*destination = value
			}
		}
		if raw := q.Get("limit"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "limit must be an integer")
				return
			}
			opts.Limit = value
		}
		page, err := list(r.Context(), p, opts)
		if writeAgentEmailOwnerError(w, err, "could not list agent email") {
			return
		}
		if page.Messages == nil {
			page.Messages = []AgentEmailMessage{}
		}
		for i := range page.Messages {
			page.Messages[i] = redactAgentEmailMessage(page.Messages[i], false)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "messages": page.Messages, "next_cursor": page.NextCursor,
		})
	}))
}

type agentEmailListenLimiter struct {
	mu      sync.Mutex
	active  int
	byAgent map[string]int
}

func (l *agentEmailListenLimiter) tryAcquire(p DomainPrincipal) bool {
	key := p.AccountID + "\x00" + p.RealmID + "\x00" + p.ID
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active >= maxConcurrentAgentEmailListens || l.byAgent[key] >= maxConcurrentAgentEmailListensPerAgent {
		return false
	}
	l.active++
	l.byAgent[key]++
	return true
}

func (l *agentEmailListenLimiter) release(p DomainPrincipal) {
	key := p.AccountID + "\x00" + p.RealmID + "\x00" + p.ID
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	if l.byAgent[key] <= 1 {
		delete(l.byAgent, key)
		return
	}
	l.byAgent[key]--
}

func agentEmailListenHandler(
	auth PrincipalAuthFunc,
	pilot AgentEmailPilotConfig,
	list func(context.Context, DomainPrincipal, AgentEmailListOptions) (AgentEmailPage, error),
) http.HandlerFunc {
	limiter := &agentEmailListenLimiter{byAgent: make(map[string]int)}
	return agentEmailNoStore(requireAgentEmailReadPrincipal(auth, pilot, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		var req AgentEmailListenRequest
		if err := decodeStrictAgentEmailJSON(w, r, &req, 16*1024); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		waitSeconds := defaultAgentEmailListenWaitSeconds
		if req.WaitSeconds != nil {
			waitSeconds = *req.WaitSeconds
		}
		if waitSeconds < 0 || waitSeconds > maxAgentEmailListenWaitSeconds {
			writeJSONError(w, http.StatusBadRequest, "wait_seconds must be between 0 and 20")
			return
		}
		if !limiter.tryAcquire(p) {
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusTooManyRequests, "too many concurrent email listens")
			return
		}
		defer limiter.release(p)
		opts := AgentEmailListOptions{Unacked: true, OldestFirst: true, Limit: req.Limit}
		deadline := time.NewTimer(time.Duration(waitSeconds) * time.Second)
		defer deadline.Stop()
		poll := time.NewTicker(agentEmailListenPollInterval)
		defer poll.Stop()
		for {
			page, err := list(r.Context(), p, opts)
			if writeAgentEmailOwnerError(w, err, "could not listen for agent email") {
				return
			}
			for i := range page.Messages {
				page.Messages[i] = redactAgentEmailMessage(page.Messages[i], false)
			}
			if len(page.Messages) != 0 {
				writeAgentEmailListenResult(w, page.Messages, false)
				return
			}
			if waitSeconds == 0 {
				writeAgentEmailListenResult(w, []AgentEmailMessage{}, true)
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-deadline.C:
				writeAgentEmailListenResult(w, []AgentEmailMessage{}, true)
				return
			case <-poll.C:
			}
		}
	}))
}

func writeAgentEmailListenResult(w http.ResponseWriter, messages []AgentEmailMessage, timedOut bool) {
	if messages == nil {
		messages = []AgentEmailMessage{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema_version": "witself.v0", "messages": messages, "timed_out": timedOut,
	})
}

func getAgentEmailCheckpointHandler(
	auth PrincipalAuthFunc,
	pilot AgentEmailPilotConfig,
	get func(context.Context, DomainPrincipal) (AgentEmailCheckpoint, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailReadPrincipal(auth, pilot, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		checkpoint, err := get(r.Context(), p)
		if writeAgentEmailOwnerError(w, err, "could not get agent email checkpoint") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "checkpoint": checkpoint,
		})
	}))
}

func agentEmailActionHandler(
	auth PrincipalAuthFunc,
	pilot AgentEmailPilotConfig,
	read func(context.Context, DomainPrincipal, string) (AgentEmailMessage, error),
	ack func(context.Context, DomainPrincipal, string) (AgentEmailMessage, error),
	codeConsumed func(context.Context, DomainPrincipal, string) (AgentEmailMessage, error),
	claim func(context.Context, DomainPrincipal, string, ClaimAgentEmailRequest) (AgentEmailProcessing, error),
	renew func(context.Context, DomainPrincipal, string, RenewAgentEmailClaimRequest) (AgentEmailProcessing, error),
	release func(context.Context, DomainPrincipal, string, ReleaseAgentEmailClaimRequest) (AgentEmailProcessing, error),
	complete func(context.Context, DomainPrincipal, string, CompleteAgentEmailRequest) (AgentEmailProcessing, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailPrincipal(auth, pilot, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		action := r.PathValue("action")
		messageID, operation, ok := strings.Cut(action, ":")
		if !ok || messageID == "" || !agentEmailOperationAllowed(operation) {
			writeJSONError(w, http.StatusNotFound, "email action not found")
			return
		}
		if operation == "read" {
			if read == nil {
				writeJSONError(w, http.StatusNotFound, "email action not found")
				return
			}
			msg, err := read(r.Context(), p, messageID)
			if writeAgentEmailOwnerError(w, err, "could not read agent email") {
				return
			}
			msg = redactAgentEmailMessage(msg, true)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "message": msg})
			return
		}
		if operation == "ack" || operation == "code-consumed" {
			var msg AgentEmailMessage
			var err error
			if operation == "ack" && ack != nil {
				msg, err = ack(r.Context(), p, messageID)
			} else if operation == "code-consumed" && codeConsumed != nil {
				msg, err = codeConsumed(r.Context(), p, messageID)
			} else {
				writeJSONError(w, http.StatusNotFound, "email action not found")
				return
			}
			if writeAgentEmailOwnerError(w, err, "could not update agent email") {
				return
			}
			msg = redactAgentEmailMessage(msg, false)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "message": msg})
			return
		}
		handleAgentEmailProcessingAction(w, r, p, messageID, operation, claim, renew, release, complete)
	}))
}

func agentEmailOperationAllowed(operation string) bool {
	switch operation {
	case "read", "ack", "code-consumed", "claim", "renew", "release", "complete":
		return true
	default:
		return false
	}
}

func handleAgentEmailProcessingAction(
	w http.ResponseWriter,
	r *http.Request,
	p DomainPrincipal,
	messageID, operation string,
	claim func(context.Context, DomainPrincipal, string, ClaimAgentEmailRequest) (AgentEmailProcessing, error),
	renew func(context.Context, DomainPrincipal, string, RenewAgentEmailClaimRequest) (AgentEmailProcessing, error),
	release func(context.Context, DomainPrincipal, string, ReleaseAgentEmailClaimRequest) (AgentEmailProcessing, error),
	complete func(context.Context, DomainPrincipal, string, CompleteAgentEmailRequest) (AgentEmailProcessing, error),
) {
	var processing AgentEmailProcessing
	var err error
	switch operation {
	case "claim":
		if claim == nil {
			writeJSONError(w, http.StatusNotFound, "email action not found")
			return
		}
		var req ClaimAgentEmailRequest
		if decodeStrictAgentEmailJSON(w, r, &req, 16*1024) != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if !agentEmailLeaseSecondsWithinBounds(req.LeaseSeconds) {
			writeJSONError(w, http.StatusBadRequest, "lease_seconds must be 0 or between 30 and 900")
			return
		}
		req.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		processing, err = claim(r.Context(), p, messageID, req)
	case "renew":
		if renew == nil {
			writeJSONError(w, http.StatusNotFound, "email action not found")
			return
		}
		var req RenewAgentEmailClaimRequest
		if decodeStrictAgentEmailJSON(w, r, &req, 16*1024) != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if !agentEmailLeaseSecondsWithinBounds(req.LeaseSeconds) {
			writeJSONError(w, http.StatusBadRequest, "lease_seconds must be 0 or between 30 and 900")
			return
		}
		processing, err = renew(r.Context(), p, messageID, req)
	case "release":
		if release == nil {
			writeJSONError(w, http.StatusNotFound, "email action not found")
			return
		}
		var req ReleaseAgentEmailClaimRequest
		if decodeStrictAgentEmailJSON(w, r, &req, 16*1024) != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		processing, err = release(r.Context(), p, messageID, req)
	case "complete":
		if complete == nil {
			writeJSONError(w, http.StatusNotFound, "email action not found")
			return
		}
		var req CompleteAgentEmailRequest
		if decodeStrictAgentEmailJSON(w, r, &req, 16*1024) != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		processing, err = complete(r.Context(), p, messageID, req)
	}
	if writeAgentEmailProcessingError(w, err) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "processing": processing})
}

func agentEmailLeaseSecondsWithinBounds(seconds int) bool {
	return seconds == 0 ||
		seconds >= minAgentEmailProcessingLeaseSeconds && seconds <= maxAgentEmailProcessingLeaseSeconds
}

func decodeStrictAgentEmailJSON(w http.ResponseWriter, r *http.Request, destination any, maximumBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maximumBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func writeAgentEmailProcessingError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrBadInput):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusNotFound, "email not found")
	case errors.Is(err, ErrBusy):
		writeJSONError(w, http.StatusConflict, "email is already claimed for processing")
	case errors.Is(err, ErrConflict), errors.Is(err, ErrIdempotencyConflict):
		writeJSONError(w, http.StatusConflict, "email processing claim is stale or conflicts")
	default:
		writeJSONError(w, http.StatusInternalServerError, "could not update email processing")
	}
	return true
}

func writeAgentEmailOwnerError(w http.ResponseWriter, err error, internalMessage string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrBadInput):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusNotFound, "email not found")
	case errors.Is(err, ErrBusy):
		writeJSONError(w, http.StatusConflict, "email is claimed for processing")
	case errors.Is(err, ErrAgentEmailCodeConsumed), errors.Is(err, ErrConflict):
		writeJSONError(w, http.StatusConflict, "email state conflicts")
	default:
		writeJSONError(w, http.StatusInternalServerError, internalMessage)
	}
	return true
}

func requireAgentEmailReadPrincipal(
	auth PrincipalAuthFunc,
	pilot AgentEmailPilotConfig,
	h func(http.ResponseWriter, *http.Request, DomainPrincipal),
) http.HandlerFunc {
	// The current immutable token model has only full and curator profiles. A
	// full agent credential carries the read tier; every curator profile remains
	// denied by requireDomainPrincipal. Processing uses a distinct wrapper below
	// so a future scoped-token migration can split the tiers without route churn.
	return requireAgentEmailPrincipal(auth, pilot, h)
}

func requireAgentEmailPrincipal(
	auth PrincipalAuthFunc,
	pilot AgentEmailPilotConfig,
	h func(http.ResponseWriter, *http.Request, DomainPrincipal),
) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may access email")
			return
		}
		if !pilot.allows(p) {
			writeJSONError(w, http.StatusForbidden, "agent is not enrolled in the email pilot")
			return
		}
		h(w, r, p)
	})
}

func redactAgentEmailMessage(msg AgentEmailMessage, includeText bool) AgentEmailMessage {
	msg.Processing.ClaimID = ""
	msg.Processing.LeaseExpiresAt = nil
	if !includeText {
		msg.Text = ""
		msg.TextKind = ""
	}
	return msg
}

func agentEmailNoStore(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		next(w, r)
	}
}

// agentEmailNoStoreMux covers method mismatches and not-found responses before
// a method-specific handler gets control.
func agentEmailNoStoreMux(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/email" || r.URL.Path == "/v1/email:listen" ||
			strings.HasPrefix(r.URL.Path, "/v1/email/") ||
			r.URL.Path == "/v1/internal/agent-email:ingest" {
			w.Header().Set("Cache-Control", "private, no-store")
		}
		next.ServeHTTP(w, r)
	})
}
