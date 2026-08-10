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
	agentEmailRateRetryMaximumSeconds      = 60
	minAgentEmailProcessingLeaseSeconds    = 30
	maxAgentEmailProcessingLeaseSeconds    = 15 * 60
	minAgentEmailRelayReplayWindow         = time.Second
	maxAgentEmailRelayReplayWindow         = 15 * time.Minute
	maximumAgentEmailProductionAccounts    = 100
)

var (
	// ErrAgentEmailUnknownRecipient reports that the signed envelope recipient
	// does not resolve to an enabled pilot mailbox.
	ErrAgentEmailUnknownRecipient = errors.New("unknown agent-email recipient")
	// ErrAgentEmailReceiveDisabled reports a known mailbox whose receive state is disabled.
	ErrAgentEmailReceiveDisabled = errors.New("agent-email receive is disabled")
	// ErrAgentEmailFeatureDisabled reports a verified relay for an account
	// whose plan does not enable inbound email. The edge accepts and drops
	// this delivery without retrying and without revealing account state.
	ErrAgentEmailFeatureDisabled = errors.New("agent-email feature is disabled")
	// ErrAgentEmailRawSizeExceeded is a permanent plan-aware per-message size
	// refusal. The edge converts this exact value-free outcome to SMTP 552.
	ErrAgentEmailRawSizeExceeded = errors.New("agent-email raw message size exceeded")
	// ErrAgentEmailAttachmentOmitted is a successful partial-retention
	// outcome: bounded text and metadata landed, while attachment-bearing raw
	// MIME was omitted at the account capacity boundary.
	ErrAgentEmailAttachmentOmitted = errors.New("agent-email attachment payload omitted at capacity")
	// ErrAgentEmailPilotUnavailable reports a transient pilot-wide ingestion failure.
	ErrAgentEmailPilotUnavailable = errors.New("agent-email pilot is unavailable")
	// ErrAgentEmailRetryCanaryTemporary reports the deliberate first-attempt
	// temporary result for the synthetic provider retry proof.
	ErrAgentEmailRetryCanaryTemporary = errors.New("agent-email retry canary temporary failure")
	// ErrAgentEmailRetryCanaryPermanent reports a synthetic retry marker that
	// no live arm can authorize and that the edge must reject without retrying.
	ErrAgentEmailRetryCanaryPermanent = errors.New("agent-email retry canary permanent rejection")
	// ErrAgentEmailRateLimited reports a non-billable safety refusal shared by
	// every API replica. Retryable refusals become a temporary SMTP result;
	// impossible debits become a permanent refusal so they cannot amplify
	// retries forever.
	ErrAgentEmailRateLimited = errors.New("agent-email rate limited")
	// ErrAgentEmailCodeConsumed reports a repeated single-use code-consumption attempt.
	ErrAgentEmailCodeConsumed = errors.New("agent-email code was already consumed")
)

// AgentEmailRateLimitError preserves only closed labels and a bounded retry
// hint across the store-to-HTTP adapter. It never carries tenant, mailbox, or
// external-sender identifiers.
type AgentEmailRateLimitError struct {
	Dimension  string
	Scope      string
	Source     string
	RetryAfter time.Duration
	Retryable  bool
}

func (e *AgentEmailRateLimitError) Error() string {
	return ErrAgentEmailRateLimited.Error()
}

func (e *AgentEmailRateLimitError) Unwrap() error { return ErrAgentEmailRateLimited }

const (
	// AgentEmailReceiveModeLegacyPilot preserves the original one-realm,
	// 5-10-agent enrollment boundary and its retry-canary contract.
	AgentEmailReceiveModeLegacyPilot = "legacy_pilot"
	// AgentEmailReceiveModeProduction authorizes dynamic local route resolution
	// only for an exact process-lifetime account cohort. It is still default-off.
	AgentEmailReceiveModeProduction = "production"
)

