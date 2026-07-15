package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/id"
)

// ErrMemoryDependency reports a live memory or active lineage edge that still
// depends on the target. Permanent deletion never silently cascades into a
// distinct live memory.
var ErrMemoryDependency = errors.New("memory has live incoming dependencies")

// DeleteMemoryInput drives the value-free preview/apply contract. Preview uses
// MemoryID only. Apply binds the exact current version and deterministic scrub
// set returned by preview to one idempotency key.
type DeleteMemoryInput struct {
	MemoryID         string
	ExpectedVersion  int64
	ScrubSetRevision string
	ReasonCode       string
	IdempotencyKey   string
	Apply            bool
}

// DeleteMemoryResult deliberately contains no memory payload, tags, links,
// evidence locators/excerpts, content-derived hash, client provenance, raw
// idempotency key, or mutation request hash.
type DeleteMemoryResult struct {
	MemoryID                      string     `json:"memory_id"`
	ReceiptID                     string     `json:"receipt_id,omitempty"`
	PriorVersion                  int64      `json:"prior_version"`
	ScrubSetRevision              string     `json:"scrub_set_revision"`
	VersionCount                  int64      `json:"version_count"`
	EvidenceCount                 int64      `json:"evidence_count"`
	RelationCount                 int64      `json:"relation_count"`
	RetryShieldCount              int64      `json:"retry_shield_count"`
	RetryShieldDigest             string     `json:"retry_shield_digest"`
	IncomingEvidenceCount         int64      `json:"incoming_evidence_count"`
	ActiveRelationDependencyCount int64      `json:"active_relation_dependency_count"`
	ActiveCurationDependencyCount int64      `json:"active_curation_dependency_count"`
	CurationRunCount              int64      `json:"curation_run_count"`
	CurationActionCount           int64      `json:"curation_action_count"`
	CurationInputCount            int64      `json:"curation_input_count"`
	CurationMutationCount         int64      `json:"curation_mutation_count"`
	Blocked                       bool       `json:"blocked"`
	DeletedAt                     *time.Time `json:"deleted_at,omitempty"`
	Applied                       bool       `json:"applied"`
	Replayed                      bool       `json:"replayed"`
}

type memoryDeleteRetryShield struct {
	Kind string
	Hash string
}

type memoryDeleteSnapshot struct {
	Result         DeleteMemoryResult
	RetryShields   []memoryDeleteRetryShield
	CurationRunIDs []string
}

