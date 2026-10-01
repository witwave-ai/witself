package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// This key is reserved to server-generated source triggers. Keeping automatic
// full-scope work separate prevents a caller's narrowed "owner" request from
// absorbing a generation whose omitted sources would then never be scanned.
const automaticMemoryCurationCoalescingKey = "system.automatic.owner"

// Server-generated requests under the reserved coalescing key carry one of two
// idempotency-key shapes. The importer accepts exactly these, so both writers
// and the archive validator must share the constants.
const (
	automaticMemoryCurationKeyPrefix   = "automatic:"
	memoryCurationFollowUpKeyPrefix    = "curation-follow-up:"
	memoryCurationFollowUpTrigger      = "generation_follow_up"
	memoryCurationSourceBacklogTrigger = "source_backlog"
)

// memoryCurationAutomaticQuietPeriod is the least time between applying a run
// for the reserved automatic lane and that agent's next automatic request
// becoming due. An agent that captures its own session appends transcript
// entries on every turn, so without it every apply leaves automatic work due
// at once and the foreground checkpoint stays pending on every turn. Thirty
// minutes allows at most about two automatic passes an hour in a continuously
// active session; checkpoint timing is eventual by design. Requests a client
// creates explicitly, and their follow-ups, are never delayed.
const memoryCurationAutomaticQuietPeriod = 30 * time.Minute

// lockMemoryCurationSourceLaneTx establishes the global owner mutation order
// used by source writers and curation apply: account -> curation lane -> source
// clocks/heads. Callers that may later mark work due acquire this lock before
// locking a transcript, memory clock, memory head, or evidence row. That keeps
// automatic queueing atomic without introducing a lane/head deadlock.
func lockMemoryCurationSourceLaneTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
) (MemoryCurationLane, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_curation_lanes
		  (account_id,realm_id,owner_kind,owner_id)
		VALUES ($1,$2,'agent',$3)
		ON CONFLICT DO NOTHING`, p.AccountID, p.RealmID, p.ID); err != nil {
		return MemoryCurationLane{}, fmt.Errorf("initialize source curation lane: %w", err)
	}
	lane, err := loadMemoryCurationLaneTx(ctx, tx, p, true)
	if err != nil {
		return MemoryCurationLane{}, fmt.Errorf("lock source curation lane: %w", err)
	}
	return lane, nil
}

// automaticMemoryCurationNotBeforeTx returns the end of the quiet period that
// follows this owner's most recently applied automatic run, or nil when no
// automatic run has been applied. A lane holds at most one active run, so the
// newest applied run by creation is also the most recently applied one.
func automaticMemoryCurationNotBeforeTx(ctx context.Context, tx pgx.Tx, p Principal) (*time.Time, error) {
	var appliedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT r.applied_at
		FROM memory_curation_runs r
		WHERE r.account_id=$1 AND r.realm_id=$2 AND r.owner_kind='agent' AND r.owner_id=$3
		  AND r.state='applied' AND r.applied_at IS NOT NULL
		  AND EXISTS (
		    SELECT 1 FROM memory_curation_requests q
		    WHERE q.id=r.request_id AND q.account_id=r.account_id AND q.coalescing_key=$4
		  )
		ORDER BY r.created_at DESC,r.id DESC
		LIMIT 1`, p.AccountID, p.RealmID, p.ID, automaticMemoryCurationCoalescingKey).Scan(&appliedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read automatic curation quiet period: %w", err)
	}
	notBefore := appliedAt.Add(memoryCurationAutomaticQuietPeriod)
	return &notBefore, nil
}

