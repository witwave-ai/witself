package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAgentEmailOutboundMigrationDowngradePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}

	t.Run("operational rate buckets alone can downgrade", func(t *testing.T) {
		ctx := context.Background()
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 89)
		account, err := st.ProvisionAccount(
			ctx, "outbound-down-bucket@witwave.ai",
			"Outbound Down Bucket", time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_email_outbound_rate_buckets
			  (account_id,realm_id,lane,scope,scope_id,
			   theoretical_arrival_microseconds)
			VALUES ($1,'','admission','account',$1,1)`,
			account.AccountID,
		); err != nil {
			t.Fatal(err)
		}

		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 88)
		assertMigrationTestTable(
			t, st, "agent_email_outbound_rate_buckets", false,
		)
	})

	for _, test := range []struct {
		name       string
		checkTable string
	}{
		{name: "realm send control", checkTable: "agent_email_realm_send_controls"},
		{name: "agent send control", checkTable: "agent_email_send_controls"},
		{name: "queued outbound message", checkTable: "agent_email_outbound_messages"},
		{name: "provider event graph", checkTable: "agent_email_outbound_provider_events"},
		{name: "recipient suppression", checkTable: "agent_email_outbound_recipient_suppressions"},
	} {
		test := test
		t.Run(test.name+" refuses downgrade", func(t *testing.T) {
			ctx := context.Background()
			st, dsn := newMigrationTestStore(t, baseDSN)
			migrationTestUpTo(t, dsn, 89)
			fixture := newAgentEmailRetentionAccountFixture(
				ctx, t, st, "down-"+strings.ReplaceAll(test.name, " ", "-"),
			)
			localPart, _, ok := strings.Cut(fixture.address.Address, "@")
			if !ok {
				t.Fatalf("fixture address = %q", fixture.address.Address)
			}

			switch test.name {
			case "realm send control":
				if _, err := st.pool.Exec(ctx, `
					INSERT INTO agent_email_realm_send_controls
					  (account_id,realm_id)
					VALUES ($1,$2)`,
					fixture.accountID, fixture.realmID,
				); err != nil {
					t.Fatal(err)
				}
			case "agent send control":
				if _, err := st.pool.Exec(ctx, `
					INSERT INTO agent_email_send_controls
					  (account_id,realm_id,owner_agent_id)
					VALUES ($1,$2,$3)`,
					fixture.accountID, fixture.realmID, fixture.owner.ID,
				); err != nil {
					t.Fatal(err)
				}
			case "queued outbound message", "provider event graph":
				if _, err := st.pool.Exec(ctx, `
					INSERT INTO agent_email_outbound_messages
					  (id,account_id,realm_id,owner_agent_id,address_id,
					   from_address,reply_to_address,to_address,subject,body_text,
					   request_kind,thread_key,idempotency_key_hash,request_hash,
					   next_attempt_at)
					VALUES
					  ('esnd_aaaaaaaaaaaaaaaa',$1,$2,$3,$4,$5,$6,
					   'recipient@example.com','downgrade guard','durable body',
					   'direct','esnd_aaaaaaaaaaaaaaaa',$7,$8,clock_timestamp())`,
					fixture.accountID, fixture.realmID, fixture.owner.ID,
					fixture.address.ID, localPart+"@send.witmail.net",
					localPart+"@witmail.net", strings.Repeat("a", 64),
					strings.Repeat("b", 64),
				); err != nil {
					t.Fatal(err)
				}
				if test.name == "provider event graph" {
					if _, err := st.pool.Exec(ctx, `
						INSERT INTO agent_email_outbound_provider_events
						  (account_id,provider,event_id_hash,event_request_hash,
						   outbound_id,event_class,occurred_at)
						VALUES ($1,'cloudflare',$2,$3,'esnd_aaaaaaaaaaaaaaaa',
						        'delivered',clock_timestamp())`,
						fixture.accountID, strings.Repeat("c", 64),
						strings.Repeat("d", 64),
					); err != nil {
						t.Fatal(err)
					}
				}
			case "recipient suppression":
				if _, err := st.pool.Exec(ctx, `
					INSERT INTO agent_email_outbound_recipient_suppressions
					  (account_id,recipient_sha256,reason,source_send_id,provider)
					VALUES ($1,$2,'hard_bounce','esnd_aaaaaaaaaaaaaaaa','cloudflare')`,
					fixture.accountID, strings.Repeat("e", 64),
				); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unhandled fixture %q", test.name)
			}

			downErr := migrationTestDown(t, dsn, true)
			if downErr == nil || !strings.Contains(
				downErr.Error(),
				"cannot downgrade schema 0089 while durable outbound agent-email state exists",
			) {
				t.Fatalf("schema-89 durable-state downgrade error = %v", downErr)
			}
			assertMigrationTestVersion(t, dsn, 89)
			var retained bool
			if err := st.pool.QueryRow(ctx,
				"SELECT EXISTS (SELECT 1 FROM "+test.checkTable+" WHERE account_id=$1)",
				fixture.accountID,
			).Scan(&retained); err != nil {
				t.Fatal(err)
			}
			if !retained {
				t.Fatalf("%s state was lost after refused downgrade", test.checkTable)
			}
		})
	}
}
