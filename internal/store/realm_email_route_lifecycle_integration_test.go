package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestRealmEmailRouteInputGrammar(t *testing.T) {
	for _, value := range []string{
		"close_1",
		"customer.request:2026-08-02",
		"A-Z_a.z:2-7",
	} {
		if !realmEmailRouteOperationIDPattern.MatchString(value) {
			t.Errorf("operation id %q was rejected", value)
		}
	}
	for _, value := range []string{
		"",
		" has-space",
		"has/slash",
		strings.Repeat("a", 129),
	} {
		if realmEmailRouteOperationIDPattern.MatchString(value) {
			t.Errorf("operation id %q was accepted", value)
		}
	}
	if !validRealmEmailRouteRealmID("realm_abcdefghijkl2345") {
		t.Fatal("canonical realm id was rejected")
	}
	for _, value := range []string{
		"realm_legacy",
		"realm_abcdefghijkl0189",
		"realm_ABCDEFGHIJKLMNOP",
		" realm_abcdefghijkl2345",
	} {
		if validRealmEmailRouteRealmID(value) {
			t.Errorf("realm id %q was accepted", value)
		}
	}
}

func TestRealmEmailRouteLifecyclePostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(
		ctx, "realm-route-lifecycle@witwave.ai", "route lifecycle", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if active, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !active {
		t.Fatalf("activate = %v / %v", active, err)
	}
	first, err := st.CreateRealm(ctx, provisioned.AccountID, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateRealm(ctx, provisioned.AccountID, "second")
	if err != nil {
		t.Fatal(err)
	}

	live, err := st.GetRealmEmailRouteLifecycle(ctx, provisioned.AccountID, first.ID)
	if err != nil || live.State != RealmEmailRouteLive || live.Generation != 1 ||
		live.OperationID != "" {
		t.Fatalf("initial route = %+v / %v", live, err)
	}
	if err := st.RefuseManagedRealmDelete(ctx, provisioned.AccountID, first.ID); !errors.Is(err, ErrRealmEmailRouteRetirementRequired) {
		t.Fatalf("managed direct delete fence = %v", err)
	}
	page, err := st.ListRealmEmailRouteLifecycles(ctx, provisioned.AccountID, "", 1)
	if err != nil || len(page.Routes) != 1 || page.NextCursor == "" {
		t.Fatalf("first inventory page = %+v / %v", page, err)
	}
	next, err := st.ListRealmEmailRouteLifecycles(
		ctx, provisioned.AccountID, page.NextCursor, 1,
	)
	if err != nil || len(next.Routes) != 1 || next.NextCursor != "" ||
		next.Routes[0].RealmID == page.Routes[0].RealmID {
		t.Fatalf("second inventory page = %+v / %v", next, err)
	}
	if _, err := st.ListRealmEmailRouteLifecycles(
		ctx, provisioned.AccountID, "not-a-canonical-cursor", 1,
	); !errors.Is(err, ErrRealmEmailRouteInputInvalid) {
		t.Fatalf("invalid inventory cursor = %v", err)
	}

	agent, err := st.CreateAgent(ctx, provisioned.AccountID, first.ID, "blocker")
	if err != nil {
		t.Fatal(err)
	}
	prepare := RealmEmailRouteRetirementInput{
		RealmID: first.ID, OperationID: "close:customer.request-1", ExpectedGeneration: 1,
	}
	if _, err := st.PrepareRealmEmailRouteRetirement(
		ctx, provisioned.AccountID, prepare,
	); !errors.Is(err, ErrRealmEmailRouteConflict) {
		t.Fatalf("prepare with live agent = %v", err)
	}
	if err := st.DeleteAgent(ctx, provisioned.AccountID, first.ID, agent.ID); err != nil {
		t.Fatal(err)
	}
	alias := ApplyAgentEmailRealmAliasInput{
		ClaimID: "era_aaaaaaaaaaaaaaaa", RealmID: first.ID,
		Domain: "agent-mail.witwave.ai", RealmLabel: "close-test",
		State: AgentEmailRealmAliasApplied, ControllerRevision: 1,
	}
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PrepareRealmEmailRouteRetirement(
		ctx, provisioned.AccountID, prepare,
	); !errors.Is(err, ErrRealmEmailRouteConflict) {
		t.Fatalf("prepare with live alias = %v", err)
	}
	alias.State = AgentEmailRealmAliasRetired
	alias.ControllerRevision = 2
	if _, err := st.ApplyAgentEmailRealmAlias(ctx, provisioned.AccountID, alias); err != nil {
		t.Fatal(err)
	}

	closing, err := st.PrepareRealmEmailRouteRetirement(
		ctx, provisioned.AccountID, prepare,
	)
	if err != nil || closing.State != RealmEmailRouteClosing ||
		closing.Generation != 2 || closing.OperationID != prepare.OperationID {
		t.Fatalf("prepared route = %+v / %v", closing, err)
	}
	replayed, err := st.PrepareRealmEmailRouteRetirement(
		ctx, provisioned.AccountID, prepare,
	)
	if err != nil || replayed != closing {
		t.Fatalf("prepare replay = %+v / %v", replayed, err)
	}
	conflictingPrepare := prepare
	conflictingPrepare.OperationID = "close_other"
	if _, err := st.PrepareRealmEmailRouteRetirement(
		ctx, provisioned.AccountID, conflictingPrepare,
	); !errors.Is(err, ErrRealmEmailRouteConflict) {
		t.Fatalf("conflicting prepare = %v", err)
	}
	if _, err := st.CreateAgent(
		ctx, provisioned.AccountID, first.ID, "too-late",
	); !errors.Is(err, ErrRealmNotFound) {
		t.Fatalf("create agent after prepare = %v", err)
	}
	lateAlias := ApplyAgentEmailRealmAliasInput{
		ClaimID: "era_bbbbbbbbbbbbbbbb", RealmID: first.ID,
		Domain: alias.Domain, RealmLabel: "too-late",
		State: AgentEmailRealmAliasApplied, ControllerRevision: 1,
	}
	if _, err := st.ApplyAgentEmailRealmAlias(
		ctx, provisioned.AccountID, lateAlias,
	); !errors.Is(err, ErrRealmNotFound) {
		t.Fatalf("applied alias after prepare = %v", err)
	}

	commit := RealmEmailRouteRetirementInput{
		RealmID: first.ID, OperationID: prepare.OperationID, ExpectedGeneration: 2,
	}
	staleCommit := commit
	staleCommit.ExpectedGeneration = 3
	if _, err := st.CommitRealmEmailRouteRetirement(
		ctx, provisioned.AccountID, staleCommit,
	); !errors.Is(err, ErrRealmEmailRouteConflict) {
		t.Fatalf("stale commit = %v", err)
	}
	retired, err := st.CommitRealmEmailRouteRetirement(
		ctx, provisioned.AccountID, commit,
	)
	if err != nil || retired.State != RealmEmailRouteRetired ||
		retired.Generation != 2 || retired.OperationID != commit.OperationID {
		t.Fatalf("committed route = %+v / %v", retired, err)
	}
	replayed, err = st.CommitRealmEmailRouteRetirement(
		ctx, provisioned.AccountID, commit,
	)
	if err != nil || replayed != retired {
		t.Fatalf("commit replay = %+v / %v", replayed, err)
	}
	if err := st.RefuseManagedRealmDelete(
		ctx, provisioned.AccountID, first.ID,
	); !errors.Is(err, ErrRealmNotFound) {
		t.Fatalf("managed delete after commit = %v", err)
	}
	gotRetired, err := st.GetRealmEmailRouteLifecycle(
		ctx, provisioned.AccountID, first.ID,
	)
	if err != nil || gotRetired != retired {
		t.Fatalf("retired tombstone read = %+v / %v", gotRetired, err)
	}

	// Portable self-hosted installs keep their direct empty-realm delete.  It
	// writes the same terminal shape without requiring an external controller.
	if err := st.DeleteRealm(ctx, provisioned.AccountID, second.ID); err != nil {
		t.Fatalf("self-hosted direct delete: %v", err)
	}
	selfHosted, err := st.GetRealmEmailRouteLifecycle(
		ctx, provisioned.AccountID, second.ID,
	)
	if err != nil || selfHosted.State != RealmEmailRouteRetired ||
		selfHosted.Generation != 2 || selfHosted.OperationID != "selfhosted_delete" {
		t.Fatalf("self-hosted route tombstone = %+v / %v", selfHosted, err)
	}
}

func TestRealmEmailRoutePrepareSerializesCreateAndAliasRacesPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	replica, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replica.Close)
	provisioned, err := st.ProvisionAccount(
		ctx, "realm-route-races@witwave.ai", "route races", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if active, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !active {
		t.Fatalf("activate = %v / %v", active, err)
	}

	runAgentRace := func(t *testing.T, realm Realm) {
		t.Helper()
		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		var prepareErr, createErr error
		go func() {
			defer wg.Done()
			<-start
			_, prepareErr = st.PrepareRealmEmailRouteRetirement(ctx,
				provisioned.AccountID, RealmEmailRouteRetirementInput{
					RealmID: realm.ID, OperationID: "race_agent", ExpectedGeneration: 1,
				})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, createErr = replica.CreateAgent(ctx, provisioned.AccountID, realm.ID, "racer")
		}()
		close(start)
		wg.Wait()
		prepareWon := prepareErr == nil && errors.Is(createErr, ErrRealmNotFound)
		createWon := createErr == nil && errors.Is(prepareErr, ErrRealmEmailRouteConflict)
		if !prepareWon && !createWon {
			t.Fatalf("agent race prepare/create = %v / %v", prepareErr, createErr)
		}
	}
	agentRealm, err := st.CreateRealm(ctx, provisioned.AccountID, "agent race")
	if err != nil {
		t.Fatal(err)
	}
	runAgentRace(t, agentRealm)

	aliasRealm, err := st.CreateRealm(ctx, provisioned.AccountID, "alias race")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	start := make(chan struct{})
	var prepareErr, aliasErr error
	go func() {
		defer wg.Done()
		<-start
		_, prepareErr = st.PrepareRealmEmailRouteRetirement(ctx,
			provisioned.AccountID, RealmEmailRouteRetirementInput{
				RealmID: aliasRealm.ID, OperationID: "race_alias", ExpectedGeneration: 1,
			})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, aliasErr = replica.ApplyAgentEmailRealmAlias(ctx,
			provisioned.AccountID, ApplyAgentEmailRealmAliasInput{
				ClaimID: "era_cccccccccccccccc", RealmID: aliasRealm.ID,
				Domain: "agent-mail.witwave.ai", RealmLabel: "alias-race",
				State: AgentEmailRealmAliasApplied, ControllerRevision: 1,
			})
	}()
	close(start)
	wg.Wait()
	prepareWon := prepareErr == nil && errors.Is(aliasErr, ErrRealmNotFound)
	aliasWon := aliasErr == nil && errors.Is(prepareErr, ErrRealmEmailRouteConflict)
	if !prepareWon && !aliasWon {
		t.Fatalf("alias race prepare/apply = %v / %v", prepareErr, aliasErr)
	}
}

