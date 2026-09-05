package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/testenv"
)

type customDomainEmailFixture struct {
	store       *Store
	schemaDSN   string
	accountID   string
	realm       Realm
	agents      []Agent
	scope       AgentEmailPilotScope
	address     AgentEmailAddress
	aliasInput  ApplyAgentEmailRealmAliasInput
	customInput ApplyAgentEmailCustomDomainRouteInput
}

func newCustomDomainEmailFixture(t *testing.T, dsn, suffix string) customDomainEmailFixture {
	t.Helper()
	ctx := context.Background()
	st, schemaDSN := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(
		ctx, "custom-domain-"+suffix+"@witwave.ai", "custom domain "+suffix, time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "custom domain realm")
	if err != nil {
		t.Fatal(err)
	}
	agents := make([]Agent, 0, 6)
	enrolled := make(map[string]bool, 6)
	for _, name := range []string{"owner", "two", "three", "four", "five", "six"} {
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, agent)
		enrolled[agent.ID] = true
	}
	scope := AgentEmailPilotScope{
		Enabled: true, Domain: "witmail.net", Audience: "cell-custom-domain-" + suffix,
		RealmIDs: map[string]bool{realm.ID: true}, AgentIDs: enrolled,
	}
	address, err := st.EnsureAgentEmailMailbox(
		ctx, scope, provisioned.AccountID, realm.ID, agents[0].ID, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	aliasInput := ApplyAgentEmailRealmAliasInput{
		ClaimID: "era_aaaaaaaaaaaaaaaa", RealmID: realm.ID,
		Domain: scope.Domain, RealmLabel: "founder",
		State: AgentEmailRealmAliasApplied, ControllerRevision: 1,
	}
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, aliasInput); err != nil {
		t.Fatal(err)
	}
	customInput := ApplyAgentEmailCustomDomainRouteInput{
		DomainRequestID:          "aedr_bbbbbbbbbbbbbbbb",
		DomainAllocationRevision: 2, DomainStateRevision: 4,
		RealmAliasClaimID: aliasInput.ClaimID, RealmAliasRevision: 1,
		RealmID: realm.ID, Domain: "agents.example.com", RealmLabel: aliasInput.RealmLabel,
		State: AgentEmailCustomDomainRouteApplied, ControllerRevision: 1,
	}
	return customDomainEmailFixture{
		store: st, schemaDSN: schemaDSN, accountID: provisioned.AccountID,
		realm: realm, agents: agents, scope: scope, address: address,
		aliasInput: aliasInput, customInput: customInput,
	}
}

