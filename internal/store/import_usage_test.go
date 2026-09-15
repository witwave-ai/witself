package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestImportUsageLegacyDimensionRoundTripPostgres(t *testing.T) {
	ctx, st, p := newUsageBoundsFixture(t)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	// Earlier releases accepted this dimension at ingestion. Seed that stored
	// shape directly because the current ingestion vocabulary must reject it.
	if _, err := st.pool.Exec(ctx, `
		WITH event AS (
		  INSERT INTO usage_events
		    (id, account_id, realm_id, agent_id, dimension, quantity, unit,
		     subject_type, subject_id, idempotency_key, metadata, occurred_at, created_at)
		  VALUES ('usg_legacy_custom_' || $1, $1, $2, $3, 'legacy_custom', 7, 'entry',
		          'legacy', 'legacy_subject', 'legacy_custom:1', '{}', $4, $4)
		  RETURNING account_id, realm_id, agent_id, dimension, quantity, unit
		)
		INSERT INTO usage_rollups
		  (account_id, realm_id, agent_id, dimension, unit, bucket, bucket_start,
		   quantity, event_count, updated_at)
		SELECT account_id, realm_id, agent_id, dimension, unit, bucket, bucket_start,
		       quantity, 1, $4
		FROM event CROSS JOIN (VALUES ('hour', $5::timestamptz), ('day', $6::timestamptz))
		  buckets(bucket, bucket_start)`,
		p.AccountID, p.RealmID, p.ID, occurredAt,
		usageBucketStart(occurredAt, UsageBucketHour), usageBucketStart(occurredAt, UsageBucketDay)); err != nil {
		t.Fatal(err)
	}
	if err := st.SuspendAccountSystem(ctx, p.AccountID, "evacuation", "legacy usage archive round trip"); err != nil {
		t.Fatal(err)
	}
	var before bytes.Buffer
	if err := st.ExportAccount(ctx, p.AccountID, "test-source", "test", &before); err != nil {
		t.Fatal(err)
	}
	manifest, beforeRows := readAvatarArchiveRows(t, before.Bytes(), SchemaVersion())
	if len(beforeRows["usage_events"]) != 1 || len(beforeRows["usage_rollups"]) != 2 {
		t.Fatalf("fixture has %d events and %d rollups, want one event and hourly/daily rollups",
			len(beforeRows["usage_events"]), len(beforeRows["usage_rollups"]))
	}
	if err := deleteAccountForIntegrationTest(ctx, st, p.AccountID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"usage_events", "usage_rollups"} {
		t.Run("malformed_"+table, func(t *testing.T) {
			_, rows := readAvatarArchiveRows(t, before.Bytes(), SchemaVersion())
			var row map[string]json.RawMessage
			if err := json.Unmarshal(rows[table][0], &row); err != nil {
				t.Fatal(err)
			}
			row["dimension"] = json.RawMessage(`"legacy-custom"`)
			var err error
			rows[table][0], err = json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			malformed := writeAvatarArchiveRows(t, manifest, manifest.Tables, rows)
			if _, err := st.ImportAccount(ctx, p.AccountID, bytes.NewReader(malformed)); !errors.Is(err, ErrArchiveContent) {
				t.Fatalf("malformed dimension import = %v, want ErrArchiveContent", err)
			}
		})
	}
	if _, err := st.ImportAccount(ctx, p.AccountID, bytes.NewReader(before.Bytes())); err != nil {
		t.Fatalf("import legacy dimension and matching rollups: %v", err)
	}
	var after bytes.Buffer
	if err := st.ExportAccount(ctx, p.AccountID, "test-destination", "test", &after); err != nil {
		t.Fatal(err)
	}
	_, afterRows := readAvatarArchiveRows(t, after.Bytes(), SchemaVersion())
	for _, table := range []string{"usage_events", "usage_rollups"} {
		if !reflect.DeepEqual(beforeRows[table], afterRows[table]) {
			t.Fatalf("%s changed across legacy-dimension import and re-export\nbefore: %s\nafter: %s",
				table, beforeRows[table], afterRows[table])
		}
	}
}
