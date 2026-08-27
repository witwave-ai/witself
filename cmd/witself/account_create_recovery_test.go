package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
)

const (
	accountCreateTestAccountID     = "acc_abcdefghijklmnop"
	accountCreateTestOperatorID    = "opr_abcdefghijklmnop"
	accountCreateTestOperatorToken = "witself_opr_accountCreateRecovery"
)

func TestAccountCreateResumesAmbiguousProvisionWithSameID(t *testing.T) {
	home := privateAccountCreateTestHome(t)
	var mu sync.Mutex
	var provisionIDs []string
	var createCalls int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/v1/accounts":
			var body struct {
				ProvisionID string `json:"provision_id"`
				Email       string `json:"email"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			createCalls++
			call := createCalls
			provisionIDs = append(provisionIDs, body.ProvisionID)
			mu.Unlock()
			if call <= 3 {
				writer.WriteHeader(http.StatusBadGateway)
				_, _ = writer.Write([]byte(`{"error":"ambiguous result"}`))
				return
			}
			writeAccountCreateTestResponse(
				writer, server.URL, body.ProvisionID, body.Email, call,
			)
		case "/v1/auth/bootstrap":
			writeAccountCreateBootstrapResponse(writer)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	args := accountCreateTestArgs(server.URL, "invite-private")
	if code := accountCreate(args); code != 1 {
		t.Fatalf("first account create exit = %d, want 1", code)
	}
	path, err := local.AccountProvisionJournalPath("default")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"owner@example.com", "invite-private", "Owner Display", server.URL,
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("pending journal contains %q", forbidden)
		}
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %v, err = %v", info.Mode(), err)
	}

	if code := accountCreate(args); code != 0 {
		t.Fatalf("resumed account create exit = %d, want 0", code)
	}
	if len(provisionIDs) != 4 || provisionIDs[0] == "" {
		t.Fatalf("provision ids = %#v", provisionIDs)
	}
	for _, provisionID := range provisionIDs[1:] {
		if provisionID != provisionIDs[0] {
			t.Fatalf("provision ids changed across restart: %#v", provisionIDs)
		}
	}
	assertAccountCreateSaved(t, home)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal survived successful save: %v", err)
	}
}

func TestAccountCreateChallengeResumesSameProvision(t *testing.T) {
	home := privateAccountCreateTestHome(t)
	var mu sync.Mutex
	var provisionIDs []string
	var tokens []*string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/v1/accounts":
			var body struct {
				ProvisionID    string  `json:"provision_id"`
				Email          string  `json:"email"`
				TurnstileToken *string `json:"turnstile_token"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			provisionIDs = append(provisionIDs, body.ProvisionID)
			tokens = append(tokens, body.TurnstileToken)
			call := len(provisionIDs)
			mu.Unlock()
			if body.TurnstileToken == nil {
				writer.WriteHeader(http.StatusForbidden)
				_, _ = writer.Write([]byte(
					`{"schema_version":"witself.v0",` +
						`"error":"turnstile challenge required",` +
						`"challenge_url":"` + server.URL + `/signup/challenge"}`,
				))
				return
			}
			writeAccountCreateTestResponse(
				writer, server.URL, body.ProvisionID, body.Email, call,
			)
		case "/v1/auth/bootstrap":
			writeAccountCreateBootstrapResponse(writer)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	args := accountCreateTestArgs(server.URL, "invite-private")
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return accountCreate(args)
	})
	if code != 1 || stdout != "" ||
		!strings.Contains(stderr, "witself: turnstile challenge required") ||
		!strings.Contains(
			stderr,
			"open "+server.URL+"/signup/challenge, complete the check, re-run with --challenge <token>",
		) {
		t.Fatalf("challenge run = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	journalPath, err := local.AccountProvisionJournalPath("default")
	if err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(journal), "challenge") {
		t.Fatalf("pending journal contains challenge material: %s", journal)
	}

	args = append(args, "--challenge", "challenge-response")
	stdout, stderr, code = captureFactDeleteCLI(t, func() int {
		return accountCreate(args)
	})
	if code != 0 {
		t.Fatalf("challenge retry = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	mu.Lock()
	gotProvisionIDs := append([]string(nil), provisionIDs...)
	gotTokens := append([]*string(nil), tokens...)
	mu.Unlock()
	if len(gotProvisionIDs) != 2 || gotProvisionIDs[0] == "" ||
		gotProvisionIDs[1] != gotProvisionIDs[0] {
		t.Fatalf("provision ids = %#v", gotProvisionIDs)
	}
	if len(gotTokens) != 2 || gotTokens[0] != nil || gotTokens[1] == nil ||
		*gotTokens[1] != "challenge-response" {
		t.Fatalf("turnstile tokens = %#v", gotTokens)
	}
	assertAccountCreateSaved(t, home)
}

func TestAccountCreateResumesAfterProvisionBeforeLogin(t *testing.T) {
	home := privateAccountCreateTestHome(t)
	var mu sync.Mutex
	var provisionIDs []string
	var bootstrapCalls int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/v1/accounts":
			var body struct {
				ProvisionID string `json:"provision_id"`
				Email       string `json:"email"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			provisionIDs = append(provisionIDs, body.ProvisionID)
			call := len(provisionIDs)
			mu.Unlock()
			writeAccountCreateTestResponse(
				writer, server.URL, body.ProvisionID, body.Email, call,
			)
		case "/v1/auth/bootstrap":
			mu.Lock()
			bootstrapCalls++
			call := bootstrapCalls
			mu.Unlock()
			if call == 1 {
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeAccountCreateBootstrapResponse(writer)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	args := accountCreateTestArgs(server.URL, "invite-private")
	if code := accountCreate(args); code != 1 {
		t.Fatalf("pre-login account create exit = %d, want 1", code)
	}
	record, err := local.ReadAccountProvisionJournal("default")
	if err != nil {
		t.Fatal(err)
	}
	if record.AccountID != "" || record.OperatorToken != "" {
		t.Fatalf("pre-login journal retained credential: %+v", record)
	}
	if code := accountCreate(args); code != 0 {
		t.Fatalf("resumed pre-login account create exit = %d, want 0", code)
	}
	if len(provisionIDs) != 2 || provisionIDs[0] != provisionIDs[1] {
		t.Fatalf("provision ids = %#v", provisionIDs)
	}
	assertAccountCreateSaved(t, home)
}

func TestAccountCreateResumesCredentialBeforeLocalSave(t *testing.T) {
	home := privateAccountCreateTestHome(t)
	var mu sync.Mutex
	var createCalls, bootstrapCalls int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/v1/accounts":
			var body struct {
				ProvisionID string `json:"provision_id"`
				Email       string `json:"email"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			createCalls++
			call := createCalls
			mu.Unlock()
			writeAccountCreateTestResponse(
				writer, server.URL, body.ProvisionID, body.Email, call,
			)
		case "/v1/auth/bootstrap":
			mu.Lock()
			bootstrapCalls++
			mu.Unlock()
			// Block the durable local token directory only after the remote
			// bootstrap succeeds. The private journal credential write uses a
			// disjoint path and must survive this local-save failure.
			if err := os.WriteFile(
				filepath.Join(home, "tokens"), []byte("blocked"), 0o600,
			); err != nil {
				t.Error(err)
			}
			writeAccountCreateBootstrapResponse(writer)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	args := accountCreateTestArgs(server.URL, "invite-private")
	if code := accountCreate(args); code != 1 {
		t.Fatalf("pre-save account create exit = %d, want 1", code)
	}
	record, err := local.ReadAccountProvisionJournal("default")
	if err != nil {
		t.Fatal(err)
	}
	if record.AccountID != accountCreateTestAccountID ||
		record.OperatorToken != accountCreateTestOperatorToken {
		t.Fatalf("credential handoff journal = %+v", record)
	}
	if err := os.Remove(filepath.Join(home, "tokens")); err != nil {
		t.Fatal(err)
	}
	if code := accountCreate(args); code != 0 {
		t.Fatalf("resumed pre-save account create exit = %d, want 0", code)
	}
	mu.Lock()
	gotCreateCalls, gotBootstrapCalls := createCalls, bootstrapCalls
	mu.Unlock()
	if gotCreateCalls != 1 || gotBootstrapCalls != 1 {
		t.Fatalf(
			"credential resume repeated remote calls: create=%d bootstrap=%d",
			gotCreateCalls, gotBootstrapCalls,
		)
	}
	assertAccountCreateSaved(t, home)
}

func TestAccountCreateRejectsConflictingPendingRequest(t *testing.T) {
	privateAccountCreateTestHome(t)
	var mu sync.Mutex
	var provisionIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/v1/accounts" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			ProvisionID string `json:"provision_id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		provisionIDs = append(provisionIDs, body.ProvisionID)
		mu.Unlock()
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	if code := accountCreate(accountCreateTestArgs(
		server.URL, "invite-private",
	)); code != 1 {
		t.Fatalf("first account create exit = %d, want 1", code)
	}
	if code := accountCreate(accountCreateTestArgs(
		server.URL, "different-invite",
	)); code != 1 {
		t.Fatalf("conflicting account create exit = %d, want 1", code)
	}
	mu.Lock()
	callCount := len(provisionIDs)
	mu.Unlock()
	if callCount != 3 {
		t.Fatalf("conflicting retry made remote calls: %#v", provisionIDs)
	}
}

func TestAccountCreateCleansJournalAfterCompletedLocalSave(t *testing.T) {
	home := privateAccountCreateTestHome(t)
	var remoteCalls int
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		remoteCalls++
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	args := accountCreateTestArgs(server.URL, "invite-private")
	fingerprint, err := client.AccountCreateRequestFingerprint(
		server.URL, "default", "owner@example.com",
		"invite-private", "Owner Display", "", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := local.BeginAccountProvisionJournal("default", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SaveAccountProvisionCredential(
		"default", fingerprint, record.ProvisionID,
		accountCreateTestAccountID, accountCreateTestOperatorToken,
	); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveProvisionedAccountDurable(
		"default",
		local.Account{
			ID: accountCreateTestAccountID, Email: "owner@example.com",
		},
		accountCreateTestOperatorToken,
	); err != nil {
		t.Fatal(err)
	}

	if code := accountCreate(args); code != 0 {
		t.Fatalf("completed-journal cleanup exit = %d, want 0", code)
	}
	if remoteCalls != 0 {
		t.Fatalf("completed journal made %d remote calls", remoteCalls)
	}
	assertAccountCreateSaved(t, home)
	path, _ := local.AccountProvisionJournalPath("default")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed journal survived cleanup: %v", err)
	}
}

func accountCreateTestArgs(endpoint, invite string) []string {
	return []string{
		"--email", "owner@example.com",
		"--invite", invite,
		"--display-name", "Owner Display",
		"--endpoint", endpoint,
	}
}

func privateAccountCreateTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_ACCOUNT", "")
	return home
}

func writeAccountCreateTestResponse(
	writer http.ResponseWriter,
	cellEndpoint, provisionID, email string,
	sequence int,
) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"schema_version":  "witself.v0",
		"provision_id":    provisionID,
		"replayed":        sequence > 1,
		"account_id":      accountCreateTestAccountID,
		"operator_id":     accountCreateTestOperatorID,
		"email":           email,
		"status":          "pending",
		"bootstrap_token": "witself_boot_accountCreateRecovery",
		"cell": map[string]string{
			"name":     "civo-dev-usw1-1",
			"endpoint": cellEndpoint,
		},
	})
}

func writeAccountCreateBootstrapResponse(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]string{
		"operator_id":    accountCreateTestOperatorID,
		"operator_token": accountCreateTestOperatorToken,
	})
}

func assertAccountCreateSaved(t *testing.T, home string) {
	t.Helper()
	name, account, operatorToken, err := local.Resolve("default")
	if err != nil {
		t.Fatal(err)
	}
	if name != "default" || account.ID != accountCreateTestAccountID ||
		account.Email != "owner@example.com" ||
		operatorToken != accountCreateTestOperatorToken {
		t.Fatalf(
			"saved local account = %q %+v %q",
			name, account, operatorToken,
		)
	}
	for _, path := range []string{
		filepath.Join(home, "config.json"),
		filepath.Join(home, "tokens", "accounts", "default", "owner.token"),
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v", path, info.Mode())
		}
	}
}
