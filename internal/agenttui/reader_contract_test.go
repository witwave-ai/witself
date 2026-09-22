package agenttui

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/dashboard"
)

type syntheticTransport func(*http.Request) (*http.Response, error)

func (f syntheticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise the real Reader's handler envelopes without sockets, credentials, or
// a fake production adapter. Every request terminates in this in-memory transport.
func TestActualReaderPreferencesAndEmailCapacityEnvelopes(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	calls := []string{}
	http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "synthetic.invalid" {
			t.Fatalf("unexpected target: %s", r.URL.Host)
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		body := ""
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/self/dashboard-preferences":
			body = `{"schema_version":"witself.v0","preferences":{"agent_id":"synthetic-agent","prefs":{"schema":"witself.dashboard-prefs.v1","theme":"amber"}}}`
		case "PUT /v1/self/dashboard-preferences":
			b, _ := io.ReadAll(r.Body)
			if string(b) != `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"paper"}}` {
				t.Fatalf("theme write exceeded exact preference: %s", b)
			}
			body = `{"schema_version":"witself.v0","preferences":{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"paper"}}}`
		case "GET /v1/email:status":
			body = `{"schema_version":"witself.v0","maximum_raw_bytes":26214400,"attachment_capacity":{"used":2048,"max":8192,"remaining":6144,"unlimited":false,"near_limit":false,"at_limit":false,"over_limit":false}}`
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	reader, err := dashboard.NewReader(dashboard.Config{Endpoint: "https://synthetic.invalid", BearerToken: "synthetic-token"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := reader.Read(context.Background(), dashboard.ReadRequest{Resource: dashboard.ResourcePreferences})
	if err != nil {
		t.Fatal(err)
	}
	prefs, err := decode(dashboard.ResourcePreferences, raw)
	if err != nil || str(obj(prefs["prefs"]), "theme") != "amber" {
		t.Fatalf("actual preferences envelope not decoded: %s", raw)
	}
	raw, err = reader.Read(context.Background(), dashboard.ReadRequest{Resource: dashboard.ResourceEmailStatus})
	if err != nil {
		t.Fatal(err)
	}
	status, err := decode(dashboard.ResourceEmailStatus, raw)
	if err != nil || num(status, "maximum_raw_bytes") != 26214400 || num(obj(status["attachment_capacity"]), "used") != 2048 {
		t.Fatalf("actual status envelope not decoded: %s", raw)
	}
	if _, err = reader.StoreTheme(context.Background(), "paper"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("unexpected calls: %v", calls)
	}
}
