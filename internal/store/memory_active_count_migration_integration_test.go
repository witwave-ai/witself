package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestActiveMemoryCountSchema74BackfillPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 73)

	provisioned, err := st.ProvisionAccount(
		ctx, "memory-count-schema-74@witwave.ai", "memory count schema 74", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "with clock")
	if err != nil {
		t.Fatal(err)
	}
	repairedOwner, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "missing clock")
	if err != nil {
		t.Fatal(err)
	}

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_change_clocks
		  (account_id,realm_id,owner_kind,owner_id,last_change_seq)
		VALUES($1,$2,'agent',$3,4)`,
		provisioned.AccountID, realm.ID, owner.ID,
	); err != nil {
		t.Fatal(err)
	}

	type memoryFixture struct {
		memoryID    string
		evidenceID  string
		ownerID     string
		state       string
		priorState  any
		changeSeq   int64
		evidenceSeq int64
		key         string
	}
	fixtures := []memoryFixture{
		{
			memoryID: "mem_aaaaaaaaaaaaaaaa", evidenceID: "mev_aaaaaaaaaaaaaaaa",
			ownerID: owner.ID, state: MemoryStateActive,
			changeSeq: 1, evidenceSeq: 2, key: "schema-74-active",
		},
		{
			memoryID: "mem_bbbbbbbbbbbbbbbb", evidenceID: "mev_bbbbbbbbbbbbbbbb",
			ownerID: owner.ID, state: MemoryStateForgotten, priorState: MemoryStateActive,
			changeSeq: 3, evidenceSeq: 4, key: "schema-74-forgotten",
		},
		{
			memoryID: "mem_cccccccccccccccc", evidenceID: "mev_cccccccccccccccc",
			ownerID: repairedOwner.ID, state: MemoryStateActive,
			changeSeq: 1, evidenceSeq: 2, key: "schema-74-repaired-owner",
		},
	}
	for _, fixture := range fixtures {
		if _, err := tx.Exec(ctx, `
			INSERT INTO memories
			  (id,account_id,realm_id,owner_kind,owner_id,origin,
			   capture_reason,authored_by_agent_id,current_version)
			VALUES($1,$2,$3,'agent',$4,'manual','manual',$4,1)`,
			fixture.memoryID, provisioned.AccountID, realm.ID, fixture.ownerID,
		); err != nil {
			t.Fatal(err)
		}
		operation := "added"
		if fixture.state == MemoryStateForgotten {
			operation = "forgotten"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO memory_versions
			  (memory_id,version,account_id,realm_id,owner_kind,owner_id,
			   change_seq,content,kind,state,prior_state,content_hash,
			   actor_kind,actor_id,operation,idempotency_key,request_hash)
			VALUES($1,1,$2,$3,'agent',$4,$5,$6,'note',$7,$8,$9,
			       'agent',$4,$10,$11,$12)`,
			fixture.memoryID, provisioned.AccountID, realm.ID, fixture.ownerID,
			fixture.changeSeq, "schema 74 "+fixture.key, fixture.state,
			fixture.priorState, strings.Repeat("a", 64), operation, fixture.key,
			strings.Repeat("b", 64),
		); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO memory_evidence
			  (id,account_id,realm_id,owner_kind,owner_id,memory_id,
			   target_version,evidence_change_seq,evidence_type,role,
			   resolution_state,terminal_reason_code,actor_id)
			VALUES($1,$2,$3,'agent',$4,$5,1,$6,'manual','supports',
			       'unavailable','runtime_did_not_record',$4)`,
			fixture.evidenceID, provisioned.AccountID, realm.ID, fixture.ownerID,
			fixture.memoryID, fixture.evidenceSeq,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	migrationTestUpTo(t, dsn, 74)
	assertMigrationTestVersion(t, dsn, 74)
	for _, testCase := range []struct {
		ownerID string
		want    int64
	}{
		{ownerID: owner.ID, want: 1},
		{ownerID: repairedOwner.ID, want: 1},
	} {
		var persisted, derived, lastChangeSeq int64
		if err := st.pool.QueryRow(ctx, `
			SELECT clock.active_memory_count,clock.last_change_seq,
			       (SELECT count(*)
			          FROM memories m
			          JOIN memory_versions v
			            ON v.memory_id=m.id AND v.version=m.current_version
			         WHERE m.account_id=clock.account_id
			           AND m.realm_id=clock.realm_id
			           AND m.owner_kind=clock.owner_kind
			           AND m.owner_id=clock.owner_id
			           AND v.state='active')
			  FROM memory_change_clocks clock
			 WHERE clock.account_id=$1 AND clock.realm_id=$2
			   AND clock.owner_kind='agent' AND clock.owner_id=$3`,
			provisioned.AccountID, realm.ID, testCase.ownerID,
		).Scan(&persisted, &lastChangeSeq, &derived); err != nil {
			t.Fatal(err)
		}
		if persisted != testCase.want || derived != testCase.want {
			t.Fatalf("owner %s active count persisted=%d derived=%d want=%d",
				testCase.ownerID, persisted, derived, testCase.want)
		}
		if testCase.ownerID == repairedOwner.ID && lastChangeSeq != 2 {
			t.Fatalf("repaired owner last_change_seq=%d want=2", lastChangeSeq)
		}
	}

	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_change_clocks SET active_memory_count=-1
		WHERE account_id=$1 AND realm_id=$2 AND owner_id=$3`,
		provisioned.AccountID, realm.ID, owner.ID,
	); err == nil {
		t.Fatal("schema 74 accepted a negative active_memory_count")
	}

	migrationTestDownTo(t, dsn, 73)
	assertMigrationTestVersion(t, dsn, 73)
	var columnCount int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema=current_schema()
		  AND table_name='memory_change_clocks'
		  AND column_name='active_memory_count'`).Scan(&columnCount); err != nil {
		t.Fatal(err)
	}
	if columnCount != 0 {
		t.Fatalf("schema 73 retained active_memory_count column count=%d", columnCount)
	}
}
