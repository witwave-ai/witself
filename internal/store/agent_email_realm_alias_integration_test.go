package store

import (
	"bytes"
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

func TestAgentEmailRealmAliasProjectionAndDeliveryPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, schemaDSN := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(
		ctx, "agent-email-realm-alias@witwave.ai", "agent email realm alias", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "alias realm")
	if err != nil {
		t.Fatal(err)
	}
	target, err := st.GetAgentEmailRealmAliasTarget(ctx, provisioned.AccountID, realm.ID)
	if err != nil || target.AccountID != provisioned.AccountID ||
		target.RealmID != realm.ID || !target.Exists {
		t.Fatalf("live alias target = %+v / %v", target, err)
	}
	if _, err := st.GetAgentEmailRealmAliasTarget(
		ctx, "acc_missing", realm.ID,
	); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("missing alias target account error = %v", err)
	}
	otherAccount, err := st.ProvisionAccount(
		ctx, "other-agent-email-realm-alias@witwave.ai",
		"other agent email realm alias", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAgentEmailRealmAliasTarget(
		ctx, otherAccount.AccountID, realm.ID,
	); !errors.Is(err, ErrRealmNotFound) {
		t.Fatalf("out-of-account alias target realm error = %v", err)
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
		Enabled: true, Domain: "witmail.net",
		LegacyDomains: []string{"agent-mail.witwave.ai"}, Audience: "cell-alias-test",
		RealmIDs: map[string]bool{realm.ID: true}, AgentIDs: enrolled,
	}
	canonical, err := st.EnsureAgentEmailMailbox(
		ctx, scope, provisioned.AccountID, realm.ID, owner.ID, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical.Addresses) != 1 ||
		canonical.Addresses[0].Role != AgentEmailAddressRolePrimary {
		t.Fatalf("new primary-domain mailbox inherited legacy route = %+v", canonical.Addresses)
	}

	firstInput := ApplyAgentEmailRealmAliasInput{
		ClaimID: "era_aaaaaaaaaaaaaaaa", RealmID: realm.ID,
		Domain: scope.Domain, RealmLabel: "founder", State: AgentEmailRealmAliasApplied,
		ControllerRevision: 1,
	}
	first, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, firstInput)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, firstInput)
	if err != nil || replayed.ClaimID != first.ClaimID ||
		!replayed.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("idempotent replay = %+v / %v", replayed, err)
	}
	firstInput.ControllerRevision = 2
	first, err = st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, firstInput)
	if err != nil || first.ControllerRevision != 2 {
		t.Fatalf("revision advance = %+v / %v", first, err)
	}
	stale := firstInput
	stale.ControllerRevision = 1
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, stale); !errors.Is(err, ErrAgentEmailRealmAliasConflict) {
		t.Fatalf("stale apply error = %v", err)
	}
	mismatched := firstInput
	mismatched.State = AgentEmailRealmAliasSuspended
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, mismatched); !errors.Is(err, ErrAgentEmailRealmAliasConflict) {
		t.Fatalf("same-revision mismatch error = %v", err)
	}
	secondInput := ApplyAgentEmailRealmAliasInput{
		ClaimID: "era_bbbbbbbbbbbbbbbb", RealmID: realm.ID,
		Domain: scope.Domain, RealmLabel: "studio", State: AgentEmailRealmAliasApplied,
		ControllerRevision: 1,
	}
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, secondInput); err != nil {
		t.Fatal(err)
	}
	reused := secondInput
	reused.ClaimID = "era_cccccccccccccccc"
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, reused); !errors.Is(err, ErrAgentEmailRealmAliasConflict) {
		t.Fatalf("label reuse error = %v", err)
	}

	listed, err := st.ListAgentEmailRealmAliases(ctx, provisioned.AccountID)
	if err != nil || len(listed) != 2 || listed[0].RealmLabel != "founder" || listed[1].RealmLabel != "studio" {
		t.Fatalf("alias list = %+v / %v", listed, err)
	}
	got, err := st.GetAgentEmailRealmAlias(ctx, provisioned.AccountID, first.ClaimID)
	if err != nil || got.ControllerRevision != first.ControllerRevision || got.State != AgentEmailRealmAliasApplied {
		t.Fatalf("alias get = %+v / %v", got, err)
	}
	shown, err := st.GetAgentEmailAddress(ctx, scope, owner)
	if err != nil || len(shown.Aliases) != 2 ||
		shown.Aliases[0].Address != "owner.founder@"+scope.Domain ||
		shown.Address != canonical.Address {
		t.Fatalf("mailbox aliases = %+v / %v", shown, err)
	}

	aliasRecipient := "owner.founder+signup@" + scope.Domain
	raw := []byte(strings.Join([]string{
		"From: sender@example.com",
		"To: " + aliasRecipient,
		"Subject: alias delivery",
		"",
		"same canonical mailbox",
	}, "\r\n"))
	digest := sha256.Sum256(raw)
	ingest := func(recipient string) (AgentEmailMessage, error) {
		return st.IngestAgentEmailPilot(ctx, scope, AgentEmailIngestInput{
			Relay: agentemail.RelayMetadata{
				Timestamp: time.Now().Unix(), KeyID: "alias-key", Audience: scope.Audience,
				EnvelopeSender: "sender@example.com", EnvelopeRecipient: recipient,
				RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
			},
			Raw: raw,
		})
	}
	legacyDomain := scope.LegacyDomains[0]
	legacyCanonicalRecipient := canonical.LocalPart + "@" + legacyDomain
	if _, err := ingest(legacyCanonicalRecipient); !errors.Is(err, ErrAgentEmailUnknownRecipient) {
		t.Fatalf("unissued legacy canonical delivery error = %v", err)
	}
	// Simulate a stale or imported projection where both a compatibility route
	// and an alias claim exist on the old domain. Ingress must still reject the
	// alias before consulting either row: only old-issued canonical locals gain
	// legacy-domain compatibility.
	legacyClaimID := "era_eeeeeeeeeeeeeeee"
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_address_domains
		  (account_id,realm_id,provisioned_agent_id,address_id,domain,local_part,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		canonical.AccountID, canonical.RealmID, canonical.OwnerAgentID,
		canonical.ID, legacyDomain, canonical.LocalPart, canonical.CreatedAt,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_realm_aliases
		  (claim_id,account_id,realm_id,domain,realm_label,state,controller_revision)
		VALUES ($1,$2,$3,$4,$5,$6,1)`,
		legacyClaimID, canonical.AccountID, canonical.RealmID, legacyDomain,
		"legacyalias", AgentEmailRealmAliasApplied,
	); err != nil {
		t.Fatal(err)
	}
	persistedLegacyAliasRecipient := "owner.legacyalias+signup@" + legacyDomain
	if _, err := ingest(persistedLegacyAliasRecipient); !errors.Is(err, ErrAgentEmailUnknownRecipient) {
		t.Fatalf("persisted legacy-domain alias delivery error = %v", err)
	}
	// Legacy-domain compatibility preserves only issued canonical local parts.
	// Realm-alias claims stay bound to their explicit primary domain and are not
	// implicitly duplicated onto the compatibility domain.
	legacyAliasRecipient := "owner.founder+signup@" + legacyDomain
	if _, err := ingest(legacyAliasRecipient); !errors.Is(err, ErrAgentEmailUnknownRecipient) {
		t.Fatalf("legacy-domain alias delivery error = %v", err)
	}
	delivered, err := ingest(aliasRecipient)
	if err != nil {
		t.Fatal(err)
	}
	if delivered.AddressID != canonical.ID || delivered.MailboxID != canonical.MailboxID ||
		delivered.RecipientRouteKind != AgentEmailRecipientRouteRealmAlias ||
		delivered.RecipientRealmAliasClaimID != first.ClaimID ||
		delivered.RealmLabel != "founder" || delivered.EnvelopeRecipient != aliasRecipient {
		t.Fatalf("alias delivery provenance = %+v", delivered)
	}
	page, err := st.ListAgentEmails(ctx, scope, owner, AgentEmailFilter{Limit: 10})
	if err != nil || len(page.Messages) != 1 ||
		page.Messages[0].RecipientRealmAliasClaimID != first.ClaimID ||
		page.Messages[0].EnvelopeRecipient != aliasRecipient {
		t.Fatalf("stored alias delivery = %+v / %v", page, err)
	}

	firstInput.State = AgentEmailRealmAliasSuspended
	firstInput.ControllerRevision = 3
	suspended, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, firstInput)
	if err != nil || suspended.SuspendedAt == nil || suspended.RetiredAt != nil {
		t.Fatalf("suspend = %+v / %v", suspended, err)
	}
	if _, err := ingest(aliasRecipient); !errors.Is(err, ErrAgentEmailReceiveDisabled) {
		t.Fatalf("suspended delivery error = %v", err)
	}

	// The account entitlement remains authoritative even for a currently
	// applied alias; Personal-style snapshots reject before any message row is
	// persisted, exactly like the canonical route.
	policies := map[string]int64{
		plans.AgentEmailRetentionDaysPolicy:      30,
		plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
	}
	hash, err := plans.SnapshotHash("personal", map[string]int64{}, policies, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(
		ctx, provisioned.AccountID, 1, hash, "personal",
		map[string]int64{}, policies, []string{},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest("owner.studio@" + scope.Domain); !errors.Is(err, ErrFeatureNotEnabled) {
		t.Fatalf("personal alias delivery error = %v", err)
	}

	firstInput.State = AgentEmailRealmAliasRetired
	firstInput.ControllerRevision = 4
	retired, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, firstInput)
	if err != nil || retired.RetiredAt == nil {
		t.Fatalf("retire = %+v / %v", retired, err)
	}
	firstInput.State = AgentEmailRealmAliasApplied
	firstInput.ControllerRevision = 5
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, firstInput); !errors.Is(err, ErrAgentEmailRealmAliasConflict) {
		t.Fatalf("retired reactivation error = %v", err)
	}
	reused.RealmLabel = "founder"
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, reused); !errors.Is(err, ErrAgentEmailRealmAliasConflict) {
		t.Fatalf("retired label reuse error = %v", err)
	}
	if _, err := ingest(aliasRecipient); !errors.Is(err, ErrAgentEmailUnknownRecipient) {
		t.Fatalf("retired delivery error = %v", err)
	}
	deletedRealm, err := st.CreateRealm(ctx, provisioned.AccountID, "deleted alias realm")
	if err != nil {
		t.Fatal(err)
	}
	deletedInput := ApplyAgentEmailRealmAliasInput{
		ClaimID: "era_dddddddddddddddd", RealmID: deletedRealm.ID,
		Domain: scope.Domain, RealmLabel: "closed-realm", State: AgentEmailRealmAliasApplied,
		ControllerRevision: 1,
	}
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, deletedInput); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRealm(ctx, provisioned.AccountID, deletedRealm.ID); !errors.Is(err, ErrRealmNotEmpty) {
		t.Fatalf("delete realm with live alias error = %v", err)
	}
	deletedInput.State = AgentEmailRealmAliasRetired
	deletedInput.ControllerRevision = 2
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, deletedInput); err != nil {
		t.Fatalf("retire alias before realm deletion: %v", err)
	}
	if err := st.DeleteRealm(ctx, provisioned.AccountID, deletedRealm.ID); err != nil {
		t.Fatalf("delete realm after alias retirement: %v", err)
	}
	if _, err := st.GetAgentEmailRealmAliasTarget(
		ctx, provisioned.AccountID, deletedRealm.ID,
	); !errors.Is(err, ErrRealmNotFound) {
		t.Fatalf("deleted alias target realm error = %v", err)
	}
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, deletedInput); err != nil {
		t.Fatalf("replay retired alias after soft realm deletion: %v", err)
	}
	deletedInput.State = AgentEmailRealmAliasApplied
	deletedInput.ControllerRevision = 3
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, deletedInput); !errors.Is(err, ErrRealmNotFound) {
		t.Fatalf("reactivate alias on deleted realm error = %v", err)
	}
	deletedInput.State = AgentEmailRealmAliasRetired
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, deletedInput); err != nil {
		t.Fatalf("retire alias after soft realm deletion: %v", err)
	}
	var projectionEvents int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		 WHERE account_id=$1 AND verb=$2`,
		provisioned.AccountID, VerbAgentEmailRealmAliasProjected,
	).Scan(&projectionEvents); err != nil {
		t.Fatal(err)
	}
	if projectionEvents != 8 {
		t.Fatalf("projection audit events = %d, want 8 state/revision advances only", projectionEvents)
	}
	// Remove the direct SQL corruption fixture before testing the safe 0087
	// downgrade. Product paths never delete either tombstone; this fixture exists
	// solely to prove ingress ignores even persisted legacy alias authority.
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM agent_email_realm_aliases WHERE claim_id=$1`, legacyClaimID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM agent_email_address_domains WHERE address_id=$1 AND domain=$2`,
		canonical.ID, legacyDomain,
	); err != nil {
		t.Fatal(err)
	}
	// Schema 0091 intentionally prevents carrying retained email into any older
	// schema. Its nonempty guard is covered independently, so remove this
	// delivery and disposable rate debt before reaching the schema-0085
	// provenance guard that this test owns. The exact alias message is recreated
	// under schema 0085 below.
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM agent_email_messages WHERE id=$1`, delivered.ID); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`DELETE FROM agent_email_rate_buckets WHERE account_id=$1`,
		`DELETE FROM agent_email_account_rate_buckets WHERE account_id=$1`,
	} {
		if _, err := st.pool.Exec(ctx, query, provisioned.AccountID); err != nil {
			t.Fatal(err)
		}
	}
	// Schema 0091 can now step back to 0090. Schema 0090 can discard its empty
	// account-wide coordination table, and schema 0089 carries no outbound
	// authority in this fixture, so it can then step back to 0088. Schema 0088
	// carries no custom-domain authority in this fixture, so it can then step
	// back to 0087. Schema 0087 can safely discard its sole original-domain
	// route, and schema 0086 can step back to 0085. The following 0085 -> 0084
	// downgrade must still refuse to discard realm-alias delivery provenance.
	// Schema 0092 only adds the nullable account purge column and its CHECK
	// constraint, so it can always step back to 0091.
	if err := migrationTestDown(t, schemaDSN, false); err != nil {
		t.Fatalf("downgrade schema 0092 to 0091: %v", err)
	}
	assertMigrationTestVersion(t, schemaDSN, 91)
	if err := migrationTestDown(t, schemaDSN, false); err != nil {
		t.Fatalf("downgrade schema 0091 to 0090: %v", err)
	}
	assertMigrationTestVersion(t, schemaDSN, 90)
	if err := migrationTestDown(t, schemaDSN, false); err != nil {
		t.Fatalf("downgrade schema 0090 to 0089: %v", err)
	}
	assertMigrationTestVersion(t, schemaDSN, 89)
	if err := migrationTestDown(t, schemaDSN, false); err != nil {
		t.Fatalf("downgrade schema 0089 to 0088: %v", err)
	}
	assertMigrationTestVersion(t, schemaDSN, 88)
	if err := migrationTestDown(t, schemaDSN, false); err != nil {
		t.Fatalf("downgrade schema 0088 to 0087: %v", err)
	}
	assertMigrationTestVersion(t, schemaDSN, 87)
	if err := migrationTestDown(t, schemaDSN, false); err != nil {
		t.Fatalf("downgrade schema 0087 to 0086: %v", err)
	}
	assertMigrationTestVersion(t, schemaDSN, 86)
	if err := migrationTestDown(t, schemaDSN, false); err != nil {
		t.Fatalf("downgrade schema 0086 to 0085: %v", err)
	}
	assertMigrationTestVersion(t, schemaDSN, 85)
	legacyRaw := []byte("schema-85 realm-alias provenance")
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_messages
		  (id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,
		   provider,envelope_sender,envelope_recipient,agent_segment,realm_label,
		   recipient_route_kind,recipient_realm_alias_claim_id,
		   raw_mime,raw_size_bytes,raw_sha256,parse_state,attachment_count,
		   body_text,body_text_kind,attachment_storage_bytes,
		   retained_attachment_storage_bytes,payload_retention_state,
		   attachment_storage_accounted,sender_verification_state,
		   duplicate_group_sha256,received_at)
		VALUES
		  ($1,$2,$3,$4,$5,$6,'migration_test','sender@example.com',$7,$8,$9,
		   'realm_alias',$10,$11,$12,repeat('a',64),'parsed',0,
		   'bounded provenance','text/plain',0,0,'retained',true,'unverified',
		   repeat('b',64),clock_timestamp())`,
		delivered.ID, provisioned.AccountID, realm.ID, canonical.MailboxID,
		canonical.OwnerAgentID, canonical.ID, aliasRecipient,
		canonical.AgentSegment, "founder", first.ClaimID, legacyRaw, len(legacyRaw),
	); err != nil {
		t.Fatal(err)
	}
	downErr := migrationTestDown(t, schemaDSN, true)
	if downErr == nil || !strings.Contains(
		downErr.Error(), "realm-alias email messages exist",
	) {
		t.Fatalf("downgrade error = %v", downErr)
	}
	assertMigrationTestVersion(t, schemaDSN, 85)
	assertMigrationTestTable(t, st, "agent_email_realm_aliases", true)
	assertMigrationTestColumn(t, st, "agent_email_messages", "recipient_route_kind", true)
	assertMigrationTestColumn(t, st, "agent_email_messages", "recipient_realm_alias_claim_id", true)

	var preservedRouteKind, preservedClaimID, preservedRealmLabel, preservedEnvelopeRecipient string
	if err := st.pool.QueryRow(ctx, `
		SELECT recipient_route_kind,recipient_realm_alias_claim_id,realm_label,envelope_recipient
		  FROM agent_email_messages WHERE id=$1`,
		delivered.ID,
	).Scan(
		&preservedRouteKind, &preservedClaimID, &preservedRealmLabel,
		&preservedEnvelopeRecipient,
	); err != nil {
		t.Fatal(err)
	}
	if preservedRouteKind != AgentEmailRecipientRouteRealmAlias ||
		preservedClaimID != first.ClaimID || preservedRealmLabel != "founder" ||
		preservedEnvelopeRecipient != aliasRecipient {
		t.Fatalf(
			"alias message after refused downgrade = %q/%q/%q/%q",
			preservedRouteKind, preservedClaimID, preservedRealmLabel,
			preservedEnvelopeRecipient,
		)
	}
	var preservedClaimState string
	if err := st.pool.QueryRow(ctx, `
		SELECT state FROM agent_email_realm_aliases WHERE claim_id=$1`,
		first.ClaimID,
	).Scan(&preservedClaimState); err != nil {
		t.Fatal(err)
	}
	if preservedClaimState != AgentEmailRealmAliasRetired {
		t.Fatalf("alias claim state after refused downgrade = %q", preservedClaimState)
	}
}

