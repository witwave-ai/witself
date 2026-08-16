package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/plans"
)

const (
	// AgentEmailOutboundRateScopeAccount identifies an account-wide rate bucket.
	AgentEmailOutboundRateScopeAccount = "account"
	// AgentEmailOutboundRateScopeAgent identifies an agent-local rate bucket.
	AgentEmailOutboundRateScopeAgent = "agent"
	// AgentEmailOutboundRateScopeRealm identifies a realm aggregate bucket.
	AgentEmailOutboundRateScopeRealm = "realm"
	// AgentEmailOutboundRateScopeRecipient identifies one normalized recipient
	// across every realm in the sending account. Only a SHA-256 id is retained.
	AgentEmailOutboundRateScopeRecipient = "recipient"

	// AgentEmailOutboundRateSourcePlan identifies a commercial plan cap.
	AgentEmailOutboundRateSourcePlan = "plan"
	// AgentEmailOutboundRateSourcePlatform identifies a defensive service cap.
	AgentEmailOutboundRateSourcePlatform = "platform"

	agentEmailOutboundRateLaneAdmission      = "admission"
	agentEmailOutboundRateLaneDispatch       = "dispatch"
	agentEmailOutboundRateLaneAdmissionDaily = "admission_daily"
	agentEmailOutboundRateLaneDispatchDaily  = "dispatch_daily"

	agentEmailOutboundRateMinuteSeconds       int64 = 60
	agentEmailOutboundRateDaySeconds          int64 = 24 * 60 * 60
	maximumAgentEmailOutboundRateCleanupBatch       = 10_000
)

// ErrAgentEmailOutboundRateLimited identifies a refused send-rate debit.
var ErrAgentEmailOutboundRateLimited = errors.New("outbound agent-email rate limited")

// AgentEmailOutboundRateLimitError is intentionally tenant/value-free. Scope,
// source, and numeric debt are closed server-owned metric labels.
type AgentEmailOutboundRateLimitError struct {
	Scope         string
	Limit         int64
	Used          int64
	WindowSeconds int64
	RetryAfter    time.Duration
	ResetAt       time.Time
	Source        string
	Retryable     bool
}

func (e *AgentEmailOutboundRateLimitError) Error() string {
	if e == nil {
		return ErrAgentEmailOutboundRateLimited.Error()
	}
	return fmt.Sprintf("%s: %s scope used %d, limit %d per %ds",
		ErrAgentEmailOutboundRateLimited, e.Scope, e.Used, e.Limit,
		e.WindowSeconds)
}

func (e *AgentEmailOutboundRateLimitError) Unwrap() error {
	return ErrAgentEmailOutboundRateLimited
}

type agentEmailOutboundRateDebit struct {
	lane          string
	planKey       string
	scope         string
	scopeID       string
	platformLimit int64
	windowSeconds int64
	burstLimit    int64
}

func enforceAgentEmailOutboundRateLimitsTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	limits map[string]int64,
	recipient string,
) error {
	// Broadest scope first: once aggregate reputation protection is saturated,
	// rotating realms or agent identities cannot create an unbounded bucket tail.
	debits := []agentEmailOutboundRateDebit{
		{
			lane:  agentEmailOutboundRateLaneAdmissionDaily,
			scope: AgentEmailOutboundRateScopeAccount, scopeID: p.AccountID,
			platformLimit: plans.MaxAgentEmailSentPerAccountDay,
			windowSeconds: agentEmailOutboundRateDaySeconds,
			burstLimit:    plans.MaxAgentEmailSentPerAccountDayBurst,
		},
		{
			lane:          agentEmailOutboundRateLaneAdmissionDaily,
			scope:         AgentEmailOutboundRateScopeRecipient,
			scopeID:       agentEmailOutboundRecipientScopeID(p.AccountID, recipient),
			platformLimit: plans.MaxAgentEmailSentPerRecipientDay,
			windowSeconds: agentEmailOutboundRateDaySeconds,
			burstLimit:    plans.MaxAgentEmailSentPerRecipientDayBurst,
		},
		{
			lane:  agentEmailOutboundRateLaneAdmission,
			scope: AgentEmailOutboundRateScopeAccount, scopeID: p.AccountID,
			platformLimit: plans.MaxAgentEmailSentPerAccountMinute,
			windowSeconds: agentEmailOutboundRateMinuteSeconds,
		},
		{
			lane:    agentEmailOutboundRateLaneAdmission,
			planKey: plans.AgentEmailSentPerRealmMinuteLimit,
			scope:   AgentEmailOutboundRateScopeRealm, scopeID: p.RealmID,
			platformLimit: plans.MaxAgentEmailSentPerRealmMinute,
			windowSeconds: agentEmailOutboundRateMinuteSeconds,
		},
		{
			lane:    agentEmailOutboundRateLaneAdmission,
			planKey: plans.AgentEmailSentPerAgentMinuteLimit,
			scope:   AgentEmailOutboundRateScopeAgent, scopeID: p.ID,
			platformLimit: plans.MaxAgentEmailSentPerAgentMinute,
			windowSeconds: agentEmailOutboundRateMinuteSeconds,
		},
	}
	for _, debit := range debits {
		limit, source := effectiveAgentEmailOutboundRateLimit(
			limits, debit.planKey, debit.platformLimit,
		)
		if err := consumeAgentEmailOutboundRateTx(
			ctx, tx, p.AccountID, p.RealmID, debit, limit, source,
		); err != nil {
			return err
		}
	}
	return nil
}

