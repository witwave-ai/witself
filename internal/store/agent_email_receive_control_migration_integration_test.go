package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAgentEmailReceiveControlSchema59BackfillPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 59)

	provisioned, err := st.ProvisionAccount(ctx,
		"agent-email-schema-59@witwave.ai", "agent email schema 59", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "email schema 59")
	if err != nil {
		t.Fatal(err)
	}
	enabledAgent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "legacy enabled")
	if err != nil {
		t.Fatal(err)
	}
	disabledAgent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "legacy disabled")
	if err != nil {
		t.Fatal(err)
	}
	enrolled := map[string]bool{enabledAgent.ID: true, disabledAgent.ID: true}
	for _, name := range []string{"legacy helper one", "legacy helper two", "legacy helper three"} {
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		enrolled[agent.ID] = true
	}
	realmLabel := strings.TrimPrefix(realm.ID, "realm_")
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_addresses
		  (id,account_id,realm_id,provisioned_agent_id,domain,agent_segment,
		   realm_label,local_part,provisioning_kind)
		VALUES
		  ('eaddr_aaaaaaaaaaaaaaaa',$1,$2,$3,'agent-mail.witwave.ai',
		   'legacy-enabled',$5,'legacy-enabled.' || $5,'derived'),
		  ('eaddr_bbbbbbbbbbbbbbbb',$1,$2,$4,'agent-mail.witwave.ai',
		   'legacy-disabled',$5,'legacy-disabled.' || $5,'derived');
		`, provisioned.AccountID, realm.ID, enabledAgent.ID, disabledAgent.ID, realmLabel); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_mailboxes
		  (id,account_id,realm_id,owner_agent_id,address_id,receive_state,
		   row_version,disabled_at)
		VALUES
		  ('emb_aaaaaaaaaaaaaaaa',$1,$2,$3,'eaddr_aaaaaaaaaaaaaaaa','enabled',7,NULL),
		  ('emb_bbbbbbbbbbbbbbbb',$1,$2,$4,'eaddr_bbbbbbbbbbbbbbbb','disabled',9,clock_timestamp())`,
		provisioned.AccountID, realm.ID, enabledAgent.ID, disabledAgent.ID); err != nil {
		t.Fatal(err)
	}

	migrationTestUpTo(t, dsn, 60)
	assertMigrationTestVersion(t, dsn, 60)
	var realmRows, realmVersion int64
	var realmState string
	var realmDisabledAt *time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*),min(receive_state),min(row_version),min(disabled_at)
		FROM agent_email_realm_receive_controls
		WHERE account_id=$1 AND realm_id=$2`, provisioned.AccountID, realm.ID).
		Scan(&realmRows, &realmState, &realmVersion, &realmDisabledAt); err != nil {
		t.Fatal(err)
	}
	if realmRows != 1 || realmState != AgentEmailReceiveEnabled ||
		realmVersion != 1 || realmDisabledAt != nil {
		t.Fatalf("schema-60 realm backfill = rows %d state %q version %d disabled_at %v",
			realmRows, realmState, realmVersion, realmDisabledAt)
	}

	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "cell-schema-59",
		RealmIDs: map[string]bool{realm.ID: true},
		AgentIDs: enrolled,
	}
	for _, testCase := range []struct {
		name           string
		agent          Agent
		wantAgentState string
		wantEffective  string
		wantRowVersion int64
	}{
		{
			name: "enabled mailbox", agent: enabledAgent,
			wantAgentState: AgentEmailReceiveEnabled,
			wantEffective:  AgentEmailReceiveEnabled, wantRowVersion: 7,
		},
		{
			name: "disabled mailbox", agent: disabledAgent,
			wantAgentState: AgentEmailReceiveDisabled,
			wantEffective:  AgentEmailReceiveDisabled, wantRowVersion: 9,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			address, err := st.GetAgentEmailAddress(ctx, scope, Principal{
				Kind: PrincipalAgent, ID: testCase.agent.ID,
				AccountID: provisioned.AccountID, RealmID: realm.ID,
				AccountStatus: "active", AccessProfile: AccessProfileFull,
			})
			if err != nil {
				t.Fatal(err)
			}
			if address.AgentReceiveState != testCase.wantAgentState ||
				address.RealmReceiveState != AgentEmailReceiveEnabled ||
				address.ReceiveState != testCase.wantEffective ||
				address.RowVersion != testCase.wantRowVersion {
				t.Fatalf("backfilled address = %#v", address)
			}
		})
	}
}