func newRouteNamespaceAliasTarget(
	t *testing.T,
	st *Store,
	suffix, claimID, domain, realmLabel string,
) (string, ApplyAgentEmailRealmAliasInput) {
	t.Helper()
	ctx := context.Background()
	provisioned, err := st.ProvisionAccount(
		ctx, "route-namespace-"+suffix+"@witwave.ai",
		"route namespace "+suffix, time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(
		ctx, provisioned.AccountID,
	); err != nil || !activated {
		t.Fatalf("activate route namespace account = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "route namespace realm")
	if err != nil {
		t.Fatal(err)
	}
	return provisioned.AccountID, ApplyAgentEmailRealmAliasInput{
		ClaimID: claimID, RealmID: realm.ID, Domain: domain,
		RealmLabel: realmLabel, State: AgentEmailRealmAliasApplied,
		ControllerRevision: 1,
	}
}

func (f customDomainEmailFixture) ingest(
	t *testing.T, scope AgentEmailPilotScope, recipient string,
) (AgentEmailMessage, error) {
	t.Helper()
	raw := []byte(strings.Join([]string{
		"From: sender@example.com", "To: " + recipient,
		"Subject: custom-domain delivery", "", "signed envelope authority",
	}, "\r\n"))
	digest := sha256.Sum256(raw)
	return f.store.IngestAgentEmailPilot(context.Background(), scope, AgentEmailIngestInput{
		Relay: agentemail.RelayMetadata{
			Timestamp: time.Now().Unix(), KeyID: "custom-domain-key",
			Audience: scope.Audience, EnvelopeSender: "sender@example.com",
			EnvelopeRecipient: recipient, RawSize: int64(len(raw)),
			RawSHA256: hex.EncodeToString(digest[:]),
		},
		Raw: raw,
	})
}

func TestAgentEmailCustomDomainProjectionDeliveryAndFencesPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	f := newCustomDomainEmailFixture(t, dsn, "projection")
	first, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, f.customInput)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, f.customInput)
	if err != nil || !replayed.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("exact replay = %+v / %v", replayed, err)
	}
	got, err := f.store.GetAgentEmailCustomDomainRoute(
		ctx, f.accountID, f.customInput.DomainRequestID, f.customInput.RealmAliasClaimID,
	)
	if err != nil || got.Domain != f.customInput.Domain || got.ControllerRevision != 1 {
		t.Fatalf("exact readback = %+v / %v", got, err)
	}
	if _, err := f.store.GetAgentEmailCustomDomainRoute(
		ctx, f.accountID, "aedr_cccccccccccccccc", f.customInput.RealmAliasClaimID,
	); !errors.Is(err, ErrAgentEmailCustomDomainRouteNotFound) {
		t.Fatalf("missing exact readback error = %v", err)
	}
	mismatch := f.customInput
	mismatch.State = AgentEmailCustomDomainRouteSuspended
	mismatch.SuspensionDisposition = AgentEmailCustomDomainSuspensionRetry
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, mismatch); !errors.Is(err, ErrAgentEmailCustomDomainRouteConflict) {
		t.Fatalf("same-revision mismatch error = %v", err)
	}
	stale := f.customInput
	stale.ControllerRevision = 0
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, stale); !errors.Is(err, ErrAgentEmailInputInvalid) {
		t.Fatalf("invalid zero revision error = %v", err)
	}

	recipient := "owner.founder+signup@" + f.customInput.Domain
	delivered, err := f.ingest(t, f.scope, recipient)
	if err != nil {
		t.Fatal(err)
	}
	if delivered.AddressID != f.address.ID ||
		delivered.RecipientRouteKind != AgentEmailRecipientRouteCustomDomain ||
		delivered.RecipientRealmAliasClaimID != f.customInput.RealmAliasClaimID ||
		delivered.RecipientCustomDomainRequestID != f.customInput.DomainRequestID ||
		delivered.EnvelopeRecipient != recipient {
		t.Fatalf("custom-domain delivery provenance = %+v", delivered)
	}
	page, err := f.store.ListAgentEmails(ctx, f.scope, Principal{
		Kind: PrincipalAgent, ID: f.agents[0].ID, AccountID: f.accountID,
		RealmID: f.realm.ID, AgentName: f.agents[0].Name, AccountStatus: "active",
	}, AgentEmailFilter{Limit: 10})
	if err != nil || len(page.Messages) != 1 ||
		page.Messages[0].RecipientCustomDomainRequestID != f.customInput.DomainRequestID {
		t.Fatalf("stored custom-domain message = %+v / %v", page, err)
	}

	unenrolled := f.scope
	unenrolled.AgentIDs = map[string]bool{}
	for _, agent := range f.agents[1:] {
		unenrolled.AgentIDs[agent.ID] = true
	}
	if _, err := f.ingest(t, unenrolled, recipient); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("unenrolled custom-domain delivery error = %v", err)
	}

	next := f.customInput
	next.State = AgentEmailCustomDomainRouteSuspended
	next.SuspensionDisposition = AgentEmailCustomDomainSuspensionRetry
	next.ControllerRevision = 2
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, next); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ingest(t, f.scope, recipient); !errors.Is(err, ErrAgentEmailReceiveDisabled) {
		t.Fatalf("retry-suspended delivery error = %v", err)
	}
	next.SuspensionDisposition = AgentEmailCustomDomainSuspensionInactive
	next.ControllerRevision = 3
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, next); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ingest(t, f.scope, recipient); !errors.Is(err, ErrAgentEmailUnknownRecipient) {
		t.Fatalf("inactive-suspended delivery error = %v", err)
	}
	next.State = AgentEmailCustomDomainRouteApplied
	next.SuspensionDisposition = ""
	next.ControllerRevision = 4
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, next); err != nil {
		t.Fatal(err)
	}

	alias := f.aliasInput
	alias.State = AgentEmailRealmAliasSuspended
	alias.ControllerRevision = 2
	if _, err := f.store.ApplyAgentEmailRealmAlias(ctx, f.accountID, alias); err != nil {
		t.Fatal(err)
	}
	if replay, err := f.store.ApplyAgentEmailCustomDomainRoute(
		ctx, f.accountID, next,
	); err != nil || replay.ControllerRevision != next.ControllerRevision {
		t.Fatalf("lost-ack exact replay after source advance = %+v / %v", replay, err)
	}
	if _, err := f.ingest(t, f.scope, recipient); !errors.Is(err, ErrAgentEmailReceiveDisabled) {
		t.Fatalf("stale suspended source alias delivery error = %v", err)
	}
	// Plan suspension converges both projections. Once the route carries the
	// exact suspended alias revision, inactive is a permanent recipient result.
	next.State = AgentEmailCustomDomainRouteSuspended
	next.SuspensionDisposition = AgentEmailCustomDomainSuspensionInactive
	next.RealmAliasRevision = 2
	next.ControllerRevision = 5
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, next); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ingest(t, f.scope, recipient); !errors.Is(err, ErrAgentEmailUnknownRecipient) {
		t.Fatalf("plan-suspended inactive delivery error = %v", err)
	}
	// A later operational/source revision advance invalidates that permanent
	// answer until the custom route projection catches up.
	alias.ControllerRevision = 3
	if _, err := f.store.ApplyAgentEmailRealmAlias(ctx, f.accountID, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ingest(t, f.scope, recipient); !errors.Is(err, ErrAgentEmailReceiveDisabled) {
		t.Fatalf("inactive route with stale alias revision error = %v", err)
	}
	alias.State = AgentEmailRealmAliasApplied
	alias.ControllerRevision = 4
	if _, err := f.store.ApplyAgentEmailRealmAlias(ctx, f.accountID, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ingest(t, f.scope, recipient); !errors.Is(err, ErrAgentEmailReceiveDisabled) {
		t.Fatalf("stale alias source revision delivery error = %v", err)
	}
	next.State = AgentEmailCustomDomainRouteApplied
	next.SuspensionDisposition = ""
	next.RealmAliasRevision = 4
	next.ControllerRevision = 6
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, next); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ingest(t, f.scope, recipient); err != nil {
		t.Fatalf("reconciled custom-domain delivery = %v", err)
	}

	policies := map[string]int64{
		plans.AgentEmailRetentionDaysPolicy:      30,
		plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
	}
	hash, err := plans.SnapshotHash("personal", map[string]int64{}, policies, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetAccountPlan(
		ctx, f.accountID, 1, hash, "personal", map[string]int64{}, policies, []string{},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ingest(t, f.scope, recipient); !errors.Is(err, ErrFeatureNotEnabled) {
		t.Fatalf("custom-domain route bypassed receive entitlement: %v", err)
	}

	next.State = AgentEmailCustomDomainRouteRetired
	next.ControllerRevision = 7
	if retired, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, next); err != nil || retired.RetiredAt == nil {
		t.Fatalf("retire = %+v / %v", retired, err)
	}
	next.State = AgentEmailCustomDomainRouteApplied
	next.ControllerRevision = 8
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, next); !errors.Is(err, ErrAgentEmailCustomDomainRouteConflict) {
		t.Fatalf("retired route resurrection error = %v", err)
	}
	var projectionEvents int
	if err := f.store.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		 WHERE account_id=$1 AND verb=$2`,
		f.accountID, VerbAgentEmailCustomDomainRouteProjected,
	).Scan(&projectionEvents); err != nil {
		t.Fatal(err)
	}
	if projectionEvents != 7 {
		t.Fatalf("custom-domain projection events = %d, want 7", projectionEvents)
	}
}

func TestAgentEmailRouteNamespaceRejectsCrossTableCollisionsPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()

	t.Run("managed alias already owns namespace", func(t *testing.T) {
		f := newCustomDomainEmailFixture(t, dsn, "namespace-alias-first")
		otherAccountID, alias := newRouteNamespaceAliasTarget(
			t, f.store, "alias-first", "era_cccccccccccccccc",
			f.customInput.Domain, f.customInput.RealmLabel,
		)
		if _, err := f.store.ApplyAgentEmailRealmAlias(
			ctx, otherAccountID, alias,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.ApplyAgentEmailCustomDomainRoute(
			ctx, f.accountID, f.customInput,
		); !errors.Is(err, ErrAgentEmailCustomDomainRouteConflict) {
			t.Fatalf("custom route collision error = %v", err)
		}
	})

	t.Run("custom route already owns namespace", func(t *testing.T) {
		f := newCustomDomainEmailFixture(t, dsn, "namespace-custom-first")
		if _, err := f.store.ApplyAgentEmailCustomDomainRoute(
			ctx, f.accountID, f.customInput,
		); err != nil {
			t.Fatal(err)
		}
		otherAccountID, alias := newRouteNamespaceAliasTarget(
			t, f.store, "custom-first", "era_dddddddddddddddd",
			f.customInput.Domain, f.customInput.RealmLabel,
		)
		if _, err := f.store.ApplyAgentEmailRealmAlias(
			ctx, otherAccountID, alias,
		); !errors.Is(err, ErrAgentEmailRealmAliasConflict) {
			t.Fatalf("managed alias collision error = %v", err)
		}
	})

	t.Run("concurrent cross-account writers choose one owner", func(t *testing.T) {
		f := newCustomDomainEmailFixture(t, dsn, "namespace-concurrent")
		otherAccountID, alias := newRouteNamespaceAliasTarget(
			t, f.store, "concurrent", "era_eeeeeeeeeeeeeeee",
			f.customInput.Domain, f.customInput.RealmLabel,
		)
		start := make(chan struct{})
		var ready sync.WaitGroup
		ready.Add(2)
		customResult := make(chan error, 1)
		aliasResult := make(chan error, 1)
		go func() {
			ready.Done()
			<-start
			_, err := f.store.ApplyAgentEmailCustomDomainRoute(
				ctx, f.accountID, f.customInput,
			)
			customResult <- err
		}()
		go func() {
			ready.Done()
			<-start
			_, err := f.store.ApplyAgentEmailRealmAlias(ctx, otherAccountID, alias)
			aliasResult <- err
		}()
		ready.Wait()
		close(start)
		customErr := <-customResult
		aliasErr := <-aliasResult
		customWon := customErr == nil && errors.Is(aliasErr, ErrAgentEmailRealmAliasConflict)
		aliasWon := aliasErr == nil && errors.Is(
			customErr, ErrAgentEmailCustomDomainRouteConflict,
		)
		if !customWon && !aliasWon {
			t.Fatalf("concurrent namespace results custom=%v alias=%v", customErr, aliasErr)
		}
		var managedCount, customCount int
		if err := f.store.pool.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM agent_email_realm_aliases
			    WHERE domain=$1 AND realm_label=$2),
			  (SELECT count(*) FROM agent_email_custom_domain_routes
			    WHERE domain=$1 AND realm_label=$2)`,
			f.customInput.Domain, f.customInput.RealmLabel,
		).Scan(&managedCount, &customCount); err != nil {
			t.Fatal(err)
		}
		if managedCount+customCount != 1 {
			t.Fatalf("durable namespace owners managed=%d custom=%d, want one",
				managedCount, customCount)
		}
	})
}

