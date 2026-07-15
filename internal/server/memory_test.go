package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMemoryRequestBodyCeilingsDelegateStoreBoundaryPayloads(t *testing.T) {
	const maxStoreContentBytes = 256 * 1024
	content := strings.Repeat("<", maxStoreContentBytes)
	evidence := []MemoryEvidenceInput{{State: "unavailable", UnavailableReason: "not_recorded"}}
	var captureCalls, adjustCalls, supersedeCalls int
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: memoryTestAuth(),
		CaptureMemory: func(_ context.Context, _ DomainPrincipal, in CaptureMemoryRequest) (MemoryMutationResult, error) {
			captureCalls++
			if len(in.Content) != maxStoreContentBytes || in.ContentEncoding != "plain" {
				t.Fatalf("capture boundary input = content:%d encoding:%q", len(in.Content), in.ContentEncoding)
			}
			return MemoryMutationResult{Memory: memoryTestRecord(now)}, nil
		},
		AdjustMemory: func(_ context.Context, _ DomainPrincipal, _ string, in AdjustMemoryRequest) (MemoryMutationResult, error) {
			adjustCalls++
			if in.SetContent == nil || len(*in.SetContent) != maxStoreContentBytes ||
				in.SetContentEncoding == nil || *in.SetContentEncoding != "plain" {
				t.Fatalf("adjust boundary input = %#v", in)
			}
			return MemoryMutationResult{Memory: memoryTestRecord(now)}, nil
		},
		SupersedeMemory: func(_ context.Context, _ DomainPrincipal, _ string, in SupersedeMemoryRequest) (SupersedeMemoryResult, error) {
			supersedeCalls++
			if len(in.Replacements) != 32 {
				t.Fatalf("replacement count = %d", len(in.Replacements))
			}
			for i, replacement := range in.Replacements {
				if len(replacement.Content) != maxStoreContentBytes || replacement.ContentEncoding != "plain" {
					t.Fatalf("replacement %d boundary input = content:%d encoding:%q", i, len(replacement.Content), replacement.ContentEncoding)
				}
			}
			return SupersedeMemoryResult{}, nil
		},
	}))
	defer srv.Close()

	captureRaw, err := json.Marshal(CaptureMemoryRequest{
		Content: content, ContentEncoding: "plain", Kind: "note",
		CaptureReason: "manual", Evidence: evidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(captureRaw)) <= maxStoreContentBytes || int64(len(captureRaw)) >= maxMemoryCaptureRequestBytes {
		t.Fatalf("capture JSON size = %d, limit = %d", len(captureRaw), maxMemoryCaptureRequestBytes)
	}
	resp := memoryTestRawRequest(t, srv.URL, http.MethodPost, "/v1/memories", "agent-token", "capture-boundary", captureRaw)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("capture boundary status = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	encoding := "plain"
	adjustRaw, err := json.Marshal(AdjustMemoryRequest{
		ExpectedVersion: 1, SetContent: &content, SetContentEncoding: &encoding,
	})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(adjustRaw)) <= maxStoreContentBytes || int64(len(adjustRaw)) >= maxMemoryAdjustRequestBytes {
		t.Fatalf("adjust JSON size = %d, limit = %d", len(adjustRaw), maxMemoryAdjustRequestBytes)
	}
	resp = memoryTestRawRequest(t, srv.URL, http.MethodPatch, "/v1/memories/mem_1", "agent-token", "adjust-boundary", adjustRaw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("adjust boundary status = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	replacements := make([]SupersedeMemoryReplacementRequest, 32)
	for i := range replacements {
		replacements[i] = SupersedeMemoryReplacementRequest{
			Content: content, ContentEncoding: "plain", Kind: "note",
			CaptureReason: "curation", Evidence: evidence,
			IdempotencyKey: fmt.Sprintf("replacement-boundary-%d", i),
		}
	}
	supersedeRaw, err := json.Marshal(SupersedeMemoryRequest{ExpectedVersion: 1, Replacements: replacements})
	if err != nil {
		t.Fatal(err)
	}
	if len(supersedeRaw) <= 16*1024*1024 || int64(len(supersedeRaw)) >= maxMemorySupersedeRequestBytes {
		t.Fatalf("supersede JSON size = %d, limit = %d", len(supersedeRaw), maxMemorySupersedeRequestBytes)
	}
	resp = memoryTestRawRequest(t, srv.URL, http.MethodPost, "/v1/memories/mem_1/supersede", "agent-token", "supersede-boundary", supersedeRaw)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("supersede boundary status = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	if captureCalls != 1 || adjustCalls != 1 || supersedeCalls != 1 {
		t.Fatalf("delegated calls = capture:%d adjust:%d supersede:%d", captureCalls, adjustCalls, supersedeCalls)
	}
}

func TestMemoryRequestBodyCeilingReturnsRequestEntityTooLarge(t *testing.T) {
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: memoryTestAuth(),
		CaptureMemory: func(context.Context, DomainPrincipal, CaptureMemoryRequest) (MemoryMutationResult, error) {
			t.Fatal("oversized capture reached callback")
			return MemoryMutationResult{}, nil
		},
	}))
	defer srv.Close()

	raw := make([]byte, 0, int(maxMemoryCaptureRequestBytes)+32)
	raw = append(raw, `{"content":"`...)
	raw = append(raw, bytes.Repeat([]byte{'a'}, int(maxMemoryCaptureRequestBytes))...)
	raw = append(raw, `"}`...)
	resp := memoryTestRawRequest(t, srv.URL, http.MethodPost, "/v1/memories", "agent-token", "too-large", raw)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d", resp.StatusCode)
	}
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if out["error"] != "memory request body exceeds the supported limit" {
		t.Fatalf("oversized error = %#v", out)
	}
}

