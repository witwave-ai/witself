package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

func TestDestroyRemovesRedactedFleetCell(t *testing.T) {
	for _, tc := range []struct {
		name            string
		backupTarget    bool
		destroyAccounts bool
	}{
		{name: "ordinary evacuate"},
		{name: "ordinary purge", destroyAccounts: true},
		{name: "backup target evacuate", backupTarget: true},
		{name: "backup target purge", backupTarget: true, destroyAccounts: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WITSELF_HOME", t.TempDir())
			t.Setenv("WITSELF_FLEET_TOKEN", "fleet-test-token")
			stack := &fakeDeploymentExporter{deployment: protectionDeployment(`{"resources":[]}`)}
			var requests []string
			drains, accounts := 0, 1
			deleted := false
			cell := map[string]any{
				"name": "cell-a", "endpoint": "https://cell.example.com",
				"accepting": !tc.backupTarget, "backup_validation_target": tc.backupTarget,
				"has_provision_token": true, "has_backup_token": true,
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request := r.Method + " " + r.URL.Path
				requests = append(requests, request)
				if stack.calls != 1 {
					t.Error("fleet request before persisted protection check")
				}
				switch request {
				case "PATCH /v1/cells/cell-a":
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode drain: %v", err)
						http.Error(w, "bad request", http.StatusBadRequest)
						return
					}
					if !reflect.DeepEqual(body, map[string]any{"accepting": false}) {
						t.Errorf("drain must send only accepting=false, got %v", body)
					}
					drains++
					cell["accepting"] = false
					_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "cell": cell})
				case "POST /v1/cells/cell-a:evacuate":
					if drains != 1 || cell["accepting"] != false {
						t.Error("evacuation must follow exactly one drain")
					}
					accounts = 0
					_ = json.NewEncoder(w).Encode(evacResponse{Evacuated: []ea{{"acc_test", true, ""}}, Remaining: 0})
				case "DELETE /v1/cells/cell-a":
					if drains != 1 || cell["accepting"] != false || accounts != 0 {
						t.Error("delete must follow drain and completed evacuation")
					}
					deleted = true
					w.WriteHeader(http.StatusNoContent)
				case "POST /v1/cells/cell-a:purge":
					if drains != 1 || cell["accepting"] != false {
						t.Error("purge must follow exactly one drain")
					}
					accounts = 0
					deleted = true
					_, _ = w.Write([]byte(`{"purged_accounts":1,"cell_deleted":true}`))
				default:
					t.Errorf("unexpected request: %s", request)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			ctx := context.Background()
			err := runDestroyAfterUnprotect(ctx, stack, "cell-a", false,
				func() error { return removeCell(ctx, server.URL, "", "cell-a", tc.destroyAccounts) },
				func() error {
					if !deleted || accounts != 0 {
						t.Error("infrastructure destroy reached before fleet removal completed")
					}
					requests = append(requests, "destroy")
					return nil
				})
			if err != nil {
				t.Fatalf("destroy flow: %v", err)
			}
			want := []string{"PATCH /v1/cells/cell-a"}
			if tc.destroyAccounts {
				want = append(want, "POST /v1/cells/cell-a:purge")
			} else {
				want = append(want, "POST /v1/cells/cell-a:evacuate", "DELETE /v1/cells/cell-a")
			}
			want = append(want, "destroy")
			if !reflect.DeepEqual(requests, want) || drains != 1 {
				t.Fatalf("requests = %v, want %v; drains = %d", requests, want, drains)
			}
		})
	}
}

func TestDestroyStopsOnUnconfirmedDrain(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		unsupported  bool
		unregistered bool
	}{
		{"unrecognized 404", http.StatusNotFound, "not found", true, false},
		{"unknown cell", http.StatusNotFound, `{"schema_version":"witself.v0","error":"unknown cell"}`, false, true},
		{"old method", http.StatusMethodNotAllowed, "method not allowed", true, false},
		{"server failure", http.StatusInternalServerError, "internal error", false, false},
		{"invalid ack", http.StatusOK, `{"schema_version":"witself.v0","cell":{"name":"cell-a","accepting":true,"backup_validation_target":false}}`, false, false},
	} {
		for _, purge := range []bool{false, true} {
			name := tc.name + "/evacuate"
			if purge {
				name = tc.name + "/purge"
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv("WITSELF_HOME", t.TempDir())
				t.Setenv("WITSELF_FLEET_TOKEN", "fleet-test-token")
				stack := &fakeDeploymentExporter{deployment: protectionDeployment(`{"resources":[]}`)}
				var requests []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests = append(requests, r.Method+" "+r.URL.Path)
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				defer server.Close()
				ctx := context.Background()
				destroyCalls := 0
				err := runDestroyAfterUnprotect(ctx, stack, "cell-a", false,
					func() error { return removeCell(ctx, server.URL, "", "cell-a", purge) },
					func() error { destroyCalls++; return nil })
				if tc.unregistered {
					if err != nil || destroyCalls != 1 {
						t.Fatalf("error = %v, destroy calls = %d; want unregistered cell to proceed once", err, destroyCalls)
					}
				} else {
					if err == nil || !strings.HasPrefix(err.Error(), "drain cell: ") || strings.Count(err.Error(), "drain cell:") != 1 {
						t.Fatalf("error = %v, want drain failure with exactly one drain cell: prefix", err)
					}
					if destroyCalls != 0 {
						t.Fatalf("infrastructure destroy reached after failed drain: %d calls", destroyCalls)
					}
				}
				if tc.unsupported {
					var unsupported *fleet.AcceptingPatchUnsupportedError
					if !errors.As(err, &unsupported) || unsupported.StatusCode != tc.status {
						t.Fatalf("error = %v, want preserved typed compatibility error", err)
					}
				}
				if want := []string{"PATCH /v1/cells/cell-a"}; !reflect.DeepEqual(requests, want) {
					t.Fatalf("requests = %v, want %v", requests, want)
				}
			})
		}
	}
}
