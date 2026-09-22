package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agenttui"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
)

const consoleFakeToken = "fake-private-bearer-never-display"

func TestTUIConsoleChildProcess(_ *testing.T) {
	if len(os.Args) == 0 {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "console-test-child":
		os.Exit(runTUIConsoleChild(os.Stdin, os.Stdout))
	case "console-test-fail":
		_, _ = fmt.Fprintln(os.Stdout, consoleFakeToken)
		_, _ = fmt.Fprintln(os.Stderr, consoleFakeToken)
		os.Exit(7)
	case "console-test-ignore-eof":
		_, _ = io.Copy(io.Discard, os.Stdin)
		time.Sleep(time.Hour)
		os.Exit(0)
	case "console-test-wait":
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
}

func consoleTestCommand(mode string) func() (*exec.Cmd, error) {
	return func() (*exec.Cmd, error) {
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return exec.Command(executable, "-test.run=^TestTUIConsoleChildProcess$", "--", mode), nil
	}
}

func consoleTestHome(t *testing.T) {
	t.Helper()
	for _, key := range []string{"HOME", "DSH_HOME", "WITSELF_HOME"} {
		t.Setenv(key, t.TempDir())
	}
}

func consoleTestIdentity() client.SelfIdentity {
	return client.SelfIdentity{AccountID: "acc_synthetic", RealmID: "rlm_synthetic", AgentID: "agt_console", AgentName: "synthetic", RealmName: "synthetic-realm"}
}

func consoleTestBackend(t *testing.T, handler http.HandlerFunc) agentConnection {
	t.Helper()
	identity := consoleTestIdentity()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+consoleFakeToken {
			t.Error("incorrect synthetic authorization")
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/v1/self" || r.URL.Query().Get("observational") != "true" {
			t.Error("non-observational console request")
			w.WriteHeader(400)
			return
		}
		if handler != nil {
			handler(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"identity": identity})
	}))
	t.Cleanup(srv.Close)
	return agentConnection{Endpoint: srv.URL, Token: consoleFakeToken, AccountID: identity.AccountID, AccountName: "synthetic-local"}
}

func consoleTestController(ctx context.Context, t *testing.T, conn agentConnection) *tuiConsole {
	t.Helper()
	c := newTUIConsole(ctx, conn, consoleTestIdentity(), time.Second)
	c.command = consoleTestCommand("console-test-child")
	c.opener = func(context.Context, string) error { t.Error("unexpected browser launch"); return nil }
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return c
}

func consoleExpect(t *testing.T, c *tuiConsole, state agenttui.ConsoleState, owned bool) agenttui.ConsoleStatus {
	t.Helper()
	status, err := c.Status(context.Background())
	if err != nil || status.State != state || status.Owned != owned {
		t.Fatalf("status=%+v error=%v", status, err)
	}
	return status
}

func TestTUIConsoleOwnedLifecycle(t *testing.T) {
	consoleTestHome(t)
	c := consoleTestController(context.Background(), t, consoleTestBackend(t, nil))
	consoleExpect(t, c, agenttui.ConsoleStopped, false)
	ctx, cancel := context.WithCancel(context.Background())
	status, err := c.Start(ctx)
	cancel() // Per-action cancellation must not kill the running child.
	if err != nil || status.State != agenttui.ConsoleRunning || !status.Owned {
		t.Fatalf("start=%+v error=%v", status, err)
	}
	port := status.Port
	child := c.child
	if child == nil || child.cmd.Process.Pid == os.Getpid() {
		t.Fatal("dashboard hosted in parent")
	}
	for range 3 {
		if got, err := c.Start(context.Background()); err != nil || got.Port != port || !got.Owned {
			t.Fatalf("repeat=%+v error=%v", got, err)
		}
	}
	opens := 0
	c.opener = func(_ context.Context, accessURL string) error {
		opens++
		if accessURL != child.entry.AccessURL {
			t.Error("wrong private browser target")
		}
		return errors.New(consoleFakeToken + accessURL)
	}
	status, err = c.Open(context.Background())
	if err != errConsoleOpen || !status.Owned || status.Port != port || opens != 1 {
		t.Fatalf("open failure=%+v error=%v", status, err)
	}
	consoleExpect(t, c, agenttui.ConsoleRunning, true)
	c.opener = func(context.Context, string) error { opens++; return nil }
	if _, err := c.Open(context.Background()); err != nil || opens != 2 {
		t.Fatalf("retry: %v", err)
	}
	status, err = c.Stop(context.Background())
	if err != nil || status.State != agenttui.ConsoleStopped {
		t.Fatalf("stop=%+v error=%v", status, err)
	}
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	owned := c.child
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-owned.done:
	default:
		t.Fatal("Close did not reap child")
	}
	if _, err := dashboard.ReadRegistryEntry(c.identity.AgentID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned registry survived Close")
	}
}

