package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

var migrationTestSchemaSequence atomic.Uint64

func TestMigration37MessageAudiencePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 36)
	insertMigrationTestMemoryPrincipals(t, st)
	ctx := context.Background()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	insertMigrationTestMessage(t, tx, "msg_schema_36_audience", "agent_memory_sender", "agent_memory_recipient")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 37)
	assertMigrationTestVersion(t, dsn, 37)
	assertMigrationTestColumn(t, st, "agent_messages", "audience_kind", true)
	assertMigrationTestColumn(t, st, "agent_messages", "audience_fingerprint", true)
	assertMigrationTestTableConstraint(t, st, "agent_messages", "agent_messages_audience_shape", true)

	var audience, fingerprint string
	var toAgentID *string
	if err := st.pool.QueryRow(ctx, `
		SELECT audience_kind,audience_fingerprint,to_agent_id
		FROM agent_messages WHERE id='msg_schema_36_audience'`).Scan(&audience, &fingerprint, &toAgentID); err != nil {
		t.Fatal(err)
	}
	if audience != MessageRecipientAgent || fingerprint != "" || toAgentID == nil || *toAgentID != "agent_memory_recipient" {
		t.Fatalf("legacy audience = %q/%q/%v", audience, fingerprint, toAgentID)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_messages
		SET audience_kind='realm', audience_fingerprint=$2
		WHERE id=$1`, "msg_schema_36_audience", strings.Repeat("a", 64)); err == nil {
		t.Fatal("realm audience retained a direct to_agent_id")
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_messages
		SET audience_kind='realm', audience_fingerprint=$2, to_agent_id=NULL
		WHERE id=$1`, "msg_schema_36_audience", strings.Repeat("a", 64)); err != nil {
		t.Fatalf("valid realm audience: %v", err)
	}
}

