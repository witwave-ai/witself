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

const avatarLockedLayerDigestBackfillBatchSize = 128

type avatarLockedLayerDigestBackfillStats struct {
	rows         int
	batches      int
	maxBatchSize int
}

// finalizeAvatarLockedLayerDigestMigration performs the domain-aware half of
// schema 51. Goose first adds a nullable column; startup then derives every
// pre-existing row with the same normalized projection used by new proposals
// and finally makes the column mandatory. A crash is retry-safe: schema 51 is
// left non-serving and the next startup resumes only NULL rows.
func (s *Store) finalizeAvatarLockedLayerDigestMigration(ctx context.Context) (avatarLockedLayerDigestBackfillStats, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return avatarLockedLayerDigestBackfillStats{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var alreadyFinalized bool
	if err := tx.QueryRow(ctx, `
		SELECT attnotnull
		  FROM pg_attribute
		 WHERE attrelid='agent_avatar_versions'::regclass
		   AND attname='locked_layers_sha256' AND NOT attisdropped`).
		Scan(&alreadyFinalized); err != nil {
		return avatarLockedLayerDigestBackfillStats{}, fmt.Errorf("inspect avatar digest migration: %w", err)
	}
	if alreadyFinalized {
		return avatarLockedLayerDigestBackfillStats{}, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE agent_avatar_versions IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return avatarLockedLayerDigestBackfillStats{}, fmt.Errorf("lock avatar versions: %w", err)
	}
	stats, err := backfillAvatarLockedLayerDigestsTx(ctx, tx)
	if err != nil {
		return avatarLockedLayerDigestBackfillStats{}, err
	}
	// A validated temporary CHECK lets PostgreSQL prove the metadata-only
	// SET NOT NULL without a second unconstrained heap validation scan.
	const notNullConstraint = "agent_avatar_versions_locked_layers_sha256_not_null"
	if _, err := tx.Exec(ctx, `
		ALTER TABLE agent_avatar_versions
		ADD CONSTRAINT `+notNullConstraint+`
		CHECK (locked_layers_sha256 IS NOT NULL) NOT VALID`); err != nil {
		return avatarLockedLayerDigestBackfillStats{}, fmt.Errorf("add avatar digest not-null proof: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		ALTER TABLE agent_avatar_versions
		VALIDATE CONSTRAINT `+notNullConstraint); err != nil {
		return avatarLockedLayerDigestBackfillStats{}, fmt.Errorf("validate avatar digest not-null proof: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		ALTER TABLE agent_avatar_versions
		ALTER COLUMN locked_layers_sha256 SET NOT NULL`); err != nil {
		return avatarLockedLayerDigestBackfillStats{}, fmt.Errorf("require avatar locked-layer digests: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		ALTER TABLE agent_avatar_versions
		DROP CONSTRAINT `+notNullConstraint); err != nil {
		return avatarLockedLayerDigestBackfillStats{}, fmt.Errorf("drop avatar digest not-null proof: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return avatarLockedLayerDigestBackfillStats{}, err
	}
	return stats, nil
}

func backfillAvatarLockedLayerDigestsTx(ctx context.Context, tx pgx.Tx) (avatarLockedLayerDigestBackfillStats, error) {
	type styleKey struct {
		accountID string
		realmID   string
		packID    string
		version   int
	}
	var stats avatarLockedLayerDigestBackfillStats
	firstBatch := true
	var afterAccount, afterRealm, afterAgent string
	var afterVersion int64
	for {
		rows, err := tx.Query(ctx, `
			SELECT account_id, realm_id, agent_id, style_pack_id,
			       style_pack_version, version, svg
			  FROM agent_avatar_versions
			 WHERE locked_layers_sha256 IS NULL
			   AND ($1::boolean OR
			        (account_id, realm_id, agent_id, version) >
			        ($2::text, $3::text, $4::text, $5::bigint))
			 ORDER BY account_id, realm_id, agent_id, version
			 LIMIT $6`, firstBatch, afterAccount, afterRealm, afterAgent,
			afterVersion, avatarLockedLayerDigestBackfillBatchSize)
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
			return stats, nil
		}
		stats.batches++
		stats.rows += len(pending)
		if len(pending) > stats.maxBatchSize {
			stats.maxBatchSize = len(pending)
		}
		// The cache is deliberately batch-scoped: imported histories can span
		// an unbounded number of immutable custom style versions.
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
		last := pending[len(pending)-1]
		afterAccount, afterRealm, afterAgent, afterVersion =
			last.accountID, last.realmID, last.agentID, last.avatarVersion
		firstBatch = false
	}
}