// AgentEmailReceiveConfig is the process-lifetime, default-off receive fence.
// LegacyPilot keeps the original bounded pilot behavior. Production removes
// the fixed realm/agent list but still requires an exact account cohort; the
// database remains authoritative for entitlement, route, mailbox, realm, and
// agent lifecycle state. RelayPublicKeys supports bounded dual-key rotation;
// the signed key id selects one exact key.
type AgentEmailReceiveConfig struct {
	Enabled            bool
	Mode               string
	Domain             string
	LegacyDomains      []string
	Audience           string
	AccountIDs         map[string]bool
	RealmIDs           map[string]bool
	AgentIDs           map[string]bool
	RetryCanaryAgentID string
	RelayPublicKeys    map[string]ed25519.PublicKey
	RelayReplayWindow  time.Duration
	Now                func() time.Time
}

// AgentEmailPilotConfig remains a source-compatible alias for the legacy
// server/configuration surface while deployments graduate to receive mode.
type AgentEmailPilotConfig = AgentEmailReceiveConfig

// ValidateAgentEmailReceiveConfig fails closed on an enabled receive mode
// whose cohort or relay trust material is incomplete or ambiguous.
func ValidateAgentEmailReceiveConfig(cfg AgentEmailReceiveConfig) error {
	if !cfg.Enabled {
		return nil
	}
	domain, err := agentemail.ValidateDomain(cfg.Domain)
	if err != nil || domain != cfg.Domain {
		return errors.New("agent-email pilot domain is invalid")
	}
	if len(cfg.LegacyDomains) > 1 {
		return errors.New("agent-email pilot accepts at most 1 legacy domain")
	}
	seenDomains := map[string]bool{domain: true}
	for _, legacy := range cfg.LegacyDomains {
		normalized, legacyErr := agentemail.ValidateDomain(legacy)
		if legacyErr != nil || normalized != legacy || seenDomains[normalized] {
			return errors.New("agent-email pilot legacy domain entry is invalid or duplicated")
		}
		seenDomains[normalized] = true
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
	mode := cfg.Mode
	if mode == "" {
		mode = AgentEmailReceiveModeLegacyPilot
	}
	switch mode {
	case AgentEmailReceiveModeLegacyPilot:
		if countEnabledAgentEmailIDs(cfg.AccountIDs, "acc") != 0 {
			return errors.New("agent-email pilot cannot include a production account cohort")
		}
		if countEnabledAgentEmailIDs(cfg.RealmIDs, "realm") != 1 {
			return errors.New("agent-email pilot requires exactly one enabled realm")
		}
		agents := countEnabledAgentEmailIDs(cfg.AgentIDs, "agent")
		if agents < 5 || agents > 10 {
			return errors.New("agent-email pilot requires 5-10 enabled agents")
		}
		if cfg.RetryCanaryAgentID != "" &&
			(!validAgentEmailGeneratedID(cfg.RetryCanaryAgentID, "agent") ||
				!cfg.AgentIDs[cfg.RetryCanaryAgentID] ||
				cfg.RetryCanaryAgentID != strings.TrimSpace(cfg.RetryCanaryAgentID)) {
			return errors.New("agent-email retry canary agent must be enrolled")
		}
	case AgentEmailReceiveModeProduction:
		if countEnabledAgentEmailIDs(cfg.RealmIDs, "realm") != 0 ||
			countEnabledAgentEmailIDs(cfg.AgentIDs, "agent") != 0 {
			return errors.New("agent-email production receive cannot include pilot realm or agent enrollment")
		}
		accounts := countEnabledAgentEmailIDs(cfg.AccountIDs, "acc")
		if accounts < 1 || accounts > maximumAgentEmailProductionAccounts {
			return errors.New("agent-email production receive requires 1-100 exact accounts")
		}
		if cfg.RetryCanaryAgentID != "" &&
			(!validAgentEmailGeneratedID(cfg.RetryCanaryAgentID, "agent") ||
				cfg.RetryCanaryAgentID != strings.TrimSpace(cfg.RetryCanaryAgentID)) {
			return errors.New("agent-email retry canary agent is invalid")
		}
	default:
		return errors.New("agent-email receive mode is invalid")
	}
	return nil
}

// ValidateAgentEmailPilotConfig is the compatibility name used by existing
// integrations and tests. It validates both supported receive modes.
func ValidateAgentEmailPilotConfig(cfg AgentEmailPilotConfig) error {
	return ValidateAgentEmailReceiveConfig(cfg)
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
	if !cfg.Enabled || p.Kind != PrincipalKindAgent {
		return false
	}
	if cfg.Mode == AgentEmailReceiveModeProduction {
		return cfg.AccountIDs[p.AccountID]
	}
	return cfg.RealmIDs[p.RealmID] && cfg.AgentIDs[p.ID]
}

// AgentEmailIngestFunc receives only a byte-identical body and metadata that
// the HTTP boundary has already verified against the configured relay key.
type AgentEmailIngestFunc func(context.Context, agentemail.RelayMetadata, []byte) error

// AgentEmailAddress is the owner-visible mailbox address and lifecycle state.
type AgentEmailAddress struct {
	ID                string                        `json:"id"`
	MailboxID         string                        `json:"mailbox_id"`
	AccountID         string                        `json:"account_id"`
	RealmID           string                        `json:"realm_id"`
	OwnerAgentID      string                        `json:"owner_agent_id"`
	Address           string                        `json:"address"`
	Domain            string                        `json:"domain"`
	LocalPart         string                        `json:"local_part"`
	AgentSegment      string                        `json:"agent_segment"`
	RealmLabel        string                        `json:"realm_label"`
	ProvisioningKind  string                        `json:"provisioning_kind"`
	ReceiveState      string                        `json:"receive_state"`
	AgentReceiveState string                        `json:"agent_receive_state"`
	RealmReceiveState string                        `json:"realm_receive_state"`
	RowVersion        int64                         `json:"row_version"`
	CreatedAt         time.Time                     `json:"created_at"`
	UpdatedAt         time.Time                     `json:"updated_at"`
	DisabledAt        *time.Time                    `json:"disabled_at,omitempty"`
	RealmDisabledAt   *time.Time                    `json:"realm_disabled_at,omitempty"`
	RetiredAt         *time.Time                    `json:"retired_at,omitempty"`
	Addresses         []AgentEmailCanonicalAddress  `json:"addresses"`
	Aliases           []AgentEmailRealmAliasAddress `json:"aliases"`
}

// AgentEmailCanonicalAddress is one primary, legacy, or historical managed
// domain address that remains permanently bound to this mailbox reservation.
type AgentEmailCanonicalAddress struct {
	Address string `json:"address"`
	Domain  string `json:"domain"`
	Role    string `json:"role"`
}

// AgentEmailRealmAlias is the control-plane-visible cell acknowledgement for
// one globally claimed realm designator.
type AgentEmailRealmAlias struct {
	ClaimID            string     `json:"claim_id"`
	AccountID          string     `json:"account_id"`
	RealmID            string     `json:"realm_id"`
	Domain             string     `json:"domain"`
	RealmLabel         string     `json:"realm_label"`
	State              string     `json:"state"`
	ControllerRevision int64      `json:"controller_revision"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	SuspendedAt        *time.Time `json:"suspended_at,omitempty"`
	RetiredAt          *time.Time `json:"retired_at,omitempty"`
}

// AgentEmailRealmAliasApplyRequest is one monotonic desired projection. The
// account target remains path-bound.
type AgentEmailRealmAliasApplyRequest struct {
	ClaimID            string `json:"claim_id"`
	RealmID            string `json:"realm_id"`
	Domain             string `json:"domain"`
	RealmLabel         string `json:"realm_label"`
	State              string `json:"state"`
	ControllerRevision int64  `json:"controller_revision"`
}

// AgentEmailCustomDomainRoute is the exact provision-token readback for one
// custom inbound-domain route projection. It intentionally contains no
// provider configuration or message content.
type AgentEmailCustomDomainRoute struct {
	SchemaVersion            string `json:"schema_version"`
	AccountID                string `json:"account_id"`
	DomainRequestID          string `json:"domain_request_id"`
	DomainAllocationRevision int64  `json:"domain_allocation_revision"`
	DomainStateRevision      int64  `json:"domain_state_revision"`
	RealmAliasClaimID        string `json:"realm_alias_claim_id"`
	RealmAliasRevision       int64  `json:"realm_alias_revision"`
	RealmID                  string `json:"realm_id"`
	Domain                   string `json:"domain"`
	RealmLabel               string `json:"realm_label"`
	State                    string `json:"state"`
	SuspensionDisposition    string `json:"suspension_disposition,omitempty"`
	ControllerRevision       int64  `json:"controller_revision"`
}

// AgentEmailCustomDomainRouteApplyRequest uses the same exact contract as the
// readback. AccountID must match the path and SchemaVersion must be witself.v0.
type AgentEmailCustomDomainRouteApplyRequest = AgentEmailCustomDomainRoute

// AgentEmailRealmAliasTarget is the content-minimal provision-token response
// proving that an account owns one live realm.
type AgentEmailRealmAliasTarget struct {
	AccountID string `json:"account_id"`
	RealmID   string `json:"realm_id"`
	Exists    bool   `json:"exists"`
}

// RealmEmailRouteLifecycle is the value-free portable cell fence for one
// canonical realm-id email route.  Retired records remain discoverable as
// tombstones so recovery cannot recreate their route.
type RealmEmailRouteLifecycle struct {
	AccountID   string `json:"account_id"`
	RealmID     string `json:"realm_id"`
	State       string `json:"state"`
	Generation  int64  `json:"generation"`
	OperationID string `json:"operation_id,omitempty"`
}

// RealmEmailRouteLifecyclePage is one bounded control-plane inventory page.
type RealmEmailRouteLifecyclePage struct {
	Routes     []RealmEmailRouteLifecycle
	NextCursor string
}

// RealmEmailRouteRetirementRequest carries the exact operation-generation
// fence.  Prepare expects the live generation; commit expects the closing
// generation returned by prepare.
type RealmEmailRouteRetirementRequest struct {
	RealmID            string `json:"realm_id"`
	OperationID        string `json:"operation_id"`
	ExpectedGeneration int64  `json:"expected_generation"`
}

// AgentEmailRealmAliasAddress is one agent-specific address derived from a
// realm-level claim.
type AgentEmailRealmAliasAddress struct {
	ClaimID            string     `json:"claim_id"`
	Address            string     `json:"address"`
	LocalPart          string     `json:"local_part"`
	RealmLabel         string     `json:"realm_label"`
	State              string     `json:"state"`
	ControllerRevision int64      `json:"controller_revision"`
	UpdatedAt          time.Time  `json:"updated_at"`
	SuspendedAt        *time.Time `json:"suspended_at,omitempty"`
	RetiredAt          *time.Time `json:"retired_at,omitempty"`
}

// AgentEmailReceiveControl is the operator-visible value-free lifecycle view
// for one enrolled mailbox. It intentionally carries no address or message
// metadata.
type AgentEmailReceiveControl struct {
	AccountID         string     `json:"account_id"`
	RealmID           string     `json:"realm_id"`
	AgentID           string     `json:"agent_id"`
	ReceiveState      string     `json:"receive_state"`
	AgentReceiveState string     `json:"agent_receive_state"`
	RealmReceiveState string     `json:"realm_receive_state"`
	RowVersion        int64      `json:"row_version"`
	UpdatedAt         time.Time  `json:"updated_at"`
	DisabledAt        *time.Time `json:"disabled_at,omitempty"`
	RealmDisabledAt   *time.Time `json:"realm_disabled_at,omitempty"`
}

// AgentEmailRealmReceiveControl is the operator-visible realm kill-switch
// state. MailboxCount is value-free and confirms the bounded blast radius.
type AgentEmailRealmReceiveControl struct {
	AccountID    string     `json:"account_id"`
	RealmID      string     `json:"realm_id"`
	ReceiveState string     `json:"receive_state"`
	MailboxCount int64      `json:"mailbox_count"`
	RowVersion   int64      `json:"row_version"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DisabledAt   *time.Time `json:"disabled_at,omitempty"`
}

// SetAgentEmailReceiveControlRequest carries one exact desired switch state.
// Target identity is path-bound and never accepted from the body.
type SetAgentEmailReceiveControlRequest struct {
	ReceiveState string `json:"receive_state"`
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
	ID                             string               `json:"id"`
	AccountID                      string               `json:"account_id"`
	RealmID                        string               `json:"realm_id"`
	MailboxID                      string               `json:"mailbox_id"`
	OwnerAgentID                   string               `json:"owner_agent_id"`
	AddressID                      string               `json:"address_id"`
	Provider                       string               `json:"provider"`
	EnvelopeSender                 string               `json:"envelope_sender"`
	EnvelopeRecipient              string               `json:"envelope_recipient"`
	AgentSegment                   string               `json:"agent_segment"`
	RealmLabel                     string               `json:"realm_label"`
	RecipientRouteKind             string               `json:"recipient_route_kind"`
	RecipientRealmAliasClaimID     string               `json:"recipient_realm_alias_claim_id,omitempty"`
	RecipientCustomDomainRequestID string               `json:"recipient_custom_domain_request_id,omitempty"`
	SubaddressTag                  string               `json:"subaddress_tag,omitempty"`
	RawSizeBytes                   int64                `json:"raw_size_bytes"`
	ParseState                     string               `json:"parse_state"`
	ParseErrorCode                 string               `json:"parse_error_code,omitempty"`
	HeaderFrom                     string               `json:"header_from,omitempty"`
	HeaderTo                       string               `json:"header_to,omitempty"`
	Subject                        string               `json:"subject,omitempty"`
	MIMEMessageID                  string               `json:"mime_message_id,omitempty"`
	MessageDate                    *time.Time           `json:"message_date,omitempty"`
	AttachmentCount                int64                `json:"attachment_count"`
	AttachmentStorageBytes         int64                `json:"attachment_storage_bytes"`
	RetainedAttachmentStorageBytes int64                `json:"retained_attachment_storage_bytes"`
	PayloadRetentionState          string               `json:"payload_retention_state"`
	SPFResult                      string               `json:"spf_result"`
	DKIMResult                     string               `json:"dkim_result"`
	DMARCResult                    string               `json:"dmarc_result"`
	SpamVerdict                    string               `json:"spam_verdict"`
	SenderVerificationState        string               `json:"sender_verification_state"`
	PossibleDuplicate              bool                 `json:"possible_duplicate"`
	PossibleDuplicateOfMessage     string               `json:"possible_duplicate_of_message_id,omitempty"`
	ReceivedAt                     time.Time            `json:"received_at"`
	CreatedAt                      time.Time            `json:"created_at"`
	Folder                         string               `json:"folder"`
	DeliveredAt                    time.Time            `json:"delivered_at"`
	ReadState                      AgentEmailReadState  `json:"read_state"`
	Processing                     AgentEmailProcessing `json:"processing"`
	Text                           string               `json:"text,omitempty"`
	TextKind                       string               `json:"text_kind,omitempty"`
}

// AgentEmailStorageStatus is the value-free account-wide storage posture.
type AgentEmailStorageStatus struct {
	MaximumRawBytes    int64             `json:"maximum_raw_bytes"`
	AttachmentCapacity MemoryLimitStatus `json:"attachment_capacity"`
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
	// Enabled is nil when talking to a pre-entitlement server. Explicit false
	// means the account has inbound email disabled and clients should stop
	// polling without reinstalling or removing their email tools.
	Enabled           *bool  `json:"enabled,omitempty"`
	Pending           bool   `json:"pending"`
	Unavailable       bool   `json:"unavailable,omitempty"`
	MailboxPending    bool   `json:"mailbox_pending,omitempty"`
	ReceiveState      string `json:"receive_state,omitempty"`
	AgentReceiveState string `json:"agent_receive_state,omitempty"`
	RealmReceiveState string `json:"realm_receive_state,omitempty"`
}

// AgentEmailRetryCanaryRequest carries one secret-like ephemeral challenge in
// a POST body. The challenge is never echoed by the server.
type AgentEmailRetryCanaryRequest struct {
	Challenge string `json:"challenge"`
}

// AgentEmailRetryCanaryCheckpoint is cumulative value-free proof that the
// edge provider observed a temporary result and retried the identical body.
type AgentEmailRetryCanaryCheckpoint struct {
	State         string `json:"state"`
	Armed         bool   `json:"armed"`
	Tempfailed    bool   `json:"tempfailed"`
	Accepted      bool   `json:"accepted"`
	TempfailCount int64  `json:"tempfail_count"`
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
		case errors.Is(err, ErrAgentEmailAttachmentOmitted):
			writeAgentEmailVerdict(w, http.StatusOK, "accepted")
		case errors.Is(err, ErrAgentEmailRawSizeExceeded):
			writeAgentEmailVerdict(w, http.StatusRequestEntityTooLarge, "over_size")
		case errors.Is(err, ErrAgentEmailFeatureDisabled):
			writeAgentEmailVerdict(w, http.StatusOK, "feature_disabled")
		case errors.Is(err, ErrAgentEmailUnknownRecipient), errors.Is(err, ErrNotFound):
			writeAgentEmailVerdict(w, http.StatusNotFound, "unknown_recipient")
		case errors.Is(err, ErrAgentEmailReceiveDisabled):
			writeAgentEmailVerdict(w, http.StatusServiceUnavailable, "receive_disabled")
		case errors.Is(err, ErrAgentEmailRateLimited):
			retryAfter := time.Minute
			var detail *AgentEmailRateLimitError
			if errors.As(err, &detail) && detail != nil {
				if !detail.Retryable {
					writeAgentEmailVerdict(w, http.StatusGone, "permanent")
					return
				}
				if detail.RetryAfter > 0 {
					retryAfter = detail.RetryAfter
				}
			}
			retrySeconds := int64((retryAfter + time.Second - 1) / time.Second)
			if retrySeconds < 1 {
				retrySeconds = 1
			}
			if retrySeconds > agentEmailRateRetryMaximumSeconds {
				retrySeconds = agentEmailRateRetryMaximumSeconds
			}
			w.Header().Set("Retry-After", strconv.FormatInt(retrySeconds, 10))
			writeAgentEmailVerdict(w, http.StatusTooManyRequests, "rate_limited")
		case errors.Is(err, ErrAgentEmailRetryCanaryTemporary):
			writeAgentEmailVerdict(w, http.StatusServiceUnavailable, "temporary")
		case errors.Is(err, ErrAgentEmailRetryCanaryPermanent):
			writeAgentEmailVerdict(w, http.StatusGone, "retry_canary_rejected")
		case errors.Is(err, ErrAgentEmailPilotUnavailable), errors.Is(err, ErrForbidden):
			writeAgentEmailVerdict(w, http.StatusServiceUnavailable, "temporary")
		default:
			writeAgentEmailVerdict(w, http.StatusServiceUnavailable, "temporary")
		}
	}
}

