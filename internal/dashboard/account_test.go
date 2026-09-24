package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type accountFixture struct {
	mu             sync.Mutex
	principal      map[string]any
	schema         any
	revoked        bool
	payloads       map[string]any
	failures       map[string]int
	requests       map[string]int
	cp             string
	beforeResponse func(*http.Request, int)
}

func newAccountFixture(t *testing.T) (*accountFixture, Config) {
	t.Helper()
	f := &accountFixture{schema: "witself.v0", principal: map[string]any{"kind": "operator", "account_id": "acct_test", "operator_id": "op_test", "account_role": "account_owner"}, payloads: map[string]any{}, failures: map[string]int{}, requests: map[string]int{}}
	ticket := map[string]any{"id": "ticket_1", "account_id": "acct_test", "subject": "<script>inert</script>\u001b[31m", "state": "open", "metadata": "PRIVATE_METADATA", "correlation": "PRIVATE_CORRELATION", "attachments": "PRIVATE_ATTACHMENT"}
	f.payloads["/v1/account"] = map[string]any{"schema_version": "witself.v0", "account": map[string]any{"id": "acct_test", "display_name": "<img src=x onerror=alert(1)>", "status": "active", "email": "fake@example.invalid", "created_at": "2026-09-24T00:00:00Z", "placement_policy": "PRIVATE_POLICY", "suspended_reason": "PRIVATE_REASON"}}
	f.payloads["/v1/support/tickets"] = map[string]any{"schema_version": "witself.v0", "tickets": []any{ticket}}
	f.payloads["/v1/support/tickets/ticket_1"] = map[string]any{"schema_version": "witself.v0", "ticket": ticket, "messages": []any{map[string]any{"id": "message_1", "ticket_id": "ticket_1", "account_id": "acct_test", "body": "<script>body</script>", "author_kind": "operator", "metadata": "PRIVATE_METADATA", "attachments": "PRIVATE_ATTACHMENT"}}}
	f.payloads["/v1/accounts/acct_test/plan"] = map[string]any{"schema_version": "witself.v0", "account_id": "acct_test", "plan": "standard", "plan_name": "Standard", "apply_blocked": "PRIVATE_RAW_VIOLATION", "limits": map[string]any{"agents": 5, "agent_email_max_raw_bytes": 12345, "UNKNOWN_POLICY": 666}, "limit_defaults": map[string]any{"agents": 3}, "features": []any{"memory", "PRIVATE_FEATURE"}, "policies": map[string]any{"PRIVATE_POLICY": 1}, "messaging": map[string]any{"enabled": true, "default_enabled": false, "overridden": true, "PRIVATE": "PRIVATE_METADATA"}, "transcript_retention": map[string]any{"default_days": nil, "effective_days": 30, "overridden": true}, "pending": map[string]any{"kind": "scheduled", "plan": "free", "effective": "2026-10-01T00:00:00Z", "url": "https://PRIVATE_ACTION.invalid"}}
	f.payloads["/v1/accounts/acct_test/billing"] = map[string]any{"schema_version": "witself.v0", "account_id": "acct_test", "billing_available": true, "configured": true, "billing_plan": "standard", "effective_plan": "standard", "customer_id": "PRIVATE_PROVIDER", "next_charge": map[string]any{"amount_cents": int64(9007199254740993), "currency": "jpy", "date": "2026-10-01T00:00:00Z"}, "pending": map[string]any{"kind": "action", "url": "https://PRIVATE_ACTION.invalid"}}
	f.payloads["/v1/accounts/acct_test/billing/invoices"] = map[string]any{"schema_version": "witself.v0", "account_id": "acct_test", "invoices": []any{map[string]any{"amount_cents": nil, "currency": "eur", "number": "1", "hosted_url": "https://PRIVATE_PAY.invalid", "pdf_url": "javascript:PRIVATE_PDF", "provider_id": "PRIVATE_PROVIDER"}, map[string]any{"currency": "usd"}}}
	f.payloads["/v1/accounts/acct_test/billing/payments"] = map[string]any{"schema_version": "witself.v0", "account_id": "acct_test", "payments": []any{map[string]any{"amount_cents": -123, "currency": "usd", "receipt_url": "https://PRIVATE_RECEIPT.invalid"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(dst http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		path := r.URL.Path
		f.requests[path]++
		count, hook := f.requests[path], f.beforeResponse
		w := httptest.NewRecorder()
		defer func() {
			f.mu.Unlock()
			// Hold a snapshotted response without blocking other upstream reads.
			if hook != nil {
				hook(r, count)
			}
			for key, values := range w.Header() {
				dst.Header()[key] = values
			}
			dst.WriteHeader(w.Code)
			_, _ = dst.Write(w.Body.Bytes())
		}()
		if r.Method != "GET" {
			t.Error("account upstream mutation")
		}
		want := "Bearer fake-manager"
		if path == "/v1/capabilities" {
			want = ""
		}
		if path == "/v1/self" {
			want = "Bearer fake-agent"
		}
		if r.Header.Get("Authorization") != want {
			t.Errorf("credential cross-use on %s", path)
		}
		if status := f.failures[path]; status != 0 {
			http.Error(w, "PRIVATE_ERROR fake-manager", status)
			return
		}
		if path == "/v1/whoami" {
			if f.revoked {
				http.Error(w, "PRIVATE_REVOKED", 401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": f.schema, "principal": f.principal})
			return
		}
		if path == "/v1/capabilities" {
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "backend": map[string]any{"kind": "managed"}, "billing": map[string]any{"supported": true, "endpoint": f.cp}})
			return
		}
		if path == "/v1/self" {
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "self": testSelfDigest()})
			return
		}
		payload, ok := f.payloads[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)
	f.cp = srv.URL
	identity := testIdentity
	identity.AccountID = "acct_test"
	return f, Config{Endpoint: srv.URL, BearerToken: "fake-agent", AccessToken: "fake-access", Identity: identity, AccountManager: &AccountManager{Endpoint: srv.URL, BearerToken: "fake-manager", Identity: AccountManagerIdentity{AccountID: "acct_test", OperatorID: "op_test", Role: "account_owner"}}}
}
func (f *accountFixture) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[path]
}
func (f *accountFixture) change(fn func()) { f.mu.Lock(); defer f.mu.Unlock(); fn() }

type accountHarness struct {
	read   func(context.Context, ReadRequest) (int, []byte)
	reader *Reader
	server *httptest.Server
	cookie *http.Cookie
}

func newAccountHarness(t *testing.T, mode string, cfg Config) accountHarness {
	t.Helper()
	if mode == "reader" {
		reader, err := NewReader(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return accountHarness{reader: reader, read: func(ctx context.Context, in ReadRequest) (int, []byte) {
			raw, err := reader.Read(ctx, in)
			if err != nil {
				var e *ReaderError
				if !errors.As(err, &e) {
					t.Error("untyped reader failure")
					return 0, nil
				}
				return e.Status, nil
			}
			return 200, raw
		}}
	}
	mux := http.NewServeMux()
	if err := Register(mux, cfg); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cookie := sessionCookie(t, srv, cfg)
	return accountHarness{server: srv, cookie: cookie, read: func(ctx context.Context, in ReadRequest) (int, []byte) {
		path := accountPaths[in.Resource]
		if in.Resource == ResourceSelf {
			path = "/api/self"
		}
		if in.ID != "" {
			path += url.PathEscape(in.ID)
		}
		if len(in.Query) > 0 {
			path += "?" + in.Query.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
		if err != nil {
			t.Error(err)
			return 0, nil
		}
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				t.Error(err)
			}
			return 0, nil
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.Header.Get("Cache-Control") != "private, no-store" || resp.Header.Get("Content-Security-Policy") == "" {
			t.Error("missing secure wrapper")
		}
		return resp.StatusCode, raw
	}}
}
func readAccountOK(t *testing.T, h accountHarness, resource Resource, id string) map[string]any {
	t.Helper()
	status, raw := h.read(t.Context(), ReadRequest{Resource: resource, ID: id})
	if status != 200 {
		t.Fatalf("resource %d status %d", resource, status)
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil || out["schema_version"] != AccountSchema {
		t.Fatal("invalid account envelope")
	}
	return out
}
func TestAccountReaderAndLoopbackProjections(t *testing.T) {
	for _, mode := range []string{"reader", "web"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg := newAccountFixture(t)
			h := newAccountHarness(t, mode, cfg)
			if f.count("/v1/whoami") != 0 {
				t.Fatal("constructor contacted account")
			}
			for resource := ResourceAccountContext; resource <= ResourceAccountClientsScan; resource++ {
				id := ""
				if resource == ResourceAccountSupportTicket {
					id = "ticket_1"
				}
				out := readAccountOK(t, h, resource, id)
				raw, _ := json.Marshal(out)
				if strings.Contains(string(raw), "PRIVATE_") || strings.Contains(string(raw), "fake-manager") || strings.Contains(string(raw), "\\u001b") {
					t.Fatalf("unallowlisted projection for resource %d", resource)
				}
				if resource == ResourceAccountContext || resource == ResourceAccountAccess {
					want := []any{"overview", "clients", "plan", "billing", "support", "access"}
					if !reflect.DeepEqual(out["sections"], want) {
						t.Fatal("sections")
					}
				}
				if resource == ResourceAccountClients || resource == ResourceAccountClientsScan {
					if out["status"] != "not_checked" || out["report"] != nil {
						t.Fatal("nil scanner readiness fabricated")
					}
				}
				if resource == ResourceAccountPlan {
					if out["apply_blocked"] != true {
						t.Fatal("blocked plan state lost")
					}
					limits := out["limits"].(map[string]any)
					if limits["agents"] != float64(5) || out["limits_units"].(map[string]any)["agent_email_max_raw_bytes"] != "bytes" {
						t.Fatal("plan limits")
					}
					if out["transcript_retention"].(map[string]any)["default_days"] != nil {
						t.Fatal("indefinite retention lost")
					}
				}
				if resource == ResourceAccountBilling {
					invoices := out["invoices"].(map[string]any)["entries"].([]any)
					for _, v := range invoices {
						row := v.(map[string]any)
						amount, exists := row["amount_cents"]
						if !exists || amount != nil {
							t.Fatal("unknown became zero")
						}
					}
					// Read raw again to avoid float64 conversion by this test helper.
					_, raw := h.read(t.Context(), ReadRequest{Resource: resource})
					if !strings.Contains(string(raw), "9007199254740993") || !strings.Contains(string(raw), `"currency":"jpy"`) {
						t.Fatal("financial precision or currency lost")
					}
				}
			}
			if f.count("/v1/support/tickets/ticket_1") != 1 {
				t.Fatal("ticket read was not deliberate")
			}
			before := f.count("/v1/support/tickets/ticket_1")
			_ = readAccountOK(t, h, ResourceAccountContext, "")
			_ = readAccountOK(t, h, ResourceAccountSupport, "")
			if f.count("/v1/support/tickets/ticket_1") != before {
				t.Fatal("passive thread fetch")
			}
			status, _ := h.read(t.Context(), ReadRequest{Resource: ResourceSelf})
			if status != 200 {
				t.Fatal("agent resource broken")
			}
		})
	}
}
func TestAccountMissingOrMismatchedConfigNoRequests(t *testing.T) {
	for _, mode := range []string{"reader", "web"} {
		for _, kind := range []string{"absent", "account", "endpoint", "same_bearer", "agent_missing", "role"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				f, cfg := newAccountFixture(t)
				var scans atomic.Int32
				cfg.AccountClientsScan = func(context.Context, string) (AccountClientsReport, error) {
					scans.Add(1)
					return AccountClientsReport{}, nil
				}
				switch kind {
				case "absent":
					cfg.AccountManager = nil
				case "account":
					cfg.AccountManager.Identity.AccountID = "acct_other"
				case "endpoint":
					cfg.AccountManager.Endpoint = "http://127.0.0.1:1"
				case "same_bearer":
					cfg.AccountManager.BearerToken = cfg.BearerToken
				case "agent_missing":
					cfg.Identity.AgentID = ""
				case "role":
					cfg.AccountManager.Identity.Role = "account_member"
				}
				h := newAccountHarness(t, mode, cfg)
				out := readAccountOK(t, h, ResourceAccountContext, "")
				if len(out) != 2 || out["available"] != false {
					t.Fatal("identity leaked on missing context")
				}
				for resource := ResourceAccountOverview; resource <= ResourceAccountClientsScan; resource++ {
					id := ""
					if resource == ResourceAccountSupportTicket {
						id = "ticket_1"
					}
					status, _ := h.read(t.Context(), ReadRequest{Resource: resource, ID: id})
					if status != 403 {
						t.Fatalf("missing context resource %d status %d", resource, status)
					}
				}
				if f.count("/v1/whoami") != 0 || scans.Load() != 0 {
					t.Fatal("invalid local context performed account work")
				}
			})
		}
	}
}
func TestAccountLiveIdentityFailClosedAndNarrowing(t *testing.T) {
	for _, mode := range []string{"reader", "web"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg := newAccountFixture(t)
			h := newAccountHarness(t, mode, cfg)
			for _, tc := range []struct {
				key   string
				value any
			}{
				{"schema", nil}, {"schema", "wrong"}, {"kind", nil}, {"kind", "agent"}, {"account_id", nil}, {"account_id", "acct_other"}, {"operator_id", nil}, {"operator_id", "op_other"}, {"account_role", nil}, {"account_role", "account_member"},
			} {
				f.change(func() {
					f.schema = "witself.v0"
					f.principal = map[string]any{"kind": "operator", "account_id": "acct_test", "operator_id": "op_test", "account_role": "account_owner"}
					if tc.key == "schema" {
						f.schema = tc.value
					} else if tc.value == nil {
						delete(f.principal, tc.key)
					} else {
						f.principal[tc.key] = tc.value
					}
				})
				out := readAccountOK(t, h, ResourceAccountContext, "")
				if len(out) != 2 || out["available"] != false {
					t.Fatalf("invalid identity accepted %s", tc.key)
				}
				before := f.count("/v1/account")
				status, raw := h.read(t.Context(), ReadRequest{Resource: ResourceAccountOverview})
				if status != 403 || f.count("/v1/account") != before || strings.Contains(string(raw), "PRIVATE_") {
					t.Fatal("invalid principal performed read")
				}
			}
			f.change(func() { f.principal["account_role"] = "account_operator" })
			out := readAccountOK(t, h, ResourceAccountContext, "")
			if !reflect.DeepEqual(out["sections"], []any{"overview", "clients", "support", "access"}) {
				t.Fatal("role not narrowed")
			}
			before := f.count("/v1/capabilities")
			for _, resource := range []Resource{ResourceAccountPlan, ResourceAccountBilling} {
				status, _ := h.read(t.Context(), ReadRequest{Resource: resource})
				if status != 403 {
					t.Fatal("billing allowed")
				}
			}
			if f.count("/v1/capabilities") != before {
				t.Fatal("forbidden billing routing")
			}
			_ = readAccountOK(t, h, ResourceAccountOverview, "")
			f.change(func() { f.revoked = true })
			out = readAccountOK(t, h, ResourceAccountContext, "")
			if out["available"] != false {
				t.Fatal("revocation ignored")
			}
		})
	}
}
func TestAccountUpstreamFailuresAndRowBounds(t *testing.T) {
	for _, mode := range []string{"reader", "web"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg := newAccountFixture(t)
			f.change(func() {
				list := f.payloads["/v1/support/tickets"].(map[string]any)
				row := list["tickets"].([]any)[0]
				rows := make([]any, 101)
				for i := range rows {
					rows[i] = row
				}
				list["tickets"] = rows
				detail := f.payloads["/v1/support/tickets/ticket_1"].(map[string]any)
				message := detail["messages"].([]any)[0].(map[string]any)
				message["body"] = strings.Repeat("x", accountBodyBytes+1)
				rows = make([]any, 101)
				for i := range rows {
					rows[i] = message
				}
				detail["messages"] = rows
				f.failures["/v1/accounts/acct_test/billing/payments"] = 500
			})
			h := newAccountHarness(t, mode, cfg)
			for _, tc := range []struct {
				resource Resource
				id, key  string
			}{{ResourceAccountSupport, "", "tickets"}, {ResourceAccountSupportTicket, "ticket_1", "messages"}} {
				out := readAccountOK(t, h, tc.resource, tc.id)
				if out["truncated"] != true || len(out[tc.key].([]any)) != 100 {
					t.Fatal("row bound lost")
				}
				if tc.key == "messages" {
					if len(out[tc.key].([]any)[0].(map[string]any)["body"].(string)) != accountBodyBytes {
						t.Fatal("body unbounded")
					}
				}
			}
			out := readAccountOK(t, h, ResourceAccountBilling, "")
			payments := out["payments"].(map[string]any)
			if payments["available"] != false || payments["entries"] != nil {
				t.Fatal("failed billing looks empty")
			}
			raw, _ := json.Marshal(out)
			if strings.Contains(string(raw), "PRIVATE_ERROR") {
				t.Fatal("raw error leak")
			}
			f.change(func() { f.payloads["/v1/account"].(map[string]any)["account"].(map[string]any)["id"] = "acct_other" })
			status, _ := h.read(t.Context(), ReadRequest{Resource: ResourceAccountOverview})
			if status != 502 {
				t.Fatal("wrong overview account accepted")
			}
			f.change(func() {
				f.payloads["/v1/support/tickets/ticket_1"].(map[string]any)["messages"].([]any)[0].(map[string]any)["account_id"] = "acct_other"
			})
			status, _ = h.read(t.Context(), ReadRequest{Resource: ResourceAccountSupportTicket, ID: "ticket_1"})
			if status != 502 {
				t.Fatal("cross account thread")
			}
		})
	}
}