func TestTUIConsoleReuseAndExplicitStop(t *testing.T) {
	consoleTestHome(t)
	conn := consoleTestBackend(t, nil)
	owner := consoleTestController(context.Background(), t, conn)
	if _, err := owner.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	reuse := consoleTestController(context.Background(), t, conn)
	consoleExpect(t, reuse, agenttui.ConsoleRunning, false)
	if got, err := reuse.Start(context.Background()); err != nil || got.Owned || reuse.child != nil {
		t.Fatalf("reuse=%+v error=%v", got, err)
	}
	if err := reuse.Close(); err != nil {
		t.Fatal(err)
	}
	consoleExpect(t, owner, agenttui.ConsoleRunning, true)
	stopper := consoleTestController(context.Background(), t, conn)
	status, err := stopper.Stop(context.Background())
	if err != nil || status.State != agenttui.ConsoleStopped {
		t.Fatalf("explicit reused stop=%+v error=%v", status, err)
	}
}

func TestTUIConsoleConcurrentStarts(t *testing.T) {
	consoleTestHome(t)
	conn := consoleTestBackend(t, nil)
	first := consoleTestController(context.Background(), t, conn)
	second := consoleTestController(context.Background(), t, conn)
	var launches atomic.Int32
	command := consoleTestCommand("console-test-child")
	for _, c := range []*tuiConsole{first, second} {
		c.command = func() (*exec.Cmd, error) { launches.Add(1); return command() }
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			if _, err := first.Start(context.Background()); err != nil {
				t.Errorf("duplicate start: %v", err)
			}
		})
	}
	wg.Go(func() {
		if _, err := second.Start(context.Background()); err != nil {
			t.Errorf("racing start: %v", err)
		}
	})
	wg.Wait()
	a, _ := first.Status(context.Background())
	b, _ := second.Status(context.Background())
	if launches.Load() > 2 || a.Port != b.Port || a.Owned == b.Owned {
		t.Fatalf("race outcomes: launches=%d a=%+v b=%+v", launches.Load(), a, b)
	}
	if !a.Owned {
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		consoleExpect(t, second, agenttui.ConsoleRunning, true)
	} else {
		if err := second.Close(); err != nil {
			t.Fatal(err)
		}
		consoleExpect(t, first, agenttui.ConsoleRunning, true)
	}
}

func TestTUIConsoleCancellationWhileStarting(t *testing.T) {
	for _, mode := range []string{"action", "parent", "close"} {
		t.Run(mode, func(t *testing.T) {
			consoleTestHome(t)
			entered := make(chan struct{}, 1)
			conn := consoleTestBackend(t, func(_ http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-r.Context().Done() })
			parent, parentCancel := context.WithCancel(context.Background())
			defer parentCancel()
			c := consoleTestController(parent, t, conn)
			action, actionCancel := context.WithCancel(context.Background())
			defer actionCancel()
			done := make(chan error, 1)
			go func() { _, err := c.Start(action); done <- err }()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("child did not begin identity check")
			}
			switch mode {
			case "action":
				actionCancel()
			case "parent":
				parentCancel()
			case "close":
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err != errConsoleCanceled {
					t.Fatalf("canceled startup: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("canceled start hung")
			}
			if c.child != nil {
				t.Fatal("canceled startup retained child")
			}
			if _, err := dashboard.ReadRegistryEntry(c.identity.AgentID); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("late startup survived cancellation")
			}
		})
	}
}

