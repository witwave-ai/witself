package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
)

// These bindings are fixture inputs, not a local operator bootstrap policy.
const (
	offlineAPIOperatorBearer = "offline-inventory-operator-bearer"
	offlineAPIAgentBearer    = "offline-inventory-agent-bearer"
	offlineAPIAgentID        = "agent_offline_inventory"
	offlineAPIOtherAccount   = "acct_offline_inventory_other"
)

type offlineAPIFixture struct {
	mu             sync.Mutex
	want           client.AccountConsoleIdentity
	current        client.AccountConsoleIdentity
	revoked        bool
	otherOverview  bool
	requests       map[string]int
	accountLookups int
	cell           *httptest.Server
	identity       client.SelfIdentity
	manager        client.AccountConsoleManager
}

func newOfflineAPIFixture(t *testing.T) *offlineAPIFixture {
	t.Helper()
	for _, key := range []string{"HOME", "USERPROFILE", "WITSELF_HOME", "DSH_HOME"} {
		t.Setenv(key, t.TempDir())
	}
	f := &offlineAPIFixture{
		want: client.AccountConsoleIdentity{
			AccountID: "acct_offline_inventory", OperatorID: "operator_offline_inventory", Role: "account_owner",
		},
		requests: make(map[string]int),
	}
	f.current = f.want
	mux := apiMux(Config{
		Authenticate: func(_ context.Context, bearer string) (string, string, string, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if bearer != offlineAPIOperatorBearer || f.revoked {
				return "", "", "", false, nil
			}
			return f.current.OperatorID, f.current.AccountID, "active", true, nil
		},
		AuthenticatePrincipal: func(_ context.Context, bearer string) (DomainPrincipal, bool, error) {
			if bearer != offlineAPIAgentBearer {
				return DomainPrincipal{}, false, nil
			}
			return DomainPrincipal{
				Kind: PrincipalKindAgent, ID: offlineAPIAgentID, AgentName: "offline-inventory",
				AccountID: f.want.AccountID, AccountStatus: "active",
				RealmID: "realm_offline_inventory", RealmName: "default",
			}, true, nil
		},
		GetOperatorAccountRole: func(_ context.Context, accountID, operatorID string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if accountID != f.current.AccountID || operatorID != f.current.OperatorID {
				t.Error("role lookup must receive the authenticated binding")
			}
			return f.current.Role, nil
		},
		GetAccount: func(_ context.Context, accountID string) (AccountRecord, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.accountLookups++
			if accountID != f.current.AccountID || accountID != f.want.AccountID {
				t.Error("account lookup must receive exactly the authenticated account")
			}
			if f.otherOverview {
				return AccountRecord{ID: offlineAPIOtherAccount, Status: "active"}, nil
			}
			return AccountRecord{ID: accountID, Status: "active"}, nil
		},
	})
	f.cell = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests[r.URL.Path]++
		f.mu.Unlock()
		switch r.URL.Path {
		case "/v1/self", "/v1/whoami", "/v1/account":
		default:
			t.Error("fixture received an unexpected API request")
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.cell.Close)
	resp := offlineAPIRequest(t, f, "/v1/self", offlineAPIAgentBearer)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatal("real self handler must recognize the fixture agent")
	}
	var self client.SelfDigest
	if json.NewDecoder(resp.Body).Decode(&self) != nil || self.Identity.AccountID != f.want.AccountID || self.Identity.AgentID != offlineAPIAgentID {
		t.Fatal("real self handler must return the expected agent binding")
	}
	f.identity = self.Identity
	principal, err := client.VerifyAccountConsoleManager(t.Context(), f.cell.URL, offlineAPIOperatorBearer, self.Identity.AccountID)
	if err != nil || principal != f.want || f.count("/v1/whoami") != 1 {
		t.Fatal("real whoami must verify exactly the explicit operator binding")
	}
	f.manager = client.AccountConsoleManager{Endpoint: f.cell.URL, BearerToken: offlineAPIOperatorBearer, Identity: principal}
	return f
}