func TestAgentEmailRouteNamespaceFenceRejectsConflictingArchivePostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	source := newCustomDomainEmailFixture(t, dsn, "namespace-archive-source")
	if _, err := source.store.ApplyAgentEmailCustomDomainRoute(
		ctx, source.accountID, source.customInput,
	); err != nil {
		t.Fatal(err)
	}
	if err := source.store.SuspendAccountSystem(
		ctx, source.accountID, "evacuation", "namespace archive collision",
	); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := source.store.ExportAccount(
		ctx, source.accountID, "namespace-source", "test", &archive,
	); err != nil {
		t.Fatal(err)
	}

	destination, _ := newMigrationTestStore(t, dsn)
	if err := destination.Migrate(); err != nil {
		t.Fatal(err)
	}
	otherAccountID, alias := newRouteNamespaceAliasTarget(
		t, destination, "archive-destination", "era_ffffffffffffffff",
		source.customInput.Domain, source.customInput.RealmLabel,
	)
	if _, err := destination.ApplyAgentEmailRealmAlias(
		ctx, otherAccountID, alias,
	); err != nil {
		t.Fatal(err)
	}
	_, err := destination.ImportAccount(
		ctx, source.accountID, bytes.NewReader(archive.Bytes()),
	)
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23505" ||
		!strings.Contains(err.Error(), "route namespace is already reserved") {
		t.Fatalf("conflicting archive import error = %v", err)
	}
	var importedAccounts int
	if err := destination.pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE id=$1`, source.accountID,
	).Scan(&importedAccounts); err != nil {
		t.Fatal(err)
	}
	if importedAccounts != 0 {
		t.Fatalf("conflicting archive left %d imported accounts", importedAccounts)
	}
}

func TestAgentEmailCustomDomainRequestIdentityFenceRejectsConflictingArchivePostgres(
	t *testing.T,
) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	destination, _ := newMigrationTestStore(t, dsn)
	if err := destination.Migrate(); err != nil {
		t.Fatal(err)
	}
	destinationAccountID, destinationAlias := newRouteNamespaceAliasTarget(
		t, destination, "request-identity-destination",
		"era_gggggggggggggggg", "witmail.net", "otherfounder",
	)
	if _, err := destination.ApplyAgentEmailRealmAlias(
		ctx, destinationAccountID, destinationAlias,
	); err != nil {
		t.Fatal(err)
	}

	source := newCustomDomainEmailFixture(t, dsn, "request-identity-source")
	source.customInput.Domain = "mail.other-example.com"
	destinationRoute := ApplyAgentEmailCustomDomainRouteInput{
		DomainRequestID:          source.customInput.DomainRequestID,
		DomainAllocationRevision: 1, DomainStateRevision: 1,
		RealmAliasClaimID: destinationAlias.ClaimID, RealmAliasRevision: 1,
		RealmID: destinationAlias.RealmID, Domain: "mail.destination-example.com",
		RealmLabel: destinationAlias.RealmLabel,
		State:      AgentEmailCustomDomainRouteApplied, ControllerRevision: 1,
	}
	if _, err := destination.ApplyAgentEmailCustomDomainRoute(
		ctx, destinationAccountID, destinationRoute,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := source.store.ApplyAgentEmailCustomDomainRoute(
		ctx, source.accountID, source.customInput,
	); err != nil {
		t.Fatal(err)
	}
	if err := source.store.SuspendAccountSystem(
		ctx, source.accountID, "evacuation", "request identity collision",
	); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := source.store.ExportAccount(
		ctx, source.accountID, "request-identity-source", "test", &archive,
	); err != nil {
		t.Fatal(err)
	}

	_, err := destination.ImportAccount(
		ctx, source.accountID, bytes.NewReader(archive.Bytes()),
	)
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23505" ||
		!strings.Contains(err.Error(), "request identity conflicts") {
		t.Fatalf("request-identity archive import error = %v", err)
	}
	var importedAccounts int
	if err := destination.pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE id=$1`, source.accountID,
	).Scan(&importedAccounts); err != nil {
		t.Fatal(err)
	}
	if importedAccounts != 0 {
		t.Fatalf("request-identity archive left %d imported accounts", importedAccounts)
	}
}

