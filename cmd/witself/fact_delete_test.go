package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
)

const testFactCandidateRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestFactDeleteCLIRequiresConfirmationBeforeNetwork(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer srv.Close()
	tokenFile := factDeleteTokenFile(t)

	_, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"fact", "delete", "--endpoint", srv.URL, "--token-file", tokenFile, "preferences/editor"})
	})
	if code != 2 || requests != 0 {
		t.Fatalf("code/requests = %d/%d, want 2/0", code, requests)
	}
	if !strings.Contains(stderr, "permanent") || !strings.Contains(stderr, "cannot be undone") || !strings.Contains(stderr, "--yes") {
		t.Fatalf("confirmation warning = %q", stderr)
	}
}

func TestFactDeleteCLIPreviewsAndAppliesWithoutPrintingFactContent(t *testing.T) {
	const secret = "DELETE_ME_SECRET_VALUE"
	applyCalls := 0
	previewCalls := 0
	getCalls := 0
	var applyKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer witself_agt_delete_test" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet:
			getCalls++
			http.Error(w, "preview must not retrieve fact content", http.StatusInternalServerError)
		case r.Method == http.MethodDelete && r.URL.Query().Get("dry_run") == "true":
			previewCalls++
			if r.URL.Query().Get("subject") != "person_spouse" || r.URL.Query().Get("predicate") != "identity/name" {
				t.Errorf("preview address query = %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"deletion":{"fact_id":"fact_spouse","subject_id":"sub_spouse","subject":"person_spouse","predicate":"identity/name","sensitive":true,"assertion_count":2,"candidate_count":1,"candidate_revision":"` + testFactCandidateRevision + `","usage_count":4,"resolved_assertion_id":"fas_current","deletion_state":"active","applied":false}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/facts/fact_spouse":
			applyCalls++
			applyKey = r.Header.Get("Idempotency-Key")
			if r.URL.Query().Get("expected_resolved_assertion_id") != "fas_current" {
				t.Errorf("delete query = %q", r.URL.RawQuery)
			}
			if r.URL.Query().Get("expected_candidate_revision") != testFactCandidateRevision {
				t.Errorf("delete candidate revision query = %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"deletion":{"fact_id":"fact_spouse","receipt_id":"fdel_spouse_1","subject_id":"sub_spouse","subject":"person_spouse","predicate":"identity/name","sensitive":true,"assertion_count":2,"candidate_count":1,"candidate_revision":"` + testFactCandidateRevision + `","usage_count":4,"resolved_assertion_id":"fas_current","deletion_state":"deleted","applied":true}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	tokenFile := factDeleteTokenFile(t)
	base := []string{"fact", "delete", "--endpoint", srv.URL, "--token-file", tokenFile, "--subject", "person_spouse"}

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run(append(append([]string{}, base...), "--dry-run", "identity/name"))
	})
	if code != 0 || stderr != "" || previewCalls != 1 || applyCalls != 0 || getCalls != 0 {
		t.Fatalf("dry-run code/output/calls = %d/%q/%q/%d/%d/%d", code, stdout, stderr, previewCalls, applyCalls, getCalls)
	}
	assertValueFreeFactDeletionOutput(t, stdout, secret, "must-not-print")
	if !strings.Contains(stdout, "would permanently delete") || !strings.Contains(stdout, "person_spouse") || !strings.Contains(stdout, "identity/name") {
		t.Fatalf("dry-run output = %q", stdout)
	}
	if !strings.Contains(stdout, "assertion=fas_current") {
		t.Fatalf("dry-run output omitted the replay assertion guard: %q", stdout)
	}
	if strings.Contains(stdout, "receipt=") {
		t.Fatalf("dry-run invented an apply receipt: %q", stdout)
	}

	stdout, stderr, code = captureFactDeleteCLI(t, func() int {
		return run(append(append([]string{}, base...), "--yes", "identity/name"))
	})
	if code != 0 || stderr != "" || previewCalls != 2 || applyCalls != 1 || getCalls != 0 {
		t.Fatalf("apply code/output/calls = %d/%q/%q/%d/%d/%d", code, stdout, stderr, previewCalls, applyCalls, getCalls)
	}
	if !strings.HasPrefix(applyKey, "fact_delete_") {
		t.Fatalf("generated idempotency key = %q", applyKey)
	}
	assertValueFreeFactDeletionOutput(t, stdout, secret, "must-not-print")
	if !strings.Contains(stdout, "permanently deleted") {
		t.Fatalf("apply output = %q", stdout)
	}
	if !strings.Contains(stdout, "receipt=fdel_spouse_1") {
		t.Fatalf("apply output omitted stable receipt id: %q", stdout)
	}
}

func TestFactDeleteAddressFailurePrintsValueFreeExactReplayCommand(t *testing.T) {
	const secret = "DELETE_ME_SECRET_VALUE"
	for _, tc := range []struct {
		name        string
		applyStatus int
		applyBody   string
	}{
		{name: "error response", applyStatus: http.StatusBadGateway, applyBody: `{"error":"upstream response was lost"}`},
		{name: "inconsistent success receipt", applyStatus: http.StatusOK, applyBody: `{"deletion":{"fact_id":"fact_spouse","deletion_state":"active","applied":false}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var applyKey string
			previewCalls, applyCalls := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodDelete && r.URL.Path == "/v1/facts" && r.URL.Query().Get("dry_run") == "true" {
					previewCalls++
					_, _ = w.Write([]byte(`{"deletion":{"fact_id":"fact_spouse","subject_id":"sub_spouse","subject":"person_spouse","predicate":"identity/name","value":"` + secret + `","source_ref":"private-evidence","sensitive":true,"assertion_count":2,"candidate_count":1,"candidate_revision":"` + testFactCandidateRevision + `","usage_count":4,"resolved_assertion_id":"fas_current","deletion_state":"active","applied":false}}`))
					return
				}
				if r.Method == http.MethodDelete && r.URL.Path == "/v1/facts/fact_spouse" {
					applyCalls++
					applyKey = r.Header.Get("Idempotency-Key")
					w.WriteHeader(tc.applyStatus)
					_, _ = w.Write([]byte(tc.applyBody))
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			tokenFile := factDeleteTokenFile(t)

			stdout, stderr, code := captureFactDeleteCLI(t, func() int {
				return run([]string{
					"fact", "delete", "--endpoint", srv.URL, "--token-file", tokenFile,
					"--subject", "person_spouse", "--yes", "identity/name",
				})
			})
			if code != 1 || stdout != "" || previewCalls != 1 || applyCalls != 1 {
				t.Fatalf("code/output/calls = %d/%q/%q/%d/%d", code, stdout, stderr, previewCalls, applyCalls)
			}
			if !strings.HasPrefix(applyKey, "fact_delete_") {
				t.Fatalf("generated idempotency key = %q", applyKey)
			}
			exactCommand := factDeletionReplayCommand(
				"", "", "", srv.URL, tokenFile,
				"fact_spouse", "fas_current", testFactCandidateRevision, applyKey,
			)
			if !strings.Contains(stderr, exactCommand) {
				t.Fatalf("failure omitted exact replay command %q: %q", exactCommand, stderr)
			}
			for _, want := range []string{"--fact-id", "--expected-assertion-id", "--expected-candidate-revision", "--idempotency-key", applyKey} {
				if !strings.Contains(stderr, want) {
					t.Errorf("replay output omitted %q: %q", want, stderr)
				}
			}
			assertValueFreeFactDeletionOutput(t, stderr, secret, "person_spouse", "identity/name")
		})
	}
}

func TestFactDeleteExactReplayRejectsInvalidInputBeforeNetwork(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer srv.Close()
	connection := []string{"--endpoint", srv.URL, "--token-file", factDeleteTokenFile(t)}
	tests := []struct {
		name string
		args []string
	}{
		{name: "requires yes", args: []string{"--fact-id", "fact_1", "--expected-assertion-id", "fas_1", "--expected-candidate-revision", testFactCandidateRevision, "--idempotency-key", "delete-1"}},
		{name: "rejects dry run", args: []string{"--yes", "--dry-run", "--fact-id", "fact_1", "--expected-assertion-id", "fas_1", "--expected-candidate-revision", testFactCandidateRevision, "--idempotency-key", "delete-1"}},
		{name: "rejects predicate", args: []string{"--yes", "--fact-id", "fact_1", "--expected-assertion-id", "fas_1", "--expected-candidate-revision", testFactCandidateRevision, "--idempotency-key", "delete-1", "identity/name"}},
		{name: "rejects subject", args: []string{"--yes", "--subject", "self", "--fact-id", "fact_1", "--expected-assertion-id", "fas_1", "--expected-candidate-revision", testFactCandidateRevision, "--idempotency-key", "delete-1"}},
		{name: "requires fact id", args: []string{"--yes", "--expected-assertion-id", "fas_1", "--expected-candidate-revision", testFactCandidateRevision, "--idempotency-key", "delete-1"}},
		{name: "requires assertion", args: []string{"--yes", "--fact-id", "fact_1", "--expected-candidate-revision", testFactCandidateRevision, "--idempotency-key", "delete-1"}},
		{name: "requires candidate revision", args: []string{"--yes", "--fact-id", "fact_1", "--expected-assertion-id", "fas_1", "--idempotency-key", "delete-1"}},
		{name: "rejects short candidate revision", args: []string{"--yes", "--fact-id", "fact_1", "--expected-assertion-id", "fas_1", "--expected-candidate-revision", "abc123", "--idempotency-key", "delete-1"}},
		{name: "rejects uppercase candidate revision", args: []string{"--yes", "--fact-id", "fact_1", "--expected-assertion-id", "fas_1", "--expected-candidate-revision", strings.Repeat("A", 64), "--idempotency-key", "delete-1"}},
		{name: "requires retry key", args: []string{"--yes", "--fact-id", "fact_1", "--expected-assertion-id", "fas_1", "--expected-candidate-revision", testFactCandidateRevision}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"fact", "delete"}, connection...)
			args = append(args, tc.args...)
			_, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
			if code != 2 || stderr == "" {
				t.Fatalf("code/stderr = %d/%q, want input rejection", code, stderr)
			}
			if requests != 0 {
				t.Fatalf("invalid exact replay made %d network request(s)", requests)
			}
		})
	}
}

func TestFactDeleteExactReplayReturnsStableReceipt(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/facts/fact_1" {
			t.Errorf("request = %s %s", r.Method, r.URL.RequestURI())
		}
		if r.URL.Query().Get("expected_resolved_assertion_id") != "fas_1" || r.URL.Query().Get("dry_run") != "" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		if r.URL.Query().Get("expected_candidate_revision") != testFactCandidateRevision {
			t.Errorf("candidate revision query = %q", r.URL.RawQuery)
		}
		if r.Header.Get("Idempotency-Key") != "delete-stable-1" {
			t.Errorf("idempotency key = %q", r.Header.Get("Idempotency-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"deletion": map[string]any{
			"fact_id": "fact_1", "receipt_id": "fdel_stable_1", "subject_id": "sub_1",
			"subject": "person_spouse", "predicate": "identity/name", "sensitive": true,
			"assertion_count": 2, "candidate_count": 1, "usage_count": 4,
			"candidate_revision":    testFactCandidateRevision,
			"resolved_assertion_id": "fas_1", "deletion_state": "deleted",
			"applied": true, "replayed": calls > 1,
		}})
	}))
	defer srv.Close()
	args := []string{
		"fact", "delete", "--endpoint", srv.URL, "--token-file", factDeleteTokenFile(t),
		"--yes", "--fact-id", "fact_1", "--expected-assertion-id", "fas_1", "--expected-candidate-revision", testFactCandidateRevision, "--idempotency-key", "delete-stable-1",
	}
	for i, wantReplay := range []string{"replayed=false", "replayed=true"} {
		stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
		if code != 0 || stderr != "" {
			t.Fatalf("call %d code/stderr = %d/%q", i+1, code, stderr)
		}
		for _, want := range []string{"receipt=fdel_stable_1", "fact=fact_1", "assertion=fas_1", wantReplay} {
			if !strings.Contains(stdout, want) {
				t.Errorf("call %d output omitted %q: %q", i+1, want, stdout)
			}
		}
	}
	if calls != 2 {
		t.Fatalf("exact apply/replay calls = %d, want 2", calls)
	}
}

func TestValidateAppliedFactDeletionReceiptGuardsExactReplay(t *testing.T) {
	valid := client.FactDeletionReceipt{
		FactID: "fact_1", ReceiptID: "fdel_1", ResolvedAssertionID: "fas_1",
		CandidateRevision: testFactCandidateRevision, DeletionState: "deleted", Applied: true,
	}
	if err := validateAppliedFactDeletionReceipt(valid, "fact_1", "fas_1", testFactCandidateRevision); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
	mutations := map[string]func(*client.FactDeletionReceipt){
		"missing receipt":          func(r *client.FactDeletionReceipt) { r.ReceiptID = "" },
		"wrong state":              func(r *client.FactDeletionReceipt) { r.DeletionState = "active" },
		"wrong fact":               func(r *client.FactDeletionReceipt) { r.FactID = "fact_other" },
		"wrong assertion":          func(r *client.FactDeletionReceipt) { r.ResolvedAssertionID = "fas_other" },
		"wrong candidate revision": func(r *client.FactDeletionReceipt) { r.CandidateRevision = strings.Repeat("b", 64) },
		"not applied":              func(r *client.FactDeletionReceipt) { r.Applied = false },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			receipt := valid
			mutate(&receipt)
			if err := validateAppliedFactDeletionReceipt(receipt, "fact_1", "fas_1", testFactCandidateRevision); err == nil {
				t.Fatalf("invalid exact replay receipt accepted: %#v", receipt)
			}
		})
	}
}

func TestFactDeleteUsageIncludesExactReplay(t *testing.T) {
	stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run([]string{"fact", "delete"}) })
	if code != 2 || stdout != "" {
		t.Fatalf("code/stdout = %d/%q", code, stdout)
	}
	for _, want := range []string{"--fact-id FACT_ID", "--expected-assertion-id ASSERTION_ID", "--expected-candidate-revision REVISION", "--idempotency-key KEY"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("fact delete usage omitted %q: %q", want, stderr)
		}
	}
	stdout, stderr, code = captureFactDeleteCLI(t, func() int { return run([]string{"help"}) })
	if code != 0 || stderr != "" || !strings.Contains(stdout, "--expected-assertion-id ID") || !strings.Contains(stdout, "--expected-candidate-revision REVISION") {
		t.Fatalf("global usage code/output = %d/%q/%q", code, stdout, stderr)
	}
}

func TestFactSetCLIForwardsRecreateDeleted(t *testing.T) {
	var in client.SetFactInput
	var retryKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		retryKey = r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"fact":{"id":"fact_new","subject":"self","predicate":"preferences/editor","value":"zed"}}`))
	}))
	defer srv.Close()
	_, _, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"fact", "set", "--endpoint", srv.URL, "--token-file", factDeleteTokenFile(t), "--recreate-deleted", "preferences/editor", "zed"})
	})
	if code != 0 || !in.RecreateDeleted || !strings.HasPrefix(retryKey, "fact_recreate_") {
		t.Fatalf("code/input/key = %d/%#v/%q", code, in, retryKey)
	}
}