func TestMemoryCaptureContractAndAgentAuthority(t *testing.T) {
	auth := memoryTestAuth()
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	calls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		CaptureMemory: func(_ context.Context, p DomainPrincipal, in CaptureMemoryRequest) (MemoryMutationResult, error) {
			calls++
			if p.Kind != PrincipalKindAgent || p.ID != "agent_1" {
				t.Fatalf("principal = %#v", p)
			}
			if in.Content != "We chose PostgreSQL." || in.ContentEncoding != "plain" || in.Kind != "decision" || in.IdempotencyKey != "capture-1" {
				t.Fatalf("capture = %#v", in)
			}
			if len(in.Evidence) != 1 || in.Evidence[0].State != "pending" || in.Evidence[0].ExternalLocator != "codex/session/turn-4" {
				t.Fatalf("evidence = %#v", in.Evidence)
			}
			memory := memoryTestRecord(now)
			return MemoryMutationResult{
				Memory: memory,
				Receipt: MemoryMutationReceipt{
					Operation: "capture", IdempotencyKey: in.IdempotencyKey,
					MemoryID: memory.ID, Version: memory.Version, CreatedAt: now,
				},
			}, nil
		},
	}))
	defer srv.Close()

	body := `{"content":"We chose PostgreSQL.","kind":"decision","capture_reason":"explicit","evidence":[{"state":"pending","role":"supports","external_locator":"codex/session/turn-4"}]}`
	resp := memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memories", "agent-token", "capture-1", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("capture status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("cache control = %q", got)
	}
	var out struct {
		SchemaVersion string                `json:"schema_version"`
		Memory        Memory                `json:"memory"`
		Receipt       MemoryMutationReceipt `json:"receipt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if out.SchemaVersion != "witself.v0" || out.Memory.ID != "mem_1" || out.Memory.ContentEncoding != "plain" || out.Receipt.IdempotencyKey != "capture-1" {
		t.Fatalf("capture response = %#v", out)
	}

	resp = memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memories", "agent-token", "", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing key status = %d", resp.StatusCode)
	}
	closeBody(t, resp)
	if calls != 1 {
		t.Fatalf("missing key reached callback; calls = %d", calls)
	}

	resp = memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memories", "operator-token", "capture-operator", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator capture status = %d", resp.StatusCode)
	}
	closeBody(t, resp)
	if calls != 1 {
		t.Fatalf("operator capture reached callback; calls = %d", calls)
	}
}

func TestMemorySupersedeContractAndValueFreeReceipt(t *testing.T) {
	auth := memoryTestAuth()
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	calls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		SupersedeMemory: func(_ context.Context, p DomainPrincipal, memoryID string, in SupersedeMemoryRequest) (SupersedeMemoryResult, error) {
			calls++
			if in.IdempotencyKey == "deleted-replay" {
				return SupersedeMemoryResult{}, ErrMemoryDeleted
			}
			if p.Kind != PrincipalKindAgent || p.ID != "agent_1" || memoryID != "mem_1" {
				t.Fatalf("supersede authority = %#v / %q", p, memoryID)
			}
			if in.ExpectedVersion != 1 || in.IdempotencyKey != "supersede-1" || in.Reason != "split the decision" {
				t.Fatalf("supersede request = %#v", in)
			}
			if len(in.Replacements) != 2 || in.Replacements[0].IdempotencyKey != "replacement-1" ||
				in.Replacements[0].ContentEncoding != "base64" ||
				in.Replacements[1].IdempotencyKey != "replacement-2" || in.Replacements[1].ContentEncoding != "plain" ||
				len(in.Replacements[0].Evidence) != 1 || in.Replacements[0].Evidence[0].ExternalLocator != "codex/turn/8" {
				t.Fatalf("supersede replacements = %#v", in.Replacements)
			}
			source := memoryTestRecord(now)
			source.Version = 2
			source.State = "superseded"
			source.Operation = "supersede"
			first := memoryTestRecord(now)
			first.ID, first.Content, first.Sensitive = "mem_2", "private replacement one", true
			second := memoryTestRecord(now)
			second.ID, second.Content = "mem_3", "replacement two"
			return SupersedeMemoryResult{
				Source: source, Replacements: []Memory{first, second},
				Receipt: MemorySupersessionReceipt{
					Operation: "supersede", Actor: MemoryActor{Kind: "agent", ID: "agent_1"},
					IdempotencyKey: "supersede-1", CanonicalRequestHash: strings.Repeat("a", 64),
					SupersessionSetID: "mset_1", SupersessionSetRevision: 1,
					ReplacementCount: 2, ReplacementDigest: strings.Repeat("b", 64),
					Source:       MemoryVersionReference{MemoryID: "mem_1", Version: 2},
					Replacements: []MemoryVersionReference{{MemoryID: "mem_2", Version: 1}, {MemoryID: "mem_3", Version: 1}},
					CreatedAt:    now,
				},
			}, nil
		},
	}))
	defer srv.Close()

	body := `{"expected_version":1,"reason":"split the decision","replacements":[{"content":"cHJpdmF0ZSByZXBsYWNlbWVudCBvbmU=","content_encoding":"base64","kind":"decision","capture_reason":"curation","idempotency_key":"replacement-1","evidence":[{"state":"pending","external_locator":"codex/turn/8"}]},{"content":"replacement two","kind":"decision","capture_reason":"curation","idempotency_key":"replacement-2","evidence":[{"state":"unavailable","unavailable_reason":"runtime_did_not_record"}]}]}`
	resp := memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memories/mem_1/supersede", "agent-token", "supersede-1", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("supersede status = %d", resp.StatusCode)
	}
	var out struct {
		SchemaVersion string                    `json:"schema_version"`
		Source        Memory                    `json:"source"`
		Replacements  []Memory                  `json:"replacements"`
		Receipt       MemorySupersessionReceipt `json:"receipt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if out.SchemaVersion != "witself.v0" || out.Source.Version != 2 || len(out.Replacements) != 2 ||
		out.Replacements[0].Content != "private replacement one" || out.Receipt.SupersessionSetID != "mset_1" ||
		out.Receipt.ReplacementCount != 2 || out.Receipt.ReplacementDigest != strings.Repeat("b", 64) ||
		len(out.Receipt.Replacements) != 2 {
		t.Fatalf("supersede response = %#v", out)
	}
	receiptJSON, err := json.Marshal(out.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(receiptJSON), "private replacement") || strings.Contains(string(receiptJSON), "codex/turn/8") {
		t.Fatalf("supersession receipt retained values: %s", receiptJSON)
	}

	for _, tc := range []struct {
		name, token, key, body string
		want                   int
	}{
		{name: "missing operation key", token: "agent-token", body: body, want: http.StatusBadRequest},
		{name: "missing replacement key", token: "agent-token", key: "supersede-missing", body: `{"expected_version":1,"replacements":[{"content":"one","evidence":[{"state":"unavailable","unavailable_reason":"not_recorded"}]}]}`, want: http.StatusBadRequest},
		{name: "operator", token: "operator-token", key: "supersede-operator", body: body, want: http.StatusForbidden},
		{name: "deleted replay", token: "agent-token", key: "deleted-replay", body: body, want: http.StatusGone},
	} {
		resp = memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memories/mem_1/supersede", tc.token, tc.key, tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s status = %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
		closeBody(t, resp)
	}
	if calls != 2 {
		t.Fatalf("invalid supersede requests reached callback; calls = %d", calls)
	}
}

