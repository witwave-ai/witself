package memorycurator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/witwave-ai/witself/internal/activity"
	"github.com/witwave-ai/witself/internal/client"
)

func TestAutomaticCuratorReadsAreObservational(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get(activity.ObservationHeader) != "1" || r.Header.Get(activity.RequestIDHeader) != "" {
			t.Error("automatic bookkeeping carried deliberate intent")
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	api := HTTPAPI{Endpoint: srv.URL, Token: "synthetic"}
	ctx := context.Background()
	if _, err := api.ListRequests(ctx, client.MemoryCurationRequestListOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.GetInputs(ctx, "mrun_synthetic", 1, "", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := api.GetPlan(ctx, "mrun_synthetic", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := api.GetRun(ctx, "mrun_synthetic"); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Status(ctx, "mrun_synthetic"); err != nil {
		t.Fatal(err)
	}
	if calls != 5 {
		t.Fatal("not all automatic read boundaries exercised", calls)
	}
}
