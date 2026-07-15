package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// Message direction, recipient, delivery, and read-state values form the
// finite storage/API vocabulary for the first direct-messaging slice.
const (
	MessageDirectionInbox  = "inbox"
	MessageDirectionOutbox = "outbox"

	MessageRecipientAgent = "agent"

	MessageDeliveryQueued    = "queued"
	MessageDeliveryDelivered = "delivered"
	MessageDeliveryFailed    = "failed"

	MessageReadUnread = "unread"
	MessageReadRead   = "read"
	MessageReadAcked  = "acked"

	MessageProcessingAvailable = "available"
	MessageProcessingClaimed   = "claimed"
	MessageProcessingCompleted = "completed"

	maxMessageSubjectBytes               = 256
	maxMessageKindBytes                  = 64
	maxMessageBodyBytes                  = 64 * 1024
	maxMessagePayloadBytes               = 16 * 1024
	maxMessageThreadIDBytes              = 128
	maxMessageReplyIDBytes               = 128
	maxMessageIdempotencyKeyBytes        = 512
	maxMessageRecipientBytes             = 256
	maxMessageClaimIDBytes               = 128
	maxMessageCausalDepth          int64 = 2147483647
	defaultMessagePageSize               = 50
	maxMessagePageSize                   = 100
	defaultMessageLeaseDuration          = 5 * time.Minute
	minMessageLeaseDuration              = 30 * time.Second
	maxMessageLeaseDuration              = 15 * time.Minute
	maxMessageProcessingGeneration int64 = 4611686018427387903
	maxMessageFailureCount         int64 = 4611686018427387903
)

var (
	// ErrMessageInputInvalid reports caller-correctable message input.
	ErrMessageInputInvalid = errors.New("invalid message input")
	// ErrMessageNotFound reports a message outside the recipient mailbox.
	ErrMessageNotFound = errors.New("message not found")
	// ErrMessageRecipientMissing reports a recipient absent from the realm.
	ErrMessageRecipientMissing = errors.New("message recipient not found")
	// ErrMessageForbidden reports use by a principal that is not an agent.
	ErrMessageForbidden = errors.New("message access forbidden")
	// ErrMessageConflict reports incompatible reuse of an idempotency key.
	ErrMessageConflict = errors.New("message idempotency conflict")
	// ErrMessageBusy reports a live processing claim owned by another attempt.
	ErrMessageBusy = errors.New("message processing claim is busy")
	// ErrMessageClaimLost reports an expired, released, completed, or superseded fence.
	ErrMessageClaimLost = errors.New("message processing claim was lost")
	// ErrMessageCursorInvalid reports an invalid mailbox cursor.
	ErrMessageCursorInvalid = errors.New("malformed message cursor")
)

// MessageAgent identifies the token-derived sender.
type MessageAgent struct {
	ID   string `json:"agent_id"`
	Name string `json:"agent_name"`
}

// MessageRecipient identifies one resolved direct recipient.
type MessageRecipient struct {
	Kind string `json:"kind"`
	ID   string `json:"agent_id"`
	Name string `json:"agent_name"`
}

// MessageDelivery is the durable recipient delivery state.
type MessageDelivery struct {
	State       string     `json:"state"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

// MessageReadState is the recipient's unread/read/acked transition state.
type MessageReadState struct {
	State   string     `json:"state"`
	ReadAt  *time.Time `json:"read_at,omitempty"`
	AckedAt *time.Time `json:"acked_at,omitempty"`
}

// MessageProcessing is the value-free delivery-processing state exposed to a
// recipient runner. Raw idempotency keys and their hashes never leave storage.
type MessageProcessing struct {
	State           string     `json:"state"`
	Generation      int64      `json:"generation"`
	FailureCount    int64      `json:"failure_count"`
	ClaimID         string     `json:"claim_id,omitempty"`
	LeaseExpiresAt  *time.Time `json:"lease_expires_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	ResultMessageID string     `json:"result_message_id,omitempty"`
}

// Message is one direct realm-local agent message plus the recipient's state.
// Body and Payload are empty on list results and populated only by send/read.
type Message struct {
	ID               string            `json:"id"`
	AccountID        string            `json:"account_id"`
	RealmID          string            `json:"realm_id"`
	From             MessageAgent      `json:"from"`
	To               MessageRecipient  `json:"to"`
	Subject          string            `json:"subject,omitempty"`
	Kind             string            `json:"kind"`
	Body             string            `json:"body,omitempty"`
	Payload          json.RawMessage   `json:"payload,omitempty"`
	ThreadID         string            `json:"thread_id"`
	ReplyToMessageID string            `json:"reply_to_message_id,omitempty"`
	CausalDepth      int64             `json:"causal_depth"`
	CreatedAt        time.Time         `json:"created_at"`
	Delivery         MessageDelivery   `json:"delivery"`
	ReadState        MessageReadState  `json:"read_state"`
	Processing       MessageProcessing `json:"processing"`
}

// SendMessageInput is the caller-controlled, non-identity send payload.
type SendMessageInput struct {
	ToAgent          string
	Subject          string
	Kind             string
	Body             string
	Payload          json.RawMessage
	ThreadID         string
	ReplyToMessageID string
	IdempotencyKey   string
}

// ReplyMessageInput is caller-controlled reply content. Recipient, realm,
// thread, and parent causality are derived from the stored parent message.
type ReplyMessageInput struct {
	Subject        string
	Kind           string
	Body           string
	Payload        json.RawMessage
	IdempotencyKey string
}

// ClaimMessageInput starts or idempotently replays one bounded processing
// lease. LeaseDuration defaults to five minutes.
type ClaimMessageInput struct {
	LeaseDuration  time.Duration
	IdempotencyKey string
}

// RenewMessageClaimInput extends one exact, still-live fencing generation.
type RenewMessageClaimInput struct {
	ClaimID              string
	ProcessingGeneration int64
	LeaseDuration        time.Duration
}

// MessageClaimFence identifies one exact processing generation.
type MessageClaimFence struct {
	ClaimID              string
	ProcessingGeneration int64
}

// ReleaseMessageClaimInput gives up one exact processing generation. Only a
// deterministic failure attributable to this message increments the durable
// poison counter; cancellation and provider-wide failures leave it unchanged.
type ReleaseMessageClaimInput struct {
	ClaimID              string
	ProcessingGeneration int64
	DeterministicFailure bool
}

// CompleteMessageInput atomically publishes a parent-derived result reply and
// closes one exact, unexpired processing generation.
type CompleteMessageInput struct {
	ClaimID              string
	ProcessingGeneration int64
	IdempotencyKey       string
	Subject              string
	Kind                 string
	Body                 string
	Payload              json.RawMessage
}

// CompleteMessageResult returns the terminal processing state and the one
// durable result message linked to it.
type CompleteMessageResult struct {
	Processing    MessageProcessing `json:"processing"`
	ResultMessage Message           `json:"result_message"`
}

// MessageFilter selects one bounded inbox or outbox page.
type MessageFilter struct {
	Direction   string
	Unread      bool
	Unacked     bool
	OldestFirst bool
	From        string
	ThreadID    string
	Kind        string
	Limit       int
	Cursor      string
}

