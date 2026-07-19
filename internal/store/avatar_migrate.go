package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

type avatarLockedLayerDigestBackfillRow struct {
	accountID     string
	realmID       string
	agentID       string
	stylePackID   string
	styleVersion  int
	avatarVersion int64
	svg           *string
}

type avatarLockedLayerDigestBackfillFilter struct {
	accountID string
	realmID   string
	agentID   string
}

const avatarLockedLayerDigestBackfillBatchSize = 128

type avatarLockedLayerDigestBackfillStats struct {
	rows         int
	batches      int
	maxBatchSize int
}

// finalizeAvatarLockedLayerDigestMigration performs the domain-aware half of
// schema 51. Goose leaves the digest nullable so schema-50 writers can keep
// inserting during a rolling deployment. Each startup repairs at most one
// bounded batch per transaction and repeats until no visible NULL row remains.
// A crash or a concurrent legacy insert is safe: a later batch or restart
// resumes from the remaining NULL rows without a table-wide lock.
func (s *Store) finalizeAvatarLockedLayerDigestMigration(ctx context.Context) (avatarLockedLayerDigestBackfillStats, error) {
	return s.backfillAvatarLockedLayerDigests(ctx,
		avatarLockedLayerDigestBackfillFilter{}, nil)
}

// backfillAvatarLockedLayerDigests is split from the public startup wrapper so
// integration tests can place an exact legacy insert between committed
// batches. afterBatch is test-only coordination and is nil in production.
func (s *Store) backfillAvatarLockedLayerDigests(
	ctx context.Context,
	filter avatarLockedLayerDigestBackfillFilter,
	afterBatch func(avatarLockedLayerDigestBackfillStats) error,
) (avatarLockedLayerDigestBackfillStats, error) {
	var total avatarLockedLayerDigestBackfillStats
	for {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return avatarLockedLayerDigestBackfillStats{}, err
		}
		batch, err := backfillAvatarLockedLayerDigestBatchTx(ctx, tx, filter)
		if err != nil {
			_ = tx.Rollback(ctx)
			return avatarLockedLayerDigestBackfillStats{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return avatarLockedLayerDigestBackfillStats{}, err
		}
		if batch.rows == 0 {
			return total, nil
		}
		total.rows += batch.rows
		total.batches++
		if batch.maxBatchSize > total.maxBatchSize {
			total.maxBatchSize = batch.maxBatchSize
		}
		if afterBatch != nil {
			if err := afterBatch(total); err != nil {
				return avatarLockedLayerDigestBackfillStats{}, err
			}
		}
	}
}

// backfillAvatarLockedLayerDigestsTx repairs one frozen account or locked
// avatar target while its caller already owns the surrounding transaction.
// Memory remains batch-bounded even though the caller deliberately keeps one
// snapshot for export or one mutation fence for quota cleanup.
func backfillAvatarLockedLayerDigestsTx(
	ctx context.Context,
	tx pgx.Tx,
	filter avatarLockedLayerDigestBackfillFilter,
) (avatarLockedLayerDigestBackfillStats, error) {
	var total avatarLockedLayerDigestBackfillStats
	for {
		batch, err := backfillAvatarLockedLayerDigestBatchTx(ctx, tx, filter)
		if err != nil {
			return avatarLockedLayerDigestBackfillStats{}, err
		}
		if batch.rows == 0 {
			return total, nil
		}
		total.rows += batch.rows
		total.batches++
		if batch.maxBatchSize > total.maxBatchSize {
			total.maxBatchSize = batch.maxBatchSize
		}
	}
}

func backfillAvatarLockedLayerDigestBatchTx(
	ctx context.Context,
	tx pgx.Tx,
	filter avatarLockedLayerDigestBackfillFilter,
) (avatarLockedLayerDigestBackfillStats, error) {
	rows, err := tx.Query(ctx, `
		SELECT account_id, realm_id, agent_id, style_pack_id,
		       style_pack_version, version, svg
		  FROM agent_avatar_versions
		 WHERE locked_layers_sha256 IS NULL
		   AND ($1::text='' OR account_id=$1)
		   AND ($2::text='' OR realm_id=$2)
		   AND ($3::text='' OR agent_id=$3)
		 ORDER BY account_id, realm_id, agent_id, version
		 LIMIT $4
		 FOR UPDATE`, filter.accountID, filter.realmID, filter.agentID,
		avatarLockedLayerDigestBackfillBatchSize)
	if err != nil {
		return avatarLockedLayerDigestBackfillStats{}, fmt.Errorf("list avatar digest backfill batch: %w", err)
	}
	pending := make([]avatarLockedLayerDigestBackfillRow, 0,
		avatarLockedLayerDigestBackfillBatchSize)
	for rows.Next() {
		var row avatarLockedLayerDigestBackfillRow
		if err := rows.Scan(&row.accountID, &row.realmID, &row.agentID,
			&row.stylePackID, &row.styleVersion, &row.avatarVersion,
			&row.svg); err != nil {
			rows.Close()
			return avatarLockedLayerDigestBackfillStats{}, err
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return avatarLockedLayerDigestBackfillStats{}, err
	}
	rows.Close()
	if len(pending) == 0 {
		return avatarLockedLayerDigestBackfillStats{}, nil
	}

	type styleKey struct {
		accountID string
		realmID   string
		packID    string
		version   int
	}
	// The cache is deliberately batch-scoped: imported histories can span an
	// unbounded number of immutable custom style versions.
	packs := make(map[styleKey]avatardomain.StylePack)
	for _, row := range pending {
		if row.svg == nil {
			return avatarLockedLayerDigestBackfillStats{}, fmt.Errorf(
				"compacted avatar %s/%d lacks its locked-layer digest",
				row.agentID, row.avatarVersion)
		}
		key := styleKey{row.accountID, row.realmID, row.stylePackID, row.styleVersion}
		pack, ok := packs[key]
		if !ok {
			pack, err = loadAvatarStylePackVersion(ctx, tx, row.accountID,
				row.realmID, row.stylePackID, row.styleVersion)
			if err != nil {
				return avatarLockedLayerDigestBackfillStats{}, err
			}
			packs[key] = pack
		}
		digest, err := avatardomain.LockedLayersSHA256([]byte(*row.svg), pack)
		if err != nil {
			return avatarLockedLayerDigestBackfillStats{}, fmt.Errorf(
				"derive avatar %s/%d locked-layer digest: %w",
				row.agentID, row.avatarVersion, err)
		}
		command, err := tx.Exec(ctx, `
			UPDATE agent_avatar_versions SET locked_layers_sha256=$7
			 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3
			   AND style_pack_id=$4 AND style_pack_version=$5 AND version=$6
			   AND locked_layers_sha256 IS NULL`, row.accountID, row.realmID,
			row.agentID, row.stylePackID, row.styleVersion, row.avatarVersion,
			digest)
		if err != nil {
			return avatarLockedLayerDigestBackfillStats{}, fmt.Errorf(
				"store avatar locked-layer digest: %w", err)
		}
		if command.RowsAffected() != 1 {
			return avatarLockedLayerDigestBackfillStats{}, fmt.Errorf(
				"avatar %s/%d digest backfill lost its fence",
				row.agentID, row.avatarVersion)
		}
	}
	return avatarLockedLayerDigestBackfillStats{
		rows: len(pending), maxBatchSize: len(pending),
	}, nil
}
