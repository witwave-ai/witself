package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/testenv"
)

func deferralAccount(t *testing.T, st *Store, n int) (ProvisionedAccount, Realm, Agent) {
	t.Helper()
	ctx := context.Background()
	account, err := st.ProvisionAccount(ctx, fmt.Sprintf("deferral-%d@example.test", n), "deferral fixture", time.Hour)
	if err != nil {
		t.Fatalf("provision fixture: %T", err)
	}
	if ok, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !ok {
		t.Fatalf("activate fixture: %T", err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "deferral realm")
	if err != nil {
		t.Fatalf("create fixture realm: %T", err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "deferral agent")
	if err != nil {
		t.Fatalf("create fixture agent: %T", err)
	}
	return account, realm, agent
}

func deferralIngestInput(recipient, audience string) AgentEmailIngestInput {
	raw := []byte("From: sender@example.test\r\nTo: " + recipient + "\r\nSubject: cohort deferral\r\n\r\nhello\r\n")
	digest := sha256.Sum256(raw)
	return AgentEmailIngestInput{
		Raw: raw,
		Relay: agentemail.RelayMetadata{
			Timestamp: time.Now().Unix(), KeyID: "test-relay", Audience: audience,
			EnvelopeSender: "sender@example.test", EnvelopeRecipient: recipient,
			RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
		},
	}
}

func deferralRowCounts(t *testing.T, st *Store, accountID string) [4]int64 {
	t.Helper()
	var counts [4]int64
	err := st.pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM agent_email_messages WHERE account_id=$1),
		(SELECT count(*) FROM agent_email_deliveries WHERE account_id=$1),
		(SELECT count(*) FROM agent_email_rate_buckets WHERE account_id=$1),
		(SELECT count(*) FROM agent_email_account_rate_buckets WHERE account_id=$1)`,
		accountID).Scan(&counts[0], &counts[1], &counts[2], &counts[3])
	if err != nil {
		t.Fatalf("count fixture rows: %T", err)
	}
	return counts
}

func TestAgentEmailCohortDeferralPostgres(t *testing.T) {
	st := cohortStore(t, testenv.RequirePostgres(t))
	ctx := context.Background()
	const audience = "deferral-cell"
	const legacyDomain = "agent-mail.witwave.ai"

	// Both alias rows use this one immutable fixture, independent of subtest order.
	aliasAccount, aliasRealm, aliasAgent := deferralAccount(t, st, 14)
	aliasScope := cohortScope(aliasAccount.AccountID, "", audience)
	aliasScope.LegacyDomains = []string{legacyDomain}
	aliasAddress, err := st.EnsureAgentEmailMailbox(ctx, aliasScope, aliasAccount.AccountID, aliasRealm.ID, aliasAgent.ID, "")
	if err != nil {
		t.Fatalf("create alias fixture mailbox: %T", err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_address_domains
		  (account_id,realm_id,provisioned_agent_id,address_id,domain,local_part,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		aliasAddress.AccountID, aliasAddress.RealmID, aliasAddress.OwnerAgentID,
		aliasAddress.ID, legacyDomain, aliasAddress.LocalPart, aliasAddress.CreatedAt,
	); err != nil {
		t.Fatalf("plant legacy address-domain fixture: %T", err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_realm_aliases
		  (claim_id,account_id,realm_id,domain,realm_label,state,controller_revision)
		VALUES ($1,$2,$3,$4,$5,$6,1)`,
		"era_eeeeeeeeeeeeeeee", aliasAccount.AccountID, aliasRealm.ID, legacyDomain,
		"legacyalias", AgentEmailRealmAliasApplied,
	); err != nil {
		t.Fatalf("plant legacy alias fixture: %T", err)
	}

	cases := []struct {
		name      string
		status    string
		listed    bool
		recipient string
		disabled  bool
		noWrites  bool
		want      error
	}{
		{name: "active_omitted", status: "active", noWrites: true, want: ErrAgentEmailReceiveCohortDeferred},
		{name: "suspended_omitted", status: "suspended", noWrites: true, want: ErrAgentEmailReceiveCohortDeferred},
		{name: "closed_omitted", status: "closed", want: ErrAgentEmailPilotNotEnrolled},
		{name: "pending_omitted", status: "pending", want: ErrAgentEmailPilotNotEnrolled},
		{name: "unknown_omitted", status: "active", recipient: "unknown", want: ErrAgentEmailUnknownRecipient},
		{name: "deleted_omitted", status: "active", recipient: "deleted", want: ErrAgentEmailUnknownRecipient},
		{name: "active_listed", status: "active", listed: true},
		{name: "suspended_listed", status: "suspended", listed: true, want: ErrAccountNotActive},
		{name: "closed_listed", status: "closed", listed: true, want: ErrAccountNotActive},
		{name: "unknown_listed", status: "active", listed: true, recipient: "unknown", want: ErrAgentEmailUnknownRecipient},
		{name: "deleted_listed", status: "active", listed: true, recipient: "deleted", want: ErrAgentEmailUnknownRecipient},
		{name: "disabled_listed", status: "active", listed: true, disabled: true, want: ErrAgentEmailReceiveDisabled},
		{name: "disabled_omitted", status: "active", disabled: true, noWrites: true, want: ErrAgentEmailReceiveCohortDeferred},
		{name: "legacy_alias_listed", status: "active", listed: true, recipient: "legacy", want: ErrAgentEmailUnknownRecipient},
		{name: "legacy_alias_omitted", status: "active", recipient: "legacy", noWrites: true, want: ErrAgentEmailPilotNotEnrolled},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account, realm, agent, address := aliasAccount, aliasRealm, aliasAgent, aliasAddress
			if tc.recipient != "legacy" {
				account, realm, agent = deferralAccount(t, st, i+1)
				var mailboxErr error
				address, mailboxErr = st.EnsureAgentEmailMailbox(ctx, cohortScope(account.AccountID, "", audience), account.AccountID, realm.ID, agent.ID, "")
				if mailboxErr != nil {
					t.Fatalf("create fixture mailbox: %T", mailboxErr)
				}
			}
			listed := cohortScope(account.AccountID, "", audience)
			omitted := cohortScope("acc_zzzzzzzzzzzzzzzz", "", audience)
			if tc.recipient == "legacy" {
				listed.LegacyDomains = []string{legacyDomain}
				omitted.LegacyDomains = []string{legacyDomain}
			}
			if tc.status != "active" {
				if _, err := st.pool.Exec(ctx, `UPDATE accounts SET status=$2 WHERE id=$1`, account.AccountID, tc.status); err != nil {
					t.Fatalf("set fixture status: %T", err)
				}
			}
			if tc.disabled {
				if _, err := st.SetAgentEmailReceiveControl(ctx, listed, account.AccountID, account.OperatorID, agent.ID, AgentEmailReceiveDisabled); err != nil {
					t.Fatalf("disable fixture receive: %T", err)
				}
			}
			recipient := address.Address
			switch tc.recipient {
			case "unknown":
				recipient = "nobody." + address.RealmLabel + "@" + address.Domain
			case "deleted":
				if err := st.DeleteAgent(ctx, account.AccountID, realm.ID, agent.ID); err != nil {
					t.Fatalf("delete fixture agent: %T", err)
				}
			case "legacy":
				recipient = address.AgentSegment + ".legacyalias@" + legacyDomain
			}
			scope := omitted
			if tc.listed {
				scope = listed
			}
			var before [4]int64
			if tc.noWrites {
				before = deferralRowCounts(t, st, account.AccountID)
			}
			message, err := st.IngestAgentEmailPilot(ctx, scope, deferralIngestInput(recipient, audience))
			if !errors.Is(err, tc.want) {
				t.Fatal("ingest error did not match expected class")
			}
			if tc.want == nil && message.ID == "" {
				t.Fatal("accepted ingest did not return a message id")
			}
			if tc.want == ErrAgentEmailReceiveCohortDeferred {
				if errors.Is(err, ErrAgentEmailPilotNotEnrolled) || errors.Is(err, ErrAgentEmailUnknownRecipient) {
					t.Fatal("deferral also matched a permanent recipient refusal")
				}
			} else if errors.Is(err, ErrAgentEmailReceiveCohortDeferred) {
				t.Fatal("unchanged recipient result became a cohort deferral")
			}
			if tc.noWrites && deferralRowCounts(t, st, account.AccountID) != before {
				t.Fatal("refused ingest stored a row or debited a rate bucket")
			}
		})
	}
}

