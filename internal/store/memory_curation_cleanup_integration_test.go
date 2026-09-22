package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMemoryCurationOwnerDeleteCascadesGraphPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx,
		"curation-owner-cleanup@witwave.ai", "curation owner cleanup", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	targetAgent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "target")
	if err != nil {
		t.Fatal(err)
	}
	otherAgent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "other")
	if err != nil {
		t.Fatal(err)
	}
	principal := func(agentID string) Principal {
		return Principal{
			Kind: PrincipalAgent, ID: agentID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AccountStatus: "active",
		}
	}
	target := principal(targetAgent.ID)
	other := principal(otherAgent.ID)

	captureSource := func(p Principal, content, key string) MemoryMutationResult {
		t.Helper()
		result, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
			Content: content, Kind: "test",
			Evidence: []MemoryEvidenceInput{{
				Type: "system", ResolutionState: MemoryEvidenceUnavailable,
				TerminalReasonCode: "not_recorded",
			}},
			IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	targetSource := captureSource(target, "target source memory", "cleanup-target-source")
	captureSource(other, "other source memory", "cleanup-other-source")

	status, err := st.GetCurationStatus(ctx, target, "")
	if err != nil || status.Request == nil {
		t.Fatalf("target queued status = %#v / %v", status, err)
	}
	started, err := st.StartCuration(ctx, target, StartMemoryCurationInput{
		RequestID: status.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "cleanup-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := json.Marshal(MemoryCurationPlanDraft{
		Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1,
		Actions: []MemoryCurationPlanAction{{
			Ordinal: 1, Operation: MemoryCurationOperationCreate,
			Create: &MemoryCurationCreateAction{
				LocalRef: "cleanup_output",
				Snapshot: MemoryCurationMemorySnapshot{
					Content: "planned output that must not outlive its owner",
					Kind:    "test",
					Evidence: []MemoryCurationEvidence{{
						Type: "memory", ResolutionState: MemoryEvidenceResolved,
						ResolvedKind: MemoryCurationSourceMemory,
						SourceMemory: &MemoryCurationVersionReference{
							MemoryID: targetSource.Memory.ID, Version: targetSource.Memory.Version,
						},
					}},
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PlanCuration(ctx, target, started.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		Draft:             draft,
		IdempotencyKey:    "cleanup-plan",
	}); err != nil {
		t.Fatal(err)
	}

	targetBefore := memoryCurationOwnerGraphCounts(ctx, t, st, target.ID)
	for table, want := range map[string]int64{
		"memory_curation_lanes":     1,
		"memory_curation_requests":  1,
		"memory_curation_runs":      1,
		"memory_curation_actions":   1,
		"memory_curation_mutations": 3,
	} {
		if targetBefore[table] != want {
			t.Fatalf("target %s rows before delete = %d, want %d", table, targetBefore[table], want)
		}
	}
	otherBefore := memoryCurationOwnerGraphCounts(ctx, t, st, other.ID)
	if otherBefore["memory_curation_lanes"] != 1 ||
		otherBefore["memory_curation_requests"] != 1 ||
		otherBefore["memory_curation_mutations"] != 1 {
		t.Fatalf("other owner graph before delete = %#v", otherBefore)
	}

	targetUsageBefore := memoryCurationOwnerUsageCounts(ctx, t, st, target)
	otherUsageBefore := memoryCurationOwnerUsageCounts(ctx, t, st, other)
	for table, count := range targetUsageBefore {
		if count == 0 {
			t.Fatalf("target %s rows after capture = 0, want activity usage", table)
		}
	}
	for table, count := range otherUsageBefore {
		if count == 0 {
			t.Fatalf("other %s rows after capture = 0, want activity usage", table)
		}
	}

	// This fixture exercises physical owner removal, whereas production
	// DeleteAgent soft-deletes the agent and retains its usage ledger. Explicitly
	// remove the target's domain and usage rows before deleting the principal.
	// Curation inputs referencing immutable memory versions cascade with them;
	// the remaining planned graph must disappear through the lane ownership root.
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM memories
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3`,
		target.AccountID, target.RealmID, target.ID); err != nil {
		t.Fatalf("delete target memories before principal cleanup: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM memory_change_clocks
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3`,
		target.AccountID, target.RealmID, target.ID); err != nil {
		t.Fatalf("delete target memory clock before principal cleanup: %v", err)
	}
	remainingGraph := memoryCurationOwnerGraphCounts(ctx, t, st, target.ID)
	if remainingGraph["memory_curation_run_inputs"] == 0 ||
		remainingGraph["memory_curation_actions"] != 1 ||
		remainingGraph["memory_curation_runs"] != 1 {
		t.Fatalf("planned graph was not retained for owner-root cleanup: %#v", remainingGraph)
	}

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		DELETE FROM usage_rollups
		WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3`,
		target.AccountID, target.RealmID, target.ID); err != nil {
		t.Fatalf("delete target usage rollups before principal cleanup: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM usage_events
		WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3`,
		target.AccountID, target.RealmID, target.ID); err != nil {
		t.Fatalf("delete target usage events before principal cleanup: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agents WHERE id=$1`, target.ID); err != nil {
		t.Fatalf("delete curation owner: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit curation owner delete: %v", err)
	}

	for table, count := range memoryCurationOwnerGraphCounts(ctx, t, st, target.ID) {
		if count != 0 {
			t.Fatalf("target %s rows after delete = %d, want 0", table, count)
		}
	}
	otherAfter := memoryCurationOwnerGraphCounts(ctx, t, st, other.ID)
	if !reflect.DeepEqual(otherAfter, otherBefore) {
		t.Fatalf("other owner graph changed during target cleanup\nbefore: %#v\nafter:  %#v", otherBefore, otherAfter)
	}
	otherUsageAfter := memoryCurationOwnerUsageCounts(ctx, t, st, other)
	if !reflect.DeepEqual(otherUsageAfter, otherUsageBefore) {
		t.Fatalf("other owner usage changed during target cleanup\nbefore: %#v\nafter:  %#v", otherUsageBefore, otherUsageAfter)
	}
	var otherExists bool
	if err := st.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agents WHERE id=$1)`, other.ID).
		Scan(&otherExists); err != nil {
		t.Fatal(err)
	}
	if !otherExists {
		t.Fatal("other owner was removed by target cleanup")
	}
}

func memoryCurationOwnerUsageCounts(
	ctx context.Context,
	t *testing.T,
	st *Store,
	owner Principal,
) map[string]int64 {
	t.Helper()
	counts := make(map[string]int64, 2)
	for _, table := range []string{"usage_events", "usage_rollups"} {
		var count int64
		if err := st.pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3`, table),
			owner.AccountID, owner.RealmID, owner.ID,
		).Scan(&count); err != nil {
			t.Fatalf("count %s for owner %s: %v", table, owner.ID, err)
		}
		counts[table] = count
	}
	return counts
}

func memoryCurationOwnerGraphCounts(
	ctx context.Context,
	t *testing.T,
	st *Store,
	ownerID string,
) map[string]int64 {
	t.Helper()
	tables := []string{
		"memory_curation_lanes",
		"memory_curation_cursors",
		"memory_curation_requests",
		"memory_curation_runs",
		"memory_curation_run_inputs",
		"memory_curation_actions",
		"memory_curation_mutations",
	}
	counts := make(map[string]int64, len(tables))
	for _, table := range tables {
		var count int64
		if err := st.pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE owner_id=$1`, table), ownerID,
		).Scan(&count); err != nil {
			t.Fatalf("count %s for owner %s: %v", table, ownerID, err)
		}
		counts[table] = count
	}
	return counts
}