func syntheticAccountClients() AccountClientsReport {
	return AccountClientsReport{SchemaVersion: AccountClientsSchema, DeviceLabel: "This device", CheckedAt: "2026-09-24T00:00:00Z", ScanStatus: "complete", Entries: []AccountClientEntry{{Runtime: "codex", RecordedVersion: "1.2.3", ExecutableStatus: "present", ConfigurationStatus: "match", ConfigurationScope: "mcp_registration", EffectiveVerification: "not_run"}}}
}
func TestAccountScannerExplicitCachedAndUnauthorized(t *testing.T) {
	for _, mode := range []string{"reader", "web"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg := newAccountFixture(t)
			var scans atomic.Int32
			cfg.AccountClientsScan = func(ctx context.Context, account string) (AccountClientsReport, error) {
				if account != "acct_test" {
					t.Error("scanner wrong account")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 10*time.Second {
					t.Error("scanner unbounded context")
				}
				scans.Add(1)
				return syntheticAccountClients(), nil
			}
			h := newAccountHarness(t, mode, cfg)
			for i := 0; i < 3; i++ {
				out := readAccountOK(t, h, ResourceAccountClients, "")
				if out["status"] != "not_checked" {
					t.Fatal("passive scan")
				}
			}
			if scans.Load() != 0 {
				t.Fatal("constructor/poll scanned")
			}
			out := readAccountOK(t, h, ResourceAccountClientsScan, "")
			if out["status"] != "checked" || scans.Load() != 1 {
				t.Fatal("explicit check failed")
			}
			out = readAccountOK(t, h, ResourceAccountClients, "")
			if out["status"] != "checked" || scans.Load() != 1 {
				t.Fatal("cached report missing or poll scanned")
			}
			// Config is snapshotted: caller edits cannot replace the principal or callback.
			cfg.AccountManager.Identity.OperatorID = "changed_after_construction"
			_ = readAccountOK(t, h, ResourceAccountContext, "")
			f.change(func() { f.revoked = true })
			status, _ := h.read(t.Context(), ReadRequest{Resource: ResourceAccountClientsScan})
			if status != 403 || scans.Load() != 1 {
				t.Fatal("unauthorized scanner")
			}
			f.change(func() { f.revoked = false })
			out = readAccountOK(t, h, ResourceAccountClients, "")
			if out["status"] != "not_checked" {
				t.Fatal("revoked cache retained")
			}
		})
	}
}
func TestAccountScannerConcurrencyCancellationAndAuthorityLoss(t *testing.T) {
	for _, mode := range []string{"reader", "web"} {
		for _, action := range []string{"cancel", "revoke", "narrow"} {
			t.Run(mode+"/"+action, func(t *testing.T) {
				f, cfg := newAccountFixture(t)
				started := make(chan struct{})
				release := make(chan struct{})
				cancelled := make(chan struct{})
				var scans atomic.Int32
				cfg.AccountClientsScan = func(ctx context.Context, _ string) (AccountClientsReport, error) {
					if scans.Add(1) == 1 {
						close(started)
					}
					select {
					case <-ctx.Done():
						close(cancelled)
						return AccountClientsReport{}, ctx.Err()
					case <-release:
						return syntheticAccountClients(), nil
					}
				}
				h := newAccountHarness(t, mode, cfg)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan int, 1)
				go func() { status, _ := h.read(ctx, ReadRequest{Resource: ResourceAccountClientsScan}); done <- status }()
				select {
				case <-started:
				case <-time.After(3 * time.Second):
					t.Fatal("scan did not start")
				}
				out := readAccountOK(t, h, ResourceAccountClients, "")
				if out["status"] != "not_checked" {
					t.Fatal("scan blocked passive cache read")
				}
				status, _ := h.read(t.Context(), ReadRequest{Resource: ResourceAccountClientsScan})
				if status != 502 || scans.Load() != 1 {
					t.Fatal("concurrent scan was not refused")
				}
				switch action {
				case "cancel":
					cancel()
					select {
					case <-cancelled:
					case <-time.After(3 * time.Second):
						t.Fatal("cancellation not propagated")
					}
				case "revoke":
					f.change(func() { f.revoked = true })
					_ = readAccountOK(t, h, ResourceAccountContext, "")
					close(release)
				case "narrow":
					f.change(func() { f.principal["account_role"] = "account_operator" })
					_ = readAccountOK(t, h, ResourceAccountContext, "")
					close(release)
				}
				select {
				case status = <-done:
					if status == 200 {
						t.Fatal("stale scan published")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("scan not finished")
				}
				f.change(func() { f.revoked = false })
				out = readAccountOK(t, h, ResourceAccountClients, "")
				if out["status"] != "not_checked" {
					t.Fatal("stale/cancelled scan cached")
				}
			})
		}
	}
}
func TestAccountLoopbackGuardsAndNoMutation(t *testing.T) {
	f, cfg := newAccountFixture(t)
	var scans atomic.Int32
	cfg.AccountClientsScan = func(context.Context, string) (AccountClientsReport, error) {
		scans.Add(1)
		return syntheticAccountClients(), nil
	}
	h := newAccountHarness(t, "web", cfg)
	for _, tc := range []struct {
		method, path, host, site string
		auth                     bool
		status                   int
	}{
		{"GET", "/api/account/context", "", "", false, 401},
		{"GET", "/api/account/clients/scan", "evil.invalid", "", true, 403},
		{"GET", "/api/account/clients/scan", "", "cross-site", true, 403},
		{"HEAD", "/api/account/clients/scan", "", "", true, 405},
		{"GET", "/api/account/clients/scan?force=true", "", "", true, 400},
		{"GET", "/api/account/support/bad%2Fid", "", "", true, 400},
	} {
		req, _ := http.NewRequest(tc.method, h.server.URL+tc.path, nil)
		if tc.host != "" {
			req.Host = tc.host
		}
		if tc.site != "" {
			req.Header.Set("Sec-Fetch-Site", tc.site)
		}
		if tc.auth {
			req.AddCookie(h.cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("guard %s %s: %d", tc.method, tc.path, resp.StatusCode)
		}
	}
	for resource, path := range accountPaths {
		if resource == ResourceAccountSupportTicket {
			path += "ticket_1"
		}
		for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
			req, _ := http.NewRequest(method, h.server.URL+path, strings.NewReader(`{"action":"mutate"}`))
			req.AddCookie(h.cookie)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode < 400 {
				t.Fatal("mutation accepted")
			}
		}
	}
	if f.count("/v1/whoami") != 0 || scans.Load() != 0 {
		t.Fatal("guarded request performed account work")
	}
	reader, err := NewReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []ReadRequest{{Resource: ResourceAccountSupportTicket}, {Resource: ResourceAccountSupportTicket, ID: "../bad"}, {Resource: ResourceAccountSupportTicket, ID: "bad/id"}, {Resource: ResourceAccountContext, ID: "unexpected"}, {Resource: ResourceAccountClientsScan, Query: url.Values{"force": {"true"}}}} {
		_, err := reader.Read(t.Context(), in)
		var e *ReaderError
		if !errors.As(err, &e) || e.Status != 400 {
			t.Fatal("Reader validation bypass")
		}
	}
	if f.count("/v1/whoami") != 0 {
		t.Fatal("invalid Reader request contacted manager")
	}
}
func TestAccountScannerBoundsAndErrors(t *testing.T) {
	for _, mode := range []string{"reader", "web"} {
		t.Run(mode, func(t *testing.T) {
			_, cfg := newAccountFixture(t)
			var fail atomic.Bool
			cfg.AccountClientsScan = func(context.Context, string) (AccountClientsReport, error) {
				if fail.Load() {
					return AccountClientsReport{}, errors.New("PRIVATE_CALLBACK_ERROR")
				}
				out := syntheticAccountClients()
				entry := out.Entries[0]
				out.Entries = make([]AccountClientEntry, len(accountClientRuntimes))
				for i := range out.Entries {
					out.Entries[i] = entry
					out.Entries[i].Runtime = accountClientRuntimes[i]
				}
				return out, nil
			}
			h := newAccountHarness(t, mode, cfg)
			out := readAccountOK(t, h, ResourceAccountClientsScan, "")
			report := out["report"].(map[string]any)
			if report["truncated"] != false || len(report["entries"].([]any)) != 8 {
				t.Fatal("scanner unbounded")
			}
			fail.Store(true)
			status, raw := h.read(t.Context(), ReadRequest{Resource: ResourceAccountClientsScan})
			if status != 502 || strings.Contains(string(raw), "PRIVATE_") {
				t.Fatal("scanner error leaked")
			}
			out = readAccountOK(t, h, ResourceAccountClients, "")
			if out["status"] != "checked" {
				t.Fatal("failed recheck erased last successful report")
			}
		})
	}
}

