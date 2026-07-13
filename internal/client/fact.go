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

// SetFactInput carries one explicit typed assertion to the fact service.
type SetFactInput struct {
	Subject     string          `json:"subject,omitempty"`
	Predicate   string          `json:"predicate"`
	ValueType   string          `json:"value_type,omitempty"`
	Value       json.RawMessage `json:"value"`
	Cardinality string          `json:"cardinality,omitempty"`
	Sensitive   bool            `json:"sensitive,omitempty"`
	SourceKind  string          `json:"source_kind,omitempty"`
	SourceRef   string          `json:"source_ref,omitempty"`
	ValidFrom   *time.Time      `json:"valid_from,omitempty"`
	ValidUntil  *time.Time      `json:"valid_until,omitempty"`
}

// FactListOptions selects a bounded fact inventory.
type FactListOptions struct {
	Subject          string
	PredicatePrefix  string
	Limit            int
	IncludeSensitive bool
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
	if err := doJSON(ctx, http.MethodPost, factsURL(endpoint), token, body, &out); err != nil {
		return nil, err
	}
	return &out.Fact, nil
}

// GetFact retrieves one fact by exact subject and predicate.
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

// ListFacts retrieves a bounded, optionally redacted fact inventory.
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

func factsURL(endpoint string) string { return strings.TrimRight(endpoint, "/") + "/v1/facts" }
