package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func lifecycleGolden(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile("../../infra/cloudflare/control-plane/test/fixtures/account-lifecycle-status.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Cases map[string]json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	return golden.Cases
}
func lifecycleBody(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(lifecycleGolden(t)["import_running"], &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func lifecycleGet(t *testing.T, body any) (*FleetAccountLifecycle, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(body) }))
	defer server.Close()
	return GetFleetAccountLifecycle(context.Background(), server.URL, "fixture-fleet", "acct_runtime")
}
func lifecycleObject(t *testing.T, root map[string]any, path string) (map[string]any, string) {
	t.Helper()
	parts := strings.Split(path, ".")
	current := root
	for _, key := range parts[:len(parts)-1] {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("missing object at %s", key)
		}
		current = next
	}
	return current, parts[len(parts)-1]
}
func lifecycleSet(t *testing.T, root map[string]any, path string, value any) {
	t.Helper()
	object, key := lifecycleObject(t, root, path)
	object[key] = value
}

func TestFleetAccountLifecycleRequest(t *testing.T) {
	body := lifecycleBody(t)
	body["private_future"] = "private-marker"
	body["operation"].(map[string]any)["archive"] = "private-marker"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.RequestURI() != "/v1/placement/accounts/acct_runtime/lifecycle" || r.Header.Get("Authorization") != "Bearer fixture-fleet" {
			t.Error("incorrect request shape")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil || len(raw) != 0 {
			t.Error("unexpected request body")
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	result, err := GetFleetAccountLifecycle(context.Background(), server.URL, "fixture-fleet", "acct_runtime")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-marker") || calls != 1 {
		t.Fatal("unknown fields leaked or incorrect call count")
	}
	for _, id := range []string{"", "with space", "with/slash", strings.Repeat("a", 129)} {
		if _, err := GetFleetAccountLifecycle(context.Background(), server.URL, "fixture-fleet", id); err == nil {
			t.Fatal("invalid id accepted")
		}
	}
	for _, endpoint := range []string{server.URL + "/path", server.URL + "?query", server.URL + "#fragment", "not-url"} {
		if _, err := GetFleetAccountLifecycle(context.Background(), endpoint, "fixture-fleet", "acct_runtime"); err == nil {
			t.Fatal("invalid endpoint accepted")
		}
	}
	if calls != 1 {
		t.Fatal("invalid arguments made a request")
	}
}

func TestFleetAccountLifecycleReject(t *testing.T) {
	for _, tc := range []struct {
		path  string
		value any
	}{
		{"schema_version", "future"}, {"account_id", "other"}, {"initialized", nil}, {"driver_active", nil}, {"observed_at", nil},
		{"revision", nil}, {"epoch", nil}, {"location", nil},
		{"operation.operation_id", ""}, {"operation.kind", ""}, {"operation.phase", ""},
		{"operation.operation_id", "private/id"}, {"operation.evacuation_id", "private.id"},
		{"operation.source_registration_id", "bad space"}, {"operation.target_registration_id", "bad/slash"},
		{"location.cell", "Bad"}, {"directory.live_cell", "bad space"}, {"directory.archived_cell", "bad/slash"},
		{"operation.source_cell", "Bad"}, {"operation.target_cell", "bad space"},
		{"revision", -1}, {"epoch", -1}, {"operation.epoch", -1}, {"operation.request_epoch", -1},
		{"operation.import_job.attempts", -1}, {"operation.import_job.manifest_schema_version", -1},
	} {
		t.Run(tc.path+"/"+strings.ReplaceAll(stringValue(tc.value), "/", "_"), func(t *testing.T) {
			body := lifecycleBody(t)
			lifecycleSet(t, body, tc.path, tc.value)
			_, err := lifecycleGet(t, body)
			if err == nil || err.Error() != "invalid account lifecycle status" {
				t.Fatalf("expected fixed rejection, got %v", err)
			}
		})
	}
	for _, tc := range []struct {
		path  string
		value any
	}{{"operation_id", ""}, {"kind", ""}, {"operation_id", "bad space"}, {"evacuation_id", "bad.id"}, {"source_cell", "Bad"}, {"target_cell", "Bad"}, {"epoch", -1}, {"completed_revision", -1}, {"final_location.cell", "Bad"}} {
		t.Run("completed/"+tc.path, func(t *testing.T) {
			body := lifecycleBody(t)
			var completed map[string]any
			if err := json.Unmarshal(lifecycleGolden(t)["restore_completed"], &completed); err != nil {
				t.Fatal(err)
			}
			body["last_completed"] = completed["last_completed"]
			lifecycleSet(t, body, "last_completed."+tc.path, tc.value)
			if _, err := lifecycleGet(t, body); err == nil || err.Error() != "invalid account lifecycle status" {
				t.Fatal("expected completed rejection")
			}
		})
	}
	t.Run("oversize", func(t *testing.T) {
		body := lifecycleBody(t)
		body["unknown"] = strings.Repeat("x", 64*1024)
		if _, err := lifecycleGet(t, body); err == nil {
			t.Fatal("unbounded success body")
		}
	})
}
func stringValue(value any) string {
	if value == nil {
		return "null"
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func TestFleetAccountLifecycleReplace(t *testing.T) {
	for _, tc := range []struct {
		path  string
		value any
		want  any
	}{
		{"location.kind", "future", "unknown"}, {"operation.kind", "future", "unknown"}, {"operation.phase", "bad/phase", "unknown"},
		{"operation.next_step.type", "bad/type", "unknown"}, {"operation.next_step.action", "bad/action", "unknown"}, {"operation.next_step.target", "bad/target", "unknown"},
		{"operation.imported_status", "future", "unknown"}, {"operation.restored_status", "future", "unknown"}, {"operation.source_finalization", "future", "unknown"},
		{"operation.target_protocol", 0, nil}, {"operation.target_protocol", 100, nil},
		{"operation.last_error", "private failure", lifecycleUnrecognizedError},
		{"operation.import_job.next_alarm_action", "future", "unknown"},
		{"last_completed.kind", "future", "unknown"}, {"last_completed.outcome", "future", "unknown"}, {"last_completed.final_location.kind", "future", "unknown"},
		{"restore_quarantine.reason", "private reason", "unknown"},
		{"projections.route.action", "future", "unknown"}, {"projections.route.status", "future", "unknown"},
	} {
		t.Run(tc.path+"/"+stringValue(tc.value), func(t *testing.T) {
			body := lifecycleBody(t)
			var q map[string]any
			if err := json.Unmarshal(lifecycleGolden(t)["quarantined"], &q); err != nil {
				t.Fatal(err)
			}
			body["last_completed"] = q["last_completed"]
			body["restore_quarantine"] = q["restore_quarantine"]
			body["projections"].(map[string]any)["route"] = map[string]any{"action": "put", "status": "pending"}
			lifecycleSet(t, body, tc.path, tc.value)
			result, err := lifecycleGet(t, body)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			object, key := lifecycleObject(t, got, tc.path)
			if !reflect.DeepEqual(object[key], tc.want) {
				t.Fatalf("replacement mismatch: %v", object[key])
			}
		})
	}
}

func TestFleetLifecycleErrorsMatchControlPlane(t *testing.T) {
	data, err := os.ReadFile("../../infra/cloudflare/control-plane/src/account-lifecycle-status.mjs")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)LIFECYCLE_STATUS_ERRORS = Object.freeze\(\[(.*?)\]\)`).FindSubmatch(data)
	if len(block) != 2 {
		t.Fatal("missing JS error list")
	}
	literals := regexp.MustCompile(`"([^"\n]+)"`).FindAllSubmatch(block[1], -1)
	if len(literals) != len(lifecycleErrors) || len(literals) != 11 {
		t.Fatal("error list length differs")
	}
	for i, literal := range literals {
		if string(literal[1]) != lifecycleErrors[i] {
			t.Fatal("error string differs")
		}
	}
}

