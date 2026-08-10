package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentCreateUsesSeparateOrdinaryAndEmailSegmentRoutes(t *testing.T) {
	var paths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent": Agent{ID: "agent_aaaaaaaaaaaaaaaa", Name: body["name"].(string)},
		})
	}))
	defer srv.Close()

	if _, err := CreateAgent(
		context.Background(), srv.URL+"/", "operator-token", "realm_1", "ordinary",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateAgentWithEmailSegment(
		context.Background(), srv.URL, "operator-token", "realm_1", "exceptional", "mail-bot",
	); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/v1/realms/realm_1/agents" ||
		paths[1] != "/v1/realms/realm_1/agents:with-email-segment" {
		t.Fatalf("agent create paths = %#v", paths)
	}
	if len(bodies[0]) != 1 || bodies[0]["name"] != "ordinary" {
		t.Fatalf("ordinary create body = %#v", bodies[0])
	}
	if len(bodies[1]) != 2 || bodies[1]["name"] != "exceptional" ||
		bodies[1]["email_agent_segment"] != "mail-bot" {
		t.Fatalf("email-segment create body = %#v", bodies[1])
	}
}

func TestAgentCreateEmailSegmentCannotMutateLegacyRoute(t *testing.T) {
	mutated := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/realms/realm_1/agents" {
			mutated = true
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := CreateAgentWithEmailSegment(
		context.Background(), srv.URL, "operator-token", "realm_1", "exceptional", "mail-bot",
	)
	if !errors.Is(err, ErrNotFound) || mutated {
		t.Fatalf("legacy explicit create error/mutation = %v/%v, want ErrNotFound/false",
			err, mutated)
	}
}

func TestAgentCreateMapsEmailAddressConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"code":           "agent_email_address_conflict",
			"error":          "agent email address is already reserved; retry with a different --email-agent-segment",
			"retryable":      false,
		})
	}))
	defer srv.Close()

	_, err := CreateAgentWithEmailSegment(
		context.Background(), srv.URL, "operator-token", "realm_1", "exceptional", "mail-bot",
	)
	if !errors.Is(err, ErrAgentEmailAddressConflict) ||
		err.Error() != ErrAgentEmailAddressConflict.Error() {
		t.Fatalf("address conflict error = %v", err)
	}
}
