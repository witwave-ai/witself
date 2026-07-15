package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

const maxMemorySupersessionReplacements = 32

// SupersedeMemoryInput atomically replaces one exact active memory head with
// one or more newly captured memories. Replacement payloads and evidence are
// entirely client-authored; the store performs no synthesis or inference.
type SupersedeMemoryInput struct {
	MemoryID        string                 `json:"memory_id"`
	ExpectedVersion int64                  `json:"expected_version"`
	Replacements    []CaptureMemoryInput   `json:"replacements"`
	Reason          string                 `json:"reason,omitempty"`
	Client          MemoryClientProvenance `json:"client,omitempty"`
	IdempotencyKey  string                 `json:"idempotency_key"`
}

// MemoryVersionReference is a value-free exact version locator used in
// supersession receipts.
type MemoryVersionReference struct {
	MemoryID string `json:"memory_id"`
	Version  int64  `json:"version"`
}

// MemorySupersessionReceipt identifies the immutable source and replacement
// versions in one server-assigned supersession set.
type MemorySupersessionReceipt struct {
	Operation               string                   `json:"operation"`
	ActorID                 string                   `json:"actor_id"`
	IdempotencyKey          string                   `json:"idempotency_key"`
	RequestHash             string                   `json:"request_hash"`
	SupersessionSetID       string                   `json:"supersession_set_id"`
	SupersessionSetRevision int64                    `json:"supersession_set_revision"`
	ReplacementCount        int64                    `json:"replacement_count"`
	ReplacementDigest       string                   `json:"replacement_digest"`
	Source                  MemoryVersionReference   `json:"source"`
	Replacements            []MemoryVersionReference `json:"replacements"`
	CreatedAt               time.Time                `json:"created_at"`
	Replayed                bool                     `json:"replayed,omitempty"`
}

// SupersedeMemoryResult returns the full authorized snapshots produced by an
// atomic supersession and a value-free deterministic retry receipt.
type SupersedeMemoryResult struct {
	Source       Memory                    `json:"source"`
	Replacements []Memory                  `json:"replacements"`
	Receipt      MemorySupersessionReceipt `json:"receipt"`
}

