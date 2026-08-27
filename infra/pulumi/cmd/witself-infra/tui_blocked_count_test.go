package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

// TestLiveLoadBlockedCountFromUnplaced pins that liveDataSource.load
// maps the control plane's scalar archived.unplaced total into
// info.blocked. Archived.Blocked is a size-capped sample bounded by
// the limit query param — the header loads with limit=1, so
// len(ps.Archived.Blocked) saturates at 1 no matter how many archives
// are unplaceable. That was the bug: seven blocked accounts rendered
// as "1 no eligible cell".
func TestLiveLoadBlockedCountFromUnplaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/cells":
			_ = json.NewEncoder(w).Encode(map[string]any{"cells": []any{}})
		case "/v1/placement-status":
			// Seven unplaceable archives, but the limit-bounded
			// sample list carries just one of them.
			_ = json.NewEncoder(w).Encode(fleet.PlacementStatus{
				Archived: fleet.PlacementArchiveStatus{
					Total:     50,
					Placeable: 43,
					Unplaced:  7,
					Blocked: []fleet.BlockedAccount{{
						AccountID: "acc_sample",
						Reason:    "no eligible cell",
					}},
				},
				Live: fleet.PlacementLiveStatus{Total: 100},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfgPath := filepath.Join(t.TempDir(), "infra.yaml")
	cfg := "version: 1\ndefaults:\n  control_plane: " + srv.URL +
		"\n  fleet_token_file: " + writeFleetToken(t) + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := liveDataSource{}.load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	info, ok := res.controlPlanes[srv.URL]
	if !ok {
		t.Fatalf("no controlPlaneInfo for %s: %#v", srv.URL, res.controlPlanes)
	}
	if !info.hasAccounts {
		t.Fatal("hasAccounts = false, want true")
	}
	if info.blocked != 7 {
		t.Fatalf("info.blocked = %d, want 7 (scalar Archived.Unplaced, not len(sample))", info.blocked)
	}
	if info.archived != 50 || info.liveAccts != 100 {
		t.Fatalf("archived/live = %d/%d, want 50/100", info.archived, info.liveAccts)
	}
}

// TestCPHeaderRendersBlockedTotal pins the render half of the same
// path: a controlPlaneInfo carrying blocked=7 must surface as
// "7 no eligible cell" in the CP header context view.
func TestCPHeaderRendersBlockedTotal(t *testing.T) {
	const cp = "https://cp.example"
	m := dashboardModel{
		controlPlanes: map[string]controlPlaneInfo{
			cp: {
				url:         cp,
				reachable:   true,
				hasAccounts: true,
				liveAccts:   100,
				archived:    50,
				blocked:     7,
			},
		},
	}
	out := stripANSIForTest(strings.Join(m.renderCPContext(row{kind: rowHeader, cp: cp}, 100), "\n"))
	if !strings.Contains(out, "7 no eligible cell") {
		t.Fatalf("CP context must render the blocked total, got:\n%s", out)
	}
}
