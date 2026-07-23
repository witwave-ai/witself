package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
	"github.com/witwave-ai/witself/internal/sealed"
)

var migrationTestSchemaSequence atomic.Uint64

func TestMigration59AgentEmailPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, dsn := newMigrationTestStore(t, baseDSN)
	tables := []string{
		"agent_email_addresses", "agent_email_mailboxes",
		"agent_email_messages", "agent_email_deliveries",
	}
	assertSchema := func(want bool) {
		t.Helper()
		for _, table := range tables {
			assertMigrationTestTable(t, st, table, want)
		}
		if want {
			assertMigrationTestColumn(t, st, "agent_email_messages", "raw_mime", true)
			assertMigrationTestColumn(t, st, "agent_email_messages", "duplicate_group_sha256", true)
			assertMigrationTestColumn(t, st, "agent_email_deliveries", "failure_count", true)
			assertMigrationTestColumn(t, st, "agent_email_deliveries", "result_message_id", false)
			assertMigrationTestTableConstraint(t, st, "agent_email_deliveries",
				"agent_email_deliveries_processing_shape", true)
			assertMigrationTestIndexShape(t, st, "agent_email_messages",
				"agent_email_messages_provider_dedupe",
				[]string{"account_id", "realm_id", "provider", "provider_message_id", "envelope_recipient"},
				[]string{"provider_message_id IS NOT NULL"})
			assertMigrationTestIndexUnique(t, st, "agent_email_messages",
				"agent_email_messages_provider_dedupe", true)
			assertMigrationTestIndexUnique(t, st, "agent_email_addresses",
				"agent_email_addresses_live_by_agent", true)
			assertMigrationTestIndexShape(t, st, "agent_email_messages",
				"agent_email_messages_duplicate_group",
				[]string{"account_id", "realm_id", "mailbox_id", "duplicate_group_sha256", "received_at", "id"}, nil)
			assertMigrationTestIndexShape(t, st, "agent_email_deliveries",
				"agent_email_deliveries_claimable",
				[]string{"account_id", "realm_id", "owner_agent_id", "processing_state", "lease_expires_at", "delivered_at", "message_id"},
				[]string{"folder", "inbox", "acked_at IS NULL"})
		}
	}

	migrationTestUpTo(t, dsn, 58)
	assertMigrationTestVersion(t, dsn, 58)
	assertSchema(false)

	migrationTestUpTo(t, dsn, 59)
	assertMigrationTestVersion(t, dsn, 59)
	assertSchema(true)
	insertMigrationTestMemoryPrincipals(t, st)

	ctx := context.Background()
	const (
		accountID  = "acc_memory_trigger"
		realmID    = "realm_aaaaaaaaaaaaaaaa"
		ownerID    = "agent_aaaaaaaaaaaaaaaa"
		addressID  = "eaddr_aaaaaaaaaaaaaaaa"
		mailboxID  = "emb_aaaaaaaaaaaaaaaa"
		messageAID = "emsg_aaaaaaaaaaaaaaaa"
		messageBID = "emsg_bbbbbbbbbbbbbbbb"
		realmLabel = "aaaaaaaaaaaaaaaa"
		recipient  = "owner.aaaaaaaaaaaaaaaa@agent-mail.witwave.ai"
	)
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO realms (id,account_id,name) VALUES ($1,$2,'agent email migration')`,
		realmID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agents (id,realm_id,name) VALUES ($1,$2,'owner')`,
		ownerID, realmID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_addresses
		  (id,account_id,realm_id,provisioned_agent_id,domain,agent_segment,
		   realm_label,local_part,provisioning_kind)
		VALUES
		  ('eaddr_zzzzzzzzzzzzzzzz',$1,$2,$3,'agent-mail.witwave.ai','owner',
		   'bbbbbbbbbbbbbbbb','owner.bbbbbbbbbbbbbbbb','derived')`,
		accountID, realmID, ownerID); err == nil {
		t.Fatal("address with a realm label inconsistent with realm_id was accepted")
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_addresses
		  (id,account_id,realm_id,provisioned_agent_id,domain,agent_segment,
		   realm_label,local_part,provisioning_kind)
		VALUES
			  ($1,$2,$3,$4,
			   'agent-mail.witwave.ai','owner',$5,'owner.' || $5,'derived')`,
		addressID, accountID, realmID, ownerID, realmLabel); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_email_mailboxes
			  (id,account_id,realm_id,owner_agent_id,address_id,receive_state)
			VALUES
			  ($1,$2,$3,$4,$5,'enabled')`, mailboxID, accountID, realmID, ownerID, addressID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_messages
		  (id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,
		   provider,envelope_sender,envelope_recipient,agent_segment,realm_label,
		   raw_mime,raw_size_bytes,raw_sha256,parse_state,
		   spf_result,dkim_result,dmarc_result,spam_verdict,
		   sender_verification_state,duplicate_group_sha256,received_at)
		VALUES
		  ('emsg_zzzzzzzzzzzzzzzz',$1,$2,$3,$4,$5,
		   'cloudflare_email_routing','sender@example.com',$6,'owner',$7,
		   convert_to(E'Subject: forged posture\r\n\r\nbody','UTF8'),31,$8,'parsed',
		   'pass','unknown','unknown','unknown','unverified',$9,clock_timestamp())`,
		accountID, realmID, mailboxID, ownerID, addressID, recipient, realmLabel,
		strings.Repeat("e", 64), strings.Repeat("f", 64)); err == nil {
		t.Fatal("Cloudflare pilot trust posture elevation was accepted")
	}
	insertMessage := func(id, providerMessageID string, raw []byte, possibleDuplicate *string) error {
		_, err := st.pool.Exec(ctx, `
				INSERT INTO agent_email_messages
				  (id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,
				   provider,provider_message_id,envelope_sender,envelope_recipient,agent_segment,
				   realm_label,raw_mime,raw_size_bytes,raw_sha256,parse_state,
				   header_subject,sender_verification_state,duplicate_group_sha256,
				   possible_duplicate_of_message_id,received_at)
				VALUES
				  ($1,$2,$3,$4,$5,$6,'cloudflare',$7,'sender@example.com',$8,
				   'owner',$9,$10,$11,$12,'parsed','migration mail','unverified',$13,
				   $14,clock_timestamp())`, id, accountID, realmID, mailboxID, ownerID,
			addressID, providerMessageID, recipient, realmLabel, raw, len(raw),
			strings.Repeat("a", 64), strings.Repeat("b", 64), possibleDuplicate)
		return err
	}
	rawA := []byte("Subject: migration A\r\n\r\nbody A")
	if err := insertMessage(messageAID, "provider-a", rawA, nil); err != nil {
		t.Fatal(err)
	}
	possibleDuplicate := messageAID
	if err := insertMessage(messageBID, "provider-b", []byte("Subject: migration B\r\n\r\nbody B"),
		&possibleDuplicate); err != nil {
		t.Fatal(err)
	}
	if err := insertMessage("emsg_cccccccccccccccc", "provider-a",
		[]byte("Subject: provider duplicate\r\n\r\nbody"), nil); err == nil {
		t.Fatal("duplicate provider identity was accepted")
	}
	var duplicateRows int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_messages
		WHERE duplicate_group_sha256=$1`, strings.Repeat("b", 64)).Scan(&duplicateRows); err != nil {
		t.Fatal(err)
	}
	if duplicateRows != 2 {
		t.Fatalf("non-destructive duplicate group rows = %d, want 2", duplicateRows)
	}
	if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_email_deliveries
			  (message_id,account_id,realm_id,mailbox_id,owner_agent_id)
			VALUES
			  ($1,$2,$3,$4,$5)`,
		messageAID, accountID, realmID, mailboxID, ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_deliveries SET processing_state='needs_attention'
		WHERE message_id=$1 AND mailbox_id=$2`, messageAID, mailboxID); err == nil {
		t.Fatal("unresolved needs_attention delivery state was accepted")
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_messages
		  (id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,
		   provider,envelope_sender,envelope_recipient,agent_segment,realm_label,
		   raw_mime,raw_size_bytes,raw_sha256,duplicate_group_sha256,received_at)
		VALUES
			  ('emsg_dddddddddddddddd',$1,$2,$3,
			   $4,$5,'cloudflare','sender@example.com',$6,'owner',$7,
			   decode(repeat('00',5242881),'hex'),5242881,$8,$9,clock_timestamp())`,
		accountID, realmID, mailboxID, ownerID, addressID, recipient, realmLabel,
		strings.Repeat("c", 64), strings.Repeat("d", 64)); err == nil {
		t.Fatal("raw MIME above the 5 MiB pilot cap was accepted")
	}

	// The mailbox and content are agent-owned and cascade on permanent agent
	// deletion. The address reservation has intentionally no agent FK, so it
	// remains to prevent the old recipient from being rebound.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := retireAgentEmailMailboxTx(ctx, tx, accountID, realmID, ownerID, "agent_deleted"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agents WHERE id=$1 AND realm_id=$2`, ownerID, realmID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{
		"agent_email_addresses":  1,
		"agent_email_mailboxes":  0,
		"agent_email_messages":   0,
		"agent_email_deliveries": 0,
	} {
		var got int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s rows after agent deletion = %d, want %d", table, got, want)
		}
	}
	var retiredAt *time.Time
	var retirementReason *string
	if err := st.pool.QueryRow(ctx, `
		SELECT retired_at,retirement_reason_code
		FROM agent_email_addresses WHERE id=$1`, addressID).
		Scan(&retiredAt, &retirementReason); err != nil {
		t.Fatal(err)
	}
	if retiredAt == nil || retirementReason == nil || *retirementReason != "agent_deleted" {
		t.Fatalf("hard-delete tombstone = retired_at %v reason %v", retiredAt, retirementReason)
	}

	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 58)
	assertSchema(false)

	migrationTestUpTo(t, dsn, 59)
	assertMigrationTestVersion(t, dsn, 59)
	assertSchema(true)
}

