package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	agentEmailCapacityAccountID = "acc_agent_email_capacity"
	agentEmailCapacityRealmID   = "realm_aaaaaaaaaaaaaaaa"
	agentEmailCapacityOwnerID   = "agent_aaaaaaaaaaaaaaaa"
	agentEmailCapacityAddressID = "eaddr_aaaaaaaaaaaaaaaa"
	agentEmailCapacityMailboxID = "emb_aaaaaaaaaaaaaaaa"
	agentEmailCapacityRecipient = "owner.aaaaaaaaaaaaaaaa@agent-mail.witwave.ai"
)

func TestMigrations79Through81AgentEmailStorageCapacityPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 78)
	assertMigrationTestVersion(t, dsn, 78)
	insertAgentEmailCapacityMigrationFixture(t, st)

	legacyAttachment := []byte("legacy attachment-bearing raw MIME")
	legacyNoAttachment := []byte("legacy text-only raw MIME")
	legacyParseError := []byte("legacy malformed raw MIME")
	insertLegacyAgentEmailCapacityMessage(
		t, st, "emsg_bbbbbbbbbbbbbbbb", legacyAttachment, "parsed", "", 1,
	)
	insertLegacyAgentEmailCapacityMessage(
		t, st, "emsg_cccccccccccccccc", legacyNoAttachment, "parsed", "", 0,
	)
	insertLegacyAgentEmailCapacityMessage(
		t, st, "emsg_dddddddddddddddd", legacyParseError, "error",
		"malformed MIME", 0,
	)

	migrationTestUpTo(t, dsn, 79)
	assertMigrationTestVersion(t, dsn, 79)
	assertAgentEmailCapacitySchema(t, st, true, false)

	var legacyPending, retainedBytes int64
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM agent_email_messages
		    WHERE payload_retention_state='legacy_pending'),
		  (SELECT retained_agent_email_attachment_bytes
		     FROM accounts WHERE id=$1)`,
		agentEmailCapacityAccountID,
	).Scan(&legacyPending, &retainedBytes); err != nil {
		t.Fatal(err)
	}
	if legacyPending != 3 || retainedBytes != 0 {
		t.Fatalf("schema-79 legacy state = pending %d retained bytes %d, want 3/0",
			legacyPending, retainedBytes)
	}

	// This insert deliberately lists only schema-78 columns. The schema-79
	// BEFORE trigger must normalize a rolling old replica before the new
	// checks and derived-counter trigger run.
	oldWriterRaw := []byte("rolling old writer attachment")
	insertLegacyAgentEmailCapacityMessage(
		t, st, "emsg_eeeeeeeeeeeeeeee", oldWriterRaw, "parsed", "", 1,
	)
	var oldWriterState string
	var oldWriterStorage, oldWriterRetained int64
	if err := st.pool.QueryRow(ctx, `
		SELECT payload_retention_state,attachment_storage_bytes,
		       retained_attachment_storage_bytes
		  FROM agent_email_messages
		 WHERE id='emsg_eeeeeeeeeeeeeeee'`,
	).Scan(&oldWriterState, &oldWriterStorage, &oldWriterRetained); err != nil {
		t.Fatal(err)
	}
	if oldWriterState != "retained" ||
		oldWriterStorage != int64(len(oldWriterRaw)) ||
		oldWriterRetained != int64(len(oldWriterRaw)) {
		t.Fatalf("old-writer normalization = state %q storage %d retained %d",
			oldWriterState, oldWriterStorage, oldWriterRetained)
	}
	assertAgentEmailCapacityCounter(t, st, 0)

	migrationTestUpTo(t, dsn, 80)
	assertMigrationTestVersion(t, dsn, 80)
	assertAgentEmailCapacitySchema(t, st, true, true)

	wantReconciled := int64(
		len(legacyAttachment) + len(legacyParseError) + len(oldWriterRaw),
	)
	var attachmentStorage, noAttachmentStorage, parseErrorStorage int64
	var nonRetainedStates, unaccountedRows int64
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT attachment_storage_bytes FROM agent_email_messages
		    WHERE id='emsg_bbbbbbbbbbbbbbbb'),
		  (SELECT attachment_storage_bytes FROM agent_email_messages
		    WHERE id='emsg_cccccccccccccccc'),
		  (SELECT attachment_storage_bytes FROM agent_email_messages
		    WHERE id='emsg_dddddddddddddddd'),
		  (SELECT count(*) FROM agent_email_messages
		    WHERE payload_retention_state <> 'retained'),
		  (SELECT count(*) FROM agent_email_messages
		    WHERE NOT attachment_storage_accounted)`,
	).Scan(
		&attachmentStorage, &noAttachmentStorage, &parseErrorStorage,
		&nonRetainedStates, &unaccountedRows,
	); err != nil {
		t.Fatal(err)
	}
	if attachmentStorage != int64(len(legacyAttachment)) ||
		noAttachmentStorage != 0 ||
		parseErrorStorage != int64(len(legacyParseError)) ||
		nonRetainedStates != 0 || unaccountedRows != 0 {
		t.Fatalf(
			"schema-80 backfill = attachment %d text %d parse-error %d non-retained %d unaccounted %d",
			attachmentStorage, noAttachmentStorage, parseErrorStorage,
			nonRetainedStates, unaccountedRows,
		)
	}
	assertAgentEmailCapacityCounter(t, st, wantReconciled)

	// Simulate harmless Phase-A drift before finite plan limits activate.
	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts
		   SET retained_agent_email_attachment_bytes=7
		 WHERE id=$1`,
		agentEmailCapacityAccountID,
	); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 81)
	assertMigrationTestVersion(t, dsn, 81)
	assertAgentEmailCapacityCounter(t, st, wantReconciled)

	testAgentEmailCapacityCounterTrigger(t, st, wantReconciled)
	testAgentEmailCapacityRowShapes(t, st, wantReconciled)
}

func TestMigration79AgentEmailStorageCapacityDownOnEmptyMessagesPostgres(
	t *testing.T,
) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 78)
	insertAgentEmailCapacityMigrationFixture(t, st)
	migrationTestUpTo(t, dsn, 81)
	assertMigrationTestVersion(t, dsn, 81)

	// Reconciliation and backfill have no inverse, so their safe Down paths
	// leave the schema-79 projection intact.
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 80)
	assertAgentEmailCapacitySchema(t, st, true, true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 79)
	assertAgentEmailCapacitySchema(t, st, true, true)

	// With no email messages, schema 79 can be removed without losing payload
	// projections or violating the old raw_mime NOT NULL shape.
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 78)
	assertAgentEmailCapacitySchema(t, st, false, false)
	assertAgentEmailRawMIMENullability(t, st, false)
	assertAgentEmailRawSizeConstraintCeiling(t, st, "5242880")

	// The empty downgrade is reversible and returns to the exact current
	// contract without operator cleanup.
	migrationTestUpTo(t, dsn, 81)
	assertMigrationTestVersion(t, dsn, 81)
	assertAgentEmailCapacitySchema(t, st, true, true)
	assertAgentEmailRawMIMENullability(t, st, true)
	assertAgentEmailRawSizeConstraintCeiling(t, st, "26214400")
}

func TestMigration80AgentEmailCapacitySupportedWriterFenceRetryPostgres(
	t *testing.T,
) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 78)
	insertAgentEmailCapacityMigrationFixture(t, st)
	migrationTestUpTo(t, dsn, 79)

	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	var accountID string
	if err := writer.QueryRow(ctx, `
		SELECT id
		  FROM accounts
		 WHERE id=$1
		 FOR NO KEY UPDATE`,
		agentEmailCapacityAccountID,
	).Scan(&accountID); err != nil {
		t.Fatal(err)
	}

	migrationDB := migrationTestSQLDB(t, dsn)
	err = migrationPromptContentionError(t, migrationDB, 80, func() {
		_ = writer.Rollback(ctx)
	})
	_ = migrationDB.Close()
	assertAgentEmailCapacityLockUnavailable(
		t, err, "schema 80 with supported account-first writer",
	)
	assertMigrationTestVersion(t, dsn, 79)

	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 80)
	assertMigrationTestVersion(t, dsn, 80)
}

func TestMigration80AgentEmailCapacityDirectMessageWriterFenceRetryPostgres(
	t *testing.T,
) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 78)
	insertAgentEmailCapacityMigrationFixture(t, st)
	migrationTestUpTo(t, dsn, 79)
	raw := []byte("direct writer")
	if err := insertCurrentAgentEmailCapacityMessage(
		st, "emsg_7777777777777777", raw, int64(len(raw)),
		"parsed", nil, 0, nil, nil, 0, 0, "retained",
	); err != nil {
		t.Fatal(err)
	}

	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `
		UPDATE agent_email_messages
		   SET header_subject=header_subject
		 WHERE id='emsg_7777777777777777'`,
	); err != nil {
		t.Fatal(err)
	}

	migrationDB := migrationTestSQLDB(t, dsn)
	err = migrationPromptContentionError(t, migrationDB, 80, func() {
		_ = writer.Rollback(ctx)
	})
	_ = migrationDB.Close()
	assertAgentEmailCapacityLockUnavailable(
		t, err, "schema 80 with direct message-table writer",
	)
	assertMigrationTestVersion(t, dsn, 79)

	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 80)
	assertMigrationTestVersion(t, dsn, 80)
}

func TestMigration81AgentEmailCapacitySupportedWriterFenceRetryPostgres(
	t *testing.T,
) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 80)
	insertAgentEmailCapacityMigrationFixture(t, st)

	// Supported ingestion takes this account row lock before it examines or
	// changes the message table. Its ROW SHARE table lock must conflict with
	// migration 81's first EXCLUSIVE NOWAIT fence, so migration cannot invert
	// the account-row/message-table lock order.
	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	var accountID string
	if err := writer.QueryRow(ctx, `
		SELECT id
		  FROM accounts
		 WHERE id=$1
		 FOR NO KEY UPDATE`,
		agentEmailCapacityAccountID,
	).Scan(&accountID); err != nil {
		t.Fatal(err)
	}

	migrationDB := migrationTestSQLDB(t, dsn)
	err = migrationPromptContentionError(t, migrationDB, 81, func() {
		_ = writer.Rollback(ctx)
	})
	_ = migrationDB.Close()
	assertAgentEmailCapacityLockUnavailable(
		t, err, "schema 81 with supported account-first writer",
	)
	assertMigrationTestVersion(t, dsn, 80)

	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 81)
	assertMigrationTestVersion(t, dsn, 81)
	assertAgentEmailCapacityCounter(t, st, 0)
}

func TestMigration81AgentEmailCapacityDirectMessageWriterFenceRetryPostgres(
	t *testing.T,
) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 80)
	insertAgentEmailCapacityMigrationFixture(t, st)
	raw := []byte("direct message-table writer")
	const messageID = "emsg_7777777777777777"
	if err := insertCurrentAgentEmailCapacityMessage(
		st, messageID, raw, int64(len(raw)), "parsed", nil, 0,
		nil, nil, 0, 0, "retained",
	); err != nil {
		t.Fatal(err)
	}

	// This direct/manual writer deliberately bypasses the supported account
	// lock. Its message-table ROW EXCLUSIVE lock must be caught by migration
	// 81's second SHARE NOWAIT fence.
	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `
		UPDATE agent_email_messages
		   SET header_subject=header_subject
		 WHERE id=$1`,
		messageID,
	); err != nil {
		t.Fatal(err)
	}

	migrationDB := migrationTestSQLDB(t, dsn)
	err = migrationPromptContentionError(t, migrationDB, 81, func() {
		_ = writer.Rollback(ctx)
	})
	_ = migrationDB.Close()
	assertAgentEmailCapacityLockUnavailable(
		t, err, "schema 81 with direct message-table writer",
	)
	assertMigrationTestVersion(t, dsn, 80)

	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 81)
	assertMigrationTestVersion(t, dsn, 81)
	assertAgentEmailCapacityCounter(t, st, 0)
}

func TestSchema81AgentEmailCapacityMixedOldWritersDoNotUpgradeAccountLockPostgres(
	t *testing.T,
) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 81)
	insertAgentEmailCapacityMigrationFixture(t, st)

	writers := make([]pgx.Tx, 0, 2)
	for range 2 {
		writer, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		writers = append(writers, writer)
		defer func() { _ = writer.Rollback(ctx) }()
		var accountID string
		if err := writer.QueryRow(ctx, `
			SELECT id
			  FROM accounts
			 WHERE id=$1
			 FOR SHARE`,
			agentEmailCapacityAccountID,
		).Scan(&accountID); err != nil {
			t.Fatal(err)
		}
	}

	for index, writer := range writers {
		raw := []byte("mixed old writer retained attachment")
		messageID := []string{
			"emsg_mmmmmmmmmmmmmmmm",
			"emsg_nnnnnnnnnnnnnnnn",
		}[index]
		writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := writer.Exec(writeCtx, `
			INSERT INTO agent_email_messages
			  (id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,
			   provider,envelope_sender,envelope_recipient,agent_segment,realm_label,
			   raw_mime,raw_size_bytes,raw_sha256,parse_state,attachment_count,
			   sender_verification_state,duplicate_group_sha256,received_at)
			VALUES
			  ($1,$2,$3,$4,$5,$6,'migration_test','sender@example.com',$7,
			   'owner','aaaaaaaaaaaaaaaa',$8,$9,$10,'parsed',1,'unverified',
			   $11,clock_timestamp())`,
			messageID, agentEmailCapacityAccountID, agentEmailCapacityRealmID,
			agentEmailCapacityMailboxID, agentEmailCapacityOwnerID,
			agentEmailCapacityAddressID, agentEmailCapacityRecipient, raw,
			len(raw), strings.Repeat("e", 64), strings.Repeat("f", 64),
		)
		cancel()
		if err != nil {
			t.Fatalf("mixed old writer %d insert: %v", index+1, err)
		}
	}
	for _, writer := range writers {
		if err := writer.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var unaccounted int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM agent_email_messages
		 WHERE account_id=$1
		   AND payload_retention_state='retained'
		   AND retained_attachment_storage_bytes=raw_size_bytes
		   AND NOT attachment_storage_accounted`,
		agentEmailCapacityAccountID,
	).Scan(&unaccounted); err != nil {
		t.Fatal(err)
	}
	if unaccounted != 2 {
		t.Fatalf("mixed old-writer unaccounted rows = %d, want 2", unaccounted)
	}
	assertAgentEmailCapacityCounter(t, st, 0)
}

