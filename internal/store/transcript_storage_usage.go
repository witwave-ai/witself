package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const transcriptStorageUsageBulkSetting = "witself.transcript_storage_usage_bulk"

// enableTranscriptStorageUsageBulkTx skips trigger accounting until this
// transaction ends. Import rebuilds the projection after loading all entries.
func enableTranscriptStorageUsageBulkTx(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT set_config($1, 'on', true)`, transcriptStorageUsageBulkSetting)
	return err
}

// recomputeTranscriptStorageUsageTx rebuilds the cell-local projection from
// canonical entries. Counter rows are never portable archive truth.
func recomputeTranscriptStorageUsageTx(ctx context.Context, tx pgx.Tx, accountID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM transcript_storage_usage WHERE account_id=$1`, accountID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO transcript_storage_usage (account_id, shard, retained_bytes)
		SELECT account_id, witself_transcript_storage_shard(transcript_id),
		       sum(witself_transcript_entry_storage_bytes(
		         external_id, role, body, payload, model, reply_to_entry_id, artifacts
		       ))::bigint
		  FROM transcript_entries
		 WHERE account_id=$1
		 GROUP BY 1, 2
		 ORDER BY 1, 2`, accountID)
	return err
}

func transcriptStorageUsedTx(ctx context.Context, tx pgx.Tx, accountID string) (int64, error) {
	var used int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(retained_bytes),0)::bigint
		  FROM transcript_storage_usage WHERE account_id=$1`, accountID).Scan(&used)
	return used, err
}

func transcriptEntriesStorageBytesTx(ctx context.Context, tx pgx.Tx, accountID string, entryIDs []string) (int64, error) {
	var used int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(witself_transcript_entry_storage_bytes(
		         external_id, role, body, payload, model, reply_to_entry_id, artifacts
		       )),0)::bigint
		  FROM transcript_entries WHERE account_id=$1 AND id = ANY($2)`, accountID, entryIDs).Scan(&used)
	return used, err
}