func TestMigration57DashboardPreferencesPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, dsn := newMigrationTestStore(t, baseDSN)

	migrationTestUpTo(t, dsn, 56)
	assertMigrationTestVersion(t, dsn, 56)
	assertMigrationTestTable(t, st, "agent_dashboard_preferences", false)

	migrationTestUpTo(t, dsn, 57)
	assertMigrationTestVersion(t, dsn, 57)
	assertMigrationTestTable(t, st, "agent_dashboard_preferences", true)
	assertMigrationTestColumn(t, st, "agent_dashboard_preferences", "prefs", true)
	assertMigrationTestColumn(t, st, "agent_dashboard_preferences", "updated_at", true)

	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 56)
	assertMigrationTestTable(t, st, "agent_dashboard_preferences", false)
	assertMigrationTestTable(t, st, "secrets", true)

	migrationTestUpTo(t, dsn, 57)
	assertMigrationTestVersion(t, dsn, 57)
	assertMigrationTestTable(t, st, "agent_dashboard_preferences", true)
}

func TestMigration55AgentSecretsPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, dsn := newMigrationTestStore(t, baseDSN)
	assertSchema := func(want bool) {
		t.Helper()
		for _, table := range []string{
			"agent_vault_keys", "secrets", "secret_fields", "secret_deks",
			"secret_mutation_receipts",
		} {
			assertMigrationTestTable(t, st, table, want)
		}
		assertMigrationTestIndex(t, st, "realms", "realms_account_id_id_unique", want)
		assertMigrationTestIndex(t, st, "agents", "agents_realm_id_id_unique", want)
		if want {
			assertMigrationTestForeignKeyDeferral(t, st, "secret_fields", "secret_deks",
				"secret_fields_current_dek_fk", true, true)
			assertMigrationTestForeignKeyDeferral(t, st, "secret_deks", "secret_fields",
				"", true, true)
			assertMigrationTestIndexShape(t, st, "secrets", "secrets_one_live_name_per_agent",
				[]string{"account_id", "realm_id", "owner_agent_id", "name"},
				[]string{"archived_at IS NULL", "deleted_at IS NULL"})
			assertMigrationTestIndex(t, st, "secrets", "secrets_search_document", true)
			assertMigrationTestIndex(t, st, "secret_fields", "secret_fields_public_search_document", true)
			assertMigrationTestIndexMethod(t, st, "secrets", "secrets_search_document", "gin")
			assertMigrationTestIndexMethod(t, st, "secret_fields", "secret_fields_public_search_document", "gin")
		}
	}

	migrationTestUpTo(t, dsn, 54)
	assertMigrationTestVersion(t, dsn, 54)
	assertSchema(false)

	migrationTestUpTo(t, dsn, 55)
	assertMigrationTestVersion(t, dsn, 55)
	assertSchema(true)

	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 54)
	assertSchema(false)

	migrationTestUpTo(t, dsn, 55)
	assertMigrationTestVersion(t, dsn, 55)
	assertSchema(true)
}

