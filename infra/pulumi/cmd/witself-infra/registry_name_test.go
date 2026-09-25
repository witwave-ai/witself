package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

const mappedInventoryCell = "civo-sandbox-use1-serving"
const mappedRegistryCell = "civo-sandbox-usw2-dev"

func registryTestConfig(t *testing.T, cp string) string {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("WITSELF_FLEET_TOKEN", "synthetic-fleet-token")
	// An unsupported synthetic cloud makes dashboard whoami fail locally.
	return writeConfig(t, fmt.Sprintf(`version: 1
defaults:
  control_plane: %s
cells:
  %s:
    registry_name: %s
    cloud: synthetic
    deletion_protection: false
`, cp, mappedInventoryCell, mappedRegistryCell))
}

type registryHealthClient struct{ names []string }

func (c *registryHealthClient) ListCells(context.Context) ([]fleet.Cell, error) { return nil, nil }
func (c *registryHealthClient) Probe(_ context.Context, name string) (fleet.ProbeResult, error) {
	c.names = append(c.names, name)
	return fleet.ProbeResult{OK: name == mappedRegistryCell}, nil
}

func TestHealthRegistryNameRoutingAndOutput(t *testing.T) {
	path := registryTestConfig(t, "https://control.invalid")
	client := &registryHealthClient{}
	targets, err := configuredHealthTargets(path, mappedInventoryCell, func(_, _ string) (fleetHealthClient, error) { return client, nil })
	if err != nil {
		t.Fatal(err)
	}
	results := probeAutomationHealth(context.Background(), targets, time.Second, time.Now)
	if !reflect.DeepEqual(client.names, []string{mappedRegistryCell}) {
		t.Fatalf("probes = %v", client.names)
	}
	var mapped automationHealthResult
	for _, result := range results {
		if result.Name == mappedInventoryCell {
			mapped = result
		}
	}
	if mapped.RegistryName != mappedRegistryCell || mapped.State != automationHealthOK {
		t.Fatalf("mapped result = %+v", mapped)
	}
	for _, jsonOutput := range []bool{true, false} {
		var out bytes.Buffer
		if err := emitAutomationHealth(results, jsonOutput, &out); err != nil {
			t.Fatal(err)
		}
		if jsonOutput {
			dec := json.NewDecoder(&out)
			for range results {
				var row map[string]any
				if err := dec.Decode(&row); err != nil {
					t.Fatal(err)
				}
				if row["name"] == mappedInventoryCell {
					if row["registry_name"] != mappedRegistryCell {
						t.Fatalf("JSON row = %v", row)
					}
				} else if _, present := row["registry_name"]; present {
					t.Fatal("control plane acquired registry name")
				}
			}
		} else if !strings.Contains(out.String(), mappedInventoryCell+" (registry "+mappedRegistryCell+")") {
			t.Fatalf("text = %s", out.String())
		}
	}
	if _, err := configuredHealthTargets(path, mappedRegistryCell, nil); err == nil {
		t.Fatal("selection must still use inventory key")
	}
}