func TestAccountResponseBoundsAndMalformedSections(t *testing.T) {
	for _, mode := range []string{"reader", "web"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg := newAccountFixture(t)
			h := newAccountHarness(t, mode, cfg)
			f.change(func() {
				overview := f.payloads["/v1/account"].(map[string]any)
				overview["oversized_private"] = strings.Repeat("x", 2<<20)
				detail := f.payloads["/v1/support/tickets/ticket_1"].(map[string]any)
				detail["oversized_private"] = strings.Repeat("x", 4<<20)
				invoice := f.payloads["/v1/accounts/acct_test/billing/invoices"].(map[string]any)
				row := invoice["invoices"].([]any)[0]
				rows := make([]any, 101)
				for i := range rows {
					rows[i] = row
				}
				invoice["invoices"] = rows
			})
			for _, in := range []ReadRequest{{Resource: ResourceAccountOverview}, {Resource: ResourceAccountSupportTicket, ID: "ticket_1"}} {
				status, _ := h.read(t.Context(), in)
				if status != 502 {
					t.Fatal("upstream bytes not bounded")
				}
			}
			out := readAccountOK(t, h, ResourceAccountBilling, "")
			invoices := out["invoices"].(map[string]any)
			if invoices["truncated"] != true || len(invoices["entries"].([]any)) != 100 {
				t.Fatal("invoices uncapped")
			}
			f.change(func() { delete(f.payloads["/v1/accounts/acct_test/billing/payments"].(map[string]any), "payments") })
			out = readAccountOK(t, h, ResourceAccountBilling, "")
			if out["payments"].(map[string]any)["available"] != false {
				t.Fatal("missing array became empty success")
			}
		})
	}
}

