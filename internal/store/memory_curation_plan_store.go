package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// Memory curation input-reference constants identify the frozen source that
// authorizes each accepted plan action.
const (
	MemoryCurationInputRefMemory       = "memory"
	MemoryCurationInputRefCreateOutput = "create_output"
	MemoryCurationInputRefEvidence     = "evidence"
	MemoryCurationInputRefTranscript   = "transcript"
	MemoryCurationInputRefMessage      = "message"
)

// PlanMemoryCurationInput carries a strict raw plan draft so the store, rather
// than a transport-specific decoder, remains the authority for unknown-field,
// duplicate-member, and canonicalization rules.
type PlanMemoryCurationInput struct {
	FencingGeneration int64           `json:"fencing_generation"`
	Draft             json.RawMessage `json:"draft"`
	IdempotencyKey    string          `json:"idempotency_key"`
}

// MemoryCurationActionInputRef is a value-free authorization index persisted
// beside one normalized action. ViaEvidenceID records that a nested source was
// authorized by an exact materialized evidence row rather than directly.
type MemoryCurationActionInputRef struct {
	Kind          string `json:"kind"`
	MemoryID      string `json:"memory_id,omitempty"`
	Version       int64  `json:"version,omitempty"`
	EvidenceID    string `json:"evidence_id,omitempty"`
	TranscriptID  string `json:"transcript_id,omitempty"`
	SequenceFrom  int64  `json:"sequence_from,omitempty"`
	SequenceUntil int64  `json:"sequence_until,omitempty"`
	MessageID     string `json:"message_id,omitempty"`
	ViaEvidenceID string `json:"via_evidence_id,omitempty"`
}

// MemoryCurationExpectedHead is the redundant lock/recheck index used by
// apply. It contains no payload and includes create outputs consumed by later
// actions as well as materialized existing heads.
type MemoryCurationExpectedHead struct {
	MemoryID        string `json:"memory_id"`
	ExpectedVersion int64  `json:"expected_version"`
}

// MemoryCurationPlanReceipt is the immutable value-free acceptance receipt.
// PlanHash identifies the separately returned normalized plan; it does not
// contain plan JSON, memory content, fact values, or provenance locators.
type MemoryCurationPlanReceipt struct {
	ID                string    `json:"id"`
	Operation         string    `json:"operation"`
	ActorID           string    `json:"actor_id"`
	IdempotencyKey    string    `json:"idempotency_key"`
	RequestHash       string    `json:"request_hash"`
	RequestID         string    `json:"request_id"`
	RunID             string    `json:"run_id"`
	RequestGeneration int64     `json:"request_generation"`
	FencingGeneration int64     `json:"fencing_generation"`
	PlanSchema        string    `json:"plan_schema"`
	PlanRevision      int64     `json:"plan_revision"`
	PlanHash          string    `json:"plan_hash"`
	ResultState       string    `json:"result_state"`
	CreatedAt         time.Time `json:"created_at"`
	Replayed          bool      `json:"replayed,omitempty"`
}

// PlanMemoryCurationResult returns the normalized accepted plan and receipt.
type PlanMemoryCurationResult struct {
	Run                   MemoryCurationRun                    `json:"run"`
	Plan                  MemoryCurationPlan                   `json:"plan"`
	PreallocatedMemoryIDs []MemoryCurationPreallocatedMemoryID `json:"preallocated_memory_ids,omitempty"`
	Preview               MemoryCurationImpactPreview          `json:"preview"`
	Receipt               MemoryCurationPlanReceipt            `json:"receipt"`
}

// GetMemoryCurationPlanResult is the verified accepted-plan view used to
// review a staged plan before apply. It intentionally omits the original
// mutation receipt and idempotency metadata: those identify the planner's
// write but are not authority for a later client to apply the plan.
type GetMemoryCurationPlanResult struct {
	Run                   MemoryCurationRun                    `json:"run"`
	Plan                  MemoryCurationPlan                   `json:"plan"`
	PreallocatedMemoryIDs []MemoryCurationPreallocatedMemoryID `json:"preallocated_memory_ids,omitempty"`
	Preview               MemoryCurationImpactPreview          `json:"preview"`
}

// memoryCurationStoredAction is the package-level handoff to apply. Apply must
// use these persisted rows and reverify PlanHash; it must never accept another
// caller-authored plan.
type memoryCurationStoredAction struct {
	ID            string
	Action        MemoryCurationPlanAction
	InputRefs     []MemoryCurationActionInputRef
	ExpectedHeads []MemoryCurationExpectedHead
	ActionHash    string
}

type memoryCurationStoredPlan struct {
	Acceptance MemoryCurationPlanAcceptance
	Actions    []memoryCurationStoredAction
}

// authorizeMemoryCurationPlanProfile keeps the restricted curator profiles on
// the non-sensitive plane even when a full credential accepted the plan first.
// Request scope constrains frozen inputs, but it does not constrain a planner
// from conservatively marking newly synthesized output sensitive, so every
// content-bearing handoff must enforce both boundaries.
func authorizeMemoryCurationPlanProfile(p Principal, plan MemoryCurationPlan) error {
	if !isRestrictedMemoryCurator(p) {
		return nil
	}
	for _, action := range plan.Actions {
		if action.Create != nil && action.Create.Snapshot.Sensitive {
			return ErrMemoryCurationForbidden
		}
		if action.Replace != nil && action.Replace.Snapshot.Sensitive {
			return ErrMemoryCurationForbidden
		}
		if action.ProposeFact != nil && action.ProposeFact.Sensitive {
			return ErrMemoryCurationForbidden
		}
	}
	return nil
}

type memoryCurationPlanMemoryInput struct {
	MemoryID       string
	Version        int64
	Sensitive      bool
	State          string
	CurrentVersion sql.NullInt64
	CurrentState   sql.NullString
}

type memoryCurationPlanEvidenceInput struct {
	Evidence  MemoryEvidence
	Sensitive bool
}

type memoryCurationPlanTranscriptInput struct {
	TranscriptID string
	From         int64
	Until        int64
}

type memoryCurationPlanAuthorization struct {
	memories    map[string]memoryCurationPlanMemoryInput
	evidence    map[string]memoryCurationPlanEvidenceInput
	transcripts []memoryCurationPlanTranscriptInput
	outputs     map[string]memoryCurationPlanOutput
}

type memoryCurationPlanOutput struct {
	Ordinal   int64
	Sensitive bool
}

type memoryCurationAuthorizedAction struct {
	Action        MemoryCurationPlanAction
	InputRefs     []MemoryCurationActionInputRef
	ExpectedHeads []MemoryCurationExpectedHead
	ActionHash    string
}