func TestTUIConsoleCanceledActionsHaveNoSideEffects(t *testing.T) {
	consoleTestHome(t)
	c := consoleTestController(context.Background(), t, consoleTestBackend(t, nil))
	c.command = func() (*exec.Cmd, error) { t.Error("canceled action spawned"); return nil, errConsoleStart }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, action := range []func(context.Context) (agenttui.ConsoleStatus, error){c.Status, c.Start, c.Open, c.Stop} {
		if _, err := action(ctx); err != errConsoleCanceled {
			t.Fatalf("canceled action: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("WITSELF_HOME"), "dashboards")); !os.IsNotExist(err) {
		t.Fatal("canceled action touched registry")
	}
}

func TestTUIConsoleParentPipeEOF(t *testing.T) {
	consoleTestHome(t)
	c := consoleTestController(context.Background(), t, consoleTestBackend(t, nil))
	process, ready, err := c.spawn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { process.closePipe(); _ = process.cmd.Process.Kill() }()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("child not ready")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("readiness timeout")
	}
	process.closePipe() // Models the kernel closing a killed TUI's private fd.
	select {
	case <-process.done:
	case <-time.After(10 * time.Second):
		t.Fatal("EOF orphaned the child")
	}
	if _, err := dashboard.ReadRegistryEntry(c.identity.AgentID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("EOF did not release registry")
	}
}

func TestTUIConsoleChildFailuresArePrivate(t *testing.T) {
	for _, mode := range []string{"console-test-fail", "console-test-wait", "missing"} {
		t.Run(mode, func(t *testing.T) {
			consoleTestHome(t)
			c := consoleTestController(context.Background(), t, consoleTestBackend(t, nil))
			c.command = consoleTestCommand(mode)
			if mode == "missing" {
				c.command = func() (*exec.Cmd, error) { return nil, errors.New(consoleFakeToken) }
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			status, err := c.Start(ctx)
			if err == nil || strings.Contains(err.Error(), consoleFakeToken) || c.child != nil {
				t.Fatalf("failure status=%+v error=%v", status, err)
			}
			raw, _ := json.Marshal(status)
			if strings.Contains(string(raw), "http") || strings.Contains(string(raw), "token") {
				t.Fatal("private status")
			}
		})
	}
}

func TestTUIConsoleChildRejectsWrongIdentity(t *testing.T) {
	consoleTestHome(t)
	conn := consoleTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		wrong := consoleTestIdentity()
		wrong.AccountID = "acc_other"
		_ = json.NewEncoder(w).Encode(map[string]any{"identity": wrong})
	})
	c := consoleTestController(context.Background(), t, conn)
	if _, err := c.Start(context.Background()); err != errConsoleStart {
		t.Fatalf("wrong child identity: %v", err)
	}
	if _, err := dashboard.ReadRegistryEntry(c.identity.AgentID); !os.IsNotExist(err) {
		t.Fatal("wrong identity child registered")
	}
}

func consoleTestEntry(t *testing.T, conn agentConnection, handler http.HandlerFunc) dashboard.RegistryEntry {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	port := server.Listener.Addr().(*net.TCPAddr).Port
	return dashboard.RegistryEntry{SchemaVersion: dashboard.RegistrySchemaVersion, AgentID: consoleTestIdentity().AgentID, PID: os.Getppid(), Port: port, URL: server.URL + "/", AccessURL: server.URL + "/?token=" + strings.Repeat("ab", 16), StartedAt: time.Now().UTC(), Endpoint: conn.Endpoint}
}

