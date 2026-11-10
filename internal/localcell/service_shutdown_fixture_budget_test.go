package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

// Keep this test-side bound tied to the production windowsShutdownRequest. A lost ACK
// can consume one request deadline before the policy retries cancellation.
const shutdownAckReadDeadline = 250 * time.Millisecond

func shutdownFixtureBudget(startup time.Duration) time.Duration {
	return max(4*startup, 2*shutdownAckReadDeadline+100*time.Millisecond)
}

// shutdownFixtureLivenessBudget only bounds a hang while a fixture completes a
// protocol step or publishes a post-ACK witness; it asserts no latency limit.
// Keep behavioural shutdown and parent-deadline budgets separate.
func shutdownFixtureLivenessBudget(startup time.Duration) time.Duration {
	return max(8*startup, 10*time.Second)
}

func TestWindowsShutdownFixtureBudget(t *testing.T) {
	for _, tc := range []struct {
		startup      time.Duration
		want         time.Duration
		wantLiveness time.Duration
	}{
		{0, 600 * time.Millisecond, 10 * time.Second},
		{time.Millisecond, 600 * time.Millisecond, 10 * time.Second},
		{10 * time.Millisecond, 600 * time.Millisecond, 10 * time.Second},
		{50 * time.Millisecond, 600 * time.Millisecond, 10 * time.Second},
		{125 * time.Millisecond, 600 * time.Millisecond, 10 * time.Second},
		{150 * time.Millisecond, 600 * time.Millisecond, 10 * time.Second},
		{200 * time.Millisecond, 800 * time.Millisecond, 10 * time.Second},
		{time.Second, 4 * time.Second, 10 * time.Second},
		{1250 * time.Millisecond, 5 * time.Second, 10 * time.Second},
		{2 * time.Second, 8 * time.Second, 16 * time.Second},
	} {
		if got := shutdownFixtureBudget(tc.startup); got != tc.want {
			t.Fatalf("shutdown fixture budget=%s want=%s startup=%s", got, tc.want, tc.startup)
		}
		if got := shutdownFixtureLivenessBudget(tc.startup); got != tc.wantLiveness {
			t.Fatalf("shutdown fixture liveness budget=%s want=%s startup=%s", got, tc.wantLiveness, tc.startup)
		}
	}
}

func TestWindowsShutdownFixtureBudgetSourceContract(t *testing.T) {
	functionBody := func(path, name string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("shutdown budget source unavailable")
		}
		// The Windows checkout may use CRLF; normalize before body comparisons.
		source := strings.ReplaceAll(string(data), "\r\n", "\n")
		positions := token.NewFileSet()
		file, err := parser.ParseFile(positions, path, source, 0)
		if err != nil {
			t.Fatal("shutdown budget source cannot be parsed")
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == name && function.Body != nil {
				return source[positions.Position(function.Body.Pos()).Offset:positions.Position(function.Body.End()).Offset]
			}
		}
		t.Fatal("shutdown budget function unavailable")
		return ""
	}
	request := functionBody("service_shutdown_windows.go", "windowsShutdownRequest")
	if shutdownAckReadDeadline != 250*time.Millisecond ||
		!strings.Contains(request, "deadline := time.Now().Add(250 * time.Millisecond)") {
		t.Fatal("fixture ACK deadline differs from the production request literal")
	}
	if functionBody("service_shutdown_fixture_budget_test.go", "shutdownFixtureBudget") != `{
	return max(4*startup, 2*shutdownAckReadDeadline+100*time.Millisecond)
}` {
		t.Fatal("shutdown fixture budget lost its protocol floor or startup allowance")
	}
	if functionBody("service_shutdown_windows_test.go", "stop") != `{
	t.Helper()
	budget := shutdownFixtureBudget(f.startup)
	ctx, cancel := context.WithTimeout(t.Context(), max(10*f.startup+time.Second, 2*budget, shutdownFixtureLivenessBudget(f.startup)))
	defer cancel()
	return f.cell.stopChildOutcomeWithBudget(ctx, "server", ProcessState{PID: f.record.PID, Alive: true, Matches: true}, budget)
}` {
		t.Fatal("shutdown fixture stop lost its shared budget or outer deadline reserve")
	}
	escalation := functionBody("service_shutdown_windows_test.go", "TestWindowsShutdownEscalation")
	_, idempotent, found := strings.Cut(escalation, `t.Run("idempotent-ack"`)
	idempotent, _, bounded := strings.Cut(idempotent, "t.Run(")
	if !found || !bounded || strings.Contains(idempotent, "2*f.startup") ||
		strings.Contains(idempotent, "shutdownFixtureBudget(f.startup)") ||
		!strings.Contains(idempotent, "completion, cancel := context.WithTimeout(t.Context(), shutdownFixtureLivenessBudget(f.startup))") {
		t.Fatal("idempotent ACK completion lost its shared protocol budget")
	}
}

