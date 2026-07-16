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

// FactAssertion is one immutable historical source claim.
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

// SetFactRequest is the POST /v1/facts request body.
type SetFactRequest struct {
	Subject         string          `json:"subject,omitempty"`
	Predicate       string          `json:"predicate"`
	ValueType       string          `json:"value_type,omitempty"`
	Value           json.RawMessage `json:"value"`
	Recurrence      string          `json:"recurrence,omitempty"`
	Cardinality     string          `json:"cardinality,omitempty"`
	Sensitive       bool            `json:"sensitive,omitempty"`
	SourceKind      string          `json:"source_kind,omitempty"`
	SourceRef       string          `json:"source_ref,omitempty"`
	Confidence      *float64        `json:"confidence,omitempty"`
	ObservedAt      time.Time       `json:"observed_at,omitempty"`
	ConfirmedAt     *time.Time      `json:"confirmed_at,omitempty"`
	ValidFrom       *time.Time      `json:"valid_from,omitempty"`
	ValidUntil      *time.Time      `json:"valid_until,omitempty"`
	RecreateDeleted bool            `json:"recreate_deleted,omitempty"`
	IdempotencyKey  string          `json:"-"`
}

// DeleteFactRequest is the value-free input to the fact deletion hook. DELETE
// requests carry the retry key in Idempotency-Key and optimistic concurrency
// guards in expected_resolved_assertion_id and expected_candidate_revision;
// they never carry the fact value.
type DeleteFactRequest struct {
	FactID                      string
	Subject                     string
	Predicate                   string
	ExpectedResolvedAssertionID string
	ExpectedCandidateRevision   string
	IdempotencyKey              string
	Apply                       bool
}

// FactDeletionReceipt is a sensitive-safe preview or deletion receipt. It is
// intentionally metadata-only: values, sources, and assertion evidence must
// never cross the deletion response boundary.
type FactDeletionReceipt struct {
	FactID              string     `json:"fact_id"`
	ReceiptID           string     `json:"receipt_id,omitempty"`
	SubjectID           string     `json:"subject_id"`
	Subject             string     `json:"subject"`
	Predicate           string     `json:"predicate"`
	Sensitive           bool       `json:"sensitive"`
	AssertionCount      int64      `json:"assertion_count"`
	CandidateCount      int64      `json:"candidate_count"`
	CandidateRevision   string     `json:"candidate_revision"`
	UsageCount          int64      `json:"usage_count"`
	ResolvedAssertionID string     `json:"resolved_assertion_id"`
	DeletionState       string     `json:"deletion_state"`
	DeletedAt           *time.Time `json:"deleted_at,omitempty"`
	Applied             bool       `json:"applied"`
	Replayed            bool       `json:"replayed,omitempty"`
}

// FactListOptions selects a bounded fact inventory.
type FactListOptions struct {
	Subject          string
	PredicatePrefix  string
	Limit            int
	IncludeSensitive bool
	OrderByUsage     bool
	UnusedOnly       bool
	RetrievalMode    FactRetrievalMode
}

// FactRetrievalMode identifies why facts were returned. Public list/search
// requests use search; automatic self hydration is kept separate by its
// dedicated server hook.
type FactRetrievalMode string

// FactRetrievalModeSearch marks an authenticated inventory or search result.
const FactRetrievalModeSearch FactRetrievalMode = "search"

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

// ProposeFactRequest is the POST /v1/fact-candidates body.
type ProposeFactRequest struct {
	SetFactRequest
	Reason string `json:"reason,omitempty"`
}

// FactCandidateListOptions selects a bounded candidate review inventory.
type FactCandidateListOptions struct {
	Status string
	Limit  int
}

// UpcomingFactOptions selects a temporal projection window.
type UpcomingFactOptions struct {
	From             time.Time
	Until            time.Time
	Timezone         string
	Subject          string
	PredicatePrefix  string
	Limit            int
	IncludeSensitive bool
}

// FactOccurrence is a derived temporal view that never mutates the fact.
type FactOccurrence struct {
	Fact     Fact       `json:"fact"`
	OccursOn string     `json:"occurs_on,omitempty"`
	OccursAt *time.Time `json:"occurs_at,omitempty"`
}

func parseObservationalRead(raw string) (bool, error) {
	if strings.TrimSpace(raw) == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New("observational must be true or false")
	}
	return value, nil
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
		req.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		fact, err := set(r.Context(), p, req)
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "fact access forbidden")
			return
		case errors.Is(err, ErrIdempotencyConflict):
			writeJSONError(w, http.StatusConflict, "idempotency key was reused for a different fact mutation")
			return
		case errors.Is(err, ErrFactDeleted):
			writeJSONError(w, http.StatusConflict, "fact was deleted; set recreate_deleted=true to create a new fact")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not set fact")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "fact": fact})
	})
}

