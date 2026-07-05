package cpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"decode"}`, http.StatusBadRequest)
			return
		}
		body["_account"] = accountID
		c.mu.Lock()
		c.applied[accountID] = body
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"schema_version":"witself.v0"}`)
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
	err := applier.Apply(context.Background(), "acct_1", "standard",
		map[string]int64{"agents": 250, "realms": 10},
		[]string{"memory", "facts", "secrets"})
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
}

func TestCellApplierPropagatesErrors(t *testing.T) {
	_, url := newFakeCell(t, "witself_prv_test", nil)
	// Wrong provision token: the cell 401s, the Applier fails, Reconcile
	// will retry — Applied stays behind Entitled, truthfully.
	applier := NewCellApplier(StaticCell(url, "WRONG"))
	if err := applier.Apply(context.Background(), "acct_1", "standard", nil, nil); err == nil {
		t.Fatal("bad provision token must fail; Reconcile retries — never silently swallow")
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
