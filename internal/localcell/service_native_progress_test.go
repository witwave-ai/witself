package localcell_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The wait result is independent of the shutdown channel, so failure reporting
// cannot consume the receipt needed by cleanup. Wait also drains stderr before
// publishing completion; an exited child therefore has a complete capture.
type nativeServiceDiagnosticExit struct {
	mu   sync.Mutex
	done bool
	err  error
}

func (e *nativeServiceDiagnosticExit) complete(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err, e.done = err, true
}

func (e *nativeServiceDiagnosticExit) exitError() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

func (e *nativeServiceDiagnosticExit) finished() bool {
	state := e.classification()
	return state != "not_started" && state != "running"
}

func (e *nativeServiceDiagnosticExit) classification() string {
	if e == nil {
		return "not_started"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.done {
		return "running"
	}
	if e.err == nil {
		return "success"
	}
	var exitError *exec.ExitError
	if errors.As(e.err, &exitError) {
		return "exit_error"
	}
	return "other_error"
}

func (d *nativeServiceFailureDiagnostic) startForeground(command *exec.Cmd, wrapStderr ...func(io.Writer) io.Writer) (<-chan error, error) {
	command.WaitDelay = time.Second
	nativeServicePrepareForegroundCommand(command)
	command.Env = nativeServiceChildEnvironment(os.LookupEnv)
	d.foregroundStderr = &nativeServiceDiagnosticBuffer{}
	d.foregroundExit = &nativeServiceDiagnosticExit{}
	command.Stdout, command.Stderr = io.Discard, d.foregroundStderr
	for _, wrap := range wrapStderr {
		command.Stderr = wrap(command.Stderr)
	}
	if err := command.Start(); err != nil {
		d.foregroundExit.complete(err)
		return nil, err
	}
	exited := make(chan error, 1)
	result := d.foregroundExit
	go func() {
		err := command.Wait()
		result.complete(err)
		exited <- err
	}()
	return exited, nil
}

func nativeServiceStep(step string) string {
	switch step {
	case "setup", "ready", "endpoint_closure", "observe", "stop", "restart", "write_witness":
		return step
	default:
		return "unknown"
	}
}

func (d *nativeServiceFailureDiagnostic) enter(step string) {
	d.step, d.controlErr = nativeServiceStep(step), nil
}

// Keep the subprocess failure available to errors.As while retaining only an
// exact diagnostic reason. Unknown stderr is never copied into this wrapper.
type nativeServiceInvocationError struct {
	cause   error
	control string
}

func (e *nativeServiceInvocationError) Error() string { return e.cause.Error() }
func (e *nativeServiceInvocationError) Unwrap() error { return e.cause }

func nativeServiceInvocationControl(stderr []byte) string {
	var found string
	for _, line := range strings.Split(string(stderr), "\n") {
		line = strings.TrimSpace(line)
		if nativeServiceControlDiagnostic(errors.New(line)) == "localcell: service_control_child_error_unclassified" {
			continue
		}
		if found != "" && found != line {
			return "" // Conflicting known errors have no unique control cause.
		}
		found = line
	}
	return found
}

func nativeServiceControlDiagnostic(err error) string {
	if err == nil {
		return "none"
	}
	line := err.Error()
	var invocation *nativeServiceInvocationError
	if errors.As(err, &invocation) {
		line = invocation.control
	}
	switch line {
	case "private Windows filesystem object required":
		return "windows_private_object"
	case "localcell: service_control_unavailable", "localcell: service_control_refused", "localcell: service_control_action_failed", "localcell: service_control_deferred", "localcell: service_unhealthy",
		"localcell: service_control_unavailable: transport:dial", "localcell: service_control_unavailable: transport:deadline",
		"localcell: service_control_unavailable: transport:eof", "localcell: service_control_unavailable: transport:response",
		"localcell: service_control_refused: endpoint:record", "localcell: service_control_refused: endpoint:random", "localcell: service_control_refused: endpoint:host_identity",
		"localcell: service_control_refused: peer:pid", "localcell: service_control_refused: peer:identity", "localcell: service_control_refused: peer:sid",
		"localcell: service_control_refused: pipe:name", "localcell: service_control_refused: pipe:policy", "localcell: service_control_refused: pipe:event",
		"localcell: service_control_refused: pipe:wait", "localcell: service_control_refused: pipe:io", "localcell: service_control_refused: pipe:create",
		"localcell: service_control_refused: pipe:connect", "localcell: service_control_refused: pipe:open":
		return line
	default:
		return "localcell: service_control_child_error_unclassified"
	}
}

func TestNativeServiceInvocationControlDiagnostic(t *testing.T) {
	const childPrefix = "native-service-control-diagnostic-fixture-"
	const known = "localcell: service_control_refused: pipe:io"
	if argument := os.Args[len(os.Args)-1]; strings.HasPrefix(argument, childPrefix) {
		switch strings.TrimPrefix(argument, childPrefix) {
		case "known":
			fmt.Fprintln(os.Stderr, known)
		case "conflicting":
			fmt.Fprintln(os.Stderr, known)
			fmt.Fprintln(os.Stderr, "localcell: service_control_action_failed")
		}
		fmt.Fprintln(os.Stderr, "postgres://fixture:private-password@localhost/private-database pid=914723", strings.Repeat("a5", 32))
		os.Exit(23)
	}
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	host, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate invocation control diagnostic helper")
	}
	for _, test := range []struct{ name, want string }{
		{"known", known},
		{"unknown", "localcell: service_control_child_error_unclassified"},
		{"conflicting", "localcell: service_control_child_error_unclassified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logged strings.Builder
			err := nativeServiceInvoke(t.Context(), host, t.TempDir(), t.TempDir(), "fixture",
				[]string{"-test.run=^TestNativeServiceInvocationControlDiagnostic$", "--", childPrefix + test.name},
				func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) },
				func(context.Context, string, ...string) ([]byte, error) { return nil, nil }, &nativeServiceDiagnosticBuffer{})
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 23 {
				t.Fatal("control diagnostic wrapper lost the invocation process error")
			}
			if nativeServiceControlDiagnostic(err) != test.want {
				t.Fatal("invocation lost its unique classified stderr control error")
			}
			if !strings.Contains(logged.String(), "<dsn>") {
				t.Fatal("invocation stderr bypassed the existing diagnostic sanitizer")
			}
			for _, private := range []string{"private-password", "private-database", "914723", strings.Repeat("a5", 32)} {
				if strings.Contains(logged.String(), private) {
					t.Fatal("invocation control diagnostics exposed private stderr")
				}
			}
		})
	}
	for _, untrusted := range []string{"localcell: unknown_private_value", "localcell: service_control_refused: pipe:unknown", "localcell: service_control_deferred: private-value"} {
		wrapped := &nativeServiceInvocationError{cause: errors.New("fixture failure"), control: untrusted}
		if nativeServiceControlDiagnostic(wrapped) != "localcell: service_control_child_error_unclassified" {
			t.Fatal("invocation wrapper bypassed the closed control vocabulary")
		}
	}
}

