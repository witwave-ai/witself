package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/routeinventory"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

// Later routes are source-reviewed: the recorder deliberately stops the first
// non-identity HTTP call with a JSON 404. This is a client routing/token guard,
// not a substitute for the server's two-account PostgreSQL matrix.
type mcpToolRow struct {
	Tool            string
	Routes          []string
	RouteCites      []string
	Args            map[string]any
	StopsBeforeHTTP string
	LocalOnly       string
}

func mcpToolMatrixRows() []mcpToolRow {
	rows := mcpToolMatrixCoreRows()
	rows = append(rows, mcpToolMatrixMemoryRows()...)
	rows = append(rows, mcpToolMatrixAuxRows()...)
	slices.SortFunc(rows, func(a, b mcpToolRow) int { return strings.Compare(a.Tool, b.Tool) })
	return rows
}

func TestMCPToolRouteMatrixCoversEveryTool(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	defer mcpToolMatrixHomeFiles(t)

	apiRoutes := mcpToolMatrixAPIRoutes(t)
	fixture := mcpToolMatrixBinding(t)
	// Listing tools must not connect to the API. The only endpoint in this
	// fixture is loopback, and no local installed binding can match its names.
	fixture.cfg.Endpoint = "http://127.0.0.1:1"
	union := mcpToolMatrixToolUnion(t, fixture.cfg)
	rows := make(map[string]mcpToolRow)
	citeRE := regexp.MustCompile(`^[a-zA-Z0-9_./-]+\.go:[1-9][0-9]*$`)
	identityOwners := map[string]string{
		"GET /v1/self":                      "witself.self.show",
		"GET /v1/memory-curation-preflight": "witself.memory.curation.preflight",
	}
	for _, row := range mcpToolMatrixRows() {
		if _, exists := rows[row.Tool]; exists {
			t.Errorf("duplicate MCP route-matrix row %q", row.Tool)
		}
		rows[row.Tool] = row
		if _, exists := union[row.Tool]; !exists {
			t.Errorf("stale MCP route-matrix row %q", row.Tool)
		}
		if row.LocalOnly != "" && row.StopsBeforeHTTP != "" {
			t.Errorf("%s: LocalOnly and StopsBeforeHTTP are mutually exclusive", row.Tool)
		}
		for _, reason := range []string{row.LocalOnly, row.StopsBeforeHTTP} {
			if reason != "" && (strings.TrimSpace(reason) == "" || len(reason) > 300) {
				t.Errorf("%s: exception reason must be 1..300 nonblank bytes", row.Tool)
			}
		}
		if row.LocalOnly != "" && len(row.Routes) != 0 {
			t.Errorf("%s: LocalOnly row has routes", row.Tool)
		}
		if row.LocalOnly == "" && row.StopsBeforeHTTP == "" && len(row.Routes) == 0 {
			t.Errorf("%s: HTTP row has no first route", row.Tool)
		}
		if len(row.RouteCites) != max(0, len(row.Routes)-1) {
			t.Errorf("%s: every route after the first needs one RouteCites entry", row.Tool)
		}
		seen := make(map[string]bool)
		for _, pattern := range row.Routes {
			if owner, identity := identityOwners[pattern]; identity && owner != row.Tool {
				t.Errorf("%s: incidental identity verification cannot be a declared tool route", row.Tool)
			}
			if !apiRoutes[pattern] {
				t.Errorf("%s: route %q is not an API inventory pattern", row.Tool, pattern)
			}
			if seen[pattern] {
				t.Errorf("%s: duplicate route %q", row.Tool, pattern)
			}
			seen[pattern] = true
		}
		for _, cite := range row.RouteCites {
			if !citeRE.MatchString(cite) {
				t.Errorf("%s: invalid route citation %q; want file.go:line", row.Tool, cite)
			}
		}
	}
	for name := range union {
		if _, exists := rows[name]; !exists {
			t.Errorf("MCP tool %q has no route-matrix row; add one to cmd/witself/mcp_tool_route_matrix_test.go", name)
		}
	}
	t.Logf("MCP tool union: %d; matrix rows: %d", len(union), len(rows))
}

