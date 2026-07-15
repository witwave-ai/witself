package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// MemoryVectorProfile identifies the immutable contract used to produce memory vectors.
type MemoryVectorProfile struct {
	ID             string    `json:"id"`
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	Recipe         string    `json:"recipe"`
	RecipeVersion  string    `json:"recipe_version"`
	Dimensions     int       `json:"dimensions"`
	DistanceMetric string    `json:"distance_metric"`
	Normalization  string    `json:"normalization"`
	ContractHash   string    `json:"contract_hash"`
	CreatedAt      time.Time `json:"created_at"`
}

// CreateMemoryVectorProfileRequest defines a client-side memory vector contract.
type CreateMemoryVectorProfileRequest struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Recipe         string `json:"recipe"`
	RecipeVersion  string `json:"recipe_version"`
	Dimensions     int    `json:"dimensions"`
	DistanceMetric string `json:"distance_metric"`
	Normalization  string `json:"normalization"`
}

// PutMemoryVectorRequest attaches a client-produced vector to an exact memory version.
type PutMemoryVectorRequest struct {
	ProfileID     string    `json:"profile_id"`
	MemoryID      string    `json:"memory_id"`
	MemoryVersion int64     `json:"memory_version"`
	ContentHash   string    `json:"content_hash"`
	Vector        []float64 `json:"vector"`
}

// MemoryVectorReceipt records the value-free result of storing a memory vector.
type MemoryVectorReceipt struct {
	ProfileID     string    `json:"profile_id"`
	MemoryID      string    `json:"memory_id"`
	MemoryVersion int64     `json:"memory_version"`
	ContentHash   string    `json:"content_hash"`
	VectorHash    string    `json:"vector_hash"`
	Dimensions    int       `json:"dimensions"`
	CreatedAt     time.Time `json:"created_at"`
	Replayed      bool      `json:"replayed,omitempty"`
}

func createMemoryVectorProfileHandler(auth PrincipalAuthFunc, create func(context.Context, DomainPrincipal, CreateMemoryVectorProfileRequest) (MemoryVectorProfile, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may create memory vector profiles")
			return
		}
		var req CreateMemoryVectorProfileRequest
		if !decodeMemoryRequestJSON(w, r, &req, 32*1024, "invalid JSON memory vector profile body") {
			return
		}
		profile, err := create(r.Context(), p, req)
		if writeMemoryError(w, err, "create memory vector profile") {
			return
		}
		setMemoryResponseHeaders(w)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(profile)
	})
}

func listMemoryVectorProfilesHandler(auth PrincipalAuthFunc, list func(context.Context, DomainPrincipal) ([]MemoryVectorProfile, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may list memory vector profiles")
			return
		}
		profiles, err := list(r.Context(), p)
		if writeMemoryError(w, err, "list memory vector profiles") {
			return
		}
		if profiles == nil {
			profiles = []MemoryVectorProfile{}
		}
		setMemoryResponseHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "items": profiles})
	})
}

func putMemoryVectorHandler(auth PrincipalAuthFunc, put func(context.Context, DomainPrincipal, PutMemoryVectorRequest) (MemoryVectorReceipt, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may store memory vectors")
			return
		}
		var req PutMemoryVectorRequest
		if !decodeMemoryRequestJSON(w, r, &req, 512*1024, "invalid JSON memory vector body") {
			return
		}
		receipt, err := put(r.Context(), p, req)
		if writeMemoryError(w, err, "put memory vector") {
			return
		}
		setMemoryResponseHeaders(w)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(receipt)
	})
}
