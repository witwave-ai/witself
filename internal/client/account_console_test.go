package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func consoleWhoami(role string) map[string]any {
	return map[string]any{"schema_version": "witself.v0", "principal": map[string]any{"kind": "operator", "account_id": "acct_test", "operator_id": "op_test", "account_role": role}}
}
func consoleManager(endpoint string) AccountConsoleManager {
	return AccountConsoleManager{Endpoint: endpoint, BearerToken: "fake-manager", Identity: AccountConsoleIdentity{AccountID: "acct_test", OperatorID: "op_test", Role: "account_owner"}}
}
func TestAccountConsoleStrictVerifier(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"schema_version", nil}, {"schema_version", "wrong"}, {"kind", nil}, {"kind", "agent"},
		{"account_id", nil}, {"account_id", "acct_other"}, {"operator_id", nil}, {"operator_id", ""}, {"operator_id", "../bad"},
		{"account_role", nil}, {"account_role", "account_member"}, {"account_role", "future"}, {"account_role", "ACCOUNT_OWNER"},
	} {
		t.Run(tc.key+fmt.Sprint(tc.value), func(t *testing.T) {
			wire := consoleWhoami("account_owner")
			target := wire
			if tc.key != "schema_version" {
				target = wire["principal"].(map[string]any)
			}
			if tc.value == nil {
				delete(target, tc.key)
			} else {
				target[tc.key] = tc.value
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/v1/whoami" || r.Header.Get("Authorization") != "Bearer fake-manager" {
					t.Error("wrong verifier request")
				}
				_ = json.NewEncoder(w).Encode(wire)
			}))
			defer srv.Close()
			p, err := VerifyAccountConsoleManager(t.Context(), srv.URL, "fake-manager", "acct_test")
			if !errors.Is(err, ErrAccountConsoleForbidden) || p != (AccountConsoleIdentity{}) {
				t.Fatal("invalid identity accepted")
			}
		})
	}
	for _, role := range []string{"account_owner", "account_admin", "account_billing", "account_operator"} {
		t.Run(role, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(consoleWhoami(role)) }))
			defer srv.Close()
			p, err := RevalidateAccountConsoleManager(t.Context(), consoleManager(srv.URL))
			if err != nil || p.Role != role {
				t.Fatal("recognized role rejected")
			}
		})
	}
}
func TestAccountConsoleTransportBoundsRedirectAndErrors(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { redirected.Add(1) }))
	defer target.Close()
	for _, tc := range []string{"redirect", "status", "bytes", "chunked", "malformed", "cancel"} {
		t.Run(tc, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch tc {
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusFound)
				case "status":
					http.Error(w, "sensitive provider error fake-manager", 500)
				case "bytes", "chunked":
					if tc == "chunked" {
						w.(http.Flusher).Flush()
					}
					_, _ = w.Write([]byte(strings.Repeat(" ", 2<<20) + `{}`))
				case "malformed":
					_, _ = w.Write([]byte(`{"secret":"fake-manager"`))
				case "cancel":
					<-r.Context().Done()
				}
			}))
			defer srv.Close()
			// Only the cancel case relies on the deadline; the bounded-body cases must
			// hit the byte limit before any timer fires, even on a loaded runner.
			timeout := 10 * time.Second
			if tc == "cancel" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			_, err := VerifyAccountConsoleManager(ctx, srv.URL, "fake-manager", "acct_test")
			want := ErrAccountConsoleUnavailable
			if tc == "bytes" || tc == "chunked" {
				want = ErrAccountConsoleResponseTooLarge
			}
			if !errors.Is(err, want) {
				t.Fatalf("unexpected error %v", err)
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect followed")
	}
}
func TestAccountConsoleClosedReads(t *testing.T) {
	var role atomic.Value
	role.Store("account_owner")
	var operator atomic.Value
	operator.Store("op_test")
	var mode atomic.Value
	mode.Store("")
	var cpCalls atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cpCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fake-manager" || r.Method != "GET" || !strings.HasPrefix(r.URL.Path, "/v1/accounts/acct_test/") {
			t.Error("wrong CP request")
		}
		if mode.Load() == "redirect" {
			http.Redirect(w, r, "/unexpected", http.StatusFound)
			return
		}
		if mode.Load() == "bytes" {
			_, _ = w.Write([]byte(strings.Repeat(" ", 2<<20) + `{}`))
			return
		}
		account := "acct_test"
		if mode.Load() == "wrong_account" {
			account = "acct_other"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "account_id": account})
	}))
	defer cp.Close()
	var readCalls atomic.Int32
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("mutation")
		}
		if r.URL.Path == "/v1/capabilities" {
			if r.Header.Get("Authorization") != "" {
				t.Error("credential sent to capability")
			}
			if mode.Load() == "cap_bytes" {
				_, _ = w.Write([]byte(strings.Repeat(" ", 2<<20) + `{}`))
				return
			}
			endpoint := cp.URL
			account := "acct_test"
			kind := "managed"
			switch mode.Load() {
			case "unsafe":
				endpoint = "http://remote.invalid"
			case "cap_account":
				kind = "self_hosted"
				account = "acct_wrong"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "backend": map[string]any{"kind": kind}, "account": map[string]any{"id": account}, "billing": map[string]any{"supported": true, "endpoint": endpoint}})
			return
		}
		if r.Header.Get("Authorization") != "Bearer fake-manager" {
			t.Error("wrong cell credential")
		}
		if r.URL.Path == "/v1/whoami" {
			wire := consoleWhoami(role.Load().(string))
			wire["principal"].(map[string]any)["operator_id"] = operator.Load()
			_ = json.NewEncoder(w).Encode(wire)
			return
		}
		readCalls.Add(1)
		if mode.Load() == "detail_bytes" {
			_, _ = w.Write([]byte(strings.Repeat(" ", 4<<20) + `{}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0"})
	}))
	defer cell.Close()
	m := consoleManager(cell.URL)
	for _, resource := range []AccountConsoleResource{AccountConsoleOverview, AccountConsoleSupport, AccountConsoleSupportTicket, AccountConsolePlan, AccountConsoleBillingSummary, AccountConsoleInvoices, AccountConsolePayments} {
		id := ""
		if resource == AccountConsoleSupportTicket {
			id = "ticket_1"
		}
		if _, err := ReadAccountConsole(t.Context(), m, resource, id); err != nil {
			t.Fatalf("read %d: %v", resource, err)
		}
	}
	before := cpCalls.Load()
	role.Store("account_operator")
	if _, err := ReadAccountConsole(t.Context(), m, AccountConsolePlan, ""); !errors.Is(err, ErrAccountConsoleForbidden) || cpCalls.Load() != before {
		t.Fatal("billing role bypass")
	}
	role.Store("account_owner")
	operator.Store("op_other")
	if _, err := ReadAccountConsole(t.Context(), m, AccountConsoleOverview, ""); !errors.Is(err, ErrAccountConsoleForbidden) {
		t.Fatal("operator rebound")
	}
	operator.Store("op_test")
	for _, v := range []string{"unsafe", "cap_account", "cap_bytes", "wrong_account", "bytes", "redirect"} {
		mode.Store(v)
		want := ErrAccountConsoleUnavailable
		if v == "bytes" || v == "cap_bytes" {
			want = ErrAccountConsoleResponseTooLarge
		}
		if _, err := ReadAccountConsole(t.Context(), m, AccountConsolePlan, ""); !errors.Is(err, want) {
			t.Fatalf("bad routing %s: %v", v, err)
		}
	}
	mode.Store("detail_bytes")
	if _, err := ReadAccountConsole(t.Context(), m, AccountConsoleSupportTicket, "ticket_1"); !errors.Is(err, ErrAccountConsoleResponseTooLarge) {
		t.Fatal("detail bytes uncapped")
	}
	before = readCalls.Load()
	for _, id := range []string{"../bad", "x/y", "x?x", "x%2f"} {
		_, _ = ReadAccountConsole(t.Context(), m, AccountConsoleSupportTicket, id)
	}
	_, _ = ReadAccountConsole(t.Context(), m, 255, "")
	_, _ = ReadAccountConsole(t.Context(), m, AccountConsoleOverview, "unexpected")
	if readCalls.Load() != before {
		t.Fatal("closed request validation bypass")
	}
}
func TestAccountConsolePrivateConfigurationAndOrigins(t *testing.T) {
	m := consoleManager("https://example.invalid")
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), m.BearerToken) || strings.Contains(fmt.Sprintf("%+v %#v", m, m), m.BearerToken) {
		t.Fatal("manager serialized")
	}
	for _, endpoint := range []string{"https://example.invalid/path", "https://u:p@example.invalid", "https://example.invalid?", "https://example.invalid#", "http://remote.invalid", "https://example.invalid:0", "https://example.invalid:65536", "https://example.invalid:", "https://example.invalid\\evil"} {
		if _, err := AccountConsoleOrigin(endpoint); err == nil {
			t.Fatalf("unsafe origin accepted %q", endpoint)
		}
	}
	if got, err := AccountConsoleOrigin("https://EXAMPLE.invalid:443/"); err != nil || got != "https://example.invalid" {
		t.Fatal("normalization")
	}
}

func TestAccountConsoleExactResponseCap(t *testing.T) {
	for _, limit := range []int{2 << 20, 4 << 20} {
		for _, chunked := range []bool{false, true} {
			for _, extra := range []int{-1, 0, 1} {
				t.Run(fmt.Sprintf("limit=%d/chunked=%t/extra=%d", limit, chunked, extra), func(t *testing.T) {
					// A valid JSON response at each boundary, not an oversized diagnostic.
					payload := `{"schema_version":"witself.v0","padding":"` + strings.Repeat("x", limit+extra-len(`{"schema_version":"witself.v0","padding":""}`)) + `"}`
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						if chunked {
							w.(http.Flusher).Flush()
						} else {
							w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
						}
						_, _ = w.Write([]byte(payload))
					}))
					defer srv.Close()
					var out json.RawMessage
					err := accountConsoleJSON(t.Context(), srv.URL, "fake-manager", int64(limit), &out)
					if extra > 0 {
						if !errors.Is(err, ErrAccountConsoleResponseTooLarge) {
							t.Fatalf("overflow classification: %v", err)
						}
					} else if err != nil || len(out) != len(payload) {
						t.Fatalf("boundary response rejected: %v", err)
					}
				})
			}
		}
	}
}
