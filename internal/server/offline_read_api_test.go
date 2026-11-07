package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/store"
)

// These are deliberately callback/handler composition proofs. Retaining this
// corpus across apiMux reconstruction does not exercise PostgreSQL or recovery.
const (
	offlineAPIAccount    = "offline-account"
	offlineAPIRealm      = "offline-realm"
	offlineAPIAgent      = "offline-agent"
	offlineAPIToken      = "offline-agent-credential"
	offlineAPIPreview    = "offline-preview-credential"
	offlineAPIApply      = "offline-apply-credential"
	offlineAPIForeign    = "offline-foreign-credential"
	offlineAPIOperator   = "offline-operator-credential"
	offlineAPISuspended  = "offline-suspended-credential"
	offlineAPITranscript = "offline-transcript"
	offlineAPIMemory     = "offline-memory"
	offlineAPIPrivate    = "offline-private-memory"
	offlineAPIFact       = "offline-fact"
	offlineAPISubject    = "offline-subject"
	offlineAPIPredicate  = "project/status"
	offlineAPISource     = "witself://transcripts/offline-transcript/entries/1"
	offlineAPICursor     = "offline-next-page"
	offlineAPIRequestID  = "offline-request"
	offlineAPIRun        = "offline-run"
	offlineAPIPlanHash   = "offline-plan-hash"
)

type offlineAPITransport struct {
	mu            sync.Mutex
	allowed       map[string]bool
	base          *http.Transport
	denied        int
	cacheFailures int
	statuses      []int
}

func (g *offlineAPITransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	allowed := g.allowed[r.URL.Scheme+"://"+r.URL.Host]
	if !allowed {
		g.denied++
	}
	g.mu.Unlock()
	if !allowed {
		return nil, errors.New("offline fixture refused an origin")
	}
	res, err := g.base.RoundTrip(r)
	wantCache := "private, no-store"
	if strings.HasPrefix(r.URL.Path, "/v1/transcripts") {
		wantCache = "no-store"
	}
	if err == nil {
		g.mu.Lock()
		g.statuses = append(g.statuses, res.StatusCode)
		if res.StatusCode < 400 && res.Header.Get("Cache-Control") != wantCache {
			g.cacheFailures++
		}
		g.mu.Unlock()
	}
	return res, err
}

func offlineAPISetup(t *testing.T) (context.Context, *offlineAPITransport) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, "witself"))
	t.Setenv("DSH_HOME", filepath.Join(home, "dsh"))
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := t.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	t.Cleanup(cancel)
	original := http.DefaultTransport
	g := &offlineAPITransport{allowed: map[string]bool{}, base: &http.Transport{Proxy: nil}}
	http.DefaultTransport = g
	t.Cleanup(func() {
		http.DefaultTransport = original
		g.base.CloseIdleConnections()
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.denied != 0 {
			t.Error("fixture attempted a disallowed origin")
		}
		if g.cacheFailures != 0 {
			t.Error("authenticated response lost its no-store policy")
		}
	})
	return ctx, g
}

func (g *offlineAPITransport) serve(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(apiMux(cfg))
	g.mu.Lock()
	g.allowed[srv.URL] = true
	g.mu.Unlock()
	t.Cleanup(srv.Close)
	return srv
}

type offlineAPICorpus struct {
	mu                sync.Mutex
	t                 *testing.T
	transcript        Transcript
	entries           []TranscriptEntry
	fact              Fact
	memories          []Memory
	calls             map[string]int
	principals        []DomainPrincipal
	transcriptCursors []int64
	memoryCursors     []string
	historyCursors    []string
	recallCursors     []string
	run               client.MemoryCurationRun
	request           client.MemoryCurationRequest
	requestReceipt    client.MemoryCurationMutationReceipt
	startReceipt      client.MemoryCurationMutationReceipt
	planReceipt       client.MemoryCurationPlanReceipt
	plan              json.RawMessage
	draft             json.RawMessage
	preview           client.MemoryCurationImpactPreview
	applied           client.ApplyMemoryCurationResult
	inputCursors      []string
	reviewed          bool
	conflict          string
}

func offlineAPINewCorpus(t *testing.T) *offlineAPICorpus {
	t.Helper()
	return &offlineAPICorpus{t: t, calls: map[string]int{}}
}

func offlineAPIPrincipal(token string) (DomainPrincipal, bool) {
	p := DomainPrincipal{Kind: PrincipalKindAgent, ID: offlineAPIAgent, AccountID: offlineAPIAccount, RealmID: offlineAPIRealm, AgentName: "offline-name", RealmName: "offline-realm-name", AccountStatus: "active", AccessProfile: AccessProfileFull, TokenID: "offline-token-id"}
	switch token {
	case offlineAPIToken:
	case offlineAPIPreview:
		p.AccessProfile = AccessProfileCuratorPreview
	case offlineAPIApply:
		p.AccessProfile = AccessProfileCuratorApply
	case offlineAPIForeign:
		p.AccountID = "offline-foreign-account"
		p.RealmID = "offline-foreign-realm"
		p.ID = "offline-foreign-agent"
	case offlineAPIOperator:
		p.Kind = PrincipalKindOperator
		p.ID = "offline-operator"
		p.RealmID = ""
	case offlineAPISuspended:
		p.AccountStatus = "suspended"
	default:
		return DomainPrincipal{}, false
	}
	return p, true
}

// enter is fixture authorization, not evidence of production SQL isolation.
// Callbacks hold mu and record the exact authenticated principal before refusing
// foreign ownership. No corpus operation infers identity from caller payloads.
func (s *offlineAPICorpus) enter(p DomainPrincipal, op string) error {
	s.calls[op]++
	s.principals = append(s.principals, p)
	if p.Kind != PrincipalKindAgent || p.ID != offlineAPIAgent || p.AccountID != offlineAPIAccount || p.RealmID != offlineAPIRealm {
		return ErrForbidden
	}
	expected, _ := offlineAPIPrincipal(offlineAPIToken)
	expected.AccessProfile = p.AccessProfile
	if !reflect.DeepEqual(p, expected) {
		s.t.Error("callback principal provenance changed")
		return ErrForbidden
	}
	return nil
}

func (s *offlineAPICorpus) counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.calls))
	for k, v := range s.calls {
		out[k] = v
	}
	return out
}

