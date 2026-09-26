package main

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agenttui"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
)

func TestTUIConsoleOwnedStopDuringCellOutage(t *testing.T) {
	consoleTestHome(t)
	var offline atomic.Bool
	conn := consoleTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		if offline.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"identity": consoleTestIdentity()})
	})
	c := consoleTestController(context.Background(), t, conn)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	child := c.child
	offline.Store(true)
	status, err := c.Stop(context.Background())
	if err != nil || status.State != agenttui.ConsoleStopped {
		t.Fatalf("owned stop during outage: state=%v error=%v", status.State, err)
	}
	select {
	case <-child.done:
	case <-time.After(3 * time.Second):
		t.Fatal("owned child was not reaped")
	}
}

func TestTUIConsoleRestartsCrashedOwnedChild(t *testing.T) {
	consoleTestHome(t)
	c := consoleTestController(context.Background(), t, consoleTestBackend(t, nil))
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	crashed := c.child
	if err := crashed.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-crashed.done:
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic child did not terminate")
	}
	status, err := c.Start(context.Background())
	if err != nil || status.State != agenttui.ConsoleRunning || !status.Owned || c.child == crashed {
		t.Fatalf("restart after crash: state=%v owned=%v error=%v", status.State, status.Owned, err)
	}
}

func TestTUIConsoleRetainsCleanupFenceAfterLockTimeout(t *testing.T) {
	consoleTestHome(t)
	c := consoleTestController(context.Background(), t, consoleTestBackend(t, nil))
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	child := c.child
	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-child.done
	locked, unlock, released := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := dashboard.WithRegistryInstance(context.Background(), child.entry, func() error {
			close(locked)
			<-unlock
			return nil
		})
		released <- err
	}()
	select {
	case <-locked:
	case err := <-released:
		t.Fatalf("could not hold synthetic claim lock: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("claim lock was not acquired")
	}
	_, firstErr := c.Start(context.Background())
	retained := c.child == child
	close(unlock)
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if firstErr == nil || !retained {
		t.Fatal("cleanup timeout discarded the retryable ownership fence")
	}
	if status, err := c.Start(context.Background()); err != nil || status.State != agenttui.ConsoleRunning || !status.Owned {
		t.Fatalf("cleanup retry did not recover: state=%v error=%v", status.State, err)
	}
}