func consoleTestWriteEntry(t *testing.T, entry dashboard.RegistryEntry) {
	t.Helper()
	path, err := dashboard.RegistryPath(consoleTestIdentity().AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestTUIConsoleMalformedDiscoveryNeverProbes(t *testing.T) {
	cases := map[string]func(*dashboard.RegistryEntry){
		"schema":   func(e *dashboard.RegistryEntry) { e.SchemaVersion = "bad" },
		"filename": func(e *dashboard.RegistryEntry) { e.AgentID = "agt_other" },
		"pid":      func(e *dashboard.RegistryEntry) { e.PID = -1 },
		"self pid": func(e *dashboard.RegistryEntry) { e.PID = os.Getpid() },
		"port":     func(e *dashboard.RegistryEntry) { e.Port++ },
		"host":     func(e *dashboard.RegistryEntry) { e.URL = "http://example.invalid/" },
		"access host": func(e *dashboard.RegistryEntry) {
			e.AccessURL = "http://example.invalid/?token=" + strings.Repeat("ab", 16)
		},
		"userinfo":        func(e *dashboard.RegistryEntry) { e.URL = strings.Replace(e.URL, "127.0.0.1", "user@127.0.0.1", 1) },
		"fragment":        func(e *dashboard.RegistryEntry) { e.AccessURL += "#private" },
		"extra query":     func(e *dashboard.RegistryEntry) { e.AccessURL += "&next=http://example.invalid" },
		"duplicate token": func(e *dashboard.RegistryEntry) { e.AccessURL += "&token=x" },
		"encoded token": func(e *dashboard.RegistryEntry) {
			e.AccessURL = strings.Replace(e.AccessURL, "token=ab", "token=%61b", 1)
		},
		"empty token": func(e *dashboard.RegistryEntry) { e.AccessURL = e.URL + "?token=" },
		"path":        func(e *dashboard.RegistryEntry) { e.URL += "other" },
		"account":     func(e *dashboard.RegistryEntry) { e.AccountID = "acc_other" },
		"realm":       func(e *dashboard.RegistryEntry) { e.RealmID = "rlm_other" },
		"endpoint":    func(e *dashboard.RegistryEntry) { e.Endpoint = "https://other.invalid" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			consoleTestHome(t)
			conn := consoleTestBackend(t, nil)
			var probes atomic.Int32
			entry := consoleTestEntry(t, conn, func(w http.ResponseWriter, _ *http.Request) { probes.Add(1); w.WriteHeader(401) })
			mutate(&entry)
			consoleTestWriteEntry(t, entry)
			c := consoleTestController(context.Background(), t, conn)
			c.command = func() (*exec.Cmd, error) { t.Error("invalid registry spawned child"); return nil, errConsoleStart }
			c.signal = func(dashboard.RegistryEntry) error { t.Error("invalid registry signaled"); return nil }
			for _, action := range []func(context.Context) (agenttui.ConsoleStatus, error){c.Status, c.Start, c.Open, c.Stop} {
				status, err := action(context.Background())
				if err != errConsoleConflict || status.State != agenttui.ConsoleConflict {
					t.Fatalf("status=%+v error=%v", status, err)
				}
			}
			if probes.Load() != 0 {
				t.Fatal("malformed record was probed")
			}
		})
	}
}

func TestTUIConsoleUnprovenDiscoveryNeverActs(t *testing.T) {
	for _, mode := range []string{"redirect", "marker only", "wrong token", "wrong account", "wrong realm", "wrong agent", "missing identity", "self redirect", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			consoleTestHome(t)
			conn := consoleTestBackend(t, nil)
			entry := consoleTestEntry(t, conn, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(dashboard.MarkerHeader, dashboard.RegistrySchemaVersion)
				if r.URL.Path == "/" {
					switch mode {
					case "redirect":
						w.Header().Set("Location", "http://example.invalid/private")
						w.WriteHeader(303)
					case "marker only":
						w.WriteHeader(200)
					case "wrong token":
						w.WriteHeader(401)
					default:
						http.SetCookie(w, &http.Cookie{Name: "session", Value: "fake-session"})
						w.Header().Set("Location", "/")
						w.WriteHeader(303)
					}
					return
				}
				if mode == "self redirect" {
					w.Header().Set("Location", "http://example.invalid/private")
					w.WriteHeader(302)
					return
				}
				if mode == "oversized" {
					_, _ = io.WriteString(w, strings.Repeat("x", 128*1024+1))
					return
				}
				identity := consoleTestIdentity()
				switch mode {
				case "wrong account":
					identity.AccountID = "other"
				case "wrong realm":
					identity.RealmID = "other"
				case "wrong agent":
					identity.AgentID = "other"
				case "missing identity":
					identity = client.SelfIdentity{}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"identity": identity})
			})
			consoleTestWriteEntry(t, entry)
			c := consoleTestController(context.Background(), t, conn)
			c.signal = func(dashboard.RegistryEntry) error { t.Error("unproven console signaled"); return nil }
			for _, action := range []func(context.Context) (agenttui.ConsoleStatus, error){c.Open, c.Stop} {
				if status, err := action(context.Background()); err != errConsoleConflict || status.State != agenttui.ConsoleConflict {
					t.Fatalf("status=%+v error=%v", status, err)
				}
			}
		})
	}
}

