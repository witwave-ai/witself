package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxFactIdempotencyKeyBytes = 512

// normalizeFactIdempotencyKey follows the same bounded retry-key contract as
// messaging. An empty key opts out; callers that need replay safety must reuse
// one non-empty key for exactly one logical mutation.
func normalizeFactIdempotencyKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if len(key) > maxFactIdempotencyKeyBytes {
		return "", ErrFactInputInvalid
	}
	return key, nil
}

// lockFactIdempotencyKey serializes concurrent first attempts for the same
// agent, mutation surface, and retry key. Unique indexes remain the durable
// backstop; the transaction-scoped advisory lock lets a waiter replay the row
// committed by the winner instead of surfacing a database uniqueness error.
func lockFactIdempotencyKey(ctx context.Context, tx pgx.Tx, p Principal, surface, key string) error {
	if key == "" {
		return nil
	}
	lockName := p.AccountID + "\x00" + p.RealmID + "\x00" + p.ID + "\x00" + surface + "\x00" + key
	sum := sha256.Sum256([]byte(lockName))
	lockID := int64(binary.BigEndian.Uint64(sum[:8]))
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockID)
	return err
}

// factSetFingerprint hashes normalized request semantics, not the persisted
// value in a second record. Omitted observed_at stays an explicit omission in
// the fingerprint so retries do not differ merely because "now" advanced.
func factSetFingerprint(in SetFactInput) (string, error) {
	return factSetFingerprintWithDefaultConfidence(in, 1.0)
}

func factSetFingerprintWithDefaultConfidence(in SetFactInput, defaultConfidence float64) (string, error) {
	observedAtProvided := !in.ObservedAt.IsZero()
	in.IdempotencyKey = ""
	normalized, err := normalizeSetFactInputAt(in, time.Unix(0, 0).UTC())
	if err != nil {
		return "", err
	}
	confidence := defaultConfidence
	if normalized.Confidence != nil {
		confidence = *normalized.Confidence
	}
	var observedAt *time.Time
	if observedAtProvided {
		value := normalized.ObservedAt.UTC()
		observedAt = &value
	}
	payload := struct {
		Subject     string          `json:"subject"`
		Predicate   string          `json:"predicate"`
		ValueType   string          `json:"value_type"`
		Value       json.RawMessage `json:"value"`
		Recurrence  string          `json:"recurrence"`
		Cardinality string          `json:"cardinality"`
		Sensitive   bool            `json:"sensitive"`
		SourceKind  string          `json:"source_kind"`
		SourceRef   string          `json:"source_ref"`
		Confidence  float64         `json:"confidence"`
		ObservedAt  *time.Time      `json:"observed_at,omitempty"`
		ConfirmedAt *time.Time      `json:"confirmed_at,omitempty"`
		ValidFrom   *time.Time      `json:"valid_from,omitempty"`
		ValidUntil  *time.Time      `json:"valid_until,omitempty"`
	}{
		Subject: normalized.Subject, Predicate: normalized.Predicate,
		ValueType: normalized.ValueType, Value: normalized.Value,
		Recurrence: normalized.Recurrence, Cardinality: normalized.Cardinality,
		Sensitive: normalized.Sensitive, SourceKind: normalized.SourceKind,
		SourceRef: normalized.SourceRef, Confidence: confidence,
		ObservedAt: observedAt, ConfirmedAt: normalized.ConfirmedAt,
		ValidFrom: normalized.ValidFrom, ValidUntil: normalized.ValidUntil,
	}
	return factMutationFingerprint(payload)
}

func factProposalFingerprint(in ProposeFactInput) (string, error) {
	setFingerprint, err := factSetFingerprintWithDefaultConfidence(in.SetFactInput, 0.5)
	if err != nil {
		return "", err
	}
	return factMutationFingerprint(struct {
		SetFingerprint string `json:"set_fingerprint"`
		Reason         string `json:"reason"`
	}{SetFingerprint: setFingerprint, Reason: in.Reason})
}

func factMutationFingerprint(payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", ErrFactInputInvalid
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func replaySetFact(ctx context.Context, tx pgx.Tx, p Principal, key, fingerprint string, in SetFactInput) (Fact, bool, error) {
	var assertionID, factID, existingFingerprint string
	err := tx.QueryRow(ctx, `
		SELECT id, fact_id, idempotency_fingerprint
		FROM fact_assertions
		WHERE account_id=$1 AND realm_id=$2 AND asserted_by_agent_id=$3
		  AND idempotency_key=$4`,
		p.AccountID, p.RealmID, p.ID, key).Scan(&assertionID, &factID, &existingFingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return Fact{}, false, nil
	}
	if err != nil {
		return Fact{}, false, err
	}
	if existingFingerprint != fingerprint {
		return Fact{}, false, ErrFactIdempotencyConflict
	}
	out, err := getFactAtAssertionTx(ctx, tx, p, factID, assertionID)
	if err == nil {
		out.Cardinality = in.Cardinality
		if out.Cardinality == "" {
			out.Cardinality = FactCardinalityOne
		}
	}
	return out, true, err
}

func replayProposeFact(ctx context.Context, tx pgx.Tx, p Principal, key, fingerprint string) (FactCandidate, bool, error) {
	var candidateID, existingFingerprint string
	err := tx.QueryRow(ctx, `
		SELECT id, idempotency_fingerprint
		FROM fact_candidates
		WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		  AND idempotency_key=$4`,
		p.AccountID, p.RealmID, p.ID, key).Scan(&candidateID, &existingFingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return FactCandidate{}, false, nil
	}
	if err != nil {
		return FactCandidate{}, false, err
	}
	if existingFingerprint != fingerprint {
		return FactCandidate{}, false, ErrFactIdempotencyConflict
	}
	out, err := getFactCandidateTx(ctx, tx, p, candidateID)
	return out, true, err
}

func ensureFactDecisionKeyAvailable(ctx context.Context, tx pgx.Tx, p Principal, candidateID, key string) error {
	if key == "" {
		return nil
	}
	var existingCandidateID string
	err := tx.QueryRow(ctx, `
		SELECT id FROM fact_candidates
		WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		  AND decision_idempotency_key=$4`,
		p.AccountID, p.RealmID, p.ID, key).Scan(&existingCandidateID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if existingCandidateID != candidateID {
		return ErrFactIdempotencyConflict
	}
	return nil
}