// markMemoryCurationDueTx records source work in the same transaction as the
// source commit. lane must have been returned by lockMemoryCurationSourceLaneTx
// in this transaction. sourceKey is an opaque retry identity, never content;
// only its digest is retained. Curation apply deliberately does not call this
// helper, preventing curator output from feeding its own next run.
func (s *Store) markMemoryCurationDueTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	lane *MemoryCurationLane,
	triggerReason, sourceKind, sourceKey string,
) error {
	if lane == nil || lane.AccountID != p.AccountID || lane.RealmID != p.RealmID ||
		lane.OwnerKind != "agent" || lane.OwnerID != p.ID {
		return fmt.Errorf("%w: source curation lane does not match owner", ErrMemoryCurationConflict)
	}
	if !memoryCodePattern.MatchString(triggerReason) || !memoryCodePattern.MatchString(sourceKind) || sourceKey == "" {
		return fmt.Errorf("%w: invalid automatic curation trigger", ErrMemoryCurationInputInvalid)
	}
	if lane.RequestGeneration >= maxMemoryCurationGeneration {
		return fmt.Errorf("%w: request generation exhausted", ErrMemoryCurationConflict)
	}

	digest := sha256.Sum256([]byte(p.AccountID + "\x00" + p.RealmID + "\x00" + p.ID + "\x00" + sourceKind + "\x00" + sourceKey))
	idempotencyKey := automaticMemoryCurationKeyPrefix + sourceKind + ":" + hex.EncodeToString(digest[:])
	requestHash, err := memoryRequestHash(struct {
		Operation     string `json:"operation"`
		SourceKind    string `json:"source_kind"`
		SourceDigest  string `json:"source_digest"`
		TriggerReason string `json:"trigger_reason"`
	}{
		Operation: "request", SourceKind: sourceKind,
		SourceDigest: hex.EncodeToString(digest[:]), TriggerReason: triggerReason,
	})
	if err != nil {
		return err
	}

	// A committed source identity is marked at most once. This is primarily a
	// defensive shield; normal source retry paths return before calling here.
	if _, replayed, err := loadMemoryCurationMutation(ctx, tx, p, "request", idempotencyKey, requestHash); err != nil {
		return err
	} else if replayed {
		return nil
	}

	nextGeneration := lane.RequestGeneration + 1
	tag, err := tx.Exec(ctx, `
		UPDATE memory_curation_lanes
		SET request_generation=$5,updated_at=clock_timestamp()
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND request_generation=$4`, p.AccountID, p.RealmID, p.ID,
		lane.RequestGeneration, nextGeneration)
	if err != nil {
		return fmt.Errorf("advance automatic curation generation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrMemoryCurationConflict
	}
	lane.RequestGeneration = nextGeneration

	scope, err := normalizeMemoryCurationScope(MemoryCurationScope{})
	if err != nil {
		return err
	}
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return err
	}

	notBefore, err := automaticMemoryCurationNotBeforeTx(ctx, tx, p)
	if err != nil {
		return err
	}
	var requestID, requestState string
	err = tx.QueryRow(ctx, `
		SELECT id FROM memory_curation_requests
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND coalescing_key=$4 AND state IN ('queued','claimed','retry_wait')
		ORDER BY created_at,id LIMIT 1 FOR UPDATE`, p.AccountID, p.RealmID, p.ID,
		automaticMemoryCurationCoalescingKey).Scan(&requestID)
	switch {
	case err == nil:
		// A source commit may make waiting work due sooner, but never inside
		// the quiet period after this owner's last automatic apply.
		err = tx.QueryRow(ctx, `
			UPDATE memory_curation_requests
			SET request_generation=$2,
			    due_at=LEAST(due_at,GREATEST(clock_timestamp(),$3::timestamptz)),
			    state=CASE WHEN state='retry_wait' THEN 'queued' ELSE state END,
			    updated_at=clock_timestamp()
			WHERE id=$1
			RETURNING state`, requestID, nextGeneration, notBefore).Scan(&requestState)
		if err != nil {
			return fmt.Errorf("coalesce automatic curation request: %w", err)
		}
	case errors.Is(err, pgx.ErrNoRows):
		requestID, err = id.New("mcrq")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO memory_curation_requests
			  (id,account_id,realm_id,owner_kind,owner_id,scope,coalescing_key,
			   trigger_reason,request_generation,priority,due_at,state,attempt_count,
			   max_attempts,fulfilled_generation,read_only_replay,actor_kind,actor_id,
			   idempotency_key,request_hash)
			VALUES ($1,$2,$3,'agent',$4,$5::jsonb,$6,$7,$8,0,
			        GREATEST(clock_timestamp(),$12::timestamptz),'queued',0,$9,0,false,
			        'agent',$4,$10,$11)`,
			requestID, p.AccountID, p.RealmID, p.ID, scopeJSON,
			automaticMemoryCurationCoalescingKey, triggerReason, nextGeneration,
			defaultMemoryCurationAttempts, idempotencyKey, requestHash, notBefore)
		if err != nil {
			return fmt.Errorf("insert automatic curation request: %w", err)
		}
		requestState = MemoryCurationRequestQueued
	default:
		return fmt.Errorf("find automatic curation request: %w", err)
	}

	receipt, err := insertMemoryCurationMutation(ctx, tx, p, MemoryCurationMutationReceipt{
		Operation: "request", ActorID: p.ID, IdempotencyKey: idempotencyKey,
		RequestHash: requestHash, RequestID: requestID,
		RequestGeneration: nextGeneration, ResultState: requestState,
	})
	if err != nil {
		return err
	}
	if err := s.logMemoryCurationEventTx(ctx, tx, p, VerbMemoryCurationRequested,
		requestID, "", receipt.RequestGeneration, 0, receipt.ResultState); err != nil {
		return err
	}
	return nil
}