func TestMigration56ReplacesHistoricalVaultKeyVersionConstraintPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 55)
	assertMigrationTestVersion(t, dsn, 55)

	provisioned, err := st.ProvisionAccount(ctx, "migration56@witwave.ai", "migration 56", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate account = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "migration-agent")
	if err != nil {
		t.Fatal(err)
	}
	insertKey := func(id string, version int64, fingerprint, state string) error {
		retiredAt := any(nil)
		if state == "retired" {
			retiredAt = time.Now().UTC()
		}
		_, err := st.pool.Exec(ctx, `
			INSERT INTO agent_vault_keys
			       (id,account_id,realm_id,owner_agent_id,key_version,
			        algorithm,fingerprint,lifecycle_state,retired_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, provisioned.AccountID,
			realm.ID, agent.ID, version, SecretAEADAlgorithm, fingerprint, state, retiredAt)
		return err
	}
	insertCancelledRotation := func(id, targetKeyID string) error {
		_, err := st.pool.Exec(ctx, `
			INSERT INTO agent_vault_key_rotations
			       (id,account_id,realm_id,owner_agent_id,source_key_id,
			        source_key_version,target_key_id,target_key_version,
			        lifecycle_state,item_count,cancelled_at)
			VALUES ($1,$2,$3,$4,'avk_aaaaaaaaaaaaaaaa',1,$5,2,
			        'cancelled',0,clock_timestamp())`, id, provisioned.AccountID,
			realm.ID, agent.ID, targetKeyID)
		return err
	}
	if err := insertKey("avk_aaaaaaaaaaaaaaaa", 1, strings.Repeat("a", 64), "current"); err != nil {
		t.Fatal(err)
	}
	if err := insertKey("avk_bbbbbbbbbbbbbbbb", 2, strings.Repeat("b", 64), "retired"); err != nil {
		t.Fatal(err)
	}
	if err := insertKey("avk_cccccccccccccccc", 2, strings.Repeat("c", 64), "pending"); !secretUniqueViolation(err) {
		t.Fatalf("schema 55 duplicate historical version error = %v", err)
	}
	if err := insertKey("avk_hhhhhhhhhhhhhhhh", 3, strings.Repeat("2", 64), "pending"); err != nil {
		t.Fatalf("schema 55 legacy pending key = %v", err)
	}
	var legacyPendingCreatedAt time.Time
	if err := st.pool.QueryRow(ctx, `
		UPDATE agent_vault_keys
		   SET row_version=7
		 WHERE id='avk_hhhhhhhhhhhhhhhh'
		 RETURNING created_at`).Scan(&legacyPendingCreatedAt); err != nil {
		t.Fatal(err)
	}

	migrationTestUpTo(t, dsn, 56)
	assertMigrationTestVersion(t, dsn, 56)
	assertMigrationTestIndexShape(t, st, "agent_vault_keys", "agent_vault_keys_one_live_version",
		[]string{"account_id", "realm_id", "owner_agent_id", "key_version"},
		[]string{"lifecycle_state", "pending", "current"})
	var legacyPendingState string
	var legacyPendingRowVersion int64
	var legacyPendingRetiredAt, migratedCreatedAt time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT lifecycle_state, row_version, created_at, retired_at
		  FROM agent_vault_keys
		 WHERE id='avk_hhhhhhhhhhhhhhhh'`).Scan(
		&legacyPendingState, &legacyPendingRowVersion,
		&migratedCreatedAt, &legacyPendingRetiredAt); err != nil {
		t.Fatal(err)
	}
	if legacyPendingState != "retired" || legacyPendingRowVersion != 7 ||
		!migratedCreatedAt.Equal(legacyPendingCreatedAt) ||
		!legacyPendingRetiredAt.Equal(legacyPendingCreatedAt) {
		t.Fatalf("migrated legacy pending key = state %q revision %d created %v retired %v, want retired/7/%v/%v",
			legacyPendingState, legacyPendingRowVersion, migratedCreatedAt,
			legacyPendingRetiredAt, legacyPendingCreatedAt, legacyPendingCreatedAt)
	}

	if err := insertKey("avk_dddddddddddddddd", 2, strings.Repeat("d", 64), "pending"); err != nil {
		t.Fatalf("first source+1 retry after schema 56 = %v", err)
	}
	err = insertKey("avk_eeeeeeeeeeeeeeee", 2, strings.Repeat("e", 64), "pending")
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "23505" ||
		postgresError.ConstraintName != "agent_vault_keys_one_live_version" {
		t.Fatalf("duplicate live version error = %#v / %v", postgresError, err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_vault_keys
		   SET lifecycle_state='retired', retired_at=clock_timestamp(), row_version=row_version+1
		 WHERE id='avk_dddddddddddddddd'`); err != nil {
		t.Fatal(err)
	}
	if err := insertCancelledRotation("vkr_dddddddddddddddd", "avk_dddddddddddddddd"); err != nil {
		t.Fatal(err)
	}
	if err := insertKey("avk_ffffffffffffffff", 2, strings.Repeat("f", 64), "pending"); err != nil {
		t.Fatalf("second source+1 retry after retiring candidate = %v", err)
	}
	var total, live int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE lifecycle_state IN ('pending','current'))
		  FROM agent_vault_keys
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND key_version=2`,
		provisioned.AccountID, realm.ID, agent.ID).Scan(&total, &live); err != nil {
		t.Fatal(err)
	}
	if total != 3 || live != 1 {
		t.Fatalf("version 2 historical/live rows = %d/%d, want 3/1", total, live)
	}

	err = migrationTestDown(t, dsn, true)
	if err == nil || !strings.Contains(err.Error(), "orphan pending vault key epoch") {
		t.Fatalf("schema 56 downgrade with orphan pending epoch error = %v", err)
	}
	assertMigrationTestVersion(t, dsn, 56)
	assertMigrationTestTable(t, st, "agent_vault_key_rotations", true)
	assertMigrationTestIndex(t, st, "agent_vault_keys", "agent_vault_keys_one_live_version", true)

	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_vault_keys
		   SET lifecycle_state='retired', retired_at=clock_timestamp(), row_version=row_version+1
		 WHERE id='avk_ffffffffffffffff'`); err != nil {
		t.Fatal(err)
	}
	if err := insertCancelledRotation("vkr_ffffffffffffffff", "avk_ffffffffffffffff"); err != nil {
		t.Fatal(err)
	}
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 55)
	assertMigrationTestTable(t, st, "agent_vault_key_rotations", false)
	assertMigrationTestTable(t, st, "agent_vault_key_enrollments", false)
	assertMigrationTestIndex(t, st, "agent_vault_keys", "agent_vault_keys_one_live_version", false)
	assertMigrationTestTableConstraint(t, st, "agent_vault_keys",
		"agent_vault_keys_scope_version_unique", true)

	var survivor string
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*), min(id)
		  FROM agent_vault_keys
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND key_version=2`,
		provisioned.AccountID, realm.ID, agent.ID).Scan(&total, &survivor); err != nil {
		t.Fatal(err)
	}
	if total != 1 || survivor != "avk_ffffffffffffffff" {
		t.Fatalf("compacted version 2 survivor = %d/%q, want 1/%q",
			total, survivor, "avk_ffffffffffffffff")
	}
	err = insertKey("avk_gggggggggggggggg", 2, strings.Repeat("1", 64), "retired")
	postgresError = nil
	if !errors.As(err, &postgresError) || postgresError.Code != "23505" ||
		postgresError.ConstraintName != "agent_vault_keys_scope_version_unique" {
		t.Fatalf("schema 55 restored version constraint error = %#v / %v", postgresError, err)
	}
}