func TestWindowsShutdownLivenessWaitSourceContract(t *testing.T) {
	data, err := os.ReadFile("service_shutdown_windows_test.go")
	if err != nil {
		t.Fatal("shutdown liveness source unavailable")
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, required := range []string{
		"c.cfg.Timeout = max(20*startup, shutdownFixtureLivenessBudget(startup))",
		`waitShutdownMarker(t, c.path("tmp", "runtime-started"), max(10*time.Since(started), shutdownFixtureLivenessBudget(startup)))`,
		"probeCtx, probeCancel := context.WithTimeout(ctx, probeBudget)",
		"args = append(args, c.cfg.Timeout.String())",
		"probeCtx, probeCancel := context.WithTimeout(t.Context(), max(f.cell.cfg.Timeout+10*f.startup, shutdownFixtureLivenessBudget(f.startup)))",
		"parentBudget := shutdownFixtureBudget(f.startup)",
		"ctx, cancel := context.WithTimeout(t.Context(), parentBudget)",
		`outcome := f.cell.stopChildOutcomeWithBudget(ctx, "server", ProcessState{PID: f.record.PID, Alive: true, Matches: true}, 8*parentBudget)`,
		"if time.Since(started) >= 4*parentBudget {",
	} {
		if !strings.Contains(source, required) {
			t.Fatal("shutdown waits lost a liveness allowance or changed a timing assertion")
		}
	}
}

func shutdownFixtureSourceFunction(t *testing.T, path, name string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("shutdown fixture source unavailable")
	}
	// Source contracts must also work in the Windows CRLF checkout.
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, path, source, 0)
	if err != nil {
		t.Fatal("shutdown fixture source cannot be parsed")
	}
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == name && function.Body != nil {
			return source[positions.Position(function.Body.Pos()).Offset:positions.Position(function.Body.End()).Offset]
		}
	}
	t.Fatal("shutdown fixture function unavailable")
	return ""
}

func TestWindowsShutdownFixturePublicationSourceContract(t *testing.T) {
	source := shutdownFixtureSourceFunction(t, "service_shutdown_windows_test.go", "TestWindowsShutdownChildHelper")
	for _, required := range []string{
		`if err := writeAtomicBytes(filepath.Join(root, "tmp", "cancel-handled"), []byte(strconv.Itoa(completed)), nil); err != nil {
			os.Exit(shutdownFixturePublishExitCode(err, false))
		}`,
		`if err := writeAtomicBytes(filepath.Join(root, "tmp", "cancel-requested"), []byte(strconv.FormatUint(uint64(cancelCalls.Add(1)), 10)), nil); err != nil {
			os.Exit(shutdownFixturePublishExitCode(err, true))
		}`,
	} {
		if !strings.Contains(source, required) {
			t.Fatal("shutdown fixture publication lost its checked error and process exit class")
		}
	}
}