func (s *offlineAPICorpus) config() Config {
	cfg := Config{AuthenticatePrincipal: func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		p, ok := offlineAPIPrincipal(token)
		return p, ok, nil
	}}
	cfg.CreateTranscript = func(_ context.Context, p DomainPrincipal, in CreateTranscriptRequest) (Transcript, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "create"); err != nil {
			return Transcript{}, err
		}
		if in.ExternalID != "offline-external" || in.Title != "offline-title" {
			s.t.Error("transcript create input changed")
		}
		s.transcript = Transcript{ID: offlineAPITranscript, AccountID: p.AccountID, RealmID: p.RealmID, OwnerAgentID: p.ID, ExternalID: in.ExternalID, Title: in.Title, Metadata: in.Metadata}
		return s.transcript, nil
	}
	cfg.AppendTranscriptEntry = func(_ context.Context, p DomainPrincipal, id string, in AppendTranscriptEntryRequest) (TranscriptEntry, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "append"); err != nil {
			return TranscriptEntry{}, err
		}
		if id != s.transcript.ID || len(s.entries) >= 2 {
			return TranscriptEntry{}, ErrBadInput
		}
		want := offlineAPIEntryInputs()[len(s.entries)]
		if in.ExternalID != want.ExternalID || in.Role != want.Role || in.Body != want.Body || in.ReplyToEntryID != want.ReplyToEntryID {
			s.t.Error("transcript entry input changed")
		}
		entry := TranscriptEntry{ID: []string{"offline-entry-one", "offline-entry-two"}[len(s.entries)], AccountID: p.AccountID, RealmID: p.RealmID, RecordedByAgentID: p.ID, TranscriptID: id, Sequence: int64(len(s.entries) + 1), ExternalID: in.ExternalID, Role: in.Role, Body: in.Body, ReplyToEntryID: in.ReplyToEntryID}
		s.entries = append(s.entries, entry)
		return entry, nil
	}
	cfg.ListTranscripts = func(_ context.Context, p DomainPrincipal) ([]Transcript, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "transcript-list"); err != nil {
			return nil, err
		}
		return []Transcript{s.transcript}, nil
	}
	transcriptRead := func(p DomainPrincipal, id string, opts TranscriptPageOptions, op string) (TranscriptPage, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, op); err != nil {
			return TranscriptPage{}, err
		}
		if id != s.transcript.ID || opts.Limit != 1 || opts.Tail || opts.AfterSequence < 0 || opts.AfterSequence > 1 {
			return TranscriptPage{}, ErrBadInput
		}
		s.transcriptCursors = append(s.transcriptCursors, opts.AfterSequence)
		n := int(opts.AfterSequence)
		next := int64(0)
		if n == 0 {
			next = 1
		}
		return TranscriptPage{Transcript: s.transcript, Entries: append([]TranscriptEntry(nil), s.entries[n:n+1]...), NextAfterSequence: next}, nil
	}
	cfg.GetTranscriptPage = func(_ context.Context, p DomainPrincipal, id string, opts TranscriptPageOptions) (TranscriptPage, error) {
		return transcriptRead(p, id, opts, "transcript-get")
	}
	cfg.GetTranscriptPageObservational = func(_ context.Context, p DomainPrincipal, id string, opts TranscriptPageOptions) (TranscriptPage, error) {
		return transcriptRead(p, id, opts, "transcript-observe")
	}
	cfg.SetFact = func(_ context.Context, p DomainPrincipal, in SetFactRequest) (Fact, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "set"); err != nil {
			return Fact{}, err
		}
		want := offlineAPIFactInput()
		if in.Subject != want.Subject || in.Predicate != want.Predicate || in.ValueType != want.ValueType || !bytes.Equal(in.Value, want.Value) || in.SourceKind != want.SourceKind || in.SourceRef != want.SourceRef || in.IdempotencyKey != want.IdempotencyKey {
			s.t.Error("fact write provenance changed")
		}
		s.fact = Fact{ID: offlineAPIFact, SubjectID: "offline-subject-id", Subject: in.Subject, Predicate: in.Predicate, ValueType: in.ValueType, Value: append(json.RawMessage(nil), in.Value...), SourceKind: in.SourceKind, SourceRef: in.SourceRef, ResolvedAssertionID: "offline-assertion", Cardinality: "one"}
		return s.fact, nil
	}
	factRead := func(p DomainPrincipal, subject, predicate, op string) (Fact, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, op); err != nil {
			return Fact{}, err
		}
		if subject != offlineAPISubject || predicate != offlineAPIPredicate {
			return Fact{}, ErrNotFound
		}
		return s.fact, nil
	}
	cfg.GetFact = func(_ context.Context, p DomainPrincipal, subject, predicate string) (Fact, error) {
		return factRead(p, subject, predicate, "fact-get")
	}
	cfg.GetFactObservational = func(_ context.Context, p DomainPrincipal, subject, predicate string) (Fact, error) {
		return factRead(p, subject, predicate, "fact-observe")
	}
	factList := func(p DomainPrincipal, in FactListOptions, op string) ([]Fact, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, op); err != nil {
			return nil, err
		}
		if in.Subject != offlineAPISubject {
			return nil, ErrBadInput
		}
		return []Fact{s.fact}, nil
	}
	cfg.ListFacts = func(_ context.Context, p DomainPrincipal, in FactListOptions) ([]Fact, error) {
		return factList(p, in, "fact-list")
	}
	cfg.ListFactsObservational = func(_ context.Context, p DomainPrincipal, in FactListOptions) ([]Fact, error) {
		return factList(p, in, "fact-list-observe")
	}
	cfg.CaptureMemory = func(_ context.Context, p DomainPrincipal, in CaptureMemoryRequest) (MemoryMutationResult, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "capture"); err != nil {
			return MemoryMutationResult{}, err
		}
		n := len(s.memories)
		if n >= 2 {
			return MemoryMutationResult{}, ErrBadInput
		}
		want := offlineAPICaptureInputs()[n]
		var converted client.CaptureMemoryInput
		offlineAPIConvert(s.t, in, &converted)
		converted.IdempotencyKey = in.IdempotencyKey
		if !reflect.DeepEqual(converted, want) {
			s.t.Error("capture input evidence changed")
		}
		actor := MemoryActor{Kind: p.Kind, ID: p.ID, Name: p.AgentName}
		memory := Memory{ID: []string{offlineAPIMemory, offlineAPIPrivate}[n], AccountID: p.AccountID, RealmID: p.RealmID, Owner: MemoryOwner{Kind: PrincipalKindAgent, AgentID: p.ID, AgentName: p.AgentName}, OriginalAuthor: actor, Actor: actor, Origin: "agent", Version: 1, ChangeSeq: int64(n + 1), Content: in.Content, ContentEncoding: in.ContentEncoding, Kind: in.Kind, Tags: in.Tags, Links: in.Links, CaptureReason: in.CaptureReason, Sensitive: in.Sensitive, State: "active", StateReason: "offline-state-reason", ContentHash: "offline-content-hash", Client: in.Client, OccurredFrom: in.OccurredFrom, OccurredUntil: in.OccurredUntil}
		for j, e := range in.Evidence {
			memory.Evidence = append(memory.Evidence, MemoryEvidence{ID: []string{"offline-evidence-transcript", "offline-evidence-memory"}[j], MemoryID: memory.ID, MemoryVersion: 1, ChangeSeq: int64(j + 1), State: e.State, Type: e.Type, Role: e.Role, TranscriptID: e.TranscriptID, EntryFromSequence: e.EntryFromSequence, EntryUntilSequence: e.EntryUntilSequence, SourceMemoryID: e.SourceMemoryID, SourceMemoryVersion: e.SourceMemoryVersion, Actor: actor})
		}
		s.memories = append(s.memories, offlineAPICopy(s.t, memory))
		return MemoryMutationResult{Memory: memory, Receipt: MemoryMutationReceipt{MemoryID: memory.ID, Version: memory.Version, Actor: actor, Operation: "capture", IdempotencyKey: in.IdempotencyKey}}, nil
	}
	cfg.GetMemory = func(_ context.Context, p DomainPrincipal, id string) (Memory, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "memory-get"); err != nil {
			return Memory{}, err
		}
		for _, m := range s.memories {
			if m.ID == id {
				return offlineAPICopy(s.t, m), nil
			}
		}
		return Memory{}, ErrNotFound
	}
	cfg.ListMemories = func(_ context.Context, p DomainPrincipal, in MemoryListOptions) (MemoryPage, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "memory-list"); err != nil {
			return MemoryPage{}, err
		}
		s.memoryCursors = append(s.memoryCursors, in.Cursor)
		if in.Limit == 1 {
			if in.Cursor == "" {
				return offlineAPICopy(s.t, MemoryPage{Items: []Memory{s.memories[0]}, NextCursor: offlineAPICursor}), nil
			}
			if in.Cursor == offlineAPICursor {
				return offlineAPICopy(s.t, MemoryPage{Items: []Memory{s.memories[1]}}), nil
			}
			return MemoryPage{}, ErrBadInput
		}
		// Return unredacted copies even for broad reads: the real handler must redact.
		return offlineAPICopy(s.t, MemoryPage{Items: s.memories}), nil
	}
	cfg.RecallMemories = func(_ context.Context, p DomainPrincipal, in MemoryRecallRequest) (MemoryRecallPage, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "recall"); err != nil {
			return MemoryRecallPage{}, err
		}
		if in.Query != "offline-query" {
			return MemoryRecallPage{}, ErrBadInput
		}
		s.recallCursors = append(s.recallCursors, in.Cursor)
		page := MemoryRecallPage{RetrievalMode: "lexical"}
		for _, m := range s.memories {
			page.Hits = append(page.Hits, MemoryRecallHit{Memory: m})
		}
		if in.Limit == 1 {
			switch in.Cursor {
			case "":
				page.Hits = page.Hits[:1]
				page.NextCursor = offlineAPICursor
			case offlineAPICursor:
				page.Hits = page.Hits[1:]
			default:
				return MemoryRecallPage{}, ErrBadInput
			}
		}
		return offlineAPICopy(s.t, page), nil
	}
	cfg.GetMemoryHistory = func(_ context.Context, p DomainPrincipal, id string, in MemoryHistoryOptions) (MemoryHistoryPage, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "history"); err != nil {
			return MemoryHistoryPage{}, err
		}
		s.historyCursors = append(s.historyCursors, in.Cursor)
		if id != offlineAPIPrivate {
			return MemoryHistoryPage{}, ErrNotFound
		}
		if in.Cursor == offlineAPICursor {
			return MemoryHistoryPage{}, nil
		}
		if in.Cursor != "" {
			return MemoryHistoryPage{}, ErrBadInput
		}
		m := s.memories[1]
		return offlineAPICopy(s.t, MemoryHistoryPage{Versions: []MemoryVersion{offlineAPIHistoryVersion(m)}, NextCursor: offlineAPICursor}), nil
	}
	s.curationConfig(&cfg)
	return cfg
}

