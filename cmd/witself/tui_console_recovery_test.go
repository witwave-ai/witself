package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agenttui"
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