func migrationPromptContentionError(
	t *testing.T,
	migrationDB *sql.DB,
	target int64,
	release func(),
) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- migrationTestUpToDB(migrationDB, target)
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-errCh:
		return err
	case <-timer.C:
		release()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
		t.Fatalf(
			"schema %d migration did not fail promptly under write contention",
			target,
		)
		return nil
	}
}

func assertAgentEmailCapacityLockUnavailable(
	t *testing.T,
	err error,
	label string,
) {
	t.Helper()
	var lockErr *pgconn.PgError
	if !errors.As(err, &lockErr) || lockErr.Code != "55P03" {
		t.Fatalf("%s error = %v, want SQLSTATE 55P03", label, err)
	}
}

func insertAgentEmailCapacityMigrationFixture(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	mustMigrationTestExec(t, tx, `
		INSERT INTO accounts (id,is_default,display_name)
		VALUES ($1,false,'agent email capacity migration')`,
		agentEmailCapacityAccountID,
	)
	mustMigrationTestExec(t, tx, `
		INSERT INTO realms (id,account_id,name)
		VALUES ($2,$1,'email capacity')`,
		agentEmailCapacityAccountID, agentEmailCapacityRealmID,
	)
	mustMigrationTestExec(t, tx, `
		INSERT INTO agents (id,realm_id,name)
		VALUES ($2,$1,'owner')`,
		agentEmailCapacityRealmID, agentEmailCapacityOwnerID,
	)
	mustMigrationTestExec(t, tx, `
		INSERT INTO agent_email_addresses
		  (id,account_id,realm_id,provisioned_agent_id,domain,agent_segment,
		   realm_label,local_part,provisioning_kind)
		VALUES
		  ($4,$1,$2,$3,'agent-mail.witwave.ai','owner',
		   'aaaaaaaaaaaaaaaa','owner.aaaaaaaaaaaaaaaa','derived')`,
		agentEmailCapacityAccountID, agentEmailCapacityRealmID,
		agentEmailCapacityOwnerID, agentEmailCapacityAddressID,
	)
	mustMigrationTestExec(t, tx, `
		INSERT INTO agent_email_mailboxes
		  (id,account_id,realm_id,owner_agent_id,address_id,receive_state)
		VALUES ($5,$1,$2,$3,$4,'enabled')`,
		agentEmailCapacityAccountID, agentEmailCapacityRealmID,
		agentEmailCapacityOwnerID, agentEmailCapacityAddressID,
		agentEmailCapacityMailboxID,
	)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func insertLegacyAgentEmailCapacityMessage(
	t *testing.T,
	st *Store,
	id string,
	raw []byte,
	parseState string,
	parseError string,
	attachmentCount int64,
) {
	t.Helper()
	var parseErrorValue any
	if parseError != "" {
		parseErrorValue = parseError
	}
	if _, err := st.pool.Exec(context.Background(), `
		INSERT INTO agent_email_messages
		  (id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,
		   provider,envelope_sender,envelope_recipient,agent_segment,realm_label,
		   raw_mime,raw_size_bytes,raw_sha256,parse_state,parse_error,
		   attachment_count,sender_verification_state,
		   duplicate_group_sha256,received_at)
		VALUES
		  ($1,$2,$3,$4,$5,$6,'migration_test','sender@example.com',$7,
		   'owner','aaaaaaaaaaaaaaaa',$8,$9,$10,$11,$12,$13,'unverified',
		   $14,clock_timestamp())`,
		id, agentEmailCapacityAccountID, agentEmailCapacityRealmID,
		agentEmailCapacityMailboxID, agentEmailCapacityOwnerID,
		agentEmailCapacityAddressID, agentEmailCapacityRecipient, raw, len(raw),
		strings.Repeat("a", 64), parseState, parseErrorValue, attachmentCount,
		strings.Repeat("b", 64),
	); err != nil {
		t.Fatal(err)
	}
}

func insertCurrentAgentEmailCapacityMessage(
	st *Store,
	id string,
	raw []byte,
	rawSize int64,
	parseState string,
	parseError any,
	attachmentCount int64,
	bodyText any,
	bodyTextKind any,
	attachmentStorage int64,
	retainedStorage int64,
	retentionState string,
) error {
	_, err := st.pool.Exec(context.Background(), `
		INSERT INTO agent_email_messages
		  (id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,
		   provider,envelope_sender,envelope_recipient,agent_segment,realm_label,
		   raw_mime,raw_size_bytes,raw_sha256,parse_state,parse_error,
		   attachment_count,body_text,body_text_kind,attachment_storage_bytes,
		   retained_attachment_storage_bytes,payload_retention_state,
		   attachment_storage_accounted,
		   sender_verification_state,duplicate_group_sha256,received_at)
		VALUES
		  ($1,$2,$3,$4,$5,$6,'migration_test','sender@example.com',$7,
		   'owner','aaaaaaaaaaaaaaaa',$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,
		   $18,true,'unverified',$19,clock_timestamp())`,
		id, agentEmailCapacityAccountID, agentEmailCapacityRealmID,
		agentEmailCapacityMailboxID, agentEmailCapacityOwnerID,
		agentEmailCapacityAddressID, agentEmailCapacityRecipient, raw, rawSize,
		strings.Repeat("c", 64), parseState, parseError, attachmentCount,
		bodyText, bodyTextKind, attachmentStorage, retainedStorage,
		retentionState, strings.Repeat("d", 64),
	)
	return err
}

func testAgentEmailCapacityCounterTrigger(
	t *testing.T,
	st *Store,
	baseline int64,
) {
	t.Helper()
	ctx := context.Background()
	raw := []byte("new retained attachment")
	id := "emsg_ffffffffffffffff"
	if err := insertCurrentAgentEmailCapacityMessage(
		st, id, raw, int64(len(raw)), "parsed", nil, 1, nil, nil,
		int64(len(raw)), int64(len(raw)), "retained",
	); err != nil {
		t.Fatal(err)
	}
	assertAgentEmailCapacityCounter(t, st, baseline+int64(len(raw)))

	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_messages
		   SET raw_mime=NULL,
		       body_text='bounded fallback',
		       body_text_kind='text/plain',
		       retained_attachment_storage_bytes=0,
		       payload_retention_state='omitted_capacity'
		 WHERE id=$1`,
		id,
	); err != nil {
		t.Fatal(err)
	}
	assertAgentEmailCapacityCounter(t, st, baseline)

	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_messages
		   SET raw_mime=$2,
		       body_text=NULL,
		       body_text_kind=NULL,
		       retained_attachment_storage_bytes=$3,
		       payload_retention_state='retained'
		 WHERE id=$1`,
		id, raw, len(raw),
	); err != nil {
		t.Fatal(err)
	}
	assertAgentEmailCapacityCounter(t, st, baseline+int64(len(raw)))

	if _, err := st.pool.Exec(ctx,
		`DELETE FROM agent_email_messages WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	assertAgentEmailCapacityCounter(t, st, baseline)
}

func testAgentEmailCapacityRowShapes(
	t *testing.T,
	st *Store,
	baseline int64,
) {
	t.Helper()
	ctx := context.Background()

	maxRaw := bytes.Repeat([]byte("x"), 25*1024*1024)
	if err := insertCurrentAgentEmailCapacityMessage(
		st, "emsg_2222222222222222", maxRaw, int64(len(maxRaw)),
		"parsed", nil, 0, nil, nil, 0, 0, "retained",
	); err != nil {
		t.Fatalf("insert exact 25 MiB raw MIME: %v", err)
	}
	assertAgentEmailCapacityCounter(t, st, baseline)
	if _, err := st.pool.Exec(ctx,
		`DELETE FROM agent_email_messages WHERE id='emsg_2222222222222222'`); err != nil {
		t.Fatal(err)
	}

	tooLarge := bytes.Repeat([]byte("y"), 25*1024*1024+1)
	err := insertCurrentAgentEmailCapacityMessage(
		st, "emsg_3333333333333333", tooLarge, int64(len(tooLarge)),
		"parsed", nil, 0, nil, nil, 0, 0, "retained",
	)
	assertAgentEmailCapacityCheckViolation(t, err, "raw above 25 MiB")

	const omittedRawSize = int64(4096)
	if err := insertCurrentAgentEmailCapacityMessage(
		st, "emsg_4444444444444444", nil, omittedRawSize,
		"parsed", nil, 1, "bounded fallback", "text/plain",
		omittedRawSize, 0, "omitted_capacity",
	); err != nil {
		t.Fatalf("insert valid omitted-capacity row: %v", err)
	}
	var raw []byte
	var body, bodyKind, state string
	var stored, retained int64
	if err := st.pool.QueryRow(ctx, `
		SELECT raw_mime,body_text,body_text_kind,attachment_storage_bytes,
		       retained_attachment_storage_bytes,payload_retention_state
		  FROM agent_email_messages
		 WHERE id='emsg_4444444444444444'`,
	).Scan(&raw, &body, &bodyKind, &stored, &retained, &state); err != nil {
		t.Fatal(err)
	}
	if raw != nil || body != "bounded fallback" || bodyKind != "text/plain" ||
		stored != omittedRawSize || retained != 0 ||
		state != "omitted_capacity" {
		t.Fatalf(
			"omitted row = raw %v body %q/%q storage %d/%d state %q",
			raw, body, bodyKind, stored, retained, state,
		)
	}
	assertAgentEmailCapacityCounter(t, st, baseline)

	err = insertCurrentAgentEmailCapacityMessage(
		st, "emsg_5555555555555555", nil, 128,
		"parsed", nil, 0, "text", "text/plain", 0, 0,
		"omitted_capacity",
	)
	assertAgentEmailCapacityCheckViolation(
		t, err, "omitted non-attachment message",
	)
	err = insertCurrentAgentEmailCapacityMessage(
		st, "emsg_6666666666666666", nil, 128,
		"parsed", nil, 1, nil, nil, 128, 128, "retained",
	)
	assertAgentEmailCapacityCheckViolation(t, err, "retained row without raw MIME")

	if _, err := st.pool.Exec(ctx,
		`DELETE FROM agent_email_messages WHERE id='emsg_4444444444444444'`); err != nil {
		t.Fatal(err)
	}
	assertAgentEmailCapacityCounter(t, st, baseline)
}

func assertAgentEmailCapacitySchema(
	t *testing.T,
	st *Store,
	want bool,
	wantValidated bool,
) {
	t.Helper()
	assertMigrationTestColumn(
		t, st, "accounts", "retained_agent_email_attachment_bytes", want,
	)
	for _, column := range []string{
		"body_text",
		"body_text_kind",
		"attachment_storage_bytes",
		"retained_attachment_storage_bytes",
		"payload_retention_state",
		"attachment_storage_accounted",
	} {
		assertMigrationTestColumn(t, st, "agent_email_messages", column, want)
	}
	for _, constraint := range []string{
		"agent_email_messages_raw_storage_shape",
		"agent_email_messages_body_projection_shape",
		"agent_email_messages_attachment_storage_shape",
	} {
		assertAgentEmailCapacityConstraint(
			t, st, "agent_email_messages", constraint, want, wantValidated,
		)
	}
	assertAgentEmailCapacityConstraint(
		t, st, "accounts",
		"accounts_retained_agent_email_attachment_bytes_range",
		want, wantValidated,
	)
	for _, trigger := range []string{
		"agent_email_messages_normalize_legacy_storage",
		"agent_email_messages_maintain_attachment_counter",
	} {
		assertAgentEmailCapacityTrigger(t, st, trigger, want)
	}
}

func assertAgentEmailCapacityConstraint(
	t *testing.T,
	st *Store,
	table string,
	name string,
	want bool,
	wantValidated bool,
) {
	t.Helper()
	var count int
	var validated bool
	if err := st.pool.QueryRow(context.Background(), `
		SELECT count(*),COALESCE(bool_and(convalidated),false)
		  FROM pg_constraint
		 WHERE conrelid=to_regclass($1) AND conname=$2`,
		table, name,
	).Scan(&count, &validated); err != nil {
		t.Fatal(err)
	}
	if (count == 1) != want || (want && validated != wantValidated) {
		t.Fatalf(
			"constraint %s.%s = count %d validated %t, want exists %t validated %t",
			table, name, count, validated, want, wantValidated,
		)
	}
}

func assertAgentEmailCapacityTrigger(
	t *testing.T,
	st *Store,
	name string,
	want bool,
) {
	t.Helper()
	var got bool
	if err := st.pool.QueryRow(context.Background(), `
		SELECT EXISTS (
		  SELECT 1
		    FROM pg_trigger
		   WHERE tgrelid='agent_email_messages'::regclass
		     AND tgname=$1
		     AND NOT tgisinternal
		)`,
		name,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("trigger %s exists = %t, want %t", name, got, want)
	}
}

func assertAgentEmailCapacityCounter(t *testing.T, st *Store, want int64) {
	t.Helper()
	var got int64
	if err := st.pool.QueryRow(context.Background(), `
		SELECT retained_agent_email_attachment_bytes
		  FROM accounts
		 WHERE id=$1`,
		agentEmailCapacityAccountID,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("retained agent-email attachment bytes = %d, want %d", got, want)
	}
}

func assertAgentEmailCapacityCheckViolation(
	t *testing.T,
	err error,
	label string,
) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("%s error = %v, want PostgreSQL check violation", label, err)
	}
}

func assertAgentEmailRawMIMENullability(
	t *testing.T,
	st *Store,
	wantNullable bool,
) {
	t.Helper()
	var gotNullable bool
	if err := st.pool.QueryRow(context.Background(), `
		SELECT NOT attnotnull
		  FROM pg_attribute
		 WHERE attrelid='agent_email_messages'::regclass
		   AND attname='raw_mime'
		   AND NOT attisdropped`,
	).Scan(&gotNullable); err != nil {
		t.Fatal(err)
	}
	if gotNullable != wantNullable {
		t.Fatalf("agent_email_messages.raw_mime nullable = %t, want %t",
			gotNullable, wantNullable)
	}
}

func assertAgentEmailRawSizeConstraintCeiling(
	t *testing.T,
	st *Store,
	want string,
) {
	t.Helper()
	var definitions []string
	rows, err := st.pool.Query(context.Background(), `
		SELECT pg_get_constraintdef(oid)
		  FROM pg_constraint
		 WHERE conrelid='agent_email_messages'::regclass
		   AND contype='c'
		   AND pg_get_constraintdef(oid) LIKE '%raw_size_bytes%'
		   AND pg_get_constraintdef(oid) LIKE '%raw_mime%'`,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var definition string
		if err := rows.Scan(&definition); err != nil {
			t.Fatal(err)
		}
		definitions = append(definitions, definition)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 || !strings.Contains(definitions[0], want) {
		t.Fatalf("raw MIME size constraints = %q, want one containing %s",
			definitions, want)
	}
}