func TestAgentEmailCustomDomainRouteDowngradeRefusesAuthorityPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	f := newCustomDomainEmailFixture(t, dsn, "downgrade")
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, f.customInput); err != nil {
		t.Fatal(err)
	}
	delivered, err := f.ingest(
		t, f.scope, "owner.founder@"+f.customInput.Domain,
	)
	if err != nil {
		t.Fatal(err)
	}
	retired := f.customInput
	retired.State = AgentEmailCustomDomainRouteRetired
	retired.ControllerRevision = 2
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, retired); err != nil {
		t.Fatal(err)
	}
	// Schema 0091 intentionally refuses every downgrade while retained email
	// exists. That invariant has its own focused migration test. Remove this
	// delivery and disposable rate coordination state so this test can reach
	// schema 0088 and exercise that older schema's independent authority fences.
	if _, err := f.store.pool.Exec(ctx, `
		DELETE FROM agent_email_messages WHERE id=$1`, delivered.ID); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`DELETE FROM agent_email_rate_buckets WHERE account_id=$1`,
		`DELETE FROM agent_email_account_rate_buckets WHERE account_id=$1`,
	} {
		if _, err := f.store.pool.Exec(ctx, query, f.accountID); err != nil {
			t.Fatal(err)
		}
	}
	migrationTestDownTo(t, f.schemaDSN, 88)
	downErr := migrationTestDown(t, f.schemaDSN, true)
	if downErr == nil || !strings.Contains(downErr.Error(), "custom-domain email routes exist") {
		t.Fatalf("schema-88 downgrade error = %v", downErr)
	}
	assertMigrationTestVersion(t, f.schemaDSN, 88)
	assertMigrationTestTable(t, f.store, "agent_email_custom_domain_routes", true)
	assertMigrationTestColumn(
		t, f.store, "agent_email_messages", "recipient_custom_domain_request_id", true,
	)

	// Exercise the independent message-provenance fence under a hostile legacy
	// shape where route authority was removed outside supported product paths.
	// Recreate the exact immutable provenance only after reaching schema 0088;
	// carrying it through 0091 would correctly stop at the newer global guard.
	legacyRaw := []byte("schema-88 custom-domain provenance")
	if _, err := f.store.pool.Exec(ctx, `
		INSERT INTO agent_email_messages
		  (id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,
		   provider,envelope_sender,envelope_recipient,agent_segment,realm_label,
		   recipient_route_kind,recipient_realm_alias_claim_id,
		   recipient_custom_domain_request_id,
		   raw_mime,raw_size_bytes,raw_sha256,parse_state,attachment_count,
		   body_text,body_text_kind,attachment_storage_bytes,
		   retained_attachment_storage_bytes,payload_retention_state,
		   attachment_storage_accounted,sender_verification_state,
		   duplicate_group_sha256,received_at)
		VALUES
		  ($1,$2,$3,$4,$5,$6,'migration_test','sender@example.com',$7,$8,$9,
		   'custom_domain',$10,$11,$12,$13,repeat('a',64),'parsed',0,
		   'bounded provenance','text/plain',0,0,'retained',true,'unverified',
		   repeat('b',64),clock_timestamp())`,
		delivered.ID, f.accountID, f.realm.ID, f.address.MailboxID,
		f.address.OwnerAgentID, f.address.ID,
		"owner.founder@"+f.customInput.Domain, f.address.AgentSegment,
		f.customInput.RealmLabel, f.customInput.RealmAliasClaimID,
		f.customInput.DomainRequestID, legacyRaw, len(legacyRaw),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `
		ALTER TABLE agent_email_messages
		  DROP CONSTRAINT agent_email_messages_custom_domain_route_fk`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `
		DELETE FROM agent_email_custom_domain_routes WHERE account_id=$1`,
		f.accountID); err != nil {
		t.Fatal(err)
	}
	downErr = migrationTestDown(t, f.schemaDSN, true)
	if downErr == nil || !strings.Contains(downErr.Error(), "custom-domain email messages exist") {
		t.Fatalf("schema-88 message-provenance downgrade error = %v", downErr)
	}
	assertMigrationTestVersion(t, f.schemaDSN, 88)
	assertMigrationTestColumn(
		t, f.store, "agent_email_messages", "recipient_custom_domain_request_id", true,
	)
}