// Each field comes from a closed classifier. Raw process errors still pass
// through the established sanitizer in the full failure diagnostic below it.
func (d *nativeServiceFailureDiagnostic) logStep() {
	d.logf("native service step=%s host_exit=%s control=%s", nativeServiceStep(d.step), d.foregroundExit.classification(), nativeServiceControlDiagnostic(d.controlErr))
}

var nativeServiceStepDiagnosticLine = regexp.MustCompile(`^native service step=(unknown|setup|ready|endpoint_closure|observe|stop|restart|write_witness) host_exit=(not_started|running|success|exit_error|other_error) control=(none|windows_private_object|localcell: service_unhealthy|localcell: service_control_(unavailable|refused|action_failed|deferred|child_error_unclassified)|localcell: service_control_unavailable: transport:(dial|deadline|eof|response)|localcell: service_control_refused: (endpoint:(record|random|host_identity)|peer:(pid|identity|sid)|pipe:(name|policy|event|wait|io|create|connect|open)))$`)

func TestNativeServiceStepDiagnosticGuard(t *testing.T) {
	source, err := os.ReadFile("service_native_progress_test.go")
	if err != nil {
		t.Fatal("cannot read native service step vocabulary")
	}
	source = bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
	if !nativeServiceStepVocabulary(source) {
		t.Fatal("native service step vocabulary changed")
	}
	for _, change := range [][2]string{
		{`case "setup", "ready",`, `case "health", "ready",`},
		{`case "setup", "ready", `, `case `},
		{`case "setup", "ready",`, `case "readiness",`},
		{`return "unknown"`, `return "preflight"`},
		{`return "unknown"`, `return "ready"`},
	} {
		mutant := strings.Replace(string(source), change[0], change[1], 1)
		if mutant == string(source) || nativeServiceStepVocabulary([]byte(mutant)) {
			t.Fatal("native service step guard survived a vocabulary mutation")
		}
	}
	if !strings.HasPrefix(nativeServiceStepDiagnosticLine.String(), `^native service step=(unknown|setup|ready|endpoint_closure|observe|stop|restart|write_witness) host_exit=`) {
		t.Fatal("native service step guard vocabulary changed")
	}
	for _, step := range []string{"setup", "ready", "endpoint_closure", "observe", "stop", "restart", "write_witness"} {
		if nativeServiceStep(step) != step {
			t.Fatal("native service step classifier omitted a reviewed step")
		}
	}
	for _, step := range []string{"", "unknown", "preflight", "health", "Ready", "endpoint-closure", "private-input"} {
		if nativeServiceStep(step) != "unknown" {
			t.Fatal("native service unknown step lost its fixed unknown fallback")
		}
	}
	private := "postgres://fixture:private-password@localhost/private-database pid=914723 " + strings.Repeat("a5", 32)
	controls := []error{nil, errors.New("private Windows filesystem object required"), errors.New("localcell: service_control_unavailable: transport:dial"), errors.New("localcell: service_control_refused: endpoint:record"), errors.New(private)}
	for _, step := range []string{"setup", "ready", "endpoint_closure", "observe", "stop", "restart", "write_witness", private} {
		for _, control := range controls {
			d := nativeServiceFailureDiagnostic{logf: func(format string, args ...any) {
				line := fmt.Sprintf(format, args...)
				if !nativeServiceStepDiagnosticLine.MatchString(line) || strings.Contains(line, private) {
					t.Fatal("native service step diagnostic escaped its closed vocabulary")
				}
			}}
			d.enter(step)
			d.controlErr = control
			d.logStep()
		}
	}
	for _, line := range []string{
		"native service step=ready host_exit=exit_error control=none " + private,
		"native service step=" + private + " host_exit=running control=none",
		"native service step=preflight host_exit=running control=none",
		"native service step=ready host_exit=exit_status_914723 control=none",
		"native service step=ready host_exit=running control=" + private,
		"native service step=ready host_exit=running control=localcell: service_control_unavailable: transport:unknown",
		"native service step=ready host_exit=running control=localcell: unknown_private_value",
		"native service step=ready host_exit=running control=localcell: service_control_refused: pipe:unknown",
		"native service step=ready host_exit=running control=localcell: service_control_deferred: private-value",
		"native service step=ready host_exit=running control=localcell: service_control_defer",
	} {
		if nativeServiceStepDiagnosticLine.MatchString(line) {
			t.Fatal("native service diagnostic guard accepted a private or unknown field")
		}
	}
	for _, test := range []struct {
		err  error
		want string
	}{
		{nil, "none"},
		{errors.New("private Windows filesystem object required"), "windows_private_object"},
		{errors.New("localcell: service_control_unavailable: transport:dial"), "localcell: service_control_unavailable: transport:dial"},
		{errors.New(private), "localcell: service_control_child_error_unclassified"},
		{errors.New("localcell: unknown_private_value"), "localcell: service_control_child_error_unclassified"},
		{errors.New("localcell: service_control_refused: pipe:unknown"), "localcell: service_control_child_error_unclassified"},
		{errors.New("localcell: service_control_deferred: private-value"), "localcell: service_control_child_error_unclassified"},
	} {
		if nativeServiceControlDiagnostic(test.err) != test.want {
			t.Fatal("native service control error lost its exact diagnostic classification")
		}
	}
	allowed := []string{
		"localcell: service_control_unavailable", "localcell: service_control_refused", "localcell: service_control_action_failed", "localcell: service_control_deferred", "localcell: service_unhealthy",
		"localcell: service_control_unavailable: transport:dial", "localcell: service_control_unavailable: transport:deadline",
		"localcell: service_control_unavailable: transport:eof", "localcell: service_control_unavailable: transport:response",
		"localcell: service_control_refused: endpoint:record", "localcell: service_control_refused: endpoint:random", "localcell: service_control_refused: endpoint:host_identity",
		"localcell: service_control_refused: peer:pid", "localcell: service_control_refused: peer:identity", "localcell: service_control_refused: peer:sid",
		"localcell: service_control_refused: pipe:name", "localcell: service_control_refused: pipe:policy", "localcell: service_control_refused: pipe:event",
		"localcell: service_control_refused: pipe:wait", "localcell: service_control_refused: pipe:io", "localcell: service_control_refused: pipe:create",
		"localcell: service_control_refused: pipe:connect", "localcell: service_control_refused: pipe:open",
	}
	for _, reason := range allowed {
		if nativeServiceControlDiagnostic(errors.New(reason)) != reason ||
			!nativeServiceStepDiagnosticLine.MatchString("native service step=ready host_exit=running control="+reason) {
			t.Fatal("native service diagnostic classifier and guard disagree on an allowed control reason")
		}
	}
	nativeServiceControlVocabulary(t, allowed)
	for _, test := range []struct {
		result *nativeServiceDiagnosticExit
		want   string
		done   bool
	}{
		{nil, "not_started", false},
		{&nativeServiceDiagnosticExit{}, "running", false},
		{&nativeServiceDiagnosticExit{done: true}, "success", true},
		{&nativeServiceDiagnosticExit{done: true, err: &exec.ExitError{}}, "exit_error", true},
		{&nativeServiceDiagnosticExit{done: true, err: errors.New(private)}, "other_error", true},
	} {
		if test.result.classification() != test.want || test.result.finished() != test.done {
			t.Fatal("native service host exit classification lost its exact state")
		}
		line := "native service step=ready host_exit=" + test.result.classification() + " control=none"
		if !nativeServiceStepDiagnosticLine.MatchString(line) {
			t.Fatal("native service diagnostic guard rejected a classified host exit")
		}
	}
}