func TestMCPToolsCallOnlyTheirMatrixRoutes(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	apiRoutes := mcpToolMatrixAPIRoutes(t)
	fixture := mcpToolMatrixBinding(t)
	var active atomic.Pointer[mcpToolMatrixRecorder]
	active.Store(&mcpToolMatrixRecorder{})
	mux := http.NewServeMux()
	for pattern := range apiRoutes {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			recorder := active.Load()
			recorder.record(r)
			identity := fixture.identity
			if recorder.mismatchedIdentity {
				identity.AgentID = "agent_bbbbbbbbbbbbbbbb"
			}
			w.Header().Set("Content-Type", "application/json")
			switch r.Pattern {
			case "GET /v1/self":
				_ = json.NewEncoder(w).Encode(client.SelfDigest{
					SchemaVersion: "witself.v0", Identity: identity,
					PrimaryFacts: []client.SelfFact{}, SalientMemories: []client.SelfMemory{},
					Index: client.SelfIndex{Kinds: []string{}, Tags: []string{}, Counts: map[string]int{}},
				})
			case "GET /v1/memory-curation-preflight":
				_ = json.NewEncoder(w).Encode(client.MemoryCurationPreflight{
					Principal: client.MemoryCurationPreflightPrincipal{
						AccountID: identity.AccountID, RealmID: identity.RealmID,
						AgentID: identity.AgentID, AgentName: identity.AgentName,
					},
					Credential: client.MemoryCurationPreflightCredential{AccessProfile: mcpProfileFull},
				})
			default:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"schema_version":"witself.v0","error":"not found"}`))
			}
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		active.Load().record(r)
		t.Error("MCP request outside API inventory (method/path and headers withheld)")
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fixture.cfg.Endpoint = srv.URL

	// T8 and its subtests must stay serial: both the transport and homes are
	// process-wide. The production API clients leave Transport unset.
	transport := http.DefaultTransport
	http.DefaultTransport = mcpToolMatrixRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != srv.Listener.Addr().String() {
			t.Error("MCP request outside recorder host (URL and headers withheld)")
			return nil, errors.New("MCP request outside recorder host")
		}
		return transport.RoundTrip(req)
	})
	t.Cleanup(func() { http.DefaultTransport = transport })
	backend := configuredMCPBackend{cfg: fixture.cfg, curationProfile: mcpProfileFull}
	session := mcpToolMatrixSession(t, backend, mcpServerOptions{Profile: mcpProfileFull})
	tools := mcpToolMatrixToolNames(t, session)
	rows := mcpToolMatrixRows()
	if len(rows) == 0 {
		t.Fatal("MCP route matrix is empty")
	}
	for _, row := range rows {
		t.Run(row.Tool, func(t *testing.T) {
			t.Setenv("WITSELF_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			defer mcpToolMatrixHomeFiles(t)
			if !tools[row.Tool] {
				t.Fatal("matrix tool is absent from tools/list")
			}
			recorder := &mcpToolMatrixRecorder{}
			active.Store(recorder)
			// HTTP errors are expected. Do not print tool output: even a local
			// password generator returns a value that must stay out of logs.
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: row.Tool, Arguments: row.Args})
			if err != nil {
				t.Fatal("MCP tool invocation failed at the transport/schema boundary")
			}
			if result == nil {
				t.Fatal("MCP tool invocation returned no result")
			}
			recorded := recorder.snapshot()
			if row.LocalOnly == "" {
				mcpToolMatrixRequireIdentityFirst(t, row.Tool, recorded)
			}
			var first string
			for _, call := range recorded {
				if call.bearer != fixture.bearer {
					t.Error("MCP bearer differs from the trimmed binding token file (values withheld)")
				}
				identity := mcpToolMatrixIdentityRoute(call.pattern)
				declared := slices.Contains(row.Routes, call.pattern)
				if !declared && !identity {
					t.Errorf("recorded route %q is outside this tool's Routes", call.pattern)
				}
				if row.LocalOnly != "" || row.StopsBeforeHTTP != "" {
					if !identity {
						t.Errorf("local/early-stop row recorded non-identity route %q", call.pattern)
					}
				} else if first == "" && (declared || !identity) {
					first = call.pattern
				}
			}
			if row.LocalOnly == "" && row.StopsBeforeHTTP == "" {
				if first == "" {
					t.Fatal("no matrix route recorded; local validation or identity failure cannot satisfy this row")
				}
				if len(row.Routes) == 0 || first != row.Routes[0] {
					t.Errorf("first route = %q; want the first declared matrix route", first)
				}
			}
			if row.LocalOnly != "" && len(recorded) != 0 {
				t.Error("LocalOnly tool sent an HTTP request")
			}
		})
	}
	t.Run("identity_mismatch", func(t *testing.T) {
		for _, tool := range []string{"witself.message.ack", "witself.memory.curation.request"} {
			t.Run(tool, func(t *testing.T) {
				t.Setenv("WITSELF_HOME", t.TempDir())
				t.Setenv("HOME", t.TempDir())
				defer mcpToolMatrixHomeFiles(t)
				index := slices.IndexFunc(rows, func(row mcpToolRow) bool { return row.Tool == tool })
				if index < 0 {
					t.Fatal("identity mismatch probe has no matrix row")
				}
				recorder := &mcpToolMatrixRecorder{mismatchedIdentity: true}
				active.Store(recorder)
				result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: rows[index].Args})
				if err != nil {
					t.Fatal("identity mismatch invocation failed at the transport/schema boundary")
				}
				if result == nil || !result.IsError {
					t.Error("mismatched identity must return a tool error (output withheld)")
				}
				recorded := recorder.snapshot()
				mcpToolMatrixRequireIdentityFirst(t, tool, recorded)
				for _, call := range recorded {
					if call.bearer != fixture.bearer {
						t.Error("MCP bearer differs from the trimmed binding token file (values withheld)")
					}
					if !mcpToolMatrixIdentityRoute(call.pattern) {
						t.Errorf("mismatched identity reached non-identity route %q", call.pattern)
					}
				}
			})
		}
	})
}

func mcpToolMatrixAPIRoutes(t *testing.T) map[string]bool {
	t.Helper()
	root, err := routeinventory.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	routes, err := routeinventory.ServerRoutes(root)
	if err != nil {
		t.Fatal(err)
	}
	api := make(map[string]bool)
	for _, route := range routes {
		if route.Listener == routeinventory.ListenerAPI {
			api[route.Pattern] = true
		}
	}
	if len(api) == 0 {
		t.Fatal("API route inventory is empty")
	}
	return api
}

type mcpToolMatrixFixture struct {
	cfg      transcriptcapture.Config
	identity client.SelfIdentity
	bearer   string
}

func mcpToolMatrixBinding(t *testing.T) mcpToolMatrixFixture {
	t.Helper()
	var random [10]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("generate isolated MCP binding suffix")
	}
	suffix := hex.EncodeToString(random[:])
	idSuffix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random[:]))
	tokenPath := filepath.Join(t.TempDir(), "binding-credential")
	if err := os.WriteFile(tokenPath, []byte("matrix-fixture-"+suffix+"\n"), 0o600); err != nil {
		t.Fatal("write isolated MCP fixture credential")
	}
	// Read only the synthetic credential just created in this test's TempDir.
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal("read isolated MCP fixture credential")
	}
	cfg := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCodex,
		Account: "tm-acct-" + suffix, AccountID: "acc_" + idSuffix,
		Realm: "tm-realm-" + suffix, RealmID: "realm_" + idSuffix,
		Agent: "tm-agent-" + suffix, AgentName: "tm-agent-" + suffix, AgentID: "agent_" + idSuffix,
		TokenFile: tokenPath,
	}
	return mcpToolMatrixFixture{
		cfg: cfg, bearer: "Bearer " + strings.TrimSpace(string(token)),
		identity: client.SelfIdentity{AccountID: cfg.AccountID, RealmID: cfg.RealmID, RealmName: cfg.Realm, AgentID: cfg.AgentID, AgentName: cfg.AgentName},
	}
}

func mcpToolMatrixToolUnion(t *testing.T, cfg transcriptcapture.Config) map[string]mcpServerOptions {
	t.Helper()
	union := make(map[string]mcpServerOptions)
	largest := 0
	for _, profile := range []string{mcpProfileFull, mcpProfileReadOnly, mcpProfileCuratorPreview, mcpProfileCuratorApply} {
		for _, noValues := range []bool{false, true} {
			opts := mcpServerOptions{Profile: profile, NoValueTools: noValues}
			backend := configuredMCPBackend{cfg: cfg, curationProfile: profile}
			session := mcpToolMatrixSession(t, backend, opts)
			profileTools := mcpToolMatrixToolNames(t, session)
			for name := range profileTools {
				if _, exists := union[name]; !exists {
					union[name] = opts
				}
			}
			largest = max(largest, len(profileTools))
			t.Logf("%s/no-value-tools=%t: %d tools", profile, noValues, len(profileTools))
		}
	}
	if len(union) == 0 || len(union) < largest {
		t.Fatal("MCP tool union is empty or smaller than a single profile")
	}
	return union
}

func mcpToolMatrixToolNames(t *testing.T, session *mcp.ClientSession) map[string]bool {
	t.Helper()
	names := make(map[string]bool)
	params := &mcp.ListToolsParams{}
	cursors := make(map[string]bool)
	for {
		page, err := session.ListTools(t.Context(), params)
		if err != nil {
			t.Fatal("list MCP profile tools")
		}
		for _, tool := range page.Tools {
			if names[tool.Name] {
				t.Errorf("duplicate MCP tool %q", tool.Name)
			}
			names[tool.Name] = true
		}
		if page.NextCursor == "" {
			break
		}
		if cursors[page.NextCursor] {
			t.Fatal("MCP tools/list repeated a cursor")
		}
		cursors[page.NextCursor] = true
		params.Cursor = page.NextCursor
	}
	if len(names) == 0 {
		t.Fatal("MCP profile listed no tools")
	}
	return names
}

func mcpToolMatrixSession(t *testing.T, backend configuredMCPBackend, opts mcpServerOptions) *mcp.ClientSession {
	t.Helper()
	server := newWitselfMCPServerForRuntimeOptions(backend, "", opts)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal("connect MCP matrix server")
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "tenant-matrix", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal("connect MCP matrix client")
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func mcpToolMatrixIdentityRoute(pattern string) bool {
	return pattern == "GET /v1/self" || pattern == "GET /v1/memory-curation-preflight"
}

func mcpToolMatrixRequireIdentityFirst(t *testing.T, tool string, calls []mcpToolMatrixCall) {
	t.Helper()
	want := "GET /v1/self"
	if strings.HasPrefix(tool, "witself.memory.curation.") {
		want = "GET /v1/memory-curation-preflight"
	}
	if len(calls) == 0 {
		t.Fatal("no identity request recorded")
	}
	if calls[0].pattern != want {
		t.Errorf("first request = %q; want identity route %q before any non-identity request", calls[0].pattern, want)
	}
}

type mcpToolMatrixRoundTripFunc func(*http.Request) (*http.Response, error)

func (f mcpToolMatrixRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type mcpToolMatrixCall struct {
	pattern string
	bearer  string
}

type mcpToolMatrixRecorder struct {
	mu                 sync.Mutex
	calls              []mcpToolMatrixCall
	mismatchedIdentity bool // Immutable after publishing this recorder.
}

func (r *mcpToolMatrixRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, mcpToolMatrixCall{pattern: req.Pattern, bearer: req.Header.Get("Authorization")})
}

func (r *mcpToolMatrixRecorder) snapshot() []mcpToolMatrixCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func mcpToolMatrixHomeFiles(t *testing.T) {
	t.Helper()
	for _, name := range []string{"WITSELF_HOME", "HOME"} {
		root := os.Getenv(name)
		var files []string
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				files = append(files, rel)
			}
			return nil
		})
		if err != nil {
			t.Error(fmt.Errorf("list pinned %s fixture filenames: %w", name, err))
		}
		t.Logf("pinned %s files (names only): %v", name, files)
	}
}

func mcpToolMatrixCoreRows() []mcpToolRow {
	return []mcpToolRow{
		{Tool: "witself.self.show", Routes: []string{"GET /v1/self"}},
		{Tool: "witself.agent.peers", Routes: []string{"GET /v1/self/peers"}},
		{Tool: "witself.fact.set", Routes: []string{"POST /v1/facts"}, Args: map[string]any{"predicate": "matrix/value", "value": "fixture", "idempotency_key": "tm-fact-set"}},
		// Preview and apply use distinct inventory patterns; the recorder exercises preview.
		{Tool: "witself.fact.delete", Routes: []string{"DELETE /v1/facts", "DELETE /v1/facts/{fact}"}, RouteCites: []string{"internal/client/fact.go:250"}, Args: map[string]any{"mode": "preview", "predicate": "matrix/value"}},
		{Tool: "witself.fact.propose", Routes: []string{"POST /v1/fact-candidates"}, Args: map[string]any{"predicate": "matrix/value", "value": "fixture", "reason": "Matrix fixture", "idempotency_key": "tm-fact-propose"}},
		// A6: transcript ownership is checked before opaque provenance is posted.
		{Tool: "witself.fact.propose_from_transcript", Routes: []string{"GET /v1/transcripts/{transcript}", "POST /v1/fact-candidates"}, RouteCites: []string{"cmd/witself/mcp.go:1446"}, Args: map[string]any{"transcript_id": "trn_aaaaaaaaaaaaaaaa", "entry_sequence": 1, "predicate": "matrix/value", "value": "fixture", "reason": "Matrix fixture", "idempotency_key": "tm-fact-transcript"}},
		{Tool: "witself.fact.review", Routes: []string{"GET /v1/fact-candidates"}},
		{Tool: "witself.fact.candidate.get", Routes: []string{"GET /v1/fact-candidates/{candidate}"}, Args: map[string]any{"candidate_id": "fcand_aaaaaaaaaaaaaaaa"}},
		{Tool: "witself.fact.confirm", Routes: []string{"POST /v1/fact-candidates/{action}"}, Args: map[string]any{"candidate_id": "fcand_aaaaaaaaaaaaaaaa", "idempotency_key": "tm-fact-confirm"}},
		{Tool: "witself.fact.reject", Routes: []string{"POST /v1/fact-candidates/{action}"}, Args: map[string]any{"candidate_id": "fcand_aaaaaaaaaaaaaaaa", "idempotency_key": "tm-fact-reject"}},
		{Tool: "witself.fact.get", Routes: []string{"GET /v1/facts"}, Args: map[string]any{"predicate": "matrix/value"}},
		{Tool: "witself.fact.list", Routes: []string{"GET /v1/facts"}},
		{Tool: "witself.fact.upcoming", Routes: []string{"GET /v1/fact-occurrences"}},
		{Tool: "witself.fact.subject.set", Routes: []string{"PUT /v1/fact-subjects/{subject}"}, Args: map[string]any{"canonical_key": "matrix_subject"}},
		{Tool: "witself.fact.subject.alias", Routes: []string{"POST /v1/fact-subjects/{subject}/aliases"}, Args: map[string]any{"canonical_key": "matrix_subject", "alias": "matrix alias"}},
		{Tool: "witself.fact.subject.list", Routes: []string{"GET /v1/fact-subjects"}},
		{Tool: "witself.transcript.list", Routes: []string{"GET /v1/transcripts"}},
		{Tool: "witself.transcript.get", Routes: []string{"GET /v1/transcripts/{transcript}"}, Args: map[string]any{"transcript_id": "trn_aaaaaaaaaaaaaaaa"}},
		{Tool: "witself.transcript.tail", Routes: []string{"GET /v1/transcripts/{transcript}"}, Args: map[string]any{"transcript_id": "trn_aaaaaaaaaaaaaaaa"}},
		{Tool: "witself.message.send", Routes: []string{"POST /v1/messages"}, Args: map[string]any{"to": "agent_aaaaaaaaaaaaaaaa", "body": "Matrix fixture", "idempotency_key": "tm-message-send"}},
		{Tool: "witself.message.reply", Routes: []string{"POST /v1/messages/{action}"}, Args: map[string]any{"message_id": "msg_aaaaaaaaaaaaaaaa", "body": "Matrix fixture", "idempotency_key": "tm-message-reply"}},
		{Tool: "witself.message.list", Routes: []string{"GET /v1/messages"}},
		{Tool: "witself.message.listen", Routes: []string{"POST /v1/messages:listen"}, Args: map[string]any{"wait_seconds": 0}},
		{Tool: "witself.message.read", Routes: []string{"POST /v1/messages/{action}"}, Args: map[string]any{"message_id": "msg_aaaaaaaaaaaaaaaa"}},
		{Tool: "witself.message.ack", Routes: []string{"POST /v1/messages/{action}"}, Args: map[string]any{"message_id": "msg_aaaaaaaaaaaaaaaa"}},
		{Tool: "witself.message.claim", Routes: []string{"POST /v1/messages/{action}"}, Args: map[string]any{"message_id": "msg_aaaaaaaaaaaaaaaa", "idempotency_key": "tm-message-claim"}},
		{Tool: "witself.message.renew", Routes: []string{"POST /v1/messages/{action}"}, Args: map[string]any{"message_id": "msg_aaaaaaaaaaaaaaaa", "claim_id": "mcl_aaaaaaaaaaaaaaaa", "generation": 1}},
		{Tool: "witself.message.release", Routes: []string{"POST /v1/messages/{action}"}, Args: map[string]any{"message_id": "msg_aaaaaaaaaaaaaaaa", "claim_id": "mcl_aaaaaaaaaaaaaaaa", "generation": 1}},
		{Tool: "witself.message.complete", Routes: []string{"POST /v1/messages/{action}"}, Args: map[string]any{"message_id": "msg_aaaaaaaaaaaaaaaa", "claim_id": "mcl_aaaaaaaaaaaaaaaa", "generation": 1, "body": "Matrix fixture", "idempotency_key": "tm-message-complete"}},
		{Tool: "witself.message.request.open", Routes: []string{"POST /v1/message-requests"}, Args: map[string]any{"body": "Matrix fixture", "idempotency_key": "tm-request-open"}},
		{Tool: "witself.message.request.list", Routes: []string{"GET /v1/message-requests"}},
		{Tool: "witself.message.request.show", Routes: []string{"GET /v1/message-requests/{request}"}, Args: map[string]any{"request_id": "mrq_aaaaaaaaaaaaaaaa"}},
		{Tool: "witself.message.request.offer", Routes: []string{"POST /v1/message-requests/{action}"}, Args: map[string]any{"request_id": "mrq_aaaaaaaaaaaaaaaa", "body": "Matrix fixture", "idempotency_key": "tm-request-offer"}},
		{Tool: "witself.message.request.decline", Routes: []string{"POST /v1/message-requests/{action}"}, Args: map[string]any{"request_id": "mrq_aaaaaaaaaaaaaaaa"}},
		{Tool: "witself.message.request.select", Routes: []string{"POST /v1/message-requests/{action}"}, Args: map[string]any{"request_id": "mrq_aaaaaaaaaaaaaaaa", "selected_agent_ids": []string{"agent_aaaaaaaaaaaaaaaa"}, "idempotency_key": "tm-request-select"}},
		{Tool: "witself.message.request.cancel", Routes: []string{"POST /v1/message-requests/{action}"}, Args: map[string]any{"request_id": "mrq_aaaaaaaaaaaaaaaa"}},
		{Tool: "witself.message.request.claim", Routes: []string{"POST /v1/message-requests/{action}"}, Args: map[string]any{"request_id": "mrq_aaaaaaaaaaaaaaaa", "idempotency_key": "tm-request-claim"}},
		{Tool: "witself.message.request.renew", Routes: []string{"POST /v1/message-requests/{action}"}, Args: map[string]any{"request_id": "mrq_aaaaaaaaaaaaaaaa", "claim_id": "mrc_aaaaaaaaaaaaaaaa", "generation": 1}},
		{Tool: "witself.message.request.release", Routes: []string{"POST /v1/message-requests/{action}"}, Args: map[string]any{"request_id": "mrq_aaaaaaaaaaaaaaaa", "claim_id": "mrc_aaaaaaaaaaaaaaaa", "generation": 1}},
		{Tool: "witself.message.request.complete", Routes: []string{"POST /v1/message-requests/{action}"}, Args: map[string]any{"request_id": "mrq_aaaaaaaaaaaaaaaa", "claim_id": "mrc_aaaaaaaaaaaaaaaa", "generation": 1, "body": "Matrix fixture", "idempotency_key": "tm-request-complete"}},
	}
}

func mcpToolMatrixMemoryRows() []mcpToolRow {
	memoryID := "mem_aaaaaaaaaaaaaaaa"
	requestID := "mcrq_aaaaaaaaaaaaaaaa"
	runID := "mrun_aaaaaaaaaaaaaaaa"
	evidence := []any{map[string]any{"state": "unavailable", "unavailable_reason": "matrix fixture"}}
	capture := map[string]any{
		"content": "Matrix fixture narrative.", "kind": "lesson", "capture_reason": "manual",
		"evidence": evidence, "idempotency_key": "tm-replacement",
	}
	return []mcpToolRow{
		{Tool: "witself.fact.status", Routes: []string{"GET /v1/facts:status"}, Args: map[string]any{}},
		{Tool: "witself.memory.status", Routes: []string{"GET /v1/memories:status"}, Args: map[string]any{}},
		{Tool: "witself.memory.vector.profile.create", Routes: []string{"POST /v1/memory-vector-profiles"}, Args: map[string]any{
			"provider": "matrix", "model": "fixture", "recipe": "fixture", "recipe_version": "1",
			"dimensions": 2, "distance_metric": "cosine", "normalization": "none",
		}},
		{Tool: "witself.memory.vector.profile.list", Routes: []string{"GET /v1/memory-vector-profiles"}, Args: map[string]any{}},
		{Tool: "witself.memory.vector.set", Routes: []string{"POST /v1/memory-vectors"}, Args: map[string]any{
			"profile_id": "mvp_aaaaaaaaaaaaaaaa", "memory_id": memoryID, "memory_version": 1,
			"content_hash": strings.Repeat("a", 64), "vector": []float64{1, 0},
		}},
		{Tool: "witself.memory.capture", Routes: []string{"POST /v1/memories"}, Args: capture},
		{Tool: "witself.memory.read", Routes: []string{"GET /v1/memories/{memory}"}, Args: map[string]any{"memory_id": memoryID}},
		{Tool: "witself.memory.list", Routes: []string{"GET /v1/memories"}, Args: map[string]any{}},
		{Tool: "witself.memory.recall", Routes: []string{"POST /v1/memories:recall"}, Args: map[string]any{"query": "matrix fixture"}},
		{Tool: "witself.memory.history", Routes: []string{"GET /v1/memories/{memory}/history"}, Args: map[string]any{"memory_id": memoryID}},
		{Tool: "witself.memory.adjust", Routes: []string{"PATCH /v1/memories/{memory}"}, Args: map[string]any{
			"memory_id": memoryID, "expected_version": 1, "set_content": "Adjusted fixture narrative.", "idempotency_key": "tm-adjust",
		}},
		{Tool: "witself.memory.supersede", Routes: []string{"POST /v1/memories/{memory}/supersede"}, Args: map[string]any{
			"memory_id": memoryID, "expected_version": 1, "replacements": []any{capture}, "idempotency_key": "tm-supersede",
		}},
		{Tool: "witself.memory.forget", Routes: []string{"POST /v1/memories/{action}"}, Args: map[string]any{
			"memory_id": memoryID, "expected_version": 1, "idempotency_key": "tm-forget",
		}},
		{Tool: "witself.memory.restore", Routes: []string{"POST /v1/memories/{action}"}, Args: map[string]any{
			"memory_id": memoryID, "expected_version": 1, "idempotency_key": "tm-restore",
		}},
		{Tool: "witself.memory.reactivate", Routes: []string{"POST /v1/memories/{action}"}, Args: map[string]any{
			"memory_id": memoryID, "expected_version": 1, "idempotency_key": "tm-reactivate",
		}},
		{Tool: "witself.memory.evidence.resolve", Routes: []string{"POST /v1/memory-evidence/{evidence}/resolution"}, Args: map[string]any{
			"evidence_id": "mev_aaaaaaaaaaaaaaaa", "unresolvable_reason": "matrix fixture", "idempotency_key": "tm-evidence",
		}},
		{Tool: "witself.memory.delete", Routes: []string{"DELETE /v1/memories/{memory}"}, Args: map[string]any{"mode": "preview", "memory_id": memoryID}},
		{Tool: "witself.memory.curation.preflight", Routes: []string{"GET /v1/memory-curation-preflight"}, Args: map[string]any{}},
		{Tool: "witself.memory.curation.requests", Routes: []string{"GET /v1/memory-curation-requests"}, Args: map[string]any{}},
		{Tool: "witself.memory.curation.request.get", Routes: []string{"GET /v1/memory-curation-requests/{request}"}, Args: map[string]any{"request_id": requestID}},
		{Tool: "witself.memory.curation.request", Routes: []string{"POST /v1/memory-curation-requests"}, Args: map[string]any{"idempotency_key": "tm-curation-request"}},
		{Tool: "witself.memory.curation.start", Routes: []string{"POST /v1/memory-curation-requests/{request}/start"}, Args: map[string]any{"request_id": requestID, "idempotency_key": "tm-curation-start"}},
		{Tool: "witself.memory.curation.run.get", Routes: []string{"GET /v1/memory-curation-runs/{run}"}, Args: map[string]any{"run_id": runID}},
		{Tool: "witself.memory.curation.renew", Routes: []string{"POST /v1/memory-curation-runs/{run}/renew"}, Args: map[string]any{
			"run_id": runID, "fencing_generation": 1, "idempotency_key": "tm-curation-renew",
		}},
		{Tool: "witself.memory.curation.get", Routes: []string{"GET /v1/memory-curation-runs/{run}/inputs"}, Args: map[string]any{"run_id": runID, "fencing_generation": 1}},
		{Tool: "witself.memory.curation.plan", Routes: []string{"POST /v1/memory-curation-runs/{run}/plan"}, Args: map[string]any{
			"run_id": runID, "fencing_generation": 1, "idempotency_key": "tm-curation-plan",
			"draft": map[string]any{"schema": "witself.memory-plan.v1", "draft_revision": 1, "actions": []any{}},
		}},
		{Tool: "witself.memory.curation.plan.get", Routes: []string{"GET /v1/memory-curation-runs/{run}/plan"}, Args: map[string]any{"run_id": runID, "fencing_generation": 1}},
		{Tool: "witself.memory.curation.apply", Routes: []string{"POST /v1/memory-curation-runs/{run}/apply"}, Args: map[string]any{
			"run_id": runID, "fencing_generation": 1, "plan_revision": 1, "plan_hash": strings.Repeat("a", 64), "idempotency_key": "tm-curation-apply",
		}},
		{Tool: "witself.memory.curation.cancel", Routes: []string{"POST /v1/memory-curation-runs/{run}/cancel"}, Args: map[string]any{"run_id": runID, "fencing_generation": 1, "idempotency_key": "tm-curation-cancel"}},
		{Tool: "witself.memory.curation.abandon", Routes: []string{"POST /v1/memory-curation-runs/{run}/abandon"}, Args: map[string]any{"run_id": runID, "fencing_generation": 1, "idempotency_key": "tm-curation-abandon"}},
		{Tool: "witself.memory.curation.rollback", Routes: []string{"POST /v1/memory-curation-runs/{run}/rollback"}, Args: map[string]any{
			"run_id": runID, "apply_receipt_id": "mrec_aaaaaaaaaaaaaaaa", "expected_produced_heads": []any{}, "idempotency_key": "tm-curation-rollback",
		}},
		{Tool: "witself.memory.curation.status", Routes: []string{"GET /v1/memory-curation-status"}, Args: map[string]any{"run_id": runID}},
	}
}

// The secret-create fixture includes one sensitive field, so its first request
// after identity is the authenticated vault binding lookup. The recorder's 404
// stops it before reconciliation can write a local key or create journal.
func mcpToolMatrixAuxRows() []mcpToolRow {
	const secretID = "sec_aaaaaaaaaaaaaaaa"
	const fieldID = "fld_aaaaaaaaaaaaaaaa"
	const emailID = "emsg_aaaaaaaaaaaaaaaa"
	const claimID = "ecl_aaaaaaaaaaaaaaaa"
	const retryKey = "tenant-matrix-aux-operation"
	return []mcpToolRow{
		{Tool: "witself.secret.status", Routes: []string{"GET /v1/secrets:status"}},
		{Tool: "witself.secret.search", Routes: []string{"GET /v1/secrets"}},
		{Tool: "witself.secret.show", Routes: []string{"GET /v1/secrets/{secret}"}, Args: map[string]any{"secret_id": secretID}},
		{
			Tool:       "witself.secret.create",
			Routes:     []string{"GET /v1/vault/key-epochs/current", "POST /v1/vault/key-epochs", "POST /v1/secrets"},
			RouteCites: []string{"internal/secretclient/secretclient.go:1149", "internal/secretclient/secretclient.go:371"},
			Args:       map[string]any{"name": "tenant matrix fixture", "fields": []any{map[string]any{"name": "password", "kind": "password", "sensitive": true, "value": "synthetic-fixture-value"}}, "idempotency_key": retryKey},
		},
		{Tool: "witself.secret.delete", Routes: []string{"POST /v1/secrets/{action}"}, Args: map[string]any{"secret_id": secretID, "expected_row_version": 1, "idempotency_key": retryKey}},
		{
			Tool:       "witself.secret.reveal",
			Routes:     []string{"GET /v1/secrets/{secret}", "GET /v1/vault/key-epochs/current", "POST /v1/vault/key-epochs", "POST /v1/secrets/{secret}/fields/{action}"},
			RouteCites: []string{"internal/secretclient/secretclient.go:1008", "internal/secretclient/secretclient.go:1149", "internal/secretclient/secretclient.go:966"},
			Args:       map[string]any{"secret_id": secretID, "field_id": fieldID, "idempotency_key": retryKey},
		},
		{Tool: "witself.password.generate", LocalOnly: "cmd/witself/mcp_secret.go:423-430 generates a password locally without a backend or API call."},
		{
			Tool:       "witself.totp.code",
			Routes:     []string{"GET /v1/secrets/{secret}", "GET /v1/vault/key-epochs/current", "POST /v1/vault/key-epochs", "POST /v1/secrets/{secret}/fields/{action}"},
			RouteCites: []string{"internal/secretclient/secretclient.go:1008", "internal/secretclient/secretclient.go:1149", "internal/secretclient/secretclient.go:966"},
			Args:       map[string]any{"secret_id": secretID, "field_id": fieldID, "idempotency_key": retryKey},
		},
		{Tool: "witself.avatar.show", Routes: []string{"GET /v1/self/avatar"}},
		{Tool: "witself.avatar.history", Routes: []string{"GET /v1/self/avatar/history"}},
		{Tool: "witself.avatar.version.show", Routes: []string{"GET /v1/self/avatar/versions/{version}"}, Args: map[string]any{"version": 1}},
		{Tool: "witself.avatar.style.show", Routes: []string{"GET /v1/self/avatar/style"}},
		{
			Tool: "witself.avatar.propose", Routes: []string{"POST /v1/self/avatar/proposals"},
			Args: map[string]any{"expected_profile_revision": 1, "style_pack_id": "tenant-matrix-style", "style_pack_version": 1, "subject_form": "symbolic", "description": "Synthetic route fixture", "visual_spec": map[string]any{"fixture": true}, "svg": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><circle cx="16" cy="16" r="8"/></svg>`, "idempotency_key": retryKey},
		},
		{Tool: "witself.avatar.activate", Routes: []string{"POST /v1/self/avatar:activate"}, Args: map[string]any{"version": 1, "expected_profile_revision": 1, "idempotency_key": retryKey}},
		{Tool: "witself.avatar.rollback", Routes: []string{"POST /v1/self/avatar:rollback"}, Args: map[string]any{"version": 1, "expected_profile_revision": 1, "idempotency_key": retryKey}},
		{Tool: "witself.avatar.reset", Routes: []string{"POST /v1/self/avatar:reset"}, Args: map[string]any{"expected_profile_revision": 1, "idempotency_key": retryKey}},
		{Tool: "witself.avatar.generation.fail", Routes: []string{"POST /v1/self/avatar:generation-failed"}, Args: map[string]any{"expected_profile_revision": 1, "reason_code": "fixture_failure", "idempotency_key": retryKey}},
		{Tool: "witself.email.status", Routes: []string{"GET /v1/email:status"}},
		{Tool: "witself.email.address.show", Routes: []string{"GET /v1/email/address"}},
		{Tool: "witself.email.list", Routes: []string{"GET /v1/email"}},
		{Tool: "witself.email.listen", Routes: []string{"POST /v1/email:listen"}, Args: map[string]any{"wait_seconds": 0}},
		{Tool: "witself.email.read", Routes: []string{"POST /v1/email/{action}"}, Args: map[string]any{"message_id": emailID}},
		{Tool: "witself.email.code.candidates", Routes: []string{"POST /v1/email/{action}"}, Args: map[string]any{"message_id": emailID}},
		{Tool: "witself.email.code.consume", Routes: []string{"POST /v1/email/{action}"}, Args: map[string]any{"message_id": emailID}},
		{Tool: "witself.email.ack", Routes: []string{"POST /v1/email/{action}"}, Args: map[string]any{"message_id": emailID}},
		{Tool: "witself.email.claim", Routes: []string{"POST /v1/email/{action}"}, Args: map[string]any{"message_id": emailID, "idempotency_key": retryKey}},
		{Tool: "witself.email.renew", Routes: []string{"POST /v1/email/{action}"}, Args: map[string]any{"message_id": emailID, "claim_id": claimID, "generation": 1}},
		{Tool: "witself.email.release", Routes: []string{"POST /v1/email/{action}"}, Args: map[string]any{"message_id": emailID, "claim_id": claimID, "generation": 1}},
		{Tool: "witself.email.complete", Routes: []string{"POST /v1/email/{action}"}, Args: map[string]any{"message_id": emailID, "claim_id": claimID, "generation": 1, "idempotency_key": retryKey}},
		{Tool: "witself.email.send", Routes: []string{"POST /v1/email:send"}, Args: map[string]any{"to": "fixture@example.invalid", "subject": "Synthetic route fixture", "text": "Synthetic route fixture", "idempotency_key": retryKey}},
		{Tool: "witself.email.reply", Routes: []string{"POST /v1/email/{action}"}, Args: map[string]any{"inbound_message_id": emailID, "text": "Synthetic route fixture", "idempotency_key": retryKey}},
		{Tool: "witself.email.sent.list", Routes: []string{"GET /v1/email/sent"}},
		{Tool: "witself.email.sent.show", Routes: []string{"GET /v1/email/sent/{id}"}, Args: map[string]any{"message_id": "esnd_aaaaaaaaaaaaaaaa"}},
	}
}
