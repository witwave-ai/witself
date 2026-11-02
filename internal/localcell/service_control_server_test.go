package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestServiceControlServerDiagnosticVocabulary(t *testing.T) {
	for _, step := range []string{"peer:pid", "peer:identity", "peer:sid", "duplicate", "conn"} {
		t.Run(step, func(t *testing.T) {
			var output bytes.Buffer
			serviceControlAcceptRejected(&output, step)
			if output.String() != "service_control accept=rejected step="+step+"\n" {
				t.Fatal("server rejection did not emit exactly one fixed diagnostic")
			}
		})
	}
	for _, line := range []string{
		"service_control action=stop response=unflushed",
		"service_control accept=rejected step=peer:pid",
		"service_control accept=rejected step=peer:identity",
		"service_control accept=rejected step=peer:sid",
		"service_control accept=rejected step=duplicate",
		"service_control accept=rejected step=conn",
	} {
		var output bytes.Buffer
		writeServiceControlDiagnostic(&output, line)
		if output.String() != line+"\n" {
			t.Fatal("server diagnostic lost an allowed line")
		}
	}
	for _, private := range []string{
		"", "service_control action=stop response=written", "peer:pid private", "peer:pid\nprivate", "pid=123", "/fixture/private", strings.Repeat("a", 64),
	} {
		var output bytes.Buffer
		serviceControlAcceptRejected(&output, private)
		writeServiceControlDiagnostic(&output, private)
		writeServiceControlDiagnostic(&output, "service_control action=stop response=unflushed "+private)
		if output.Len() != 0 {
			t.Fatal("server diagnostic accepted a value outside its closed vocabulary")
		}
	}
}

type serviceControlFlushWriter struct {
	header     http.Header
	body       bytes.Buffer
	entered    chan struct{}
	release    chan struct{}
	writeError error
	flushError error
	shortWrite bool
}

func (w *serviceControlFlushWriter) Header() http.Header { return w.header }
func (*serviceControlFlushWriter) WriteHeader(int)       {}
func (w *serviceControlFlushWriter) Write(data []byte) (int, error) {
	if w.writeError != nil {
		return 0, w.writeError
	}
	if w.shortWrite {
		return w.body.Write(data[:len(data)-1])
	}
	return w.body.Write(data)
}
func (w *serviceControlFlushWriter) FlushError() error {
	close(w.entered)
	<-w.release
	return w.flushError
}

func TestServiceControlStopResponseWriteAndFlush(t *testing.T) {
	for _, failFlush := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "flush_failure"}[failFlush], func(t *testing.T) {
			writer := &serviceControlFlushWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
			if failFlush {
				writer.flushError = errors.New("fixture flush failure")
			}
			var diagnostic bytes.Buffer
			done := make(chan struct{})
			go func() {
				defer close(done)
				writeServiceControlResponse(writer, "stop", ServiceStatus{DesiredState: "stopped"}, &diagnostic)
			}()
			select {
			case <-writer.entered:
			case <-t.Context().Done():
				t.Fatal("stop response never reached its flush boundary")
			}
			var status ServiceStatus
			if json.Unmarshal(writer.body.Bytes(), &status) != nil || status.DesiredState != "stopped" || writer.header.Get("Content-Type") != "application/json" {
				t.Error("stop response flushed before its JSON bytes were written")
			}
			if writer.header.Get("Content-Length") != strconv.Itoa(writer.body.Len()) {
				t.Error("stop response flush left HTTP framing incomplete until handler return")
			}
			close(writer.release)
			<-done
			want := ""
			if failFlush {
				want = "service_control action=stop response=unflushed\n"
			}
			if diagnostic.String() != want {
				t.Fatal("stop response diagnostic did not match flush failure")
			}
		})
	}
	for _, shortWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "write_failure", true: "short_write"}[shortWrite], func(t *testing.T) {
			writer := &serviceControlFlushWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{}), shortWrite: shortWrite}
			if !shortWrite {
				writer.writeError = errors.New("fixture write failure")
			}
			var diagnostic bytes.Buffer
			writeServiceControlResponse(writer, "stop", ServiceStatus{}, &diagnostic)
			if diagnostic.String() != "service_control action=stop response=unflushed\n" {
				t.Fatal("failed stop response did not emit exactly one fixed diagnostic")
			}
			select {
			case <-writer.entered:
				t.Fatal("failed response write attempted a flush")
			default:
			}
		})
	}
}

// These are static kills on Unix. The child-process matrix and existing pipe
// exchanges execute the Windows transport on the Windows CI row.
func TestServiceControlWindowsAcceptRejectionSites(t *testing.T) {
	function := serviceControlWindowsFunction(t, "acceptInstance")
	logged := 0
	ast.Inspect(function.Body, func(node ast.Node) bool {
		branch, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		var emitted, disconnected token.Pos
		ast.Inspect(branch.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if foregroundDiagnosticIdent(call.Fun, "serviceControlAcceptRejected") {
				emitted = call.Pos()
			}
			if foregroundDiagnosticSelector(call.Fun, "windows", "DisconnectNamedPipe") {
				disconnected = call.Pos()
			}
			return true
		})
		if disconnected != token.NoPos {
			if emitted == token.NoPos || emitted > disconnected {
				t.Error("Windows accept rejected a peer without first emitting its fixed diagnostic")
			}
			logged++
		}
		return true
	})
	if logged != 3 {
		t.Fatal("Windows accept rejection diagnostic coverage changed")
	}
}

func TestServiceControlWindowsDrainBeforeDisconnect(t *testing.T) {
	function := serviceControlWindowsFunction(t, "closeWithDrain")
	var drain, disconnect, dispose token.Pos
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch {
		case foregroundDiagnosticSelector(call.Fun, "c", "drainPeer"):
			drain = call.Pos()
		case foregroundDiagnosticSelector(call.Fun, "windows", "DisconnectNamedPipe"):
			disconnect = call.Pos()
		case foregroundDiagnosticSelector(call.Fun, "windows", "CloseHandle") && len(call.Args) == 1 && foregroundDiagnosticSelector(call.Args[0], "c", "handle"):
			dispose = call.Pos()
		}
		return true
	})
	if drain == token.NoPos || disconnect == token.NoPos || dispose == token.NoPos || drain >= disconnect || disconnect >= dispose {
		t.Fatal("Windows response drain must precede disconnect and instance disposal")
	}
}

func serviceControlWindowsFunction(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "service_control_windows.go", nil, 0)
	if err != nil {
		t.Fatal("cannot inspect Windows control transport")
	}
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == name {
			return function
		}
	}
	t.Fatal("Windows control transport function is missing")
	return nil
}