func TestTUIConsoleStopReportsSuccessor(t *testing.T) {
	consoleTestHome(t)
	conn := consoleTestBackend(t, nil)
	owner := consoleTestController(context.Background(), t, conn)
	if _, err := owner.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := owner.child.entry
	successor := original
	successor.StartedAt = successor.StartedAt.Add(time.Second) // Same PID, different instance.
	stopper := consoleTestController(context.Background(), t, conn)
	stopper.signal = func(entry dashboard.RegistryEntry) error {
		if !dashboard.SameRegistryInstance(entry, original) {
			t.Error("wrong instance signaled")
		}
		// Simulate a replacement immediately after signal delivery, before polling.
		return dashboard.WriteRegistryEntry(successor)
	}
	status, err := stopper.Stop(context.Background())
	if err != nil || status.State != agenttui.ConsoleRunning || status.Owned {
		t.Fatalf("successor=%+v error=%v", status, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := dashboard.ReadRegistryEntry(original.AgentID)
	if err != nil || !dashboard.SameRegistryInstance(current, successor) {
		t.Fatal("old owner removed successor")
	}
}

func TestTUIConsolePrivateBootstrapAndReadiness(t *testing.T) {
	consoleTestHome(t)
	conn := consoleTestBackend(t, nil)
	bootstrap := tuiConsoleBootstrap{Connection: conn, Identity: consoleTestIdentity(), Poll: time.Second, AccessToken: strings.Repeat("ab", 16)}
	raw, _ := json.Marshal(bootstrap)
	command, err := consoleTestCommand("console-test-child")()
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	outputReader, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = outputReader.Close(); _ = outputWriter.Close() }()
	var diagnostics bytes.Buffer
	command.Stdin, command.Stdout, command.Stderr = reader, outputWriter, &diagnostics
	command.Env = consoleChildEnvironment()
	if strings.Contains(strings.Join(command.Args, " ")+strings.Join(command.Env, " "), conn.Token) {
		t.Fatal("credential outside bootstrap")
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill() }()
	_ = reader.Close()
	_ = outputWriter.Close()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	if _, err := writer.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	ready := make(chan []byte, 1)
	go func() { var data [6]byte; _, _ = io.ReadFull(outputReader, data[:]); ready <- data[:] }()
	select {
	case data := <-ready:
		if string(data) != "ready\n" {
			t.Fatal("readiness contained unexpected data")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("readiness timeout")
	}
	_ = writer.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child exit timeout")
	}
	rest, _ := io.ReadAll(outputReader)
	if len(rest) != 0 || diagnostics.Len() != 0 {
		t.Fatal("child emitted diagnostic/private output")
	}
}

func TestTUIConsoleLegacyReuseAndParentCancellation(t *testing.T) {
	consoleTestHome(t)
	conn := consoleTestBackend(t, nil)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := consoleTestController(parent, t, conn)
	if _, err := owner.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := owner.child.entry
	legacy := original
	legacy.AccountID = ""
	legacy.RealmID = ""
	legacy.Endpoint = ""
	if err := dashboard.WriteRegistryEntry(legacy); err != nil {
		t.Fatal(err)
	}
	reusedParent, reusedCancel := context.WithCancel(context.Background())
	reused := consoleTestController(reusedParent, t, conn)
	consoleExpect(t, reused, agenttui.ConsoleRunning, false)
	opens := 0
	reused.opener = func(context.Context, string) error { opens++; return nil }
	if status, err := reused.Open(context.Background()); err != nil || status.Owned || opens != 1 {
		t.Fatalf("legacy open=%+v error=%v", status, err)
	}
	reusedCancel()
	if err := reused.Close(); err != nil {
		t.Fatal(err)
	}
	// Cancellation of the reuser leaves the independently owned console intact.
	probe := consoleTestController(context.Background(), t, conn)
	consoleExpect(t, probe, agenttui.ConsoleRunning, false)
	if err := dashboard.WriteRegistryEntry(original); err != nil {
		t.Fatal(err)
	}
	child := owner.child
	cancel()
	select {
	case <-child.done:
	case <-time.After(10 * time.Second):
		t.Fatal("parent cancellation orphaned owned child")
	}
	if _, err := dashboard.ReadRegistryEntry(original.AgentID); !os.IsNotExist(err) {
		t.Fatal("parent cancellation retained registry")
	}
}

func TestTUIConsoleBootstrapBoundsAndMetadata(t *testing.T) {
	consoleTestHome(t)
	conn := consoleTestBackend(t, nil)
	good := tuiConsoleBootstrap{Connection: conn, Identity: consoleTestIdentity(), Poll: time.Second, AccessToken: strings.Repeat("ab", 16)}
	wrongAccount := good
	wrongAccount.Connection.AccountID = "acc_other"
	wrongIdentity := good
	wrongIdentity.Identity.RealmID = ""
	for _, input := range []string{"{broken\n", strings.Repeat("x", consoleBootstrapLimit+1) + "\n", consoleBootstrapJSON(t, wrongAccount), consoleBootstrapJSON(t, wrongIdentity)} {
		var stdout bytes.Buffer
		if code := runTUIConsoleChild(strings.NewReader(input), &stdout); code != 1 || stdout.Len() != 0 {
			t.Fatalf("bad bootstrap code=%d output_bytes=%d", code, stdout.Len())
		}
	}
}

func consoleBootstrapJSON(t *testing.T, bootstrap tuiConsoleBootstrap) string {
	t.Helper()
	raw, err := json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

func TestTUIConsoleIgnoresProxyAndPrivateEnvironment(t *testing.T) {
	consoleTestHome(t)
	var proxyRequests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { proxyRequests.Add(1); w.WriteHeader(500) }))
	defer proxy.Close()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy"} {
		t.Setenv(key, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("WITSELF_TOKEN", consoleFakeToken)
	t.Setenv("UNRELATED_SECRET", consoleFakeToken)
	for _, value := range consoleChildEnvironment() {
		if strings.Contains(value, consoleFakeToken) {
			t.Fatal("private environment inherited")
		}
	}
	owner := consoleTestController(context.Background(), t, consoleTestBackend(t, nil))
	if _, err := owner.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	consoleExpect(t, owner, agenttui.ConsoleRunning, true)
	if proxyRequests.Load() != 0 {
		t.Fatal("console used environment proxy")
	}
}

func TestTUIConsoleOpenRechecksExactInstance(t *testing.T) {
	consoleTestHome(t)
	conn := consoleTestBackend(t, nil)
	owner := consoleTestController(context.Background(), t, conn)
	if _, err := owner.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := owner.child.entry
	changed := original
	changed.StartedAt = changed.StartedAt.Add(time.Second)
	if err := dashboard.WriteRegistryEntry(changed); err != nil {
		t.Fatal(err)
	}
	opened := false
	matched, err := dashboard.WithRegistryInstance(context.Background(), original, func() error { opened = true; return nil })
	if err != nil || matched || opened {
		t.Fatalf("replaced instance acted: matched=%v opened=%v error=%v", matched, opened, err)
	}
	if !dashboardEntryReleased(original) {
		t.Fatal("same-PID replacement not recognized as released")
	}
	if err := dashboard.WriteRegistryEntry(original); err != nil {
		t.Fatal(err)
	}
}

func TestTUIConsoleForcedCleanupOnlyRetainedChild(t *testing.T) {
	consoleTestHome(t)
	c := consoleTestController(context.Background(), t, consoleTestBackend(t, nil))
	c.command = consoleTestCommand("console-test-ignore-eof")
	process, _, err := c.spawn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.child = process
	// An uncooperative child ignores EOF. Close may force only this handle.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.done:
	default:
		t.Fatal("forced cleanup did not reap child")
	}
	if process.cmd.ProcessState == nil || process.cmd.ProcessState.Success() {
		t.Fatal("fixture did not exercise forced cleanup")
	}
}