func TestTUIConsoleSlowBrowserDoesNotBlockServeCleanup(t *testing.T) {
	consoleTestHome(t)
	conn := consoleTestBackend(t, nil)
	owner := consoleTestController(context.Background(), t, conn)
	if _, err := owner.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	child := owner.child
	reuser := consoleTestController(context.Background(), t, conn)
	entered := make(chan struct{})
	reuser.opener = func(ctx context.Context, _ string) error {
		close(entered)
		select {
		case <-time.After(2800 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	opened := make(chan error, 1)
	go func() { _, err := reuser.Open(context.Background()); opened <- err }()
	select {
	case <-entered:
	case err := <-opened:
		t.Fatalf("synthetic opener was not reached: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic opener timed out")
	}
	child.closePipe()
	select {
	case <-child.done:
	case <-time.After(6 * time.Second):
		t.Fatal("console shutdown did not complete")
	}
	if err := <-opened; err != nil {
		t.Fatalf("synthetic browser launch: %v", err)
	}
	// Inspect before owner.Close can repair the record: serve cleanup must be
	// sufficient for an ordinary standalone console with no retained owner.
	if _, err := dashboard.ReadRegistryInstance(owner.identity.AgentID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("slow browser stranded the exiting console's registry record")
	}
}

func TestTUIConsoleChildIdentityBudgetMatchesParent(t *testing.T) {
	consoleTestHome(t)
	identity := client.SelfIdentity{
		AccountID: "acc_" + strings.Repeat("a", 16), AgentID: "agt_" + strings.Repeat("b", 16),
		RealmID: "realm_" + strings.Repeat("c", 16), AgentName: strings.Repeat("a", 255), RealmName: strings.Repeat("r", 255),
	}
	// Match the production self envelope, including capacities which are
	// always present and cannot be trimmed even with content flags disabled.
	digest := client.SelfDigest{SchemaVersion: "witself.v0", Identity: identity,
		PrimaryFacts: []client.SelfFact{}, SalientMemories: []client.SelfMemory{},
		FactCapacity: &client.FactLimitStatus{Unlimited: true}, MemoryCapacity: &client.MemoryLimitStatus{Unlimited: true},
		Index: client.SelfIndex{Kinds: []string{}, Tags: []string{}, Counts: map[string]int{}}, Elided: true,
	}
	raw, err := json.Marshal(digest)
	if err != nil || len(raw) <= 1024 || len(raw) > 8192 {
		t.Fatal("fixture does not exercise the valid identity budget boundary")
	}
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		budget := 8192 // Same default as the production self handler.
		if value := r.URL.Query().Get("max_bytes"); value != "" {
			budget, _ = strconv.Atoi(value)
		}
		if len(raw) > budget {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(raw)
	}))
	defer cell.Close()
	if _, err := client.GetSelf(context.Background(), cell.URL, consoleFakeToken, client.SelfOptions{Observational: true}); err != nil {
		t.Fatal("parent identity fixture failed")
	}
	bootstrap := tuiConsoleBootstrap{Identity: identity, Connection: agentConnection{Endpoint: cell.URL, Token: consoleFakeToken, AccountID: identity.AccountID}}
	if err := verifyConsoleChildIdentity(context.Background(), bootstrap); err != nil {
		t.Fatal("managed console rejected an identity accepted by parent startup")
	}
}

// This subprocess publishes only AFTER the parent closes its lifetime pipe.
// It deliberately stalls so cleanup must exercise its bounded forced exit.
func TestTUIConsoleLatePublicationProcess(_ *testing.T) {
	if os.Args[len(os.Args)-1] != "console-test-late-publish" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	raw, err := reader.ReadBytes('\n')
	if err != nil {
		os.Exit(2)
	}
	var bootstrap tuiConsoleBootstrap
	if json.Unmarshal(raw, &bootstrap) != nil {
		os.Exit(2)
	}
	_, _ = io.Copy(io.Discard, reader)
	entry := dashboard.RegistryEntry{
		SchemaVersion: dashboard.RegistrySchemaVersion,
		AgentID:       bootstrap.Identity.AgentID, AccountID: bootstrap.Identity.AccountID,
		RealmID: bootstrap.Identity.RealmID, Endpoint: bootstrap.Connection.Endpoint,
		PID: os.Getpid(), Port: 51235, StartedAt: time.Now().UTC(),
		URL: "http://127.0.0.1:51235/", AccessURL: "http://127.0.0.1:51235/?token=" + bootstrap.AccessToken,
	}
	if dashboard.WriteRegistryEntry(entry) != nil {
		os.Exit(2)
	}
	if os.WriteFile(filepath.Join(os.Getenv("WITSELF_HOME"), "late-publication-proof"), []byte("published"), 0o600) != nil {
		os.Exit(2)
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

func TestTUIConsoleCanceledStartupReleasesLatePublication(t *testing.T) {
	consoleTestHome(t)
	c := consoleTestController(context.Background(), t, consoleTestBackend(t, nil))
	c.command = func() (*exec.Cmd, error) {
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return exec.Command(executable, "-test.run=^TestTUIConsoleLatePublicationProcess$", "--", "console-test-late-publish"), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := c.Start(ctx)
	if err == nil || strings.Contains(err.Error(), consoleFakeToken) {
		t.Fatal("canceled startup did not return a private failure")
	}
	if c.child != nil {
		t.Fatal("forced cleanup retained child")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("WITSELF_HOME"), "late-publication-proof")); err != nil {
		t.Fatal("synthetic child did not exercise late publication")
	}
	if _, err := dashboard.ReadRegistryInstance(c.identity.AgentID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("late registry publication survived forced cleanup")
	}
}

func TestTUIConsoleRecoversUnownedDeadRecord(t *testing.T) {
	consoleTestHome(t)
	conn := consoleTestBackend(t, nil)
	owner := consoleTestController(context.Background(), t, conn)
	if _, err := owner.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	dead := owner.child
	if err := dead.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-dead.done
	// Another TUI has no retained child handle and must recover the record itself.
	reuser := consoleTestController(context.Background(), t, conn)
	status, err := reuser.Start(context.Background())
	if err != nil || status.State != agenttui.ConsoleRunning || !status.Owned {
		t.Fatal("unowned dead record prevented restart")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	consoleExpect(t, reuser, agenttui.ConsoleRunning, true)
}

func TestTUIConsolePreservesForeignRecords(t *testing.T) {
	for _, differentAgent := range []bool{false, true} {
		t.Run(strconv.FormatBool(differentAgent), func(t *testing.T) {
			consoleTestHome(t)
			conn := consoleTestBackend(t, nil)
			entry := consoleTestEntry(t, conn, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) })
			if differentAgent {
				entry.AgentID = "agt_other"
			}
			consoleTestWriteEntry(t, entry)
			path, _ := dashboard.RegistryPath(consoleTestIdentity().AgentID)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			c := consoleTestController(context.Background(), t, conn)
			status, err := c.Start(context.Background())
			if err != errConsoleConflict || status.State != agenttui.ConsoleConflict {
				t.Fatal("foreign record was adopted")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("foreign record changed")
			}
		})
	}
}

func TestTUIConsoleBrowserUnknownIsInformational(t *testing.T) {
	consoleTestHome(t)
	c := consoleTestController(context.Background(), t, consoleTestBackend(t, nil))
	c.opener = func(context.Context, string) error { return agenttui.ErrBrowserOutcomeUnknown }
	status, err := c.Open(context.Background())
	if status.State != agenttui.ConsoleRunning || !errors.Is(err, agenttui.ErrBrowserOutcomeUnknown) {
		t.Fatal("launch timeout lost informational outcome")
	}
}

func TestTUIConsoleReusesReaderPrincipalAndRejectsObservedDenial(t *testing.T) {
	consoleTestHome(t)
	identity := consoleTestIdentity()
	var checks atomic.Int32
	var denied atomic.Bool
	var changedRole atomic.Bool
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" || r.Header.Get("Authorization") != "Bearer "+accountFakeBearer {
			t.Error("unexpected private authority request")
			w.WriteHeader(403)
			return
		}
		checks.Add(1)
		if denied.Load() {
			w.WriteHeader(403)
			return
		}
		role := "account_owner"
		if changedRole.Load() {
			role = "account_admin"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "principal": map[string]any{"kind": "operator", "account_id": identity.AccountID, "operator_id": "op_synthetic", "account_role": role}})
	}))
	defer cp.Close()
	manager := &dashboard.AccountManager{Endpoint: cp.URL, BearerToken: accountFakeBearer, Identity: dashboard.AccountManagerIdentity{AccountID: identity.AccountID, OperatorID: "op_synthetic", Role: "account_owner"}}
	conn := agentConnection{Endpoint: cp.URL, Token: consoleFakeToken, AccountID: identity.AccountID}
	reader, err := dashboard.NewReader(dashboard.Config{Endpoint: cp.URL, BearerToken: conn.Token, Identity: identity, AccountManager: manager})
	if err != nil {
		t.Fatal(err)
	}
	c := newTUIConsole(context.Background(), conn, identity, time.Second, accountConsoleOptions{Manager: manager})
	defer func() { _ = c.Close() }()
	model := agenttui.New(context.Background(), &tuiSecretSource{Source: reader}, agenttui.Options{Console: c})
	defer model.Close()
	if c.authority == nil {
		t.Fatal("model did not bind reader authority")
	}
	// Cold discovery validates once; subsequent Account checks remain fresh reads.
	if err := c.authority(context.Background(), manager.Identity); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(context.Background(), dashboard.ReadRequest{Resource: dashboard.ResourceAccountContext}); err != nil {
		t.Fatal(err)
	}
	before := checks.Load()
	for range 6 {
		if err := c.authority(context.Background(), manager.Identity); err != nil {
			t.Fatal(err)
		}
	}
	if checks.Load() != before {
		t.Fatal("fresh principal triggered duplicate control-plane requests")
	}
	other := manager.Identity
	other.OperatorID = "op_other"
	if c.authority(context.Background(), other) == nil {
		t.Fatal("principal reused for another operator")
	}
	changedRole.Store(true)
	if _, err := reader.Read(context.Background(), dashboard.ReadRequest{Resource: dashboard.ResourceAccountContext}); err != nil {
		t.Fatal(err)
	}
	before = checks.Load()
	if err := c.authority(context.Background(), manager.Identity); err != nil || checks.Load() != before {
		t.Fatal("recognized role change broke principal reuse")
	}
	denied.Store(true)
	if _, err := reader.Read(context.Background(), dashboard.ReadRequest{Resource: dashboard.ResourceAccountContext}); err != nil {
		t.Fatal(err)
	}
	if c.authority(context.Background(), manager.Identity) == nil {
		t.Fatal("observed denial reused cached authority")
	}
}

