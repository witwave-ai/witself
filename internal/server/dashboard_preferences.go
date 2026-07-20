package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxDashboardPreferencesRequestBytes bounds the PUT body: the prefs document
// itself is capped at 4 KiB by the store, so twice that comfortably covers
// the request envelope while refusing anything unreasonable.
const maxDashboardPreferencesRequestBytes int64 = 8 * 1024

// DashboardPreferences is one agent's dashboard UI preference row. Prefs is
// the canonical strictly validated document; UpdatedAt is server-stamped.
type DashboardPreferences struct {
	AgentID   string          `json:"agent_id"`
	Prefs     json.RawMessage `json:"prefs"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// PutDashboardPreferencesRequest carries the raw prefs document. The store
// enforces the strict v1 contract (schema marker, theme, size, no unknown
// keys); the HTTP boundary owns transport shape and the byte cap.
type PutDashboardPreferencesRequest struct {
	Prefs json.RawMessage `json:"prefs"`
}

// dashboardPreferencesAgentHandler is the shared authz gate: dashboard
// preferences are an agent-owned self surface, so only a full agent token may
// read or write its own row.
func dashboardPreferencesAgentHandler(auth PrincipalAuthFunc, next func(http.ResponseWriter, *http.Request, DomainPrincipal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
			if p.Kind != PrincipalKindAgent || strings.TrimSpace(p.ID) == "" ||
				strings.TrimSpace(p.AccountID) == "" || strings.TrimSpace(p.RealmID) == "" {
				writeJSONError(w, http.StatusForbidden, "only a full agent token may use dashboard preferences")
				return
			}
			next(w, r, p)
		})(w, r)
	}
}

// getDashboardPreferencesHandler serves GET /v1/self/dashboard-preferences: the
// agent's own row, or a null default when none was ever stored. Reading
// records no usage and writes no audit event — the dashboard polls it freely.
func getDashboardPreferencesHandler(auth PrincipalAuthFunc, get func(context.Context, DomainPrincipal) (*DashboardPreferences, error)) http.HandlerFunc {
	return dashboardPreferencesAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if r.URL.RawQuery != "" {
			writeJSONError(w, http.StatusBadRequest, "dashboard preferences do not accept query parameters")
			return
		}
		prefs, err := get(r.Context(), p)
		if writeDashboardPreferencesError(w, err, "read dashboard preferences") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "preferences": prefs,
		})
	})
}

// putDashboardPreferencesHandler serves PUT /v1/self/dashboard-preferences:
// the strict upsert of the agent's own row. Every shape or size violation is
// a 400; the store's validator is the authoritative contract.
func putDashboardPreferencesHandler(auth PrincipalAuthFunc, put func(context.Context, DomainPrincipal, PutDashboardPreferencesRequest) (DashboardPreferences, error)) http.HandlerFunc {
	return dashboardPreferencesAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if r.URL.RawQuery != "" {
			writeJSONError(w, http.StatusBadRequest, "dashboard preferences do not accept query parameters")
			return
		}
		var in PutDashboardPreferencesRequest
		if !decodeDashboardPreferencesRequest(w, r, &in) {
			return
		}
		if len(in.Prefs) == 0 {
			writeJSONError(w, http.StatusBadRequest, "prefs is required")
			return
		}
		prefs, err := put(r.Context(), p, in)
		if writeDashboardPreferencesError(w, err, "write dashboard preferences") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "preferences": prefs,
		})
	})
}

// decodeDashboardPreferencesRequest is the strict bounded decode: unknown
// envelope keys, trailing JSON, and oversized bodies are all 400s per the
// prefs contract (size violations are client shape errors here, not 413s).
func decodeDashboardPreferencesRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxDashboardPreferencesRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid dashboard preferences body")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid dashboard preferences body")
		return false
	}
	return true
}

func writeDashboardPreferencesError(w http.ResponseWriter, err error, operation string) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrBadInput):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusForbidden, "dashboard preferences access forbidden")
	case errors.Is(err, ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "agent not found")
	default:
		writeJSONError(w, http.StatusInternalServerError, "could not "+operation)
	}
	return true
}
