package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// TestMemoryCurationInputsMaxBytesHTTP pins the max_bytes query parameter of
// the run-input page route (issue #649): a value in range reaches the
// callback as sent, its absence reaches the callback as zero, and a value
// outside 8192-65536 or not an integer is a 400 with an exact message before
// the callback runs.
func TestMemoryCurationInputsMaxBytesHTTP(t *testing.T) {
	var seen []int
	cfg := Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
			return DomainPrincipal{
				Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1",
				RealmID: "realm_1", AccountStatus: "active",
			}, true, nil
		},
		GetMemoryCurationRunInputs: func(_ context.Context, _ DomainPrincipal, _ string, opts MemoryCurationRunInputOptions) (any, error) {
			seen = append(seen, opts.MaxBytes)
			return map[string]any{"run": map[string]any{"id": "mrun_1"}, "inputs": []any{}}, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()
	const base = "/v1/memory-curation-runs/mrun_1/inputs?fencing_generation=7"

	for _, query := range []string{"", "&max_bytes=24576", "&max_bytes=8192", "&max_bytes=65536"} {
		response := memoryCurationHTTPResponse(t, srv.URL, "agent", http.MethodGet, base+query, "", "")
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%q: status %d, want 200", query, response.StatusCode)
		}
	}
	if want := []int{0, 24576, 8192, 65536}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("callback saw max_bytes %v, want %v", seen, want)
	}

	const wantError = "max_bytes must be between 8192 and 65536"
	for _, query := range []string{"&max_bytes=8191", "&max_bytes=65537", "&max_bytes=-1", "&max_bytes=24k"} {
		response := memoryCurationHTTPResponse(t, srv.URL, "agent", http.MethodGet, base+query, "", "")
		var body map[string]any
		err := json.NewDecoder(response.Body).Decode(&body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatalf("%q: decode body: %v", query, err)
		}
		if response.StatusCode != http.StatusBadRequest || body["error"] != wantError ||
			body["schema_version"] != "witself.v0" {
			t.Fatalf("%q: status %d body %v, want 400 %q", query, response.StatusCode, body, wantError)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("callback ran %d times after the refused reads, want 4", len(seen))
	}
}