// Returned slices must not alias the retained expected corpus: a production
// projection that blanks nested evidence must not also rewrite the oracle.
func offlineAPICopy[T any](t *testing.T, value T) T {
	t.Helper()
	var out T
	offlineAPIConvert(t, value, &out)
	return out
}

func offlineAPIConvert(t *testing.T, from, to any) {
	t.Helper()
	raw, err := json.Marshal(from)
	if err != nil {
		t.Fatal("fixture conversion encode failed")
	}
	if json.Unmarshal(raw, to) != nil {
		t.Fatal("fixture conversion decode failed")
	}
}
func offlineAPIEntryInputs() []client.AppendTranscriptEntryInput {
	return []client.AppendTranscriptEntryInput{{ExternalID: "offline-first", Role: "user", Body: "offline first entry"}, {ExternalID: "offline-second", Role: "assistant", Body: "offline second entry", ReplyToEntryID: "offline-entry-one"}}
}
func offlineAPIFactInput() client.SetFactInput {
	return client.SetFactInput{Subject: offlineAPISubject, Predicate: offlineAPIPredicate, ValueType: "string", Value: json.RawMessage(`"offline accepted"`), SourceKind: "agent", SourceRef: offlineAPISource, IdempotencyKey: "offline-fact-write"}
}
func offlineAPICaptureInputs() []client.CaptureMemoryInput {
	from, until, version := int64(1), int64(2), int64(1)
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	end := start.Add(time.Minute)
	evidence := []client.MemoryEvidenceInput{{State: "resolved", Type: "transcript_entry", Role: "source", TranscriptID: offlineAPITranscript, EntryFromSequence: &from, EntryUntilSequence: &until}}
	one := client.CaptureMemoryInput{Content: "offline visible capsule", ContentEncoding: "utf-8", Kind: "lesson", Tags: []string{"offline-tag"}, Links: []string{"offline-link"}, CaptureReason: "offline-capture", Evidence: evidence, Client: client.MemoryClientProvenance{Runtime: "offline-runtime"}, IdempotencyKey: "offline-capture-one"}
	two := one
	two.Content = "offline private capsule"
	two.Sensitive = true
	two.IdempotencyKey = "offline-capture-two"
	two.OccurredFrom = &start
	two.OccurredUntil = &end
	two.Evidence = append(append([]client.MemoryEvidenceInput(nil), evidence...), client.MemoryEvidenceInput{State: "resolved", Type: "memory_version", Role: "source", SourceMemoryID: offlineAPIMemory, SourceMemoryVersion: &version})
	return []client.CaptureMemoryInput{one, two}
}

func offlineAPISeed(ctx context.Context, t *testing.T, endpoint string, s *offlineAPICorpus) {
	t.Helper()
	tr, err := client.CreateTranscript(ctx, endpoint, offlineAPIToken, client.CreateTranscriptInput{ExternalID: "offline-external", Title: "offline-title"})
	if err != nil || tr.ID != offlineAPITranscript {
		t.Fatal("transcript create composition failed")
	}
	offlineAPIAssertTranscript(t, tr, s.transcript)
	for i, in := range offlineAPIEntryInputs() {
		entry, e := client.AppendTranscriptEntry(ctx, endpoint, offlineAPIToken, tr.ID, in)
		if e != nil || entry.Sequence != int64(i+1) || entry.TranscriptID != tr.ID || entry.Body != in.Body {
			t.Fatal("transcript append composition failed")
		}
		offlineAPIAssertEntry(t, entry, s.entries[i])
	}
	fact, err := client.SetFact(ctx, endpoint, offlineAPIToken, offlineAPIFactInput())
	if err != nil || fact.ID != offlineAPIFact || fact.SourceRef != offlineAPISource {
		t.Fatal("fact set composition failed")
	}
	offlineAPIAssertFact(t, *fact, s.fact)
	for i, in := range offlineAPICaptureInputs() {
		got, e := client.CaptureMemory(ctx, endpoint, offlineAPIToken, in)
		if e != nil || got.Memory.ID != []string{offlineAPIMemory, offlineAPIPrivate}[i] || got.Memory.Version != 1 || got.Receipt.MemoryID != got.Memory.ID || got.Receipt.Version != 1 {
			t.Fatal("memory capture composition failed")
		}
		offlineAPIAssertMemory(t, got.Memory, s.memories[i])
		actor := s.memories[i].Actor
		wantReceipt := client.MemoryMutationReceipt{MemoryID: s.memories[i].ID, Version: s.memories[i].Version, Operation: "capture", Actor: client.MemoryActor{Kind: actor.Kind, ID: actor.ID, Name: actor.Name}, IdempotencyKey: in.IdempotencyKey}
		if !reflect.DeepEqual(got.Receipt, wantReceipt) {
			t.Fatal("memory capture receipt provenance changed")
		}
	}
}

