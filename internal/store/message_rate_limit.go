package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/plans"
)

const (
	// MessageRateDimensionSent meters logical message-send attempts.
	MessageRateDimensionSent = "message_sent"
	// MessageRateDimensionDelivered meters resolved delivery targets.
	MessageRateDimensionDelivered = "message_delivered"

	// MessageRateScopeAgent identifies a sending-agent bucket.
	MessageRateScopeAgent = "agent"
	// MessageRateScopeRealm identifies a realm-wide delivery bucket.
	MessageRateScopeRealm = "realm"
	// MessageRateScopeRecipient identifies a receiving-agent bucket.
	MessageRateScopeRecipient = "recipient"

	// MessageRateSourcePlan identifies a plan or account-override limit.
	MessageRateSourcePlan = "plan"
	// MessageRateSourcePlatform identifies a platform safety breaker.
	MessageRateSourcePlatform = "platform"

	messageRateWindowSeconds      int64 = 60
	messageRateWindowMicroseconds int64 = messageRateWindowSeconds * int64(time.Second/time.Microsecond)
	maxMessageRateCleanupBatch          = 10_000
)

// ErrMessageRateLimited identifies a message-admission refusal from the shared
// rate budget. Inspect MessageRateLimitError.Retryable before retrying: an
// exhausted debit that fits the bucket may be retried after RetryAfter, while a
// debit that can never fit the effective limit is a hard refusal.
var ErrMessageRateLimited = errors.New("message rate limited")

// MessageRateLimitError is a value-free GCRA refusal. It deliberately carries
// no account, realm, agent, recipient, or message identifier so HTTP adapters
// and metrics can surface only bounded server-owned labels.
type MessageRateLimitError struct {
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

// DeleteStaleMessageRateBuckets removes one bounded, cooperatively locked
// batch for the general cell worker. PostgreSQL enforces a full idle window
// even when a caller supplies a newer cutoff, so no bucket with live GCRA debt
// can be removed. The API process never schedules this maintenance method.
func (s *Store) DeleteStaleMessageRateBuckets(
	ctx context.Context,
	before time.Time,
	limit int,
) (int64, error) {
	if before.IsZero() {
		return 0, errors.New("message rate bucket cleanup cutoff is required")
	}
	if limit < 1 || limit > maxMessageRateCleanupBatch {
		return 0, fmt.Errorf("message rate bucket cleanup limit must be 1-%d", maxMessageRateCleanupBatch)
	}
	var deleted int64
	err := s.pool.QueryRow(ctx, `
		WITH stale AS MATERIALIZED (
		  SELECT bucket.ctid
		    FROM agent_message_rate_buckets bucket
		   WHERE bucket.updated_at < LEAST(
		           $1::timestamptz,
		           clock_timestamp() - interval '60 seconds'
		         )
		     AND bucket.theoretical_arrival_microseconds <=
		         floor(extract(epoch FROM clock_timestamp()) * 1000000)::bigint
		   ORDER BY bucket.updated_at, bucket.account_id, bucket.realm_id,
		            bucket.dimension, bucket.scope, bucket.scope_id
		   FOR UPDATE SKIP LOCKED
		   LIMIT $2
		), removed AS (
		  DELETE FROM agent_message_rate_buckets bucket
		   USING stale
		   WHERE bucket.ctid = stale.ctid
		   RETURNING 1
		)
		SELECT count(*) FROM removed`, before.UTC(), limit).Scan(&deleted)
	if err != nil {
		return 0, fmt.Errorf("delete stale message rate buckets: %w", err)
	}
	return deleted, nil
}

func (e *MessageRateLimitError) Error() string {
	if e == nil {
		return ErrMessageRateLimited.Error()
	}
	return fmt.Sprintf("%s: %s %s scope used %d, attempted %d, limit %d per %ds",
		ErrMessageRateLimited, e.Dimension, e.Scope, e.Used, e.Attempted,
		e.Limit, e.WindowSeconds)
}

func (e *MessageRateLimitError) Unwrap() error { return ErrMessageRateLimited }

type messageRateDebit struct {
	planKey       string
	dimension     string
	scope         string
	scopeID       string
	quantity      int64
	platformLimit int64
}

// enforceMessageRateLimitsTx debits every scope in deterministic lock order.
// It runs only after insertMessageTargetsTx has proven this is a newly inserted
// message. A later refusal rolls back the message row and every earlier debit.
func enforceMessageRateLimitsTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	targets []messageDeliveryTarget,
) error {
	limits, err := messagePlanLimitsTx(ctx, tx, p.AccountID)
	if err != nil {
		return err
	}

	debits := []messageRateDebit{
		{
			planKey:   plans.MessageSentPerAgentMinuteLimit,
			dimension: MessageRateDimensionSent, scope: MessageRateScopeAgent,
			scopeID: p.ID, quantity: 1,
			platformLimit: plans.MaxMessageSentPerAgentMinute,
		},
		{
			planKey:   plans.MessageDeliveredPerRealmMinuteLimit,
			dimension: MessageRateDimensionDelivered, scope: MessageRateScopeRealm,
			scopeID: p.RealmID, quantity: int64(len(targets)),
			platformLimit: plans.MaxMessageDeliveredPerRealmMinute,
		},
	}

	recipientIDs := make([]string, len(targets))
	for i := range targets {
		recipientIDs[i] = targets[i].agent.ID
	}
	sort.Strings(recipientIDs)
	for _, recipientID := range recipientIDs {
		debits = append(debits, messageRateDebit{
			planKey:   plans.MessageDeliveredPerRecipientMinuteLimit,
			dimension: MessageRateDimensionDelivered, scope: MessageRateScopeRecipient,
			scopeID: recipientID, quantity: 1,
			platformLimit: plans.MaxMessageDeliveredPerRecipientMinute,
		})
	}

	for _, debit := range debits {
		limit, source := effectiveMessageRateLimit(limits, debit.planKey, debit.platformLimit)
		if err := consumeMessageRateTx(ctx, tx, p.AccountID, p.RealmID, debit, limit, source); err != nil {
			return err
		}
	}
	return nil
}

