package capturemix

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The test executable supplies children without a database or product binary.
// All child output is a fixed literal consumed by the real startup classifier.
func TestProcessRetryChild(_ *testing.T) {
	mode := ""
	for _, argument := range os.Args {
		if value, ok := strings.CutPrefix(argument, "--capturemix-retry-child="); ok {
			mode = value
		}
	}
	if mode == "" {
		return
	}
	switch mode {
	case "listener":
		_, _ = io.WriteString(os.Stdout, "witself-server: listen tcp bind: address already in use\n")
		os.Exit(2)
	case "other":
		_, _ = io.WriteString(os.Stdout, "witself-server: configuration: rejected\n")
		os.Exit(2)
	case "unclassified":
		_, _ = io.WriteString(os.Stdout, "witself-server: startup rejected\n")
		os.Exit(2)
	}
	listener, err := net.Listen("tcp", os.Getenv("WITSELF_HEALTH_ADDR"))
	if err != nil {
		os.Exit(3)
	}
	stopped := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if mode == "alive" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	})
	mux.HandleFunc("/stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(stopped)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	if mode == "alive" || mode == "metrics" {
		_, _ = io.WriteString(os.Stdout, "witself-server: listen tcp bind: address already in use\n")
	}
	<-stopped
	// Shutdown, unlike Close, waits for the in-flight stop reply to finish, so
	// the parent always reads a complete response before this process exits.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = server.Shutdown(ctx)
	cancel()
	os.Exit(0)
}

// Windows cannot send os.Interrupt to a child. This test-only handle uses the
// helper's loopback shutdown endpoint; stopProcesses still performs its real join.
type retryChildHandle struct{ child *childProcess }

func (h retryChildHandle) Signal(os.Signal) error {
	status, _, err := httpRequest(context.Background(), "GET", "http://"+h.child.health+"/stop", "", "")
	if err != nil || status != http.StatusOK {
		return errors.New("capturemix retry helper shutdown failed")
	}
	return nil
}

func (h retryChildHandle) Kill() error { return h.child.cmd.Process.Kill() }

type retryHarness struct {
	fixture  *fixture
	children []*childProcess
	started  func(*childProcess)
}

func newRetryHarness(t *testing.T, modes ...string) *retryHarness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("DSH_HOME", home)
	t.Setenv("TMPDIR", home)
	t.Setenv("TMP", home)
	t.Setenv("TEMP", home)
	h := &retryHarness{fixture: &fixture{cluster: &cluster{}, dsn: "postgres://fixture/fixture", name: "retry", started: time.Now()}}
	previous := processCommand
	processCommand = func(_ context.Context, p *childProcess) *exec.Cmd {
		h.children = append(h.children, p)
		mode := modes[min(len(h.children)-1, len(modes)-1)]
		if mode == "listener" {
			// Reserve the failed attempt's addresses until test cleanup. This
			// makes accidental address reuse observable without relying on the
			// operating system choosing different ephemeral ports by chance.
			reserved := map[string]bool{}
			for _, address := range []string{p.api, p.health, p.metrics} {
				if reserved[address] {
					continue
				}
				reserved[address] = true
				listener, err := net.Listen("tcp", address)
				if err != nil {
					t.Fatal("capturemix retry address reservation failed")
				}
				t.Cleanup(func() { _ = listener.Close() })
				go func() {
					for {
						connection, err := listener.Accept()
						if err != nil {
							return
						}
						_ = connection.Close()
					}
				}()
			}
		}
		if h.started != nil {
			h.started(p)
		}
		if mode == "start" {
			return exec.Command(filepath.Join(home, "missing-child"))
		}
		// Intentionally independent of ctx: cancelled-context tests must prove
		// the retry guard itself, not CommandContext refusing a second start.
		return exec.Command(os.Args[0], "-test.run=^TestProcessRetryChild$", "--", "--capturemix-retry-child="+mode)
	}
	t.Cleanup(func() {
		processCommand = previous
		for _, p := range h.children {
			if p.cmd.Process != nil {
				if p.alive() {
					_ = p.cmd.Process.Kill()
				}
				select {
				case <-p.done:
				case <-time.After(time.Second):
					t.Error("capturemix retry helper cleanup did not join")
				}
			}
			if os.RemoveAll(p.directory) != nil {
				t.Error("capturemix retry helper directory cleanup failed")
			}
		}
	})
	return h
}