func agentEmailStorageStatusHandler(
	auth PrincipalAuthFunc,
	pilot AgentEmailPilotConfig,
	requireEntitlement func(context.Context, DomainPrincipal) error,
	status func(context.Context, DomainPrincipal) (AgentEmailStorageStatus, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailReadPrincipal(
		auth, pilot, requireEntitlement,
		func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
			value, err := status(r.Context(), p)
			if writeAgentEmailOwnerError(
				w, err, "could not read agent-email storage capacity",
			) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version":      "witself.v0",
				"maximum_raw_bytes":   value.MaximumRawBytes,
				"attachment_capacity": value.AttachmentCapacity,
			})
		},
	))
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
	requireEntitlement func(context.Context, DomainPrincipal) error,
	get func(context.Context, DomainPrincipal) (AgentEmailAddress, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailReadPrincipal(auth, pilot, requireEntitlement, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		address, err := get(r.Context(), p)
		if writeAgentEmailOwnerError(w, err, "could not get agent email address") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "address": address})
	}))
}

func getAgentEmailReceiveControlHandler(
	auth AuthFunc,
	get func(context.Context, string, string, string) (AgentEmailReceiveControl, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		if !allowAgentEmailReceiveControlStatus(w, p.accountStatus, "") {
			return
		}
		if len(r.URL.Query()) != 0 {
			writeJSONError(w, http.StatusBadRequest, "email receive control does not accept query parameters")
			return
		}
		agentID := strings.TrimSpace(r.PathValue("agent"))
		if agentID == "" {
			writeJSONError(w, http.StatusBadRequest, "agent id is required")
			return
		}
		control, err := get(r.Context(), p.accountID, p.operatorID, agentID)
		if writeAgentEmailOwnerError(w, err, "could not get agent email receive control") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "control": control,
		})
	}))
}

