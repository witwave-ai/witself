package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

// This is a configured-backend protocol fixture, complementary to the real API
// handler tests. Its retained maps do not prove SQL isolation or disk recovery.
// All identities, content, credentials and evidence below are synthetic. Failure
// messages deliberately contain no fixture values or returned response bodies.
const offlineReadMCPToken = "offline-read-fixture-agent"

type offlineReadMCPTransport struct {
	allowed   map[string]bool
	transport *http.Transport
	denied    atomic.Int32
}

func (g *offlineReadMCPTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !g.allowed[r.URL.Scheme+"://"+r.URL.Host] {
		g.denied.Add(1)
		return nil, errors.New("offline fixture refused origin")
	}
	return g.transport.RoundTrip(r)
}

func offlineReadMCPContext(t *testing.T) context.Context {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := t.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	t.Cleanup(cancel)
	return ctx
}

func offlineReadMCPConfig(t *testing.T) transcriptcapture.Config {
	t.Helper()
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "WITSELF_HOME", "DSH_HOME"} {
		t.Setenv(key, filepath.Join(home, key))
	}
	cfg := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeClaudeCode, CaptureMode: transcriptcapture.ModeMessages, HookMode: transcriptcapture.HookModeUser,
		Account: "offline-account", AccountID: "acc_offline_read", Realm: "offline-realm", RealmID: "rlm_offline_read",
		Agent: "offline-agent", AgentID: "agt_offline_read", AgentName: "offline-agent",
		Location: transcriptcapture.Location{ID: "loc_offline_read", Name: "offline-read"},
	}
	if local.Save(cfg.Account, local.Account{ID: cfg.AccountID}, "offline-fixture-owner") != nil {
		t.Fatal("fixture account setup failed")
	}
	tokenPath, err := local.AgentTokenPath(cfg.Account, cfg.Realm, cfg.Agent)
	if err != nil {
		t.Fatal("fixture token path failed")
	}
	if os.MkdirAll(filepath.Dir(tokenPath), 0o700) != nil || os.WriteFile(tokenPath, []byte(offlineReadMCPToken), 0o600) != nil {
		t.Fatal("fixture credential setup failed")
	}
	if transcriptcapture.SaveConfig(cfg) != nil {
		t.Fatal("fixture binding persistence failed")
	}
	return cfg
}

func offlineReadMCPIsolation(t *testing.T, endpoints ...string) (*atomic.Int32, *offlineReadMCPTransport) {
	t.Helper()
	directory := &atomic.Int32{}
	originalLocate := connectionLocate
	connectionLocate = func(context.Context, string, string) (string, string, error) {
		directory.Add(1)
		return "", "", errors.New("offline directory refused")
	}
	originalTransport := http.DefaultTransport
	guard := &offlineReadMCPTransport{allowed: map[string]bool{}, transport: &http.Transport{Proxy: nil}}
	for _, endpoint := range endpoints {
		guard.allowed[endpoint] = true
	}
	http.DefaultTransport = guard
	t.Cleanup(func() {
		http.DefaultTransport = originalTransport
		connectionLocate = originalLocate
		guard.transport.CloseIdleConnections()
		if directory.Load() != 0 {
			t.Error("directory discovery was attempted")
		}
		if guard.denied.Load() != 0 {
			t.Error("disallowed origin request was attempted")
		}
	})
	return directory, guard
}

type offlineReadMCPSession struct {
	client    *mcp.ClientSession
	server    *mcp.ServerSession
	closeOnce sync.Once
}

func (s *offlineReadMCPSession) close() {
	s.closeOnce.Do(func() { _ = s.client.Close(); _ = s.server.Close() })
}

func offlineReadMCPOpen(t *testing.T, cfg transcriptcapture.Config, profile string) *offlineReadMCPSession {
	t.Helper()
	backend, err := newConfiguredMCPBackend(cfg, profile)
	if err != nil {
		t.Fatal("configured backend construction failed")
	}
	server := newWitselfMCPServerForRuntimeOptions(backend, "", mcpServerOptions{Profile: profile})
	ct, st := mcp.NewInMemoryTransports()
	ctx := offlineReadMCPContext(t)
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal("MCP server connection failed")
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "offline-read", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		_ = ss.Close()
		t.Fatal("MCP client connection failed")
	}
	session := &offlineReadMCPSession{client: cs, server: ss}
	t.Cleanup(session.close)
	return session
}

