package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/plans"
)

const (
	// AgentEmailSendEnabled is the enabled value for agent and realm controls.
	AgentEmailSendEnabled = "enabled"
	// AgentEmailSendDisabled is the disabled value for agent and realm controls.
	AgentEmailSendDisabled = "disabled"

	// AgentEmailOutboundQueued is ready for a worker claim.
	AgentEmailOutboundQueued = "queued"
	// AgentEmailOutboundClaimed is claimed before the provider boundary.
	AgentEmailOutboundClaimed = "claimed"
	// AgentEmailOutboundProviderStarted has crossed the no-blind-retry boundary.
	AgentEmailOutboundProviderStarted = "provider_started"
	// AgentEmailOutboundAccepted records known provider acceptance.
	AgentEmailOutboundAccepted = "accepted"
	// AgentEmailOutboundDelivered records delivery to the recipient server.
	AgentEmailOutboundDelivered = "delivered"
	// AgentEmailOutboundDeferred records temporary provider deferral.
	AgentEmailOutboundDeferred = "deferred"
	// AgentEmailOutboundBounced records a permanent recipient bounce.
	AgentEmailOutboundBounced = "bounced"
	// AgentEmailOutboundRejected records a known terminal rejection.
	AgentEmailOutboundRejected = "rejected"
	// AgentEmailOutboundFailed records a terminal provider failure.
	AgentEmailOutboundFailed = "failed"
	// AgentEmailOutboundAmbiguous records an uncertain provider outcome.
	AgentEmailOutboundAmbiguous = "ambiguous"
	// AgentEmailOutboundCanceled records a pre-provider policy cancellation.
	AgentEmailOutboundCanceled = "canceled"

	// AgentEmailOutboundRequestDirect is a caller-addressed new email.
	AgentEmailOutboundRequestDirect = "direct"
	// AgentEmailOutboundRequestReply is a server-derived reply to inbound mail.
	AgentEmailOutboundRequestReply = "reply"

	// AgentEmailOutboundCloudflareProvider is the normalized managed provider
	// recorded by dispatch settlement and provider-event processing.
	AgentEmailOutboundCloudflareProvider = "cloudflare_email_sending"

	// AgentEmailOutboundErrorProviderUnavailable is a closed provider outage code.
	AgentEmailOutboundErrorProviderUnavailable = "provider_unavailable"
	// AgentEmailOutboundErrorProviderRateLimited is a closed provider throttle code.
	AgentEmailOutboundErrorProviderRateLimited = "provider_rate_limited"
	// AgentEmailOutboundErrorProviderRejected is a closed provider rejection code.
	AgentEmailOutboundErrorProviderRejected = "provider_rejected"
	// AgentEmailOutboundErrorProviderFailed is a closed provider failure code.
	AgentEmailOutboundErrorProviderFailed = "provider_failed"
	// AgentEmailOutboundErrorProviderTimeout is a closed transport-timeout code.
	AgentEmailOutboundErrorProviderTimeout = "provider_timeout"
	// AgentEmailOutboundErrorProviderConnectionReset is a closed transport-reset code.
	AgentEmailOutboundErrorProviderConnectionReset = "provider_connection_reset"
	// AgentEmailOutboundErrorProviderResponseInvalid is a closed response error code.
	AgentEmailOutboundErrorProviderResponseInvalid = "provider_response_invalid"
	// AgentEmailOutboundErrorRecipientHardBounce is a permanent bounce code.
	AgentEmailOutboundErrorRecipientHardBounce = "recipient_hard_bounce"
	// AgentEmailOutboundErrorRecipientComplained is a complaint code.
	AgentEmailOutboundErrorRecipientComplained = "recipient_complained"
	// AgentEmailOutboundErrorDispatchCanceled is a policy-cancellation code.
	AgentEmailOutboundErrorDispatchCanceled = "dispatch_canceled"
	// AgentEmailOutboundErrorWorkerLeaseExpired is a worker expiry code.
	AgentEmailOutboundErrorWorkerLeaseExpired = "worker_lease_expired"

	agentEmailOutboundCanonicalDomain = "witmail.net"
	agentEmailOutboundSendingDomain   = "send.witmail.net"

	maximumAgentEmailOutboundSubjectBytes          = 4 * 1024
	maximumAgentEmailOutboundTextBytes             = 256 * 1024
	maximumAgentEmailOutboundKeyBytes              = 512
	maximumAgentEmailOutboundProviderIDBytes       = 512
	maximumAgentEmailOutboundProviderBytes         = 64
	defaultAgentEmailOutboundPageSize              = 50
	maximumAgentEmailOutboundPageSize              = 100
	maximumAgentEmailOutboundGeneration      int64 = 4611686018427387903
)