func TestTUIConsoleNetworkIdentityProcess(_ *testing.T) {
	if os.Args[len(os.Args)-1] != "console-test-network-identity" {
		return
	}
	var bootstrap tuiConsoleBootstrap
	if json.NewDecoder(os.Stdin).Decode(&bootstrap) != nil || verifyConsoleChildIdentity(context.Background(), bootstrap) != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestTUIConsoleChildIdentityUsesProxyAndCustomCA(t *testing.T) {
	for _, mode := range []string{"proxy", "custom-ca"} {
		t.Run(mode, func(t *testing.T) {
			consoleTestHome(t)
			var requests atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer "+consoleFakeToken {
					t.Error("incorrect identity request")
					w.WriteHeader(403)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"identity": consoleTestIdentity()})
			})
			for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
				t.Setenv(key, "")
			}
			endpoint := "http://synthetic-cell.invalid"
			if mode == "proxy" {
				proxy := httptest.NewServer(handler)
				defer proxy.Close()
				t.Setenv("HTTP_PROXY", proxy.URL)
			} else {
				cell := httptest.NewTLSServer(handler)
				defer cell.Close()
				endpoint = cell.URL
				ca := filepath.Join(t.TempDir(), "synthetic-ca.pem")
				if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cell.Certificate().Raw}), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("SSL_CERT_FILE", ca)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(executable, "-test.run=^TestTUIConsoleNetworkIdentityProcess$", "--", "console-test-network-identity")
			command.Env = consoleChildEnvironment()
			command.Stdin = strings.NewReader(consoleBootstrapJSON(t, tuiConsoleBootstrap{Connection: agentConnection{Endpoint: endpoint, Token: consoleFakeToken}, Identity: consoleTestIdentity()}))
			if err := command.Run(); err != nil {
				t.Fatal("child identity networking failed")
			}
			if requests.Load() != 1 {
				t.Fatal("identity did not reach synthetic endpoint")
			}
		})
	}
}