func TestMigration56DownPreservesCurrentAndReferencedVaultKeyEpochPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}

	t.Run("committed current epoch wins", func(t *testing.T) {
		ctx := context.Background()
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 56)
		p := migration56TestPrincipal(ctx, t, st)
		_, source := migration56RegisterKey(ctx, t, st, p, 1,
			"migration56-committed-source")
		targetKey, err := sealed.GenerateAgentVaultKey(2)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(targetKey.Clear)
		target := targetKey.Metadata()
		rotationID := mustSecretTestID(t, "vkr")
		rotation, _, err := st.StartVaultKeyRotation(ctx, p, StartVaultKeyRotationInput{
			ID: rotationID, ExpectedSourceKeyID: source.ID,
			ExpectedSourceKeyVersion:    source.KeyVersion,
			ExpectedSourceKeyRowVersion: source.RowVersion,
			TargetKeyID:                 target.ID, TargetKeyVersion: int64(target.Version),
			TargetAlgorithm: target.Algorithm, TargetFingerprint: target.Fingerprint,
			IdempotencyKey: "migration56-committed-start",
		})
		if err != nil || rotation.ItemCount != 0 || rotation.StagedPlanHash == "" {
			t.Fatalf("start empty committed rotation = %#v / %v", rotation, err)
		}
		rotation, _, err = st.CommitVaultKeyRotation(ctx, p, rotation.ID,
			CommitVaultKeyRotationInput{
				ExpectedRotationRowVersion: rotation.RowVersion,
				ExpectedItemCount:          rotation.ItemCount,
				ExpectedPlanHash:           rotation.StagedPlanHash,
				RecoveryDisposition: VaultKeyRotationRecoveryDisposition{
					Mode: VaultKeyRotationRiskAccepted,
				},
				IdempotencyKey: "migration56-committed-commit",
			})
		if err != nil || rotation.LifecycleState != VaultKeyRotationCommitted {
			t.Fatalf("commit empty rotation = %#v / %v", rotation, err)
		}
		migration56CreateSecret(ctx, t, st, p, targetKey, "committed-current")
		migration56InsertNewerRetiredDuplicate(ctx, t, st, p, 2)
		migration56AssertDownPreservesKey(ctx, t, st, dsn, p, 2, target.ID)
	})

	t.Run("referenced retired epoch wins over newer unreferenced", func(t *testing.T) {
		ctx := context.Background()
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 56)
		p := migration56TestPrincipal(ctx, t, st)
		originalKey, original := migration56RegisterKey(ctx, t, st, p, 1,
			"migration56-preserve-original")
		migration56CreateSecret(ctx, t, st, p, originalKey, "preserved-retired")
		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_vault_keys
			   SET lifecycle_state='retired', retired_at=clock_timestamp(),
			       row_version=row_version+1
			 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4`,
			p.AccountID, p.RealmID, p.ID, original.ID); err != nil {
			t.Fatal(err)
		}
		migration56InsertNewerRetiredDuplicate(ctx, t, st, p, 1)
		migration56AssertDownPreservesKey(ctx, t, st, dsn, p, 1, original.ID)
	})
}

func TestMigration56DownRefusesReferencedLosingDuplicatePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 56)
	p := migration56TestPrincipal(ctx, t, st)
	firstKey, first := migration56RegisterKey(ctx, t, st, p, 1,
		"migration56-losing-first")
	migration56CreateSecret(ctx, t, st, p, firstKey, "first")
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_vault_keys
		   SET lifecycle_state='retired', retired_at=clock_timestamp(),
		       row_version=row_version+1
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4`,
		p.AccountID, p.RealmID, p.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	secondKey, second := migration56RegisterKey(ctx, t, st, p, 1,
		"migration56-losing-second")
	migration56CreateSecret(ctx, t, st, p, secondKey, "second")

	err := migrationTestDown(t, dsn, true)
	if err == nil || !strings.Contains(err.Error(),
		"duplicate retired vault key epoch is still referenced") {
		t.Fatalf("schema 56 downgrade with referenced losing duplicate error = %v", err)
	}
	assertMigrationTestVersion(t, dsn, 56)
	assertMigrationTestTable(t, st, "agent_vault_key_rotations", true)
	assertMigrationTestIndex(t, st, "agent_vault_keys", "agent_vault_keys_one_live_version", true)
	var keyCount int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_vault_keys
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND id IN ($4,$5)`, p.AccountID, p.RealmID, p.ID,
		first.ID, second.ID).Scan(&keyCount); err != nil {
		t.Fatal(err)
	}
	if keyCount != 2 {
		t.Fatalf("keys after refused downgrade = %d, want 2", keyCount)
	}
}

func TestMigration56DownRefusesActiveVaultLifecyclePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	t.Run("pending enrollment", func(t *testing.T) {
		ctx := context.Background()
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 56)
		p := migration56TestPrincipal(ctx, t, st)
		_, current := migration56RegisterKey(ctx, t, st, p, 1,
			"migration56-active-enrollment")
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_vault_key_enrollments
			       (id,account_id,realm_id,owner_agent_id,vault_key_id,
			        vault_key_version,target_location_id,target_location_name,
			        target_public_key,target_key_algorithm,pairing_commitment,expires_at)
			VALUES ('enr_aaaaaaaaaaaaaaaa',$1,$2,$3,$4,$5,
			        'loc_aaaaaaaaaaaaaaaa','test location',$6,$7,$8,
			        clock_timestamp()+interval '10 minutes')`, p.AccountID, p.RealmID,
			p.ID, current.ID, current.KeyVersion, strings.Repeat("A", 43),
			"X25519_RAW_32_BASE64URL_V1", strings.Repeat("a", 64)); err != nil {
			t.Fatal(err)
		}

		migration56AssertActiveLifecycleDownRefused(ctx, t, st, dsn)
	})

	t.Run("open rotation", func(t *testing.T) {
		ctx := context.Background()
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 56)
		p := migration56TestPrincipal(ctx, t, st)
		_, current := migration56RegisterKey(ctx, t, st, p, 1,
			"migration56-active-rotation")
		targetKey, err := sealed.GenerateAgentVaultKey(2)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(targetKey.Clear)
		target := targetKey.Metadata()
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_vault_keys
			       (id,account_id,realm_id,owner_agent_id,key_version,
			        algorithm,fingerprint,lifecycle_state)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'pending')`, target.ID, p.AccountID,
			p.RealmID, p.ID, target.Version, target.Algorithm,
			target.Fingerprint); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_vault_key_rotations
			       (id,account_id,realm_id,owner_agent_id,source_key_id,
			        source_key_version,target_key_id,target_key_version,item_count)
			VALUES ('vkr_aaaaaaaaaaaaaaaa',$1,$2,$3,$4,$5,$6,$7,0)`,
			p.AccountID, p.RealmID, p.ID, current.ID, current.KeyVersion,
			target.ID, target.Version); err != nil {
			t.Fatal(err)
		}

		migration56AssertActiveLifecycleDownRefused(ctx, t, st, dsn)
	})
}

