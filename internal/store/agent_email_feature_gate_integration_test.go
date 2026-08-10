package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/plans"
)

func TestAgentEmailFeatureGateCoversOwnerAndIngressOperationsPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(
		ctx, "agent-email-feature-gate@witwave.ai", "agent email feature gate", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "email feature gate")
	if err != nil {
		t.Fatal(err)
	}
	agents := make([]Agent, 0, 5)
	enrolled := make(map[string]bool, 5)
	for _, name := range []string{"owner", "two", "three", "four", "five"} {
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, agent)
		enrolled[agent.ID] = true
	}
	owner := Principal{
		Kind: PrincipalAgent, ID: agents[0].ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: agents[0].Name, AccountStatus: "active",
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "cell-feature-gate",
		RealmIDs: map[string]bool{realm.ID: true}, AgentIDs: enrolled,
		RetryCanaryAgentID: owner.ID,
	}
	address, err := st.EnsureAgentEmailMailbox(
		ctx, scope, provisioned.AccountID, realm.ID, owner.ID, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	applyPlan := func(revision int64, policies map[string]int64, features []string) {
		t.Helper()
		hash, err := plans.SnapshotHash("test", map[string]int64{}, policies, features)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetAccountPlan(
			ctx, provisioned.AccountID, revision, hash, "test",
			map[string]int64{}, policies, features,
		); err != nil {
			t.Fatal(err)
		}
	}
	applyPlan(1, map[string]int64{TranscriptRetentionDaysPolicy: 30}, []string{"memory"})
	if err := st.RequireAgentEmailReceiveEnabled(ctx, owner); err != nil {
		t.Fatalf("legacy entitlement precheck was gated: %v", err)
	}
	if checkpoint, err := st.GetSelfAgentEmailCheckpoint(ctx, scope, owner); err != nil || !checkpoint.Enabled {
		t.Fatalf("legacy checkpoint = %+v / %v", checkpoint, err)
	}

	emailPolicies := map[string]int64{
		plans.AgentEmailRetentionDaysPolicy:      30,
		plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
	}
	applyPlan(2, emailPolicies, []string{"memory", "facts"})
	assertDisabled := func(name string, call func() error) {
		t.Helper()
		err := call()
		var featureErr *FeatureNotEnabledError
		if !errors.Is(err, ErrFeatureNotEnabled) || !errors.As(err, &featureErr) ||
			featureErr.Feature != plans.AgentEmailReceiveFeature {
			t.Fatalf("%s error = %v, want agent-email FeatureNotEnabledError", name, err)
		}
	}
	assertDisabled("precheck", func() error {
		return st.RequireAgentEmailReceiveEnabled(ctx, owner)
	})

	raw := []byte(strings.Join([]string{
		"From: sender@example.com",
		"To: " + address.Address,
		"Subject: must be discarded",
		"",
		"never persist",
	}, "\r\n"))
	digest := sha256.Sum256(raw)
	ingestInput := AgentEmailIngestInput{
		Relay: agentemail.RelayMetadata{
			Timestamp: time.Now().Unix(), KeyID: "pilot-key", Audience: scope.Audience,
			EnvelopeSender: "sender@example.com", EnvelopeRecipient: address.Address,
			RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
		},
		Raw: raw,
	}
	messageID := "emsg_aaaaaaaaaaaaaaaa"
	claimID := "ecl_aaaaaaaaaaaaaaaa"
	challenge := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	disabledOperations := []struct {
		name string
		call func() error
	}{
		{"address", func() error {
			_, err := st.GetAgentEmailAddress(ctx, scope, owner)
			return err
		}},
		{"list", func() error {
			_, err := st.ListAgentEmails(ctx, scope, owner, AgentEmailFilter{})
			return err
		}},
		{"read", func() error {
			_, err := st.ReadAgentEmail(ctx, scope, owner, messageID)
			return err
		}},
		{"ack", func() error {
			_, err := st.AckAgentEmail(ctx, scope, owner, messageID)
			return err
		}},
		{"code consumed", func() error {
			_, err := st.MarkAgentEmailCodeConsumed(ctx, scope, owner, messageID)
			return err
		}},
		{"claim", func() error {
			_, err := st.ClaimAgentEmail(ctx, scope, owner, messageID, ClaimAgentEmailInput{
				IdempotencyKey: "disabled-claim",
			})
			return err
		}},
		{"renew", func() error {
			_, err := st.RenewAgentEmailClaim(ctx, scope, owner, messageID, RenewAgentEmailClaimInput{
				ClaimID: claimID, Generation: 1,
			})
			return err
		}},
		{"release", func() error {
			_, err := st.ReleaseAgentEmailClaim(ctx, scope, owner, messageID, ReleaseAgentEmailClaimInput{
				ClaimID: claimID, Generation: 1,
			})
			return err
		}},
		{"complete", func() error {
			_, err := st.CompleteAgentEmail(ctx, scope, owner, messageID, CompleteAgentEmailInput{
				ClaimID: claimID, Generation: 1, IdempotencyKey: "disabled-complete",
			})
			return err
		}},
		{"retry canary arm", func() error {
			_, err := st.ArmAgentEmailRetryCanary(ctx, scope, owner, challenge)
			return err
		}},
		{"retry canary status", func() error {
			_, err := st.GetAgentEmailRetryCanaryStatus(ctx, scope, owner, challenge)
			return err
		}},
		{"ingest", func() error {
			_, err := st.IngestAgentEmailPilot(ctx, scope, ingestInput)
			return err
		}},
	}
	for _, operation := range disabledOperations {
		assertDisabled(operation.name, operation.call)
	}
	var messages, deliveries, canaries int
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM agent_email_messages WHERE account_id=$1),
		  (SELECT count(*) FROM agent_email_deliveries WHERE account_id=$1),
		  (SELECT count(*) FROM agent_email_retry_canary_arms WHERE account_id=$1)`,
		provisioned.AccountID,
	).Scan(&messages, &deliveries, &canaries); err != nil {
		t.Fatal(err)
	}
	if messages != 0 || deliveries != 0 || canaries != 0 {
		t.Fatalf("disabled email mutated messages/deliveries/canaries = %d/%d/%d",
			messages, deliveries, canaries)
	}
	checkpoint, err := st.GetSelfAgentEmailCheckpoint(ctx, scope, owner)
	if err != nil || checkpoint.Enabled || checkpoint.Pending || checkpoint.MailboxPending {
		t.Fatalf("disabled checkpoint = %+v / %v", checkpoint, err)
	}

	// Operator receive controls stay configurable while the account entitlement
	// is disabled; they take effect if receipt is enabled again.
	control, err := st.SetAgentEmailReceiveControl(
		ctx, scope, provisioned.AccountID, "operator-test", owner.ID,
		AgentEmailReceiveDisabled,
	)
	if err != nil || control.AgentReceiveState != AgentEmailReceiveDisabled {
		t.Fatalf("disabled-plan operator control = %+v / %v", control, err)
	}
	if _, err := st.SetAgentEmailReceiveControl(
		ctx, scope, provisioned.AccountID, "operator-test", owner.ID,
		AgentEmailReceiveEnabled,
	); err != nil {
		t.Fatalf("re-enable operator control under disabled plan: %v", err)
	}

	applyPlan(3, emailPolicies, []string{
		"memory", "facts", plans.AgentEmailReceiveFeature,
	})
	if err := st.RequireAgentEmailReceiveEnabled(ctx, owner); err != nil {
		t.Fatalf("enabled entitlement precheck = %v", err)
	}
	if checkpoint, err := st.GetSelfAgentEmailCheckpoint(ctx, scope, owner); err != nil || !checkpoint.Enabled {
		t.Fatalf("enabled checkpoint = %+v / %v", checkpoint, err)
	}
	if message, err := st.IngestAgentEmailPilot(ctx, scope, ingestInput); err != nil || message.ID == "" {
		t.Fatalf("enabled ingest = %+v / %v", message, err)
	}

	// Production receive replaces only the fixed realm/agent allowlist. The
	// exact account cohort is independent, and the same local route, plan,
	// mailbox, realm, and agent checks remain authoritative.
	legacyReserved, err := st.CreateAgent(
		ctx, provisioned.AccountID, realm.ID, "postmaster",
	)
	if err != nil {
		t.Fatal(err)
	}
	agents = append(agents, legacyReserved)
	var collisionAgents []Agent
	for _, name := range []string{"collision-one", "collision-two"} {
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		collisionAgents = append(collisionAgents, agent)
		agents = append(agents, agent)
	}
	productionScope := AgentEmailReceiveScope{
		Enabled: true, Mode: AgentEmailReceiveModeProduction,
		Domain: scope.Domain, Audience: scope.Audience,
		AccountIDs:         map[string]bool{provisioned.AccountID: true},
		RetryCanaryAgentID: owner.ID,
	}
	retiredAgent, retiredAddress, err := st.CreateAgentWithEmailMailbox(
		ctx, productionScope, provisioned.AccountID, realm.ID,
		"retired-reservation", "retired-segment",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAgent(
		ctx, provisioned.AccountID, realm.ID, retiredAgent.ID,
	); err != nil {
		t.Fatal(err)
	}
	replacement, err := st.CreateAgent(
		ctx, provisioned.AccountID, realm.ID, "reservation-replacement",
	)
	if err != nil {
		t.Fatal(err)
	}
	agents = append(agents, replacement)
	preflight, err := st.PreflightAgentEmailProductionCohort(ctx, productionScope)
	if err != nil || preflight.AccountCount != 1 ||
		preflight.LiveAgentCount != int64(len(agents)) ||
		preflight.ReadyMailboxCount != 1 ||
		preflight.MissingMailboxCount != int64(len(agents)-1) ||
		!preflight.RetryCanaryReady {
		t.Fatalf("production read-only preflight = %+v / %v", preflight, err)
	}
	if _, err := st.ListAgentEmailProductionCanaryAgents(
		ctx, productionScope,
	); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("production canary with missing mailboxes error = %v", err)
	}
	if reconciled, err := st.ReconcileAgentEmailProductionCohortWithOverrides(
		ctx, productionScope,
		map[string]string{
			collisionAgents[0].ID: "shared-segment",
			collisionAgents[1].ID: "shared-segment",
		},
	); reconciled != 0 || !errors.Is(err, ErrAgentEmailConflict) {
		t.Fatalf("duplicate production override preflight = %d / %v", reconciled, err)
	}
	if reconciled, err := st.ReconcileAgentEmailProductionCohortWithOverrides(
		ctx, productionScope,
		map[string]string{legacyReserved.ID: address.AgentSegment},
	); reconciled != 0 || !errors.Is(err, ErrAgentEmailConflict) {
		t.Fatalf("reserved production override preflight = %d / %v", reconciled, err)
	}
	if reconciled, err := st.ReconcileAgentEmailProductionCohortWithOverrides(
		ctx, productionScope,
		map[string]string{replacement.ID: retiredAddress.AgentSegment},
	); reconciled != 0 || !errors.Is(err, ErrAgentEmailConflict) {
		t.Fatalf("tombstoned production override preflight = %d / %v", reconciled, err)
	}
	unchanged, err := st.PreflightAgentEmailProductionCohort(ctx, productionScope)
	if err != nil || unchanged.ReadyMailboxCount != 1 ||
		unchanged.MissingMailboxCount != int64(len(agents)-1) {
		t.Fatalf("override preflight wrote before rejection = %+v / %v", unchanged, err)
	}
	if _, err := st.ReconcileAgentEmailProductionCohort(
		ctx, productionScope,
	); !errors.Is(err, ErrAgentEmailInputInvalid) {
		t.Fatalf("production backfill without required override error = %v", err)
	} else {
		var exception *AgentEmailProductionBackfillError
		if !errors.As(err, &exception) || exception.AgentID != legacyReserved.ID ||
			exception.RealmID != realm.ID ||
			exception.ReasonCode != "agent_segment_requires_override" ||
			strings.Contains(err.Error(), legacyReserved.ID) {
			t.Fatalf("private production backfill exception = %#v / %v", exception, err)
		}
	}
	reconciled, err := st.ReconcileAgentEmailProductionCohortWithOverrides(
		ctx, productionScope,
		map[string]string{legacyReserved.ID: "legacy-postmaster"},
	)
	if err != nil || reconciled != int64(len(agents)) {
		t.Fatalf("production cohort reconciliation = %d / %v", reconciled, err)
	}
	preflight, err = st.PreflightAgentEmailProductionCohort(ctx, productionScope)
	if err != nil || preflight.ReadyMailboxCount != int64(len(agents)) ||
		preflight.MissingMailboxCount != 0 {
		t.Fatalf("production reconciled preflight = %+v / %v", preflight, err)
	}
	for _, agent := range agents {
		if _, err := st.GetAgentEmailAddress(ctx, productionScope, Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AccountStatus: "active",
		}); err != nil {
			t.Fatalf("production mailbox for %s: %v", agent.ID, err)
		}
	}
	reconciled, err = st.ReconcileAgentEmailProductionCohortWithOverrides(
		ctx, productionScope,
		map[string]string{legacyReserved.ID: "legacy-postmaster"},
	)
	if err != nil || reconciled != int64(len(agents)) {
		t.Fatalf("idempotent production override replay = %d / %v", reconciled, err)
	}
	if err := st.reconcileProductionAgentEmailMailbox(
		ctx, productionScope, scope.Domain, provisioned.AccountID, "active",
		realm.ID, legacyReserved.ID, "different-postmaster",
	); !errors.Is(err, ErrAgentEmailConflict) {
		t.Fatalf("mismatched production override convergence error = %v", err)
	}
	canaryAgents, err := st.ListAgentEmailProductionCanaryAgents(ctx, productionScope)
	if err != nil || len(canaryAgents) != len(agents) {
		t.Fatalf("production canary agents = %+v / %v", canaryAgents, err)
	}
	canaryOwnerFound := false
	for i, candidate := range canaryAgents {
		if candidate.AgentID == owner.ID {
			canaryOwnerFound = true
		}
		if candidate.RealmID != realm.ID || !strings.HasSuffix(
			candidate.Address, "."+strings.TrimPrefix(realm.ID, "realm_")+"@"+productionScope.Domain,
		) {
			t.Fatalf("production canary candidate = %+v", candidate)
		}
		if i > 0 && canaryAgents[i-1].Address >= candidate.Address {
			t.Fatalf("production canary is not strictly address-sorted: %+v", canaryAgents)
		}
	}
	if !canaryOwnerFound {
		t.Fatalf("production canary omitted configured retry agent %s", owner.ID)
	}
	created, createdAddress, err := st.CreateAgentWithEmailMailbox(
		ctx, productionScope, provisioned.AccountID, realm.ID, "production-new", "",
	)
	if err != nil || created.ID == "" || createdAddress.OwnerAgentID != created.ID ||
		createdAddress.AccountID != provisioned.AccountID ||
		createdAddress.RealmID != realm.ID ||
		!agentEmailAddressHasPrimaryDomain(createdAddress, productionScope.Domain) {
		t.Fatalf("atomic production agent/mailbox = %+v / %+v / %v", created, createdAddress, err)
	}
	preflight, err = st.PreflightAgentEmailProductionCohort(ctx, productionScope)
	if err != nil || preflight.LiveAgentCount != int64(len(agents)+1) ||
		preflight.ReadyMailboxCount != int64(len(agents)+1) ||
		preflight.MissingMailboxCount != 0 {
		t.Fatalf("atomic mailbox preflight = %+v / %v", preflight, err)
	}
	if _, _, err := st.CreateAgentWithEmailMailbox(
		ctx, productionScope, provisioned.AccountID, realm.ID, "abuse", "",
	); !errors.Is(err, ErrAgentEmailInputInvalid) {
		t.Fatalf("reserved production agent segment error = %v", err)
	}
	for _, testCase := range []struct {
		name    string
		segment string
	}{
		{name: "noncanonical-upper", segment: "Mail-Bot"},
		{name: "noncanonical-space", segment: " mail-bot "},
		{name: "noncanonical-blank", segment: "   "},
	} {
		if _, _, err := st.CreateAgentWithEmailMailbox(
			ctx, productionScope, provisioned.AccountID, realm.ID,
			testCase.name, testCase.segment,
		); !errors.Is(err, ErrAgentEmailInputInvalid) {
			t.Fatalf("noncanonical production agent segment %q error = %v",
				testCase.segment, err)
		}
	}
	currentAgents, err := st.ListAgents(ctx, provisioned.AccountID, realm.ID)
	if err != nil || len(currentAgents) != len(agents)+1 {
		t.Fatalf("failed atomic mailbox create leaked an agent = %d / %v", len(currentAgents), err)
	}
	overridden, overriddenAddress, err := st.CreateAgentWithEmailMailbox(
		ctx, productionScope, provisioned.AccountID, realm.ID,
		"abuse", "support-agent",
	)
	if err != nil || overridden.ID == "" ||
		overriddenAddress.ProvisioningKind != "operator_override" ||
		overriddenAddress.AgentSegment != "support-agent" {
		t.Fatalf("operator override production agent/mailbox = %+v / %+v / %v",
			overridden, overriddenAddress, err)
	}
	outOfCohort := productionScope
	outOfCohort.AccountIDs = map[string]bool{"acc_aaaaaaaaaaaaaaaa": true}
	if _, err := st.IngestAgentEmailPilot(
		ctx, outOfCohort, ingestInput,
	); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("out-of-cohort production ingest error = %v", err)
	}

	applyPlan(4, emailPolicies, []string{"memory", "facts"})
	if _, err := st.ListAgentEmailProductionCanaryAgents(
		ctx, productionScope,
	); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("personal account production canary error = %v", err)
	}
	if _, err := st.IngestAgentEmailPilot(
		ctx, productionScope, ingestInput,
	); !errors.Is(err, ErrFeatureNotEnabled) {
		t.Fatalf("personal production ingest error = %v", err)
	}
	applyPlan(5, emailPolicies, []string{
		"memory", "facts", plans.AgentEmailReceiveFeature,
	})
	if message, err := st.IngestAgentEmailPilot(
		ctx, productionScope, ingestInput,
	); err != nil || message.ID == "" {
		t.Fatalf("production enabled ingest = %+v / %v", message, err)
	}
}