// enforceAgentEmailOutboundDispatchRateLimitsTx independently bounds provider
// attempts. Admission limits bound caller-created work; this platform-only
// lane prevents retries and multiple workers from bursting the shared sending
// domain without consuming the caller's commercial admission budget twice.
func enforceAgentEmailOutboundDispatchRateLimitsTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	recipient string,
) error {
	debits := []agentEmailOutboundRateDebit{
		{
			lane:  agentEmailOutboundRateLaneDispatchDaily,
			scope: AgentEmailOutboundRateScopeAccount, scopeID: p.AccountID,
			platformLimit: plans.MaxAgentEmailSentPerAccountDay,
			windowSeconds: agentEmailOutboundRateDaySeconds,
			burstLimit:    plans.MaxAgentEmailSentPerAccountDayBurst,
		},
		{
			lane:          agentEmailOutboundRateLaneDispatchDaily,
			scope:         AgentEmailOutboundRateScopeRecipient,
			scopeID:       agentEmailOutboundRecipientScopeID(p.AccountID, recipient),
			platformLimit: plans.MaxAgentEmailSentPerRecipientDay,
			windowSeconds: agentEmailOutboundRateDaySeconds,
			burstLimit:    plans.MaxAgentEmailSentPerRecipientDayBurst,
		},
		{
			lane:  agentEmailOutboundRateLaneDispatch,
			scope: AgentEmailOutboundRateScopeAccount, scopeID: p.AccountID,
			platformLimit: plans.MaxAgentEmailSentPerAccountMinute,
			windowSeconds: agentEmailOutboundRateMinuteSeconds,
		},
		{
			lane:  agentEmailOutboundRateLaneDispatch,
			scope: AgentEmailOutboundRateScopeRealm, scopeID: p.RealmID,
			platformLimit: plans.MaxAgentEmailSentPerRealmMinute,
			windowSeconds: agentEmailOutboundRateMinuteSeconds,
		},
		{
			lane:  agentEmailOutboundRateLaneDispatch,
			scope: AgentEmailOutboundRateScopeAgent, scopeID: p.ID,
			platformLimit: plans.MaxAgentEmailSentPerAgentMinute,
			windowSeconds: agentEmailOutboundRateMinuteSeconds,
		},
	}
	for _, debit := range debits {
		if err := consumeAgentEmailOutboundRateTx(
			ctx, tx, p.AccountID, p.RealmID, debit,
			debit.platformLimit, AgentEmailOutboundRateSourcePlatform,
		); err != nil {
			return err
		}
	}
	return nil
}

func effectiveAgentEmailOutboundRateLimit(
	limits map[string]int64,
	key string,
	platformLimit int64,
) (int64, string) {
	if key != "" {
		if limit, present := limits[key]; present && limit <= platformLimit {
			return limit, AgentEmailOutboundRateSourcePlan
		}
	}
	return platformLimit, AgentEmailOutboundRateSourcePlatform
}

func agentEmailOutboundRateIntervalMicroseconds(limit, windowSeconds int64) int64 {
	if limit <= 0 || windowSeconds <= 0 {
		return 0
	}
	windowMicroseconds := windowSeconds * int64(time.Second/time.Microsecond)
	return (windowMicroseconds + limit - 1) / limit
}

func agentEmailOutboundRecipientScopeID(accountID, normalizedRecipient string) string {
	// Domain-separate recipient pseudonyms by both purpose and account. This
	// keeps the closed bucket id stable inside one account without allowing the
	// same external address to be correlated across tenants.
	digest := sha256.Sum256([]byte(
		"witself.agent-email-outbound-recipient.v1\x00" +
			accountID + "\x00" + normalizedRecipient,
	))
	return fmt.Sprintf("%x", digest[:])
}

