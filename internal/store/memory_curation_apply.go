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

// ApplyMemoryCurationInput identifies the one immutable accepted plan. The
// plan itself is never accepted again at this boundary: apply reconstructs it
// from the validated action rows and verifies the run-level hash.
type ApplyMemoryCurationInput struct {
	FencingGeneration int64  `json:"fencing_generation"`
	PlanRevision      int64  `json:"plan_revision"`
	PlanHash          string `json:"plan_hash"`
	IdempotencyKey    string `json:"idempotency_key"`
}

// MemoryCurationHeadChange is a value-free optimistic mutation receipt.
type MemoryCurationHeadChange struct {
	MemoryID      string `json:"memory_id"`
	BeforeVersion int64  `json:"before_version,omitempty"`
	AfterVersion  int64  `json:"after_version"`
}

// MemoryCurationCursorInterval proves the exact frozen interval advanced by a
// successful apply. Cursors are monotonic and rollback never rewinds them.
type MemoryCurationCursorInterval struct {
	SourceKind     string `json:"source_kind"`
	SourceStreamID string `json:"source_stream_id"`
	ExpectedPrior  int64  `json:"expected_prior"`
	Upper          int64  `json:"upper"`
}

// MemoryCurationActionApplyResult is persisted in applied_result. It contains
// identifiers and versions only; value-bearing plan data remains solely in
// proposed_payload and the resources produced by the action.
type MemoryCurationActionApplyResult struct {
	ActionID                   string                   `json:"action_id"`
	Ordinal                    int64                    `json:"ordinal"`
	Operation                  string                   `json:"operation"`
	BeforeHeads                []MemoryVersionReference `json:"before_heads"`
	AfterHeads                 []MemoryVersionReference `json:"after_heads"`
	CreatedMemoryIDs           []string                 `json:"created_memory_ids"`
	EvidenceIDs                []string                 `json:"evidence_ids"`
	RelationIDs                []string                 `json:"relation_ids"`
	CandidateIDs               []string                 `json:"candidate_ids"`
	SupersessionSetID          string                   `json:"supersession_set_id,omitempty"`
	SupersessionSetRevision    int64                    `json:"supersession_set_revision,omitempty"`
	SupersessionReplacementIDs []MemoryVersionReference `json:"supersession_replacements,omitempty"`
}

// MemoryCurationApplyReceipt is an exact, value-free replay receipt.
type MemoryCurationApplyReceipt struct {
	ID                 string                            `json:"id"`
	Operation          string                            `json:"operation"`
	ActorID            string                            `json:"actor_id"`
	IdempotencyKey     string                            `json:"idempotency_key"`
	RequestHash        string                            `json:"request_hash"`
	RequestID          string                            `json:"request_id"`
	RunID              string                            `json:"run_id"`
	RequestGeneration  int64                             `json:"request_generation"`
	FencingGeneration  int64                             `json:"fencing_generation"`
	PlanRevision       int64                             `json:"plan_revision"`
	PlanHash           string                            `json:"plan_hash"`
	ActionResults      []MemoryCurationActionApplyResult `json:"action_results"`
	CursorIntervals    []MemoryCurationCursorInterval    `json:"cursor_intervals"`
	FollowUpRequestID  string                            `json:"follow_up_request_id,omitempty"`
	FollowUpGeneration int64                             `json:"follow_up_generation,omitempty"`
	CreatedAt          time.Time                         `json:"created_at"`
	Replayed           bool                              `json:"replayed,omitempty"`
}

// ApplyMemoryCurationResult reports the terminal run state, its request, any
// follow-up work, and the durable apply receipt.
type ApplyMemoryCurationResult struct {
	Run             MemoryCurationRun          `json:"run"`
	Request         MemoryCurationRequest      `json:"request"`
	FollowUpRequest *MemoryCurationRequest     `json:"follow_up_request,omitempty"`
	Receipt         MemoryCurationApplyReceipt `json:"receipt"`
}

var errMemoryCurationApplyStale = errors.New("curation apply snapshot is stale")

