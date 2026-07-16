package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGetSelfDecodesMemoryCheckpoint(t *testing.T) {
	dueAt := time.Date(2026, 7, 15, 21, 2, 3, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/self" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("include_facts") != "false" || r.URL.Query().Get("include_salient") != "false" ||
			r.URL.Query().Get("include_counts") != "false" || r.URL.Query().Get("include_checkpoint") != "false" ||
			r.URL.Query().Get("include_sensitive") != "false" {
			t.Fatalf("self query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","agent_id":"agt_1","agent_name":"scott","realm_id":"rlm_1","realm_name":"default"},"primary_facts":[],"salient_memories":[],"memory_checkpoint":{"pending":true,"request_id":"mcrq_1","request_generation":5,"due_at":"2026-07-15T21:02:03Z"},"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`))
	}))
	defer srv.Close()

	got, err := GetSelf(context.Background(), srv.URL, "token", SelfOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryCheckpoint == nil || !got.MemoryCheckpoint.Pending ||
		got.MemoryCheckpoint.RequestID != "mcrq_1" || got.MemoryCheckpoint.RequestGeneration != 5 ||
		got.MemoryCheckpoint.DueAt == nil || !got.MemoryCheckpoint.DueAt.Equal(dueAt) {
		t.Fatalf("memory checkpoint = %#v", got.MemoryCheckpoint)
	}
}

func TestGetSelfPeersUsesTokenDerivedEndpointAndDecodesActivity(t *testing.T) {
	lastActive := time.Date(2026, 7, 15, 21, 2, 3, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/self/peers" || r.URL.RawQuery != "" {
			t.Fatalf("request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer witself_agt_scott" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","peers":[{"id":"agent_bob","name":"bob","last_activity_at":"2026-07-15T21:02:03Z","last_runtime":"claude-code","last_location":"home","last_event":"prompt"},{"id":"agent_idle","name":"idle"}]}`))
	}))
	defer srv.Close()

	got, err := GetSelfPeers(context.Background(), srv.URL+"/", "witself_agt_scott")
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != "witself.v0" || len(got.Peers) != 2 {
		t.Fatalf("response = %#v", got)
	}
	peer := got.Peers[0]
	if peer.ID != "agent_bob" || peer.Name != "bob" || peer.LastActivityAt == nil ||
		!peer.LastActivityAt.Equal(lastActive) || peer.LastRuntime != "claude-code" ||
		peer.LastLocation != "home" || peer.LastEvent != "prompt" {
		t.Fatalf("active peer = %#v", peer)
	}
	if got.Peers[1].LastActivityAt != nil {
		t.Fatalf("inactive peer has activity = %#v", got.Peers[1])
	}
}

func TestGetSelfPeersNormalizesNullPeers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","peers":null}`))
	}))
	defer srv.Close()

	got, err := GetSelfPeers(context.Background(), srv.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	if got.Peers == nil || len(got.Peers) != 0 {
		t.Fatalf("peers = %#v, want non-nil empty slice", got.Peers)
	}
}