// DeleteMemory previews or permanently deletes one agent-owned narrative
// memory. Transport wiring is responsible for proving direct current-user
// authority; this store boundary enforces token-derived ownership, exact
// optimistic guards, retry safety, and the physical scrub transaction.
func (s *Store) DeleteMemory(ctx context.Context, p Principal, in DeleteMemoryInput) (DeleteMemoryResult, error) {
	if p.Kind != PrincipalAgent {
		return DeleteMemoryResult{}, ErrMemoryForbidden
	}
	var err error
	in, err = normalizeDeleteMemoryInput(in)
	if err != nil {
		return DeleteMemoryResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeleteMemoryResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return DeleteMemoryResult{}, err
	}
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return DeleteMemoryResult{}, err
	}
	if err := lockMemoryDeleteOwnerLane(ctx, tx, p); err != nil {
		return DeleteMemoryResult{}, err
	}

	if in.Apply {
		replayed, ok, err := replayDeleteMemory(ctx, tx, p, in)
		if err != nil {
			return DeleteMemoryResult{}, err
		}
		if ok {
			if err := tx.Commit(ctx); err != nil {
				return DeleteMemoryResult{}, err
			}
			return replayed, nil
		}
	}

	snapshot, deleted, err := loadMemoryDeleteSnapshot(ctx, tx, p, in.MemoryID)
	if err != nil {
		return DeleteMemoryResult{}, err
	}
	if deleted {
		return DeleteMemoryResult{}, ErrMemoryDeleted
	}
	if !in.Apply {
		if err := tx.Commit(ctx); err != nil {
			return DeleteMemoryResult{}, err
		}
		return snapshot.Result, nil
	}
	if snapshot.Result.PriorVersion != in.ExpectedVersion ||
		snapshot.Result.ScrubSetRevision != in.ScrubSetRevision {
		return DeleteMemoryResult{}, ErrMemoryConflict
	}
	if snapshot.Result.Blocked {
		return DeleteMemoryResult{}, ErrMemoryDependency
	}

	var deletedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&deletedAt); err != nil {
		return DeleteMemoryResult{}, err
	}
	receiptID, err := id.New("mdel")
	if err != nil {
		return DeleteMemoryResult{}, err
	}
	if err := preserveMemoryDeleteRetryShields(ctx, tx, p, in.MemoryID,
		snapshot.RetryShields, deletedAt); err != nil {
		return DeleteMemoryResult{}, err
	}
	if err := verifyMemoryDeleteRetryShields(ctx, tx, p, in.MemoryID,
		snapshot.Result.RetryShieldCount, snapshot.Result.RetryShieldDigest); err != nil {
		return DeleteMemoryResult{}, err
	}

	deleteKeyHash := memoryIdempotencyKeyHash(in.IdempotencyKey)
	tag, err := tx.Exec(ctx, `
		UPDATE memories SET current_version=NULL, origin='deleted',
		  capture_reason='deleted', permanently_deleted_at=$1,
		  permanently_deleted_by_id=$2, permanent_delete_reason=$3,
		  delete_receipt_id=$4, delete_idempotency_key_hash=$5,
		  deleted_prior_version=$6, deleted_scrub_set_revision=$7,
		  deleted_version_count=$8, deleted_evidence_count=$9,
		  deleted_relation_count=$10, deleted_retry_shield_count=$11,
		  deleted_retry_shield_digest=$12, deleted_curation_run_count=$13,
		  deleted_curation_action_count=$14, deleted_curation_input_count=$15,
		  deleted_curation_mutation_count=$16, updated_at=$1
		WHERE id=$17 AND account_id=$18 AND realm_id=$19
		  AND owner_kind='agent' AND owner_id=$20
		  AND current_version=$6 AND permanently_deleted_at IS NULL`,
		deletedAt, p.ID, in.ReasonCode, receiptID, deleteKeyHash,
		snapshot.Result.PriorVersion, snapshot.Result.ScrubSetRevision,
		snapshot.Result.VersionCount, snapshot.Result.EvidenceCount,
		snapshot.Result.RelationCount, snapshot.Result.RetryShieldCount,
		snapshot.Result.RetryShieldDigest, snapshot.Result.CurationRunCount,
		snapshot.Result.CurationActionCount, snapshot.Result.CurationInputCount,
		snapshot.Result.CurationMutationCount,
		in.MemoryID, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return DeleteMemoryResult{}, ErrMemoryIdempotencyConflict
		}
		return DeleteMemoryResult{}, fmt.Errorf("write memory deletion tombstone: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return DeleteMemoryResult{}, ErrMemoryConflict
	}

	if err := scrubMemoryDeleteCurationRows(ctx, tx, p, snapshot, deletedAt); err != nil {
		return DeleteMemoryResult{}, err
	}
	if err := purgeMemoryDeleteRows(ctx, tx, p, snapshot.Result); err != nil {
		return DeleteMemoryResult{}, err
	}

	out := snapshot.Result
	out.ReceiptID = receiptID
	out.DeletedAt = &deletedAt
	out.Applied = true
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
		Verb: VerbMemoryDeleted,
		Metadata: map[string]any{
			"memory_id": out.MemoryID, "receipt_id": out.ReceiptID,
			"prior_version":      strconv.FormatInt(out.PriorVersion, 10),
			"version_count":      strconv.FormatInt(out.VersionCount, 10),
			"evidence_count":     strconv.FormatInt(out.EvidenceCount, 10),
			"relation_count":     strconv.FormatInt(out.RelationCount, 10),
			"retry_shield_count": strconv.FormatInt(out.RetryShieldCount, 10),
		},
	}); err != nil {
		return DeleteMemoryResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DeleteMemoryResult{}, err
	}
	return out, nil
}