func migration56TestPrincipal(ctx context.Context, t *testing.T, st *Store) Principal {
	t.Helper()
	provisioned, err := st.ProvisionAccount(ctx,
		"migration56-down@witwave.ai", "migration 56 down", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate account = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "migration-agent")
	if err != nil {
		t.Fatal(err)
	}
	return Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
		AccessProfile: AccessProfileFull}
}

func migration56RegisterKey(
	ctx context.Context,
	t *testing.T,
	st *Store,
	p Principal,
	version uint64,
	idempotencyKey string,
) (*sealed.AgentVaultKey, VaultKeyBinding) {
	t.Helper()
	key, err := sealed.GenerateAgentVaultKey(version)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(key.Clear)
	metadata := key.Metadata()
	binding, _, err := st.RegisterVaultKey(ctx, p, RegisterVaultKeyInput{
		ID: metadata.ID, KeyVersion: int64(metadata.Version), Algorithm: metadata.Algorithm,
		Fingerprint: metadata.Fingerprint, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return key, binding
}

func migration56CreateSecret(
	ctx context.Context,
	t *testing.T,
	st *Store,
	p Principal,
	key *sealed.AgentVaultKey,
	suffix string,
) {
	t.Helper()
	secretID := mustSecretTestID(t, "sec")
	fieldID := mustSecretTestID(t, "fld")
	scope := sealed.FieldScope{Domain: sealed.FieldValueDomain, AccountID: p.AccountID,
		RealmID: p.RealmID, OwnerAgentID: p.ID, SecretID: secretID, FieldID: fieldID}
	envelope, err := sealed.SealSensitiveField(key, []byte("migration 56 canary"),
		sealed.SensitiveFieldOptions{Scope: scope, ValueVersion: 1, DEKGeneration: 1,
			ValueEncoding: sealed.ValueEncodingUTF8, WrapRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSecret(ctx, p, CreateSecretInput{
		ID: secretID, Name: "migration 56 " + suffix, Template: "login",
		IdempotencyKey: "migration56-create-" + suffix,
		Fields: []CreateSecretFieldInput{{
			ID: fieldID, Name: "password", Kind: SecretFieldPassword, Sensitive: true,
			Encoding: SecretEncodingUTF8, ValueVersion: 1, Sealed: secretStoreEnvelope(envelope),
		}},
	}); err != nil {
		t.Fatal(err)
	}
}

func migration56InsertNewerRetiredDuplicate(
	ctx context.Context,
	t *testing.T,
	st *Store,
	p Principal,
	version uint64,
) {
	t.Helper()
	key, err := sealed.GenerateAgentVaultKey(version)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(key.Clear)
	metadata := key.Metadata()
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_vault_keys
		       (id,account_id,realm_id,owner_agent_id,key_version,
		        algorithm,fingerprint,lifecycle_state,created_at,retired_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'retired',
		        clock_timestamp()+interval '1 hour',
		        clock_timestamp()+interval '1 hour')`, metadata.ID, p.AccountID,
		p.RealmID, p.ID, metadata.Version, metadata.Algorithm,
		metadata.Fingerprint); err != nil {
		t.Fatal(err)
	}
}

func migration56AssertDownPreservesKey(
	ctx context.Context,
	t *testing.T,
	st *Store,
	dsn string,
	p Principal,
	keyVersion int64,
	wantKeyID string,
) {
	t.Helper()
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 55)
	assertMigrationTestTable(t, st, "agent_vault_key_rotations", false)
	assertMigrationTestTable(t, st, "agent_vault_key_enrollments", false)
	assertMigrationTestTableConstraint(t, st, "agent_vault_keys",
		"agent_vault_keys_scope_version_unique", true)

	var count int64
	var survivor string
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*), min(id)
		  FROM agent_vault_keys
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND key_version=$4`,
		p.AccountID, p.RealmID, p.ID, keyVersion).Scan(&count, &survivor); err != nil {
		t.Fatal(err)
	}
	if count != 1 || survivor != wantKeyID {
		t.Fatalf("schema 55 key survivor = %d/%q, want 1/%q", count, survivor, wantKeyID)
	}
	var wrappingKeyID string
	if err := st.pool.QueryRow(ctx, `
		SELECT wrapping_key_id
		  FROM secret_deks
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3`,
		p.AccountID, p.RealmID, p.ID).Scan(&wrappingKeyID); err != nil {
		t.Fatal(err)
	}
	if wrappingKeyID != wantKeyID {
		t.Fatalf("preserved DEK wrapping key = %q, want %q", wrappingKeyID, wantKeyID)
	}
}

func migration56AssertActiveLifecycleDownRefused(
	ctx context.Context,
	t *testing.T,
	st *Store,
	dsn string,
) {
	t.Helper()
	err := migrationTestDown(t, dsn, true)
	if err == nil || !strings.Contains(err.Error(),
		"active vault key enrollment or rotation") {
		t.Fatalf("schema 56 downgrade with active lifecycle error = %v", err)
	}
	assertMigrationTestVersion(t, dsn, 56)
	assertMigrationTestTable(t, st, "agent_vault_key_enrollments", true)
	assertMigrationTestTable(t, st, "agent_vault_key_rotations", true)
	assertMigrationTestIndex(t, st, "agent_vault_keys", "agent_vault_keys_one_live_version", true)
	var lifecycleRows int64
	if err := st.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM agent_vault_key_enrollments
		         WHERE lifecycle_state IN ('pending','approved')) +
		       (SELECT count(*) FROM agent_vault_key_rotations
		         WHERE lifecycle_state='open')`).Scan(&lifecycleRows); err != nil {
		t.Fatal(err)
	}
	if lifecycleRows != 1 {
		t.Fatalf("active lifecycle rows after refused downgrade = %d, want 1", lifecycleRows)
	}
}

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

func TestMigration41Postgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}

	t.Run("fresh database applies every migration", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
		assertMigrationTestIndexShape(t, st, "agent_message_requests", "agent_message_requests_open_by_coordinator",
			[]string{"account_id", "realm_id", "coordinator_agent_id", "expires_at", "offer_deadline", "id"},
			[]string{"state", "open"})
		assertMigrationTestIndexShape(t, st, "agent_message_request_candidates", "agent_message_request_candidates_pending_by_request",
			[]string{"request_id"}, []string{"response_state", "pending"})
		assertMigrationTestIndex(t, st, "memory_curation_requests", "memory_curation_requests_due_by_owner", true)
		assertMigrationTestTable(t, st, "agent_activity", true)
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

	t.Run("avatar schema rolls back and reapplies cleanly", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 41)
		assertMigrationTestVersion(t, dsn, 41)
		assertMigrationTestTable(t, st, "agent_avatar_profiles", false)

		migrationTestUpTo(t, dsn, 50)
		assertMigrationTestVersion(t, dsn, 50)
		for _, table := range []string{
			"avatar_style_packs", "avatar_style_pack_versions", "realm_avatar_styles",
			"agent_avatar_profiles", "agent_avatar_versions", "agent_avatar_activations",
			"agent_avatar_rejections", "agent_avatar_resets", "avatar_mutation_receipts",
		} {
			assertMigrationTestTable(t, st, table, true)
		}
		assertMigrationTestColumn(t, st, "agent_avatar_profiles", "lineage_generation", true)
		assertMigrationTestColumn(t, st, "agent_avatar_versions", "lineage_generation", true)
		assertMigrationTestColumn(t, st, "agent_avatar_activations", "lineage_generation", true)
		assertMigrationTestColumn(t, st, "avatar_mutation_receipts", "result_lineage_generation", true)

		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 41)
		assertMigrationTestTable(t, st, "agent_avatar_profiles", false)
		assertMigrationTestTable(t, st, "agent_avatar_resets", false)

		migrationTestUpTo(t, dsn, 50)
		assertMigrationTestVersion(t, dsn, 50)
		assertMigrationTestTable(t, st, "agent_avatar_profiles", true)
		assertMigrationTestTable(t, st, "agent_avatar_resets", true)
	})

	t.Run("curation queue, activity, message request, and audience migrations roll back in order", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 41)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 40)
		assertMigrationTestIndex(t, st, "agent_message_requests", "agent_message_requests_open_by_coordinator", false)
		assertMigrationTestIndex(t, st, "agent_message_request_candidates", "agent_message_request_candidates_pending_by_request", false)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 39)
		assertMigrationTestIndex(t, st, "memory_curation_requests", "memory_curation_requests_due_by_owner", false)
		assertMigrationTestTable(t, st, "agent_activity", true)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 38)
		assertMigrationTestTable(t, st, "agent_activity", false)
		assertMigrationTestTable(t, st, "agent_message_requests", true)
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
		migrationTestUpTo(t, dsn, 41)
		assertMigrationTestVersion(t, dsn, 41)
		assertMigrationTestIndex(t, st, "agent_message_requests", "agent_message_requests_open_by_coordinator", true)
		assertMigrationTestIndex(t, st, "agent_message_request_candidates", "agent_message_request_candidates_pending_by_request", true)
		assertMigrationTestIndex(t, st, "memory_curation_requests", "memory_curation_requests_due_by_owner", true)
		assertMigrationTestTable(t, st, "agent_activity", true)
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
		// This fixture intentionally runs current code against schema 30. Insert
		// the pre-avatar realm and agent shape directly: CreateRealm/CreateAgent
		// now atomically create avatar rows that do not exist until schema 50.
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO realms (id,account_id,name)
			VALUES ('realm_migration_curator',$1,'default')`,
			provisioned.AccountID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agents (id,realm_id,name)
			VALUES ('agent_migration_curator','realm_migration_curator','legacy')`); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := st.CreateAgentToken(ctx, provisioned.AccountID, provisioned.OperatorID, "agent_migration_curator"); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := st.CreateOperatorToken(ctx, provisioned.AccountID, provisioned.OperatorID, "legacy operator", nil); err != nil {
			t.Fatal(err)
		}

		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
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
		assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
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
		assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
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
		assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
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
		assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
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
		assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
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
		assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
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

	t.Run("clean down removes curation queue, messaging, reply, and narrative schemas and can re-upgrade", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 41)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 40)
		assertMigrationTestIndex(t, st, "agent_message_requests", "agent_message_requests_open_by_coordinator", false)
		assertMigrationTestIndex(t, st, "agent_message_request_candidates", "agent_message_request_candidates_pending_by_request", false)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 39)
		assertMigrationTestIndex(t, st, "memory_curation_requests", "memory_curation_requests_due_by_owner", false)
		assertMigrationTestTable(t, st, "agent_activity", true)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 38)
		assertMigrationTestTable(t, st, "agent_activity", false)
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
		migrationTestUpTo(t, dsn, 41)
		assertMigrationTestVersion(t, dsn, 41)
		assertMigrationTestIndex(t, st, "agent_message_requests", "agent_message_requests_open_by_coordinator", true)
		assertMigrationTestIndex(t, st, "agent_message_request_candidates", "agent_message_request_candidates_pending_by_request", true)
		assertMigrationTestIndex(t, st, "memory_curation_requests", "memory_curation_requests_due_by_owner", true)
		assertMigrationTestTable(t, st, "agent_activity", true)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", false)
		assertMigrationTestColumn(t, st, "agent_messages", "reply_to_message_id", true)
	})

	t.Run("down refuses duplicate recreated address without data loss", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 41)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 40)
		assertMigrationTestIndex(t, st, "agent_message_requests", "agent_message_requests_open_by_coordinator", false)
		assertMigrationTestIndex(t, st, "agent_message_request_candidates", "agent_message_request_candidates_pending_by_request", false)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 39)
		assertMigrationTestIndex(t, st, "memory_curation_requests", "memory_curation_requests_due_by_owner", false)
		assertMigrationTestTable(t, st, "agent_activity", true)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 38)
		assertMigrationTestTable(t, st, "agent_activity", false)
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

func TestMigrateSerializesReplicasWithAdvisoryLockPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}

	first, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 61)

	second, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	if err := second.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}

	gateDB := migrationTestSQLDB(t, dsn)
	t.Cleanup(func() { _ = gateDB.Close() })
	gateConn, err := gateDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gateConn.Close() })
	if err := acquireMigrationAdvisoryLock(context.Background(), gateConn); err != nil {
		t.Fatalf("acquire test migration gate: %v", err)
	}
	gateHeld := true
	t.Cleanup(func() {
		if gateHeld {
			_ = releaseMigrationAdvisoryLock(context.Background(), gateConn)
		}
	})

	started := make(chan struct{}, 2)
	results := make(chan error, 2)
	run := func(st *Store) {
		started <- struct{}{}
		results <- st.Migrate()
	}
	go run(first)
	go run(second)
	<-started
	<-started

	var completed []error
	select {
	case err := <-results:
		completed = append(completed, err)
		t.Errorf("Migrate returned while another session held the migration advisory lock: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	if err := releaseMigrationAdvisoryLock(context.Background(), gateConn); err != nil {
		t.Fatalf("release test migration gate: %v", err)
	}
	gateHeld = false

	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	for len(completed) < 2 {
		select {
		case err := <-results:
			completed = append(completed, err)
		case <-timeout.C:
			t.Fatalf("timed out waiting for concurrent Migrate calls; completed %d of 2", len(completed))
		}
	}
	for i, err := range completed {
		if err != nil {
			t.Errorf("concurrent Migrate call %d: %v", i+1, err)
		}
	}
	assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
}

func TestTranscriptRetentionCheckUsesStagedValidationPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, dsn := newMigrationTestStore(t, baseDSN)
	const (
		legacyConstraint = "memory_curation_run_inputs_check"
		stagedConstraint = "memory_curation_run_inputs_retention_check"
	)
	assertConstraint := func(name string, wantExists, wantValidated bool) {
		t.Helper()
		var exists, validated bool
		if err := st.pool.QueryRow(context.Background(), `
			SELECT count(*) = 1,
			       COALESCE(bool_and(convalidated), false)
			  FROM pg_constraint
			 WHERE conrelid=to_regclass('memory_curation_run_inputs')
			   AND conname=$1`, name).Scan(&exists, &validated); err != nil {
			t.Fatal(err)
		}
		if exists != wantExists || validated != wantValidated {
			t.Fatalf("constraint %s = exists:%t validated:%t; want %t/%t",
				name, exists, validated, wantExists, wantValidated)
		}
	}

	migrationTestUpTo(t, dsn, 61)
	assertConstraint(legacyConstraint, true, true)
	assertConstraint(stagedConstraint, false, false)

	// 0062 adds the widened check without scanning existing rows under the
	// metadata lock; the legacy validated constraint remains authoritative.
	migrationTestUpTo(t, dsn, 62)
	assertConstraint(legacyConstraint, true, true)
	assertConstraint(stagedConstraint, true, false)

	// 0063 validates with PostgreSQL's lower-lock validation path.
	migrationTestUpTo(t, dsn, 63)
	assertConstraint(legacyConstraint, true, true)
	assertConstraint(stagedConstraint, true, true)

	// 0064 is only a brief metadata drop/rename after both shapes are valid.
	migrationTestUpTo(t, dsn, 64)
	assertConstraint(legacyConstraint, true, true)
	assertConstraint(stagedConstraint, false, false)
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

func assertMigrationTestForeignKeyDeferral(
	t *testing.T,
	st *Store,
	table string,
	referencedTable string,
	name string,
	wantDeferrable bool,
	wantInitiallyDeferred bool,
) {
	t.Helper()
	var count int
	var deferrable, initiallyDeferred bool
	if err := st.pool.QueryRow(context.Background(), `
		SELECT count(*),
		       COALESCE(bool_and(condeferrable), false),
		       COALESCE(bool_and(condeferred), false)
		  FROM pg_constraint
		 WHERE conrelid=to_regclass($1)
		   AND confrelid=to_regclass($2)
		   AND contype='f'
		   AND ($3='' OR conname=$3)`, table, referencedTable, name).Scan(
		&count, &deferrable, &initiallyDeferred,
	); err != nil {
		t.Fatal(err)
	}
	if count != 1 || deferrable != wantDeferrable || initiallyDeferred != wantInitiallyDeferred {
		t.Fatalf("foreign key %s -> %s (%q) = count %d, deferrable %t, initially deferred %t; want 1, %t, %t",
			table, referencedTable, name, count, deferrable, initiallyDeferred,
			wantDeferrable, wantInitiallyDeferred)
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

func assertMigrationTestIndexMethod(t *testing.T, st *Store, table, index, want string) {
	t.Helper()
	var got string
	if err := st.pool.QueryRow(context.Background(), `
		SELECT access_method.amname
		  FROM pg_index index_row
		  JOIN pg_class index_class ON index_class.oid=index_row.indexrelid
		  JOIN pg_am access_method ON access_method.oid=index_class.relam
		 WHERE index_row.indrelid=to_regclass($1)
		   AND index_row.indexrelid=to_regclass($2)`, table, index).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("index %s.%s access method = %q, want %q", table, index, got, want)
	}
}

func assertMigrationTestIndexShape(
	t *testing.T,
	st *Store,
	table string,
	index string,
	wantColumns []string,
	wantPredicateFragments []string,
) {
	t.Helper()
	var columns []string
	var predicate string
	if err := st.pool.QueryRow(context.Background(), `
		SELECT ARRAY(
		         SELECT attribute.attname
		           FROM unnest(index_row.indkey) WITH ORDINALITY AS key(attnum, position)
		           JOIN pg_attribute attribute
		             ON attribute.attrelid=index_row.indrelid
		            AND attribute.attnum=key.attnum
		          ORDER BY key.position
		       ),
		       COALESCE(pg_get_expr(index_row.indpred,index_row.indrelid),'')
		  FROM pg_index index_row
		 WHERE index_row.indrelid=to_regclass($1)
		   AND index_row.indexrelid=to_regclass($2)`, table, index).Scan(&columns, &predicate); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(columns, wantColumns) {
		t.Fatalf("index %s.%s columns = %v, want %v", table, index, columns, wantColumns)
	}
	for _, fragment := range wantPredicateFragments {
		if !strings.Contains(predicate, fragment) {
			t.Fatalf("index %s.%s predicate = %q, want fragment %q", table, index, predicate, fragment)
		}
	}
}

func assertMigrationTestIndexUnique(
	t *testing.T,
	st *Store,
	table string,
	index string,
	want bool,
) {
	t.Helper()
	var got bool
	if err := st.pool.QueryRow(context.Background(), `
		SELECT indisunique
		FROM pg_index
		WHERE indrelid=to_regclass($1) AND indexrelid=to_regclass($2)`,
		table, index).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("index %s.%s unique = %t, want %t", table, index, got, want)
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
