package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

const (
	defaultAgentEmailOutboundLease                = 5 * time.Minute
	minimumAgentEmailOutboundLease                = 30 * time.Second
	maximumAgentEmailOutboundLease                = 15 * time.Minute
	maximumAgentEmailOutboundAttempts       int64 = 12
	maximumAgentEmailOutboundWorkAge              = 72 * time.Hour
	maximumAgentEmailOutboundReconcileBatch       = 1_000
)

// ErrAgentEmailOutboundProviderAlreadyStarted prevents a blind provider replay.
var ErrAgentEmailOutboundProviderAlreadyStarted = errors.New(
	"outbound agent-email provider call already started",
)

// AgentEmailOutboundClaimInput controls the duration of one worker claim.
type AgentEmailOutboundClaimInput struct {
	LeaseDuration time.Duration
}

// AgentEmailOutboundClaimFence is the exact capability for one claimed send.
type AgentEmailOutboundClaimFence struct {
	ClaimID    string `json:"claim_id"`
	Generation int64  `json:"generation"`
}

// AgentEmailOutboundDispatch is the sole content-bearing worker projection.
// The worker signs these immutable fields and never performs identity lookup.
type AgentEmailOutboundDispatch struct {
	Message    AgentEmailOutboundMessage    `json:"message"`
	Text       string                       `json:"text"`
	InReplyTo  string                       `json:"in_reply_to,omitempty"`
	References []string                     `json:"references,omitempty"`
	Claim      AgentEmailOutboundClaimFence `json:"claim"`
}

// FinalizeAgentEmailOutboundInput records one explicit terminal provider result.
type FinalizeAgentEmailOutboundInput struct {
	Claim             AgentEmailOutboundClaimFence
	State             string
	Provider          string
	ProviderMessageID string
	ErrorCode         string
}

// RetryAgentEmailOutboundInput proves known non-acceptance and schedules retry.
type RetryAgentEmailOutboundInput struct {
	Claim                    AgentEmailOutboundClaimFence
	RetryAt                  time.Time
	ErrorCode                string
	Provider                 string
	PreserveProviderBoundary bool
}

// AmbiguousAgentEmailOutboundInput closes an uncertain provider boundary.
type AmbiguousAgentEmailOutboundInput struct {
	Claim     AgentEmailOutboundClaimFence
	Provider  string
	ErrorCode string
}

