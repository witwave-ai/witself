package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMemoryClientVerticalSlice(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	setContent := "Updated decision"
	setContentEncoding := "base64"
	setSensitive := true
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		key := r.Method + " " + r.URL.Path
		seen[key]++
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/memories":
			if r.Header.Get("Idempotency-Key") != "capture-1" {
				t.Fatalf("capture idempotency = %q", r.Header.Get("Idempotency-Key"))
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["content"] != "We chose PostgreSQL." || body["content_encoding"] != "plain" || body["capture_reason"] != "explicit" {
				t.Fatalf("capture body = %#v", body)
			}
			if _, ok := body["idempotency_key"]; ok {
				t.Fatalf("retry key leaked into body = %#v", body)
			}
			writeClientMemoryResult(t, w, "mem_1", 1, "capture", "capture-1", now)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/memories/mem_1":
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","memory":{"id":"mem_1","version":1,"content":"We chose PostgreSQL.","content_encoding":"plain","kind":"decision","tags":[],"links":[],"state":"active","supersession_set_id":"mset_receipt","supersession_set_revision":2,"supersession_replacement_count":3,"supersession_replacement_digest":"`+strings.Repeat("c", 64)+`","active_supersession_set_id":"mset_active","active_supersession_set_revision":4,"evidence":[]}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/memories":
			q := r.URL.Query()
			if q.Get("owner_agent_id") != "agent_1" || q.Get("state") != "active" || q.Get("kind") != "decision" || q.Get("limit") != "25" || q.Get("cursor") != "opaque" {
				t.Fatalf("list query = %s", r.URL.RawQuery)
			}
			if got := q["tag"]; len(got) != 2 || got[0] != "postgres" || got[1] != "architecture" {
				t.Fatalf("list tags = %#v", got)
			}
			if q.Get("occurred_from") != "2026-07-01T00:00:00Z" || q.Get("include_sensitive") != "true" {
				t.Fatalf("list time/sensitive = %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","items":[{"id":"mem_1","version":1,"content_encoding":"plain","kind":"decision","tags":[],"links":[],"state":"active","evidence":[]}],"next_cursor":"next"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/memories:recall":
			body := decodeClientMemoryBody(t, r)
			if body["query"] != "database decision" || body["kind"] != "decision" || body["limit"] != float64(5) || body["cursor"] != "recall" {
				t.Fatalf("recall body = %#v", body)
			}
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","hits":[{"memory":{"id":"mem_1","version":1,"kind":"decision","tags":[],"links":[],"state":"active","evidence":[]},"score":{"lexical":0.8,"salience":0.9,"recency":0.7,"total":0.82}}],"retrieval_mode":"lexical","vector_coverage":0,"degraded":false}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/memories/mem_1/history":
			if r.URL.Query().Get("limit") != "10" || r.URL.Query().Get("cursor") != "history" {
				t.Fatalf("history query = %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","versions":[{"memory_id":"mem_1","version":1,"content_encoding":"base64","kind":"decision","tags":[],"links":[],"state":"active","supersession_set_id":"mset_receipt","supersession_set_revision":2,"supersession_replacement_count":3,"supersession_replacement_digest":"`+strings.Repeat("c", 64)+`","active_supersession_set_id":"mset_active","active_supersession_set_revision":4,"evidence":[]}]}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/memories/mem_1":
			if r.Header.Get("Idempotency-Key") != "adjust-1" {
				t.Fatalf("adjust idempotency = %q", r.Header.Get("Idempotency-Key"))
			}
			body := decodeClientMemoryBody(t, r)
			if body["expected_version"] != float64(1) || body["set_content"] != setContent || body["set_content_encoding"] != setContentEncoding || body["set_sensitive"] != setSensitive {
				t.Fatalf("adjust body = %#v", body)
			}
			assertNoClientRoutingFields(t, body)
			writeClientMemoryResult(t, w, "mem_1", 2, "adjust", "adjust-1", now)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/memories/mem_1/supersede":
			if r.Header.Get("Idempotency-Key") != "supersede-1" {
				t.Fatalf("supersede idempotency = %q", r.Header.Get("Idempotency-Key"))
			}
			body := decodeClientMemoryBody(t, r)
			if body["expected_version"] != float64(2) || body["reason"] != "split decision" {
				t.Fatalf("supersede body = %#v", body)
			}
			assertNoClientRoutingFields(t, body)
			replacements, ok := body["replacements"].([]any)
			if !ok || len(replacements) != 2 {
				t.Fatalf("supersede replacements = %#v", body["replacements"])
			}
			first, ok := replacements[0].(map[string]any)
			if !ok || first["idempotency_key"] != "replacement-1" || first["content"] != "UG9zdGdyZVNRTCBpcyBhdXRob3JpdGF0aXZlLg==" || first["content_encoding"] != "base64" {
				t.Fatalf("first supersede replacement = %#v", replacements[0])
			}
			second, ok := replacements[1].(map[string]any)
			if !ok || second["idempotency_key"] != "replacement-2" {
				t.Fatalf("second supersede replacement = %#v", replacements[1])
			}
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","source":{"id":"mem_1","version":3,"content":"We chose PostgreSQL.","content_encoding":"plain","kind":"decision","tags":[],"links":[],"state":"superseded","operation":"supersede","evidence":[]},"replacements":[{"id":"mem_2","version":1,"content":"UG9zdGdyZVNRTCBpcyBhdXRob3JpdGF0aXZlLg==","content_encoding":"base64","kind":"decision","tags":[],"links":[],"state":"active","evidence":[]},{"id":"mem_3","version":1,"content":"The backend has no AI.","content_encoding":"plain","kind":"decision","tags":[],"links":[],"state":"active","evidence":[]}],"receipt":{"operation":"supersede","actor":{"kind":"agent","id":"agent_1"},"idempotency_key":"supersede-1","canonical_request_hash":"`+strings.Repeat("c", 64)+`","supersession_set_id":"mset_1","supersession_set_revision":1,"replacement_count":2,"replacement_digest":"`+strings.Repeat("d", 64)+`","source":{"memory_id":"mem_1","version":3},"replacements":[{"memory_id":"mem_2","version":1},{"memory_id":"mem_3","version":1}],"created_at":"2026-07-14T08:00:00Z"}}`)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/memories/mem_1:"):
			action := strings.TrimPrefix(r.URL.Path, "/v1/memories/mem_1:")
			if action != "forget" && action != "restore" && action != "reactivate" {
				http.NotFound(w, r)
				return
			}
			if r.Header.Get("Idempotency-Key") != action+"-1" {
				t.Fatalf("%s idempotency = %q", action, r.Header.Get("Idempotency-Key"))
			}
			body := decodeClientMemoryBody(t, r)
			if body["expected_version"] != float64(2) || body["reason"] != "test" {
				t.Fatalf("%s body = %#v", action, body)
			}
			if action == "reactivate" && body["expected_supersession_set_revision"] != float64(7) {
				t.Fatalf("reactivate revision body = %#v", body)
			}
			assertNoClientRoutingFields(t, body)
			writeClientMemoryResult(t, w, "mem_1", 3, action, action+"-1", now)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/memory-evidence/mev_pending/resolution":
			if r.Header.Get("Idempotency-Key") != "resolve-1" {
				t.Fatalf("resolve idempotency = %q", r.Header.Get("Idempotency-Key"))
			}
			body := decodeClientMemoryBody(t, r)
			if body["transcript_id"] != "trn_1" || body["entry_from_sequence"] != float64(2) {
				t.Fatalf("resolve body = %#v", body)
			}
			assertNoClientRoutingFields(t, body)
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","evidence":{"id":"mev_terminal","memory_id":"mem_1","memory_version":1,"state":"resolved","role":"supports","pending_evidence_id":"mev_pending","change_seq":9,"actor":{"kind":"agent","id":"agent_1"},"created_at":"2026-07-14T08:00:00Z"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/memories/mem_delete" && r.URL.Query().Get("dry_run") == "true":
			if r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Witself-Direct-User-Authorized") != "" {
				t.Fatalf("preview mutation headers = %#v", r.Header)
			}
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","deletion":{"memory_id":"mem_delete","prior_version":3,"scrub_set_revision":"`+strings.Repeat("a", 64)+`","version_count":3,"evidence_count":2,"relation_count":0,"retry_shield_count":3,"retry_shield_digest":"`+strings.Repeat("b", 64)+`","blocked":false,"applied":false}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/memories/mem_delete":
			if r.URL.Query().Get("expected_version") != "3" || r.URL.Query().Get("scrub_set_revision") != strings.Repeat("a", 64) {
				t.Fatalf("delete query = %s", r.URL.RawQuery)
			}
			if r.Header.Get("Idempotency-Key") != "delete-1" || r.Header.Get("X-Witself-Direct-User-Authorized") != "true" {
				t.Fatalf("delete headers = %#v", r.Header)
			}
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","deletion":{"memory_id":"mem_delete","receipt_id":"mdel_1","prior_version":3,"scrub_set_revision":"`+strings.Repeat("a", 64)+`","version_count":3,"evidence_count":2,"relation_count":0,"retry_shield_count":3,"retry_shield_digest":"`+strings.Repeat("b", 64)+`","deleted_at":"2026-07-14T08:00:00Z","blocked":false,"applied":true}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	salience := 0.9
	result, err := CaptureMemory(context.Background(), srv.URL, "token", CaptureMemoryInput{
		Content: "We chose PostgreSQL.", ContentEncoding: "plain",
		Kind: "decision", Tags: []string{"postgres"},
		Salience: &salience, CaptureReason: "explicit", IdempotencyKey: "capture-1",
		Evidence: []MemoryEvidenceInput{{State: "pending", Role: "supports", ExternalLocator: "codex/session/turn-4"}},
		Client:   MemoryClientProvenance{Runtime: "codex", Model: "gpt-5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Memory.ID != "mem_1" || result.Receipt.IdempotencyKey != "capture-1" {
		t.Fatalf("capture result = %#v", result)
	}

	memory, err := GetMemory(context.Background(), srv.URL, "token", "mem_1")
	if err != nil || memory.Content != "We chose PostgreSQL." || memory.ContentEncoding != "plain" ||
		memory.SupersessionSetID != "mset_receipt" || memory.SupersessionSetRevision != 2 ||
		memory.SupersessionReplacementCount != 3 ||
		memory.SupersessionReplacementDigest != strings.Repeat("c", 64) ||
		memory.ActiveSupersessionSetID != "mset_active" || memory.ActiveSupersessionSetRevision != 4 {
		t.Fatalf("get = %#v / %v", memory, err)
	}

	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	page, err := ListMemories(context.Background(), srv.URL, "token", MemoryListOptions{
		OwnerAgentID: "agent_1", State: "active", Kind: "decision",
		Tags: []string{"postgres", "architecture"}, IncludeSensitive: true,
		OccurredFrom: &from, Limit: 25, Cursor: "opaque",
	})
	if err != nil || page.NextCursor != "next" || len(page.Items) != 1 {
		t.Fatalf("list = %#v / %v", page, err)
	}

	recalled, err := RecallMemories(context.Background(), srv.URL, "token", MemoryRecallInput{
		Query: "database decision", Kind: "decision", Limit: 5, Cursor: "recall",
	})
	if err != nil || len(recalled.Hits) != 1 || recalled.Hits[0].Score.Total != .82 || recalled.RetrievalMode != "lexical" {
		t.Fatalf("recall = %#v / %v", recalled, err)
	}

	history, err := GetMemoryHistory(context.Background(), srv.URL, "token", "mem_1", MemoryHistoryOptions{Limit: 10, Cursor: "history"})
	if err != nil || len(history.Versions) != 1 || history.Versions[0].MemoryID != "mem_1" ||
		history.Versions[0].ContentEncoding != "base64" ||
		history.Versions[0].SupersessionReplacementCount != 3 ||
		history.Versions[0].ActiveSupersessionSetRevision != 4 {
		t.Fatalf("history = %#v / %v", history, err)
	}

	adjusted, err := AdjustMemory(context.Background(), srv.URL, "token", AdjustMemoryInput{
		MemoryID: "mem_1", ExpectedVersion: 1, SetContent: &setContent,
		SetContentEncoding: &setContentEncoding,
		SetSensitive:       &setSensitive, IdempotencyKey: "adjust-1",
	})
	if err != nil || adjusted.Memory.Version != 2 {
		t.Fatalf("adjust = %#v / %v", adjusted, err)
	}

	superseded, err := SupersedeMemory(context.Background(), srv.URL, "token", SupersedeMemoryInput{
		MemoryID: "mem_1", ExpectedVersion: 2, Reason: "split decision",
		IdempotencyKey: "supersede-1",
		Replacements: []SupersedeMemoryReplacementInput{
			{
				Content: "UG9zdGdyZVNRTCBpcyBhdXRob3JpdGF0aXZlLg==", ContentEncoding: "base64",
				Kind: "decision", CaptureReason: "curation",
				Evidence:       []MemoryEvidenceInput{{State: "pending", ExternalLocator: "codex/turn/8"}},
				IdempotencyKey: "replacement-1",
			},
			{
				Content: "The backend has no AI.", Kind: "decision", CaptureReason: "curation",
				Evidence:       []MemoryEvidenceInput{{State: "unavailable", UnavailableReason: "runtime_did_not_record"}},
				IdempotencyKey: "replacement-2",
			},
		},
	})
	if err != nil || superseded.Source.State != "superseded" || len(superseded.Replacements) != 2 ||
		superseded.Replacements[0].ContentEncoding != "base64" ||
		superseded.Receipt.SupersessionSetID != "mset_1" || superseded.Receipt.ReplacementCount != 2 ||
		superseded.Receipt.ReplacementDigest != strings.Repeat("d", 64) || len(superseded.Receipt.Replacements) != 2 {
		t.Fatalf("supersede = %#v / %v", superseded, err)
	}

	for _, tc := range []struct {
		action string
		call   func(context.Context, string, string, MemoryLifecycleInput) (*MemoryMutationResult, error)
	}{
		{"forget", ForgetMemory},
		{"restore", RestoreMemory},
		{"reactivate", ReactivateMemory},
	} {
		input := MemoryLifecycleInput{
			MemoryID: "mem_1", ExpectedVersion: 2, Reason: "test",
			IdempotencyKey: tc.action + "-1",
		}
		if tc.action == "reactivate" {
			revision := int64(7)
			input.ExpectedSupersessionSetRevision = &revision
		}
		got, callErr := tc.call(context.Background(), srv.URL, "token", input)
		if callErr != nil || got.Memory.Version != 3 || got.Receipt.Operation != tc.action {
			t.Fatalf("%s = %#v / %v", tc.action, got, callErr)
		}
	}

	fromSequence, untilSequence := int64(2), int64(4)
	evidence, err := ResolveMemoryEvidence(context.Background(), srv.URL, "token", ResolveMemoryEvidenceInput{
		EvidenceID: "mev_pending", TranscriptID: "trn_1",
		EntryFromSequence: &fromSequence, EntryUntilSequence: &untilSequence,
		IdempotencyKey: "resolve-1",
	})
	if err != nil || evidence.ID != "mev_terminal" || evidence.PendingEvidenceID != "mev_pending" {
		t.Fatalf("resolve evidence = %#v / %v", evidence, err)
	}

	preview, err := PreviewDeleteMemory(context.Background(), srv.URL, "token", "mem_delete")
	if err != nil || preview.Applied || preview.PriorVersion != 3 || preview.ScrubSetRevision != strings.Repeat("a", 64) {
		t.Fatalf("preview deletion = %#v / %v", preview, err)
	}
	deleted, err := DeleteMemory(context.Background(), srv.URL, "token", DeleteMemoryInput{
		MemoryID: "mem_delete", ExpectedVersion: 3,
		ScrubSetRevision: strings.Repeat("a", 64), IdempotencyKey: "delete-1",
		DirectUserAuthorized: true,
	})
	if err != nil || !deleted.Applied || deleted.ReceiptID != "mdel_1" || deleted.DeletedAt == nil {
		t.Fatalf("delete memory = %#v / %v", deleted, err)
	}

	want := []string{
		http.MethodPost + " /v1/memories",
		http.MethodGet + " /v1/memories/mem_1",
		http.MethodGet + " /v1/memories",
		http.MethodPost + " /v1/memories:recall",
		http.MethodGet + " /v1/memories/mem_1/history",
		http.MethodPatch + " /v1/memories/mem_1",
		http.MethodPost + " /v1/memories/mem_1/supersede",
		http.MethodPost + " /v1/memories/mem_1:forget",
		http.MethodPost + " /v1/memories/mem_1:restore",
		http.MethodPost + " /v1/memories/mem_1:reactivate",
		http.MethodPost + " /v1/memory-evidence/mev_pending/resolution",
	}
	for _, key := range want {
		if seen[key] != 1 {
			t.Fatalf("request %q count = %d", key, seen[key])
		}
	}
	if seen[http.MethodDelete+" /v1/memories/mem_delete"] != 2 {
		t.Fatalf("memory deletion requests = %d", seen[http.MethodDelete+" /v1/memories/mem_delete"])
	}
}

func decodeClientMemoryBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func assertNoClientRoutingFields(t *testing.T, body map[string]any) {
	t.Helper()
	for _, key := range []string{"MemoryID", "memory_id", "IdempotencyKey", "idempotency_key"} {
		if _, ok := body[key]; ok {
			t.Fatalf("client-only field %q leaked into body: %#v", key, body)
		}
	}
}

func writeClientMemoryResult(t *testing.T, w http.ResponseWriter, memoryID string, version int64, operation, idempotencyKey string, now time.Time) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"schema_version": "witself.v0",
		"memory": map[string]any{
			"id": memoryID, "version": version, "kind": "decision",
			"content_encoding": "plain",
			"tags":             []string{}, "links": []string{}, "state": "active",
			"evidence": []any{},
		},
		"receipt": map[string]any{
			"operation": operation, "idempotency_key": idempotencyKey,
			"memory_id": memoryID, "version": version, "created_at": now,
		},
	}); err != nil {
		t.Fatal(err)
	}
}
