package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Fact is the public resolved fact representation.
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

// FactAssertion is one immutable historical source claim.
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

// SetFactRequest is the POST /v1/facts request body.
type SetFactRequest struct {
	Subject     string          `json:"subject,omitempty"`
	Predicate   string          `json:"predicate"`
	ValueType   string          `json:"value_type,omitempty"`
	Value       json.RawMessage `json:"value"`
	Cardinality string          `json:"cardinality,omitempty"`
	Sensitive   bool            `json:"sensitive,omitempty"`
	SourceKind  string          `json:"source_kind,omitempty"`
	SourceRef   string          `json:"source_ref,omitempty"`
	Confidence  *float64        `json:"confidence,omitempty"`
	ObservedAt  time.Time       `json:"observed_at,omitempty"`
	ConfirmedAt *time.Time      `json:"confirmed_at,omitempty"`
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

func setFactHandler(auth PrincipalAuthFunc, set func(context.Context, DomainPrincipal, SetFactRequest) (Fact, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may set facts")
			return
		}
		var req SetFactRequest
		if err := decodeLimitedJSON(w, r, &req, 96*1024); err != nil || len(req.Value) == 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON fact body")
			return
		}
		if req.SourceKind != "" && req.SourceKind != "agent" {
			writeJSONError(w, http.StatusBadRequest, "source_kind is derived from the authenticated agent")
			return
		}
		fact, err := set(r.Context(), p, req)
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "fact access forbidden")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not set fact")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "fact": fact})
	})
}

func factsReadHandler(auth PrincipalAuthFunc, get func(context.Context, DomainPrincipal, string, string) (Fact, error), list func(context.Context, DomainPrincipal, FactListOptions) ([]Fact, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may read facts")
			return
		}
		q := r.URL.Query()
		predicate := strings.TrimSpace(q.Get("predicate"))
		if predicate != "" {
			fact, err := get(r.Context(), p, q.Get("subject"), predicate)
			if writeFactError(w, err) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "fact": fact})
			return
		}
		opts := FactListOptions{Subject: q.Get("subject"), PredicatePrefix: q.Get("predicate_prefix")}
		if raw := q.Get("limit"); raw != "" {
			limit, err := strconv.Atoi(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "limit must be an integer")
				return
			}
			opts.Limit = limit
		}
		if raw := q.Get("include_sensitive"); raw != "" {
			include, err := strconv.ParseBool(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "include_sensitive must be true or false")
				return
			}
			opts.IncludeSensitive = include
		}
		facts, err := list(r.Context(), p, opts)
		if writeFactError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "facts": facts})
	})
}

func factHistoryHandler(auth PrincipalAuthFunc, history func(context.Context, DomainPrincipal, string) ([]FactAssertion, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may read fact history")
			return
		}
		assertions, err := history(r.Context(), p, r.PathValue("fact"))
		if writeFactError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "assertions": assertions})
	})
}

func writeFactError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrBadInput):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "fact not found")
	case errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusForbidden, "fact access forbidden")
	default:
		writeJSONError(w, http.StatusInternalServerError, "could not read facts")
	}
	return true
}
