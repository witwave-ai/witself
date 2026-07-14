package store

import (
	"context"
	"fmt"
	"strings"
)

// CountFacts returns the full resolved inventory size for the supplied
// filters. Limit and ordering do not affect the count, which lets bounded
// projections such as /v1/self report how much state is available even when
// they only hydrate a small prefix.
func (s *Store) CountFacts(ctx context.Context, p Principal, opts FactListOptions) (int, error) {
	if p.Kind != PrincipalAgent {
		return 0, ErrFactForbidden
	}
	if opts.Subject != "" {
		opts.Subject = normalizeFactSubject(opts.Subject)
		if !validFactSubjectAlias(opts.Subject) {
			return 0, ErrFactInputInvalid
		}
	}
	if opts.PredicatePrefix != "" && !validFactPredicate(strings.TrimSuffix(opts.PredicatePrefix, "/")) {
		return 0, ErrFactInputInvalid
	}

	args := []any{p.AccountID, p.RealmID, p.ID}
	where := []string{
		"f.account_id = $1",
		"f.realm_id = $2",
		"f.owner_agent_id = $3",
		"f.deleted_at IS NULL",
		"f.resolved_assertion_id IS NOT NULL",
	}
	if opts.Subject != "" {
		args = append(args, opts.Subject)
		where = append(where, fmt.Sprintf("(s.canonical_key = $%d OR s.aliases ? $%d)", len(args), len(args)))
	}
	if opts.PredicatePrefix != "" {
		args = append(args, opts.PredicatePrefix)
		where = append(where, fmt.Sprintf("starts_with(f.predicate, $%d)", len(args)))
	}
	if opts.UnusedOnly {
		where = append(where, `NOT EXISTS (
			SELECT 1 FROM usage_events u
			WHERE u.account_id = f.account_id AND u.realm_id = f.realm_id
			  AND u.agent_id = f.owner_agent_id
			  AND u.dimension = 'fact_returned' AND u.unit = 'fact'
			  AND u.subject_type = 'fact' AND u.subject_id = f.id
			  AND COALESCE(u.metadata->>'retrieval_mode', 'exact')
			      IN ('exact', 'search', 'temporal')
		)`)
	}

	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM facts f
		JOIN fact_subjects s ON s.id = f.subject_id
		WHERE `+strings.Join(where, " AND "), args...).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count facts: %w", err)
	}
	return count, nil
}
