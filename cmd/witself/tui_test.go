package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/witwave-ai/witself/internal/agenttui"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/dashboard/stubcell"
)

func isolateAgentTUI(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	for _, key := range []string{"HOME", "WITSELF_HOME", "DSH_HOME"} {
		t.Setenv(key, filepath.Join(home, key))
	}
	for _, key := range []string{"WITSELF_ACCOUNT", "WITSELF_REALM", "WITSELF_AGENT"} {
		t.Setenv(key, "")
	}
	oldTerminal, oldRunner := agentTUITerminal, runAgentTUI
	agentTUITerminal = func() bool { return true }
	t.Cleanup(func() { agentTUITerminal, runAgentTUI = oldTerminal, oldRunner })
}

func TestAgentTUIDemoDoesNotResolveConnection(t *testing.T) {
	isolateAgentTUI(t)
	// These point at nonexistent managed credentials. Demo must ignore them.
	t.Setenv("WITSELF_ACCOUNT", "missing-account")
	t.Setenv("WITSELF_AGENT", "missing-agent")
	t.Setenv("WITSELF_REALM", "missing-realm")
	called := false
	runAgentTUI = func(ctx context.Context, source agenttui.Source, opts agenttui.Options) error {
		called = true
		if !opts.Demo || opts.Agent != "Atlas" || opts.Realm != "studio" || opts.Theme != "paper" {
			t.Fatal("demo options did not describe the synthetic workspace")
		}
		if _, err := source.Read(ctx, dashboard.ReadRequest{Resource: dashboard.ResourceSelf}); err != nil {
			t.Fatalf("demo self: %v", err)
		}
		return nil
	}
	if got := agentTUI(context.Background(), []string{"--demo", "--theme", "paper"}); got != 0 || !called {
		t.Fatalf("exit=%d runner=%v", got, called)
	}
	if _, err := os.Stat(os.Getenv("WITSELF_HOME")); !os.IsNotExist(err) {
		t.Fatal("demo created managed account state")
	}
}

func TestAgentTUIRejectsInvalidOrNoninteractiveBeforeConnection(t *testing.T) {
	isolateAgentTUI(t)
	runAgentTUI = func(context.Context, agenttui.Source, agenttui.Options) error {
		t.Fatal("invalid invocation started the TUI")
		return nil
	}
	for _, args := range [][]string{
		{"--demo", "--agent", "real"}, {"--demo", "--token-file", "/does/not/exist"},
		{"--theme", "bogus"}, {"--poll", "0s"}, {"--poll", "1h"}, {"unexpected"},
	} {
		if got := agentTUI(context.Background(), args); got != 2 {
			t.Errorf("args %v: exit=%d, want2", args, got)
		}
	}
	agentTUITerminal = func() bool { return false }
	if got := agentTUI(context.Background(), []string{"--endpoint", "http://127.0.0.1:1", "--token-file", "/does/not/exist"}); got != 2 {
		t.Fatalf("redirected invocation reached credential resolution: exit=%d", got)
	}
	if got := agentTUI(context.Background(), []string{"--help"}); got != 0 {
		t.Fatalf("help requires a terminal: exit=%d", got)
	}
}

func TestAgentTUIAuthenticatesIdentityAndUsesObservationalReader(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching", true: "wrong-agent"}[mismatch], func(t *testing.T) {
			isolateAgentTUI(t)
			identity := stubcell.Identity("atlas")
			if mismatch {
				identity.AgentName = "somebody-else"
			}
			requests := 0
			cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet || r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer synthetic-tui-token" {
					t.Error("unexpected method, route or authentication")
				}
				if r.URL.Query().Get("include_facts") != "false" {
					t.Error("identity or overview fetched facts implicitly")
				}
				_ = json.NewEncoder(w).Encode(client.SelfDigest{Identity: identity})
			}))
			defer cell.Close()
			tokenFile := filepath.Join(t.TempDir(), "synthetic.token")
			if err := os.WriteFile(tokenFile, []byte("synthetic-tui-token"), 0o600); err != nil {
				t.Fatal(err)
			}
			called := false
			runAgentTUI = func(ctx context.Context, source agenttui.Source, opts agenttui.Options) error {
				called = true
				secretSource, ok := source.(*tuiSecretSource)
				if !ok || secretSource.connection.AccountName != "work" {
					t.Fatal("explicit local account was lost for secret custody")
				}
				if opts.Agent != "atlas" || opts.Realm != "default" || opts.Demo {
					t.Fatal("incorrect authenticated options")
				}
				_, err := source.Read(ctx, dashboard.ReadRequest{Resource: dashboard.ResourceSelf})
				return err
			}
			got := agentTUI(context.Background(), []string{"--account", "work", "--agent", "atlas", "--endpoint", cell.URL, "--token-file", tokenFile})
			if mismatch {
				if got != 1 || called || requests != 1 {
					t.Fatalf("identity mismatch was not rejected: exit=%d runner=%v requests=%d", got, called, requests)
				}
			} else if got != 0 || !called || requests != 2 {
				t.Fatalf("matching identity did not start reader: exit=%d runner=%v requests=%d", got, called, requests)
			}
		})
	}
}

func TestTUIClipboardClearIsFencedToLatestCopy(t *testing.T) {
	var writes []string
	c := &tuiClipboard{write: func(_ context.Context, value string) error {
		writes = append(writes, value)
		return nil
	}}
	defer c.Close()
	if err := c.Copy(context.Background(), "synthetic-first"); err != nil {
		t.Fatal(err)
	}
	first := c.generation
	if err := c.Copy(context.Background(), "synthetic-second"); err != nil {
		t.Fatal(err)
	}
	c.clear(first)
	if len(writes) != 2 {
		t.Fatal("expired first copy erased newer copy")
	}
	c.Close()
	if !reflect.DeepEqual(writes, []string{"synthetic-first", "synthetic-second", ""}) {
		t.Fatal("close did not clear exactly once")
	}
	if err := c.Copy(context.Background(), "after-close"); err == nil {
		t.Fatal("copy allowed after close")
	}
}

func TestTUIClipboardCanceledWriteStillClears(t *testing.T) {
	var writes []string
	c := &tuiClipboard{write: func(ctx context.Context, value string) error {
		writes = append(writes, value)
		if value != "" {
			return context.Canceled
		}
		if ctx.Err() != nil {
			return errors.New("cleanup inherited cancellation")
		}
		return nil
	}}
	defer c.Close()
	if err := c.Copy(context.Background(), "synthetic"); err == nil || c.copied || !reflect.DeepEqual(writes, []string{"synthetic", ""}) {
		t.Fatal("canceled clipboard write was not cleared independently")
	}
}
