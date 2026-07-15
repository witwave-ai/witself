package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestTouchAgentActivityContract(t *testing.T) {
	occurredAt := time.Date(2026, 7, 15, 18, 20, 0, 123, time.UTC)
	observedAt := time.Date(2026, 7, 15, 18, 20, 1, 0, time.UTC)
	want := AgentActivityInput{
		Runtime: "codex", LocationID: "loc_1", Location: "home",
		Event: "UserPromptSubmit", EventID: "evt_1", EventOccurredAt: occurredAt,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/self/activity" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Fatalf("authorization = %q", got)
		}
		var got AgentActivityInput
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("input = %#v, want %#v", got, want)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"activity": AgentActivity{
			LastActivityAt: observedAt, LastRuntime: "codex", LastLocation: "home", LastEvent: "UserPromptSubmit",
		}})
	}))
	defer srv.Close()

	got, err := TouchAgentActivity(context.Background(), srv.URL+"/", "agent-token", want)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastActivityAt != observedAt || got.LastRuntime != "codex" ||
		got.LastLocation != "home" || got.LastEvent != "UserPromptSubmit" {
		t.Fatalf("activity = %#v", got)
	}
}

func TestTouchAgentActivityDistinguishesOlderServerRouteNotFound(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := TouchAgentActivity(context.Background(), srv.URL, "agent-token", AgentActivityInput{
		Runtime: "codex", LocationID: "loc_1", Event: "SessionStart",
		EventID: "evt_1", EventOccurredAt: time.Now().UTC(),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if err.Error() != "not found" {
		t.Fatalf("error text = %q, want preserved fallback", err.Error())
	}
}
