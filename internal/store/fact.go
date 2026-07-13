package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// Fact cardinality, provenance, and usage constants form the first core contract.
const (
	FactCardinalityOne       = "one"
	FactCardinalityMany      = "many"
	FactCardinalityOneAtTime = "one_at_a_time"

	FactSourceSelf      = "self"
	FactSourceOperator  = "operator"
	FactSourceAgent     = "agent"
	FactSourceImport    = "import"
	FactSourceInference = "inference"

	FactRecurrenceAnnual = "annual"

	UsageDimensionFactReturned = "fact_returned"
	UsageUnitFact              = "fact"

	FactRetrievalModeExact         FactRetrievalMode = "exact"
	FactRetrievalModeSearch        FactRetrievalMode = "search"
	FactRetrievalModeSelfHydration FactRetrievalMode = "self_hydration"
	FactRetrievalModeTemporal      FactRetrievalMode = "temporal"
)

var (
	// ErrFactNotFound reports an unknown fact in the token-bound collection.
	ErrFactNotFound = errors.New("fact not found")
	// ErrFactForbidden reports a principal that cannot access the fact surface.
	ErrFactForbidden = errors.New("fact access forbidden")
	// ErrFactInputInvalid reports caller-correctable fact content.
	ErrFactInputInvalid = errors.New("invalid fact input")
	// ErrFactConflict reports a candidate whose reviewed canonical assertion
	// changed before promotion. The caller must review the new truth before
	// deciding whether to propose or confirm another candidate.
	ErrFactConflict = errors.New("fact changed since candidate proposal")

	factSubjectPattern   = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,254}$`)
	factPredicatePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]*(/[a-z0-9_.-]+){0,7}$`)
	factTypePattern      = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

