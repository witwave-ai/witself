package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

func TestMemoryCommandsUseOptimisticGuardsAndIdempotency(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacementsFile := filepath.Join(t.TempDir(), "replacements.json")
	replacementsJSON := `[{"content":"ZGF0YWJhc2UgZGVjaXNpb24=","content_encoding":"base64","kind":"decision","capture_reason":"curation","idempotency_key":"replacement-1","evidence":[{"state":"resolved","type":"memory","source_memory_id":"mem_1","source_memory_version":2}]}]`
	if err := os.WriteFile(replacementsFile, []byte(replacementsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer witself_agt_memory" {
			t.Errorf("Authorization = %q", got)
		}
		key := r.Method + " " + r.URL.Path
		mu.Lock()
		seen[key]++
		mu.Unlock()
		switch key {
		case "POST /v1/memories":
			if got := r.Header.Get("Idempotency-Key"); got != "capture-1" {
				t.Errorf("capture idempotency key = %q", got)
			}
			var in client.CaptureMemoryInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode capture: %v", err)
			}
			if in.Content != "we chose PostgreSQL" || in.ContentEncoding != "plain" || in.Kind != "decision" || len(in.Tags) != 2 {
				t.Errorf("capture input = %#v", in)
			}
			if len(in.Evidence) != 1 || in.Evidence[0].TranscriptID != "trn_1" || in.Evidence[0].EntryFromSequence == nil || *in.Evidence[0].EntryFromSequence != 2 {
				t.Errorf("capture evidence = %#v", in.Evidence)
			}
			writeMemoryMutation(t, w, "mem_1", 1, "active", "capture", "capture-1")
		case "GET /v1/memories/mem_1":
			_ = json.NewEncoder(w).Encode(map[string]any{"memory": memoryFixture("mem_1", 1, "active", "capture")})
		case "GET /v1/memories":
			if r.URL.Query().Get("state") != "active" || r.URL.Query().Get("kind") != "decision" || r.URL.Query().Get("limit") != "10" {
				t.Errorf("list query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(client.MemoryPage{Items: []client.Memory{memoryFixture("mem_1", 1, "active", "capture")}})
		case "POST /v1/memories:recall":
			var in client.MemoryRecallInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode recall: %v", err)
			}
			if in.Query != "database decision" || in.Kind != "decision" || in.Limit != 4 {
				t.Errorf("recall input = %#v", in)
			}
			_ = json.NewEncoder(w).Encode(client.MemoryRecallPage{
				Hits: []client.MemoryRecallHit{{
					Memory: memoryFixture("mem_1", 1, "active", "capture"),
					Score:  client.MemoryRecallScore{Lexical: .8, Salience: .7, Recency: .6, Total: .745},
				}},
				RetrievalMode: "lexical",
			})
		case "GET /v1/memories/mem_1/history":
			if r.URL.Query().Get("limit") != "5" {
				t.Errorf("history query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(client.MemoryHistoryPage{Versions: []client.MemoryVersion{{
				MemoryID: "mem_1", Version: 1, ContentEncoding: "plain", State: "active", Operation: "capture",
				SupersessionSetID: "mset_receipt", SupersessionSetRevision: 2,
				SupersessionReplacementCount: 3, SupersessionReplacementDigest: strings.Repeat("c", 64),
				ActiveSupersessionSetID: "mset_active", ActiveSupersessionSetRevision: 4,
			}}})
		case "PATCH /v1/memories/mem_1":
			if got := r.Header.Get("Idempotency-Key"); got != "adjust-1" {
				t.Errorf("adjust idempotency key = %q", got)
			}
			var in client.AdjustMemoryInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode adjust: %v", err)
			}
			if in.ExpectedVersion != 1 || in.SetContent == nil || *in.SetContent != "dXBkYXRlZCBkZWNpc2lvbg==" ||
				in.SetContentEncoding == nil || *in.SetContentEncoding != "base64" || len(in.AddTags) != 1 {
				t.Errorf("adjust input = %#v", in)
			}
			writeMemoryMutation(t, w, "mem_1", 2, "active", "adjust", "adjust-1")
		case "POST /v1/memories/mem_1/supersede":
			if got := r.Header.Get("Idempotency-Key"); got != "supersede-1" {
				t.Errorf("supersede idempotency key = %q", got)
			}
			var in client.SupersedeMemoryInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode supersede: %v", err)
			}
			if in.ExpectedVersion != 2 || len(in.Replacements) != 1 ||
				in.Replacements[0].IdempotencyKey != "replacement-1" || in.Replacements[0].ContentEncoding != "base64" ||
				len(in.Replacements[0].Evidence) != 1 {
				t.Errorf("supersede input = %#v", in)
			}
			_ = json.NewEncoder(w).Encode(client.SupersedeMemoryResult{
				Source:       memoryFixture("mem_1", 3, "superseded", "supersede"),
				Replacements: []client.Memory{memoryFixture("mem_2", 1, "active", "capture")},
				Receipt: client.MemorySupersessionReceipt{
					Operation: "supersede", IdempotencyKey: "supersede-1",
					SupersessionSetID: "mset_1", SupersessionSetRevision: 1,
					ReplacementCount: 1, ReplacementDigest: strings.Repeat("c", 64),
					Source:       client.MemoryVersionReference{MemoryID: "mem_1", Version: 3},
					Replacements: []client.MemoryVersionReference{{MemoryID: "mem_2", Version: 1}},
				},
			})
		case "POST /v1/memories/mem_1:forget", "POST /v1/memories/mem_1:restore", "POST /v1/memories/mem_1:reactivate":
			var in client.MemoryLifecycleInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode lifecycle: %v", err)
			}
			if in.ExpectedVersion < 2 || r.Header.Get("Idempotency-Key") == "" {
				t.Errorf("lifecycle input/header = %#v / %q", in, r.Header.Get("Idempotency-Key"))
			}
			state, operation := "forgotten", "forget"
			if strings.HasSuffix(r.URL.Path, ":restore") {
				state, operation = "active", "restore"
			}
			if strings.HasSuffix(r.URL.Path, ":reactivate") {
				state, operation = "active", "reactivate"
				if in.ExpectedSupersessionSetRevision == nil || *in.ExpectedSupersessionSetRevision != 2 {
					t.Errorf("reactivate supersession revision = %#v", in.ExpectedSupersessionSetRevision)
				}
			}
			writeMemoryMutation(t, w, "mem_1", in.ExpectedVersion+1, state, operation, r.Header.Get("Idempotency-Key"))
		case "POST /v1/memory-evidence/mev_pending/resolution":
			var in client.ResolveMemoryEvidenceInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode evidence resolution: %v", err)
			}
			if in.TranscriptID != "trn_1" || in.EntryFromSequence == nil || *in.EntryFromSequence != 2 || r.Header.Get("Idempotency-Key") != "resolve-1" {
				t.Errorf("evidence resolution input/header = %#v / %q", in, r.Header.Get("Idempotency-Key"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"evidence": client.MemoryEvidence{
				ID: "mev_terminal", MemoryID: "mem_1", MemoryVersion: 1,
				State: "resolved", PendingEvidenceID: "mev_pending",
			}})
		case "DELETE /v1/memories/mem_delete":
			revision, digest := strings.Repeat("a", 64), strings.Repeat("b", 64)
			receipt := client.MemoryDeletionReceipt{
				MemoryID: "mem_delete", PriorVersion: 3, ScrubSetRevision: revision,
				VersionCount: 3, EvidenceCount: 2, RetryShieldCount: 3,
				RetryShieldDigest: digest,
			}
			if r.URL.Query().Get("dry_run") == "true" {
				if r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Witself-Direct-User-Authorized") != "" {
					t.Errorf("preview headers = %#v", r.Header)
				}
			} else {
				if r.URL.Query().Get("expected_version") != "3" || r.URL.Query().Get("scrub_set_revision") != revision ||
					r.Header.Get("Idempotency-Key") != "delete-1" || r.Header.Get("X-Witself-Direct-User-Authorized") != "true" {
					t.Errorf("delete request = %s / %#v", r.URL.RawQuery, r.Header)
				}
				now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
				receipt.ReceiptID, receipt.DeletedAt, receipt.Applied = "mdel_1", &now, true
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"deletion": receipt})
		default:
			t.Errorf("unexpected request %s", key)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	connection := []string{"--endpoint", srv.URL, "--token-file", tokenFile}
	commands := [][]string{
		append([]string{"memory", "capture"}, append(connection,
			"--content", "we chose PostgreSQL", "--kind", "decision", "--tag", "architecture,database",
			"--evidence-transcript", "trn_1", "--evidence-from-sequence", "2", "--evidence-until-sequence", "4",
			"--idempotency-key", "capture-1")...),
		append([]string{"memory", "show", "mem_1"}, connection...),
		append([]string{"memory", "list"}, append(connection, "--state", "active", "--kind", "decision", "--limit", "10")...),
		append([]string{"memory", "recall", "database decision"}, append(connection, "--kind", "decision", "--limit", "4")...),
		append([]string{"memory", "history", "mem_1"}, append(connection, "--limit", "5")...),
		append([]string{"memory", "adjust", "mem_1"}, append(connection,
			"--expected-version", "1", "--content", "dXBkYXRlZCBkZWNpc2lvbg==", "--content-encoding", "base64",
			"--add-tag", "settled", "--idempotency-key", "adjust-1")...),
		append([]string{"memory", "supersede", "mem_1"}, append(connection,
			"--expected-version", "2", "--replacements-file", replacementsFile, "--idempotency-key", "supersede-1")...),
		append([]string{"memory", "forget", "mem_1"}, append(connection, "--expected-version", "2", "--idempotency-key", "forget-1")...),
		append([]string{"memory", "restore", "mem_1"}, append(connection, "--expected-version", "3", "--idempotency-key", "restore-1")...),
		append([]string{"memory", "reactivate", "mem_1"}, append(connection,
			"--expected-version", "4", "--expected-supersession-set-revision", "2", "--idempotency-key", "reactivate-1")...),
		append([]string{"memory", "evidence", "resolve", "mev_pending"}, append(connection,
			"--transcript", "trn_1", "--from-sequence", "2", "--until-sequence", "4", "--idempotency-key", "resolve-1")...),
		append([]string{"memory", "delete", "mem_delete"}, append(connection, "--dry-run")...),
		append([]string{"memory", "delete", "mem_delete"}, append(connection,
			"--yes", "--expected-version", "3", "--scrub-set-revision", strings.Repeat("a", 64), "--idempotency-key", "delete-1")...),
	}
	for _, command := range commands {
		stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(command) })
		if code != 0 {
			t.Fatalf("run(%q) = %d", command, code)
		}
		if command[1] == "show" && (!strings.Contains(stdout, "supersession set:\tmset_receipt") ||
			!strings.Contains(stdout, "supersession replacement digest:\t"+strings.Repeat("c", 64)) ||
			!strings.Contains(stdout, "active supersession set:\tmset_active")) {
			t.Fatalf("show omitted supersession metadata:\n%s", stdout)
		}
		if command[1] == "history" && (!strings.Contains(stderr, "receipt-set") ||
			!strings.Contains(stderr, "active-set") || !strings.Contains(stdout, "mset_receipt") ||
			!strings.Contains(stdout, "mset_active")) {
			t.Fatalf("history omitted supersession metadata:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 12 || seen["DELETE /v1/memories/mem_delete"] != 2 {
		t.Fatalf("requests = %#v", seen)
	}
}

