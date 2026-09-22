package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard/stubcell"
)

func newTestReader(t *testing.T, backend http.Handler) (*Reader, Config) {
	t.Helper()
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+testBearer {
			t.Error("missing synthetic bearer token")
		}
		backend.ServeHTTP(w, req)
	}))
	t.Cleanup(cell.Close)
	cfg := Config{Endpoint: cell.URL, BearerToken: testBearer, Identity: testIdentity, Version: "reader-test", PollInterval: 3 * time.Second}
	r, err := NewReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r, cfg
}

func assertReaderError(t *testing.T, raw json.RawMessage, err error, status int, code string) {
	t.Helper()
	var got *ReaderError
	if raw != nil || !errors.As(err, &got) || got.Status != status || got.Code != code ||
		err.Error() != "dashboard reader request failed" {
		t.Fatalf("got output length %d, error %#v; want nil output, status %d, code %q", len(raw), err, status, code)
	}
}

func TestReaderConfig(t *testing.T) {
	for _, endpoint := range []string{"", "cell.example", "/relative", "ftp://cell.example", "https://",
		"https://user:fake@cell.example", "https://cell.example/path", "https://cell.example?", "https://cell.example?q=x",
		"https://cell.example#", "https://cell.example#fragment", "https://cell.example/%2f", "https://cell.example\\path",
		"https://cell.example:0", "https://cell.example:65536", "https://cell.example:", "https://cell.example:bad",
		" https://cell.example", "https://cell.example\n", "https://cell.example\u0085", "https://cell.example\xff"} {
		t.Run(endpoint, func(t *testing.T) {
			r, err := NewReader(Config{Endpoint: endpoint})
			if r != nil {
				t.Fatal("invalid endpoint accepted")
			}
			assertReaderError(t, nil, err, 400, "")
		})
	}
	for _, endpoint := range []string{"https://cell.example", "https://cell.example/", "http://127.0.0.1:12345", "http://[::1]:12345/"} {
		r, err := NewReader(Config{Endpoint: endpoint, BearerToken: testBearer, Identity: testIdentity, Version: "v-test", AccessToken: "unused-fake"})
		if err != nil || r.cfg.PollInterval != defaultPollInterval || r.cfg.AccessToken != "" || r.cfg.Identity != testIdentity ||
			r.cfg.Version != "v-test" || r.cfg.BearerToken != testBearer || r.cfg.Endpoint != endpoint {
			t.Fatalf("config was not preserved/defaulted: %v", err)
		}
	}
}

// This backend reuses the console fixtures, adding the detail routes that the
// browser acceptance stub does not need. All material is synthetic.
func readerFixtureBackend(t *testing.T) http.Handler {
	stub := stubcell.New(stubcell.Config{BearerToken: testBearer, Identity: testIdentity})
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			t.Errorf("unexpected mutation %s %s", req.Method, req.URL.Path)
		}
		switch req.URL.Path {
		case "/v1/facts/fact_1/history":
			writeTestJSON(t, w, map[string]any{"assertions": []client.FactAssertion{{ID: "fas_1", FactID: "fact_1", Value: json.RawMessage(`"leaked-history"`), SourceRef: "leaked-ref"}}})
		case "/v1/secrets":
			writeTestJSON(t, w, map[string]any{"items": []any{leakySecretJSON()}})
		case "/v1/secrets/sec_1":
			writeTestJSON(t, w, map[string]any{"secret": leakySecretJSON()})
		case "/v1/vault/key-epochs/current":
			writeTestJSON(t, w, map[string]any{"key_epoch": map[string]any{"id": "avk_1", "key_version": 3, "private_key": "leaked-key"}})
		default:
			stub.ServeHTTP(w, req)
		}
	})
}