func (h *retryHarness) assertRetirements(t *testing.T, count int) {
	t.Helper()
	want := []string(nil)
	for i := 0; i < count; i++ {
		want = append(want, "capturemix process start retried: kind=server version=new attempt="+strconv.Itoa(i+1)+" cause=listener_failed")
		p := h.children[i]
		if p.alive() {
			t.Fatal("capturemix retired child is alive")
		}
		if _, err := os.Stat(p.directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("capturemix retired child directory remains")
		}
		for _, later := range h.children[i+1:] {
			if p.directory == later.directory {
				t.Fatal("capturemix retry reused a process directory")
			}
			before := []string{p.api, p.health, p.metrics}
			after := []string{later.api, later.health, later.metrics}
			for n := range before {
				if before[n] == after[n] {
					t.Fatal("capturemix retry reused a process address")
				}
			}
		}
	}
	if !reflect.DeepEqual(h.fixture.options.startRetries, want) {
		t.Fatal("capturemix retry lines differ from retired attempts")
	}
	if h.fixture.server != nil || h.fixture.oldServer != nil || h.fixture.worker != nil {
		t.Fatal("capturemix start assigned a fixture process reference")
	}
}

func TestProcessStartRetryThenReady(t *testing.T) {
	for _, failures := range []int{1, 2} {
		t.Run(strconv.Itoa(failures), func(t *testing.T) {
			modes := make([]string, failures)
			for i := range modes {
				modes[i] = "listener"
			}
			h := newRetryHarness(t, append(modes, "ready")...)
			p, err := h.fixture.startProcess(context.Background(), "server", "new")
			if err != nil || p == nil || len(h.children) != failures+1 {
				t.Fatal("capturemix listener failure did not recover on the expected attempt")
			}
			h.assertRetirements(t, failures)
			if !p.alive() || len(h.fixture.processes) != 1 || h.fixture.processes[0] != p {
				t.Fatal("capturemix retry retained an incorrect child set")
			}
			p.handle = retryChildHandle{child: p}
			if h.fixture.stopProcesses() != nil {
				t.Fatal("capturemix recovered child cleanup failed")
			}
		})
	}
}

func (h *retryHarness) assertFailedState(t *testing.T, expectedAttempts int, cause string, result *childProcess, err error) {
	t.Helper()
	if len(h.children) != expectedAttempts || result != nil || err == nil || err.Error() != "capturemix process readiness failed: kind=server version=new cause="+cause {
		t.Fatal("capturemix failed startup changed its attempt count or readiness error")
	}
	last := h.children[len(h.children)-1]
	if len(h.fixture.processes) != 1 || h.fixture.processes[0] != last || last.alive() || last.waitErr == nil {
		t.Fatal("capturemix failed startup changed its retained child state")
	}
	if _, err := os.Stat(last.directory); err != nil {
		t.Fatal("capturemix failed startup lost its retained directory")
	}
	wantCleanup := "capturemix process cleanup failed: kind=server version=new\ncapturemix process cleanup: reason=unexpected_exit"
	if err := h.fixture.stopProcesses(); err == nil || err.Error() != wantCleanup {
		t.Fatal("capturemix failed startup changed its cleanup error")
	}
	if len(h.fixture.processes) != 0 {
		t.Fatal("capturemix failed startup cleanup retained processes")
	}
	if _, err := os.Stat(last.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("capturemix failed startup cleanup retained its directory")
	}
}

func TestProcessStartRetryExhausted(t *testing.T) {
	h := newRetryHarness(t, "listener")
	p, err := h.fixture.startProcess(context.Background(), "server", "new")
	if len(h.children) != 3 {
		t.Fatal("capturemix listener retry bound changed")
	}
	h.assertRetirements(t, 2)
	h.assertFailedState(t, 3, "listener_failed", p, err)
}

func TestProcessStartRetryOtherCause(t *testing.T) {
	for _, row := range []struct{ mode, cause string }{{"other", "configuration_refused"}, {"unclassified", "unclassified"}} {
		t.Run(row.mode, func(t *testing.T) {
			h := newRetryHarness(t, row.mode)
			p, err := h.fixture.startProcess(context.Background(), "server", "new")
			h.assertRetirements(t, 0)
			h.assertFailedState(t, 1, row.cause, p, err)
		})
	}
}

// This synthetic context stops only the readiness observation, allowing the
// retry decision to see an active caller. It tests the alive guard independently
// without spending the production poll's ten-second minimum budget.
type retryObservationContext struct {
	context.Context
	done  chan struct{}
	calls atomic.Int32
}

func (c *retryObservationContext) Done() <-chan struct{} { return c.done }
func (c *retryObservationContext) Err() error {
	// Cancellation propagation reads both Err and Cause (which falls back to
	// Err). The third read is the independent caller-state retry guard.
	if c.calls.Add(1) == 3 {
		return nil
	}
	return context.DeadlineExceeded
}

