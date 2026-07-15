package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
)

func TestAgentPeersUsesDefaultAccountAndRealmWithNamedAgent(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("WITSELF_ACCOUNT", "")
	t.Setenv("WITSELF_REALM", "")
	if err := local.Save("default", local.Account{ID: "acc_1"}, "witself_opr_owner"); err != nil {
		t.Fatal(err)
	}
	tokenPath, err := local.AgentTokenPath("default", "default", "scott")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("witself_agt_scott\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lastActive := time.Now().UTC().Add(-5*time.Minute - 30*time.Second)
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer witself_agt_scott" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/self":
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","realm_id":"realm_1","realm_name":"default","agent_id":"agent_scott","agent_name":"scott"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`))
		case "/v1/self/peers":
			_, _ = fmt.Fprintf(w, `{"schema_version":"witself.v0","peers":[{"id":"agent_bob","name":"bob","last_activity_at":%q,"last_runtime":"claude-code","last_location":"home","last_event":"prompt"},{"id":"agent_idle","name":"idle"}]}`, lastActive.Format(time.RFC3339Nano))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"agent", "peers", "--agent", "scott", "--endpoint", srv.URL, "--json"})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("run = %d, stderr = %q", code, stderr)
	}
	if strings.Join(paths, ",") != "/v1/self,/v1/self/peers" {
		t.Fatalf("request paths = %#v", paths)
	}
	var got client.SelfPeers
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode stdout: %v (%q)", err, stdout)
	}
	if got.SchemaVersion != "witself.v0" || len(got.Peers) != 2 || got.Peers[0].Name != "bob" || got.Peers[1].LastActivityAt != nil {
		t.Fatalf("output = %#v", got)
	}

	paths = nil
	stdout, stderr, code = captureFactDeleteCLI(t, func() int {
		return run([]string{"agent", "peers", "--agent", "scott", "--endpoint", srv.URL})
	})
	if code != 0 {
		t.Fatalf("text run = %d, stderr = %q", code, stderr)
	}
	if stderr != "agent\tlast active\truntime\tlocation\tevent\n" {
		t.Fatalf("text header = %q", stderr)
	}
	wantRows := "bob\t5m ago\tclaude-code\thome\tprompt\nidle\tnever\t-\t-\t-\n"
	if stdout != wantRows {
		t.Fatalf("text rows = %q, want %q", stdout, wantRows)
	}
	if strings.Join(paths, ",") != "/v1/self,/v1/self/peers" {
		t.Fatalf("text request paths = %#v", paths)
	}
}

func TestPeerActivityFormattingDoesNotInferAvailability(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 7, 3, 0, time.UTC)
	at := time.Date(2026, 7, 15, 21, 2, 3, 0, time.FixedZone("MDT", -6*60*60))
	if got := peerActivityTime(&at, now); got != "5m ago" {
		t.Fatalf("activity time = %q", got)
	}
	if got := peerActivityTime(nil, now); got != "never" {
		t.Fatalf("missing activity = %q", got)
	}
	future := now.Add(time.Minute)
	if got := peerActivityTime(&future, now); got != "<1m ago" {
		t.Fatalf("future-skewed activity = %q", got)
	}
	if got := peerActivityField(""); got != "-" {
		t.Fatalf("missing field = %q", got)
	}
}
