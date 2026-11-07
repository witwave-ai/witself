package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/clientinventory"
	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/local"
)

const (
	offlineInventoryOperator     = "op_offline_fixture"
	offlineInventoryOtherAccount = "acc_offline_excluded"
	offlineInventoryExcluded     = "excluded_inventory_private"
	offlineInventoryVersion      = "1.2.3"
	offlineInventoryInstalled    = "2025-01-02T03:04:05Z"
)

type offlineInventoryFile struct {
	bytes    []byte
	mode     os.FileMode
	size     int64
	modified time.Time
}

type offlineInventoryFixture struct {
	roots                                                accountConsoleRoots
	name, endpoint, marker, executable, provider, record string
	files                                                []string
	requests                                             atomic.Int32
	directory, credentials, scans                        int
	locate                                               accountLocator
}

func newOfflineInventoryFixture(t *testing.T, name string) *offlineInventoryFixture {
	t.Helper()
	// consoleTestHome pins HOME/WITSELF_HOME/DSH_HOME, but not USERPROFILE.
	// Keep all four coherent and below one test-owned root on every platform.
	home := t.TempDir()
	f := &offlineInventoryFixture{name: name, roots: accountConsoleRoots{Home: home, WitselfHome: filepath.Join(home, ".witself"), DSHHome: filepath.Join(home, ".dsh")}}
	for key, value := range map[string]string{"HOME": home, "USERPROFILE": home, "WITSELF_HOME": f.roots.WitselfHome, "DSH_HOME": f.roots.DSHHome, "WITSELF_ACCOUNT": ""} {
		t.Setenv(key, value)
	}
	f.locate = accountConsoleRouteBlackhole(&f.directory)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.URL.Path != "/v1/whoami" || r.Header.Get("Authorization") != "Bearer "+accountFakeBearer {
			t.Error("unexpected offline fixture request")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "principal": map[string]string{"kind": "operator", "account_id": consoleTestIdentity().AccountID, "operator_id": offlineInventoryOperator, "account_role": "account_admin"}})
	}))
	t.Cleanup(server.Close)
	f.endpoint = server.URL
	accountConsoleRouteSave(t, name, &local.Route{Kind: "local", Endpoint: server.URL}, accountFakeBearer)
	previousResolve, previousScan := accountConsoleResolve, accountConsoleScan
	accountConsoleResolve = func(selected string) (string, local.Account, string, error) {
		f.credentials++
		return local.Resolve(selected)
	}
	accountConsoleScan = func(ctx context.Context, opts clientinventory.Options) (clientinventory.Report, error) {
		f.scans++
		return clientinventory.Scan(ctx, opts)
	}
	t.Cleanup(func() { accountConsoleResolve, accountConsoleScan = previousResolve, previousScan })
	f.marker = filepath.Join(home, "provider-executed")
	f.executable = filepath.Join(home, "bin", "cursor-agent")
	f.provider = filepath.Join(home, ".cursor", "mcp.json")
	f.record = filepath.Join(f.roots.WitselfHome, "integrations", "cursor", "config.json")
	f.write(t, f.executable, []byte("#!/bin/sh\n: > "+strconv.Quote(f.marker)+"\n"), 0o700)
	f.writeJSON(t, f.provider, map[string]any{"mcpServers": map[string]any{"witself": map[string]any{
		"command": f.executable, "args": []string{"mcp", "serve", "--runtime", "cursor", "--account", name, "--realm", "default", "--agent", "offline-fixture"}, "env": map[string]string{"WITSELF_HOME": f.roots.WitselfHome},
	}}})
	f.writeRecord(t, offlineInventoryVersion)
	f.writeJSON(t, filepath.Join(f.roots.WitselfHome, "integrations", "codex", "config.json"), map[string]any{
		"schema_version": "witself.capture.v1", "runtime": "codex", "account_id": offlineInventoryOtherAccount, "runtime_version": "8.7.6", "account": offlineInventoryExcluded,
		"runtime_cli_command": f.executable, "runtime_mcp_config_path": f.provider,
	})
	f.files = append(f.files, filepath.Join(f.roots.WitselfHome, "config.json"))
	return f
}