func setAgentEmailReceiveControlHandler(
	auth AuthFunc,
	set func(context.Context, string, string, string, string) (AgentEmailReceiveControl, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		if len(r.URL.Query()) != 0 {
			writeJSONError(w, http.StatusBadRequest, "email receive control does not accept query parameters")
			return
		}
		agentID := strings.TrimSpace(r.PathValue("agent"))
		if agentID == "" {
			writeJSONError(w, http.StatusBadRequest, "agent id is required")
			return
		}
		var req SetAgentEmailReceiveControlRequest
		if err := decodeStrictAgentEmailJSON(w, r, &req, 16*1024); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.ReceiveState != "enabled" && req.ReceiveState != "disabled" {
			writeJSONError(w, http.StatusBadRequest, "receive_state must be enabled or disabled")
			return
		}
		if !allowAgentEmailReceiveControlStatus(w, p.accountStatus, req.ReceiveState) {
			return
		}
		control, err := set(r.Context(), p.accountID, p.operatorID, agentID, req.ReceiveState)
		if writeAgentEmailOwnerError(w, err, "could not set agent email receive control") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "control": control,
		})
	}))
}

func getRealmAgentEmailReceiveControlHandler(
	auth AuthFunc,
	get func(context.Context, string, string, string) (AgentEmailRealmReceiveControl, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		if !allowAgentEmailReceiveControlStatus(w, p.accountStatus, "") {
			return
		}
		if len(r.URL.Query()) != 0 {
			writeJSONError(w, http.StatusBadRequest, "email receive control does not accept query parameters")
			return
		}
		realmID := strings.TrimSpace(r.PathValue("realm"))
		if realmID == "" {
			writeJSONError(w, http.StatusBadRequest, "realm id is required")
			return
		}
		control, err := get(r.Context(), p.accountID, p.operatorID, realmID)
		if writeAgentEmailOwnerError(w, err, "could not get realm email receive control") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "control": control,
		})
	}))
}