func TestMemoryReadListHistoryAndLifecycleContracts(t *testing.T) {
	auth := memoryTestAuth()
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	var lifecycleActions []string
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		CaptureMemory: func(context.Context, DomainPrincipal, CaptureMemoryRequest) (MemoryMutationResult, error) {
			return MemoryMutationResult{}, nil
		},
		GetMemory: func(_ context.Context, p DomainPrincipal, id string) (Memory, error) {
			if p.ID != "agent_1" || id != "mem_1" {
				t.Fatalf("get = %#v / %q", p, id)
			}
			return memoryTestRecord(now), nil
		},
		ListMemories: func(_ context.Context, p DomainPrincipal, opts MemoryListOptions) (MemoryPage, error) {
			if p.ID != "agent_1" || opts.State != "active" || opts.Kind != "decision" || opts.Limit != 25 || opts.Cursor != "opaque" {
				t.Fatalf("list = %#v / %#v", p, opts)
			}
			if len(opts.Tags) != 2 || opts.Tags[0] != "postgres" || opts.Tags[1] != "architecture" || opts.OccurredFrom == nil {
				t.Fatalf("list filters = %#v", opts)
			}
			sensitive := memoryTestRecord(now)
			sensitive.Sensitive = true
			sensitive.Content = "must not leak"
			sensitive.ContentHash = "value-derived-hash"
			sensitive.Links = []string{"witself://memory/private"}
			sensitive.StateReason = "private lifecycle reason"
			sensitive.OccurredFrom = &now
			sensitive.OccurredUntil = &now
			sensitive.Client = MemoryClientProvenance{Runtime: "private-runtime", Model: "private-model"}
			sensitive.Evidence = []MemoryEvidence{{ID: "evidence-secret"}}
			return MemoryPage{Items: []Memory{sensitive}, NextCursor: "next"}, nil
		},
		GetMemoryHistory: func(_ context.Context, p DomainPrincipal, id string, opts MemoryHistoryOptions) (MemoryHistoryPage, error) {
			if p.ID != "agent_1" || id != "mem_1" || opts.Limit != 10 || opts.Cursor != "history-cursor" {
				t.Fatalf("history = %#v / %q / %#v", p, id, opts)
			}
			return MemoryHistoryPage{Versions: []MemoryVersion{{
				MemoryID: id, Version: 1, ContentEncoding: "base64", State: "active",
				SupersessionSetID: "mset_receipt", SupersessionSetRevision: 2,
				SupersessionReplacementCount: 3, SupersessionReplacementDigest: strings.Repeat("c", 64),
				ActiveSupersessionSetID: "mset_active", ActiveSupersessionSetRevision: 4,
			}}}, nil
		},
		AdjustMemory: func(_ context.Context, _ DomainPrincipal, id string, in AdjustMemoryRequest) (MemoryMutationResult, error) {
			if id != "mem_1" || in.ExpectedVersion != 1 || in.SetContent == nil || *in.SetContent != "Updated" ||
				in.SetContentEncoding == nil || *in.SetContentEncoding != "base64" || in.IdempotencyKey != "adjust-1" {
				t.Fatalf("adjust = %q / %#v", id, in)
			}
			memory := memoryTestRecord(now)
			memory.Version = 2
			return MemoryMutationResult{Memory: memory}, nil
		},
		ForgetMemory:     memoryTestLifecycle(t, "forget", &lifecycleActions, now),
		RestoreMemory:    memoryTestLifecycle(t, "restore", &lifecycleActions, now),
		ReactivateMemory: memoryTestLifecycle(t, "reactivate", &lifecycleActions, now),
		ResolveMemoryEvidence: func(_ context.Context, p DomainPrincipal, evidenceID string, in ResolveMemoryEvidenceRequest) (MemoryEvidence, error) {
			if p.ID != "agent_1" || evidenceID != "mev_pending" || in.IdempotencyKey != "resolve-1" || in.TranscriptID != "trn_1" || in.EntryFromSequence == nil || *in.EntryFromSequence != 2 {
				t.Fatalf("evidence resolution = %#v / %q / %#v", p, evidenceID, in)
			}
			return MemoryEvidence{ID: "mev_terminal", MemoryID: "mem_1", MemoryVersion: 1, State: "resolved", PendingEvidenceID: evidenceID}, nil
		},
	}))
	defer srv.Close()

	resp := memoryTestRequest(t, srv.URL, http.MethodGet, "/v1/memories/mem_1", "agent-token", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	var getOut struct {
		Memory Memory `json:"memory"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&getOut); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if getOut.Memory.Content != "We chose PostgreSQL." ||
		getOut.Memory.SupersessionSetID != "mset_receipt" ||
		getOut.Memory.ActiveSupersessionSetID != "mset_active" {
		t.Fatalf("get memory = %#v", getOut.Memory)
	}

	listPath := "/v1/memories?state=active&kind=decision&tag=postgres&tag=architecture&occurred_from=2026-07-01T00%3A00%3A00Z&limit=25&cursor=opaque"
	resp = memoryTestRequest(t, srv.URL, http.MethodGet, listPath, "agent-token", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	var listOut MemoryPage
	if err := json.NewDecoder(resp.Body).Decode(&listOut); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if listOut.SchemaVersion != "witself.v0" || listOut.NextCursor != "next" || len(listOut.Items) != 1 {
		t.Fatalf("list response = %#v", listOut)
	}
	assertMemoryBroadRedacted(t, listOut.Items[0])

	resp = memoryTestRequest(t, srv.URL, http.MethodGet, "/v1/memories/mem_1/history?limit=10&cursor=history-cursor", "agent-token", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history status = %d", resp.StatusCode)
	}
	var historyOut MemoryHistoryPage
	if err := json.NewDecoder(resp.Body).Decode(&historyOut); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if historyOut.SchemaVersion != "witself.v0" || len(historyOut.Versions) != 1 ||
		historyOut.Versions[0].ContentEncoding != "base64" ||
		historyOut.Versions[0].SupersessionReplacementCount != 3 ||
		historyOut.Versions[0].ActiveSupersessionSetRevision != 4 {
		t.Fatalf("history response = %#v", historyOut)
	}

	resp = memoryTestRequest(t, srv.URL, http.MethodPatch, "/v1/memories/mem_1", "agent-token", "adjust-1", `{"expected_version":1,"set_content":"Updated","set_content_encoding":"base64"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("adjust status = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	for i, action := range []string{"forget", "restore", "reactivate"} {
		body := `{"expected_version":2,"expected_supersession_set_revision":7,"reason":"test"}`
		resp = memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memories/mem_1:"+action, "agent-token", action+"-1", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", action, resp.StatusCode)
		}
		closeBody(t, resp)
		if len(lifecycleActions) != i+1 || lifecycleActions[i] != action {
			t.Fatalf("lifecycle actions = %#v", lifecycleActions)
		}
	}

	resp = memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memory-evidence/mev_pending/resolution", "agent-token", "resolve-1", `{"transcript_id":"trn_1","entry_from_sequence":2,"entry_until_sequence":4}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("resolve evidence status = %d", resp.StatusCode)
	}
	var evidenceOut struct {
		Evidence MemoryEvidence `json:"evidence"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&evidenceOut); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if evidenceOut.Evidence.ID != "mev_terminal" || evidenceOut.Evidence.PendingEvidenceID != "mev_pending" {
		t.Fatalf("resolve evidence response = %#v", evidenceOut)
	}
}

func TestMemoryValidationAndConflicts(t *testing.T) {
	auth := memoryTestAuth()
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		CaptureMemory: func(context.Context, DomainPrincipal, CaptureMemoryRequest) (MemoryMutationResult, error) {
			return MemoryMutationResult{}, ErrIdempotencyConflict
		},
		ListMemories: func(context.Context, DomainPrincipal, MemoryListOptions) (MemoryPage, error) {
			t.Fatal("invalid list reached callback")
			return MemoryPage{}, nil
		},
		AdjustMemory: func(context.Context, DomainPrincipal, string, AdjustMemoryRequest) (MemoryMutationResult, error) {
			return MemoryMutationResult{}, ErrConflict
		},
		ForgetMemory: func(context.Context, DomainPrincipal, string, MemoryLifecycleRequest) (MemoryMutationResult, error) {
			return MemoryMutationResult{}, nil
		},
	}))
	defer srv.Close()

	resp := memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memories", "agent-token", "no-evidence", `{"content":"one","kind":"note","capture_reason":"explicit"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("evidence-free capture = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	resp = memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memories", "agent-token", "duplicate-key", `{"content":"one","capture_reason":"explicit","evidence":[{"state":"unavailable","unavailable_reason":"no hook"}]}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("idempotency conflict = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	resp = memoryTestRequest(t, srv.URL, http.MethodPatch, "/v1/memories/mem_1", "agent-token", "adjust-2", `{"expected_version":1,"set_content":"two"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("version conflict = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	resp = memoryTestRequest(t, srv.URL, http.MethodGet, "/v1/memories?limit=501", "agent-token", "", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad limit = %d", resp.StatusCode)
	}
	closeBody(t, resp)

	resp = memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memories/mem_1:unknown", "agent-token", "unknown-1", `{"expected_version":1}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown action = %d", resp.StatusCode)
	}
	closeBody(t, resp)
}