func TestAgentEmailRecipientRouteRefusedPermanently(t *testing.T) {
	route := func(kind, segment, realm string) agentEmailRecipientRoute {
		return agentEmailRecipientRoute{
			Kind: kind, Address: AgentEmailAddress{AgentSegment: segment, RealmLabel: realm},
		}
	}
	custom := func(state, disposition string, revisionMatches bool, segment string) agentEmailRecipientRoute {
		r := route(AgentEmailRecipientRouteCustomDomain, segment, "founder")
		r.CustomState, r.CustomSuspensionDisposition, r.AliasRevisionMatches = state, disposition, revisionMatches
		return r
	}
	parts := func(domain string) agentemail.AddressParts {
		return agentemail.AddressParts{Domain: domain, AgentSegment: "owner", RealmLabel: "founder"}
	}
	cases := []struct {
		name  string
		route agentEmailRecipientRoute
		parts agentemail.AddressParts
		want  bool
	}{
		{"canonical_matching", route(AgentEmailRecipientRouteCanonical, "owner", "founder"), parts("witmail.net"), false},
		{"canonical_legacy", route(AgentEmailRecipientRouteCanonical, "owner", "founder"), parts("agent-mail.witwave.ai"), false},
		{"canonical_other_agent", route(AgentEmailRecipientRouteCanonical, "other", "founder"), parts("witmail.net"), true},
		{"canonical_other_realm", route(AgentEmailRecipientRouteCanonical, "owner", "other"), parts("witmail.net"), true},
		{"alias_primary", route(AgentEmailRecipientRouteRealmAlias, "owner", "other"), parts("witmail.net"), false},
		{"alias_legacy", route(AgentEmailRecipientRouteRealmAlias, "owner", "founder"), parts("agent-mail.witwave.ai"), true},
		{"alias_other_agent", route(AgentEmailRecipientRouteRealmAlias, "other", "founder"), parts("witmail.net"), true},
		{"custom_inactive_matching", custom(AgentEmailCustomDomainRouteSuspended, AgentEmailCustomDomainSuspensionInactive, true, "owner"), parts("agents.example.com"), true},
		{"custom_inactive_stale", custom(AgentEmailCustomDomainRouteSuspended, AgentEmailCustomDomainSuspensionInactive, false, "owner"), parts("agents.example.com"), false},
		{"custom_suspended_retry", custom(AgentEmailCustomDomainRouteSuspended, AgentEmailCustomDomainSuspensionRetry, true, "owner"), parts("agents.example.com"), false},
		{"custom_applied", custom(AgentEmailCustomDomainRouteApplied, "", true, "owner"), parts("agents.example.com"), false},
		{"custom_other_agent", custom(AgentEmailCustomDomainRouteApplied, "", true, "other"), parts("agents.example.com"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentEmailRecipientRouteRefusedPermanently(tc.route, tc.parts, "witmail.net"); got != tc.want {
				t.Fatalf("permanent route refusal = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAgentEmailCohortDeferralDepartedPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	source, destination := cohortStore(t, dsn), cohortStore(t, dsn)
	ctx := context.Background()
	account, realm, agent := cohortAccount(t, source)
	listed := cohortScope(account, "", "source-cell")
	address, err := source.EnsureAgentEmailMailbox(ctx, listed, account, realm, agent, "")
	if err != nil {
		t.Fatalf("create source mailbox: %T", err)
	}
	const epoch = "deferral-forward"
	archive := cohortArchive(t, source, account, epoch)
	if _, _, err := destination.ImportAccountEvacuation(ctx, account, epoch, bytes.NewReader(archive)); err != nil {
		t.Fatalf("import departed fixture: %T", err)
	}
	if _, err := destination.CompleteAccountEvacuation(ctx, account, epoch); err != nil {
		t.Fatalf("complete departed fixture: %T", err)
	}
	if _, err := source.FinalizeAccountEvacuationSource(ctx, account, epoch); err != nil {
		t.Fatalf("finalize departed fixture: %T", err)
	}
	for _, tc := range []struct {
		name  string
		scope AgentEmailReceiveScope
	}{
		{"listed", listed},
		{"omitted", cohortScope("acc_zzzzzzzzzzzzzzzz", "", listed.Audience)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := source.IngestAgentEmailPilot(ctx, tc.scope, deferralIngestInput(address.Address, listed.Audience))
			if !errors.Is(err, ErrAgentEmailUnknownRecipient) || errors.Is(err, ErrAgentEmailReceiveCohortDeferred) {
				t.Fatal("departed recipient did not remain unknown and permanent")
			}
		})
	}
}
