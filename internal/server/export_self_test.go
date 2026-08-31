package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	archiveexport "github.com/witwave-ai/witself/internal/export"
)

func TestAccountSelfExportRequiresAccountScopedOperatorCredential(t *testing.T) {
	archive := selfExportTestArchive(t, "acc_export")
	var exportedAccountID string
	auth := func(_ context.Context, token string) (string, string, string, bool, error) {
		if token != "operator-token" {
			return "", "", "", false, nil
		}
		return "opr_export", "acc_export", "active", true, nil
	}
	handler := apiMux(Config{
		Authenticate: auth,
		// The account-scoped operator credential remains sufficient when the
		// account has multiple realms; no realm-count fallback is consulted.
		ListRealms: func(context.Context, string) ([]Realm, error) {
			return []Realm{{ID: "realm_one"}, {ID: "realm_two"}}, nil
		},
		// Principal auth deliberately recognizes the agent credential. The
		// export route must use Authenticate instead and keep it realm-scoped.
		AuthenticatePrincipal: func(_ context.Context, token string) (DomainPrincipal, bool, error) {
			if token != "agent-token" {
				return DomainPrincipal{}, false, nil
			}
			return DomainPrincipal{
				Kind: PrincipalKindAgent, ID: "agent_export",
				AccountID: "acc_export", RealmID: "realm_export",
				AccountStatus: "active", AccessProfile: AccessProfileFull,
			}, true, nil
		},
		StreamAccountSelf: func(_ context.Context, accountID string, w io.Writer) error {
			exportedAccountID = accountID
			_, err := w.Write(archive)
			return err
		},
	})

	request := func(token string) *http.Response {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/export", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response.Result()
	}

	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "missing token"},
		{name: "realm-scoped agent token", token: "agent-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := request(tc.token)
			closeBody(t, resp)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
	if exportedAccountID != "" {
		t.Fatalf("export ran before operator authorization for %q", exportedAccountID)
	}

	resp := request("operator-token")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", resp.StatusCode, body)
	}
	if exportedAccountID != "acc_export" {
		t.Fatalf("export account = %q, want acc_export", exportedAccountID)
	}
	if !bytes.Equal(body, archive) {
		t.Fatal("response did not preserve archive bytes")
	}
	if got := resp.Header.Get("Content-Type"); got != "application/gzip" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := resp.Header.Get("X-Witself-Export-Format"); got != "1" {
		t.Errorf("X-Witself-Export-Format = %q", got)
	}
	if got := resp.Header.Get("X-Witself-Export-Purpose"); got != archiveexport.PurposeSelf {
		t.Errorf("X-Witself-Export-Purpose = %q", got)
	}
	disposition := resp.Header.Get("Content-Disposition")
	if !strings.HasPrefix(disposition, `attachment; filename="witself-export-acc_export-`) ||
		!strings.HasSuffix(disposition, `.tar.gz"`) {
		t.Errorf("Content-Disposition = %q", disposition)
	}
}

func TestAccountSelfExportRejectsConcurrentExportForSameAccount(t *testing.T) {
	archive := selfExportTestArchive(t, "acc_export")
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	auth := func(_ context.Context, token string) (string, string, string, bool, error) {
		return "opr_export", "acc_export", "active", token == "operator-token", nil
	}
	handler := apiMux(Config{
		Authenticate: auth,
		StreamAccountSelf: func(_ context.Context, _ string, w io.Writer) error {
			if calls.Add(1) == 1 {
				close(started)
				<-release
			}
			_, err := w.Write(archive)
			return err
		},
	})

	do := func() *http.Response {
		req := httptest.NewRequest(http.MethodGet, "/v1/export", nil)
		req.Header.Set("Authorization", "Bearer operator-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response.Result()
	}

	firstResult := make(chan *http.Response, 1)
	go func() {
		firstResult <- do()
	}()
	<-started

	second := do()
	var refusal map[string]any
	if err := json.NewDecoder(second.Body).Decode(&refusal); err != nil {
		t.Fatal(err)
	}
	closeBody(t, second)
	if second.StatusCode != http.StatusConflict ||
		refusal["code"] != "account_export_in_progress" {
		t.Fatalf("concurrent response status=%d body=%v", second.StatusCode, refusal)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("stream calls during overlap = %d, want 1", got)
	}

	close(release)
	first := <-firstResult
	closeBody(t, first)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.StatusCode)
	}

	third := do()
	closeBody(t, third)
	if third.StatusCode != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("post-release status=%d calls=%d", third.StatusCode, calls.Load())
	}
}

func TestAccountSelfExportMapsPreflightErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantText   string
	}{
		{
			name: "vault lifecycle", err: ErrVaultLifecycleInProgress,
			wantStatus: http.StatusConflict, wantCode: "vault_lifecycle_in_progress",
			wantText: "finish or cancel",
		},
		{
			name: "schema ahead", err: ErrExportSchemaAhead,
			wantStatus: http.StatusServiceUnavailable, wantCode: "export_schema_ahead",
			wantText: "upgraded",
		},
		{
			name: "unexpected", err: errors.New("database contains private detail"),
			wantStatus: http.StatusInternalServerError, wantCode: "account_export_failed",
			wantText: "could not export account",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := apiMux(Config{
				Authenticate: func(_ context.Context, token string) (string, string, string, bool, error) {
					return "opr_export", "acc_export", "suspended", token == "operator-token", nil
				},
				StreamAccountSelf: func(context.Context, string, io.Writer) error {
					return tc.err
				},
			})

			req := httptest.NewRequest(http.MethodGet, "/v1/export", nil)
			req.Header.Set("Authorization", "Bearer operator-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			resp := response.Result()
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			closeBody(t, resp)
			if resp.StatusCode != tc.wantStatus || body["code"] != tc.wantCode {
				t.Fatalf("status=%d body=%v", resp.StatusCode, body)
			}
			if !strings.Contains(body["error"].(string), tc.wantText) {
				t.Errorf("error = %q, want text %q", body["error"], tc.wantText)
			}
			if strings.Contains(body["error"].(string), "private detail") {
				t.Errorf("response leaked internal error: %q", body["error"])
			}
			if resp.Header.Get("X-Witself-Export-Format") != "" ||
				resp.Header.Get("Content-Disposition") != "" {
				t.Errorf("archive headers remained on error: %v", resp.Header)
			}
		})
	}
}

func selfExportTestArchive(t *testing.T, accountID string) []byte {
	t.Helper()
	var archive bytes.Buffer
	err := archiveexport.Write(context.Background(), &archive, archiveexport.Manifest{
		SchemaVersion: 72,
		ServerVersion: "v0.0.test",
		Purpose:       archiveexport.PurposeSelf,
		AccountID:     accountID,
		Status:        "active",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

// TestAccountSelfExportSpoolsBeforeStreaming pins the review fix for the
// customer-paced-transaction hazard: the store phase completes into a
// server-side spool before any response byte, so a mid-export store failure
// — even one after bytes were produced — still yields a clean typed error,
// and a successful response carries an exact Content-Length (impossible if
// the archive streamed straight out of the transaction).
func TestAccountSelfExportSpoolsBeforeStreaming(t *testing.T) {
	auth := func(_ context.Context, token string) (string, string, string, bool, error) {
		return "opr_export", "acc_export", "active", token == "operator-token", nil
	}
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/v1/export", nil)
		req.Header.Set("Authorization", "Bearer operator-token")
		return req
	}

	failing := apiMux(Config{
		Authenticate: auth,
		StreamAccountSelf: func(_ context.Context, _ string, w io.Writer) error {
			if _, err := w.Write([]byte("partial archive bytes")); err != nil {
				return err
			}
			return ErrVaultLifecycleInProgress
		},
	})
	rec := httptest.NewRecorder()
	failing.ServeHTTP(rec, request())
	if rec.Code != http.StatusConflict {
		t.Fatalf("post-write failure status = %d, want 409", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "vault_lifecycle_in_progress") {
		t.Fatalf("post-write failure body = %q, want typed vault_lifecycle_in_progress error", body)
	}

	payload := []byte("complete archive payload")
	succeeding := apiMux(Config{
		Authenticate: auth,
		StreamAccountSelf: func(_ context.Context, _ string, w io.Writer) error {
			_, err := w.Write(payload)
			return err
		},
	})
	rec = httptest.NewRecorder()
	succeeding.ServeHTTP(rec, request())
	if rec.Code != http.StatusOK {
		t.Fatalf("success status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(payload)) {
		t.Fatalf("Content-Length = %q, want %d (spooled size)", got, len(payload))
	}
	if rec.Body.String() != string(payload) {
		t.Fatal("response body does not match the spooled archive")
	}
}