func offlineAPIReadCorpus(ctx context.Context, t *testing.T, endpoint string, s *offlineAPICorpus) {
	t.Helper()
	transcriptList, err := client.ListTranscripts(ctx, endpoint, offlineAPIToken)
	if err != nil || len(transcriptList) != 1 || transcriptList[0].ID != offlineAPITranscript {
		t.Fatal("transcript list composition failed")
	}
	offlineAPIAssertTranscript(t, transcriptList[0], s.transcript)
	for i := range 2 {
		page, e := client.GetTranscriptPage(ctx, endpoint, offlineAPIToken, offlineAPITranscript, client.TranscriptPageOptions{AfterSequence: int64(i), Limit: 1})
		if e != nil || len(page.Entries) != 1 {
			t.Fatal("transcript page composition failed")
		}
		offlineAPIAssertEntry(t, page.Entries[0], s.entries[i])
		offlineAPIAssertTranscript(t, page.Transcript, s.transcript)
		if page.NextAfterSequence != []int64{1, 0}[i] {
			t.Fatal("transcript sequence or provenance changed")
		}
	}
	var expectedFact client.Fact
	offlineAPIConvert(t, s.fact, &expectedFact)
	fact, err := client.GetFact(ctx, endpoint, offlineAPIToken, offlineAPISubject, offlineAPIPredicate)
	if err != nil || !reflect.DeepEqual(fact, &expectedFact) {
		t.Fatal("fact read provenance changed")
	}
	facts, err := client.ListFacts(ctx, endpoint, offlineAPIToken, client.FactListOptions{Subject: offlineAPISubject})
	if err != nil || !reflect.DeepEqual(facts, []client.Fact{expectedFact}) {
		t.Fatal("fact list provenance changed")
	}
	for i, cursor := range []string{"", offlineAPICursor} {
		page, e := client.ListMemories(ctx, endpoint, offlineAPIToken, client.MemoryListOptions{Limit: 1, Cursor: cursor, IncludeSensitive: true})
		if e != nil || len(page.Items) != 1 {
			t.Fatal("memory list page failed")
		}
		offlineAPIAssertMemory(t, page.Items[0], s.memories[i])
		if page.NextCursor != []string{offlineAPICursor, ""}[i] {
			t.Fatal("memory list cursor changed")
		}
	}
	for i, cursor := range []string{"", offlineAPICursor} {
		page, e := client.RecallMemories(ctx, endpoint, offlineAPIToken, client.MemoryRecallInput{Query: "offline-query", Limit: 1, Cursor: cursor, IncludeSensitive: true})
		if e != nil || len(page.Hits) != 1 {
			t.Fatal("memory recall page failed")
		}
		offlineAPIAssertMemory(t, page.Hits[0].Memory, s.memories[i])
		if page.NextCursor != []string{offlineAPICursor, ""}[i] {
			t.Fatal("memory recall cursor changed")
		}
	}
	memory, err := client.GetMemory(ctx, endpoint, offlineAPIToken, offlineAPIPrivate)
	if err != nil {
		t.Fatal("exact memory read failed")
	}
	offlineAPIAssertMemory(t, *memory, s.memories[1])
	history, err := client.GetMemoryHistory(ctx, endpoint, offlineAPIToken, offlineAPIPrivate, client.MemoryHistoryOptions{Limit: 1})
	if err != nil || len(history.Versions) != 1 || history.NextCursor != offlineAPICursor {
		t.Fatal("memory history page failed")
	}
	var expectedVersion client.MemoryVersion
	offlineAPIConvert(t, offlineAPIHistoryVersion(s.memories[1]), &expectedVersion)
	if !reflect.DeepEqual(history.Versions[0], expectedVersion) {
		t.Fatal("memory history exact evidence changed")
	}
	tail, err := client.GetMemoryHistory(ctx, endpoint, offlineAPIToken, offlineAPIPrivate, client.MemoryHistoryOptions{Limit: 1, Cursor: history.NextCursor})
	if err != nil || len(tail.Versions) != 0 || tail.NextCursor != "" {
		t.Fatal("memory history cursor changed")
	}
}
func offlineAPIAssertMemory(t *testing.T, got client.Memory, want Memory) {
	t.Helper()
	// Expected identity is authored by the fixture, never a production normalizer.
	var expected client.Memory
	offlineAPIConvert(t, want, &expected)
	if !reflect.DeepEqual(got, expected) {
		t.Fatal("memory content or exact provenance changed")
	}
}

func offlineAPIAssertTranscript(t *testing.T, got client.Transcript, want Transcript) {
	t.Helper()
	var expected client.Transcript
	offlineAPIConvert(t, want, &expected)
	if !reflect.DeepEqual(got, expected) {
		t.Fatal("transcript owner provenance changed")
	}
}
func offlineAPIAssertEntry(t *testing.T, got client.TranscriptEntry, want TranscriptEntry) {
	t.Helper()
	var expected client.TranscriptEntry
	offlineAPIConvert(t, want, &expected)
	if !reflect.DeepEqual(got, expected) {
		t.Fatal("transcript entry provenance changed")
	}
}
func offlineAPIAssertFact(t *testing.T, got client.Fact, want Fact) {
	t.Helper()
	var expected client.Fact
	offlineAPIConvert(t, want, &expected)
	if !reflect.DeepEqual(got, expected) {
		t.Fatal("fact returned provenance changed")
	}
}
func offlineAPIHistoryVersion(m Memory) MemoryVersion {
	return MemoryVersion{MemoryID: m.ID, Version: m.Version, PreviousVersion: m.PreviousVersion, ChangeSeq: m.ChangeSeq, Actor: m.Actor, Content: m.Content, ContentEncoding: m.ContentEncoding, Kind: m.Kind, Tags: m.Tags, Links: m.Links, Evidence: m.Evidence, Client: m.Client, Sensitive: m.Sensitive, State: m.State, StateReason: m.StateReason, ContentHash: m.ContentHash}
}

func offlineAPIDenials(ctx context.Context, t *testing.T, endpoint string, s *offlineAPICorpus) {
	t.Helper()
	rows := []struct {
		name, token, method, path string
		status                    int
	}{
		{"operator-fact-read", offlineAPIOperator, http.MethodGet, "/v1/facts?predicate=project%2Fstatus", http.StatusForbidden},
		{"operator-fact-write", offlineAPIOperator, http.MethodPost, "/v1/facts", http.StatusForbidden},
		{"operator-recall", offlineAPIOperator, http.MethodPost, "/v1/memories:recall", http.StatusForbidden},
		{"unauthenticated", "", http.MethodGet, "/v1/memories", http.StatusUnauthorized},
		{"invalid-credential", "offline-invalid", http.MethodGet, "/v1/memories", http.StatusUnauthorized},
		{"suspended", offlineAPISuspended, http.MethodGet, "/v1/memories", http.StatusForbidden},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			before := s.counts()
			req, err := http.NewRequestWithContext(ctx, row.method, endpoint+row.path, strings.NewReader(`{}`))
			if err != nil {
				t.Fatal("denial request construction failed")
			}
			if row.token != "" {
				req.Header.Set("Authorization", "Bearer "+row.token)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal("denial request failed")
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode != row.status || !reflect.DeepEqual(before, s.counts()) {
				t.Fatal("handler denial reached a callback or changed status")
			}
		})
	}
	// The foreign callback refusal is intentionally a separate assertion layer.
	for _, read := range []func() error{
		func() error { _, err := client.ListTranscripts(ctx, endpoint, offlineAPIForeign); return err },
		func() error {
			_, err := client.GetTranscriptPage(ctx, endpoint, offlineAPIForeign, offlineAPITranscript, client.TranscriptPageOptions{Limit: 1})
			return err
		},
		func() error {
			_, err := client.GetFact(ctx, endpoint, offlineAPIForeign, offlineAPISubject, offlineAPIPredicate)
			return err
		},
		func() error {
			_, err := client.ListFacts(ctx, endpoint, offlineAPIForeign, client.FactListOptions{Subject: offlineAPISubject})
			return err
		},
		func() error {
			_, err := client.GetMemory(ctx, endpoint, offlineAPIForeign, offlineAPIPrivate)
			return err
		},
		func() error {
			_, err := client.ListMemories(ctx, endpoint, offlineAPIForeign, client.MemoryListOptions{})
			return err
		},
		func() error {
			_, err := client.RecallMemories(ctx, endpoint, offlineAPIForeign, client.MemoryRecallInput{Query: "offline-query"})
			return err
		},
		func() error {
			_, err := client.GetMemoryHistory(ctx, endpoint, offlineAPIForeign, offlineAPIPrivate, client.MemoryHistoryOptions{})
			return err
		},
	} {
		if !errors.Is(read(), client.ErrForbidden) {
			t.Fatal("fixture foreign-principal authorization failed")
		}
	}
}