// ApplyCuration atomically applies one fenced accepted plan, advances every
// frozen input cursor with compare-and-swap, fulfills the captured generation,
// and releases the owner lane. All semantic judgment remains client-side.
func (s *Store) ApplyCuration(
	ctx context.Context,
	p Principal,
	runID string,
	in ApplyMemoryCurationInput,
) (ApplyMemoryCurationResult, error) {
	if p.Kind != PrincipalAgent {
		return ApplyMemoryCurationResult{}, ErrMemoryCurationForbidden
	}
	runID = strings.TrimSpace(runID)
	in.PlanHash = strings.ToLower(strings.TrimSpace(in.PlanHash))
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if !validCurationID(runID, "mrun") || in.FencingGeneration < 1 ||
		in.PlanRevision < 1 || !isSHA256Hex(in.PlanHash) ||
		validateMemoryIdempotencyKey(in.IdempotencyKey) != nil {
		return ApplyMemoryCurationResult{}, ErrMemoryCurationInputInvalid
	}
	hashInput := in
	hashInput.IdempotencyKey = ""
	requestHash, err := memoryRequestHash(struct {
		Operation string                   `json:"operation"`
		RunID     string                   `json:"run_id"`
		Input     ApplyMemoryCurationInput `json:"input"`
	}{Operation: "apply", RunID: runID, Input: hashInput})
	if err != nil {
		return ApplyMemoryCurationResult{}, ErrMemoryCurationInputInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prepareMemoryCurationMutationTx(ctx, tx, p, "apply", in.IdempotencyKey); err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	if err := authorizeMemoryCurationRunID(ctx, tx, p, runID); err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	if mutation, replayed, err := loadMemoryCurationMutation(
		ctx, tx, p, "apply", in.IdempotencyKey, requestHash,
	); err != nil || replayed {
		if err != nil {
			return ApplyMemoryCurationResult{}, err
		}
		if mutation.RunID != runID || mutation.FencingGeneration != in.FencingGeneration ||
			mutation.PlanRevision != in.PlanRevision || mutation.PlanHash != in.PlanHash {
			return ApplyMemoryCurationResult{}, ErrMemoryCurationIdempotencyConflict
		}
		if mutation.ResultState == MemoryCurationRunConflict {
			if err := tx.Commit(ctx); err != nil {
				return ApplyMemoryCurationResult{}, err
			}
			return ApplyMemoryCurationResult{}, ErrMemoryCurationConflict
		}
		out, err := loadMemoryCurationApplyResult(ctx, tx, p, mutation, true)
		if err != nil {
			return ApplyMemoryCurationResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ApplyMemoryCurationResult{}, err
		}
		return out, nil
	}

	lane, err := loadMemoryCurationLaneTx(ctx, tx, p, true)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	expired, err := interruptExpiredCurationRunTx(ctx, tx, p, &lane)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	if expired {
		if err := tx.Commit(ctx); err != nil {
			return ApplyMemoryCurationResult{}, err
		}
		return ApplyMemoryCurationResult{}, ErrMemoryCurationLeaseExpired
	}
	if lane.ActiveRunID != runID || lane.FencingGeneration != in.FencingGeneration {
		return ApplyMemoryCurationResult{}, ErrMemoryCurationFenceMismatch
	}
	// The curation owner lane is the first owner-scoped data lock for every
	// memory apply. Fact mutations do not acquire the curation lane, so taking
	// their shared subject-namespace lock second cannot form a lock cycle.
	if err := lockFactSubjectNamespace(ctx, tx, p, false); err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	run, err := loadMemoryCurationRun(ctx, tx, p, runID, true)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	if run.FencingGeneration != in.FencingGeneration {
		return ApplyMemoryCurationResult{}, ErrMemoryCurationFenceMismatch
	}
	if run.State != MemoryCurationRunPlanned || run.PlanRevision != in.PlanRevision ||
		run.PlanHash != in.PlanHash || run.LeaseExpiresAt == nil {
		return ApplyMemoryCurationResult{}, ErrMemoryCurationConflict
	}
	request, err := loadMemoryCurationRequest(ctx, tx, p, run.RequestID, true)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	if err := authorizeMemoryCurationRequestScope(p, request); err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	if request.State != MemoryCurationRequestClaimed || request.ClaimedRunID != run.ID {
		return ApplyMemoryCurationResult{}, ErrMemoryCurationConflict
	}
	stored, err := loadMemoryCurationStoredPlan(ctx, tx, p, run)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	if stored.Acceptance.Plan.PlanRevision != in.PlanRevision ||
		stored.Acceptance.PlanHash != in.PlanHash {
		return ApplyMemoryCurationResult{}, ErrMemoryCurationConflict
	}

	// This lock is deliberately acquired before all memory heads, matching
	// direct writes and permanent deletion. The nested transaction below is a
	// savepoint so stale heads/cursors can discard every produced row while the
	// outer transaction records one durable conflict result.
	if err := lockMemoryCurationChangeClockTx(ctx, tx, p); err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	work, err := tx.Begin(ctx)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	actionResults, cursorIntervals, applyErr := applyMemoryCurationPlanTx(
		ctx, work, p, run, stored,
	)
	if applyErr != nil {
		_ = work.Rollback(ctx)
		if !errors.Is(applyErr, errMemoryCurationApplyStale) &&
			!errors.Is(applyErr, ErrMemoryConflict) &&
			!errors.Is(applyErr, ErrMemoryNotFound) &&
			!errors.Is(applyErr, ErrMemoryInputInvalid) &&
			!errors.Is(applyErr, ErrFactDeleted) {
			return ApplyMemoryCurationResult{}, applyErr
		}
		if err := markMemoryCurationApplyConflictTx(ctx, tx, p, &lane, run, request,
			in, requestHash, in.IdempotencyKey, "snapshot_stale"); err != nil {
			return ApplyMemoryCurationResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ApplyMemoryCurationResult{}, err
		}
		return ApplyMemoryCurationResult{}, ErrMemoryCurationConflict
	}
	if err := work.Commit(ctx); err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	backlog, err := hasMemoryCurationSourceBacklogTx(ctx, tx, p, run, request)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}

	receiptID, err := id.New("mrec")
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	request, followUp, err := fulfillMemoryCurationGenerationTx(
		ctx, tx, p, &lane, run, request, backlog,
	)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE memory_curation_runs
		SET state='applied',lease_expires_at=NULL,apply_receipt_id=$2,
		    applied_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE id=$1 AND state='planned' AND plan_revision=$3 AND plan_hash=$4
		  AND lease_expires_at > clock_timestamp()`, run.ID, receiptID,
		run.PlanRevision, run.PlanHash); err != nil {
		return ApplyMemoryCurationResult{}, fmt.Errorf("mark curation run applied: %w", err)
	} else if tag.RowsAffected() != 1 {
		return ApplyMemoryCurationResult{}, ErrMemoryCurationLeaseExpired
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE memory_curation_lanes
		SET active_run_id=NULL,updated_at=clock_timestamp()
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND active_run_id=$4 AND fencing_generation=$5`, p.AccountID, p.RealmID,
		p.ID, run.ID, run.FencingGeneration); err != nil {
		return ApplyMemoryCurationResult{}, fmt.Errorf("release applied curation lane: %w", err)
	} else if tag.RowsAffected() != 1 {
		return ApplyMemoryCurationResult{}, ErrMemoryCurationConflict
	}
	mutation, err := insertMemoryCurationMutation(ctx, tx, p, MemoryCurationMutationReceipt{
		Operation: "apply", ActorID: p.ID, IdempotencyKey: in.IdempotencyKey,
		RequestHash: requestHash, RequestID: run.RequestID, RunID: run.ID,
		RequestGeneration: run.RequestGeneration,
		FencingGeneration: run.FencingGeneration, PlanRevision: run.PlanRevision,
		PlanHash: run.PlanHash, ResultState: MemoryCurationRunApplied,
		ReceiptID: receiptID,
	})
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	if err := logMemoryCurationEventTx(ctx, tx, p, VerbMemoryCurationApplied,
		run.RequestID, run.ID, run.RequestGeneration, run.FencingGeneration,
		MemoryCurationRunApplied); err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	run, err = loadMemoryCurationRun(ctx, tx, p, run.ID, false)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	receipt := MemoryCurationApplyReceipt{
		ID: receiptID, Operation: "apply", ActorID: p.ID,
		IdempotencyKey: in.IdempotencyKey, RequestHash: requestHash,
		RequestID: run.RequestID, RunID: run.ID,
		RequestGeneration: run.RequestGeneration,
		FencingGeneration: run.FencingGeneration, PlanRevision: run.PlanRevision,
		PlanHash: run.PlanHash, ActionResults: actionResults,
		CursorIntervals: cursorIntervals, CreatedAt: mutation.CreatedAt,
	}
	if followUp != nil {
		receipt.FollowUpRequestID = followUp.ID
		receipt.FollowUpGeneration = followUp.RequestGeneration
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	return ApplyMemoryCurationResult{
		Run: run, Request: request, FollowUpRequest: followUp, Receipt: receipt,
	}, nil
}

