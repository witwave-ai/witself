package cpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/witwave-ai/witself/internal/billing/lifecycle"
)

// fakeCell stands in for a witself-server: it exposes /v1/accounts/{id}:plan
// gated by a provision token, and /v1/whoami gated by an operator bearer.
// It records applied snapshots for assertions.
type fakeCell struct {
	provisionToken string
	whoami         map[string]string // operator token -> account id
	mu             sync.Mutex
	applied        map[string]any // last snapshot per account
}

func newFakeCell(t *testing.T, provisionToken string, whoami map[string]string) (*fakeCell, string) {
	t.Helper()
	c := &fakeCell{provisionToken: provisionToken, whoami: whoami, applied: map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(c.handle))
	t.Cleanup(srv.Close)
	return c, srv.URL
}

func (c *fakeCell) handle(w http.ResponseWriter, r *http.Request) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	if r.URL.Path == "/v1/whoami" {
		acct, ok := c.whoami[bearer]
		if !ok {
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"principal": map[string]string{
				"operator_id": "opr_" + acct,
				"account_id":  acct,
			},
		})
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v1/accounts/") && strings.HasSuffix(r.URL.Path, ":plan") {
		if bearer != c.provisionToken {
			http.Error(w, `{"error":"bad provision token"}`, http.StatusUnauthorized)
			return
		}
		accountID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/accounts/"), ":plan")
		if r.Method == http.MethodGet {
			c.mu.Lock()
			body, ok := c.applied[accountID]
			c.mu.Unlock()
			if !ok {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"decode"}`, http.StatusBadRequest)
			return
		}
		body["account_id"] = accountID
		body["applied_at"] = nil
		c.mu.Lock()
		c.applied[accountID] = body
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(body)
		return
	}

	http.NotFound(w, r)
}

func (c *fakeCell) snapshot(accountID string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.applied[accountID]; ok {
		return v.(map[string]any)
	}
	return nil
}

func TestCellApplierPushesSnapshot(t *testing.T) {
	cell, url := newFakeCell(t, "witself_prv_test", nil)
	applier := NewCellApplier(StaticCell(url, "witself_prv_test"))
	request := lifecycle.ApplyRequest{Revision: 7, PlanSnapshot: lifecycle.PlanSnapshot{
		Plan: "standard", Hash: strings.Repeat("a", 64),
		Limits:   map[string]int64{"agents": 250, "realms": 10},
		Policies: map[string]int64{"transcript_retention_days": 90},
		Features: []string{"memory", "facts", "secrets"},
	}}
	_, err := applier.Apply(context.Background(), "acct_1", request)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	snap := cell.snapshot("acct_1")
	if snap == nil || snap["plan"] != "standard" {
		t.Fatalf("cell did not receive the snapshot: %v", snap)
	}
	if limits, ok := snap["limits"].(map[string]any); !ok || limits["agents"].(float64) != 250 {
		t.Fatalf("limits missing/wrong: %v", snap["limits"])
	}
	if feats, ok := snap["features"].([]any); !ok || feats[2] != "secrets" {
		t.Fatalf("features missing/wrong: %v", snap["features"])
	}
	if policies, ok := snap["policies"].(map[string]any); !ok || policies["transcript_retention_days"].(float64) != 90 {
		t.Fatalf("policies missing/wrong: %v", snap["policies"])
	}
	reader, ok := applier.(lifecycle.ApplyFenceReader)
	if !ok {
		t.Fatal("production cell applier has no fence read side")
	}
	fence, err := reader.ReadApplyFence(context.Background(), "acct_1")
	if err != nil || fence.Revision != 7 || fence.Hash != request.Hash {
		t.Fatalf("ReadApplyFence = %+v, %v", fence, err)
	}
}

func TestBridgeApplierReadsValueMinimalFence(t *testing.T) {
	hash := strings.Repeat("b", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet ||
			r.URL.Path != "/v1/internal/accounts/acct_1:apply-plan" ||
			r.Header.Get("Authorization") != "Bearer bridge-secret" {
			http.Error(w, `{"error":"bad bridge request"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account_id":    "acct_1",
			"revision":      41,
			"snapshot_hash": hash,
			// Payload is present because this relays the cell GET, but the
			// bridge applier must return only the fence.
			"plan":     "stale-cell-plan",
			"limits":   map[string]int64{"agents": 999},
			"policies": map[string]int64{"transcript_retention_days": 999},
			"features": []string{"stale-cell-feature"},
		})
	}))
	t.Cleanup(srv.Close)

	applier := NewBridgeApplier(srv.URL, "bridge-secret")
	reader, ok := applier.(lifecycle.ApplyFenceReader)
	if !ok {
		t.Fatal("production bridge applier has no fence read side")
	}
	fence, err := reader.ReadApplyFence(context.Background(), "acct_1")
	if err != nil || fence.Revision != 41 || fence.Hash != hash {
		t.Fatalf("ReadApplyFence = %+v, %v", fence, err)
	}
}