var (
	// ErrAgentEmailOutboundInputInvalid reports malformed outbound input.
	ErrAgentEmailOutboundInputInvalid = errors.New("invalid outbound agent-email input")
	// ErrAgentEmailOutboundForbidden reports an unauthorized principal.
	ErrAgentEmailOutboundForbidden = errors.New("outbound agent-email access forbidden")
	// ErrAgentEmailOutboundNotFound reports an owner-invisible send.
	ErrAgentEmailOutboundNotFound = errors.New("outbound agent email not found")
	// ErrAgentEmailOutboundConflict reports a conflicting immutable request.
	ErrAgentEmailOutboundConflict = errors.New("outbound agent-email conflict")
	// ErrAgentEmailOutboundEmpty reports that no send is claimable.
	ErrAgentEmailOutboundEmpty = errors.New("no outbound agent email is ready")
	// ErrAgentEmailOutboundClaimLost reports a stale worker fence.
	ErrAgentEmailOutboundClaimLost = errors.New("outbound agent-email claim was lost")
	// ErrAgentEmailSendDisabled reports an operator-disabled scope.
	ErrAgentEmailSendDisabled = errors.New("outbound agent email is disabled")
	// ErrAgentEmailSenderUnavailable reports a missing canonical sender.
	ErrAgentEmailSenderUnavailable = errors.New("canonical outbound agent-email sender is unavailable")
	// ErrAgentEmailReplyUnavailable reports unsafe or incomplete reply provenance.
	ErrAgentEmailReplyUnavailable = errors.New("agent email cannot be replied to")
	// ErrAgentEmailRecipientSuppressed reports a local reputation suppression.
	ErrAgentEmailRecipientSuppressed = errors.New("outbound agent-email recipient is suppressed")
	// ErrAgentEmailOutboundCursorInvalid reports a malformed list cursor.
	ErrAgentEmailOutboundCursorInvalid = errors.New("malformed outbound agent-email cursor")
)

var agentEmailOutboundErrorCodes = map[string]bool{
	AgentEmailOutboundErrorProviderUnavailable:     true,
	AgentEmailOutboundErrorProviderRateLimited:     true,
	AgentEmailOutboundErrorProviderRejected:        true,
	AgentEmailOutboundErrorProviderFailed:          true,
	AgentEmailOutboundErrorProviderTimeout:         true,
	AgentEmailOutboundErrorProviderConnectionReset: true,
	AgentEmailOutboundErrorProviderResponseInvalid: true,
	AgentEmailOutboundErrorRecipientHardBounce:     true,
	AgentEmailOutboundErrorRecipientComplained:     true,
	AgentEmailOutboundErrorDispatchCanceled:        true,
	AgentEmailOutboundErrorWorkerLeaseExpired:      true,
}