func TestAccountReaderLoopbackExactParity(t *testing.T) {
	_, cfg := newAccountFixture(t)
	reader := newAccountHarness(t, "reader", cfg)
	web := newAccountHarness(t, "web", cfg)
	for resource := ResourceAccountContext; resource <= ResourceAccountClientsScan; resource++ {
		id := ""
		if resource == ResourceAccountSupportTicket {
			id = "ticket_1"
		}
		left := readAccountOK(t, reader, resource, id)
		right := readAccountOK(t, web, resource, id)
		if !reflect.DeepEqual(left, right) {
			t.Fatalf("projection divergence %d", resource)
		}
	}
}

func TestAccountUpstreamForbiddenInvalidatesCachedScan(t *testing.T) {
	for _, mode := range []string{"reader", "web"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg := newAccountFixture(t)
			cfg.AccountClientsScan = func(context.Context, string) (AccountClientsReport, error) { return syntheticAccountClients(), nil }
			h := newAccountHarness(t, mode, cfg)
			_ = readAccountOK(t, h, ResourceAccountClientsScan, "")
			f.change(func() { f.failures["/v1/support/tickets"] = 403 })
			status, _ := h.read(t.Context(), ReadRequest{Resource: ResourceAccountSupport})
			if status != 403 {
				t.Fatal("backend authorization ignored")
			}
			f.change(func() { delete(f.failures, "/v1/support/tickets") })
			out := readAccountOK(t, h, ResourceAccountClients, "")
			if out["status"] != "not_checked" {
				t.Fatal("cache survived observed authorization denial")
			}
		})
	}
}