func offlineReadMCPCall(t *testing.T, session *offlineReadMCPSession, name string, args any, out any) {
	t.Helper()
	result, err := session.client.CallTool(offlineReadMCPContext(t), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || result.IsError {
		t.Fatal("MCP fixture tool call failed")
	}
	if out != nil {
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil || json.Unmarshal(raw, out) != nil {
			t.Fatal("MCP structured result decoding failed")
		}
	}
}

func offlineReadMCPRefusal(t *testing.T, session *offlineReadMCPSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.client.CallTool(offlineReadMCPContext(t), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal("MCP refusal transport failed")
	}
	if !result.IsError {
		t.Fatal("MCP identity refusal missing")
	}
	return result
}

type offlineReadMCPFixture struct {
	t                                                      *testing.T
	cfg                                                    transcriptcapture.Config
	server                                                 *httptest.Server
	mu                                                     sync.Mutex
	identity                                               client.SelfIdentity
	transcript                                             client.Transcript
	entries                                                []client.TranscriptEntry
	fact                                                   client.Fact
	memories                                               []client.Memory
	captures                                               []client.CaptureMemoryInput
	factSets                                               []client.SetFactInput
	recalls                                                []client.MemoryRecallInput
	events                                                 []string
	preflight                                              client.MemoryCurationPreflight
	request                                                client.MemoryCurationRequest
	run                                                    client.MemoryCurationRun
	plan                                                   json.RawMessage
	preview                                                client.MemoryCurationImpactPreview
	cursors                                                []string
	starts, plans, planGets, applies, requests, inputCalls int
	conflict                                               string
}

func offlineReadMCPFixtureNew(t *testing.T, cfg transcriptcapture.Config) *offlineReadMCPFixture {
	t.Helper()
	first, last, version := int64(1), int64(2), int64(1)
	f := &offlineReadMCPFixture{t: t, cfg: cfg}
	f.identity = client.SelfIdentity{AccountID: cfg.AccountID, RealmID: cfg.RealmID, RealmName: cfg.Realm, AgentID: cfg.AgentID, AgentName: cfg.AgentName}
	f.transcript = client.Transcript{ID: "trn_offline_read", AccountID: cfg.AccountID, RealmID: cfg.RealmID, OwnerAgentID: cfg.AgentID, ExternalID: "offline-thread", Title: "Offline source", Metadata: json.RawMessage(`{}`)}
	f.fact = client.Fact{ID: "fct_offline_read", SubjectID: "sub_offline_read", Subject: "self", Predicate: "preferences/offline", ValueType: "string", Value: json.RawMessage(`"fixture preference"`), SourceKind: "agent", SourceRef: f.transcript.ID + ":1", ResolvedAssertionID: "fas_offline_read"}
	f.memories = []client.Memory{
		{ID: "mem_offline_read", AccountID: cfg.AccountID, RealmID: cfg.RealmID, Owner: client.MemoryOwner{Kind: "agent", AgentID: cfg.AgentID}, Version: 1, Content: "Offline visible lesson", ContentEncoding: "plain", Kind: "lesson", State: "active", Tags: []string{"offline"}, Links: []string{}, Evidence: []client.MemoryEvidence{{ID: "mev_offline_transcript", MemoryID: "mem_offline_read", MemoryVersion: 1, State: "resolved", Type: "conversation", Role: "supports", TranscriptID: f.transcript.ID, EntryFromSequence: &first, EntryUntilSequence: &last}}},
		{ID: "mem_offline_private", AccountID: cfg.AccountID, RealmID: cfg.RealmID, Owner: client.MemoryOwner{Kind: "agent", AgentID: cfg.AgentID}, Version: 1, Content: "Offline private lesson", ContentEncoding: "plain", Kind: "lesson", State: "active", Sensitive: true, Tags: []string{"offline"}, Links: []string{}, Evidence: []client.MemoryEvidence{{ID: "mev_offline_memory", MemoryID: "mem_offline_private", MemoryVersion: 1, State: "resolved", Type: "memory", Role: "supports", SourceMemoryID: "mem_offline_read", SourceMemoryVersion: &version}}},
	}
	// Nonempty actor metadata makes dropped provenance observable at every wire boundary.
	for i := range f.memories {
		memory := &f.memories[i]
		memory.Owner.AgentName = cfg.AgentName
		memory.OriginalAuthor = client.MemoryActor{Kind: "agent", ID: cfg.AgentID, Name: cfg.AgentName}
		memory.Actor = memory.OriginalAuthor
		memory.Client = client.MemoryClientProvenance{Runtime: string(cfg.Runtime), RecipeVersion: "1"}
		memory.Evidence[0].Actor = memory.Actor
		memory.Evidence[0].ChangeSeq = int64(i + 1)
	}
	f.preflight = client.MemoryCurationPreflight{
		Principal:   client.MemoryCurationPreflightPrincipal{AccountID: cfg.AccountID, RealmID: cfg.RealmID, AgentID: cfg.AgentID, AgentName: cfg.AgentName},
		Credential:  client.MemoryCurationPreflightCredential{TokenID: cfg.AgentID + "-token", AccessProfile: mcpProfileFull},
		Protocol:    client.MemoryCurationPreflightProtocol{PlanSchema: "witself.memory-plan.v1", AllowedPrimitives: []string{"create", "relate"}, ClientInferenceRequired: true},
		Permissions: client.MemoryCurationPreflightPermissions{Start: true, GetInputs: true, Plan: true, GetPlan: true, Apply: true, CreateRequest: true}, MemoryCapacity: client.MemoryLimitStatus{Unlimited: true},
	}
	f.request = client.MemoryCurationRequest{ID: "curq_offline_read", AccountID: cfg.AccountID, RealmID: cfg.RealmID, OwnerKind: "agent", OwnerID: cfg.AgentID, ActorKind: "agent", ActorID: cfg.AgentID, State: "queued", RequestGeneration: 1}
	f.run = client.MemoryCurationRun{ID: "curr_offline_read", RequestID: f.request.ID, AccountID: cfg.AccountID, RealmID: cfg.RealmID, OwnerKind: "agent", OwnerID: cfg.AgentID, ActorKind: "agent", ActorID: cfg.AgentID, RequestGeneration: f.request.RequestGeneration, FencingGeneration: 7, State: "open", Budgets: json.RawMessage(`{}`)}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *offlineReadMCPFixture) encode(w http.ResponseWriter, out any) {
	if json.NewEncoder(w).Encode(out) != nil {
		f.t.Error("fixture response encoding failed")
	}
}
func (f *offlineReadMCPFixture) decode(r *http.Request, out any) bool {
	if json.NewDecoder(r.Body).Decode(out) != nil {
		f.t.Error("fixture request decoding failed")
		return false
	}
	return true
}
func (f *offlineReadMCPFixture) check(ok bool) bool {
	if !ok {
		f.t.Error("fixture request contract mismatch")
	}
	return ok
}
func (f *offlineReadMCPFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.check(r.Header.Get("Authorization") == "Bearer "+offlineReadMCPToken) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	f.events = append(f.events, r.URL.Path)
	switch {
	case r.URL.Path == "/v1/self":
		f.check(r.URL.Query().Get("observational") == "true")
		f.encode(w, client.SelfDigest{SchemaVersion: "witself.v0", Identity: f.identity, PrimaryFacts: []client.SelfFact{}, SalientMemories: []client.SelfMemory{}, Index: client.SelfIndex{Kinds: []string{}, Tags: []string{}, Counts: map[string]int{}}})
	case r.URL.Path == "/v1/transcripts" && r.Method == http.MethodPost:
		var in client.CreateTranscriptInput
		if !f.decode(r, &in) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.check(in.ExternalID == f.transcript.ExternalID && in.Title == f.transcript.Title)
		f.encode(w, map[string]any{"transcript": f.transcript})
	case r.URL.Path == "/v1/transcripts/"+f.transcript.ID+"/entries" && r.Method == http.MethodPost:
		var in client.AppendTranscriptEntryInput
		if !f.decode(r, &in) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		seq := int64(len(f.entries) + 1)
		f.check(seq <= 2)
		entry := client.TranscriptEntry{ID: "tre_offline_" + strconv.FormatInt(seq, 10), AccountID: f.cfg.AccountID, RealmID: f.cfg.RealmID, TranscriptID: f.transcript.ID, RecordedByAgentID: f.cfg.AgentID, Sequence: seq, ExternalID: in.ExternalID, Role: in.Role, Body: in.Body, ReplyToEntryID: in.ReplyToEntryID, Artifacts: json.RawMessage(`[]`)}
		f.entries = append(f.entries, entry)
		f.encode(w, map[string]any{"entry": entry})
	case r.URL.Path == "/v1/transcripts":
		f.encode(w, map[string]any{"transcripts": []client.Transcript{f.transcript}})
	case r.URL.Path == "/v1/transcripts/"+f.transcript.ID:
		f.check(r.URL.Query().Get("observational") == "true" && r.URL.Query().Get("limit") == "1")
		after, _ := strconv.Atoi(r.URL.Query().Get("after_sequence"))
		if !f.check(after >= 0 && after < len(f.entries)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		next := int64(0)
		if after == 0 {
			next = 1
		}
		f.encode(w, client.TranscriptDetail{Transcript: f.transcript, Entries: f.entries[after : after+1], NextAfterSequence: next})
	case r.URL.Path == "/v1/facts" && r.Method == http.MethodPost:
		var in client.SetFactInput
		if !f.decode(r, &in) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.check(in.Subject == f.fact.Subject && in.Predicate == f.fact.Predicate && in.ValueType == f.fact.ValueType && in.SourceKind == f.fact.SourceKind && reflect.DeepEqual(in.Value, f.fact.Value) && in.SourceRef == f.fact.SourceRef && r.Header.Get("Idempotency-Key") != "")
		f.factSets = append(f.factSets, in)
		f.encode(w, map[string]any{"fact": f.fact})
	case r.URL.Path == "/v1/facts":
		f.check(r.URL.Query().Get("observational") == "true")
		if r.URL.Query().Get("predicate") != "" {
			f.check(r.URL.Query().Get("subject") == f.fact.Subject && r.URL.Query().Get("predicate") == f.fact.Predicate)
			f.encode(w, map[string]any{"fact": f.fact})
		} else {
			f.encode(w, map[string]any{"facts": []client.Fact{f.fact}})
		}
	case r.URL.Path == "/v1/memories" && r.Method == http.MethodPost:
		var in client.CaptureMemoryInput
		if !f.decode(r, &in) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		index := len(f.captures)
		if !f.check(index < len(f.memories)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		want := f.memories[index]
		f.check(in.Content == want.Content && in.Sensitive == want.Sensitive && in.ContentEncoding == "plain" && len(in.Evidence) == 1 && r.Header.Get("Idempotency-Key") != "")
		if len(in.Evidence) == 1 {
			ev, expected := in.Evidence[0], want.Evidence[0]
			f.check(ev.State == expected.State && ev.Type == expected.Type && ev.Role == expected.Role && ev.TranscriptID == expected.TranscriptID && reflect.DeepEqual(ev.EntryFromSequence, expected.EntryFromSequence) && reflect.DeepEqual(ev.EntryUntilSequence, expected.EntryUntilSequence) && ev.SourceMemoryID == expected.SourceMemoryID && reflect.DeepEqual(ev.SourceMemoryVersion, expected.SourceMemoryVersion))
		}
		f.captures = append(f.captures, in)
		f.encode(w, client.MemoryMutationResult{Memory: want, Receipt: client.MemoryMutationReceipt{MemoryID: want.ID, Version: want.Version, Actor: want.Actor}})
	case r.URL.Path == "/v1/memories":
		cursor := r.URL.Query().Get("cursor")
		if !f.check(cursor == "" || cursor == "memory-page-two") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		index, next := 0, "memory-page-two"
		if cursor != "" {
			index, next = 1, ""
		}
		f.check(r.URL.Query().Get("limit") == "1" && r.URL.Query().Get("include_sensitive") == "true")
		f.encode(w, client.MemoryPage{Items: []client.Memory{f.memories[index]}, NextCursor: next})
	case r.URL.Path == "/v1/memories:recall":
		var in client.MemoryRecallInput
		if !f.decode(r, &in) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.recalls = append(f.recalls, in)
		f.check(in.Query == "offline" && in.Limit == 1 && (in.Cursor == "" || in.Cursor == "recall-page-two"))
		index, next := 0, "recall-page-two"
		if in.Cursor != "" {
			index, next = 1, ""
		}
		// This fixture records projection intent; the API companion tests the actual redactor.
		f.encode(w, client.MemoryRecallPage{Hits: []client.MemoryRecallHit{{Memory: f.memories[index]}}, NextCursor: next, RetrievalMode: "lexical"})
	case strings.HasPrefix(r.URL.Path, "/v1/memories/"):
		for _, memory := range f.memories {
			if r.URL.Path == "/v1/memories/"+memory.ID {
				f.encode(w, map[string]any{"memory": memory})
				return
			}
			if r.URL.Path == "/v1/memories/"+memory.ID+"/history" {
				f.check(r.URL.Query().Get("limit") == "1" && r.URL.Query().Get("cursor") == "history-exact")
				f.encode(w, client.MemoryHistoryPage{Versions: []client.MemoryVersion{{MemoryID: memory.ID, Version: memory.Version, Content: memory.Content, ContentEncoding: memory.ContentEncoding, Actor: memory.Actor, Client: memory.Client, Evidence: memory.Evidence}}})
				return
			}
		}
		f.t.Error("unexpected memory fixture route")
		w.WriteHeader(http.StatusNotFound)
	case r.URL.Path == "/v1/memory-curation-preflight":
		f.encode(w, f.preflight)
	case strings.HasPrefix(r.URL.Path, "/v1/memory-curation-"):
		f.serveCuration(w, r)
	default:
		f.t.Error("unexpected fixture route")
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *offlineReadMCPFixture) eventCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}
func (f *offlineReadMCPFixture) ordinary(t *testing.T, session *offlineReadMCPSession, name string, args any, out any) {
	t.Helper()
	before := f.eventCount()
	offlineReadMCPCall(t, session, name, args, out)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) != before+2 || f.events[before] != "/v1/self" || f.events[before+1] == "/v1/self" {
		t.Fatal("ordinary operation lacked one live identity check before data")
	}
}

func offlineReadMCPRoute(t *testing.T, cfg transcriptcapture.Config, endpoint string) {
	t.Helper()
	if local.SetRoute(cfg.Account, &local.Route{Kind: "local", Endpoint: endpoint}) != nil {
		t.Fatal("fixture route activation failed")
	}
}

func (f *offlineReadMCPFixture) seedTranscript(t *testing.T) {
	t.Helper()
	ctx := offlineReadMCPContext(t)
	tr, err := client.CreateTranscript(ctx, f.server.URL, offlineReadMCPToken, client.CreateTranscriptInput{ExternalID: f.transcript.ExternalID, Title: f.transcript.Title})
	if err != nil || !reflect.DeepEqual(tr, f.transcript) {
		t.Fatal("transcript fixture creation failed")
	}
	for i, body := range []string{"Offline user entry", "Offline assistant entry"} {
		role := "user"
		if i == 1 {
			role = "assistant"
		}
		replyTo := ""
		if i > 0 {
			replyTo = f.entries[i-1].ID
		}
		entry, err := client.AppendTranscriptEntry(ctx, f.server.URL, offlineReadMCPToken, tr.ID, client.AppendTranscriptEntryInput{ExternalID: tr.ExternalID + strconv.Itoa(i), Role: role, Body: body, ReplyToEntryID: replyTo})
		if err != nil || !reflect.DeepEqual(entry, f.entries[i]) {
			t.Fatal("transcript fixture append provenance changed")
		}
	}
}

func (f *offlineReadMCPFixture) seedTools(t *testing.T, s *offlineReadMCPSession) {
	t.Helper()
	var fact mcpFactOutput
	f.ordinary(t, s, "witself.fact.set", map[string]any{"subject": f.fact.Subject, "predicate": f.fact.Predicate, "value": "fixture preference", "value_type": "string", "source_ref": f.fact.SourceRef, "idempotency_key": "offline-fact-set"}, &fact)
	if !reflect.DeepEqual(fact.Fact, f.expectedFact(t)) {
		t.Fatal("fact set lost provenance")
	}
	for i, memory := range f.memories {
		evidence := memory.Evidence[0]
		ev := map[string]any{"state": evidence.State, "type": evidence.Type, "role": evidence.Role}
		if evidence.TranscriptID != "" {
			ev["transcript_id"] = evidence.TranscriptID
			ev["entry_from_sequence"] = *evidence.EntryFromSequence
			ev["entry_until_sequence"] = *evidence.EntryUntilSequence
		}
		if evidence.SourceMemoryID != "" {
			ev["source_memory_id"] = evidence.SourceMemoryID
			ev["source_memory_version"] = *evidence.SourceMemoryVersion
		}
		var out client.MemoryMutationResult
		f.ordinary(t, s, "witself.memory.capture", map[string]any{"content": memory.Content, "kind": memory.Kind, "sensitive": memory.Sensitive, "capture_reason": "explicit_remember", "idempotency_key": "offline-capture-" + strconv.Itoa(i), "evidence": []any{ev}}, &out)
		if !reflect.DeepEqual(out.Memory, memory) || out.Receipt.MemoryID != memory.ID || out.Receipt.Version != memory.Version || out.Receipt.Actor != memory.Actor {
			t.Fatal("memory capture lost exact evidence")
		}
	}
}

// These expected projections are test-authored and never call production converters.
func (f *offlineReadMCPFixture) expectedFact(t *testing.T) mcpFact {
	t.Helper()
	var value any
	if json.Unmarshal(f.fact.Value, &value) != nil {
		t.Fatal("expected fact value decoding failed")
	}
	return mcpFact{ID: f.fact.ID, SubjectID: f.fact.SubjectID, Subject: f.fact.Subject, Predicate: f.fact.Predicate, ValueType: f.fact.ValueType, Value: value, SourceKind: f.fact.SourceKind, SourceRef: f.fact.SourceRef, ResolvedAssertionID: f.fact.ResolvedAssertionID}
}

func (f *offlineReadMCPFixture) expectedTranscript() mcpTranscript {
	return mcpTranscript{ID: f.transcript.ID, AccountID: f.transcript.AccountID, RealmID: f.transcript.RealmID, OwnerAgentID: f.transcript.OwnerAgentID, ExternalID: f.transcript.ExternalID, Title: f.transcript.Title, Metadata: map[string]any{}}
}

func (f *offlineReadMCPFixture) readCorpus(t *testing.T, s *offlineReadMCPSession) {
	t.Helper()
	var transcripts mcpTranscriptListOutput
	f.ordinary(t, s, "witself.transcript.list", map[string]any{"limit": 1}, &transcripts)
	if len(transcripts.Transcripts) != 1 || !reflect.DeepEqual(transcripts.Transcripts[0], f.expectedTranscript()) {
		t.Fatal("transcript inventory changed")
	}
	after := int64(0)
	for i := 0; i < 2; i++ {
		var page mcpTranscriptReadOutput
		f.ordinary(t, s, "witself.transcript.get", map[string]any{"transcript_id": f.transcript.ID, "after_sequence": after, "limit": 1}, &page)
		if !reflect.DeepEqual(page.Transcript, f.expectedTranscript()) || len(page.Entries) != 1 {
			t.Fatal("transcript page lost enclosing provenance")
		}
		entry, want := page.Entries[0], f.entries[i]
		if entry.ID != want.ID || entry.TranscriptID != want.TranscriptID || entry.AccountID != want.AccountID || entry.RealmID != want.RealmID || entry.RecordedByAgentID != want.RecordedByAgentID || entry.Sequence != want.Sequence || entry.Body != want.Body || entry.ExternalID != want.ExternalID || entry.ReplyToEntryID != want.ReplyToEntryID || entry.Role != want.Role {
			t.Fatal("transcript page lost entry provenance, sequence or content")
		}
		if i == 0 && page.NextAfterSequence != 1 || i == 1 && page.NextAfterSequence != 0 {
			t.Fatal("transcript page cursor changed")
		}
		after = page.NextAfterSequence
	}
	var fact mcpFactOutput
	f.ordinary(t, s, "witself.fact.get", map[string]any{"subject": f.fact.Subject, "predicate": f.fact.Predicate}, &fact)
	if !reflect.DeepEqual(fact.Fact, f.expectedFact(t)) {
		t.Fatal("fact exact read lost source or value")
	}
	var facts mcpFactListOutput
	f.ordinary(t, s, "witself.fact.list", map[string]any{"limit": 1}, &facts)
	if len(facts.Facts) != 1 || !reflect.DeepEqual(facts.Facts[0], f.expectedFact(t)) {
		t.Fatal("fact list changed exact fact")
	}
	cursor := ""
	for _, memory := range f.memories {
		var read mcpMemoryOutput
		f.ordinary(t, s, "witself.memory.read", map[string]any{"memory_id": memory.ID}, &read)
		if !reflect.DeepEqual(read.Memory, memory) {
			t.Fatal("memory exact read lost provenance")
		}
		var list client.MemoryPage
		f.ordinary(t, s, "witself.memory.list", map[string]any{"limit": 1, "cursor": cursor, "include_sensitive": true}, &list)
		if len(list.Items) != 1 || !reflect.DeepEqual(list.Items[0], memory) {
			t.Fatal("memory list lost provenance")
		}
		if cursor == "" && list.NextCursor != "memory-page-two" || cursor != "" && list.NextCursor != "" {
			t.Fatal("memory list cursor changed")
		}
		cursor = list.NextCursor
		var history client.MemoryHistoryPage
		f.ordinary(t, s, "witself.memory.history", map[string]any{"memory_id": memory.ID, "limit": 1, "cursor": "history-exact"}, &history)
		expectedVersion := client.MemoryVersion{MemoryID: memory.ID, Version: memory.Version, Content: memory.Content, ContentEncoding: memory.ContentEncoding, Actor: memory.Actor, Client: memory.Client, Evidence: memory.Evidence}
		if len(history.Versions) != 1 || !reflect.DeepEqual(history.Versions[0], expectedVersion) {
			t.Fatal("memory history lost exact version evidence")
		}
	}
	cursor = ""
	for i, memory := range f.memories {
		var recall client.MemoryRecallPage
		f.ordinary(t, s, "witself.memory.recall", map[string]any{"query": "offline", "limit": 1, "cursor": cursor, "include_sensitive": true}, &recall)
		if len(recall.Hits) != 1 || !reflect.DeepEqual(recall.Hits[0].Memory, memory) {
			t.Fatal("recall lost exact corpus reference")
		}
		if i == 0 && recall.NextCursor != "recall-page-two" || i == 1 && recall.NextCursor != "" {
			t.Fatal("recall cursor changed")
		}
		cursor = recall.NextCursor
	}
}

func TestOfflineReadMCPComposition(t *testing.T) {
	cfg := offlineReadMCPConfig(t)
	f := offlineReadMCPFixtureNew(t, cfg)
	offlineReadMCPIsolation(t, f.server.URL)
	offlineReadMCPRoute(t, cfg, f.server.URL)
	f.seedTranscript(t)
	s := offlineReadMCPOpen(t, cfg, mcpProfileFull)
	tools, err := s.client.ListTools(offlineReadMCPContext(t), nil)
	if err != nil {
		t.Fatal("MCP registered tool inventory failed")
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	for _, name := range []string{"witself.transcript.list", "witself.transcript.get", "witself.fact.set", "witself.fact.get", "witself.fact.list", "witself.memory.capture", "witself.memory.read", "witself.memory.list", "witself.memory.recall", "witself.memory.history"} {
		if !names[name] {
			t.Fatal("required registered read tool missing")
		}
	}
	f.seedTools(t, s)
	f.readCorpus(t, s)
	t.Run("explicit-redacted-recall", func(t *testing.T) {
		f.ordinary(t, s, "witself.memory.recall", map[string]any{"query": "offline", "limit": 1, "include_sensitive": false}, nil)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.recalls[len(f.recalls)-1].IncludeSensitive {
			t.Fatal("explicit false recall became sensitive HTTP request")
		}
	})
	t.Run("omitted-recall-default", func(t *testing.T) {
		f.ordinary(t, s, "witself.memory.recall", map[string]any{"query": "offline", "limit": 1}, nil)
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.recalls[len(f.recalls)-1].IncludeSensitive {
			t.Fatal("omitted recall no longer preserves MCP sensitive default")
		}
	})
}

func TestOfflineReadMCPIdentityAndReopen(t *testing.T) {
	cfg := offlineReadMCPConfig(t)
	first, second := offlineReadMCPFixtureNew(t, cfg), offlineReadMCPFixtureNew(t, cfg)
	offlineReadMCPIsolation(t, first.server.URL, second.server.URL)
	offlineReadMCPRoute(t, cfg, first.server.URL)
	first.seedTranscript(t)
	second.seedTranscript(t)
	running := offlineReadMCPOpen(t, cfg, mcpProfileFull)
	first.seedTools(t, running)
	first.readCorpus(t, running)
	for _, field := range []string{"account-id", "realm-id", "agent-id"} {
		t.Run(field, func(t *testing.T) {
			first.mu.Lock()
			saved := first.identity
			switch field {
			case "account-id":
				first.identity.AccountID = "acc_other"
			case "realm-id":
				first.identity.RealmID = "rlm_other"
			case "agent-id":
				first.identity.AgentID = "agt_other"
			}
			before := len(first.events)
			first.mu.Unlock()
			offlineReadMCPRefusal(t, running, "witself.memory.read", map[string]any{"memory_id": first.memories[0].ID})
			first.mu.Lock()
			defer first.mu.Unlock()
			first.identity = saved
			if len(first.events) != before+1 || first.events[before] != "/v1/self" {
				t.Fatal("identity mismatch reached a data request")
			}
		})
	}
	// Retained HTTP fixture state survives reconstructed clients; no process or disk recovery is claimed.
	running.close()
	saved, err := transcriptcapture.LoadConfig(cfg.Runtime)
	if err != nil {
		t.Fatal("persisted binding reconstruction failed")
	}
	running = offlineReadMCPOpen(t, saved, mcpProfileFull)
	first.readCorpus(t, running)
	t.Run("route-snapshot", func(t *testing.T) {
		offlineReadMCPRoute(t, cfg, second.server.URL)
		beforeFirst, beforeSecond := first.eventCount(), second.eventCount()
		// Avoid the ordinary helper here so R1 raises at the snapshot-specific assertion.
		offlineReadMCPCall(t, running, "witself.transcript.list", map[string]any{"limit": 1}, nil)
		if first.eventCount() != beforeFirst+2 || second.eventCount() != beforeSecond {
			t.Fatal("existing ordinary session did not retain its first route")
		}
		first.readCorpus(t, running)
		reopened := offlineReadMCPOpen(t, saved, mcpProfileFull)
		beforeReopenedFirst := first.eventCount()
		second.readCorpus(t, reopened)
		if first.eventCount() != beforeReopenedFirst {
			t.Fatal("reconstructed session mixed endpoint origins")
		}
		second.server.Close()
		before := second.eventCount()
		offlineReadMCPRefusal(t, reopened, "witself.transcript.list", map[string]any{"limit": 1})
		if second.eventCount() != before {
			t.Fatal("unavailable local endpoint unexpectedly accepted a data request")
		}
	})
	first.mu.Lock()
	defer first.mu.Unlock()
	if len(first.factSets) != 1 || len(first.captures) != 2 {
		t.Fatal("reconstruction repeated a durable fixture write")
	}
}

// serveCuration retains a small protocol transcript, not a store/lease engine.
// These checks prove the client-authored sequence and transport projection only.
func (f *offlineReadMCPFixture) serveCuration(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/memory-curation-requests":
		var in client.RequestMemoryCurationInput
		if !f.decode(r, &in) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.check(r.Method == http.MethodPost && reflect.DeepEqual(in.Scope.Sources, []string{"memory", "transcript"}) && r.Header.Get("Idempotency-Key") != "")
		f.requests++
		f.encode(w, client.RequestMemoryCurationResult{Request: f.request, Receipt: f.curationMutationReceipt("request")})
	case "/v1/memory-curation-requests/" + f.request.ID + "/start":
		var in struct {
			LeaseSeconds int64 `json:"lease_seconds"`
		}
		if !f.decode(r, &in) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.check(f.starts == 0 && in.LeaseSeconds == 300 && r.Header.Get("Idempotency-Key") != "")
		f.starts++
		f.encode(w, client.StartMemoryCurationResult{Run: f.run, Request: f.request, Receipt: f.curationMutationReceipt("start"), FirstInputCursor: "input-first"})
	case "/v1/memory-curation-runs/" + f.run.ID + "/inputs":
		f.inputCalls++
		if f.conflict != "" {
			w.WriteHeader(http.StatusConflict)
			if f.conflict == "run_not_open" {
				f.encode(w, map[string]any{"error": "memory curation state conflict", "code": "memory_curation_conflict", "reason": f.conflict})
			} else {
				f.encode(w, map[string]any{"error": f.conflict})
			}
			return
		}
		cursor := r.URL.Query().Get("cursor")
		f.check(r.URL.Query().Get("fencing_generation") == strconv.FormatInt(f.run.FencingGeneration, 10) && r.URL.Query().Get("limit") == "1")
		var input client.MemoryCurationRunInput
		next := ""
		if cursor == "input-first" {
			f.check(len(f.cursors) == 0)
			input = client.MemoryCurationRunInput{RunID: f.run.ID, Ordinal: 1, Kind: "transcript", TranscriptID: f.transcript.ID, SequenceFrom: 1, SequenceUntil: 2, TranscriptEntries: f.entries}
			next = "input-second"
		} else {
			f.check(cursor == "input-second" && reflect.DeepEqual(f.cursors, []string{"input-first"}))
			input = client.MemoryCurationRunInput{RunID: f.run.ID, Ordinal: 2, Kind: "memory", MemoryID: f.memories[0].ID, MemoryVersion: f.memories[0].Version, Memory: &f.memories[0]}
		}
		f.cursors = append(f.cursors, cursor)
		f.encode(w, client.MemoryCurationRunInputPage{Run: f.run, Inputs: []client.MemoryCurationRunInput{input}, NextCursor: next})
	case "/v1/memory-curation-runs/" + f.run.ID + "/plan":
		if r.Method == http.MethodGet {
			f.check(f.plans == 1 && r.URL.Query().Get("fencing_generation") == strconv.FormatInt(f.run.FencingGeneration, 10))
			f.planGets++
			f.encode(w, client.GetMemoryCurationPlanResult{Run: f.run, Plan: f.plan, Preview: f.preview, PreallocatedMemoryIDs: f.allocations()})
			return
		}
		var in client.PlanMemoryCurationInput
		if !f.decode(r, &in) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.check(reflect.DeepEqual(f.cursors, []string{"input-first", "input-second"}) && in.FencingGeneration == f.run.FencingGeneration && f.applies == 0 && r.Header.Get("Idempotency-Key") != "")
		var draft mcpMemoryCurationPlanDraft
		if json.Unmarshal(in.Draft, &draft) != nil {
			f.t.Error("curation draft decoding failed")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.check(draft.Schema == "witself.memory-plan.v1" && draft.DraftRevision == 1 && len(draft.Actions) <= 1)
		if len(draft.Actions) == 1 {
			action := draft.Actions[0]
			f.check(action.Ordinal == 1 && action.Operation == "create" && action.Create != nil)
			if action.Create != nil {
				ev := action.Create.Snapshot.Evidence
				f.check(action.Create.Snapshot.Content == "Offline reviewed synthesis" && len(ev) == 2)
				if len(ev) == 2 {
					f.check(ev[0].SourceTranscriptID == f.transcript.ID && ev[0].SourceSequenceFrom == 1 && ev[0].SourceSequenceUntil == 2 && ev[1].SourceMemory != nil)
					if ev[1].SourceMemory != nil {
						f.check(ev[1].SourceMemory.MemoryID == f.memories[0].ID && ev[1].SourceMemory.Version == f.memories[0].Version)
					}
				}
			}
		}
		// Production acceptance uses plan_revision and allocates create memory IDs;
		// retain that normalized wire shape, not a copy of the client draft.
		var normalized store.MemoryCurationPlan
		if json.Unmarshal(in.Draft, &normalized) != nil {
			f.t.Error("normalized fixture decoding failed")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		normalized.PlanRevision = 3
		if len(normalized.Actions) == 1 {
			normalized.Actions[0].Create.MemoryID = "mem_offline_curated"
		}
		var err error
		f.plan, err = json.Marshal(normalized)
		if err != nil {
			f.t.Error("normalized fixture encoding failed")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.preview = client.MemoryCurationImpactPreview{ActionCount: len(draft.Actions), CreateActions: len(draft.Actions), NewMemories: len(draft.Actions), MemoryVersionWrites: len(draft.Actions), EvidenceRows: 2 * len(draft.Actions), ActiveMemoryDelta: int64(len(draft.Actions)), ProjectedActiveMemories: 2 + int64(len(draft.Actions))}
		f.run.State = "planned"
		f.run.PlanRevision = normalized.PlanRevision
		digest := sha256.Sum256(f.plan)
		f.run.PlanHash = hex.EncodeToString(digest[:])
		f.plans++
		f.encode(w, client.PlanMemoryCurationResult{Run: f.run, Plan: f.plan, Preview: f.preview, PreallocatedMemoryIDs: f.allocations(), Receipt: f.curationPlanReceipt()})
	case "/v1/memory-curation-runs/" + f.run.ID + "/apply":
		var in client.ApplyMemoryCurationInput
		if !f.decode(r, &in) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.check(f.planGets == 1 && f.plans == 1 && reflect.DeepEqual(f.cursors, []string{"input-first", "input-second"}) && f.applies == 0 && in.FencingGeneration == f.run.FencingGeneration && in.PlanRevision == f.run.PlanRevision && in.PlanHash == f.run.PlanHash && r.Header.Get("Idempotency-Key") != "")
		f.applies++
		f.run.State = "applied"
		f.encode(w, client.ApplyMemoryCurationResult{Run: f.run, Request: f.request, Receipt: f.curationApplyReceipt()})
	default:
		f.t.Error("unexpected curation fixture route")
		w.WriteHeader(http.StatusNotFound)
	}
}

// Receipt expectations originate in the fixture, independently of MCP projections.
func (f *offlineReadMCPFixture) curationMutationReceipt(operation string) client.MemoryCurationMutationReceipt {
	receipt := client.MemoryCurationMutationReceipt{
		Operation: operation, ActorID: f.cfg.AgentID, IdempotencyKey: f.request.ID + "-" + operation,
		RequestHash: f.request.ID + "-hash", RequestID: f.request.ID,
		RequestGeneration: f.request.RequestGeneration, ResultState: f.request.State,
		ReceiptID: f.request.ID + "-receipt-" + operation,
	}
	if operation == "start" {
		receipt.RunID = f.run.ID
		receipt.FencingGeneration = f.run.FencingGeneration
		receipt.ResultState = f.run.State
	}
	return receipt
}

func (f *offlineReadMCPFixture) curationPlanReceipt() client.MemoryCurationPlanReceipt {
	return client.MemoryCurationPlanReceipt{
		ID: f.run.ID + "-plan-receipt", Operation: "plan", ActorID: f.cfg.AgentID,
		IdempotencyKey: f.run.ID + "-plan", RequestHash: f.request.ID + "-plan-hash",
		RequestID: f.request.ID, RunID: f.run.ID, RequestGeneration: f.request.RequestGeneration,
		FencingGeneration: f.run.FencingGeneration, PlanSchema: "witself.memory-plan.v1",
		PlanRevision: f.run.PlanRevision, PlanHash: f.run.PlanHash, ResultState: f.run.State,
	}
}

func (f *offlineReadMCPFixture) curationApplyReceipt() client.MemoryCurationApplyReceipt {
	receipt := client.MemoryCurationApplyReceipt{
		ID: f.run.ID + "-apply-receipt", Operation: "apply", ActorID: f.cfg.AgentID,
		IdempotencyKey: f.run.ID + "-apply", RequestHash: f.request.ID + "-apply-hash",
		RequestID: f.request.ID, RunID: f.run.ID, RequestGeneration: f.request.RequestGeneration,
		FencingGeneration: f.run.FencingGeneration, PlanRevision: f.run.PlanRevision, PlanHash: f.run.PlanHash,
	}
	if f.preview.CreateActions != 0 {
		receipt.ActionResults = []client.MemoryCurationActionApplyResult{{
			ActionID: f.run.ID + "-action", Ordinal: 1, Operation: "create",
			AfterHeads:       []client.MemoryVersionReference{{MemoryID: f.allocations()[0].MemoryID, Version: 1}},
			CreatedMemoryIDs: []string{f.allocations()[0].MemoryID},
			EvidenceIDs:      []string{f.run.ID + "-transcript-evidence", f.run.ID + "-memory-evidence"},
		}}
	}
	return receipt
}

func (f *offlineReadMCPFixture) allocations() []client.MemoryCurationPreallocatedMemoryID {
	if f.preview.CreateActions == 0 {
		return nil
	}
	return []client.MemoryCurationPreallocatedMemoryID{{LocalRef: "reviewed-offline", MemoryID: "mem_offline_curated"}}
}

func offlineReadMCPDraft(f *offlineReadMCPFixture, empty bool) mcpMemoryCurationPlanDraft {
	draft := mcpMemoryCurationPlanDraft{Schema: "witself.memory-plan.v1", DraftRevision: 1, Actions: []mcpMemoryCurationPlanAction{}}
	if !empty {
		draft.Actions = append(draft.Actions, mcpMemoryCurationPlanAction{Ordinal: 1, Operation: "create", Create: &mcpMemoryCurationCreateAction{LocalRef: "reviewed-offline", Snapshot: mcpMemoryCurationCreateSnapshot{Content: "Offline reviewed synthesis", ContentEncoding: "plain", Kind: "lesson", Evidence: []mcpMemoryCurationEvidence{
			{Type: "conversation", Role: "supports", ResolutionState: "resolved", ResolvedKind: "transcript", SourceTranscriptID: f.transcript.ID, SourceSequenceFrom: 1, SourceSequenceUntil: 2},
			{Type: "memory", Role: "context", ResolutionState: "resolved", ResolvedKind: "memory", SourceMemory: &mcpMemoryCurationVersionReference{MemoryID: f.memories[0].ID, Version: f.memories[0].Version}},
		}}}})
	}
	return draft
}

func offlineReadMCPCurationStep(t *testing.T, f *offlineReadMCPFixture, s *offlineReadMCPSession, name string, args any, out any) {
	t.Helper()
	before := f.eventCount()
	offlineReadMCPCall(t, s, name, args, out)
	f.mu.Lock()
	defer f.mu.Unlock()
	expected := 2
	if name == "witself.memory.curation.preflight" {
		expected = 1
	}
	if len(f.events) != before+expected || f.events[before] != "/v1/memory-curation-preflight" {
		t.Fatal("curation operation lacked live preflight before protocol access")
	}
}

func TestOfflineReadMCPCurationProtocol(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "reversible-plan"
		if empty {
			name = "empty-plan"
		}
		t.Run(name, func(t *testing.T) {
			cfg := offlineReadMCPConfig(t)
			f := offlineReadMCPFixtureNew(t, cfg)
			offlineReadMCPIsolation(t, f.server.URL)
			offlineReadMCPRoute(t, cfg, f.server.URL)
			f.seedTranscript(t)
			s := offlineReadMCPOpen(t, cfg, mcpProfileFull)
			var preflight client.MemoryCurationPreflight
			offlineReadMCPCurationStep(t, f, s, "witself.memory.curation.preflight", map[string]any{}, &preflight)
			if !reflect.DeepEqual(preflight, f.preflight) {
				t.Fatal("curation preflight identity or inference contract changed")
			}
			var request client.RequestMemoryCurationResult
			offlineReadMCPCurationStep(t, f, s, "witself.memory.curation.request", map[string]any{"sources": []string{"memory", "transcript"}, "idempotency_key": "offline-request"}, &request)
			if !reflect.DeepEqual(request.Request, f.request) || !reflect.DeepEqual(request.Receipt, f.curationMutationReceipt("request")) {
				t.Fatal("curation request or receipt provenance changed")
			}
			var start client.StartMemoryCurationResult
			offlineReadMCPCurationStep(t, f, s, "witself.memory.curation.start", map[string]any{"request_id": request.Request.ID, "lease_seconds": 300, "idempotency_key": "offline-start"}, &start)
			if !reflect.DeepEqual(start.Run, f.run) || !reflect.DeepEqual(start.Request, f.request) || !reflect.DeepEqual(start.Receipt, f.curationMutationReceipt("start")) || start.FirstInputCursor != "input-first" {
				t.Fatal("curation start fence or input cursor changed")
			}
			cursor := start.FirstInputCursor
			var inputs []client.MemoryCurationRunInput
			for i := 0; i < 2; i++ {
				var page client.MemoryCurationRunInputPage
				offlineReadMCPCurationStep(t, f, s, "witself.memory.curation.get", map[string]any{"run_id": start.Run.ID, "fencing_generation": start.Run.FencingGeneration, "cursor": cursor, "limit": 1}, &page)
				if !reflect.DeepEqual(page.Run, f.run) || len(page.Inputs) != 1 {
					t.Fatal("curation input page scope changed")
				}
				if i == 0 && page.NextCursor != "input-second" || i == 1 && page.NextCursor != "" {
					t.Fatal("curation input cursor order changed")
				}
				inputs = append(inputs, page.Inputs...)
				cursor = page.NextCursor
			}
			expectedInputs := []client.MemoryCurationRunInput{
				{RunID: f.run.ID, Ordinal: 1, Kind: "transcript", TranscriptID: f.transcript.ID, SequenceFrom: 1, SequenceUntil: 2, TranscriptEntries: f.entries},
				{RunID: f.run.ID, Ordinal: 2, Kind: "memory", MemoryID: f.memories[0].ID, MemoryVersion: f.memories[0].Version, Memory: &f.memories[0]},
			}
			if cursor != "" || !reflect.DeepEqual(inputs, expectedInputs) {
				t.Fatal("curation client review lost frozen input provenance")
			}
			draft := offlineReadMCPDraft(f, empty)
			var accepted client.PlanMemoryCurationResult
			offlineReadMCPCurationStep(t, f, s, "witself.memory.curation.plan", map[string]any{"run_id": start.Run.ID, "fencing_generation": start.Run.FencingGeneration, "draft": draft, "idempotency_key": "offline-plan"}, &accepted)
			var review client.GetMemoryCurationPlanResult
			offlineReadMCPCurationStep(t, f, s, "witself.memory.curation.plan.get", map[string]any{"run_id": start.Run.ID, "fencing_generation": start.Run.FencingGeneration}, &review)
			var acceptedPlan, reviewedPlan, expectedPlan store.MemoryCurationPlan
			var reviewedShape map[string]json.RawMessage
			if json.Unmarshal(accepted.Plan, &acceptedPlan) != nil || json.Unmarshal(review.Plan, &reviewedPlan) != nil || json.Unmarshal(f.plan, &expectedPlan) != nil || json.Unmarshal(review.Plan, &reviewedShape) != nil {
				t.Fatal("normalized curation plan could not be reviewed")
			}
			if _, exists := reviewedShape["draft_revision"]; exists {
				t.Fatal("accepted curation plan retained a draft revision")
			}
			if reviewedPlan.Schema != draft.Schema || reviewedPlan.PlanRevision != 3 || len(reviewedPlan.Actions) != len(draft.Actions) || !reflect.DeepEqual(acceptedPlan, expectedPlan) || !reflect.DeepEqual(reviewedPlan, expectedPlan) || !reflect.DeepEqual(accepted.Run, f.run) || !reflect.DeepEqual(review.Run, f.run) || !reflect.DeepEqual(accepted.Receipt, f.curationPlanReceipt()) {
				t.Fatal("accepted normalized curation plan failed independent review")
			}
			expectedPreview := client.MemoryCurationImpactPreview{ActionCount: len(draft.Actions), CreateActions: len(draft.Actions), NewMemories: len(draft.Actions), MemoryVersionWrites: len(draft.Actions), EvidenceRows: 2 * len(draft.Actions), ActiveMemoryDelta: int64(len(draft.Actions)), ProjectedActiveMemories: 2 + int64(len(draft.Actions))}
			if !reflect.DeepEqual(review.Preview, expectedPreview) || !reflect.DeepEqual(accepted.Preview, expectedPreview) || !reflect.DeepEqual(review.PreallocatedMemoryIDs, f.allocations()) || !reflect.DeepEqual(accepted.PreallocatedMemoryIDs, f.allocations()) {
				t.Fatal("normalized plan impact or allocation changed before apply")
			}
			if empty {
				if len(review.PreallocatedMemoryIDs) != 0 {
					t.Fatal("empty plan allocated a memory")
				}
			} else {
				action := reviewedPlan.Actions[0]
				if action.Ordinal != 1 || action.Operation != "create" || action.Create == nil || action.Replace != nil || action.Supersede != nil || action.Relate != nil || action.ProposeFact != nil {
					t.Fatal("normalized plan changed reversible action")
				}
				created := action.Create
				if created.LocalRef != draft.Actions[0].Create.LocalRef || created.MemoryID != "mem_offline_curated" || len(review.PreallocatedMemoryIDs) != 1 || review.PreallocatedMemoryIDs[0].LocalRef != created.LocalRef || review.PreallocatedMemoryIDs[0].MemoryID != created.MemoryID || len(created.Relations) != 0 {
					t.Fatal("normalized plan allocation failed review")
				}
				expectedSnapshot := store.MemoryCurationMemorySnapshot{Content: draft.Actions[0].Create.Snapshot.Content, ContentEncoding: "plain", Kind: "lesson", Evidence: []store.MemoryCurationEvidence{
					{Type: "conversation", Role: "supports", ResolutionState: "resolved", ResolvedKind: "transcript", SourceTranscriptID: inputs[0].TranscriptID, SourceSequenceFrom: inputs[0].SequenceFrom, SourceSequenceUntil: inputs[0].SequenceUntil},
					{Type: "memory", Role: "context", ResolutionState: "resolved", ResolvedKind: "memory", SourceMemory: &store.MemoryCurationVersionReference{MemoryID: inputs[1].MemoryID, Version: inputs[1].MemoryVersion}},
				}}
				if !reflect.DeepEqual(created.Snapshot, expectedSnapshot) {
					t.Fatal("normalized plan evidence or snapshot failed input review")
				}
			}
			var applied client.ApplyMemoryCurationResult
			offlineReadMCPCurationStep(t, f, s, "witself.memory.curation.apply", map[string]any{"run_id": review.Run.ID, "fencing_generation": review.Run.FencingGeneration, "plan_revision": review.Run.PlanRevision, "plan_hash": review.Run.PlanHash, "idempotency_key": "offline-apply"}, &applied)
			if applied.Run.State != "applied" || !reflect.DeepEqual(applied.Run, f.run) || !reflect.DeepEqual(applied.Request, f.request) || applied.FollowUpRequest != nil || !reflect.DeepEqual(applied.Receipt, f.curationApplyReceipt()) {
				t.Fatal("curation apply lost reviewed guards")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.requests != 1 || f.starts != 1 || f.inputCalls != 2 || f.plans != 1 || f.planGets != 1 || f.applies != 1 {
				t.Fatal("curation driver did unexpected protocol work")
			}
		})
	}
	for _, mismatch := range []string{"wrong-agent", "wrong-account", "wrong-realm", "wrong-profile"} {
		t.Run(mismatch, func(t *testing.T) {
			cfg := offlineReadMCPConfig(t)
			f := offlineReadMCPFixtureNew(t, cfg)
			offlineReadMCPIsolation(t, f.server.URL)
			offlineReadMCPRoute(t, cfg, f.server.URL)
			f.mu.Lock()
			switch mismatch {
			case "wrong-agent":
				f.preflight.Principal.AgentID = "agt_other"
			case "wrong-account":
				f.preflight.Principal.AccountID = "acc_other"
			case "wrong-realm":
				f.preflight.Principal.RealmID = "rlm_other"
			case "wrong-profile":
				f.preflight.Credential.AccessProfile = mcpProfileCuratorPreview
			}
			f.mu.Unlock()
			s := offlineReadMCPOpen(t, cfg, mcpProfileFull)
			// Valid input and profile except for the one changed field make the production guard reachable.
			offlineReadMCPRefusal(t, s, "witself.memory.curation.get", map[string]any{"run_id": f.run.ID, "fencing_generation": f.run.FencingGeneration, "cursor": "input-first", "limit": 1})
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.inputCalls != 0 || f.starts != 0 || f.plans != 0 || f.applies != 0 || len(f.events) != 1 {
				t.Fatal("preflight mismatch reached curation protocol requests")
			}
		})
	}
	for _, conflict := range []struct{ name, message string }{
		{"stale-fence", "memory curation fence mismatch"},
		{"lease-expired", "memory curation lease expired"},
		{"typed-conflict", "run_not_open"},
	} {
		t.Run(conflict.name, func(t *testing.T) {
			cfg := offlineReadMCPConfig(t)
			f := offlineReadMCPFixtureNew(t, cfg)
			offlineReadMCPIsolation(t, f.server.URL)
			offlineReadMCPRoute(t, cfg, f.server.URL)
			f.conflict = conflict.message
			s := offlineReadMCPOpen(t, cfg, mcpProfileFull)
			result := offlineReadMCPRefusal(t, s, "witself.memory.curation.get", map[string]any{"run_id": f.run.ID, "fencing_generation": f.run.FencingGeneration, "cursor": "input-first", "limit": 1})
			want := conflict.message
			if conflict.name == "typed-conflict" {
				want = "memory curation state conflict (reason=run_not_open)"
			}
			if len(result.Content) != 1 {
				t.Fatal("curation refusal shape changed")
			}
			message, ok := result.Content[0].(*mcp.TextContent)
			if !ok || message.Text != want {
				t.Fatal("curation refusal did not expose defined safe reason")
			}
			// The direct HTTP call also proves the typed recognized conflict survives the real client.
			if conflict.name == "typed-conflict" {
				_, err := client.GetMemoryCurationRunInputs(offlineReadMCPContext(t), f.server.URL, offlineReadMCPToken, f.run.ID, f.run.FencingGeneration, "input-first", 1)
				var typed *client.MemoryCurationConflictError
				if !errors.As(err, &typed) || typed.Reason != "run_not_open" || !errors.Is(err, client.ErrConflict) {
					t.Fatal("HTTP client lost typed curation conflict")
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			expectedCalls := 1
			if conflict.name == "typed-conflict" {
				expectedCalls = 2
			}
			if f.inputCalls != expectedCalls || f.starts != 0 || f.plans != 0 || f.applies != 0 || f.requests != 0 {
				t.Fatal("curation refusal automatically retried protocol work")
			}
		})
	}
}
