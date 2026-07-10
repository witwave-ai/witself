package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRescueArchivedPlacement(t *testing.T) {
	var got struct {
		Axes []string `json:"axes"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/placement/archives/acc_1:rescue" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer witself_flt_test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account_id":   "acc_1",
			"changed":      true,
			"cleared_axes": got.Axes,
			"placement_policy": map[string]any{
				"preferred_clouds":   []string{"gcp"},
				"preferred_regions":  []string{},
				"preferred_channels": []string{},
				"allowed_clouds":     []string{},
				"allowed_regions":    []string{},
				"allowed_channels":   []string{},
				"rebalance_on":       []string{"cloud"},
			},
		})
	}))
	defer srv.Close()

	res, err := RescueArchivedPlacement(
		context.Background(), srv.URL, "witself_flt_test", "acc_1", []string{"region", "channel"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.AccountID != "acc_1" {
		t.Fatalf("result = %#v", res)
	}
	if len(got.Axes) != 2 || got.Axes[0] != "region" || got.Axes[1] != "channel" {
		t.Fatalf("axes = %#v", got.Axes)
	}
}