func TestAccountErrorsDoNotChangeAgentErrorCodes(t *testing.T) {
	for _, code := range []string{"forbidden", "unavailable", "response_too_large"} {
		raw, _ := json.Marshal(map[string]any{"error": code})
		if readerErrorCode(raw) != "" {
			t.Fatal("existing agent error behavior changed")
		}
		raw, _ = json.Marshal(map[string]any{"schema_version": AccountSchema, "error": code})
		if readerErrorCode(raw) != code {
			t.Fatal("account code missing")
		}
	}
}

func TestAccountReversedAuthorityResponses(t *testing.T) {
	for _, mode := range []string{"reader", "web"} {
		for _, resource := range []Resource{ResourceAccountContext, ResourceAccountOverview} {
			t.Run(fmt.Sprintf("%s/%d", mode, resource), func(t *testing.T) {
				f, cfg := newAccountFixture(t)
				held, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				f.beforeResponse = func(r *http.Request, count int) {
					if r.URL.Path == "/v1/whoami" && count == 1 {
						close(held)
						select {
						case <-release:
						case <-r.Context().Done():
						}
					}
				}
				h := newAccountHarness(t, mode, cfg)
				type result struct {
					status int
					raw    []byte
				}
				done := make(chan result, 1)
				go func() {
					status, raw := h.read(t.Context(), ReadRequest{Resource: resource})
					done <- result{status, raw}
				}()
				select {
				case <-held:
				case <-time.After(5 * time.Second):
					t.Fatal("first identity check never arrived")
				}
				readAccountOK(t, h, ResourceAccountOverview, "")
				unblock()
				got := <-done
				var out map[string]any
				if got.status != 200 || json.Unmarshal(got.raw, &out) != nil || out["available"] != true {
					t.Fatalf("unchanged authority rejected: status=%d body=%s", got.status, got.raw)
				}
			})
		}
	}
}