// MessagePage is a metadata-only mailbox page and continuation cursor.
type MessagePage struct {
	Messages   []Message
	NextCursor string
}

// SendMessage resolves the direct recipient in the token-derived realm and
// atomically creates the immutable message, delivery row, and audit events.
func (s *Store) SendMessage(ctx context.Context, p Principal, in SendMessageInput) (Message, error) {
	if p.Kind != PrincipalAgent {
		return Message{}, ErrMessageForbidden
	}
	if strings.TrimSpace(in.ReplyToMessageID) != "" {
		return Message{}, fmt.Errorf("%w: direct send cannot set reply_to_message_id", ErrMessageInputInvalid)
	}
	var err error
	in, err = normalizeSendMessageInput(in)
	if err != nil {
		return Message{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return Message{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return Message{}, err
	}
	if existing, replay, err := messageReplayByIdempotencyKey(ctx, tx, p, in.IdempotencyKey); err != nil {
		return Message{}, err
	} else if replay {
		if !messageMatchesDirectSendReplay(existing, in) {
			return Message{}, ErrMessageConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Message{}, err
		}
		return redactMessageProcessingFence(existing), nil
	}

	to, err := resolveMessageAgent(ctx, tx, p.AccountID, p.RealmID, in.ToAgent)
	if err != nil {
		return Message{}, err
	}
	msg, err := s.insertMessageTx(ctx, tx, p, to, in)
	if err != nil {
		return Message{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, err
	}
	return redactMessageProcessingFence(msg), nil
}

// ReplyMessage creates one recipient-only reply. The caller must be the
// parent's delivery recipient; the recipient, realm, thread, and causal parent
// are all derived inside the transaction. Reading or acknowledging the parent
// remains a separate operation.
func (s *Store) ReplyMessage(ctx context.Context, p Principal, parentMessageID string, in ReplyMessageInput) (Message, error) {
	if p.Kind != PrincipalAgent {
		return Message{}, ErrMessageForbidden
	}
	parentMessageID = strings.TrimSpace(parentMessageID)
	if !strings.HasPrefix(parentMessageID, "msg_") || len(parentMessageID) > maxMessageReplyIDBytes {
		return Message{}, fmt.Errorf("%w: parent message id must be a msg_ id no longer than %d bytes", ErrMessageInputInvalid, maxMessageReplyIDBytes)
	}
	kind := strings.TrimSpace(in.Kind)
	if kind == "" {
		kind = "reply"
	}
	draft, err := normalizeSendMessageInput(SendMessageInput{
		ToAgent: "parent", Subject: in.Subject, Kind: kind, Body: in.Body,
		Payload: in.Payload, IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return Message{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return Message{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return Message{}, err
	}
	if existing, replay, err := messageReplayByIdempotencyKey(ctx, tx, p, draft.IdempotencyKey); err != nil {
		return Message{}, err
	} else if replay {
		if !messageMatchesReplyReplay(existing, parentMessageID, draft) {
			return Message{}, ErrMessageConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Message{}, err
		}
		return redactMessageProcessingFence(existing), nil
	}

	parent, err := scanMessage(tx.QueryRow(ctx, messageSelect(false)+`
		WHERE m.id = $1 AND m.account_id = $2 AND m.realm_id = $3
		  AND m.to_agent_id = $4 AND d.recipient_agent_id = $4
		FOR SHARE OF m, d`, parentMessageID, p.AccountID, p.RealmID, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrMessageNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("resolve reply parent: %w", err)
	}
	to, err := resolveMessageAgent(ctx, tx, p.AccountID, p.RealmID, parent.From.ID)
	if err != nil {
		return Message{}, err
	}
	draft.ToAgent = to.ID
	draft.ThreadID = parent.ThreadID
	draft.ReplyToMessageID = parent.ID
	msg, err := s.insertMessageTx(ctx, tx, p, to, draft)
	if err != nil {
		return Message{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, err
	}
	return redactMessageProcessingFence(msg), nil
}

// ClaimMessage acquires one recipient-only processing lease. A retry with the
// same idempotency key while its claim remains live returns the same claim.
// Another live claim is busy; an expired claim is fenced by a new generation.
// Completed deliveries return their linked terminal state without mutation.
func (s *Store) ClaimMessage(ctx context.Context, p Principal, messageID string, in ClaimMessageInput) (Message, error) {
	if p.Kind != PrincipalAgent {
		return Message{}, ErrMessageForbidden
	}
	messageID, err := normalizeProcessingMessageID(messageID)
	if err != nil {
		return Message{}, err
	}
	lease, err := normalizeMessageLeaseDuration(in.LeaseDuration)
	if err != nil {
		return Message{}, err
	}
	keyHash, err := normalizeMessageProcessingKey(in.IdempotencyKey, "claim")
	if err != nil {
		return Message{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return Message{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return Message{}, err
	}
	msg, err := lockMessageProcessingDelivery(ctx, tx, p, messageID, true)
	if err != nil {
		return Message{}, err
	}
	if msg.Processing.State == MessageProcessingCompleted {
		return msg, nil
	}

	var databaseNow time.Time
	var storedClaimKeyHash string
	if err := tx.QueryRow(ctx, `
		SELECT clock_timestamp(), claim_key_hash
		FROM agent_message_deliveries
		WHERE message_id=$1 AND recipient_agent_id=$2`, messageID, p.ID).
		Scan(&databaseNow, &storedClaimKeyHash); err != nil {
		return Message{}, fmt.Errorf("read message processing clock: %w", err)
	}
	if msg.Processing.State == MessageProcessingClaimed && msg.Processing.LeaseExpiresAt != nil &&
		msg.Processing.LeaseExpiresAt.After(databaseNow) {
		if storedClaimKeyHash == keyHash {
			return msg, nil
		}
		return Message{}, ErrMessageBusy
	}
	if msg.Processing.Generation >= maxMessageProcessingGeneration {
		return Message{}, fmt.Errorf("%w: processing generation exhausted", ErrMessageConflict)
	}
	claimID, err := id.New("mcl")
	if err != nil {
		return Message{}, err
	}
	var leaseExpiresAt time.Time
	err = tx.QueryRow(ctx, `
		UPDATE agent_message_deliveries
		SET processing_state='claimed',
		    processing_generation=processing_generation+1,
		    claim_id=$3, claim_key_hash=$4,
		    lease_expires_at=clock_timestamp()+($5::double precision * interval '1 second'),
		    completed_at=NULL, complete_key_hash='', result_message_id=NULL
		WHERE message_id=$1 AND recipient_agent_id=$2 AND acked_at IS NULL
		RETURNING processing_generation, lease_expires_at`,
		messageID, p.ID, claimID, keyHash, lease.Seconds()).
		Scan(&msg.Processing.Generation, &leaseExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrMessageNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("claim message delivery: %w", err)
	}
	msg.Processing = MessageProcessing{
		State: MessageProcessingClaimed, Generation: msg.Processing.Generation,
		FailureCount: msg.Processing.FailureCount,
		ClaimID:      claimID, LeaseExpiresAt: &leaseExpiresAt,
	}
	if err := logMessageProcessingEvent(ctx, tx, VerbMessageProcessingClaimed, p.ID, msg, ""); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// RenewMessageClaim extends one exact, unexpired claim from database time.
func (s *Store) RenewMessageClaim(ctx context.Context, p Principal, messageID string, in RenewMessageClaimInput) (Message, error) {
	if p.Kind != PrincipalAgent {
		return Message{}, ErrMessageForbidden
	}
	messageID, err := normalizeProcessingMessageID(messageID)
	if err != nil {
		return Message{}, err
	}
	fence, err := normalizeMessageClaimFence(MessageClaimFence{
		ClaimID: in.ClaimID, ProcessingGeneration: in.ProcessingGeneration,
	})
	if err != nil {
		return Message{}, err
	}
	lease, err := normalizeMessageLeaseDuration(in.LeaseDuration)
	if err != nil {
		return Message{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return Message{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return Message{}, err
	}
	msg, err := lockMessageProcessingDelivery(ctx, tx, p, messageID, false)
	if err != nil {
		return Message{}, err
	}
	if msg.Processing.State != MessageProcessingClaimed ||
		msg.Processing.ClaimID != fence.ClaimID || msg.Processing.Generation != fence.ProcessingGeneration {
		return Message{}, ErrMessageClaimLost
	}
	var leaseExpiresAt time.Time
	err = tx.QueryRow(ctx, `
		UPDATE agent_message_deliveries
		SET lease_expires_at=clock_timestamp()+($5::double precision * interval '1 second')
		WHERE message_id=$1 AND recipient_agent_id=$2
		  AND processing_state='claimed' AND claim_id=$3
		  AND processing_generation=$4 AND lease_expires_at > clock_timestamp()
		RETURNING lease_expires_at`, messageID, p.ID, fence.ClaimID,
		fence.ProcessingGeneration, lease.Seconds()).Scan(&leaseExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrMessageClaimLost
	}
	if err != nil {
		return Message{}, fmt.Errorf("renew message claim: %w", err)
	}
	msg.Processing.LeaseExpiresAt = &leaseExpiresAt
	if err := logMessageProcessingEvent(ctx, tx, VerbMessageProcessingRenewed, p.ID, msg, ""); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// ReleaseMessageClaim gives up one exact generation. An expired claim may be
// released until another claimant has consumed the next fencing generation.
func (s *Store) ReleaseMessageClaim(ctx context.Context, p Principal, messageID string, in ReleaseMessageClaimInput) (Message, error) {
	if p.Kind != PrincipalAgent {
		return Message{}, ErrMessageForbidden
	}
	messageID, err := normalizeProcessingMessageID(messageID)
	if err != nil {
		return Message{}, err
	}
	fence, err := normalizeMessageClaimFence(MessageClaimFence{
		ClaimID: in.ClaimID, ProcessingGeneration: in.ProcessingGeneration,
	})
	if err != nil {
		return Message{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return Message{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return Message{}, err
	}
	msg, err := lockMessageProcessingDelivery(ctx, tx, p, messageID, false)
	if err != nil {
		return Message{}, err
	}
	if msg.Processing.State != MessageProcessingClaimed ||
		msg.Processing.ClaimID != fence.ClaimID || msg.Processing.Generation != fence.ProcessingGeneration {
		return Message{}, ErrMessageClaimLost
	}
	if in.DeterministicFailure && msg.Processing.FailureCount >= maxMessageFailureCount {
		return Message{}, fmt.Errorf("%w: message failure count exhausted", ErrMessageConflict)
	}
	var failureCount int64
	err = tx.QueryRow(ctx, `
		UPDATE agent_message_deliveries
		SET processing_state='available', claim_id=NULL, claim_key_hash='',
		    lease_expires_at=NULL, completed_at=NULL, complete_key_hash='',
		    result_message_id=NULL,
		    failure_count=failure_count + CASE WHEN $5 THEN 1 ELSE 0 END
		WHERE message_id=$1 AND recipient_agent_id=$2
		  AND processing_state='claimed' AND claim_id=$3 AND processing_generation=$4
		RETURNING failure_count`, messageID, p.ID, fence.ClaimID,
		fence.ProcessingGeneration, in.DeterministicFailure).Scan(&failureCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrMessageClaimLost
	}
	if err != nil {
		return Message{}, fmt.Errorf("release message claim: %w", err)
	}
	msg.Processing = MessageProcessing{
		State: MessageProcessingAvailable, Generation: fence.ProcessingGeneration,
		FailureCount: failureCount,
	}
	if err := logMessageProcessingEvent(ctx, tx, VerbMessageProcessingReleased, p.ID, msg, ""); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// CompleteMessage atomically creates a parent-derived result reply and closes
// an exact, unexpired processing claim. It never reads or acknowledges the
// source delivery. A same-key retry returns the original linked result.
func (s *Store) CompleteMessage(ctx context.Context, p Principal, messageID string, in CompleteMessageInput) (CompleteMessageResult, error) {
	if p.Kind != PrincipalAgent {
		return CompleteMessageResult{}, ErrMessageForbidden
	}
	messageID, err := normalizeProcessingMessageID(messageID)
	if err != nil {
		return CompleteMessageResult{}, err
	}
	fence, err := normalizeMessageClaimFence(MessageClaimFence{
		ClaimID: in.ClaimID, ProcessingGeneration: in.ProcessingGeneration,
	})
	if err != nil {
		return CompleteMessageResult{}, err
	}
	completeKeyHash, err := normalizeMessageProcessingKey(in.IdempotencyKey, "completion")
	if err != nil {
		return CompleteMessageResult{}, err
	}
	kind := strings.TrimSpace(in.Kind)
	if kind == "" {
		kind = "result"
	}
	draft, err := normalizeSendMessageInput(SendMessageInput{
		ToAgent: "parent", Subject: in.Subject, Kind: kind,
		Body: in.Body, Payload: in.Payload,
	})
	if err != nil {
		return CompleteMessageResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CompleteMessageResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return CompleteMessageResult{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return CompleteMessageResult{}, err
	}
	parent, err := lockMessageProcessingDelivery(ctx, tx, p, messageID, false)
	if err != nil {
		return CompleteMessageResult{}, err
	}
	var storedCompleteKeyHash string
	if err := tx.QueryRow(ctx, `
		SELECT complete_key_hash FROM agent_message_deliveries
		WHERE message_id=$1 AND recipient_agent_id=$2`, messageID, p.ID).
		Scan(&storedCompleteKeyHash); err != nil {
		return CompleteMessageResult{}, fmt.Errorf("read message completion key: %w", err)
	}
	if parent.Processing.State == MessageProcessingCompleted {
		if storedCompleteKeyHash != completeKeyHash {
			return CompleteMessageResult{}, ErrMessageConflict
		}
		if parent.Processing.ClaimID != fence.ClaimID ||
			parent.Processing.Generation != fence.ProcessingGeneration {
			return CompleteMessageResult{}, ErrMessageClaimLost
		}
		result, err := messageByScopedID(ctx, tx, p.AccountID, p.RealmID,
			parent.Processing.ResultMessageID, true)
		if err != nil {
			return CompleteMessageResult{}, fmt.Errorf("read completed result message: %w", err)
		}
		if !messageMatchesCompletion(result, draft) {
			return CompleteMessageResult{}, ErrMessageConflict
		}
		return CompleteMessageResult{
			Processing: parent.Processing, ResultMessage: redactMessageProcessingFence(result),
		}, nil
	}
	if parent.Processing.State != MessageProcessingClaimed ||
		parent.Processing.ClaimID != fence.ClaimID ||
		parent.Processing.Generation != fence.ProcessingGeneration {
		return CompleteMessageResult{}, ErrMessageClaimLost
	}
	var leaseLive bool
	if err := tx.QueryRow(ctx, `
		SELECT lease_expires_at > clock_timestamp()
		FROM agent_message_deliveries
		WHERE message_id=$1 AND recipient_agent_id=$2`, messageID, p.ID).Scan(&leaseLive); err != nil {
		return CompleteMessageResult{}, fmt.Errorf("verify message claim lease: %w", err)
	}
	if !leaseLive {
		return CompleteMessageResult{}, ErrMessageClaimLost
	}

	to, recipientDeleted, err := resolveCompletionResultRecipient(
		ctx, tx, p.AccountID, p.RealmID, parent.From.ID,
	)
	if err != nil {
		return CompleteMessageResult{}, err
	}
	draft.ToAgent = to.ID
	draft.ThreadID = parent.ThreadID
	draft.ReplyToMessageID = parent.ID
	deliveryState := MessageDeliveryDelivered
	if recipientDeleted {
		deliveryState = MessageDeliveryFailed
	}
	result, err := s.insertMessageWithDeliveryTx(ctx, tx, p, to, draft, deliveryState)
	if err != nil {
		return CompleteMessageResult{}, err
	}
	var completedAt time.Time
	err = tx.QueryRow(ctx, `
		UPDATE agent_message_deliveries
		SET processing_state='completed', lease_expires_at=NULL,
		    completed_at=clock_timestamp(), complete_key_hash=$5,
		    result_message_id=$6
		WHERE message_id=$1 AND recipient_agent_id=$2
		  AND processing_state='claimed' AND claim_id=$3
		  AND processing_generation=$4 AND lease_expires_at > clock_timestamp()
		RETURNING completed_at`, messageID, p.ID, fence.ClaimID,
		fence.ProcessingGeneration, completeKeyHash, result.ID).Scan(&completedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CompleteMessageResult{}, ErrMessageClaimLost
	}
	if err != nil {
		return CompleteMessageResult{}, fmt.Errorf("complete message claim: %w", err)
	}
	parent.Processing = MessageProcessing{
		State: MessageProcessingCompleted, Generation: fence.ProcessingGeneration,
		FailureCount: parent.Processing.FailureCount,
		ClaimID:      fence.ClaimID, CompletedAt: &completedAt, ResultMessageID: result.ID,
	}
	if err := logMessageProcessingEvent(ctx, tx, VerbMessageProcessingCompleted, p.ID, parent, result.ID); err != nil {
		return CompleteMessageResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CompleteMessageResult{}, err
	}
	return CompleteMessageResult{
		Processing: parent.Processing, ResultMessage: redactMessageProcessingFence(result),
	}, nil
}

func (s *Store) insertMessageTx(ctx context.Context, tx pgx.Tx, p Principal, to Agent, in SendMessageInput) (Message, error) {
	return s.insertMessageWithDeliveryTx(ctx, tx, p, to, in, MessageDeliveryDelivered)
}

// insertMessageWithDeliveryTx is deliberately private to the fenced
// completion path. Direct sends and ordinary replies always use the delivered
// wrapper above and still require a live recipient.
func (s *Store) insertMessageWithDeliveryTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	to Agent,
	in SendMessageInput,
	deliveryState string,
) (Message, error) {
	if deliveryState != MessageDeliveryDelivered && deliveryState != MessageDeliveryFailed {
		return Message{}, fmt.Errorf("%w: unsupported message delivery state", ErrMessageInputInvalid)
	}
	var err error
	if in.IdempotencyKey != "" {
		existing, findErr := messageByIdempotencyKey(ctx, tx, p.AccountID, p.ID, in.IdempotencyKey)
		switch {
		case findErr == nil:
			if !messageMatchesSend(existing, to.ID, in) {
				return Message{}, ErrMessageConflict
			}
			return existing, nil
		case !errors.Is(findErr, pgx.ErrNoRows):
			return Message{}, fmt.Errorf("find message retry: %w", findErr)
		}
	}

	causalDepth := int64(1)
	if in.ReplyToMessageID != "" {
		var parentDepth int64
		err := tx.QueryRow(ctx, `
			SELECT causal_depth
			FROM agent_messages
			WHERE id=$1 AND account_id=$2 AND realm_id=$3 AND thread_id=$4
			  AND from_agent_id=$5 AND to_agent_id=$6
			FOR SHARE`, in.ReplyToMessageID, p.AccountID, p.RealmID,
			in.ThreadID, to.ID, p.ID).Scan(&parentDepth)
		if errors.Is(err, pgx.ErrNoRows) {
			return Message{}, ErrMessageNotFound
		}
		if err != nil {
			return Message{}, fmt.Errorf("derive message causal depth: %w", err)
		}
		if parentDepth < 1 || parentDepth >= maxMessageCausalDepth {
			return Message{}, fmt.Errorf("%w: message causal depth exhausted", ErrMessageConflict)
		}
		causalDepth = parentDepth + 1
	}

	threadID := in.ThreadID
	if threadID == "" {
		threadID, err = id.New("thr")
		if err != nil {
			return Message{}, err
		}
	}
	messageID, err := id.New("msg")
	if err != nil {
		return Message{}, err
	}
	var payload any
	if len(in.Payload) > 0 {
		payload = string(in.Payload)
	}
	var createdAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO agent_messages
		  (id, account_id, realm_id, from_agent_id, to_agent_id,
		   subject, kind, body, payload, thread_id, reply_to_message_id,
		   idempotency_key, causal_depth)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
		        CASE WHEN $9::text IS NULL THEN NULL ELSE $9::jsonb END,
		        $10, NULLIF($11, ''), NULLIF($12, ''), $13)
		ON CONFLICT (account_id, from_agent_id, idempotency_key) DO NOTHING
		RETURNING created_at`,
		messageID, p.AccountID, p.RealmID, p.ID, to.ID,
		in.Subject, in.Kind, in.Body, payload, threadID, in.ReplyToMessageID,
		in.IdempotencyKey, causalDepth).Scan(&createdAt)
	if errors.Is(err, pgx.ErrNoRows) && in.IdempotencyKey != "" {
		existing, findErr := messageByIdempotencyKey(ctx, tx, p.AccountID, p.ID, in.IdempotencyKey)
		if findErr != nil {
			return Message{}, fmt.Errorf("find concurrent message retry: %w", findErr)
		}
		if !messageMatchesSend(existing, to.ID, in) {
			return Message{}, ErrMessageConflict
		}
		return existing, nil
	}
	if err != nil {
		return Message{}, fmt.Errorf("insert message: %w", err)
	}

	var delivery MessageDelivery
	err = tx.QueryRow(ctx, `
		INSERT INTO agent_message_deliveries
		  (message_id, account_id, realm_id, recipient_agent_id, state, delivered_at)
		VALUES ($1, $2, $3, $4, $5,
		        CASE WHEN $5 = 'delivered' THEN clock_timestamp() ELSE NULL END)
		RETURNING state, delivered_at`, messageID, p.AccountID, p.RealmID, to.ID, deliveryState).
		Scan(&delivery.State, &delivery.DeliveredAt)
	if err != nil {
		return Message{}, fmt.Errorf("insert message delivery: %w", err)
	}

	msg := Message{
		ID: messageID, AccountID: p.AccountID, RealmID: p.RealmID,
		From:    MessageAgent{ID: p.ID, Name: p.AgentName},
		To:      MessageRecipient{Kind: MessageRecipientAgent, ID: to.ID, Name: to.Name},
		Subject: in.Subject, Kind: in.Kind, Body: in.Body, Payload: in.Payload,
		ThreadID: threadID, ReplyToMessageID: in.ReplyToMessageID, CausalDepth: causalDepth,
		CreatedAt: createdAt, Delivery: delivery,
		ReadState:  MessageReadState{State: MessageReadUnread},
		Processing: MessageProcessing{State: MessageProcessingAvailable},
	}
	if err := logMessageEvent(ctx, tx, VerbMessageSent, ActorAgent, p.ID, msg); err != nil {
		return Message{}, err
	}
	deliveryVerb := VerbMessageDelivered
	if delivery.State == MessageDeliveryFailed {
		deliveryVerb = VerbMessageDeliveryFailed
	}
	if err := logMessageEvent(ctx, tx, deliveryVerb, ActorSystem, "", msg); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// ListMessages returns metadata-only mailbox rows; listing does not mark read.
func (s *Store) ListMessages(ctx context.Context, p Principal, filter MessageFilter) (MessagePage, error) {
	if p.Kind != PrincipalAgent {
		return MessagePage{}, ErrMessageForbidden
	}
	filter, cursorTime, cursorID, err := normalizeMessageFilter(filter)
	if err != nil {
		return MessagePage{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MessagePage{}, fmt.Errorf("begin message mailbox snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// A listen request authenticates once and may poll repeatedly. Revalidate its
	// cached principal in every short mailbox snapshot so an agent deletion or
	// account suspension committed between polls immediately closes metadata
	// access on the next query.
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return MessagePage{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MessagePage{}, err
	}

	args := []any{p.AccountID, p.RealmID, p.ID}
	q := &strings.Builder{}
	q.WriteString(messageSelect(false))
	if filter.Direction == MessageDirectionInbox {
		q.WriteString(` WHERE d.account_id = $1 AND d.realm_id = $2 AND d.recipient_agent_id = $3
			AND m.account_id = $1 AND m.realm_id = $2 AND m.to_agent_id = $3`)
	} else {
		q.WriteString(` WHERE m.account_id = $1 AND m.realm_id = $2 AND m.from_agent_id = $3`)
	}
	if filter.Unread {
		q.WriteString(` AND d.read_at IS NULL`)
	}
	if filter.Unacked {
		q.WriteString(` AND d.acked_at IS NULL`)
	}
	if filter.From != "" {
		appendMessageFromFilter(q, &args, filter.From)
	}
	if filter.ThreadID != "" {
		args = append(args, filter.ThreadID)
		fmt.Fprintf(q, ` AND m.thread_id = $%d`, len(args))
	}
	if filter.Kind != "" {
		args = append(args, filter.Kind)
		fmt.Fprintf(q, ` AND m.kind = $%d`, len(args))
	}
	if !cursorTime.IsZero() {
		args = append(args, cursorTime, cursorID)
		comparison := "<"
		if filter.OldestFirst {
			comparison = ">"
		}
		fmt.Fprintf(q, ` AND (m.created_at, m.id) %s ($%d, $%d)`, comparison, len(args)-1, len(args))
	}
	args = append(args, filter.Limit+1)
	order := "DESC"
	if filter.OldestFirst {
		order = "ASC"
	}
	fmt.Fprintf(q, ` ORDER BY m.created_at %s, m.id %s LIMIT $%d`, order, order, len(args))

	rows, err := tx.Query(ctx, q.String(), args...)
	if err != nil {
		return MessagePage{}, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	out := make([]Message, 0, filter.Limit)
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return MessagePage{}, err
		}
		out = append(out, redactMessageProcessingFence(msg))
	}
	if err := rows.Err(); err != nil {
		return MessagePage{}, err
	}
	var next string
	if len(out) > filter.Limit {
		last := out[filter.Limit-1]
		next = encodeMessageCursor(last.CreatedAt, last.ID)
		out = out[:filter.Limit]
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return MessagePage{}, fmt.Errorf("commit message mailbox snapshot: %w", err)
	}
	return MessagePage{Messages: out, NextCursor: next}, nil
}

// ReadMessage returns content to the recipient and idempotently marks it read.
func (s *Store) ReadMessage(ctx context.Context, p Principal, messageID string) (Message, error) {
	return s.transitionMessage(ctx, p, strings.TrimSpace(messageID), false)
}

// AckMessage idempotently marks the recipient delivery read and acknowledged.
func (s *Store) AckMessage(ctx context.Context, p Principal, messageID string) (Message, error) {
	return s.transitionMessage(ctx, p, strings.TrimSpace(messageID), true)
}

func (s *Store) transitionMessage(ctx context.Context, p Principal, messageID string, ack bool) (Message, error) {
	if p.Kind != PrincipalAgent {
		return Message{}, ErrMessageForbidden
	}
	if messageID == "" {
		return Message{}, fmt.Errorf("%w: message id is required", ErrMessageInputInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return Message{}, err
	}
	if err := lockLiveMessageAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return Message{}, err
	}

	// Ack is intentionally metadata-only. Reading untrusted body/payload is a
	// separate explicit operation and must not happen as a side effect of
	// acknowledging work discovered through list/listen.
	row := tx.QueryRow(ctx, messageSelect(!ack)+`
		WHERE m.id = $1 AND d.account_id = $2 AND d.realm_id = $3
		  AND d.recipient_agent_id = $4
		FOR UPDATE OF d`, messageID, p.AccountID, p.RealmID, p.ID)
	msg, err := scanMessage(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrMessageNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("lock message delivery: %w", err)
	}
	if ack && msg.Processing.State == MessageProcessingClaimed {
		return Message{}, ErrMessageBusy
	}
	wasUnread := msg.ReadState.ReadAt == nil
	wasUnacked := msg.ReadState.AckedAt == nil
	if ack {
		err = tx.QueryRow(ctx, `
			UPDATE agent_message_deliveries
			SET read_at = COALESCE(read_at, now()), acked_at = COALESCE(acked_at, now())
			WHERE message_id = $1 AND recipient_agent_id = $2
			RETURNING read_at, acked_at`, messageID, p.ID).
			Scan(&msg.ReadState.ReadAt, &msg.ReadState.AckedAt)
	} else {
		err = tx.QueryRow(ctx, `
			UPDATE agent_message_deliveries
			SET read_at = COALESCE(read_at, now())
			WHERE message_id = $1 AND recipient_agent_id = $2
			RETURNING read_at, acked_at`, messageID, p.ID).
			Scan(&msg.ReadState.ReadAt, &msg.ReadState.AckedAt)
	}
	if err != nil {
		return Message{}, fmt.Errorf("advance message state: %w", err)
	}
	msg.ReadState.State = readState(msg.ReadState.ReadAt, msg.ReadState.AckedAt)
	if wasUnread {
		if err := logMessageEvent(ctx, tx, VerbMessageRead, ActorAgent, p.ID, msg); err != nil {
			return Message{}, err
		}
	}
	if ack && wasUnacked {
		if err := logMessageEvent(ctx, tx, VerbMessageAcked, ActorAgent, p.ID, msg); err != nil {
			return Message{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, err
	}
	return redactMessageProcessingFence(msg), nil
}

func normalizeSendMessageInput(in SendMessageInput) (SendMessageInput, error) {
	in.ToAgent = strings.TrimSpace(in.ToAgent)
	in.Subject = strings.TrimSpace(in.Subject)
	in.Kind = strings.TrimSpace(in.Kind)
	in.ThreadID = strings.TrimSpace(in.ThreadID)
	in.ReplyToMessageID = strings.TrimSpace(in.ReplyToMessageID)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.Kind == "" {
		// Ordinary sends are work requests in the autonomous messaging surface.
		// Callers must opt into note when they intentionally want terminal FYI
		// delivery that a runner records without invoking inference.
		in.Kind = "request"
	}
	switch {
	case in.ToAgent == "":
		return SendMessageInput{}, fmt.Errorf("%w: recipient is required", ErrMessageInputInvalid)
	case len(in.ToAgent) > maxMessageRecipientBytes:
		return SendMessageInput{}, fmt.Errorf("%w: recipient exceeds %d bytes", ErrMessageInputInvalid, maxMessageRecipientBytes)
	case len(in.Subject) > maxMessageSubjectBytes:
		return SendMessageInput{}, fmt.Errorf("%w: subject exceeds %d bytes", ErrMessageInputInvalid, maxMessageSubjectBytes)
	case len(in.Kind) > maxMessageKindBytes:
		return SendMessageInput{}, fmt.Errorf("%w: kind exceeds %d bytes", ErrMessageInputInvalid, maxMessageKindBytes)
	case strings.TrimSpace(in.Body) == "":
		return SendMessageInput{}, fmt.Errorf("%w: body is required", ErrMessageInputInvalid)
	case len(in.Body) > maxMessageBodyBytes:
		return SendMessageInput{}, fmt.Errorf("%w: body exceeds %d bytes", ErrMessageInputInvalid, maxMessageBodyBytes)
	case in.ThreadID != "" && (!strings.HasPrefix(in.ThreadID, "thr_") || len(in.ThreadID) > maxMessageThreadIDBytes):
		return SendMessageInput{}, fmt.Errorf("%w: thread_id must be a thr_ id no longer than %d bytes", ErrMessageInputInvalid, maxMessageThreadIDBytes)
	case in.ReplyToMessageID != "" && (!strings.HasPrefix(in.ReplyToMessageID, "msg_") || len(in.ReplyToMessageID) > maxMessageReplyIDBytes):
		return SendMessageInput{}, fmt.Errorf("%w: reply_to_message_id must be a msg_ id no longer than %d bytes", ErrMessageInputInvalid, maxMessageReplyIDBytes)
	case len(in.IdempotencyKey) > maxMessageIdempotencyKeyBytes:
		return SendMessageInput{}, fmt.Errorf("%w: idempotency key exceeds %d bytes", ErrMessageInputInvalid, maxMessageIdempotencyKeyBytes)
	}
	if strings.TrimSpace(string(in.Payload)) == "null" {
		in.Payload = nil
	}
	payload, err := normalizeJSONObject(in.Payload, false)
	if err != nil {
		return SendMessageInput{}, fmt.Errorf("%w: payload %v", ErrMessageInputInvalid, err)
	}
	if len(payload) > maxMessagePayloadBytes {
		return SendMessageInput{}, fmt.Errorf("%w: payload exceeds %d bytes", ErrMessageInputInvalid, maxMessagePayloadBytes)
	}
	in.Payload = payload
	return in, nil
}

func normalizeMessageFilter(filter MessageFilter) (MessageFilter, time.Time, string, error) {
	filter.Direction = strings.TrimSpace(filter.Direction)
	filter.From = strings.TrimSpace(filter.From)
	filter.ThreadID = strings.TrimSpace(filter.ThreadID)
	filter.Kind = strings.TrimSpace(filter.Kind)
	if filter.Direction == "" {
		filter.Direction = MessageDirectionInbox
	}
	if filter.Direction != MessageDirectionInbox && filter.Direction != MessageDirectionOutbox {
		return MessageFilter{}, time.Time{}, "", fmt.Errorf("%w: direction must be inbox or outbox", ErrMessageInputInvalid)
	}
	if filter.Direction == MessageDirectionOutbox && (filter.Unread || filter.Unacked || filter.From != "") {
		return MessageFilter{}, time.Time{}, "", fmt.Errorf("%w: unread/unacked/from filters apply only to inbox", ErrMessageInputInvalid)
	}
	if filter.OldestFirst && filter.Cursor != "" {
		return MessageFilter{}, time.Time{}, "", fmt.Errorf("%w: oldest-first receive does not accept a pagination cursor", ErrMessageInputInvalid)
	}
	if filter.ThreadID != "" && (!strings.HasPrefix(filter.ThreadID, "thr_") || len(filter.ThreadID) > maxMessageThreadIDBytes) {
		return MessageFilter{}, time.Time{}, "", fmt.Errorf("%w: invalid thread_id", ErrMessageInputInvalid)
	}
	if filter.Limit == 0 {
		filter.Limit = defaultMessagePageSize
	}
	if filter.Limit < 1 || filter.Limit > maxMessagePageSize {
		return MessageFilter{}, time.Time{}, "", fmt.Errorf("%w: limit must be 1-%d", ErrMessageInputInvalid, maxMessagePageSize)
	}
	if filter.Cursor == "" {
		return filter, time.Time{}, "", nil
	}
	cursorTime, cursorID, err := decodeMessageCursor(filter.Cursor)
	if err != nil {
		return MessageFilter{}, time.Time{}, "", err
	}
	return filter, cursorTime, cursorID, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanMessage(row rowScanner) (Message, error) {
	var msg Message
	if err := row.Scan(
		&msg.ID, &msg.AccountID, &msg.RealmID,
		&msg.From.ID, &msg.From.Name, &msg.To.ID, &msg.To.Name,
		&msg.Subject, &msg.Kind, &msg.Body, &msg.Payload, &msg.ThreadID, &msg.CreatedAt,
		&msg.ReplyToMessageID, &msg.CausalDepth,
		&msg.Delivery.State, &msg.Delivery.DeliveredAt,
		&msg.ReadState.ReadAt, &msg.ReadState.AckedAt,
		&msg.Processing.State, &msg.Processing.Generation,
		&msg.Processing.FailureCount,
		&msg.Processing.ClaimID, &msg.Processing.LeaseExpiresAt,
		&msg.Processing.CompletedAt, &msg.Processing.ResultMessageID,
	); err != nil {
		return Message{}, err
	}
	msg.To.Kind = MessageRecipientAgent
	msg.ReadState.State = readState(msg.ReadState.ReadAt, msg.ReadState.AckedAt)
	return msg, nil
}

func messageSelect(includeContent bool) string {
	body := `''::text`
	payload := `NULL::jsonb`
	if includeContent {
		body = `m.body`
		payload = `m.payload`
	}
	return fmt.Sprintf(`
		SELECT m.id, m.account_id, m.realm_id,
		       m.from_agent_id, sender.name, m.to_agent_id, recipient.name,
		       m.subject, m.kind, %s, %s, m.thread_id, m.created_at,
		       COALESCE(m.reply_to_message_id, ''), m.causal_depth,
		       d.state, d.delivered_at, d.read_at, d.acked_at,
		       d.processing_state, d.processing_generation, d.failure_count,
		       COALESCE(d.claim_id, ''), d.lease_expires_at,
		       d.completed_at, COALESCE(d.result_message_id, '')
		FROM agent_messages m
		JOIN agents sender ON sender.id = m.from_agent_id
		JOIN agents recipient ON recipient.id = m.to_agent_id
		JOIN agent_message_deliveries d
		  ON d.message_id = m.id AND d.recipient_agent_id = m.to_agent_id
	`, body, payload)
}

func resolveMessageAgent(ctx context.Context, tx pgx.Tx, accountID, realmID, selector string) (Agent, error) {
	var agent Agent
	idSelector := messageAgentSelectorIsID(selector)
	err := tx.QueryRow(ctx, `
		SELECT a.id, a.name
		FROM agents a
		JOIN realms r ON r.id = a.realm_id
		WHERE r.account_id = $1 AND a.realm_id = $2
		  AND a.deleted_at IS NULL AND r.deleted_at IS NULL
		  AND (a.id = $3 OR (NOT $4 AND a.name = $3))
		ORDER BY CASE WHEN a.id = $3 THEN 0 ELSE 1 END
		LIMIT 1
		FOR SHARE OF a`, accountID, realmID, selector, idSelector).Scan(&agent.ID, &agent.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrMessageRecipientMissing
	}
	if err != nil {
		return Agent{}, fmt.Errorf("resolve message recipient: %w", err)
	}
	return agent, nil
}

func messageAgentSelectorIsID(selector string) bool {
	return strings.HasPrefix(selector, "agent_")
}

// appendMessageFromFilter keeps the lowercase agent_ namespace exact across
// both routing and retrieval. Without the guard, a stale agent id could also
// match a different live or deleted sender whose name happens to equal that id.
func appendMessageFromFilter(q *strings.Builder, args *[]any, selector string) {
	*args = append(*args, selector)
	parameter := len(*args)
	if messageAgentSelectorIsID(selector) {
		fmt.Fprintf(q, ` AND m.from_agent_id = $%d`, parameter)
		return
	}
	fmt.Fprintf(q, ` AND (m.from_agent_id = $%d OR sender.name = $%d)`, parameter, parameter)
}

// lockLiveMessageAgentScope serializes every messaging operation with realm or
// agent deletion. Callers lock the account first, preserving the repository's
// account -> agent lock order. A deletion that committed first fails this
// live-row query; one that starts later waits until the message transaction is
// complete. Locking the live caller agent is also sufficient to keep its realm
// live: DeleteRealm refuses a realm while any live agent remains. Deliberately
// do not lock the joined realm here, because DeleteAgent's existing join may
// lock its target agent before that realm and the inverse order could deadlock.
func lockLiveMessageAgentScope(ctx context.Context, tx pgx.Tx, accountID, realmID, agentID string) error {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT true FROM agents a
		JOIN realms r ON r.id = a.realm_id
		WHERE a.id = $1 AND a.realm_id = $2 AND r.account_id = $3
		  AND a.deleted_at IS NULL AND r.deleted_at IS NULL
		FOR SHARE OF a`, agentID, realmID, accountID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAgentNotFound
	}
	if err != nil {
		return fmt.Errorf("lock live message agent: %w", err)
	}
	return nil
}

// resolveCompletionResultRecipient is the sole deleted-recipient exception.
// The target id is derived from a locked parent message, never caller input;
// a tombstone produces a failed delivery while preserving the terminal reply
// and causal graph for audit and archive portability.
func resolveCompletionResultRecipient(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	realmID string,
	agentID string,
) (Agent, bool, error) {
	var agent Agent
	var deleted bool
	err := tx.QueryRow(ctx, `
		SELECT a.id, a.name, a.deleted_at IS NOT NULL
		FROM agents a
		JOIN realms r ON r.id = a.realm_id
			WHERE r.account_id = $1 AND a.realm_id = $2 AND a.id = $3
			  AND r.deleted_at IS NULL
			FOR SHARE OF a`, accountID, realmID, agentID).
		Scan(&agent.ID, &agent.Name, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, false, ErrMessageRecipientMissing
	}
	if err != nil {
		return Agent{}, false, fmt.Errorf("resolve completion result recipient: %w", err)
	}
	return agent, deleted, nil
}

func messageByIdempotencyKey(ctx context.Context, tx pgx.Tx, accountID, senderID, key string) (Message, error) {
	return scanMessage(tx.QueryRow(ctx, messageSelect(true)+`
		WHERE m.account_id = $1 AND m.from_agent_id = $2 AND m.idempotency_key = $3`,
		accountID, senderID, key))
}

// messageReplayByIdempotencyKey runs before live recipient resolution. A
// successful send or reply remains replayable after its target becomes a
// tombstone, while the caller itself is still revalidated and locked above.
// New operations continue through live recipient resolution as usual.
func messageReplayByIdempotencyKey(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	key string,
) (Message, bool, error) {
	if key == "" {
		return Message{}, false, nil
	}
	existing, err := messageByIdempotencyKey(ctx, tx, p.AccountID, p.ID, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, fmt.Errorf("find message retry: %w", err)
	}
	return existing, true, nil
}

func messageByScopedID(ctx context.Context, tx pgx.Tx, accountID, realmID, messageID string, includeContent bool) (Message, error) {
	return scanMessage(tx.QueryRow(ctx, messageSelect(includeContent)+`
		WHERE m.id=$1 AND m.account_id=$2 AND m.realm_id=$3`, messageID, accountID, realmID))
}

func lockMessageProcessingDelivery(ctx context.Context, tx pgx.Tx, p Principal, messageID string, requireUnacked bool) (Message, error) {
	query := messageSelect(false) + `
		WHERE m.id=$1 AND d.account_id=$2 AND d.realm_id=$3
		  AND d.recipient_agent_id=$4`
	if requireUnacked {
		query += ` AND d.acked_at IS NULL`
	}
	query += ` FOR UPDATE OF d`
	msg, err := scanMessage(tx.QueryRow(ctx, query, messageID, p.AccountID, p.RealmID, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrMessageNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("lock message processing delivery: %w", err)
	}
	return msg, nil
}

func normalizeProcessingMessageID(messageID string) (string, error) {
	messageID = strings.TrimSpace(messageID)
	if !strings.HasPrefix(messageID, "msg_") || len(messageID) > maxMessageReplyIDBytes {
		return "", fmt.Errorf("%w: message id must be a msg_ id no longer than %d bytes", ErrMessageInputInvalid, maxMessageReplyIDBytes)
	}
	return messageID, nil
}

func normalizeMessageLeaseDuration(lease time.Duration) (time.Duration, error) {
	if lease == 0 {
		lease = defaultMessageLeaseDuration
	}
	if lease < minMessageLeaseDuration || lease > maxMessageLeaseDuration {
		return 0, fmt.Errorf("%w: lease duration must be between %s and %s", ErrMessageInputInvalid, minMessageLeaseDuration, maxMessageLeaseDuration)
	}
	return lease, nil
}

func normalizeMessageClaimFence(fence MessageClaimFence) (MessageClaimFence, error) {
	fence.ClaimID = strings.TrimSpace(fence.ClaimID)
	if !strings.HasPrefix(fence.ClaimID, "mcl_") || len(fence.ClaimID) > maxMessageClaimIDBytes {
		return MessageClaimFence{}, fmt.Errorf("%w: claim_id must be an mcl_ id no longer than %d bytes", ErrMessageInputInvalid, maxMessageClaimIDBytes)
	}
	if fence.ProcessingGeneration < 1 || fence.ProcessingGeneration > maxMessageProcessingGeneration {
		return MessageClaimFence{}, fmt.Errorf("%w: processing_generation is invalid", ErrMessageInputInvalid)
	}
	return fence, nil
}

func normalizeMessageProcessingKey(key, operation string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("%w: %s idempotency key is required", ErrMessageInputInvalid, operation)
	}
	if len(key) > maxMessageIdempotencyKeyBytes {
		return "", fmt.Errorf("%w: %s idempotency key exceeds %d bytes", ErrMessageInputInvalid, operation, maxMessageIdempotencyKeyBytes)
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]), nil
}

func messageMatchesSend(msg Message, toAgentID string, in SendMessageInput) bool {
	threadMatches := in.ThreadID == "" || msg.ThreadID == in.ThreadID
	return msg.To.ID == toAgentID && msg.Subject == in.Subject && msg.Kind == in.Kind &&
		msg.Body == in.Body && rawJSONEqual(msg.Payload, in.Payload) && threadMatches &&
		msg.ReplyToMessageID == in.ReplyToMessageID
}

func messageMatchesDirectSendReplay(msg Message, in SendMessageInput) bool {
	recipientMatches := msg.To.ID == in.ToAgent
	if !messageAgentSelectorIsID(in.ToAgent) {
		recipientMatches = recipientMatches || msg.To.Name == in.ToAgent
	}
	return recipientMatches && messageMatchesSend(msg, msg.To.ID, in)
}

func messageMatchesReplyReplay(msg Message, parentMessageID string, in SendMessageInput) bool {
	return msg.ReplyToMessageID == parentMessageID && messageMatchesCompletion(msg, in)
}

func messageMatchesCompletion(result Message, draft SendMessageInput) bool {
	return result.Subject == draft.Subject && result.Kind == draft.Kind &&
		result.Body == draft.Body && rawJSONEqual(result.Payload, draft.Payload)
}

// redactMessageProcessingFence keeps general message projections useful for
// state inspection without exposing the claim capability. The dedicated
// claim/renew/release results return the full fence directly.
func redactMessageProcessingFence(msg Message) Message {
	msg.Processing.ClaimID = ""
	msg.Processing.LeaseExpiresAt = nil
	return msg
}

func readState(readAt, ackedAt *time.Time) string {
	if ackedAt != nil {
		return MessageReadAcked
	}
	if readAt != nil {
		return MessageReadRead
	}
	return MessageReadUnread
}

func logMessageEvent(ctx context.Context, tx pgx.Tx, verb, actorKind, actorID string, msg Message) error {
	return logEventTx(ctx, tx, EventInput{
		AccountID: msg.AccountID, ActorKind: actorKind, ActorID: actorID,
		Verb: verb, Metadata: messageEventMetadata(msg),
	})
}

func logMessageProcessingEvent(ctx context.Context, tx pgx.Tx, verb, actorID string, msg Message, resultMessageID string) error {
	metadata := messageEventMetadata(msg)
	metadata["processing_generation"] = strconv.FormatInt(msg.Processing.Generation, 10)
	metadata["failure_count"] = strconv.FormatInt(msg.Processing.FailureCount, 10)
	if resultMessageID != "" {
		metadata["result_message_id"] = resultMessageID
	}
	return logEventTx(ctx, tx, EventInput{
		AccountID: msg.AccountID, ActorKind: ActorAgent, ActorID: actorID,
		Verb: verb, Metadata: metadata,
	})
}

func messageEventMetadata(msg Message) map[string]any {
	metadata := map[string]any{
		"message_id": msg.ID, "from_agent_id": msg.From.ID,
		"recipient_kind": MessageRecipientAgent, "recipient_agent_id": msg.To.ID,
		"kind": msg.Kind, "thread_id": msg.ThreadID,
		"subject_present": msg.Subject != "",
	}
	if msg.CausalDepth > 0 {
		metadata["causal_depth"] = strconv.FormatInt(msg.CausalDepth, 10)
	}
	if msg.ReplyToMessageID != "" {
		metadata["reply_to_message_id"] = msg.ReplyToMessageID
	}
	return metadata
}

func encodeMessageCursor(t time.Time, messageID string) string {
	return fmt.Sprintf("%d:%s", t.UnixNano(), messageID)
}

func decodeMessageCursor(cursor string) (time.Time, string, error) {
	i := strings.IndexByte(cursor, ':')
	if i <= 0 || i == len(cursor)-1 {
		return time.Time{}, "", ErrMessageCursorInvalid
	}
	tsPart := cursor[:i]
	for _, r := range tsPart {
		if r < '0' || r > '9' {
			return time.Time{}, "", ErrMessageCursorInvalid
		}
	}
	ns, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil || !strings.HasPrefix(cursor[i+1:], "msg_") {
		return time.Time{}, "", ErrMessageCursorInvalid
	}
	return time.Unix(0, ns).UTC(), cursor[i+1:], nil
}