func factsReadHandler(
	auth PrincipalAuthFunc,
	get func(context.Context, DomainPrincipal, string, string) (Fact, error),
	getObservational func(context.Context, DomainPrincipal, string, string) (Fact, error),
	list func(context.Context, DomainPrincipal, FactListOptions) ([]Fact, error),
	listObservational func(context.Context, DomainPrincipal, FactListOptions) ([]Fact, error),
) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may read facts")
			return
		}
		q := r.URL.Query()
		observational, err := parseObservationalRead(q.Get("observational"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		predicate := strings.TrimSpace(q.Get("predicate"))
		if predicate != "" {
			getForRead := get
			if observational {
				getForRead = getObservational
			}
			if getForRead == nil {
				writeJSONError(w, http.StatusNotImplemented, "observational fact reads are unavailable")
				return
			}
			fact, err := getForRead(r.Context(), p, q.Get("subject"), predicate)
			if writeFactError(w, err) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "private, no-store")
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "fact": fact})
			return
		}
		opts := FactListOptions{
			Subject: q.Get("subject"), PredicatePrefix: q.Get("predicate_prefix"),
			RetrievalMode: FactRetrievalModeSearch,
		}
		opts.OrderByUsage = q.Get("sort") == "usage"
		if raw := q.Get("unused"); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				writeJSONError(w, 400, "unused must be true or false")
				return
			}
			opts.UnusedOnly = value
		}
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
		listForRead := list
		if observational {
			listForRead = listObservational
		}
		if listForRead == nil {
			writeJSONError(w, http.StatusNotImplemented, "observational fact reads are unavailable")
			return
		}
		facts, err := listForRead(r.Context(), p, opts)
		if writeFactError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
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
		w.Header().Set("Cache-Control", "private, no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "assertions": assertions})
	})
}

func deleteFactHandler(auth PrincipalAuthFunc, deleteFact func(context.Context, DomainPrincipal, DeleteFactRequest) (FactDeletionReceipt, error)) http.HandlerFunc {
	protected := requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may delete facts")
			return
		}

		factID := strings.TrimSpace(r.PathValue("fact"))
		dryRun := false
		if raw := r.URL.Query().Get("dry_run"); raw != "" {
			var err error
			dryRun, err = strconv.ParseBool(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "dry_run must be true or false")
				return
			}
		}

		req := DeleteFactRequest{
			FactID:                      factID,
			Subject:                     strings.TrimSpace(r.URL.Query().Get("subject")),
			Predicate:                   strings.TrimSpace(r.URL.Query().Get("predicate")),
			ExpectedResolvedAssertionID: strings.TrimSpace(r.URL.Query().Get("expected_resolved_assertion_id")),
			ExpectedCandidateRevision:   strings.TrimSpace(r.URL.Query().Get("expected_candidate_revision")),
			IdempotencyKey:              strings.TrimSpace(r.Header.Get("Idempotency-Key")),
			Apply:                       !dryRun,
		}
		if factID == "" {
			if req.Apply || req.Subject == "" || req.Predicate == "" || req.IdempotencyKey != "" || req.ExpectedResolvedAssertionID != "" || req.ExpectedCandidateRevision != "" {
				writeJSONError(w, http.StatusBadRequest, "address preview requires dry_run=true, subject, and predicate only")
				return
			}
		} else if req.Subject != "" || req.Predicate != "" {
			writeJSONError(w, http.StatusBadRequest, "fact-id deletion does not accept subject or predicate")
			return
		}
		if !req.Apply && (req.IdempotencyKey != "" || req.ExpectedResolvedAssertionID != "" || req.ExpectedCandidateRevision != "") {
			writeJSONError(w, http.StatusBadRequest, "deletion preview accepts no idempotency or concurrency guard")
			return
		}
		if req.Apply && req.IdempotencyKey == "" {
			writeJSONError(w, http.StatusBadRequest, "Idempotency-Key is required when deleting a fact")
			return
		}
		if req.Apply && req.ExpectedResolvedAssertionID == "" {
			writeJSONError(w, http.StatusBadRequest, "expected_resolved_assertion_id is required when deleting a fact")
			return
		}
		if req.Apply && req.ExpectedCandidateRevision == "" {
			writeJSONError(w, http.StatusBadRequest, "expected_candidate_revision is required when deleting a fact")
			return
		}

		receipt, err := deleteFact(r.Context(), p, req)
		if writeDeleteFactError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "deletion": receipt})
	})
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		protected(w, r)
	}
}

func writeDeleteFactError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrBadInput):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusForbidden, "fact access forbidden")
	case errors.Is(err, ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "fact not found")
	case errors.Is(err, ErrFactDeleted):
		writeJSONError(w, http.StatusGone, "fact already deleted")
	case errors.Is(err, ErrIdempotencyConflict):
		writeJSONError(w, http.StatusConflict, "idempotency key was reused for a different fact deletion")
	case errors.Is(err, ErrConflict):
		writeJSONError(w, http.StatusConflict, "fact changed since deletion preview")
	default:
		writeJSONError(w, http.StatusInternalServerError, "could not delete fact")
	}
	return true
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
	case errors.Is(err, ErrIdempotencyConflict):
		writeJSONError(w, http.StatusConflict, "idempotency key was reused for a different fact mutation")
	case errors.Is(err, ErrConflict):
		writeJSONError(w, http.StatusConflict, "fact changed since candidate proposal")
	default:
		writeJSONError(w, http.StatusInternalServerError, "could not read facts")
	}
	return true
}