func TestProcessStartRetryAliveChild(t *testing.T) {
	h := newRetryHarness(t, "alive")
	ctx := &retryObservationContext{Context: context.Background(), done: make(chan struct{})}
	h.started = func(p *childProcess) {
		if len(h.children) != 1 {
			t.Fatal("capturemix alive listener failure started another attempt")
		}
		go func() {
			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				if p.logs.startupFailure() == "listener_failed" {
					close(ctx.done)
					return
				}
				select {
				case <-deadline.C:
					close(ctx.done)
					return
				case <-ticker.C:
				}
			}
		}()
	}
	p, err := h.fixture.startProcess(ctx, "server", "new")
	if len(h.children) != 1 || p != nil || err == nil || err.Error() != "capturemix process readiness failed: kind=server version=new cause=listener_failed" {
		t.Fatal("capturemix alive listener failure was retried or changed its error")
	}
	h.assertRetirements(t, 0)
	child := h.children[0]
	if !child.alive() || len(h.fixture.processes) != 1 || h.fixture.processes[0] != child {
		t.Fatal("capturemix alive readiness failure changed fixture state")
	}
	child.handle = retryChildHandle{child: child}
	if h.fixture.stopProcesses() != nil {
		t.Fatal("capturemix alive retry helper cleanup failed")
	}
}

type retryCanceledContext struct {
	context.Context
	child **childProcess
}

func (c retryCanceledContext) Err() error {
	if *c.child != nil && !(*c.child).alive() {
		return context.Canceled
	}
	return nil
}

func TestProcessStartRetryCanceledContext(t *testing.T) {
	h := newRetryHarness(t, "listener")
	var child *childProcess
	h.started = func(p *childProcess) { child = p }
	ctx := retryCanceledContext{Context: context.Background(), child: &child}
	p, err := h.fixture.startProcess(ctx, "server", "new")
	h.assertRetirements(t, 0)
	h.assertFailedState(t, 1, "listener_failed", p, err)
}

func TestProcessStartRetryNonReadinessFailure(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		h := newRetryHarness(t, "start")
		p, err := h.fixture.startProcess(context.Background(), "server", "new")
		if p != nil || err == nil || err.Error() != "capturemix process start failed" || len(h.children) != 1 || len(h.fixture.processes) != 0 {
			t.Fatal("capturemix process start failure was retried or changed state")
		}
		h.assertRetirements(t, 0)
		if _, err := os.Stat(h.children[0].directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("capturemix process start failure retained its directory")
		}
	})
	t.Run("metrics", func(t *testing.T) {
		h := newRetryHarness(t, "metrics")
		p, err := h.fixture.startProcess(context.Background(), "worker", "new")
		if p != nil || err == nil || err.Error() != "capturemix initial metrics unavailable" || len(h.children) != 1 || len(h.fixture.processes) != 1 || len(h.fixture.options.startRetries) != 0 {
			t.Fatal("capturemix metrics failure was retried or changed state")
		}
		child := h.children[0]
		child.handle = retryChildHandle{child: child}
		if h.fixture.stopProcesses() != nil {
			t.Fatal("capturemix metrics retry helper cleanup failed")
		}
	})
}

func TestProcessStartRetrySourceContract(t *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "process.go", nil, 0)
	if err != nil {
		t.Fatal("capturemix retry source unavailable")
	}
	allocations, bounds := 0, 0
	for _, declaration := range source.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "loopbackAddress" {
					allocations++
					if function.Name.Name != "startProcessAttempt" {
						t.Fatal("capturemix address allocation escaped the attempt")
					}
				}
			}
			if comparison, ok := node.(*ast.BinaryExpr); ok && function.Name.Name == "startProcess" && comparison.Op == token.EQL {
				if name, ok := comparison.X.(*ast.Ident); ok && name.Name == "attempt" {
					bounds++
					literal, ok := comparison.Y.(*ast.BasicLit)
					if !ok || literal.Kind != token.INT || literal.Value != "3" {
						t.Fatal("capturemix retry bound is not the literal three")
					}
				}
			}
			return true
		})
	}
	if allocations != 1 || bounds != 1 {
		t.Fatal("capturemix retry allocation or bound contract changed")
	}
	// Single-line pins are independent of checkout line endings and pin the
	// result plumbing for retry lines.
	for _, contract := range []struct{ file, text string }{
		{"capturemix.go", "result.StartRetries = append(result.StartRetries, fixture.options.startRetries...)"},
		{"../../store/capture_mix_rehearsal_test.go", "for _, line := range result.StartRetries {"},
	} {
		raw, err := os.ReadFile(contract.file)
		if err != nil || !strings.Contains(string(raw), contract.text) {
			t.Fatal("capturemix retry lines no longer reach the matrix log")
		}
	}
	// The helper child drains its stop reply with Shutdown before it exits;
	// Close would race the parent's read of that reply.
	helper, err := parser.ParseFile(token.NewFileSet(), "process_retry_test.go", nil, 0)
	if err != nil {
		t.Fatal("capturemix retry helper source unavailable")
	}
	drains, closes := 0, 0
	for _, declaration := range helper.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "TestProcessRetryChild" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok {
				if name, ok := selector.X.(*ast.Ident); ok && name.Name == "server" {
					switch selector.Sel.Name {
					case "Shutdown":
						drains++
					case "Close":
						closes++
					}
				}
			}
			return true
		})
	}
	if drains != 1 || closes != 0 {
		t.Fatal("capturemix retry helper no longer drains its stop reply")
	}
}