// The source side is independent of the diagnostic regexp and the positive
// examples: every switch arm and the fallback must match this reviewed set.
func nativeServiceStepVocabulary(source []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "progress.go", source, 0)
	if err != nil {
		return false
	}
	want := map[string]bool{"setup": true, "ready": true, "endpoint_closure": true, "observe": true, "stop": true, "restart": true, "write_witness": true}
	functions, defaults := 0, 0
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "nativeServiceStep" {
			continue
		}
		functions++
		if len(function.Body.List) != 1 {
			return false
		}
		selection, ok := function.Body.List[0].(*ast.SwitchStmt)
		if !ok || nativeServiceProgressNode(selection.Tag) != "step" || selection.Init != nil {
			return false
		}
		for _, statement := range selection.Body.List {
			branch, ok := statement.(*ast.CaseClause)
			if !ok || len(branch.Body) != 1 {
				return false
			}
			if branch.List == nil {
				defaults++
				if nativeServiceProgressNode(branch.Body[0]) != `return "unknown"` {
					return false
				}
				continue
			}
			if nativeServiceProgressNode(branch.Body[0]) != "return step" {
				return false
			}
			for _, expression := range branch.List {
				literal, ok := expression.(*ast.BasicLit)
				if !ok {
					return false
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil || !want[value] {
					return false
				}
				delete(want, value)
			}
		}
	}
	return functions == 1 && defaults == 1 && len(want) == 0
}