func offlineAPIRequest(t *testing.T, f *offlineAPIFixture, path, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.cell.URL+path, nil)
	if err != nil {
		t.Fatal("fixture request construction failed")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := f.cell.Client().Do(req)
	if err != nil {
		t.Fatal("fixture API request failed")
	}
	return resp
}

func (f *offlineAPIFixture) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[path]
}

func (f *offlineAPIFixture) change(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *offlineAPIFixture) reader(t *testing.T, scan dashboard.AccountClientsScan) *dashboard.Reader {
	t.Helper()
	r, err := dashboard.NewReader(dashboard.Config{
		Endpoint: f.cell.URL, BearerToken: offlineAPIAgentBearer, Identity: f.identity,
		AccountManager: &f.manager, AccountClientsScan: scan,
	})
	if err != nil {
		t.Fatal("fixture account reader construction failed")
	}
	return r
}

func offlineAPIReport() dashboard.AccountClientsReport {
	return dashboard.AccountClientsReport{
		SchemaVersion: dashboard.AccountClientsSchema, DeviceLabel: "This device",
		CheckedAt: "2026-01-02T03:04:05Z", ScanStatus: "complete",
		Entries: []dashboard.AccountClientEntry{{
			Runtime: "codex", RecordedVersion: "1.2.3", ExecutableStatus: "present",
			ConfigurationStatus: "match", ConfigurationScope: "mcp_registration", EffectiveVerification: "not_run",
		}},
	}
}

func offlineAPIRead(t *testing.T, r *dashboard.Reader, resource dashboard.Resource) json.RawMessage {
	t.Helper()
	raw, err := r.Read(t.Context(), dashboard.ReadRequest{Resource: resource})
	if err != nil {
		t.Fatal("authorized account view must be available")
	}
	return raw
}

func offlineAPIClients(t *testing.T, r *dashboard.Reader, resource dashboard.Resource, status string) {
	t.Helper()
	raw := offlineAPIRead(t, r, resource)
	var out struct {
		Available bool                            `json:"available"`
		Status    string                          `json:"status"`
		Report    *dashboard.AccountClientsReport `json:"report"`
	}
	if json.Unmarshal(raw, &out) != nil || !out.Available || out.Status != status {
		t.Fatal("clients view must have the expected available scan status")
	}
	if status == "not_checked" {
		if out.Report != nil {
			t.Fatal("unchecked clients must not disclose a cached inventory")
		}
	} else if out.Report == nil || !reflect.DeepEqual(*out.Report, offlineAPIReport()) {
		t.Fatal("clients view must contain exactly the bounded synthetic report")
	}
}

