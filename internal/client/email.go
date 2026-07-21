package client

import (
	"context"
	"encoding/json"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

// AgentEmailAddress is the authenticated agent's one pilot receive address.
type AgentEmailAddress struct {
	ID                string     `json:"id"`
	MailboxID         string     `json:"mailbox_id"`
	OwnerAgentID      string     `json:"owner_agent_id"`
	Address           string     `json:"address"`
	Domain            string     `json:"domain"`
	LocalPart         string     `json:"local_part"`
	AgentSegment      string     `json:"agent_segment"`
	RealmLabel        string     `json:"realm_label"`
	ProvisioningKind  string     `json:"provisioning_kind"`
	ReceiveState      string     `json:"receive_state"`
	AgentReceiveState string     `json:"agent_receive_state"`
	RealmReceiveState string     `json:"realm_receive_state"`
	RowVersion        int64      `json:"row_version"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	DisabledAt        *time.Time `json:"disabled_at,omitempty"`
	RealmDisabledAt   *time.Time `json:"realm_disabled_at,omitempty"`
	RetiredAt         *time.Time `json:"retired_at,omitempty"`
}

// AgentEmailReceiveControl is an operator-visible value-free mailbox switch.
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

// AgentEmailRealmReceiveControl is an operator-visible realm kill switch.
type AgentEmailRealmReceiveControl struct {
	AccountID    string     `json:"account_id"`
	RealmID      string     `json:"realm_id"`
	ReceiveState string     `json:"receive_state"`
	MailboxCount int64      `json:"mailbox_count"`
	RowVersion   int64      `json:"row_version"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DisabledAt   *time.Time `json:"disabled_at,omitempty"`
}

// AgentEmailReadState records independent read, acknowledgement, and
// single-use code-consumption transitions for one owner delivery.
type AgentEmailReadState struct {
	State          string     `json:"state"`
	ReadAt         *time.Time `json:"read_at,omitempty"`
	AckedAt        *time.Time `json:"acked_at,omitempty"`
	CodeConsumedAt *time.Time `json:"code_consumed_at,omitempty"`
}

// AgentEmailProcessing is the exact processing fence on lifecycle actions.
// List and read projections intentionally omit ClaimID and LeaseExpiresAt.
type AgentEmailProcessing struct {
	State          string     `json:"state"`
	Generation     int64      `json:"generation"`
	FailureCount   int64      `json:"failure_count"`
	ClaimID        string     `json:"claim_id,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// AgentEmailMessage contains metadata plus bounded decoded Text only after an
// explicit read. Sender and content are always unverified, untrusted input.
type AgentEmailMessage struct {
	ID                         string               `json:"id"`
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
	Folder                     string               `json:"folder"`
	DeliveredAt                time.Time            `json:"delivered_at"`
	ReadState                  AgentEmailReadState  `json:"read_state"`
	Processing                 AgentEmailProcessing `json:"processing"`
	Text                       string               `json:"text,omitempty"`
	TextKind                   string               `json:"text_kind,omitempty"`
}

// AgentEmailPage is one metadata-only cursor page from the owner mailbox.
type AgentEmailPage struct {
	Messages   []AgentEmailMessage `json:"messages"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

// AgentEmailListOptions selects one bounded metadata-only mailbox page.
type AgentEmailListOptions struct {
	Unread  bool
	Unacked bool
	Limit   int
	Cursor  string
}

// AgentEmailListenOptions controls the bounded metadata-only wait operation.
type AgentEmailListenOptions struct {
	WaitSeconds *int
	Limit       int
}

// AgentEmailListenResult reports currently pending metadata or a clean timeout.
type AgentEmailListenResult struct {
	Messages []AgentEmailMessage `json:"messages"`
	TimedOut bool                `json:"timed_out"`
}

// ClaimAgentEmailInput requests one idempotent processing lease.
type ClaimAgentEmailInput struct {
	LeaseSeconds   int
	IdempotencyKey string
}

// AgentEmailClaimInput identifies one exact fence for release.
type AgentEmailClaimInput struct {
	ClaimID              string
	Generation           int64
	DeterministicFailure bool
}

// RenewAgentEmailClaimInput identifies and extends one exact live fence.
type RenewAgentEmailClaimInput struct {
	ClaimID      string
	Generation   int64
	LeaseSeconds int
}

// CompleteAgentEmailInput idempotently closes one exact live fence.
type CompleteAgentEmailInput struct {
	ClaimID        string
	Generation     int64
	IdempotencyKey string
}

// ShowAgentEmailAddress returns the authenticated enrolled agent's one address.
func ShowAgentEmailAddress(ctx context.Context, endpoint, token string) (AgentEmailAddress, error) {
	var out struct {
		Address AgentEmailAddress `json:"address"`
	}
	if err := doJSON(ctx, http.MethodGet, agentEmailURL(endpoint)+"/address", token, nil, &out); err != nil {
		return AgentEmailAddress{}, err
	}
	return out.Address, nil
}

// GetAgentEmailReceiveControl returns one account agent's value-free receive
// lifecycle state through the operator surface.
func GetAgentEmailReceiveControl(ctx context.Context, endpoint, token, agentID string) (AgentEmailReceiveControl, error) {
	var out struct {
		Control AgentEmailReceiveControl `json:"control"`
	}
	if err := doJSON(ctx, http.MethodGet, agentEmailAgentReceiveURL(endpoint, agentID), token, nil, &out); err != nil {
		return AgentEmailReceiveControl{}, err
	}
	return out.Control, nil
}

// SetAgentEmailReceiveControl sets only one agent's receive layer.
func SetAgentEmailReceiveControl(ctx context.Context, endpoint, token, agentID, receiveState string) (AgentEmailReceiveControl, error) {
	body, err := json.Marshal(struct {
		ReceiveState string `json:"receive_state"`
	}{ReceiveState: receiveState})
	if err != nil {
		return AgentEmailReceiveControl{}, err
	}
	var out struct {
		Control AgentEmailReceiveControl `json:"control"`
	}
	if err := doJSON(ctx, http.MethodPatch, agentEmailAgentReceiveURL(endpoint, agentID), token, body, &out); err != nil {
		return AgentEmailReceiveControl{}, err
	}
	return out.Control, nil
}

// GetRealmAgentEmailReceiveControl returns one realm's value-free receive
// kill-switch state through the operator surface.
func GetRealmAgentEmailReceiveControl(ctx context.Context, endpoint, token, realmID string) (AgentEmailRealmReceiveControl, error) {
	var out struct {
		Control AgentEmailRealmReceiveControl `json:"control"`
	}
	if err := doJSON(ctx, http.MethodGet, agentEmailRealmReceiveURL(endpoint, realmID), token, nil, &out); err != nil {
		return AgentEmailRealmReceiveControl{}, err
	}
	return out.Control, nil
}

// SetRealmAgentEmailReceiveControl atomically sets the enrolled realm layer.
func SetRealmAgentEmailReceiveControl(ctx context.Context, endpoint, token, realmID, receiveState string) (AgentEmailRealmReceiveControl, error) {
	body, err := json.Marshal(struct {
		ReceiveState string `json:"receive_state"`
	}{ReceiveState: receiveState})
	if err != nil {
		return AgentEmailRealmReceiveControl{}, err
	}
	var out struct {
		Control AgentEmailRealmReceiveControl `json:"control"`
	}
	if err := doJSON(ctx, http.MethodPatch, agentEmailRealmReceiveURL(endpoint, realmID), token, body, &out); err != nil {
		return AgentEmailRealmReceiveControl{}, err
	}
	return out.Control, nil
}

// ListAgentEmails returns a metadata-only page from the authenticated mailbox.
func ListAgentEmails(ctx context.Context, endpoint, token string, opts AgentEmailListOptions) (AgentEmailPage, error) {
	params := neturl.Values{}
	if opts.Unread {
		params.Set("unread", "true")
	}
	if opts.Unacked {
		params.Set("unacked", "true")
	}
	if opts.Limit != 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		params.Set("cursor", opts.Cursor)
	}
	url := agentEmailURL(endpoint)
	if len(params) != 0 {
		url += "?" + params.Encode()
	}
	var out AgentEmailPage
	if err := doJSON(ctx, http.MethodGet, url, token, nil, &out); err != nil {
		return AgentEmailPage{}, err
	}
	if out.Messages == nil {
		out.Messages = []AgentEmailMessage{}
	}
	return out, nil
}

// ListenAgentEmails waits for metadata without reading or acknowledging mail.
func ListenAgentEmails(ctx context.Context, endpoint, token string, opts AgentEmailListenOptions) (AgentEmailListenResult, error) {
	body, err := json.Marshal(struct {
		WaitSeconds *int `json:"wait_seconds,omitempty"`
		Limit       int  `json:"limit,omitempty"`
	}{WaitSeconds: opts.WaitSeconds, Limit: opts.Limit})
	if err != nil {
		return AgentEmailListenResult{}, err
	}
	var out AgentEmailListenResult
	if err := doJSONWithHeadersTimeout(ctx, http.MethodPost, agentEmailURL(endpoint)+":listen", token, nil, body, &out, agentEmailListenTransportTimeout(opts)); err != nil {
		return AgentEmailListenResult{}, err
	}
	if out.Messages == nil {
		out.Messages = []AgentEmailMessage{}
	}
	return out, nil
}

// ReadAgentEmail marks one owned email read and returns bounded untrusted text.
func ReadAgentEmail(ctx context.Context, endpoint, token, messageID string) (AgentEmailMessage, error) {
	return agentEmailMessageAction(ctx, endpoint, token, messageID, "read")
}

// AckAgentEmail records durable owner handling without returning body text.
func AckAgentEmail(ctx context.Context, endpoint, token, messageID string) (AgentEmailMessage, error) {
	return agentEmailMessageAction(ctx, endpoint, token, messageID, "ack")
}

// MarkAgentEmailCodeConsumed records one successful code use without its value.
func MarkAgentEmailCodeConsumed(ctx context.Context, endpoint, token, messageID string) (AgentEmailMessage, error) {
	return agentEmailMessageAction(ctx, endpoint, token, messageID, "code-consumed")
}

// ClaimAgentEmail acquires or replays one bounded owner processing lease.
func ClaimAgentEmail(ctx context.Context, endpoint, token, messageID string, in ClaimAgentEmailInput) (AgentEmailProcessing, error) {
	body, err := json.Marshal(struct {
		LeaseSeconds int `json:"lease_seconds"`
	}{LeaseSeconds: in.LeaseSeconds})
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	headers := map[string]string{}
	if in.IdempotencyKey != "" {
		headers["Idempotency-Key"] = in.IdempotencyKey
	}
	return agentEmailProcessingAction(ctx, endpoint, token, messageID, "claim", headers, body)
}

// RenewAgentEmailClaim extends one exact unexpired processing fence.
func RenewAgentEmailClaim(ctx context.Context, endpoint, token, messageID string, in RenewAgentEmailClaimInput) (AgentEmailProcessing, error) {
	body, err := json.Marshal(struct {
		ClaimID      string `json:"claim_id"`
		Generation   int64  `json:"generation"`
		LeaseSeconds int    `json:"lease_seconds"`
	}{ClaimID: in.ClaimID, Generation: in.Generation, LeaseSeconds: in.LeaseSeconds})
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	return agentEmailProcessingAction(ctx, endpoint, token, messageID, "renew", nil, body)
}

// ReleaseAgentEmailClaim makes one exact claimed email available for retry.
func ReleaseAgentEmailClaim(ctx context.Context, endpoint, token, messageID string, in AgentEmailClaimInput) (AgentEmailProcessing, error) {
	body, err := json.Marshal(struct {
		ClaimID              string `json:"claim_id"`
		Generation           int64  `json:"generation"`
		DeterministicFailure bool   `json:"deterministic_failure,omitempty"`
	}{ClaimID: in.ClaimID, Generation: in.Generation, DeterministicFailure: in.DeterministicFailure})
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	return agentEmailProcessingAction(ctx, endpoint, token, messageID, "release", nil, body)
}

// CompleteAgentEmail idempotently closes one exact claim without a reply.
func CompleteAgentEmail(ctx context.Context, endpoint, token, messageID string, in CompleteAgentEmailInput) (AgentEmailProcessing, error) {
	body, err := json.Marshal(struct {
		ClaimID    string `json:"claim_id"`
		Generation int64  `json:"generation"`
	}{ClaimID: in.ClaimID, Generation: in.Generation})
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	headers := map[string]string{}
	if in.IdempotencyKey != "" {
		headers["Idempotency-Key"] = in.IdempotencyKey
	}
	return agentEmailProcessingAction(ctx, endpoint, token, messageID, "complete", headers, body)
}

func agentEmailMessageAction(ctx context.Context, endpoint, token, messageID, action string) (AgentEmailMessage, error) {
	var out struct {
		Message AgentEmailMessage `json:"message"`
	}
	if err := doJSON(ctx, http.MethodPost, agentEmailActionURL(endpoint, messageID, action), token, []byte(`{}`), &out); err != nil {
		return AgentEmailMessage{}, err
	}
	return out.Message, nil
}

func agentEmailProcessingAction(ctx context.Context, endpoint, token, messageID, action string, headers map[string]string, body []byte) (AgentEmailProcessing, error) {
	var out struct {
		Processing AgentEmailProcessing `json:"processing"`
	}
	if err := doJSONWithHeaders(ctx, http.MethodPost, agentEmailActionURL(endpoint, messageID, action), token, headers, body, &out); err != nil {
		return AgentEmailProcessing{}, err
	}
	return out.Processing, nil
}

func agentEmailActionURL(endpoint, messageID, action string) string {
	return agentEmailURL(endpoint) + "/" + neturl.PathEscape(messageID) + ":" + action
}

func agentEmailURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/email"
}

func agentEmailAgentReceiveURL(endpoint, agentID string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/agents/" + neturl.PathEscape(agentID) + "/email-receive"
}

func agentEmailRealmReceiveURL(endpoint, realmID string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/realms/" + neturl.PathEscape(realmID) + "/email-receive"
}

func agentEmailListenTransportTimeout(opts AgentEmailListenOptions) time.Duration {
	effectiveWaitSeconds := 20
	if opts.WaitSeconds != nil {
		effectiveWaitSeconds = *opts.WaitSeconds
	}
	timeout := 15 * time.Second
	if candidate := time.Duration(effectiveWaitSeconds+5) * time.Second; candidate > timeout {
		timeout = candidate
	}
	return timeout
}
