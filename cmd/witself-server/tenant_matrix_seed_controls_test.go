package main

import (
	"context"
	"testing"

	"github.com/witwave-ai/witself/internal/store"
)

// The email control integration tests use CreateRealm/CreateAgent to prepare
// independent controls. These dedicated objects keep PATCH controls from
// changing the mailbox and realm needed by subsequent message/email probes.
func tenantSeedEmailControls(t *testing.T, rt *tenantRuntime, side tenantSide) {
	t.Helper()
	v := rt.f.bySide[side]
	ctx := context.Background()
	for _, key := range []string{"realm_email_receive", "realm_email_send"} {
		name := string(side) + "-" + tenantDomainCanary(v, side, key+".name")
		realm, err := rt.st.CreateRealm(ctx, v["account"], name)
		tenantSeedObjectError(t, "CreateRealm email control", err)
		v[key], v["seed."+key+".name"] = realm.ID, name
	}
	for _, key := range []string{"agent_email_receive", "agent_email_send"} {
		name := string(side) + "-" + tenantDomainCanary(v, side, key+".name")
		agent, err := rt.st.CreateAgent(ctx, v["account"], v["realm"], name)
		tenantSeedObjectError(t, "CreateAgent email control", err)
		v[key], v["seed."+key+".name"] = agent.ID, name
		mailbox, err := rt.st.EnsureAgentEmailMailbox(ctx, store.AgentEmailReceiveScope{
			Enabled: true, Mode: store.AgentEmailReceiveModeProduction, Domain: "witmail.net", Audience: "tenant-matrix",
			AccountIDs: map[string]bool{v["account"]: true},
		}, v["account"], v["realm"], agent.ID, "")
		tenantSeedObjectError(t, "EnsureAgentEmailMailbox control", err)
		v["seed."+key+".email"], v["seed."+key+".mailbox"] = mailbox.Address, mailbox.MailboxID
	}
}
