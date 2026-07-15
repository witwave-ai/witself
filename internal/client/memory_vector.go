package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// MemoryVectorProfile is an immutable contract for client-generated memory
// vectors.
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

// CreateMemoryVectorProfileInput defines a client vector recipe and its shape.
type CreateMemoryVectorProfileInput struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Recipe         string `json:"recipe"`
	RecipeVersion  string `json:"recipe_version"`
	Dimensions     int    `json:"dimensions"`
	DistanceMetric string `json:"distance_metric"`
	Normalization  string `json:"normalization"`
}

// MemoryVectorReceipt is a value-free receipt for a stored memory vector.
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

// PutMemoryVectorInput binds a client-generated vector to one immutable memory
// version and content hash.
type PutMemoryVectorInput struct {
	ProfileID     string    `json:"profile_id"`
	MemoryID      string    `json:"memory_id"`
	MemoryVersion int64     `json:"memory_version"`
	ContentHash   string    `json:"content_hash"`
	Vector        []float64 `json:"vector"`
}

// CreateMemoryVectorProfile registers or replays an immutable vector profile.
func CreateMemoryVectorProfile(ctx context.Context, endpoint, token string, in CreateMemoryVectorProfileInput) (*MemoryVectorProfile, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out MemoryVectorProfile
	if err := doJSON(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/v1/memory-vector-profiles", token, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListMemoryVectorProfiles retrieves all vector profiles visible to the token.
func ListMemoryVectorProfiles(ctx context.Context, endpoint, token string) ([]MemoryVectorProfile, error) {
	var out struct {
		Items []MemoryVectorProfile `json:"items"`
	}
	if err := doJSON(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/v1/memory-vector-profiles", token, nil, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []MemoryVectorProfile{}
	}
	return out.Items, nil
}

// PutMemoryVector stores or replays one client-generated vector.
func PutMemoryVector(ctx context.Context, endpoint, token string, in PutMemoryVectorInput) (*MemoryVectorReceipt, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out MemoryVectorReceipt
	if err := doJSON(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/v1/memory-vectors", token, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