// GetCurationPlan reconstructs and verifies the exact accepted plan for one
// live, fenced planned run. The repeatable-read, read-only transaction keeps
// the run, lane, and action rows on one snapshot. Lease expiry is reported but
// never reconciled here; RenewCuration is the sole prescribed mutation path.
func (s *Store) GetCurationPlan(
	ctx context.Context,
	p Principal,
	runID string,
	fencingGeneration int64,
) (GetMemoryCurationPlanResult, error) {
	if p.Kind != PrincipalAgent {
		return GetMemoryCurationPlanResult{}, ErrMemoryCurationForbidden
	}
	runID = strings.TrimSpace(runID)
	if !validCurationID(runID, "mrun") || fencingGeneration < 1 {
		return GetMemoryCurationPlanResult{}, ErrMemoryCurationInputInvalid
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return GetMemoryCurationPlanResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return GetMemoryCurationPlanResult{}, err
	}
	if err := authorizeMemoryCurationRunID(ctx, tx, p, runID); err != nil {
		return GetMemoryCurationPlanResult{}, err
	}
	run, err := loadMemoryCurationRun(ctx, tx, p, runID, false)
	if err != nil {
		return GetMemoryCurationPlanResult{}, err
	}
	if run.FencingGeneration != fencingGeneration {
		return GetMemoryCurationPlanResult{}, ErrMemoryCurationFenceMismatch
	}
	lane, err := loadMemoryCurationLaneTx(ctx, tx, p, false)
	if err != nil {
		return GetMemoryCurationPlanResult{}, err
	}
	if lane.ActiveRunID != runID || lane.FencingGeneration != fencingGeneration {
		return GetMemoryCurationPlanResult{}, ErrMemoryCurationFenceMismatch
	}
	if run.State != MemoryCurationRunPlanned || run.PlanRevision < 1 || run.PlanHash == "" {
		return GetMemoryCurationPlanResult{}, ErrMemoryCurationConflict
	}
	stored, err := loadMemoryCurationStoredPlan(ctx, tx, p, run)
	if err != nil {
		return GetMemoryCurationPlanResult{}, err
	}
	if err := authorizeMemoryCurationPlanProfile(p, stored.Acceptance.Plan); err != nil {
		return GetMemoryCurationPlanResult{}, err
	}
	var expired bool
	if err := tx.QueryRow(ctx, `
		SELECT lease_expires_at IS NULL OR lease_expires_at <= clock_timestamp()
		FROM memory_curation_runs WHERE id=$1`, run.ID).Scan(&expired); err != nil {
		return GetMemoryCurationPlanResult{}, fmt.Errorf("check curation lease: %w", err)
	}
	if expired {
		return GetMemoryCurationPlanResult{}, ErrMemoryCurationLeaseExpired
	}
	if err := tx.Commit(ctx); err != nil {
		return GetMemoryCurationPlanResult{}, err
	}
	return GetMemoryCurationPlanResult{
		Run:  run,
		Plan: stored.Acceptance.Plan,
		PreallocatedMemoryIDs: append([]MemoryCurationPreallocatedMemoryID(nil),
			stored.Acceptance.PreallocatedMemoryIDs...),
		Preview: stored.Acceptance.Preview,
	}, nil
}