func TestFactDeletionPreviewAllowsCanonicalizedSubjectAlias(t *testing.T) {
	receipt := client.FactDeletionReceipt{
		FactID: "fact_spouse", Subject: "person_spouse", Predicate: "identity/name",
		ResolvedAssertionID: "fas_1", CandidateRevision: testFactCandidateRevision, DeletionState: "active",
	}
	if err := validateFactDeletionPreview(receipt, "my wife", "identity/name"); err != nil {
		t.Fatalf("canonicalized subject alias was rejected: %v", err)
	}
	if err := validateMCPFactDeletionPreview(receipt, "my wife", "identity/name"); err != nil {
		t.Fatalf("MCP canonicalized subject alias was rejected: %v", err)
	}
}

func TestProviderRoutingContractsGuardPermanentFactDeletion(t *testing.T) {
	contracts := map[string]string{
		"codex":   codexMemoryRoutingInstructions,
		"claude":  claudeMemoryRoutingInstructions,
		"grok":    grokMemoryRoutingInstructions,
		"cursor":  cursorMemoryRoutingInstructions,
		"neutral": runtimeNeutralMemoryRoutingInstructions,
	}
	for name, contract := range contracts {
		t.Run(name, func(t *testing.T) {
			text := strings.ToLower(contract)
			for _, want := range []string{
				"permanent", "preview", "apply", "fact-shaped target", "correction", "forget", "ambiguous",
				"direct_user_authorized=true", "autonomous", "background", "standing", "subagent", "delegat",
				"retrieved", "untrusted", "transcript", "export", "backup", "recreat", "explicit destination wins",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("deletion routing contract does not contain %q:\n%s", want, contract)
				}
			}
			if !strings.Contains(text, "direct current-user") && !strings.Contains(text, "current user's direct") {
				t.Errorf("deletion routing contract lacks direct current-user authority:\n%s", contract)
			}
			if !strings.Contains(text, "without naming witself") && !strings.Contains(text, "witself is not named") {
				t.Errorf("deletion routing contract still requires the user to name Witself:\n%s", contract)
			}
			if !strings.Contains(text, "one live fact") && !strings.Contains(text, "one fact resolves") {
				t.Errorf("deletion routing contract lacks unique fact resolution:\n%s", contract)
			}
			if !strings.Contains(text, "same turn") && !strings.Contains(text, "same-turn") {
				t.Errorf("deletion routing contract lacks same-turn authorization:\n%s", contract)
			}
			if !strings.Contains(text, "no undo") && !strings.Contains(text, "cannot be undone") {
				t.Errorf("deletion routing contract does not explain permanence:\n%s", contract)
			}
			if !strings.Contains(text, "fall back") && !strings.Contains(text, "fallback") {
				t.Errorf("deletion routing contract does not prohibit native-memory fallback:\n%s", contract)
			}
			// Claude's complete deletion mechanics are reinforced by the tool
			// description because its server instructions have a hard 2 KiB cap.
			if name != "claude" && !strings.Contains(text, "preserved") && !strings.Contains(text, "remain") {
				t.Errorf("deletion routing contract does not preserve immutable value-free usage history:\n%s", contract)
			}
			if name != "claude" {
				for _, want := range []string{"person_spouse", "identity/name", "web", "message", "tool", "usage", "rollup"} {
					if !strings.Contains(text, want) {
						t.Errorf("full deletion routing contract does not contain %q:\n%s", want, contract)
					}
				}
			}
		})
	}
	for name, contract := range map[string]string{
		"codex": codexMemoryRoutingInstructions, "claude": claudeMemoryRoutingInstructions,
		"grok": grokMemoryRoutingInstructions, "cursor": cursorMemoryRoutingInstructions,
	} {
		if !strings.Contains(contract, "witself.fact.delete") {
			t.Errorf("%s contract does not name the permanent deletion tool", name)
		}
	}
}

func factDeleteTokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "delete.token")
	if err := os.WriteFile(path, []byte("witself_agt_delete_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func captureFactDeleteCLI(t *testing.T, fn func() int) (stdout, stderr string, code int) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code = fn()
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()
	outBytes, outReadErr := io.ReadAll(outR)
	errBytes, errReadErr := io.ReadAll(errR)
	_ = outR.Close()
	_ = errR.Close()
	if outReadErr != nil || errReadErr != nil {
		t.Fatalf("read captured output: stdout=%v stderr=%v", outReadErr, errReadErr)
	}
	return string(outBytes), string(errBytes), code
}

func assertValueFreeFactDeletionOutput(t *testing.T, output string, forbidden ...string) {
	t.Helper()
	for _, value := range forbidden {
		if strings.Contains(output, value) {
			t.Errorf("deletion output leaked %q: %q", value, output)
		}
	}
	for _, field := range []string{"source_ref", "evidence", "value="} {
		if strings.Contains(strings.ToLower(output), field) {
			t.Errorf("deletion output contains forbidden content field %q: %q", field, output)
		}
	}
}
