package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
)

func TestActiveFactCountSchema76And77BackfillDownAndInvariantPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 77)

	account, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("stored-fact-schema-76-77-%d@witwave.ai", time.Now().UnixNano()),
		"stored fact schema 76 and 77",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	newOwner := func(name string) Principal {
		t.Helper()
		agent, createErr := st.CreateAgent(ctx, account.AccountID, realm.ID, name)
		if createErr != nil {
			t.Fatal(createErr)
		}
		return Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
			RealmID: realm.ID, AccountStatus: "active",
		}
	}
	firstOwner := newOwner("first")
	secondOwner := newOwner("second")

	if _, err := st.SetFact(ctx, firstOwner, SetFactInput{
		Predicate: "preferences/active", Value: json.RawMessage(`"active"`),
		SourceKind: FactSourceAgent,
	}); err != nil {
		t.Fatal(err)
	}
	unresolved, err := st.SetFact(ctx, firstOwner, SetFactInput{
		Predicate: "preferences/unresolved", Value: json.RawMessage(`"unresolved"`),
		SourceKind: FactSourceAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := st.SetFact(ctx, firstOwner, SetFactInput{
		Predicate: "preferences/deleted", Value: json.RawMessage(`"deleted"`),
		SourceKind: FactSourceAgent, IdempotencyKey: "schema-76-delete-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := st.DeleteFact(ctx, firstOwner, DeleteFactInput{FactID: deleted.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteFact(ctx, firstOwner, DeleteFactInput{
		FactID:                      deleted.ID,
		ExpectedResolvedAssertionID: preview.PriorResolvedAssertionID,
		ExpectedCandidateRevision:   preview.CandidateRevision,
		IdempotencyKey:              "schema-76-delete",
		Apply:                       true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetFact(ctx, secondOwner, SetFactInput{
		Predicate: "preferences/active", Value: json.RawMessage(`"also active"`),
		SourceKind: FactSourceAgent,
	}); err != nil {
		t.Fatal(err)
	}

	// This simulates a legacy unresolved address and deliberately damages both
	// projections. Migration 77 must derive from canonical rows rather than
	// trusting the pre-upgrade counters.
	if _, err := st.pool.Exec(ctx, `
		UPDATE facts SET resolved_assertion_id=NULL WHERE id=$1`,
		unresolved.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agents
		   SET active_fact_count=CASE id WHEN $1 THEN 17 WHEN $2 THEN 9 END
		 WHERE id IN ($1,$2)`,
		firstOwner.ID, secondOwner.ID,
	); err != nil {
		t.Fatal(err)
	}

	migrationTestDownTo(t, dsn, 75)
	assertMigrationTestVersion(t, dsn, 75)
	var columnCount int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema=current_schema()
		   AND table_name='agents'
		   AND column_name='active_fact_count'`,
	).Scan(&columnCount); err != nil {
		t.Fatal(err)
	}
	if columnCount != 0 {
		t.Fatalf("schema 75 retained agents.active_fact_count column count=%d", columnCount)
	}

	migrationTestUpTo(t, dsn, 76)
	assertMigrationTestVersion(t, dsn, 76)
	for _, ownerID := range []string{firstOwner.ID, secondOwner.ID} {
		var count int64
		if err := st.pool.QueryRow(ctx, `
			SELECT active_fact_count FROM agents WHERE id=$1`,
			ownerID,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("schema 76 performed the separated backfill for %s: count=%d",
				ownerID, count)
		}
	}
	var rangeValidated bool
	if err := st.pool.QueryRow(ctx, `
		SELECT convalidated
		  FROM pg_constraint
		 WHERE conname='agents_active_fact_count_range'
		   AND conrelid='agents'::regclass`,
	).Scan(&rangeValidated); err != nil {
		t.Fatal(err)
	}
	if rangeValidated {
		t.Fatal("schema 76 validated the range constraint under the short DDL lock")
	}

	migrationTestUpTo(t, dsn, 77)
	assertMigrationTestVersion(t, dsn, 77)
	if err := st.pool.QueryRow(ctx, `
		SELECT convalidated
		  FROM pg_constraint
		 WHERE conname='agents_active_fact_count_range'
		   AND conrelid='agents'::regclass`,
	).Scan(&rangeValidated); err != nil {
		t.Fatal(err)
	}
	if !rangeValidated {
		t.Fatal("schema 77 left the active fact count range constraint unvalidated")
	}
	assertStoredFactCountInvariant(ctx, t, st, firstOwner, 1)
	assertStoredFactCountInvariant(ctx, t, st, secondOwner, 1)

	if _, err := st.pool.Exec(ctx, `
		UPDATE agents SET active_fact_count=-1 WHERE id=$1`,
		firstOwner.ID,
	); err == nil {
		t.Fatal("schema 76 accepted a negative active_fact_count")
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agents SET active_fact_count=$2 WHERE id=$1`,
		firstOwner.ID, maxActiveFactCount+1,
	); err == nil {
		t.Fatal("schema 76 accepted active_fact_count above the implementation bound")
	}

	var mismatched int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM agents owner
		  JOIN realms realm ON realm.id=owner.realm_id
		 WHERE realm.account_id=$1
		   AND owner.active_fact_count <> (
		     SELECT count(*)
		       FROM facts fact
		      WHERE fact.account_id=$1
		        AND fact.owner_agent_id=owner.id
		        AND fact.deleted_at IS NULL
		        AND fact.resolved_assertion_id IS NOT NULL
		   )`,
		account.AccountID,
	).Scan(&mismatched); err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 {
		t.Fatalf("schema 77 left %d active fact count mismatches", mismatched)
	}
}

func TestActiveFactCountArchiveRestoreIsDerivedAndPreservesOverLimitPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	account, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("stored-fact-archive-%d@witwave.ai", time.Now().UnixNano()),
		"stored fact archive",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "archivist")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}
	active, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "preferences/kept", Value: json.RawMessage(`"keep"`),
		SourceKind: FactSourceAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	toDelete, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "preferences/deleted", Value: json.RawMessage(`"delete"`),
		SourceKind: FactSourceAgent, IdempotencyKey: "archive-count-delete-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := st.DeleteFact(ctx, p, DeleteFactInput{FactID: toDelete.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteFact(ctx, p, DeleteFactInput{
		FactID:                      toDelete.ID,
		ExpectedResolvedAssertionID: preview.PriorResolvedAssertionID,
		ExpectedCandidateRevision:   preview.CandidateRevision,
		IdempotencyKey:              "archive-count-delete",
		Apply:                       true,
	}); err != nil {
		t.Fatal(err)
	}
	assertStoredFactCountInvariant(ctx, t, st, p, 1)

	// Lowering to zero is allowed and must survive a move without dropping the
	// current fact. A corrupted local projection must not cross the archive.
	setStoredFactLimitPlan(ctx, t, st, account.AccountID, 1, ptrInt64(0))
	if _, err := st.pool.Exec(ctx, `
		UPDATE agents SET active_fact_count=41 WHERE id=$1`,
		p.ID,
	); err != nil {
		t.Fatal(err)
	}
	if err := st.SuspendAccountSystem(
		ctx, account.AccountID, "evacuation", "active fact count archive test",
	); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := st.ExportAccount(
		ctx, account.AccountID, "test-source", "test", &archive,
	); err != nil {
		t.Fatal(err)
	}
	agentSeen := false
	if _, err := archiveexport.Read(
		ctx,
		bytes.NewReader(archive.Bytes()),
		archiveexport.ImportOptions{
			CurrentSchema: SchemaVersion(),
			Row: func(table string, row []byte) error {
				if table != "agents" {
					return nil
				}
				var object map[string]json.RawMessage
				if err := json.Unmarshal(row, &object); err != nil {
					return err
				}
				var agentID string
				if err := json.Unmarshal(object["id"], &agentID); err != nil {
					return err
				}
				if agentID != p.ID {
					return nil
				}
				agentSeen = true
				if _, archived := object["active_fact_count"]; archived {
					return fmt.Errorf("derived agents.active_fact_count was archived")
				}
				return nil
			},
		},
	); err != nil {
		t.Fatal(err)
	}
	if !agentSeen {
		t.Fatal("archive omitted the owner agent")
	}

	if err := deleteFactTestAccount(ctx, st, account.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportAccount(
		ctx, account.AccountID, bytes.NewReader(archive.Bytes()),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.ResumeAccountSystem(ctx, account.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}

	assertStoredFactCountInvariant(ctx, t, st, p, 1)
	assertStoredFactStatus(ctx, t, st, p, 1, ptrInt64(0), false, false, true)
	restored, err := st.GetFact(ctx, p, "self", active.Predicate)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != active.ID || restored.ResolvedAssertionID != active.ResolvedAssertionID {
		t.Fatalf("restore changed active fact: got %#v want %#v", restored, active)
	}
	if _, err := st.SetFact(ctx, p, SetFactInput{
		Predicate:  "preferences/blocked-after-restore",
		Value:      json.RawMessage(`"blocked"`),
		SourceKind: FactSourceAgent,
	}); err == nil {
		t.Fatal("over-limit restore admitted a new fact")
	} else {
		assertStoredFactLimitError(t, err, 1, 0)
	}
}