func TestOfflineReadAPIProvenance(t *testing.T) {
	ctx, guard := offlineAPISetup(t)
	s := offlineAPINewCorpus(t)
	srv := guard.serve(t, s.config())
	offlineAPISeed(ctx, t, srv.URL, s)
	offlineAPIReadCorpus(ctx, t, srv.URL, s)
	offlineAPIDenials(ctx, t, srv.URL, s)
	if !reflect.DeepEqual(s.transcriptCursors, []int64{0, 1}) || !reflect.DeepEqual(s.memoryCursors, []string{"", offlineAPICursor}) || !reflect.DeepEqual(s.recallCursors, []string{"", offlineAPICursor}) || !reflect.DeepEqual(s.historyCursors, []string{"", offlineAPICursor}) {
		t.Fatal("request pagination provenance changed")
	}
	counts := s.counts()
	if counts["create"] != 1 || counts["append"] != 2 || counts["set"] != 1 || counts["capture"] != 2 {
		t.Fatal("composition performed unexpected writes")
	}
}

func TestOfflineReadAPIRedaction(t *testing.T) {
	ctx, guard := offlineAPISetup(t)
	s := offlineAPINewCorpus(t)
	srv := guard.serve(t, s.config())
	offlineAPISeed(ctx, t, srv.URL, s)
	assertRedacted := func(t *testing.T, m client.Memory) {
		t.Helper()
		want := s.memories[1]
		if m.AccountID != want.AccountID || m.RealmID != want.RealmID || m.Owner.Kind != want.Owner.Kind || m.Owner.AgentID != want.Owner.AgentID || m.Owner.AgentName != want.Owner.AgentName || m.Actor.Kind != want.Actor.Kind || m.Actor.ID != want.Actor.ID || m.Actor.Name != want.Actor.Name || m.OriginalAuthor.Kind != want.OriginalAuthor.Kind || m.OriginalAuthor.ID != want.OriginalAuthor.ID || m.OriginalAuthor.Name != want.OriginalAuthor.Name || m.Origin != want.Origin || m.ChangeSeq != want.ChangeSeq {
			t.Fatal("redacted memory retained identity changed")
		}
		if m.ID != offlineAPIPrivate || m.Version != 1 || !m.Sensitive || !m.Redacted || m.Content != "" || m.ContentHash != "" || len(m.Tags) != 0 || len(m.Links) != 0 || m.CaptureReason != "" || m.StateReason != "" || m.OccurredFrom != nil || m.OccurredUntil != nil || m.Client != (client.MemoryClientProvenance{}) || len(m.Evidence) != 0 {
			t.Fatal("sensitive broad response retained content or evidence")
		}
	}
	t.Run("list", func(t *testing.T) {
		page, err := client.ListMemories(ctx, srv.URL, offlineAPIToken, client.MemoryListOptions{})
		if err != nil || len(page.Items) != 2 {
			t.Fatal("broad list failed")
		}
		offlineAPIAssertMemory(t, page.Items[0], s.memories[0])
		assertRedacted(t, page.Items[1])
	})
	t.Run("recall", func(t *testing.T) {
		page, err := client.RecallMemories(ctx, srv.URL, offlineAPIToken, client.MemoryRecallInput{Query: "offline-query", IncludeSensitive: false})
		if err != nil || len(page.Hits) != 2 {
			t.Fatal("redacted recall failed")
		}
		offlineAPIAssertMemory(t, page.Hits[0].Memory, s.memories[0])
		assertRedacted(t, page.Hits[1].Memory)
	})
	exact, err := client.GetMemory(ctx, srv.URL, offlineAPIToken, offlineAPIPrivate)
	if err != nil {
		t.Fatal("exact sensitive read failed")
	}
	offlineAPIAssertMemory(t, *exact, s.memories[1])
	included, err := client.RecallMemories(ctx, srv.URL, offlineAPIToken, client.MemoryRecallInput{Query: "offline-query", IncludeSensitive: true})
	if err != nil || len(included.Hits) != 2 {
		t.Fatal("authorized sensitive recall failed")
	}
	offlineAPIAssertMemory(t, included.Hits[1].Memory, s.memories[1])
	before := s.counts()
	fact, err := client.GetFactObservational(ctx, srv.URL, offlineAPIToken, offlineAPISubject, offlineAPIPredicate)
	if err != nil || fact.SourceRef != offlineAPISource {
		t.Fatal("observational fact read failed")
	}
	offlineAPIAssertFact(t, *fact, s.fact)
	facts, err := client.ListFacts(ctx, srv.URL, offlineAPIToken, client.FactListOptions{Subject: offlineAPISubject, Observational: true})
	if err != nil || len(facts) != 1 || facts[0].SourceRef != offlineAPISource {
		t.Fatal("observational fact list failed")
	}
	offlineAPIAssertFact(t, facts[0], s.fact)
	page, err := client.GetTranscriptPage(ctx, srv.URL, offlineAPIToken, offlineAPITranscript, client.TranscriptPageOptions{Limit: 1, Observational: true})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Sequence != 1 {
		t.Fatal("observational transcript read failed")
	}
	offlineAPIAssertTranscript(t, page.Transcript, s.transcript)
	offlineAPIAssertEntry(t, page.Entries[0], s.entries[0])
	after := s.counts()
	for _, op := range []string{"fact-get", "fact-list", "transcript-get"} {
		if before[op] != after[op] {
			t.Fatal("observational read invoked ordinary callback")
		}
	}
	for _, op := range []string{"fact-observe", "fact-list-observe", "transcript-observe"} {
		if after[op] != before[op]+1 {
			t.Fatal("observational callback count changed")
		}
	}
}

func offlineAPIPlan(empty bool) (store.MemoryCurationPlanDraft, store.MemoryCurationPlan) {
	actions := []store.MemoryCurationPlanAction{}
	if !empty {
		actions = append(actions, store.MemoryCurationPlanAction{Ordinal: 1, Operation: "relate", Relate: &store.MemoryCurationRelateAction{RelationType: "supports", From: store.MemoryCurationVersionReference{MemoryID: offlineAPIMemory, Version: 1}, To: store.MemoryCurationVersionReference{MemoryID: offlineAPIPrivate, Version: 1}}})
	}
	return store.MemoryCurationPlanDraft{Schema: "witself.memory.plan.v1", DraftRevision: 1, Actions: actions}, store.MemoryCurationPlan{Schema: "witself.memory.plan.v1", PlanRevision: 3, Actions: actions}
}