func TestAccountSupportRetainsNewestMessages(t *testing.T) {
	for _, mode := range []string{"reader", "web"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg := newAccountFixture(t)
			for _, count := range []int{101, 102} {
				f.change(func() {
					messages := make([]any, count)
					for i := range messages {
						messages[i] = map[string]any{"id": fmt.Sprintf("message_%03d", i), "account_id": "acct_test", "ticket_id": "ticket_1", "author_kind": "support", "body": fmt.Sprintf("reply %d", i)}
					}
					f.payloads["/v1/support/tickets/ticket_1"].(map[string]any)["messages"] = messages
				})
				h := newAccountHarness(t, mode, cfg)
				out := readAccountOK(t, h, ResourceAccountSupportTicket, "ticket_1")
				messages := out["messages"].([]any)
				if out["truncated"] != true || len(messages) != 100 {
					t.Fatal("missing row bound")
				}
				for i, message := range messages {
					index := count - 100 + i
					row := message.(map[string]any)
					if row["id"] != fmt.Sprintf("message_%03d", index) || row["body"] != fmt.Sprintf("reply %d", index) {
						t.Fatalf("newest chronological window lost at row %d: %v", i, row)
					}
				}
			}
		})
	}
}

func TestAccountBillingStalledHistory(t *testing.T) {
	for _, mode := range []string{"reader", "web"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg := newAccountFixture(t)
			stalled := make(chan struct{})
			f.beforeResponse = func(r *http.Request, _ int) {
				if r.URL.Path == "/v1/accounts/acct_test/billing/invoices" {
					close(stalled)
					<-r.Context().Done()
				}
			}
			h := newAccountHarness(t, mode, cfg)
			out := readAccountOK(t, h, ResourceAccountBilling, "")
			select {
			case <-stalled:
			default:
				t.Fatal("invoices never started")
			}
			for _, key := range []string{"summary", "payments"} {
				if out[key].(map[string]any)["available"] != true {
					t.Fatalf("healthy %s discarded", key)
				}
			}
			invoices := out["invoices"].(map[string]any)
			if invoices["available"] != false || invoices["error"] != "unavailable" || invoices["entries"] != nil {
				t.Fatal("stalled history did not remain independently unavailable")
			}
		})
	}
}

func TestAccountReversedAuthorityStillInvalidates(t *testing.T) {
	for _, change := range []string{"revoked", "role", "older_failure"} {
		t.Run(change, func(t *testing.T) {
			f, cfg := newAccountFixture(t)
			c := newAccountCollector(cfg)
			if _, _, err := c.authority(t.Context()); err != nil {
				t.Fatal(err)
			}
			c.cached = &AccountClientsReport{}
			held, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			f.change(func() {
				f.revoked = change == "older_failure"
				f.beforeResponse = func(r *http.Request, count int) {
					if r.URL.Path == "/v1/whoami" && count == 2 {
						close(held)
						select {
						case <-release:
						case <-r.Context().Done():
						}
					}
				}
			})
			done := make(chan error, 1)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			go func() { _, _, err := c.authority(ctx); done <- err }()
			select {
			case <-held:
			case <-ctx.Done():
				t.Fatal("first check never arrived")
			}
			f.change(func() {
				f.revoked = change == "revoked"
				if change == "role" {
					f.principal["account_role"] = "account_operator"
				}
			})
			_, _, err := c.authority(ctx)
			if (err != nil) != (change == "revoked") {
				t.Fatalf("unexpected newer authority result: %v", err)
			}
			unblock()
			if err := <-done; err == nil {
				t.Fatal("stale or failed authority accepted")
			}
			c.mu.Lock()
			cached, principal := c.cached, c.principal
			c.mu.Unlock()
			if cached != nil {
				t.Fatal("observed authority change retained cached work")
			}
			if change == "role" && principal.Role != "account_operator" {
				t.Fatal("older success overwrote latest principal")
			}
			if change != "role" && principal != (AccountManagerIdentity{}) {
				t.Fatal("old success revived authority")
			}
			f.change(func() { f.revoked = false; f.principal["account_role"] = "account_owner" })
			p, _, err := c.authority(ctx)
			if err != nil || p.Role != "account_owner" {
				t.Fatal("legitimate validation did not recover")
			}
		})
	}
}