// PlanCuration validates and immutably accepts one client-authored plan. It
// performs no inference and applies no semantic action.
func (s *Store) PlanCuration(
	ctx context.Context,
	p Principal,
	runID string,
	in PlanMemoryCurationInput,
) (PlanMemoryCurationResult, error) {
	if p.Kind != PrincipalAgent {
		return PlanMemoryCurationResult{}, ErrMemoryCurationForbidden
	}
	runID = strings.TrimSpace(runID)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if !validCurationID(runID, "mrun") || in.FencingGeneration < 1 ||
		validateMemoryIdempotencyKey(in.IdempotencyKey) != nil || len(in.Draft) == 0 {
		return PlanMemoryCurationResult{}, ErrMemoryCurationInputInvalid
	}
	draft, err := DecodeMemoryCurationPlanDraft(in.Draft)
	if err != nil {
		return PlanMemoryCurationResult{}, ErrMemoryCurationInputInvalid
	}
	requestHash, err := hashMemoryCurationPlanRequest(runID, in.FencingGeneration, draft)
	if err != nil {
		return PlanMemoryCurationResult{}, ErrMemoryCurationInputInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PlanMemoryCurationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prepareMemoryCurationMutationTx(ctx, tx, p, "plan", in.IdempotencyKey); err != nil {
		return PlanMemoryCurationResult{}, err
	}
	if err := authorizeMemoryCurationRunID(ctx, tx, p, runID); err != nil {
		return PlanMemoryCurationResult{}, err
	}
	if receipt, replayed, err := loadMemoryCurationPlanMutation(
		ctx, tx, p, in.IdempotencyKey, requestHash,
	); err != nil || replayed {
		if err != nil {
			return PlanMemoryCurationResult{}, err
		}
		if receipt.RunID != runID || receipt.FencingGeneration != in.FencingGeneration {
			return PlanMemoryCurationResult{}, ErrMemoryCurationIdempotencyConflict
		}
		run, err := loadMemoryCurationRun(ctx, tx, p, runID, false)
		if err != nil {
			return PlanMemoryCurationResult{}, err
		}
		stored, err := loadMemoryCurationStoredPlan(ctx, tx, p, run)
		if err != nil {
			return PlanMemoryCurationResult{}, err
		}
		if receipt.PlanHash != stored.Acceptance.PlanHash || receipt.PlanRevision != stored.Acceptance.Plan.PlanRevision {
			return PlanMemoryCurationResult{}, ErrMemoryCurationConflict
		}
		if err := authorizeMemoryCurationPlanProfile(p, stored.Acceptance.Plan); err != nil {
			return PlanMemoryCurationResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return PlanMemoryCurationResult{}, err
		}
		return planMemoryCurationResult(run, stored.Acceptance, receipt), nil
	}
	if err := authorizeMemoryCurationPlanProfile(p, MemoryCurationPlan{Actions: draft.Actions}); err != nil {
		return PlanMemoryCurationResult{}, err
	}

	lane, err := loadMemoryCurationLaneTx(ctx, tx, p, true)
	if err != nil {
		return PlanMemoryCurationResult{}, err
	}
	expired, err := interruptExpiredCurationRunTx(ctx, tx, p, &lane)
	if err != nil {
		return PlanMemoryCurationResult{}, err
	}
	if expired {
		if err := tx.Commit(ctx); err != nil {
			return PlanMemoryCurationResult{}, err
		}
		return PlanMemoryCurationResult{}, ErrMemoryCurationLeaseExpired
	}
	if lane.ActiveRunID != runID || lane.FencingGeneration != in.FencingGeneration {
		return PlanMemoryCurationResult{}, ErrMemoryCurationFenceMismatch
	}
	run, err := loadMemoryCurationRun(ctx, tx, p, runID, true)
	if err != nil {
		return PlanMemoryCurationResult{}, err
	}
	if run.FencingGeneration != in.FencingGeneration {
		return PlanMemoryCurationResult{}, ErrMemoryCurationFenceMismatch
	}
	if run.State != MemoryCurationRunOpen || run.PlanRevision != 0 || run.PlanHash != "" {
		return PlanMemoryCurationResult{}, ErrMemoryCurationConflict
	}
	request, err := loadMemoryCurationRequest(ctx, tx, p, run.RequestID, false)
	if err != nil {
		return PlanMemoryCurationResult{}, err
	}
	if err := authorizeMemoryCurationRequestScope(p, request); err != nil {
		return PlanMemoryCurationResult{}, err
	}
	if request.State != MemoryCurationRequestClaimed || request.ClaimedRunID != run.ID {
		return PlanMemoryCurationResult{}, ErrMemoryCurationConflict
	}
	// The lane is always the first owner-scoped data lock. Bind every fact
	// proposal while a shared lock freezes the subject/alias namespace, then
	// hash and persist only the canonical subject key. Apply and rollback must
	// never reinterpret the caller's alias after this transaction commits.
	if memoryCurationDraftHasFactProposals(draft) {
		if err := lockFactSubjectNamespace(ctx, tx, p, false); err != nil {
			return PlanMemoryCurationResult{}, err
		}
		if err := bindMemoryCurationDraftFactSubjects(ctx, tx, p, &draft); err != nil {
			if errors.Is(err, ErrFactInputInvalid) {
				return PlanMemoryCurationResult{}, ErrMemoryCurationInputInvalid
			}
			return PlanMemoryCurationResult{}, err
		}
	}

	acceptance, err := AcceptMemoryCurationPlan(draft, MemoryCurationPlanAcceptOptions{
		PlanRevision: 1,
		Allocator: MemoryCurationMemoryIDAllocatorFunc(func(string) (string, error) {
			return id.New("mem")
		}),
	})
	if err != nil {
		return PlanMemoryCurationResult{}, ErrMemoryCurationInputInvalid
	}
	if err := authorizeMemoryCurationPlanProfile(p, acceptance.Plan); err != nil {
		return PlanMemoryCurationResult{}, err
	}
	authorization, err := loadMemoryCurationPlanAuthorization(ctx, tx, p, run.ID, request.Scope, acceptance)
	if err != nil {
		return PlanMemoryCurationResult{}, err
	}
	rows, err := authorizeMemoryCurationPlan(acceptance.Plan.Actions, authorization)
	if err != nil {
		return PlanMemoryCurationResult{}, err
	}
	if err := persistMemoryCurationPlanActions(ctx, tx, p, run.ID, acceptance.Plan.PlanRevision, rows); err != nil {
		return PlanMemoryCurationResult{}, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_curation_runs
		SET state='planned',plan_schema=$2,plan_revision=$3,plan_hash=$4,
		    planned_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE id=$1 AND state='open' AND plan_revision=0
		  AND lease_expires_at > clock_timestamp()`, run.ID, acceptance.Plan.Schema,
		acceptance.Plan.PlanRevision, acceptance.PlanHash)
	if err != nil {
		return PlanMemoryCurationResult{}, fmt.Errorf("store accepted curation plan: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return PlanMemoryCurationResult{}, ErrMemoryCurationConflict
	}
	run, err = loadMemoryCurationRun(ctx, tx, p, run.ID, false)
	if err != nil {
		return PlanMemoryCurationResult{}, err
	}
	receipt, err := insertMemoryCurationPlanMutation(ctx, tx, p, MemoryCurationPlanReceipt{
		Operation: "plan", ActorID: p.ID, IdempotencyKey: in.IdempotencyKey,
		RequestHash: requestHash, RequestID: run.RequestID, RunID: run.ID,
		RequestGeneration: run.RequestGeneration,
		FencingGeneration: run.FencingGeneration, PlanSchema: run.PlanSchema,
		PlanRevision: run.PlanRevision, PlanHash: run.PlanHash,
		ResultState: run.State,
	})
	if err != nil {
		return PlanMemoryCurationResult{}, err
	}
	if err := logMemoryCurationEventTx(ctx, tx, p, VerbMemoryCurationPlanned,
		run.RequestID, run.ID, run.RequestGeneration, run.FencingGeneration,
		run.State); err != nil {
		return PlanMemoryCurationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PlanMemoryCurationResult{}, err
	}
	return planMemoryCurationResult(run, acceptance, receipt), nil
}

func planMemoryCurationResult(
	run MemoryCurationRun,
	acceptance MemoryCurationPlanAcceptance,
	receipt MemoryCurationPlanReceipt,
) PlanMemoryCurationResult {
	return PlanMemoryCurationResult{
		Run: run, Plan: acceptance.Plan,
		PreallocatedMemoryIDs: append([]MemoryCurationPreallocatedMemoryID(nil), acceptance.PreallocatedMemoryIDs...),
		Preview:               acceptance.Preview, Receipt: receipt,
	}
}

func loadMemoryCurationPlanMutation(
	ctx context.Context,
	q memoryQuerier,
	p Principal,
	key, requestHash string,
) (MemoryCurationPlanReceipt, bool, error) {
	var out MemoryCurationPlanReceipt
	err := q.QueryRow(ctx, `
		SELECT receipt_id,operation,actor_id,idempotency_key,request_hash,
		       request_id,COALESCE(run_id,''),request_generation,
		       fencing_generation,plan_revision,plan_hash,result_state,created_at
		FROM memory_curation_mutations
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND actor_kind='agent' AND actor_id=$3 AND operation='plan'
		  AND idempotency_key=$4`, p.AccountID, p.RealmID, p.ID, key).Scan(
		&out.ID, &out.Operation, &out.ActorID, &out.IdempotencyKey,
		&out.RequestHash, &out.RequestID, &out.RunID,
		&out.RequestGeneration, &out.FencingGeneration, &out.PlanRevision,
		&out.PlanHash, &out.ResultState, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryCurationPlanReceipt{}, false, nil
	}
	if err != nil {
		return MemoryCurationPlanReceipt{}, false, fmt.Errorf("load curation plan receipt: %w", err)
	}
	if out.RequestHash != requestHash {
		return MemoryCurationPlanReceipt{}, false, ErrMemoryCurationIdempotencyConflict
	}
	out.PlanSchema = MemoryCurationPlanSchemaV1
	out.Replayed = true
	return out, true, nil
}

func insertMemoryCurationPlanMutation(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	receipt MemoryCurationPlanReceipt,
) (MemoryCurationPlanReceipt, error) {
	mutationID, err := id.New("mcmu")
	if err != nil {
		return MemoryCurationPlanReceipt{}, err
	}
	receipt.ID, err = id.New("mrec")
	if err != nil {
		return MemoryCurationPlanReceipt{}, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO memory_curation_mutations
		  (id,account_id,realm_id,owner_kind,owner_id,actor_kind,actor_id,
		   operation,idempotency_key,request_hash,request_id,run_id,
		   request_generation,fencing_generation,plan_revision,plan_hash,
		   result_state,receipt_id)
		VALUES ($1,$2,$3,'agent',$4,'agent',$4,'plan',$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING created_at`, mutationID, p.AccountID, p.RealmID, p.ID,
		receipt.IdempotencyKey, receipt.RequestHash, receipt.RequestID,
		receipt.RunID, receipt.RequestGeneration, receipt.FencingGeneration,
		receipt.PlanRevision, receipt.PlanHash, receipt.ResultState,
		receipt.ID).Scan(&receipt.CreatedAt)
	if err != nil {
		return MemoryCurationPlanReceipt{}, fmt.Errorf("insert curation plan receipt: %w", err)
	}
	return receipt, nil
}

func memoryCurationPlanVersionKey(memoryID string, version int64) string {
	return memoryID + "\x00" + strconv.FormatInt(version, 10)
}

func hashMemoryCurationPlanRequest(
	runID string,
	fencingGeneration int64,
	draft MemoryCurationPlanDraft,
) (string, error) {
	requestCanonical, err := canonicalMemoryCurationJSON(struct {
		Operation         string                  `json:"operation"`
		RunID             string                  `json:"run_id"`
		FencingGeneration int64                   `json:"fencing_generation"`
		Draft             MemoryCurationPlanDraft `json:"draft"`
	}{Operation: "plan", RunID: runID, FencingGeneration: fencingGeneration, Draft: draft})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(requestCanonical)
	return hex.EncodeToString(sum[:]), nil
}

func memoryCurationDraftHasFactProposals(draft MemoryCurationPlanDraft) bool {
	for index := range draft.Actions {
		if draft.Actions[index].ProposeFact != nil {
			return true
		}
	}
	return false
}

func bindMemoryCurationDraftFactSubjects(
	ctx context.Context,
	q factQuerier,
	p Principal,
	draft *MemoryCurationPlanDraft,
) error {
	if draft == nil {
		return ErrFactInputInvalid
	}
	for index := range draft.Actions {
		proposal := draft.Actions[index].ProposeFact
		if proposal == nil {
			continue
		}
		canonical, err := resolveFactSubjectCanonicalKey(ctx, q, p, proposal.Subject)
		if err != nil {
			return err
		}
		proposal.Subject = canonical
	}
	return nil
}

func loadMemoryCurationPlanAuthorization(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	runID string,
	scope MemoryCurationScope,
	acceptance MemoryCurationPlanAcceptance,
) (*memoryCurationPlanAuthorization, error) {
	auth := &memoryCurationPlanAuthorization{
		memories: make(map[string]memoryCurationPlanMemoryInput),
		evidence: make(map[string]memoryCurationPlanEvidenceInput),
		outputs:  make(map[string]memoryCurationPlanOutput),
	}
	outputIDs := make([]string, 0, len(acceptance.PreallocatedMemoryIDs))
	for _, action := range acceptance.Plan.Actions {
		if action.Create == nil {
			continue
		}
		auth.outputs[action.Create.MemoryID] = memoryCurationPlanOutput{
			Ordinal: action.Ordinal, Sensitive: action.Create.Snapshot.Sensitive,
		}
		outputIDs = append(outputIDs, action.Create.MemoryID)
	}
	if len(outputIDs) > 0 {
		var collision bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM memories WHERE id=ANY($1::text[])
		)`, outputIDs).Scan(&collision); err != nil {
			return nil, fmt.Errorf("check preallocated curation memory ids: %w", err)
		}
		if collision {
			return nil, ErrMemoryCurationConflict
		}
	}

	memoryRows, err := tx.Query(ctx, `
		SELECT i.memory_id,i.memory_version,v.sensitive,v.state,m.current_version,current.state
		FROM memory_curation_run_inputs i
		JOIN memory_versions v ON v.memory_id=i.memory_id AND v.version=i.memory_version
		JOIN memories m ON m.id=v.memory_id
		LEFT JOIN memory_versions current
		  ON current.memory_id=m.id AND current.version=m.current_version
		WHERE i.run_id=$1 AND i.account_id=$2 AND i.realm_id=$3
		  AND i.owner_kind='agent' AND i.owner_id=$4 AND i.input_kind='memory'
		ORDER BY i.ordinal
		FOR SHARE OF m`, runID, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return nil, fmt.Errorf("load curation plan memory inputs: %w", err)
	}
	for memoryRows.Next() {
		var input memoryCurationPlanMemoryInput
		if err := memoryRows.Scan(&input.MemoryID, &input.Version, &input.Sensitive,
			&input.State, &input.CurrentVersion, &input.CurrentState); err != nil {
			memoryRows.Close()
			return nil, err
		}
		if input.Sensitive && !scope.IncludeSensitive {
			memoryRows.Close()
			return nil, ErrMemoryCurationConflict
		}
		auth.memories[memoryCurationPlanVersionKey(input.MemoryID, input.Version)] = input
	}
	if err := memoryRows.Err(); err != nil {
		memoryRows.Close()
		return nil, err
	}
	memoryRows.Close()

	evidenceRows, err := tx.Query(ctx, `
		SELECT e.id,e.memory_id,e.target_version,e.evidence_change_seq,e.evidence_type,e.role,
		       e.resolution_state,COALESCE(e.external_locator,''),
		       COALESCE(e.pending_evidence_id,''),COALESCE(e.resolved_kind,''),
		       COALESCE(e.source_transcript_id,''),COALESCE(e.source_sequence_from,0),
		       COALESCE(e.source_sequence_until,0),COALESCE(e.source_memory_id,''),
		       COALESCE(e.source_memory_version,0),COALESCE(e.source_message_id,''),
		       COALESCE(e.source_import_locator,''),e.artifact_excerpt,
		       e.artifact_sensitive,COALESCE(e.terminal_reason_code,''),
		       COALESCE(e.source_digest,''),e.actor_id,COALESCE(e.idempotency_key,''),
		       COALESCE(e.request_hash,''),e.created_at,
		       (target.sensitive OR e.artifact_sensitive OR COALESCE(source.sensitive,false))
		FROM memory_curation_run_inputs i
		JOIN memory_evidence e ON e.id=i.evidence_id
		JOIN memory_versions target ON target.memory_id=e.memory_id AND target.version=e.target_version
		LEFT JOIN memory_versions source ON source.memory_id=e.source_memory_id
		                                  AND source.version=e.source_memory_version
		WHERE i.run_id=$1 AND i.account_id=$2 AND i.realm_id=$3
		  AND i.owner_kind='agent' AND i.owner_id=$4 AND i.input_kind='evidence'
		ORDER BY i.ordinal`, runID, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return nil, fmt.Errorf("load curation plan evidence inputs: %w", err)
	}
	for evidenceRows.Next() {
		var input memoryCurationPlanEvidenceInput
		var artifact []byte
		if err := evidenceRows.Scan(
			&input.Evidence.ID, &input.Evidence.MemoryID, &input.Evidence.TargetVersion,
			&input.Evidence.EvidenceChangeSeq, &input.Evidence.Type, &input.Evidence.Role,
			&input.Evidence.ResolutionState, &input.Evidence.ExternalLocator,
			&input.Evidence.PendingEvidenceID, &input.Evidence.ResolvedKind,
			&input.Evidence.SourceTranscriptID, &input.Evidence.SourceSequenceFrom,
			&input.Evidence.SourceSequenceUntil, &input.Evidence.SourceMemoryID,
			&input.Evidence.SourceMemoryVersion, &input.Evidence.SourceMessageID,
			&input.Evidence.SourceImportLocator, &artifact,
			&input.Evidence.ArtifactSensitive, &input.Evidence.TerminalReasonCode,
			&input.Evidence.SourceDigest, &input.Evidence.ActorID,
			&input.Evidence.IdempotencyKey, &input.Evidence.RequestHash,
			&input.Evidence.CreatedAt, &input.Sensitive,
		); err != nil {
			evidenceRows.Close()
			return nil, err
		}
		input.Evidence.ArtifactExcerpt = append([]byte(nil), artifact...)
		if input.Sensitive && !scope.IncludeSensitive {
			evidenceRows.Close()
			return nil, ErrMemoryCurationConflict
		}
		auth.evidence[input.Evidence.ID] = input
	}
	if err := evidenceRows.Err(); err != nil {
		evidenceRows.Close()
		return nil, err
	}
	evidenceRows.Close()

	transcriptRows, err := tx.Query(ctx, `
		SELECT transcript_id,sequence_from,sequence_until
		FROM memory_curation_run_inputs
		WHERE run_id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4 AND input_kind='transcript'
		ORDER BY ordinal`, runID, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return nil, fmt.Errorf("load curation plan transcript inputs: %w", err)
	}
	for transcriptRows.Next() {
		var input memoryCurationPlanTranscriptInput
		if err := transcriptRows.Scan(&input.TranscriptID, &input.From, &input.Until); err != nil {
			transcriptRows.Close()
			return nil, err
		}
		auth.transcripts = append(auth.transcripts, input)
	}
	if err := transcriptRows.Err(); err != nil {
		transcriptRows.Close()
		return nil, err
	}
	transcriptRows.Close()
	return auth, nil
}

func authorizeMemoryCurationPlan(
	actions []MemoryCurationPlanAction,
	auth *memoryCurationPlanAuthorization,
) ([]memoryCurationAuthorizedAction, error) {
	if auth == nil {
		return nil, ErrMemoryCurationConflict
	}
	rows := make([]memoryCurationAuthorizedAction, 0, len(actions))
	mutableTargets := make(map[string]struct{})
	for index := range actions {
		action := actions[index]
		builder := memoryCurationPlanAuthorizationBuilder{
			auth: auth, ordinal: action.Ordinal, previouslyMutated: mutableTargets,
		}
		mutatedMemoryID := ""
		switch action.Operation {
		case MemoryCurationOperationCreate:
			if action.Create == nil {
				return nil, ErrMemoryCurationConflict
			}
			tainted, err := builder.authorizeEvidenceList(action.Create.Snapshot.Evidence)
			if err != nil {
				return nil, err
			}
			for _, relation := range action.Create.Relations {
				sensitive, err := builder.authorizeVersion(relation.To, false)
				if err != nil {
					return nil, err
				}
				tainted = tainted || sensitive
			}
			if tainted && !action.Create.Snapshot.Sensitive {
				return nil, ErrMemoryCurationConflict
			}
		case MemoryCurationOperationReplace:
			if action.Replace == nil {
				return nil, ErrMemoryCurationConflict
			}
			if _, duplicate := mutableTargets[action.Replace.Target.MemoryID]; duplicate {
				return nil, ErrMemoryCurationConflict
			}
			tainted, err := builder.authorizeTarget(action.Replace.Target)
			if err != nil {
				return nil, err
			}
			evidenceTainted, err := builder.authorizeEvidenceList(action.Replace.Snapshot.Evidence)
			if err != nil {
				return nil, err
			}
			if (tainted || evidenceTainted) && !action.Replace.Snapshot.Sensitive {
				return nil, ErrMemoryCurationConflict
			}
			mutatedMemoryID = action.Replace.Target.MemoryID
		case MemoryCurationOperationSupersede:
			if action.Supersede == nil {
				return nil, ErrMemoryCurationConflict
			}
			if _, duplicate := mutableTargets[action.Supersede.Target.MemoryID]; duplicate {
				return nil, ErrMemoryCurationConflict
			}
			targetSensitive, err := builder.authorizeTarget(action.Supersede.Target)
			if err != nil {
				return nil, err
			}
			for _, replacement := range action.Supersede.Replacements {
				replacementSensitive, err := builder.authorizeVersion(replacement, true)
				if err != nil {
					return nil, err
				}
				if targetSensitive && !replacementSensitive {
					return nil, ErrMemoryCurationConflict
				}
			}
			mutatedMemoryID = action.Supersede.Target.MemoryID
		case MemoryCurationOperationRelate:
			if action.Relate == nil {
				return nil, ErrMemoryCurationConflict
			}
			if _, err := builder.authorizeVersion(action.Relate.From, false); err != nil {
				return nil, err
			}
			if _, err := builder.authorizeVersion(action.Relate.To, false); err != nil {
				return nil, err
			}
		case MemoryCurationOperationProposeFact:
			if action.ProposeFact == nil {
				return nil, ErrMemoryCurationConflict
			}
			tainted, err := builder.authorizeEvidenceList(action.ProposeFact.Evidence)
			if err != nil {
				return nil, err
			}
			if tainted && !action.ProposeFact.Sensitive {
				return nil, ErrMemoryCurationConflict
			}
		default:
			return nil, ErrMemoryCurationConflict
		}

		inputRefs, err := normalizeMemoryCurationActionInputRefs(builder.inputRefs)
		if err != nil {
			return nil, err
		}
		expectedHeads, err := normalizeMemoryCurationExpectedHeads(builder.expectedHeads)
		if err != nil {
			return nil, err
		}
		canonical, err := canonicalMemoryCurationJSON(action)
		if err != nil {
			return nil, ErrMemoryCurationConflict
		}
		sum := sha256.Sum256(canonical)
		rows = append(rows, memoryCurationAuthorizedAction{
			Action: action, InputRefs: inputRefs, ExpectedHeads: expectedHeads,
			ActionHash: hex.EncodeToString(sum[:]),
		})
		if mutatedMemoryID != "" {
			mutableTargets[mutatedMemoryID] = struct{}{}
		}
	}
	return rows, nil
}

type memoryCurationPlanAuthorizationBuilder struct {
	auth              *memoryCurationPlanAuthorization
	ordinal           int64
	previouslyMutated map[string]struct{}
	inputRefs         []MemoryCurationActionInputRef
	expectedHeads     []MemoryCurationExpectedHead
}

func (b *memoryCurationPlanAuthorizationBuilder) authorizeTarget(
	target MemoryCurationTargetReference,
) (bool, error) {
	return b.authorizeVersion(MemoryCurationVersionReference{
		MemoryID: target.MemoryID, Version: target.ExpectedVersion,
	}, true)
}

func (b *memoryCurationPlanAuthorizationBuilder) authorizeVersion(
	reference MemoryCurationVersionReference,
	requireCurrent bool,
) (bool, error) {
	if reference.MemoryID == "" || reference.LocalRef != "" || reference.Version < 1 {
		return false, ErrMemoryCurationConflict
	}
	if requireCurrent {
		if _, alreadyMutated := b.previouslyMutated[reference.MemoryID]; alreadyMutated {
			return false, ErrMemoryCurationConflict
		}
	}
	if output, ok := b.auth.outputs[reference.MemoryID]; ok {
		if reference.Version != 1 || output.Ordinal >= b.ordinal {
			return false, ErrMemoryCurationConflict
		}
		b.inputRefs = append(b.inputRefs, MemoryCurationActionInputRef{
			Kind:     MemoryCurationInputRefCreateOutput,
			MemoryID: reference.MemoryID, Version: reference.Version,
		})
		if requireCurrent {
			b.expectedHeads = append(b.expectedHeads, MemoryCurationExpectedHead{
				MemoryID: reference.MemoryID, ExpectedVersion: reference.Version,
			})
		}
		return output.Sensitive, nil
	}
	input, ok := b.auth.memories[memoryCurationPlanVersionKey(reference.MemoryID, reference.Version)]
	if !ok || !input.CurrentVersion.Valid || !input.CurrentState.Valid ||
		input.CurrentState.String == MemoryStateForgotten || input.CurrentState.String == MemoryStateReverted ||
		input.State == MemoryStateForgotten || input.State == MemoryStateReverted {
		return false, ErrMemoryCurationConflict
	}
	if requireCurrent && (input.State != MemoryStateActive || input.CurrentVersion.Int64 != reference.Version) {
		return false, ErrMemoryCurationConflict
	}
	b.inputRefs = append(b.inputRefs, MemoryCurationActionInputRef{
		Kind:     MemoryCurationInputRefMemory,
		MemoryID: reference.MemoryID, Version: reference.Version,
	})
	if requireCurrent {
		b.expectedHeads = append(b.expectedHeads, MemoryCurationExpectedHead{
			MemoryID: reference.MemoryID, ExpectedVersion: reference.Version,
		})
	}
	return input.Sensitive, nil
}

func (b *memoryCurationPlanAuthorizationBuilder) authorizeEvidenceList(
	evidence []MemoryCurationEvidence,
) (bool, error) {
	tainted := false
	for index := range evidence {
		sensitive, err := b.authorizeEvidence(evidence[index])
		if err != nil {
			return false, err
		}
		tainted = tainted || sensitive
	}
	return tainted, nil
}

func (b *memoryCurationPlanAuthorizationBuilder) authorizeEvidence(
	evidence MemoryCurationEvidence,
) (bool, error) {
	if evidence.InputEvidenceID != "" {
		input, ok := b.auth.evidence[evidence.InputEvidenceID]
		if !ok || !sameMemoryCurationInputEvidence(evidence, input.Evidence) {
			return false, ErrMemoryCurationConflict
		}
		b.inputRefs = append(b.inputRefs, MemoryCurationActionInputRef{
			Kind: MemoryCurationInputRefEvidence, EvidenceID: evidence.InputEvidenceID,
		})
		switch evidence.ResolvedKind {
		case "":
		case "transcript":
			b.inputRefs = append(b.inputRefs, MemoryCurationActionInputRef{
				Kind:         MemoryCurationInputRefTranscript,
				TranscriptID: evidence.SourceTranscriptID,
				SequenceFrom: evidence.SourceSequenceFrom, SequenceUntil: evidence.SourceSequenceUntil,
				ViaEvidenceID: evidence.InputEvidenceID,
			})
		case "memory":
			if evidence.SourceMemory == nil {
				return false, ErrMemoryCurationConflict
			}
			b.inputRefs = append(b.inputRefs, MemoryCurationActionInputRef{
				Kind:     MemoryCurationInputRefMemory,
				MemoryID: evidence.SourceMemory.MemoryID, Version: evidence.SourceMemory.Version,
				ViaEvidenceID: evidence.InputEvidenceID,
			})
		case "message":
			b.inputRefs = append(b.inputRefs, MemoryCurationActionInputRef{
				Kind: MemoryCurationInputRefMessage, MessageID: evidence.SourceMessageID,
				ViaEvidenceID: evidence.InputEvidenceID,
			})
		case "import_artifact", "artifact":
		default:
			return false, ErrMemoryCurationConflict
		}
		return input.Sensitive, nil
	}

	if evidence.ResolutionState != MemoryEvidenceResolved {
		return false, ErrMemoryCurationConflict
	}
	switch evidence.ResolvedKind {
	case "transcript":
		if !memoryCurationTranscriptRangeCovered(b.auth.transcripts, evidence.SourceTranscriptID,
			evidence.SourceSequenceFrom, evidence.SourceSequenceUntil) {
			return false, ErrMemoryCurationConflict
		}
		b.inputRefs = append(b.inputRefs, MemoryCurationActionInputRef{
			Kind:         MemoryCurationInputRefTranscript,
			TranscriptID: evidence.SourceTranscriptID,
			SequenceFrom: evidence.SourceSequenceFrom, SequenceUntil: evidence.SourceSequenceUntil,
		})
		return evidence.ArtifactSensitive, nil
	case "memory":
		if evidence.SourceMemory == nil {
			return false, ErrMemoryCurationConflict
		}
		return b.authorizeVersion(*evidence.SourceMemory, false)
	default:
		// Message, import, external, and artifact provenance is authoritative
		// only through an exact materialized evidence row.
		return false, ErrMemoryCurationConflict
	}
}

func sameMemoryCurationInputEvidence(plan MemoryCurationEvidence, row MemoryEvidence) bool {
	expected := memoryCurationEvidenceFromInputRow(row)
	planCanonical, err := canonicalMemoryCurationJSON(plan)
	if err != nil {
		return false
	}
	expectedCanonical, err := canonicalMemoryCurationJSON(expected)
	return err == nil && bytes.Equal(planCanonical, expectedCanonical)
}

func memoryCurationEvidenceFromInputRow(row MemoryEvidence) MemoryCurationEvidence {
	expected := MemoryCurationEvidence{
		InputEvidenceID: row.ID,
		Type:            row.Type, Role: row.Role, ResolutionState: row.ResolutionState,
		ExternalLocator: row.ExternalLocator, ResolvedKind: row.ResolvedKind,
		SourceTranscriptID: row.SourceTranscriptID,
		SourceSequenceFrom: row.SourceSequenceFrom, SourceSequenceUntil: row.SourceSequenceUntil,
		SourceMessageID: row.SourceMessageID, SourceImportLocator: row.SourceImportLocator,
		ArtifactExcerpt:   append([]byte(nil), row.ArtifactExcerpt...),
		ArtifactSensitive: row.ArtifactSensitive, TerminalReasonCode: row.TerminalReasonCode,
		SourceDigest: row.SourceDigest,
	}
	if row.SourceMemoryID != "" {
		expected.SourceMemory = &MemoryCurationVersionReference{
			MemoryID: row.SourceMemoryID, Version: row.SourceMemoryVersion,
		}
	}
	return expected
}

func memoryCurationTranscriptRangeCovered(
	inputs []memoryCurationPlanTranscriptInput,
	transcriptID string,
	from, until int64,
) bool {
	if transcriptID == "" || from < 1 || until < from {
		return false
	}
	intervals := make([]memoryCurationPlanTranscriptInput, 0)
	for _, input := range inputs {
		if input.TranscriptID == transcriptID && input.Until >= from && input.From <= until {
			intervals = append(intervals, input)
		}
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].From != intervals[j].From {
			return intervals[i].From < intervals[j].From
		}
		return intervals[i].Until > intervals[j].Until
	})
	next := from
	for _, interval := range intervals {
		if interval.From > next {
			return false
		}
		if interval.Until >= until {
			return true
		}
		if interval.Until >= next {
			next = interval.Until + 1
		}
	}
	return false
}

func normalizeMemoryCurationActionInputRefs(
	refs []MemoryCurationActionInputRef,
) ([]MemoryCurationActionInputRef, error) {
	out := append([]MemoryCurationActionInputRef(nil), refs...)
	for _, ref := range out {
		valid := false
		switch ref.Kind {
		case MemoryCurationInputRefMemory, MemoryCurationInputRefCreateOutput:
			valid = validMemoryID(ref.MemoryID) && ref.Version > 0 && ref.EvidenceID == "" &&
				ref.TranscriptID == "" && ref.MessageID == ""
		case MemoryCurationInputRefEvidence:
			valid = validCurationID(ref.EvidenceID, "mev") && ref.MemoryID == "" &&
				ref.TranscriptID == "" && ref.MessageID == "" && ref.ViaEvidenceID == ""
		case MemoryCurationInputRefTranscript:
			valid = ref.TranscriptID != "" && ref.SequenceFrom > 0 && ref.SequenceUntil >= ref.SequenceFrom &&
				ref.MemoryID == "" && ref.EvidenceID == "" && ref.MessageID == ""
		case MemoryCurationInputRefMessage:
			valid = ref.MessageID != "" && ref.MemoryID == "" && ref.EvidenceID == "" && ref.TranscriptID == ""
		}
		if !valid || (ref.ViaEvidenceID != "" && !validCurationID(ref.ViaEvidenceID, "mev")) {
			return nil, ErrMemoryCurationConflict
		}
	}
	sort.Slice(out, func(i, j int) bool { return memoryCurationInputRefKey(out[i]) < memoryCurationInputRefKey(out[j]) })
	unique := out[:0]
	for _, ref := range out {
		if len(unique) == 0 || memoryCurationInputRefKey(unique[len(unique)-1]) != memoryCurationInputRefKey(ref) {
			unique = append(unique, ref)
		}
	}
	if unique == nil {
		unique = make([]MemoryCurationActionInputRef, 0)
	}
	return unique, nil
}

func memoryCurationInputRefKey(ref MemoryCurationActionInputRef) string {
	return strings.Join([]string{
		ref.Kind, ref.MemoryID, strconv.FormatInt(ref.Version, 10), ref.EvidenceID,
		ref.TranscriptID, strconv.FormatInt(ref.SequenceFrom, 10), strconv.FormatInt(ref.SequenceUntil, 10),
		ref.MessageID, ref.ViaEvidenceID,
	}, "\x00")
}

func normalizeMemoryCurationExpectedHeads(
	heads []MemoryCurationExpectedHead,
) ([]MemoryCurationExpectedHead, error) {
	out := append([]MemoryCurationExpectedHead(nil), heads...)
	for _, head := range out {
		if !validMemoryID(head.MemoryID) || head.ExpectedVersion < 1 {
			return nil, ErrMemoryCurationConflict
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MemoryID != out[j].MemoryID {
			return out[i].MemoryID < out[j].MemoryID
		}
		return out[i].ExpectedVersion < out[j].ExpectedVersion
	})
	unique := out[:0]
	for _, head := range out {
		if len(unique) > 0 && unique[len(unique)-1].MemoryID == head.MemoryID {
			if unique[len(unique)-1].ExpectedVersion != head.ExpectedVersion {
				return nil, ErrMemoryCurationConflict
			}
			continue
		}
		unique = append(unique, head)
	}
	if unique == nil {
		unique = make([]MemoryCurationExpectedHead, 0)
	}
	return unique, nil
}

func persistMemoryCurationPlanActions(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	runID string,
	planRevision int64,
	rows []memoryCurationAuthorizedAction,
) error {
	for _, row := range rows {
		actionID, err := id.New("mact")
		if err != nil {
			return err
		}
		payload, err := canonicalMemoryCurationJSON(row.Action)
		if err != nil {
			return ErrMemoryCurationConflict
		}
		inputRefs, err := canonicalMemoryCurationJSON(row.InputRefs)
		if err != nil {
			return ErrMemoryCurationConflict
		}
		expectedHeads, err := canonicalMemoryCurationJSON(row.ExpectedHeads)
		if err != nil {
			return ErrMemoryCurationConflict
		}
		validationResult, err := canonicalMemoryCurationJSON(struct {
			Authorized        bool `json:"authorized"`
			InputRefCount     int  `json:"input_ref_count"`
			ExpectedHeadCount int  `json:"expected_head_count"`
		}{true, len(row.InputRefs), len(row.ExpectedHeads)})
		if err != nil {
			return ErrMemoryCurationConflict
		}
		localRef := ""
		if row.Action.Create != nil {
			localRef = row.Action.Create.LocalRef
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO memory_curation_actions
			  (id,run_id,account_id,realm_id,owner_kind,owner_id,ordinal,
			   plan_revision,primitive,state,local_ref,input_refs,expected_heads,
			   proposed_payload,validation_result,action_hash,validated_at)
			VALUES ($1,$2,$3,$4,'agent',$5,$6,$7,$8,'validated',$9,
			        $10::jsonb,$11::jsonb,$12::jsonb,$13::jsonb,$14,clock_timestamp())`,
			actionID, runID, p.AccountID, p.RealmID, p.ID, row.Action.Ordinal,
			planRevision, row.Action.Operation, localRef, string(inputRefs),
			string(expectedHeads), string(payload), string(validationResult), row.ActionHash); err != nil {
			return fmt.Errorf("persist curation plan action: %w", err)
		}
	}
	return nil
}