func TestMigration38Postgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}

	t.Run("fresh database applies every migration", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 38)
		assertMigrationTestColumn(t, st, "agent_messages", "audience_kind", true)
		assertMigrationTestColumn(t, st, "agent_messages", "audience_fingerprint", true)
		assertMigrationTestTableConstraint(t, st, "agent_messages", "agent_messages_audience_shape", true)
		for _, table := range []string{
			"agent_message_requests", "agent_message_request_candidates",
			"agent_message_request_selections", "agent_message_request_claims",
		} {
			assertMigrationTestTable(t, st, table, true)
		}
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", false)
		assertMigrationTestTableConstraint(t, st, "tokens", "tokens_access_profile_kind_check", true)
		assertMigrationTestColumn(t, st, "tokens", "access_profile", true)
		assertMigrationTestColumn(t, st, "agent_messages", "reply_to_message_id", true)
		assertMigrationTestTableConstraint(t, st, "agent_messages", "agent_messages_reply_parent_fk", true)
		assertMigrationTestTableConstraint(t, st, "agent_messages", "agent_messages_reply_not_self", true)
		assertMigrationTestColumn(t, st, "agent_messages", "causal_depth", true)
		assertMigrationTestTableConstraint(t, st, "agent_messages", "agent_messages_causal_depth_range", true)
		assertMigrationTestIndex(t, st, "agent_messages", "agent_messages_by_recipient_activity", true)
		assertMigrationTestColumn(t, st, "agent_message_deliveries", "processing_state", true)
		assertMigrationTestColumn(t, st, "agent_message_deliveries", "failure_count", true)
		assertMigrationTestTableConstraint(t, st, "agent_message_deliveries", "agent_message_deliveries_failure_count_range", true)
		assertMigrationTestTableConstraint(t, st, "agent_message_deliveries", "agent_message_deliveries_processing_shape", true)
		assertMigrationTestTableConstraint(t, st, "agent_message_deliveries", "agent_message_deliveries_result_message_fk", true)
		assertMigrationTestTableConstraint(t, st, "agent_message_deliveries", "agent_message_deliveries_result_message_unique", true)
	})

	t.Run("message request and audience migrations roll back in order", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 37)
		assertMigrationTestTable(t, st, "agent_message_requests", false)
		assertMigrationTestColumn(t, st, "agent_messages", "audience_kind", true)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 36)
		assertMigrationTestColumn(t, st, "agent_messages", "audience_kind", false)
	})

	t.Run("schema 30 credentials upgrade to full profiles", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 30)
		ctx := context.Background()
		provisioned, err := st.ProvisionAccount(ctx, "migration-curator@witwave.ai", "migration curator", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
			t.Fatalf("activate = %t / %v", activated, err)
		}
		realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
		if err != nil {
			t.Fatal(err)
		}
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "legacy")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := st.CreateAgentToken(ctx, provisioned.AccountID, provisioned.OperatorID, agent.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := st.CreateOperatorToken(ctx, provisioned.AccountID, provisioned.OperatorID, "legacy operator", nil); err != nil {
			t.Fatal(err)
		}

		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 38)
		assertMigrationTestTableConstraint(t, st, "tokens", "tokens_access_profile_kind_check", true)
		var total, full int
		if err := st.pool.QueryRow(ctx, `
			SELECT COUNT(*), COUNT(*) FILTER (WHERE access_profile='full')
			FROM tokens WHERE account_id=$1`, provisioned.AccountID).Scan(&total, &full); err != nil {
			t.Fatal(err)
		}
		if total == 0 || full != total {
			t.Fatalf("full-profile legacy tokens = %d/%d", full, total)
		}

		if _, err := st.pool.Exec(ctx, `
			UPDATE tokens SET access_profile='curator-preview'
			WHERE account_id=$1 AND kind='operator'`, provisioned.AccountID); err == nil {
			t.Fatal("operator token accepted curator-preview profile")
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE tokens SET access_profile='curator-admin'
			WHERE account_id=$1 AND kind='agent'`, provisioned.AccountID); err == nil {
			t.Fatal("agent token accepted unknown profile")
		}
		for _, profile := range []string{AccessProfileCuratorPreview, AccessProfileCuratorApply, AccessProfileFull} {
			if _, err := st.pool.Exec(ctx, `
				UPDATE tokens SET access_profile=$1
				WHERE account_id=$2 AND kind='agent'`, profile, provisioned.AccountID); err != nil {
				t.Fatalf("agent profile %q: %v", profile, err)
			}
		}
	})

	t.Run("schema 32 messages upgrade with null reply parent", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 32)
		insertMigrationTestMemoryPrincipals(t, st)
		ctx := context.Background()
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		insertMigrationTestMessage(t, tx, "msg_schema_32", "agent_memory_sender", "agent_memory_recipient")
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 38)
		assertMigrationTestColumn(t, st, "agent_messages", "reply_to_message_id", true)
		var parent *string
		if err := st.pool.QueryRow(ctx, `
			SELECT reply_to_message_id FROM agent_messages WHERE id='msg_schema_32'`).Scan(&parent); err != nil {
			t.Fatal(err)
		}
		if parent != nil {
			t.Fatalf("schema-32 message reply parent = %q, want NULL", *parent)
		}
	})

	t.Run("schema 33 deliveries upgrade to available processing", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 33)
		insertMigrationTestMemoryPrincipals(t, st)
		ctx := context.Background()
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		insertMigrationTestMessage(t, tx, "msg_schema_33", "agent_memory_sender", "agent_memory_recipient")
		mustMigrationTestExec(t, tx, `
			INSERT INTO agent_message_deliveries
			  (message_id,account_id,realm_id,recipient_agent_id,state,delivered_at)
			VALUES
			  ('msg_schema_33','acc_memory_trigger','realm_memory_trigger',
			   'agent_memory_recipient','delivered',clock_timestamp())`)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 38)
		var state, claimHash, completeHash string
		var generation int64
		var claimID, lease, completedAt, resultID any
		if err := st.pool.QueryRow(ctx, `
			SELECT processing_state,processing_generation,claim_id,claim_key_hash,
			       lease_expires_at,completed_at,complete_key_hash,result_message_id
			FROM agent_message_deliveries WHERE message_id='msg_schema_33'`).
			Scan(&state, &generation, &claimID, &claimHash, &lease, &completedAt, &completeHash, &resultID); err != nil {
			t.Fatal(err)
		}
		if state != MessageProcessingAvailable || generation != 0 || claimID != nil ||
			claimHash != "" || lease != nil || completedAt != nil || completeHash != "" || resultID != nil {
			t.Fatalf("schema-33 processing defaults = %q/%d/%v/%q/%v/%v/%q/%v",
				state, generation, claimID, claimHash, lease, completedAt, completeHash, resultID)
		}
	})

	t.Run("schema 34 reply graph upgrades to trusted causal depth", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 34)
		insertMigrationTestMemoryPrincipals(t, st)
		ctx := context.Background()
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		insertMigrationTestMessage(t, tx, "msg_depth_root", "agent_memory_sender", "agent_memory_recipient")
		mustMigrationTestExec(t, tx, `
			INSERT INTO agent_messages
			  (id,account_id,realm_id,from_agent_id,to_agent_id,body,thread_id,reply_to_message_id)
			VALUES
			  ('msg_depth_reply_1','acc_memory_trigger','realm_memory_trigger',
			   'agent_memory_recipient','agent_memory_sender','reply one','thr_memory_trigger','msg_depth_root'),
			  ('msg_depth_reply_2','acc_memory_trigger','realm_memory_trigger',
			   'agent_memory_sender','agent_memory_recipient','reply two','thr_memory_trigger','msg_depth_reply_1'),
			  ('msg_depth_forged_thread_root','acc_memory_trigger','realm_memory_trigger',
			   'agent_memory_sender','agent_memory_recipient','new root','thr_memory_trigger',NULL)`)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 38)
		for messageID, want := range map[string]int64{
			"msg_depth_root": 1, "msg_depth_reply_1": 2,
			"msg_depth_reply_2": 3, "msg_depth_forged_thread_root": 1,
		} {
			var got int64
			if err := st.pool.QueryRow(ctx, `SELECT causal_depth FROM agent_messages WHERE id=$1`, messageID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("%s causal depth = %d, want %d", messageID, got, want)
			}
		}
	})

	t.Run("schema 35 deliveries upgrade with zero deterministic failures", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 35)
		insertMigrationTestMemoryPrincipals(t, st)
		ctx := context.Background()
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		insertMigrationTestMessage(t, tx, "msg_schema_35", "agent_memory_sender", "agent_memory_recipient")
		mustMigrationTestExec(t, tx, `
			INSERT INTO agent_message_deliveries
			  (message_id,account_id,realm_id,recipient_agent_id,state,delivered_at)
			VALUES
			  ('msg_schema_35','acc_memory_trigger','realm_memory_trigger',
			   'agent_memory_recipient','delivered',clock_timestamp())`)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 38)
		var failureCount int64
		if err := st.pool.QueryRow(ctx, `
			SELECT failure_count FROM agent_message_deliveries
			WHERE message_id='msg_schema_35'`).Scan(&failureCount); err != nil {
			t.Fatal(err)
		}
		if failureCount != 0 {
			t.Fatalf("schema-35 failure count = %d, want 0", failureCount)
		}
		for _, invalid := range []int64{-1, maxMessageFailureCount + 1} {
			if _, err := st.pool.Exec(ctx, `
				UPDATE agent_message_deliveries SET failure_count=$1
				WHERE message_id='msg_schema_35'`, invalid); err == nil {
				t.Fatalf("failure_count range constraint accepted %d", invalid)
			}
		}
	})

	t.Run("message evidence requires owner participation", func(t *testing.T) {
		st, _ := newMigrationTestStore(t, baseDSN)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		insertMigrationTestMemoryPrincipals(t, st)

		ctx := context.Background()
		validTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		insertMigrationTestMessage(t, validTx, "msg_owner_participant", "agent_memory_owner", "agent_memory_sender")
		insertMigrationTestMemoryVersion(t, validTx, memoryArchiveOneID, 1)
		insertMigrationTestMessageEvidence(t, validTx, "mev_message_valid", memoryArchiveOneID, "msg_owner_participant", 2)
		if err := validTx.Commit(ctx); err != nil {
			t.Fatalf("commit evidence for a message participant: %v", err)
		}

		invalidTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = invalidTx.Rollback(ctx) }()
		insertMigrationTestMessage(t, invalidTx, "msg_owner_absent", "agent_memory_sender", "agent_memory_recipient")
		insertMigrationTestMemoryVersion(t, invalidTx, memoryArchiveBadID, 3)
		insertMigrationTestMessageEvidence(t, invalidTx, "mev_message_invalid", memoryArchiveBadID, "msg_owner_absent", 4)
		err = invalidTx.Commit(ctx)
		if err == nil || !strings.Contains(err.Error(), "memory owner did not participate in source message") {
			t.Fatalf("commit error = %v, want message participant constraint", err)
		}
	})

	t.Run("superseded version has one active supersession set", func(t *testing.T) {
		st, _ := newMigrationTestStore(t, baseDSN)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		insertMigrationTestMemoryPrincipals(t, st)

		ctx := context.Background()
		validTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		insertMigrationTestMemoryWithUnavailableEvidence(t, validTx, memoryArchiveTargetID, 1, 2)
		insertMigrationTestMemoryWithUnavailableEvidence(t, validTx, memoryArchiveReplacementAID, 3, 4)
		insertMigrationTestMemoryWithUnavailableEvidence(t, validTx, memoryArchiveReplacementBID, 5, 6)
		insertMigrationTestSupersessionRelation(t, validTx, memoryArchiveRelationAID, memoryArchiveReplacementAID, memoryArchiveTargetID, memoryArchivePrimarySetID, 1)
		insertMigrationTestSupersessionRelation(t, validTx, memoryArchiveRelationBID, memoryArchiveReplacementBID, memoryArchiveTargetID, memoryArchivePrimarySetID, 1)
		if err := validTx.Commit(ctx); err != nil {
			t.Fatalf("commit replacement edges in one supersession set: %v", err)
		}

		invalidTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = invalidTx.Rollback(ctx) }()
		insertMigrationTestMemoryWithUnavailableEvidence(t, invalidTx, memoryArchiveReplacementCID, 7, 8)
		insertMigrationTestSupersessionRelation(t, invalidTx, memoryArchiveRelationCID, memoryArchiveReplacementCID, memoryArchiveTargetID, memoryArchiveConflictSetID, 1)
		err = invalidTx.Commit(ctx)
		if err == nil || !strings.Contains(err.Error(), "memory version belongs to multiple active supersession sets") {
			t.Fatalf("commit error = %v, want one active supersession set constraint", err)
		}
	})

	t.Run("deleted references require scoped tombstones", func(t *testing.T) {
		st, _ := newMigrationTestStore(t, baseDSN)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		insertMigrationTestMemoryPrincipals(t, st)
		ctx := context.Background()
		seed, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		insertMigrationTestMemoryWithUnavailableEvidence(t, seed, memoryArchiveLiveID, 1, 2)
		if err := seed.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		liveTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = liveTx.Exec(ctx, `
			INSERT INTO memory_deleted_references
			  (id,account_id,realm_id,owner_kind,owner_id,deleted_memory_id,
			   former_reference_kind,related_resource_id,reason_code)
			VALUES
			  ('mdr_aaaaaaaaaaaaaaaa','acc_memory_trigger','realm_memory_trigger','agent',
			   'agent_memory_owner','mem_bbbbbbbbbbbbbbbb','idempotency.added',$1,
			   'permanent_delete')`, strings.Repeat("c", 64))
		if err != nil {
			t.Fatal(err)
		}
		err = liveTx.Commit(ctx)
		if err == nil || !strings.Contains(err.Error(), "requires a matching tombstone") {
			t.Fatalf("live deleted-reference commit error = %v", err)
		}

		crossOwner, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = crossOwner.Exec(ctx, `
			INSERT INTO memory_deleted_references
			  (id,account_id,realm_id,owner_kind,owner_id,deleted_memory_id,
			   former_reference_kind,related_resource_id,reason_code)
			VALUES
			  ('mdr_bbbbbbbbbbbbbbbb','acc_memory_trigger','realm_memory_trigger','agent',
			   'agent_memory_sender','mem_bbbbbbbbbbbbbbbb','idempotency.added',$1,
			   'permanent_delete')`, strings.Repeat("d", 64))
		if err != nil {
			t.Fatal(err)
		}
		err = crossOwner.Commit(ctx)
		if err == nil || !strings.Contains(err.Error(), "foreign key constraint") {
			t.Fatalf("cross-owner deleted-reference commit error = %v", err)
		}
	})

	t.Run("interrupted empty schema 26 install resumes", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 26)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 38)
	})

	t.Run("populated schema 26 cannot skip compatibility release", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 26)
		if _, err := st.EnsureDefaultAccount(context.Background()); err != nil {
			t.Fatal(err)
		}
		err := st.Migrate()
		if !errors.Is(err, ErrMigrationCompatibilityRequired) {
			t.Fatalf("Migrate error = %v, want errors.Is(_, ErrMigrationCompatibilityRequired)", err)
		}
		assertMigrationTestVersion(t, dsn, 26)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", true)
		var deletionColumnExists bool
		if err := st.pool.QueryRow(context.Background(), `
			SELECT EXISTS (
			  SELECT 1 FROM pg_attribute
			  WHERE attrelid=to_regclass('facts') AND attname='deleted_at' AND NOT attisdropped
			)`).Scan(&deletionColumnExists); err != nil {
			t.Fatal(err)
		}
		if deletionColumnExists {
			t.Fatal("schema-27 deletion column was applied despite compatibility preflight refusal")
		}
	})

	t.Run("populated schema 27 proceeds", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 27)
		if _, err := st.EnsureDefaultAccount(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 38)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", false)
	})

	t.Run("wrong-column legacy constraint is rejected", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 27)
		if _, err := st.pool.Exec(context.Background(), `
			ALTER TABLE facts DROP CONSTRAINT facts_owner_agent_id_subject_id_predicate_key;
			ALTER TABLE facts ADD CONSTRAINT facts_owner_agent_id_subject_id_predicate_key
			UNIQUE (owner_agent_id, subject_id, created_at)`); err != nil {
			t.Fatal(err)
		}
		err := st.Migrate()
		if err == nil || !strings.Contains(err.Error(), "legacy full-address UNIQUE constraint") {
			t.Fatalf("Migrate error = %v, want strict legacy-constraint precondition", err)
		}
		assertMigrationTestVersion(t, dsn, 27)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", true)
	})

	t.Run("wrong-column partial index is rejected", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 27)
		if _, err := st.pool.Exec(context.Background(), `
			DROP INDEX facts_one_active_address;
			CREATE UNIQUE INDEX facts_one_active_address
			ON facts (owner_agent_id, subject_id, created_at)
			WHERE deleted_at IS NULL`); err != nil {
			t.Fatal(err)
		}
		err := st.Migrate()
		if err == nil || !strings.Contains(err.Error(), "active-address partial UNIQUE index") {
			t.Fatalf("Migrate error = %v, want strict partial-index precondition", err)
		}
		assertMigrationTestVersion(t, dsn, 27)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", true)
	})

	t.Run("incomplete schema 27 deletion shape is rejected", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 27)
		if _, err := st.pool.Exec(context.Background(), `
			ALTER TABLE facts DROP CONSTRAINT facts_replacement_shape`); err != nil {
			t.Fatal(err)
		}
		err := st.Migrate()
		if err == nil || !strings.Contains(err.Error(), "complete schema-27 fact-deletion shape") {
			t.Fatalf("Migrate error = %v, want complete schema-27 shape precondition", err)
		}
		assertMigrationTestVersion(t, dsn, 27)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", true)
	})

	t.Run("clean down removes messaging, reply, and narrative schemas and can re-upgrade", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 37)
		assertMigrationTestTable(t, st, "agent_message_requests", false)
		assertMigrationTestColumn(t, st, "agent_messages", "audience_kind", true)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 36)
		assertMigrationTestColumn(t, st, "agent_messages", "audience_kind", false)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 35)
		assertMigrationTestColumn(t, st, "agent_message_deliveries", "failure_count", false)
		assertMigrationTestColumn(t, st, "agent_messages", "causal_depth", true)
		assertMigrationTestColumn(t, st, "agent_message_deliveries", "processing_state", true)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 34)
		assertMigrationTestColumn(t, st, "agent_messages", "causal_depth", false)
		assertMigrationTestIndex(t, st, "agent_messages", "agent_messages_by_recipient_activity", false)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 33)
		assertMigrationTestColumn(t, st, "agent_message_deliveries", "processing_state", false)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 32)
		assertMigrationTestColumn(t, st, "agent_messages", "reply_to_message_id", false)
		assertMigrationTestTableConstraint(t, st, "agent_messages", "agent_messages_reply_parent_fk", false)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 31)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 30)
		assertMigrationTestTableConstraint(t, st, "tokens", "tokens_access_profile_kind_check", false)
		assertMigrationTestColumn(t, st, "tokens", "access_profile", false)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 29)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 28)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", false)
		if err := st.Migrate(); err != nil {
			t.Fatalf("re-upgrade schema 28 to 38: %v", err)
		}
		assertMigrationTestVersion(t, dsn, 38)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", false)
		assertMigrationTestColumn(t, st, "agent_messages", "reply_to_message_id", true)
	})

	t.Run("down refuses duplicate recreated address without data loss", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 37)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 36)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 35)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 34)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 33)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 32)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 31)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 30)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 29)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		insertMigrationTestRecreatedAddress(t, st)
		err := migrationTestDown(t, dsn, true)
		if err == nil || !strings.Contains(err.Error(), "no fact rows were removed") {
			t.Fatalf("Down error = %v, want non-destructive duplicate refusal", err)
		}
		assertMigrationTestVersion(t, dsn, 28)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", false)
		var rows int
		if err := st.pool.QueryRow(context.Background(), `
			SELECT COUNT(*) FROM facts
			WHERE owner_agent_id='agent_migration' AND subject_id='sub_migration'
			  AND predicate='identity/name'`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 2 {
			t.Fatalf("duplicate address rows after refused Down = %d, want 2", rows)
		}
	})
}

func insertMigrationTestMemoryPrincipals(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	mustMigrationTestExec(t, tx, `
		INSERT INTO accounts (id, is_default, display_name)
		VALUES ('acc_memory_trigger', true, 'memory trigger test')`)
	mustMigrationTestExec(t, tx, `
		INSERT INTO realms (id, account_id, name)
		VALUES ('realm_memory_trigger', 'acc_memory_trigger', 'default')`)
	mustMigrationTestExec(t, tx, `
		INSERT INTO agents (id, realm_id, name)
		VALUES
		  ('agent_memory_owner', 'realm_memory_trigger', 'owner'),
		  ('agent_memory_sender', 'realm_memory_trigger', 'sender'),
		  ('agent_memory_recipient', 'realm_memory_trigger', 'recipient')`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func insertMigrationTestMessage(t *testing.T, tx pgx.Tx, id, fromAgentID, toAgentID string) {
	t.Helper()
	mustMigrationTestExec(t, tx, `
		INSERT INTO agent_messages
		  (id, account_id, realm_id, from_agent_id, to_agent_id, body, thread_id)
		VALUES ($1, 'acc_memory_trigger', 'realm_memory_trigger', $2, $3,
		        'migration trigger evidence', 'thr_memory_trigger')`, id, fromAgentID, toAgentID)
}

func insertMigrationTestMemoryVersion(t *testing.T, tx pgx.Tx, memoryID string, changeSeq int64) {
	t.Helper()
	mustMigrationTestExec(t, tx, `
		INSERT INTO memories
		  (id, account_id, realm_id, owner_kind, owner_id, origin,
		   authored_by_agent_id, current_version)
		VALUES
		  ($1, 'acc_memory_trigger', 'realm_memory_trigger', 'agent',
		   'agent_memory_owner', 'migration_test', 'agent_memory_owner', 1)`, memoryID)
	mustMigrationTestExec(t, tx, `
		INSERT INTO memory_versions
		  (memory_id, version, account_id, realm_id, owner_kind, owner_id,
		   change_seq, content, kind, content_hash, actor_id, operation,
		   idempotency_key, request_hash)
		VALUES
		  ($1, 1, 'acc_memory_trigger', 'realm_memory_trigger', 'agent',
		   'agent_memory_owner', $2, 'migration trigger memory', 'decision',
		   $3, 'agent_memory_owner', 'added', $4, $5)`,
		memoryID, changeSeq, strings.Repeat("a", 64), "capture-"+memoryID, strings.Repeat("b", 64))
}

func insertMigrationTestMessageEvidence(
	t *testing.T,
	tx pgx.Tx,
	evidenceID, memoryID, messageID string,
	changeSeq int64,
) {
	t.Helper()
	mustMigrationTestExec(t, tx, `
		INSERT INTO memory_evidence
		  (id, account_id, realm_id, owner_kind, owner_id, memory_id,
		   target_version, evidence_change_seq, evidence_type, resolution_state,
		   resolved_kind, source_message_id, actor_id)
		VALUES
		  ($1, 'acc_memory_trigger', 'realm_memory_trigger', 'agent',
		   'agent_memory_owner', $2, 1, $4, 'message', 'resolved', 'message', $3,
		   'agent_memory_owner')`, evidenceID, memoryID, messageID, changeSeq)
}

func insertMigrationTestMemoryWithUnavailableEvidence(
	t *testing.T,
	tx pgx.Tx,
	memoryID string,
	versionChangeSeq, evidenceChangeSeq int64,
) {
	t.Helper()
	insertMigrationTestMemoryVersion(t, tx, memoryID, versionChangeSeq)
	mustMigrationTestExec(t, tx, `
		INSERT INTO memory_evidence
		  (id, account_id, realm_id, owner_kind, owner_id, memory_id,
		   target_version, evidence_change_seq, evidence_type, resolution_state,
		   terminal_reason_code, actor_id)
		VALUES
		  ($1, 'acc_memory_trigger', 'realm_memory_trigger', 'agent',
		   'agent_memory_owner', $2, 1, $3, 'unavailable', 'unavailable',
		   'migration_fixture', 'agent_memory_owner')`,
		"mev_"+strings.TrimPrefix(memoryID, "mem_"), memoryID, evidenceChangeSeq)
}

func insertMigrationTestSupersessionRelation(
	t *testing.T,
	tx pgx.Tx,
	relationID, fromMemoryID, toMemoryID, setID string,
	setRevision int64,
) {
	t.Helper()
	mustMigrationTestExec(t, tx, `
		INSERT INTO memory_relations
		  (id, account_id, realm_id, owner_kind, owner_id,
		   from_memory_id, from_version, to_memory_id, to_version,
		   relation_type, supersession_set_id, supersession_set_revision)
		VALUES
		  ($1, 'acc_memory_trigger', 'realm_memory_trigger', 'agent',
		   'agent_memory_owner', $2, 1, $3, 1, 'supersedes', $4, $5)`,
		relationID, fromMemoryID, toMemoryID, setID, setRevision)
}

func mustMigrationTestExec(t *testing.T, tx pgx.Tx, query string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

// migrationTestReporter is the small testing surface used by the shared
// schema-isolated store fixture. Ordinary tests pass *testing.T. Cloud
// certification passes a reporter that suppresses connection metadata before
// any failure or cleanup diagnostic reaches the test log.
type migrationTestReporter interface {
	Helper()
	Cleanup(func())
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
}

func newMigrationTestStore(t migrationTestReporter, baseDSN string) (*Store, string) {
	t.Helper()
	schema := fmt.Sprintf("witself_migration_%d_%d", os.Getpid(), migrationTestSchemaSequence.Add(1))
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	dsn, err := migrationTestDSNWithSearchPath(baseDSN, schema)
	if err != nil {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
		t.Fatal(err)
	}
	st, err := Open(context.Background(), dsn)
	if err != nil {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
		t.Fatal(err)
	}
	if err := st.Ping(context.Background()); err != nil {
		st.Close()
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(cleanupCtx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop migration test schema %s: %v", schema, err)
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close migration test admin connection: %v", err)
		}
	})
	return st, dsn
}

func migrationTestDSNWithSearchPath(baseDSN, schema string) (string, error) {
	if strings.HasPrefix(baseDSN, "postgres://") || strings.HasPrefix(baseDSN, "postgresql://") {
		u, err := url.Parse(baseDSN)
		if err != nil {
			return "", fmt.Errorf("parse PostgreSQL test URL: %w", err)
		}
		query := u.Query()
		query.Set("search_path", schema)
		u.RawQuery = query.Encode()
		return u.String(), nil
	}
	// A separate startup parameter preserves provider-required `options`
	// (for example statement timeouts or routing flags). pgx applies the last
	// keyword occurrence, so this controlled value also overrides any caller
	// search_path without creating a second options parameter.
	return baseDSN + " search_path='" + schema + "'", nil
}

func migrationTestUpTo(t *testing.T, dsn string, version int64) {
	t.Helper()
	db := migrationTestSQLDB(t, dsn)
	defer func() { _ = db.Close() }()
	if err := goose.UpTo(db, "migrations", version); err != nil {
		t.Fatalf("migrate test database to schema %d: %v", version, err)
	}
}

func migrationTestDown(t *testing.T, dsn string, wantError bool) error {
	t.Helper()
	db := migrationTestSQLDB(t, dsn)
	defer func() { _ = db.Close() }()
	err := goose.Down(db, "migrations")
	if !wantError && err != nil {
		t.Fatalf("migrate test database down: %v", err)
	}
	return err
}

func migrationTestSQLDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func assertMigrationTestVersion(t *testing.T, dsn string, want int64) {
	t.Helper()
	db := migrationTestSQLDB(t, dsn)
	defer func() { _ = db.Close() }()
	got, err := goose.GetDBVersion(db)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("database schema version = %d, want %d", got, want)
	}
}

func assertMigrationTestConstraint(t *testing.T, st *Store, name string, want bool) {
	t.Helper()
	assertMigrationTestTableConstraint(t, st, "facts", name, want)
}

func assertMigrationTestTableConstraint(t *testing.T, st *Store, table, name string, want bool) {
	t.Helper()
	var got bool
	if err := st.pool.QueryRow(context.Background(), `
		SELECT EXISTS (
		  SELECT 1 FROM pg_constraint
		  WHERE conrelid=to_regclass($1) AND conname=$2
		)`, table, name).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("constraint %s.%s exists = %t, want %t", table, name, got, want)
	}
}

func assertMigrationTestColumn(t *testing.T, st *Store, table, column string, want bool) {
	t.Helper()
	var got bool
	if err := st.pool.QueryRow(context.Background(), `
		SELECT EXISTS (
		  SELECT 1 FROM pg_attribute
		  WHERE attrelid=to_regclass($1) AND attname=$2 AND NOT attisdropped
		)`, table, column).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("column %s.%s exists = %t, want %t", table, column, got, want)
	}
}

func assertMigrationTestTable(t *testing.T, st *Store, table string, want bool) {
	t.Helper()
	var got bool
	if err := st.pool.QueryRow(context.Background(), `
		SELECT to_regclass($1) IS NOT NULL`, table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("table %s exists = %t, want %t", table, got, want)
	}
}

func assertMigrationTestIndex(t *testing.T, st *Store, table, index string, want bool) {
	t.Helper()
	var got bool
	if err := st.pool.QueryRow(context.Background(), `
		SELECT EXISTS (
		  SELECT 1 FROM pg_index
		  WHERE indrelid=to_regclass($1) AND indexrelid=to_regclass($2)
		)`, table, index).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("index %s.%s exists = %t, want %t", table, index, got, want)
	}
}

func insertMigrationTestRecreatedAddress(t *testing.T, st *Store) {
	t.Helper()
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	ctx := context.Background()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	statements := []string{
		`INSERT INTO accounts (id, is_default, display_name)
		 VALUES ('acc_migration', true, 'migration test')`,
		`INSERT INTO realms (id, account_id, name)
		 VALUES ('realm_migration', 'acc_migration', 'default')`,
		`INSERT INTO agents (id, realm_id, name)
		 VALUES ('agent_migration', 'realm_migration', 'migration-agent')`,
		`INSERT INTO fact_subjects
		   (id, account_id, realm_id, owner_agent_id, canonical_key, display_name)
		 VALUES
		   ('sub_migration', 'acc_migration', 'realm_migration', 'agent_migration', 'person_spouse', 'Spouse')`,
		`INSERT INTO facts
		   (id, account_id, realm_id, owner_agent_id, subject_id, predicate)
		 VALUES
		   ('fact_migration_active', 'acc_migration', 'realm_migration', 'agent_migration', 'sub_migration', 'identity/name')`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO facts
		  (id, account_id, realm_id, owner_agent_id, subject_id, predicate,
		   deleted_at, deleted_by_agent_id, delete_receipt_id,
		   delete_idempotency_key_hash, deleted_prior_assertion_id,
		   deleted_assertion_count, deleted_candidate_revision,
		   recreated_at, replacement_fact_id)
		VALUES
		  ('fact_migration_deleted', 'acc_migration', 'realm_migration', 'agent_migration', 'sub_migration', 'identity/name',
		   clock_timestamp() - interval '1 second', 'agent_migration', 'fdel_migration',
		   $1, 'fas_migration_prior', 1, $2,
		   clock_timestamp(), 'fact_migration_active')`, hashA, hashB); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
