package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// AgentEmailOutboundCloudflareProvider is the normalized provider identity
	// committed by the managed Cloudflare outbound adapter.
	AgentEmailOutboundCloudflareProvider = "cloudflare_email_sending"

	agentEmailProviderEventCanaryMaximumAcceptedAge = 15 * time.Minute
)

var (
	// ErrAgentEmailProviderEventCanaryFence reports that the selected send has
	// neither the exact pristine shape nor the exact deterministic completed
	// shape required by the destructive provider-event proof.
	ErrAgentEmailProviderEventCanaryFence = errors.New(
		"outbound agent-email provider-event canary fence mismatch",
	)
)

// AgentEmailProviderEventCanaryTarget is a private, short-lived fence returned
// by the preflight and consumed by verification. ProviderMessageID must never
// be logged or serialized.
type AgentEmailProviderEventCanaryTarget struct {
	AccountID         string    `json:"-"`
	SendID            string    `json:"-"`
	ProviderMessageID string    `json:"-"`
	AcceptedAt        time.Time `json:"-"`
}

// AgentEmailProviderEventCanaryVerification is the value-free result of the
// postflight. Both counts must be exactly one for a successful proof.
type AgentEmailProviderEventCanaryVerification struct {
	SendID                    string
	State                     string
	ProviderEventReceiptCount int64
	EmailSentUsageEventCount  int64
}

type agentEmailProviderEventCanarySnapshot struct {
	state                      string
	providerState              string
	provider                   string
	providerMessageID          string
	acceptedAt                 *time.Time
	deliveredAt                *time.Time
	databaseNow                time.Time
	providerEventCount         int64
	eventIdentityCount         int64
	matchingCanaryReceiptCount int64
	emailSentUsageCount        int64
	canonicalEmailUsageCount   int64
}

// PrepareAgentEmailProviderEventCanary resolves the provider correlation only
// after proving an exact account/send/accepted-at fence. It is read-only and
// must complete before the caller opens a localhost HTTP listener.
func (s *Store) PrepareAgentEmailProviderEventCanary(
	ctx context.Context,
	accountID, sendID string,
	expectedAcceptedAt time.Time,
	eventID string,
) (AgentEmailProviderEventCanaryTarget, error) {
	if !validAgentEmailGeneratedID(accountID, "acc") ||
		!validAgentEmailGeneratedID(sendID, "esnd") ||
		expectedAcceptedAt.IsZero() ||
		!validAgentEmailProviderEventCanaryEventID(eventID) {
		return AgentEmailProviderEventCanaryTarget{}, ErrAgentEmailOutboundInputInvalid
	}

	snapshot, err := s.agentEmailProviderEventCanarySnapshot(
		ctx, accountID, sendID, eventID,
	)
	if err != nil {
		return AgentEmailProviderEventCanaryTarget{}, err
	}
	if err := validateAgentEmailProviderEventCanaryPreflight(
		snapshot, expectedAcceptedAt,
	); err != nil {
		return AgentEmailProviderEventCanaryTarget{}, err
	}
	return AgentEmailProviderEventCanaryTarget{
		AccountID: accountID, SendID: sendID,
		ProviderMessageID: snapshot.providerMessageID,
		AcceptedAt:        snapshot.acceptedAt.UTC(),
	}, nil
}