func TestWindowsShutdownFixtureDiagnosticsSourceContract(t *testing.T) {
	escalation := shutdownFixtureSourceFunction(t, "service_shutdown_windows_test.go", "TestWindowsShutdownEscalation")
	_, idempotent, found := strings.Cut(escalation, `t.Run("idempotent-ack"`)
	idempotent, _, bounded := strings.Cut(idempotent, "t.Run(")
	if !found || !bounded {
		t.Fatal("shutdown fixture idempotent ACK source unavailable")
	}
	for _, required := range []string{
		`handled, err := readPrivateFile(f.cell.path("tmp", "cancel-handled"))`,
		`if err == nil && string(handled) == "2" {`,
		`exitErr := windows.GetExitCodeProcess(f.handle, &code)
				t.Logf("shutdown handler completion class: %s", shutdownFixtureCompletionClass(handled, err, code, exitErr))
				t.Fatal("second cancel handler did not complete")`,
		`data, err := readPrivateFile(f.cell.path("tmp", "cancel-requested"))
		if err != nil || string(data) != "1" {
			t.Logf("shutdown handler completion class: %s", f.completionClass())
			t.Fatal("retry invoked cancellation more than once")
		}`,
	} {
		if !strings.Contains(idempotent, required) {
			t.Fatal("shutdown fixture lost its completion witness, exit diagnostic or once assertion")
		}
	}
	for _, failure := range []string{"clean cancel request refused", "retried cancel did not return identical ACK"} {
		if !strings.Contains(idempotent, "t.Logf(\"shutdown handler completion class: %s\", f.completionClass())\n\t\t\tt.Fatal(\""+failure+"\")") {
			t.Fatal("shutdown fixture early request failure lost its publish diagnostic")
		}
	}
	launch := shutdownFixtureSourceFunction(t, "service_shutdown_windows_test.go", "launchShutdownFixture")
	if !strings.Contains(launch, `if t.Failed() && windows.GetExitCodeProcess(f.handle, &code) == nil && code >= 90 && code <= 97 {
			t.Logf("shutdown handler completion class: %s", f.completionClass())
		}`) {
		t.Fatal("shutdown fixture lost its publish diagnostic for subtests that do not read the marker")
	}
	deadline := shutdownFixtureSourceFunction(t, "service_shutdown_windows_test.go", "TestWindowsShutdownParentDeadline")
	wait := `waitShutdownMarker(t, f.cell.path("tmp", "cancel-handled"), shutdownFixtureLivenessBudget(f.startup))`
	accepted := strings.Index(deadline, `t.Fatal("deadline fixture never accepted runtime-ready cancellation")`)
	waitIndex := strings.Index(deadline, wait)
	budget := strings.Index(deadline, "parentBudget := shutdownFixtureBudget(f.startup)")
	if accepted < 0 || waitIndex <= accepted || budget <= waitIndex || strings.Count(deadline, wait) != 1 {
		t.Fatal("shutdown fixture parent deadline lost its pre-budget publication fence")
	}
	marker := shutdownFixtureSourceFunction(t, "service_shutdown_windows_test.go", "waitShutdownMarker")
	if !strings.Contains(marker, "if _, err := readPrivateFile(path); err == nil {") || strings.Contains(marker, "os.Stat(") {
		t.Fatal("shutdown fixture marker wait no longer fences private publication completion")
	}
	if !strings.Contains(deadline, `if outcome.Kind != serviceStopUnknown || outcome.Detail != "shutdown:deadline" || ctx.Err() != context.DeadlineExceeded {
		t.Logf("shutdown parent deadline class: %s", shutdownFixtureDeadlineClass(outcome, ctx.Err()))
		t.Logf("shutdown descriptor replay class: %s", f.descriptorClass())
		t.Fatal("parent exhaustion did not raise at the actual shutdown deadline")
	}`) {
		t.Fatal("shutdown fixture parent deadline assertion or diagnostic changed")
	}
}