func TestAgentEmailCustomDomainRouteMustRetireBeforeRealmClosePostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	f := newCustomDomainEmailFixture(t, dsn, "realm-close")
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(
		ctx, f.accountID, f.customInput,
	); err != nil {
		t.Fatal(err)
	}
	for _, agent := range f.agents {
		if err := f.store.DeleteAgent(
			ctx, f.accountID, f.realm.ID, agent.ID,
		); err != nil {
			t.Fatal(err)
		}
	}
	alias := f.aliasInput
	alias.State = AgentEmailRealmAliasRetired
	alias.ControllerRevision = 2
	if _, err := f.store.ApplyAgentEmailRealmAlias(
		ctx, f.accountID, alias,
	); err != nil {
		t.Fatal(err)
	}

	prepare := RealmEmailRouteRetirementInput{
		RealmID: f.realm.ID, OperationID: "close_custom_domain_route",
		ExpectedGeneration: 1,
	}
	if _, err := f.store.PrepareRealmEmailRouteRetirement(
		ctx, f.accountID, prepare,
	); !errors.Is(err, ErrRealmEmailRouteConflict) {
		t.Fatalf("prepare with non-retired custom-domain route = %v", err)
	}

	retired := f.customInput
	retired.State = AgentEmailCustomDomainRouteRetired
	retired.RealmAliasRevision = alias.ControllerRevision
	retired.ControllerRevision = 2
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(
		ctx, f.accountID, retired,
	); err != nil {
		t.Fatal(err)
	}
	closing, err := f.store.PrepareRealmEmailRouteRetirement(
		ctx, f.accountID, prepare,
	)
	if err != nil || closing.State != RealmEmailRouteClosing ||
		closing.Generation != 2 {
		t.Fatalf("prepare after custom-domain retirement = %+v / %v", closing, err)
	}
	committed, err := f.store.CommitRealmEmailRouteRetirement(
		ctx, f.accountID, RealmEmailRouteRetirementInput{
			RealmID: f.realm.ID, OperationID: prepare.OperationID,
			ExpectedGeneration: closing.Generation,
		},
	)
	if err != nil || committed.State != RealmEmailRouteRetired {
		t.Fatalf("commit after custom-domain retirement = %+v / %v", committed, err)
	}
}