// Cancel one identity request while unrelated scan work or a cached report exists.
func TestAccountAuthorityCancellationIsLocal(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, superseded := range []bool{false, true} {
			t.Run(fmt.Sprintf("cached=%t/superseded=%t", cached, superseded), func(t *testing.T) {
				f, cfg := newAccountFixture(t)
				scanStarted, releaseScan := make(chan struct{}), make(chan struct{})
				var once sync.Once
				release := func() { once.Do(func() { close(releaseScan) }) }
				defer release()
				cfg.AccountClientsScan = func(ctx context.Context, _ string) (AccountClientsReport, error) {
					if !cached {
						close(scanStarted)
						select {
						case <-releaseScan:
						case <-ctx.Done():
							return AccountClientsReport{}, ctx.Err()
						}
					}
					return syntheticAccountClients(), nil
				}
				h := newAccountHarness(t, "reader", cfg)
				scanDone := make(chan int, 1)
				if cached {
					readAccountOK(t, h, ResourceAccountClientsScan, "")
				} else {
					go func() {
						status, _ := h.read(t.Context(), ReadRequest{Resource: ResourceAccountClientsScan})
						scanDone <- status
					}()
					<-scanStarted
				}
				held := make(chan struct{})
				target := f.count("/v1/whoami") + 1
				f.change(func() {
					f.beforeResponse = func(r *http.Request, count int) {
						if r.URL.Path == "/v1/whoami" && count == target {
							close(held)
							<-r.Context().Done()
						}
					}
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan int, 1)
				go func() { status, _ := h.read(ctx, ReadRequest{Resource: ResourceAccountContext}); done <- status }()
				<-held
				if superseded {
					readAccountOK(t, h, ResourceAccountContext, "")
				}
				cancel()
				if status := <-done; status != 504 {
					t.Fatalf("cancel status=%d", status)
				}
				if !cached {
					release()
					if status := <-scanDone; status != 200 {
						t.Errorf("unrelated scan status=%d", status)
					}
				}
				if out := readAccountOK(t, h, ResourceAccountClients, ""); out["status"] != "checked" {
					t.Error("cancellation invalidated report")
				}
			})
		}
	}
}

func TestAccountScannerClosedCallbackContract(t *testing.T) {
	cases := map[string]func(*AccountClientsReport){
		"schema":              func(r *AccountClientsReport) { r.SchemaVersion = "private" },
		"device":              func(r *AccountClientsReport) { r.DeviceLabel = "private-host" },
		"scan_status":         func(r *AccountClientsReport) { r.ScanStatus = "diagnostic" },
		"missing_scan_status": func(r *AccountClientsReport) { r.ScanStatus = "" },
		"checked_at":          func(r *AccountClientsReport) { r.CheckedAt = "private-path" },
		"zero_checked_at":     func(r *AccountClientsReport) { r.CheckedAt = "0001-01-01T00:00:00Z" },
		"invalid_calendar":    func(r *AccountClientsReport) { r.CheckedAt = "2026-02-30T00:00:00Z" },
		"invalid_offset":      func(r *AccountClientsReport) { r.CheckedAt = "2026-09-24T00:00:00+24:00" },
		"unchecked_time":      func(r *AccountClientsReport) { r.CheckedAt = "2026-09-24T00:00:00+99:99" },
		"installed_at":        func(r *AccountClientsReport) { r.Entries[0].InstalledAt = "private-path" },
		"runtime":             func(r *AccountClientsReport) { r.Entries[0].Runtime = "unknown" },
		"executable":          func(r *AccountClientsReport) { r.Entries[0].ExecutableStatus = "running" },
		"configuration":       func(r *AccountClientsReport) { r.Entries[0].ConfigurationStatus = "configured" },
		"scope":               func(r *AccountClientsReport) { r.Entries[0].ConfigurationScope = "private-path" },
		"effective":           func(r *AccountClientsReport) { r.Entries[0].EffectiveVerification = "not_checked" },
		"version":             func(r *AccountClientsReport) { r.Entries[0].RecordedVersion = "diagnostic private-path" },
		"duplicate":           func(r *AccountClientsReport) { r.Entries = append(r.Entries, r.Entries[0]) },
		"over_limit":          func(r *AccountClientsReport) { r.Entries = make([]AccountClientEntry, 9) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, cfg := newAccountFixture(t)
			report := syntheticAccountClients()
			cfg.AccountClientsScan = func(context.Context, string) (AccountClientsReport, error) { return report, nil }
			h := newAccountHarness(t, "reader", cfg)
			readAccountOK(t, h, ResourceAccountClientsScan, "")
			mutate(&report)
			if status, _ := h.read(t.Context(), ReadRequest{Resource: ResourceAccountClientsScan}); status != 502 {
				t.Fatalf("malformed report accepted: %d", status)
			}
			got := readAccountOK(t, h, ResourceAccountClients, "")["report"].(map[string]any)
			raw, _ := json.Marshal(got)
			expected, _ := json.Marshal(syntheticAccountClients())
			var want map[string]any
			_ = json.Unmarshal(expected, &want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("callback mutation changed cached copy: %s", raw)
			}
		})
	}
}

