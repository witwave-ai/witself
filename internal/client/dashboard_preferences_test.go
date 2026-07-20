package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDashboardPreferencesClientRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	stored := DashboardPreferences{
		AgentID:   "agent_1",
		Prefs:     json.RawMessage(`{"schema":"witself.dashboard-prefs.v1","theme":"amber"}`),
		UpdatedAt: now,
	}
	var putBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.URL.Path != "/v1/self/dashboard-preferences" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0", "preferences": stored,
			})
		case http.MethodPut:
			var body struct {
				Prefs json.RawMessage `json:"prefs"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode put body: %v", err)
			}
			putBody = body.Prefs
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"preferences": DashboardPreferences{
					AgentID: "agent_1", Prefs: body.Prefs, UpdatedAt: now,
				},
			})
		default:
			t.Fatalf("method = %q", r.Method)
		}
	}))
	defer srv.Close()

	got, err := GetDashboardPreferences(context.Background(), srv.URL, "agent-token")
	if err != nil || got == nil {
		t.Fatalf("get = %#v / %v", got, err)
	}
	if got.AgentID != "agent_1" || string(got.Prefs) != string(stored.Prefs) {
		t.Fatalf("get preferences = %#v", got)
	}

	doc := json.RawMessage(`{"schema":"witself.dashboard-prefs.v1","theme":"midnight"}`)
	updated, err := PutDashboardPreferences(context.Background(), srv.URL, "agent-token", doc)
	if err != nil || updated == nil {
		t.Fatalf("put = %#v / %v", updated, err)
	}
	if string(putBody) != string(doc) || string(updated.Prefs) != string(doc) {
		t.Fatalf("put forwarded %s, returned %#v", putBody, updated)
	}
}

func TestDashboardPreferencesClientHandlesAbsentRowAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","preferences":null}`))
		case http.MethodPut:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid dashboard preferences"}`))
		}
	}))
	defer srv.Close()

	got, err := GetDashboardPreferences(context.Background(), srv.URL, "agent-token")
	if err != nil || got != nil {
		t.Fatalf("absent row = %#v / %v", got, err)
	}
	_, err = PutDashboardPreferences(context.Background(), srv.URL, "agent-token", json.RawMessage(`{"theme":"x"}`))
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("put error = %v, want ErrBadRequest", err)
	}
}