func TestRealmEmailRouteLifecycleArchiveRoundTripPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	source, _ := newMigrationTestStore(t, baseDSN)
	destination, _ := newMigrationTestStore(t, baseDSN)
	if err := source.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := destination.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := source.ProvisionAccount(
		ctx, "realm-route-archive@witwave.ai", "route archive", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if active, err := source.ActivateAccount(ctx, provisioned.AccountID); err != nil || !active {
		t.Fatalf("activate = %v / %v", active, err)
	}
	realm, err := source.CreateRealm(ctx, provisioned.AccountID, "portable closing")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := source.PrepareRealmEmailRouteRetirement(ctx,
		provisioned.AccountID, RealmEmailRouteRetirementInput{
			RealmID: realm.ID, OperationID: "portable_close", ExpectedGeneration: 1,
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SuspendAccountOwner(
		ctx, provisioned.AccountID, provisioned.OperatorID, "portable route test",
	); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := source.ExportAccount(
		ctx, provisioned.AccountID, "route-source", "test", &archive,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.ImportAccount(
		ctx, provisioned.AccountID, bytes.NewReader(archive.Bytes()),
	); err != nil {
		t.Fatal(err)
	}
	restored, err := destination.GetRealmEmailRouteLifecycle(
		ctx, provisioned.AccountID, realm.ID,
	)
	if err != nil || restored != prepared {
		t.Fatalf("restored closing route = %+v / %v; want %+v", restored, err, prepared)
	}
	committed, err := destination.CommitRealmEmailRouteRetirement(ctx,
		provisioned.AccountID, RealmEmailRouteRetirementInput{
			RealmID: realm.ID, OperationID: prepared.OperationID,
			ExpectedGeneration: prepared.Generation,
		})
	if err != nil || committed.State != RealmEmailRouteRetired {
		t.Fatalf("commit restored route = %+v / %v", committed, err)
	}
}
