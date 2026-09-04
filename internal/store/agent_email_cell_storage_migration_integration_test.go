package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/witwave-ai/witself/internal/testenv"
)

type agentEmailCellStorageState struct {
	retainedBytes int64
	rootRows      int64
	countedRows   int64
}

func TestAgentEmailCellStorageMigrationBackfillAndCascadePostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 90)
	insertAgentEmailCapacityMigrationFixture(t, st)

	raw := []byte("schema-90 retained raw MIME")
	if err := insertCurrentAgentEmailCapacityMessage(
		st, "emsg_mmmmmmmmmmmmmmmm", raw, int64(len(raw)),
		"parsed", nil, 0, "bounded body", "text/plain",
		0, 0, "retained",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_deliveries
		  (message_id,account_id,realm_id,mailbox_id,owner_agent_id,folder)
		VALUES
		  ('emsg_mmmmmmmmmmmmmmmm',$1,$2,$3,$4,'inbox')`,
		agentEmailCapacityAccountID, agentEmailCapacityRealmID,
		agentEmailCapacityMailboxID, agentEmailCapacityOwnerID,
	); err != nil {
		t.Fatal(err)
	}
	insertAgentEmailCellStorageOutboundFixture(t, st)

	migrationTestUpTo(t, dsn, 91)
	assertMigrationTestVersion(t, dsn, 91)
	state := readAgentEmailCellStorageState(t, st)
	if state.rootRows != 2 || state.countedRows != 5 {
		t.Fatalf("backfilled rows = roots %d counted %d, want 2/5",
			state.rootRows, state.countedRows)
	}
	var measuredBytes int64
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  COALESCE((SELECT sum(witself_agent_email_message_cell_storage_bytes(row_value))
		              FROM agent_email_messages AS row_value),0)::bigint
		+ COALESCE((SELECT sum(witself_agent_email_delivery_cell_storage_bytes(row_value))
		              FROM agent_email_deliveries AS row_value),0)::bigint
		+ COALESCE((SELECT sum(witself_agent_email_outbound_cell_storage_bytes(row_value))
		              FROM agent_email_outbound_messages AS row_value),0)::bigint
		+ COALESCE((SELECT sum(witself_agent_email_provider_event_cell_storage_bytes(row_value))
		              FROM agent_email_outbound_provider_events AS row_value),0)::bigint
		+ COALESCE((SELECT sum(witself_agent_email_suppression_cell_storage_bytes(row_value))
		              FROM agent_email_outbound_recipient_suppressions AS row_value),0)::bigint`,
	).Scan(&measuredBytes); err != nil {
		t.Fatal(err)
	}
	if state.retainedBytes != measuredBytes || measuredBytes <= 5*8192 {
		t.Fatalf("backfilled bytes = ledger %d measured %d", state.retainedBytes, measuredBytes)
	}

	var defaultAdmissionBytes, defaultAdmissionRows, defaultHardBytes, defaultHardRows int64
	if err := st.pool.QueryRow(ctx, `
		SELECT admission_bytes,admission_root_rows,hard_bytes,hard_counted_rows
		  FROM agent_email_cell_storage_capacity WHERE singleton=1`,
	).Scan(
		&defaultAdmissionBytes, &defaultAdmissionRows,
		&defaultHardBytes, &defaultHardRows,
	); err != nil {
		t.Fatal(err)
	}
	if defaultAdmissionBytes != 3*1024*1024*1024 ||
		defaultAdmissionRows != 25000 ||
		defaultHardBytes != 4*1024*1024*1024 || defaultHardRows != 100000 {
		t.Fatalf("default limits = %d/%d %d/%d",
			defaultAdmissionBytes, defaultAdmissionRows,
			defaultHardBytes, defaultHardRows)
	}

	if err := st.ConfigureAgentEmailCellStorageLimits(
		ctx, 1<<20, 10, 2<<20, 20,
	); err != nil {
		t.Fatal(err)
	}
	var admissionBytes, admissionRows, hardBytes, hardRows int64
	if err := st.pool.QueryRow(ctx, `
		SELECT admission_bytes,admission_root_rows,hard_bytes,hard_counted_rows
		  FROM agent_email_cell_storage_capacity WHERE singleton=1`,
	).Scan(&admissionBytes, &admissionRows, &hardBytes, &hardRows); err != nil {
		t.Fatal(err)
	}
	if admissionBytes != 1<<20 || admissionRows != 10 ||
		hardBytes != 2<<20 || hardRows != 20 {
		t.Fatalf("configured limits = %d/%d %d/%d",
			admissionBytes, admissionRows, hardBytes, hardRows)
	}

	beforeUpdate := state.retainedBytes
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_messages
		   SET header_subject='a materially longer retained header projection'
		 WHERE id='emsg_mmmmmmmmmmmmmmmm'`,
	); err != nil {
		t.Fatal(err)
	}
	state = readAgentEmailCellStorageState(t, st)
	if state.retainedBytes <= beforeUpdate || state.rootRows != 2 || state.countedRows != 5 {
		t.Fatalf("updated ledger = %+v, before bytes %d", state, beforeUpdate)
	}

	deleteAgentEmailCellStorageFixtureAccount(t, st)
	state = readAgentEmailCellStorageState(t, st)
	if state != (agentEmailCellStorageState{}) {
		t.Fatalf("account cascade retained ledger state %+v", state)
	}
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 90)
}

func TestAgentEmailCellStorageCapacityAndRecoveryPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 91)
	insertAgentEmailCapacityMigrationFixture(t, st)
	if err := st.ConfigureAgentEmailCellStorageLimits(
		ctx, 1<<20, 1, 2<<20, 10,
	); err != nil {
		t.Fatal(err)
	}

	raw := []byte("concurrent retained payload")
	ids := []string{"emsg_nnnnnnnnnnnnnnnn", "emsg_pppppppppppppppp"}
	errorsByID := make([]error, len(ids))
	var group sync.WaitGroup
	for index, messageID := range ids {
		index, messageID := index, messageID
		group.Add(1)
		go func() {
			defer group.Done()
			errorsByID[index] = insertCurrentAgentEmailCapacityMessage(
				st, messageID, raw, int64(len(raw)),
				"parsed", nil, 0, "body", "text/plain",
				0, 0, "retained",
			)
		}()
	}
	group.Wait()
	var acceptedID string
	var accepted, refused int
	for index, err := range errorsByID {
		switch {
		case err == nil:
			accepted++
			acceptedID = ids[index]
		case IsAgentEmailDatabaseCapacityError(err):
			refused++
		default:
			t.Fatalf("concurrent insert %d = %v", index, err)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("concurrent admission accepted=%d refused=%d", accepted, refused)
	}
	state := readAgentEmailCellStorageState(t, st)
	if state.rootRows != 1 || state.countedRows != 1 || state.retainedBytes <= 8192 {
		t.Fatalf("concurrent ledger = %+v", state)
	}

	// The reserved hard row admits the root's required delivery. A second
	// lifecycle child cannot cross the hard boundary, and its failed statement
	// must not alter the ledger.
	if err := st.ConfigureAgentEmailCellStorageLimits(
		ctx, 1<<20, 1, 2<<20, 2,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_deliveries
		  (message_id,account_id,realm_id,mailbox_id,owner_agent_id,folder)
		VALUES ($1,$2,$3,$4,$5,'inbox')`,
		acceptedID, agentEmailCapacityAccountID, agentEmailCapacityRealmID,
		agentEmailCapacityMailboxID, agentEmailCapacityOwnerID,
	); err != nil {
		t.Fatalf("reserved lifecycle delivery = %v", err)
	}
	state = readAgentEmailCellStorageState(t, st)
	if state.rootRows != 1 || state.countedRows != 2 {
		t.Fatalf("root plus reserved lifecycle row = %+v", state)
	}
	if err := st.ConfigureAgentEmailCellStorageLimits(
		ctx, state.retainedBytes-1, 1, state.retainedBytes, 2,
	); err != nil {
		t.Fatal(err)
	}
	lifecycleBaseline := state
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_deliveries
		   SET processing_state='claimed',processing_generation=1,
		       claim_id='ecl_capacity_boundary',claim_key_hash=repeat('a',64),
		       lease_expires_at=clock_timestamp()+interval '5 minutes'
		 WHERE message_id=$1`, acceptedID,
	); err != nil {
		t.Fatalf("claim delivery at hard byte boundary: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_deliveries
		   SET processing_state='completed',lease_expires_at=NULL,
		       completed_at=clock_timestamp(),complete_key_hash=repeat('b',64)
		 WHERE message_id=$1`, acceptedID,
	); err != nil {
		t.Fatalf("complete delivery at hard byte boundary: %v", err)
	}
	if after := readAgentEmailCellStorageState(t, st); after != lifecycleBaseline {
		t.Fatalf("delivery lifecycle changed ledger from %+v to %+v",
			lifecycleBaseline, after)
	}
	_, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_outbound_recipient_suppressions
		  (account_id,recipient_sha256,reason,source_send_id,provider)
		VALUES ($1,repeat('c',64),'hard_bounce','esnd_nnnnnnnnnnnnnnnn','test_provider')`,
		agentEmailCapacityAccountID,
	)
	if !IsAgentEmailDatabaseCapacityError(err) {
		t.Fatalf("hard child-row error = %v", err)
	}
	if after := readAgentEmailCellStorageState(t, st); after != state {
		t.Fatalf("failed child changed ledger from %+v to %+v", state, after)
	}

	// Deletion remains available at the boundary and transactionally releases
	// the exact charge, after which a different root can be admitted.
	if _, err := st.pool.Exec(ctx, `DELETE FROM agent_email_messages WHERE id=$1`,
		acceptedID,
	); err != nil {
		t.Fatal(err)
	}
	if after := readAgentEmailCellStorageState(t, st); after != (agentEmailCellStorageState{}) {
		t.Fatalf("delete did not release ledger: %+v", after)
	}
	if err := st.ConfigureAgentEmailCellStorageLimits(
		ctx, 1<<20, 1, 2<<20, 2,
	); err != nil {
		t.Fatal(err)
	}
	if err := insertCurrentAgentEmailCapacityMessage(
		st, "emsg_qqqqqqqqqqqqqqqq", raw, int64(len(raw)),
		"parsed", nil, 0, "body", "text/plain", 0, 0, "retained",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_deliveries
		  (message_id,account_id,realm_id,mailbox_id,owner_agent_id,folder)
		VALUES ('emsg_qqqqqqqqqqqqqqqq',$1,$2,$3,$4,'inbox')`,
		agentEmailCapacityAccountID, agentEmailCapacityRealmID,
		agentEmailCapacityMailboxID, agentEmailCapacityOwnerID,
	); err != nil {
		t.Fatal(err)
	}
	state = readAgentEmailCellStorageState(t, st)
	if state.rootRows != 1 || state.countedRows != 2 {
		t.Fatalf("root plus child ledger = %+v", state)
	}
	if err := st.ConfigureAgentEmailCellStorageLimits(
		ctx, state.retainedBytes-1, 1, state.retainedBytes, 2,
	); err != nil {
		t.Fatal(err)
	}
	beforePositiveUpdate := state
	_, err = st.pool.Exec(ctx, `
		UPDATE agent_email_messages
		   SET body_text=body_text || 'x'
		 WHERE id='emsg_qqqqqqqqqqqqqqqq'`)
	if !IsAgentEmailDatabaseCapacityError(err) {
		t.Fatalf("positive update at hard boundary = %v", err)
	}
	if after := readAgentEmailCellStorageState(t, st); after != beforePositiveUpdate {
		t.Fatalf("failed positive update changed ledger from %+v to %+v",
			beforePositiveUpdate, after)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_messages
		   SET body_text='b'
		 WHERE id='emsg_qqqqqqqqqqqqqqqq'`); err != nil {
		t.Fatalf("charge-reducing update above admission failed: %v", err)
	}
	state = readAgentEmailCellStorageState(t, st)
	if state.retainedBytes >= beforePositiveUpdate.retainedBytes ||
		state.rootRows != 1 || state.countedRows != 2 {
		t.Fatalf("charge-reducing update ledger = %+v, before %+v",
			state, beforePositiveUpdate)
	}

	// Lowering limits below existing usage does not delete data. It closes new
	// roots while the existing root and its lifecycle child remain intact.
	if err := st.ConfigureAgentEmailCellStorageLimits(ctx, 1, 1, 2, 2); err != nil {
		t.Fatal(err)
	}
	err = insertCurrentAgentEmailCapacityMessage(
		st, "emsg_rrrrrrrrrrrrrrrr", raw, int64(len(raw)),
		"parsed", nil, 0, "body", "text/plain", 0, 0, "retained",
	)
	if !IsAgentEmailDatabaseCapacityError(err) {
		t.Fatalf("byte admission error = %v", err)
	}
	if after := readAgentEmailCellStorageState(t, st); after != state {
		t.Fatalf("failed byte admission changed ledger from %+v to %+v", state, after)
	}

	// Outbound worker claims and provider settlement are metadata lifecycle,
	// not fresh retained customer content. They remain charge-neutral even when
	// the byte hard boundary equals the root's current charge.
	if _, err := st.pool.Exec(ctx, `DELETE FROM agent_email_messages
		WHERE id='emsg_qqqqqqqqqqqqqqqq'`); err != nil {
		t.Fatal(err)
	}
	if err := st.ConfigureAgentEmailCellStorageLimits(ctx, 1<<20, 1, 2<<20, 2); err != nil {
		t.Fatal(err)
	}
	insertQueuedAgentEmailCellStorageOutbound(t, st)
	state = readAgentEmailCellStorageState(t, st)
	if state.rootRows != 1 || state.countedRows != 1 {
		t.Fatalf("queued outbound ledger = %+v", state)
	}
	if err := st.ConfigureAgentEmailCellStorageLimits(
		ctx, state.retainedBytes-1, 1, state.retainedBytes, 2,
	); err != nil {
		t.Fatal(err)
	}
	outboundBaseline := state
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_outbound_messages
		   SET state='claimed',claim_generation=1,
		       claim_id='escl_aaaaaaaaaaaaaaaa',
		       lease_expires_at=clock_timestamp()+interval '5 minutes',
		       next_attempt_at=NULL,updated_at=clock_timestamp()
		 WHERE id='esnd_pppppppppppppppp'`); err != nil {
		t.Fatalf("claim outbound at hard byte boundary: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_outbound_messages
		   SET state='provider_started',provider_started_at=clock_timestamp(),
		       updated_at=clock_timestamp()
		 WHERE id='esnd_pppppppppppppppp'`); err != nil {
		t.Fatalf("start outbound provider at hard byte boundary: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_email_outbound_messages
		   SET state='accepted',provider_state='accepted',
		       provider='test_provider',provider_message_id='provider-capacity-boundary',
		       claim_id=NULL,lease_expires_at=NULL,
		       accepted_at=clock_timestamp(),updated_at=clock_timestamp()
		 WHERE id='esnd_pppppppppppppppp'`); err != nil {
		t.Fatalf("settle outbound at hard byte boundary: %v", err)
	}
	if after := readAgentEmailCellStorageState(t, st); after != outboundBaseline {
		t.Fatalf("outbound lifecycle changed ledger from %+v to %+v",
			outboundBaseline, after)
	}
}