// VerifyAgentEmailProviderEventCanary proves the exact synthetic receipt, the
// single pre-existing send-usage debit, and the delivered fold without
// returning provider or event identifiers.
func (s *Store) VerifyAgentEmailProviderEventCanary(
	ctx context.Context,
	target AgentEmailProviderEventCanaryTarget,
	eventID string,
) (AgentEmailProviderEventCanaryVerification, error) {
	if !validAgentEmailGeneratedID(target.AccountID, "acc") ||
		!validAgentEmailGeneratedID(target.SendID, "esnd") ||
		target.AcceptedAt.IsZero() ||
		strings.TrimSpace(target.ProviderMessageID) == "" ||
		!validAgentEmailProviderEventCanaryEventID(eventID) {
		return AgentEmailProviderEventCanaryVerification{}, ErrAgentEmailOutboundInputInvalid
	}
	snapshot, err := s.agentEmailProviderEventCanarySnapshot(
		ctx, target.AccountID, target.SendID, eventID,
	)
	if err != nil {
		return AgentEmailProviderEventCanaryVerification{}, err
	}
	if err := validateAgentEmailProviderEventCanaryPostflight(snapshot, target); err != nil {
		return AgentEmailProviderEventCanaryVerification{}, err
	}
	return AgentEmailProviderEventCanaryVerification{
		SendID: target.SendID, State: snapshot.state,
		ProviderEventReceiptCount: snapshot.providerEventCount,
		EmailSentUsageEventCount:  snapshot.emailSentUsageCount,
	}, nil
}