func (f *offlineInventoryFixture) write(t *testing.T, path string, raw []byte, mode os.FileMode) {
	t.Helper()
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil || os.WriteFile(path, raw, mode) != nil {
		t.Fatal("write bounded inventory fixture failed")
	}
	f.files = append(f.files, path)
}

func (f *offlineInventoryFixture) writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal("encode bounded inventory fixture failed")
	}
	f.write(t, path, raw, 0o600)
}

func (f *offlineInventoryFixture) writeRecord(t *testing.T, version string) {
	t.Helper()
	f.writeJSON(t, f.record, map[string]any{
		"schema_version": "witself.capture.v1", "runtime": "cursor", "account_id": consoleTestIdentity().AccountID,
		"runtime_version": version, "installed_at": offlineInventoryInstalled, "runtime_cli_command": f.executable,
		"runtime_config_root": filepath.Dir(f.provider), "runtime_mcp_config_path": f.provider, "mcp_command": f.executable,
		"mcp_environment": map[string]string{"WITSELF_HOME": f.roots.WitselfHome}, "account": f.name, "realm": "default", "agent": "offline-fixture",
		"private_sentinel": accountFakeBearer, "endpoint": f.endpoint,
	})
}

func (f *offlineInventoryFixture) snapshot(t *testing.T) map[string]offlineInventoryFile {
	t.Helper()
	out := make(map[string]offlineInventoryFile)
	// Only the enumerated non-credential fixtures are opened; never walk home.
	for _, path := range f.files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("read non-secret fixture snapshot failed")
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal("stat fixture snapshot failed")
		}
		out[path] = offlineInventoryFile{raw, info.Mode(), info.Size(), info.ModTime()}
	}
	return out
}

func (f *offlineInventoryFixture) unchanged(t *testing.T, before map[string]offlineInventoryFile) {
	t.Helper()
	if !reflect.DeepEqual(before, f.snapshot(t)) {
		t.Error("inventory changed fixture bytes or metadata")
	}
	if _, err := os.Stat(f.marker); !errors.Is(err, os.ErrNotExist) {
		t.Error("inventory executed provider stub")
	}
}

func (f *offlineInventoryFixture) open(t *testing.T, selector string) (*dashboard.Reader, *dashboard.AccountManager) {
	t.Helper()
	name, account, err := local.ResolveAccount(selector)
	if err != nil || account.Route == nil {
		t.Fatal("persisted offline account selection failed")
	}
	conn := agentConnection{Endpoint: account.Route.Endpoint, Token: consoleFakeToken, AccountName: name, AccountID: account.ID}
	manager := resolveAccountConsoleManager(context.Background(), conn, consoleTestIdentity(), false, f.locate)
	if manager == nil || f.directory != 0 {
		t.Fatalf("offline manager unavailable or directory consulted (directory calls: %d)", f.directory)
	}
	want := client.AccountConsoleIdentity{AccountID: consoleTestIdentity().AccountID, OperatorID: offlineInventoryOperator, Role: "account_admin"}
	if manager.Identity != want || manager.Endpoint != f.endpoint || manager.BearerToken != accountFakeBearer {
		t.Fatal("offline manager binding changed")
	}
	reader, err := dashboard.NewReader(dashboard.Config{Endpoint: conn.Endpoint, BearerToken: conn.Token, Identity: consoleTestIdentity(), AccountManager: manager, AccountClientsScan: newAccountConsoleScanner(context.Background(), f.roots, manager)})
	if err != nil {
		t.Fatal("offline reader construction failed")
	}
	return reader, manager
}

type offlineInventoryProjection struct {
	Available  bool                            `json:"available"`
	AccountID  string                          `json:"account_id"`
	OperatorID string                          `json:"operator_id"`
	Role       string                          `json:"role"`
	Status     string                          `json:"status"`
	Report     *dashboard.AccountClientsReport `json:"report"`
}

