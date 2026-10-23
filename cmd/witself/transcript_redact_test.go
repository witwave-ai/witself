package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
)

func transcriptRedactTestHome(t *testing.T) string {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "operator-credential")
	if err := os.WriteFile(path, []byte("op-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func transcriptRedactTestReceipt(applied bool, from, until int64) client.TranscriptRedactionReceipt {
	r := client.TranscriptRedactionReceipt{
		TranscriptID: "trn_test", FromSequence: from, UntilSequence: until,
		RedactionSetRevision: strings.Repeat("a", 64), EntryCount: until - from + 1, RedactableCount: until - from + 1,
		MemoryEvidenceCount: 1, EvidenceMemoryIDs: []string{"mem_test"}, CurationRunsToCancel: 2,
		PartialCaptureEvent: true, Applied: applied,
	}
	for seq := from; seq <= until; seq++ {
		r.Entries = append(r.Entries, client.TranscriptRedactionEntry{
			EntryID: fmt.Sprintf("tre_%d", seq), Sequence: seq, Role: "tool", Kind: "tool.result", Redacted: applied,
			CreatedAt: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC),
		})
	}
	if applied {
		at := time.Date(2026, 10, 22, 13, 0, 0, 0, time.UTC)
		r.ReceiptID, r.RedactedAt = "trd_"+strings.Repeat("b", 32), &at
		r.RedactedCount, r.CancelledCurationRunCount = r.EntryCount, 2
	}
	return r
}

func TestTranscriptRedactCLIUsageBeforeNetwork(t *testing.T) {
	tokenFile := transcriptRedactTestHome(t)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer srv.Close()
	cases := [][]string{
		{}, {"--dry-run"}, {"--sequence", "0", "--dry-run"}, {"--sequence", "-1", "--dry-run"},
		{"--sequence", "1", "--from-sequence", "0", "--dry-run"},
		{"--sequence", "1", "--until-sequence", "1", "--dry-run"},
		{"--from-sequence", "2", "--until-sequence", "1", "--dry-run"},
		{"--from-sequence", "1", "--until-sequence", "101", "--dry-run"},
		{"--from-sequence", "1", "--dry-run"}, {"--until-sequence", "1", "--dry-run"},
		{"--sequence", "1"}, {"--sequence", "1", "--yes"},
		{"--sequence", "1", "--dry-run", "--yes"},
		{"--sequence", "1", "--dry-run", "--idempotency-key", ""},
		{"--sequence", "1", "--dry-run", "--idempotency-key", "key"},
		{"--sequence", "1", "--dry-run", "--expected-redaction-set-revision", ""},
		{"--sequence", "1", "--yes", "--expected-redaction-set-revision", "invalid", "--idempotency-key", "key"},
		{"--sequence", "1", "--yes", "--expected-redaction-set-revision", strings.Repeat("a", 64), "--idempotency-key", " "},
		{"--sequence", "1", "--yes", "--expected-redaction-set-revision", strings.Repeat("a", 64), "--idempotency-key", strings.Repeat("k", 513)},
		{"--sequence", "1", "--dry-run", "extra"},
		{"--sequence", "1", "--expected-redaction-set-revision", strings.Repeat("a", 64), "--idempotency-key", "key"},
	}
	for i, args := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			base := []string{"transcript", "redact", "trn_test", "--endpoint", srv.URL, "--token-file", tokenFile}
			_, stderr, code := captureFactDeleteCLI(t, func() int { return run(append(base, args...)) })
			if code != 2 || requests != 0 {
				t.Fatalf("usage code=%d requests=%d; want 2 and no request", code, requests)
			}
			if i == 10 && !strings.Contains(stderr, "transcript redaction is permanent and cannot be undone; preview with --dry-run, then apply the exact guards with --yes") {
				t.Fatal("missing irreversible-action warning")
			}
		})
	}
	_, _, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"transcript", "redact", "--sequence", "1", "--dry-run", "--endpoint", srv.URL, "--token-file", tokenFile})
	})
	if code != 2 || requests != 0 {
		t.Fatal("missing transcript id did not fail before network")
	}
}