func TestReaderMatchesEveryHandlerProjection(t *testing.T) {
	var recorder recordingBackend
	fixture := readerFixtureBackend(t)
	r, cfg := newTestReader(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.mu.Lock()
		recorder.calls = append(recorder.calls, req.Method+" "+req.URL.RequestURI())
		recorder.mu.Unlock()
		fixture.ServeHTTP(w, req)
	}))
	cases := []struct {
		resource Resource
		id, path string
		handler  http.Handler
	}{
		{ResourceSelf, "", "/api/self", selfHandler(cfg)},
		{ResourceThemes, "", "/api/themes", http.HandlerFunc(themesHandler)},
		{ResourcePreferences, "", "/api/prefs", prefsHandler(cfg)},
		{ResourceTranscripts, "", "/api/transcripts", transcriptsHandler(cfg)},
		{ResourceTranscript, "tr_1", "/api/transcripts/tr_1", transcriptPageHandler(cfg)},
		{ResourceMemories, "", "/api/memories", memoriesHandler(cfg)},
		{ResourceMemory, "mem_1", "/api/memories/mem_1", memoryHandler(cfg)},
		{ResourceMemoryHistory, "mem_1", "/api/memories/mem_1/history", memoryHistoryHandler(cfg)},
		{ResourceFacts, "", "/api/facts", factsHandler(cfg, &factReadCapability{})},
		{ResourceFactHistory, "fact_1", "/api/facts/fact_1/history", factHistoryHandler(cfg, &factReadCapability{})},
		{ResourceMessages, "", "/api/messages", messagesHandler(cfg)},
		{ResourceEmailAddress, "", "/api/email/address", agentEmailAddressHandler(cfg)},
		{ResourceEmailStatus, "", "/api/email/status", agentEmailStatusHandler(cfg)},
		{ResourceEmailReceived, "", "/api/email", agentEmailsHandler(cfg)},
		{ResourceEmailSent, "", "/api/email/sent", agentEmailSentHandler(cfg)},
		{ResourceSecrets, "", "/api/secrets", secretsHandler(cfg, &secretsCapability{})},
		{ResourceSecret, "sec_1", "/api/secrets/sec_1", secretHandler(cfg, &secretsCapability{})},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			start := len(recorder.snapshot())
			got, err := r.Read(t.Context(), ReadRequest{Resource: tc.resource, ID: tc.id})
			if err != nil {
				t.Fatal(err)
			}
			middle := len(recorder.snapshot())
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.SetPathValue("id", tc.id)
			w := httptest.NewRecorder()
			tc.handler.ServeHTTP(w, req)
			if w.Code != 200 || !bytes.Equal(got, w.Body.Bytes()) {
				t.Fatalf("reader differs from handler: %s != %s (status %d)", got, w.Body.Bytes(), w.Code)
			}
			calls := recorder.snapshot()
			if !reflect.DeepEqual(calls[start:middle], calls[middle:]) {
				t.Fatalf("default upstream requests differ: %v != %v", calls[start:middle], calls[middle:])
			}
			for _, forbidden := range []string{"leaked", "private-", "claimable-message-id", "cursor-2", stubcell.SecretCanary} {
				if bytes.Contains(got, []byte(forbidden)) {
					t.Fatalf("forbidden fixture material %q in projection", forbidden)
				}
			}
		})
	}
}