func normalizeDeleteMemoryInput(in DeleteMemoryInput) (DeleteMemoryInput, error) {
	in.MemoryID = strings.TrimSpace(in.MemoryID)
	in.ScrubSetRevision = strings.ToLower(strings.TrimSpace(in.ScrubSetRevision))
	in.ReasonCode = strings.TrimSpace(in.ReasonCode)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if !validMemoryID(in.MemoryID) {
		return DeleteMemoryInput{}, ErrMemoryInputInvalid
	}
	if !in.Apply {
		if in.ExpectedVersion != 0 || in.ScrubSetRevision != "" || in.IdempotencyKey != "" || in.ReasonCode != "" {
			return DeleteMemoryInput{}, fmt.Errorf("%w: preview accepts memory id only", ErrMemoryInputInvalid)
		}
		return in, nil
	}
	if in.ExpectedVersion < 1 || !isSHA256Hex(in.ScrubSetRevision) {
		return DeleteMemoryInput{}, fmt.Errorf("%w: apply requires expected version and scrub-set revision", ErrMemoryInputInvalid)
	}
	if err := validateMemoryIdempotencyKey(in.IdempotencyKey); err != nil {
		return DeleteMemoryInput{}, err
	}
	if in.ReasonCode == "" {
		in.ReasonCode = "direct_user_request"
	}
	if in.ReasonCode != "direct_user_request" {
		return DeleteMemoryInput{}, fmt.Errorf("%w: unsupported memory deletion reason", ErrMemoryInputInvalid)
	}
	return in, nil
}

func lockMemoryDeleteOwnerLane(ctx context.Context, tx pgx.Tx, p Principal) error {
	// Curation claims its lane before it locks memory heads/change clocks. Use
	// the same order so apply and permanent deletion cannot deadlock, and so a
	// frozen run cannot appear after the dependency snapshot.
	// Materialize the lane first: tolerating a missing row would let a
	// concurrent request insert and claim it after deletion had moved on to the
	// memory clock, escaping the dependency snapshot.
	if _, err := lockMemoryCurationSourceLaneTx(ctx, tx, p); err != nil {
		return fmt.Errorf("lock memory curation lane for deletion: %w", err)
	}
	var lastChangeSeq int64
	err := tx.QueryRow(ctx, `
		SELECT last_change_seq FROM memory_change_clocks
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		FOR UPDATE`, p.AccountID, p.RealmID, p.ID).Scan(&lastChangeSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrMemoryNotFound
	}
	if err != nil {
		return fmt.Errorf("lock memory owner lane for deletion: %w", err)
	}
	return nil
}