func (s *offlineAPICorpus) curationConfig(cfg *Config) {
	// apiMux advertises preflight only when every curation callback is supplied.
	// Uncommissioned operations are counted refusals, never another workflow.
	refuse := func(p DomainPrincipal) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = s.enter(p, "unexpected-curation")
		s.t.Error("unrequested curation callback invoked")
		return nil, ErrForbidden
	}
	cfg.ListMemoryCurationRequests = func(_ context.Context, p DomainPrincipal, _ MemoryCurationRequestListOptions) (any, error) {
		return refuse(p)
	}
	cfg.GetMemoryCurationRequest = func(_ context.Context, p DomainPrincipal, _ string) (any, error) { return refuse(p) }
	cfg.RenewMemoryCuration = func(_ context.Context, p DomainPrincipal, _ string, _ RenewMemoryCurationRequest) (any, error) {
		return refuse(p)
	}
	cfg.CancelMemoryCuration = func(_ context.Context, p DomainPrincipal, _ string, _ FinishMemoryCurationRequest) (any, error) {
		return refuse(p)
	}
	cfg.AbandonMemoryCuration = func(_ context.Context, p DomainPrincipal, _ string, _ FinishMemoryCurationRequest) (any, error) {
		return refuse(p)
	}
	cfg.RollbackMemoryCuration = func(_ context.Context, p DomainPrincipal, _ string, _ RollbackMemoryCurationRequest) (any, error) {
		return refuse(p)
	}
	cfg.GetMemoryCurationStatus = func(_ context.Context, p DomainPrincipal, _ string) (any, error) { return refuse(p) }

	cfg.RequestMemoryCuration = func(_ context.Context, p DomainPrincipal, in RequestMemoryCurationRequest) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "request"); err != nil {
			return nil, err
		}
		if in.IdempotencyKey != "offline-request-key" || in.TriggerReason != "offline-proof" || !reflect.DeepEqual(in.Scope.Sources, []string{"memories", "transcripts"}) {
			s.t.Error("curation request input changed")
		}
		s.request = client.MemoryCurationRequest{ID: offlineAPIRequestID, AccountID: p.AccountID, RealmID: p.RealmID, OwnerKind: p.Kind, OwnerID: p.ID, ActorKind: p.Kind, ActorID: p.ID, State: "queued", RequestGeneration: 1}
		s.requestReceipt = client.MemoryCurationMutationReceipt{Operation: "request", ActorID: p.ID, RequestID: s.request.ID, RequestGeneration: s.request.RequestGeneration, IdempotencyKey: in.IdempotencyKey, ResultState: s.request.State}
		return client.RequestMemoryCurationResult{Request: s.request, Receipt: s.requestReceipt}, nil
	}
	cfg.StartMemoryCuration = func(_ context.Context, p DomainPrincipal, in StartMemoryCurationRequest) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "start"); err != nil {
			return nil, err
		}
		if in.RequestID != s.request.ID || in.LeaseDuration != time.Minute || in.IdempotencyKey != "offline-start-key" {
			s.t.Error("curation start input changed")
		}
		s.run = client.MemoryCurationRun{ID: offlineAPIRun, AccountID: p.AccountID, RealmID: p.RealmID, OwnerKind: p.Kind, OwnerID: p.ID, ActorKind: p.Kind, ActorID: p.ID, RequestID: s.request.ID, RequestGeneration: 1, FencingGeneration: 7, State: "open", InputCount: 2}
		s.startReceipt = client.MemoryCurationMutationReceipt{Operation: "start", ActorID: p.ID, RequestID: s.request.ID, RunID: s.run.ID, RequestGeneration: s.run.RequestGeneration, FencingGeneration: s.run.FencingGeneration, IdempotencyKey: in.IdempotencyKey, ResultState: s.run.State}
		return client.StartMemoryCurationResult{Run: s.run, Request: s.request, Receipt: s.startReceipt, FirstInputCursor: ""}, nil
	}
	cfg.GetMemoryCurationRun = func(_ context.Context, p DomainPrincipal, id string) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "run-get"); err != nil {
			return nil, err
		}
		if id != s.run.ID {
			return nil, ErrNotFound
		}
		return s.run, nil
	}
	cfg.GetMemoryCurationRunInputs = func(_ context.Context, p DomainPrincipal, id string, in MemoryCurationRunInputOptions) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "inputs"); err != nil {
			return nil, err
		}
		switch s.conflict {
		case "stale-fence":
			return nil, ErrMemoryCurationFenceMismatch
		case "lease-expired":
			return nil, ErrMemoryCurationLeaseExpired
		case "typed-conflict":
			return nil, &MemoryCurationConflictError{Reason: "transcript_range_not_covered", ActionOrdinal: 1}
		}
		if id != s.run.ID || in.FencingGeneration != s.run.FencingGeneration || in.Limit != 1 {
			return nil, ErrBadInput
		}
		expected := []string{"", offlineAPICursor}
		if len(s.inputCursors) >= len(expected) || in.Cursor != expected[len(s.inputCursors)] {
			return nil, ErrBadInput
		}
		s.inputCursors = append(s.inputCursors, in.Cursor)
		if in.Cursor == "" {
			var memory client.Memory
			offlineAPIConvert(s.t, s.memories[0], &memory)
			return client.MemoryCurationRunInputPage{Run: s.run, Inputs: []client.MemoryCurationRunInput{{RunID: id, Ordinal: 1, Kind: "memory", MemoryID: memory.ID, MemoryVersion: memory.Version, Memory: &memory}}, NextCursor: offlineAPICursor}, nil
		}
		var memory client.Memory
		offlineAPIConvert(s.t, s.memories[1], &memory)
		return client.MemoryCurationRunInputPage{Run: s.run, Inputs: []client.MemoryCurationRunInput{{RunID: id, Ordinal: 2, Kind: "memory", MemoryID: memory.ID, MemoryVersion: memory.Version, Memory: &memory}}}, nil
	}
	cfg.PlanMemoryCuration = func(_ context.Context, p DomainPrincipal, id string, in PlanMemoryCurationRequest) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "plan"); err != nil {
			return nil, err
		}
		if id != s.run.ID || in.FencingGeneration != s.run.FencingGeneration || len(s.inputCursors) != 2 || in.IdempotencyKey != "offline-plan-key" {
			s.t.Error("plan preceded complete input review or changed fence")
			return nil, ErrConflict
		}
		var got, want store.MemoryCurationPlanDraft
		if json.Unmarshal(in.Draft, &got) != nil || json.Unmarshal(s.draft, &want) != nil || !reflect.DeepEqual(got, want) {
			s.t.Error("client-authored plan changed")
			return nil, ErrBadInput
		}
		s.run.PlanRevision = 3
		s.run.PlanHash = offlineAPIPlanHash
		s.run.State = "planned"
		s.planReceipt = client.MemoryCurationPlanReceipt{ID: "offline-plan-receipt", Operation: "plan", ActorID: p.ID, IdempotencyKey: in.IdempotencyKey, RequestID: s.request.ID, RequestGeneration: s.run.RequestGeneration, RunID: s.run.ID, FencingGeneration: s.run.FencingGeneration, PlanRevision: s.run.PlanRevision, PlanHash: s.run.PlanHash}
		return client.PlanMemoryCurationResult{Run: s.run, Plan: s.plan, Preview: s.preview, Receipt: s.planReceipt}, nil
	}
	cfg.GetMemoryCurationPlan = func(_ context.Context, p DomainPrincipal, id string, fence int64) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "plan-get"); err != nil {
			return nil, err
		}
		if id != s.run.ID || fence != s.run.FencingGeneration || s.run.State != "planned" {
			return nil, ErrConflict
		}
		s.reviewed = true
		return client.GetMemoryCurationPlanResult{Run: s.run, Plan: s.plan, Preview: s.preview}, nil
	}
	cfg.ApplyMemoryCuration = func(_ context.Context, p DomainPrincipal, id string, in ApplyMemoryCurationRequest) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.enter(p, "apply"); err != nil {
			return nil, err
		}
		if id != s.run.ID || in.FencingGeneration != s.run.FencingGeneration || in.PlanRevision != s.run.PlanRevision || in.PlanHash != s.run.PlanHash || in.IdempotencyKey != "offline-apply-key" || len(s.inputCursors) != 2 || !s.reviewed || s.calls["apply"] != 1 {
			s.t.Error("apply did not use exactly reviewed plan and fence")
			return nil, ErrConflict
		}
		s.run.State = "applied"
		s.run.ApplyReceiptID = "offline-apply-receipt"
		s.request.State = "fulfilled"
		s.applied = client.ApplyMemoryCurationResult{Run: s.run, Request: s.request, Receipt: client.MemoryCurationApplyReceipt{ID: s.run.ApplyReceiptID, Operation: "apply", ActorID: p.ID, IdempotencyKey: in.IdempotencyKey, RunID: s.run.ID, RequestID: s.request.ID, RequestGeneration: s.run.RequestGeneration, FencingGeneration: s.run.FencingGeneration, PlanRevision: s.run.PlanRevision, PlanHash: s.run.PlanHash, ActionResults: []client.MemoryCurationActionApplyResult{}, CursorIntervals: []client.MemoryCurationCursorInterval{}}}
		if s.preview.RelateActions != 0 {
			s.applied.Receipt.ActionResults = []client.MemoryCurationActionApplyResult{{ActionID: "offline-relate-action", Ordinal: 1, Operation: "relate", BeforeHeads: []client.MemoryVersionReference{}, AfterHeads: []client.MemoryVersionReference{}, CreatedMemoryIDs: []string{}, EvidenceIDs: []string{}, RelationIDs: []string{"offline-relation"}, CandidateIDs: []string{}}}
		}
		return offlineAPICopy(s.t, s.applied), nil
	}
}