func proposeFactHandler(auth PrincipalAuthFunc, propose func(context.Context, DomainPrincipal, ProposeFactRequest) (FactCandidate, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, 403, "only an agent token may propose facts")
			return
		}
		var in ProposeFactRequest
		if decodeLimitedJSON(w, r, &in, 96*1024) != nil || len(in.Value) == 0 {
			writeJSONError(w, 400, "invalid fact proposal")
			return
		}
		in.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		out, err := propose(r.Context(), p, in)
		if errors.Is(err, ErrFactDeleted) {
			writeJSONError(w, http.StatusConflict, "fact was deleted; use fact set with recreate_deleted=true to create a new fact")
			return
		}
		if writeFactError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "candidate": out})
	})
}

func getFactCandidateHandler(auth PrincipalAuthFunc, get func(context.Context, DomainPrincipal, string) (FactCandidate, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may review fact candidates")
			return
		}
		out, err := get(r.Context(), p, r.PathValue("candidate"))
		if writeFactError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "candidate": out})
	})
}

func listFactCandidatesHandler(auth PrincipalAuthFunc, list func(context.Context, DomainPrincipal, FactCandidateListOptions) ([]FactCandidate, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may review fact candidates")
			return
		}
		opts := FactCandidateListOptions{Status: r.URL.Query().Get("status"), Limit: 100}
		if raw := r.URL.Query().Get("limit"); raw != "" {
			limit, err := strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 500 {
				writeJSONError(w, http.StatusBadRequest, "limit must be between 1 and 500")
				return
			}
			opts.Limit = limit
		}
		rows, err := list(r.Context(), p, opts)
		if writeFactError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "candidates": rows})
	})
}

func factCandidateActionHandler(auth PrincipalAuthFunc, confirm func(context.Context, DomainPrincipal, string, string) (Fact, error), reject func(context.Context, DomainPrincipal, string, string) (FactCandidate, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may decide fact candidates")
			return
		}
		candidateID, action, ok := strings.Cut(r.PathValue("action"), ":")
		if !ok || candidateID == "" || (action != "confirm" && action != "reject") {
			writeJSONError(w, 404, "candidate action not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if action == "confirm" {
			out, err := confirm(r.Context(), p, candidateID, idempotencyKey)
			if errors.Is(err, ErrFactDeleted) {
				writeJSONError(w, http.StatusConflict, "fact was deleted; use fact set with recreate_deleted=true to create a new fact")
				return
			}
			if writeFactError(w, err) {
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "fact": out})
			return
		}
		out, err := reject(r.Context(), p, candidateID, idempotencyKey)
		if writeFactError(w, err) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "candidate": out})
	})
}

func upcomingFactsHandler(
	auth PrincipalAuthFunc,
	upcoming func(context.Context, DomainPrincipal, UpcomingFactOptions) ([]FactOccurrence, error),
	upcomingObservational func(context.Context, DomainPrincipal, UpcomingFactOptions) ([]FactOccurrence, error),
) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may review upcoming facts")
			return
		}
		q := r.URL.Query()
		observational, err := parseObservationalRead(q.Get("observational"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		opts := UpcomingFactOptions{Timezone: q.Get("timezone"), Subject: q.Get("subject"), PredicatePrefix: q.Get("predicate_prefix")}
		if raw := q.Get("include_sensitive"); raw != "" {
			opts.IncludeSensitive, err = strconv.ParseBool(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "include_sensitive must be true or false")
				return
			}
		}
		if raw := q.Get("from"); raw != "" {
			opts.From, err = time.Parse(time.RFC3339, raw)
			if err != nil {
				writeJSONError(w, 400, "from must be RFC3339")
				return
			}
		}
		if raw := q.Get("until"); raw != "" {
			opts.Until, err = time.Parse(time.RFC3339, raw)
			if err != nil {
				writeJSONError(w, 400, "until must be RFC3339")
				return
			}
		}
		if raw := q.Get("limit"); raw != "" {
			opts.Limit, err = strconv.Atoi(raw)
			if err != nil {
				writeJSONError(w, 400, "limit must be an integer")
				return
			}
		}
		upcomingForRead := upcoming
		if observational {
			upcomingForRead = upcomingObservational
		}
		if upcomingForRead == nil {
			writeJSONError(w, http.StatusNotImplemented, "observational upcoming-fact reads are unavailable")
			return
		}
		rows, err := upcomingForRead(r.Context(), p, opts)
		if writeFactError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "occurrences": rows})
	})
}