func offlineInventoryRead(t *testing.T, reader *dashboard.Reader, resource dashboard.Resource) (offlineInventoryProjection, json.RawMessage) {
	t.Helper()
	raw, err := reader.Read(context.Background(), dashboard.ReadRequest{Resource: resource})
	var out offlineInventoryProjection
	if err != nil || json.Unmarshal(raw, &out) != nil {
		t.Fatal("offline account projection failed")
	}
	return out, raw
}

func offlineInventoryDenied(t *testing.T, reader *dashboard.Reader, resource dashboard.Resource, status int, code string) {
	t.Helper()
	raw, err := reader.Read(context.Background(), dashboard.ReadRequest{Resource: resource})
	var readerErr *dashboard.ReaderError
	if !errors.As(err, &readerErr) || readerErr.Status != status || readerErr.Code != code || len(raw) != 0 {
		t.Fatal("offline denial contract changed or disclosed inventory")
	}
}

func (f *offlineInventoryFixture) scan(t *testing.T, reader *dashboard.Reader, version string) *dashboard.AccountClientsReport {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		offlineInventoryDenied(t, reader, dashboard.ResourceAccountClientsScan, http.StatusBadGateway, "unavailable")
		out, _ := offlineInventoryRead(t, reader, dashboard.ResourceAccountClients)
		if !out.Available || out.Status != "not_checked" || out.Report != nil {
			t.Fatal("unsupported scanner published inventory")
		}
		return nil
	}
	out, raw := offlineInventoryRead(t, reader, dashboard.ResourceAccountClientsScan)
	if !out.Available || out.Status != "checked" || out.Report == nil {
		t.Fatal("explicit offline inventory unavailable")
	}
	report := out.Report
	want := dashboard.AccountClientEntry{Runtime: "cursor", RecordedVersion: version, ExecutableStatus: "present", ConfigurationStatus: "match", ConfigurationScope: "mcp_registration", EffectiveVerification: "not_run", InstalledAt: offlineInventoryInstalled}
	checked, err := time.Parse(time.RFC3339Nano, report.CheckedAt)
	if report.SchemaVersion != dashboard.AccountClientsSchema || report.DeviceLabel != "This device" || report.ScanStatus != "complete" || report.Truncated || err != nil || checked.IsZero() || checked.After(time.Now()) || len(report.Entries) != 1 || report.Entries[0] != want {
		t.Fatal("selected account bounded inventory metadata changed")
	}
	for _, private := range []string{f.roots.Home, f.roots.WitselfHome, f.roots.DSHHome, f.executable, f.provider, f.record, accountFakeBearer, consoleFakeToken, f.name, f.endpoint, consoleTestIdentity().AccountID, offlineInventoryOperator, offlineInventoryOtherAccount, offlineInventoryExcluded, "8.7.6"} {
		encoded, _ := json.Marshal(private)
		if bytes.Contains(raw, []byte(private)) || bytes.Contains(raw, encoded[1:len(encoded)-1]) {
			t.Fatal("inventory disclosed private fixture data")
		}
	}
	return report
}