func nativeServiceProgressNode(node ast.Node) string {
	var formatted bytes.Buffer
	if node == nil || format.Node(&formatted, token.NewFileSet(), node) != nil {
		return ""
	}
	return strings.Join(strings.Fields(formatted.String()), " ")
}

// Expanding the classifier also requires independent review of its complete
// accepted vocabulary; positive examples alone would not detect a new arm.
func nativeServiceControlVocabulary(t *testing.T, allowed []string) {
	t.Helper()
	source, err := os.ReadFile("service_native_progress_test.go")
	if err != nil {
		t.Fatal("cannot read native control diagnostic vocabulary")
	}
	source = bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
	file, err := parser.ParseFile(token.NewFileSet(), "progress.go", source, 0)
	if err != nil {
		t.Fatal("cannot parse native control diagnostic vocabulary")
	}
	want := map[string]bool{"private Windows filesystem object required": true}
	for _, reason := range allowed {
		want[reason] = true
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "nativeServiceControlDiagnostic" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if branch, ok := node.(*ast.CaseClause); ok {
				for _, expression := range branch.List {
					literal, ok := expression.(*ast.BasicLit)
					if !ok {
						t.Fatal("native control diagnostic vocabulary includes a dynamic case")
					}
					value, err := strconv.Unquote(literal.Value)
					if err != nil || !want[value] {
						t.Fatal("native control diagnostic vocabulary includes an unreviewed case")
					}
					delete(want, value)
				}
			}
			return true
		})
	}
	if len(want) != 0 {
		t.Fatal("native control diagnostic vocabulary omitted a reviewed case")
	}
}