func TestHealthRegistryNameWithoutControlPlane(t *testing.T) {
	path := writeConfig(t, fmt.Sprintf("version: 1\ncells:\n  %s: {registry_name: %s}\n", mappedInventoryCell, mappedRegistryCell))
	targets, err := configuredHealthTargets(path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	results := probeAutomationHealth(context.Background(), targets, time.Second, time.Now)
	if len(results) != 1 || results[0].RegistryName != mappedRegistryCell || results[0].State != automationHealthDown {
		t.Fatalf("results = %+v", results)
	}
}

func TestDashboardRegistryNameLookupAndProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/cells":
			_ = json.NewEncoder(w).Encode(map[string]any{"cells": []fleet.Cell{{Name: mappedRegistryCell}, {Name: "orphan-cell"}}})
		case "/v1/placement-status":
			_, _ = w.Write([]byte(`{}`))
		case "/v1/cells/" + mappedRegistryCell + ":probe", "/v1/cells/orphan-cell:probe":
			_ = json.NewEncoder(w).Encode(fleet.ProbeResult{OK: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	path := registryTestConfig(t, srv.URL)
	result, err := (liveDataSource{}).load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.states) != 2 {
		t.Fatalf("states = %d, mapping became an orphan", len(result.states))
	}
	st := result.states[0]
	if st.name != mappedInventoryCell || st.fleet == nil || st.fleet.Name != mappedRegistryCell {
		t.Fatalf("mapped state = %+v", st)
	}
	for _, name := range []string{mappedInventoryCell, "orphan-cell"} {
		probe, err := (liveDataSource{}).probe(context.Background(), path, name)
		if err != nil || !probe.ok {
			t.Fatalf("probe %s: %+v, %v", name, probe, err)
		}
	}
}

type destroyConfirmationReader func([]byte) (int, error)

func (f destroyConfirmationReader) Read(p []byte) (int, error) { return f(p) }

type destroyRequestLog struct {
	mu     sync.Mutex
	events []string
}

func (l *destroyRequestLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *destroyRequestLog) Write(p []byte) (int, error) {
	l.add("output: " + string(p))
	return len(p), nil
}

func TestDestroyRegistryNameAccountGuardAndConfirmation(t *testing.T) {
	path := registryTestConfig(t, "https://control.invalid")
	for _, typed := range []string{mappedInventoryCell, mappedRegistryCell} {
		var out bytes.Buffer
		answered := false
		opts := destroySafetyOptions{
			ConfigPath: path, Interactive: true, Output: &out,
			ControlPlane:       "https://control.invalid",
			PlacementStatusAPI: &fakePlacementStatusReader{},
		}
		var status fleet.PlacementStatus
		if err := json.Unmarshal([]byte(fmt.Sprintf(`{"cells":[{"name":%q,"account_count":0,"archived_count":0}]}`, mappedRegistryCell)), &status); err != nil {
			t.Fatal(err)
		}
		opts.PlacementStatusAPI = &fakePlacementStatusReader{status: status}
		opts.Input = destroyConfirmationReader(func(p []byte) (int, error) {
			want := fmt.Sprintf("Destroy target %q (fleet registry entry %q). Type the exact cell name to confirm: ", mappedInventoryCell, mappedRegistryCell)
			if out.String() != want {
				t.Fatalf("prompt before input = %q, want %q", out.String(), want)
			}
			answered = true
			return copy(p, typed+"\n"), io.EOF
		})
		err := runDestroySafety(context.Background(), mappedInventoryCell, opts)
		if !answered || (err == nil) != (typed == mappedInventoryCell) {
			t.Fatalf("typed %q: answered=%t, err=%v", typed, answered, err)
		}
	}
	for _, live := range []int{0, 2} {
		var status fleet.PlacementStatus
		if err := json.Unmarshal([]byte(fmt.Sprintf(`{"cells":[{"name":%q,"account_count":%d,"archived_count":0}]}`, mappedRegistryCell, live)), &status); err != nil {
			t.Fatal(err)
		}
		opts := destroySafetyOptions{ConfigPath: path, ControlPlane: "https://control.invalid", YesCell: mappedInventoryCell, PlacementStatusAPI: &fakePlacementStatusReader{status: status}}
		err := runDestroySafety(context.Background(), mappedInventoryCell, opts)
		if live == 0 && err != nil {
			t.Fatal(err)
		}
		if live > 0 && (err == nil || !strings.Contains(err.Error(), "2 live account(s)") || !strings.Contains(err.Error(), mappedInventoryCell) || !strings.Contains(err.Error(), mappedRegistryCell)) {
			t.Fatalf("account guard = %v", err)
		}
		opts.ForceWithAccounts = true
		opts.YesCell = mappedRegistryCell
		if err := runDestroySafety(context.Background(), mappedInventoryCell, opts); err == nil || !strings.Contains(err.Error(), "--yes-cell") {
			t.Fatalf("registry name must not confirm stack destruction: %v", err)
		}
	}
}

func TestDestroyRegistryNamePreflightAndRemoval(t *testing.T) {
	for _, purge := range []bool{false, true} {
		t.Run(fmt.Sprintf("purge=%t", purge), func(t *testing.T) {
			// A backup target exercises the acknowledged registration path.
			// Ordinary drain has a pre-existing typed-nil decode bug in fleet.Register.
			calls := &destroyRequestLog{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.add(r.Method + " " + r.URL.Path)
				switch r.URL.Path {
				case "/v1/placement-status":
					_, _ = fmt.Fprintf(w, `{"cells":[{"name":%q,"account_count":0,"archived_count":0}]}`, mappedRegistryCell)
				case "/v1/cells":
					if r.Method == http.MethodGet {
						_ = json.NewEncoder(w).Encode(map[string]any{"cells": []fleet.Cell{{Name: mappedRegistryCell, BackupValidationTarget: true}}})
					} else {
						var cell fleet.Cell
						if err := json.NewDecoder(r.Body).Decode(&cell); err != nil || cell.Name != mappedRegistryCell || cell.Accepting == nil || *cell.Accepting {
							t.Error("drain must re-register the existing registry identity with accepting=false")
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "cell": cell})
					}
				case "/v1/cells/" + mappedRegistryCell + ":purge":
					_, _ = w.Write([]byte(`{}`))
				case "/v1/cells/" + mappedRegistryCell:
					w.WriteHeader(http.StatusNoContent)
				case "/v1/cells/" + mappedRegistryCell + ":evacuate":
					_, _ = w.Write([]byte(`{"remaining":0}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			path := registryTestConfig(t, srv.URL)
			if err := runDestroySafety(context.Background(), mappedInventoryCell, destroySafetyOptions{
				ConfigPath: path, ControlPlane: srv.URL, YesCell: mappedInventoryCell,
				SkipAccountCheck: true, Output: calls,
			}); err != nil {
				t.Fatal(err)
			}
			if err := (liveDataSource{}).destroyPreflight(context.Background(), path, mappedInventoryCell); err != nil {
				t.Fatal(err)
			}
			if err := removeConfiguredCell(context.Background(), path, srv.URL, "", mappedInventoryCell, purge, calls); err != nil {
				t.Fatal(err)
			}
			disclosure := fmt.Sprintf("output: Destroy target %q (fleet registry entry %q).\n", mappedInventoryCell, mappedRegistryCell)
			want := []string{
				fmt.Sprintf("output: warning: --skip-account-check bypassed live/archived account placement verification for cell %q (fleet registry entry %q)\n", mappedInventoryCell, mappedRegistryCell),
				disclosure, "GET /v1/placement-status", disclosure, "GET /v1/cells", "POST /v1/cells",
			}
			if purge {
				want = append(want, "POST /v1/cells/"+mappedRegistryCell+":purge")
			} else {
				want = append(want, "POST /v1/cells/"+mappedRegistryCell+":evacuate", "DELETE /v1/cells/"+mappedRegistryCell)
			}
			calls.mu.Lock()
			defer calls.mu.Unlock()
			if !reflect.DeepEqual(calls.events, want) {
				t.Fatalf("calls = %v, want %v", calls.events, want)
			}
		})
	}
}

func TestDestroyAccountDiagnosticIdentity(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		registryName := destroyTestCell
		identity := fmt.Sprintf("%q", destroyTestCell)
		if mapped {
			registryName = mappedRegistryCell
			identity += fmt.Sprintf(" (fleet registry entry %q)", registryName)
		}
		var status fleet.PlacementStatus
		if err := json.Unmarshal([]byte(fmt.Sprintf(`{"cells":[{"name":%q,"account_count":2,"archived_count":3}]}`, registryName)), &status); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name                     string
			force, skip, unavailable bool
			want                     string
			wantErr                  bool
		}{
			{"skip", false, true, false, "warning: --skip-account-check bypassed live/archived account placement verification for cell " + identity + "\n", false},
			{"force", true, false, false, "warning: --force-with-accounts permits destroy of cell " + identity + " with 2 live account(s) and 3 archived account(s) still placed there\n", false},
			{"refuse", false, false, false, "live-accounts protection: cell " + identity + " has 2 live account(s) and 3 archived account(s) still placed there; refusing destroy (pass --force-with-accounts to proceed)", true},
			{"unavailable", false, false, true, "live-accounts protection: account placement status is unavailable for cell " + identity + " (no control plane is configured); refusing destroy (pass --skip-account-check to bypass)", true},
		} {
			t.Run(fmt.Sprintf("mapped=%t/%s", mapped, tc.name), func(t *testing.T) {
				cp := "https://control.invalid"
				if tc.unavailable {
					cp = ""
				}
				var out bytes.Buffer
				err := checkDestroyAccounts(context.Background(), destroyTestCell, registryName, cp, "", tc.force, tc.skip, &fakePlacementStatusReader{status: status}, &out)
				got := out.String()
				if tc.wantErr {
					if err == nil {
						t.Fatal("expected refusal")
					}
					got = err.Error()
				} else if err != nil {
					t.Fatal(err)
				}
				if got != tc.want {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			})
		}
	}
}

func TestDestroyUnmappedOutputUnchanged(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("WITSELF_FLEET_TOKEN", "synthetic-fleet-token")
	path := writeConfig(t, fmt.Sprintf("version: 1\ncells:\n  %s: {deletion_protection: false}\n", destroyTestCell))
	for _, interactive := range []bool{false, true} {
		var out bytes.Buffer
		err := runDestroySafety(context.Background(), destroyTestCell, destroySafetyOptions{
			ConfigPath: path, SkipAccountCheck: true, YesCell: destroyTestCell,
			Interactive: interactive, Input: strings.NewReader(destroyTestCell + "\n"), Output: &out,
		})
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("warning: --skip-account-check bypassed live/archived account placement verification for cell %q\n", destroyTestCell)
		if interactive {
			want += fmt.Sprintf("Destroy target %q. Type the exact cell name to confirm: ", destroyTestCell)
		}
		if out.String() != want {
			t.Fatalf("output = %q, want %q", out.String(), want)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"cells":[]}`))
	}))
	defer srv.Close()
	var out bytes.Buffer
	if err := removeConfiguredCell(context.Background(), path, srv.URL, "", destroyTestCell, false, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("unmapped removal added output: %q", out.String())
	}
}

func TestDestroyRegistryNameDialogText(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		entry := cellEntry{}
		if mapped {
			name := mappedRegistryCell
			entry.RegistryName = &name
		}
		m := seedModel([]cellState{{name: mappedInventoryCell, entry: entry}}, 120, 30)
		m.destroyChecking = mappedInventoryCell
		next, _ := m.Update(destroyPreflightMsg{cell: mappedInventoryCell})
		m = next.(dashboardModel)
		if m.pending == nil {
			t.Fatal("preflight did not open dialog")
		}
		want := "DESTROY " + mappedInventoryCell + "\n\n"
		if mapped {
			want += fmt.Sprintf("Destroy target %q (fleet registry entry %q).\n\n", mappedInventoryCell, mappedRegistryCell)
		}
		want += "destroy will DRAIN the cell, EVACUATE every account to R2, then DELETE the fleet entry and tear down every cloud resource.\n\ntype `" + mappedInventoryCell + "` then enter to confirm:\n  ▏\n\n\n"
		if got := m.pending.render(); got != want {
			t.Fatalf("dialog = %q, want %q", got, want)
		}
		m.pending.typed = mappedRegistryCell
		if m.pending.canConfirm() {
			t.Fatal("registry name must not confirm destruction")
		}
		m.pending.typed = mappedInventoryCell
		if !m.pending.canConfirm() {
			t.Fatal("inventory key must confirm destruction")
		}
	}
}