func TestMemoryRecallContractAndRedaction(t *testing.T) {
	auth := memoryTestAuth()
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		RecallMemories: func(_ context.Context, p DomainPrincipal, in MemoryRecallRequest) (MemoryRecallPage, error) {
			if p.ID != "agent_1" || in.Query != "database decision" || in.Kind != "decision" || in.Limit != 5 || in.Cursor != "opaque" {
				t.Fatalf("recall input = %#v / %#v", p, in)
			}
			memory := memoryTestRecord(now)
			memory.Sensitive = true
			memory.Content = "private database decision"
			memory.ContentHash = "value-derived"
			memory.Links = []string{"witself://memory/private"}
			memory.StateReason = "private state reason"
			memory.OccurredFrom = &now
			memory.OccurredUntil = &now
			memory.Client = MemoryClientProvenance{Runtime: "private-runtime", Recipe: "private-recipe"}
			memory.Evidence = []MemoryEvidence{{ID: "private-evidence"}}
			return MemoryRecallPage{
				Hits:       []MemoryRecallHit{{Memory: memory, Score: MemoryRecallScore{Lexical: .8, Salience: .9, Recency: .7, Total: .82}}},
				NextCursor: "next", RetrievalMode: "lexical",
			}, nil
		},
	}))
	defer srv.Close()
	resp := memoryTestRequest(t, srv.URL, http.MethodPost, "/v1/memories:recall", "agent-token", "", `{"query":"database decision","kind":"decision","limit":5,"cursor":"opaque"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recall status = %d", resp.StatusCode)
	}
	var out MemoryRecallPage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if out.SchemaVersion != "witself.v0" || out.RetrievalMode != "lexical" || len(out.Hits) != 1 || out.NextCursor != "next" {
		t.Fatalf("recall response = %#v", out)
	}
	if hit := out.Hits[0]; hit.Score.Total != .82 {
		t.Fatalf("recall hit = %#v", hit)
	}
	assertMemoryBroadRedacted(t, out.Hits[0].Memory)
}

func TestMemoryPermanentDeleteRequiresPreviewGuardsAndDirectUserAuthority(t *testing.T) {
	auth := memoryTestAuth()
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	revision := strings.Repeat("a", 64)
	digest := strings.Repeat("b", 64)
	callbackCalls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		DeleteMemory: func(_ context.Context, p DomainPrincipal, in DeleteMemoryRequest) (MemoryDeletionReceipt, error) {
			callbackCalls++
			if p.ID != "agent_1" || in.MemoryID != "mem_delete" {
				t.Fatalf("delete principal/input = %#v / %#v", p, in)
			}
			receipt := MemoryDeletionReceipt{
				MemoryID: in.MemoryID, PriorVersion: 3, ScrubSetRevision: revision,
				VersionCount: 3, EvidenceCount: 2, RetryShieldCount: 3,
				RetryShieldDigest: digest,
			}
			if in.Apply {
				if in.ExpectedVersion != 3 || in.ScrubSetRevision != revision || in.IdempotencyKey != "delete-1" || in.ReasonCode != "" {
					t.Fatalf("delete apply = %#v", in)
				}
				receipt.ReceiptID, receipt.DeletedAt, receipt.Applied = "mdel_1", &now, true
			} else if in.ExpectedVersion != 0 || in.ScrubSetRevision != "" || in.IdempotencyKey != "" || in.ReasonCode != "" {
				t.Fatalf("delete preview = %#v", in)
			}
			return receipt, nil
		},
	}))
	defer srv.Close()

	preview := memoryTestRequest(t, srv.URL, http.MethodDelete, "/v1/memories/mem_delete?dry_run=true", "agent-token", "", "")
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("preview status = %d", preview.StatusCode)
	}
	var previewOut struct {
		Deletion MemoryDeletionReceipt `json:"deletion"`
	}
	if err := json.NewDecoder(preview.Body).Decode(&previewOut); err != nil {
		t.Fatal(err)
	}
	closeBody(t, preview)
	if previewOut.Deletion.Applied || previewOut.Deletion.MemoryID != "mem_delete" || previewOut.Deletion.ScrubSetRevision != revision {
		t.Fatalf("preview = %#v", previewOut)
	}

	unauthorized := memoryTestRequest(t, srv.URL, http.MethodDelete,
		"/v1/memories/mem_delete?expected_version=3&scrub_set_revision="+revision,
		"agent-token", "delete-1", "")
	if unauthorized.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthorized apply status = %d", unauthorized.StatusCode)
	}
	closeBody(t, unauthorized)
	if callbackCalls != 1 {
		t.Fatalf("unauthorized apply reached callback; calls = %d", callbackCalls)
	}

	req, err := http.NewRequest(http.MethodDelete,
		srv.URL+"/v1/memories/mem_delete?expected_version=3&scrub_set_revision="+revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer agent-token")
	req.Header.Set("Idempotency-Key", "delete-1")
	req.Header.Set("X-Witself-Direct-User-Authorized", "true")
	applied, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if applied.StatusCode != http.StatusOK {
		t.Fatalf("apply status = %d", applied.StatusCode)
	}
	var applyOut struct {
		Deletion MemoryDeletionReceipt `json:"deletion"`
	}
	if err := json.NewDecoder(applied.Body).Decode(&applyOut); err != nil {
		t.Fatal(err)
	}
	closeBody(t, applied)
	if !applyOut.Deletion.Applied || applyOut.Deletion.ReceiptID != "mdel_1" || applyOut.Deletion.DeletedAt == nil {
		t.Fatalf("apply = %#v", applyOut)
	}
	if callbackCalls != 2 {
		t.Fatalf("callback calls = %d", callbackCalls)
	}

	for _, path := range []string{
		"/v1/memories/mem_delete?dry_run=true&expected_version=3",
		"/v1/memories/mem_delete?dry_run=true&reason_code=private_123456789",
		"/v1/memories/mem_delete?expected_version=3&scrub_set_revision=" + revision + "&reason_code=private_123456789",
	} {
		resp := memoryTestRequest(t, srv.URL, http.MethodDelete, path, "agent-token", "", "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("invalid deletion %q status = %d", path, resp.StatusCode)
		}
		closeBody(t, resp)
	}
	if callbackCalls != 2 {
		t.Fatalf("invalid requests reached callback; calls = %d", callbackCalls)
	}
}

func TestMemoryCapabilityRequiresCompleteSurface(t *testing.T) {
	auth := memoryTestAuth()
	complete := Config{
		AuthenticatePrincipal: auth,
		CaptureMemory: func(context.Context, DomainPrincipal, CaptureMemoryRequest) (MemoryMutationResult, error) {
			return MemoryMutationResult{}, nil
		},
		GetMemory: func(context.Context, DomainPrincipal, string) (Memory, error) { return Memory{}, nil },
		ListMemories: func(context.Context, DomainPrincipal, MemoryListOptions) (MemoryPage, error) {
			return MemoryPage{}, nil
		},
		RecallMemories: func(context.Context, DomainPrincipal, MemoryRecallRequest) (MemoryRecallPage, error) {
			return MemoryRecallPage{}, nil
		},
		GetMemoryHistory: func(context.Context, DomainPrincipal, string, MemoryHistoryOptions) (MemoryHistoryPage, error) {
			return MemoryHistoryPage{}, nil
		},
		AdjustMemory: func(context.Context, DomainPrincipal, string, AdjustMemoryRequest) (MemoryMutationResult, error) {
			return MemoryMutationResult{}, nil
		},
		SupersedeMemory: func(context.Context, DomainPrincipal, string, SupersedeMemoryRequest) (SupersedeMemoryResult, error) {
			return SupersedeMemoryResult{}, nil
		},
		ForgetMemory: func(context.Context, DomainPrincipal, string, MemoryLifecycleRequest) (MemoryMutationResult, error) {
			return MemoryMutationResult{}, nil
		},
		RestoreMemory: func(context.Context, DomainPrincipal, string, MemoryLifecycleRequest) (MemoryMutationResult, error) {
			return MemoryMutationResult{}, nil
		},
		ReactivateMemory: func(context.Context, DomainPrincipal, string, MemoryLifecycleRequest) (MemoryMutationResult, error) {
			return MemoryMutationResult{}, nil
		},
		ResolveMemoryEvidence: func(context.Context, DomainPrincipal, string, ResolveMemoryEvidenceRequest) (MemoryEvidence, error) {
			return MemoryEvidence{}, nil
		},
		DeleteMemory: func(context.Context, DomainPrincipal, DeleteMemoryRequest) (MemoryDeletionReceipt, error) {
			return MemoryDeletionReceipt{}, nil
		},
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	recorder := httptest.NewRecorder()
	apiMux(complete).ServeHTTP(recorder, request)
	var out struct {
		Features map[string]feature `json:"features"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Features["memories"].Supported {
		t.Fatalf("memories capability = %#v", out.Features["memories"])
	}
	if !out.Features["memory_recall"].Supported || !out.Features["memory_supersede"].Supported || !out.Features["memory_permanent_delete"].Supported ||
		out.Features["automatic_capture"].Supported || out.Features["automatic_capture"].Reason != "not_implemented" {
		t.Fatalf("memory sub-capabilities = %#v", out.Features)
	}

	complete.SupersedeMemory = nil
	recorder = httptest.NewRecorder()
	apiMux(complete).ServeHTTP(recorder, request)
	if err := json.NewDecoder(recorder.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Features["memories"].Supported || out.Features["memories"].Reason != "not_implemented" {
		t.Fatalf("partial memories capability = %#v", out.Features["memories"])
	}
	if !out.Features["memory_recall"].Supported || out.Features["memory_supersede"].Supported || !out.Features["memory_permanent_delete"].Supported {
		t.Fatalf("independent memory sub-capabilities were hidden = %#v", out.Features)
	}
}

func memoryTestLifecycle(t *testing.T, action string, calls *[]string, now time.Time) func(context.Context, DomainPrincipal, string, MemoryLifecycleRequest) (MemoryMutationResult, error) {
	t.Helper()
	return func(_ context.Context, p DomainPrincipal, id string, in MemoryLifecycleRequest) (MemoryMutationResult, error) {
		if p.ID != "agent_1" || id != "mem_1" || in.ExpectedVersion != 2 || in.IdempotencyKey != action+"-1" {
			t.Fatalf("%s = %#v / %q / %#v", action, p, id, in)
		}
		if action == "reactivate" && (in.ExpectedSupersessionSetRevision == nil || *in.ExpectedSupersessionSetRevision != 7) {
			t.Fatalf("reactivate revision = %v", in.ExpectedSupersessionSetRevision)
		}
		*calls = append(*calls, action)
		memory := memoryTestRecord(now)
		memory.Version = 3
		memory.State = map[string]string{"forget": "forgotten", "restore": "active", "reactivate": "active"}[action]
		return MemoryMutationResult{Memory: memory}, nil
	}
}

func memoryTestRecord(now time.Time) Memory {
	return Memory{
		ID: "mem_1", AccountID: "acc_1", RealmID: "realm_1",
		Owner:  MemoryOwner{Kind: "agent", AgentID: "agent_1", AgentName: "scott"},
		Origin: "agent", CaptureReason: "explicit",
		OriginalAuthor: MemoryActor{Kind: "agent", ID: "agent_1", Name: "scott"},
		Version:        1, ChangeSeq: 1, Content: "We chose PostgreSQL.", ContentEncoding: "plain",
		Kind: "decision",
		Tags: []string{"postgres"}, Salience: 0.9, Links: []string{}, State: "active",
		ContentHash: "sha256:test", Operation: "capture",
		SupersessionSetID: "mset_receipt", SupersessionSetRevision: 2,
		SupersessionReplacementCount: 3, SupersessionReplacementDigest: strings.Repeat("c", 64),
		ActiveSupersessionSetID: "mset_active", ActiveSupersessionSetRevision: 4,
		Actor:    MemoryActor{Kind: "agent", ID: "agent_1", Name: "scott"},
		Evidence: []MemoryEvidence{}, CreatedAt: now, UpdatedAt: now,
	}
}

func assertMemoryBroadRedacted(t *testing.T, memory Memory) {
	t.Helper()
	if !memory.Redacted || memory.Content != "" || memory.ContentHash != "" ||
		len(memory.Tags) != 0 || len(memory.Links) != 0 || memory.CaptureReason != "" ||
		memory.StateReason != "" || memory.OccurredFrom != nil || memory.OccurredUntil != nil ||
		memory.Client != (MemoryClientProvenance{}) || len(memory.Evidence) != 0 ||
		memory.SupersessionSetID != "mset_receipt" || memory.SupersessionSetRevision != 2 ||
		memory.SupersessionReplacementCount != 3 ||
		memory.SupersessionReplacementDigest != strings.Repeat("c", 64) ||
		memory.ActiveSupersessionSetID != "mset_active" || memory.ActiveSupersessionSetRevision != 4 {
		t.Fatalf("sensitive broad-read memory was not fully redacted = %#v", memory)
	}
}

func memoryTestAuth() PrincipalAuthFunc {
	return func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		switch token {
		case "agent-token":
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, true, nil
		case "operator-token":
			return DomainPrincipal{Kind: PrincipalKindOperator, ID: "opr_1", AccountID: "acc_1", AccountStatus: "active"}, true, nil
		default:
			return DomainPrincipal{}, false, nil
		}
	}
}

func memoryTestRequest(t *testing.T, base, method, path, token, idempotencyKey, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func memoryTestRawRequest(t *testing.T, base, method, path, token, idempotencyKey string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
