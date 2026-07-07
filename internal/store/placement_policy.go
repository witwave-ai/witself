package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/placement"
)

func (s *Store) GetPlacementPolicy(ctx context.Context, accountID, operatorID string) (placement.Policy, error) {
	var raw []byte
	var isOwner bool
	err := s.pool.QueryRow(ctx,
		`SELECT a.placement_policy, (o.is_root OR o.role = 'account_owner')
		 FROM accounts a
		 JOIN operators o ON o.account_id = a.id
		 WHERE a.id = $1 AND o.id = $2 AND o.deleted_at IS NULL`,
		accountID, operatorID).Scan(&raw, &isOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return placement.Policy{}, ErrAccountNotFound
	}
	if err != nil {
		return placement.Policy{}, fmt.Errorf("get placement policy: %w", err)
	}
	if !isOwner {
		return placement.Policy{}, ErrNotAccountOwner
	}
	return placement.FromJSON(raw)
}

func (s *Store) SetPlacementPolicy(ctx context.Context, accountID, operatorID string, next placement.Policy) (placement.Policy, error) {
	next, err := placement.Normalize(next)
	if err != nil {
		return placement.Policy{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return placement.Policy{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var raw []byte
	var status string
	var isOwner bool
	err = tx.QueryRow(ctx,
		`SELECT a.placement_policy, a.status, (o.is_root OR o.role = 'account_owner')
		 FROM accounts a
		 JOIN operators o ON o.account_id = a.id
		 WHERE a.id = $1 AND o.id = $2 AND o.deleted_at IS NULL
		 FOR UPDATE OF a`,
		accountID, operatorID).Scan(&raw, &status, &isOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return placement.Policy{}, ErrAccountNotFound
	}
	if err != nil {
		return placement.Policy{}, fmt.Errorf("lock placement policy: %w", err)
	}
	if !isOwner {
		return placement.Policy{}, ErrNotAccountOwner
	}
	if status != "active" {
		return placement.Policy{}, ErrAccountNotActive
	}

	current, err := placement.FromJSON(raw)
	if err != nil {
		return placement.Policy{}, fmt.Errorf("decode placement policy: %w", err)
	}
	if reflect.DeepEqual(current, next) {
		if err := tx.Commit(ctx); err != nil {
			return placement.Policy{}, err
		}
		return next, nil
	}

	nextRaw := placement.MustJSON(next)
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET placement_policy = $2 WHERE id = $1`,
		accountID, nextRaw); err != nil {
		return placement.Policy{}, fmt.Errorf("update placement policy: %w", err)
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: accountID,
		ActorKind: ActorOwner,
		ActorID:   operatorID,
		Verb:      VerbAccountPlacementPolicyChanged,
		Metadata:  placementPolicyEventMetadata(next),
	}); err != nil {
		return placement.Policy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return placement.Policy{}, err
	}
	return next, nil
}

func placementPolicyEventMetadata(p placement.Policy) map[string]any {
	raw := placement.MustJSON(p)
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}
