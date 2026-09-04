package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSecurityHeaderWireStrings pins the exact names and values shared with
// the control-plane Worker. These are wire policy, not presentation strings.
func TestSecurityHeaderWireStrings(t *testing.T) {
	tests := []struct {
		label string
		got   string
		want  string
	}{
		{"HSTS name", strictTransportSecurityHeaderName, "Strict-Transport-Security"},
		{"HSTS value", strictTransportSecurityHeaderValue, "max-age=31536000; includeSubDomains"},
		{"content type options name", contentTypeOptionsHeaderName, "X-Content-Type-Options"},
		{"content type options value", contentTypeOptionsHeaderValue, "nosniff"},
		{"referrer policy name", referrerPolicyHeaderName, "Referrer-Policy"},
		{"referrer policy value", referrerPolicyHeaderValue, "no-referrer"},
		{"cache control name", cacheControlHeaderName, "Cache-Control"},
		{"authenticated cache control value", authenticatedCacheControlValue, "no-store"},
	}
	for _, tc := range tests {
		t.Run(tc.label, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("wire string = %q, want %q", tc.got, tc.want)
			}
		})
	}
}

func TestSecurityHeadersByRouteAuthentication(t *testing.T) {
	auth := func(_ context.Context, token string) (string, string, string, bool, error) {
		if token == "operator-token" {
			return "operator_1", "account_1", "active", true, nil
		}
		return "", "", "", false, nil
	}

	t.Run("authenticated JSON route", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
		request.Header.Set("Authorization", "Bearer operator-token")
		apiMux(Config{Authenticate: auth}).ServeHTTP(recorder, request)
		response := recorder.Result()
		defer func() { _ = response.Body.Close() }()

		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
		}
		assertSecurityHeaders(t, response.Header)
		if got := response.Header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want %q", got, "no-store")
		}
	})

	publicRoutes := []struct {
		name    string
		handler http.Handler
		path    string
	}{
		{"API version", apiMux(Config{}), "/v1/version"},
		{"health", healthMux(nil), "/healthz"},
		{"metrics", metricsMux(), "/metrics"},
	}
	for _, tc := range publicRoutes {
		t.Run("public "+tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tc.path, nil)
			tc.handler.ServeHTTP(recorder, request)
			response := recorder.Result()
			defer func() { _ = response.Body.Close() }()

			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
			}
			assertSecurityHeaders(t, response.Header)
			if values := response.Header.Values("Cache-Control"); len(values) != 0 {
				t.Fatalf("Cache-Control = %q, want header absent", values)
			}
		})
	}
}

func TestAuthenticatedNoStoreDefaultPreservesExplicitCacheControl(t *testing.T) {
	auth := func(context.Context, string) (string, string, string, bool, error) {
		return "operator_1", "account_1", "active", true, nil
	}
	handler := securityResponseHeaders(requireOperator(auth, func(w http.ResponseWriter, _ *http.Request, _ principal) {
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(http.StatusNoContent)
	}))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/authenticated", nil)
	request.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	defer func() { _ = response.Body.Close() }()

	if got := response.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q, want explicit route value", got)
	}
}

func assertSecurityHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for name, want := range map[string]string{
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "no-referrer",
	} {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestWireStringsThatTheWorkerGreps pins the exact 409 response bodies the
// Cloudflare Worker's restore/evacuate code classifies. If any of these
// strings drift — a rename, a punctuation edit, even a stylistic tweak —
// a benign retry can wedge or an exact-evacuation conflict can be mistaken
// for success. The coupling is unavoidable given the JSON error format, so
// pinning the wire text is the only defense against silent drift.
//
// Each subtest constructs the failure path through the actual handler
// (Config callback returns the matching sentinel) and asserts the exact
// body substring the Worker greps for.
func TestWireStringsThatTheWorkerGreps(t *testing.T) {
	const evacuationID = "evac_01J00000000000000000000000"
	tests := []struct {
		name         string
		cfg          Config
		path         string
		body         string
		evacuationID string
		wantStatus   int
		wantContains string // exact substring the Worker's index.js greps for
		grepper      string // the file:function that depends on this string
	}{
		{
			// A different evacuation owns the destination row. Unlike the
			// old generic "already exists" response, this is never a benign
			// import retry: the Worker must retain it as a hard failure.
			name: "import 409 different evacuation — restoreAccount hard failure",
			cfg: Config{
				ImportAccountArchive: func(context.Context, string, string, io.Reader) (ImportSummary, error) {
					return ImportSummary{}, ErrConflict
				},
			},
			path:         "/v1/accounts/acc_x:import-evacuation",
			body:         "archive-bytes",
			evacuationID: evacuationID,
			wantStatus:   http.StatusConflict,
			wantContains: "account exists under a different evacuation",
			grepper:      "index.js restoreAccount mismatched-evacuation 409 branch",
		},
		{
			// Under the exact-epoch contract, a resume must return the
			// matching completion acknowledgement. "Not suspended" is not
			// proof that this evacuation completed and must stay a hard fail.
			name: "resume system 409 not suspended — restoreAccount hard failure",
			cfg: Config{
				ResumeAccountSystem: func(context.Context, string, string, string) (AccountEvacuationRecord, error) {
					return AccountEvacuationRecord{}, ErrAccountNotSuspended
				},
			},
			path:         "/v1/accounts/acc_x:complete-evacuation",
			body:         `{"for":"evacuation","evacuation_id":"` + evacuationID + `"}`,
			wantStatus:   http.StatusConflict,
			wantContains: "account is not suspended",
			grepper:      "index.js restoreAccount completion-ack guard",
		},
		{
			// A wrong category likewise cannot stand in for an exact
			// completion acknowledgement, even if legacy cells once treated
			// owner suspension this way.
			name: "resume system 409 wrong category — restoreAccount hard failure",
			cfg: Config{
				ResumeAccountSystem: func(context.Context, string, string, string) (AccountEvacuationRecord, error) {
					return AccountEvacuationRecord{}, ErrResumeWrongCategory
				},
			},
			path:         "/v1/accounts/acc_x:complete-evacuation",
			body:         `{"for":"evacuation","evacuation_id":"` + evacuationID + `"}`,
			wantStatus:   http.StatusConflict,
			wantContains: "suspension category does not match",
			grepper:      "index.js restoreAccount completion-ack guard",
		},
		{
			// Worker evacuateAccount treats this 409 as "signup landed
			// moments before drain; reap the pending tombstone and skip
			// archiving". A drift here would re-wedge the destroy path
			// that the pending-reap fix closed.
			name: "begin evacuation 409 pending — evacuateAccount pending-reap branch",
			cfg: Config{
				BeginAccountEvacuation: func(context.Context, string, string, string, string) (AccountEvacuationRecord, error) {
					return AccountEvacuationRecord{}, ErrAccountPending
				},
			},
			path:         "/v1/accounts/acc_x:begin-evacuation",
			body:         `{"for":"evacuation","evacuation_id":"` + evacuationID + `"}`,
			wantStatus:   http.StatusConflict,
			wantContains: "pending",
			grepper:      "index.js evacuateAccount 'pending' 409 branch",
		},
		{
			// A different epoch already owns the source fence. This response
			// must not contain the pending marker the Worker uses to authorize
			// a reap; it is an exact-evacuation hard failure.
			name: "begin evacuation 409 different evacuation — evacuateAccount hard failure",
			cfg: Config{
				BeginAccountEvacuation: func(context.Context, string, string, string, string) (AccountEvacuationRecord, error) {
					return AccountEvacuationRecord{}, ErrConflict
				},
			},
			path:         "/v1/accounts/acc_x:begin-evacuation",
			body:         `{"for":"evacuation","evacuation_id":"` + evacuationID + `"}`,
			wantStatus:   http.StatusConflict,
			wantContains: "account evacuation id does not match",
			grepper:      "index.js evacuateAccount exact-evacuation hard-failure guard",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Every lifecycle verb here needs the provision-token pair
			// mounted so accountLifecycleHandler is registered.
			cfg := tc.cfg
			cfg.ProvisionToken = "witself_prv_test"
			cfg.ProvisionAccountExact = func(context.Context, string, string, string, string, string) (ProvisionedAccount, error) {
				return ProvisionedAccount{}, errors.New("unused")
			}
			srv := httptest.NewServer(apiMux(cfg))
			defer srv.Close()

			req, _ := http.NewRequest(http.MethodPost, srv.URL+tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer witself_prv_test")
			if tc.evacuationID != "" {
				req.Header.Set(AccountEvacuationIDHeader, tc.evacuationID)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			bodyBytes, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d — body: %s", resp.StatusCode, tc.wantStatus, string(bodyBytes))
			}
			if !bytes.Contains(bodyBytes, []byte(tc.wantContains)) {
				t.Errorf("body does not contain %q — %s will misfire\nbody: %s",
					tc.wantContains, tc.grepper, string(bodyBytes))
			}
			// The Worker parses JSON, so also confirm the string lives
			// on the `error` field specifically. A stringified body that
			// only mentions the string in a comment or field name would
			// fool the naive substring test but not the Worker.
			var decoded struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(bodyBytes, &decoded); err != nil {
				t.Errorf("body is not JSON: %v — %s greps a JSON field", err, tc.grepper)
			}
			if !strings.Contains(decoded.Error, tc.wantContains) {
				t.Errorf("error field %q does not contain %q — %s will misfire",
					decoded.Error, tc.wantContains, tc.grepper)
			}
		})
	}
}
