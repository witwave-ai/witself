package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// AgentEmailOutboundProviderEventDelivered is a normalized delivery event.
	AgentEmailOutboundProviderEventDelivered = "delivered"
	// AgentEmailOutboundProviderEventDeferred is a normalized temporary deferral.
	AgentEmailOutboundProviderEventDeferred = "deferred"
	// AgentEmailOutboundProviderEventBounced is a normalized permanent bounce.
	AgentEmailOutboundProviderEventBounced = "bounced"
	// AgentEmailOutboundProviderEventFailed is a normalized terminal failure.
	AgentEmailOutboundProviderEventFailed = "failed"
	// AgentEmailOutboundProviderEventRejected is a normalized rejection.
	AgentEmailOutboundProviderEventRejected = "rejected"
	// AgentEmailOutboundProviderEventComplained is a normalized complaint.
	AgentEmailOutboundProviderEventComplained = "complained"

	maximumAgentEmailOutboundProviderEventIDBytes = 512
)

// AgentEmailOutboundProviderEventInput is a content-free provider lifecycle event.
type AgentEmailOutboundProviderEventInput struct {
	Provider          string
	EventID           string
	ProviderMessageID string
	EventClass        string
	OccurredAt        time.Time
}

// AgentEmailOutboundProviderEventResult reports the folded message state.
type AgentEmailOutboundProviderEventResult struct {
	Message AgentEmailOutboundMessage `json:"message"`
	Applied bool                      `json:"applied"`
}

