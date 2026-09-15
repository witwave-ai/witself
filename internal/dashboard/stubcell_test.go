package dashboard

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/avatar"
	"github.com/witwave-ai/witself/internal/dashboard/stubcell"
)

func TestDashboardSharedStubCellAvatar(t *testing.T) {
	cell := stubcell.New(stubcell.Config{BearerToken: testBearer, Identity: testIdentity})
	srv, cfg := newDashboard(t, cell.ServeHTTP, nil)
	response := authedGet(t, srv, cfg, "/api/avatar.svg")
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("avatar status %d: %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); got != "image/svg+xml" {
		t.Fatalf("avatar Content-Type = %q, want image/svg+xml", got)
	}
	canonical, err := avatar.SanitizeSVG(body)
	if err != nil {
		t.Fatalf("avatar SVG: %v", err)
	}
	if !bytes.Equal(body, canonical) {
		t.Fatal("avatar response is not canonical SVG")
	}
}

func TestDashboardSharedStubCellPanelProjections(t *testing.T) {
	cell := stubcell.New(stubcell.Config{BearerToken: testBearer, Identity: testIdentity})
	srv, cfg := newDashboard(t, cell.ServeHTTP, nil)
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/api/self", []string{`"observational":true`, `"agent_name":"dash"`, `"facts":2`, `"enforced_plan_id":"standard"`, `"mem_1"`}},
		{"/api/prefs", []string{`"preferences":null`}},
		{"/api/transcripts", []string{`"id":"tr_1"`}},
		{"/api/facts?limit=100", []string{`"id":"fact_1"`, `"Scott"`}},
		{"/api/memories?limit=100", []string{`"id":"mem_1"`}},
		{"/api/messages?direction=inbox&limit=100", []string{`"id":"msg_1"`, `"subject":"greetings"`}},
		{"/api/messages?direction=outbox&limit=100", []string{`"id":"msg_1"`}},
		{"/api/email?limit=100", []string{`"subject":"safe subject"`, `"attachment_count":2`, `"failure_count":2`}},
		{"/api/email/sent?limit=100", []string{`"subject":"safe sent subject"`, `"state":"delivered"`, `"attempt_count":2`}},
		{"/api/email/status", []string{`"maximum_raw_bytes":26214400`, `"used":4096`, `"max":8192`}},
		{"/api/secrets?limit=100", []string{`"id":"sec_1"`, `"name":"prod-db"`, `"field_count":2`, `"sensitive_field_count":1`}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := authedGet(t, srv, cfg, tc.path)
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", response.StatusCode, body)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(body), want) {
					t.Errorf("missing %q: %s", want, body)
				}
			}
			for _, forbidden := range []string{"leaked", stubcell.SecretCanary} {
				if strings.Contains(string(body), forbidden) {
					t.Errorf("browser response contains forbidden fixture material %q", forbidden)
				}
			}
		})
	}
}

func TestDashboardSharedLeakySecretCanaryIsRedacted(t *testing.T) {
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "GET /v1/secrets" {
			t.Errorf("unexpected cell request %s %s", r.Method, r.URL.Path)
		}
		writeTestJSON(t, w, map[string]any{"items": []map[string]any{stubcell.LeakySecret()}})
	}, nil)
	response := authedGet(t, srv, cfg, "/api/secrets?limit=100")
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"id":"sec_1"`) {
		t.Fatalf("secret fixture failed: %d %s", response.StatusCode, body)
	}
	if strings.Contains(string(body), stubcell.SecretCanary) {
		t.Fatal("dashboard API exposed canary from a misbehaving cell")
	}
}
