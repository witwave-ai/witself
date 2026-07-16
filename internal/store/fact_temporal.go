package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const defaultUpcomingFactWindow = 30 * 24 * time.Hour

// UpcomingFactOptions selects a bounded, read-only projection of resolved
// temporal facts. From is inclusive and Until is exclusive. Date-only values
// are calendar dates in Location (UTC by default), not instants; the current
// local date remains upcoming for the whole day.
type UpcomingFactOptions struct {
	From             time.Time
	Until            time.Time
	Location         *time.Location
	Subject          string
	PredicatePrefix  string
	Limit            int
	IncludeSensitive bool
}

// FactOccurrence is a derived view over one resolved fact. Date-only facts set
// OccursOn; datetime facts set OccursAt. Projection never changes the fact,
// its resolved assertion, its usage, or its history.
type FactOccurrence struct {
	Fact     Fact       `json:"fact"`
	OccursOn string     `json:"occurs_on,omitempty"`
	OccursAt *time.Time `json:"occurs_at,omitempty"`
}

type upcomingFactWindow struct {
	from     time.Time
	until    time.Time
	location *time.Location
}

type projectedFactOccurrence struct {
	occurrence FactOccurrence
	sortAt     time.Time
}

// UpcomingFacts returns the authenticated agent's resolved date and datetime
// facts in chronological order. Validity is evaluated as of From: a resolved
// assertion whose truth window has not started or has already ended is not
// projected. Sensitive temporal facts are omitted unless explicitly included,
// because an occurrence timestamp is itself the fact value and cannot be
// meaningfully redacted.
//
// Recurrence is never inferred from predicates such as birthday or anniversary.
// An assertion may explicitly opt a date into annual recurrence; February 29
// occurrences are skipped in non-leap years rather than moved to another date.
func (s *Store) UpcomingFacts(ctx context.Context, p Principal, opts UpcomingFactOptions) ([]FactOccurrence, error) {
	return s.upcomingFacts(ctx, p, opts, true)
}

// UpcomingFactsObservational returns the same temporal projection without
// writing retrieval usage.
func (s *Store) UpcomingFactsObservational(ctx context.Context, p Principal, opts UpcomingFactOptions) ([]FactOccurrence, error) {
	return s.upcomingFacts(ctx, p, opts, false)
}