func TestAgentEmailRealmAliasMigrationDowngradeWithoutDeliveriesPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, schemaDSN := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(
		ctx, "agent-email-realm-alias-downgrade@witwave.ai",
		"agent email realm alias downgrade", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "alias downgrade realm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, ApplyAgentEmailRealmAliasInput{
		ClaimID: "era_eeeeeeeeeeeeeeee", RealmID: realm.ID,
		Domain: "agent-mail.witwave.ai", RealmLabel: "downgrade",
		State: AgentEmailRealmAliasApplied, ControllerRevision: 1,
	}); err != nil {
		t.Fatal(err)
	}

	migrationTestDownTo(t, schemaDSN, 84)
	assertMigrationTestVersion(t, schemaDSN, 84)
	assertMigrationTestTable(t, st, "agent_email_realm_aliases", false)
	assertMigrationTestColumn(t, st, "agent_email_messages", "recipient_route_kind", false)
	assertMigrationTestColumn(t, st, "agent_email_messages", "recipient_realm_alias_claim_id", false)
	assertMigrationTestTableConstraint(
		t, st, "agent_email_messages", "agent_email_messages_realm_label_check", true,
	)
}

func TestAgentEmailRealmAliasAppliedReplayRevalidatesLiveRealmPostgres(t *testing.T) {
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
		ctx, "agent-email-realm-alias-replay@witwave.ai",
		"agent email realm alias replay", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "alias replay realm")
	if err != nil {
		t.Fatal(err)
	}
	input := ApplyAgentEmailRealmAliasInput{
		ClaimID: "era_ffffffffffffffff", RealmID: realm.ID,
		Domain: "agent-mail.witwave.ai", RealmLabel: "replay",
		State: AgentEmailRealmAliasApplied, ControllerRevision: 1,
	}
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, input); err != nil {
		t.Fatal(err)
	}

	// Simulate recovery encountering an older or externally-created lifecycle
	// race. The public realm delete path now prevents this state, but an exact
	// applied replay must still fail closed rather than authorize publication.
	if _, err := st.pool.Exec(ctx, `
		UPDATE realms
		   SET deleted_at=clock_timestamp(),updated_at=clock_timestamp(),
		       email_route_state='retired',email_route_generation=2,
		       email_route_operation_id='legacy_delete'
		 WHERE account_id=$1 AND id=$2`, provisioned.AccountID, realm.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyAgentEmailRealmAlias(
		ctx, provisioned.AccountID, input,
	); !errors.Is(err, ErrRealmNotFound) {
		t.Fatalf("applied replay on deleted realm error = %v", err)
	}
	preserved, err := st.GetAgentEmailRealmAlias(
		ctx, provisioned.AccountID, input.ClaimID,
	)
	if err != nil || preserved.State != AgentEmailRealmAliasApplied ||
		preserved.ControllerRevision != input.ControllerRevision {
		t.Fatalf("projection after refused applied replay = %+v / %v", preserved, err)
	}
}