func TestTranscriptRedactCLIUpperBound100(t *testing.T) {
	tokenFile := transcriptRedactTestHome(t)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			FromSequence  int64 `json:"from_sequence"`
			UntilSequence int64 `json:"until_sequence"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.FromSequence != 1 || body.UntilSequence != 100 || r.URL.RawQuery != "dry_run=true" {
			t.Error("100-entry preview changed the requested range or mode")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"redaction": transcriptRedactTestReceipt(false, 1, 100)})
	}))
	defer srv.Close()
	_, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"transcript", "redact", "trn_test", "--from-sequence", "1", "--until-sequence", "100", "--dry-run", "--endpoint", srv.URL, "--token-file", tokenFile})
	})
	if code != 0 || stderr != "" || requests != 1 {
		t.Fatalf("100-entry preview code=%d requests=%d; want 0 and exactly one request", code, requests)
	}
}

func TestTranscriptRedactCLIWireOutputAndReplayCommand(t *testing.T) {
	tokenFile := transcriptRedactTestHome(t)
	const canary = "WITSELF-CANARY-160C-PRIVATE-CONTENT"
	previews, applies := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/transcripts/trn_test/entries:redact" {
			t.Error("incorrect redaction method/path")
		}
		if r.Header.Get("Authorization") != "Bearer op-token" {
			t.Error("request did not use explicit operator token")
		}
		if _, ok := r.Header["X-Witself-Direct-User-Authorized"]; ok {
			t.Error("redaction sent a direct-user header")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		preview := r.URL.Query().Get("dry_run") == "true"
		want := map[string]any{"from_sequence": float64(3), "until_sequence": float64(3)}
		if preview {
			previews++
			_, hasKey := r.Header["Idempotency-Key"]
			if r.URL.RawQuery != "dry_run=true" || hasKey {
				t.Error("preview must have dry_run=true and no idempotency key")
			}
		} else {
			applies++
			if r.URL.RawQuery != "" || r.Header.Get("Idempotency-Key") != "fresh-key" {
				t.Error("apply did not send the expected idempotency key on the wire")
			}
			want["expected_redaction_set_revision"] = strings.Repeat("a", 64)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil || !reflect.DeepEqual(got, want) {
			t.Error("incorrect redaction request body")
		}
		wire, err := json.Marshal(transcriptRedactTestReceipt(!preview, 3, 3))
		if err != nil {
			t.Error(err)
		}
		var receipt map[string]any
		if err := json.Unmarshal(wire, &receipt); err != nil {
			t.Error(err)
		}
		receipt["body"] = canary
		receipt["entries"].([]any)[0].(map[string]any)["body"] = canary
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "redaction": receipt, "raw": canary})
	}))
	defer srv.Close()
	base := []string{"transcript", "redact", "trn_test", "--sequence", "3", "--endpoint", srv.URL, "--token-file", tokenFile}
	stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(append(base, "--dry-run")) })
	if code != 0 || stderr != "" || previews != 1 || applies != 0 {
		t.Fatalf("preview code=%d previews=%d applies=%d", code, previews, applies)
	}
	for _, forbidden := range []string{canary, "op-token", tokenFile, "body", "raw"} {
		if strings.Contains(stdout, forbidden) {
			t.Error("preview output contained content or credential material")
		}
	}
	for _, field := range []string{"range\t3-3", "curation runs to cancel\t2", "partial capture event\tyes", "evidence memory\tmem_test", "seq=3\trole=tool\tkind=tool.result", "<operator token file>", "<fresh key>"} {
		if !strings.Contains(stdout, field) {
			t.Errorf("preview missing %q", field)
		}
	}
	_, command, found := strings.Cut(stdout, "apply command:\n  ")
	if !found {
		t.Fatal("preview omitted apply command")
	}
	args, err := splitGrokCommand(strings.TrimSpace(command))
	if err != nil {
		t.Fatal("apply command was not shell-parseable")
	}
	for i, arg := range args {
		switch arg {
		case "<operator token file>":
			args[i] = tokenFile
		case "<fresh key>":
			args[i] = "  fresh-key  "
		}
	}
	stdout, stderr, code = captureFactDeleteCLI(t, func() int { return run(args[1:]) })
	if code != 0 || stderr != "" || applies != 1 || !strings.Contains(stdout, "cancelled curation runs\t2") || strings.Contains(stdout, canary) {
		t.Fatalf("printed apply command code=%d applies=%d; invalid output=%t", code, applies, strings.Contains(stdout, canary))
	}
	for _, applied := range []bool{false, true} {
		args := append([]string{}, base...)
		if applied {
			args = append(args, "--yes", "--expected-redaction-set-revision", strings.Repeat("a", 64), "--idempotency-key", " fresh-key ")
		} else {
			args = append(args, "--dry-run")
		}
		args = append(args, "--json")
		stdout, stderr, code = captureFactDeleteCLI(t, func() int { return run(args) })
		var decoded struct {
			Redaction client.TranscriptRedactionReceipt `json:"redaction"`
		}
		if code != 0 || stderr != "" || strings.Contains(stdout, canary) || json.Unmarshal([]byte(stdout), &decoded) != nil || !reflect.DeepEqual(decoded.Redaction, transcriptRedactTestReceipt(applied, 3, 3)) {
			t.Fatalf("JSON receipt did not round-trip; applied=%t code=%d", applied, code)
		}
	}
}

func TestTranscriptRedactCLILocalOwnerToken(t *testing.T) {
	transcriptRedactTestHome(t)
	if err := local.Save("acct", local.Account{ID: "acc_x"}, "op-token"); err != nil {
		t.Fatal(err)
	}
	agentPath, err := local.AgentTokenPath("acct", "default", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(agentPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentPath, []byte("agent-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITSELF_AGENT", "a1")
	t.Setenv("WITSELF_TOKEN_FILE", agentPath)
	t.Setenv("WITSELF_TOKEN", "agent-token")
	t.Setenv("WITSELF_ENDPOINT", "http://invalid.invalid")
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer op-token" {
			t.Error("local account did not supply owner operator bearer")
		}
		_, hasKey := r.Header["Idempotency-Key"]
		_, hasDirectUser := r.Header["X-Witself-Direct-User-Authorized"]
		if r.URL.Query().Get("dry_run") != "true" || hasKey || hasDirectUser {
			t.Error("local owner preview sent mutation guards")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"redaction": transcriptRedactTestReceipt(false, 1, 1)})
	}))
	defer srv.Close()
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"transcript", "redact", "trn_test", "--sequence", "1", "--dry-run", "--account", "acct", "--endpoint", srv.URL})
	})
	if code != 0 || stderr != "" || requests != 1 {
		t.Fatalf("local owner preview code=%d requests=%d", code, requests)
	}
	if !strings.Contains(stdout, "--account 'acct'") || !strings.Contains(stdout, "--endpoint '"+srv.URL+"'") || strings.Contains(stdout, "--token-file") {
		t.Error("apply command lost local connection selectors")
	}
}

func TestTranscriptRedactCLIRefusesInconsistentReceipt(t *testing.T) {
	tokenFile := transcriptRedactTestHome(t)
	for _, applied := range []bool{false, true} {
		for _, jsonOut := range []bool{false, true} {
			t.Run(fmt.Sprintf("applied=%t/json=%t", applied, jsonOut), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					r := transcriptRedactTestReceipt(applied, 1, 1)
					r.EntryCount++
					_ = json.NewEncoder(w).Encode(map[string]any{"redaction": r})
				}))
				defer srv.Close()
				args := []string{"transcript", "redact", "trn_test", "--sequence", "1", "--endpoint", srv.URL, "--token-file", tokenFile}
				if applied {
					args = append(args, "--yes", "--expected-redaction-set-revision", strings.Repeat("a", 64), "--idempotency-key", "key")
				} else {
					args = append(args, "--dry-run")
				}
				if jsonOut {
					args = append(args, "--json")
				}
				stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
				if code != 1 || stdout != "" {
					t.Errorf("inconsistent receipt accepted; code=%d", code)
				}
				if applied {
					for _, text := range []string{"server accepted the request", "redaction may already be applied", "rerun with --dry-run", "SAME --idempotency-key to replay the receipt"} {
						if !strings.Contains(stderr, text) {
							t.Errorf("invalid apply receipt omitted recovery guidance %q", text)
						}
					}
				} else if strings.Contains(stderr, "server accepted the request") {
					t.Error("invalid preview receipt must not imply a redaction was applied")
				}
			})
		}
	}
}

func TestTranscriptRedactCLIConnectionErrorHasNoRedactionHint(t *testing.T) {
	transcriptRedactTestHome(t)
	if err := local.Save("acct", local.Account{ID: "acc_x"}, "op-token"); err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			requests := 0
			originalTransport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = originalTransport })
			http.DefaultTransport = accountCreateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != http.MethodGet || r.URL.Path != "/v1/directory/acc_x" {
					t.Error("expected only the account endpoint lookup")
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})
			stdout, stderr, code := captureFactDeleteCLI(t, func() int {
				return run([]string{"transcript", "redact", "trn_test", "--sequence", "1", "--dry-run", "--account", "acct"})
			})
			if code != 1 || stdout != "" || requests != 1 || !strings.HasPrefix(stderr, "witself: locate account") {
				t.Errorf("connection error code=%d requests=%d; expected ordinary connection error and empty stdout", code, requests)
			}
			for _, hint := range []string{"transcript redaction", "features.transcript_redaction", "operator token", "--dry-run", "--idempotency-key"} {
				if strings.Contains(stderr, hint) {
					t.Errorf("connection error included redaction-specific hint %q", hint)
				}
			}
		})
	}
}

func TestTranscriptRedactCLINothingToRedact(t *testing.T) {
	tokenFile := transcriptRedactTestHome(t)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		r := transcriptRedactTestReceipt(false, 1, 2)
		r.RedactableCount, r.AlreadyRedactedCount = 0, 2
		for i := range r.Entries {
			r.Entries[i].Redacted = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"redaction": r})
	}))
	defer srv.Close()
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"transcript", "redact", "trn_test", "--from-sequence", "1", "--until-sequence", "2", "--dry-run", "--endpoint", srv.URL, "--token-file", tokenFile})
	})
	if code != 0 || stderr != "" || requests != 1 || !strings.Contains(stdout, "nothing to redact (every entry in the range is already redacted)") {
		t.Fatalf("all-redacted preview code=%d requests=%d; expected nothing-to-redact message", code, requests)
	}
	if strings.Contains(stdout, "apply command:") || strings.Contains(stdout, "witself transcript redact") {
		t.Error("all-redacted preview offered an apply command")
	}
}

func TestTranscriptRedactCLIErrorHints(t *testing.T) {
	tokenFile := transcriptRedactTestHome(t)
	for _, tc := range []struct {
		status int
		hint   string
	}{
		{403, "(transcript redaction needs the account owner's operator token, not an agent token)"},
		{404, "(or this server does not support transcript redaction; check features.transcript_redaction)"},
		{400, "bad request"}, {409, "conflict"}, {410, "request failed"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
			defer srv.Close()
			stdout, stderr, code := captureFactDeleteCLI(t, func() int {
				return run([]string{"transcript", "redact", "trn_test", "--sequence", "1", "--dry-run", "--endpoint", srv.URL, "--token-file", tokenFile})
			})
			if code != 1 || stdout != "" || !strings.Contains(stderr, tc.hint) {
				t.Errorf("status %d missing expected error hint (code=%d)", tc.status, code)
			}
		})
	}
}

func TestTranscriptRedactionReceiptValidation(t *testing.T) {
	for _, applied := range []bool{false, true} {
		mutations := map[string]func(*client.TranscriptRedactionReceipt){
			"id":              func(r *client.TranscriptRedactionReceipt) { r.TranscriptID = "other" },
			"range":           func(r *client.TranscriptRedactionReceipt) { r.UntilSequence++ },
			"revision":        func(r *client.TranscriptRedactionReceipt) { r.RedactionSetRevision = "bad" },
			"entry count":     func(r *client.TranscriptRedactionReceipt) { r.EntryCount++ },
			"entry list":      func(r *client.TranscriptRedactionReceipt) { r.Entries = nil },
			"count sum":       func(r *client.TranscriptRedactionReceipt) { r.AlreadyRedactedCount++ },
			"sequence gap":    func(r *client.TranscriptRedactionReceipt) { r.Entries[1].Sequence++ },
			"negative impact": func(r *client.TranscriptRedactionReceipt) { r.CurationRunsToCancel = -1 },
			"applied":         func(r *client.TranscriptRedactionReceipt) { r.Applied = !r.Applied },
		}
		if applied {
			mutations["receipt id"] = func(r *client.TranscriptRedactionReceipt) { r.ReceiptID = "invalid" }
			mutations["time"] = func(r *client.TranscriptRedactionReceipt) { r.RedactedAt = nil }
			mutations["redacted count"] = func(r *client.TranscriptRedactionReceipt) { r.RedactedCount = 0 }
			mutations["redacted redactable mismatch"] = func(r *client.TranscriptRedactionReceipt) { r.RedactedCount-- }
			mutations["unredacted entry"] = func(r *client.TranscriptRedactionReceipt) { r.Entries[0].Redacted = false }
			mutations["changed revision"] = func(r *client.TranscriptRedactionReceipt) { r.RedactionSetRevision = strings.Repeat("b", 64) }
		} else {
			mutations["receipt id"] = func(r *client.TranscriptRedactionReceipt) { r.ReceiptID = "trd_" + strings.Repeat("b", 32) }
			mutations["time"] = func(r *client.TranscriptRedactionReceipt) { at := time.Now(); r.RedactedAt = &at }
			mutations["cancelled runs"] = func(r *client.TranscriptRedactionReceipt) { r.CancelledCurationRunCount = 1 }
			mutations["entry state"] = func(r *client.TranscriptRedactionReceipt) { r.Entries[0].Redacted = true }
			mutations["replayed"] = func(r *client.TranscriptRedactionReceipt) { r.Replayed = true }
		}
		validate := func(r client.TranscriptRedactionReceipt) error {
			if applied {
				return validateTranscriptRedactionApply(r, "trn_test", 1, 2, strings.Repeat("a", 64))
			}
			return validateTranscriptRedactionPreview(r, "trn_test", 1, 2)
		}
		if err := validate(transcriptRedactTestReceipt(applied, 1, 2)); err != nil {
			t.Fatal("valid receipt rejected")
		}
		t.Run(fmt.Sprintf("applied=%t/100 entries", applied), func(t *testing.T) {
			r := transcriptRedactTestReceipt(applied, 1, 100)
			var err error
			if applied {
				err = validateTranscriptRedactionApply(r, "trn_test", 1, 100, strings.Repeat("a", 64))
			} else {
				err = validateTranscriptRedactionPreview(r, "trn_test", 1, 100)
			}
			if err != nil {
				t.Fatal("valid 100-entry receipt rejected")
			}
		})
		mixed := transcriptRedactTestReceipt(applied, 1, 2)
		mixed.RedactableCount, mixed.AlreadyRedactedCount = 1, 1
		mixed.Entries[0].Redacted = true
		if applied {
			mixed.RedactedCount = 1
		}
		if err := validate(mixed); err != nil {
			t.Fatal("valid mixed redaction receipt rejected")
		}
		if applied {
			mixed.Replayed = true
			mixed.CancelledCurationRunCount = 0
			if err := validate(mixed); err != nil {
				t.Fatal("valid redaction replay rejected")
			}
		}
		for name, mutate := range mutations {
			t.Run(fmt.Sprintf("applied=%t/%s", applied, name), func(t *testing.T) {
				r := transcriptRedactTestReceipt(applied, 1, 2)
				mutate(&r)
				if validate(r) == nil {
					t.Error("inconsistent receipt accepted")
				}
			})
		}
	}
}

func TestTranscriptRedactionApplyCommandSelectors(t *testing.T) {
	for _, tc := range []struct {
		account, endpoint, tokenFile string
		selectors                    []string
	}{
		{selectors: nil},
		{account: "acct", selectors: []string{"--account", "acct"}},
		{endpoint: "http://localhost", selectors: []string{"--endpoint", "http://localhost"}},
		{endpoint: "http://localhost", tokenFile: "private-file", selectors: []string{"--endpoint", "http://localhost", "--token-file", "<operator token file>"}},
		{account: "a'b", endpoint: "http://localhost", selectors: []string{"--account", "a'b", "--endpoint", "http://localhost"}},
	} {
		r := transcriptRedactTestReceipt(false, 1, 2)
		command := transcriptRedactionApplyCommand(r, tc.account, tc.endpoint, tc.tokenFile)
		args, err := splitGrokCommand(command)
		if err != nil {
			t.Fatal("apply command has invalid shell quoting")
		}
		want := []string{"witself", "transcript", "redact", "trn_test", "--from-sequence", "1", "--until-sequence", "2"}
		want = append(want, tc.selectors...)
		want = append(want, "--yes", "--expected-redaction-set-revision", r.RedactionSetRevision, "--idempotency-key", "<fresh key>")
		if !reflect.DeepEqual(args, want) {
			t.Error("apply command changed selectors or guards")
		}
	}
}

func TestTranscriptRedactCLIApplyUnknownOutcomeGuidance(t *testing.T) {
	tokenFile := transcriptRedactTestHome(t)
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		unknown bool
		hint    string
	}{
		{"truncated-200", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"redaction":{"transcript_id":"trn_te`)
		}, true, ""},
		{"500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, true, ""},
		{"504", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGatewayTimeout) }, true, ""},
		{"403", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }, false, "account owner's operator token"},
		{"404", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }, false, "features.transcript_redaction"},
		{"409", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusConflict) }, false, "conflict"},
		{"400", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) }, false, "bad request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			stdout, stderr, code := captureFactDeleteCLI(t, func() int {
				return run([]string{"transcript", "redact", "trn_test", "--sequence", "1", "--yes", "--expected-redaction-set-revision", strings.Repeat("a", 64), "--idempotency-key", "key", "--endpoint", srv.URL, "--token-file", tokenFile})
			})
			if code != 1 || stdout != "" {
				t.Fatalf("apply error code=%d; expected failure with empty stdout", code)
			}
			for _, text := range []string{"outcome is unknown", "may already be applied", "rerun with --dry-run", "SAME --idempotency-key to replay the receipt"} {
				if got := strings.Contains(stderr, text); got != tc.unknown {
					t.Errorf("unknown-outcome guidance %q present=%t, want %t", text, got, tc.unknown)
				}
			}
			if tc.hint != "" && !strings.Contains(stderr, tc.hint) {
				t.Errorf("definite refusal missing hint %q", tc.hint)
			}
		})
	}
}
