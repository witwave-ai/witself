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

	UsageDimensionFactReturned = "fact_returned"
	UsageUnitFact              = "fact"
)

var (
	// ErrFactNotFound reports an unknown fact in the token-bound collection.
	ErrFactNotFound = errors.New("fact not found")
	// ErrFactForbidden reports a principal that cannot access the fact surface.
	ErrFactForbidden = errors.New("fact access forbidden")
	// ErrFactInputInvalid reports caller-correctable fact content.
	ErrFactInputInvalid = errors.New("invalid fact input")

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
	SourceKind          string          `json:"source_kind"`
	SourceRef           string          `json:"source_ref,omitempty"`
	Confidence          float64         `json:"confidence"`
	ObservedAt          time.Time       `json:"observed_at"`
	ConfirmedAt         *time.Time      `json:"confirmed_at,omitempty"`
	ValidFrom           *time.Time      `json:"valid_from,omitempty"`
	ValidUntil          *time.Time      `json:"valid_until,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// FactAssertion is one immutable source claim in a fact's history.
type FactAssertion struct {
	ID           string          `json:"id"`
	FactID       string          `json:"fact_id"`
	ValueType    string          `json:"value_type"`
	Value        json.RawMessage `json:"value"`
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

// FactListOptions bounds deterministic inventory reads.
type FactListOptions struct {
	Subject          string
	PredicatePrefix  string
	Limit            int
	IncludeSensitive bool
}

// SetFact appends an immutable assertion and atomically resolves the fact to
// it. An existing assertion is retained and linked through supersedes_id.
func (s *Store) SetFact(ctx context.Context, p Principal, in SetFactInput) (Fact, error) {
	if p.Kind != PrincipalAgent {
		return Fact{}, ErrFactForbidden
	}
	in, err := normalizeSetFactInput(in)
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
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
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
		  (id, account_id, realm_id, owner_agent_id, subject_id, predicate, cardinality, sensitive)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (owner_agent_id, subject_id, predicate)
		DO UPDATE SET cardinality = EXCLUDED.cardinality,
		              sensitive = EXCLUDED.sensitive,
		              updated_at = now()
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
		   value, source_kind, source_ref, confidence, observed_at, confirmed_at,
		   valid_from, valid_until, supersedes_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13, $14, NULLIF($15, ''))`,
		assertionID, factID, p.AccountID, p.RealmID, p.ID, in.ValueType,
		string(in.Value), in.SourceKind, in.SourceRef, confidence, in.ObservedAt,
		in.ConfirmedAt, in.ValidFrom, in.ValidUntil, resolvedID)
	if err != nil {
		return Fact{}, fmt.Errorf("insert fact assertion: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE facts SET resolved_assertion_id = $1, updated_at = now()
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
	if !factSubjectPattern.MatchString(subject) || !validFactPredicate(predicate) {
		return Fact{}, ErrFactInputInvalid
	}
	out, err := getFactTx(ctx, s.pool, p, "", subject, predicate, true)
	if err != nil {
		return Fact{}, err
	}
	// Usage is a companion projection: a failed meter must not turn a valid
	// fact read into an unavailable fact service.
	_ = s.recordUsage(ctx, usageEventInput{
		AccountID: p.AccountID, RealmID: p.RealmID, AgentID: p.ID,
		Dimension: UsageDimensionFactReturned, Quantity: 1, Unit: UsageUnitFact,
		SubjectType: "fact", SubjectID: out.ID,
		IdempotencyKey: "fact_returned:" + out.ID + ":" + time.Now().UTC().Format(time.RFC3339Nano),
		OccurredAt:     time.Now().UTC(),
	})
	return out, nil
}

// ListFacts returns a stable inventory. Sensitive values are JSON null unless
// the caller explicitly requests them; exact GetFact remains an authorized read.
func (s *Store) ListFacts(ctx context.Context, p Principal, opts FactListOptions) ([]Fact, error) {
	if p.Kind != PrincipalAgent {
		return nil, ErrFactForbidden
	}
	if opts.Limit == 0 {
		opts.Limit = 100
	}
	if opts.Limit < 1 || opts.Limit > 500 {
		return nil, fmt.Errorf("%w: limit must be between 1 and 500", ErrFactInputInvalid)
	}
	args := []any{p.AccountID, p.RealmID, p.ID}
	where := []string{"f.account_id = $1", "f.realm_id = $2", "f.owner_agent_id = $3"}
	if opts.Subject != "" {
		opts.Subject = normalizeFactSubject(opts.Subject)
		if !factSubjectPattern.MatchString(opts.Subject) {
			return nil, ErrFactInputInvalid
		}
		args = append(args, opts.Subject)
		where = append(where, fmt.Sprintf("s.canonical_key = $%d", len(args)))
	}
	if opts.PredicatePrefix != "" {
		if !validFactPredicate(strings.TrimSuffix(opts.PredicatePrefix, "/")) {
			return nil, ErrFactInputInvalid
		}
		args = append(args, opts.PredicatePrefix+"%")
		where = append(where, fmt.Sprintf("f.predicate LIKE $%d", len(args)))
	}
	args = append(args, opts.Limit)
	query := factSelect + " WHERE " + strings.Join(where, " AND ") +
		fmt.Sprintf(" ORDER BY f.predicate, s.canonical_key, f.id LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Fact{}
	for rows.Next() {
		fact, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		if fact.Sensitive && !opts.IncludeSensitive {
			fact.Value = json.RawMessage(`null`)
		}
		out = append(out, fact)
	}
	return out, rows.Err()
}

// FactHistory returns source assertions newest first without changing usage.
func (s *Store) FactHistory(ctx context.Context, p Principal, factID string) ([]FactAssertion, error) {
	if p.Kind != PrincipalAgent {
		return nil, ErrFactForbidden
	}
	if _, err := getFactTx(ctx, s.pool, p, factID, "", "", true); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, fact_id, value_type, value, source_kind, source_ref, confidence,
		       observed_at, confirmed_at, valid_from, valid_until,
		       COALESCE(supersedes_id, ''), created_at
		FROM fact_assertions WHERE fact_id = $1 AND account_id = $2
		ORDER BY created_at DESC, id DESC`, factID, p.AccountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FactAssertion{}
	for rows.Next() {
		var a FactAssertion
		if err := rows.Scan(&a.ID, &a.FactID, &a.ValueType, &a.Value,
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
	       a.id, a.value_type, a.value, a.source_kind, a.source_ref,
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
		query += ` AND s.canonical_key = $4 AND f.predicate = $5`
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
		&out.Sensitive, &out.ResolvedAssertionID, &out.ValueType, &out.Value,
		&out.SourceKind, &out.SourceRef, &out.Confidence, &out.ObservedAt,
		&out.ConfirmedAt, &out.ValidFrom, &out.ValidUntil, &out.CreatedAt,
		&out.UpdatedAt)
	return out, err
}

func ensureFactSubject(ctx context.Context, tx pgx.Tx, p Principal, key string) (string, error) {
	subjectID, err := id.New("sub")
	if err != nil {
		return "", err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO fact_subjects
		  (id, account_id, realm_id, owner_agent_id, canonical_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (owner_agent_id, canonical_key)
		DO UPDATE SET canonical_key = EXCLUDED.canonical_key
		RETURNING id`, subjectID, p.AccountID, p.RealmID, p.ID, key).Scan(&subjectID)
	return subjectID, err
}

func normalizeSetFactInput(in SetFactInput) (SetFactInput, error) {
	in.Subject = normalizeFactSubject(in.Subject)
	in.Predicate = strings.TrimSpace(in.Predicate)
	in.ValueType = strings.TrimSpace(in.ValueType)
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
	subject = strings.TrimSpace(strings.ToLower(subject))
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