// AgentEmailOutboundMessage is the content-minimal owner projection of one
// logical send. Text and worker claim capabilities are exposed only through
// AgentEmailOutboundDispatch. ProviderMessageID is retained for store/worker
// correlation but is never serialized by public adapters.
type AgentEmailOutboundMessage struct {
	ID                      string     `json:"id"`
	AccountID               string     `json:"account_id"`
	RealmID                 string     `json:"realm_id"`
	OwnerAgentID            string     `json:"owner_agent_id"`
	FromAddress             string     `json:"from"`
	ReplyToAddress          string     `json:"reply_to"`
	ToAddress               string     `json:"to"`
	Subject                 string     `json:"subject"`
	State                   string     `json:"state"`
	ProviderState           string     `json:"provider_state,omitempty"`
	Provider                string     `json:"provider,omitempty"`
	ProviderMessageID       string     `json:"-"`
	LastErrorCode           string     `json:"error_code,omitempty"`
	RequestKind             string     `json:"request_kind"`
	ReplyToInboundMessageID string     `json:"reply_to_inbound_message_id,omitempty"`
	ThreadKey               string     `json:"thread_key"`
	AttemptCount            int64      `json:"attempt_count"`
	QueuedAt                time.Time  `json:"queued_at"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
	ProviderStartedAt       *time.Time `json:"provider_started_at,omitempty"`
	AcceptedAt              *time.Time `json:"accepted_at,omitempty"`
	DeliveredAt             *time.Time `json:"delivered_at,omitempty"`
	DeferredAt              *time.Time `json:"deferred_at,omitempty"`
	FailedAt                *time.Time `json:"failed_at,omitempty"`
	AmbiguousAt             *time.Time `json:"ambiguous_at,omitempty"`
	CanceledAt              *time.Time `json:"canceled_at,omitempty"`

	addressID       string
	bodyText        string
	inReplyToHeader string
	references      []string
	requestHash     string
	claimID         string
	claimGeneration int64
	leaseExpiresAt  *time.Time
	nextAttemptAt   *time.Time
}

// SendAgentEmailInput is one direct, single-recipient plain-text request.
type SendAgentEmailInput struct {
	To             string
	Subject        string
	Text           string
	IdempotencyKey string
}

// ReplyAgentEmailInput is one server-threaded plain-text reply request.
type ReplyAgentEmailInput struct {
	Text           string
	IdempotencyKey string
}

// AgentEmailOutboundFilter selects a bounded owner-visible sent-mail page.
type AgentEmailOutboundFilter struct {
	State       string
	OldestFirst bool
	Limit       int
	Cursor      string
}

// AgentEmailOutboundPage contains a metadata-only sent-mail page.
type AgentEmailOutboundPage struct {
	Messages   []AgentEmailOutboundMessage `json:"messages"`
	NextCursor string                      `json:"next_cursor,omitempty"`
}

type agentEmailOutboundDraft struct {
	requestKind             string
	to                      string
	subject                 string
	text                    string
	idempotencyKey          string
	replyToInboundMessageID string
	threadKey               string
	inReplyToHeader         string
	references              []string
	requestHash             string
}

// RequireAgentEmailSendEnabled proves the complete authenticated send gate:
// active account, explicit plan feature, live agent, and both independent
// operator controls. It does not provision an address or enqueue work.
func (s *Store) RequireAgentEmailSendEnabled(ctx context.Context, p Principal) error {
	if err := requireAgentEmailOutboundPrincipal(p); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAccountForAgentEmailSend(ctx, tx, p.AccountID); err != nil {
		return err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return mapAgentEmailOutboundPrincipalError(err)
	}
	if err := readAgentEmailSendControlsTx(ctx, tx, p); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// QueueAgentEmail creates one immutable single-recipient plain-text send. From
// and Reply-To are derived from the live canonical address inside the same
// transaction; a caller cannot select an alias or customer domain in v1.
func (s *Store) QueueAgentEmail(
	ctx context.Context,
	p Principal,
	in SendAgentEmailInput,
) (AgentEmailOutboundMessage, error) {
	if err := requireAgentEmailOutboundPrincipal(p); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	draft, err := normalizeSendAgentEmailInput(in)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	return s.queueAgentEmailDraft(ctx, p, draft)
}

// ReplyAgentEmail derives the external recipient, subject, thread, and safe
// reply headers from an inbound message owned by the authenticated agent.
func (s *Store) ReplyAgentEmail(
	ctx context.Context,
	p Principal,
	inboundMessageID string,
	in ReplyAgentEmailInput,
) (AgentEmailOutboundMessage, error) {
	if err := requireAgentEmailOutboundPrincipal(p); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	inboundMessageID = strings.TrimSpace(inboundMessageID)
	if !validAgentEmailGeneratedID(inboundMessageID, "emsg") {
		return AgentEmailOutboundMessage{}, fmt.Errorf(
			"%w: inbound message id is invalid", ErrAgentEmailOutboundInputInvalid)
	}
	text, key, err := normalizeAgentEmailOutboundTextAndKey(in.Text, in.IdempotencyKey)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	draft := agentEmailOutboundDraft{
		requestKind: AgentEmailOutboundRequestReply,
		text:        text, idempotencyKey: key,
		replyToInboundMessageID: inboundMessageID,
	}
	draft.requestHash = agentEmailOutboundRequestHash(
		draft.requestKind, inboundMessageID, "", text,
	)
	return s.queueAgentEmailDraft(ctx, p, draft)
}

func (s *Store) queueAgentEmailDraft(
	ctx context.Context,
	p Principal,
	draft agentEmailOutboundDraft,
) (AgentEmailOutboundMessage, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	limits, err := lockAccountForAgentEmailSend(ctx, tx, p.AccountID)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return AgentEmailOutboundMessage{}, mapAgentEmailOutboundPrincipalError(err)
	}
	if err := requireAgentEmailSendControlsTx(ctx, tx, p); err != nil {
		return AgentEmailOutboundMessage{}, err
	}

	keyHash := agentEmailOutboundSHA256(draft.idempotencyKey)
	if existing, found, err := agentEmailOutboundByIdempotencyTx(
		ctx, tx, p, keyHash, false,
	); err != nil {
		return AgentEmailOutboundMessage{}, err
	} else if found {
		if existing.requestHash != draft.requestHash {
			return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return AgentEmailOutboundMessage{}, err
		}
		return redactAgentEmailOutbound(existing), nil
	}

	if draft.requestKind == AgentEmailOutboundRequestReply {
		draft, err = resolveAgentEmailReplyDraftTx(ctx, tx, p, draft)
		if err != nil {
			return AgentEmailOutboundMessage{}, err
		}
	}
	sender, err := resolveAgentEmailOutboundSenderTx(ctx, tx, p)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	if err := requireAgentEmailOutboundRecipientAllowedTx(ctx, tx, p.AccountID, draft.to); err != nil {
		return AgentEmailOutboundMessage{}, err
	}

	sendID, err := id.New("esnd")
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	if draft.requestKind == AgentEmailOutboundRequestDirect {
		draft.threadKey = sendID
	}
	msg, inserted, err := insertAgentEmailOutboundTx(
		ctx, tx, p, sender, sendID, keyHash, draft,
	)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	if !inserted {
		existing, found, findErr := agentEmailOutboundByIdempotencyTx(
			ctx, tx, p, keyHash, false,
		)
		if findErr != nil {
			return AgentEmailOutboundMessage{}, findErr
		}
		if !found || existing.requestHash != draft.requestHash {
			return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return AgentEmailOutboundMessage{}, err
		}
		return redactAgentEmailOutbound(existing), nil
	}
	if err := enforceAgentEmailOutboundRateLimitsTx(
		ctx, tx, p, limits, draft.to,
	); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	return redactAgentEmailOutbound(msg), nil
}

// ListAgentEmailOutbox returns metadata only. Body text and worker/provider
// capabilities never appear in list results.
func (s *Store) ListAgentEmailOutbox(
	ctx context.Context,
	p Principal,
	filter AgentEmailOutboundFilter,
) (AgentEmailOutboundPage, error) {
	if err := requireAgentEmailOutboundPrincipal(p); err != nil {
		return AgentEmailOutboundPage{}, err
	}
	filter, cursorTime, cursorID, err := normalizeAgentEmailOutboundFilter(filter)
	if err != nil {
		return AgentEmailOutboundPage{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailOutboundPage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAccountForAgentEmailSend(ctx, tx, p.AccountID); err != nil {
		return AgentEmailOutboundPage{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return AgentEmailOutboundPage{}, mapAgentEmailOutboundPrincipalError(err)
	}

	query := agentEmailOutboundSelect() + `
		WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3`
	args := []any{p.AccountID, p.RealmID, p.ID}
	if filter.State != "" {
		args = append(args, filter.State)
		query += fmt.Sprintf(" AND state=$%d", len(args))
	}
	if !cursorTime.IsZero() {
		args = append(args, cursorTime, cursorID)
		query += fmt.Sprintf(" AND (created_at,id) < ($%d,$%d)", len(args)-1, len(args))
	}
	if filter.OldestFirst {
		query += " ORDER BY created_at,id"
	} else {
		query += " ORDER BY created_at DESC,id DESC"
	}
	args = append(args, filter.Limit+1)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return AgentEmailOutboundPage{}, fmt.Errorf("list outbound agent email: %w", err)
	}
	defer rows.Close()
	page := AgentEmailOutboundPage{Messages: []AgentEmailOutboundMessage{}}
	for rows.Next() {
		msg, scanErr := scanAgentEmailOutbound(rows)
		if scanErr != nil {
			return AgentEmailOutboundPage{}, fmt.Errorf("scan outbound agent email: %w", scanErr)
		}
		page.Messages = append(page.Messages, redactAgentEmailOutbound(msg))
	}
	if err := rows.Err(); err != nil {
		return AgentEmailOutboundPage{}, err
	}
	if len(page.Messages) > filter.Limit {
		page.Messages = page.Messages[:filter.Limit]
		last := page.Messages[len(page.Messages)-1]
		page.NextCursor = encodeAgentEmailOutboundCursor(last.CreatedAt, last.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailOutboundPage{}, err
	}
	return page, nil
}

// GetAgentEmailOutbound returns one metadata-only send owned by the principal.
func (s *Store) GetAgentEmailOutbound(
	ctx context.Context,
	p Principal,
	sendID string,
) (AgentEmailOutboundMessage, error) {
	if err := requireAgentEmailOutboundPrincipal(p); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	if !validAgentEmailGeneratedID(strings.TrimSpace(sendID), "esnd") {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAccountForAgentEmailSend(ctx, tx, p.AccountID); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	msg, err := agentEmailOutboundByOwnerIDTx(ctx, tx, p, sendID, false)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	return redactAgentEmailOutbound(msg), nil
}

func requireAgentEmailOutboundPrincipal(p Principal) error {
	if p.Kind != PrincipalAgent || p.AccountID == "" || p.RealmID == "" || p.ID == "" ||
		(strings.TrimSpace(p.AccessProfile) != "" && p.AccessProfile != AccessProfileFull) {
		return ErrAgentEmailOutboundForbidden
	}
	return nil
}

func mapAgentEmailOutboundPrincipalError(err error) error {
	if errors.Is(err, ErrAgentNotFound) {
		return ErrAgentEmailOutboundForbidden
	}
	return err
}

func lockAccountForAgentEmailSend(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
) (map[string]int64, error) {
	var status string
	var limitsJSON, featuresJSON []byte
	err := tx.QueryRow(ctx, `
		SELECT status,plan_limits,plan_features
		  FROM accounts WHERE id=$1 FOR SHARE`, accountID).
		Scan(&status, &limitsJSON, &featuresJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock account for outbound agent email: %w", err)
	}
	if status != "active" {
		return nil, ErrAccountNotActive
	}
	var features []string
	if err := json.Unmarshal(featuresJSON, &features); err != nil {
		return nil, fmt.Errorf("decode outbound agent-email features: %w", err)
	}
	if !slices.Contains(features, plans.AgentEmailSendFeature) {
		return nil, &FeatureNotEnabledError{Feature: plans.AgentEmailSendFeature}
	}
	limits := map[string]int64{}
	if err := json.Unmarshal(limitsJSON, &limits); err != nil {
		return nil, fmt.Errorf("decode outbound agent-email limits: %w", err)
	}
	return limits, nil
}

type agentEmailOutboundSender struct {
	addressID string
	from      string
	replyTo   string
}

func resolveAgentEmailOutboundSenderTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
) (agentEmailOutboundSender, error) {
	var sender agentEmailOutboundSender
	var localPart string
	err := tx.QueryRow(ctx, `
		SELECT address.id,route.local_part
		  FROM agent_email_addresses address
		  JOIN agent_email_mailboxes mailbox
		    ON mailbox.account_id=address.account_id
		   AND mailbox.realm_id=address.realm_id
		   AND mailbox.owner_agent_id=address.provisioned_agent_id
		   AND mailbox.address_id=address.id
		  JOIN agent_email_address_domains route
		    ON route.account_id=address.account_id
		   AND route.realm_id=address.realm_id
		   AND route.provisioned_agent_id=address.provisioned_agent_id
		   AND route.address_id=address.id
		  JOIN realms realm
		    ON realm.account_id=address.account_id AND realm.id=address.realm_id
		 WHERE address.account_id=$1 AND address.realm_id=$2
		   AND address.provisioned_agent_id=$3 AND route.domain=$4
		   AND address.retired_at IS NULL AND mailbox.retired_at IS NULL
		   AND realm.deleted_at IS NULL AND realm.email_route_state='live'
		 FOR SHARE OF address,mailbox,route,realm`,
		p.AccountID, p.RealmID, p.ID, agentEmailOutboundCanonicalDomain).
		Scan(&sender.addressID, &localPart)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentEmailOutboundSender{}, ErrAgentEmailSenderUnavailable
	}
	if err != nil {
		return agentEmailOutboundSender{}, fmt.Errorf("resolve outbound agent-email sender: %w", err)
	}
	sender.replyTo = strings.ToLower(localPart + "@" + agentEmailOutboundCanonicalDomain)
	sender.from = strings.ToLower(localPart + "@" + agentEmailOutboundSendingDomain)
	return sender, nil
}

func resolveAgentEmailReplyDraftTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	draft agentEmailOutboundDraft,
) (agentEmailOutboundDraft, error) {
	var rawMIME []byte
	var headerFrom, subject, mimeMessageID string
	err := tx.QueryRow(ctx, `
			SELECT raw_mime,COALESCE(header_from,''),COALESCE(header_subject,''),
			       COALESCE(mime_message_id,'')
			  FROM agent_email_messages
			 WHERE id=$1 AND account_id=$2 AND realm_id=$3 AND owner_agent_id=$4
			 FOR SHARE`, draft.replyToInboundMessageID,
		p.AccountID, p.RealmID, p.ID).
		Scan(&rawMIME, &headerFrom, &subject, &mimeMessageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentEmailOutboundDraft{}, ErrAgentEmailOutboundNotFound
	}
	if err != nil {
		return agentEmailOutboundDraft{}, fmt.Errorf("resolve inbound agent-email reply: %w", err)
	}
	draft.to, err = resolveAgentEmailOutboundReplyTarget(rawMIME, headerFrom)
	if err != nil {
		return agentEmailOutboundDraft{}, ErrAgentEmailReplyUnavailable
	}
	draft.subject = safeAgentEmailReplySubject(subject)
	draft.threadKey = draft.replyToInboundMessageID
	if validAgentEmailOutboundMessageID(mimeMessageID) {
		draft.inReplyToHeader = mimeMessageID
		draft.references = []string{mimeMessageID}
	}
	return draft, nil
}

func insertAgentEmailOutboundTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	sender agentEmailOutboundSender,
	sendID, keyHash string,
	draft agentEmailOutboundDraft,
) (AgentEmailOutboundMessage, bool, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO agent_email_outbound_messages
		  (id,account_id,realm_id,owner_agent_id,address_id,
		   from_address,reply_to_address,to_address,subject,body_text,
		   request_kind,reply_to_inbound_message_id,thread_key,
		   in_reply_to_header,references_headers,idempotency_key_hash,
		   request_hash,next_attempt_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13,
		        NULLIF($14,''),COALESCE($15,'{}'::text[]),$16,$17,clock_timestamp())
		ON CONFLICT (account_id,realm_id,owner_agent_id,idempotency_key_hash)
		DO NOTHING
		RETURNING `+agentEmailOutboundReturningColumns(),
		sendID, p.AccountID, p.RealmID, p.ID, sender.addressID,
		sender.from, sender.replyTo, draft.to, draft.subject, draft.text,
		draft.requestKind, draft.replyToInboundMessageID, draft.threadKey,
		draft.inReplyToHeader, draft.references, keyHash, draft.requestHash)
	msg, err := scanAgentEmailOutbound(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundMessage{}, false, nil
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, false,
			fmt.Errorf("queue outbound agent email: %w", err)
	}
	return msg, true, nil
}

func agentEmailOutboundByIdempotencyTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	keyHash string,
	lock bool,
) (AgentEmailOutboundMessage, bool, error) {
	query := agentEmailOutboundSelect() + `
		WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		  AND idempotency_key_hash=$4`
	if lock {
		query += " FOR UPDATE"
	}
	msg, err := scanAgentEmailOutbound(tx.QueryRow(
		ctx, query, p.AccountID, p.RealmID, p.ID, keyHash,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundMessage{}, false, nil
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, false,
			fmt.Errorf("read outbound agent-email idempotency receipt: %w", err)
	}
	return msg, true, nil
}

func agentEmailOutboundByOwnerIDTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	sendID string,
	lock bool,
) (AgentEmailOutboundMessage, error) {
	query := agentEmailOutboundSelect() + `
		WHERE id=$1 AND account_id=$2 AND realm_id=$3 AND owner_agent_id=$4`
	if lock {
		query += " FOR UPDATE"
	}
	msg, err := scanAgentEmailOutbound(tx.QueryRow(
		ctx, query, sendID, p.AccountID, p.RealmID, p.ID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundNotFound
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, fmt.Errorf("read outbound agent email: %w", err)
	}
	return msg, nil
}

func agentEmailOutboundSelect() string {
	return `SELECT ` + agentEmailOutboundReturningColumns() + `
		FROM agent_email_outbound_messages`
}

func agentEmailOutboundReturningColumns() string {
	return agentEmailOutboundReturningColumnsQualified("")
}

func agentEmailOutboundReturningColumnsQualified(alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	return prefix + `id,` + prefix + `account_id,` + prefix + `realm_id,` +
		prefix + `owner_agent_id,` + prefix + `address_id,` +
		prefix + `from_address,` + prefix + `reply_to_address,` +
		prefix + `to_address,` + prefix + `subject,` + prefix + `body_text,` +
		prefix + `request_kind,COALESCE(` + prefix + `reply_to_inbound_message_id,''),` +
		prefix + `thread_key,COALESCE(` + prefix + `in_reply_to_header,''),` +
		prefix + `references_headers,` + prefix + `request_hash,` +
		prefix + `state,` + prefix + `provider_state,` + prefix + `provider,` +
		prefix + `provider_message_id,` + prefix + `last_error_code,` +
		prefix + `attempt_count,` + prefix + `claim_generation,COALESCE(` +
		prefix + `claim_id,''),` + prefix + `lease_expires_at,` +
		prefix + `next_attempt_at,` + prefix + `queued_at,` +
		prefix + `provider_started_at,` + prefix + `accepted_at,` +
		prefix + `delivered_at,` + prefix + `deferred_at,` +
		prefix + `failed_at,` + prefix + `ambiguous_at,` +
		prefix + `canceled_at,` + prefix + `created_at,` + prefix + `updated_at`
}

func scanAgentEmailOutbound(row rowScanner) (AgentEmailOutboundMessage, error) {
	var msg AgentEmailOutboundMessage
	err := row.Scan(
		&msg.ID, &msg.AccountID, &msg.RealmID, &msg.OwnerAgentID, &msg.addressID,
		&msg.FromAddress, &msg.ReplyToAddress, &msg.ToAddress, &msg.Subject,
		&msg.bodyText, &msg.RequestKind, &msg.ReplyToInboundMessageID,
		&msg.ThreadKey, &msg.inReplyToHeader, &msg.references, &msg.requestHash,
		&msg.State, &msg.ProviderState, &msg.Provider, &msg.ProviderMessageID,
		&msg.LastErrorCode, &msg.AttemptCount, &msg.claimGeneration,
		&msg.claimID, &msg.leaseExpiresAt, &msg.nextAttemptAt, &msg.QueuedAt,
		&msg.ProviderStartedAt, &msg.AcceptedAt, &msg.DeliveredAt,
		&msg.DeferredAt, &msg.FailedAt, &msg.AmbiguousAt, &msg.CanceledAt,
		&msg.CreatedAt, &msg.UpdatedAt,
	)
	return msg, err
}

func redactAgentEmailOutbound(msg AgentEmailOutboundMessage) AgentEmailOutboundMessage {
	msg.ProviderMessageID = ""
	msg.addressID = ""
	msg.bodyText = ""
	msg.inReplyToHeader = ""
	msg.references = nil
	msg.requestHash = ""
	msg.claimID = ""
	msg.claimGeneration = 0
	msg.leaseExpiresAt = nil
	msg.nextAttemptAt = nil
	return msg
}

func normalizeSendAgentEmailInput(in SendAgentEmailInput) (agentEmailOutboundDraft, error) {
	to, err := normalizeAgentEmailOutboundMailbox(in.To)
	if err != nil {
		return agentEmailOutboundDraft{}, fmt.Errorf(
			"%w: recipient is invalid", ErrAgentEmailOutboundInputInvalid)
	}
	subject, err := normalizeAgentEmailOutboundSubject(in.Subject)
	if err != nil {
		return agentEmailOutboundDraft{}, err
	}
	text, key, err := normalizeAgentEmailOutboundTextAndKey(in.Text, in.IdempotencyKey)
	if err != nil {
		return agentEmailOutboundDraft{}, err
	}
	draft := agentEmailOutboundDraft{
		requestKind: AgentEmailOutboundRequestDirect,
		to:          to, subject: subject, text: text, idempotencyKey: key,
	}
	draft.requestHash = agentEmailOutboundRequestHash(
		draft.requestKind, to, subject, text,
	)
	return draft, nil
}

func normalizeAgentEmailOutboundTextAndKey(text, key string) (string, string, error) {
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" ||
		len(text) > maximumAgentEmailOutboundTextBytes || strings.IndexByte(text, 0) >= 0 {
		return "", "", fmt.Errorf(
			"%w: text must be 1-%d bytes of UTF-8 without NUL",
			ErrAgentEmailOutboundInputInvalid, maximumAgentEmailOutboundTextBytes)
	}
	key = strings.TrimSpace(key)
	if key == "" || len(key) > maximumAgentEmailOutboundKeyBytes {
		return "", "", fmt.Errorf(
			"%w: idempotency key must be 1-%d bytes",
			ErrAgentEmailOutboundInputInvalid, maximumAgentEmailOutboundKeyBytes)
	}
	return text, key, nil
}

func normalizeAgentEmailOutboundSubject(subject string) (string, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" || !utf8.ValidString(subject) || len(subject) > maximumAgentEmailOutboundSubjectBytes ||
		strings.ContainsAny(subject, "\r\n") || agentEmailOutboundHasControl(subject) {
		return "", fmt.Errorf(
			"%w: subject exceeds %d bytes or contains controls",
			ErrAgentEmailOutboundInputInvalid, maximumAgentEmailOutboundSubjectBytes)
	}
	return subject, nil
}

func resolveAgentEmailOutboundReplyTarget(rawMIME []byte, headerFrom string) (string, error) {
	replyTo, err := agentemail.ReplyToHeader(rawMIME)
	if err != nil {
		return "", ErrAgentEmailReplyUnavailable
	}
	target := replyTo
	if strings.TrimSpace(target) == "" {
		target = headerFrom
	}
	addresses, err := mail.ParseAddressList(target)
	if err != nil || len(addresses) != 1 {
		return "", ErrAgentEmailReplyUnavailable
	}
	return normalizeAgentEmailOutboundMailbox(addresses[0].Address)
}

func normalizeAgentEmailOutboundMailbox(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 320 || strings.ContainsAny(value, "\r\n<>") {
		return "", ErrAgentEmailOutboundInputInvalid
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || parsed.Address != value || strings.Count(value, "@") != 1 {
		return "", ErrAgentEmailOutboundInputInvalid
	}
	local, domain, _ := strings.Cut(value, "@")
	if local == "" || len(local) > 64 {
		return "", ErrAgentEmailOutboundInputInvalid
	}
	for _, c := range []byte(local) {
		if c < 0x21 || c > 0x7e {
			return "", ErrAgentEmailOutboundInputInvalid
		}
	}
	domain, err = agentemail.ValidateDomain(domain)
	if err != nil {
		return "", ErrAgentEmailOutboundInputInvalid
	}
	return local + "@" + domain, nil
}

func safeAgentEmailReplySubject(subject string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(subject) {
		if unicode.IsControl(r) {
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
	}
	clean := strings.Join(strings.Fields(b.String()), " ")
	if clean == "" {
		clean = "(no subject)"
	}
	if !strings.HasPrefix(strings.ToLower(clean), "re:") {
		clean = "Re: " + clean
	}
	if len(clean) <= maximumAgentEmailOutboundSubjectBytes {
		return clean
	}
	clean = clean[:maximumAgentEmailOutboundSubjectBytes]
	for !utf8.ValidString(clean) {
		clean = clean[:len(clean)-1]
	}
	return strings.TrimSpace(clean)
}

func agentEmailOutboundHasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func validAgentEmailOutboundMessageID(value string) bool {
	if len(value) < 3 || len(value) > 998 || value[0] != '<' || value[len(value)-1] != '>' {
		return false
	}
	for _, c := range []byte(value[1 : len(value)-1]) {
		if c <= 0x20 || c >= 0x7f || c == '<' || c == '>' {
			return false
		}
	}
	return true
}

func agentEmailOutboundRequestHash(kind, target, subject, text string) string {
	canonical, _ := json.Marshal(struct {
		Kind    string `json:"kind"`
		Target  string `json:"target"`
		Subject string `json:"subject"`
		Text    string `json:"text"`
	}{kind, target, subject, text})
	return agentEmailOutboundSHA256(string(canonical))
}

func agentEmailOutboundSHA256(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func normalizeAgentEmailOutboundFilter(
	filter AgentEmailOutboundFilter,
) (AgentEmailOutboundFilter, time.Time, string, error) {
	filter.State = strings.TrimSpace(filter.State)
	if filter.State != "" && !validAgentEmailOutboundState(filter.State) {
		return AgentEmailOutboundFilter{}, time.Time{}, "",
			fmt.Errorf("%w: state is invalid", ErrAgentEmailOutboundInputInvalid)
	}
	if filter.OldestFirst && filter.Cursor != "" {
		return AgentEmailOutboundFilter{}, time.Time{}, "",
			fmt.Errorf("%w: oldest-first does not accept a cursor", ErrAgentEmailOutboundInputInvalid)
	}
	if filter.Limit == 0 {
		filter.Limit = defaultAgentEmailOutboundPageSize
	}
	if filter.Limit < 1 || filter.Limit > maximumAgentEmailOutboundPageSize {
		return AgentEmailOutboundFilter{}, time.Time{}, "",
			fmt.Errorf("%w: limit must be 1-%d", ErrAgentEmailOutboundInputInvalid,
				maximumAgentEmailOutboundPageSize)
	}
	if filter.Cursor == "" {
		return filter, time.Time{}, "", nil
	}
	t, sendID, err := decodeAgentEmailOutboundCursor(filter.Cursor)
	if err != nil {
		return AgentEmailOutboundFilter{}, time.Time{}, "", err
	}
	return filter, t, sendID, nil
}

func validAgentEmailOutboundState(state string) bool {
	switch state {
	case AgentEmailOutboundQueued, AgentEmailOutboundClaimed,
		AgentEmailOutboundProviderStarted, AgentEmailOutboundAccepted,
		AgentEmailOutboundDelivered, AgentEmailOutboundDeferred,
		AgentEmailOutboundBounced, AgentEmailOutboundRejected,
		AgentEmailOutboundFailed, AgentEmailOutboundAmbiguous,
		AgentEmailOutboundCanceled:
		return true
	default:
		return false
	}
}

func encodeAgentEmailOutboundCursor(createdAt time.Time, sendID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(
		strconv.FormatInt(createdAt.UTC().UnixNano(), 10) + "\x00" + sendID,
	))
}

func decodeAgentEmailOutboundCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) > 256 {
		return time.Time{}, "", ErrAgentEmailOutboundCursorInvalid
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 2 || !validAgentEmailGeneratedID(parts[1], "esnd") {
		return time.Time{}, "", ErrAgentEmailOutboundCursorInvalid
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", ErrAgentEmailOutboundCursorInvalid
	}
	return time.Unix(0, nanos).UTC(), parts[1], nil
}

func normalizeAgentEmailOutboundErrorCode(code string) (string, error) {
	code = strings.TrimSpace(code)
	if !agentEmailOutboundErrorCodes[code] {
		return "", fmt.Errorf("%w: error code is invalid", ErrAgentEmailOutboundInputInvalid)
	}
	return code, nil
}
