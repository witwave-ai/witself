package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMemoryVectorRoutesEnforceAgentAuthorityAndValueFreeReceipts(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	profile := MemoryVectorProfile{
		ID: "mvp_1", Provider: "local", Model: "embed-v1", Recipe: "plain",
		RecipeVersion: "1", Dimensions: 2, DistanceMetric: "cosine",
		Normalization: "l2", ContractHash: strings.Repeat("a", 64), CreatedAt: now,
	}
	receipt := MemoryVectorReceipt{
		ProfileID: "mvp_1", MemoryID: "mem_1", MemoryVersion: 3,
		ContentHash: strings.Repeat("b", 64), VectorHash: strings.Repeat("c", 64),
		Dimensions: 2, CreatedAt: now,
	}
	var createCalls, listCalls, putCalls int
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: memoryTestAuth(),
		CreateMemoryVectorProfile: func(_ context.Context, p DomainPrincipal, in CreateMemoryVectorProfileRequest) (MemoryVectorProfile, error) {
			createCalls++
			if p.Kind != PrincipalKindAgent || p.ID != "agent_1" {
				t.Errorf("create principal = %#v", p)
			}
			if in.Provider != "local" || in.Model != "embed-v1" || in.Recipe != "plain" ||
				in.RecipeVersion != "1" || in.Dimensions != 2 || in.DistanceMetric != "cosine" || in.Normalization != "l2" {
				t.Errorf("create input = %#v", in)
			}
			return profile, nil
		},
		ListMemoryVectorProfiles: func(_ context.Context, p DomainPrincipal) ([]MemoryVectorProfile, error) {
			listCalls++
			if p.Kind != PrincipalKindAgent || p.ID != "agent_1" {
				t.Errorf("list principal = %#v", p)
			}
			return nil, nil
		},
		PutMemoryVector: func(_ context.Context, p DomainPrincipal, in PutMemoryVectorRequest) (MemoryVectorReceipt, error) {
			putCalls++
			if p.Kind != PrincipalKindAgent || p.ID != "agent_1" {
				t.Errorf("put principal = %#v", p)
			}
			if in.ProfileID != "mvp_1" || in.MemoryID != "mem_1" || in.MemoryVersion != 3 ||
				in.ContentHash != strings.Repeat("b", 64) || len(in.Vector) != 2 ||
				in.Vector[0] != 12345.6789 || in.Vector[1] != -98765.4321 {
				t.Errorf("put input = %#v", in)
			}
			return receipt, nil
		},
	}))
	defer srv.Close()

	createBody := `{"provider":"local","model":"embed-v1","recipe":"plain","recipe_version":"1","dimensions":2,"distance_metric":"cosine","normalization":"l2"}`
	resp := memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memory-vector-profiles", "agent-token", "", createBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	assertMemoryVectorPrivateJSON(t, resp)
	var created MemoryVectorProfile
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if created != profile {
		t.Fatalf("created profile = %#v", created)
	}

	resp = memoryTestRequest(t, srv.URL, http.MethodGet, "/v1/memory-vector-profiles", "agent-token", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	assertMemoryVectorPrivateJSON(t, resp)
	var listed struct {
		SchemaVersion string                `json:"schema_version"`
		Items         []MemoryVectorProfile `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if listed.SchemaVersion != "witself.v0" || listed.Items == nil || len(listed.Items) != 0 {
		t.Fatalf("list response = %#v", listed)
	}

	putBody := `{"profile_id":"mvp_1","memory_id":"mem_1","memory_version":3,"content_hash":"` +
		strings.Repeat("b", 64) + `","vector":[12345.6789,-98765.4321]}`
	resp = memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memory-vectors", "agent-token", "", putBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("put status = %d", resp.StatusCode)
	}
	assertMemoryVectorPrivateJSON(t, resp)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if bytes.Contains(raw, []byte("12345.6789")) || bytes.Contains(raw, []byte("-98765.4321")) {
		t.Fatalf("receipt echoed raw vector components: %s", raw)
	}
	var received map[string]any
	if err := json.Unmarshal(raw, &received); err != nil {
		t.Fatal(err)
	}
	if _, ok := received["vector"]; ok {
		t.Fatalf("receipt contains vector field: %#v", received)
	}
	if received["vector_hash"] != strings.Repeat("c", 64) || received["dimensions"] != float64(2) {
		t.Fatalf("receipt = %#v", received)
	}

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/v1/memory-vector-profiles", createBody},
		{http.MethodGet, "/v1/memory-vector-profiles", ""},
		{http.MethodPost, "/v1/memory-vectors", putBody},
	} {
		resp = memoryTestRequest(t, srv.URL, tc.method, tc.path, "operator-token", "", tc.body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("operator %s %s status = %d", tc.method, tc.path, resp.StatusCode)
		}
		closeBody(t, resp)
	}
	resp = memoryTestRequest(t, srv.URL, http.MethodGet, "/v1/memory-vector-profiles", "", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated list status = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	if createCalls != 1 || listCalls != 1 || putCalls != 1 {
		t.Fatalf("callbacks = create:%d list:%d put:%d", createCalls, listCalls, putCalls)
	}
}

func TestMemoryVectorRequestBodyBoundsRejectBeforeDispatch(t *testing.T) {
	var createCalls, putCalls int
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: memoryTestAuth(),
		CreateMemoryVectorProfile: func(context.Context, DomainPrincipal, CreateMemoryVectorProfileRequest) (MemoryVectorProfile, error) {
			createCalls++
			return MemoryVectorProfile{}, nil
		},
		PutMemoryVector: func(context.Context, DomainPrincipal, PutMemoryVectorRequest) (MemoryVectorReceipt, error) {
			putCalls++
			return MemoryVectorReceipt{}, nil
		},
	}))
	defer srv.Close()

	profileBody := `{"provider":"` + strings.Repeat("p", 32*1024) + `"}`
	resp := memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memory-vector-profiles", "agent-token", "", profileBody)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized profile status = %d", resp.StatusCode)
	}
	assertMemoryVectorBoundError(t, resp, strings.Repeat("p", 128))

	vectorBody := `{"profile_id":"mvp_1","vector":[` + strings.Repeat("0,", 256*1024) + `0]}`
	resp = memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memory-vectors", "agent-token", "", vectorBody)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized vector status = %d", resp.StatusCode)
	}
	assertMemoryVectorBoundError(t, resp, strings.Repeat("0,", 64))

	if createCalls != 0 || putCalls != 0 {
		t.Fatalf("oversized request reached callbacks = create:%d put:%d", createCalls, putCalls)
	}
}

func assertMemoryVectorPrivateJSON(t *testing.T, resp *http.Response) {
	t.Helper()
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("cache control = %q", got)
	}
}

func assertMemoryVectorBoundError(t *testing.T, resp *http.Response, forbidden string) {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if bytes.Contains(raw, []byte(forbidden)) {
		t.Fatalf("error echoed request data: %s", raw)
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["error"] != "memory request body exceeds the supported limit" {
		t.Fatalf("oversized error = %#v", out)
	}
}
