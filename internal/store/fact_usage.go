package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/id"
)

// factUsageSelect projects ranking-eligible delivery usage from the immutable
// usage ledger. It deliberately excludes automatic self hydration and does not
// update facts: browsing and ranking are read-only views.
const factUsageSelect = `
	SELECT f.id, f.account_id, f.realm_id, f.owner_agent_id, f.subject_id,
	       s.canonical_key, f.predicate, f.cardinality, f.sensitive,
	       a.id, a.value_type, a.value, a.recurrence, a.source_kind, a.source_ref,
	       a.confidence, a.observed_at, a.confirmed_at, a.valid_from,
	       a.valid_until, f.created_at, f.updated_at,
	       COALESCE(u.usage_count, 0)::bigint AS usage_count, u.last_used_at
	FROM facts f
	JOIN fact_subjects s ON s.id = f.subject_id
	JOIN fact_assertions a ON a.id = f.resolved_assertion_id
	LEFT JOIN (
		SELECT subject_id AS fact_id, SUM(quantity) AS usage_count,
		       MAX(occurred_at) AS last_used_at
		FROM usage_events
		WHERE account_id = $1 AND realm_id = $2 AND agent_id = $3
		  AND dimension = 'fact_returned' AND unit = 'fact'
		  AND subject_type = 'fact'
		  AND COALESCE(metadata->>'retrieval_mode', 'exact') IN ('exact', 'search', 'temporal')
		GROUP BY subject_id
	) u ON u.fact_id = f.id`

