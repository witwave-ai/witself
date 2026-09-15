//go:build !windows

package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestCaptureFlushHandoffHelper(t *testing.T) {
	dir := os.Getenv("WITSELF_HANDOFF_FIXTURE_DIR")
	if dir == "" {
		return
	}
	startCaptureFlushProcess = func(_ *exec.Cmd) error {
		if err := os.WriteFile(filepath.Join(dir, "unexpected-successor"), []byte("attempted"), 0o600); err != nil {
			t.Fatal(err)
		}
		return errors.New("fixture forbids a second generation")
	}
	code := transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode})
	if err := os.WriteFile(filepath.Join(dir, "finished"), []byte(fmt.Sprint(code)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Retain the exact child until the parent kills and reaps it in cleanup.
	// The test timeout bounds orphan lifetime if the parent itself crashes.
	time.Sleep(45 * time.Second)
}

func TestCaptureFlushNewArrivalSurvivesOwnerFailure(t *testing.T) {
	runCaptureFlushHandoff(t, "recover")
}

func TestCaptureFlushFatalHandoffControls(t *testing.T) {
	for _, mode := range []string{"persistent", "unchanged", "privacy-held", "binding-held", "spawn-failure", "later-group"} {
		t.Run(mode, func(t *testing.T) { runCaptureFlushHandoff(t, mode) })
	}
}

func runCaptureFlushHandoff(t *testing.T, mode string) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("WITSELF_HOME", filepath.Join(dir, "witself"))
	t.Setenv("DSH_HOME", filepath.Join(dir, "dsh"))
	t.Setenv(captureDetachedFlushEnv, "1")
	t.Setenv("WITSELF_HANDOFF_FIXTURE_DIR", dir)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITSELF_HANDOFF_FIXTURE_BINARY", executable)
	wrapper := filepath.Join(dir, "flush-child")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec \"$WITSELF_HANDOFF_FIXTURE_BINARY\" -test.run='^TestCaptureFlushHandoffHelper$' -test.timeout=60s\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(witselfExecutableTestEnv, wrapper)
	if mode == "spawn-failure" {
		t.Setenv(witselfExecutableTestEnv, filepath.Join(dir, "missing"))
	}
	var childMu sync.Mutex
	var children []*os.Process
	previousStarter := startCaptureFlushProcess
	startCaptureFlushProcess = func(cmd *exec.Cmd) error {
		if err := cmd.Start(); err != nil {
			return err
		}
		childMu.Lock()
		children = append(children, cmd.Process)
		childMu.Unlock()
		return nil
	}
	t.Cleanup(func() {
		startCaptureFlushProcess = previousStarter
		childMu.Lock()
		defer childMu.Unlock()
		for _, child := range children {
			if err := child.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error(err)
			}
			if _, err := child.Wait(); err != nil {
				t.Error(err)
			}
		}
	})

	started := make(chan struct{})
	resume := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(resume) })
	var first sync.Once
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/self/activity":
			http.NotFound(w, r)
		case "/v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_fixture","metadata":{}}}`))
		case "/v1/transcripts/trn_fixture/entries:batch":
			request := requests.Add(1)
			fail := request == 1
			if mode == "later-group" {
				fail = request == 2
			}
			if mode == "persistent" {
				fail = true
			}
			first.Do(func() {
				close(started)
				<-resume
			})
			if fail {
				http.Error(w, "fixture temporary failure", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() {
		release.Do(func() { close(resume) })
		srv.Close()
	}()
	token := filepath.Join(dir, "synthetic.token")
	if err := os.WriteFile(token, []byte("synthetic-fixture-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	location, err := transcriptcapture.EnsureLocation("fixture")
	if err != nil {
		t.Fatal(err)
	}
	cfg := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeClaudeCode, CaptureMode: transcriptcapture.ModeMessages,
		Account: "fixture", Realm: "default", Agent: "fixture", AgentID: "agent_fixture",
		AgentName: "fixture", Location: location, Endpoint: srv.URL, TokenFile: token,
	}
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	enqueue := func(session string) {
		t.Helper()
		if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode,
			[]byte(fmt.Sprintf(`{"session_id":%q,"hook_event_name":"SessionStart"}`, session))); err != nil {
			t.Fatal(err)
		}
	}
	enqueue("first")
	done := make(chan int, 1)
	go func() { done <- transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode}) }()
	ownerFinished := false
	t.Cleanup(func() {
		if !ownerFinished {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("owner cleanup incomplete")
			}
		}
	})
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("owner never reached the controlled request")
	}
	switch mode {
	case "unchanged":
	case "privacy-held":
		if _, err := transcriptcapture.EnqueueHook(cfg.Runtime, []byte(`{"session_id":"second","hook_event_name":"UserPromptSubmit","prompt":"synthetic held turn"}`)); err != nil {
			t.Fatal(err)
		}
	case "binding-held":
		foreign := cfg
		foreign.Agent, foreign.AgentID, foreign.AgentName = "foreign", "agent_foreign", "foreign"
		if err := transcriptcapture.SaveConfig(foreign); err != nil {
			t.Fatal(err)
		}
		enqueue("second")
		if err := transcriptcapture.SaveConfig(cfg); err != nil {
			t.Fatal(err)
		}
	default:
		enqueue("second")
		if mode == "later-group" {
			enqueue("third")
		}
	}
	if code := transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode}); code != 0 {
		t.Fatalf("competing detached trigger returned %d", code)
	}
	release.Do(func() { close(resume) })
	select {
	case code := <-done:
		ownerFinished = true
		if code != 1 {
			t.Fatalf("failed owner returned %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("failed owner did not finish")
	}
	// No later hook and no manual flush is issued. A retained competing
	// trigger must transfer to a new owner after the failed owner's release.
	wantChild := mode == "recover" || mode == "persistent" || mode == "later-group"
	if wantChild {
		deadline := time.Now().Add(15 * time.Second)
		finished := false
		for time.Now().Before(deadline) {
			raw, err := os.ReadFile(filepath.Join(dir, "finished"))
			if err == nil && len(raw) > 0 {
				want := "0"
				if mode == "persistent" {
					want = "1"
				}
				if string(raw) != want {
					t.Fatalf("successor exit = %q, want %s", raw, want)
				}
				finished = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !finished {
			t.Fatal("new arrival stayed queued after its trigger lost the lock and the owner failed")
		}
	}
	// Started handles are recorded synchronously. The helper reports any
	// attempted second generation before it could create another process.
	childMu.Lock()
	starts := len(children)
	childMu.Unlock()
	wantStarts := 0
	if wantChild {
		wantStarts = 1
	}
	if starts != wantStarts {
		t.Fatalf("successor starts = %d, want %d", starts, wantStarts)
	}
	if _, err := os.Stat(filepath.Join(dir, "unexpected-successor")); !os.IsNotExist(err) {
		t.Fatalf("successor attempted another generation: %v", err)
	}
	pending, err := transcriptcapture.Pending(cfg.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	wantPending := 2
	switch mode {
	case "recover", "later-group":
		wantPending = 0
	case "unchanged":
		wantPending = 1
	}
	if len(pending) != wantPending {
		t.Fatalf("pending = %d, want %d", len(pending), wantPending)
	}
}
