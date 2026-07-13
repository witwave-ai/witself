package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// FactSubject is one stable, agent-owned identity used by durable facts.
type FactSubject struct {
	ID           string    `json:"id"`
	CanonicalKey string    `json:"canonical_key"`
	DisplayName  string    `json:"display_name,omitempty"`
	Aliases      []string  `json:"aliases"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// UpsertFactSubjectRequest is the PUT /v1/fact-subjects/{subject} body.
type UpsertFactSubjectRequest struct {
	DisplayName string `json:"display_name,omitempty"`
}

// AddFactSubjectAliasRequest is the aliases collection request body.
type AddFactSubjectAliasRequest struct {
	Alias string `json:"alias"`
}

func upsertFactSubjectHandler(auth PrincipalAuthFunc, upsert func(context.Context, DomainPrincipal, string, UpsertFactSubjectRequest) (FactSubject, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may manage fact subjects")
			return
		}
		canonicalKey := strings.TrimSpace(r.PathValue("subject"))
		var in UpsertFactSubjectRequest
		if canonicalKey == "" || decodeLimitedJSON(w, r, &in, 8*1024) != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid fact subject body")
			return
		}
		subject, err := upsert(r.Context(), p, canonicalKey, in)
		if writeFactSubjectError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "subject": subject})
	})
}

func listFactSubjectsHandler(auth PrincipalAuthFunc, list func(context.Context, DomainPrincipal) ([]FactSubject, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may read fact subjects")
			return
		}
		subjects, err := list(r.Context(), p)
		if writeFactSubjectError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "subjects": subjects})
	})
}

func addFactSubjectAliasHandler(auth PrincipalAuthFunc, add func(context.Context, DomainPrincipal, string, string) (FactSubject, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may manage fact subject aliases")
			return
		}
		canonicalKey := strings.TrimSpace(r.PathValue("subject"))
		var in AddFactSubjectAliasRequest
		if canonicalKey == "" || decodeLimitedJSON(w, r, &in, 8*1024) != nil || strings.TrimSpace(in.Alias) == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid fact subject alias body")
			return
		}
		subject, err := add(r.Context(), p, canonicalKey, in.Alias)
		if writeFactSubjectError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "subject": subject})
	})
}

func writeFactSubjectError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrBadInput):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "fact subject not found")
	case errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusForbidden, "fact subject access forbidden")
	default:
		writeJSONError(w, http.StatusInternalServerError, "could not manage fact subjects")
	}
	return true
}