// ClaimAgentEmailOutbound claims one ready send. Expired pre-provider claims
// are safe to reclaim. An expired provider_started row is reclaimed only so
// the worker can replay its exact immutable envelope to the adapter's durable
// send-ID receipt; it never authorizes a fresh provider call.
func (s *Store) ClaimAgentEmailOutbound(
	ctx context.Context,
	in AgentEmailOutboundClaimInput,
) (AgentEmailOutboundDispatch, error) {
	lease, err := normalizeAgentEmailOutboundLease(in.LeaseDuration)
	if err != nil {
		return AgentEmailOutboundDispatch{}, err
	}
	claimID, err := id.New("escl")
	if err != nil {
		return AgentEmailOutboundDispatch{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailOutboundDispatch{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	msg, err := scanAgentEmailOutbound(tx.QueryRow(ctx, `
		WITH candidate AS MATERIALIZED (
		  SELECT outbound.id,outbound.state
		    FROM agent_email_outbound_messages outbound
		    JOIN accounts account ON account.id=outbound.account_id
		   WHERE account.status='active' AND (
		     (outbound.state='queued' AND
		      outbound.next_attempt_at <= clock_timestamp()) OR
		     (outbound.state IN ('claimed','provider_started') AND
		      outbound.lease_expires_at <= clock_timestamp())
		   )
		     AND outbound.attempt_count < $3
		     AND outbound.queued_at >
		         clock_timestamp()-($4::bigint*interval '1 microsecond')
		   ORDER BY outbound.queued_at,outbound.id
		   FOR UPDATE OF outbound SKIP LOCKED
		   LIMIT 1
		)
		UPDATE agent_email_outbound_messages outbound
		   SET state=CASE WHEN candidate.state='provider_started'
		                  THEN 'provider_started' ELSE 'claimed' END,
		       provider_state='',provider='',provider_message_id='',
		       last_error_code=CASE WHEN candidate.state='claimed'
		         THEN 'worker_lease_expired' ELSE outbound.last_error_code END,
		       attempt_count=outbound.attempt_count+1,
		       claim_generation=outbound.claim_generation+1,
		       claim_id=$1,
		       lease_expires_at=clock_timestamp()+($2::bigint*interval '1 microsecond'),
		       next_attempt_at=NULL,
		       provider_started_at=CASE WHEN candidate.state='provider_started'
		         THEN outbound.provider_started_at ELSE NULL END,
		       updated_at=clock_timestamp()
		  FROM candidate
		 WHERE outbound.id=candidate.id
		 RETURNING `+agentEmailOutboundReturningColumnsQualified("outbound"),
		claimID, lease.Microseconds(), maximumAgentEmailOutboundAttempts,
		maximumAgentEmailOutboundWorkAge.Microseconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundDispatch{}, ErrAgentEmailOutboundEmpty
	}
	if err != nil {
		return AgentEmailOutboundDispatch{}, fmt.Errorf("claim outbound agent email: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailOutboundDispatch{}, err
	}
	return dispatchFromAgentEmailOutbound(msg), nil
}

// StartAgentEmailOutboundProviderCall performs the last policy, suppression,
// and provider-attempt rate check before durably crossing the provider
// boundary. A recovered provider_started claim returns its exact immutable
// dispatch with ErrAgentEmailOutboundProviderAlreadyStarted; the worker may
// replay only that envelope to the adapter's send-ID receipt.
func (s *Store) StartAgentEmailOutboundProviderCall(
	ctx context.Context,
	sendID string,
	claim AgentEmailOutboundClaimFence,
) (AgentEmailOutboundDispatch, error) {
	sendID, claim, err := normalizeAgentEmailOutboundClaim(sendID, claim)
	if err != nil {
		return AgentEmailOutboundDispatch{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailOutboundDispatch{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var accountID, realmID, agentID string
	err = tx.QueryRow(ctx, `
		SELECT account_id,realm_id,owner_agent_id
		  FROM agent_email_outbound_messages WHERE id=$1`, sendID).
		Scan(&accountID, &realmID, &agentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundDispatch{}, ErrAgentEmailOutboundNotFound
	}
	if err != nil {
		return AgentEmailOutboundDispatch{}, fmt.Errorf("resolve outbound send scope: %w", err)
	}
	p := Principal{
		Kind: PrincipalAgent, ID: agentID, AccountID: accountID,
		RealmID: realmID, AccessProfile: AccessProfileFull,
	}
	_, gateErr := lockAccountForAgentEmailSend(ctx, tx, accountID)
	if gateErr != nil && !errors.Is(gateErr, ErrFeatureNotEnabled) &&
		!errors.Is(gateErr, ErrAccountNotActive) {
		return AgentEmailOutboundDispatch{}, gateErr
	}
	if gateErr == nil {
		if err := lockLiveMessageAgentScope(ctx, tx, accountID, realmID, agentID); err != nil {
			mapped := mapAgentEmailOutboundPrincipalError(err)
			if !errors.Is(mapped, ErrAgentEmailOutboundForbidden) {
				return AgentEmailOutboundDispatch{}, mapped
			}
			gateErr = mapped
		}
	}
	controlErr := error(nil)
	if gateErr == nil {
		controlErr = requireAgentEmailSendControlsTx(ctx, tx, p)
		if controlErr != nil && !errors.Is(controlErr, ErrAgentEmailSendDisabled) {
			return AgentEmailOutboundDispatch{}, controlErr
		}
	}
	msg, err := agentEmailOutboundByIDForUpdateTx(ctx, tx, sendID)
	if err != nil {
		return AgentEmailOutboundDispatch{}, err
	}
	if msg.claimID != claim.ClaimID || msg.claimGeneration != claim.Generation {
		return AgentEmailOutboundDispatch{}, ErrAgentEmailOutboundClaimLost
	}
	if (msg.State != AgentEmailOutboundClaimed &&
		msg.State != AgentEmailOutboundProviderStarted) || msg.leaseExpiresAt == nil {
		return AgentEmailOutboundDispatch{}, ErrAgentEmailOutboundClaimLost
	}
	policyErr := gateErr
	if policyErr == nil {
		policyErr = controlErr
	}
	if policyErr == nil {
		suppressionErr := requireAgentEmailOutboundRecipientAllowedTx(
			ctx, tx, msg.AccountID, msg.ToAddress,
		)
		if suppressionErr != nil &&
			!errors.Is(suppressionErr, ErrAgentEmailRecipientSuppressed) {
			return AgentEmailOutboundDispatch{}, suppressionErr
		}
		policyErr = suppressionErr
	}
	if policyErr != nil {
		if msg.State == AgentEmailOutboundProviderStarted {
			msg, err = abandonAgentEmailOutboundProviderReplayTx(ctx, tx, msg, claim)
		} else {
			msg, err = cancelAgentEmailOutboundClaimTx(ctx, tx, msg, claim)
		}
		if err != nil {
			return AgentEmailOutboundDispatch{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return AgentEmailOutboundDispatch{}, err
		}
		return dispatchFromAgentEmailOutbound(msg), policyErr
	}
	if msg.State == AgentEmailOutboundProviderStarted {
		if err := tx.Commit(ctx); err != nil {
			return AgentEmailOutboundDispatch{}, err
		}
		return dispatchFromAgentEmailOutbound(msg),
			ErrAgentEmailOutboundProviderAlreadyStarted
	}
	if err := enforceAgentEmailOutboundDispatchRateLimitsTx(
		ctx, tx, p, msg.ToAddress,
	); err != nil {
		return dispatchFromAgentEmailOutbound(msg), err
	}
	err = tx.QueryRow(ctx, `
		UPDATE agent_email_outbound_messages
		   SET state='provider_started',provider_started_at=clock_timestamp(),
		       last_error_code='',updated_at=clock_timestamp()
		 WHERE id=$1 AND state='claimed' AND claim_id=$2
		   AND claim_generation=$3 AND lease_expires_at>clock_timestamp()
		 RETURNING `+agentEmailOutboundReturningColumns(),
		sendID, claim.ClaimID, claim.Generation).Scan(agentEmailOutboundScanTargets(&msg)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundDispatch{}, ErrAgentEmailOutboundClaimLost
	}
	if err != nil {
		return AgentEmailOutboundDispatch{}, fmt.Errorf("start outbound provider call: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailOutboundDispatch{}, err
	}
	return dispatchFromAgentEmailOutbound(msg), nil
}

// FinalizeAgentEmailOutbound records a normalized explicit provider result.
// A response may finalize after the lease timestamp while its exact fence is
// still current. Once a new worker reclaims the row for receipt replay, the
// generation changes and the stale settlement is rejected.
func (s *Store) FinalizeAgentEmailOutbound(
	ctx context.Context,
	sendID string,
	in FinalizeAgentEmailOutboundInput,
) (AgentEmailOutboundMessage, error) {
	sendID, claim, err := normalizeAgentEmailOutboundClaim(sendID, in.Claim)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	in.Claim = claim
	if err := normalizeAgentEmailOutboundFinalizeInput(&in); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	msg, err := agentEmailOutboundByIDForUpdateTx(ctx, tx, sendID)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	if msg.State != AgentEmailOutboundProviderStarted ||
		msg.claimID != claim.ClaimID || msg.claimGeneration != claim.Generation {
		if agentEmailOutboundMatchesFinalization(msg, in) {
			if err := tx.Commit(ctx); err != nil {
				return AgentEmailOutboundMessage{}, err
			}
			return redactAgentEmailOutbound(msg), nil
		}
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundClaimLost
	}
	msg, err = finalizeAgentEmailOutboundTx(ctx, tx, msg, in)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	if in.State == AgentEmailOutboundBounced {
		if err := suppressAgentEmailOutboundRecipientTx(
			ctx, tx, msg, "hard_bounce", in.Provider,
		); err != nil {
			return AgentEmailOutboundMessage{}, err
		}
	}
	if agentEmailOutboundCountsAsSent(in.State) {
		if _, err := recordUsageEventTx(ctx, tx, usageEventInput{
			AccountID: msg.AccountID, RealmID: msg.RealmID, AgentID: msg.OwnerAgentID,
			Dimension: UsageDimensionEmailSent, Quantity: 1, Unit: UsageUnitEmail,
			SubjectType: "agent_email_outbound", SubjectID: msg.ID,
			IdempotencyKey: "email_sent:" + msg.ID,
			Metadata:       json.RawMessage(`{}`), OccurredAt: *msg.AcceptedAt,
		}); err != nil {
			return AgentEmailOutboundMessage{}, fmt.Errorf("record outbound email sent usage: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	return redactAgentEmailOutbound(msg), nil
}

// RetryAgentEmailOutbound schedules either a known-safe provider retry or an
// exact adapter-receipt replay. Uncertain calls remain provider_started so the
// cell never forgets that a provider boundary may already have been crossed.
func (s *Store) RetryAgentEmailOutbound(
	ctx context.Context,
	sendID string,
	in RetryAgentEmailOutboundInput,
) (AgentEmailOutboundMessage, error) {
	sendID, claim, err := normalizeAgentEmailOutboundClaim(sendID, in.Claim)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	errorCode, err := normalizeAgentEmailOutboundRetryError(in.ErrorCode)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	provider, err := normalizeAgentEmailOutboundProvider(in.Provider, true)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	retryAt := in.RetryAt.UTC()
	if in.RetryAt.IsZero() {
		retryAt = time.Now().UTC()
	}
	if retryAt.After(time.Now().UTC().Add(24*time.Hour + time.Minute)) {
		return AgentEmailOutboundMessage{}, fmt.Errorf(
			"%w: retry_at exceeds 24 hours", ErrAgentEmailOutboundInputInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	msg, err := agentEmailOutboundByIDForUpdateTx(ctx, tx, sendID)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	if msg.claimID != claim.ClaimID || msg.claimGeneration != claim.Generation ||
		(msg.State != AgentEmailOutboundClaimed &&
			msg.State != AgentEmailOutboundProviderStarted) {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundClaimLost
	}
	if msg.State == AgentEmailOutboundClaimed {
		var live bool
		if err := tx.QueryRow(ctx, `SELECT lease_expires_at>clock_timestamp()
			FROM agent_email_outbound_messages WHERE id=$1`, sendID).Scan(&live); err != nil {
			return AgentEmailOutboundMessage{}, err
		}
		if !live {
			return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundClaimLost
		}
	}
	providerCrossed := msg.State == AgentEmailOutboundProviderStarted
	preserveProviderBoundary := in.PreserveProviderBoundary ||
		isAgentEmailOutboundUncertainRetryError(errorCode)
	if preserveProviderBoundary && !providerCrossed {
		return AgentEmailOutboundMessage{}, fmt.Errorf(
			"%w: provider boundary was not crossed",
			ErrAgentEmailOutboundInputInvalid)
	}
	if providerCrossed && provider == "" {
		return AgentEmailOutboundMessage{}, fmt.Errorf(
			"%w: provider is required after provider start",
			ErrAgentEmailOutboundInputInvalid)
	}
	var exhausted bool
	if err := tx.QueryRow(ctx, `
		SELECT attempt_count >= $2 OR queued_at <=
		       clock_timestamp()-($3::bigint*interval '1 microsecond')
		  FROM agent_email_outbound_messages WHERE id=$1`,
		sendID, maximumAgentEmailOutboundAttempts,
		maximumAgentEmailOutboundWorkAge.Microseconds()).Scan(&exhausted); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	switch {
	case exhausted && providerCrossed && preserveProviderBoundary:
		msg, err = markAgentEmailOutboundAmbiguousTx(
			ctx, tx, sendID, provider, errorCode,
		)
	case exhausted && providerCrossed:
		msg, err = failAgentEmailOutboundRetryTx(
			ctx, tx, sendID, claim, provider, errorCode,
		)
	case exhausted:
		msg, err = cancelAgentEmailOutboundClaimTx(ctx, tx, msg, claim)
	case providerCrossed && preserveProviderBoundary:
		msg, err = rescheduleAgentEmailOutboundReceiptReplayTx(
			ctx, tx, sendID, claim, errorCode, retryAt,
		)
	default:
		msg, err = returnAgentEmailOutboundToQueueTx(
			ctx, tx, sendID, claim, errorCode, retryAt,
		)
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	return redactAgentEmailOutbound(msg), nil
}

// MarkAgentEmailOutboundAmbiguous terminally records an uncertain provider call.
func (s *Store) MarkAgentEmailOutboundAmbiguous(
	ctx context.Context,
	sendID string,
	in AmbiguousAgentEmailOutboundInput,
) (AgentEmailOutboundMessage, error) {
	sendID, claim, err := normalizeAgentEmailOutboundClaim(sendID, in.Claim)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	provider, err := normalizeAgentEmailOutboundProvider(in.Provider, true)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	errorCode, err := normalizeAgentEmailOutboundAmbiguousError(in.ErrorCode)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	msg, err := agentEmailOutboundByIDForUpdateTx(ctx, tx, sendID)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	if msg.State == AgentEmailOutboundAmbiguous &&
		msg.Provider == provider && msg.LastErrorCode == errorCode {
		if err := tx.Commit(ctx); err != nil {
			return AgentEmailOutboundMessage{}, err
		}
		return redactAgentEmailOutbound(msg), nil
	}
	if msg.State != AgentEmailOutboundProviderStarted ||
		msg.claimID != claim.ClaimID || msg.claimGeneration != claim.Generation {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundClaimLost
	}
	msg, err = markAgentEmailOutboundAmbiguousTx(
		ctx, tx, sendID, provider, errorCode,
	)
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	return redactAgentEmailOutbound(msg), nil
}

// ReconcileExhaustedAgentEmailOutbound closes work only after its bounded
// attempt or age budget. Non-exhausted expired claims remain reclaimable;
// provider-started work is preserved for exact adapter-receipt replay.
func (s *Store) ReconcileExhaustedAgentEmailOutbound(
	ctx context.Context,
	limit int,
) (int64, error) {
	if limit < 1 || limit > maximumAgentEmailOutboundReconcileBatch {
		return 0, fmt.Errorf("outbound work reconcile limit must be 1-%d",
			maximumAgentEmailOutboundReconcileBatch)
	}
	var updated int64
	err := s.pool.QueryRow(ctx, `
		WITH expired AS MATERIALIZED (
		  SELECT ctid,state FROM agent_email_outbound_messages
		   WHERE (attempt_count >= $2 OR queued_at <=
		          clock_timestamp()-($3::bigint*interval '1 microsecond'))
		     AND (state='queued' OR
		          (state IN ('claimed','provider_started') AND
		           lease_expires_at<=clock_timestamp()))
		   ORDER BY COALESCE(lease_expires_at,next_attempt_at,queued_at),id
		   FOR UPDATE SKIP LOCKED LIMIT $1
		), changed AS (
		  UPDATE agent_email_outbound_messages outbound
		     SET state=CASE WHEN expired.state='provider_started'
		                    THEN 'ambiguous' ELSE 'canceled' END,
		         provider_state='',provider='',provider_message_id='',
		         last_error_code=CASE WHEN expired.state='provider_started'
		           THEN 'worker_lease_expired' ELSE 'dispatch_canceled' END,
		         claim_id=NULL,lease_expires_at=NULL,next_attempt_at=NULL,
		         provider_started_at=CASE WHEN expired.state='provider_started'
		           THEN outbound.provider_started_at ELSE NULL END,
		         ambiguous_at=CASE WHEN expired.state='provider_started'
		           THEN clock_timestamp() ELSE NULL END,
		         canceled_at=CASE WHEN expired.state='provider_started'
		           THEN NULL ELSE clock_timestamp() END,
		         updated_at=clock_timestamp()
		    FROM expired WHERE outbound.ctid=expired.ctid RETURNING 1
		)
		SELECT count(*) FROM changed`, limit, maximumAgentEmailOutboundAttempts,
		maximumAgentEmailOutboundWorkAge.Microseconds()).Scan(&updated)
	if err != nil {
		return 0, fmt.Errorf("reconcile exhausted outbound agent email: %w", err)
	}
	return updated, nil
}

func dispatchFromAgentEmailOutbound(msg AgentEmailOutboundMessage) AgentEmailOutboundDispatch {
	public := redactAgentEmailOutbound(msg)
	return AgentEmailOutboundDispatch{
		Message: public, Text: msg.bodyText, InReplyTo: msg.inReplyToHeader,
		References: append([]string(nil), msg.references...),
		Claim: AgentEmailOutboundClaimFence{
			ClaimID: msg.claimID, Generation: msg.claimGeneration,
		},
	}
}

func normalizeAgentEmailOutboundLease(lease time.Duration) (time.Duration, error) {
	if lease == 0 {
		lease = defaultAgentEmailOutboundLease
	}
	if lease < minimumAgentEmailOutboundLease || lease > maximumAgentEmailOutboundLease {
		return 0, fmt.Errorf(
			"%w: lease must be between %s and %s",
			ErrAgentEmailOutboundInputInvalid,
			minimumAgentEmailOutboundLease, maximumAgentEmailOutboundLease)
	}
	return lease, nil
}

func normalizeAgentEmailOutboundClaim(
	sendID string,
	claim AgentEmailOutboundClaimFence,
) (string, AgentEmailOutboundClaimFence, error) {
	sendID = strings.TrimSpace(sendID)
	claim.ClaimID = strings.TrimSpace(claim.ClaimID)
	if !validAgentEmailGeneratedID(sendID, "esnd") ||
		!validAgentEmailGeneratedID(claim.ClaimID, "escl") ||
		claim.Generation < 1 || claim.Generation > maximumAgentEmailOutboundGeneration {
		return "", AgentEmailOutboundClaimFence{}, fmt.Errorf(
			"%w: send claim is invalid", ErrAgentEmailOutboundInputInvalid)
	}
	return sendID, claim, nil
}

func normalizeAgentEmailOutboundFinalizeInput(in *FinalizeAgentEmailOutboundInput) error {
	var err error
	in.Provider, err = normalizeAgentEmailOutboundProvider(in.Provider, false)
	if err != nil {
		return err
	}
	in.ProviderMessageID, err = normalizeAgentEmailOutboundProviderMessageID(
		in.ProviderMessageID, false,
	)
	if err != nil {
		return err
	}
	switch in.State {
	case AgentEmailOutboundAccepted, AgentEmailOutboundDelivered,
		AgentEmailOutboundDeferred:
		if in.ProviderMessageID == "" || strings.TrimSpace(in.ErrorCode) != "" {
			return fmt.Errorf("%w: successful provider result shape is invalid",
				ErrAgentEmailOutboundInputInvalid)
		}
		in.ErrorCode = ""
	case AgentEmailOutboundBounced:
		if in.ErrorCode != AgentEmailOutboundErrorRecipientHardBounce {
			return fmt.Errorf("%w: bounce result must be a hard bounce",
				ErrAgentEmailOutboundInputInvalid)
		}
	case AgentEmailOutboundRejected:
		if in.ErrorCode != AgentEmailOutboundErrorProviderRejected {
			return fmt.Errorf("%w: rejected result has invalid error code",
				ErrAgentEmailOutboundInputInvalid)
		}
	case AgentEmailOutboundFailed:
		if strings.TrimSpace(in.ErrorCode) != AgentEmailOutboundErrorProviderFailed {
			return fmt.Errorf("%w: failed result has invalid error code",
				ErrAgentEmailOutboundInputInvalid)
		}
		in.ErrorCode = AgentEmailOutboundErrorProviderFailed
	default:
		return fmt.Errorf("%w: provider result state is invalid",
			ErrAgentEmailOutboundInputInvalid)
	}
	return nil
}

func normalizeAgentEmailOutboundProvider(value string, allowEmpty bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" && allowEmpty {
		return "", nil
	}
	if len(value) < 1 || len(value) > maximumAgentEmailOutboundProviderBytes ||
		value[0] < 'a' || value[0] > 'z' {
		return "", fmt.Errorf("%w: provider is invalid", ErrAgentEmailOutboundInputInvalid)
	}
	for _, c := range []byte(value[1:]) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') &&
			c != '_' && c != '.' && c != '-' {
			return "", fmt.Errorf("%w: provider is invalid", ErrAgentEmailOutboundInputInvalid)
		}
	}
	return value, nil
}

func normalizeAgentEmailOutboundProviderMessageID(
	value string,
	required bool,
) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" && !required {
		return "", nil
	}
	if value == "" || len(value) > maximumAgentEmailOutboundProviderIDBytes ||
		!utf8.ValidString(value) || agentEmailOutboundHasControl(value) {
		return "", fmt.Errorf(
			"%w: provider message id is invalid", ErrAgentEmailOutboundInputInvalid)
	}
	return value, nil
}

func normalizeAgentEmailOutboundRetryError(code string) (string, error) {
	code, err := normalizeAgentEmailOutboundErrorCode(code)
	if err != nil {
		return "", err
	}
	switch code {
	case AgentEmailOutboundErrorProviderUnavailable,
		AgentEmailOutboundErrorProviderRateLimited,
		AgentEmailOutboundErrorProviderFailed,
		AgentEmailOutboundErrorProviderTimeout,
		AgentEmailOutboundErrorProviderConnectionReset,
		AgentEmailOutboundErrorProviderResponseInvalid:
		return code, nil
	default:
		return "", fmt.Errorf("%w: error code is not retryable",
			ErrAgentEmailOutboundInputInvalid)
	}
}

func isAgentEmailOutboundUncertainRetryError(code string) bool {
	switch code {
	case AgentEmailOutboundErrorProviderTimeout,
		AgentEmailOutboundErrorProviderConnectionReset,
		AgentEmailOutboundErrorProviderResponseInvalid:
		return true
	default:
		return false
	}
}

func normalizeAgentEmailOutboundAmbiguousError(code string) (string, error) {
	code, err := normalizeAgentEmailOutboundErrorCode(code)
	if err != nil {
		return "", err
	}
	switch code {
	case AgentEmailOutboundErrorProviderTimeout,
		AgentEmailOutboundErrorProviderConnectionReset,
		AgentEmailOutboundErrorProviderResponseInvalid,
		AgentEmailOutboundErrorWorkerLeaseExpired:
		return code, nil
	default:
		return "", fmt.Errorf("%w: error code is not ambiguous",
			ErrAgentEmailOutboundInputInvalid)
	}
}

func agentEmailOutboundByIDForUpdateTx(
	ctx context.Context,
	tx pgx.Tx,
	sendID string,
) (AgentEmailOutboundMessage, error) {
	msg, err := scanAgentEmailOutbound(tx.QueryRow(ctx,
		agentEmailOutboundSelect()+" WHERE id=$1 FOR UPDATE", sendID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundNotFound
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, fmt.Errorf("lock outbound agent email: %w", err)
	}
	return msg, nil
}

func returnAgentEmailOutboundToQueueTx(
	ctx context.Context,
	tx pgx.Tx,
	sendID string,
	claim AgentEmailOutboundClaimFence,
	errorCode string,
	retryAt time.Time,
) (AgentEmailOutboundMessage, error) {
	msg, err := scanAgentEmailOutbound(tx.QueryRow(ctx, `
		UPDATE agent_email_outbound_messages
		   SET state='queued',provider_state='',provider='',provider_message_id='',
		       last_error_code=$4,claim_id=NULL,lease_expires_at=NULL,
		       next_attempt_at=GREATEST($5::timestamptz,clock_timestamp()),
		       provider_started_at=NULL,updated_at=clock_timestamp()
		 WHERE id=$1 AND claim_id=$2 AND claim_generation=$3
		   AND state IN ('claimed','provider_started')
		 RETURNING `+agentEmailOutboundReturningColumns(),
		sendID, claim.ClaimID, claim.Generation, errorCode, retryAt.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundClaimLost
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, fmt.Errorf("retry outbound agent email: %w", err)
	}
	return msg, nil
}

func rescheduleAgentEmailOutboundReceiptReplayTx(
	ctx context.Context,
	tx pgx.Tx,
	sendID string,
	claim AgentEmailOutboundClaimFence,
	errorCode string,
	retryAt time.Time,
) (AgentEmailOutboundMessage, error) {
	msg, err := scanAgentEmailOutbound(tx.QueryRow(ctx, `
		UPDATE agent_email_outbound_messages
		   SET last_error_code=$4,
		       lease_expires_at=GREATEST($5::timestamptz,clock_timestamp()),
		       updated_at=clock_timestamp()
		 WHERE id=$1 AND state='provider_started' AND claim_id=$2
		   AND claim_generation=$3
		 RETURNING `+agentEmailOutboundReturningColumns(),
		sendID, claim.ClaimID, claim.Generation, errorCode, retryAt.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundClaimLost
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, fmt.Errorf(
			"reschedule outbound agent-email receipt replay: %w", err)
	}
	return msg, nil
}

func failAgentEmailOutboundRetryTx(
	ctx context.Context,
	tx pgx.Tx,
	sendID string,
	claim AgentEmailOutboundClaimFence,
	provider, errorCode string,
) (AgentEmailOutboundMessage, error) {
	msg, err := scanAgentEmailOutbound(tx.QueryRow(ctx, `
		UPDATE agent_email_outbound_messages
		   SET state='failed',provider_state='failed',provider=$4,
		       provider_message_id='',last_error_code=$5,claim_id=NULL,
		       lease_expires_at=NULL,next_attempt_at=NULL,
		       failed_at=clock_timestamp(),updated_at=clock_timestamp()
		 WHERE id=$1 AND state='provider_started' AND claim_id=$2
		   AND claim_generation=$3
		 RETURNING `+agentEmailOutboundReturningColumns(),
		sendID, claim.ClaimID, claim.Generation, provider, errorCode))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundClaimLost
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, fmt.Errorf(
			"exhaust outbound agent-email retry: %w", err)
	}
	return msg, nil
}

func cancelAgentEmailOutboundClaimTx(
	ctx context.Context,
	tx pgx.Tx,
	msg AgentEmailOutboundMessage,
	claim AgentEmailOutboundClaimFence,
) (AgentEmailOutboundMessage, error) {
	updated, err := scanAgentEmailOutbound(tx.QueryRow(ctx, `
		UPDATE agent_email_outbound_messages
		   SET state='canceled',provider_state='',provider='',provider_message_id='',
		       last_error_code='dispatch_canceled',claim_id=NULL,
		       lease_expires_at=NULL,next_attempt_at=NULL,
		       canceled_at=clock_timestamp(),updated_at=clock_timestamp()
		 WHERE id=$1 AND state='claimed' AND claim_id=$2 AND claim_generation=$3
		 RETURNING `+agentEmailOutboundReturningColumns(),
		msg.ID, claim.ClaimID, claim.Generation))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundClaimLost
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, fmt.Errorf("cancel outbound agent email: %w", err)
	}
	return updated, nil
}

func abandonAgentEmailOutboundProviderReplayTx(
	ctx context.Context,
	tx pgx.Tx,
	msg AgentEmailOutboundMessage,
	claim AgentEmailOutboundClaimFence,
) (AgentEmailOutboundMessage, error) {
	updated, err := scanAgentEmailOutbound(tx.QueryRow(ctx, `
		UPDATE agent_email_outbound_messages
		   SET state='ambiguous',provider_state='',
		       last_error_code='dispatch_canceled',claim_id=NULL,
		       lease_expires_at=NULL,next_attempt_at=NULL,
		       ambiguous_at=clock_timestamp(),updated_at=clock_timestamp()
		 WHERE id=$1 AND state='provider_started' AND claim_id=$2
		   AND claim_generation=$3
		 RETURNING `+agentEmailOutboundReturningColumns(),
		msg.ID, claim.ClaimID, claim.Generation))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundClaimLost
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, fmt.Errorf(
			"abandon outbound agent-email provider replay: %w", err)
	}
	return updated, nil
}

func finalizeAgentEmailOutboundTx(
	ctx context.Context,
	tx pgx.Tx,
	msg AgentEmailOutboundMessage,
	in FinalizeAgentEmailOutboundInput,
) (AgentEmailOutboundMessage, error) {
	updated, err := scanAgentEmailOutbound(tx.QueryRow(ctx, `
		UPDATE agent_email_outbound_messages
		   SET state=$2,provider_state=$2,provider=$3,provider_message_id=$4,
		       last_error_code=$5,claim_id=NULL,lease_expires_at=NULL,
		       accepted_at=CASE WHEN $2 IN ('accepted','delivered','deferred')
		                        THEN COALESCE(accepted_at,clock_timestamp())
		                        ELSE accepted_at END,
		       delivered_at=CASE WHEN $2='delivered' THEN clock_timestamp()
		                         ELSE delivered_at END,
		       deferred_at=CASE WHEN $2='deferred' THEN clock_timestamp()
		                        ELSE deferred_at END,
		       failed_at=CASE WHEN $2 IN ('bounced','rejected','failed')
		                      THEN clock_timestamp() ELSE failed_at END,
		       updated_at=clock_timestamp()
		 WHERE id=$1 AND state='provider_started' AND claim_id=$6
		   AND claim_generation=$7
		 RETURNING `+agentEmailOutboundReturningColumns(),
		msg.ID, in.State, in.Provider, in.ProviderMessageID, in.ErrorCode,
		in.Claim.ClaimID, in.Claim.Generation))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundClaimLost
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, fmt.Errorf("finalize outbound agent email: %w", err)
	}
	return updated, nil
}

func markAgentEmailOutboundAmbiguousTx(
	ctx context.Context,
	tx pgx.Tx,
	sendID, provider, errorCode string,
) (AgentEmailOutboundMessage, error) {
	msg, err := scanAgentEmailOutbound(tx.QueryRow(ctx, `
		UPDATE agent_email_outbound_messages
		   SET state='ambiguous',provider_state='',provider=$2,
		       provider_message_id='',last_error_code=$3,claim_id=NULL,
		       lease_expires_at=NULL,next_attempt_at=NULL,
		       ambiguous_at=clock_timestamp(),updated_at=clock_timestamp()
		 WHERE id=$1 AND state='provider_started'
		 RETURNING `+agentEmailOutboundReturningColumns(), sendID, provider, errorCode))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEmailOutboundMessage{}, ErrAgentEmailOutboundClaimLost
	}
	if err != nil {
		return AgentEmailOutboundMessage{}, fmt.Errorf("mark outbound agent email ambiguous: %w", err)
	}
	return msg, nil
}

func agentEmailOutboundMatchesFinalization(
	msg AgentEmailOutboundMessage,
	in FinalizeAgentEmailOutboundInput,
) bool {
	return msg.State == in.State && msg.ProviderState == in.State &&
		msg.Provider == in.Provider && msg.ProviderMessageID == in.ProviderMessageID &&
		msg.LastErrorCode == in.ErrorCode
}

func agentEmailOutboundCountsAsSent(state string) bool {
	return state == AgentEmailOutboundAccepted ||
		state == AgentEmailOutboundDelivered || state == AgentEmailOutboundDeferred
}

// agentEmailOutboundScanTargets allows UPDATE ... RETURNING call sites to use
// the same authoritative projection without hand-maintaining a second scan.
func agentEmailOutboundScanTargets(msg *AgentEmailOutboundMessage) []any {
	return []any{
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
	}
}