func TestBridgeAccountExistsChecksCellFence(t *testing.T) {
	hash := strings.Repeat("c", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet ||
			r.Header.Get("Authorization") != "Bearer bridge-secret" {
			http.Error(w, `{"error":"bad bridge request"}`, http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/internal/accounts/acct_present:apply-plan":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"account_id":    "acct_present",
				"revision":      9,
				"snapshot_hash": hash,
			})
		case "/v1/internal/accounts/acct_missing:apply-plan":
			http.Error(w, `{"error":"account not found"}`, http.StatusNotFound)
		case "/v1/internal/accounts/acct_unavailable:apply-plan":
			http.Error(w, `{"error":"cell unavailable"}`, http.StatusBadGateway)
		default:
			http.Error(w, `{"error":"unexpected bridge path"}`, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	exists := BridgeAccountExists(srv.URL, "bridge-secret")
	ctx := context.Background()
	ok, err := exists(ctx, "acct_present")
	if err != nil || !ok {
		t.Fatalf("present account = (%v, %v), want (true, nil)", ok, err)
	}
	ok, err = exists(ctx, "acct_missing")
	if err != nil || ok {
		t.Fatalf("missing account = (%v, %v), want (false, nil)", ok, err)
	}
	if _, err := exists(ctx, "acct_unavailable"); err == nil {
		t.Fatal("cell/bridge failure must propagate instead of reading as absent")
	}
}

func TestCellApplierPropagatesErrors(t *testing.T) {
	_, url := newFakeCell(t, "witself_prv_test", nil)
	// Wrong provision token: the cell 401s, the Applier fails, Reconcile
	// will retry — Applied stays behind Entitled, truthfully.
	applier := NewCellApplier(StaticCell(url, "WRONG"))
	request := lifecycle.ApplyRequest{Revision: 1, PlanSnapshot: lifecycle.PlanSnapshot{
		Plan: "standard", Hash: strings.Repeat("a", 64),
		Limits: map[string]int64{}, Policies: map[string]int64{}, Features: []string{},
	}}
	if _, err := applier.Apply(context.Background(), "acct_1", request); err == nil {
		t.Fatal("bad provision token must fail; Reconcile retries — never silently swallow")
	}
}

func TestCellApplierRejectsLegacyCellAcknowledgement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// An older cell ignores the new policies/revision fields and returns
		// its historical generic success document. The control plane must not
		// record the new snapshot as applied.
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0"}`))
	}))
	t.Cleanup(srv.Close)
	applier := NewCellApplier(StaticCell(srv.URL, "witself_prv_test"))
	request := lifecycle.ApplyRequest{Revision: 1, PlanSnapshot: lifecycle.PlanSnapshot{
		Plan: "free", Hash: strings.Repeat("a", 64),
		Limits: map[string]int64{}, Policies: map[string]int64{}, Features: []string{},
	}}
	if _, err := applier.Apply(context.Background(), "acct_1", request); err == nil {
		t.Fatal("legacy generic success was accepted as an exact snapshot acknowledgement")
	}
}

func TestCellAuthenticateGranularity(t *testing.T) {
	_, url := newFakeCell(t, "witself_prv_test", map[string]string{
		"opr_token_a": "acct_A",
		"opr_token_b": "acct_B",
	})
	auth := CellAuthenticate(StaticCell(url, "witself_prv_test"))
	ctx := context.Background()

	ok, err := auth(ctx, "acct_A", "opr_token_a")
	if err != nil || !ok {
		t.Fatalf("A's token on A = (%v, %v); want true", ok, err)
	}
	// Different account: the cell says the token belongs to acct_A, so the
	// CP refuses. The cross-account leak the interim dev-token had is closed.
	if ok, _ := auth(ctx, "acct_B", "opr_token_a"); ok {
		t.Fatal("A's token on B was authorized — cross-account leak")
	}
	if ok, _ := auth(ctx, "acct_A", "bogus"); ok {
		t.Fatal("invalid token was authorized")
	}
}

// TestCellAuthenticateDistinguishesTransportFromAuth: a cell blip must not
// present as a 403 fleet-wide auth incident — transport/5xx errors bubble up
// (→ 500), only real auth denial reads as unauthorized.
func TestCellAuthenticateDistinguishesTransportFromAuth(t *testing.T) {
	// Point the resolver at a black hole to force a transport error.
	auth := CellAuthenticate(StaticCell("http://127.0.0.1:1", "witself_prv_test"))
	_, err := auth(context.Background(), "acct_1", "opr_x")
	if err == nil {
		t.Fatal("transport failure must propagate as an error (→ 500), not silently 403")
	}

	// A real 401 from the cell reads as "not authorized" (false, nil).
	_, url := newFakeCell(t, "witself_prv_test", map[string]string{}) // no operators known
	auth = CellAuthenticate(StaticCell(url, "witself_prv_test"))
	ok, err := auth(context.Background(), "acct_1", "bogus")
	if err != nil || ok {
		t.Fatalf("invalid token = (%v, %v); want (false, nil)", ok, err)
	}
}