func TestAccountConsoleOfflineInventory(t *testing.T) {
	for _, row := range []struct{ name, saved, selector string }{{"named", "offline-selected", "offline-selected"}, {"implicit-default", "default", ""}} {
		t.Run(row.name, func(t *testing.T) {
			f := newOfflineInventoryFixture(t, row.saved)
			before := f.snapshot(t)
			reader, manager := f.open(t, row.selector)
			if f.credentials != 1 || f.requests.Load() != 1 {
				t.Fatal("offline resolver crossed credential or operator request boundary")
			}
			for _, resource := range []dashboard.Resource{dashboard.ResourceAccountContext, dashboard.ResourceAccountAccess} {
				out, _ := offlineInventoryRead(t, reader, resource)
				if !out.Available || out.AccountID != manager.Identity.AccountID || out.OperatorID != manager.Identity.OperatorID || out.Role != manager.Identity.Role {
					t.Fatal("offline account view lost verified operator binding")
				}
			}
			initial, _ := offlineInventoryRead(t, reader, dashboard.ResourceAccountClients)
			if !initial.Available || initial.Status != "not_checked" || initial.Report != nil || f.scans != 0 {
				t.Fatal("ordinary initial clients read scanned or published inventory")
			}
			if f.directory != 0 || f.requests.Load() != 6 {
				t.Fatal("offline initial views crossed request boundary")
			}
			report := f.scan(t, reader, offlineInventoryVersion)
			cached, _ := offlineInventoryRead(t, reader, dashboard.ResourceAccountClients)
			wantStatus := "checked"
			if report == nil {
				wantStatus = "not_checked"
			}
			if !cached.Available || cached.Status != wantStatus || !reflect.DeepEqual(cached.Report, report) || f.scans != 1 || f.directory != 0 || f.credentials != 1 {
				t.Fatal("ordinary clients read failed to reuse explicit inventory")
			}
			f.unchanged(t, before)
		})
	}
}

func TestAccountConsoleOfflineInventoryDenials(t *testing.T) {
	for _, row := range []struct {
		name     string
		reads    int
		requests int32
	}{
		{"agent-only", 1, 0}, {"recognized-agent", 1, 1}, {"endpoint", 0, 0}, {"empty-endpoint", 0, 0}, {"token-file", 0, 0}, {"empty-token-file", 0, 0},
		{"selected-origin", 0, 0}, {"wrong-principal-account", 1, 1}, {"stale-route", 1, 0}, {"stale-account", 1, 0}, {"stale-name", 1, 0}, {"scanner-account", 1, 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newOfflineInventoryFixture(t, "offline-selected")
			var alternateRequests atomic.Int32
			alternate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				alternateRequests.Add(1)
				kind, accountID := "operator", offlineInventoryOtherAccount
				if row.name == "recognized-agent" {
					kind, accountID = "agent", consoleTestIdentity().AccountID
				}
				if r.URL.Path != "/v1/whoami" || r.Header.Get("Authorization") != "Bearer "+accountFakeBearer {
					t.Error("unexpected alternate fixture request")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "principal": map[string]string{"kind": kind, "account_id": accountID, "operator_id": offlineInventoryOperator, "account_role": "account_admin"}})
			}))
			t.Cleanup(alternate.Close)
			conn := agentConnection{Endpoint: f.endpoint, Token: consoleFakeToken, AccountName: f.name, AccountID: consoleTestIdentity().AccountID}
			explicit := false
			switch row.name {
			case "agent-only":
				if local.RefreshToken(f.name, consoleFakeToken) != nil {
					t.Fatal("replace synthetic bearer failed")
				}
			case "recognized-agent", "wrong-principal-account":
				conn.Endpoint = alternate.URL
				if local.SetRoute(f.name, &local.Route{Kind: "local", Endpoint: alternate.URL}) != nil {
					t.Fatal("replace synthetic route failed")
				}
			case "selected-origin":
				conn.Endpoint = alternate.URL
			case "endpoint", "empty-endpoint", "token-file", "empty-token-file":
				fs := flag.NewFlagSet("offline", flag.ContinueOnError)
				fs.String("endpoint", "", "")
				fs.String("token-file", "", "")
				key, value := strings.TrimPrefix(row.name, "empty-"), alternate.URL
				if key == "token-file" {
					value = filepath.Join(f.roots.Home, "explicit-credential")
				}
				if strings.HasPrefix(row.name, "empty-") {
					value = ""
				}
				if fs.Set(key, value) != nil {
					t.Fatal("explicit fixture flag failed")
				}
				explicit = accountConsoleExplicit(fs)
			}
			if strings.HasPrefix(row.name, "stale-") {
				resolve := accountConsoleResolve
				accountConsoleResolve = func(name string) (string, local.Account, string, error) {
					selected, account, bearer, err := resolve(name)
					if err != nil {
						t.Fatal("second fixture credential read failed")
					}
					switch row.name {
					case "stale-route":
						account.Route.Endpoint = alternate.URL
					case "stale-account":
						account.ID = offlineInventoryOtherAccount
					case "stale-name":
						selected = "offline-other"
					}
					return selected, account, bearer, err
				}
			}
			wantPrimary, wantAlternate := row.requests, int32(0)
			if row.name == "recognized-agent" || row.name == "wrong-principal-account" {
				wantPrimary, wantAlternate = 0, row.requests
			}
			manager := resolveAccountConsoleManager(context.Background(), conn, consoleTestIdentity(), explicit, f.locate)
			if row.name == "scanner-account" {
				if manager == nil {
					t.Fatal("scanner mismatch fixture manager unavailable")
				}
				scan := newAccountConsoleScanner(context.Background(), f.roots, manager)
				for _, account := range []string{offlineInventoryOtherAccount, ""} {
					_, err := scan(context.Background(), account)
					if !errors.Is(err, client.ErrAccountConsoleForbidden) || f.scans != 0 {
						t.Error("scanner account mismatch did not forbid before underlying scan")
					}
				}
				if f.directory != 0 || f.credentials != row.reads || f.requests.Load() != wantPrimary || alternateRequests.Load() != wantAlternate {
					t.Error("scanner mismatch crossed resolver request boundary")
				}
				return
			}
			if manager != nil || f.credentials != row.reads || f.requests.Load() != wantPrimary || alternateRequests.Load() != wantAlternate || f.directory != 0 {
				t.Fatal("denied manager crossed credential or operator request boundary")
			}
			reader, err := dashboard.NewReader(dashboard.Config{Endpoint: conn.Endpoint, BearerToken: conn.Token, Identity: consoleTestIdentity(), AccountManager: manager, AccountClientsScan: newAccountConsoleScanner(context.Background(), f.roots, manager)})
			if err != nil {
				t.Fatal("agent-only reader construction failed")
			}
			out, _ := offlineInventoryRead(t, reader, dashboard.ResourceAccountContext)
			if out.Available || out.AccountID != "" || out.OperatorID != "" {
				t.Fatal("denied reader exposed account context")
			}
			for _, resource := range []dashboard.Resource{dashboard.ResourceAccountAccess, dashboard.ResourceAccountClients, dashboard.ResourceAccountClientsScan} {
				offlineInventoryDenied(t, reader, resource, http.StatusForbidden, "forbidden")
			}
			if f.scans != 0 || f.directory != 0 || f.credentials != row.reads || f.requests.Load() != wantPrimary || alternateRequests.Load() != wantAlternate {
				t.Fatal("denied reader performed new inventory or authority work")
			}
		})
	}
}

