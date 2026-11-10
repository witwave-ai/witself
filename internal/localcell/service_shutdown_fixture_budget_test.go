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