func offlineAPIBinding(t *testing.T, r *dashboard.Reader, resource dashboard.Resource, want client.AccountConsoleIdentity, available bool) {
	t.Helper()
	raw := offlineAPIRead(t, r, resource)
	var out struct {
		Available  bool   `json:"available"`
		AccountID  string `json:"account_id"`
		OperatorID string `json:"operator_id"`
		Role       string `json:"role"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Available != available {
		t.Fatal("account binding availability must follow current authority")
	}
	if available && (out.AccountID != want.AccountID || out.OperatorID != want.OperatorID || out.Role != want.Role) {
		t.Fatal("account view must expose exactly the verified operator binding")
	}
	if !available && (out.AccountID != "" || out.OperatorID != "" || out.Role != "") {
		t.Fatal("unavailable account context must disclose no principal")
	}
}

func offlineAPIForbidden(t *testing.T, r *dashboard.Reader, resource dashboard.Resource) {
	t.Helper()
	raw, err := r.Read(t.Context(), dashboard.ReadRequest{Resource: resource})
	var denied *dashboard.ReaderError
	if !errors.As(err, &denied) || denied.Status != http.StatusForbidden || denied.Code != "forbidden" || len(raw) != 0 {
		t.Fatal("changed authority must forbid account reads and disclose no inventory")
	}
}

func TestAccountConsoleOfflineInventoryAPI(t *testing.T) {
	f := newOfflineAPIFixture(t)
	var scans atomic.Int32
	r := f.reader(t, func(ctx context.Context, accountID string) (dashboard.AccountClientsReport, error) {
		if accountID != f.want.AccountID {
			t.Error("scan must receive exactly the authenticated account")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 10*time.Second {
			t.Error("scan must receive a bounded context")
		}
		scans.Add(1)
		return offlineAPIReport(), nil
	})
	for _, resource := range []dashboard.Resource{dashboard.ResourceAccountContext, dashboard.ResourceAccountAccess} {
		offlineAPIBinding(t, r, resource, f.want, true)
	}
	raw := offlineAPIRead(t, r, dashboard.ResourceAccountOverview)
	var overview struct {
		Available bool `json:"available"`
		Account   struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if json.Unmarshal(raw, &overview) != nil || !overview.Available || overview.Account.ID != f.want.AccountID || bytes.Contains(raw, []byte(offlineAPIOtherAccount)) {
		t.Fatal("overview must contain only the authenticated account")
	}
	f.mu.Lock()
	lookups := f.accountLookups
	f.mu.Unlock()
	if lookups != 1 || f.count("/v1/account") != 1 {
		t.Fatal("overview must use the real authenticated account handler exactly once")
	}
	offlineAPIClients(t, r, dashboard.ResourceAccountClients, "not_checked")
	if scans.Load() != 0 {
		t.Fatal("ordinary account views must not invoke inventory")
	}
	offlineAPIClients(t, r, dashboard.ResourceAccountClientsScan, "checked")
	offlineAPIClients(t, r, dashboard.ResourceAccountClients, "checked")
	if scans.Load() != 1 {
		t.Fatal("explicit inventory must be cached without ordinary rescans")
	}

	resp := offlineAPIRequest(t, f, "/v1/whoami", offlineAPIAgentBearer)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("known agent must be rejected by real operator whoami")
	}
	principal, err := client.VerifyAccountConsoleManager(t.Context(), f.cell.URL, offlineAPIAgentBearer, f.want.AccountID)
	if !errors.Is(err, client.ErrAccountConsoleForbidden) || principal != (client.AccountConsoleIdentity{}) {
		t.Fatal("known agent must not verify as an account manager")
	}
	// A mistaken account callback cannot leak another account through projection.
	f.change(func() { f.otherOverview = true })
	raw, err = r.Read(t.Context(), dashboard.ReadRequest{Resource: dashboard.ResourceAccountOverview})
	var unavailable *dashboard.ReaderError
	if !errors.As(err, &unavailable) || unavailable.Status != http.StatusBadGateway || unavailable.Code != "unavailable" || len(raw) != 0 {
		t.Fatal("mismatched account overview must fail without disclosing another account")
	}
}

func TestAccountConsoleOfflineInventoryAuthorityLoss(t *testing.T) {
	for _, transition := range []string{"revoked", "operator", "account", "role"} {
		t.Run(transition, func(t *testing.T) {
			f := newOfflineAPIFixture(t)
			var scans atomic.Int32
			r := f.reader(t, func(_ context.Context, accountID string) (dashboard.AccountClientsReport, error) {
				if accountID != f.want.AccountID {
					t.Error("scan must preserve its original account binding")
				}
				scans.Add(1)
				return offlineAPIReport(), nil
			})
			offlineAPIClients(t, r, dashboard.ResourceAccountClientsScan, "checked")
			offlineAPIClients(t, r, dashboard.ResourceAccountClients, "checked")
			f.change(func() {
				switch transition {
				case "revoked":
					f.revoked = true
				case "operator":
					f.current.OperatorID = "operator_offline_replacement"
				case "account":
					f.current.AccountID = offlineAPIOtherAccount
				case "role":
					f.current.Role = "unrecognized"
				}
			})
			// Read clients first: the denial must fence the already-cached report.
			offlineAPIForbidden(t, r, dashboard.ResourceAccountClients)
			offlineAPIBinding(t, r, dashboard.ResourceAccountContext, f.want, false)
			for _, resource := range []dashboard.Resource{dashboard.ResourceAccountAccess, dashboard.ResourceAccountOverview, dashboard.ResourceAccountClientsScan} {
				offlineAPIForbidden(t, r, resource)
			}
			if scans.Load() != 1 || f.count("/v1/account") != 0 {
				t.Fatal("authority loss must stop both scans and account lookup work")
			}
			f.change(func() { f.revoked, f.current = false, f.want })
			offlineAPIClients(t, r, dashboard.ResourceAccountClients, "not_checked")
			if scans.Load() != 1 {
				t.Fatal("authority restoration must require an explicit rescan")
			}
			offlineAPIClients(t, r, dashboard.ResourceAccountClientsScan, "checked")
			if scans.Load() != 2 {
				t.Fatal("restored authority must explicitly rebuild its inventory")
			}
		})
	}

	t.Run("revoked-during-scan", func(t *testing.T) {
		f := newOfflineAPIFixture(t)
		deadline := time.Now().Add(5 * time.Second)
		if testDeadline, ok := t.Deadline(); ok && testDeadline.Before(deadline) {
			deadline = testDeadline
		}
		ctx, cancel := context.WithDeadline(t.Context(), deadline)
		defer cancel()
		started, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		var scans atomic.Int32
		r := f.reader(t, func(ctx context.Context, accountID string) (dashboard.AccountClientsReport, error) {
			if accountID != f.want.AccountID {
				t.Error("slow scan must preserve the authenticated account")
			}
			if scans.Add(1) == 2 {
				close(started)
				select {
				case <-ctx.Done():
					return dashboard.AccountClientsReport{}, ctx.Err()
				case <-release:
				}
			}
			return offlineAPIReport(), nil
		})
		offlineAPIClients(t, r, dashboard.ResourceAccountClientsScan, "checked")
		before := f.count("/v1/whoami")
		type result struct {
			raw json.RawMessage
			err error
		}
		done := make(chan result, 1)
		go func() {
			raw, err := r.Read(ctx, dashboard.ReadRequest{Resource: dashboard.ResourceAccountClientsScan})
			done <- result{raw: raw, err: err}
		}()
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("slow scan did not reach the channel handshake")
		}
		if f.count("/v1/whoami") != before+1 {
			t.Fatal("slow scan must start after exactly one authority check")
		}
		f.change(func() { f.revoked = true })
		// No authority reader runs here: only this scan's second validation can
		// observe the revocation before its result is published or cached.
		releaseOnce.Do(func() { close(release) })
		select {
		case got := <-done:
			var denied *dashboard.ReaderError
			if !errors.As(got.err, &denied) || denied.Status != http.StatusForbidden || denied.Code != "forbidden" || len(got.raw) != 0 {
				t.Fatal("released scan must fail post-scan authorization without publishing inventory")
			}
		case <-ctx.Done():
			t.Fatal("released scan did not complete within its bounded context")
		}
		if f.count("/v1/whoami") != before+2 || scans.Load() != 2 {
			t.Fatal("slow result must be rejected by its own second authority check")
		}
		f.change(func() { f.revoked = false })
		offlineAPIClients(t, r, dashboard.ResourceAccountClients, "not_checked")
		if scans.Load() != 2 {
			t.Fatal("rejected slow result must not cause an implicit rescan")
		}
		offlineAPIClients(t, r, dashboard.ResourceAccountClientsScan, "checked")
		if scans.Load() != 3 {
			t.Fatal("rejected slow result must require a fresh explicit scan")
		}
	})
}
