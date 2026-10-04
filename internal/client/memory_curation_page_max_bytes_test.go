package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// TestGetMemoryCurationRunInputPageSendsMaxBytes pins that the client sends
// max_bytes exactly when it is nonzero, so a server without the parameter still
// accepts an unbounded read, and that the unbounded helper never sends it.
func TestGetMemoryCurationRunInputPageSendsMaxBytes(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/memory-curation-runs/mrun_1/inputs" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		queries = append(queries, r.URL.RawQuery)
		_, _ = io.WriteString(w, `{"run":{"id":"mrun_1"},"inputs":[],"next_cursor":""}`)
	}))
	defer srv.Close()
	ctx := context.Background()
	if _, err := GetMemoryCurationRunInputPage(ctx, srv.URL, "token", "mrun_1", MemoryCurationRunInputOptions{
		FencingGeneration: 7, Cursor: "input cursor", Limit: 23, MaxBytes: 24576,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := GetMemoryCurationRunInputPage(ctx, srv.URL, "token", "mrun_1", MemoryCurationRunInputOptions{
		FencingGeneration: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := GetMemoryCurationRunInputs(ctx, srv.URL, "token", "mrun_1", 7, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := GetMemoryCurationRunInputPage(ctx, srv.URL, "token", "mrun_1", MemoryCurationRunInputOptions{
		FencingGeneration: 7, MaxBytes: -1,
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"cursor=input+cursor&fencing_generation=7&limit=23&max_bytes=24576",
		"fencing_generation=7",
		"fencing_generation=7",
		"fencing_generation=7&max_bytes=-1",
	}
	if !reflect.DeepEqual(queries, want) {
		t.Fatalf("queries = %q, want %q", queries, want)
	}
}