func TestNativeServiceForegroundProcessDiagnostics(t *testing.T) {
	const childArgument = "native-service-foreground-diagnostic-fixture"
	const reason = "private Windows filesystem object required"
	if os.Args[len(os.Args)-1] == childArgument {
		fmt.Fprintln(os.Stderr, reason)
		fmt.Fprintln(os.Stderr, "postgres://fixture:private-password@localhost/private-database pid=914723", strings.Repeat("a5", 32))
		os.Exit(23)
	}
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	host, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate foreground diagnostic helper")
	}
	var logged strings.Builder
	diagnostics := nativeServiceFailureDiagnostic{
		goos: "windows", root: t.TempDir(), home: t.TempDir(),
		logf: func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) },
		run:  func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
	}
	// Bind before start, exactly as the proof does, then replace the capture on
	// restart. A copied receiver or a stale buffer fails the second iteration.
	logFailure := diagnostics.logFailure
	for _, step := range []string{"ready", "restart"} {
		logged.Reset()
		diagnostics.enter(step)
		diagnostics.controlErr = errors.New("localcell: service_control_unavailable: transport:dial")
		command := exec.CommandContext(t.Context(), host, "-test.run=^TestNativeServiceForegroundProcessDiagnostics$", "--", childArgument)
		exited, err := diagnostics.startForeground(command)
		if err != nil {
			t.Fatal("cannot start hermetic foreground diagnostic helper")
		}
		if err := <-exited; err == nil {
			t.Fatal("foreground diagnostic helper did not fail")
		}
		logFailure(errors.New("fixture failure"), nil)
		output := logged.String()
		for _, want := range []string{
			"native service step=" + step + " host_exit=exit_error control=localcell: service_control_unavailable: transport:dial",
			"native service host exit error: exit status 23",
			"native service host stderr error: <nil>",
			"native service host stderr (last 30 lines):\n" + reason + "\n<dsn>",
		} {
			if !strings.Contains(output, want) {
				t.Fatal("foreground diagnostics lost the current child capture or exit result")
			}
		}
		for _, forbidden := range []string{"native_service_host_stderr_unavailable", "private-password", "private-database", "914723", strings.Repeat("a5", 32)} {
			if strings.Contains(output, forbidden) {
				t.Fatal("foreground diagnostics exposed private child output or claimed capture unavailable")
			}
		}
	}
	logged.Reset()
	diagnostics.foregroundExit.complete(errors.New("postgres://fixture:private-password@localhost/private-database pid=914723 " + strings.Repeat("a5", 32)))
	logFailure(errors.New("fixture failure"), nil)
	if !strings.Contains(logged.String(), "native service host exit error: <dsn>") {
		t.Fatal("foreground exit error bypassed the established diagnostic sanitizer")
	}
}