// loadMemoryCurationStoredPlan is the only plan handoff accepted by apply.
// It reconstructs the immutable plan from ordered action rows and verifies
// every action hash plus the run-level canonical plan hash.
func loadMemoryCurationStoredPlan(
	ctx context.Context,
	q memoryQuerier,
	p Principal,
	run MemoryCurationRun,
) (memoryCurationStoredPlan, error) {
	if run.AccountID != p.AccountID || run.RealmID != p.RealmID || run.OwnerID != p.ID ||
		run.OwnerKind != PrincipalAgent || run.PlanSchema != MemoryCurationPlanSchemaV1 ||
		run.PlanRevision < 1 || len(run.PlanHash) != 64 {
		return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
	}
	rows, err := q.Query(ctx, `
		SELECT id,ordinal,plan_revision,primitive,state,local_ref,
		       input_refs::text,expected_heads::text,proposed_payload::text,
		       action_hash,scrubbed_at
		FROM memory_curation_actions
		WHERE run_id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4
		ORDER BY ordinal`, run.ID, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return memoryCurationStoredPlan{}, fmt.Errorf("load stored curation plan: %w", err)
	}
	defer rows.Close()

	stored := memoryCurationStoredPlan{}
	plan := MemoryCurationPlan{
		Schema: MemoryCurationPlanSchemaV1, PlanRevision: run.PlanRevision,
		Actions: make([]MemoryCurationPlanAction, 0),
	}
	mapping := make([]MemoryCurationPreallocatedMemoryID, 0)
	for rows.Next() {
		var actionRow memoryCurationStoredAction
		var ordinal, planRevision int64
		var primitive, state, localRef, actionHash string
		var inputRaw, headsRaw, payloadRaw []byte
		var scrubbedAt sql.NullTime
		if err := rows.Scan(&actionRow.ID, &ordinal, &planRevision, &primitive, &state,
			&localRef, &inputRaw, &headsRaw, &payloadRaw, &actionHash, &scrubbedAt); err != nil {
			return memoryCurationStoredPlan{}, err
		}
		if scrubbedAt.Valid || state == "draft" || planRevision != run.PlanRevision ||
			ordinal != int64(len(plan.Actions)+1) || !validCurationID(actionRow.ID, "mact") {
			return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
		}
		if err := decodeMemoryCurationStoredJSON(payloadRaw, &actionRow.Action); err != nil {
			return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
		}
		if actionRow.Action.Ordinal != ordinal || actionRow.Action.Operation != primitive ||
			!validMemoryCurationStoredActionEnvelope(actionRow.Action) {
			return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
		}
		if actionRow.Action.Create != nil {
			if localRef != actionRow.Action.Create.LocalRef {
				return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
			}
			mapping = append(mapping, MemoryCurationPreallocatedMemoryID{
				LocalRef: localRef, MemoryID: actionRow.Action.Create.MemoryID,
			})
		} else if localRef != "" {
			return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
		}
		if err := decodeMemoryCurationStoredJSON(inputRaw, &actionRow.InputRefs); err != nil {
			return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
		}
		normalizedRefs, err := normalizeMemoryCurationActionInputRefs(actionRow.InputRefs)
		if err != nil || !sameCanonicalMemoryCurationJSON(actionRow.InputRefs, normalizedRefs) {
			return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
		}
		if err := decodeMemoryCurationStoredJSON(headsRaw, &actionRow.ExpectedHeads); err != nil {
			return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
		}
		normalizedHeads, err := normalizeMemoryCurationExpectedHeads(actionRow.ExpectedHeads)
		if err != nil || !sameCanonicalMemoryCurationJSON(actionRow.ExpectedHeads, normalizedHeads) {
			return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
		}
		canonical, err := canonicalMemoryCurationJSON(actionRow.Action)
		if err != nil {
			return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
		}
		sum := sha256.Sum256(canonical)
		if actionHash != hex.EncodeToString(sum[:]) {
			return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
		}
		actionRow.ActionHash = actionHash
		stored.Actions = append(stored.Actions, actionRow)
		plan.Actions = append(plan.Actions, actionRow.Action)
		if len(plan.Actions) > MaxMemoryCurationPlanActions {
			return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
		}
	}
	if err := rows.Err(); err != nil {
		return memoryCurationStoredPlan{}, err
	}
	canonical, err := canonicalMemoryCurationJSON(plan)
	if err != nil || len(canonical) > MaxMemoryCurationPlanCanonicalJSONBytes {
		return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
	}
	sum := sha256.Sum256(canonical)
	if run.PlanHash != hex.EncodeToString(sum[:]) {
		return memoryCurationStoredPlan{}, ErrMemoryCurationConflict
	}
	stored.Acceptance = MemoryCurationPlanAcceptance{
		Plan: plan, PlanHash: run.PlanHash, PreallocatedMemoryIDs: mapping,
		Preview:       memoryCurationStoredPlanPreview(plan.Actions),
		canonicalJSON: append([]byte(nil), canonical...),
	}
	return stored, nil
}