type agentEmailOutboundRateDecision struct {
	admitted        bool
	currentTAT      int64
	nowMicroseconds int64
}

func consumeAgentEmailOutboundRateTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID, realmID string,
	debit agentEmailOutboundRateDebit,
	limit int64,
	source string,
) error {
	interval := agentEmailOutboundRateIntervalMicroseconds(limit, debit.windowSeconds)
	bucketCapacity := agentEmailOutboundRateBucketCapacity(debit, limit)
	bucketRealmID := realmID
	if debit.scope == AgentEmailOutboundRateScopeAccount ||
		debit.scope == AgentEmailOutboundRateScopeRecipient {
		bucketRealmID = ""
	}
	var decision agentEmailOutboundRateDecision
	err := tx.QueryRow(ctx, `
		SELECT admitted,current_tat,now_microseconds
		  FROM witself_consume_agent_email_outbound_rate_bucket(
		    $1,$2,$3,$4,$5,$6,$7
		  )`, accountID, bucketRealmID, debit.lane, debit.scope, debit.scopeID,
		interval, bucketCapacity).
		Scan(&decision.admitted, &decision.currentTAT, &decision.nowMicroseconds)
	if err != nil {
		return fmt.Errorf("debit outbound agent-email %s %s rate: %w",
			debit.lane, debit.scope, err)
	}
	if decision.admitted {
		return nil
	}
	used := int64(0)
	if interval > 0 && decision.currentTAT > decision.nowMicroseconds {
		used = (decision.currentTAT - decision.nowMicroseconds + interval - 1) / interval
		if used > limit {
			used = limit
		}
	}
	retryable := limit > 0
	retryAfter := time.Duration(0)
	resetAt := time.Time{}
	if retryable {
		candidate := max(decision.currentTAT, decision.nowMicroseconds) + interval
		ceiling := decision.nowMicroseconds + bucketCapacity*interval
		retryMicros := candidate - ceiling
		if retryMicros < 1 {
			retryMicros = 1
		}
		retryAfter = time.Duration(retryMicros) * time.Microsecond
		resetAt = time.UnixMicro(decision.nowMicroseconds + retryMicros).UTC()
	}
	return &AgentEmailOutboundRateLimitError{
		Scope: debit.scope, Limit: limit, Used: used,
		WindowSeconds: debit.windowSeconds,
		RetryAfter:    retryAfter, ResetAt: resetAt, Source: source,
		Retryable: retryable,
	}
}

func agentEmailOutboundRateBucketCapacity(
	debit agentEmailOutboundRateDebit,
	limit int64,
) int64 {
	if debit.burstLimit > 0 && debit.burstLimit < limit {
		return debit.burstLimit
	}
	return limit
}

// DeleteStaleAgentEmailOutboundRateBuckets is bounded general-worker
// maintenance. It never deletes live debt even if the caller supplies a newer
// cutoff, and every replica cooperates through SKIP LOCKED.
func (s *Store) DeleteStaleAgentEmailOutboundRateBuckets(
	ctx context.Context,
	before time.Time,
	limit int,
) (int64, error) {
	if before.IsZero() {
		return 0, errors.New("outbound agent-email rate cleanup cutoff is required")
	}
	if limit < 1 || limit > maximumAgentEmailOutboundRateCleanupBatch {
		return 0, fmt.Errorf(
			"outbound agent-email rate cleanup limit must be 1-%d",
			maximumAgentEmailOutboundRateCleanupBatch)
	}
	var deleted int64
	err := s.pool.QueryRow(ctx, `
		WITH stale AS MATERIALIZED (
		  SELECT bucket.ctid
		    FROM agent_email_outbound_rate_buckets bucket
		   WHERE bucket.updated_at < LEAST(
		           $1::timestamptz,clock_timestamp()-interval '60 seconds'
		         )
		     AND bucket.theoretical_arrival_microseconds <=
		         floor(extract(epoch FROM clock_timestamp())*1000000)::bigint
		   ORDER BY bucket.updated_at,bucket.account_id,bucket.realm_id,
		            bucket.lane,bucket.scope,bucket.scope_id
		   FOR UPDATE SKIP LOCKED
		   LIMIT $2
		), removed AS (
		  DELETE FROM agent_email_outbound_rate_buckets bucket
		   USING stale WHERE bucket.ctid=stale.ctid RETURNING 1
		)
		SELECT count(*) FROM removed`, before.UTC(), limit).Scan(&deleted)
	if err != nil {
		return 0, fmt.Errorf("delete stale outbound agent-email rate buckets: %w", err)
	}
	return deleted, nil
}