func TestNativeServiceProgressSourceContract(t *testing.T) {
	source, err := os.ReadFile("service_native_test.go")
	if err != nil {
		t.Fatal("cannot read native service proof contract")
	}
	source = bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
	if !nativeServiceProgressSourceContract(source) {
		t.Fatal("native service proof lost failing-step or classified-control bindings")
	}
	// A deferred response must keep polling regardless of where a terminal
	// branch is inserted, including after the last health/exit observation.
	file, parseErr := parser.ParseFile(token.NewFileSet(), "native.go", source, 0)
	if parseErr != nil {
		t.Fatal("cannot parse readiness insertion points")
	}
	insertions := 0
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "waitServiceReadyAfter" {
			continue
		}
		for _, statement := range function.Body.List {
			loop, ok := statement.(*ast.ForStmt)
			if !ok {
				continue
			}
			positions := []token.Pos{loop.Body.Rbrace}
			for _, statement := range loop.Body.List {
				positions = append(positions, statement.Pos())
			}
			for _, position := range positions {
				offset := int(position) - 1
				terminal := []byte("if nativeServiceControlDiagnostic(err) == \"localcell: service_control_deferred\" { t.Fatal(\"deferred became terminal\") }\n")
				mutant := append(append(append([]byte(nil), source[:offset]...), terminal...), source[offset:]...)
				if nativeServiceProgressSourceContract(mutant) {
					t.Fatal("readiness contract admitted a terminal deferred branch at an insertion point")
				}
				insertions++
			}
		}
	}
	if insertions != 8 {
		t.Fatal("readiness contract did not check every loop insertion point")
	}
	for _, change := range [][2]string{
		{`diagnostics.enter("ready")`, `diagnostics.enter("unknown")`},
		{`waitServiceReadyAfter(proofCtx, t, cell, firstStarted, 0, &diagnostics)`, `waitServiceReadyAfter(proofCtx, t, cell, firstStarted, 0)`},
		{`diagnostics.controlErr = err`, `diagnostics.controlErr = nil`},
		{`diagnostics[0].controlErr = err`, `diagnostics[0].controlErr = nil`},
		{`logFailure := diagnostics.logFailure`, `logFailure := diagnostics.ignoreFailure`},
		{`if runtime.GOOS == "darwin" {\n\t\t\tlogFailure(planErr, nil)`, `if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {\n\t\t\tlogFailure(planErr, nil)`},
	} {
		from, to := strings.ReplaceAll(change[0], `\n\t\t\t`, "\n\t\t\t"), strings.ReplaceAll(change[1], `\n\t\t\t`, "\n\t\t\t")
		mutant := strings.Replace(string(source), from, to, 1)
		if mutant == string(source) || nativeServiceProgressSourceContract([]byte(mutant)) {
			t.Fatal("native service progress contract survived its wiring mutation")
		}
	}
	// Mutate each concrete failure receipt independently, including repeated
	// expressions in different startup/shutdown branches.
	needle := []byte("logFailure(")
	for start := 0; ; {
		offset := bytes.Index(source[start:], needle)
		if offset < 0 {
			break
		}
		offset += start
		mutant := append([]byte(nil), source...)
		mutant = append(mutant[:offset], append([]byte("omitFailure("), source[offset+len(needle):]...)...)
		if nativeServiceProgressSourceContract(mutant) {
			t.Fatal("native service progress contract survived removal of a proof failure receipt")
		}
		start = offset + len(needle)
	}
	diagnostic, err := os.ReadFile("service_native_diagnostics_test.go")
	if err != nil {
		t.Fatal("cannot read native service failure receipt bridge")
	}
	diagnostic = bytes.ReplaceAll(diagnostic, []byte("\r\n"), []byte("\n"))
	if !nativeServiceStepReceiptContract(diagnostic) {
		t.Fatal("native service failure path lost its step receipt")
	}
	mutant := bytes.Replace(diagnostic, []byte("d.logStep()"), []byte("d.omitStep()"), 1)
	if bytes.Equal(mutant, diagnostic) || nativeServiceStepReceiptContract(mutant) {
		t.Fatal("native service progress contract survived removal of its step receipt bridge")
	}
}

