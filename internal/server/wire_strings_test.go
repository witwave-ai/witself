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

// TestWireStringsThatTheWorkerGreps pins the exact 409 response bodies the
// Cloudflare Worker's restore/evacuate code greps for as idempotency
// signals. If any of these strings drift — a rename, a punctuation edit,
// even a stylistic tweak — the Worker's benign-409 branches will misfire
// and either wedge the restore (treating a benign signal as a hard fail)
// or blow past a real failure (treating a hard fail as benign). The
// coupling is unavoidable given the JSON error format, so pinning the
// wire text is the only defense against silent drift.
//
// Each subtest constructs the failure path through the actual handler
// (Config callback returns the matching sentinel) and asserts the exact
// body substring the Worker greps for.
func TestWireStringsThatTheWorkerGreps(t *testing.T) {
	tests := []struct {
		name         string
		cfg          Config
		path         string
		body         string
		wantStatus   int
		wantContains string // exact substring the Worker's index.js greps for
		grepper      string // the file:function that depends on this string
	}{
		{
			// Worker restoreAccount treats this 409 as "prior import
			// succeeded but died before step 3" and continues to :resume.
			// If this string changes, restore idempotency breaks: a lost
			// response during import turns into a hard failure on retry.
			name: "import 409 already exists — restoreAccount benign branch",
			cfg: Config{
				ImportAccountArchive: func(context.Context, string, io.Reader) (ImportSummary, error) {
					return ImportSummary{}, ErrConflict
				},
			},
			path:         "/v1/accounts/acc_x:import",
			body:         "archive-bytes",
			wantStatus:   http.StatusConflict,
			wantContains: "account already exists on this cell",
			grepper:      "index.js restoreAccount 'already exists' 409 branch",
		},
		{
			// Worker restoreAccount treats this 409 as "the account came
			// back suspended_for=owner_request — legitimate; leave it".
			// A drift here would cause the Worker to throw and the whole
			// restore of that account to fail.
			name: "resume system 409 not suspended — restoreAccount benign branch",
			cfg: Config{
				ResumeAccountSystem: func(context.Context, string, string) error {
					return ErrAccountNotSuspended
				},
			},
			path:         "/v1/accounts/acc_x:resume",
			body:         `{"for":"evacuation"}`,
			wantStatus:   http.StatusConflict,
			wantContains: "account is not suspended",
			grepper:      "index.js restoreAccount 'not suspended' 409 branch",
		},
		{
			// Sibling benign 409: the ResumeAccountSystem category check
			// refuses to lift an owner_request suspension. Same benign
			// branch on the Worker, same importance.
			name: "resume system 409 wrong category — restoreAccount benign branch",
			cfg: Config{
				ResumeAccountSystem: func(context.Context, string, string) error {
					return ErrResumeWrongCategory
				},
			},
			path:         "/v1/accounts/acc_x:resume",
			body:         `{"for":"evacuation"}`,
			wantStatus:   http.StatusConflict,
			wantContains: "suspension category does not match",
			grepper:      "index.js restoreAccount 'category' 409 branch",
		},
		{
			// Worker evacuateAccount treats this 409 as "signup landed
			// moments before drain; reap the pending tombstone and skip
			// archiving". A drift here would re-wedge the destroy path
			// that the pending-reap fix closed.
			name: "suspend system 409 pending — evacuateAccount pending-reap branch",
			cfg: Config{
				SuspendAccountSystem: func(context.Context, string, string, string) error {
					return ErrConflict
				},
			},
			path:         "/v1/accounts/acc_x:suspend",
			body:         `{"for":"evacuation"}`,
			wantStatus:   http.StatusConflict,
			wantContains: "pending",
			grepper:      "index.js evacuateAccount 'pending' 409 branch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Every lifecycle verb here needs the provision-token pair
			// mounted so accountLifecycleHandler is registered.
			cfg := tc.cfg
			cfg.ProvisionToken = "witself_prv_test"
			cfg.ProvisionAccount = func(context.Context, string, string) (ProvisionedAccount, error) {
				return ProvisionedAccount{}, errors.New("unused")
			}
			srv := httptest.NewServer(apiMux(cfg))
			defer srv.Close()

			req, _ := http.NewRequest(http.MethodPost, srv.URL+tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer witself_prv_test")
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
