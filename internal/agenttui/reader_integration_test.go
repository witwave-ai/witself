package agenttui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/dashboard/stubcell"
)

// Exercise the actual browser projection envelopes, not demo-only lookalikes.
func TestReaderEnvelopesReachTerminalViews(t *testing.T) {
	identity := stubcell.Identity("terminal-fixture")
	stub := stubcell.New(stubcell.Config{Identity: identity})
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("view performed a mutation")
			w.WriteHeader(405)
			return
		}
		if r.URL.Path == "/v1/self/dashboard-preferences" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"preferences": map[string]any{"agent_id": identity.AgentID, "prefs": map[string]any{"schema": "witself.dashboard-prefs.v1", "theme": "paper"}, "updated_at": "2026-09-21T10:00:00Z"}})
			return
		}
		stub.ServeHTTP(w, r)
	}))
	defer cell.Close()
	reader, err := dashboard.NewReader(dashboard.Config{Endpoint: cell.URL, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	read := func(resource dashboard.Resource) object {
		t.Helper()
		raw, err := reader.Read(context.Background(), dashboard.ReadRequest{Resource: resource})
		if err != nil {
			t.Fatal(err)
		}
		data, err := decode(resource, raw)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	prefs := read(dashboard.ResourcePreferences)
	if str(obj(prefs["prefs"]), "theme") != "paper" {
		t.Fatal("saved theme lost in actual handler envelope")
	}
	status := read(dashboard.ResourceEmailStatus)
	if num(status, "maximum_raw_bytes") != 25*1024*1024 || num(obj(status["attachment_capacity"]), "used") != 4096 {
		t.Fatal("email capacity lost in actual handler envelope")
	}
	self := read(dashboard.ResourceSelf)
	rows := makeRows(0, nil, self, false)
	if len(rows) != 1 || !strings.Contains(rows[0].title, "Acceptance fixture memory") {
		t.Fatal("salient memory snippet lost")
	}
	secrets := read(dashboard.ResourceSecrets)
	encoded, _ := json.Marshal(secrets)
	if strings.Contains(string(encoded), "public_value") || strings.Contains(string(encoded), stubcell.SecretCanary) {
		t.Fatal("secret inventory retained a field value")
	}
}
