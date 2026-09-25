package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
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
				case "GET /v1/cells":
					_ = json.NewEncoder(w).Encode(map[string]any{"cells": []any{cell}})
				case "POST /v1/cells":
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode registration: %v", err)
						http.Error(w, "bad request", http.StatusBadRequest)
						return
					}
					if body["name"] != "cell-a" || body["accepting"] != false || body["backup_validation_target"] != tc.backupTarget {
						t.Error("drain must preserve identity and purpose and set accepting=false")
					}
					for _, key := range []string{"provision_token", "backup_token"} {
						if _, present := body[key]; present {
							t.Errorf("drain must omit %s", key)
						}
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
			want := []string{"GET /v1/cells", "POST /v1/cells"}
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