func TestMemoryCommandsRejectUnguardedMutations(t *testing.T) {
	for _, command := range [][]string{
		{"memory", "capture", "--content", "decision"},
		{"memory", "adjust", "mem_1", "--expected-version", "1", "--content", "new"},
		{"memory", "supersede", "mem_1", "--expected-version", "1", "--idempotency-key", "supersede-1"},
		{"memory", "forget", "mem_1", "--expected-version", "1"},
		{"memory", "restore", "mem_1", "--idempotency-key", "restore-1"},
		{"memory", "reactivate", "mem_1", "--expected-version", "0", "--idempotency-key", "reactivate-1"},
		{"memory", "evidence", "resolve", "mev_pending", "--transcript", "trn_1", "--from-sequence", "1", "--until-sequence", "2"},
		{"memory", "delete", "mem_1"},
		{"memory", "delete", "mem_1", "--yes", "--expected-version", "1", "--scrub-set-revision", strings.Repeat("a", 64)},
		{"memory", "delete", "mem_1", "--dry-run", "--expected-version", "1"},
	} {
		if code := run(command); code != 2 {
			t.Errorf("run(%q) = %d, want 2", command, code)
		}
	}
}

func TestMemorySupersedeReplacementFileRejectsDuplicateKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replacements.json")
	raw := `[
	  {"content":"one","idempotency_key":"replacement-1","evidence":[{"state":"unavailable","unavailable_reason":"test"}]},
	  {"content":"two","idempotency_key":"replacement-1","evidence":[{"state":"unavailable","unavailable_reason":"test"}]}
	]`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMemorySupersedeReplacements(path, "supersede-1"); err == nil ||
		!strings.Contains(err.Error(), "reuses") {
		t.Fatalf("duplicate replacement key error = %v", err)
	}
	if _, err := readMemorySupersedeReplacements(path, "replacement-1"); err == nil ||
		!strings.Contains(err.Error(), "reuses") {
		t.Fatalf("operation/replacement key collision error = %v", err)
	}
}

