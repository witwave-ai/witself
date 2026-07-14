package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// DeleteFactInput drives the two-step permanent-deletion contract. Apply=false
// is a value-free preview by either FactID or exact Subject+Predicate address.
// Apply=true uses FactID only and requires the preview's resolved assertion id
// plus a non-empty idempotency key, preventing an unseen update from being
// erased and making a lost response safely replayable.
type DeleteFactInput struct {
	FactID                      string
	Subject                     string
	Predicate                   string
	ExpectedResolvedAssertionID string
	ExpectedCandidateRevision   string
	IdempotencyKey              string
	Apply                       bool
}

// DeleteFactResult is both the preview and permanent-deletion receipt. It
// deliberately contains no assertion value, value type, source reference,
// candidate reason, request fingerprint, or raw idempotency key.
type DeleteFactResult struct {
	FactID                   string     `json:"fact_id"`
	ReceiptID                string     `json:"receipt_id,omitempty"`
	SubjectID                string     `json:"subject_id"`
	Subject                  string     `json:"subject"`
	Predicate                string     `json:"predicate"`
	PriorResolvedAssertionID string     `json:"prior_resolved_assertion_id"`
	CandidateRevision        string     `json:"candidate_revision"`
	AssertionCount           int64      `json:"assertion_count"`
	CandidateCount           int64      `json:"candidate_count"`
	UsageCount               int64      `json:"usage_count"`
	Sensitive                bool       `json:"sensitive"`
	DeletedAt                *time.Time `json:"deleted_at,omitempty"`
	Applied                  bool       `json:"applied"`
	Replayed                 bool       `json:"replayed"`
}

