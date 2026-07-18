package store

import (
	"context"
	"fmt"

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

// finalizeAvatarLockedLayerDigestMigration performs the domain-aware half of
// schema 51. Goose first adds a nullable column; startup then derives every
// pre-existing row with the same normalized projection used by new proposals
// and finally makes the column mandatory. A crash is retry-safe: schema 51 is
// left non-serving and the next startup resumes only NULL rows.
func (s *Store) finalizeAvatarLockedLayerDigestMigration(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var alreadyFinalized bool
	if err := tx.QueryRow(ctx, `
		SELECT attnotnull
		  FROM pg_attribute
		 WHERE attrelid='agent_avatar_versions'::regclass
		   AND attname='locked_layers_sha256' AND NOT attisdropped`).
		Scan(&alreadyFinalized); err != nil {
		return fmt.Errorf("inspect avatar digest migration: %w", err)
	}
	if alreadyFinalized {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE agent_avatar_versions IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("lock avatar versions: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT account_id, realm_id, agent_id, style_pack_id,
		       style_pack_version, version, svg
		  FROM agent_avatar_versions
		 WHERE locked_layers_sha256 IS NULL
		 ORDER BY account_id, realm_id, agent_id, version`)
	if err != nil {
		return fmt.Errorf("list avatar digest backfill: %w", err)
	}
	pending := make([]avatarLockedLayerDigestBackfillRow, 0)
	for rows.Next() {
		var row avatarLockedLayerDigestBackfillRow
		if err := rows.Scan(&row.accountID, &row.realmID, &row.agentID,
			&row.stylePackID, &row.styleVersion, &row.avatarVersion,
			&row.svg); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	type styleKey struct {
		accountID string
		realmID   string
		packID    string
		version   int
	}
	packs := make(map[styleKey]avatardomain.StylePack)
	for _, row := range pending {
		if row.svg == nil {
			return fmt.Errorf("compacted avatar %s/%d lacks its locked-layer digest",
				row.agentID, row.avatarVersion)
		}
		key := styleKey{row.accountID, row.realmID, row.stylePackID, row.styleVersion}
		pack, ok := packs[key]
		if !ok {
			pack, err = loadAvatarStylePackVersion(ctx, tx, row.accountID,
				row.realmID, row.stylePackID, row.styleVersion)
			if err != nil {
				return err
			}
			packs[key] = pack
		}
		digest, err := avatardomain.LockedLayersSHA256([]byte(*row.svg), pack)
		if err != nil {
			return fmt.Errorf("derive avatar %s/%d locked-layer digest: %w",
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
			return fmt.Errorf("store avatar locked-layer digest: %w", err)
		}
		if command.RowsAffected() != 1 {
			return fmt.Errorf("avatar %s/%d digest backfill lost its fence",
				row.agentID, row.avatarVersion)
		}
	}
	if _, err := tx.Exec(ctx, `
		ALTER TABLE agent_avatar_versions
		ALTER COLUMN locked_layers_sha256 SET NOT NULL`); err != nil {
		return fmt.Errorf("require avatar locked-layer digests: %w", err)
	}
	return tx.Commit(ctx)
}
