package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// RollbackMemoryCurationInput identifies an apply receipt and pins every
// produced head that may be compensated.
type RollbackMemoryCurationInput struct {
	ApplyReceiptID        string                   `json:"apply_receipt_id"`
	ExpectedProducedHeads []MemoryVersionReference `json:"expected_produced_heads"`
	Reason                string                   `json:"reason,omitempty"`
	IdempotencyKey        string                   `json:"idempotency_key"`
}

// MemoryCurationRollbackBlocker identifies a dependent resource that prevents
// safe compensation of an applied curation plan.
type MemoryCurationRollbackBlocker struct {
	Kind       string `json:"kind"`
	MemoryID   string `json:"memory_id,omitempty"`
	Version    int64  `json:"version,omitempty"`
	ResourceID string `json:"resource_id,omitempty"`
}

// MemoryCurationRollbackBlockedError is value-free and unwraps to the common
// curation conflict sentinel so transports may preserve their existing status
// mapping while still returning actionable blocker kinds.
type MemoryCurationRollbackBlockedError struct {
	Blockers []MemoryCurationRollbackBlocker
}

func (e *MemoryCurationRollbackBlockedError) Error() string {
	return fmt.Sprintf("%s: rollback has %d dependent resources", ErrMemoryCurationConflict, len(e.Blockers))
}

func (e *MemoryCurationRollbackBlockedError) Unwrap() error {
	return ErrMemoryCurationConflict
}

// MemoryCurationActionRollbackResult records value-free compensation metadata
// for one applied action.
type MemoryCurationActionRollbackResult struct {
	ActionID              string                   `json:"action_id"`
	Ordinal               int64                    `json:"ordinal"`
	Operation             string                   `json:"operation"`
	CompensationHeads     []MemoryVersionReference `json:"compensation_heads"`
	RevertedRelationIDs   []string                 `json:"reverted_relation_ids"`
	WithdrawnCandidateIDs []string                 `json:"withdrawn_candidate_ids"`
}

// MemoryCurationRollbackReceipt is the durable, value-free receipt for one
// compensating rollback.
type MemoryCurationRollbackReceipt struct {
	ID                    string                               `json:"id"`
	Operation             string                               `json:"operation"`
	ActorID               string                               `json:"actor_id"`
	IdempotencyKey        string                               `json:"idempotency_key"`
	RequestHash           string                               `json:"request_hash"`
	RequestID             string                               `json:"request_id"`
	RunID                 string                               `json:"run_id"`
	ApplyReceiptID        string                               `json:"apply_receipt_id"`
	ExpectedProducedHeads []MemoryVersionReference             `json:"expected_produced_heads"`
	ActionResults         []MemoryCurationActionRollbackResult `json:"action_results"`
	ReplayRequestID       string                               `json:"replay_request_id"`
	ReplayGeneration      int64                                `json:"replay_generation"`
	CreatedAt             time.Time                            `json:"created_at"`
	Replayed              bool                                 `json:"replayed,omitempty"`
}

// RollbackMemoryCurationResult reports the rolled-back run, its replay request,
// and the durable rollback receipt.
type RollbackMemoryCurationResult struct {
	Run           MemoryCurationRun             `json:"run"`
	ReplayRequest MemoryCurationRequest         `json:"replay_request"`
	Receipt       MemoryCurationRollbackReceipt `json:"receipt"`
}