func (s *Store) upcomingFacts(ctx context.Context, p Principal, opts UpcomingFactOptions, recordUsage bool) ([]FactOccurrence, error) {
	if p.Kind != PrincipalAgent {
		return nil, ErrFactForbidden
	}
	window, opts, err := normalizeUpcomingFactOptions(opts)
	if err != nil {
		return nil, err
	}

	args := []any{p.AccountID, p.RealmID, p.ID}
	where := []string{
		"f.account_id = $1",
		"f.realm_id = $2",
		"f.owner_agent_id = $3",
		"a.value_type IN ('date', 'datetime')",
	}
	if !opts.IncludeSensitive {
		where = append(where, "NOT f.sensitive")
	}
	if opts.Subject != "" {
		args = append(args, opts.Subject)
		where = append(where, fmt.Sprintf("(s.canonical_key = $%d OR s.aliases ? $%d)", len(args), len(args)))
	}
	if opts.PredicatePrefix != "" {
		args = append(args, opts.PredicatePrefix)
		where = append(where, fmt.Sprintf("starts_with(f.predicate, $%d)", len(args)))
	}

	rows, err := s.pool.Query(ctx, factSelect+" WHERE "+strings.Join(where, " AND ")+" ORDER BY f.id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	projected := []projectedFactOccurrence{}
	for rows.Next() {
		fact, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		projected = append(projected, projectFactOccurrences(fact, window, opts.Limit)...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(projected, func(i, j int) bool {
		if !projected[i].sortAt.Equal(projected[j].sortAt) {
			return projected[i].sortAt.Before(projected[j].sortAt)
		}
		left, right := projected[i].occurrence.Fact, projected[j].occurrence.Fact
		if left.Subject != right.Subject {
			return left.Subject < right.Subject
		}
		if left.Predicate != right.Predicate {
			return left.Predicate < right.Predicate
		}
		return left.ID < right.ID
	})
	if len(projected) > opts.Limit {
		projected = projected[:opts.Limit]
	}
	out := make([]FactOccurrence, len(projected))
	facts := make([]Fact, 0, len(projected))
	seenFacts := make(map[string]struct{}, len(projected))
	for i := range projected {
		out[i] = projected[i].occurrence
		fact := projected[i].occurrence.Fact
		if _, seen := seenFacts[fact.ID]; !seen {
			seenFacts[fact.ID] = struct{}{}
			facts = append(facts, fact)
		}
	}
	if recordUsage {
		_ = s.recordFactRetrievals(ctx, p, FactRetrievalModeTemporal, facts)
	}
	return out, nil
}

func normalizeUpcomingFactOptions(opts UpcomingFactOptions) (upcomingFactWindow, UpcomingFactOptions, error) {
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	if opts.From.IsZero() {
		opts.From = time.Now().UTC()
	} else {
		opts.From = opts.From.UTC()
	}
	if opts.Until.IsZero() {
		opts.Until = opts.From.Add(defaultUpcomingFactWindow)
	} else {
		opts.Until = opts.Until.UTC()
	}
	if !opts.Until.After(opts.From) {
		return upcomingFactWindow{}, UpcomingFactOptions{}, fmt.Errorf("%w: until must be after from", ErrFactInputInvalid)
	}
	if opts.Limit == 0 {
		opts.Limit = 100
	}
	if opts.Limit < 1 || opts.Limit > 500 {
		return upcomingFactWindow{}, UpcomingFactOptions{}, fmt.Errorf("%w: limit must be between 1 and 500", ErrFactInputInvalid)
	}
	if opts.Subject != "" {
		opts.Subject = normalizeFactSubject(opts.Subject)
		if !validFactSubjectAlias(opts.Subject) {
			return upcomingFactWindow{}, UpcomingFactOptions{}, ErrFactInputInvalid
		}
	}
	if opts.PredicatePrefix != "" {
		opts.PredicatePrefix = strings.TrimSpace(opts.PredicatePrefix)
		if !validFactPredicate(strings.TrimSuffix(opts.PredicatePrefix, "/")) {
			return upcomingFactWindow{}, UpcomingFactOptions{}, ErrFactInputInvalid
		}
	}
	return upcomingFactWindow{from: opts.From, until: opts.Until, location: opts.Location}, opts, nil
}

func projectFactOccurrence(fact Fact, window upcomingFactWindow) (projectedFactOccurrence, bool) {
	occurrences := projectFactOccurrences(fact, window, 1)
	if len(occurrences) == 0 {
		return projectedFactOccurrence{}, false
	}
	return occurrences[0], true
}

func projectFactOccurrences(fact Fact, window upcomingFactWindow, limit int) []projectedFactOccurrence {
	if fact.ValidFrom != nil && fact.ValidFrom.After(window.from) {
		return nil
	}
	if fact.ValidUntil != nil && fact.ValidUntil.Before(window.from) {
		return nil
	}

	var text string
	if err := json.Unmarshal(fact.Value, &text); err != nil {
		return nil
	}
	switch fact.ValueType {
	case "date":
		date, err := time.ParseInLocation(time.DateOnly, text, window.location)
		if err != nil {
			return nil
		}
		fromDate := time.Date(window.from.In(window.location).Year(), window.from.In(window.location).Month(), window.from.In(window.location).Day(), 0, 0, 0, 0, window.location)
		if fact.Recurrence == FactRecurrenceAnnual {
			return projectAnnualDateOccurrences(fact, date, fromDate, window.until.In(window.location), limit)
		}
		if date.Before(fromDate) || !date.Before(window.until.In(window.location)) {
			return nil
		}
		return []projectedFactOccurrence{{
			occurrence: FactOccurrence{Fact: fact, OccursOn: text},
			sortAt:     date.UTC(),
		}}
	case "datetime":
		at, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || at.Before(window.from) || !at.Before(window.until) {
			return nil
		}
		at = at.UTC()
		return []projectedFactOccurrence{{
			occurrence: FactOccurrence{Fact: fact, OccursAt: &at},
			sortAt:     at,
		}}
	default:
		return nil
	}
}

func projectAnnualDateOccurrences(fact Fact, baseDate, fromDate, until time.Time, limit int) []projectedFactOccurrence {
	if limit < 1 {
		return nil
	}
	out := make([]projectedFactOccurrence, 0, limit)
	for year := fromDate.Year(); year <= until.Year() && len(out) < limit; year++ {
		date := time.Date(year, baseDate.Month(), baseDate.Day(), 0, 0, 0, 0, fromDate.Location())
		// time.Date normalizes impossible dates; reject that normalization so
		// February 29 is absent instead of silently becoming March 1.
		if date.Month() != baseDate.Month() || date.Day() != baseDate.Day() {
			continue
		}
		if date.Before(baseDate) || date.Before(fromDate) || !date.Before(until) {
			continue
		}
		out = append(out, projectedFactOccurrence{
			occurrence: FactOccurrence{Fact: fact, OccursOn: date.Format(time.DateOnly)},
			sortAt:     date.UTC(),
		})
	}
	return out
}