func setRealmAgentEmailReceiveControlHandler(
	auth AuthFunc,
	set func(context.Context, string, string, string, string) (AgentEmailRealmReceiveControl, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		if len(r.URL.Query()) != 0 {
			writeJSONError(w, http.StatusBadRequest, "email receive control does not accept query parameters")
			return
		}
		realmID := strings.TrimSpace(r.PathValue("realm"))
		if realmID == "" {
			writeJSONError(w, http.StatusBadRequest, "realm id is required")
			return
		}
		var req SetAgentEmailReceiveControlRequest
		if err := decodeStrictAgentEmailJSON(w, r, &req, 16*1024); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.ReceiveState != "enabled" && req.ReceiveState != "disabled" {
			writeJSONError(w, http.StatusBadRequest, "receive_state must be enabled or disabled")
			return
		}
		if !allowAgentEmailReceiveControlStatus(w, p.accountStatus, req.ReceiveState) {
			return
		}
		control, err := set(r.Context(), p.accountID, p.operatorID, realmID, req.ReceiveState)
		if writeAgentEmailOwnerError(w, err, "could not set realm email receive control") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "control": control,
		})
	}))
}

func allowAgentEmailReceiveControlStatus(w http.ResponseWriter, accountStatus, desiredState string) bool {
	if accountStatus == "active" ||
		accountStatus == "suspended" && (desiredState == "" || desiredState == "disabled") {
		return true
	}
	if desiredState == "enabled" {
		writeJSONError(w, http.StatusForbidden, "enabling email receive requires an active account")
		return false
	}
	writeJSONError(w, http.StatusForbidden, "email receive control requires an active or suspended account")
	return false
}

