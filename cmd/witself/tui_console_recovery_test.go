package main

import (
	"bufio"
	"context"
	"encoding/json"
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
