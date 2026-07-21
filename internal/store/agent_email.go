package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/id"
)

const (
	// AgentEmailReceiveEnabled accepts new deliveries for the mailbox.
	AgentEmailReceiveEnabled = "enabled"
	// AgentEmailReceiveDisabled preserves the mailbox while refusing new deliveries.
	AgentEmailReceiveDisabled = "disabled"
	// AgentEmailReceiveRetired permanently makes the reserved address unroutable.
	AgentEmailReceiveRetired = "retired"

	// AgentEmailFolderInbox is the pilot's visible delivery folder.
	AgentEmailFolderInbox = "inbox"
	// AgentEmailFolderQuarantine is reserved for isolated deliveries.
	AgentEmailFolderQuarantine = "quarantine"

	// AgentEmailParseParsed indicates a successful bounded MIME projection.
	AgentEmailParseParsed = "parsed"
	// AgentEmailParseError indicates a value-free bounded MIME parser failure.
	AgentEmailParseError = "error"

	// AgentEmailProcessingAvailable indicates claimable mailbox work.
	AgentEmailProcessingAvailable = "available"
	// AgentEmailProcessingClaimed indicates work protected by a live claim fence.
	AgentEmailProcessingClaimed = "claimed"
	// AgentEmailProcessingCompleted indicates durably finished mailbox work.
	AgentEmailProcessingCompleted = "completed"

	// AgentEmailSenderUnverified is the mandatory receive-pilot sender posture.
	AgentEmailSenderUnverified = "unverified"

	agentEmailPilotProvider       = "cloudflare_email_routing"
	defaultAgentEmailPageSize     = 50
	maximumAgentEmailPageSize     = 100
	maximumAgentEmailFailureCount = int64(4611686018427387903)
	maximumAgentEmailGeneration   = int64(4611686018427387903)
	defaultAgentEmailLease        = 5 * time.Minute
	minimumAgentEmailLease        = 30 * time.Second
	maximumAgentEmailLease        = 15 * time.Minute
	maximumAgentEmailKeyBytes     = 512
	maximumAgentEmailClaimIDBytes = 128
)

var (
	// ErrAgentEmailInputInvalid reports malformed or out-of-bounds input.
	ErrAgentEmailInputInvalid = errors.New("invalid agent-email input")
	// ErrAgentEmailForbidden reports a principal that may not use agent email.
	ErrAgentEmailForbidden = errors.New("agent-email access forbidden")
	// ErrAgentEmailNotFound hides absent and out-of-scope email messages.
	ErrAgentEmailNotFound = errors.New("agent email not found")
	// ErrAgentEmailAddressMissing reports an enrolled agent without a mailbox.
	ErrAgentEmailAddressMissing = errors.New("agent-email address not provisioned")
	// ErrAgentEmailAddressConflict reports an already-reserved address.
	ErrAgentEmailAddressConflict = errors.New("agent-email address is already reserved")
	// ErrAgentEmailUnknownRecipient reports an unroutable envelope recipient.
	ErrAgentEmailUnknownRecipient = errors.New("unknown agent-email recipient")
	// ErrAgentEmailReceiveDisabled reports a known mailbox that refuses delivery.
	ErrAgentEmailReceiveDisabled = errors.New("agent-email receive is disabled")
	// ErrAgentEmailPilotDisabled reports the process-lifetime default-off gate.
	ErrAgentEmailPilotDisabled = errors.New("agent-email receive pilot is disabled")
	// ErrAgentEmailPilotNotEnrolled reports a principal outside the exact allowlist.
	ErrAgentEmailPilotNotEnrolled = errors.New("agent is not enrolled in the email pilot")
	// ErrAgentEmailBusy reports an email protected by another live processing claim.
	ErrAgentEmailBusy = errors.New("agent email is claimed for processing")
	// ErrAgentEmailClaimLost reports a stale or expired processing fence.
	ErrAgentEmailClaimLost = errors.New("agent-email processing claim was lost")
	// ErrAgentEmailConflict reports an incompatible idempotent state transition.
	ErrAgentEmailConflict = errors.New("agent-email state conflict")
	// ErrAgentEmailCodeConsumed reports repeated use of the single-use code marker.
	ErrAgentEmailCodeConsumed = errors.New("agent-email code was already consumed")
	// ErrAgentEmailCursorInvalid reports an invalid mailbox pagination cursor.
	ErrAgentEmailCursorInvalid = errors.New("malformed agent-email cursor")
)

// AgentEmailAddress is the durable reservation and current mailbox state for
// one agent. Address tombstones intentionally survive permanent agent deletion.
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

// AgentEmailReadState distinguishes explicit content reads, acknowledgement,
// and the single-use code-consumption marker.
type AgentEmailReadState struct {
	State          string     `json:"state"`
	ReadAt         *time.Time `json:"read_at,omitempty"`
	AckedAt        *time.Time `json:"acked_at,omitempty"`
	CodeConsumedAt *time.Time `json:"code_consumed_at,omitempty"`
}