func TestAgentEmailCustomDomainArchiveRoundTripAndEnvelopeValidationPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	f := newCustomDomainEmailFixture(t, dsn, "archive")
	destination, _ := newMigrationTestStore(t, dsn)
	if err := destination.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, f.customInput); err != nil {
		t.Fatal(err)
	}
	recipient := "owner.founder+move@" + f.customInput.Domain
	delivered, err := f.ingest(t, f.scope, recipient)
	if err != nil {
		t.Fatal(err)
	}
	retired := f.customInput
	retired.State = AgentEmailCustomDomainRouteRetired
	retired.ControllerRevision = 2
	if _, err := f.store.ApplyAgentEmailCustomDomainRoute(ctx, f.accountID, retired); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SuspendAccountSystem(
		ctx, f.accountID, "evacuation", "custom-domain archive round trip",
	); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := f.store.ExportAccount(ctx, f.accountID, "custom-domain-source", "test", &archive); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.ImportAccount(ctx, f.accountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	route, err := destination.GetAgentEmailCustomDomainRoute(
		ctx, f.accountID, f.customInput.DomainRequestID, f.customInput.RealmAliasClaimID,
	)
	if err != nil || route.State != AgentEmailCustomDomainRouteRetired ||
		route.ControllerRevision != 2 || route.RetiredAt == nil {
		t.Fatalf("restored custom-domain route = %+v / %v", route, err)
	}
	var routeKind, domainRequestID, aliasClaimID, envelopeRecipient string
	if err := destination.pool.QueryRow(ctx, `
		SELECT recipient_route_kind,recipient_custom_domain_request_id,
		       recipient_realm_alias_claim_id,envelope_recipient
		  FROM agent_email_messages WHERE account_id=$1 AND id=$2`,
		f.accountID, delivered.ID,
	).Scan(&routeKind, &domainRequestID, &aliasClaimID, &envelopeRecipient); err != nil {
		t.Fatal(err)
	}
	if routeKind != AgentEmailRecipientRouteCustomDomain ||
		domainRequestID != f.customInput.DomainRequestID ||
		aliasClaimID != f.customInput.RealmAliasClaimID || envelopeRecipient != recipient {
		t.Fatalf("restored custom-domain provenance = %q/%q/%q/%q",
			routeKind, domainRequestID, aliasClaimID, envelopeRecipient)
	}

	manifest, rows := readAvatarArchiveRows(t, archive.Bytes(), SchemaVersion())
	messageRows := rows["agent_email_messages"]
	if len(messageRows) != 1 {
		t.Fatalf("archive message rows = %d, want 1", len(messageRows))
	}
	var hostile map[string]any
	decoder := json.NewDecoder(bytes.NewReader(messageRows[0]))
	decoder.UseNumber()
	if err := decoder.Decode(&hostile); err != nil {
		t.Fatal(err)
	}
	hostile["envelope_recipient"] = "owner.founder+move@wrong.example.com"
	rows["agent_email_messages"][0], err = json.Marshal(hostile)
	if err != nil {
		t.Fatal(err)
	}
	hostileArchive := writeAvatarArchiveRows(t, manifest, manifest.Tables, rows)
	hostileDestination, _ := newMigrationTestStore(t, dsn)
	if err := hostileDestination.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := hostileDestination.ImportAccount(
		ctx, f.accountID, bytes.NewReader(hostileArchive),
	); err == nil || !errors.Is(err, ErrArchiveContent) ||
		!strings.Contains(err.Error(), "envelope_recipient does not match") {
		t.Fatalf("hostile envelope-domain import error = %v", err)
	}
}