// DeleteFact previews or permanently deletes one stable fact id. The fact row
// remains as a value-free tombstone; assertions and every candidate at its
// canonical address are physically removed while subjects, aliases, usage,
// and the account audit ledger remain intact.
func (s *Store) DeleteFact(ctx context.Context, p Principal, in DeleteFactInput) (DeleteFactResult, error) {
	if p.Kind != PrincipalAgent {
		return DeleteFactResult{}, ErrFactForbidden
	}
	in.FactID = strings.TrimSpace(in.FactID)
	in.Subject = strings.TrimSpace(in.Subject)
	in.Predicate = strings.TrimSpace(in.Predicate)
	in.ExpectedResolvedAssertionID = strings.TrimSpace(in.ExpectedResolvedAssertionID)
	in.ExpectedCandidateRevision = strings.TrimSpace(in.ExpectedCandidateRevision)
	if in.Apply {
		if in.FactID == "" || in.Subject != "" || in.Predicate != "" {
			return DeleteFactResult{}, fmt.Errorf("%w: apply requires fact id only", ErrFactInputInvalid)
		}
	} else if in.FactID != "" {
		if in.Subject != "" || in.Predicate != "" {
			return DeleteFactResult{}, fmt.Errorf("%w: preview accepts one selector", ErrFactInputInvalid)
		}
	} else {
		if in.Subject == "" || in.Predicate == "" {
			return DeleteFactResult{}, fmt.Errorf("%w: preview requires fact id or exact address", ErrFactInputInvalid)
		}
		in.Subject = normalizeFactSubject(in.Subject)
		if !validFactSubjectAlias(in.Subject) || !validFactPredicate(in.Predicate) {
			return DeleteFactResult{}, ErrFactInputInvalid
		}
	}
	var err error
	in.IdempotencyKey, err = normalizeFactIdempotencyKey(in.IdempotencyKey)
	if err != nil {
		return DeleteFactResult{}, err
	}
	if in.Apply && (in.IdempotencyKey == "" || in.ExpectedResolvedAssertionID == "" ||
		!validFactSHA256(in.ExpectedCandidateRevision)) {
		return DeleteFactResult{}, fmt.Errorf("%w: apply requires expected assertion, candidate revision, and idempotency key", ErrFactInputInvalid)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeleteFactResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return DeleteFactResult{}, err
	}
	// Exclusive ownership of the subject namespace serializes delete against
	// SetFact, ProposeFact, candidate confirmation, alias changes, and explicit
	// recreation for this agent.
	if err := lockFactSubjectNamespace(ctx, tx, p, true); err != nil {
		return DeleteFactResult{}, err
	}
	if err := lockFactIdempotencyKey(ctx, tx, p, "delete", in.IdempotencyKey); err != nil {
		return DeleteFactResult{}, err
	}

	if in.Apply {
		replayed, ok, err := replayDeleteFact(ctx, tx, p, in.FactID,
			in.ExpectedResolvedAssertionID, in.ExpectedCandidateRevision, in.IdempotencyKey)
		if err != nil {
			return DeleteFactResult{}, err
		}
		if ok {
			if err := tx.Commit(ctx); err != nil {
				return DeleteFactResult{}, err
			}
			return replayed, nil
		}
	}

	out, deleted, err := loadFactDeletionResult(ctx, tx, p, in.FactID, in.Subject, in.Predicate)
	if err != nil {
		return DeleteFactResult{}, err
	}
	if deleted {
		return DeleteFactResult{}, ErrFactDeleted
	}
	out.CandidateRevision, err = factCandidateRevision(ctx, tx, p, out)
	if err != nil {
		return DeleteFactResult{}, err
	}
	if !in.Apply {
		if err := tx.Commit(ctx); err != nil {
			return DeleteFactResult{}, err
		}
		return out, nil
	}
	if out.PriorResolvedAssertionID != in.ExpectedResolvedAssertionID {
		return DeleteFactResult{}, ErrFactConflict
	}
	if out.CandidateRevision != in.ExpectedCandidateRevision {
		return DeleteFactResult{}, ErrFactConflict
	}

	var deletedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&deletedAt); err != nil {
		return DeleteFactResult{}, err
	}
	receiptID, err := id.New("fdel")
	if err != nil {
		return DeleteFactResult{}, err
	}
	mutationKeyCount, err := preserveDeletedFactMutationKeys(ctx, tx, p, out, deletedAt)
	if err != nil {
		return DeleteFactResult{}, err
	}

	// Delete candidates first so all content rows disappear explicitly rather
	// than depending on assertion FK cascades. The exclusive namespace lock
	// keeps these counts stable against fact mutation surfaces.
	candidateTag, err := tx.Exec(ctx, `
		DELETE FROM fact_candidates c USING fact_subjects s
		WHERE c.account_id=$1 AND c.realm_id=$2 AND c.owner_agent_id=$3
		  AND s.id=$4 AND (c.subject_key=$5 OR s.aliases ? c.subject_key)
		  AND c.predicate=$6`, p.AccountID, p.RealmID, p.ID,
		out.SubjectID, out.Subject, out.Predicate)
	if err != nil {
		return DeleteFactResult{}, fmt.Errorf("delete fact candidates: %w", err)
	}
	if candidateTag.RowsAffected() != out.CandidateCount {
		return DeleteFactResult{}, ErrFactConflict
	}
	assertionTag, err := tx.Exec(ctx, `DELETE FROM fact_assertions WHERE fact_id=$1`, out.FactID)
	if err != nil {
		return DeleteFactResult{}, fmt.Errorf("delete fact assertions: %w", err)
	}
	if assertionTag.RowsAffected() != out.AssertionCount {
		return DeleteFactResult{}, ErrFactConflict
	}

	deleteKeyHash := factIdempotencyKeyHash(in.IdempotencyKey)
	tag, err := tx.Exec(ctx, `
		UPDATE facts SET resolved_assertion_id=NULL, deleted_at=$1,
		  deleted_by_agent_id=$2, delete_receipt_id=$3,
		  delete_idempotency_key_hash=$4, deleted_prior_assertion_id=$5,
		  deleted_assertion_count=$6, deleted_candidate_count=$7,
		  deleted_usage_count=$8,
		  deleted_mutation_key_count=$9,
		  deleted_candidate_revision=$10,
		  updated_at=$1
		WHERE id=$11 AND account_id=$12 AND realm_id=$13 AND owner_agent_id=$14
		  AND deleted_at IS NULL AND resolved_assertion_id=$5`, deletedAt, p.ID,
		receiptID, deleteKeyHash, out.PriorResolvedAssertionID,
		out.AssertionCount, out.CandidateCount, out.UsageCount, mutationKeyCount,
		out.CandidateRevision, out.FactID,
		p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return DeleteFactResult{}, fmt.Errorf("write fact tombstone: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return DeleteFactResult{}, ErrFactConflict
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: p.AccountID,
		ActorKind: ActorAgent,
		ActorID:   p.ID,
		Verb:      VerbFactDeleted,
		Metadata: map[string]any{
			"fact_id":         out.FactID,
			"subject_id":      out.SubjectID,
			"subject":         out.Subject,
			"predicate":       out.Predicate,
			"receipt_id":      receiptID,
			"assertion_count": out.AssertionCount,
			"candidate_count": out.CandidateCount,
			"usage_count":     out.UsageCount,
			"sensitive":       out.Sensitive,
		},
	}); err != nil {
		return DeleteFactResult{}, err
	}

	out.DeletedAt = &deletedAt
	out.ReceiptID = receiptID
	out.Applied = true
	if err := tx.Commit(ctx); err != nil {
		return DeleteFactResult{}, err
	}
	return out, nil
}