func TestReaderRejectsInvalidRequestsLocally(t *testing.T) {
	var calls atomic.Int32
	r, _ := newTestReader(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, map[string]any{})
	}))
	cases := []ReadRequest{
		{}, {Resource: 255}, {Resource: ResourceSelf, ID: "unexpected"},
		{Resource: ResourcePreferences, Query: url.Values{"theme": {"amber"}}},
		{Resource: ResourceMessages, Query: url.Values{"method": {"POST"}}},
		{Resource: ResourceSelf, Query: url.Values{"url": {"https://other.invalid"}}},
		{Resource: ResourceTranscripts, Query: url.Values{"limit": {"10"}}},
		{Resource: ResourceMemories, Query: url.Values{"state kind": {"x"}}},
		{Resource: ResourceMemories, Query: url.Values{"": {"x"}}},
		{Resource: ResourceMemories, Query: url.Values{"tag": {}}},
		{Resource: ResourceMemories, Query: url.Values{"tag": make([]string, maxReaderTags+1)}},
		{Resource: ResourceMemories, Query: url.Values{"include_sensitive": {"true"}}},
		{Resource: ResourceFacts, Query: url.Values{"observational": {"false"}}},
		{Resource: ResourceFacts, Query: url.Values{"predicate": {"identity/name"}}},
		{Resource: ResourceSecrets, Query: url.Values{"include_fields": {"false"}}},
		{Resource: ResourceEmailSent, Query: url.Values{"cursor": {"cursor"}}},
		{Resource: ResourceEmailReceived, Query: url.Values{"cursor": {""}}},
		{Resource: ResourceMessages, Query: url.Values{"unread": {"yes"}}},
		{Resource: ResourceEmailReceived, Query: url.Values{"unacked": {""}}},
		{Resource: ResourceTranscript, ID: "tr_1", Query: url.Values{"tail": {"wrong"}}},
		{Resource: ResourceTranscript, ID: "tr_1", Query: url.Values{"after_sequence": {"-1"}}},
		{Resource: ResourceTranscript, ID: "tr_1", Query: url.Values{"after_sequence": {"9223372036854775808"}}},
		{Resource: ResourceMessages, Query: url.Values{"cursor": {"a", "b"}}},
		{Resource: ResourceMemories, Query: url.Values{"kind": {"line\nbreak"}}},
		{Resource: ResourceMemories, Query: url.Values{"kind": {"\xff"}}},
		{Resource: ResourceMemories, Query: url.Values{"cursor": {strings.Repeat("x", maxReaderStringBytes+1)}}},
		{Resource: ResourceMemories, Query: url.Values{"tag": {strings.Repeat("x", 4096), strings.Repeat("x", 4096), strings.Repeat("x", 4096), strings.Repeat("x", 4096)}}},
	}
	for _, resource := range []Resource{ResourceTranscript, ResourceMemories, ResourceMemoryHistory, ResourceMessages, ResourceFacts, ResourceEmailReceived, ResourceEmailSent, ResourceSecrets} {
		for _, limit := range []string{"", "0", "-1", "no", "1.5", "99999999999999999999999999", "501"} {
			id := ""
			if resource == ResourceTranscript || resource == ResourceMemoryHistory {
				id = "opaque"
			}
			cases = append(cases, ReadRequest{Resource: resource, ID: id, Query: url.Values{"limit": {limit}}})
			if resource != ResourceTranscript {
				cases = append(cases, ReadRequest{Resource: resource, ID: id, Query: url.Values{"limit": {"101"}}})
			}
		}
	}
	for _, resource := range []Resource{ResourceTranscript, ResourceMemory, ResourceMemoryHistory, ResourceFactHistory, ResourceSecret} {
		for _, id := range []string{"", ".", "..", "a/b", "a\\b", "a?b", "a#b", "a:read", "%2f", "a\x00b", "a\u0085b", "a b", "\xff", strings.Repeat("x", 257)} {
			cases = append(cases, ReadRequest{Resource: resource, ID: id})
		}
	}
	for i, in := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			raw, err := r.Read(t.Context(), in)
			assertReaderError(t, raw, err, 400, "")
		})
	}
	for _, theme := range []string{"", "../amber", "a/b", "https://x", "a\n", strings.Repeat("x", 65)} {
		raw, err := r.StoreTheme(t.Context(), theme)
		assertReaderError(t, raw, err, 400, "")
	}
	for _, id := range []string{"", "../msg", "msg:read", "msg?read", strings.Repeat("x", 129)} {
		raw, err := r.PreviewMessage(t.Context(), id)
		assertReaderError(t, raw, err, 400, "")
	}
	for _, address := range [][2]string{{"", "p"}, {"s", ""}, {"s\n", "p"}, {"s", strings.Repeat("p", 4097)}} {
		raw, err := r.RevealFact(t.Context(), address[0], address[1])
		assertReaderError(t, raw, err, 400, "")
	}
	for _, method := range []string{"POST", "DELETE", "HEAD", "PATCH", "OPTIONS", "PUT"} {
		raw, err := r.invoke(t.Context(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid method invoked handler") }), method, "/api/self", "", nil, nil)
		assertReaderError(t, raw, err, 400, "")
	}
	if calls.Load() != 0 {
		t.Fatalf("local validation made %d upstream requests", calls.Load())
	}
}

func TestReaderForwardsAllowedFiltersAndOpaqueIDs(t *testing.T) {
	cases := []struct {
		resource Resource
		id, path string
		query    url.Values
	}{
		{ResourceTranscript, "custom.123-xyz", "/v1/transcripts/custom.123-xyz", url.Values{"after_sequence": {"0"}, "tail": {"true"}, "limit": {"500"}}},
		{ResourceMemories, "", "/v1/memories", url.Values{"kind": {"decision"}, "state": {"active"}, "tag": {"one", "two"}, "cursor": {"opaque:/+="}, "limit": {"100"}}},
		{ResourceMemoryHistory, "123-any-prefix", "/v1/memories/123-any-prefix/history", url.Values{"cursor": {"opaque"}, "limit": {"100"}}},
		{ResourceMessages, "", "/v1/messages", url.Values{"direction": {"inbox"}, "from": {"someone"}, "thread_id": {"thread"}, "kind": {"note"}, "cursor": {"opaque"}, "unread": {"true"}, "limit": {"100"}}},
		{ResourceFacts, "", "/v1/facts", url.Values{"subject": {"self"}, "predicate_prefix": {"identity/"}, "limit": {"100"}}},
		{ResourceEmailReceived, "", "/v1/email", url.Values{"unread": {"true"}, "unacked": {"true"}, "limit": {"100"}}},
		{ResourceEmailSent, "", "/v1/email/sent", url.Values{"limit": {"100"}}},
		{ResourceSecrets, "", "/v1/secrets", url.Values{"lifecycle": {"active"}, "cursor": {"opaque"}, "limit": {"100"}}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			var calls atomic.Int32
			r, _ := newTestReader(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if answerFactReadProbe(w, req) {
					return
				}
				calls.Add(1)
				if req.Method != "GET" || req.URL.Path != tc.path {
					t.Errorf("unexpected route %s %s", req.Method, req.URL.Path)
				}
				want := tc.query.Encode()
				q := req.URL.Query()
				// The client omits zero cursors and adds observational/metadata flags.
				if tc.resource == ResourceTranscript {
					if q.Get("observational") != "true" {
						t.Error("transcript not observational")
					}
					q.Del("observational")
					q.Set("after_sequence", "0")
				}
				if tc.resource == ResourceFacts {
					if q.Get("observational") != "true" {
						t.Error("facts not observational")
					}
					q.Del("observational")
				}
				if tc.resource == ResourceSecrets {
					if q.Get("include_fields") != "true" {
						t.Error("secret field metadata missing")
					}
					q.Del("include_fields")
				}
				if q.Encode() != want {
					t.Errorf("query = %s, want %s", q.Encode(), want)
				}
				writeJSON(w, map[string]any{})
			}))
			before := tc.query.Encode()
			if _, err := r.Read(t.Context(), ReadRequest{Resource: tc.resource, ID: tc.id, Query: tc.query}); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || tc.query.Encode() != before {
				t.Fatal("request count or caller query changed")
			}
		})
	}
}