func listAgentEmailsHandler(
	auth PrincipalAuthFunc,
	pilot AgentEmailPilotConfig,
	requireEntitlement func(context.Context, DomainPrincipal) error,
	list func(context.Context, DomainPrincipal, AgentEmailListOptions) (AgentEmailPage, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailReadPrincipal(auth, pilot, requireEntitlement, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
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
	requireEntitlement func(context.Context, DomainPrincipal) error,
	list func(context.Context, DomainPrincipal, AgentEmailListOptions) (AgentEmailPage, error),
) http.HandlerFunc {
	limiter := &agentEmailListenLimiter{byAgent: make(map[string]int)}
	return agentEmailNoStore(requireAgentEmailReadPrincipal(auth, pilot, requireEntitlement, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
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
	requireEntitlement func(context.Context, DomainPrincipal) error,
	get func(context.Context, DomainPrincipal) (AgentEmailCheckpoint, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailReadPrincipal(auth, pilot, requireEntitlement, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
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

func agentEmailRetryCanaryHandler(
	auth PrincipalAuthFunc,
	pilot AgentEmailPilotConfig,
	requireEntitlement func(context.Context, DomainPrincipal) error,
	operation func(context.Context, DomainPrincipal, string) (AgentEmailRetryCanaryCheckpoint, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailReadPrincipal(auth, pilot, requireEntitlement, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if len(r.URL.Query()) != 0 {
			writeJSONError(w, http.StatusBadRequest, "retry canary does not accept query parameters")
			return
		}
		if pilot.RetryCanaryAgentID == "" || p.ID != pilot.RetryCanaryAgentID {
			writeJSONError(w, http.StatusForbidden, "agent-email access forbidden")
			return
		}
		var req AgentEmailRetryCanaryRequest
		if err := decodeStrictAgentEmailJSON(w, r, &req, 1024); err != nil ||
			agentemail.ValidateRetryCanaryChallenge(req.Challenge) != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid retry canary body")
			return
		}
		checkpoint, err := operation(r.Context(), p, req.Challenge)
		if writeAgentEmailOwnerError(w, err, "could not advance retry canary") {
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
	requireEntitlement func(context.Context, DomainPrincipal) error,
	read func(context.Context, DomainPrincipal, string) (AgentEmailMessage, error),
	ack func(context.Context, DomainPrincipal, string) (AgentEmailMessage, error),
	codeConsumed func(context.Context, DomainPrincipal, string) (AgentEmailMessage, error),
	claim func(context.Context, DomainPrincipal, string, ClaimAgentEmailRequest) (AgentEmailProcessing, error),
	renew func(context.Context, DomainPrincipal, string, RenewAgentEmailClaimRequest) (AgentEmailProcessing, error),
	release func(context.Context, DomainPrincipal, string, ReleaseAgentEmailClaimRequest) (AgentEmailProcessing, error),
	complete func(context.Context, DomainPrincipal, string, CompleteAgentEmailRequest) (AgentEmailProcessing, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailPrincipal(auth, pilot, requireEntitlement, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
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
	case errors.Is(err, ErrFeatureNotEnabled):
		writeFeatureNotEnabledError(w, err)
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
	case errors.Is(err, ErrFeatureNotEnabled):
		writeFeatureNotEnabledError(w, err)
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
	requireEntitlement func(context.Context, DomainPrincipal) error,
	h func(http.ResponseWriter, *http.Request, DomainPrincipal),
) http.HandlerFunc {
	// The current immutable token model has only full and curator profiles. A
	// full agent credential carries the read tier; every curator profile remains
	// denied by requireDomainPrincipal. Processing uses a distinct wrapper below
	// so a future scoped-token migration can split the tiers without route churn.
	return requireAgentEmailPrincipal(auth, pilot, requireEntitlement, h)
}

func requireAgentEmailPrincipal(
	auth PrincipalAuthFunc,
	pilot AgentEmailPilotConfig,
	requireEntitlement func(context.Context, DomainPrincipal) error,
	h func(http.ResponseWriter, *http.Request, DomainPrincipal),
) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may access email")
			return
		}
		if requireEntitlement != nil {
			if err := requireEntitlement(r.Context(), p); writeAgentEmailOwnerError(
				w, err, "could not check agent-email entitlement",
			) {
				return
			}
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
			r.URL.Path == "/v1/email:status" ||
			strings.HasPrefix(r.URL.Path, "/v1/email/") ||
			r.URL.Path == "/v1/internal/agent-email:ingest" {
			w.Header().Set("Cache-Control", "private, no-store")
		}
		next.ServeHTTP(w, r)
	})
}
