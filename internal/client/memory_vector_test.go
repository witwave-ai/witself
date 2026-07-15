package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMemoryVectorClientRequestAndResponseContracts(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("authorization = %q", got)
		}
		key := r.Method + " " + r.URL.Path
		seen[key]++
		switch key {
		case "POST /v1/memory-vector-profiles":
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("create content type = %q", got)
			}
			var in CreateMemoryVectorProfileInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode create: %v", err)
			}
			if in.Provider != "local" || in.Model != "embed-v1" || in.Recipe != "plain" ||
				in.RecipeVersion != "1" || in.Dimensions != 2 || in.DistanceMetric != "cosine" || in.Normalization != "l2" {
				t.Errorf("create input = %#v", in)
			}
			_ = json.NewEncoder(w).Encode(MemoryVectorProfile{
				ID: "mvp_1", Provider: in.Provider, Model: in.Model, Recipe: in.Recipe,
				RecipeVersion: in.RecipeVersion, Dimensions: in.Dimensions,
				DistanceMetric: in.DistanceMetric, Normalization: in.Normalization,
				ContractHash: strings.Repeat("a", 64), CreatedAt: now,
			})
		case "GET /v1/memory-vector-profiles":
			if r.Header.Get("Content-Type") != "" {
				t.Errorf("GET unexpectedly has content type %q", r.Header.Get("Content-Type"))
			}
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","items":null}`)
		case "POST /v1/memory-vectors":
			var in PutMemoryVectorInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode put: %v", err)
			}
			if in.ProfileID != "mvp_1" || in.MemoryID != "mem_1" || in.MemoryVersion != 3 ||
				in.ContentHash != strings.Repeat("b", 64) || len(in.Vector) != 2 ||
				in.Vector[0] != 12345.6789 || in.Vector[1] != -98765.4321 {
				t.Errorf("put input = %#v", in)
			}
			_ = json.NewEncoder(w).Encode(MemoryVectorReceipt{
				ProfileID: in.ProfileID, MemoryID: in.MemoryID, MemoryVersion: in.MemoryVersion,
				ContentHash: in.ContentHash, VectorHash: strings.Repeat("c", 64),
				Dimensions: len(in.Vector), CreatedAt: now,
			})
		default:
			t.Errorf("unexpected request %s", key)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	profile, err := CreateMemoryVectorProfile(ctx, srv.URL+"/", "token", CreateMemoryVectorProfileInput{
		Provider: "local", Model: "embed-v1", Recipe: "plain", RecipeVersion: "1",
		Dimensions: 2, DistanceMetric: "cosine", Normalization: "l2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != "mvp_1" || profile.ContractHash != strings.Repeat("a", 64) || !profile.CreatedAt.Equal(now) {
		t.Fatalf("profile = %#v", profile)
	}

	profiles, err := ListMemoryVectorProfiles(ctx, srv.URL+"/", "token")
	if err != nil {
		t.Fatal(err)
	}
	if profiles == nil || len(profiles) != 0 {
		t.Fatalf("nil list was not normalized = %#v", profiles)
	}

	receipt, err := PutMemoryVector(ctx, srv.URL+"/", "token", PutMemoryVectorInput{
		ProfileID: "mvp_1", MemoryID: "mem_1", MemoryVersion: 3,
		ContentHash: strings.Repeat("b", 64), Vector: []float64{12345.6789, -98765.4321},
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ProfileID != "mvp_1" || receipt.MemoryID != "mem_1" || receipt.MemoryVersion != 3 ||
		receipt.VectorHash != strings.Repeat("c", 64) || receipt.Dimensions != 2 || !receipt.CreatedAt.Equal(now) {
		t.Fatalf("receipt = %#v", receipt)
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "12345.6789") || strings.Contains(string(raw), "-98765.4321") ||
		strings.Contains(string(raw), `"vector":`) {
		t.Fatalf("receipt exposes raw vector: %s", raw)
	}

	if seen["POST /v1/memory-vector-profiles"] != 1 || seen["GET /v1/memory-vector-profiles"] != 1 ||
		seen["POST /v1/memory-vectors"] != 1 || len(seen) != 3 {
		t.Fatalf("requests = %#v", seen)
	}
}

func TestMemoryVectorClientPropagatesServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"vector contract mismatch"}`)
	}))
	defer srv.Close()

	_, err := PutMemoryVector(context.Background(), srv.URL, "token", PutMemoryVectorInput{
		ProfileID: "mvp_1", MemoryID: "mem_1", MemoryVersion: 1,
		ContentHash: strings.Repeat("b", 64), Vector: []float64{1, 2},
	})
	if err == nil || err.Error() != "vector contract mismatch" {
		t.Fatalf("put error = %v", err)
	}
}