func decodeMemoryCurationStoredJSON(raw []byte, destination any) error {
	if len(raw) == 0 || !json.Valid(raw) || rejectDuplicateJSONNames(raw) != nil {
		return ErrMemoryCurationConflict
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	return nil
}

func validMemoryCurationStoredActionEnvelope(action MemoryCurationPlanAction) bool {
	payloads := 0
	if action.Create != nil {
		payloads++
	}
	if action.Replace != nil {
		payloads++
	}
	if action.Supersede != nil {
		payloads++
	}
	if action.Relate != nil {
		payloads++
	}
	if action.ProposeFact != nil {
		payloads++
	}
	if payloads != 1 {
		return false
	}
	switch action.Operation {
	case MemoryCurationOperationCreate:
		return action.Create != nil && validMemoryID(action.Create.MemoryID) &&
			memoryCurationLocalRefPattern.MatchString(action.Create.LocalRef)
	case MemoryCurationOperationReplace:
		return action.Replace != nil
	case MemoryCurationOperationSupersede:
		return action.Supersede != nil
	case MemoryCurationOperationRelate:
		return action.Relate != nil
	case MemoryCurationOperationProposeFact:
		return action.ProposeFact != nil
	default:
		return false
	}
}

func sameCanonicalMemoryCurationJSON(a, b any) bool {
	aJSON, aErr := canonicalMemoryCurationJSON(a)
	bJSON, bErr := canonicalMemoryCurationJSON(b)
	return aErr == nil && bErr == nil && bytes.Equal(aJSON, bJSON)
}

func memoryCurationStoredPlanPreview(actions []MemoryCurationPlanAction) MemoryCurationImpactPreview {
	preview := MemoryCurationImpactPreview{ActionCount: len(actions)}
	for _, action := range actions {
		switch action.Operation {
		case MemoryCurationOperationCreate:
			preview.CreateActions++
			preview.NewMemories++
			preview.MemoryVersionWrites++
			preview.EvidenceRows += len(action.Create.Snapshot.Evidence)
			preview.RelationRows += len(action.Create.Relations)
		case MemoryCurationOperationReplace:
			preview.ReplaceActions++
			preview.MemoryVersionWrites++
			preview.ExpectedVersionChecks++
			preview.EvidenceRows += len(action.Replace.Snapshot.Evidence)
		case MemoryCurationOperationSupersede:
			preview.SupersedeActions++
			preview.MemoryVersionWrites++
			preview.ExpectedVersionChecks++
			preview.RelationRows += len(action.Supersede.Replacements)
		case MemoryCurationOperationRelate:
			preview.RelateActions++
			preview.RelationRows++
		case MemoryCurationOperationProposeFact:
			preview.ProposeFactActions++
			preview.FactCandidates++
			preview.EvidenceRows += len(action.ProposeFact.Evidence)
		}
	}
	return preview
}