func nativeServiceProgressSourceContract(source []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "service_native_test.go", source, 0)
	if err != nil {
		return false
	}
	var proof, ready *ast.FuncDecl
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok {
			switch function.Name.Name {
			case "TestNativeServiceLifecycle":
				proof = function
			case "waitServiceReadyAfter":
				ready = function
			}
		}
	}
	if proof == nil || ready == nil || !nativeServiceReadinessCauseContract(ready) {
		return false
	}
	steps, waits, controls, prestart := []string{}, 0, 0, 0
	failures, bindings, directSteps := []string{}, 0, 0
	valid := true
	ast.Inspect(proof.Body, func(node ast.Node) bool {
		if conditional, ok := node.(*ast.IfStmt); ok {
			if len(conditional.Body.List) == 1 {
				if expression, ok := conditional.Body.List[0].(*ast.ExprStmt); ok {
					if call, ok := expression.X.(*ast.CallExpr); ok && len(call.Args) == 2 {
						if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "logFailure" {
							if argument, ok := call.Args[0].(*ast.Ident); ok && argument.Name == "planErr" {
								comparison, ok := conditional.Cond.(*ast.BinaryExpr)
								if !ok || comparison.Op != token.EQL || !serviceControlChildSelector(comparison.X, "runtime", "GOOS") {
									valid = false
								} else if literal, ok := comparison.Y.(*ast.BasicLit); !ok || literal.Value != `"darwin"` {
									valid = false
								} else {
									prestart++
								}
							}
						}
					}
				}
			}
		}
		if assignment, ok := node.(*ast.AssignStmt); ok && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 && serviceControlChildSelector(assignment.Lhs[0], "diagnostics", "controlErr") {
			if value, ok := assignment.Rhs[0].(*ast.Ident); ok && value.Name == "err" {
				controls++
			}
		}
		if assignment, ok := node.(*ast.AssignStmt); ok && nativeServiceProgressNode(assignment) == "logFailure := diagnostics.logFailure" {
			bindings++
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if serviceControlChildSelector(call.Fun, "diagnostics", "logStep") {
			directSteps++
		}
		if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "logFailure" {
			failures = append(failures, nativeServiceProgressNode(call))
		}
		if serviceControlChildSelector(call.Fun, "diagnostics", "enter") {
			if len(call.Args) != 1 {
				valid = false
			} else if literal, ok := call.Args[0].(*ast.BasicLit); ok {
				step, err := strconv.Unquote(literal.Value)
				valid = valid && err == nil && nativeServiceStep(step) == step
				steps = append(steps, step)
			} else {
				valid = false
			}
		}
		if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "waitServiceReadyAfter" {
			if len(call.Args) != 6 {
				valid = false
			} else if pointer, ok := call.Args[5].(*ast.UnaryExpr); ok && pointer.Op == token.AND {
				if value, ok := pointer.X.(*ast.Ident); ok && value.Name == "diagnostics" {
					waits++
				}
			}
		}
		return true
	})
	want := "setup,ready,endpoint_closure,observe,write_witness,stop,endpoint_closure,restart,endpoint_closure,observe,write_witness,stop,observe,stop,endpoint_closure"
	wantFailures := []string{
		"logFailure(planErr, nil)",
		"logFailure(err, diagnostics.foregroundStderr.snapshot())",
		"logFailure(err, nil)",
		"logFailure(err, diagnostics.foregroundStderr.snapshot())",
		"logFailure(err, diagnostics.foregroundStderr.snapshot())",
		"logFailure(err, diagnostics.foregroundStderr.snapshot())",
		`logFailure(errors.New("native service proof failed"), stderr)`,
		"logFailure(planErr, nil)",
		"logFailure(proofCtx.Err(), nil)",
	}
	return valid && strings.Join(steps, ",") == want && waits == 2 && controls == 2 && prestart == 1 &&
		bindings == 1 && directSteps == 0 && strings.Join(failures, "\n") == strings.Join(wantFailures, "\n")
}