func loadMemoryDeleteSnapshot(ctx context.Context, tx pgx.Tx, p Principal, memoryID string) (memoryDeleteSnapshot, bool, error) {
	var currentVersion sql.NullInt64
	err := tx.QueryRow(ctx, `
		SELECT current_version FROM memories
		WHERE id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4
		FOR UPDATE`, memoryID, p.AccountID, p.RealmID, p.ID).Scan(&currentVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return memoryDeleteSnapshot{}, false, ErrMemoryNotFound
	}
	if err != nil {
		return memoryDeleteSnapshot{}, false, fmt.Errorf("lock memory for deletion: %w", err)
	}
	if !currentVersion.Valid {
		return memoryDeleteSnapshot{}, true, nil
	}

	digest := sha256.New()
	writeMemoryDeleteRevisionField(digest, "witself.memory-delete.scrub.v1")
	writeMemoryDeleteRevisionField(digest, memoryID)
	writeMemoryDeleteRevisionField(digest, strconv.FormatInt(currentVersion.Int64, 10))
	shields := make([]memoryDeleteRetryShield, 0, currentVersion.Int64+4)
	seenShields := map[string]bool{}

	var versionCount int64
	rows, err := tx.Query(ctx, `
		SELECT version, operation, idempotency_key,
		       supersession_set_id, supersession_set_revision,
		       supersession_replacement_count, supersession_replacement_digest
		FROM memory_versions
		WHERE memory_id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4
		ORDER BY version`, memoryID, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return memoryDeleteSnapshot{}, false, fmt.Errorf("read memory versions for deletion: %w", err)
	}
	for rows.Next() {
		var version int64
		var operation, key, supersessionSetID, supersessionDigest string
		var supersessionRevision, supersessionCount int64
		if err := rows.Scan(&version, &operation, &key, &supersessionSetID,
			&supersessionRevision, &supersessionCount, &supersessionDigest); err != nil {
			rows.Close()
			return memoryDeleteSnapshot{}, false, err
		}
		versionCount++
		keyHash := memoryIdempotencyKeyHash(key)
		for _, field := range []string{
			"version", strconv.FormatInt(version, 10), operation, keyHash,
			"supersession", supersessionSetID,
			strconv.FormatInt(supersessionRevision, 10),
			strconv.FormatInt(supersessionCount, 10), supersessionDigest,
		} {
			writeMemoryDeleteRevisionField(digest, field)
		}
		if err := appendMemoryDeleteRetryShield(&shields, seenShields,
			"idempotency."+operation, keyHash); err != nil {
			rows.Close()
			return memoryDeleteSnapshot{}, false, err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return memoryDeleteSnapshot{}, false, err
	}
	rows.Close()
	if versionCount != currentVersion.Int64 {
		return memoryDeleteSnapshot{}, false, ErrMemoryConflict
	}

	var evidenceCount int64
	rows, err = tx.Query(ctx, `
		SELECT id, target_version, evidence_change_seq, resolution_state,
		       COALESCE(pending_evidence_id,''), COALESCE(resolved_kind,''),
		       COALESCE(idempotency_key,'')
		FROM memory_evidence
		WHERE memory_id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4
		ORDER BY target_version, evidence_change_seq, id`,
		memoryID, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return memoryDeleteSnapshot{}, false, fmt.Errorf("read memory evidence for deletion: %w", err)
	}
	for rows.Next() {
		var evidenceID, state, pendingID, resolvedKind, key string
		var targetVersion, evidenceChangeSeq int64
		if err := rows.Scan(&evidenceID, &targetVersion, &evidenceChangeSeq,
			&state, &pendingID, &resolvedKind, &key); err != nil {
			rows.Close()
			return memoryDeleteSnapshot{}, false, err
		}
		evidenceCount++
		for _, field := range []string{
			"evidence", evidenceID, strconv.FormatInt(targetVersion, 10),
			strconv.FormatInt(evidenceChangeSeq, 10), state, pendingID, resolvedKind,
		} {
			writeMemoryDeleteRevisionField(digest, field)
		}
		if key != "" {
			if err := appendMemoryDeleteRetryShield(&shields, seenShields,
				"idempotency.evidence_resolution", memoryIdempotencyKeyHash(key)); err != nil {
				rows.Close()
				return memoryDeleteSnapshot{}, false, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return memoryDeleteSnapshot{}, false, err
	}
	rows.Close()
	if evidenceCount < 1 {
		return memoryDeleteSnapshot{}, false, ErrMemoryConflict
	}

	var relationCount int64
	rows, err = tx.Query(ctx, `
		SELECT id, from_memory_id, from_version, to_memory_id, to_version,
		       relation_type, COALESCE(supersession_set_id,''),
		       COALESCE(supersession_set_revision,0), reverted_at IS NOT NULL
		FROM memory_relations
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND (from_memory_id=$4 OR to_memory_id=$4)
		ORDER BY id`, p.AccountID, p.RealmID, p.ID, memoryID)
	if err != nil {
		return memoryDeleteSnapshot{}, false, fmt.Errorf("read memory relations for deletion: %w", err)
	}
	for rows.Next() {
		var relationID, fromID, toID, relationType, setID string
		var fromVersion, toVersion, setRevision int64
		var reverted bool
		if err := rows.Scan(&relationID, &fromID, &fromVersion, &toID, &toVersion,
			&relationType, &setID, &setRevision, &reverted); err != nil {
			rows.Close()
			return memoryDeleteSnapshot{}, false, err
		}
		relationCount++
		for _, field := range []string{
			"relation", relationID, fromID, strconv.FormatInt(fromVersion, 10),
			toID, strconv.FormatInt(toVersion, 10), relationType, setID,
			strconv.FormatInt(setRevision, 10), strconv.FormatBool(reverted),
		} {
			writeMemoryDeleteRevisionField(digest, field)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return memoryDeleteSnapshot{}, false, err
	}
	rows.Close()

	var incomingEvidenceCount, activeRelationDependencyCount, preexistingReferences int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM memory_evidence e
		JOIN memories dependent ON dependent.id=e.memory_id
		WHERE e.account_id=$1 AND e.realm_id=$2 AND e.owner_kind='agent' AND e.owner_id=$3
		  AND e.source_memory_id=$4 AND e.memory_id<>$4
		  AND dependent.current_version IS NOT NULL`,
		p.AccountID, p.RealmID, p.ID, memoryID).Scan(&incomingEvidenceCount); err != nil {
		return memoryDeleteSnapshot{}, false, fmt.Errorf("count incoming memory evidence: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM memory_relations r
		WHERE r.account_id=$1 AND r.realm_id=$2 AND r.owner_kind='agent' AND r.owner_id=$3
		  AND r.reverted_at IS NULL
		  AND ((r.from_memory_id=$4 AND r.to_memory_id<>$4) OR
		       (r.to_memory_id=$4 AND r.from_memory_id<>$4))
		  AND EXISTS (
		    SELECT 1 FROM memories other
		    WHERE other.id=CASE WHEN r.from_memory_id=$4 THEN r.to_memory_id ELSE r.from_memory_id END
		      AND other.current_version IS NOT NULL
		  )`, p.AccountID, p.RealmID, p.ID, memoryID).Scan(&activeRelationDependencyCount); err != nil {
		return memoryDeleteSnapshot{}, false, fmt.Errorf("count active memory relation dependencies: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM memory_deleted_references
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND deleted_memory_id=$4`, p.AccountID, p.RealmID, p.ID, memoryID).
		Scan(&preexistingReferences); err != nil {
		return memoryDeleteSnapshot{}, false, fmt.Errorf("count preexisting deleted-memory references: %w", err)
	}
	if preexistingReferences != 0 {
		return memoryDeleteSnapshot{}, false, ErrMemoryConflict
	}

	curationRunIDs, activeCurationDependencies, curationActions,
		curationInputs, curationMutations, err := loadMemoryDeleteCurationSnapshot(
		ctx, tx, p, memoryID,
	)
	if err != nil {
		return memoryDeleteSnapshot{}, false, err
	}

	sort.Slice(shields, func(i, j int) bool {
		if shields[i].Kind != shields[j].Kind {
			return shields[i].Kind < shields[j].Kind
		}
		return shields[i].Hash < shields[j].Hash
	})
	if int64(len(shields)) < versionCount {
		return memoryDeleteSnapshot{}, false, ErrMemoryConflict
	}
	shieldDigest := memoryDeleteRetryShieldDigest(shields)
	for _, field := range []string{
		"counts", strconv.FormatInt(versionCount, 10), strconv.FormatInt(evidenceCount, 10),
		strconv.FormatInt(relationCount, 10), strconv.FormatInt(incomingEvidenceCount, 10),
		strconv.FormatInt(activeRelationDependencyCount, 10),
		"curation", strconv.FormatInt(activeCurationDependencies, 10),
		strconv.FormatInt(int64(len(curationRunIDs)), 10),
		strconv.FormatInt(curationActions, 10), strconv.FormatInt(curationInputs, 10),
		strconv.FormatInt(curationMutations, 10),
		strconv.FormatInt(int64(len(shields)), 10), shieldDigest,
	} {
		writeMemoryDeleteRevisionField(digest, field)
	}
	for _, runID := range curationRunIDs {
		writeMemoryDeleteRevisionField(digest, runID)
	}

	result := DeleteMemoryResult{
		MemoryID: memoryID, PriorVersion: currentVersion.Int64,
		ScrubSetRevision: hex.EncodeToString(digest.Sum(nil)),
		VersionCount:     versionCount, EvidenceCount: evidenceCount,
		RelationCount: relationCount, RetryShieldCount: int64(len(shields)),
		RetryShieldDigest:             shieldDigest,
		IncomingEvidenceCount:         incomingEvidenceCount,
		ActiveRelationDependencyCount: activeRelationDependencyCount,
		ActiveCurationDependencyCount: activeCurationDependencies,
		CurationRunCount:              int64(len(curationRunIDs)),
		CurationActionCount:           curationActions,
		CurationInputCount:            curationInputs,
		CurationMutationCount:         curationMutations,
		Blocked: incomingEvidenceCount > 0 || activeRelationDependencyCount > 0 ||
			activeCurationDependencies > 0,
	}
	return memoryDeleteSnapshot{
		Result: result, RetryShields: shields, CurationRunIDs: curationRunIDs,
	}, false, nil
}

func loadMemoryDeleteCurationSnapshot(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	memoryID string,
) (runIDs []string, activeDependencies, actionCount, inputCount, mutationCount int64, err error) {
	rows, err := tx.Query(ctx, `
		WITH target_runs AS (
		  SELECT i.run_id
		  FROM memory_curation_run_inputs i
		  LEFT JOIN memory_evidence e ON e.id=i.evidence_id
		  WHERE i.account_id=$1 AND i.realm_id=$2 AND i.owner_kind='agent' AND i.owner_id=$3
		    AND (i.memory_id=$4 OR e.memory_id=$4)
		  UNION
		  SELECT v.curation_run_id
		  FROM memory_versions v
		  WHERE v.account_id=$1 AND v.realm_id=$2 AND v.owner_kind='agent' AND v.owner_id=$3
		    AND v.memory_id=$4 AND v.curation_run_id IS NOT NULL
		  UNION
		  SELECT r.curation_run_id
		  FROM memory_relations r
		  WHERE r.account_id=$1 AND r.realm_id=$2 AND r.owner_kind='agent' AND r.owner_id=$3
		    AND (r.from_memory_id=$4 OR r.to_memory_id=$4) AND r.curation_run_id IS NOT NULL
		  UNION
		  SELECT r.reverted_by_run_id
		  FROM memory_relations r
		  WHERE r.account_id=$1 AND r.realm_id=$2 AND r.owner_kind='agent' AND r.owner_id=$3
		    AND (r.from_memory_id=$4 OR r.to_memory_id=$4) AND r.reverted_by_run_id IS NOT NULL
		)
		SELECT run.id, run.state, run.scrubbed_at IS NOT NULL
		FROM target_runs target
		JOIN memory_curation_runs run ON run.id=target.run_id
		WHERE run.account_id=$1 AND run.realm_id=$2 AND run.owner_kind='agent' AND run.owner_id=$3
		ORDER BY run.id`, p.AccountID, p.RealmID, p.ID, memoryID)
	if err != nil {
		return nil, 0, 0, 0, 0, fmt.Errorf("read memory curation dependencies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var runID, state string
		var scrubbed bool
		if err := rows.Scan(&runID, &state, &scrubbed); err != nil {
			return nil, 0, 0, 0, 0, err
		}
		if state == "open" || state == "planned" {
			activeDependencies++
			continue
		}
		if !scrubbed {
			runIDs = append(runIDs, runID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, 0, 0, err
	}
	if len(runIDs) == 0 {
		return runIDs, activeDependencies, 0, 0, 0, nil
	}
	if err := tx.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memory_curation_actions
		   WHERE run_id=ANY($1) AND scrubbed_at IS NULL),
		  (SELECT count(*) FROM memory_curation_run_inputs WHERE run_id=ANY($1)),
		  (SELECT count(*) FROM memory_curation_mutations WHERE run_id=ANY($1))`,
		runIDs).Scan(&actionCount, &inputCount, &mutationCount); err != nil {
		return nil, 0, 0, 0, 0, fmt.Errorf("count memory curation scrub rows: %w", err)
	}
	return runIDs, activeDependencies, actionCount, inputCount, mutationCount, nil
}

func appendMemoryDeleteRetryShield(dst *[]memoryDeleteRetryShield, seen map[string]bool, kind, keyHash string) error {
	if !memoryCodePattern.MatchString(kind) || !isSHA256Hex(keyHash) {
		return ErrMemoryConflict
	}
	identity := kind + "\x00" + keyHash
	if seen[identity] {
		return ErrMemoryConflict
	}
	seen[identity] = true
	*dst = append(*dst, memoryDeleteRetryShield{Kind: kind, Hash: keyHash})
	return nil
}

func memoryDeleteRetryShieldDigest(shields []memoryDeleteRetryShield) string {
	ordered := append([]memoryDeleteRetryShield(nil), shields...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Kind != ordered[j].Kind {
			return ordered[i].Kind < ordered[j].Kind
		}
		return ordered[i].Hash < ordered[j].Hash
	})
	digest := sha256.New()
	writeMemoryDeleteRevisionField(digest, "witself.memory-delete.retry-shields.v1")
	for _, shield := range ordered {
		writeMemoryDeleteRevisionField(digest, shield.Kind)
		writeMemoryDeleteRevisionField(digest, shield.Hash)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeMemoryDeleteRevisionField(digest hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write([]byte(value))
}

func preserveMemoryDeleteRetryShields(ctx context.Context, tx pgx.Tx, p Principal, memoryID string, shields []memoryDeleteRetryShield, deletedAt time.Time) error {
	for _, shield := range shields {
		referenceID, err := id.New("mdr")
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO memory_deleted_references
			  (id,account_id,realm_id,owner_kind,owner_id,deleted_memory_id,
			   former_reference_kind,related_resource_id,reason_code,created_at)
			VALUES($1,$2,$3,'agent',$4,$5,$6,$7,'permanent_delete',$8)
			ON CONFLICT DO NOTHING`, referenceID, p.AccountID, p.RealmID, p.ID,
			memoryID, shield.Kind, shield.Hash, deletedAt)
		if err != nil {
			return fmt.Errorf("preserve deleted memory retry shield: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var existingMemoryID string
			err := tx.QueryRow(ctx, `
				SELECT deleted_memory_id FROM memory_deleted_references
				WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
				  AND former_reference_kind=$4 AND related_resource_id=$5`,
				p.AccountID, p.RealmID, p.ID, shield.Kind, shield.Hash).
				Scan(&existingMemoryID)
			if err != nil {
				return fmt.Errorf("resolve deleted memory retry shield conflict: %w", err)
			}
			if existingMemoryID != memoryID {
				return ErrMemoryIdempotencyConflict
			}
		}
	}
	return nil
}

func verifyMemoryDeleteRetryShields(ctx context.Context, q memoryQuerier, p Principal, memoryID string, wantCount int64, wantDigest string) error {
	rows, err := q.Query(ctx, `
		SELECT former_reference_kind, related_resource_id
		FROM memory_deleted_references
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND deleted_memory_id=$4 AND former_reference_kind LIKE 'idempotency.%'
		ORDER BY former_reference_kind, related_resource_id`,
		p.AccountID, p.RealmID, p.ID, memoryID)
	if err != nil {
		return fmt.Errorf("read deleted memory retry shields: %w", err)
	}
	defer rows.Close()
	shields := make([]memoryDeleteRetryShield, 0, wantCount)
	for rows.Next() {
		var shield memoryDeleteRetryShield
		if err := rows.Scan(&shield.Kind, &shield.Hash); err != nil {
			return err
		}
		shields = append(shields, shield)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if int64(len(shields)) != wantCount || memoryDeleteRetryShieldDigest(shields) != wantDigest {
		return ErrMemoryConflict
	}
	return nil
}

func scrubMemoryDeleteCurationRows(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	snapshot memoryDeleteSnapshot,
	deletedAt time.Time,
) error {
	if len(snapshot.CurationRunIDs) == 0 {
		return nil
	}
	mutationTag, err := tx.Exec(ctx, `
		DELETE FROM memory_curation_mutations
		WHERE run_id=ANY($1) AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4`,
		snapshot.CurationRunIDs, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return fmt.Errorf("scrub memory curation mutation receipts: %w", err)
	}
	if mutationTag.RowsAffected() != snapshot.Result.CurationMutationCount {
		return ErrMemoryConflict
	}
	inputTag, err := tx.Exec(ctx, `
		DELETE FROM memory_curation_run_inputs
		WHERE run_id=ANY($1) AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4`,
		snapshot.CurationRunIDs, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return fmt.Errorf("scrub memory curation materialized inputs: %w", err)
	}
	if inputTag.RowsAffected() != snapshot.Result.CurationInputCount {
		return ErrMemoryConflict
	}
	actionTag, err := tx.Exec(ctx, `
		UPDATE memory_curation_actions SET
		  local_ref='', input_refs='[]'::jsonb, expected_heads='[]'::jsonb,
		  proposed_payload='{}'::jsonb, validation_result='{}'::jsonb,
		  applied_result='{}'::jsonb, rollback_result='{}'::jsonb,
		  action_hash='', scrubbed_at=$1, scrubbed_reason_code='permanent_delete'
		WHERE run_id=ANY($2) AND account_id=$3 AND realm_id=$4
		  AND owner_kind='agent' AND owner_id=$5 AND scrubbed_at IS NULL`,
		deletedAt, snapshot.CurationRunIDs, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return fmt.Errorf("scrub memory curation actions: %w", err)
	}
	if actionTag.RowsAffected() != snapshot.Result.CurationActionCount {
		return ErrMemoryConflict
	}
	runTag, err := tx.Exec(ctx, `
		UPDATE memory_curation_runs SET
		  input_count=0, memory_input_count=0, evidence_input_count=0,
		  transcript_input_count=0, cursor_input_count=0, plan_hash='',
		  scrubbed_at=$1, scrubbed_reason_code='permanent_delete', updated_at=$1
		WHERE id=ANY($2) AND account_id=$3 AND realm_id=$4
		  AND owner_kind='agent' AND owner_id=$5 AND scrubbed_at IS NULL
		  AND state NOT IN ('open','planned')`,
		deletedAt, snapshot.CurationRunIDs, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return fmt.Errorf("scrub memory curation runs: %w", err)
	}
	if runTag.RowsAffected() != snapshot.Result.CurationRunCount {
		return ErrMemoryConflict
	}
	return nil
}

func purgeMemoryDeleteRows(ctx context.Context, tx pgx.Tx, p Principal, result DeleteMemoryResult) error {
	relationTag, err := tx.Exec(ctx, `
		DELETE FROM memory_relations
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND (from_memory_id=$4 OR to_memory_id=$4)`,
		p.AccountID, p.RealmID, p.ID, result.MemoryID)
	if err != nil {
		return fmt.Errorf("delete memory relations: %w", err)
	}
	if relationTag.RowsAffected() != result.RelationCount {
		return ErrMemoryConflict
	}
	evidenceTag, err := tx.Exec(ctx, `
		DELETE FROM memory_evidence
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND memory_id=$4`, p.AccountID, p.RealmID, p.ID, result.MemoryID)
	if err != nil {
		return fmt.Errorf("delete memory evidence: %w", err)
	}
	if evidenceTag.RowsAffected() != result.EvidenceCount {
		return ErrMemoryConflict
	}
	versionTag, err := tx.Exec(ctx, `
		DELETE FROM memory_versions
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND memory_id=$4`, p.AccountID, p.RealmID, p.ID, result.MemoryID)
	if err != nil {
		return fmt.Errorf("delete memory versions: %w", err)
	}
	if versionTag.RowsAffected() != result.VersionCount {
		return ErrMemoryConflict
	}
	return nil
}

func replayDeleteMemory(ctx context.Context, tx pgx.Tx, p Principal, in DeleteMemoryInput) (DeleteMemoryResult, bool, error) {
	keyHash := memoryIdempotencyKeyHash(in.IdempotencyKey)
	var out DeleteMemoryResult
	var deletedAt time.Time
	var reasonCode string
	err := tx.QueryRow(ctx, `
		SELECT id, delete_receipt_id, deleted_prior_version,
		       deleted_scrub_set_revision, deleted_version_count,
		       deleted_evidence_count, deleted_relation_count,
		       deleted_retry_shield_count, deleted_retry_shield_digest,
		       deleted_curation_run_count, deleted_curation_action_count,
		       deleted_curation_input_count, deleted_curation_mutation_count,
		       permanently_deleted_at, permanent_delete_reason
		FROM memories
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND delete_idempotency_key_hash=$4
		FOR UPDATE`, p.AccountID, p.RealmID, p.ID, keyHash).Scan(
		&out.MemoryID, &out.ReceiptID, &out.PriorVersion,
		&out.ScrubSetRevision, &out.VersionCount, &out.EvidenceCount,
		&out.RelationCount, &out.RetryShieldCount, &out.RetryShieldDigest,
		&out.CurationRunCount, &out.CurationActionCount,
		&out.CurationInputCount, &out.CurationMutationCount,
		&deletedAt, &reasonCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeleteMemoryResult{}, false, nil
	}
	if err != nil {
		return DeleteMemoryResult{}, false, fmt.Errorf("read memory deletion replay: %w", err)
	}
	if out.MemoryID != in.MemoryID || out.PriorVersion != in.ExpectedVersion ||
		out.ScrubSetRevision != in.ScrubSetRevision || reasonCode != in.ReasonCode {
		return DeleteMemoryResult{}, false, ErrMemoryIdempotencyConflict
	}
	if err := verifyMemoryDeleteRetryShields(ctx, tx, p, out.MemoryID,
		out.RetryShieldCount, out.RetryShieldDigest); err != nil {
		return DeleteMemoryResult{}, false, err
	}
	out.DeletedAt = &deletedAt
	out.Applied = true
	out.Replayed = true
	return out, true, nil
}