func (s *Store) agentEmailProviderEventCanarySnapshot(
	ctx context.Context,
	accountID, sendID, eventID string,
) (agentEmailProviderEventCanarySnapshot, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return agentEmailProviderEventCanarySnapshot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var snapshot agentEmailProviderEventCanarySnapshot
	err = tx.QueryRow(ctx, `
		SELECT message.state,message.provider_state,message.provider,
		       message.provider_message_id,message.accepted_at,message.delivered_at,
		       clock_timestamp()
		  FROM agent_email_outbound_messages message
		 WHERE message.account_id=$1 AND message.id=$2`,
		accountID, sendID,
	).Scan(
		&snapshot.state, &snapshot.providerState, &snapshot.provider,
		&snapshot.providerMessageID, &snapshot.acceptedAt, &snapshot.deliveredAt,
		&snapshot.databaseNow,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentEmailProviderEventCanarySnapshot{}, ErrAgentEmailOutboundNotFound
	}
	if err != nil {
		return agentEmailProviderEventCanarySnapshot{}, fmt.Errorf(
			"read outbound agent-email provider-event canary target: %w", err,
		)
	}

	eventIDHash := agentEmailOutboundSHA256(eventID)
	exactRequestHash := ""
	if snapshot.acceptedAt != nil {
		exactRequestHash = agentEmailOutboundProviderEventRequestHash(
			AgentEmailOutboundProviderEventInput{
				Provider: AgentEmailOutboundCloudflareProvider,
				EventID:  eventID, ProviderMessageID: snapshot.providerMessageID,
				EventClass: AgentEmailOutboundProviderEventDelivered,
				OccurredAt: snapshot.acceptedAt.UTC(),
			},
		)
	}
	err = tx.QueryRow(ctx, `
		SELECT
		       (SELECT count(*)
		          FROM agent_email_outbound_provider_events event
		         WHERE event.account_id=message.account_id
		           AND event.outbound_id=message.id),
		       (SELECT count(*)
		          FROM agent_email_outbound_provider_events event
		         WHERE event.provider=$3
		           AND event.event_id_hash=$4),
		       (SELECT count(*)
		          FROM agent_email_outbound_provider_events event
		         WHERE event.account_id=message.account_id
		           AND event.outbound_id=message.id
		           AND event.provider=$3
		           AND event.event_id_hash=$4
		           AND event.event_request_hash=$5
		           AND event.event_class='delivered'
		           AND event.occurred_at=message.accepted_at),
		       (SELECT count(*)
		          FROM usage_events usage
		         WHERE usage.account_id=message.account_id
		           AND usage.dimension=$6
		           AND usage.subject_id=message.id),
		       (SELECT count(*)
		          FROM usage_events usage
		         WHERE usage.account_id=message.account_id
		           AND usage.dimension=$6
		           AND usage.subject_id=message.id
		           AND usage.subject_type='agent_email_outbound'
		           AND usage.quantity=1
		           AND usage.unit=$7
		           AND usage.realm_id=message.realm_id
		           AND usage.agent_id=message.owner_agent_id
		           AND usage.idempotency_key='email_sent:' || message.id
		           AND usage.metadata='{}'::jsonb
		           AND usage.occurred_at=message.accepted_at)
		  FROM agent_email_outbound_messages message
		 WHERE message.account_id=$1 AND message.id=$2`,
		accountID, sendID, AgentEmailOutboundCloudflareProvider, eventIDHash,
		exactRequestHash,
		UsageDimensionEmailSent, UsageUnitEmail,
	).Scan(
		&snapshot.providerEventCount,
		&snapshot.eventIdentityCount,
		&snapshot.matchingCanaryReceiptCount, &snapshot.emailSentUsageCount,
		&snapshot.canonicalEmailUsageCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentEmailProviderEventCanarySnapshot{}, ErrAgentEmailOutboundNotFound
	}
	if err != nil {
		return agentEmailProviderEventCanarySnapshot{}, fmt.Errorf(
			"read outbound agent-email provider-event canary ledgers: %w", err,
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return agentEmailProviderEventCanarySnapshot{}, err
	}
	return snapshot, nil
}

func validateAgentEmailProviderEventCanaryPreflight(
	snapshot agentEmailProviderEventCanarySnapshot,
	expectedAcceptedAt time.Time,
) error {
	if snapshot.provider != AgentEmailOutboundCloudflareProvider ||
		strings.TrimSpace(snapshot.providerMessageID) == "" ||
		snapshot.acceptedAt == nil ||
		!snapshot.acceptedAt.Equal(expectedAcceptedAt) ||
		snapshot.acceptedAt.After(snapshot.databaseNow) ||
		snapshot.acceptedAt.Before(
			snapshot.databaseNow.Add(-agentEmailProviderEventCanaryMaximumAcceptedAge),
		) ||
		snapshot.emailSentUsageCount != 1 ||
		snapshot.canonicalEmailUsageCount != 1 {
		return ErrAgentEmailProviderEventCanaryFence
	}
	pristine := snapshot.state == AgentEmailOutboundAccepted &&
		snapshot.providerState == AgentEmailOutboundAccepted &&
		snapshot.deliveredAt == nil &&
		snapshot.providerEventCount == 0 &&
		snapshot.eventIdentityCount == 0 &&
		snapshot.matchingCanaryReceiptCount == 0
	completed := snapshot.state == AgentEmailOutboundDelivered &&
		snapshot.providerState == AgentEmailOutboundDelivered &&
		snapshot.deliveredAt != nil &&
		snapshot.deliveredAt.Equal(expectedAcceptedAt) &&
		snapshot.providerEventCount == 1 &&
		snapshot.eventIdentityCount == 1 &&
		snapshot.matchingCanaryReceiptCount == 1
	if !pristine && !completed {
		return ErrAgentEmailProviderEventCanaryFence
	}
	return nil
}

func validateAgentEmailProviderEventCanaryPostflight(
	snapshot agentEmailProviderEventCanarySnapshot,
	target AgentEmailProviderEventCanaryTarget,
) error {
	if snapshot.state != AgentEmailOutboundDelivered ||
		snapshot.providerState != AgentEmailOutboundDelivered ||
		snapshot.provider != AgentEmailOutboundCloudflareProvider ||
		snapshot.providerMessageID != target.ProviderMessageID ||
		snapshot.acceptedAt == nil ||
		!snapshot.acceptedAt.Equal(target.AcceptedAt) ||
		snapshot.deliveredAt == nil ||
		!snapshot.deliveredAt.Equal(*snapshot.acceptedAt) ||
		snapshot.providerEventCount != 1 ||
		snapshot.eventIdentityCount != 1 ||
		snapshot.matchingCanaryReceiptCount != 1 ||
		snapshot.emailSentUsageCount != 1 ||
		snapshot.canonicalEmailUsageCount != 1 {
		return ErrAgentEmailProviderEventCanaryFence
	}
	return nil
}

func validAgentEmailProviderEventCanaryEventID(eventID string) bool {
	return strings.TrimSpace(eventID) != "" &&
		eventID == strings.TrimSpace(eventID) &&
		len(eventID) <= maximumAgentEmailOutboundProviderEventIDBytes &&
		!agentEmailOutboundHasControl(eventID)
}