func TestAccountScannerStatusesAndVersions(t *testing.T) {
	for _, status := range []string{"complete", "partial", "unavailable"} {
		r := syntheticAccountClients()
		r.ScanStatus = status
		r.Entries = nil
		got, err := sanitizeAccountClients(r)
		if err != nil || got.ScanStatus != status || got.Entries == nil {
			t.Fatalf("lost empty scan status %s", status)
		}
	}
	for _, exe := range []string{"present", "missing", "unchecked"} {
		for _, config := range []string{"match", "incomplete", "changed", "unavailable", "unsupported"} {
			for _, scope := range []string{"none", "mcp_registration"} {
				r := syntheticAccountClients()
				r.Entries[0].ExecutableStatus = exe
				r.Entries[0].ConfigurationStatus = config
				r.Entries[0].ConfigurationScope = scope
				if _, err := sanitizeAccountClients(r); err != nil {
					t.Fatal("valid closed value rejected")
				}
			}
		}
	}
	for _, version := range []string{"", "1.2.3", "v1.2.3", "0.0.0", "999999999.1.2-rc.999999999", "1.2.3-alpha", "1.2.3-beta.0", "1.2.3-rc.1", "2026.07.16-899851b", "2024.02.29-0123456789abcdef"} {
		if !validAccountClientVersion("cursor", version) {
			t.Errorf("valid version rejected %q", version)
		}
	}
	for _, version := range []string{"v2026.07.16-899851b", "2026.02.29-899851b", "2026.13.16-899851b", "2026.07.16-899851B", "2026.07.16-899851", "2026.07.16-0123456789abcdef0", "1.2.3+private", "1.2.3-diagnostic", "01.2.3", "1.2.3-rc.01", "1000000000.2.3", "1.2.3\n", strings.Repeat("v", 49)} {
		if validAccountClientVersion("cursor", version) {
			t.Errorf("unsafe version accepted %q", version)
		}
	}
	if validAccountClientVersion("codex", "2026.07.16-899851b") {
		t.Fatal("Cursor format accepted for Codex")
	}
	r := syntheticAccountClients()
	r.Truncated = true
	got, err := sanitizeAccountClients(r)
	if err != nil || !got.Truncated || got.ScanStatus != "partial" {
		t.Fatal("truncated scan presented as complete")
	}
}

func TestAccountOversizeThreadCodeAndAuthority(t *testing.T) {
	for _, mode := range []string{"reader", "web"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg := newAccountFixture(t)
			cfg.AccountClientsScan = func(context.Context, string) (AccountClientsReport, error) { return syntheticAccountClients(), nil }
			h := newAccountHarness(t, mode, cfg)
			readAccountOK(t, h, ResourceAccountClientsScan, "")
			f.change(func() {
				detail := f.payloads["/v1/support/tickets/ticket_1"].(map[string]any)
				rows := make([]any, 65)
				for i := range rows {
					rows[i] = map[string]any{"id": fmt.Sprintf("message_%03d", i), "account_id": "acct_test", "ticket_id": "ticket_1", "body": strings.Repeat("x", 65536)}
				}
				detail["messages"] = rows
				encoded, _ := json.Marshal(detail)
				if len(encoded) <= 4<<20 {
					t.Error("fixture must exceed cap")
				}
			})
			in := ReadRequest{Resource: ResourceAccountSupportTicket, ID: "ticket_1"}
			if mode == "reader" {
				_, err := h.reader.Read(t.Context(), in)
				var re *ReaderError
				if !errors.As(err, &re) || re.Status != 502 || re.Code != "response_too_large" {
					t.Fatalf("reader classification: %v", err)
				}
			} else {
				status, raw := h.read(t.Context(), in)
				var out map[string]any
				_ = json.Unmarshal(raw, &out)
				if status != 502 || out["error"] != "response_too_large" || out["schema_version"] != AccountSchema || len(out) != 3 {
					t.Fatalf("web classification: %d %s", status, raw)
				}
			}
			if readAccountOK(t, h, ResourceAccountContext, "")["available"] != true {
				t.Fatal("oversize revoked authority")
			}
			if readAccountOK(t, h, ResourceAccountClients, "")["status"] != "checked" {
				t.Fatal("oversize cleared cache")
			}
			// Below the transport cap, valid maximum-size messages retain the body cap.
			f.change(func() {
				d := f.payloads["/v1/support/tickets/ticket_1"].(map[string]any)
				d["messages"] = d["messages"].([]any)[:63]
			})
			out := readAccountOK(t, h, ResourceAccountSupportTicket, "ticket_1")
			if out["truncated"] != true || len(out["messages"].([]any)) != 63 {
				t.Fatal("bounded thread failed")
			}
			for _, row := range out["messages"].([]any) {
				if len(row.(map[string]any)["body"].(string)) != accountBodyBytes {
					t.Fatal("body cap changed")
				}
			}
		})
	}
}

func TestAccountOversizeIdentityPreservesCache(t *testing.T) {
	f, cfg := newAccountFixture(t)
	cfg.AccountClientsScan = func(context.Context, string) (AccountClientsReport, error) { return syntheticAccountClients(), nil }
	h := newAccountHarness(t, "reader", cfg)
	readAccountOK(t, h, ResourceAccountClientsScan, "")
	f.change(func() { f.principal["private_padding"] = strings.Repeat("x", 2<<20) })
	if readAccountOK(t, h, ResourceAccountContext, "")["available"] != false {
		t.Fatal("oversize context accepted")
	}
	_, err := h.reader.Read(t.Context(), ReadRequest{Resource: ResourceAccountOverview})
	var re *ReaderError
	if !errors.As(err, &re) || re.Code != "response_too_large" {
		t.Fatalf("identity error: %v", err)
	}
	f.change(func() { delete(f.principal, "private_padding") })
	if readAccountOK(t, h, ResourceAccountClients, "")["status"] != "checked" {
		t.Fatal("oversize identity cleared cache")
	}
}