func TestFleetAccountLifecycleGoldenContract(t *testing.T) {
	golden := lifecycleGolden(t)
	for _, name := range []string{"export_running", "projections_pending", "operation_attention"} {
		if _, ok := golden[name]; !ok {
			t.Fatalf("missing golden case %s", name)
		}
	}
	for name, raw := range golden {
		t.Run(name, func(t *testing.T) {
			var strict FleetAccountLifecycle
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&strict); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(raw) }))
			defer server.Close()
			result, err := GetFleetAccountLifecycle(context.Background(), server.URL, "fixture-fleet", strict.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result, &strict) {
				t.Fatal("golden required substitutions")
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			lifecycleCompareJSON(t, want, got)
		})
	}
}
func lifecycleCompareJSON(t *testing.T, want, got any) {
	t.Helper()
	if object, ok := want.(map[string]any); ok {
		actual, ok := got.(map[string]any)
		if !ok || len(object) != len(actual) {
			t.Fatal("contract key count differs")
		}
		for key, value := range object {
			other, exists := actual[key]
			if !exists {
				t.Fatalf("missing key %s", key)
			}
			lifecycleCompareJSON(t, value, other)
		}
		return
	}
	if s, ok := want.(string); ok {
		if instant, err := time.Parse(time.RFC3339Nano, s); err == nil {
			other, ok := got.(string)
			if !ok {
				t.Fatal("timestamp type differs")
			}
			parsed, err := time.Parse(time.RFC3339Nano, other)
			if err != nil || !instant.Equal(parsed) {
				t.Fatal("timestamp instant differs")
			}
			return
		}
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("contract leaf differs")
	}
}

func TestFleetAccountLifecycleExportCounters(t *testing.T) {
	for _, field := range []string{"stream_attempts", "verify_attempts", "stream_ms"} {
		for _, value := range []any{nil, float64(0), float64(3), float64(-1)} {
			t.Run(field+"/"+stringValue(value), func(t *testing.T) {
				body := lifecycleBody(t)
				body["operation"].(map[string]any)["export_job"] = map[string]any{field: value}
				result, err := lifecycleGet(t, body)
				if value == float64(-1) {
					if err == nil || err.Error() != "invalid account lifecycle status" {
						t.Fatal("negative export counter accepted or unsafe error")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(result.Operation.ExportJob)
				if err != nil {
					t.Fatal(err)
				}
				var job map[string]any
				if err := json.Unmarshal(data, &job); err != nil {
					t.Fatal(err)
				}
				if got, ok := job[field]; !ok || got != value {
					t.Fatal("export counter lost its nullable value")
				}
			})
		}
	}
}