func TestAccountConsoleOfflineInventoryReopen(t *testing.T) {
	f := newOfflineInventoryFixture(t, "offline-selected")
	first, _ := f.open(t, f.name)
	original := f.scan(t, first, offlineInventoryVersion)
	second, _ := f.open(t, f.name)
	initial, _ := offlineInventoryRead(t, second, dashboard.ResourceAccountClients)
	if initial.Status != "not_checked" || initial.Report != nil || f.scans != 1 {
		t.Fatal("reconstructed reader inherited inventory cache")
	}
	again := f.scan(t, second, offlineInventoryVersion)
	if original != nil && (again == nil || !reflect.DeepEqual(original.Entries, again.Entries)) {
		t.Fatal("reconstructed explicit scan changed persisted metadata")
	}
	f.writeRecord(t, "1.2.4")
	before := f.snapshot(t)
	third, _ := f.open(t, f.name)
	initial, _ = offlineInventoryRead(t, third, dashboard.ResourceAccountClients)
	if initial.Status != "not_checked" || initial.Report != nil || f.scans != 2 {
		t.Fatal("changed fixture became visible without explicit rescan")
	}
	f.scan(t, third, "1.2.4")
	if f.directory != 0 || f.credentials != 3 || f.scans != 3 {
		t.Fatal("reconstruction crossed offline scan boundary")
	}
	f.unchanged(t, before)
}
