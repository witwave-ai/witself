package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// This key is reserved to server-generated source triggers. Keeping automatic
// full-scope work separate prevents a caller's narrowed "owner" request from
// absorbing a generation whose omitted sources would then never be scanned.
const automaticMemoryCurationCoalescingKey = "system.automatic.owner"

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

// markMemoryCurationDueTx records source work in the same transaction as the
// source commit. lane must have been returned by lockMemoryCurationSourceLaneTx
// in this transaction. sourceKey is an opaque retry identity, never content;
// only its digest is retained. Curation apply deliberately does not call this
// helper, preventing curator output from feeding its own next run.
func markMemoryCurationDueTx(
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
	idempotencyKey := "automatic:" + sourceKind + ":" + hex.EncodeToString(digest[:])
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

	var requestID, requestState string
	err = tx.QueryRow(ctx, `
		SELECT id FROM memory_curation_requests
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND coalescing_key=$4 AND state IN ('queued','claimed','retry_wait')
		ORDER BY created_at,id LIMIT 1 FOR UPDATE`, p.AccountID, p.RealmID, p.ID,
		automaticMemoryCurationCoalescingKey).Scan(&requestID)
	switch {
	case err == nil:
		err = tx.QueryRow(ctx, `
			UPDATE memory_curation_requests
			SET request_generation=$2,due_at=LEAST(due_at,clock_timestamp()),
			    state=CASE WHEN state='retry_wait' THEN 'queued' ELSE state END,
			    updated_at=clock_timestamp()
			WHERE id=$1
			RETURNING state`, requestID, nextGeneration).Scan(&requestState)
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
			        clock_timestamp(),'queued',0,$9,0,false,'agent',$4,$10,$11)`,
			requestID, p.AccountID, p.RealmID, p.ID, scopeJSON,
			automaticMemoryCurationCoalescingKey, triggerReason, nextGeneration,
			defaultMemoryCurationAttempts, idempotencyKey, requestHash)
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
	if err := logMemoryCurationEventTx(ctx, tx, p, VerbMemoryCurationRequested,
		requestID, "", receipt.RequestGeneration, 0, receipt.ResultState); err != nil {
		return err
	}
	return nil
}
