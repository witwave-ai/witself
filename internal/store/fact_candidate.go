package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// FactCandidate is an agent-observed assertion awaiting an explicit decision.
type FactCandidate struct {
	ID             string          `json:"id"`
	Subject        string          `json:"subject"`
	Predicate      string          `json:"predicate"`
	ValueType      string          `json:"value_type"`
	Value          json.RawMessage `json:"value"`
	Recurrence     string          `json:"recurrence,omitempty"`
	Cardinality    string          `json:"cardinality"`
	Sensitive      bool            `json:"sensitive"`
	SourceRef      string          `json:"source_ref,omitempty"`
	Confidence     float64         `json:"confidence"`
	ObservedAt     time.Time       `json:"observed_at"`
	ValidFrom      *time.Time      `json:"valid_from,omitempty"`
	ValidUntil     *time.Time      `json:"valid_until,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	Status         string          `json:"status"`
	ConflictFactID string          `json:"conflict_fact_id,omitempty"`
	// ObservedAssertionID pins the exact canonical assertion that was visible
	// when this candidate was proposed. Confirmation refuses if that pointer
	// has since changed, including from/to no resolved fact.
	ObservedAssertionID string     `json:"observed_assertion_id,omitempty"`
	ResolvedFactID      string     `json:"resolved_fact_id,omitempty"`
	ProposedAt          time.Time  `json:"proposed_at"`
	DecidedAt           *time.Time `json:"decided_at,omitempty"`
}

// ProposeFactInput carries an uncertain or agent-discovered durable assertion.
type ProposeFactInput struct {
	SetFactInput
	Reason string
}

// FactCandidateListOptions selects a bounded candidate review inventory.
type FactCandidateListOptions struct {
	Status string
	Limit  int
}

const (
	defaultFactCandidateListLimit = 100
	maxFactCandidateListLimit     = 500
)

// ProposeFact stores a pending candidate and marks it conflict when a different
// resolved value already occupies the exact subject/predicate address.
func (s *Store) ProposeFact(ctx context.Context, p Principal, proposal ProposeFactInput) (FactCandidate, error) {
	if p.Kind != PrincipalAgent {
		return FactCandidate{}, ErrFactForbidden
	}
	if len(proposal.Reason) > 1024 {
		return FactCandidate{}, ErrFactInputInvalid
	}
	if proposal.RecreateDeleted {
		return FactCandidate{}, fmt.Errorf("%w: proposals cannot recreate deleted facts", ErrFactInputInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FactCandidate{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return FactCandidate{}, err
	}
	if err := lockFactSubjectNamespace(ctx, tx, p, false); err != nil {
		return FactCandidate{}, err
	}
	proposal.IdempotencyKey, err = normalizeFactIdempotencyKey(proposal.IdempotencyKey)
	if err != nil {
		return FactCandidate{}, err
	}
	if err := lockFactIdempotencyKey(ctx, tx, p, "proposal", proposal.IdempotencyKey); err != nil {
		return FactCandidate{}, err
	}
	unresolvedSubject := normalizeFactSubject(proposal.Subject)
	proposal.Subject, err = resolveFactSubjectCanonicalKey(ctx, tx, p, proposal.Subject)
	if err != nil {
		return FactCandidate{}, err
	}
	fingerprint := ""
	if proposal.IdempotencyKey != "" {
		fingerprint, err = factProposalFingerprint(proposal)
		if err != nil {
			return FactCandidate{}, err
		}
		acceptedFingerprints := []string{fingerprint}
		if unresolvedSubject != proposal.Subject && factSubjectPattern.MatchString(unresolvedSubject) {
			unresolvedProposal := proposal
			unresolvedProposal.Subject = unresolvedSubject
			unresolvedFingerprint, fingerprintErr := factProposalFingerprint(unresolvedProposal)
			if fingerprintErr != nil {
				return FactCandidate{}, fingerprintErr
			}
			acceptedFingerprints = append(acceptedFingerprints, unresolvedFingerprint)
		}
		out, replayed, replayErr := replayProposeFact(ctx, tx, p, proposal.IdempotencyKey, acceptedFingerprints...)
		if replayErr != nil {
			return FactCandidate{}, replayErr
		}
		if replayed {
			if err := tx.Commit(ctx); err != nil {
				return FactCandidate{}, err
			}
			return out, nil
		}
	}
	in, err := normalizeSetFactInput(proposal.SetFactInput)
	if err != nil {
		return FactCandidate{}, err
	}
	if in.Confidence == nil {
		confidence := 0.5
		in.Confidence = &confidence
	}
	deleted, err := factAddressHasUnrecreatedTombstone(ctx, tx, p, in.Subject, in.Predicate)
	if err != nil {
		return FactCandidate{}, err
	}
	if deleted {
		return FactCandidate{}, ErrFactDeleted
	}

	status, conflictID, observedAssertionID := "pending", "", ""
	var currentFactID string
	var currentSensitive bool
	var sameValue bool
	err = tx.QueryRow(ctx, `
		SELECT f.id, a.id,
		       a.value = $6::jsonb AND a.value_type = $7 AND a.recurrence = $8,
		       f.sensitive
		FROM facts f JOIN fact_subjects s ON s.id = f.subject_id
		JOIN fact_assertions a ON a.id = f.resolved_assertion_id
		WHERE f.account_id = $1 AND f.realm_id = $2 AND f.owner_agent_id = $3
		  AND s.canonical_key = $4 AND f.predicate = $5 AND f.deleted_at IS NULL`,
		p.AccountID, p.RealmID, p.ID, in.Subject, in.Predicate, string(in.Value),
		in.ValueType, in.Recurrence).Scan(
		&currentFactID, &observedAssertionID, &sameValue, &currentSensitive)
	if err == nil {
		in.Sensitive = in.Sensitive || currentSensitive
		if !sameValue {
			status, conflictID = "conflict", currentFactID
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return FactCandidate{}, err
	}

	candidateID, err := id.New("fcand")
	if err != nil {
		return FactCandidate{}, err
	}
	var out FactCandidate
	err = tx.QueryRow(ctx, `
		INSERT INTO fact_candidates
		  (id, account_id, realm_id, owner_agent_id, subject_key, predicate,
		   value_type, value, recurrence, cardinality, sensitive, source_ref, confidence,
		   observed_at, valid_from, valid_until, reason, status, conflict_fact_id,
		   observed_assertion_id, idempotency_key, idempotency_fingerprint, proposed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,NULLIF($19,''),NULLIF($20,''),$21,$22,clock_timestamp())
		RETURNING id, subject_key, predicate, value_type, value, recurrence, cardinality,
		          sensitive, source_ref, confidence, observed_at, valid_from,
		          valid_until, reason, status,
		          COALESCE(conflict_fact_id,''), COALESCE(observed_assertion_id,''),
		          COALESCE(resolved_fact_id,''),
		          proposed_at, decided_at`,
		candidateID, p.AccountID, p.RealmID, p.ID, in.Subject, in.Predicate,
		in.ValueType, string(in.Value), in.Recurrence, in.Cardinality, in.Sensitive, in.SourceRef,
		*in.Confidence, in.ObservedAt, in.ValidFrom, in.ValidUntil, proposal.Reason,
		status, conflictID, observedAssertionID, proposal.IdempotencyKey, fingerprint).Scan(
		&out.ID, &out.Subject, &out.Predicate, &out.ValueType, &out.Value, &out.Recurrence,
		&out.Cardinality, &out.Sensitive, &out.SourceRef, &out.Confidence,
		&out.ObservedAt, &out.ValidFrom, &out.ValidUntil, &out.Reason, &out.Status,
		&out.ConflictFactID, &out.ObservedAssertionID, &out.ResolvedFactID,
		&out.ProposedAt, &out.DecidedAt)
	if err != nil {
		return FactCandidate{}, fmt.Errorf("insert fact candidate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return FactCandidate{}, err
	}
	return out, nil
}

// GetFactCandidate retrieves one candidate owned by the authenticated agent.
// Unlike broad review lists, the detail boundary returns a sensitive value so
// the agent can make an explicit decision about this one candidate.
func (s *Store) GetFactCandidate(ctx context.Context, p Principal, candidateID string) (FactCandidate, error) {
	if p.Kind != PrincipalAgent {
		return FactCandidate{}, ErrFactForbidden
	}
	return getFactCandidateTx(ctx, s.pool, p, candidateID)
}

func getFactCandidateTx(ctx context.Context, q factQuerier, p Principal, candidateID string) (FactCandidate, error) {
	var out FactCandidate
	err := q.QueryRow(ctx, `SELECT id, subject_key, predicate, value_type,
		value, recurrence, cardinality, sensitive, source_ref, confidence, observed_at,
		valid_from, valid_until, reason, status,
		COALESCE(conflict_fact_id,''), COALESCE(observed_assertion_id,''),
		COALESCE(resolved_fact_id,''), proposed_at, decided_at
		FROM fact_candidates
		WHERE id=$1 AND account_id=$2 AND realm_id=$3 AND owner_agent_id=$4`,
		candidateID, p.AccountID, p.RealmID, p.ID).Scan(
		&out.ID, &out.Subject, &out.Predicate, &out.ValueType, &out.Value, &out.Recurrence,
		&out.Cardinality, &out.Sensitive, &out.SourceRef, &out.Confidence,
		&out.ObservedAt, &out.ValidFrom, &out.ValidUntil, &out.Reason, &out.Status,
		&out.ConflictFactID, &out.ObservedAssertionID, &out.ResolvedFactID,
		&out.ProposedAt, &out.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return FactCandidate{}, ErrFactNotFound
	}
	return out, err
}

// ListFactCandidates returns pending/conflicting candidates newest first using
// the default bounded review size. Sensitive values are always redacted.
func (s *Store) ListFactCandidates(ctx context.Context, p Principal, status string) ([]FactCandidate, error) {
	return s.ListFactCandidatesWithOptions(ctx, p, FactCandidateListOptions{Status: status})
}

// ListFactCandidatesWithOptions returns a bounded, redacted candidate review
// inventory. Single-candidate detail is the only read path for raw sensitive
// candidate values.
func (s *Store) ListFactCandidatesWithOptions(ctx context.Context, p Principal, opts FactCandidateListOptions) ([]FactCandidate, error) {
	if p.Kind != PrincipalAgent {
		return nil, ErrFactForbidden
	}
	var err error
	opts, err = normalizeFactCandidateListOptions(opts)
	if err != nil {
		return nil, err
	}
	status := opts.Status
	if status != "open" && status != "pending" && status != "conflict" && status != "confirmed" && status != "rejected" {
		return nil, ErrFactInputInvalid
	}
	where := `account_id=$1 AND realm_id=$2 AND owner_agent_id=$3`
	if status == "open" {
		where += ` AND status IN ('pending','conflict')`
	} else {
		where += ` AND status=$4`
	}
	args := []any{p.AccountID, p.RealmID, p.ID}
	if status != "open" {
		args = append(args, status)
	}
	limitPlaceholder := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, opts.Limit)
	rows, err := s.pool.Query(ctx, `SELECT id, subject_key, predicate, value_type,
		value, recurrence, cardinality, sensitive, source_ref, confidence, observed_at,
		valid_from, valid_until, reason, status,
		COALESCE(conflict_fact_id,''), COALESCE(observed_assertion_id,''),
		COALESCE(resolved_fact_id,''), proposed_at, decided_at
		FROM fact_candidates WHERE `+where+`
		ORDER BY proposed_at DESC, id LIMIT `+limitPlaceholder, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FactCandidate{}
	for rows.Next() {
		var c FactCandidate
		if err := rows.Scan(&c.ID, &c.Subject, &c.Predicate, &c.ValueType, &c.Value, &c.Recurrence,
			&c.Cardinality, &c.Sensitive, &c.SourceRef, &c.Confidence, &c.ObservedAt,
			&c.ValidFrom, &c.ValidUntil, &c.Reason, &c.Status, &c.ConflictFactID,
			&c.ObservedAssertionID, &c.ResolvedFactID, &c.ProposedAt, &c.DecidedAt); err != nil {
			return nil, err
		}
		if c.Sensitive {
			c.Value = json.RawMessage(`null`)
			c.SourceRef = ""
			c.Reason = ""
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func normalizeFactCandidateListOptions(opts FactCandidateListOptions) (FactCandidateListOptions, error) {
	if opts.Status == "" {
		opts.Status = "open"
	}
	if opts.Status != "open" && opts.Status != "pending" && opts.Status != "conflict" && opts.Status != "confirmed" && opts.Status != "rejected" {
		return FactCandidateListOptions{}, ErrFactInputInvalid
	}
	if opts.Limit == 0 {
		opts.Limit = defaultFactCandidateListLimit
	}
	if opts.Limit < 1 || opts.Limit > maxFactCandidateListLimit {
		return FactCandidateListOptions{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrFactInputInvalid, maxFactCandidateListLimit)
	}
	return opts, nil
}

// RejectFactCandidate closes one candidate without changing canonical facts.
func (s *Store) RejectFactCandidate(ctx context.Context, p Principal, candidateID string) (FactCandidate, error) {
	return s.RejectFactCandidateIdempotent(ctx, p, candidateID, "")
}

// RejectFactCandidateIdempotent closes one candidate and replays the result
// when the same decision key is retried after a lost response.
func (s *Store) RejectFactCandidateIdempotent(ctx context.Context, p Principal, candidateID, idempotencyKey string) (FactCandidate, error) {
	if p.Kind != PrincipalAgent {
		return FactCandidate{}, ErrFactForbidden
	}
	var err error
	idempotencyKey, err = normalizeFactIdempotencyKey(idempotencyKey)
	if err != nil {
		return FactCandidate{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FactCandidate{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return FactCandidate{}, err
	}
	if err := lockFactSubjectNamespace(ctx, tx, p, false); err != nil {
		return FactCandidate{}, err
	}
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return FactCandidate{}, err
	}
	if err := lockFactIdempotencyKey(ctx, tx, p, "decision", idempotencyKey); err != nil {
		return FactCandidate{}, err
	}
	var out FactCandidate
	var existingDecisionKey string
	err = tx.QueryRow(ctx, `SELECT id, subject_key, predicate, value_type, value,
		recurrence, cardinality, sensitive, source_ref, confidence, observed_at,
		valid_from, valid_until, reason, status,
		COALESCE(conflict_fact_id,''), COALESCE(observed_assertion_id,''),
		COALESCE(resolved_fact_id,''), proposed_at, decided_at,
		decision_idempotency_key
		FROM fact_candidates
		WHERE id=$1 AND account_id=$2 AND realm_id=$3 AND owner_agent_id=$4
		FOR UPDATE`, candidateID, p.AccountID, p.RealmID, p.ID).Scan(
		&out.ID, &out.Subject, &out.Predicate, &out.ValueType, &out.Value,
		&out.Recurrence, &out.Cardinality, &out.Sensitive, &out.SourceRef,
		&out.Confidence, &out.ObservedAt, &out.ValidFrom, &out.ValidUntil,
		&out.Reason, &out.Status, &out.ConflictFactID, &out.ObservedAssertionID,
		&out.ResolvedFactID, &out.ProposedAt, &out.DecidedAt, &existingDecisionKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return FactCandidate{}, ErrFactNotFound
	}
	if err != nil {
		return FactCandidate{}, err
	}
	if err := ensureFactDecisionKeyAvailable(ctx, tx, p, candidateID, idempotencyKey); err != nil {
		return FactCandidate{}, err
	}
	if out.Status == "rejected" && idempotencyKey != "" && existingDecisionKey == idempotencyKey {
		if err := tx.Commit(ctx); err != nil {
			return FactCandidate{}, err
		}
		return out, nil
	}
	if out.Status != "pending" && out.Status != "conflict" {
		if idempotencyKey != "" {
			return FactCandidate{}, ErrFactIdempotencyConflict
		}
		return FactCandidate{}, ErrFactNotFound
	}
	err = tx.QueryRow(ctx, `UPDATE fact_candidates
		SET status='rejected', decided_at=clock_timestamp(), decision_idempotency_key=$5
		WHERE id=$1 AND account_id=$2 AND realm_id=$3 AND owner_agent_id=$4
		RETURNING id, subject_key, predicate, value_type, value, recurrence, cardinality,
		sensitive, source_ref, confidence, observed_at, valid_from, valid_until,
		reason, status,
		COALESCE(conflict_fact_id,''), COALESCE(observed_assertion_id,''),
		COALESCE(resolved_fact_id,''), proposed_at, decided_at`,
		candidateID, p.AccountID, p.RealmID, p.ID, idempotencyKey).Scan(&out.ID, &out.Subject,
		&out.Predicate, &out.ValueType, &out.Value, &out.Recurrence, &out.Cardinality, &out.Sensitive,
		&out.SourceRef, &out.Confidence, &out.ObservedAt, &out.ValidFrom,
		&out.ValidUntil, &out.Reason, &out.Status, &out.ConflictFactID,
		&out.ObservedAssertionID, &out.ResolvedFactID, &out.ProposedAt, &out.DecidedAt)
	if err != nil {
		return FactCandidate{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FactCandidate{}, err
	}
	return out, nil
}

// ConfirmFactCandidate atomically promotes a candidate through the canonical
// SetFact path and closes the candidate. The row lock prevents double decisions.
func (s *Store) ConfirmFactCandidate(ctx context.Context, p Principal, candidateID string) (Fact, error) {
	return s.ConfirmFactCandidateIdempotent(ctx, p, candidateID, "")
}

// ConfirmFactCandidateIdempotent promotes one candidate and replays the
// resulting fact when the same decision key is retried.
func (s *Store) ConfirmFactCandidateIdempotent(ctx context.Context, p Principal, candidateID, idempotencyKey string) (Fact, error) {
	if p.Kind != PrincipalAgent {
		return Fact{}, ErrFactForbidden
	}
	var err error
	idempotencyKey, err = normalizeFactIdempotencyKey(idempotencyKey)
	if err != nil {
		return Fact{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Fact{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return Fact{}, err
	}
	if err := lockFactSubjectNamespace(ctx, tx, p, false); err != nil {
		return Fact{}, err
	}
	if err := lockFactIdempotencyKey(ctx, tx, p, "decision", idempotencyKey); err != nil {
		return Fact{}, err
	}
	var c FactCandidate
	var existingDecisionKey, decisionAssertionID string
	err = tx.QueryRow(ctx, `SELECT id, subject_key, predicate, value_type, value,
		recurrence, cardinality, sensitive, source_ref, confidence, observed_at, valid_from,
		valid_until, reason, status,
		COALESCE(conflict_fact_id,''), COALESCE(observed_assertion_id,''),
		COALESCE(resolved_fact_id,''), proposed_at, decided_at,
		decision_idempotency_key, COALESCE(decision_assertion_id,'')
		FROM fact_candidates WHERE id=$1 AND account_id=$2 AND realm_id=$3
		AND owner_agent_id=$4 FOR UPDATE`,
		candidateID, p.AccountID, p.RealmID, p.ID).Scan(&c.ID, &c.Subject, &c.Predicate,
		&c.ValueType, &c.Value, &c.Recurrence, &c.Cardinality, &c.Sensitive, &c.SourceRef, &c.Confidence,
		&c.ObservedAt, &c.ValidFrom, &c.ValidUntil, &c.Reason, &c.Status,
		&c.ConflictFactID, &c.ObservedAssertionID, &c.ResolvedFactID,
		&c.ProposedAt, &c.DecidedAt, &existingDecisionKey, &decisionAssertionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Fact{}, ErrFactNotFound
	}
	if err != nil {
		return Fact{}, err
	}
	if err := ensureFactDecisionKeyAvailable(ctx, tx, p, candidateID, idempotencyKey); err != nil {
		return Fact{}, err
	}
	if c.Status == "confirmed" && idempotencyKey != "" && existingDecisionKey == idempotencyKey {
		out, replayErr := getFactAtAssertionTx(ctx, tx, p, c.ResolvedFactID, decisionAssertionID)
		if replayErr != nil {
			return Fact{}, replayErr
		}
		out.Cardinality = c.Cardinality
		if err := tx.Commit(ctx); err != nil {
			return Fact{}, err
		}
		return out, nil
	}
	if c.Status != "pending" && c.Status != "conflict" {
		if idempotencyKey != "" {
			return Fact{}, ErrFactIdempotencyConflict
		}
		return Fact{}, ErrFactNotFound
	}
	c.Subject, err = resolveFactSubjectCanonicalKey(ctx, tx, p, c.Subject)
	if err != nil {
		return Fact{}, err
	}
	confidence := c.Confidence
	normalized, err := normalizeSetFactInput(SetFactInput{
		Subject: c.Subject, Predicate: c.Predicate, ValueType: c.ValueType,
		Value: c.Value, Recurrence: c.Recurrence, Cardinality: c.Cardinality, Sensitive: c.Sensitive,
		SourceKind: FactSourceInference, SourceRef: c.SourceRef,
		Confidence: &confidence, ObservedAt: c.ObservedAt,
		ValidFrom: c.ValidFrom, ValidUntil: c.ValidUntil,
	})
	if err != nil {
		return Fact{}, err
	}
	c.Subject, c.Predicate, c.ValueType, c.Value, c.Recurrence = normalized.Subject, normalized.Predicate, normalized.ValueType, normalized.Value, normalized.Recurrence
	c.Cardinality, c.Sensitive, c.SourceRef = normalized.Cardinality, normalized.Sensitive, normalized.SourceRef
	c.Confidence, c.ObservedAt = *normalized.Confidence, normalized.ObservedAt
	c.ValidFrom, c.ValidUntil = normalized.ValidFrom, normalized.ValidUntil

	subjectID, err := ensureFactSubject(ctx, tx, p, c.Subject)
	if err != nil {
		return Fact{}, err
	}
	deleted, err := factAddressHasUnrecreatedTombstone(ctx, tx, p, c.Subject, c.Predicate)
	if err != nil {
		return Fact{}, err
	}
	if deleted {
		return Fact{}, ErrFactDeleted
	}
	factID, err := id.New("fact")
	if err != nil {
		return Fact{}, err
	}
	var prior string
	err = tx.QueryRow(ctx, `INSERT INTO facts
		(id,account_id,realm_id,owner_agent_id,subject_id,predicate,cardinality,sensitive,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp(),clock_timestamp())
		ON CONFLICT(owner_agent_id,subject_id,predicate) WHERE deleted_at IS NULL DO UPDATE SET
		cardinality=EXCLUDED.cardinality,
		sensitive=facts.sensitive OR EXCLUDED.sensitive,updated_at=clock_timestamp()
		RETURNING id,COALESCE(resolved_assertion_id,'')`, factID, p.AccountID, p.RealmID,
		p.ID, subjectID, c.Predicate, c.Cardinality, c.Sensitive).Scan(&factID, &prior)
	if err != nil {
		return Fact{}, err
	}
	if prior != c.ObservedAssertionID {
		return Fact{}, ErrFactConflict
	}
	assertionID, err := id.New("fas")
	if err != nil {
		return Fact{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO fact_assertions
		(id,fact_id,account_id,realm_id,asserted_by_agent_id,value_type,value,recurrence,
		source_kind,source_ref,confidence,observed_at,confirmed_at,valid_from,
		valid_until,supersedes_id,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,'inference',$9,$10,$11,$12,$13,$14,NULLIF($15,''),clock_timestamp())`,
		assertionID, factID, p.AccountID, p.RealmID, p.ID, c.ValueType, string(c.Value),
		c.Recurrence, c.SourceRef, c.Confidence, c.ObservedAt, time.Now().UTC(), c.ValidFrom,
		c.ValidUntil, prior)
	if err != nil {
		return Fact{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE facts SET resolved_assertion_id=$1,updated_at=clock_timestamp() WHERE id=$2`, assertionID, factID); err != nil {
		return Fact{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE fact_candidates
		SET status='confirmed', resolved_fact_id=$1, decided_at=clock_timestamp(),
		    decision_idempotency_key=$3,
		    decision_assertion_id=CASE WHEN $3='' THEN NULL ELSE $4 END
		WHERE id=$2`, factID, candidateID, idempotencyKey, assertionID); err != nil {
		return Fact{}, err
	}
	out, err := getFactTx(ctx, tx, p, factID, "", "", true)
	if err != nil {
		return Fact{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Fact{}, err
	}
	return out, nil
}

func jsonValuesEqual(left, right json.RawMessage) bool {
	var l, r any
	if json.Unmarshal(left, &l) != nil || json.Unmarshal(right, &r) != nil {
		return false
	}
	return reflect.DeepEqual(l, r)
}