func TestWindowsShutdownFixtureDescriptorReplaySourceContract(t *testing.T) {
	reader := shutdownFixtureSourceFunction(t, "service_shutdown_windows_test.go", "shutdownFixtureReadDescriptor")
	for _, required := range []string{
		`if !shutdownDescriptorPath(root, path) {`,
		`{"tree", func() error { return validatePrivateTree(root) }}`,
		`{"root", func() error { return privateDir(root, false) }}`,
		`{"service_dir", func() error { return privateDir(filepath.Join(root, "service"), false) }}`,
		`{"children", func() error { return privateDir(filepath.Join(root, "service", "children"), false) }}`,
		`{"record", func() error { return servicePrivateRecord(path) }}`,
		`if err := check.read(); err != nil {`,
		`return serviceShutdownDescriptor{}, check.step, err`,
		`file, err := openPrivateFile(path, os.O_RDONLY, 0)`,
		`return serviceShutdownDescriptor{}, "open", err`,
		`defer func() { _ = file.Close() }()`,
		`d, err := decodeShutdownDescriptor(file)`,
		`return d, "decode", err`,
		`return d, "none", nil`,
	} {
		if !strings.Contains(reader, required) {
			t.Fatal("shutdown descriptor replay lost a read-only production observation")
		}
	}
	// The replay deliberately expands the production reader's flattened error.
	// Make changes to that production sequence require reviewing the replay too.
	production := shutdownFixtureSourceFunction(t, "service_shutdown_windows.go", "readShutdownDescriptor")
	if !strings.Contains(production, `if !shutdownDescriptorPath(root, path) || validatePrivateTree(root) != nil || privateDir(root, false) != nil ||
		privateDir(filepath.Join(root, "service"), false) != nil || privateDir(filepath.Join(root, "service", "children"), false) != nil || servicePrivateRecord(path) != nil {`) ||
		!strings.Contains(production, "openPrivateFile(path, os.O_RDONLY, 0)") || !strings.Contains(production, "return decodeShutdownDescriptor(f)") {
		t.Fatal("shutdown descriptor replay must track the production reader checks")
	}
	replay := shutdownFixtureSourceFunction(t, "service_shutdown_windows_test.go", "descriptorClass")
	for _, required := range []string{
		`d, step, readErr := shutdownFixtureReadDescriptor(f.cell.cfg.Root, f.record.ShutdownDescriptor)`,
		`rootErr := f.cell.validateRoot(false)`,
		`service, serviceErr := f.cell.ServiceDescriptor()`,
		`boot, bootErr := serviceBootID()`,
		`match := shutdownFixtureDescriptorMatchClass(d, f.record, service, boot, readErr, serviceErr, bootErr)`,
		`pending, scanErr := shutdownFixturePendingCount(f.cell.cfg.Root)`,
		`"descriptor=" + step + " error=" + shutdownFixtureObservationClass(readErr)`,
		`" root=" + shutdownFixtureObservationClass(rootErr) + " service=" + shutdownFixtureObservationClass(serviceErr)`,
		`" boot=" + shutdownFixtureObservationClass(bootErr) + " match=" + match`,
		`" pending=" + shutdownFixtureCountClass(pending) + " scan=" + shutdownFixtureObservationClass(scanErr)`,
	} {
		if !strings.Contains(replay, required) {
			t.Fatal("shutdown descriptor replay lost its fixed classes or temporary count")
		}
	}
}

func TestWindowsShutdownHandlerCompletionSourceContract(t *testing.T) {
	source := shutdownFixtureSourceFunction(t, "service_shutdown_windows.go", "serve")
	// The count is a callback-completion fence only while the hook follows the
	// handler call, whose deferred once guard has already finished on return.
	if !strings.Contains(source, `_ = c.handle(request, func(response serviceShutdownResponse) error {
				if !windowsShutdownReply(request, response) {
					return nil
				}
				return json.NewEncoder(conn).Encode(response)
			})
			if windowsShutdownHandlerCompleted != nil {
				windowsShutdownHandlerCompleted(request)
			}`) {
		t.Fatal("shutdown fixture completion hook no longer fences the handler callback")
	}
}