// The test driver, not an MCP tool or backend, owns this orchestration. The
// callbacks merely retain a bounded protocol record and reject wrong ordering.
func offlineAPICurate(ctx context.Context, t *testing.T, endpoint string, s *offlineAPICorpus, token string, empty, previewOnly bool) {
	t.Helper()
	preflight, err := client.GetMemoryCurationPreflight(ctx, endpoint, token)
	if err != nil {
		t.Fatal("curation preflight failed")
	}
	p, _ := offlineAPIPrincipal(token)
	if preflight.Principal.AccountID != p.AccountID || preflight.Principal.RealmID != p.RealmID || preflight.Principal.AgentID != p.ID || preflight.Principal.AgentName != p.AgentName || preflight.Credential.TokenID != p.TokenID || preflight.Credential.AccessProfile != p.AccessProfile || preflight.Protocol.BackendInference || !preflight.Protocol.ClientInferenceRequired || !preflight.Permissions.Plan || preflight.Permissions.Apply == previewOnly {
		t.Fatal("curation preflight contract changed")
	}
	// Creating a request requires full credentials even when planning is preview.
	request, err := client.RequestMemoryCuration(ctx, endpoint, offlineAPIToken, client.RequestMemoryCurationInput{Scope: client.MemoryCurationScope{Sources: []string{"memories", "transcripts"}}, TriggerReason: "offline-proof", IdempotencyKey: "offline-request-key"})
	if err != nil || !reflect.DeepEqual(request.Request, s.request) || !reflect.DeepEqual(request.Receipt, s.requestReceipt) {
		t.Fatal("curation request failed")
	}
	start, err := client.StartMemoryCuration(ctx, endpoint, token, client.StartMemoryCurationInput{RequestID: request.Request.ID, LeaseDuration: time.Minute, IdempotencyKey: "offline-start-key"})
	if err != nil || start.Run.ID != offlineAPIRun || start.Run.FencingGeneration != 7 {
		t.Fatal("curation start fence changed")
	}
	offlineAPIAssertRun(t, start.Run, s.run)
	if !reflect.DeepEqual(start.Request, s.request) || !reflect.DeepEqual(start.Receipt, s.startReceipt) || start.FirstInputCursor != "" {
		t.Fatal("curation start provenance changed")
	}
	cursor := start.FirstInputCursor
	for i := range 2 {
		page, e := client.GetMemoryCurationRunInputs(ctx, endpoint, token, start.Run.ID, start.Run.FencingGeneration, cursor, 1)
		if e != nil || len(page.Inputs) != 1 || page.Run.ID != start.Run.ID || page.Run.FencingGeneration != start.Run.FencingGeneration || page.Inputs[0].Ordinal != int64(i+1) {
			t.Fatal("curation input page failed")
		}
		offlineAPIAssertRun(t, page.Run, s.run)
		in := page.Inputs[0]
		if in.RunID != s.run.ID || in.Kind != "memory" || in.MemoryID != s.memories[i].ID || in.MemoryVersion != 1 || in.Memory == nil || page.NextCursor != []string{offlineAPICursor, ""}[i] {
			t.Fatal("curation memory evidence changed")
		}
		offlineAPIAssertMemory(t, *in.Memory, s.memories[i])
		if len(in.Memory.Evidence) == 0 || in.Memory.Evidence[0].TranscriptID != offlineAPITranscript || in.Memory.Evidence[0].EntryFromSequence == nil || *in.Memory.Evidence[0].EntryFromSequence != 1 || in.Memory.Evidence[0].EntryUntilSequence == nil || *in.Memory.Evidence[0].EntryUntilSequence != 2 {
			t.Fatal("curation exact transcript evidence changed")
		}
		var expectedMemory client.Memory
		offlineAPIConvert(t, s.memories[i], &expectedMemory)
		wantInput := client.MemoryCurationRunInput{RunID: s.run.ID, Ordinal: int64(i + 1), Kind: "memory", MemoryID: expectedMemory.ID, MemoryVersion: expectedMemory.Version, Memory: &expectedMemory}
		if !reflect.DeepEqual(in, wantInput) {
			t.Fatal("curation input identity changed")
		}
		cursor = page.NextCursor
		if s.counts()["apply"] != 0 || s.counts()["plan"] != 0 {
			t.Fatal("curation mutated before all input pages")
		}
	}
	draft, normalized := offlineAPIPlan(empty)
	s.draft, err = json.Marshal(draft)
	if err != nil {
		t.Fatal("curation draft encoding failed")
	}
	s.plan, err = json.Marshal(normalized)
	if err != nil {
		t.Fatal("curation plan encoding failed")
	}
	s.preview = client.MemoryCurationImpactPreview{ActionCount: len(draft.Actions), RelateActions: len(draft.Actions), RelationRows: len(draft.Actions)}
	plan, err := client.PlanMemoryCuration(ctx, endpoint, token, client.PlanMemoryCurationInput{RunID: start.Run.ID, FencingGeneration: start.Run.FencingGeneration, Draft: s.draft, IdempotencyKey: "offline-plan-key"})
	if err != nil || plan.Run.PlanRevision != 3 || plan.Run.PlanHash != offlineAPIPlanHash || plan.Receipt.PlanHash != plan.Run.PlanHash {
		t.Fatal("curation accepted plan metadata changed")
	}
	offlineAPIAssertRun(t, plan.Run, s.run)
	var accepted store.MemoryCurationPlan
	if json.Unmarshal(plan.Plan, &accepted) != nil || !reflect.DeepEqual(accepted, normalized) || plan.Preview != s.preview || !reflect.DeepEqual(plan.Receipt, s.planReceipt) || len(plan.PreallocatedMemoryIDs) != 0 {
		t.Fatal("curation accepted plan provenance changed")
	}
	review, err := client.GetMemoryCurationPlan(ctx, endpoint, token, start.Run.ID, start.Run.FencingGeneration)
	if err != nil {
		t.Fatal("curation plan review failed")
	}
	offlineAPIAssertRun(t, review.Run, s.run)
	if len(review.PreallocatedMemoryIDs) != 0 {
		t.Fatal("curation review added unallocated identities")
	}
	var reviewed store.MemoryCurationPlan
	if json.Unmarshal(review.Plan, &reviewed) != nil || !reflect.DeepEqual(reviewed, normalized) || review.Preview != s.preview || review.Run.FencingGeneration != start.Run.FencingGeneration || review.Run.PlanRevision != plan.Run.PlanRevision || review.Run.PlanHash != plan.Run.PlanHash {
		t.Fatal("curation review changed normalized actions or impact")
	}
	// Only these exact plan-get coordinates may be applied after client review.
	result, err := client.ApplyMemoryCuration(ctx, endpoint, token, client.ApplyMemoryCurationInput{RunID: review.Run.ID, FencingGeneration: review.Run.FencingGeneration, PlanRevision: review.Run.PlanRevision, PlanHash: review.Run.PlanHash, IdempotencyKey: "offline-apply-key"})
	if previewOnly {
		if !errors.Is(err, client.ErrForbidden) || s.counts()["apply"] != 0 {
			t.Fatal("preview apply must be forbidden before its callback")
		}
	} else if err != nil || result.Run.State != "applied" || result.Receipt.PlanHash != review.Run.PlanHash || result.Receipt.PlanRevision != review.Run.PlanRevision || result.Receipt.FencingGeneration != review.Run.FencingGeneration || s.counts()["apply"] != 1 {
		t.Fatal("curation exact reviewed apply failed")
	} else {
		var expected client.ApplyMemoryCurationResult
		offlineAPIConvert(t, s.applied, &expected)
		if !reflect.DeepEqual(*result, expected) {
			t.Fatal("curation apply result provenance changed")
		}
	}
	counts := s.counts()
	if counts["start"] != 1 || counts["inputs"] != 2 || counts["plan"] != 1 || counts["plan-get"] != 1 || !reflect.DeepEqual(s.inputCursors, []string{"", offlineAPICursor}) {
		t.Fatal("curation performed unexpected orchestration calls")
	}
}