func loadFactDeletionResult(ctx context.Context, q factQuerier, p Principal, factID, subject, predicate string) (DeleteFactResult, bool, error) {
	var out DeleteFactResult
	var deletedAt *time.Time
	query := `
		SELECT f.id, f.delete_receipt_id, f.subject_id, s.canonical_key, f.predicate,
		       COALESCE(f.resolved_assertion_id,f.deleted_prior_assertion_id,''),
		       f.deleted_candidate_revision, f.sensitive, f.deleted_at,
		       CASE WHEN f.deleted_at IS NULL
		         THEN (SELECT COUNT(*) FROM fact_assertions a WHERE a.fact_id=f.id)
		         ELSE f.deleted_assertion_count END,
		       CASE WHEN f.deleted_at IS NULL
		         THEN (SELECT COUNT(*) FROM fact_candidates c
		               WHERE c.account_id=f.account_id AND c.realm_id=f.realm_id
		                 AND c.owner_agent_id=f.owner_agent_id
		                 AND (c.subject_key=s.canonical_key OR s.aliases ? c.subject_key)
		                 AND c.predicate=f.predicate)
		         ELSE f.deleted_candidate_count END,
		       CASE WHEN f.deleted_at IS NULL THEN
		         COALESCE((SELECT SUM(u.quantity) FROM usage_events u
		           WHERE u.account_id=f.account_id AND u.realm_id=f.realm_id
		             AND u.agent_id=f.owner_agent_id AND u.dimension='fact_returned'
		             AND u.unit='fact' AND u.subject_type='fact' AND u.subject_id=f.id
		             AND COALESCE(u.metadata->>'retrieval_mode','exact')
		                 IN ('exact','search','temporal')),0)::bigint
		         ELSE f.deleted_usage_count END
		FROM facts f JOIN fact_subjects s ON s.id=f.subject_id
		WHERE f.account_id=$1 AND f.realm_id=$2 AND f.owner_agent_id=$3`
	args := []any{p.AccountID, p.RealmID, p.ID}
	if factID != "" {
		query += ` AND f.id=$4 FOR UPDATE OF f`
		args = append(args, factID)
	} else {
		query += ` AND (s.canonical_key=$4 OR s.aliases ? $4) AND f.predicate=$5
		           ORDER BY (s.canonical_key=$4) DESC, s.canonical_key,
		                    f.deleted_at ASC NULLS FIRST, f.id
		           LIMIT 1 FOR UPDATE OF f`
		args = append(args, subject, predicate)
	}
	err := q.QueryRow(ctx, query, args...).Scan(
		&out.FactID, &out.ReceiptID, &out.SubjectID, &out.Subject, &out.Predicate,
		&out.PriorResolvedAssertionID, &out.CandidateRevision, &out.Sensitive, &deletedAt,
		&out.AssertionCount, &out.CandidateCount, &out.UsageCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeleteFactResult{}, false, ErrFactNotFound
	}
	if err != nil {
		return DeleteFactResult{}, false, err
	}
	if deletedAt != nil {
		out.DeletedAt = deletedAt
		out.Applied = true
		return out, true, nil
	}
	return out, false, nil
}

func replayDeleteFact(ctx context.Context, tx pgx.Tx, p Principal, factID, expectedAssertionID, expectedCandidateRevision, key string) (DeleteFactResult, bool, error) {
	hash := factIdempotencyKeyHash(key)
	var existingFactID string
	err := tx.QueryRow(ctx, `
		SELECT id FROM facts
		WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		  AND delete_idempotency_key_hash=$4`, p.AccountID, p.RealmID, p.ID,
		hash).Scan(&existingFactID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeleteFactResult{}, false, nil
	}
	if err != nil {
		return DeleteFactResult{}, false, err
	}
	if existingFactID != factID {
		return DeleteFactResult{}, false, ErrFactIdempotencyConflict
	}
	out, deleted, err := loadFactDeletionResult(ctx, tx, p, factID, "", "")
	if err != nil {
		return DeleteFactResult{}, false, err
	}
	if !deleted {
		return DeleteFactResult{}, false, ErrFactIdempotencyConflict
	}
	if out.PriorResolvedAssertionID != expectedAssertionID || out.CandidateRevision != expectedCandidateRevision {
		return DeleteFactResult{}, false, ErrFactIdempotencyConflict
	}
	out.Replayed = true
	return out, true, nil
}