func TestAgentEmailCellStorageOldWriterAndDownFencePostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 91)
	insertAgentEmailCapacityMigrationFixture(t, st)
	insertLegacyAgentEmailCapacityMessage(
		t, st, "emsg_ssssssssssssssss", []byte("rolling old writer"),
		"parsed", "", 0,
	)
	state := readAgentEmailCellStorageState(t, st)
	if state.rootRows != 1 || state.countedRows != 1 || state.retainedBytes <= 8192 {
		t.Fatalf("old-writer ledger = %+v", state)
	}
	_, truncateErr := st.pool.Exec(context.Background(),
		`TRUNCATE agent_email_messages CASCADE`)
	var truncatePGErr interface{ SQLState() string }
	if truncateErr == nil || !errors.As(truncateErr, &truncatePGErr) ||
		truncatePGErr.SQLState() != "55000" {
		t.Fatalf("truncate fence error = %v", truncateErr)
	}
	if after := readAgentEmailCellStorageState(t, st); after != state {
		t.Fatalf("refused truncate changed ledger from %+v to %+v", state, after)
	}
	downErr := migrationTestDown(t, dsn, true)
	var pgErr interface{ SQLState() string }
	if downErr == nil || !errors.As(downErr, &pgErr) || pgErr.SQLState() != "55000" {
		t.Fatalf("nonempty down error = %v", downErr)
	}
	assertMigrationTestVersion(t, dsn, 91)
	deleteAgentEmailCellStorageFixtureAccount(t, st)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 90)
}