func TestReaderFactCapabilitiesAndHistory(t *testing.T) {
	for _, mode := range []string{"old", "unwired", "sensitive", "public", "mismatched"} {
		t.Run(mode, func(t *testing.T) {
			var probes, lists, exact atomic.Int32
			r, cfg := newTestReader(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != "GET" {
					t.Error("fact mutation")
				}
				switch req.URL.Path {
				case "/v1/facts/fact_1/history":
					writeJSON(w, map[string]any{"assertions": []client.FactAssertion{{ID: "fas_1", FactID: "fact_1", Value: json.RawMessage(`"history-value"`), SourceRef: "history-ref"}}})
				case "/v1/facts":
					q := req.URL.Query()
					if q.Get("observational") == "probe" {
						probes.Add(1)
						if q.Get("limit") != "probe" {
							t.Error("unsafe capability probe")
						}
						message := "observational must be true or false"
						if mode == "old" {
							message = "limit must be an integer"
						}
						writeJSONError(w, 400, message)
						return
					}
					if q.Get("observational") != "true" || q.Has("include_sensitive") || mode == "old" {
						t.Error("unsafe fact fallback")
					}
					if q.Has("predicate") {
						exact.Add(1)
						id := "fact_1"
						if mode == "mismatched" {
							id = "fact_other"
						}
						writeJSON(w, map[string]any{"fact": client.Fact{ID: id, Sensitive: mode == "sensitive"}})
						return
					}
					lists.Add(1)
					if mode == "unwired" {
						writeJSONError(w, 501, "observational fact reads are unavailable")
						return
					}
					writeTestJSON(t, w, stubcell.Facts())
				default:
					t.Errorf("unexpected route %s", req.URL.Path)
				}
			}))
			for range 2 {
				raw, err := r.Read(t.Context(), ReadRequest{Resource: ResourceFacts})
				if mode == "old" || mode == "unwired" {
					assertReaderError(t, raw, err, 501, "unavailable")
				} else if err != nil || bytes.Contains(raw, []byte("leaked")) {
					t.Fatalf("unsafe fact list: %v", err)
				}
			}
			for _, query := range []url.Values{nil, {"subject": {"self"}, "predicate": {"identity/name"}}} {
				raw, err := r.Read(t.Context(), ReadRequest{Resource: ResourceFactHistory, ID: "fact_1", Query: query})
				if err != nil {
					t.Fatal(err)
				}
				wantValue := query != nil && (mode == "public" || mode == "unwired")
				if bytes.Contains(raw, []byte("history-value")) != wantValue || bytes.Contains(raw, []byte("history-ref")) != wantValue {
					t.Fatal("history sensitivity proof not preserved")
				}
			}
			if probes.Load() != 1 || (mode == "old" && (lists.Load() != 0 || exact.Load() != 0)) {
				t.Fatalf("capability not reused or old-cell read occurred: probes=%d lists=%d exact=%d", probes.Load(), lists.Load(), exact.Load())
			}
			// Another Reader must resolve its own capability, even at the same endpoint.
			other, err := NewReader(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = other.Read(t.Context(), ReadRequest{Resource: ResourceFacts})
			if probes.Load() != 2 {
				t.Fatal("capability leaked between instances")
			}
		})
	}
}