// factCandidateRevision binds a value-free deletion preview to the exact
// candidate set and its mutable lifecycle state. Candidate ids are ordered and
// every field is length-prefixed. Candidate content, request fingerprints, and
// retry keys (including their hashes) are deliberately excluded, so the
// persisted revision carries no commitment derived from erased private data.
func factCandidateRevision(ctx context.Context, tx pgx.Tx, p Principal, out DeleteFactResult) (string, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.id, c.subject_key, c.predicate, c.status,
		       COALESCE(c.conflict_fact_id,''), COALESCE(c.observed_assertion_id,''),
		       COALESCE(c.resolved_fact_id,''), COALESCE(c.decision_assertion_id,''),
		       c.proposed_at, c.decided_at
		FROM fact_candidates c
		JOIN fact_subjects s ON s.id=$4
		WHERE c.account_id=$1 AND c.realm_id=$2 AND c.owner_agent_id=$3
		  AND (c.subject_key=$5 OR s.aliases ? c.subject_key)
		  AND c.predicate=$6
		ORDER BY c.id`, p.AccountID, p.RealmID, p.ID, out.SubjectID, out.Subject, out.Predicate)
	if err != nil {
		return "", fmt.Errorf("read fact candidate revision: %w", err)
	}
	defer rows.Close()

	digest := sha256.New()
	writeFactRevisionField(digest, "witself.fact-delete.candidates.v1")
	for rows.Next() {
		var candidateID, subject, predicate, status string
		var conflictFactID, observedAssertionID, resolvedFactID, decisionAssertionID string
		var proposedAt time.Time
		var decidedAt *time.Time
		if err := rows.Scan(&candidateID, &subject, &predicate, &status,
			&conflictFactID, &observedAssertionID, &resolvedFactID, &decisionAssertionID,
			&proposedAt, &decidedAt); err != nil {
			return "", err
		}
		for _, field := range []string{
			candidateID, subject, predicate, status,
			conflictFactID, observedAssertionID, resolvedFactID, decisionAssertionID,
			proposedAt.UTC().Format(time.RFC3339Nano),
		} {
			writeFactRevisionField(digest, field)
		}
		if decidedAt == nil {
			writeFactRevisionField(digest, "nil")
		} else {
			writeFactRevisionField(digest, decidedAt.UTC().Format(time.RFC3339Nano))
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeFactRevisionField(digest hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write([]byte(value))
}

func preserveDeletedFactMutationKeys(ctx context.Context, tx pgx.Tx, p Principal, out DeleteFactResult, deletedAt time.Time) (int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT 'set', idempotency_key FROM fact_assertions
		WHERE fact_id=$1 AND idempotency_key<>''
		UNION ALL
		SELECT 'proposal', idempotency_key FROM fact_candidates
		WHERE account_id=$2 AND realm_id=$3 AND owner_agent_id=$4
		  AND (subject_key=$5 OR EXISTS (
		    SELECT 1 FROM fact_subjects s WHERE s.id=$6 AND s.aliases ? fact_candidates.subject_key
		  )) AND predicate=$7 AND idempotency_key<>''`, out.FactID,
		p.AccountID, p.RealmID, p.ID, out.Subject, out.SubjectID, out.Predicate)
	if err != nil {
		return 0, fmt.Errorf("read deleted fact retry keys: %w", err)
	}
	defer rows.Close()
	type mutationKey struct{ surface, hash string }
	keys := make([]mutationKey, 0)
	seen := map[string]bool{}
	for rows.Next() {
		var surface, rawKey string
		if err := rows.Scan(&surface, &rawKey); err != nil {
			return 0, err
		}
		hash := factIdempotencyKeyHash(rawKey)
		identity := surface + "\x00" + hash
		if !seen[identity] {
			seen[identity] = true
			keys = append(keys, mutationKey{surface: surface, hash: hash})
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()
	for _, key := range keys {
		tombstoneID, err := id.New("fmt")
		if err != nil {
			return 0, err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO fact_mutation_tombstones
			  (id,account_id,realm_id,owner_agent_id,fact_id,surface,
			   idempotency_key_hash,deleted_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT(owner_agent_id,surface,idempotency_key_hash) DO NOTHING`,
			tombstoneID, p.AccountID, p.RealmID, p.ID, out.FactID, key.surface,
			key.hash, deletedAt)
		if err != nil {
			return 0, fmt.Errorf("preserve deleted fact retry key: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var existingFactID string
			if err := tx.QueryRow(ctx, `
				SELECT fact_id FROM fact_mutation_tombstones
				WHERE owner_agent_id=$1 AND surface=$2 AND idempotency_key_hash=$3`,
				p.ID, key.surface, key.hash).Scan(&existingFactID); err != nil {
				return 0, err
			}
			if existingFactID != out.FactID {
				return 0, ErrFactIdempotencyConflict
			}
		}
	}
	return int64(len(keys)), nil
}