// RollbackCuration appends compensating versions and reverts only resources
// proven to have been produced by one exact apply receipt. It refuses to race
// a successor curator, never deletes history, and never rewinds source cursors.
func (s *Store) RollbackCuration(
	ctx context.Context,
	p Principal,
	runID string,
	in RollbackMemoryCurationInput,
) (RollbackMemoryCurationResult, error) {
	if p.Kind != PrincipalAgent {
		return RollbackMemoryCurationResult{}, ErrMemoryCurationForbidden
	}
	runID = strings.TrimSpace(runID)
	in.ApplyReceiptID = strings.TrimSpace(in.ApplyReceiptID)
	in.Reason = strings.TrimSpace(in.Reason)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	heads, err := normalizeMemoryCurationProducedHeads(in.ExpectedProducedHeads)
	if !validCurationID(runID, "mrun") || !validCurationID(in.ApplyReceiptID, "mrec") ||
		len(in.Reason) > 1024 || validateMemoryIdempotencyKey(in.IdempotencyKey) != nil || err != nil {
		return RollbackMemoryCurationResult{}, ErrMemoryCurationInputInvalid
	}
	in.ExpectedProducedHeads = heads
	hashInput := in
	hashInput.IdempotencyKey = ""
	requestHash, err := memoryRequestHash(struct {
		Operation string                      `json:"operation"`
		RunID     string                      `json:"run_id"`
		Input     RollbackMemoryCurationInput `json:"input"`
	}{Operation: "rollback", RunID: runID, Input: hashInput})
	if err != nil {
		return RollbackMemoryCurationResult{}, ErrMemoryCurationInputInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prepareMemoryCurationMutationTx(ctx, tx, p, "rollback", in.IdempotencyKey); err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	if mutation, replayed, err := loadMemoryCurationMutation(
		ctx, tx, p, "rollback", in.IdempotencyKey, requestHash,
	); err != nil || replayed {
		if err != nil {
			return RollbackMemoryCurationResult{}, err
		}
		if mutation.RunID != runID || mutation.ResultState != MemoryCurationRunRolledBack {
			return RollbackMemoryCurationResult{}, ErrMemoryCurationIdempotencyConflict
		}
		out, err := loadMemoryCurationRollbackResult(ctx, tx, p, mutation, in, true)
		if err != nil {
			return RollbackMemoryCurationResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RollbackMemoryCurationResult{}, err
		}
		return out, nil
	}

	// Rollback has no worker fence, so exclusive ownership of an idle lane is
	// its concurrency token. Even an expired successor is rejected here rather
	// than being implicitly interrupted by an unrelated historical operation.
	lane, err := loadMemoryCurationLaneTx(ctx, tx, p, true)
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	if lane.ActiveRunID != "" {
		return RollbackMemoryCurationResult{}, ErrMemoryCurationBusy
	}
	if err := lockFactSubjectNamespace(ctx, tx, p, false); err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	run, err := loadMemoryCurationRun(ctx, tx, p, runID, true)
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	if run.State != MemoryCurationRunApplied || run.ApplyReceiptID != in.ApplyReceiptID ||
		run.RollbackReceiptID != "" {
		return RollbackMemoryCurationResult{}, ErrMemoryCurationConflict
	}
	request, err := loadMemoryCurationRequest(ctx, tx, p, run.RequestID, false)
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	stored, err := loadMemoryCurationStoredPlan(ctx, tx, p, run)
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	applyResults, err := loadMemoryCurationAppliedActionResults(ctx, tx, p, run, stored)
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	if err := verifyMemoryCurationAppliedProvenanceTx(
		ctx, tx, p, run, stored, applyResults,
	); err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	producedHeads, producedVersions := memoryCurationProducedHeads(applyResults)
	if !sameMemoryVersionReferences(producedHeads, in.ExpectedProducedHeads) {
		return RollbackMemoryCurationResult{}, ErrMemoryCurationConflict
	}
	if err := lockMemoryCurationChangeClockTx(ctx, tx, p); err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	if err := lockMemoryCurationRollbackHeadsTx(ctx, tx, p, producedHeads); err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	blockers, err := findMemoryCurationRollbackBlockersTx(
		ctx, tx, p, run, applyResults, producedHeads, producedVersions,
	)
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	if len(blockers) > 0 {
		return RollbackMemoryCurationResult{}, &MemoryCurationRollbackBlockedError{Blockers: blockers}
	}

	actionResults, err := compensateMemoryCurationActionsTx(
		ctx, tx, p, run, stored, applyResults, requestHash,
	)
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	receiptID, err := id.New("mrec")
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	replayRequest, err := createMemoryCurationRollbackReplayTx(ctx, tx, p, &lane, run, request)
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE memory_curation_runs
		SET state='rolled_back',rollback_receipt_id=$2,
		    rolled_back_at=clock_timestamp(),terminal_at=clock_timestamp(),
		    terminal_reason_code='curation_rollback',updated_at=clock_timestamp()
		WHERE id=$1 AND state='applied' AND apply_receipt_id=$3
		  AND rollback_receipt_id=''`, run.ID, receiptID, in.ApplyReceiptID); err != nil {
		return RollbackMemoryCurationResult{}, fmt.Errorf("mark curation run rolled back: %w", err)
	} else if tag.RowsAffected() != 1 {
		return RollbackMemoryCurationResult{}, ErrMemoryCurationConflict
	}
	mutation, err := insertMemoryCurationMutation(ctx, tx, p, MemoryCurationMutationReceipt{
		Operation: "rollback", ActorID: p.ID, IdempotencyKey: in.IdempotencyKey,
		RequestHash: requestHash, RequestID: run.RequestID, RunID: run.ID,
		RequestGeneration: run.RequestGeneration,
		FencingGeneration: run.FencingGeneration,
		ResultState:       MemoryCurationRunRolledBack, ReceiptID: receiptID,
	})
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	if err := logMemoryCurationEventTx(ctx, tx, p, VerbMemoryCurationRolledBack,
		run.RequestID, run.ID, run.RequestGeneration, run.FencingGeneration,
		MemoryCurationRunRolledBack); err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	run, err = loadMemoryCurationRun(ctx, tx, p, run.ID, false)
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	receipt := MemoryCurationRollbackReceipt{
		ID: receiptID, Operation: "rollback", ActorID: p.ID,
		IdempotencyKey: in.IdempotencyKey, RequestHash: requestHash,
		RequestID: run.RequestID, RunID: run.ID, ApplyReceiptID: in.ApplyReceiptID,
		ExpectedProducedHeads: producedHeads, ActionResults: actionResults,
		ReplayRequestID:  replayRequest.ID,
		ReplayGeneration: replayRequest.RequestGeneration, CreatedAt: mutation.CreatedAt,
	}
	if err := tx.Commit(ctx); err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	return RollbackMemoryCurationResult{
		Run: run, ReplayRequest: replayRequest, Receipt: receipt,
	}, nil
}

func normalizeMemoryCurationProducedHeads(
	heads []MemoryVersionReference,
) ([]MemoryVersionReference, error) {
	out := append(make([]MemoryVersionReference, 0, len(heads)), heads...)
	for _, head := range out {
		if !validMemoryID(head.MemoryID) || head.Version < 1 {
			return nil, ErrMemoryCurationInputInvalid
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MemoryID != out[j].MemoryID {
			return out[i].MemoryID < out[j].MemoryID
		}
		return out[i].Version < out[j].Version
	})
	for index := 1; index < len(out); index++ {
		if out[index-1].MemoryID == out[index].MemoryID {
			return nil, ErrMemoryCurationInputInvalid
		}
	}
	return out, nil
}

func sameMemoryVersionReferences(a, b []MemoryVersionReference) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func loadMemoryCurationAppliedActionResults(
	ctx context.Context,
	q memoryQuerier,
	p Principal,
	run MemoryCurationRun,
	stored memoryCurationStoredPlan,
) ([]MemoryCurationActionApplyResult, error) {
	rows, err := q.Query(ctx, `
		SELECT id,ordinal,primitive,applied_result::text
		FROM memory_curation_actions
		WHERE run_id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4 AND state='applied'
		ORDER BY ordinal`, run.ID, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MemoryCurationActionApplyResult, 0, len(stored.Actions))
	for rows.Next() {
		var actionID, operation string
		var ordinal int64
		var raw []byte
		if err := rows.Scan(&actionID, &ordinal, &operation, &raw); err != nil {
			return nil, err
		}
		var result MemoryCurationActionApplyResult
		if err := decodeMemoryCurationStoredJSON(raw, &result); err != nil ||
			result.ActionID != actionID || result.Ordinal != ordinal ||
			result.Operation != operation {
			return nil, ErrMemoryCurationConflict
		}
		out = append(out, result)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(stored.Actions) {
		return nil, ErrMemoryCurationConflict
	}
	for index := range out {
		if out[index].ActionID != stored.Actions[index].ID ||
			out[index].Operation != stored.Actions[index].Action.Operation {
			return nil, ErrMemoryCurationConflict
		}
	}
	return out, nil
}

// verifyMemoryCurationAppliedProvenanceTx treats applied_result as an index,
// never as authority. This matters after archive import: a hostile receipt must
// not be able to exempt unrelated evidence from blocker checks, revert an
// unrelated relation, withdraw another candidate, or compensate another head.
func verifyMemoryCurationAppliedProvenanceTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	stored memoryCurationStoredPlan,
	results []MemoryCurationActionApplyResult,
) error {
	if len(results) != len(stored.Actions) {
		return ErrMemoryCurationConflict
	}
	for index, result := range results {
		row := stored.Actions[index]
		if err := verifyMemoryCurationAppliedShape(row.Action, result); err != nil {
			return err
		}
		for _, after := range result.AfterHeads {
			memory, err := loadMemoryAtVersion(ctx, tx, p, after.MemoryID, after.Version, false)
			if err != nil || memory.CurationRunID != run.ID ||
				memory.CurationActionID != row.ID {
				return ErrMemoryCurationConflict
			}
			expectedPrevious := int64(0)
			if len(result.BeforeHeads) == 1 {
				expectedPrevious = result.BeforeHeads[0].Version
			}
			if memory.PreviousVersion != expectedPrevious {
				return ErrMemoryCurationConflict
			}
		}
		if err := verifyMemoryCurationAppliedEvidenceTx(ctx, tx, p, row, result); err != nil {
			return err
		}
		if err := verifyMemoryCurationAppliedRelationsTx(ctx, tx, p, run, row, result); err != nil {
			return err
		}
		if err := verifyMemoryCurationAppliedCandidateTx(ctx, tx, p, run, row, result); err != nil {
			return err
		}
	}
	return nil
}

func verifyMemoryCurationAppliedShape(
	action MemoryCurationPlanAction,
	result MemoryCurationActionApplyResult,
) error {
	empty := func(values ...int) bool {
		for _, value := range values {
			if value != 0 {
				return false
			}
		}
		return true
	}
	if hasDuplicateCurationResultResources(result) {
		return ErrMemoryCurationConflict
	}
	switch action.Operation {
	case MemoryCurationOperationCreate:
		create := action.Create
		if create == nil || len(result.BeforeHeads) != 0 || len(result.AfterHeads) != 1 ||
			len(result.CreatedMemoryIDs) != 1 || result.CreatedMemoryIDs[0] != create.MemoryID ||
			result.AfterHeads[0] != (MemoryVersionReference{MemoryID: create.MemoryID, Version: 1}) ||
			len(result.EvidenceIDs) != len(create.Snapshot.Evidence) ||
			len(result.RelationIDs) != len(create.Relations) ||
			!empty(len(result.CandidateIDs), len(result.SupersessionReplacementIDs)) ||
			result.SupersessionSetID != "" || result.SupersessionSetRevision != 0 {
			return ErrMemoryCurationConflict
		}
	case MemoryCurationOperationReplace:
		replace := action.Replace
		if replace == nil {
			return ErrMemoryCurationConflict
		}
		before := MemoryVersionReference{MemoryID: replace.Target.MemoryID, Version: replace.Target.ExpectedVersion}
		after := MemoryVersionReference{MemoryID: before.MemoryID, Version: before.Version + 1}
		if !sameMemoryVersionReferences(result.BeforeHeads, []MemoryVersionReference{before}) ||
			!sameMemoryVersionReferences(result.AfterHeads, []MemoryVersionReference{after}) ||
			len(result.EvidenceIDs) != len(replace.Snapshot.Evidence) ||
			!empty(len(result.CreatedMemoryIDs), len(result.RelationIDs), len(result.CandidateIDs), len(result.SupersessionReplacementIDs)) ||
			result.SupersessionSetID != "" || result.SupersessionSetRevision != 0 {
			return ErrMemoryCurationConflict
		}
	case MemoryCurationOperationSupersede:
		supersede := action.Supersede
		if supersede == nil {
			return ErrMemoryCurationConflict
		}
		before := MemoryVersionReference{MemoryID: supersede.Target.MemoryID, Version: supersede.Target.ExpectedVersion}
		after := MemoryVersionReference{MemoryID: before.MemoryID, Version: before.Version + 1}
		replacements := make([]MemoryVersionReference, len(supersede.Replacements))
		for index, replacement := range supersede.Replacements {
			replacements[index] = MemoryVersionReference{MemoryID: replacement.MemoryID, Version: replacement.Version}
		}
		if !sameMemoryVersionReferences(result.BeforeHeads, []MemoryVersionReference{before}) ||
			!sameMemoryVersionReferences(result.AfterHeads, []MemoryVersionReference{after}) ||
			!sameMemoryVersionReferences(result.SupersessionReplacementIDs, replacements) ||
			len(result.RelationIDs) != len(replacements) ||
			!empty(len(result.CreatedMemoryIDs), len(result.EvidenceIDs), len(result.CandidateIDs)) ||
			!validCurationID(result.SupersessionSetID, "mset") || result.SupersessionSetRevision != 1 {
			return ErrMemoryCurationConflict
		}
	case MemoryCurationOperationRelate:
		if action.Relate == nil || len(result.RelationIDs) != 1 ||
			!empty(len(result.BeforeHeads), len(result.AfterHeads), len(result.CreatedMemoryIDs),
				len(result.EvidenceIDs), len(result.CandidateIDs), len(result.SupersessionReplacementIDs)) ||
			result.SupersessionSetID != "" || result.SupersessionSetRevision != 0 {
			return ErrMemoryCurationConflict
		}
	case MemoryCurationOperationProposeFact:
		if action.ProposeFact == nil || len(result.CandidateIDs) != 1 ||
			!empty(len(result.BeforeHeads), len(result.AfterHeads), len(result.CreatedMemoryIDs),
				len(result.EvidenceIDs), len(result.RelationIDs), len(result.SupersessionReplacementIDs)) ||
			result.SupersessionSetID != "" || result.SupersessionSetRevision != 0 {
			return ErrMemoryCurationConflict
		}
	default:
		return ErrMemoryCurationConflict
	}
	return nil
}

func hasDuplicateCurationResultResources(result MemoryCurationActionApplyResult) bool {
	seenIDs := make(map[string]struct{})
	for _, list := range [][]string{
		result.CreatedMemoryIDs, result.EvidenceIDs, result.RelationIDs, result.CandidateIDs,
	} {
		for _, value := range list {
			if value == "" {
				return true
			}
			if _, ok := seenIDs[value]; ok {
				return true
			}
			seenIDs[value] = struct{}{}
		}
	}
	for _, refs := range [][]MemoryVersionReference{
		result.BeforeHeads, result.AfterHeads, result.SupersessionReplacementIDs,
	} {
		seen := make(map[MemoryVersionReference]struct{})
		for _, ref := range refs {
			if !validMemoryID(ref.MemoryID) || ref.Version < 1 {
				return true
			}
			if _, ok := seen[ref]; ok {
				return true
			}
			seen[ref] = struct{}{}
		}
	}
	return false
}

func verifyMemoryCurationAppliedEvidenceTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	row memoryCurationStoredAction,
	result MemoryCurationActionApplyResult,
) error {
	var expected []MemoryCurationEvidence
	if row.Action.Create != nil {
		expected = row.Action.Create.Snapshot.Evidence
	} else if row.Action.Replace != nil {
		expected = row.Action.Replace.Snapshot.Evidence
	}
	if len(expected) != len(result.EvidenceIDs) {
		return ErrMemoryCurationConflict
	}
	var target MemoryVersionReference
	if len(result.AfterHeads) == 1 {
		target = result.AfterHeads[0]
	}
	for index, evidenceID := range result.EvidenceIDs {
		evidence, err := scanMemoryEvidence(tx.QueryRow(ctx, `
			SELECT id,memory_id,target_version,evidence_change_seq,
			       evidence_type,role,resolution_state,
			       COALESCE(external_locator,''),COALESCE(pending_evidence_id,''),
			       COALESCE(resolved_kind,''),COALESCE(source_transcript_id,''),
			       COALESCE(source_sequence_from,0),COALESCE(source_sequence_until,0),
			       COALESCE(source_memory_id,''),COALESCE(source_memory_version,0),
			       COALESCE(source_message_id,''),COALESCE(source_import_locator,''),
			       artifact_excerpt,artifact_sensitive,
			       COALESCE(terminal_reason_code,''),COALESCE(source_digest,''),
			       actor_id,COALESCE(idempotency_key,''),COALESCE(request_hash,''),created_at
			FROM memory_evidence
			WHERE id=$1 AND account_id=$2 AND realm_id=$3
			  AND owner_kind='agent' AND owner_id=$4`, evidenceID,
			p.AccountID, p.RealmID, p.ID))
		if err != nil || evidence.MemoryID != target.MemoryID ||
			evidence.TargetVersion != target.Version {
			return ErrMemoryCurationConflict
		}
		// InputEvidenceID authorizes the source row and is not copied into the
		// new evidence row. Substitute it only for exact semantic comparison.
		evidence.ID = expected[index].InputEvidenceID
		if !sameMemoryCurationInputEvidence(expected[index], evidence) {
			return ErrMemoryCurationConflict
		}
	}
	return nil
}

type memoryCurationAppliedRelation struct {
	from, to          MemoryVersionReference
	relationType      string
	supersessionSetID string
	setRevision       int64
}

func verifyMemoryCurationAppliedRelationsTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	row memoryCurationStoredAction,
	result MemoryCurationActionApplyResult,
) error {
	for index, relationID := range result.RelationIDs {
		var relation memoryCurationAppliedRelation
		var relationRunID, relationActionID string
		if err := tx.QueryRow(ctx, `
			SELECT from_memory_id,from_version,to_memory_id,to_version,relation_type,
			       COALESCE(supersession_set_id,''),COALESCE(supersession_set_revision,0),
			       COALESCE(curation_run_id,''),COALESCE(curation_action_id,'')
			FROM memory_relations
			WHERE id=$1 AND account_id=$2 AND realm_id=$3
			  AND owner_kind='agent' AND owner_id=$4`, relationID,
			p.AccountID, p.RealmID, p.ID).Scan(
			&relation.from.MemoryID, &relation.from.Version,
			&relation.to.MemoryID, &relation.to.Version, &relation.relationType,
			&relation.supersessionSetID, &relation.setRevision,
			&relationRunID, &relationActionID); err != nil {
			return ErrMemoryCurationConflict
		}
		if relationRunID != run.ID || relationActionID != row.ID {
			return ErrMemoryCurationConflict
		}
		switch row.Action.Operation {
		case MemoryCurationOperationCreate:
			expected := row.Action.Create.Relations[index]
			if relation.from != result.AfterHeads[0] ||
				relation.to != (MemoryVersionReference{MemoryID: expected.To.MemoryID, Version: expected.To.Version}) ||
				relation.relationType != expected.RelationType || relation.supersessionSetID != "" ||
				relation.setRevision != 0 {
				return ErrMemoryCurationConflict
			}
		case MemoryCurationOperationSupersede:
			expected := result.SupersessionReplacementIDs[index]
			if relation.from != expected || relation.to != result.AfterHeads[0] ||
				relation.relationType != "supersedes" ||
				relation.supersessionSetID != result.SupersessionSetID ||
				relation.setRevision != result.SupersessionSetRevision {
				return ErrMemoryCurationConflict
			}
		case MemoryCurationOperationRelate:
			expected := row.Action.Relate
			if relation.from != (MemoryVersionReference{MemoryID: expected.From.MemoryID, Version: expected.From.Version}) ||
				relation.to != (MemoryVersionReference{MemoryID: expected.To.MemoryID, Version: expected.To.Version}) ||
				relation.relationType != expected.RelationType || relation.supersessionSetID != "" ||
				relation.setRevision != 0 {
				return ErrMemoryCurationConflict
			}
		default:
			return ErrMemoryCurationConflict
		}
	}
	return nil
}

func verifyMemoryCurationAppliedCandidateTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	row memoryCurationStoredAction,
	result MemoryCurationActionApplyResult,
) error {
	if len(result.CandidateIDs) == 0 {
		return nil
	}
	action := row.Action.ProposeFact
	if action == nil || action.Confidence == nil || len(result.CandidateIDs) != 1 {
		return ErrMemoryCurationConflict
	}
	candidate, err := getFactCandidateTx(ctx, tx, p, result.CandidateIDs[0])
	if err != nil {
		return ErrMemoryCurationConflict
	}
	var curationRunID, curationActionID string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(curation_run_id,''),COALESCE(curation_action_id,'')
		FROM fact_candidates
		WHERE id=$1 AND account_id=$2 AND realm_id=$3 AND owner_agent_id=$4`,
		candidate.ID, p.AccountID, p.RealmID, p.ID).Scan(
		&curationRunID, &curationActionID); err != nil ||
		curationRunID != run.ID || curationActionID != row.ID {
		return ErrMemoryCurationConflict
	}
	wantValue, err := canonicalizeMemoryCurationRawJSON(action.Value)
	if err != nil {
		return ErrMemoryCurationConflict
	}
	gotValue, err := canonicalizeMemoryCurationRawJSON(candidate.Value)
	if err != nil {
		return ErrMemoryCurationConflict
	}
	if action.Subject != candidate.Subject || action.Predicate != candidate.Predicate ||
		action.ValueType != candidate.ValueType || string(wantValue) != string(gotValue) ||
		action.Recurrence != candidate.Recurrence || action.Cardinality != candidate.Cardinality ||
		(action.Sensitive && !candidate.Sensitive) || *action.Confidence != candidate.Confidence ||
		action.Reason != candidate.Reason || candidate.SourceRef != "curation:"+run.ID+":"+row.ID ||
		!sameOptionalCurationTime(action.ValidFrom, candidate.ValidFrom) ||
		!sameOptionalCurationTime(action.ValidUntil, candidate.ValidUntil) {
		return ErrMemoryCurationConflict
	}
	return nil
}

func sameOptionalCurationTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func memoryCurationProducedHeads(
	results []MemoryCurationActionApplyResult,
) ([]MemoryVersionReference, []MemoryVersionReference) {
	final := make(map[string]MemoryVersionReference)
	versions := make(map[MemoryVersionReference]struct{})
	for _, result := range results {
		for _, head := range result.AfterHeads {
			final[head.MemoryID] = head
			versions[head] = struct{}{}
		}
	}
	heads := make([]MemoryVersionReference, 0, len(final))
	for _, head := range final {
		heads = append(heads, head)
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].MemoryID < heads[j].MemoryID })
	all := make([]MemoryVersionReference, 0, len(versions))
	for version := range versions {
		all = append(all, version)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].MemoryID != all[j].MemoryID {
			return all[i].MemoryID < all[j].MemoryID
		}
		return all[i].Version < all[j].Version
	})
	return heads, all
}

func lockMemoryCurationRollbackHeadsTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	heads []MemoryVersionReference,
) error {
	for _, head := range heads {
		var current sql.NullInt64
		err := tx.QueryRow(ctx, `
			SELECT current_version FROM memories
			WHERE id=$1 AND account_id=$2 AND realm_id=$3
			  AND owner_kind='agent' AND owner_id=$4 FOR UPDATE`,
			head.MemoryID, p.AccountID, p.RealmID, p.ID).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) || !current.Valid || current.Int64 != head.Version {
			return &MemoryCurationRollbackBlockedError{Blockers: []MemoryCurationRollbackBlocker{{
				Kind: "later_head", MemoryID: head.MemoryID, Version: head.Version,
			}}}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func findMemoryCurationRollbackBlockersTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	results []MemoryCurationActionApplyResult,
	producedHeads, producedVersions []MemoryVersionReference,
) ([]MemoryCurationRollbackBlocker, error) {
	blockers := make([]MemoryCurationRollbackBlocker, 0)
	appendBlocker := func(blocker MemoryCurationRollbackBlocker) {
		if len(blockers) < 256 {
			blockers = append(blockers, blocker)
		}
	}
	for _, head := range producedHeads {
		memory, err := loadCurrentMemory(ctx, tx, p, head.MemoryID, false)
		if err != nil || memory.Version != head.Version {
			appendBlocker(MemoryCurationRollbackBlocker{
				Kind: "later_head", MemoryID: head.MemoryID, Version: head.Version,
			})
		}
	}
	ownEvidence, ownRelations := make([]string, 0), make([]string, 0)
	for _, result := range results {
		ownEvidence = append(ownEvidence, result.EvidenceIDs...)
		ownRelations = append(ownRelations, result.RelationIDs...)
	}
	for _, version := range producedVersions {
		var evidenceID string
		err := tx.QueryRow(ctx, `
			SELECT id FROM memory_evidence
			WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
			  AND ((memory_id=$4 AND target_version=$5) OR
			       (source_memory_id=$4 AND source_memory_version=$5))
			  AND NOT (id=ANY($6::text[]))
			ORDER BY created_at,id LIMIT 1`, p.AccountID, p.RealmID, p.ID,
			version.MemoryID, version.Version, ownEvidence).Scan(&evidenceID)
		if err == nil {
			appendBlocker(MemoryCurationRollbackBlocker{
				Kind: "later_evidence", MemoryID: version.MemoryID,
				Version: version.Version, ResourceID: evidenceID,
			})
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		var relationID string
		err = tx.QueryRow(ctx, `
			SELECT id FROM memory_relations
			WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
			  AND ((from_memory_id=$4 AND from_version=$5) OR
			       (to_memory_id=$4 AND to_version=$5))
			  AND reverted_at IS NULL AND NOT (id=ANY($6::text[]))
			ORDER BY created_at,id LIMIT 1`, p.AccountID, p.RealmID, p.ID,
			version.MemoryID, version.Version, ownRelations).Scan(&relationID)
		if err == nil {
			appendBlocker(MemoryCurationRollbackBlocker{
				Kind: "later_relation", MemoryID: version.MemoryID,
				Version: version.Version, ResourceID: relationID,
			})
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		var actionID string
		err = tx.QueryRow(ctx, `
			SELECT a.id FROM memory_curation_actions a
			JOIN memory_curation_runs consumer ON consumer.id=a.run_id
			WHERE a.account_id=$1 AND a.realm_id=$2 AND a.owner_kind='agent' AND a.owner_id=$3
			  AND a.run_id<>$4 AND a.state='applied' AND consumer.state='applied'
			  AND EXISTS (
			    SELECT 1 FROM jsonb_array_elements(a.input_refs) ref
			    WHERE ref->>'memory_id'=$5 AND ref ? 'version'
			      AND (ref->>'version')::bigint=$6
			  )
			ORDER BY a.created_at,a.id LIMIT 1`, p.AccountID, p.RealmID, p.ID,
			run.ID, version.MemoryID, version.Version).Scan(&actionID)
		if err == nil {
			appendBlocker(MemoryCurationRollbackBlocker{
				Kind: "later_action", MemoryID: version.MemoryID,
				Version: version.Version, ResourceID: actionID,
			})
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	for _, result := range results {
		for _, relationID := range result.RelationIDs {
			var reverted bool
			err := tx.QueryRow(ctx, `
				SELECT reverted_at IS NOT NULL FROM memory_relations
				WHERE id=$1 AND account_id=$2 AND realm_id=$3
				  AND owner_kind='agent' AND owner_id=$4
				  AND curation_run_id=$5 AND curation_action_id=$6
				FOR UPDATE`, relationID, p.AccountID, p.RealmID, p.ID,
				run.ID, result.ActionID).Scan(&reverted)
			if errors.Is(err, pgx.ErrNoRows) || reverted {
				appendBlocker(MemoryCurationRollbackBlocker{Kind: "produced_relation_changed", ResourceID: relationID})
			} else if err != nil {
				return nil, err
			}
		}
		for _, candidateID := range result.CandidateIDs {
			var status string
			err := tx.QueryRow(ctx, `
				SELECT status FROM fact_candidates
				WHERE id=$1 AND account_id=$2 AND realm_id=$3 AND owner_agent_id=$4
				  AND curation_run_id=$5 AND curation_action_id=$6
				FOR UPDATE`, candidateID, p.AccountID, p.RealmID, p.ID,
				run.ID, result.ActionID).Scan(&status)
			if errors.Is(err, pgx.ErrNoRows) {
				appendBlocker(MemoryCurationRollbackBlocker{Kind: "produced_candidate_changed", ResourceID: candidateID})
			} else if err != nil {
				return nil, err
			} else if status == "confirmed" {
				appendBlocker(MemoryCurationRollbackBlocker{Kind: "confirmed_candidate", ResourceID: candidateID})
			} else if status != "pending" && status != "conflict" && status != "rejected" {
				appendBlocker(MemoryCurationRollbackBlocker{Kind: "produced_candidate_changed", ResourceID: candidateID})
			}
		}
	}
	return blockers, nil
}

func compensateMemoryCurationActionsTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	stored memoryCurationStoredPlan,
	applyResults []MemoryCurationActionApplyResult,
	rollbackRequestHash string,
) ([]MemoryCurationActionRollbackResult, error) {
	results := make([]MemoryCurationActionRollbackResult, len(applyResults))
	for index := len(applyResults) - 1; index >= 0; index-- {
		applied := applyResults[index]
		row := stored.Actions[index]
		result := MemoryCurationActionRollbackResult{
			ActionID: row.ID, Ordinal: row.Action.Ordinal, Operation: row.Action.Operation,
			CompensationHeads:     make([]MemoryVersionReference, 0),
			RevertedRelationIDs:   make([]string, 0),
			WithdrawnCandidateIDs: make([]string, 0),
		}
		if len(applied.RelationIDs) > 0 {
			tag, err := tx.Exec(ctx, `
				UPDATE memory_relations
				SET reverted_at=clock_timestamp(),reverted_by_run_id=$2,
				    reverted_by_action_id=$3
				WHERE id=ANY($1::text[]) AND curation_run_id=$2
				  AND curation_action_id=$3 AND reverted_at IS NULL`,
				applied.RelationIDs, run.ID, row.ID)
			if err != nil {
				return nil, err
			}
			if tag.RowsAffected() != int64(len(applied.RelationIDs)) {
				return nil, ErrMemoryCurationConflict
			}
			result.RevertedRelationIDs = append(result.RevertedRelationIDs, applied.RelationIDs...)
		}
		for _, candidateID := range applied.CandidateIDs {
			var status string
			if err := tx.QueryRow(ctx, `SELECT status FROM fact_candidates WHERE id=$1 FOR UPDATE`,
				candidateID).Scan(&status); err != nil {
				return nil, err
			}
			if status != "pending" && status != "conflict" {
				continue
			}
			withdrawalKey := "curation:" + run.ID + ":" + row.ID + ":withdraw:" + candidateID
			if tag, err := tx.Exec(ctx, `
				UPDATE fact_candidates
				SET status='withdrawn',decided_at=clock_timestamp(),
				    withdrawal_reason='curation_rollback',
				    withdrawal_idempotency_key=$2,withdrawal_request_hash=$3
				WHERE id=$1 AND status IN ('pending','conflict')`, candidateID,
				withdrawalKey, rollbackRequestHash); err != nil {
				return nil, err
			} else if tag.RowsAffected() != 1 {
				return nil, ErrMemoryCurationConflict
			}
			result.WithdrawnCandidateIDs = append(result.WithdrawnCandidateIDs, candidateID)
		}
		switch row.Action.Operation {
		case MemoryCurationOperationCreate:
			for _, memoryID := range applied.CreatedMemoryIDs {
				ref, err := appendMemoryCurationRollbackVersionTx(ctx, tx, p, run,
					row.ID, memoryID, nil, MemoryStateReverted, rollbackRequestHash)
				if err != nil {
					return nil, err
				}
				result.CompensationHeads = append(result.CompensationHeads, ref)
			}
		case MemoryCurationOperationReplace, MemoryCurationOperationSupersede:
			if len(applied.BeforeHeads) != 1 || len(applied.AfterHeads) != 1 {
				return nil, ErrMemoryCurationConflict
			}
			before := applied.BeforeHeads[0]
			ref, err := appendMemoryCurationRollbackVersionTx(ctx, tx, p, run,
				row.ID, before.MemoryID, &before, MemoryStateActive, rollbackRequestHash)
			if err != nil {
				return nil, err
			}
			result.CompensationHeads = append(result.CompensationHeads, ref)
		}
		raw, err := canonicalMemoryCurationJSON(result)
		if err != nil {
			return nil, err
		}
		if tag, err := tx.Exec(ctx, `
			UPDATE memory_curation_actions
			SET state='reverted',rollback_result=$2::jsonb,reverted_at=clock_timestamp()
			WHERE id=$1 AND run_id=$3 AND state='applied'`, row.ID, string(raw), run.ID); err != nil {
			return nil, err
		} else if tag.RowsAffected() != 1 {
			return nil, ErrMemoryCurationConflict
		}
		results[index] = result
	}
	return results, nil
}

func appendMemoryCurationRollbackVersionTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	actionID, memoryID string,
	restore *MemoryVersionReference,
	state, requestHash string,
) (MemoryVersionReference, error) {
	current, err := loadCurrentMemory(ctx, tx, p, memoryID, false)
	if err != nil {
		return MemoryVersionReference{}, err
	}
	base := current
	if restore != nil {
		base, err = loadMemoryAtVersion(ctx, tx, p, restore.MemoryID, restore.Version, false)
		if err != nil {
			return MemoryVersionReference{}, err
		}
	}
	seq, err := allocateMemoryChangeSeq(ctx, tx, p)
	if err != nil {
		return MemoryVersionReference{}, err
	}
	next := base
	next.ID = current.ID
	next.AccountID, next.RealmID = current.AccountID, current.RealmID
	next.OwnerKind, next.OwnerID = current.OwnerKind, current.OwnerID
	next.Origin, next.CaptureReason = current.Origin, current.CaptureReason
	next.AuthoredByAgentID = current.AuthoredByAgentID
	next.Version = current.Version + 1
	next.PreviousVersion = current.Version
	next.ChangeSeq = seq
	next.State = state
	next.PriorState = ""
	next.LifecycleReason = "curation_rollback"
	next.ActorKind, next.ActorID = "agent", p.ID
	next.Operation = "reverted"
	next.IdempotencyKey = "curation:" + run.ID + ":" + actionID + ":rollback"
	next.RequestHash = requestHash
	next.Client = run.Client
	next.CurationRunID, next.CurationActionID = run.ID, actionID
	next.SupersessionSetID = ""
	next.SupersessionSetRevision = 0
	next.SupersessionReplacementCount = 0
	next.SupersessionReplacementDigest = ""
	next.ActiveSupersessionSetID = ""
	next.ActiveSupersessionSetRevision = 0
	next.Evidence = nil
	createdAt, err := insertMemoryVersionTx(ctx, tx, next)
	if err != nil {
		return MemoryVersionReference{}, err
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE memories SET current_version=$2,updated_at=$3
		WHERE id=$1 AND current_version=$4`, memoryID, next.Version,
		createdAt, current.Version); err != nil {
		return MemoryVersionReference{}, err
	} else if tag.RowsAffected() != 1 {
		return MemoryVersionReference{}, ErrMemoryCurationConflict
	}
	return MemoryVersionReference{MemoryID: memoryID, Version: next.Version}, nil
}

func createMemoryCurationRollbackReplayTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	lane *MemoryCurationLane,
	run MemoryCurationRun,
	request MemoryCurationRequest,
) (MemoryCurationRequest, error) {
	if lane.RequestGeneration >= maxMemoryCurationGeneration {
		return MemoryCurationRequest{}, ErrMemoryCurationConflict
	}
	generation := lane.RequestGeneration + 1
	if tag, err := tx.Exec(ctx, `
		UPDATE memory_curation_lanes
		SET request_generation=$5,updated_at=clock_timestamp()
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND request_generation=$4 AND active_run_id IS NULL`, p.AccountID,
		p.RealmID, p.ID, lane.RequestGeneration, generation); err != nil {
		return MemoryCurationRequest{}, err
	} else if tag.RowsAffected() != 1 {
		return MemoryCurationRequest{}, ErrMemoryCurationConflict
	}
	requestID, err := id.New("mcrq")
	if err != nil {
		return MemoryCurationRequest{}, err
	}
	scope, err := json.Marshal(request.Scope)
	if err != nil {
		return MemoryCurationRequest{}, err
	}
	key := "curation-rollback-replay:" + run.ID
	hash, err := memoryRequestHash(struct {
		Operation  string `json:"operation"`
		RunID      string `json:"run_id"`
		Generation int64  `json:"generation"`
	}{Operation: "rollback_replay", RunID: run.ID, Generation: generation})
	if err != nil {
		return MemoryCurationRequest{}, err
	}
	out, err := scanMemoryCurationRequest(tx.QueryRow(ctx, `
		INSERT INTO memory_curation_requests
		  (id,account_id,realm_id,owner_kind,owner_id,scope,coalescing_key,
		   trigger_reason,request_generation,priority,due_at,state,attempt_count,
		   max_attempts,fulfilled_generation,replay_run_id,read_only_replay,
		   actor_kind,actor_id,idempotency_key,request_hash)
		VALUES ($1,$2,$3,'agent',$4,$5::jsonb,$6,'curation_rollback',$7,$8,
		        clock_timestamp(),'queued',0,$9,0,$10,true,'agent',$4,$11,$12)
		RETURNING id,account_id,realm_id,owner_kind,owner_id,scope,coalescing_key,
		          trigger_reason,request_generation,priority,due_at,state,attempt_count,
		          max_attempts,COALESCE(claimed_run_id,''),fulfilled_generation,
		          COALESCE(replay_run_id,''),read_only_replay,actor_kind,actor_id,
		          idempotency_key,request_hash,claimed_at,fulfilled_at,cancelled_at,
		          dead_lettered_at,created_at,updated_at`, requestID, p.AccountID,
		p.RealmID, p.ID, string(scope), "rollback."+run.ID, generation,
		request.Priority, request.MaxAttempts, run.ID, key, hash))
	if err != nil {
		return MemoryCurationRequest{}, fmt.Errorf("queue rollback replay: %w", err)
	}
	return out, nil
}

func loadMemoryCurationRollbackResult(
	ctx context.Context,
	q memoryQuerier,
	p Principal,
	mutation MemoryCurationMutationReceipt,
	in RollbackMemoryCurationInput,
	replayed bool,
) (RollbackMemoryCurationResult, error) {
	run, err := loadMemoryCurationRun(ctx, q, p, mutation.RunID, false)
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	if run.State != MemoryCurationRunRolledBack || run.RollbackReceiptID != mutation.ReceiptID ||
		run.ApplyReceiptID != in.ApplyReceiptID {
		return RollbackMemoryCurationResult{}, ErrMemoryCurationConflict
	}
	var replayID string
	if err := q.QueryRow(ctx, `
		SELECT id FROM memory_curation_requests
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND idempotency_key=$4`, p.AccountID, p.RealmID, p.ID,
		"curation-rollback-replay:"+run.ID).Scan(&replayID); err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	replayRequest, err := loadMemoryCurationRequest(ctx, q, p, replayID, false)
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	rows, err := q.Query(ctx, `
		SELECT rollback_result::text FROM memory_curation_actions
		WHERE run_id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4 AND state='reverted'
		ORDER BY ordinal`, run.ID, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return RollbackMemoryCurationResult{}, err
	}
	actions := make([]MemoryCurationActionRollbackResult, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return RollbackMemoryCurationResult{}, err
		}
		var action MemoryCurationActionRollbackResult
		if err := decodeMemoryCurationStoredJSON(raw, &action); err != nil {
			rows.Close()
			return RollbackMemoryCurationResult{}, ErrMemoryCurationConflict
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return RollbackMemoryCurationResult{}, err
	}
	rows.Close()
	receipt := MemoryCurationRollbackReceipt{
		ID: mutation.ReceiptID, Operation: "rollback", ActorID: p.ID,
		IdempotencyKey: mutation.IdempotencyKey, RequestHash: mutation.RequestHash,
		RequestID: run.RequestID, RunID: run.ID, ApplyReceiptID: in.ApplyReceiptID,
		ExpectedProducedHeads: append([]MemoryVersionReference(nil), in.ExpectedProducedHeads...),
		ActionResults:         actions, ReplayRequestID: replayRequest.ID,
		ReplayGeneration: replayRequest.RequestGeneration,
		CreatedAt:        mutation.CreatedAt, Replayed: replayed,
	}
	return RollbackMemoryCurationResult{Run: run, ReplayRequest: replayRequest, Receipt: receipt}, nil
}
