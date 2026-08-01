package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/plans"
)

const (
	// AgentEmailRateDimensionReceived meters signed inbound delivery attempts.
	AgentEmailRateDimensionReceived = "email_received"
	// AgentEmailRateDimensionReceivedBytes meters signed raw-MIME bytes.
	AgentEmailRateDimensionReceivedBytes = "email_received_bytes"

	// AgentEmailRateScopeRealm identifies an aggregate realm breaker.
	AgentEmailRateScopeRealm = "realm"
	// AgentEmailRateScopeRecipient identifies one receiving agent.
	AgentEmailRateScopeRecipient = "recipient"
	// AgentEmailRateScopeSender identifies a hashed unverified
	// envelope-sender/recipient pair.
	AgentEmailRateScopeSender = "sender"

	// AgentEmailRateSourcePlan identifies an account snapshot or override cap.
	AgentEmailRateSourcePlan = "plan"
	// AgentEmailRateSourcePlatform identifies the non-optional safety breaker.
	AgentEmailRateSourcePlatform = "platform"

	agentEmailRateWindowSeconds     int64 = 60
	agentEmailRateWindowNanoseconds int64 = agentEmailRateWindowSeconds * int64(time.Second)
	maxAgentEmailRateCleanupBatch         = 10_000
)

// ErrAgentEmailRateLimited identifies an inbound-email safety refusal. It is
// never billable usage and never reveals an external sender or tenant id.
var ErrAgentEmailRateLimited = errors.New("agent-email rate limited")

// AgentEmailRateLimitError carries only bounded/value-free rate state. Sender
// and recipient identifiers stay in the database bucket key and are never
// copied into errors, metrics, or HTTP responses.
type AgentEmailRateLimitError struct {
	Dimension     string
	Scope         string
	Limit         int64
	Used          int64
	Attempted     int64
	WindowSeconds int64
	RetryAfter    time.Duration
	ResetAt       time.Time
	Source        string
	Retryable     bool
}

func (e *AgentEmailRateLimitError) Error() string {
	if e == nil {
		return ErrAgentEmailRateLimited.Error()
	}
	return fmt.Sprintf(
		"%s: %s %s scope used %d, attempted %d, limit %d per %ds",
		ErrAgentEmailRateLimited,
		e.Dimension,
		e.Scope,
		e.Used,
		e.Attempted,
		e.Limit,
		e.WindowSeconds,
	)
}

func (e *AgentEmailRateLimitError) Unwrap() error { return ErrAgentEmailRateLimited }

type agentEmailRateDebit struct {
	planKey       string
	dimension     string
	scope         string
	scopeID       string
	quantity      int64
	platformLimit int64
}

// enforceAgentEmailRateLimitsTx applies account-plan caps without ever
// removing the platform breaker. Aggregate buckets are debited before the
// hashed sender bucket so rotating sender addresses cannot create unbounded
// rows after a realm or recipient is already saturated. A later refusal rolls
// every earlier debit back with the surrounding ingest transaction.
func enforceAgentEmailRateLimitsTx(
	ctx context.Context,
	tx pgx.Tx,
	policy agentEmailIngestAccountPolicy,
	address AgentEmailAddress,
	relay agentemail.RelayMetadata,
	rawBytes int64,
) error {
	senderScopeID := agentEmailSenderScopeID(relay.EnvelopeSender, address.Address)
	debits := []agentEmailRateDebit{
		{
			planKey:   plans.AgentEmailReceivedPerRealmMinuteLimit,
			dimension: AgentEmailRateDimensionReceived,
			scope:     AgentEmailRateScopeRealm, scopeID: address.RealmID,
			quantity: 1, platformLimit: plans.MaxAgentEmailReceivedPerRealmMinute,
		},
		{
			planKey:   plans.AgentEmailReceivedBytesPerRealmMinuteLimit,
			dimension: AgentEmailRateDimensionReceivedBytes,
			scope:     AgentEmailRateScopeRealm, scopeID: address.RealmID,
			quantity: rawBytes, platformLimit: plans.MaxAgentEmailReceivedBytesPerRealmMinute,
		},
		{
			planKey:   plans.AgentEmailReceivedPerRecipientMinuteLimit,
			dimension: AgentEmailRateDimensionReceived,
			scope:     AgentEmailRateScopeRecipient, scopeID: address.OwnerAgentID,
			quantity: 1, platformLimit: plans.MaxAgentEmailReceivedPerRecipientMinute,
		},
		{
			planKey:   plans.AgentEmailReceivedBytesPerRecipientMinuteLimit,
			dimension: AgentEmailRateDimensionReceivedBytes,
			scope:     AgentEmailRateScopeRecipient, scopeID: address.OwnerAgentID,
			quantity: rawBytes, platformLimit: plans.MaxAgentEmailReceivedBytesPerRecipientMinute,
		},
		{
			planKey:   plans.AgentEmailReceivedPerSenderMinuteLimit,
			dimension: AgentEmailRateDimensionReceived,
			scope:     AgentEmailRateScopeSender, scopeID: senderScopeID,
			quantity: 1, platformLimit: plans.MaxAgentEmailReceivedPerSenderMinute,
		},
		{
			planKey:   plans.AgentEmailReceivedBytesPerSenderMinuteLimit,
			dimension: AgentEmailRateDimensionReceivedBytes,
			scope:     AgentEmailRateScopeSender, scopeID: senderScopeID,
			quantity: rawBytes, platformLimit: plans.MaxAgentEmailReceivedBytesPerSenderMinute,
		},
	}

	for _, debit := range debits {
		limit, source := effectiveAgentEmailRateLimit(
			policy.Limits,
			debit.planKey,
			debit.platformLimit,
		)
		if err := consumeAgentEmailRateTx(
			ctx,
			tx,
			address.AccountID,
			address.RealmID,
			debit,
			limit,
			source,
		); err != nil {
			return err
		}
	}
	return nil
}

