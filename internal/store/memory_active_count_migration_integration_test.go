package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestActiveMemoryCountSchema74BackfillPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
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

func TestActiveMemoryCountSchema75ReconcilePostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 74)

	provisioned, err := st.ProvisionAccount(
		ctx, "memory-count-schema-75@witwave.ai", "memory count schema 75", time.Hour,
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
	activeOwner, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "active and forgotten")
	if err != nil {
		t.Fatal(err)
	}
	repairedOwner, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "missing clock")
	if err != nil {
		t.Fatal(err)
	}
	inactiveOwner, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "zero active")
	if err != nil {
		t.Fatal(err)
	}
	principal := func(owner Agent) Principal {
		return Principal{
			Kind: PrincipalAgent, ID: owner.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AccountStatus: "active",
		}
	}
	capture := func(owner Principal, key string) MemoryMutationResult {
		t.Helper()
		out, err := st.CaptureMemory(ctx, owner, CaptureMemoryInput{
			Content: "schema 75 " + key, Kind: "note",
			Evidence: []MemoryEvidenceInput{{
				ResolutionState:    MemoryEvidenceUnavailable,
				TerminalReasonCode: "runtime_did_not_record",
			}},
			IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	activePrincipal := principal(activeOwner)
	capture(activePrincipal, "schema-75-active")
	toForget := capture(activePrincipal, "schema-75-forgotten")
	if _, err := st.ForgetMemory(ctx, activePrincipal, toForget.Memory.ID, MemoryLifecycleInput{
		ExpectedVersion: toForget.Memory.Version,
		IdempotencyKey:  "schema-75-forget-active-owner",
	}); err != nil {
		t.Fatal(err)
	}
	capture(principal(repairedOwner), "schema-75-repaired-owner")
	inactivePrincipal := principal(inactiveOwner)
	inactive := capture(inactivePrincipal, "schema-75-inactive-owner")
	if _, err := st.ForgetMemory(ctx, inactivePrincipal, inactive.Memory.ID, MemoryLifecycleInput{
		ExpectedVersion: inactive.Memory.Version,
		IdempotencyKey:  "schema-75-forget-inactive-owner",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_change_clocks
		   SET active_memory_count = CASE owner_id
		         WHEN $3 THEN 17
		         WHEN $4 THEN 9
		         ELSE active_memory_count
		       END
		 WHERE account_id=$1 AND realm_id=$2
		   AND owner_kind='agent' AND owner_id IN ($3,$4)`,
		provisioned.AccountID, realm.ID, activeOwner.ID, inactiveOwner.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM memory_change_clocks
		 WHERE account_id=$1 AND realm_id=$2
		   AND owner_kind='agent' AND owner_id=$3`,
		provisioned.AccountID, realm.ID, repairedOwner.ID,
	); err != nil {
		t.Fatal(err)
	}

	migrationTestUpTo(t, dsn, 75)
	assertMigrationTestVersion(t, dsn, 75)

	assertOwner := func(ownerID string, wantCount, wantLastSeq int64) {
		t.Helper()
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
			           AND m.current_version IS NOT NULL
			           AND v.state='active')
			  FROM memory_change_clocks clock
			 WHERE clock.account_id=$1 AND clock.realm_id=$2
			   AND clock.owner_kind='agent' AND clock.owner_id=$3`,
			provisioned.AccountID, realm.ID, ownerID,
		).Scan(&persisted, &lastChangeSeq, &derived); err != nil {
			t.Fatal(err)
		}
		if persisted != wantCount || derived != wantCount || lastChangeSeq != wantLastSeq {
			t.Fatalf(
				"owner %s active count persisted=%d derived=%d last_change_seq=%d, want %d/%d/%d",
				ownerID, persisted, derived, lastChangeSeq,
				wantCount, wantCount, wantLastSeq,
			)
		}
	}
	assertOwner(activeOwner.ID, 1, 5)
	assertOwner(repairedOwner.ID, 1, 2)
	assertOwner(inactiveOwner.ID, 0, 3)

	var missingClockCount, mismatchedCount int64
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*)
		     FROM memories m
		     JOIN memory_versions v
		       ON v.memory_id=m.id AND v.version=m.current_version
		     LEFT JOIN memory_change_clocks clock
		       ON clock.account_id=m.account_id
		      AND clock.realm_id=m.realm_id
		      AND clock.owner_kind=m.owner_kind
		      AND clock.owner_id=m.owner_id
		    WHERE m.current_version IS NOT NULL
		      AND v.state='active' AND clock.owner_id IS NULL),
		  (SELECT count(*)
		     FROM memory_change_clocks clock
		    WHERE clock.active_memory_count <> (
		      SELECT count(*)
		        FROM memories m
		        JOIN memory_versions v
		          ON v.memory_id=m.id AND v.version=m.current_version
		       WHERE m.account_id=clock.account_id
		         AND m.realm_id=clock.realm_id
		         AND m.owner_kind=clock.owner_kind
		         AND m.owner_id=clock.owner_id
		         AND m.current_version IS NOT NULL
		         AND v.state='active'
		    ))`,
	).Scan(&missingClockCount, &mismatchedCount); err != nil {
		t.Fatal(err)
	}
	if missingClockCount != 0 || mismatchedCount != 0 {
		t.Fatalf("schema 75 invariant missing=%d mismatched=%d",
			missingClockCount, mismatchedCount)
	}

	migrationTestDownTo(t, dsn, 74)
	assertMigrationTestVersion(t, dsn, 74)
	assertOwner(activeOwner.ID, 1, 5)
	assertOwner(repairedOwner.ID, 1, 2)
	assertOwner(inactiveOwner.ID, 0, 3)

	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_change_clocks
		   SET active_memory_count=7
		 WHERE account_id=$1 AND realm_id=$2
		   AND owner_kind='agent' AND owner_id=$3`,
		provisioned.AccountID, realm.ID, activeOwner.ID,
	); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 75)
	assertMigrationTestVersion(t, dsn, 75)
	assertOwner(activeOwner.ID, 1, 5)
}

func TestActiveMemoryCountSchema75FenceRetryPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 74)

	provisioned, err := st.ProvisionAccount(
		ctx, "memory-count-schema-75-fence@witwave.ai",
		"memory count schema 75 fence", time.Hour,
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
	owner, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "writer")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{
		Kind: PrincipalAgent, ID: owner.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}
	if _, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "schema 75 preexisting active memory", Kind: "note",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:    MemoryEvidenceUnavailable,
			TerminalReasonCode: "runtime_did_not_record",
		}},
		IdempotencyKey: "schema-75-fence-preexisting",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_change_clocks
		   SET active_memory_count=17
		 WHERE account_id=$1 AND realm_id=$2
		   AND owner_kind='agent' AND owner_id=$3`,
		provisioned.AccountID, realm.ID, owner.ID,
	); err != nil {
		t.Fatal(err)
	}

	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	var changeSeq, evidenceSeq int64
	if err := writer.QueryRow(ctx, `
		UPDATE memory_change_clocks
		   SET last_change_seq=last_change_seq+2,
		       active_memory_count=active_memory_count+1
		 WHERE account_id=$1 AND realm_id=$2
		   AND owner_kind='agent' AND owner_id=$3
		RETURNING last_change_seq-1,last_change_seq`,
		provisioned.AccountID, realm.ID, owner.ID,
	).Scan(&changeSeq, &evidenceSeq); err != nil {
		t.Fatal(err)
	}
	const memoryID = "mem_dddddddddddddddd"
	const evidenceID = "mev_dddddddddddddddd"
	if _, err := writer.Exec(ctx, `
		INSERT INTO memories
		  (id,account_id,realm_id,owner_kind,owner_id,origin,
		   capture_reason,authored_by_agent_id,current_version)
		VALUES($1,$2,$3,'agent',$4,'manual','manual',$4,1)`,
		memoryID, provisioned.AccountID, realm.ID, owner.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(ctx, `
		INSERT INTO memory_versions
		  (memory_id,version,account_id,realm_id,owner_kind,owner_id,
		   change_seq,content,kind,state,content_hash,actor_kind,actor_id,
		   operation,idempotency_key,request_hash)
		VALUES($1,1,$2,$3,'agent',$4,$5,$6,'note','active',$7,
		       'agent',$4,'added',$8,$9)`,
		memoryID, provisioned.AccountID, realm.ID, owner.ID, changeSeq,
		"schema 75 in-flight active memory", strings.Repeat("c", 64),
		"schema-75-fence-in-flight", strings.Repeat("d", 64),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(ctx, `
		INSERT INTO memory_evidence
		  (id,account_id,realm_id,owner_kind,owner_id,memory_id,
		   target_version,evidence_change_seq,evidence_type,role,
		   resolution_state,terminal_reason_code,actor_id)
		VALUES($1,$2,$3,'agent',$4,$5,1,$6,'manual','supports',
		       'unavailable','runtime_did_not_record',$4)`,
		evidenceID, provisioned.AccountID, realm.ID, owner.ID, memoryID, evidenceSeq,
	); err != nil {
		t.Fatal(err)
	}

	migrationDB := migrationTestSQLDB(t, dsn)
	err = migrationTestUpToDB(migrationDB, 75)
	_ = migrationDB.Close()
	var lockErr *pgconn.PgError
	if !errors.As(err, &lockErr) || lockErr.Code != "55P03" {
		t.Fatalf("schema 75 migration with active writer error=%v, want SQLSTATE 55P03", err)
	}
	assertMigrationTestVersion(t, dsn, 74)

	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 75)
	assertMigrationTestVersion(t, dsn, 75)

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
		           AND m.current_version IS NOT NULL
		           AND v.state='active')
		  FROM memory_change_clocks clock
		 WHERE clock.account_id=$1 AND clock.realm_id=$2
		   AND clock.owner_kind='agent' AND clock.owner_id=$3`,
		provisioned.AccountID, realm.ID, owner.ID,
	).Scan(&persisted, &lastChangeSeq, &derived); err != nil {
		t.Fatal(err)
	}
	if persisted != 2 || derived != 2 || lastChangeSeq != evidenceSeq {
		t.Fatalf("post-retry active count persisted=%d derived=%d last_change_seq=%d, want 2/2/%d",
			persisted, derived, lastChangeSeq, evidenceSeq)
	}
}
