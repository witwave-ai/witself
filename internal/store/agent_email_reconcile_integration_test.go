package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestReconcileAgentEmailPilotPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		"agent-email-reconcile@witwave.ai", "agent email reconcile", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "email reconcile")
	if err != nil {
		t.Fatal(err)
	}
	agents := make([]Agent, 0, 6)
	for _, name := range []string{"pilot alpha", "pilot beta", "pilot gamma", "pilot delta", "pilot epsilon", "not enrolled"} {
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, agent)
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "cell-reconcile",
		RealmIDs: map[string]bool{realm.ID: true}, AgentIDs: map[string]bool{},
	}
	for _, agent := range agents[:5] {
		scope.AgentIDs[agent.ID] = true
	}
	type reconcileResult struct {
		addresses []AgentEmailAddress
		err       error
	}
	start := make(chan struct{})
	results := make(chan reconcileResult, 2)
	for range 2 {
		go func() {
			<-start
			addresses, err := st.ReconcileAgentEmailPilot(ctx, scope)
			results <- reconcileResult{addresses: addresses, err: err}
		}()
	}
	close(start)
	firstResult, secondResult := <-results, <-results
	if firstResult.err != nil || len(firstResult.addresses) != 5 ||
		secondResult.err != nil || len(secondResult.addresses) != 5 {
		t.Fatalf("concurrent reconciliation = %#v / %#v", firstResult, secondResult)
	}
	first := firstResult.addresses
	byOwner := make(map[string]AgentEmailAddress, len(first))
	for _, address := range first {
		if !scope.AgentIDs[address.OwnerAgentID] || address.RealmID != realm.ID ||
			address.AccountID != provisioned.AccountID || address.Domain != scope.Domain {
			t.Fatalf("reconciled out-of-scope address = %#v", address)
		}
		byOwner[address.OwnerAgentID] = address
	}
	if len(byOwner) != 5 || byOwner[agents[5].ID].ID != "" {
		t.Fatalf("reconciled owners = %#v", byOwner)
	}
	second, err := st.ReconcileAgentEmailPilot(ctx, scope)
	if err != nil || len(second) != len(first) {
		t.Fatalf("idempotent reconciliation = %#v / %v", second, err)
	}
	for _, address := range second {
		if prior := byOwner[address.OwnerAgentID]; prior.ID != address.ID || prior.MailboxID != address.MailboxID {
			t.Fatalf("reconciliation changed identity: prior %#v next %#v", prior, address)
		}
	}
	var total, unenrolled int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*),count(*) FILTER (WHERE owner_agent_id=$1)
		FROM agent_email_mailboxes
		WHERE account_id=$2 AND realm_id=$3`, agents[5].ID, provisioned.AccountID, realm.ID).
		Scan(&total, &unenrolled); err != nil {
		t.Fatal(err)
	}
	if total != 5 || unenrolled != 0 {
		t.Fatalf("mailbox counts = total %d unenrolled %d", total, unenrolled)
	}

	missingScope := scope
	missingScope.AgentIDs = make(map[string]bool, 5)
	for _, agent := range agents[:4] {
		missingScope.AgentIDs[agent.ID] = true
	}
	missingScope.AgentIDs["agent_aaaaaaaaaaaaaaaa"] = true
	if _, err := st.ReconcileAgentEmailPilot(ctx, missingScope); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("missing agent reconciliation error = %v", err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_email_mailboxes
		WHERE account_id=$1 AND realm_id=$2`, provisioned.AccountID, realm.ID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 5 {
		t.Fatalf("failed preflight mutated mailboxes: %d", total)
	}

	collisionRealm, err := st.CreateRealm(ctx, provisioned.AccountID, "email collision")
	if err != nil {
		t.Fatal(err)
	}
	collisionAgents := make([]Agent, 0, 5)
	for _, name := range []string{"Collision Agent", "collision.agent", "collision helper one", "collision helper two", "collision helper three"} {
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, collisionRealm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		collisionAgents = append(collisionAgents, agent)
	}
	collisionScope := AgentEmailPilotScope{
		Enabled: true, Domain: scope.Domain, Audience: scope.Audience,
		RealmIDs: map[string]bool{collisionRealm.ID: true}, AgentIDs: map[string]bool{},
	}
	for _, agent := range collisionAgents {
		collisionScope.AgentIDs[agent.ID] = true
	}
	if _, err := st.ReconcileAgentEmailPilot(ctx, collisionScope); !errors.Is(err, ErrAgentEmailAddressConflict) {
		t.Fatalf("collision reconciliation error = %v", err)
	}
	var outOfScope int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM agent_email_mailboxes
		WHERE realm_id=$1 AND NOT (owner_agent_id=ANY($2::text[]))`,
		collisionRealm.ID, enabledAgentEmailPilotIDs(collisionScope.AgentIDs)).Scan(&outOfScope); err != nil {
		t.Fatal(err)
	}
	if outOfScope != 0 {
		t.Fatalf("collision reconciliation provisioned %d out-of-scope mailboxes", outOfScope)
	}
}