// AgentEmailProcessing is a value-free fenced processing lease. General list
// and read results redact ClaimID and LeaseExpiresAt.
type AgentEmailProcessing struct {
	State          string     `json:"state"`
	Generation     int64      `json:"generation"`
	FailureCount   int64      `json:"failure_count"`
	ClaimID        string     `json:"claim_id,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// AgentEmailMessage is one immutable external message plus owner-only delivery
// state. Text is populated only by ReadAgentEmail and remains untrusted input.
// Raw MIME and attachment bytes are never surfaced by the pilot API.
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
	rawMIME                    []byte
	duplicateGroupSHA256       string
}

// AgentEmailPage is one metadata-only owner mailbox page.
type AgentEmailPage struct {
	Messages   []AgentEmailMessage `json:"messages"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

// AgentEmailFilter selects a bounded metadata-only mailbox page.
type AgentEmailFilter struct {
	Unread      bool
	Unacked     bool
	OldestFirst bool
	Limit       int
	Cursor      string
}

// ClaimAgentEmailInput configures one idempotent processing claim.
type ClaimAgentEmailInput struct {
	LeaseDuration  time.Duration
	IdempotencyKey string
}

// RenewAgentEmailClaimInput identifies and extends one live claim fence.
type RenewAgentEmailClaimInput struct {
	ClaimID       string
	Generation    int64
	LeaseDuration time.Duration
}

// ReleaseAgentEmailClaimInput returns one exact claim to the available state.
type ReleaseAgentEmailClaimInput struct {
	ClaimID              string
	Generation           int64
	DeterministicFailure bool
}

// CompleteAgentEmailInput idempotently completes one exact live claim.
type CompleteAgentEmailInput struct {
	ClaimID        string
	Generation     int64
	IdempotencyKey string
}

// AgentEmailPilotScope is the process-lifetime default-off allowlist. Both a
// mailbox row and these exact realm/agent entries are required for ingestion.
type AgentEmailPilotScope struct {
	Enabled  bool
	Domain   string
	Audience string
	RealmIDs map[string]bool
	AgentIDs map[string]bool
}

// AgentEmailIngestInput carries already signature-verified relay metadata and
// the byte-identical request body.
type AgentEmailIngestInput struct {
	Relay agentemail.RelayMetadata
	Raw   []byte
}

// AgentEmailCheckpoint is a bounded value-free foreground-work hint.
type AgentEmailCheckpoint struct {
	Pending        bool `json:"pending"`
	MailboxPending bool `json:"mailbox_pending,omitempty"`
}

// EnsureAgentEmailMailbox idempotently provisions one pilot mailbox. An
// explicit segment selects the operator-override path; otherwise the settled
// deterministic derivation is applied to the current agent name.
func (s *Store) EnsureAgentEmailMailbox(
	ctx context.Context,
	scope AgentEmailPilotScope,
	accountID, realmID, agentID, explicitSegment string,
) (AgentEmailAddress, error) {
	domain, err := requireAgentEmailPilotEnrollment(scope, realmID, agentID)
	if err != nil {
		return AgentEmailAddress{}, err
	}
	addressID, err := id.New("eaddr")
	if err != nil {
		return AgentEmailAddress{}, err
	}
	mailboxID, err := id.New("emb")
	if err != nil {
		return AgentEmailAddress{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailAddress{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, accountID, false); err != nil {
		return AgentEmailAddress{}, err
	}
	if existing, err := agentEmailAddressForOwnerTx(ctx, tx, accountID, realmID, agentID); err == nil {
		if existing.ReceiveState == AgentEmailReceiveRetired || existing.RetiredAt != nil {
			return AgentEmailAddress{}, ErrAgentNotFound
		}
		if existing.Domain != domain {
			return AgentEmailAddress{}, fmt.Errorf("%w: existing mailbox uses another domain", ErrAgentEmailConflict)
		}
		if err := tx.Commit(ctx); err != nil {
			return AgentEmailAddress{}, err
		}
		return existing, nil
	} else if !errors.Is(err, ErrAgentEmailAddressMissing) {
		return AgentEmailAddress{}, err
	}

	var agentName string
	err = tx.QueryRow(ctx, `
		SELECT a.name
		FROM agents a JOIN realms r ON r.id=a.realm_id
		WHERE a.id=$1 AND a.realm_id=$2 AND r.account_id=$3
		  AND a.deleted_at IS NULL AND r.deleted_at IS NULL
		FOR NO KEY UPDATE OF a`, agentID, realmID, accountID).Scan(&agentName)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailAddress{}, ErrAgentNotFound
	}
	if err != nil {
		return AgentEmailAddress{}, fmt.Errorf("resolve email owner: %w", err)
	}
	provisioningKind := "derived"
	segment := explicitSegment
	if strings.TrimSpace(segment) == "" {
		segment, err = agentemail.DeriveAgentSegment(agentName)
	} else {
		provisioningKind = "operator_override"
		segment, err = agentemail.ValidateAgentSegment(segment)
	}
	if err != nil {
		return AgentEmailAddress{}, fmt.Errorf("%w: %v", ErrAgentEmailInputInvalid, err)
	}
	parts, err := agentemail.ComposeAddress(segment, realmID, domain)
	if err != nil {
		return AgentEmailAddress{}, fmt.Errorf("%w: %v", ErrAgentEmailInputInvalid, err)
	}
	var createdAt, updatedAt time.Time
	err = tx.QueryRow(ctx, `
		WITH inserted_address AS (
		  INSERT INTO agent_email_addresses
		    (id,account_id,realm_id,provisioned_agent_id,domain,agent_segment,
		     realm_label,local_part,provisioning_kind)
		  VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		  RETURNING created_at
		), inserted_mailbox AS (
		  INSERT INTO agent_email_mailboxes
		    (id,account_id,realm_id,owner_agent_id,address_id,receive_state)
		  VALUES ($10,$2,$3,$4,$1,'enabled')
		  RETURNING created_at,updated_at
		)
		SELECT inserted_address.created_at, inserted_mailbox.updated_at
		FROM inserted_address, inserted_mailbox`,
		addressID, accountID, realmID, agentID, parts.Domain, parts.AgentSegment,
		parts.RealmLabel, parts.LocalPart, provisioningKind, mailboxID).
		Scan(&createdAt, &updatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return AgentEmailAddress{}, ErrAgentEmailAddressConflict
		}
		return AgentEmailAddress{}, fmt.Errorf("provision agent-email mailbox: %w", err)
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: accountID, ActorKind: ActorSystem,
		Verb: VerbAgentEmailAddressProvisioned,
		Metadata: map[string]any{
			"address_id": addressID, "mailbox_id": mailboxID,
			"owner_agent_id": agentID, "realm_id": realmID,
		},
	}); err != nil {
		return AgentEmailAddress{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailAddress{}, err
	}
	return AgentEmailAddress{
		ID: addressID, MailboxID: mailboxID, AccountID: accountID, RealmID: realmID,
		OwnerAgentID: agentID, Address: parts.BaseAddress, Domain: parts.Domain,
		LocalPart: parts.LocalPart, AgentSegment: parts.AgentSegment,
		RealmLabel: parts.RealmLabel, ProvisioningKind: provisioningKind,
		ReceiveState: AgentEmailReceiveEnabled, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}, nil
}

// GetAgentEmailAddress returns only the authenticated agent's own address.
func (s *Store) GetAgentEmailAddress(
	ctx context.Context,
	scope AgentEmailPilotScope,
	p Principal,
) (AgentEmailAddress, error) {
	if err := requireAgentEmailPilotPrincipal(scope, p); err != nil {
		return AgentEmailAddress{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailAddress{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return AgentEmailAddress{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return AgentEmailAddress{}, mapAgentEmailPrincipalError(err)
	}
	address, err := agentEmailAddressForOwnerTx(ctx, tx, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return AgentEmailAddress{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailAddress{}, err
	}
	return address, nil
}

// IngestAgentEmailPilot durably stores one signed Cloudflare delivery. Digest
// matches only group suspected duplicates; every pilot invocation inserts a
// new immutable row and delivery.
func (s *Store) IngestAgentEmailPilot(
	ctx context.Context,
	scope AgentEmailPilotScope,
	in AgentEmailIngestInput,
) (AgentEmailMessage, error) {
	if !scope.Enabled {
		return AgentEmailMessage{}, ErrAgentEmailPilotDisabled
	}
	domain, err := normalizeAgentEmailPilotScope(scope)
	if err != nil {
		return AgentEmailMessage{}, err
	}
	relay, err := in.Relay.Normalize()
	if err != nil || len(in.Raw) < 1 || len(in.Raw) > agentemail.PilotMaximumRawBytes {
		return AgentEmailMessage{}, fmt.Errorf("%w: relay metadata or body is invalid", ErrAgentEmailInputInvalid)
	}
	digest := sha256.Sum256(in.Raw)
	rawSHA := hex.EncodeToString(digest[:])
	if relay.RawSize != int64(len(in.Raw)) || relay.RawSHA256 != rawSHA {
		return AgentEmailMessage{}, fmt.Errorf("%w: raw body does not match relay metadata", ErrAgentEmailInputInvalid)
	}
	if relay.Audience != strings.ToLower(strings.TrimSpace(scope.Audience)) {
		return AgentEmailMessage{}, fmt.Errorf("%w: relay audience does not match this cell", ErrAgentEmailInputInvalid)
	}
	parts, err := agentemail.ParseRecipient(relay.EnvelopeRecipient, domain)
	if err != nil {
		return AgentEmailMessage{}, ErrAgentEmailUnknownRecipient
	}
	// Validate the bounded decoded-text projection during ingest as well as on
	// read. Text remains transient here; only its validated MIME metadata is
	// persisted by the receive-only pilot.
	parsed, parseErr := agentemail.ParseMessage(in.Raw, true)
	parseState := AgentEmailParseParsed
	parseErrorCode := ""
	if parseErr != nil {
		parseState = AgentEmailParseError
		parseErrorCode = agentemail.ParseErrorCode(parseErr)
	}
	duplicateGroup := agentEmailDuplicateGroup(rawSHA, parts.Address, relay.EnvelopeSender)
	messageID, err := id.New("emsg")
	if err != nil {
		return AgentEmailMessage{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailMessage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Resolve once without taking a mailbox/address lock so the global write
	// order remains account -> agent -> mailbox/address. Re-read under a share
	// lock after those parent locks are held to close the resolution race.
	candidate, err := agentEmailAddressByRecipientTx(ctx, tx, parts.Domain, parts.LocalPart, false)
	if err != nil {
		return AgentEmailMessage{}, err
	}
	if !scope.RealmIDs[candidate.RealmID] || !scope.AgentIDs[candidate.OwnerAgentID] {
		return AgentEmailMessage{}, ErrAgentEmailPilotNotEnrolled
	}
	if err := lockAccountForMint(ctx, tx, candidate.AccountID, false); err != nil {
		return AgentEmailMessage{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, candidate.AccountID, candidate.RealmID, candidate.OwnerAgentID); err != nil {
		return AgentEmailMessage{}, ErrAgentEmailPilotNotEnrolled
	}
	address, err := agentEmailAddressByRecipientTx(ctx, tx, parts.Domain, parts.LocalPart, true)
	if err != nil {
		return AgentEmailMessage{}, err
	}
	if address.AccountID != candidate.AccountID || address.RealmID != candidate.RealmID ||
		address.OwnerAgentID != candidate.OwnerAgentID || address.ID != candidate.ID {
		return AgentEmailMessage{}, ErrAgentEmailUnknownRecipient
	}
	if !scope.RealmIDs[address.RealmID] || !scope.AgentIDs[address.OwnerAgentID] {
		return AgentEmailMessage{}, ErrAgentEmailPilotNotEnrolled
	}
	if address.ReceiveState != AgentEmailReceiveEnabled {
		return AgentEmailMessage{}, ErrAgentEmailReceiveDisabled
	}
	if address.AgentSegment != parts.AgentSegment || address.RealmLabel != parts.RealmLabel {
		return AgentEmailMessage{}, ErrAgentEmailUnknownRecipient
	}
	lockKey := int64(binary.BigEndian.Uint64(digest[:8]))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return AgentEmailMessage{}, fmt.Errorf("lock email duplicate group: %w", err)
	}
	var possibleDuplicate string
	err = tx.QueryRow(ctx, `
		SELECT id FROM agent_email_messages
		WHERE account_id=$1 AND realm_id=$2 AND mailbox_id=$3
		  AND duplicate_group_sha256=$4
		ORDER BY received_at,id LIMIT 1`, address.AccountID, address.RealmID,
		address.MailboxID, duplicateGroup).Scan(&possibleDuplicate)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailMessage{}, fmt.Errorf("locate suspected email duplicate: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		possibleDuplicate = ""
	}

	var receivedAt, createdAt, deliveredAt time.Time
	err = tx.QueryRow(ctx, `
		WITH inserted_message AS (
		  INSERT INTO agent_email_messages
		    (id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,
		     provider,provider_message_id,envelope_sender,envelope_recipient,
		     agent_segment,realm_label,subaddress_tag,raw_mime,raw_size_bytes,
		     raw_sha256,parse_state,parse_error,header_from,header_to,
		     header_subject,mime_message_id,message_date,attachment_count,
		     spf_result,dkim_result,dmarc_result,spam_verdict,
		     sender_verification_state,duplicate_group_sha256,
		     possible_duplicate_of_message_id,received_at)
		  VALUES
		    ($1,$2,$3,$4,$5,$6,$7,NULL,$8,$9,$10,$11,$12,$13,$14,$15,
		     $16,$17,$18,$19,$20,$21,$22,$23,
		     'unknown','unknown','unknown','unknown','unverified',$24,$25,
		     clock_timestamp())
		  RETURNING received_at,created_at
		), inserted_delivery AS (
		  INSERT INTO agent_email_deliveries
		    (message_id,account_id,realm_id,mailbox_id,owner_agent_id,folder)
		  VALUES ($1,$2,$3,$4,$5,'inbox')
		  RETURNING delivered_at
		)
		SELECT inserted_message.received_at,inserted_message.created_at,
		       inserted_delivery.delivered_at
		FROM inserted_message,inserted_delivery`,
		messageID, address.AccountID, address.RealmID, address.MailboxID,
		address.OwnerAgentID, address.ID, agentEmailPilotProvider,
		relay.EnvelopeSender, parts.Address, parts.AgentSegment, parts.RealmLabel,
		agentEmailNullableString(parts.SubaddressTag), in.Raw, len(in.Raw), rawSHA,
		parseState, agentEmailNullableString(parseErrorCode), agentEmailNullableString(parsed.HeaderFrom),
		agentEmailNullableString(parsed.HeaderTo), agentEmailNullableString(parsed.HeaderSubject),
		agentEmailNullableString(parsed.MIMEMessageID), parsed.MessageDate, parsed.AttachmentCount,
		duplicateGroup, agentEmailNullableString(possibleDuplicate)).
		Scan(&receivedAt, &createdAt, &deliveredAt)
	if err != nil {
		return AgentEmailMessage{}, fmt.Errorf("store agent email: %w", err)
	}
	msg := AgentEmailMessage{
		ID: messageID, AccountID: address.AccountID, RealmID: address.RealmID,
		MailboxID: address.MailboxID, OwnerAgentID: address.OwnerAgentID,
		AddressID: address.ID, Provider: agentEmailPilotProvider,
		EnvelopeSender: relay.EnvelopeSender, EnvelopeRecipient: parts.Address,
		AgentSegment: parts.AgentSegment, RealmLabel: parts.RealmLabel,
		SubaddressTag: parts.SubaddressTag, RawSizeBytes: int64(len(in.Raw)),
		ParseState: parseState, ParseErrorCode: parseErrorCode,
		HeaderFrom: parsed.HeaderFrom, HeaderTo: parsed.HeaderTo,
		Subject: parsed.HeaderSubject, MIMEMessageID: parsed.MIMEMessageID,
		MessageDate: parsed.MessageDate, AttachmentCount: parsed.AttachmentCount,
		SPFResult: "unknown", DKIMResult: "unknown", DMARCResult: "unknown",
		SpamVerdict: "unknown", SenderVerificationState: AgentEmailSenderUnverified,
		PossibleDuplicate:          possibleDuplicate != "",
		PossibleDuplicateOfMessage: possibleDuplicate,
		ReceivedAt:                 receivedAt, CreatedAt: createdAt, Folder: AgentEmailFolderInbox,
		DeliveredAt: deliveredAt, ReadState: AgentEmailReadState{State: MessageReadUnread},
		Processing:           AgentEmailProcessing{State: AgentEmailProcessingAvailable},
		duplicateGroupSHA256: duplicateGroup,
	}
	if err := logAgentEmailEvent(ctx, tx, VerbAgentEmailReceived, ActorSystem, "", msg, false); err != nil {
		return AgentEmailMessage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailMessage{}, err
	}
	return msg, nil
}

// ListAgentEmails returns metadata only and never marks a delivery read.
func (s *Store) ListAgentEmails(
	ctx context.Context,
	scope AgentEmailPilotScope,
	p Principal,
	filter AgentEmailFilter,
) (AgentEmailPage, error) {
	if err := requireAgentEmailPilotPrincipal(scope, p); err != nil {
		return AgentEmailPage{}, err
	}
	filter, cursorTime, cursorID, err := normalizeAgentEmailFilter(filter)
	if err != nil {
		return AgentEmailPage{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailPage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return AgentEmailPage{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return AgentEmailPage{}, mapAgentEmailPrincipalError(err)
	}
	args := []any{p.AccountID, p.RealmID, p.ID}
	q := &strings.Builder{}
	q.WriteString(agentEmailSelect(false))
	q.WriteString(` WHERE d.account_id=$1 AND d.realm_id=$2 AND d.owner_agent_id=$3
		AND d.folder='inbox'`)
	if filter.Unread {
		q.WriteString(` AND d.read_at IS NULL`)
	}
	if filter.Unacked {
		q.WriteString(` AND d.acked_at IS NULL`)
	}
	if !cursorTime.IsZero() {
		args = append(args, cursorTime, cursorID)
		comparison := "<"
		if filter.OldestFirst {
			comparison = ">"
		}
		fmt.Fprintf(q, ` AND (m.received_at,m.id) %s ($%d,$%d)`, comparison, len(args)-1, len(args))
	}
	args = append(args, filter.Limit+1)
	order := "DESC"
	if filter.OldestFirst {
		order = "ASC"
	}
	fmt.Fprintf(q, ` ORDER BY m.received_at %s,m.id %s LIMIT $%d`, order, order, len(args))
	rows, err := tx.Query(ctx, q.String(), args...)
	if err != nil {
		return AgentEmailPage{}, fmt.Errorf("list agent emails: %w", err)
	}
	defer rows.Close()
	messages := make([]AgentEmailMessage, 0, filter.Limit)
	for rows.Next() {
		msg, err := scanAgentEmail(rows)
		if err != nil {
			return AgentEmailPage{}, err
		}
		messages = append(messages, redactAgentEmailFence(msg))
	}
	if err := rows.Err(); err != nil {
		return AgentEmailPage{}, err
	}
	var next string
	if len(messages) > filter.Limit {
		last := messages[filter.Limit-1]
		next = encodeAgentEmailCursor(last.ReceivedAt, last.ID)
		messages = messages[:filter.Limit]
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailPage{}, err
	}
	return AgentEmailPage{Messages: messages, NextCursor: next}, nil
}

// ReadAgentEmail explicitly crosses the untrusted-content boundary, marks the
// delivery read, and returns only bounded decoded text. Raw MIME, HTML markup,
// and attachment bytes remain unavailable in the pilot.
func (s *Store) ReadAgentEmail(
	ctx context.Context,
	scope AgentEmailPilotScope,
	p Principal,
	messageID string,
) (AgentEmailMessage, error) {
	msg, err := s.transitionAgentEmail(ctx, scope, p, messageID, false, false)
	if err != nil {
		return AgentEmailMessage{}, err
	}
	parsed, parseErr := agentemail.ParseMessage(msg.rawMIME, true)
	msg.rawMIME = nil
	if parseErr == nil {
		msg.Text = parsed.Text
		msg.TextKind = parsed.TextKind
	}
	return redactAgentEmailFence(msg), nil
}

// AckAgentEmail is metadata-only and idempotently marks read and acknowledged.
func (s *Store) AckAgentEmail(
	ctx context.Context,
	scope AgentEmailPilotScope,
	p Principal,
	messageID string,
) (AgentEmailMessage, error) {
	msg, err := s.transitionAgentEmail(ctx, scope, p, messageID, true, false)
	return redactAgentEmailFence(msg), err
}

// MarkAgentEmailCodeConsumed records the one-time client-side extraction/use
// ceremony without storing the code itself. A repeated call is a conflict so
// prompt-injected or accidental re-consumption is visible.
func (s *Store) MarkAgentEmailCodeConsumed(
	ctx context.Context,
	scope AgentEmailPilotScope,
	p Principal,
	messageID string,
) (AgentEmailMessage, error) {
	msg, err := s.transitionAgentEmail(ctx, scope, p, messageID, false, true)
	return redactAgentEmailFence(msg), err
}

// GetSelfAgentEmailCheckpoint returns a value-free pending-mail hint.
func (s *Store) GetSelfAgentEmailCheckpoint(
	ctx context.Context,
	scope AgentEmailPilotScope,
	p Principal,
) (AgentEmailCheckpoint, error) {
	if err := requireAgentEmailPilotPrincipal(scope, p); err != nil {
		return AgentEmailCheckpoint{}, err
	}
	var pending bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM agent_email_deliveries d
		  JOIN agent_email_mailboxes mb
		    ON mb.id=d.mailbox_id AND mb.account_id=d.account_id
		   AND mb.realm_id=d.realm_id AND mb.owner_agent_id=d.owner_agent_id
		  JOIN agents a ON a.id=mb.owner_agent_id AND a.realm_id=mb.realm_id
		  JOIN realms r ON r.id=mb.realm_id AND r.account_id=mb.account_id
		  JOIN accounts account ON account.id=mb.account_id
		  WHERE d.account_id=$1 AND d.realm_id=$2 AND d.owner_agent_id=$3
		    AND d.folder='inbox' AND d.acked_at IS NULL
		    AND mb.receive_state<>'retired' AND a.deleted_at IS NULL
		    AND r.deleted_at IS NULL AND account.status='active'
		)`, p.AccountID, p.RealmID, p.ID).Scan(&pending)
	if err != nil {
		return AgentEmailCheckpoint{}, fmt.Errorf("read agent-email checkpoint: %w", err)
	}
	return AgentEmailCheckpoint{Pending: pending, MailboxPending: pending}, nil
}

// ClaimAgentEmail acquires one exact owner-only processing lease. General
// mailbox projections never expose the returned claim capability.
func (s *Store) ClaimAgentEmail(
	ctx context.Context,
	scope AgentEmailPilotScope,
	p Principal,
	messageID string,
	in ClaimAgentEmailInput,
) (AgentEmailProcessing, error) {
	if err := requireAgentEmailPilotPrincipal(scope, p); err != nil {
		return AgentEmailProcessing{}, err
	}
	messageID, err := normalizeAgentEmailMessageID(messageID)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	lease, err := normalizeAgentEmailLease(in.LeaseDuration)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	keyHash, err := normalizeAgentEmailKey(in.IdempotencyKey, "claim")
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return AgentEmailProcessing{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return AgentEmailProcessing{}, mapAgentEmailPrincipalError(err)
	}
	msg, completeKeyHash, claimKeyHash, err := lockAgentEmailProcessingTx(ctx, tx, p, messageID)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	_ = completeKeyHash
	if msg.Processing.State == AgentEmailProcessingCompleted {
		if err := tx.Commit(ctx); err != nil {
			return AgentEmailProcessing{}, err
		}
		return msg.Processing, nil
	}
	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return AgentEmailProcessing{}, err
	}
	if msg.Processing.State == AgentEmailProcessingClaimed &&
		msg.Processing.LeaseExpiresAt != nil && msg.Processing.LeaseExpiresAt.After(databaseNow) {
		if claimKeyHash == keyHash {
			if err := tx.Commit(ctx); err != nil {
				return AgentEmailProcessing{}, err
			}
			return msg.Processing, nil
		}
		return AgentEmailProcessing{}, ErrAgentEmailBusy
	}
	if msg.Processing.Generation >= maximumAgentEmailGeneration {
		return AgentEmailProcessing{}, fmt.Errorf("%w: processing generation exhausted", ErrAgentEmailConflict)
	}
	claimID, err := id.New("ecl")
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	var generation int64
	var leaseExpiresAt time.Time
	err = tx.QueryRow(ctx, `
		UPDATE agent_email_deliveries
		SET processing_state='claimed',
		    processing_generation=processing_generation+1,
		    claim_id=$3,claim_key_hash=$4,
		    lease_expires_at=clock_timestamp()+($5::double precision * interval '1 second'),
		    completed_at=NULL,complete_key_hash=''
		WHERE message_id=$1 AND mailbox_id=$2 AND acked_at IS NULL
		RETURNING processing_generation,lease_expires_at`,
		messageID, msg.MailboxID, claimID, keyHash, lease.Seconds()).
		Scan(&generation, &leaseExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailProcessing{}, ErrAgentEmailNotFound
	}
	if err != nil {
		return AgentEmailProcessing{}, fmt.Errorf("claim agent email: %w", err)
	}
	processing := AgentEmailProcessing{
		State: AgentEmailProcessingClaimed, Generation: generation,
		FailureCount: msg.Processing.FailureCount, ClaimID: claimID,
		LeaseExpiresAt: &leaseExpiresAt,
	}
	msg.Processing = processing
	if err := logAgentEmailEvent(ctx, tx, VerbAgentEmailProcessingClaimed, ActorAgent, p.ID, msg, true); err != nil {
		return AgentEmailProcessing{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailProcessing{}, err
	}
	return processing, nil
}

// RenewAgentEmailClaim extends one exact, still-live owner claim.
func (s *Store) RenewAgentEmailClaim(
	ctx context.Context,
	scope AgentEmailPilotScope,
	p Principal,
	messageID string,
	in RenewAgentEmailClaimInput,
) (AgentEmailProcessing, error) {
	if err := requireAgentEmailPilotPrincipal(scope, p); err != nil {
		return AgentEmailProcessing{}, err
	}
	messageID, err := normalizeAgentEmailMessageID(messageID)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	claimID, generation, err := normalizeAgentEmailFence(in.ClaimID, in.Generation)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	lease, err := normalizeAgentEmailLease(in.LeaseDuration)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	msg, err := s.mutateAgentEmailClaim(ctx, p, messageID, func(ctx context.Context, tx pgx.Tx, locked AgentEmailMessage, _, _ string) (AgentEmailProcessing, error) {
		if locked.Processing.State != AgentEmailProcessingClaimed ||
			locked.Processing.ClaimID != claimID || locked.Processing.Generation != generation {
			return AgentEmailProcessing{}, ErrAgentEmailClaimLost
		}
		var expires time.Time
		err := tx.QueryRow(ctx, `
			UPDATE agent_email_deliveries
			SET lease_expires_at=clock_timestamp()+($5::double precision * interval '1 second')
			WHERE message_id=$1 AND mailbox_id=$2 AND processing_state='claimed'
			  AND claim_id=$3 AND processing_generation=$4
			  AND lease_expires_at>clock_timestamp()
			RETURNING lease_expires_at`, messageID, locked.MailboxID, claimID,
			generation, lease.Seconds()).Scan(&expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return AgentEmailProcessing{}, ErrAgentEmailClaimLost
		}
		if err != nil {
			return AgentEmailProcessing{}, fmt.Errorf("renew agent-email claim: %w", err)
		}
		locked.Processing.LeaseExpiresAt = &expires
		if err := logAgentEmailEvent(ctx, tx, VerbAgentEmailProcessingRenewed, ActorAgent, p.ID, locked, true); err != nil {
			return AgentEmailProcessing{}, err
		}
		return locked.Processing, nil
	})
	return msg, err
}

// ReleaseAgentEmailClaim releases one exact owner claim for later retry.
func (s *Store) ReleaseAgentEmailClaim(
	ctx context.Context,
	scope AgentEmailPilotScope,
	p Principal,
	messageID string,
	in ReleaseAgentEmailClaimInput,
) (AgentEmailProcessing, error) {
	if err := requireAgentEmailPilotPrincipal(scope, p); err != nil {
		return AgentEmailProcessing{}, err
	}
	messageID, err := normalizeAgentEmailMessageID(messageID)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	claimID, generation, err := normalizeAgentEmailFence(in.ClaimID, in.Generation)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	return s.mutateAgentEmailClaim(ctx, p, messageID, func(ctx context.Context, tx pgx.Tx, locked AgentEmailMessage, _, _ string) (AgentEmailProcessing, error) {
		if locked.Processing.State != AgentEmailProcessingClaimed ||
			locked.Processing.ClaimID != claimID || locked.Processing.Generation != generation {
			return AgentEmailProcessing{}, ErrAgentEmailClaimLost
		}
		if in.DeterministicFailure && locked.Processing.FailureCount >= maximumAgentEmailFailureCount {
			return AgentEmailProcessing{}, fmt.Errorf("%w: failure count exhausted", ErrAgentEmailConflict)
		}
		var failureCount int64
		err := tx.QueryRow(ctx, `
			UPDATE agent_email_deliveries
			SET processing_state='available',claim_id=NULL,claim_key_hash='',
			    lease_expires_at=NULL,completed_at=NULL,complete_key_hash='',
			    failure_count=failure_count+CASE WHEN $5 THEN 1 ELSE 0 END
			WHERE message_id=$1 AND mailbox_id=$2 AND processing_state='claimed'
			  AND claim_id=$3 AND processing_generation=$4
			RETURNING failure_count`, messageID, locked.MailboxID, claimID,
			generation, in.DeterministicFailure).Scan(&failureCount)
		if errors.Is(err, pgx.ErrNoRows) {
			return AgentEmailProcessing{}, ErrAgentEmailClaimLost
		}
		if err != nil {
			return AgentEmailProcessing{}, fmt.Errorf("release agent-email claim: %w", err)
		}
		processing := AgentEmailProcessing{
			State: AgentEmailProcessingAvailable, Generation: generation,
			FailureCount: failureCount,
		}
		locked.Processing = processing
		if err := logAgentEmailEvent(ctx, tx, VerbAgentEmailProcessingReleased, ActorAgent, p.ID, locked, true); err != nil {
			return AgentEmailProcessing{}, err
		}
		return processing, nil
	})
}

// CompleteAgentEmail closes one exact, still-live claim without creating an
// outbound result artifact. Same-key retries of the same fence are idempotent.
func (s *Store) CompleteAgentEmail(
	ctx context.Context,
	scope AgentEmailPilotScope,
	p Principal,
	messageID string,
	in CompleteAgentEmailInput,
) (AgentEmailProcessing, error) {
	if err := requireAgentEmailPilotPrincipal(scope, p); err != nil {
		return AgentEmailProcessing{}, err
	}
	messageID, err := normalizeAgentEmailMessageID(messageID)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	claimID, generation, err := normalizeAgentEmailFence(in.ClaimID, in.Generation)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	keyHash, err := normalizeAgentEmailKey(in.IdempotencyKey, "completion")
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	return s.mutateAgentEmailClaim(ctx, p, messageID, func(ctx context.Context, tx pgx.Tx, locked AgentEmailMessage, storedCompleteHash, _ string) (AgentEmailProcessing, error) {
		if locked.Processing.State == AgentEmailProcessingCompleted {
			if locked.Processing.ClaimID != claimID || locked.Processing.Generation != generation {
				return AgentEmailProcessing{}, ErrAgentEmailClaimLost
			}
			if storedCompleteHash != keyHash {
				return AgentEmailProcessing{}, ErrAgentEmailConflict
			}
			return locked.Processing, nil
		}
		if locked.Processing.State != AgentEmailProcessingClaimed ||
			locked.Processing.ClaimID != claimID || locked.Processing.Generation != generation {
			return AgentEmailProcessing{}, ErrAgentEmailClaimLost
		}
		var completedAt time.Time
		err := tx.QueryRow(ctx, `
			UPDATE agent_email_deliveries
			SET processing_state='completed',lease_expires_at=NULL,
			    completed_at=clock_timestamp(),complete_key_hash=$5
			WHERE message_id=$1 AND mailbox_id=$2 AND processing_state='claimed'
			  AND claim_id=$3 AND processing_generation=$4
			  AND lease_expires_at>clock_timestamp()
			RETURNING completed_at`, messageID, locked.MailboxID, claimID,
			generation, keyHash).Scan(&completedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return AgentEmailProcessing{}, ErrAgentEmailClaimLost
		}
		if err != nil {
			return AgentEmailProcessing{}, fmt.Errorf("complete agent-email claim: %w", err)
		}
		processing := AgentEmailProcessing{
			State: AgentEmailProcessingCompleted, Generation: generation,
			FailureCount: locked.Processing.FailureCount, ClaimID: claimID,
			CompletedAt: &completedAt,
		}
		locked.Processing = processing
		if err := logAgentEmailEvent(ctx, tx, VerbAgentEmailProcessingCompleted, ActorAgent, p.ID, locked, true); err != nil {
			return AgentEmailProcessing{}, err
		}
		return processing, nil
	})
}

func (s *Store) mutateAgentEmailClaim(
	ctx context.Context,
	p Principal,
	messageID string,
	mutation func(context.Context, pgx.Tx, AgentEmailMessage, string, string) (AgentEmailProcessing, error),
) (AgentEmailProcessing, error) {
	if p.Kind != PrincipalAgent {
		return AgentEmailProcessing{}, ErrAgentEmailForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return AgentEmailProcessing{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return AgentEmailProcessing{}, mapAgentEmailPrincipalError(err)
	}
	locked, completeHash, claimHash, err := lockAgentEmailProcessingTx(ctx, tx, p, messageID)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	processing, err := mutation(ctx, tx, locked, completeHash, claimHash)
	if err != nil {
		return AgentEmailProcessing{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailProcessing{}, err
	}
	return processing, nil
}

func lockAgentEmailProcessingTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	messageID string,
) (AgentEmailMessage, string, string, error) {
	row := tx.QueryRow(ctx, agentEmailSelect(false)+`
		WHERE m.id=$1 AND d.account_id=$2 AND d.realm_id=$3
		  AND d.owner_agent_id=$4 AND d.folder='inbox'
		FOR UPDATE OF d`, messageID, p.AccountID, p.RealmID, p.ID)
	msg, err := scanAgentEmail(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailMessage{}, "", "", ErrAgentEmailNotFound
	}
	if err != nil {
		return AgentEmailMessage{}, "", "", fmt.Errorf("lock agent-email processing: %w", err)
	}
	var completeHash, claimHash string
	if err := tx.QueryRow(ctx, `
		SELECT complete_key_hash,claim_key_hash
		FROM agent_email_deliveries
		WHERE message_id=$1 AND mailbox_id=$2`, messageID, msg.MailboxID).
		Scan(&completeHash, &claimHash); err != nil {
		return AgentEmailMessage{}, "", "", err
	}
	return msg, completeHash, claimHash, nil
}

func (s *Store) transitionAgentEmail(
	ctx context.Context,
	scope AgentEmailPilotScope,
	p Principal,
	messageID string,
	ack, consumeCode bool,
) (AgentEmailMessage, error) {
	if err := requireAgentEmailPilotPrincipal(scope, p); err != nil {
		return AgentEmailMessage{}, err
	}
	messageID = strings.TrimSpace(messageID)
	if !validAgentEmailMessageID(messageID) {
		return AgentEmailMessage{}, fmt.Errorf("%w: message id is invalid", ErrAgentEmailInputInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailMessage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return AgentEmailMessage{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return AgentEmailMessage{}, mapAgentEmailPrincipalError(err)
	}
	includeRaw := !ack && !consumeCode
	row := tx.QueryRow(ctx, agentEmailSelect(includeRaw)+`
		WHERE m.id=$1 AND d.account_id=$2 AND d.realm_id=$3
		  AND d.owner_agent_id=$4 AND d.folder='inbox'
		FOR UPDATE OF d`, messageID, p.AccountID, p.RealmID, p.ID)
	msg, err := scanAgentEmail(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailMessage{}, ErrAgentEmailNotFound
	}
	if err != nil {
		return AgentEmailMessage{}, fmt.Errorf("lock agent email: %w", err)
	}
	if ack && msg.Processing.State == AgentEmailProcessingClaimed {
		return AgentEmailMessage{}, ErrAgentEmailBusy
	}
	if consumeCode && msg.ReadState.CodeConsumedAt != nil {
		return AgentEmailMessage{}, ErrAgentEmailCodeConsumed
	}
	wasUnread := msg.ReadState.ReadAt == nil
	wasUnacked := msg.ReadState.AckedAt == nil
	if consumeCode {
		err = tx.QueryRow(ctx, `
			UPDATE agent_email_deliveries
			SET read_at=COALESCE(read_at,clock_timestamp()),
			    code_consumed_at=clock_timestamp()
			WHERE message_id=$1 AND mailbox_id=$2 AND code_consumed_at IS NULL
			RETURNING read_at,acked_at,code_consumed_at`, messageID, msg.MailboxID).
			Scan(&msg.ReadState.ReadAt, &msg.ReadState.AckedAt, &msg.ReadState.CodeConsumedAt)
	} else if ack {
		err = tx.QueryRow(ctx, `
			UPDATE agent_email_deliveries
			SET read_at=COALESCE(read_at,clock_timestamp()),
			    acked_at=COALESCE(acked_at,clock_timestamp())
			WHERE message_id=$1 AND mailbox_id=$2
			RETURNING read_at,acked_at,code_consumed_at`, messageID, msg.MailboxID).
			Scan(&msg.ReadState.ReadAt, &msg.ReadState.AckedAt, &msg.ReadState.CodeConsumedAt)
	} else {
		err = tx.QueryRow(ctx, `
			UPDATE agent_email_deliveries
			SET read_at=COALESCE(read_at,clock_timestamp())
			WHERE message_id=$1 AND mailbox_id=$2
			RETURNING read_at,acked_at,code_consumed_at`, messageID, msg.MailboxID).
			Scan(&msg.ReadState.ReadAt, &msg.ReadState.AckedAt, &msg.ReadState.CodeConsumedAt)
	}
	if errors.Is(err, pgx.ErrNoRows) && consumeCode {
		return AgentEmailMessage{}, ErrAgentEmailCodeConsumed
	}
	if err != nil {
		return AgentEmailMessage{}, fmt.Errorf("advance agent-email state: %w", err)
	}
	msg.ReadState.State = agentEmailReadState(msg.ReadState.ReadAt, msg.ReadState.AckedAt)
	if wasUnread {
		if err := logAgentEmailEvent(ctx, tx, VerbAgentEmailRead, ActorAgent, p.ID, msg, false); err != nil {
			return AgentEmailMessage{}, err
		}
	}
	if ack && wasUnacked {
		if err := logAgentEmailEvent(ctx, tx, VerbAgentEmailAcked, ActorAgent, p.ID, msg, false); err != nil {
			return AgentEmailMessage{}, err
		}
	}
	if consumeCode {
		if err := logAgentEmailEvent(ctx, tx, VerbAgentEmailCodeConsumed, ActorAgent, p.ID, msg, false); err != nil {
			return AgentEmailMessage{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailMessage{}, err
	}
	return msg, nil
}

func agentEmailAddressForOwnerTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, realmID, agentID string,
) (AgentEmailAddress, error) {
	row := tx.QueryRow(ctx, agentEmailAddressSelect()+`
		WHERE mb.account_id=$1 AND mb.realm_id=$2 AND mb.owner_agent_id=$3`,
		accountID, realmID, agentID)
	address, err := scanAgentEmailAddress(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailAddress{}, ErrAgentEmailAddressMissing
	}
	if err != nil {
		return AgentEmailAddress{}, fmt.Errorf("read agent-email address: %w", err)
	}
	return address, nil
}

func agentEmailAddressByRecipientTx(
	ctx context.Context,
	tx pgx.Tx,
	domain, localPart string,
	lock bool,
) (AgentEmailAddress, error) {
	query := agentEmailAddressSelect() + `
		WHERE addr.domain=$1 AND addr.local_part=$2 AND addr.retired_at IS NULL`
	if lock {
		query += ` FOR SHARE OF addr,mb`
	}
	row := tx.QueryRow(ctx, query, domain, localPart)
	address, err := scanAgentEmailAddress(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailAddress{}, ErrAgentEmailUnknownRecipient
	}
	if err != nil {
		return AgentEmailAddress{}, fmt.Errorf("resolve agent-email recipient: %w", err)
	}
	return address, nil
}

func agentEmailAddressSelect() string {
	return `SELECT addr.id,mb.id,addr.account_id,addr.realm_id,mb.owner_agent_id,
		       addr.local_part || '@' || addr.domain,addr.domain,addr.local_part,
		       addr.agent_segment,addr.realm_label,addr.provisioning_kind,
		       mb.receive_state,addr.created_at,mb.updated_at,mb.disabled_at,
		       COALESCE(mb.retired_at,addr.retired_at)
		FROM agent_email_addresses addr
		JOIN agent_email_mailboxes mb
		  ON mb.address_id=addr.id AND mb.account_id=addr.account_id
		 AND mb.realm_id=addr.realm_id
		 AND mb.owner_agent_id=addr.provisioned_agent_id`
}

func scanAgentEmailAddress(row rowScanner) (AgentEmailAddress, error) {
	var address AgentEmailAddress
	err := row.Scan(
		&address.ID, &address.MailboxID, &address.AccountID, &address.RealmID,
		&address.OwnerAgentID, &address.Address, &address.Domain,
		&address.LocalPart, &address.AgentSegment, &address.RealmLabel,
		&address.ProvisioningKind, &address.ReceiveState, &address.CreatedAt,
		&address.UpdatedAt, &address.DisabledAt, &address.RetiredAt,
	)
	return address, err
}

func agentEmailSelect(includeRaw bool) string {
	raw := `NULL::bytea`
	if includeRaw {
		raw = `m.raw_mime`
	}
	return fmt.Sprintf(`SELECT
		m.id,m.account_id,m.realm_id,m.mailbox_id,m.owner_agent_id,m.address_id,
		m.provider,m.envelope_sender,m.envelope_recipient,m.agent_segment,
		m.realm_label,COALESCE(m.subaddress_tag,''),m.raw_size_bytes,
		m.parse_state,COALESCE(m.parse_error,''),COALESCE(m.header_from,''),
		COALESCE(m.header_to,''),COALESCE(m.header_subject,''),
		COALESCE(m.mime_message_id,''),m.message_date,m.attachment_count,
		COALESCE(m.spf_result,'unknown'),COALESCE(m.dkim_result,'unknown'),
		COALESCE(m.dmarc_result,'unknown'),COALESCE(m.spam_verdict,'unknown'),
		m.sender_verification_state,COALESCE(m.possible_duplicate_of_message_id,''),
		m.received_at,m.created_at,d.folder,d.delivered_at,
		d.read_at,d.acked_at,d.code_consumed_at,
		d.processing_state,d.processing_generation,d.failure_count,
		COALESCE(d.claim_id,''),d.lease_expires_at,d.completed_at,
		%s,m.duplicate_group_sha256
		FROM agent_email_messages m
		JOIN agent_email_deliveries d
		  ON d.message_id=m.id AND d.mailbox_id=m.mailbox_id`, raw)
}

func scanAgentEmail(row rowScanner) (AgentEmailMessage, error) {
	var msg AgentEmailMessage
	err := row.Scan(
		&msg.ID, &msg.AccountID, &msg.RealmID, &msg.MailboxID,
		&msg.OwnerAgentID, &msg.AddressID, &msg.Provider,
		&msg.EnvelopeSender, &msg.EnvelopeRecipient, &msg.AgentSegment,
		&msg.RealmLabel, &msg.SubaddressTag, &msg.RawSizeBytes,
		&msg.ParseState, &msg.ParseErrorCode, &msg.HeaderFrom, &msg.HeaderTo,
		&msg.Subject, &msg.MIMEMessageID, &msg.MessageDate, &msg.AttachmentCount,
		&msg.SPFResult, &msg.DKIMResult, &msg.DMARCResult, &msg.SpamVerdict,
		&msg.SenderVerificationState, &msg.PossibleDuplicateOfMessage,
		&msg.ReceivedAt, &msg.CreatedAt, &msg.Folder, &msg.DeliveredAt,
		&msg.ReadState.ReadAt, &msg.ReadState.AckedAt, &msg.ReadState.CodeConsumedAt,
		&msg.Processing.State, &msg.Processing.Generation, &msg.Processing.FailureCount,
		&msg.Processing.ClaimID, &msg.Processing.LeaseExpiresAt,
		&msg.Processing.CompletedAt, &msg.rawMIME, &msg.duplicateGroupSHA256,
	)
	if err != nil {
		return AgentEmailMessage{}, err
	}
	msg.PossibleDuplicate = msg.PossibleDuplicateOfMessage != ""
	msg.ReadState.State = agentEmailReadState(msg.ReadState.ReadAt, msg.ReadState.AckedAt)
	return msg, nil
}

func normalizeAgentEmailFilter(filter AgentEmailFilter) (AgentEmailFilter, time.Time, string, error) {
	if filter.OldestFirst && filter.Cursor != "" {
		return AgentEmailFilter{}, time.Time{}, "", fmt.Errorf("%w: oldest-first does not accept a cursor", ErrAgentEmailInputInvalid)
	}
	if filter.Limit == 0 {
		filter.Limit = defaultAgentEmailPageSize
	}
	if filter.Limit < 1 || filter.Limit > maximumAgentEmailPageSize {
		return AgentEmailFilter{}, time.Time{}, "", fmt.Errorf("%w: limit must be 1-%d", ErrAgentEmailInputInvalid, maximumAgentEmailPageSize)
	}
	if filter.Cursor == "" {
		return filter, time.Time{}, "", nil
	}
	t, messageID, err := decodeAgentEmailCursor(filter.Cursor)
	if err != nil {
		return AgentEmailFilter{}, time.Time{}, "", err
	}
	return filter, t, messageID, nil
}

func encodeAgentEmailCursor(t time.Time, messageID string) string {
	return strconv.FormatInt(t.UnixNano(), 10) + ":" + messageID
}

func decodeAgentEmailCursor(cursor string) (time.Time, string, error) {
	left, right, ok := strings.Cut(cursor, ":")
	if !ok || !validAgentEmailMessageID(right) {
		return time.Time{}, "", ErrAgentEmailCursorInvalid
	}
	ns, err := strconv.ParseInt(left, 10, 64)
	if err != nil || ns <= 0 {
		return time.Time{}, "", ErrAgentEmailCursorInvalid
	}
	return time.Unix(0, ns).UTC(), right, nil
}

func validAgentEmailMessageID(value string) bool {
	if len(value) != len("emsg_")+16 || !strings.HasPrefix(value, "emsg_") {
		return false
	}
	for _, c := range []byte(strings.TrimPrefix(value, "emsg_")) {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

func normalizeAgentEmailMessageID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !validAgentEmailMessageID(value) {
		return "", fmt.Errorf("%w: message id is invalid", ErrAgentEmailInputInvalid)
	}
	return value, nil
}

func normalizeAgentEmailLease(lease time.Duration) (time.Duration, error) {
	if lease == 0 {
		lease = defaultAgentEmailLease
	}
	if lease < minimumAgentEmailLease || lease > maximumAgentEmailLease {
		return 0, fmt.Errorf(
			"%w: lease duration must be between %s and %s",
			ErrAgentEmailInputInvalid, minimumAgentEmailLease, maximumAgentEmailLease,
		)
	}
	return lease, nil
}

func normalizeAgentEmailKey(key, operation string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("%w: %s idempotency key is required", ErrAgentEmailInputInvalid, operation)
	}
	if len(key) > maximumAgentEmailKeyBytes {
		return "", fmt.Errorf(
			"%w: %s idempotency key exceeds %d bytes",
			ErrAgentEmailInputInvalid, operation, maximumAgentEmailKeyBytes,
		)
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]), nil
}

func normalizeAgentEmailFence(claimID string, generation int64) (string, int64, error) {
	claimID = strings.TrimSpace(claimID)
	if len(claimID) > maximumAgentEmailClaimIDBytes || !validAgentEmailGeneratedID(claimID, "ecl") {
		return "", 0, fmt.Errorf("%w: claim id is invalid", ErrAgentEmailInputInvalid)
	}
	if generation < 1 || generation > maximumAgentEmailGeneration {
		return "", 0, fmt.Errorf("%w: processing generation is invalid", ErrAgentEmailInputInvalid)
	}
	return claimID, generation, nil
}

func validAgentEmailGeneratedID(value, prefix string) bool {
	body := strings.TrimPrefix(value, prefix+"_")
	if body == value || len(body) != 16 || len(value) != len(prefix)+17 {
		return false
	}
	for _, char := range body {
		if (char < 'a' || char > 'z') && (char < '2' || char > '7') {
			return false
		}
	}
	return true
}

func normalizeAgentEmailPilotScope(scope AgentEmailPilotScope) (string, error) {
	domain, err := agentemail.ValidateDomain(scope.Domain)
	if err != nil {
		return "", fmt.Errorf("%w: pilot domain is invalid", ErrAgentEmailInputInvalid)
	}
	audience := strings.ToLower(strings.TrimSpace(scope.Audience))
	if audience == "" || len(audience) > 128 || audience[0] < 'a' || audience[0] > 'z' {
		return "", fmt.Errorf("%w: pilot audience is invalid", ErrAgentEmailInputInvalid)
	}
	for _, char := range []byte(audience) {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return "", fmt.Errorf("%w: pilot audience is invalid", ErrAgentEmailInputInvalid)
		}
	}
	if audience[len(audience)-1] == '-' {
		return "", fmt.Errorf("%w: pilot audience is invalid", ErrAgentEmailInputInvalid)
	}
	realms := 0
	for realmID, enabled := range scope.RealmIDs {
		if !enabled {
			continue
		}
		if !validAgentEmailGeneratedID(realmID, "realm") {
			return "", fmt.Errorf("%w: pilot realm id is invalid", ErrAgentEmailInputInvalid)
		}
		realms++
	}
	if realms != 1 {
		return "", fmt.Errorf("%w: pilot requires exactly one enrolled realm", ErrAgentEmailInputInvalid)
	}
	agents := 0
	for agentID, enabled := range scope.AgentIDs {
		if !enabled {
			continue
		}
		if !validAgentEmailGeneratedID(agentID, "agent") {
			return "", fmt.Errorf("%w: pilot agent id is invalid", ErrAgentEmailInputInvalid)
		}
		agents++
	}
	if agents < 5 || agents > 10 {
		return "", fmt.Errorf("%w: pilot requires 5-10 enrolled agents", ErrAgentEmailInputInvalid)
	}
	return domain, nil
}

func requireAgentEmailPilotEnrollment(
	scope AgentEmailPilotScope,
	realmID, agentID string,
) (string, error) {
	if !scope.Enabled {
		return "", ErrAgentEmailPilotDisabled
	}
	domain, err := normalizeAgentEmailPilotScope(scope)
	if err != nil {
		return "", err
	}
	if !scope.RealmIDs[realmID] || !scope.AgentIDs[agentID] {
		return "", ErrAgentEmailPilotNotEnrolled
	}
	return domain, nil
}

func requireAgentEmailPilotPrincipal(scope AgentEmailPilotScope, p Principal) error {
	if !scope.Enabled {
		return ErrAgentEmailPilotDisabled
	}
	if _, err := normalizeAgentEmailPilotScope(scope); err != nil {
		return err
	}
	if p.Kind != PrincipalAgent ||
		(strings.TrimSpace(p.AccessProfile) != "" && p.AccessProfile != AccessProfileFull) {
		return ErrAgentEmailForbidden
	}
	if !scope.RealmIDs[p.RealmID] || !scope.AgentIDs[p.ID] {
		return ErrAgentEmailPilotNotEnrolled
	}
	return nil
}

func agentEmailDuplicateGroup(rawSHA, recipient, sender string) string {
	sum := sha256.Sum256([]byte(rawSHA + "\x00" + recipient + "\x00" + sender))
	return hex.EncodeToString(sum[:])
}

func agentEmailReadState(readAt, ackedAt *time.Time) string {
	return readState(readAt, ackedAt)
}

func redactAgentEmailFence(msg AgentEmailMessage) AgentEmailMessage {
	msg.Processing.ClaimID = ""
	msg.Processing.LeaseExpiresAt = nil
	msg.rawMIME = nil
	msg.duplicateGroupSHA256 = ""
	return msg
}

func agentEmailNullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func mapAgentEmailPrincipalError(err error) error {
	if errors.Is(err, ErrMessageForbidden) || errors.Is(err, ErrMessageRecipientMissing) {
		return ErrAgentEmailForbidden
	}
	return err
}

func logAgentEmailEvent(
	ctx context.Context,
	tx pgx.Tx,
	verb, actorKind, actorID string,
	msg AgentEmailMessage,
	processing bool,
) error {
	metadata := map[string]any{
		"message_id": msg.ID, "mailbox_id": msg.MailboxID,
		"owner_agent_id": msg.OwnerAgentID, "address_id": msg.AddressID,
	}
	if verb == VerbAgentEmailReceived {
		metadata["raw_size_bytes"] = strconv.FormatInt(msg.RawSizeBytes, 10)
		metadata["possible_duplicate"] = msg.PossibleDuplicate
	}
	if processing {
		metadata["processing_generation"] = strconv.FormatInt(msg.Processing.Generation, 10)
		metadata["failure_count"] = strconv.FormatInt(msg.Processing.FailureCount, 10)
	}
	return logEventTx(ctx, tx, EventInput{
		AccountID: msg.AccountID, ActorKind: actorKind, ActorID: actorID,
		Verb: verb, Metadata: metadata,
	})
}

// retireAgentEmailMailboxTx is called inside DeleteAgent's account/agent lock.
// It makes the address unroutable while retaining the reservation tombstone.
func retireAgentEmailMailboxTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, realmID, agentID, reason string,
) error {
	if reason == "" {
		reason = "agent_deleted"
	}
	_, err := tx.Exec(ctx, `
		WITH retired_mailbox AS (
		  UPDATE agent_email_mailboxes
		  SET receive_state='retired',retired_at=COALESCE(retired_at,clock_timestamp()),
		      updated_at=clock_timestamp(),row_version=row_version+1
		  WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		    AND receive_state<>'retired'
		  RETURNING address_id
		)
		UPDATE agent_email_addresses address
		SET retired_at=COALESCE(address.retired_at,clock_timestamp()),
		    retirement_reason_code=COALESCE(address.retirement_reason_code,$4)
		FROM retired_mailbox
		WHERE address.id=retired_mailbox.address_id
		  AND address.account_id=$1 AND address.realm_id=$2`,
		accountID, realmID, agentID, reason)
	if err != nil {
		return fmt.Errorf("retire agent-email mailbox: %w", err)
	}
	return nil
}