func lockMemoryCurationChangeClockTx(ctx context.Context, tx pgx.Tx, p Principal) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_change_clocks
		  (account_id,realm_id,owner_kind,owner_id)
		VALUES ($1,$2,'agent',$3) ON CONFLICT DO NOTHING`,
		p.AccountID, p.RealmID, p.ID); err != nil {
		return fmt.Errorf("initialize curation memory clock: %w", err)
	}
	var position int64
	if err := tx.QueryRow(ctx, `
		SELECT last_change_seq FROM memory_change_clocks
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		FOR UPDATE`, p.AccountID, p.RealmID, p.ID).Scan(&position); err != nil {
		return fmt.Errorf("lock curation memory clock: %w", err)
	}
	return nil
}

type memoryCurationLockedHead struct {
	CurrentVersion int64
	Current        Memory
}

func applyMemoryCurationPlanTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	stored memoryCurationStoredPlan,
) ([]MemoryCurationActionApplyResult, []MemoryCurationCursorInterval, error) {
	created := make(map[string]struct{})
	for _, row := range stored.Actions {
		if row.Action.Create != nil {
			created[row.Action.Create.MemoryID] = struct{}{}
		}
	}

	lockIDs := make(map[string]struct{})
	for _, row := range stored.Actions {
		for _, ref := range row.InputRefs {
			if ref.MemoryID == "" {
				continue
			}
			if _, isCreated := created[ref.MemoryID]; !isCreated {
				lockIDs[ref.MemoryID] = struct{}{}
			}
		}
		for _, head := range row.ExpectedHeads {
			if _, isCreated := created[head.MemoryID]; !isCreated {
				lockIDs[head.MemoryID] = struct{}{}
			}
		}
	}
	orderedIDs := make([]string, 0, len(lockIDs))
	for memoryID := range lockIDs {
		orderedIDs = append(orderedIDs, memoryID)
	}
	sort.Strings(orderedIDs)
	locked := make(map[string]memoryCurationLockedHead, len(orderedIDs))
	for _, memoryID := range orderedIDs {
		var currentVersion sql.NullInt64
		err := tx.QueryRow(ctx, `
			SELECT current_version FROM memories
			WHERE id=$1 AND account_id=$2 AND realm_id=$3
			  AND owner_kind='agent' AND owner_id=$4
			FOR UPDATE`, memoryID, p.AccountID, p.RealmID, p.ID).Scan(&currentVersion)
		if errors.Is(err, pgx.ErrNoRows) || !currentVersion.Valid {
			return nil, nil, errMemoryCurationApplyStale
		}
		if err != nil {
			return nil, nil, fmt.Errorf("lock curation memory head: %w", err)
		}
		current, err := loadMemoryAtVersion(ctx, tx, p, memoryID, currentVersion.Int64, false)
		if err != nil {
			return nil, nil, err
		}
		locked[memoryID] = memoryCurationLockedHead{
			CurrentVersion: currentVersion.Int64, Current: current,
		}
		// A plan may deliberately cite an immutable historical version, but it
		// may never continue to derive from a memory whose current lifecycle
		// state became forgotten, reverted, or superseded after acceptance.
		if current.State != MemoryStateActive {
			return nil, nil, errMemoryCurationApplyStale
		}
	}
	for memoryID := range created {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memories WHERE id=$1)`, memoryID).Scan(&exists); err != nil {
			return nil, nil, err
		}
		if exists {
			return nil, nil, errMemoryCurationApplyStale
		}
	}
	for _, row := range stored.Actions {
		for _, ref := range row.InputRefs {
			if ref.MemoryID == "" {
				continue
			}
			if _, isCreated := created[ref.MemoryID]; isCreated {
				continue
			}
			memory, err := loadMemoryAtVersion(ctx, tx, p, ref.MemoryID, ref.Version, false)
			if err != nil || memory.State == MemoryStateForgotten || memory.State == MemoryStateReverted {
				return nil, nil, errMemoryCurationApplyStale
			}
		}
		for _, expected := range row.ExpectedHeads {
			if _, isCreated := created[expected.MemoryID]; isCreated {
				continue
			}
			head, ok := locked[expected.MemoryID]
			if !ok || head.CurrentVersion != expected.ExpectedVersion || head.Current.State != MemoryStateActive {
				return nil, nil, errMemoryCurationApplyStale
			}
		}
	}

	results := make([]MemoryCurationActionApplyResult, 0, len(stored.Actions))
	for _, row := range stored.Actions {
		result, err := applyMemoryCurationActionTx(ctx, tx, p, run, row)
		if err != nil {
			return nil, nil, err
		}
		resultJSON, err := canonicalMemoryCurationJSON(result)
		if err != nil {
			return nil, nil, err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE memory_curation_actions
			SET state='applied',applied_result=$2::jsonb,
			    applied_at=clock_timestamp()
			WHERE id=$1 AND run_id=$3 AND state='validated'`,
			row.ID, string(resultJSON), run.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("record curation action result: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return nil, nil, errMemoryCurationApplyStale
		}
		results = append(results, result)
	}
	intervals, err := advanceMemoryCurationCursorsTx(ctx, tx, p, run.ID)
	if err != nil {
		return nil, nil, err
	}
	return results, intervals, nil
}

func applyMemoryCurationActionTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	row memoryCurationStoredAction,
) (MemoryCurationActionApplyResult, error) {
	result := MemoryCurationActionApplyResult{
		ActionID: row.ID, Ordinal: row.Action.Ordinal, Operation: row.Action.Operation,
		BeforeHeads:      make([]MemoryVersionReference, 0),
		AfterHeads:       make([]MemoryVersionReference, 0),
		CreatedMemoryIDs: make([]string, 0), EvidenceIDs: make([]string, 0),
		RelationIDs: make([]string, 0), CandidateIDs: make([]string, 0),
	}
	switch row.Action.Operation {
	case MemoryCurationOperationCreate:
		return applyMemoryCurationCreateTx(ctx, tx, p, run, row, result)
	case MemoryCurationOperationReplace:
		return applyMemoryCurationReplaceTx(ctx, tx, p, run, row, result)
	case MemoryCurationOperationSupersede:
		return applyMemoryCurationSupersedeTx(ctx, tx, p, run, row, result)
	case MemoryCurationOperationRelate:
		return applyMemoryCurationRelateTx(ctx, tx, p, run, row, result)
	case MemoryCurationOperationProposeFact:
		return applyMemoryCurationProposeFactTx(ctx, tx, p, run, row, result)
	default:
		return result, ErrMemoryCurationConflict
	}
}

func applyMemoryCurationCreateTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	row memoryCurationStoredAction,
	result MemoryCurationActionApplyResult,
) (MemoryCurationActionApplyResult, error) {
	action := row.Action.Create
	if action == nil {
		return result, ErrMemoryCurationConflict
	}
	seq, err := allocateMemoryChangeSeq(ctx, tx, p)
	if err != nil {
		return result, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memories
		  (id,account_id,realm_id,owner_kind,owner_id,origin,capture_reason,
		   authored_by_agent_id,current_version)
		VALUES ($1,$2,$3,'agent',$4,'agent','curation',$4,1)`,
		action.MemoryID, p.AccountID, p.RealmID, p.ID); err != nil {
		return result, fmt.Errorf("insert curated memory head: %w", err)
	}
	memory := memoryFromCurationSnapshot(p, run, row.ID, action.MemoryID, 1, 0,
		seq, action.Snapshot, MemoryStateActive, "", "added")
	createdAt, err := insertMemoryVersionTx(ctx, tx, memory)
	if err != nil {
		return result, err
	}
	evidenceIDs, err := insertMemoryCurationEvidenceListTx(ctx, tx, p,
		action.MemoryID, 1, action.Snapshot.Evidence)
	if err != nil {
		return result, err
	}
	if _, err := tx.Exec(ctx, `UPDATE memories SET updated_at=$2 WHERE id=$1`,
		action.MemoryID, createdAt); err != nil {
		return result, err
	}
	result.CreatedMemoryIDs = append(result.CreatedMemoryIDs, action.MemoryID)
	result.AfterHeads = append(result.AfterHeads, MemoryVersionReference{MemoryID: action.MemoryID, Version: 1})
	result.EvidenceIDs = append(result.EvidenceIDs, evidenceIDs...)
	for _, relation := range action.Relations {
		relationID, err := insertMemoryCurationRelationTx(ctx, tx, p, run.ID,
			row.ID, relation.RelationType,
			MemoryCurationVersionReference{MemoryID: action.MemoryID, Version: 1},
			relation.To, "", 0)
		if err != nil {
			return result, err
		}
		result.RelationIDs = append(result.RelationIDs, relationID)
	}
	if err := logMemoryVersionEventTx(ctx, tx, p, memory); err != nil {
		return result, err
	}
	return result, nil
}

func applyMemoryCurationReplaceTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	row memoryCurationStoredAction,
	result MemoryCurationActionApplyResult,
) (MemoryCurationActionApplyResult, error) {
	action := row.Action.Replace
	if action == nil {
		return result, ErrMemoryCurationConflict
	}
	current, err := loadCurrentMemory(ctx, tx, p, action.Target.MemoryID, false)
	if err != nil || current.Version != action.Target.ExpectedVersion || current.State != MemoryStateActive {
		return result, errMemoryCurationApplyStale
	}
	seq, err := allocateMemoryChangeSeq(ctx, tx, p)
	if err != nil {
		return result, err
	}
	next := memoryFromCurationSnapshot(p, run, row.ID, current.ID,
		current.Version+1, current.Version, seq, action.Snapshot,
		MemoryStateActive, action.Reason, "adjusted")
	next.AccountID, next.RealmID, next.OwnerKind, next.OwnerID = current.AccountID, current.RealmID, current.OwnerKind, current.OwnerID
	next.Origin, next.CaptureReason, next.AuthoredByAgentID = current.Origin, current.CaptureReason, current.AuthoredByAgentID
	if memoryPayloadEqual(current, next) {
		return result, errMemoryCurationApplyStale
	}
	createdAt, err := insertMemoryVersionTx(ctx, tx, next)
	if err != nil {
		return result, err
	}
	evidenceIDs, err := insertMemoryCurationEvidenceListTx(ctx, tx, p,
		current.ID, next.Version, action.Snapshot.Evidence)
	if err != nil {
		return result, err
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE memories SET current_version=$2,updated_at=$3
		WHERE id=$1 AND current_version=$4`, current.ID, next.Version,
		createdAt, current.Version); err != nil {
		return result, err
	} else if tag.RowsAffected() != 1 {
		return result, errMemoryCurationApplyStale
	}
	result.BeforeHeads = append(result.BeforeHeads, MemoryVersionReference{MemoryID: current.ID, Version: current.Version})
	result.AfterHeads = append(result.AfterHeads, MemoryVersionReference{MemoryID: current.ID, Version: next.Version})
	result.EvidenceIDs = append(result.EvidenceIDs, evidenceIDs...)
	if err := logMemoryVersionEventTx(ctx, tx, p, next); err != nil {
		return result, err
	}
	return result, nil
}

func applyMemoryCurationSupersedeTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	row memoryCurationStoredAction,
	result MemoryCurationActionApplyResult,
) (MemoryCurationActionApplyResult, error) {
	action := row.Action.Supersede
	if action == nil || len(action.Replacements) == 0 {
		return result, ErrMemoryCurationConflict
	}
	current, err := loadCurrentMemory(ctx, tx, p, action.Target.MemoryID, false)
	if err != nil || current.Version != action.Target.ExpectedVersion || current.State != MemoryStateActive {
		return result, errMemoryCurationApplyStale
	}
	for _, replacement := range action.Replacements {
		memory, err := loadCurrentMemory(ctx, tx, p, replacement.MemoryID, false)
		if err != nil || memory.Version != replacement.Version || memory.State != MemoryStateActive {
			return result, errMemoryCurationApplyStale
		}
	}
	setID, err := id.New("mset")
	if err != nil {
		return result, err
	}
	const setRevision int64 = 1
	replacements := make([]MemoryVersionReference, len(action.Replacements))
	for index, replacement := range action.Replacements {
		replacements[index] = MemoryVersionReference{
			MemoryID: replacement.MemoryID, Version: replacement.Version,
		}
	}
	seq, err := allocateMemoryChangeSeq(ctx, tx, p)
	if err != nil {
		return result, err
	}
	next := current
	next.PreviousVersion = current.Version
	next.Version = current.Version + 1
	next.ChangeSeq = seq
	next.State = MemoryStateSuperseded
	next.PriorState = ""
	next.LifecycleReason = action.Reason
	next.ActorKind = "agent"
	next.ActorID = p.ID
	next.Operation = "superseded"
	next.IdempotencyKey = fmt.Sprintf("curation:%s:%s:superseded", run.ID, row.ID)
	next.RequestHash = run.PlanHash
	next.Client = run.Client
	next.CurationRunID = run.ID
	next.CurationActionID = row.ID
	next.SupersessionSetID = setID
	next.SupersessionSetRevision = setRevision
	next.SupersessionReplacementCount = int64(len(replacements))
	next.SupersessionReplacementDigest = memorySupersessionMembershipDigest(replacements)
	next.ActiveSupersessionSetID = ""
	next.ActiveSupersessionSetRevision = 0
	next.Evidence = nil
	createdAt, err := insertMemoryVersionTx(ctx, tx, next)
	if err != nil {
		return result, err
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE memories SET current_version=$2,updated_at=$3
		WHERE id=$1 AND current_version=$4`, current.ID, next.Version,
		createdAt, current.Version); err != nil {
		return result, err
	} else if tag.RowsAffected() != 1 {
		return result, errMemoryCurationApplyStale
	}
	for _, replacement := range action.Replacements {
		relationID, err := insertMemoryCurationRelationTx(ctx, tx, p, run.ID,
			row.ID, "supersedes", replacement,
			MemoryCurationVersionReference{MemoryID: next.ID, Version: next.Version},
			setID, setRevision)
		if err != nil {
			return result, err
		}
		result.RelationIDs = append(result.RelationIDs, relationID)
	}
	result.BeforeHeads = append(result.BeforeHeads, MemoryVersionReference{
		MemoryID: current.ID, Version: current.Version,
	})
	result.AfterHeads = append(result.AfterHeads, MemoryVersionReference{
		MemoryID: next.ID, Version: next.Version,
	})
	result.SupersessionSetID = setID
	result.SupersessionSetRevision = setRevision
	result.SupersessionReplacementIDs = replacements
	if err := logMemoryVersionEventTx(ctx, tx, p, next); err != nil {
		return result, err
	}
	return result, nil
}

func applyMemoryCurationRelateTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	row memoryCurationStoredAction,
	result MemoryCurationActionApplyResult,
) (MemoryCurationActionApplyResult, error) {
	action := row.Action.Relate
	if action == nil {
		return result, ErrMemoryCurationConflict
	}
	relationID, err := insertMemoryCurationRelationTx(ctx, tx, p, run.ID,
		row.ID, action.RelationType, action.From, action.To, "", 0)
	if err != nil {
		return result, err
	}
	result.RelationIDs = append(result.RelationIDs, relationID)
	return result, nil
}

func applyMemoryCurationProposeFactTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	row memoryCurationStoredAction,
	result MemoryCurationActionApplyResult,
) (MemoryCurationActionApplyResult, error) {
	action := row.Action.ProposeFact
	if action == nil || action.Confidence == nil {
		return result, ErrMemoryCurationConflict
	}
	for index, evidence := range action.Evidence {
		if err := validateMemoryEvidenceSourceTx(ctx, tx, p,
			memoryCurationEvidenceInput(evidence)); err != nil {
			return result, fmt.Errorf("validate curation fact evidence %d: %w", index, err)
		}
	}
	// Plan acceptance resolves aliases to one canonical subject while holding
	// the shared subject-namespace lock, then includes that key in the plan
	// hash. Apply consumes only that persisted canonical value.
	subject := action.Subject
	if !factSubjectPattern.MatchString(subject) || normalizeFactSubject(subject) != subject {
		return result, ErrMemoryCurationConflict
	}
	deleted, err := factAddressHasUnrecreatedTombstone(ctx, tx, p, subject, action.Predicate)
	if err != nil {
		return result, err
	}
	if deleted {
		return result, ErrFactDeleted
	}
	status, conflictID, observedAssertionID := "pending", "", ""
	var currentFactID string
	var currentSensitive, sameValue bool
	err = tx.QueryRow(ctx, `
		SELECT f.id,a.id,
		       a.value=$6::jsonb AND a.value_type=$7 AND a.recurrence=$8,
		       f.sensitive
		FROM facts f JOIN fact_subjects s ON s.id=f.subject_id
		JOIN fact_assertions a ON a.id=f.resolved_assertion_id
		WHERE f.account_id=$1 AND f.realm_id=$2 AND f.owner_agent_id=$3
		  AND s.canonical_key=$4 AND f.predicate=$5 AND f.deleted_at IS NULL`,
		p.AccountID, p.RealmID, p.ID, subject, action.Predicate,
		string(action.Value), action.ValueType, action.Recurrence).Scan(
		&currentFactID, &observedAssertionID, &sameValue, &currentSensitive)
	if err == nil {
		action.Sensitive = action.Sensitive || currentSensitive
		if !sameValue {
			status, conflictID = "conflict", currentFactID
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	candidateID, err := id.New("fcand")
	if err != nil {
		return result, err
	}
	sourceRef := "curation:" + run.ID + ":" + row.ID
	if _, err := tx.Exec(ctx, `
		INSERT INTO fact_candidates
		  (id,account_id,realm_id,owner_agent_id,subject_key,predicate,value_type,
		   value,recurrence,cardinality,sensitive,source_ref,confidence,observed_at,
		   valid_from,valid_until,reason,status,conflict_fact_id,
		   observed_assertion_id,curation_run_id,curation_action_id,proposed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12,$13,
		        clock_timestamp(),$14,$15,$16,$17,NULLIF($18,''),NULLIF($19,''),
		        $20,$21,clock_timestamp())`, candidateID, p.AccountID, p.RealmID,
		p.ID, subject, action.Predicate, action.ValueType, string(action.Value),
		action.Recurrence, action.Cardinality, action.Sensitive, sourceRef,
		*action.Confidence, action.ValidFrom, action.ValidUntil, action.Reason,
		status, conflictID, observedAssertionID, run.ID, row.ID); err != nil {
		return result, fmt.Errorf("insert curation fact candidate: %w", err)
	}
	result.CandidateIDs = append(result.CandidateIDs, candidateID)
	return result, nil
}

func memoryFromCurationSnapshot(
	p Principal,
	run MemoryCurationRun,
	actionID, memoryID string,
	version, previousVersion, changeSeq int64,
	snapshot MemoryCurationMemorySnapshot,
	state, reason, operation string,
) Memory {
	return Memory{
		ID: memoryID, AccountID: p.AccountID, RealmID: p.RealmID,
		OwnerKind: "agent", OwnerID: p.ID, Origin: "agent",
		CaptureReason: "curation", AuthoredByAgentID: p.ID,
		Version: version, PreviousVersion: previousVersion, ChangeSeq: changeSeq,
		Content: snapshot.Content, ContentEncoding: snapshot.ContentEncoding,
		Kind: snapshot.Kind, Tags: append(make([]string, 0, len(snapshot.Tags)), snapshot.Tags...),
		Links: append(make([]string, 0, len(snapshot.Links)), snapshot.Links...), Salience: *snapshot.Salience,
		Sensitive: snapshot.Sensitive, OccurredFrom: snapshot.OccurredFrom,
		OccurredUntil: snapshot.OccurredUntil, State: state,
		LifecycleReason: reason, ContentHash: memoryContentHash(snapshot.Content),
		ActorKind: "agent", ActorID: p.ID, Operation: operation,
		IdempotencyKey: fmt.Sprintf("curation:%s:%s:%s", run.ID, actionID, operation),
		RequestHash:    run.PlanHash, Client: run.Client,
		CurationRunID: run.ID, CurationActionID: actionID,
	}
}

func memoryCurationEvidenceInput(in MemoryCurationEvidence) MemoryEvidenceInput {
	out := MemoryEvidenceInput{
		Type: in.Type, Role: in.Role, ResolutionState: in.ResolutionState,
		ExternalLocator: in.ExternalLocator, ResolvedKind: in.ResolvedKind,
		SourceTranscriptID:  in.SourceTranscriptID,
		SourceSequenceFrom:  in.SourceSequenceFrom,
		SourceSequenceUntil: in.SourceSequenceUntil,
		SourceMessageID:     in.SourceMessageID,
		SourceImportLocator: in.SourceImportLocator,
		ArtifactExcerpt:     append([]byte(nil), in.ArtifactExcerpt...),
		ArtifactSensitive:   in.ArtifactSensitive,
		TerminalReasonCode:  in.TerminalReasonCode, SourceDigest: in.SourceDigest,
	}
	if in.SourceMemory != nil {
		out.SourceMemoryID = in.SourceMemory.MemoryID
		out.SourceMemoryVersion = in.SourceMemory.Version
	}
	return out
}

func insertMemoryCurationEvidenceListTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	memoryID string,
	version int64,
	inputs []MemoryCurationEvidence,
) ([]string, error) {
	ids := make([]string, 0, len(inputs))
	for index, input := range inputs {
		evidence := memoryCurationEvidenceInput(input)
		if err := validateMemoryEvidenceSourceTx(ctx, tx, p, evidence); err != nil {
			return nil, fmt.Errorf("curation evidence %d: %w", index, err)
		}
		out, err := insertMemoryEvidenceTx(ctx, tx, p, memoryID, version, evidence, "", "")
		if err != nil {
			return nil, fmt.Errorf("insert curation evidence %d: %w", index, err)
		}
		ids = append(ids, out.ID)
	}
	return ids, nil
}

func insertMemoryCurationRelationTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	runID, actionID, relationType string,
	from, to MemoryCurationVersionReference,
	setID string,
	setRevision int64,
) (string, error) {
	var duplicate bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM memory_relations
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND from_memory_id=$4 AND from_version=$5
		  AND to_memory_id=$6 AND to_version=$7 AND relation_type=$8
		  AND reverted_at IS NULL
	)`, p.AccountID, p.RealmID, p.ID, from.MemoryID, from.Version,
		to.MemoryID, to.Version, relationType).Scan(&duplicate); err != nil {
		return "", err
	}
	if duplicate {
		return "", errMemoryCurationApplyStale
	}
	relationID, err := id.New("mrel")
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_relations
		  (id,account_id,realm_id,owner_kind,owner_id,from_memory_id,from_version,
		   to_memory_id,to_version,relation_type,supersession_set_id,
		   supersession_set_revision,curation_run_id,curation_action_id)
		VALUES ($1,$2,$3,'agent',$4,$5,$6,$7,$8,$9,NULLIF($10,''),
		        NULLIF($11,0),$12,$13)`, relationID, p.AccountID, p.RealmID, p.ID,
		from.MemoryID, from.Version, to.MemoryID, to.Version, relationType,
		setID, setRevision, runID, actionID); err != nil {
		return "", fmt.Errorf("insert curation relation: %w", err)
	}
	return relationID, nil
}

func advanceMemoryCurationCursorsTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	runID string,
) ([]MemoryCurationCursorInterval, error) {
	rows, err := tx.Query(ctx, `
		SELECT cursor_source_kind,cursor_stream_id,cursor_expected_prior,cursor_upper
		FROM memory_curation_run_inputs
		WHERE run_id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4 AND input_kind='cursor'
		ORDER BY cursor_source_kind,cursor_stream_id`, runID, p.AccountID,
		p.RealmID, p.ID)
	if err != nil {
		return nil, err
	}
	intervals := make([]MemoryCurationCursorInterval, 0)
	for rows.Next() {
		var item MemoryCurationCursorInterval
		if err := rows.Scan(&item.SourceKind, &item.SourceStreamID,
			&item.ExpectedPrior, &item.Upper); err != nil {
			rows.Close()
			return nil, err
		}
		intervals = append(intervals, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, item := range intervals {
		tag, err := tx.Exec(ctx, `
			UPDATE memory_curation_cursors
			SET position=$6,updated_at=clock_timestamp()
			WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
			  AND source_kind=$4 AND source_stream_id=$5 AND position=$7`,
			p.AccountID, p.RealmID, p.ID, item.SourceKind, item.SourceStreamID,
			item.Upper, item.ExpectedPrior)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() != 1 {
			return nil, errMemoryCurationApplyStale
		}
	}
	return intervals, nil
}

// hasMemoryCurationSourceBacklogTx runs after the frozen cursor intervals have
// advanced while the owner lane is still locked. It distinguishes a complete
// snapshot from one truncated by an input cap, including transcript streams
// that were omitted entirely once the aggregate transcript cap was exhausted.
func hasMemoryCurationSourceBacklogTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	run MemoryCurationRun,
	request MemoryCurationRequest,
) (bool, error) {
	if request.ReadOnlyReplay {
		return false, nil
	}
	if curationHasSource(request.Scope, MemoryCurationSourceMemory) {
		streamID, err := memoryCurationFilteredStreamID(MemoryCurationSourceMemory, request.Scope)
		if err != nil {
			return false, err
		}
		position, err := loadMemoryCurationCursorPosition(ctx, tx, p, MemoryCurationSourceMemory, streamID)
		if err != nil {
			return false, err
		}
		if position < run.MemoryChangeUpper {
			return true, nil
		}
	}
	if curationHasSource(request.Scope, MemoryCurationSourceEvidence) {
		streamID, err := memoryCurationFilteredStreamID(MemoryCurationSourceEvidence, request.Scope)
		if err != nil {
			return false, err
		}
		position, err := loadMemoryCurationCursorPosition(ctx, tx, p, MemoryCurationSourceEvidence, streamID)
		if err != nil {
			return false, err
		}
		if position < run.EvidenceChangeUpper {
			return true, nil
		}
	}
	if curationHasSource(request.Scope, MemoryCurationSourceTranscript) {
		var backlog bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1 FROM transcript_conversations c
			  LEFT JOIN memory_curation_cursors cursor
			    ON cursor.account_id=c.account_id AND cursor.realm_id=c.realm_id
			   AND cursor.owner_kind='agent' AND cursor.owner_id=c.owner_agent_id
			   AND cursor.source_kind='transcript' AND cursor.source_stream_id=c.id
			  WHERE c.account_id=$1 AND c.realm_id=$2 AND c.owner_agent_id=$3
			    AND c.next_sequence-1 > COALESCE(cursor.position,0)
			)`, p.AccountID, p.RealmID, p.ID).Scan(&backlog); err != nil {
			return false, fmt.Errorf("check curation transcript backlog: %w", err)
		}
		if backlog {
			return true, nil
		}
	}
	return false, nil
}

func fulfillMemoryCurationGenerationTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	lane *MemoryCurationLane,
	run MemoryCurationRun,
	request MemoryCurationRequest,
	backlog bool,
) (MemoryCurationRequest, *MemoryCurationRequest, error) {
	if request.RequestGeneration < run.RequestGeneration {
		return MemoryCurationRequest{}, nil, ErrMemoryCurationConflict
	}
	followUpGeneration := request.RequestGeneration
	if backlog && followUpGeneration == run.RequestGeneration {
		if lane == nil || lane.RequestGeneration >= maxMemoryCurationGeneration {
			return MemoryCurationRequest{}, nil, ErrMemoryCurationConflict
		}
		followUpGeneration = lane.RequestGeneration + 1
		if tag, err := tx.Exec(ctx, `
			UPDATE memory_curation_lanes
			SET request_generation=$5,updated_at=clock_timestamp()
			WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
			  AND request_generation=$4`, p.AccountID, p.RealmID, p.ID,
			lane.RequestGeneration, followUpGeneration); err != nil {
			return MemoryCurationRequest{}, nil, fmt.Errorf("advance curation backlog generation: %w", err)
		} else if tag.RowsAffected() != 1 {
			return MemoryCurationRequest{}, nil, ErrMemoryCurationConflict
		}
		lane.RequestGeneration = followUpGeneration
	}
	request, err := scanMemoryCurationRequest(tx.QueryRow(ctx, `
		UPDATE memory_curation_requests
		SET state='fulfilled',fulfilled_generation=$2,
		    fulfilled_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE id=$1 AND state='claimed' AND claimed_run_id=$3
		RETURNING id,account_id,realm_id,owner_kind,owner_id,scope,coalescing_key,
		          trigger_reason,request_generation,priority,due_at,state,attempt_count,
		          max_attempts,COALESCE(claimed_run_id,''),fulfilled_generation,
		          COALESCE(replay_run_id,''),read_only_replay,actor_kind,actor_id,
		          idempotency_key,request_hash,claimed_at,fulfilled_at,cancelled_at,
		          dead_lettered_at,created_at,updated_at`, request.ID,
		run.RequestGeneration, run.ID))
	if err != nil {
		return MemoryCurationRequest{}, nil, fmt.Errorf("fulfill curation request: %w", err)
	}
	if followUpGeneration == run.RequestGeneration {
		return request, nil, nil
	}

	followUpID, err := id.New("mcrq")
	if err != nil {
		return MemoryCurationRequest{}, nil, err
	}
	followUpKey := "curation-follow-up:" + run.ID
	followUpHash, err := memoryRequestHash(struct {
		Operation  string `json:"operation"`
		RunID      string `json:"run_id"`
		Generation int64  `json:"generation"`
	}{Operation: "follow_up", RunID: run.ID, Generation: followUpGeneration})
	if err != nil {
		return MemoryCurationRequest{}, nil, err
	}
	scope, err := json.Marshal(request.Scope)
	if err != nil {
		return MemoryCurationRequest{}, nil, err
	}
	triggerReason := "generation_follow_up"
	if backlog {
		triggerReason = "source_backlog"
	}
	followUp, err := scanMemoryCurationRequest(tx.QueryRow(ctx, `
		INSERT INTO memory_curation_requests
		  (id,account_id,realm_id,owner_kind,owner_id,scope,coalescing_key,
		   trigger_reason,request_generation,priority,due_at,state,attempt_count,
		   max_attempts,fulfilled_generation,read_only_replay,actor_kind,actor_id,
		   idempotency_key,request_hash)
		VALUES ($1,$2,$3,'agent',$4,$5::jsonb,$6,$7,$8,$9,
		        clock_timestamp(),'queued',0,$10,0,false,'agent',$4,$11,$12)
		RETURNING id,account_id,realm_id,owner_kind,owner_id,scope,coalescing_key,
		          trigger_reason,request_generation,priority,due_at,state,attempt_count,
		          max_attempts,COALESCE(claimed_run_id,''),fulfilled_generation,
		          COALESCE(replay_run_id,''),read_only_replay,actor_kind,actor_id,
		          idempotency_key,request_hash,claimed_at,fulfilled_at,cancelled_at,
		          dead_lettered_at,created_at,updated_at`, followUpID, p.AccountID,
		p.RealmID, p.ID, string(scope), request.CoalescingKey, triggerReason,
		followUpGeneration, request.Priority, request.MaxAttempts, followUpKey,
		followUpHash))
	if err != nil {
		return MemoryCurationRequest{}, nil, fmt.Errorf("queue curation generation follow-up: %w", err)
	}
	return request, &followUp, nil
}

func markMemoryCurationApplyConflictTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	lane *MemoryCurationLane,
	run MemoryCurationRun,
	request MemoryCurationRequest,
	in ApplyMemoryCurationInput,
	requestHash, idempotencyKey, reason string,
) error {
	nextAttempt := request.AttemptCount + 1
	requestState := MemoryCurationRequestRetryWait
	var dueAt any
	if nextAttempt >= request.MaxAttempts {
		requestState = MemoryCurationRequestDeadLetter
		dueAt = request.DueAt
	} else {
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		dueAt = now.Add(curationBackoff(nextAttempt))
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE memory_curation_runs
		SET state='conflict',lease_expires_at=NULL,conflict_reason_code=$2,
		    terminal_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE id=$1 AND state='planned'`, run.ID, reason); err != nil {
		return fmt.Errorf("mark curation apply conflict: %w", err)
	} else if tag.RowsAffected() != 1 {
		return ErrMemoryCurationConflict
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_curation_requests
		SET state=$2,attempt_count=$3,claimed_run_id=NULL,claimed_at=NULL,due_at=$4,
		    dead_lettered_at=CASE WHEN $2='dead_letter' THEN clock_timestamp() ELSE NULL END,
		    updated_at=clock_timestamp()
		WHERE id=$1 AND state='claimed' AND claimed_run_id=$5`, request.ID,
		requestState, nextAttempt, dueAt, run.ID); err != nil {
		return fmt.Errorf("requeue conflicting curation request: %w", err)
	}
	if lane.FencingGeneration >= maxMemoryCurationGeneration {
		return ErrMemoryCurationConflict
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE memory_curation_lanes
		SET active_run_id=NULL,fencing_generation=$5,updated_at=clock_timestamp()
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND fencing_generation=$4 AND active_run_id=$6`, p.AccountID, p.RealmID,
		p.ID, lane.FencingGeneration, lane.FencingGeneration+1, run.ID); err != nil {
		return err
	} else if tag.RowsAffected() != 1 {
		return ErrMemoryCurationConflict
	}
	if _, err := insertMemoryCurationMutation(ctx, tx, p, MemoryCurationMutationReceipt{
		Operation: "apply", ActorID: p.ID, IdempotencyKey: idempotencyKey,
		RequestHash: requestHash, RequestID: run.RequestID, RunID: run.ID,
		RequestGeneration: run.RequestGeneration,
		FencingGeneration: run.FencingGeneration, PlanRevision: in.PlanRevision,
		PlanHash: in.PlanHash, ResultState: MemoryCurationRunConflict,
	}); err != nil {
		return err
	}
	return logMemoryCurationEventTx(ctx, tx, p, VerbMemoryCurationConflicted,
		run.RequestID, run.ID, run.RequestGeneration, run.FencingGeneration,
		MemoryCurationRunConflict)
}