func readAgentEmailCellStorageState(
	t *testing.T,
	st *Store,
) agentEmailCellStorageState {
	t.Helper()
	var state agentEmailCellStorageState
	if err := st.pool.QueryRow(context.Background(), `
		SELECT retained_bytes,root_rows,counted_rows
		  FROM agent_email_cell_storage_capacity
		 WHERE singleton=1`,
	).Scan(&state.retainedBytes, &state.rootRows, &state.countedRows); err != nil {
		t.Fatal(err)
	}
	return state
}

func deleteAgentEmailCellStorageFixtureAccount(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The product's account-deletion path removes agents and realms before the
	// account; those parent relationships intentionally do not cascade downward.
	// Agent deletion releases correspondence rows, then account deletion releases
	// account-only lifecycle rows such as recipient suppressions.
	if _, err := tx.Exec(ctx, `
		DELETE FROM agents
		 WHERE realm_id IN (SELECT id FROM realms WHERE account_id=$1)`,
		agentEmailCapacityAccountID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM realms WHERE account_id=$1`,
		agentEmailCapacityAccountID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM accounts WHERE id=$1`,
		agentEmailCapacityAccountID,
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func insertAgentEmailCellStorageOutboundFixture(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	localPart := "owner.aaaaaaaaaaaaaaaa"
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_outbound_messages
		  (id,account_id,realm_id,owner_agent_id,address_id,
		   from_address,reply_to_address,to_address,subject,body_text,
		   request_kind,thread_key,idempotency_key_hash,request_hash,
		   state,provider_state,provider,provider_message_id,
		   provider_started_at,accepted_at,delivered_at)
		VALUES
		  ('esnd_mmmmmmmmmmmmmmmm',$1,$2,$3,$4,
		   $5,$6,'recipient@example.com','subject','outbound body',
		   'direct','thread',repeat('d',64),repeat('e',64),
		   'delivered','delivered','test_provider','provider-message-1',
		   clock_timestamp(),clock_timestamp(),clock_timestamp())`,
		agentEmailCapacityAccountID, agentEmailCapacityRealmID,
		agentEmailCapacityOwnerID, agentEmailCapacityAddressID,
		localPart+"@send.witmail.net", localPart+"@witmail.net",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_outbound_provider_events
		  (account_id,provider,event_id_hash,event_request_hash,outbound_id,
		   event_class,occurred_at)
		VALUES
		  ($1,'test_provider',repeat('f',64),repeat('a',64),
		   'esnd_mmmmmmmmmmmmmmmm','delivered',clock_timestamp())`,
		agentEmailCapacityAccountID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_outbound_recipient_suppressions
		  (account_id,recipient_sha256,reason,source_send_id,provider)
		VALUES
		  ($1,repeat('b',64),'hard_bounce','esnd_mmmmmmmmmmmmmmmm','test_provider')`,
		agentEmailCapacityAccountID,
	); err != nil {
		t.Fatal(err)
	}
}

func insertQueuedAgentEmailCellStorageOutbound(t *testing.T, st *Store) {
	t.Helper()
	localPart := "owner.aaaaaaaaaaaaaaaa"
	if _, err := st.pool.Exec(context.Background(), `
		INSERT INTO agent_email_outbound_messages
		  (id,account_id,realm_id,owner_agent_id,address_id,
		   from_address,reply_to_address,to_address,subject,body_text,
		   request_kind,thread_key,idempotency_key_hash,request_hash,
		   next_attempt_at)
		VALUES
		  ('esnd_pppppppppppppppp',$1,$2,$3,$4,
		   $5,$6,'recipient@example.com','queued subject','queued body',
		   'direct','thread',repeat('c',64),repeat('d',64),clock_timestamp())`,
		agentEmailCapacityAccountID, agentEmailCapacityRealmID,
		agentEmailCapacityOwnerID, agentEmailCapacityAddressID,
		localPart+"@send.witmail.net", localPart+"@witmail.net",
	); err != nil {
		t.Fatal(err)
	}
}

func TestAgentEmailCellStorageChargeContract(t *testing.T) {
	// Customer content and immutable identities are charged exactly. Small,
	// bounded lifecycle fields are deliberately absorbed by the fixed 8 KiB so
	// claim/release/terminal transitions cannot fail for lack of one more byte.
	raw, err := os.ReadFile("migrations/0091_bound_agent_email_cell_storage.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	contracts := []struct {
		function string
		charged  []string
		fixed    []string
	}{
		{
			function: "witself_agent_email_message_cell_storage_bytes",
			charged: []string{
				"raw_mime", "body_text", "header_subject", "envelope_sender",
				"provider_message_id", "possible_duplicate_of_message_id",
			},
			fixed: []string{
				"parse_state", "parse_error", "payload_retention_state",
				"spf_result", "dkim_result", "dmarc_result", "spam_verdict",
				"sender_verification_state",
			},
		},
		{
			function: "witself_agent_email_delivery_cell_storage_bytes",
			charged:  []string{"message_id", "account_id", "mailbox_id", "owner_agent_id"},
			fixed: []string{
				"folder", "processing_state", "claim_id", "claim_key_hash",
				"complete_key_hash",
			},
		},
		{
			function: "witself_agent_email_outbound_cell_storage_bytes",
			charged: []string{
				"to_address", "subject", "body_text", "references_headers",
				"idempotency_key_hash", "request_hash",
			},
			fixed: []string{
				"state", "provider_state", "provider", "provider_message_id",
				"last_error_code", "claim_id",
			},
		},
		{
			function: "witself_agent_email_provider_event_cell_storage_bytes",
			charged:  []string{"provider", "event_id_hash", "event_request_hash", "event_class"},
		},
		{
			function: "witself_agent_email_suppression_cell_storage_bytes",
			charged:  []string{"account_id", "recipient_sha256"},
			fixed:    []string{"reason", "source_send_id", "provider"},
		},
	}
	for _, contract := range contracts {
		body := agentEmailCellStorageChargeFunctionSource(t, sql, contract.function)
		for _, field := range contract.charged {
			if !strings.Contains(body, "candidate."+field) {
				t.Errorf("%s does not charge %s", contract.function, field)
			}
		}
		for _, field := range contract.fixed {
			if strings.Contains(body, "candidate."+field) {
				t.Errorf("%s makes lifecycle field %s variable", contract.function, field)
			}
		}
	}
	for _, constraint := range []string{
		"agent_email_deliveries_claim_id_storage_bound",
		"agent_email_outbound_claim_id_storage_bound",
	} {
		if !strings.Contains(sql, constraint) {
			t.Errorf("schema-91 omits %s", constraint)
		}
	}
}

func agentEmailCellStorageChargeFunctionSource(
	t *testing.T,
	sql, name string,
) string {
	t.Helper()
	start := strings.Index(sql, "CREATE FUNCTION "+name+"(")
	if start < 0 {
		t.Fatalf("schema-91 omits charge function %s", name)
	}
	end := strings.Index(sql[start:], "-- +goose StatementEnd")
	if end < 0 {
		t.Fatalf("schema-91 charge function %s has no statement end", name)
	}
	return sql[start : start+end]
}
