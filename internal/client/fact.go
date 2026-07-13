package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Fact is the client representation of one resolved durable fact.
type Fact struct {
	ID                  string          `json:"id"`
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
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	UsageCount          int64           `json:"usage_count"`
	LastUsedAt          *time.Time      `json:"last_used_at,omitempty"`
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

// SetFactInput carries one explicit typed assertion to the fact service.
type SetFactInput struct {
	Subject        string          `json:"subject,omitempty"`
	Predicate      string          `json:"predicate"`
	ValueType      string          `json:"value_type,omitempty"`
	Value          json.RawMessage `json:"value"`
	Recurrence     string          `json:"recurrence,omitempty"`
	Cardinality    string          `json:"cardinality,omitempty"`
	Sensitive      bool            `json:"sensitive,omitempty"`
	SourceKind     string          `json:"source_kind,omitempty"`
	SourceRef      string          `json:"source_ref,omitempty"`
	Confidence     *float64        `json:"confidence,omitempty"`
	ObservedAt     time.Time       `json:"observed_at,omitempty"`
	ConfirmedAt    *time.Time      `json:"confirmed_at,omitempty"`
	ValidFrom      *time.Time      `json:"valid_from,omitempty"`
	ValidUntil     *time.Time      `json:"valid_until,omitempty"`
	IdempotencyKey string          `json:"-"`
}

// FactListOptions selects a bounded fact inventory.
type FactListOptions struct {
	Subject          string
	PredicatePrefix  string
	Limit            int
	IncludeSensitive bool
	OrderByUsage     bool
	UnusedOnly       bool
}

// FactCandidate is an unresolved agent observation awaiting review.
type FactCandidate struct {
	ID                  string          `json:"id"`
	Subject             string          `json:"subject"`
	Predicate           string          `json:"predicate"`
	ValueType           string          `json:"value_type"`
	Value               json.RawMessage `json:"value"`
	Recurrence          string          `json:"recurrence,omitempty"`
	Cardinality         string          `json:"cardinality"`
	Sensitive           bool            `json:"sensitive"`
	SourceRef           string          `json:"source_ref,omitempty"`
	Confidence          float64         `json:"confidence"`
	ObservedAt          time.Time       `json:"observed_at"`
	ValidFrom           *time.Time      `json:"valid_from,omitempty"`
	ValidUntil          *time.Time      `json:"valid_until,omitempty"`
	Reason              string          `json:"reason,omitempty"`
	Status              string          `json:"status"`
	ConflictFactID      string          `json:"conflict_fact_id,omitempty"`
	ObservedAssertionID string          `json:"observed_assertion_id,omitempty"`
	ResolvedFactID      string          `json:"resolved_fact_id,omitempty"`
	ProposedAt          time.Time       `json:"proposed_at"`
	DecidedAt           *time.Time      `json:"decided_at,omitempty"`
}

// ProposeFactInput carries a candidate rather than an immediately resolved fact.
type ProposeFactInput struct {
	SetFactInput
	Reason string `json:"reason,omitempty"`
}

// FactCandidateListOptions selects a bounded candidate review inventory.
type FactCandidateListOptions struct {
	Status string
	Limit  int
}

// FactUpcomingOptions selects a bounded temporal fact projection.
type FactUpcomingOptions struct {
	From             time.Time
	Until            time.Time
	Timezone         string
	IncludeSensitive bool
}

// FactOccurrence is a derived temporal fact view.
type FactOccurrence struct {
	Fact     Fact       `json:"fact"`
	OccursOn string     `json:"occurs_on,omitempty"`
	OccursAt *time.Time `json:"occurs_at,omitempty"`
}

// SetFact creates or supersedes the resolved assertion for a subject/predicate.
func SetFact(ctx context.Context, endpoint, token string, in SetFactInput) (*Fact, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out struct {
		Fact Fact `json:"fact"`
	}
	headers := factIdempotencyHeaders(in.IdempotencyKey)
	if err := doJSONWithHeaders(ctx, http.MethodPost, factsURL(endpoint), token, headers, body, &out); err != nil {
		return nil, err
	}
	return &out.Fact, nil
}

// GetFact retrieves one fact by exact subject and predicate. Successful
// retrieval is audited as an exact, ranking-eligible fact delivery.
func GetFact(ctx context.Context, endpoint, token, subject, predicate string) (*Fact, error) {
	query := url.Values{"subject": {subject}, "predicate": {predicate}}
	var out struct {
		Fact Fact `json:"fact"`
	}
	if err := doJSON(ctx, http.MethodGet, factsURL(endpoint)+"?"+query.Encode(), token, nil, &out); err != nil {
		return nil, err
	}
	return &out.Fact, nil
}

// ListFacts retrieves a bounded, optionally redacted fact inventory. Returned
// facts are audited as search deliveries and contribute to usage ranking.
func ListFacts(ctx context.Context, endpoint, token string, opts FactListOptions) ([]Fact, error) {
	query := url.Values{}
	if opts.Subject != "" {
		query.Set("subject", opts.Subject)
	}
	if opts.PredicatePrefix != "" {
		query.Set("predicate_prefix", opts.PredicatePrefix)
	}
	if opts.Limit != 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.IncludeSensitive {
		query.Set("include_sensitive", "true")
	}
	if opts.OrderByUsage {
		query.Set("sort", "usage")
	}
	if opts.UnusedOnly {
		query.Set("unused", "true")
	}
	requestURL := factsURL(endpoint)
	if len(query) != 0 {
		requestURL += "?" + query.Encode()
	}
	var out struct {
		Facts []Fact `json:"facts"`
	}
	if err := doJSON(ctx, http.MethodGet, requestURL, token, nil, &out); err != nil {
		return nil, err
	}
	return out.Facts, nil
}

// GetFactHistory retrieves immutable assertions newest first.
func GetFactHistory(ctx context.Context, endpoint, token, factID string) ([]FactAssertion, error) {
	var out struct {
		Assertions []FactAssertion `json:"assertions"`
	}
	if err := doJSON(ctx, http.MethodGet, factsURL(endpoint)+"/"+url.PathEscape(factID)+"/history", token, nil, &out); err != nil {
		return nil, err
	}
	return out.Assertions, nil
}

// ProposeFact submits an uncertain fact for review.
func ProposeFact(ctx context.Context, endpoint, token string, in ProposeFactInput) (*FactCandidate, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out struct {
		Candidate FactCandidate `json:"candidate"`
	}
	headers := factIdempotencyHeaders(in.IdempotencyKey)
	if err = doJSONWithHeaders(ctx, http.MethodPost, factCandidatesURL(endpoint), token, headers, body, &out); err != nil {
		return nil, err
	}
	return &out.Candidate, nil
}

// GetFactCandidate retrieves one candidate for explicit review. This detail
// boundary can return a sensitive candidate value that broad lists redact.
func GetFactCandidate(ctx context.Context, endpoint, token, id string) (*FactCandidate, error) {
	var out struct {
		Candidate FactCandidate `json:"candidate"`
	}
	if err := doJSON(ctx, http.MethodGet, factCandidatesURL(endpoint)+"/"+url.PathEscape(id), token, nil, &out); err != nil {
		return nil, err
	}
	return &out.Candidate, nil
}

// ListFactCandidates lists candidates by lifecycle status with the default
// bounded review size.
func ListFactCandidates(ctx context.Context, endpoint, token, status string) ([]FactCandidate, error) {
	return ListFactCandidatesWithOptions(ctx, endpoint, token, FactCandidateListOptions{Status: status})
}

// ListFactCandidatesWithOptions lists a bounded candidate review inventory.
func ListFactCandidatesWithOptions(ctx context.Context, endpoint, token string, opts FactCandidateListOptions) ([]FactCandidate, error) {
	q := url.Values{}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.Limit != 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	u := factCandidatesURL(endpoint)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var out struct {
		Candidates []FactCandidate `json:"candidates"`
	}
	if err := doJSON(ctx, http.MethodGet, u, token, nil, &out); err != nil {
		return nil, err
	}
	return out.Candidates, nil
}

// ConfirmFactCandidate promotes a candidate into canonical fact history.
func ConfirmFactCandidate(ctx context.Context, endpoint, token, id string) (*Fact, error) {
	return ConfirmFactCandidateWithIdempotency(ctx, endpoint, token, id, "")
}

// ConfirmFactCandidateWithIdempotency safely retries one logical decision.
func ConfirmFactCandidateWithIdempotency(ctx context.Context, endpoint, token, id, idempotencyKey string) (*Fact, error) {
	var out struct {
		Fact Fact `json:"fact"`
	}
	if err := doJSONWithHeaders(ctx, http.MethodPost, factCandidatesURL(endpoint)+"/"+url.PathEscape(id)+":confirm", token, factIdempotencyHeaders(idempotencyKey), nil, &out); err != nil {
		return nil, err
	}
	return &out.Fact, nil
}

// RejectFactCandidate closes a candidate without changing canonical facts.
func RejectFactCandidate(ctx context.Context, endpoint, token, id string) (*FactCandidate, error) {
	return RejectFactCandidateWithIdempotency(ctx, endpoint, token, id, "")
}

// RejectFactCandidateWithIdempotency safely retries one logical decision.
func RejectFactCandidateWithIdempotency(ctx context.Context, endpoint, token, id, idempotencyKey string) (*FactCandidate, error) {
	var out struct {
		Candidate FactCandidate `json:"candidate"`
	}
	if err := doJSONWithHeaders(ctx, http.MethodPost, factCandidatesURL(endpoint)+"/"+url.PathEscape(id)+":reject", token, factIdempotencyHeaders(idempotencyKey), nil, &out); err != nil {
		return nil, err
	}
	return &out.Candidate, nil
}

func factIdempotencyHeaders(key string) map[string]string {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	return map[string]string{"Idempotency-Key": key}
}

// UpcomingFacts lists resolved date/datetime facts in a future window.
// Returned occurrences are audited as temporal, ranking-eligible deliveries.
func UpcomingFacts(ctx context.Context, endpoint, token string, from, until time.Time, timezone string) ([]FactOccurrence, error) {
	return UpcomingFactsWithOptions(ctx, endpoint, token, FactUpcomingOptions{
		From: from, Until: until, Timezone: timezone,
	})
}

// UpcomingFactsWithOptions lists temporal facts with explicit sensitivity
// handling. Sensitive dates are excluded unless the caller opts in.
func UpcomingFactsWithOptions(ctx context.Context, endpoint, token string, opts FactUpcomingOptions) ([]FactOccurrence, error) {
	q := url.Values{}
	if !opts.From.IsZero() {
		q.Set("from", opts.From.Format(time.RFC3339))
	}
	if !opts.Until.IsZero() {
		q.Set("until", opts.Until.Format(time.RFC3339))
	}
	if opts.Timezone != "" {
		q.Set("timezone", opts.Timezone)
	}
	if opts.IncludeSensitive {
		q.Set("include_sensitive", "true")
	}
	var out struct {
		Occurrences []FactOccurrence `json:"occurrences"`
	}
	u := strings.TrimRight(endpoint, "/") + "/v1/fact-occurrences"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	if err := doJSON(ctx, http.MethodGet, u, token, nil, &out); err != nil {
		return nil, err
	}
	return out.Occurrences, nil
}

func factsURL(endpoint string) string { return strings.TrimRight(endpoint, "/") + "/v1/facts" }
func factCandidatesURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/fact-candidates"
}