func loadMemoryCurationApplyResult(
	ctx context.Context,
	q memoryQuerier,
	p Principal,
	mutation MemoryCurationMutationReceipt,
	replayed bool,
) (ApplyMemoryCurationResult, error) {
	run, err := loadMemoryCurationRun(ctx, q, p, mutation.RunID, false)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	if run.State != MemoryCurationRunApplied && run.State != MemoryCurationRunRolledBack {
		return ApplyMemoryCurationResult{}, ErrMemoryCurationConflict
	}
	if run.ApplyReceiptID == "" || run.ApplyReceiptID != mutation.ReceiptID ||
		run.PlanRevision != mutation.PlanRevision || run.PlanHash != mutation.PlanHash {
		return ApplyMemoryCurationResult{}, ErrMemoryCurationConflict
	}
	request, err := loadMemoryCurationRequest(ctx, q, p, run.RequestID, false)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	rows, err := q.Query(ctx, `
		SELECT applied_result::text FROM memory_curation_actions
		WHERE run_id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4 AND state IN ('applied','reverted')
		ORDER BY ordinal`, run.ID, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	actions := make([]MemoryCurationActionApplyResult, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return ApplyMemoryCurationResult{}, err
		}
		var action MemoryCurationActionApplyResult
		if err := decodeMemoryCurationStoredJSON(raw, &action); err != nil {
			rows.Close()
			return ApplyMemoryCurationResult{}, ErrMemoryCurationConflict
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ApplyMemoryCurationResult{}, err
	}
	rows.Close()
	intervals, err := loadMemoryCurationCursorIntervals(ctx, q, p, run.ID)
	if err != nil {
		return ApplyMemoryCurationResult{}, err
	}
	var followUp *MemoryCurationRequest
	var followUpID string
	err = q.QueryRow(ctx, `
		SELECT id FROM memory_curation_requests
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND idempotency_key=$4`, p.AccountID, p.RealmID, p.ID,
		"curation-follow-up:"+run.ID).Scan(&followUpID)
	if err == nil {
		item, loadErr := loadMemoryCurationRequest(ctx, q, p, followUpID, false)
		if loadErr != nil {
			return ApplyMemoryCurationResult{}, loadErr
		}
		followUp = &item
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ApplyMemoryCurationResult{}, err
	}
	receipt := MemoryCurationApplyReceipt{
		ID: mutation.ReceiptID, Operation: "apply", ActorID: p.ID,
		IdempotencyKey: mutation.IdempotencyKey, RequestHash: mutation.RequestHash,
		RequestID: run.RequestID, RunID: run.ID,
		RequestGeneration: run.RequestGeneration,
		FencingGeneration: run.FencingGeneration, PlanRevision: run.PlanRevision,
		PlanHash: run.PlanHash, ActionResults: actions, CursorIntervals: intervals,
		CreatedAt: mutation.CreatedAt, Replayed: replayed,
	}
	if followUp != nil {
		receipt.FollowUpRequestID = followUp.ID
		receipt.FollowUpGeneration = followUp.RequestGeneration
	}
	return ApplyMemoryCurationResult{
		Run: run, Request: request, FollowUpRequest: followUp, Receipt: receipt,
	}, nil
}

func loadMemoryCurationCursorIntervals(
	ctx context.Context,
	q memoryQuerier,
	p Principal,
	runID string,
) ([]MemoryCurationCursorInterval, error) {
	rows, err := q.Query(ctx, `
		SELECT cursor_source_kind,cursor_stream_id,cursor_expected_prior,cursor_upper
		FROM memory_curation_run_inputs
		WHERE run_id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4 AND input_kind='cursor'
		ORDER BY cursor_source_kind,cursor_stream_id`, runID, p.AccountID,
		p.RealmID, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MemoryCurationCursorInterval, 0)
	for rows.Next() {
		var item MemoryCurationCursorInterval
		if err := rows.Scan(&item.SourceKind, &item.SourceStreamID,
			&item.ExpectedPrior, &item.Upper); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