// SupersedeMemory appends a superseded source version, creates a nonempty set
// of active replacement memories with required evidence, and connects every
// replacement version to the exact new source version in one transaction.
func (s *Store) SupersedeMemory(ctx context.Context, p Principal, in SupersedeMemoryInput) (SupersedeMemoryResult, error) {
	if p.Kind != PrincipalAgent {
		return SupersedeMemoryResult{}, ErrMemoryForbidden
	}
	var err error
	in, err = normalizeSupersedeMemoryInput(in)
	if err != nil {
		return SupersedeMemoryResult{}, err
	}
	requestHash, err := memoryRequestHash(struct {
		Operation string               `json:"operation"`
		Input     SupersedeMemoryInput `json:"input"`
	}{Operation: "superseded", Input: in})
	if err != nil {
		return SupersedeMemoryResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SupersedeMemoryResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return SupersedeMemoryResult{}, err
	}
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return SupersedeMemoryResult{}, err
	}
	if replay, ok, err := memorySupersessionReplay(ctx, tx, p, in.IdempotencyKey, requestHash); err != nil || ok {
		return replay, err
	}
	lane, err := lockMemoryCurationSourceLaneTx(ctx, tx, p)
	if err != nil {
		return SupersedeMemoryResult{}, err
	}
	if replay, ok, err := memorySupersessionReplay(ctx, tx, p, in.IdempotencyKey, requestHash); err != nil || ok {
		return replay, err
	}
	if err := markMemoryCurationDueTx(ctx, tx, p, &lane,
		"memory_changed", MemoryCurationSourceMemory, in.IdempotencyKey); err != nil {
		return SupersedeMemoryResult{}, err
	}

	changeSeq, err := allocateMemoryChangeSeq(ctx, tx, p)
	if err != nil {
		return SupersedeMemoryResult{}, err
	}
	// The owner clock is the lane lock. Recheck under it so concurrent exact
	// retries cannot race the version-idempotency constraint.
	if replay, ok, err := memorySupersessionReplay(ctx, tx, p, in.IdempotencyKey, requestHash); err != nil || ok {
		return replay, err
	}

	var currentVersion int64
	err = tx.QueryRow(ctx, `
		SELECT current_version FROM memories
		WHERE id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4
		  AND current_version IS NOT NULL
		FOR UPDATE`, in.MemoryID, p.AccountID, p.RealmID, p.ID).Scan(&currentVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return SupersedeMemoryResult{}, ErrMemoryNotFound
	}
	if err != nil {
		return SupersedeMemoryResult{}, fmt.Errorf("lock supersession source: %w", err)
	}
	if currentVersion != in.ExpectedVersion {
		return SupersedeMemoryResult{}, ErrMemoryConflict
	}
	current, err := loadMemoryAtVersion(ctx, tx, p, in.MemoryID, currentVersion, false)
	if err != nil {
		return SupersedeMemoryResult{}, err
	}
	if current.State != MemoryStateActive {
		return SupersedeMemoryResult{}, ErrMemoryConflict
	}
	setID, err := id.New("mset")
	if err != nil {
		return SupersedeMemoryResult{}, err
	}
	const setRevision int64 = 1
	replacementRefs := make([]MemoryVersionReference, len(in.Replacements))
	for i := range replacementRefs {
		memoryID, err := id.New("mem")
		if err != nil {
			return SupersedeMemoryResult{}, err
		}
		replacementRefs[i] = MemoryVersionReference{MemoryID: memoryID, Version: 1}
	}

	source := current
	source.PreviousVersion = current.Version
	source.Version = current.Version + 1
	source.ChangeSeq = changeSeq
	source.State = MemoryStateSuperseded
	source.PriorState = ""
	source.LifecycleReason = in.Reason
	source.ActorKind = "agent"
	source.ActorID = p.ID
	source.Operation = "superseded"
	source.IdempotencyKey = in.IdempotencyKey
	source.RequestHash = requestHash
	source.Client = in.Client
	source.Evidence = nil
	source.SupersessionSetID = setID
	source.SupersessionSetRevision = setRevision
	source.SupersessionReplacementCount = int64(len(replacementRefs))
	source.SupersessionReplacementDigest = memorySupersessionMembershipDigest(replacementRefs)
	createdAt, err := insertMemoryVersionTx(ctx, tx, source)
	if err != nil {
		return SupersedeMemoryResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memories SET current_version=$2, updated_at=$3 WHERE id=$1`,
		in.MemoryID, source.Version, createdAt); err != nil {
		return SupersedeMemoryResult{}, fmt.Errorf("move superseded memory head: %w", err)
	}

	for i := range in.Replacements {
		replacement := in.Replacements[i]
		replacementSeq, err := allocateMemoryChangeSeq(ctx, tx, p)
		if err != nil {
			return SupersedeMemoryResult{}, err
		}
		memoryID := replacementRefs[i].MemoryID
		_, err = tx.Exec(ctx, `
			INSERT INTO memories
			  (id, account_id, realm_id, owner_kind, owner_id, origin,
			   capture_reason, authored_by_agent_id, current_version)
			VALUES ($1,$2,$3,'agent',$4,$5,$6,$4,1)`,
			memoryID, p.AccountID, p.RealmID, p.ID,
			replacement.Origin, replacement.CaptureReason)
		if err != nil {
			return SupersedeMemoryResult{}, fmt.Errorf("insert replacement memory head: %w", err)
		}
		snapshot := Memory{
			ID: memoryID, AccountID: p.AccountID, RealmID: p.RealmID,
			OwnerKind: "agent", OwnerID: p.ID, Origin: replacement.Origin,
			CaptureReason: replacement.CaptureReason, AuthoredByAgentID: p.ID,
			Version: 1, ChangeSeq: replacementSeq, Content: replacement.Content,
			ContentEncoding: replacement.ContentEncoding, Kind: replacement.Kind,
			Tags: replacement.Tags, Links: replacement.Links,
			Salience: *replacement.Salience, Sensitive: replacement.Sensitive,
			OccurredFrom: replacement.OccurredFrom, OccurredUntil: replacement.OccurredUntil,
			State: MemoryStateActive, ContentHash: memoryContentHash(replacement.Content),
			ActorKind: "agent", ActorID: p.ID, Operation: "added",
			IdempotencyKey: replacement.IdempotencyKey, RequestHash: requestHash,
			Client: replacement.Client,
		}
		replacementCreatedAt, err := insertMemoryVersionTx(ctx, tx, snapshot)
		if err != nil {
			return SupersedeMemoryResult{}, err
		}
		for evidenceIndex := range replacement.Evidence {
			evidence := replacement.Evidence[evidenceIndex]
			if err := validateMemoryEvidenceSourceTx(ctx, tx, p, evidence); err != nil {
				return SupersedeMemoryResult{}, fmt.Errorf("replacement %d evidence %d: %w", i, evidenceIndex, err)
			}
			if _, err := insertMemoryEvidenceTx(ctx, tx, p, memoryID, 1, evidence, "", ""); err != nil {
				return SupersedeMemoryResult{}, fmt.Errorf("replacement %d evidence %d: %w", i, evidenceIndex, err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE memories SET updated_at=$2 WHERE id=$1`,
			memoryID, replacementCreatedAt); err != nil {
			return SupersedeMemoryResult{}, fmt.Errorf("update replacement memory timestamp: %w", err)
		}
	}
	for _, replacement := range replacementRefs {
		relationID, err := id.New("mrel")
		if err != nil {
			return SupersedeMemoryResult{}, err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO memory_relations
			  (id, account_id, realm_id, owner_kind, owner_id,
			   from_memory_id, from_version, to_memory_id, to_version,
			   relation_type, supersession_set_id, supersession_set_revision)
			VALUES ($1,$2,$3,'agent',$4,$5,$6,$7,$8,'supersedes',$9,$10)`,
			relationID, p.AccountID, p.RealmID, p.ID,
			replacement.MemoryID, replacement.Version, source.ID, source.Version,
			setID, setRevision)
		if err != nil {
			return SupersedeMemoryResult{}, fmt.Errorf("insert supersession relation: %w", err)
		}
	}

	out, err := loadMemorySupersessionResult(ctx, tx, p, setID, setRevision,
		MemoryVersionReference{MemoryID: source.ID, Version: source.Version},
		in.IdempotencyKey, requestHash, false)
	if err != nil {
		return SupersedeMemoryResult{}, err
	}
	if err := logMemoryVersionEventTx(ctx, tx, p, out.Source); err != nil {
		return SupersedeMemoryResult{}, err
	}
	for i := range out.Replacements {
		if err := logMemoryVersionEventTx(ctx, tx, p, out.Replacements[i]); err != nil {
			return SupersedeMemoryResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SupersedeMemoryResult{}, err
	}
	return out, nil
}

func normalizeSupersedeMemoryInput(in SupersedeMemoryInput) (SupersedeMemoryInput, error) {
	// Supersession is an API boundary: normalization must never write through
	// caller-owned replacement slices or their nested slices and pointers. In
	// particular, a caller may safely reuse one immutable request for concurrent
	// idempotent retries.
	in = cloneSupersedeMemoryInput(in)
	in.MemoryID = strings.TrimSpace(in.MemoryID)
	if !validMemoryID(in.MemoryID) || in.ExpectedVersion < 1 {
		return SupersedeMemoryInput{}, fmt.Errorf("%w: memory id and expected version are required", ErrMemoryInputInvalid)
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Reason) > 2048 {
		return SupersedeMemoryInput{}, fmt.Errorf("%w: reason is too large", ErrMemoryInputInvalid)
	}
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if err := validateMemoryIdempotencyKey(in.IdempotencyKey); err != nil {
		return SupersedeMemoryInput{}, err
	}
	if err := normalizeMemoryClient(&in.Client); err != nil {
		return SupersedeMemoryInput{}, err
	}
	if len(in.Replacements) < 1 || len(in.Replacements) > maxMemorySupersessionReplacements {
		return SupersedeMemoryInput{}, fmt.Errorf("%w: replacements must contain 1-%d memories",
			ErrMemoryInputInvalid, maxMemorySupersessionReplacements)
	}
	seenKeys := map[string]struct{}{in.IdempotencyKey: {}}
	for i := range in.Replacements {
		var err error
		in.Replacements[i], err = normalizeCaptureMemoryInput(in.Replacements[i])
		if err != nil {
			return SupersedeMemoryInput{}, fmt.Errorf("replacement %d: %w", i, err)
		}
		key := in.Replacements[i].IdempotencyKey
		if _, exists := seenKeys[key]; exists {
			return SupersedeMemoryInput{}, fmt.Errorf("%w: supersession and replacement idempotency keys must be unique", ErrMemoryInputInvalid)
		}
		seenKeys[key] = struct{}{}
	}
	return in, nil
}

func cloneSupersedeMemoryInput(in SupersedeMemoryInput) SupersedeMemoryInput {
	out := in
	out.Replacements = slices.Clone(in.Replacements)
	return out
}

func memorySupersessionReplay(
	ctx context.Context,
	q memoryQuerier,
	p Principal,
	idempotencyKey, requestHash string,
) (SupersedeMemoryResult, bool, error) {
	blocked, err := memoryRetryBlocked(ctx, q, p, "superseded", idempotencyKey)
	if err != nil {
		return SupersedeMemoryResult{}, false, err
	}
	if blocked {
		return SupersedeMemoryResult{}, false, ErrMemoryDeleted
	}
	var memoryID, storedHash, operation string
	var version int64
	err = q.QueryRow(ctx, `
		SELECT memory_id, version, request_hash, operation
		FROM memory_versions
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND idempotency_key=$4`, p.AccountID, p.RealmID, p.ID, idempotencyKey).
		Scan(&memoryID, &version, &storedHash, &operation)
	if errors.Is(err, pgx.ErrNoRows) {
		return SupersedeMemoryResult{}, false, nil
	}
	if err != nil {
		return SupersedeMemoryResult{}, false, fmt.Errorf("find memory supersession retry: %w", err)
	}
	if storedHash != requestHash || operation != "superseded" {
		return SupersedeMemoryResult{}, false, ErrMemoryIdempotencyConflict
	}
	source, err := loadMemoryAtVersion(ctx, q, p, memoryID, version, false)
	if err != nil {
		return SupersedeMemoryResult{}, false, err
	}
	if !validMemorySupersessionReceipt(source) {
		return SupersedeMemoryResult{}, false, ErrMemoryConflict
	}
	out, err := loadMemorySupersessionResult(ctx, q, p,
		source.SupersessionSetID, source.SupersessionSetRevision,
		MemoryVersionReference{MemoryID: memoryID, Version: version},
		idempotencyKey, requestHash, true)
	if err != nil {
		return SupersedeMemoryResult{}, false, err
	}
	return out, true, nil
}

func loadMemorySupersessionResult(
	ctx context.Context,
	q memoryQuerier,
	p Principal,
	setID string,
	setRevision int64,
	expectedSource MemoryVersionReference,
	idempotencyKey, requestHash string,
	replayed bool,
) (SupersedeMemoryResult, error) {
	source, err := loadMemoryAtVersion(ctx, q, p,
		expectedSource.MemoryID, expectedSource.Version, true)
	if err != nil {
		return SupersedeMemoryResult{}, err
	}
	if source.Operation != "superseded" || source.RequestHash != requestHash ||
		source.IdempotencyKey != idempotencyKey || source.State != MemoryStateSuperseded ||
		!validMemorySupersessionReceipt(source) ||
		source.SupersessionSetID != setID ||
		source.SupersessionSetRevision != setRevision {
		return SupersedeMemoryResult{}, ErrMemoryConflict
	}
	rows, err := q.Query(ctx, `
		SELECT from_memory_id, from_version, to_memory_id, to_version
		FROM memory_relations
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND relation_type='supersedes' AND supersession_set_id=$4
		  AND supersession_set_revision=$5
		  AND to_memory_id=$6 AND to_version=$7
		ORDER BY from_memory_id, from_version, to_memory_id, to_version`,
		p.AccountID, p.RealmID, p.ID, setID, setRevision,
		expectedSource.MemoryID, expectedSource.Version)
	if err != nil {
		return SupersedeMemoryResult{}, fmt.Errorf("read memory supersession set: %w", err)
	}
	defer rows.Close()
	var sourceRef MemoryVersionReference
	replacementRefs := make([]MemoryVersionReference, 0)
	seenReplacements := make(map[MemoryVersionReference]struct{})
	for rows.Next() {
		var from, to MemoryVersionReference
		if err := rows.Scan(&from.MemoryID, &from.Version, &to.MemoryID, &to.Version); err != nil {
			return SupersedeMemoryResult{}, fmt.Errorf("scan memory supersession set: %w", err)
		}
		if to != expectedSource {
			return SupersedeMemoryResult{}, ErrMemoryConflict
		}
		if sourceRef.MemoryID == "" {
			sourceRef = to
		} else if sourceRef != to {
			return SupersedeMemoryResult{}, ErrMemoryConflict
		}
		if _, exists := seenReplacements[from]; exists {
			return SupersedeMemoryResult{}, ErrMemoryConflict
		}
		seenReplacements[from] = struct{}{}
		replacementRefs = append(replacementRefs, from)
	}
	if err := rows.Err(); err != nil {
		return SupersedeMemoryResult{}, fmt.Errorf("read memory supersession set: %w", err)
	}
	rows.Close()
	sort.Slice(replacementRefs, func(i, j int) bool {
		if replacementRefs[i].MemoryID != replacementRefs[j].MemoryID {
			return replacementRefs[i].MemoryID < replacementRefs[j].MemoryID
		}
		return replacementRefs[i].Version < replacementRefs[j].Version
	})
	if sourceRef != expectedSource ||
		int64(len(replacementRefs)) != source.SupersessionReplacementCount ||
		memorySupersessionMembershipDigest(replacementRefs) != source.SupersessionReplacementDigest {
		// Authorized permanent deletion can remove a reverted relation and its
		// replacement version. The immutable source receipt makes that loss
		// detectable; never reconstruct a smaller successful replay.
		return SupersedeMemoryResult{}, ErrMemoryDeleted
	}
	replacements := make([]Memory, 0, len(replacementRefs))
	for _, ref := range replacementRefs {
		memory, err := loadMemoryAtVersion(ctx, q, p, ref.MemoryID, ref.Version, true)
		if errors.Is(err, ErrMemoryNotFound) {
			return SupersedeMemoryResult{}, ErrMemoryDeleted
		}
		if err != nil {
			return SupersedeMemoryResult{}, err
		}
		if memory.Operation != "added" || memory.RequestHash != requestHash ||
			memory.State != MemoryStateActive || memory.Version != 1 {
			return SupersedeMemoryResult{}, ErrMemoryConflict
		}
		replacements = append(replacements, memory)
	}
	receipt := MemorySupersessionReceipt{
		Operation: "superseded", ActorID: source.ActorID,
		IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		SupersessionSetID: setID, SupersessionSetRevision: setRevision,
		ReplacementCount:  source.SupersessionReplacementCount,
		ReplacementDigest: source.SupersessionReplacementDigest,
		Source:            sourceRef, Replacements: replacementRefs,
		CreatedAt: source.CreatedAt, Replayed: replayed,
	}
	return SupersedeMemoryResult{Source: source, Replacements: replacements, Receipt: receipt}, nil
}

func validMemorySupersessionReceipt(memory Memory) bool {
	return memory.Operation == "superseded" &&
		strings.HasPrefix(memory.SupersessionSetID, "mset_") &&
		memory.SupersessionSetRevision > 0 &&
		memory.SupersessionReplacementCount > 0 &&
		memory.SupersessionReplacementCount <= maxMemorySupersessionReplacements &&
		isSHA256Hex(memory.SupersessionReplacementDigest)
}

// memorySupersessionMembershipDigest commits only to value-free exact
// replacement version locators. Sorting makes the replacement set independent
// of response order while the length-prefix encoding prevents ambiguity.
func memorySupersessionMembershipDigest(refs []MemoryVersionReference) string {
	ordered := append([]MemoryVersionReference(nil), refs...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].MemoryID != ordered[j].MemoryID {
			return ordered[i].MemoryID < ordered[j].MemoryID
		}
		return ordered[i].Version < ordered[j].Version
	})
	digest := sha256.New()
	writeMemoryDeleteRevisionField(digest, "witself.memory-supersession.membership.v1")
	for _, ref := range ordered {
		writeMemoryDeleteRevisionField(digest, ref.MemoryID)
		writeMemoryDeleteRevisionField(digest, fmt.Sprintf("%d", ref.Version))
	}
	return hex.EncodeToString(digest.Sum(nil))
}
