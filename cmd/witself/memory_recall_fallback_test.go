package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
)

func TestMemoryRecallQueryFallbackNote(t *testing.T) {
	const note = "note: no memory matched every word; showing memories that match any word"
	for _, tc := range []struct {
		name     string
		hits     int
		fallback string
		json     bool
	}{
		{name: "zero_hits", fallback: "any_terms"},
		{name: "one_hit", hits: 1, fallback: "any_terms"},
		{name: "json_zero_hits", fallback: "any_terms", json: true},
		{name: "json_one_hit", hits: 1, fallback: "any_terms", json: true},
		{name: "strict_zero_hits"},
		{name: "strict_one_hit", hits: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WITSELF_HOME", t.TempDir())
			t.Setenv("DSH_HOME", t.TempDir())
			tokenFile := filepath.Join(t.TempDir(), "test-auth")
			if err := os.WriteFile(tokenFile, []byte("synthetic-test-token"), 0o600); err != nil {
				t.Fatal(err)
			}
			page := client.MemoryRecallPage{
				Hits: []client.MemoryRecallHit{}, RetrievalMode: "lexical", QueryFallback: tc.fallback,
			}
			if tc.hits > 0 {
				page.Hits = append(page.Hits, client.MemoryRecallHit{Memory: memoryFixture("mem_1", 1, "active", "capture")})
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/memories:recall" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				var in client.MemoryRecallInput
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					t.Error(err)
				}
				if in.QueryFallback != "any_terms" {
					t.Errorf("query fallback opt-in = %q", in.QueryFallback)
				}
				_ = json.NewEncoder(w).Encode(page)
			}))
			defer srv.Close()
			args := []string{"alpha delta", "--endpoint", srv.URL, "--token-file", tokenFile}
			if tc.json {
				args = append(args, "--json")
			}
			stdout, stderr, code := captureFactDeleteCLI(t, func() int { return memoryRecall(args) })
			if code != 0 {
				t.Fatalf("memoryRecall exit = %d", code)
			}
			wantCount := 0
			if !tc.json && tc.fallback == "any_terms" {
				wantCount = 1
			}
			if got := strings.Count(stderr, note+"\n"); got != wantCount {
				t.Fatalf("stderr note count = %d, want %d; stderr = %q", got, wantCount, stderr)
			}
			if strings.Contains(stdout, note) {
				t.Fatal("human note leaked into stdout")
			}
			if tc.json {
				var decoded client.MemoryRecallPage
				if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
					t.Fatal(err)
				}
				if decoded.QueryFallback != tc.fallback {
					t.Fatalf("JSON fallback = %q, want %q", decoded.QueryFallback, tc.fallback)
				}
			}
		})
	}
}

func TestMemoryRecallHelpDescribesAnyTermsFallback(t *testing.T) {
	const guidance = "Without a vector profile, if no memory contains every word of a plain query of two or more words, recall retries once with any-word matching (common words dropped) and reports query_fallback=any_terms; queries with double quotes, a leading minus, OR, a single word, or only common words are not retried. Older servers never retry: if query_fallback is absent, no any-word retry ran, so zero hits still means re-query with fewer keywords."
	_, stderr, code := captureFactDeleteCLI(t, func() int { return memoryRecall([]string{"--help"}) })
	if code != 2 || !strings.Contains(stderr, guidance) {
		t.Fatalf("recall help exit = %d; missing fallback guidance = %t", code, !strings.Contains(stderr, guidance))
	}
}