func TestMemoryContentEncodingDefaultsAndRejectsUnknownValues(t *testing.T) {
	if got, err := normalizeMemoryContentEncoding(""); err != nil || got != "plain" {
		t.Fatalf("default encoding = %q / %v", got, err)
	}
	if got, err := normalizeMemoryContentEncoding(" base64 "); err != nil || got != "base64" {
		t.Fatalf("base64 encoding = %q / %v", got, err)
	}
	if _, err := normalizeMemoryContentEncoding("gzip"); err == nil {
		t.Fatal("unsupported content encoding was accepted")
	}
}

func TestMemoryEvidenceModesAreExclusiveAndExact(t *testing.T) {
	if _, err := memoryCaptureEvidence(memoryEvidenceFlags{Role: "supports"}); err == nil {
		t.Fatal("evidence-free capture was accepted")
	}
	if _, err := memoryCaptureEvidence(memoryEvidenceFlags{
		TranscriptID: "trn_1", FromSequence: 3, UntilSequence: 2,
	}); err == nil {
		t.Fatal("reversed transcript range was accepted")
	}
	if _, err := memoryCaptureEvidence(memoryEvidenceFlags{
		ExternalLocator: "codex://session/1", UnavailableReason: "not captured",
	}); err == nil {
		t.Fatal("two evidence states were accepted")
	}
	if _, err := memoryCaptureEvidence(memoryEvidenceFlags{
		ExternalLocator: "codex://session/1", Role: "source",
	}); err == nil {
		t.Fatal("invalid evidence role was accepted")
	}
	evidence, err := memoryCaptureEvidence(memoryEvidenceFlags{ExternalLocator: "codex://session/1", Role: "supports"})
	if err != nil || len(evidence) != 1 || evidence[0].State != "pending" {
		t.Fatalf("pending evidence = %#v / %v", evidence, err)
	}
}

func TestMemoryEvidenceFileRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown":  `[{"state":"unavailable","unavailable_reason":"test","typo":true}]`,
		"trailing": `[{"state":"unavailable","unavailable_reason":"test"}] {}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evidence.json")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := memoryCaptureEvidence(memoryEvidenceFlags{File: path}); err == nil {
				t.Fatalf("hostile evidence file %q was accepted", raw)
			}
		})
	}
}

func memoryFixture(id string, version int64, state, operation string) client.Memory {
	return client.Memory{
		ID: id, Version: version, State: state, Operation: operation,
		Kind: "decision", Content: "we chose PostgreSQL", ContentEncoding: "plain",
		Tags:              []string{"architecture"},
		SupersessionSetID: "mset_receipt", SupersessionSetRevision: 2,
		SupersessionReplacementCount: 3, SupersessionReplacementDigest: strings.Repeat("c", 64),
		ActiveSupersessionSetID: "mset_active", ActiveSupersessionSetRevision: 4,
		Links: []string{}, Evidence: []client.MemoryEvidence{},
	}
}

func writeMemoryMutation(t *testing.T, w http.ResponseWriter, id string, version int64, state, operation, idempotencyKey string) {
	t.Helper()
	_ = json.NewEncoder(w).Encode(client.MemoryMutationResult{
		Memory: memoryFixture(id, version, state, operation),
		Receipt: client.MemoryMutationReceipt{
			Operation: operation, IdempotencyKey: idempotencyKey,
			MemoryID: id, Version: version,
		},
	})
}