func TestReaderSecretCapabilityReuse(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(fmt.Sprint(supported), func(t *testing.T) {
			var lists, details atomic.Int32
			r, _ := newTestReader(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				rejectSecretMutations(t, req)
				if req.URL.Path == "/v1/secrets" {
					lists.Add(1)
					if supported {
						writeJSON(w, map[string]any{"items": []any{}})
						return
					}
				} else if req.URL.Path == "/v1/secrets/unknown-123" {
					details.Add(1)
				} else {
					t.Errorf("unexpected path %s", req.URL.Path)
				}
				writeJSONError(w, 404, "private missing secret")
			}))
			_, _ = r.Read(t.Context(), ReadRequest{Resource: ResourceSecrets})
			for range 2 {
				raw, err := r.Read(t.Context(), ReadRequest{Resource: ResourceSecret, ID: "unknown-123"})
				if supported {
					assertReaderError(t, raw, err, 404, "")
				} else {
					assertReaderError(t, raw, err, 501, "unavailable")
				}
			}
			if lists.Load() != 1 || details.Load() != 2 {
				t.Fatalf("list capability was not reused: list=%d detail=%d", lists.Load(), details.Load())
			}
		})
	}
}

func TestReaderSafeAvailabilityErrors(t *testing.T) {
	cases := []struct {
		name       string
		resource   Resource
		status     int
		body       string
		wantStatus int
		code       string
	}{
		{"receive disabled", ResourceEmailReceived, 403, `{"code":"feature_not_enabled","feature":"agent_email_receive","error":"private upstream text"}`, 403, "feature_not_enabled"},
		{"send disabled", ResourceEmailSent, 403, `{"error":{"code":"feature_not_enabled","feature":"agent_email_send","message":"private upstream text"}}`, 403, "feature_not_enabled"},
		{"unenrolled", ResourceEmailAddress, 403, `{"error":"agent email is not enabled for this agent"}`, 403, "unenrolled"},
		{"legacy unenrolled", ResourceEmailStatus, 403, `{"error":"agent is not enrolled in the email pilot"}`, 403, "unenrolled"},
		{"old email", ResourceEmailReceived, 404, `{"error":"private upstream text"}`, 501, "unavailable"},
		{"old sent", ResourceEmailSent, 404, `{"error":"private upstream text"}`, 501, "unavailable"},
		{"generic bad input", ResourceMemories, 400, `{"code":"private-code","error":"private upstream text"}`, 400, ""},
		{"generic missing", ResourceMemories, 404, `{"error":"private upstream text"}`, 404, ""},
		{"generic failure", ResourceMemories, 500, `{"error":"private upstream text"}`, 502, ""},
		{"generic code erased by handler", ResourceMessages, 403, `{"code":"feature_not_enabled","feature":"messaging","error":"private upstream text"}`, 502, ""},
		{"malformed upstream", ResourceMemories, 200, `{"items":[`, 502, ""},
		{"no upstream JSON", ResourceMemories, 200, ``, 502, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestReader(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			raw, err := r.Read(t.Context(), ReadRequest{Resource: tc.resource})
			assertReaderError(t, raw, err, tc.wantStatus, tc.code)
		})
	}
}

func TestReaderExplicitRevealAndOldCellFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint(fallback), func(t *testing.T) {
			var calls atomic.Int32
			r, cfg := newTestReader(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				n := calls.Add(1)
				q := req.URL.Query()
				if req.Method != "GET" || req.URL.Path != "/v1/facts" || q.Get("subject") != "my spouse" || q.Get("predicate") != "identity/name" {
					t.Error("incorrect exact reveal")
				}
				if q.Get("observational") == "true" && fallback {
					writeJSONError(w, 501, "observational fact reads are unavailable")
					return
				}
				if !fallback && q.Get("observational") != "true" || fallback && n%2 != 0 {
					t.Error("incorrect fallback order")
				}
				writeJSON(w, map[string]any{"fact": client.Fact{ID: "fact_1", Sensitive: true, Value: json.RawMessage(`"intentional-reveal"`)}})
			}))
			raw, err := r.RevealFact(t.Context(), "my spouse", "identity/name")
			if err != nil || !bytes.Contains(raw, []byte("intentional-reveal")) {
				t.Fatalf("explicit reveal failed: %v", err)
			}
			w := httptest.NewRecorder()
			factRevealHandler(cfg).ServeHTTP(w, httptest.NewRequest("GET", "/api/fact?subject=my+spouse&predicate=identity%2Fname", nil))
			if !bytes.Equal(raw, w.Body.Bytes()) {
				t.Fatal("reveal projection differs")
			}
			want := int32(2)
			if fallback {
				want = 4
			}
			if calls.Load() != want {
				t.Fatalf("calls=%d, want %d", calls.Load(), want)
			}
		})
	}
}

func TestReaderMessagePreviewIdentitySizeAndNoFallback(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"body only", 200, `{"message":{"id":"custom_123","body":"hello","payload":{"private":"leaked"},"subject":"leaked"}}`, 200},
		{"empty body", 200, `{"message":{"id":"custom_123"}}`, 200},
		{"maximum", 200, `{"message":{"id":"custom_123","body":"` + strings.Repeat("x", maxMessagePreviewBodyBytes) + `"}}`, 200},
		{"too large", 200, `{"message":{"id":"custom_123","body":"` + strings.Repeat("x", maxMessagePreviewBodyBytes+1) + `"}}`, 502},
		{"wrong id", 200, `{"message":{"id":"other","body":"leaked"}}`, 502},
		{"missing id", 200, `{"message":{"body":"leaked"}}`, 502},
		{"malformed", 200, `{"message":`, 502},
		{"missing", 404, `{"error":"leaked"}`, 404},
		{"forbidden", 403, `{"error":"leaked"}`, 403},
		{"old cell", 501, `{"error":"leaked"}`, 502},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			r, cfg := newTestReader(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if req.Method+" "+req.URL.Path != "GET /v1/messages/custom_123:peek" || req.URL.RawQuery != "" {
					t.Errorf("unsafe preview fallback %s %s", req.Method, req.URL.RequestURI())
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			raw, err := r.PreviewMessage(t.Context(), "custom_123")
			if calls.Load() != 1 {
				t.Fatal("preview retried or fell back")
			}
			if tc.want != 200 {
				assertReaderError(t, raw, err, tc.want, "")
				return
			}
			if err != nil || bytes.Contains(raw, []byte("leaked")) {
				t.Fatalf("unsafe preview: %v", err)
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil || len(fields) != 1 || fields["body"] == nil {
				t.Fatal("preview contains more than body")
			}
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/api/messages/custom_123/body", nil)
			req.SetPathValue("id", "custom_123")
			messageBodyHandler(cfg).ServeHTTP(w, req)
			if !bytes.Equal(raw, w.Body.Bytes()) {
				t.Fatal("preview projection differs")
			}
		})
	}
}

func TestReaderOnlyThemeMutates(t *testing.T) {
	var recorder recordingBackend
	backend := recorder.handler()
	r, cfg := newTestReader(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" {
			if req.Method+" "+req.URL.Path != "PUT /v1/self/dashboard-preferences" {
				t.Error("unexpected mutation")
			}
			raw, _ := io.ReadAll(req.Body)
			var envelope map[string]json.RawMessage
			if json.Unmarshal(raw, &envelope) != nil || len(envelope) != 1 || string(envelope["prefs"]) != prefsTestDoc {
				t.Errorf("unexpected prefs schema %s", raw)
			}
			req.Body = io.NopCloser(bytes.NewReader(raw))
		}
		backend.ServeHTTP(w, req)
	}))
	if _, err := r.Read(t.Context(), ReadRequest{Resource: ResourcePreferences}); err != nil {
		t.Fatal(err)
	}
	raw, err := r.StoreTheme(t.Context(), "amber")
	if err != nil {
		t.Fatal(err)
	}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, []string{"GET /v1/self/dashboard-preferences", "PUT /v1/self/dashboard-preferences"}) {
		t.Fatalf("unexpected methods %v", got)
	}
	w := httptest.NewRecorder()
	prefsHandler(cfg).ServeHTTP(w, httptest.NewRequest("PUT", "/api/prefs", strings.NewReader(`{"prefs":`+prefsTestDoc+`}`)))
	if !bytes.Equal(raw, w.Body.Bytes()) {
		t.Fatal("theme projection differs")
	}
}

