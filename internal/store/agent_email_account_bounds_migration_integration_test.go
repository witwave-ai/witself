package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestAgentEmailAccountBoundsMigrationPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 89)
	account, err := st.ProvisionAccount(
		ctx, "agent-email-bounds-migration@witwave.ai",
		"Agent Email Bounds Migration", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if active, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !active {
		t.Fatalf("activate = %t / %v", active, err)
	}
	firstRealm, err := st.CreateRealm(ctx, account.AccountID, "bounds one")
	if err != nil {
		t.Fatal(err)
	}
	secondRealm, err := st.CreateRealm(ctx, account.AccountID, "bounds two")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_rate_buckets
		  (account_id,realm_id,dimension,scope,scope_id,
		   theoretical_arrival_nanoseconds)
		VALUES ($1,$2,'email_received','realm',$2,1)`,
		account.AccountID, firstRealm.ID,
	); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 90)

	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_account_rate_buckets
		  (account_id,dimension,theoretical_arrival_nanoseconds)
		VALUES ($1,'email_received',1)`,
		account.AccountID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_rate_buckets
		  (account_id,realm_id,dimension,scope,scope_id,
		   theoretical_arrival_nanoseconds)
		VALUES ($1,'realm_missing','email_received','realm','realm_missing',1)`,
		account.AccountID,
	); err == nil {
		t.Fatal("schema 90 accepted realm debt without a realm")
	} else if _, ok := err.(*pgconn.PgError); !ok {
		t.Fatalf("invalid realm debt error = %T %v", err, err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM realms WHERE account_id=$1 AND id=$2`,
		account.AccountID, firstRealm.ID,
	); err != nil {
		t.Fatal(err)
	}
	var removedRealmDebt int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_rate_buckets
		 WHERE account_id=$1 AND realm_id=$2`, account.AccountID, firstRealm.ID,
	).Scan(&removedRealmDebt); err != nil {
		t.Fatal(err)
	}
	if removedRealmDebt != 0 {
		t.Fatalf("deleted realm retained %d inbound rate buckets", removedRealmDebt)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_rate_buckets
		  (account_id,realm_id,dimension,scope,scope_id,
		   theoretical_arrival_nanoseconds)
		VALUES ($1,$2,'email_received','realm',$2,1)`,
		account.AccountID, secondRealm.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_outbound_rate_buckets
		  (account_id,realm_id,lane,scope,scope_id,
		   theoretical_arrival_microseconds)
		VALUES ($1,'','admission_daily','recipient',$2,1)`,
		account.AccountID,
		agentEmailOutboundRecipientScopeID(account.AccountID, "person@example.com"),
	); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct {
		lane, scope, realmID, scopeID string
	}{
		{lane: "admission_daily", scope: "agent", realmID: secondRealm.ID, scopeID: "agent_aaaaaaaaaaaaaaaa"},
		{lane: "admission", scope: "recipient", scopeID: agentEmailOutboundRecipientScopeID(account.AccountID, "other@example.com")},
	} {
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_email_outbound_rate_buckets
			  (account_id,realm_id,lane,scope,scope_id,
			   theoretical_arrival_microseconds)
			VALUES ($1,$2,$3,$4,$5,1)`,
			account.AccountID, invalid.realmID, invalid.lane, invalid.scope, invalid.scopeID,
		); err == nil {
			t.Fatalf("schema 90 accepted invalid outbound lane/scope %s/%s",
				invalid.lane, invalid.scope)
		}
	}

	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 89)
	var inbound, outbound int64
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM agent_email_rate_buckets WHERE account_id=$1),
		  (SELECT count(*) FROM agent_email_outbound_rate_buckets WHERE account_id=$1)`,
		account.AccountID,
	).Scan(&inbound, &outbound); err != nil {
		t.Fatal(err)
	}
	if inbound != 1 || outbound != 0 {
		t.Fatalf("rate debt after downgrade: inbound=%d outbound=%d, want 1/0",
			inbound, outbound)
	}
}
