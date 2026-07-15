package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMemoryPermanentDeletionPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx,
		"memory-delete@witwave.ai", "memory delete", time.Hour)
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
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "primary")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}

	const secret = "private narrative secret 9c78d3"
	capture := CaptureMemoryInput{
		Content: secret, Kind: "decision", Sensitive: true,
		CaptureReason: "explicit_remember",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidencePending,
			ExternalLocator: "codex://private-secret-locator-9c78d3",
		}},
		IdempotencyKey: "capture-private-memory-9c78d3",
	}
	created, err := st.CaptureMemory(ctx, p, capture)
	if err != nil {
		t.Fatal(err)
	}
	pendingID := created.Memory.Evidence[0].ID
	updatedContent := secret + " updated"
	adjust := AdjustMemoryInput{
		ExpectedVersion: 1, Content: &updatedContent,
		Reason: "private correction", IdempotencyKey: "adjust-private-memory-9c78d3",
	}
	adjusted, err := st.AdjustMemory(ctx, p, created.Memory.ID, adjust)
	if err != nil {
		t.Fatal(err)
	}
	preResolutionPreview, err := st.DeleteMemory(ctx, p, DeleteMemoryInput{MemoryID: created.Memory.ID})
	if err != nil {
		t.Fatal(err)
	}
	resolve := ResolveMemoryEvidenceInput{
		UnresolvableReason: "source_expired",
		IdempotencyKey:     "resolve-private-memory-9c78d3",
	}
	if _, err := st.ResolveMemoryEvidence(ctx, p, pendingID, resolve); err != nil {
		t.Fatal(err)
	}
	if _, err := applyMemoryDeletePreview(ctx, st, p, preResolutionPreview, "stale-evidence-set-delete"); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("stale evidence scrub set = %v", err)
	}

	dependent, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "depends on target", Kind: "note",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:     MemoryEvidenceResolved,
			ResolvedKind:        "memory",
			SourceMemoryID:      created.Memory.ID,
			SourceMemoryVersion: adjusted.Memory.Version,
		}},
		IdempotencyKey: "capture-delete-dependent",
	})
	if err != nil {
		t.Fatal(err)
	}
	blockedByEvidence, err := st.DeleteMemory(ctx, p, DeleteMemoryInput{MemoryID: created.Memory.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !blockedByEvidence.Blocked || blockedByEvidence.IncomingEvidenceCount != 1 {
		t.Fatalf("incoming evidence preview = %#v", blockedByEvidence)
	}
	if _, err := applyMemoryDeletePreview(ctx, st, p, blockedByEvidence, "blocked-evidence-delete"); !errors.Is(err, ErrMemoryDependency) {
		t.Fatalf("incoming evidence apply = %v", err)
	}
	dependentPreview, err := st.DeleteMemory(ctx, p, DeleteMemoryInput{MemoryID: dependent.Memory.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyMemoryDeletePreview(ctx, st, p, dependentPreview, "delete-dependent-memory"); err != nil {
		t.Fatal(err)
	}

	relationOwner, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "active replacement lineage", Kind: "note",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:    MemoryEvidenceUnavailable,
			TerminalReasonCode: "runtime_did_not_record",
		}},
		IdempotencyKey: "capture-delete-relation-owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	const (
		relationID        = "mrel_hhhhhhhhhhhhhhhh"
		supersessionSetID = "mset_hhhhhhhhhhhhhhhh"
	)
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO memory_relations
		  (id,account_id,realm_id,owner_kind,owner_id,from_memory_id,from_version,
		   to_memory_id,to_version,relation_type,supersession_set_id,supersession_set_revision)
		VALUES($1,$2,$3,'agent',$4,$5,1,$6,$7,'supersedes',$8,1)`,
		relationID, p.AccountID, p.RealmID, p.ID, relationOwner.Memory.ID,
		created.Memory.ID, adjusted.Memory.Version, supersessionSetID); err != nil {
		t.Fatal(err)
	}
	blockedByRelation, err := st.DeleteMemory(ctx, p, DeleteMemoryInput{MemoryID: created.Memory.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !blockedByRelation.Blocked || blockedByRelation.ActiveRelationDependencyCount != 1 {
		t.Fatalf("active relation preview = %#v", blockedByRelation)
	}
	if _, err := applyMemoryDeletePreview(ctx, st, p, blockedByRelation, "blocked-relation-delete"); !errors.Is(err, ErrMemoryDependency) {
		t.Fatalf("active relation apply = %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_relations SET reverted_at=clock_timestamp()
		WHERE id=$1`, relationID); err != nil {
		t.Fatal(err)
	}

	curationTx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = curationTx.Rollback(ctx) }()
	curationHash := strings.Repeat("c", 64)
	execCuration := func(query string, args ...any) {
		t.Helper()
		if _, execErr := curationTx.Exec(ctx, query, args...); execErr != nil {
			t.Fatal(execErr)
		}
	}
	// Capture/adjust writes already coalesced automatic curation work. Replace
	// that incidental graph with the targeted deletion dependency fixture.
	execCuration(`DELETE FROM memory_curation_lanes
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3`,
		p.AccountID, p.RealmID, p.ID)
	execCuration(`
		INSERT INTO memory_curation_lanes
		  (account_id,realm_id,owner_kind,owner_id,request_generation,
		   fencing_generation,active_run_id)
		VALUES($1,$2,'agent',$3,1,1,'mrun_cccccccccccccccc')`,
		p.AccountID, p.RealmID, p.ID)
	execCuration(`INSERT INTO memory_curation_requests
		  (id,account_id,realm_id,owner_kind,owner_id,scope,coalescing_key,
		   trigger_reason,request_generation,state,claimed_run_id,actor_id,
		   idempotency_key,request_hash,claimed_at)
		VALUES('mcrq_cccccccccccccccc',$1,$2,'agent',$3,'{}','delete-blocker',
		       'manual',1,'claimed','mrun_cccccccccccccccc',$3,
		       'delete-curation-request',$4,now())`,
		p.AccountID, p.RealmID, p.ID, strings.Repeat("d", 64))
	execCuration(`INSERT INTO memory_curation_runs
		  (id,account_id,realm_id,owner_kind,owner_id,request_id,
		   request_generation,fencing_generation,state,actor_id,idempotency_key,
		   request_hash,lease_expires_at,input_count,memory_input_count,
		   plan_schema,plan_revision,plan_hash,planned_at)
		VALUES('mrun_cccccccccccccccc',$1,$2,'agent',$3,
		       'mcrq_cccccccccccccccc',1,1,'planned',$3,'delete-curation-start',
		       $4,now()+interval '1 hour',1,1,'witself.memory-plan.v1',1,$5,now())`,
		p.AccountID, p.RealmID, p.ID, strings.Repeat("d", 64), curationHash)
	execCuration(`INSERT INTO memory_curation_run_inputs
		  (run_id,ordinal,account_id,realm_id,owner_kind,owner_id,input_kind,
		   order_key,memory_id,memory_version)
		VALUES('mrun_cccccccccccccccc',1,$1,$2,'agent',$3,'memory',
		       'memory/target',$4,$5)`,
		p.AccountID, p.RealmID, p.ID, created.Memory.ID, adjusted.Memory.Version)
	execCuration(`INSERT INTO memory_curation_actions
		  (id,run_id,account_id,realm_id,owner_kind,owner_id,ordinal,
		   plan_revision,primitive,state,proposed_payload,action_hash,validated_at)
		VALUES('mact_cccccccccccccccc','mrun_cccccccccccccccc',$1,$2,'agent',$3,
		       1,1,'replace','validated',jsonb_build_object('content',$4::text),$5,now())`,
		p.AccountID, p.RealmID, p.ID, secret, curationHash)
	execCuration(`INSERT INTO memory_curation_mutations
		  (id,account_id,realm_id,owner_kind,owner_id,actor_id,operation,
		   idempotency_key,request_hash,request_id,run_id,request_generation,
		   fencing_generation,plan_revision,plan_hash,result_state)
		VALUES('mcmu_cccccccccccccccc',$1,$2,'agent',$3,$3,'plan',
		       'delete-curation-plan',$4,'mcrq_cccccccccccccccc',
		       'mrun_cccccccccccccccc',1,1,1,$5,'planned')`,
		p.AccountID, p.RealmID, p.ID, strings.Repeat("d", 64), curationHash)
	if err := curationTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	blockedByCuration, err := st.DeleteMemory(ctx, p, DeleteMemoryInput{MemoryID: created.Memory.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !blockedByCuration.Blocked || blockedByCuration.ActiveCurationDependencyCount != 1 {
		t.Fatalf("active curation preview = %#v", blockedByCuration)
	}
	if _, err := applyMemoryDeletePreview(ctx, st, p, blockedByCuration, "blocked-curation-delete"); !errors.Is(err, ErrMemoryDependency) {
		t.Fatalf("active curation apply = %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_curation_lanes
		SET active_run_id=NULL,fencing_generation=2,updated_at=now()
		WHERE account_id=$1 AND realm_id=$2 AND owner_id=$3`,
		p.AccountID, p.RealmID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_curation_requests
		SET state='retry_wait',claimed_run_id=NULL,claimed_at=NULL,due_at=now(),updated_at=now()
		WHERE id='mcrq_cccccccccccccccc'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_curation_runs
		SET state='interrupted',lease_expires_at=NULL,
		    terminal_reason_code='lease_expired',terminal_at=now(),updated_at=now()
		WHERE id='mrun_cccccccccccccccc'`); err != nil {
		t.Fatal(err)
	}

	preview, err := st.DeleteMemory(ctx, p, DeleteMemoryInput{MemoryID: created.Memory.ID})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Blocked || preview.PriorVersion != 2 || preview.VersionCount != 2 ||
		preview.EvidenceCount != 2 || preview.RelationCount != 1 ||
		preview.RetryShieldCount != 3 || preview.CurationRunCount != 1 ||
		preview.CurationActionCount != 1 || preview.CurationInputCount != 1 ||
		preview.CurationMutationCount != 1 || !isSHA256Hex(preview.ScrubSetRevision) ||
		!isSHA256Hex(preview.RetryShieldDigest) {
		t.Fatalf("delete preview = %#v", preview)
	}
	if preview.ScrubSetRevision == blockedByRelation.ScrubSetRevision {
		t.Fatal("reverting an active dependency did not change scrub-set revision")
	}
	if _, err := st.DeleteMemory(ctx, p, DeleteMemoryInput{
		MemoryID: created.Memory.ID, ExpectedVersion: 1,
		ScrubSetRevision: preview.ScrubSetRevision,
		IdempotencyKey:   "stale-version-delete", Apply: true,
	}); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("stale delete version = %v", err)
	}
	if _, err := st.DeleteMemory(ctx, p, DeleteMemoryInput{
		MemoryID: created.Memory.ID, ExpectedVersion: preview.PriorVersion,
		ScrubSetRevision: strings.Repeat("f", 64),
		IdempotencyKey:   "stale-revision-delete", Apply: true,
	}); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("stale delete revision = %v", err)
	}

	apply := DeleteMemoryInput{
		MemoryID: created.Memory.ID, ExpectedVersion: preview.PriorVersion,
		ScrubSetRevision: preview.ScrubSetRevision,
		IdempotencyKey:   "delete-private-memory-9c78d3", Apply: true,
	}
	receipt, err := st.DeleteMemory(ctx, p, apply)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Applied || receipt.Replayed || receipt.ReceiptID == "" || receipt.DeletedAt == nil {
		t.Fatalf("delete receipt = %#v", receipt)
	}
	rawReceipt, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		secret, updatedContent, capture.IdempotencyKey, adjust.IdempotencyKey,
		resolve.IdempotencyKey, apply.IdempotencyKey, "private-secret-locator",
	} {
		if strings.Contains(string(rawReceipt), forbidden) {
			t.Fatalf("receipt leaked %q: %s", forbidden, rawReceipt)
		}
	}

	var versions, evidence, relations, shields int64
	var currentIsNull bool
	var origin, captureReason, tombstoneJSON string
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memory_versions WHERE memory_id=$1),
		  (SELECT count(*) FROM memory_evidence WHERE memory_id=$1),
		  (SELECT count(*) FROM memory_relations WHERE from_memory_id=$1 OR to_memory_id=$1),
		  (SELECT count(*) FROM memory_deleted_references WHERE deleted_memory_id=$1
		     AND former_reference_kind LIKE 'idempotency.%'),
		  m.current_version IS NULL, m.origin, m.capture_reason, to_jsonb(m)::text
		FROM memories m WHERE m.id=$1`, created.Memory.ID).Scan(
		&versions, &evidence, &relations, &shields,
		&currentIsNull, &origin, &captureReason, &tombstoneJSON); err != nil {
		t.Fatal(err)
	}
	if versions != 0 || evidence != 0 || relations != 0 || shields != receipt.RetryShieldCount ||
		!currentIsNull || origin != "deleted" || captureReason != "deleted" {
		t.Fatalf("post-delete rows versions=%d evidence=%d relations=%d shields=%d null=%v origin=%q reason=%q",
			versions, evidence, relations, shields, currentIsNull, origin, captureReason)
	}
	var scrubbedRuns, remainingInputs, remainingMutations int64
	var scrubbedPayload, scrubbedPlanHash, scrubbedActionHash string
	if err := st.pool.QueryRow(ctx, `
		SELECT
		 (SELECT count(*) FROM memory_curation_runs
		  WHERE id='mrun_cccccccccccccccc' AND scrubbed_reason_code='permanent_delete'),
		 (SELECT count(*) FROM memory_curation_run_inputs
		  WHERE run_id='mrun_cccccccccccccccc'),
		 (SELECT count(*) FROM memory_curation_mutations
		  WHERE run_id='mrun_cccccccccccccccc'),
		 a.proposed_payload::text,r.plan_hash,a.action_hash
		FROM memory_curation_actions a
		JOIN memory_curation_runs r ON r.id=a.run_id
		WHERE a.id='mact_cccccccccccccccc'`).Scan(
		&scrubbedRuns, &remainingInputs, &remainingMutations,
		&scrubbedPayload, &scrubbedPlanHash, &scrubbedActionHash,
	); err != nil {
		t.Fatal(err)
	}
	if scrubbedRuns != 1 || remainingInputs != 0 || remainingMutations != 0 ||
		scrubbedPayload != "{}" || scrubbedPlanHash != "" || scrubbedActionHash != "" {
		t.Fatalf("curation scrub incomplete: runs=%d inputs=%d mutations=%d payload=%s plan=%q action=%q",
			scrubbedRuns, remainingInputs, remainingMutations,
			scrubbedPayload, scrubbedPlanHash, scrubbedActionHash)
	}
	for _, forbidden := range []string{
		secret, updatedContent, capture.IdempotencyKey, adjust.IdempotencyKey,
		resolve.IdempotencyKey, apply.IdempotencyKey, "private-secret-locator",
	} {
		if strings.Contains(tombstoneJSON, forbidden) {
			t.Fatalf("tombstone leaked %q: %s", forbidden, tombstoneJSON)
		}
	}
	var auditMetadata string
	if err := st.pool.QueryRow(ctx, `
		SELECT metadata::text FROM account_events
		WHERE account_id=$1 AND verb=$2
		ORDER BY occurred_at DESC, id DESC LIMIT 1`, p.AccountID, VerbMemoryDeleted).
		Scan(&auditMetadata); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{created.Memory.ID, receipt.ReceiptID} {
		if !strings.Contains(auditMetadata, required) {
			t.Fatalf("memory deletion audit metadata omitted %q: %s", required, auditMetadata)
		}
	}
	for _, forbidden := range []string{secret, updatedContent, capture.IdempotencyKey, apply.IdempotencyKey} {
		if strings.Contains(auditMetadata, forbidden) {
			t.Fatalf("memory deletion audit metadata leaked %q: %s", forbidden, auditMetadata)
		}
	}

	replayed, err := st.DeleteMemory(ctx, p, apply)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.ReceiptID != receipt.ReceiptID ||
		replayed.ScrubSetRevision != receipt.ScrubSetRevision ||
		replayed.RetryShieldDigest != receipt.RetryShieldDigest {
		t.Fatalf("delete replay = %#v; original = %#v", replayed, receipt)
	}
	changedReplay := apply
	changedReplay.ScrubSetRevision = strings.Repeat("e", 64)
	if _, err := st.DeleteMemory(ctx, p, changedReplay); !errors.Is(err, ErrMemoryIdempotencyConflict) {
		t.Fatalf("changed delete replay = %v", err)
	}
	if _, err := st.CaptureMemory(ctx, p, capture); !errors.Is(err, ErrMemoryDeleted) {
		t.Fatalf("delayed capture retry = %v", err)
	}
	if _, err := st.AdjustMemory(ctx, p, created.Memory.ID, adjust); !errors.Is(err, ErrMemoryDeleted) {
		t.Fatalf("delayed adjustment retry = %v", err)
	}
	if _, err := st.ResolveMemoryEvidence(ctx, p, pendingID, resolve); !errors.Is(err, ErrMemoryDeleted) {
		t.Fatalf("delayed evidence retry = %v", err)
	}
	if _, err := st.GetMemory(ctx, p, created.Memory.ID); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("deleted memory read = %v", err)
	}
}

func applyMemoryDeletePreview(ctx context.Context, st *Store, p Principal, preview DeleteMemoryResult, key string) (DeleteMemoryResult, error) {
	return st.DeleteMemory(ctx, p, DeleteMemoryInput{
		MemoryID: preview.MemoryID, ExpectedVersion: preview.PriorVersion,
		ScrubSetRevision: preview.ScrubSetRevision,
		IdempotencyKey:   key, Apply: true,
	})
}