func TestAgentEmailRealmAliasArchiveRoundTripPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	source, _ := newMigrationTestStore(t, dsn)
	destination, _ := newMigrationTestStore(t, dsn)
	if err := source.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := destination.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := source.ProvisionAccount(
		ctx, "agent-email-alias-archive@witwave.ai", "alias archive", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := source.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := source.CreateRealm(ctx, provisioned.AccountID, "portable aliases")
	if err != nil {
		t.Fatal(err)
	}
	agents := make([]Agent, 0, 5)
	enrolled := make(map[string]bool, 5)
	for _, name := range []string{"owner", "two", "three", "four", "five"} {
		agent, err := source.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, agent)
		enrolled[agent.ID] = true
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "agent-mail.witwave.ai", Audience: "cell-alias-archive",
		RealmIDs: map[string]bool{realm.ID: true}, AgentIDs: enrolled,
	}
	address, err := source.EnsureAgentEmailMailbox(
		ctx, scope, provisioned.AccountID, realm.ID, agents[0].ID, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	first := ApplyAgentEmailRealmAliasInput{
		ClaimID: "era_dddddddddddddddd", RealmID: realm.ID, Domain: scope.Domain,
		RealmLabel: "portable", State: AgentEmailRealmAliasApplied, ControllerRevision: 1,
	}
	second := ApplyAgentEmailRealmAliasInput{
		ClaimID: "era_eeeeeeeeeeeeeeee", RealmID: realm.ID, Domain: scope.Domain,
		RealmLabel: "standby", State: AgentEmailRealmAliasApplied, ControllerRevision: 1,
	}
	if _, err := source.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, first); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, second); err != nil {
		t.Fatal(err)
	}
	recipient := "owner.portable+move@" + scope.Domain
	raw := []byte("From: sender@example.com\r\nTo: " + recipient + "\r\nSubject: portable alias\r\n\r\nmove me")
	digest := sha256.Sum256(raw)
	delivered, err := source.IngestAgentEmailPilot(ctx, scope, AgentEmailIngestInput{
		Relay: agentemail.RelayMetadata{
			Timestamp: time.Now().Unix(), KeyID: "archive-key", Audience: scope.Audience,
			EnvelopeSender: "sender@example.com", EnvelopeRecipient: recipient,
			RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
		},
		Raw: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	first.State, first.ControllerRevision = AgentEmailRealmAliasRetired, 2
	second.State, second.ControllerRevision = AgentEmailRealmAliasSuspended, 2
	if _, err := source.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, first); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, second); err != nil {
		t.Fatal(err)
	}
	if err := source.SuspendAccountSystem(
		ctx, provisioned.AccountID, "evacuation", "move alias account",
	); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := source.ExportAccount(ctx, provisioned.AccountID, "alias-source", "test", &archive); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.ImportAccount(
		ctx, provisioned.AccountID, bytes.NewReader(archive.Bytes()),
	); err != nil {
		t.Fatal(err)
	}
	aliases, err := destination.ListAgentEmailRealmAliases(ctx, provisioned.AccountID)
	if err != nil || len(aliases) != 2 || aliases[0].State != AgentEmailRealmAliasRetired ||
		aliases[0].ControllerRevision != 2 || aliases[0].RetiredAt == nil ||
		aliases[1].State != AgentEmailRealmAliasSuspended || aliases[1].SuspendedAt == nil {
		t.Fatalf("restored aliases = %+v / %v", aliases, err)
	}
	var routeKind, claimID, realmLabel, envelopeRecipient, restoredAddressID string
	if err := destination.pool.QueryRow(ctx, `
		SELECT recipient_route_kind,recipient_realm_alias_claim_id,realm_label,
		       envelope_recipient,address_id
		  FROM agent_email_messages
		 WHERE account_id=$1 AND id=$2`, provisioned.AccountID, delivered.ID).
		Scan(&routeKind, &claimID, &realmLabel, &envelopeRecipient, &restoredAddressID); err != nil {
		t.Fatal(err)
	}
	if routeKind != AgentEmailRecipientRouteRealmAlias || claimID != first.ClaimID ||
		realmLabel != first.RealmLabel || envelopeRecipient != recipient ||
		restoredAddressID != address.ID {
		t.Fatalf("restored route = %q/%q/%q/%q/%q", routeKind, claimID,
			realmLabel, envelopeRecipient, restoredAddressID)
	}
}