func messagePlanLimitsTx(ctx context.Context, tx pgx.Tx, accountID string) (map[string]int64, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT plan_limits FROM accounts WHERE id=$1`, accountID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read message plan limits: %w", err)
	}
	limits := map[string]int64{}
	if err := json.Unmarshal(raw, &limits); err != nil {
		return nil, fmt.Errorf("decode message plan limits: %w", err)
	}
	return limits, nil
}

func effectiveMessageRateLimit(limits map[string]int64, key string, platformLimit int64) (int64, string) {
	if planLimit, present := limits[key]; present && planLimit <= platformLimit {
		return planLimit, MessageRateSourcePlan
	}
	return platformLimit, MessageRateSourcePlatform
}

func messageRateDebitRetryable(limit, attempted int64) bool {
	return limit > 0 && attempted > 0 && attempted <= limit
}

func messageRateIntervalMicroseconds(limit int64) int64 {
	if limit <= 0 {
		return 0
	}
	return (messageRateWindowMicroseconds + limit - 1) / limit
}

type messageRateDecision struct {
	admitted        bool
	currentTAT      int64
	nowMicroseconds int64
}

func consumeMessageRateBucketTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, realmID string,
	debit messageRateDebit,
	limit int64,
) (messageRateDecision, error) {
	var decision messageRateDecision
	err := tx.QueryRow(ctx, `
		SELECT admitted, current_tat, now_microseconds
		  FROM witself_consume_agent_message_rate_bucket($1,$2,$3,$4,$5,$6,$7,$8)`,
		accountID, realmID, debit.dimension, debit.scope, debit.scopeID,
		messageRateIntervalMicroseconds(limit), limit, debit.quantity,
	).Scan(&decision.admitted, &decision.currentTAT, &decision.nowMicroseconds)
	return decision, err
}

// consumeMessageRateTx implements a one-minute GCRA bucket with burst capacity
// equal to the effective per-minute limit. One invoker-rights PostgreSQL
// function call is the cross-replica serialization point: it locks the bucket,
// then captures one database clock and decides the weighted debit. A refusal is
// rolled back with the caller's message transaction.
func consumeMessageRateTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, realmID string,
	debit messageRateDebit,
	limit int64,
	source string,
) error {
	intervalMicroseconds := messageRateIntervalMicroseconds(limit)
	retryable := messageRateDebitRetryable(limit, debit.quantity)
	decision, err := consumeMessageRateBucketTx(ctx, tx, accountID, realmID, debit, limit)
	if err != nil {
		return fmt.Errorf("debit message %s %s rate: %w", debit.dimension, debit.scope, err)
	}
	if decision.admitted {
		return nil
	}
	currentTAT := decision.currentTAT
	nowMicroseconds := decision.nowMicroseconds

	used := int64(0)
	if intervalMicroseconds > 0 && currentTAT > nowMicroseconds {
		debt := currentTAT - nowMicroseconds
		used = (debt + intervalMicroseconds - 1) / intervalMicroseconds
		if used > limit {
			used = limit
		}
	}
	retryMicroseconds := int64(0)
	resetAt := time.Time{}
	if retryable {
		baseTAT := max(currentTAT, nowMicroseconds)
		admittedTAT := baseTAT + debit.quantity*intervalMicroseconds
		bucketCeiling := nowMicroseconds + limit*intervalMicroseconds
		retryMicroseconds = admittedTAT - bucketCeiling
		if retryMicroseconds < 1 {
			retryMicroseconds = 1
		}
		resetAt = time.UnixMicro(nowMicroseconds + retryMicroseconds).UTC()
	}
	return &MessageRateLimitError{
		Dimension: debit.dimension, Scope: debit.scope,
		Limit: limit, Used: used, Attempted: debit.quantity,
		WindowSeconds: messageRateWindowSeconds,
		RetryAfter:    time.Duration(retryMicroseconds) * time.Microsecond,
		ResetAt:       resetAt,
		Source:        source,
		Retryable:     retryable,
	}
}