// Fact is the stable, resolved subject/predicate identity. Value and
// provenance are copied from its currently resolved assertion.
type Fact struct {
	ID                  string          `json:"id"`
	AccountID           string          `json:"account_id"`
	RealmID             string          `json:"realm_id"`
	OwnerAgentID        string          `json:"owner_agent_id"`
	SubjectID           string          `json:"subject_id"`
	Subject             string          `json:"subject"`
	Predicate           string          `json:"predicate"`
	Cardinality         string          `json:"cardinality"`
	Sensitive           bool            `json:"sensitive"`
	ResolvedAssertionID string          `json:"resolved_assertion_id"`
	ValueType           string          `json:"value_type"`
	Value               json.RawMessage `json:"value"`
	Recurrence          string          `json:"recurrence,omitempty"`
	SourceKind          string          `json:"source_kind"`
	SourceRef           string          `json:"source_ref,omitempty"`
	Confidence          float64         `json:"confidence"`
	ObservedAt          time.Time       `json:"observed_at"`
	ConfirmedAt         *time.Time      `json:"confirmed_at,omitempty"`
	ValidFrom           *time.Time      `json:"valid_from,omitempty"`
	ValidUntil          *time.Time      `json:"valid_until,omitempty"`
	UsageCount          int64           `json:"usage_count"`
	LastUsedAt          *time.Time      `json:"last_used_at,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// FactAssertion is one immutable source claim in a fact's history.
type FactAssertion struct {
	ID           string          `json:"id"`
	FactID       string          `json:"fact_id"`
	ValueType    string          `json:"value_type"`
	Value        json.RawMessage `json:"value"`
	Recurrence   string          `json:"recurrence,omitempty"`
	SourceKind   string          `json:"source_kind"`
	SourceRef    string          `json:"source_ref,omitempty"`
	Confidence   float64         `json:"confidence"`
	ObservedAt   time.Time       `json:"observed_at"`
	ConfirmedAt  *time.Time      `json:"confirmed_at,omitempty"`
	ValidFrom    *time.Time      `json:"valid_from,omitempty"`
	ValidUntil   *time.Time      `json:"valid_until,omitempty"`
	SupersedesID string          `json:"supersedes_id,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// SetFactInput is the explicit assertion path. Subject defaults to self;
// source defaults to self and is always attributed to the token-bound agent.
type SetFactInput struct {
	Subject     string
	Predicate   string
	ValueType   string
	Value       json.RawMessage
	Recurrence  string
	Cardinality string
	Sensitive   bool
	SourceKind  string
	SourceRef   string
	Confidence  *float64
	ObservedAt  time.Time
	ConfirmedAt *time.Time
	ValidFrom   *time.Time
	ValidUntil  *time.Time
}

// FactRetrievalMode identifies the delivery path recorded in immutable fact
// usage events. Exact, search, and temporal deliveries are ranking-eligible;
// self hydration is audited separately so automatic context loading cannot
// reinforce its own ranking.
type FactRetrievalMode string

// FactListOptions bounds deterministic inventory reads.
type FactListOptions struct {
	Subject          string
	PredicatePrefix  string
	Limit            int
	IncludeSensitive bool
	OrderByUsage     bool
	UnusedOnly       bool
	RetrievalMode    FactRetrievalMode
}

// SetFact appends an immutable assertion and atomically resolves the fact to
// it. An existing assertion is retained and linked through supersedes_id.
func (s *Store) SetFact(ctx context.Context, p Principal, in SetFactInput) (Fact, error) {
	if p.Kind != PrincipalAgent {
		return Fact{}, ErrFactForbidden
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
	in.Subject, err = resolveFactSubjectCanonicalKey(ctx, tx, p, in.Subject)
	if err != nil {
		return Fact{}, err
	}
	in, err = normalizeSetFactInput(in)
	if err != nil {
		return Fact{}, err
	}

	subjectID, err := ensureFactSubject(ctx, tx, p, in.Subject)
	if err != nil {
		return Fact{}, err
	}
	factID, err := id.New("fact")
	if err != nil {
		return Fact{}, err
	}
	var resolvedID string
	err = tx.QueryRow(ctx, `
		INSERT INTO facts
		  (id, account_id, realm_id, owner_agent_id, subject_id, predicate,
		   cardinality, sensitive, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, clock_timestamp(), clock_timestamp())
		ON CONFLICT (owner_agent_id, subject_id, predicate)
		DO UPDATE SET cardinality = EXCLUDED.cardinality,
		              sensitive = facts.sensitive OR EXCLUDED.sensitive,
		              updated_at = clock_timestamp()
		RETURNING id, COALESCE(resolved_assertion_id, '')`,
		factID, p.AccountID, p.RealmID, p.ID, subjectID, in.Predicate,
		in.Cardinality, in.Sensitive).Scan(&factID, &resolvedID)
	if err != nil {
		return Fact{}, fmt.Errorf("upsert fact: %w", err)
	}

	assertionID, err := id.New("fas")
	if err != nil {
		return Fact{}, err
	}
	confidence := 1.0
	if in.Confidence != nil {
		confidence = *in.Confidence
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO fact_assertions
		  (id, fact_id, account_id, realm_id, asserted_by_agent_id, value_type,
		   value, recurrence, source_kind, source_ref, confidence, observed_at, confirmed_at,
		   valid_from, valid_until, supersedes_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13, $14, $15, NULLIF($16, ''), clock_timestamp())`,
		assertionID, factID, p.AccountID, p.RealmID, p.ID, in.ValueType,
		string(in.Value), in.Recurrence, in.SourceKind, in.SourceRef, confidence, in.ObservedAt,
		in.ConfirmedAt, in.ValidFrom, in.ValidUntil, resolvedID)
	if err != nil {
		return Fact{}, fmt.Errorf("insert fact assertion: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE facts SET resolved_assertion_id = $1, updated_at = clock_timestamp()
		WHERE id = $2`, assertionID, factID); err != nil {
		return Fact{}, fmt.Errorf("resolve fact assertion: %w", err)
	}

	out, err := getFactTx(ctx, tx, p, factID, "", "", true)
	if err != nil {
		return Fact{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Fact{}, err
	}
	return out, nil
}

// GetFact resolves an exact subject/predicate pair and records delivery usage.
func (s *Store) GetFact(ctx context.Context, p Principal, subject, predicate string) (Fact, error) {
	if p.Kind != PrincipalAgent {
		return Fact{}, ErrFactForbidden
	}
	subject = normalizeFactSubject(subject)
	if !validFactSubjectAlias(subject) || !validFactPredicate(predicate) {
		return Fact{}, ErrFactInputInvalid
	}
	out, err := getFactTx(ctx, s.pool, p, "", subject, predicate, true)
	if err != nil {
		return Fact{}, err
	}
	// Usage is a companion projection: a failed meter must not turn a valid
	// fact read into an unavailable fact service.
	_ = s.recordFactRetrievals(ctx, p, FactRetrievalModeExact, []Fact{out})
	return out, nil
}

// ListFacts returns a stable inventory. Sensitive values are JSON null unless
// the caller explicitly requests them; exact GetFact remains an authorized read.
func (s *Store) ListFacts(ctx context.Context, p Principal, opts FactListOptions) ([]Fact, error) {
	if p.Kind != PrincipalAgent {
		return nil, ErrFactForbidden
	}
	return s.listFactsWithUsage(ctx, p, opts)
}

// FactHistory returns source assertions newest first without changing usage.
func (s *Store) FactHistory(ctx context.Context, p Principal, factID string) ([]FactAssertion, error) {
	if p.Kind != PrincipalAgent {
		return nil, ErrFactForbidden
	}
	if _, err := getFactTx(ctx, s.pool, p, factID, "", "", true); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `WITH RECURSIVE history AS (
		SELECT a.*, 0 AS chain_depth
		FROM fact_assertions a
		JOIN facts f ON f.resolved_assertion_id = a.id
		WHERE f.id = $1 AND f.account_id = $2
		UNION ALL
		SELECT parent.*, child.chain_depth + 1
		FROM fact_assertions parent
		JOIN history child ON parent.id = child.supersedes_id
		WHERE parent.account_id = $2
	)
		SELECT id, fact_id, value_type, value, recurrence, source_kind, source_ref, confidence,
		       observed_at, confirmed_at, valid_from, valid_until,
		       COALESCE(supersedes_id, ''), created_at
		FROM history ORDER BY chain_depth`, factID, p.AccountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FactAssertion{}
	for rows.Next() {
		var a FactAssertion
		if err := rows.Scan(&a.ID, &a.FactID, &a.ValueType, &a.Value, &a.Recurrence,
			&a.SourceKind, &a.SourceRef, &a.Confidence, &a.ObservedAt,
			&a.ConfirmedAt, &a.ValidFrom, &a.ValidUntil, &a.SupersedesID,
			&a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const factSelect = `
	SELECT f.id, f.account_id, f.realm_id, f.owner_agent_id, f.subject_id,
	       s.canonical_key, f.predicate, f.cardinality, f.sensitive,
	       a.id, a.value_type, a.value, a.recurrence, a.source_kind, a.source_ref,
	       a.confidence, a.observed_at, a.confirmed_at, a.valid_from,
	       a.valid_until, f.created_at, f.updated_at
	FROM facts f
	JOIN fact_subjects s ON s.id = f.subject_id
	JOIN fact_assertions a ON a.id = f.resolved_assertion_id`

type factQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getFactTx(ctx context.Context, q factQuerier, p Principal, factID, subject, predicate string, includeSensitive bool) (Fact, error) {
	query := factSelect + ` WHERE f.account_id = $1 AND f.realm_id = $2 AND f.owner_agent_id = $3`
	args := []any{p.AccountID, p.RealmID, p.ID}
	if factID != "" {
		query += ` AND f.id = $4`
		args = append(args, factID)
	} else {
		query += ` AND (s.canonical_key = $4 OR s.aliases ? $4) AND f.predicate = $5
		           ORDER BY (s.canonical_key = $4) DESC, s.canonical_key, f.id LIMIT 1`
		args = append(args, subject, predicate)
	}
	out, err := scanFact(q.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Fact{}, ErrFactNotFound
	}
	if err != nil {
		return Fact{}, err
	}
	if out.Sensitive && !includeSensitive {
		out.Value = json.RawMessage(`null`)
	}
	return out, nil
}

type factScanner interface{ Scan(...any) error }

func scanFact(row factScanner) (Fact, error) {
	var out Fact
	err := row.Scan(&out.ID, &out.AccountID, &out.RealmID, &out.OwnerAgentID,
		&out.SubjectID, &out.Subject, &out.Predicate, &out.Cardinality,
		&out.Sensitive, &out.ResolvedAssertionID, &out.ValueType, &out.Value, &out.Recurrence,
		&out.SourceKind, &out.SourceRef, &out.Confidence, &out.ObservedAt,
		&out.ConfirmedAt, &out.ValidFrom, &out.ValidUntil, &out.CreatedAt,
		&out.UpdatedAt)
	return out, err
}

func ensureFactSubject(ctx context.Context, tx pgx.Tx, p Principal, key string) (string, error) {
	var existingID string
	err := tx.QueryRow(ctx, `
		SELECT id FROM fact_subjects
		WHERE account_id = $1 AND realm_id = $2 AND owner_agent_id = $3
		  AND (canonical_key = $4 OR aliases ? $4)
		ORDER BY (canonical_key = $4) DESC, canonical_key
		LIMIT 1`, p.AccountID, p.RealmID, p.ID, key).Scan(&existingID)
	if err == nil {
		return existingID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	subjectID, err := id.New("sub")
	if err != nil {
		return "", err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO fact_subjects
		  (id, account_id, realm_id, owner_agent_id, canonical_key, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, clock_timestamp(), clock_timestamp())
		ON CONFLICT (owner_agent_id, canonical_key)
		DO UPDATE SET canonical_key = EXCLUDED.canonical_key
		RETURNING id`, subjectID, p.AccountID, p.RealmID, p.ID, key).Scan(&subjectID)
	return subjectID, err
}

func normalizeSetFactInput(in SetFactInput) (SetFactInput, error) {
	in.Subject = normalizeFactSubject(in.Subject)
	in.Predicate = strings.TrimSpace(in.Predicate)
	in.ValueType = strings.TrimSpace(in.ValueType)
	in.Recurrence = strings.TrimSpace(in.Recurrence)
	if in.ValueType == "" {
		in.ValueType = inferFactValueType(in.Value)
	}
	if in.Cardinality == "" {
		in.Cardinality = FactCardinalityOne
	}
	if in.SourceKind == "" {
		in.SourceKind = FactSourceSelf
	}
	if in.ObservedAt.IsZero() {
		in.ObservedAt = time.Now().UTC()
	} else {
		in.ObservedAt = in.ObservedAt.UTC()
	}
	if !factSubjectPattern.MatchString(in.Subject) || !validFactPredicate(in.Predicate) ||
		!factTypePattern.MatchString(in.ValueType) || !json.Valid(in.Value) || len(in.Value) > 65536 {
		return SetFactInput{}, ErrFactInputInvalid
	}
	value, err := normalizeFactValue(in.ValueType, in.Value)
	if err != nil {
		return SetFactInput{}, err
	}
	if len(value) > 65536 {
		return SetFactInput{}, ErrFactInputInvalid
	}
	in.Value = value
	if in.Recurrence != "" && (in.Recurrence != FactRecurrenceAnnual || in.ValueType != "date") {
		return SetFactInput{}, ErrFactInputInvalid
	}
	if in.Cardinality != FactCardinalityOne && in.Cardinality != FactCardinalityMany && in.Cardinality != FactCardinalityOneAtTime {
		return SetFactInput{}, ErrFactInputInvalid
	}
	if in.SourceKind != FactSourceSelf && in.SourceKind != FactSourceOperator &&
		in.SourceKind != FactSourceAgent && in.SourceKind != FactSourceImport &&
		in.SourceKind != FactSourceInference {
		return SetFactInput{}, ErrFactInputInvalid
	}
	if len(in.SourceRef) > 1024 || (in.Confidence != nil && (*in.Confidence < 0 || *in.Confidence > 1)) {
		return SetFactInput{}, ErrFactInputInvalid
	}
	if in.ValidFrom != nil && in.ValidUntil != nil && in.ValidUntil.Before(*in.ValidFrom) {
		return SetFactInput{}, ErrFactInputInvalid
	}
	return in, nil
}

func normalizeFactSubject(subject string) string {
	subject = normalizeFactSubjectAlias(subject)
	if subject == "" || subject == "me" || subject == "myself" || subject == "user" {
		return "self"
	}
	return subject
}

func validFactPredicate(predicate string) bool {
	return len(predicate) <= 255 && factPredicatePattern.MatchString(predicate)
}

func inferFactValueType(value json.RawMessage) string {
	var v any
	if json.Unmarshal(value, &v) != nil {
		return ""
	}
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "list"
	case map[string]any:
		return "object"
	default:
		return "json"
	}
}