func TestReaderCollectionBoundsAndContext(t *testing.T) {
	r, err := NewReader(Config{Endpoint: "https://unused.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", `{"partial":`, `{} {}`, strings.Repeat(" ", maxReaderResponseBytes) + `{}`} {
		raw, err := r.invoke(t.Context(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, value)
		}), "GET", "/api/self", "", nil, nil)
		assertReaderError(t, raw, err, 502, "")
	}
	// Accept exactly the byte cap; crossing it after a complete JSON value
	// still rejects everything, and subsequent writes cannot recover it.
	full := `"` + strings.Repeat("x", maxReaderResponseBytes-2) + `"`
	raw, err := r.invoke(t.Context(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, full)
	}), "GET", "/api/self", "", nil, nil)
	if err != nil || len(raw) != maxReaderResponseBytes {
		t.Fatalf("exact cap rejected: %v", err)
	}
	raw, err = r.invoke(t.Context(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, full)
		if _, err := w.Write([]byte(" ")); !errors.Is(err, io.ErrShortWrite) {
			t.Error("overflow not signaled")
		}
		if _, err := w.Write([]byte(`{}`)); !errors.Is(err, io.ErrShortWrite) {
			t.Error("overflow recovered")
		}
	}), "GET", "/api/self", "", nil, nil)
	assertReaderError(t, raw, err, 502, "")
	ctx, cancel := context.WithCancel(t.Context())
	raw, err = r.invoke(ctx, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		deadline, ok := req.Context().Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Error("missing ten-second context cap")
		}
		_, _ = io.WriteString(w, `{}`)
		cancel()
	}), "GET", "/api/self", "", nil, nil)
	assertReaderError(t, raw, err, 504, "")
	raw, err = r.Read(ctx, ReadRequest{Resource: ResourceThemes})
	assertReaderError(t, raw, err, 504, "")
	parent, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	parentDeadline, _ := parent.Deadline()
	_, err = r.invoke(parent, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		deadline, _ := req.Context().Deadline()
		if !deadline.Equal(parentDeadline) {
			t.Error("earlier caller deadline extended")
		}
		writeJSON(w, map[string]any{})
	}), "GET", "/api/self", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestReaderFakeUpstreamCancellationAndOverflow(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		entered := make(chan struct{})
		ended := make(chan struct{})
		r, _ := newTestReader(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			_, _ = io.WriteString(w, `{"transcripts":[`)
			w.(http.Flusher).Flush()
			close(entered)
			<-req.Context().Done()
			close(ended)
		}))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { <-entered; cancel() }()
		raw, err := r.Read(ctx, ReadRequest{Resource: ResourceTranscripts})
		assertReaderError(t, raw, err, 504, "")
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream context not canceled")
		}
	})
	t.Run("projection overflow", func(t *testing.T) {
		r, _ := newTestReader(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"transcripts": []client.Transcript{{ID: strings.Repeat("x", maxReaderResponseBytes)}}})
		}))
		raw, err := r.Read(t.Context(), ReadRequest{Resource: ResourceTranscripts})
		assertReaderError(t, raw, err, 502, "")
	})
}

func TestReaderConcurrentCapabilityReuse(t *testing.T) {
	var probes atomic.Int32
	r, _ := newTestReader(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("observational") == "probe" {
			probes.Add(1)
		}
		if answerFactReadProbe(w, req) {
			return
		}
		writeTestJSON(t, w, stubcell.Facts())
	}))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := r.Read(t.Context(), ReadRequest{Resource: ResourceFacts}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if probes.Load() != 1 {
		t.Fatalf("concurrent probes = %d", probes.Load())
	}
}