func nativeServiceReadinessCauseContract(ready *ast.FuncDecl) bool {
	loops, assignments := 0, 0
	valid := true
	ast.Inspect(ready.Body, func(node ast.Node) bool {
		if assignment, ok := node.(*ast.AssignStmt); ok {
			for _, left := range assignment.Lhs {
				if nativeServiceProgressNode(left) == "diagnostics[0].controlErr" {
					assignments++
					valid = valid && nativeServiceProgressNode(assignment) == "diagnostics[0].controlErr = err"
				}
			}
		}
		loop, ok := node.(*ast.ForStmt)
		if !ok {
			return true
		}
		loops++
		// Retain the newest health result before either success or early-exit
		// observation. A stale earlier cause is not useful in a ready failure.
		want := []string{
			`attemptStarted := time.Now()`,
			`status, err := cell.Control(ctx, "health")`,
			`stats.observe(err, time.Since(waitingStarted), time.Since(attemptStarted))`,
			`if len(diagnostics) != 0 { diagnostics[0].controlErr = err }`,
			`if err == nil && status.Healthy { return time.Since(started) }`,
			`if len(diagnostics) != 0 && diagnostics[0].foregroundExit.finished() { stats.log(t.Logf, time.Since(waitingStarted), budget) t.Fatal("native service foreground host exited before readiness") }`,
			`select { case <-ctx.Done(): stats.log(t.Logf, time.Since(waitingStarted), budget) t.Fatal("native service authenticated health did not become ready") case <-ticker.C: }`,
		}
		if loop.Init != nil || loop.Cond != nil || loop.Post != nil || len(loop.Body.List) != len(want) {
			valid = false
			return true
		}
		for index, statement := range want {
			valid = valid && nativeServiceProgressNode(loop.Body.List[index]) == statement
		}
		return true
	})
	return valid && loops == 1 && assignments == 1
}

func nativeServiceStepReceiptContract(source []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "diagnostics.go", source, 0)
	if err != nil {
		return false
	}
	bridges, calls := 0, 0
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "logFailure" {
			continue
		}
		bridges++
		if len(function.Body.List) < 2 || nativeServiceProgressNode(function.Body.List[0]) != "d.logProofBudget()" || nativeServiceProgressNode(function.Body.List[1]) != "d.logStep()" {
			return false
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && serviceControlChildSelector(call.Fun, "d", "logStep") {
				calls++
			}
			return true
		})
	}
	return bridges == 1 && calls == 1
}