func agentEmailSenderScopeID(envelopeSender, recipient string) string {
	digest := sha256.Sum256([]byte(envelopeSender + "\x00" + recipient))
	return fmt.Sprintf("%x", digest[:])
}

func effectiveAgentEmailRateLimit(
	limits map[string]int64,
	key string,
	platformLimit int64,
) (int64, string) {
	if planLimit, present := limits[key]; present && planLimit <= platformLimit {
		return planLimit, AgentEmailRateSourcePlan
	}
	return platformLimit, AgentEmailRateSourcePlatform
}

func agentEmailRateDebitRetryable(limit, attempted int64) bool {
	return limit > 0 && attempted > 0 && attempted <= limit
}

func agentEmailRateIntervalNanoseconds(limit int64) int64 {
	if limit <= 0 {
		return 0
	}
	return (agentEmailRateWindowNanoseconds + limit - 1) / limit
}

type agentEmailRateDecision struct {
	admitted       bool
	currentTAT     int64
	nowNanoseconds int64
}

func consumeAgentEmailRateBucketTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, realmID string,
	debit agentEmailRateDebit,
	limit int64,
) (agentEmailRateDecision, error) {
	var decision agentEmailRateDecision
	err := tx.QueryRow(ctx, `
		SELECT admitted, current_tat, now_nanoseconds
		  FROM witself_consume_agent_email_rate_bucket($1,$2,$3,$4,$5,$6,$7,$8)`,
		accountID,
		realmID,
		debit.dimension,
		debit.scope,
		debit.scopeID,
		agentEmailRateIntervalNanoseconds(limit),
		limit,
		debit.quantity,
	).Scan(&decision.admitted, &decision.currentTAT, &decision.nowNanoseconds)
	return decision, err
}

func consumeAgentEmailRateTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, realmID string,
	debit agentEmailRateDebit,
	limit int64,
	source string,
) error {
	intervalNanoseconds := agentEmailRateIntervalNanoseconds(limit)
	retryable := agentEmailRateDebitRetryable(limit, debit.quantity)
	decision, err := consumeAgentEmailRateBucketTx(
		ctx,
		tx,
		accountID,
		realmID,
		debit,
		limit,
	)
	if err != nil {
		return fmt.Errorf(
			"debit agent-email %s %s rate: %w",
			debit.dimension,
			debit.scope,
			err,
		)
	}
	if decision.admitted {
		return nil
	}

	used := int64(0)
	if intervalNanoseconds > 0 && decision.currentTAT > decision.nowNanoseconds {
		debt := decision.currentTAT - decision.nowNanoseconds
		used = (debt + intervalNanoseconds - 1) / intervalNanoseconds
		if used > limit {
			used = limit
		}
	}
	retryNanoseconds := int64(0)
	resetAt := time.Time{}
	if retryable {
		baseTAT := max(decision.currentTAT, decision.nowNanoseconds)
		admittedTAT := baseTAT + debit.quantity*intervalNanoseconds
		bucketCeiling := decision.nowNanoseconds + limit*intervalNanoseconds
		retryNanoseconds = admittedTAT - bucketCeiling
		if retryNanoseconds < 1 {
			retryNanoseconds = 1
		}
		resetAt = time.Unix(0, decision.nowNanoseconds+retryNanoseconds).UTC()
	}
	return &AgentEmailRateLimitError{
		Dimension:     debit.dimension,
		Scope:         debit.scope,
		Limit:         limit,
		Used:          used,
		Attempted:     debit.quantity,
		WindowSeconds: agentEmailRateWindowSeconds,
		RetryAfter:    time.Duration(retryNanoseconds),
		ResetAt:       resetAt,
		Source:        source,
		Retryable:     retryable,
	}
}

// DeleteStaleAgentEmailRateBuckets removes one bounded cooperatively locked
// batch. PostgreSQL independently requires the full window to be idle and all
// GCRA debt to have expired before deletion.
func (s *Store) DeleteStaleAgentEmailRateBuckets(
	ctx context.Context,
	before time.Time,
	limit int,
) (int64, error) {
	if before.IsZero() {
		return 0, errors.New("agent-email rate bucket cleanup cutoff is required")
	}
	if limit < 1 || limit > maxAgentEmailRateCleanupBatch {
		return 0, fmt.Errorf(
			"agent-email rate bucket cleanup limit must be 1-%d",
			maxAgentEmailRateCleanupBatch,
		)
	}
	var deleted int64
	err := s.pool.QueryRow(ctx, `
		WITH stale AS MATERIALIZED (
		  SELECT bucket.ctid
		    FROM agent_email_rate_buckets bucket
		   WHERE bucket.updated_at < LEAST(
		           $1::timestamptz,
		           clock_timestamp() - interval '60 seconds'
		         )
		     AND bucket.theoretical_arrival_nanoseconds <=
		         floor(extract(epoch FROM clock_timestamp()) * 1000000000)::bigint
		   ORDER BY bucket.updated_at, bucket.account_id, bucket.realm_id,
		            bucket.dimension, bucket.scope, bucket.scope_id
		   FOR UPDATE SKIP LOCKED
		   LIMIT $2
		), removed AS (
		  DELETE FROM agent_email_rate_buckets bucket
		   USING stale
		   WHERE bucket.ctid = stale.ctid
		   RETURNING 1
		)
		SELECT count(*) FROM removed`, before.UTC(), limit).Scan(&deleted)
	if err != nil {
		return 0, fmt.Errorf("delete stale agent-email rate buckets: %w", err)
	}
	return deleted, nil
}