func (s *Store) listFactsWithUsage(ctx context.Context, p Principal, opts FactListOptions) ([]Fact, error) {
	opts, err := normalizeFactListOptions(opts)
	if err != nil {
		return nil, err
	}

	args := []any{p.AccountID, p.RealmID, p.ID}
	where := []string{"f.account_id = $1", "f.realm_id = $2", "f.owner_agent_id = $3"}
	if opts.Subject != "" {
		args = append(args, opts.Subject)
		where = append(where, fmt.Sprintf("(s.canonical_key = $%d OR s.aliases ? $%d)", len(args), len(args)))
	}
	if opts.PredicatePrefix != "" {
		args = append(args, opts.PredicatePrefix)
		where = append(where, fmt.Sprintf("starts_with(f.predicate, $%d)", len(args)))
	}
	if opts.UnusedOnly {
		where = append(where, "u.fact_id IS NULL")
	}
	args = append(args, opts.Limit)
	query := factUsageSelect + " WHERE " + strings.Join(where, " AND ") +
		factListOrderClause(opts.OrderByUsage) + fmt.Sprintf(" LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Fact{}
	for rows.Next() {
		fact, err := scanFactWithUsage(rows)
		if err != nil {
			return nil, err
		}
		if fact.Sensitive && !opts.IncludeSensitive {
			fact.Value = json.RawMessage(`null`)
			fact.SourceRef = ""
		}
		out = append(out, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// Meter only a successfully completed result set. Usage is a companion
	// projection, so a metering failure never makes fact retrieval unavailable.
	_ = s.recordFactRetrievals(ctx, p, opts.RetrievalMode, out)
	return out, nil
}

func normalizeFactListOptions(opts FactListOptions) (FactListOptions, error) {
	if opts.Limit == 0 {
		opts.Limit = 100
	}
	if opts.RetrievalMode == "" {
		opts.RetrievalMode = FactRetrievalModeSearch
	}
	if !validFactRetrievalMode(opts.RetrievalMode) {
		return FactListOptions{}, fmt.Errorf("%w: invalid retrieval mode", ErrFactInputInvalid)
	}
	if opts.Limit < 1 || opts.Limit > 500 {
		return FactListOptions{}, fmt.Errorf("%w: limit must be between 1 and 500", ErrFactInputInvalid)
	}
	if opts.Subject != "" {
		opts.Subject = normalizeFactSubject(opts.Subject)
		if !validFactSubjectAlias(opts.Subject) {
			return FactListOptions{}, ErrFactInputInvalid
		}
	}
	if opts.PredicatePrefix != "" && !validFactPredicate(strings.TrimSuffix(opts.PredicatePrefix, "/")) {
		return FactListOptions{}, ErrFactInputInvalid
	}
	return opts, nil
}

func validFactRetrievalMode(mode FactRetrievalMode) bool {
	switch mode {
	case FactRetrievalModeExact, FactRetrievalModeSearch,
		FactRetrievalModeSelfHydration, FactRetrievalModeTemporal:
		return true
	default:
		return false
	}
}

func factRetrievalRanks(mode FactRetrievalMode) bool {
	return mode == FactRetrievalModeExact || mode == FactRetrievalModeSearch || mode == FactRetrievalModeTemporal
}

// recordFactRetrievals appends one immutable delivery event per returned fact
// in a single transaction and updates each time-bucket rollup once for the
// whole result set. Callers intentionally ignore failures because usage must
// not become an availability dependency for fact reads.
func (s *Store) recordFactRetrievals(ctx context.Context, p Principal, mode FactRetrievalMode, facts []Fact) error {
	if len(facts) == 0 {
		return nil
	}
	if !validFactRetrievalMode(mode) {
		return fmt.Errorf("%w: invalid retrieval mode", ErrFactInputInvalid)
	}
	metadata, err := json.Marshal(map[string]any{
		"retrieval_mode":   mode,
		"ranking_eligible": factRetrievalRanks(mode),
	})
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return err
	}
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return err
	}
	requestTime := time.Now().UTC()
	requestID, err := id.New("frq")
	if err != nil {
		return err
	}

	var query strings.Builder
	query.WriteString(`INSERT INTO usage_events
		(id, account_id, realm_id, agent_id, dimension, quantity, unit,
		 subject_type, subject_id, idempotency_key, metadata, occurred_at) VALUES `)
	args := make([]any, 0, len(facts)*12)
	for i, fact := range facts {
		if i != 0 {
			query.WriteString(", ")
		}
		base := i*12 + 1
		fmt.Fprintf(&query,
			"($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d::jsonb, $%d)",
			base, base+1, base+2, base+3, base+4, base+5,
			base+6, base+7, base+8, base+9, base+10, base+11)
		eventID, err := id.New("usg")
		if err != nil {
			return err
		}
		args = append(args, eventID, p.AccountID, p.RealmID, p.ID,
			UsageDimensionFactReturned, int64(1), UsageUnitFact, "fact", fact.ID,
			"fact_returned:"+string(mode)+":"+requestID+":"+fact.ID,
			string(metadata), requestTime)
	}
	query.WriteString(" ON CONFLICT (account_id, idempotency_key) DO NOTHING")
	tag, err := tx.Exec(ctx, query.String(), args...)
	if err != nil {
		return fmt.Errorf("record fact retrieval usage: %w", err)
	}
	inserted := tag.RowsAffected()
	if inserted == 0 {
		return tx.Commit(ctx)
	}
	for _, bucket := range []string{UsageBucketHour, UsageBucketDay} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO usage_rollups
			  (account_id, realm_id, agent_id, dimension, unit, bucket,
			   bucket_start, quantity, event_count)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
			ON CONFLICT (agent_id, dimension, unit, bucket, bucket_start)
			DO UPDATE SET
			  quantity = usage_rollups.quantity + EXCLUDED.quantity,
			  event_count = usage_rollups.event_count + EXCLUDED.event_count,
			  updated_at = now()`,
			p.AccountID, p.RealmID, p.ID, UsageDimensionFactReturned,
			UsageUnitFact, bucket, usageBucketStart(requestTime, bucket), inserted); err != nil {
			return fmt.Errorf("update fact retrieval %s rollup: %w", bucket, err)
		}
	}
	return tx.Commit(ctx)
}

func factListOrderClause(orderByUsage bool) string {
	if orderByUsage {
		// Refer to the nullable join input explicitly. PostgreSQL sorts NULLs
		// first for DESC, so ordering by u.usage_count directly would put facts
		// with no usage ahead of used facts.
		return " ORDER BY COALESCE(u.usage_count, 0) DESC, u.last_used_at DESC NULLS LAST, f.predicate, s.canonical_key, f.id"
	}
	return " ORDER BY f.predicate, s.canonical_key, f.id"
}

func scanFactWithUsage(row factScanner) (Fact, error) {
	var out Fact
	err := row.Scan(&out.ID, &out.AccountID, &out.RealmID, &out.OwnerAgentID,
		&out.SubjectID, &out.Subject, &out.Predicate, &out.Cardinality,
		&out.Sensitive, &out.ResolvedAssertionID, &out.ValueType, &out.Value, &out.Recurrence,
		&out.SourceKind, &out.SourceRef, &out.Confidence, &out.ObservedAt,
		&out.ConfirmedAt, &out.ValidFrom, &out.ValidUntil, &out.CreatedAt,
		&out.UpdatedAt, &out.UsageCount, &out.LastUsedAt)
	return out, err
}