func offlineAPIAssertRun(t *testing.T, got, want client.MemoryCurationRun) {
	t.Helper()
	var expected client.MemoryCurationRun
	offlineAPIConvert(t, want, &expected)
	if !reflect.DeepEqual(got, expected) {
		t.Fatal("curation run provenance changed")
	}
}

func TestOfflineReadAPICurationProtocol(t *testing.T) {
	for _, row := range []struct {
		name, token    string
		empty, preview bool
	}{{"full", offlineAPIToken, false, false}, {"empty-plan", offlineAPIToken, true, false}, {"curator-apply", offlineAPIApply, false, false}, {"preview-apply", offlineAPIPreview, false, true}} {
		t.Run(row.name, func(t *testing.T) {
			ctx, guard := offlineAPISetup(t)
			s := offlineAPINewCorpus(t)
			srv := guard.serve(t, s.config())
			offlineAPISeed(ctx, t, srv.URL, s)
			offlineAPICurate(ctx, t, srv.URL, s, row.token, row.empty, row.preview)
		})
	}
	for _, name := range []string{"stale-fence", "lease-expired", "typed-conflict"} {
		t.Run(name, func(t *testing.T) {
			ctx, guard := offlineAPISetup(t)
			s := offlineAPINewCorpus(t)
			s.conflict = name
			srv := guard.serve(t, s.config())
			// A failed page is a stopping boundary. The actual lease/fence branches have
			// fixed 409 text, not a typed reason; a separate classified conflict proves
			// the client's existing typed safe-reason projection.
			_, err := client.GetMemoryCurationRunInputs(ctx, srv.URL, offlineAPIToken, offlineAPIRun, 7, "", 1)
			if err == nil {
				t.Fatal("curation conflict was accepted")
			}
			guard.mu.Lock()
			statuses := append([]int(nil), guard.statuses...)
			guard.mu.Unlock()
			if !reflect.DeepEqual(statuses, []int{http.StatusConflict}) {
				t.Fatal("curation conflict response status changed")
			}
			var classified *client.MemoryCurationConflictError
			if name == "typed-conflict" {
				if !errors.As(err, &classified) || classified.Reason != "transcript_range_not_covered" || classified.ActionOrdinal != 1 || !errors.Is(err, client.ErrConflict) {
					t.Fatal("typed curation conflict lost safe reason")
				}
			} else {
				want := ErrMemoryCurationFenceMismatch
				if name == "lease-expired" {
					want = ErrMemoryCurationLeaseExpired
				}
				if errors.As(err, &classified) || !strings.HasSuffix(err.Error(), want.Error()) {
					t.Fatal("lease or fence refusal lost its fixed safe error")
				}
			}
			counts := s.counts()
			if counts["inputs"] != 1 || counts["start"] != 0 || counts["plan"] != 0 || counts["apply"] != 0 {
				t.Fatal("curation conflict triggered automatic protocol writes")
			}
		})
	}
}

func TestOfflineReadAPIReopen(t *testing.T) {
	ctx, guard := offlineAPISetup(t)
	s := offlineAPINewCorpus(t)
	first := guard.serve(t, s.config())
	offlineAPISeed(ctx, t, first.URL, s)
	offlineAPICurate(ctx, t, first.URL, s, offlineAPIToken, false, false)
	before := s.counts()
	first.Close()
	// Only these callback values survive: this is handler/client reconstruction,
	// with no process restart, filesystem commit or PostgreSQL durability claim.
	second := guard.serve(t, s.config())
	offlineAPIReadCorpus(ctx, t, second.URL, s)
	run, err := client.GetMemoryCurationRun(ctx, second.URL, offlineAPIToken, offlineAPIRun)
	var expectedRun client.MemoryCurationRun
	offlineAPIConvert(t, s.applied.Run, &expectedRun)
	if err != nil || !reflect.DeepEqual(*run, expectedRun) || run.State != "applied" || run.ApplyReceiptID != s.applied.Receipt.ID {
		t.Fatal("retained completed curation result changed")
	}
	offlineAPIDenials(ctx, t, second.URL, s)
	after := s.counts()
	for _, op := range []string{"create", "append", "set", "capture", "request", "start", "plan", "apply"} {
		if before[op] != after[op] {
			t.Fatal("handler reconstruction repeated a write")
		}
	}
}