// ApplyAgentEmailOutboundProviderEvent records one value-free normalized
// provider receipt. The raw event id is hashed before persistence. `bounced`
// is the adapter's hard/permanent-bounce class; soft/transient outcomes must
// arrive as `deferred` and never create suppression.
func (s *Store) ApplyAgentEmailOutboundProviderEvent(
	ctx context.Context,
	in AgentEmailOutboundProviderEventInput,
) (AgentEmailOutboundProviderEventResult, error) {
	if err := normalizeAgentEmailOutboundProviderEvent(&in); err != nil {
		return AgentEmailOutboundProviderEventResult{}, err
	}
	eventIDHash := agentEmailOutboundSHA256(in.EventID)
	requestHash := agentEmailOutboundProviderEventRequestHash(in)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailOutboundProviderEventResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if outboundID, storedHash, found, err := agentEmailOutboundProviderEventReceiptTx(
		ctx, tx, in.Provider, eventIDHash,
	); err != nil {
		return AgentEmailOutboundProviderEventResult{}, err
	} else if found {
		if storedHash != requestHash {
			return AgentEmailOutboundProviderEventResult{}, ErrAgentEmailOutboundConflict
		}
		msg, err := agentEmailOutboundByIDForUpdateTx(ctx, tx, outboundID)
		if err != nil {
			return AgentEmailOutboundProviderEventResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return AgentEmailOutboundProviderEventResult{}, err
		}
		return AgentEmailOutboundProviderEventResult{
			Message: redactAgentEmailOutbound(msg), Applied: false,
		}, nil
	}

	msg, err := agentEmailOutboundByProviderMessageTx(
		ctx, tx, in.Provider, in.ProviderMessageID,
	)
	if err != nil {
		return AgentEmailOutboundProviderEventResult{}, err
	}
	var inserted bool
	var receivedAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO agent_email_outbound_provider_events
		  (account_id,provider,event_id_hash,event_request_hash,outbound_id,
		   event_class,occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (provider,event_id_hash) DO NOTHING
		RETURNING true,received_at`, msg.AccountID, in.Provider, eventIDHash, requestHash,
		msg.ID, in.EventClass, in.OccurredAt).Scan(&inserted, &receivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		outboundID, storedHash, found, receiptErr := agentEmailOutboundProviderEventReceiptTx(
			ctx, tx, in.Provider, eventIDHash,
		)
		if receiptErr != nil {
			return AgentEmailOutboundProviderEventResult{}, receiptErr
		}
		if !found || outboundID != msg.ID || storedHash != requestHash {
			return AgentEmailOutboundProviderEventResult{}, ErrAgentEmailOutboundConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return AgentEmailOutboundProviderEventResult{}, err
		}
		return AgentEmailOutboundProviderEventResult{
			Message: redactAgentEmailOutbound(msg), Applied: false,
		}, nil
	}
	if err != nil {
		return AgentEmailOutboundProviderEventResult{}, fmt.Errorf(
			"record outbound agent-email provider event: %w", err)
	}

	nextState, errorCode, changeState, suppressionReason :=
		agentEmailOutboundProviderEventTransition(msg.State, in.EventClass)
	if changeState {
		// Keep the provider's original timestamp in the immutable event receipt,
		// but never let external clock skew create a message lifecycle timestamp
		// outside the interval from the locally committed provider boundary to
		// the database receipt. Such a row would be operationally valid yet fail
		// the archive/import portability contract.
		lifecycleAt := in.OccurredAt
		lifecycleFloor := msg.CreatedAt
		if msg.ProviderStartedAt != nil && msg.ProviderStartedAt.After(lifecycleFloor) {
			lifecycleFloor = *msg.ProviderStartedAt
		}
		if lifecycleAt.Before(lifecycleFloor) {
			lifecycleAt = lifecycleFloor
		}
		if lifecycleAt.After(receivedAt) {
			lifecycleAt = receivedAt
		}
		msg, err = applyAgentEmailOutboundProviderStateTx(
			ctx, tx, msg, nextState, errorCode, lifecycleAt,
		)
		if err != nil {
			return AgentEmailOutboundProviderEventResult{}, err
		}
	}
	if suppressionReason != "" {
		if err := suppressAgentEmailOutboundRecipientTx(
			ctx, tx, msg, suppressionReason, in.Provider,
		); err != nil {
			return AgentEmailOutboundProviderEventResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailOutboundProviderEventResult{}, err
	}
	return AgentEmailOutboundProviderEventResult{
		Message: redactAgentEmailOutbound(msg), Applied: true,
	}, nil
}

func normalizeAgentEmailOutboundProviderEvent(
	in *AgentEmailOutboundProviderEventInput,
) error {
	provider, err := normalizeAgentEmailOutboundProvider(in.Provider, false)
	if err != nil {
		return err
	}
	in.Provider = provider
	in.EventID = strings.TrimSpace(in.EventID)
	in.ProviderMessageID, err = normalizeAgentEmailOutboundProviderMessageID(
		in.ProviderMessageID, true,
	)
	if err != nil {
		return err
	}
	in.EventClass = strings.TrimSpace(in.EventClass)
	if in.EventID == "" || len(in.EventID) > maximumAgentEmailOutboundProviderEventIDBytes ||
		agentEmailOutboundHasControl(in.EventID) {
		return fmt.Errorf("%w: provider event identity is invalid",
			ErrAgentEmailOutboundInputInvalid)
	}
	switch in.EventClass {
	case AgentEmailOutboundProviderEventDelivered,
		AgentEmailOutboundProviderEventDeferred,
		AgentEmailOutboundProviderEventBounced,
		AgentEmailOutboundProviderEventFailed,
		AgentEmailOutboundProviderEventRejected,
		AgentEmailOutboundProviderEventComplained:
	default:
		return fmt.Errorf("%w: provider event class is invalid",
			ErrAgentEmailOutboundInputInvalid)
	}
	if in.OccurredAt.IsZero() || in.OccurredAt.After(time.Now().UTC().Add(5*time.Minute)) {
		return fmt.Errorf("%w: provider event occurred_at is invalid",
			ErrAgentEmailOutboundInputInvalid)
	}
	in.OccurredAt = in.OccurredAt.UTC()
	return nil
}

func agentEmailOutboundProviderEventRequestHash(
	in AgentEmailOutboundProviderEventInput,
) string {
	canonical, _ := json.Marshal(struct {
		ProviderMessageID string `json:"provider_message_id"`
		EventClass        string `json:"event_class"`
		OccurredAt        string `json:"occurred_at"`
	}{in.ProviderMessageID, in.EventClass, in.OccurredAt.Format(time.RFC3339Nano)})
	return agentEmailOutboundSHA256(string(canonical))
}

func agentEmailOutboundProviderEventReceiptTx(
	ctx context.Context,
	tx pgx.Tx,
	provider, eventIDHash string,
) (string, string, bool, error) {
	var outboundID, requestHash string
	err := tx.QueryRow(ctx, `
		SELECT outbound_id,event_request_hash
		  FROM agent_email_outbound_provider_events
		 WHERE provider=$1 AND event_id_hash=$2`, provider, eventIDHash).
		Scan(&outboundID, &requestHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf(
			"read outbound agent-email provider event receipt: %w", err)
	}
	return outboundID, requestHash, true, nil
}

func agentEmailOutboundByProviderMessageTx(
	ctx context.Context,
	tx pgx.Tx,
	provider, providerMessageID string,
) (AgentEmailOutboundMessage, error) {
	// Resolve the immutable account key without a row lock, then take the
	// account evacuation fence before locking the child. This preserves the
	// global account -> tenant-row lock order and makes BeginAccountEvacuation
	// the linearization point for a concurrent provider event.
	var sendID, accountID string
	err := tx.QueryRow(ctx, `
		SELECT id,account_id FROM agent_email_outbound_messages
		 WHERE provider=$1 AND provider_message_id=$2`,
		provider, providerMessageID).Scan(&sendID, &accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundNotFound
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, fmt.Errorf(
			"resolve outbound provider message: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`SELECT witself_check_account_evacuation_fence($1)`, accountID,
	); err != nil {
		return AgentEmailOutboundMessage{}, fmt.Errorf(
			"fence outbound provider event account: %w", err)
	}
	msg, err := scanAgentEmailOutbound(tx.QueryRow(ctx,
		agentEmailOutboundSelect()+`
		WHERE id=$1 AND account_id=$2 AND provider=$3
		  AND provider_message_id=$4 FOR UPDATE`,
		sendID, accountID, provider, providerMessageID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundNotFound
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, fmt.Errorf(
			"lock outbound provider message: %w", err)
	}
	return msg, nil
}

func agentEmailOutboundProviderEventTransition(
	currentState, eventClass string,
) (nextState, errorCode string, changeState bool, suppressionReason string) {
	switch eventClass {
	case AgentEmailOutboundProviderEventDelivered:
		if currentState == AgentEmailOutboundAccepted ||
			currentState == AgentEmailOutboundDeferred {
			return AgentEmailOutboundDelivered, "", true, ""
		}
	case AgentEmailOutboundProviderEventDeferred:
		if currentState == AgentEmailOutboundAccepted ||
			currentState == AgentEmailOutboundDeferred {
			return AgentEmailOutboundDeferred, "", true, ""
		}
	case AgentEmailOutboundProviderEventBounced:
		if currentState == AgentEmailOutboundAccepted ||
			currentState == AgentEmailOutboundDeferred ||
			currentState == AgentEmailOutboundDelivered {
			return AgentEmailOutboundBounced,
				AgentEmailOutboundErrorRecipientHardBounce, true, "hard_bounce"
		}
		return "", "", false, "hard_bounce"
	case AgentEmailOutboundProviderEventFailed:
		if currentState == AgentEmailOutboundAccepted ||
			currentState == AgentEmailOutboundDeferred {
			return AgentEmailOutboundFailed,
				AgentEmailOutboundErrorProviderFailed, true, ""
		}
	case AgentEmailOutboundProviderEventRejected:
		if currentState == AgentEmailOutboundAccepted ||
			currentState == AgentEmailOutboundDeferred {
			return AgentEmailOutboundRejected,
				AgentEmailOutboundErrorProviderRejected, true, ""
		}
	case AgentEmailOutboundProviderEventComplained:
		if currentState == AgentEmailOutboundAccepted ||
			currentState == AgentEmailOutboundDeferred ||
			currentState == AgentEmailOutboundDelivered ||
			currentState == AgentEmailOutboundBounced {
			return AgentEmailOutboundFailed,
				AgentEmailOutboundErrorRecipientComplained, true, "complained"
		}
		return "", "", false, "complained"
	}
	return "", "", false, ""
}

func applyAgentEmailOutboundProviderStateTx(
	ctx context.Context,
	tx pgx.Tx,
	msg AgentEmailOutboundMessage,
	state, errorCode string,
	occurredAt time.Time,
) (AgentEmailOutboundMessage, error) {
	updated, err := scanAgentEmailOutbound(tx.QueryRow(ctx, `
		UPDATE agent_email_outbound_messages
		   SET state=$2,provider_state=$2,last_error_code=$3,
		       delivered_at=CASE WHEN $2='delivered'
		                         THEN COALESCE(delivered_at,$4) ELSE delivered_at END,
		       deferred_at=CASE WHEN $2='deferred' THEN $4 ELSE deferred_at END,
		       failed_at=CASE WHEN $2 IN ('bounced','rejected','failed')
		                      THEN COALESCE(failed_at,$4) ELSE failed_at END,
		       updated_at=clock_timestamp()
		 WHERE id=$1
		 RETURNING `+agentEmailOutboundReturningColumns(),
		msg.ID, state, errorCode, occurredAt))
	if err != nil {
		return AgentEmailOutboundMessage{}, fmt.Errorf(
			"apply outbound provider state: %w", err)
	}
	return updated, nil
}

func suppressAgentEmailOutboundRecipientTx(
	ctx context.Context,
	tx pgx.Tx,
	msg AgentEmailOutboundMessage,
	reason, provider string,
) error {
	digest := agentEmailOutboundSHA256(msg.ToAddress)
	if err := lockAgentEmailOutboundRecipientTx(ctx, tx, msg.AccountID, digest); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO agent_email_outbound_recipient_suppressions
		  (account_id,recipient_sha256,reason,source_send_id,provider)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (account_id,recipient_sha256) DO UPDATE SET
		  reason=CASE
		    WHEN agent_email_outbound_recipient_suppressions.reason='complained'
		      OR EXCLUDED.reason='complained' THEN 'complained'
		    ELSE 'hard_bounce' END,
		  source_send_id=EXCLUDED.source_send_id,
		  provider=EXCLUDED.provider,updated_at=clock_timestamp()`,
		msg.AccountID, digest, reason, msg.ID, provider)
	if err != nil {
		return fmt.Errorf("suppress outbound agent-email recipient: %w", err)
	}
	return nil
}

func requireAgentEmailOutboundRecipientAllowedTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, recipient string,
) error {
	digest := agentEmailOutboundSHA256(recipient)
	if err := lockAgentEmailOutboundRecipientTx(ctx, tx, accountID, digest); err != nil {
		return err
	}
	var suppressed bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM agent_email_outbound_recipient_suppressions
		   WHERE account_id=$1 AND recipient_sha256=$2
		)`, accountID, digest).Scan(&suppressed)
	if err != nil {
		return fmt.Errorf("check outbound agent-email recipient suppression: %w", err)
	}
	if suppressed {
		return ErrAgentEmailRecipientSuppressed
	}
	return nil
}

// lockAgentEmailOutboundRecipientTx linearizes an absent suppression check
// with a concurrent hard-bounce or complaint insertion. A row lock cannot
// protect a key that does not exist yet, so both paths take the same
// transaction-scoped advisory lock derived only from account and digest.
func lockAgentEmailOutboundRecipientTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, recipientDigest string,
) error {
	const namespace = "witself:agent-email-recipient-suppression:v1:"
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		namespace+accountID+":"+recipientDigest,
	); err != nil {
		return fmt.Errorf("lock outbound agent-email recipient suppression: %w", err)
	}
	return nil
}
