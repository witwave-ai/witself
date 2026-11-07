package localcell

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// serviceShutdownDeadlineContext expires synchronously when its embedded
// context is canceled, preserving a fixed deadline without a wall-clock sleep.
type serviceShutdownDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c *serviceShutdownDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func (c *serviceShutdownDeadlineContext) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

func TestServiceRunShutdownSharedDeadline(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	// P5 enlarges the Windows whole-tree budget to include both app waits,
	// PostgreSQL, and exit slack. Unix retains the original thirty-second cap.
	maximum := 30 * time.Second
	if runtime.GOOS == "windows" {
		maximum = ServiceShutdownTimeout
	}
	for _, tc := range []struct {
		name          string
		timeout       time.Duration
		want          time.Duration
		wantWindows   time.Duration
		exhaust       bool
		repairReturns bool
	}{
		{name: "remaining_budget", want: maximum},
		{name: "negative_policy_uses_default", timeout: -time.Second, want: maximum},
		{name: "oversized_policy_is_capped", timeout: 2 * ServiceShutdownTimeout, want: maximum},
		{name: "smaller_policy_is_preserved", timeout: time.Second, want: time.Second},
		{name: "exhausted_repair_returns_nil", timeout: 30 * time.Second, want: 30 * time.Second, exhaust: true},
		{name: "exhausted_repair_returns_deadline", timeout: time.Minute, want: 30 * time.Second, wantWindows: time.Minute, exhaust: true, repairReturns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, m := unitCell(t)
			if runtime.GOOS != "windows" {
				// Keep the control socket within the native Unix path limit.
				root, err := os.MkdirTemp("/tmp", "lc-deadline-")
				if err != nil {
					t.Fatal("cannot create short private fixture")
				}
				t.Cleanup(func() { _ = os.RemoveAll(root) })
				c, err = New(Config{Root: root, Timeout: time.Second})
				if err != nil || c.layout() != nil || writeAtomic(c.path("manifest.json"), m, nil) != nil {
					t.Fatal("cannot prepare short private fixture")
				}
			}
			prepareSupervisorTest(t, c)
			// The canceled caller is itself deadline-bound. Neither property may
			// reach the one fresh shutdown parent shared by repair and stop.
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			h, _, _, _, _ := supervisorTestHooks()
			if runtime.GOOS != "windows" {
				h.boot = func() (string, error) { return "synthetic-boot", nil }
			}
			var budget *serviceShutdownDeadlineContext
			var expire context.CancelFunc
			factories, repairs, stops, canceled := 0, 0, 0, false
			want := tc.want
			if runtime.GOOS == "windows" && tc.wantWindows != 0 {
				want = tc.wantWindows
			}
			deadline := time.Now().Add(want)
			h.shutdownContext = func(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
				factories++
				if factories != 1 || timeout != want || parent.Err() != nil || parent.Done() != nil || ctx.Err() == nil {
					t.Fatal("shutdown did not create one fresh signal-independent platform budget")
				}
				if _, bounded := parent.Deadline(); bounded {
					t.Fatal("shutdown parent inherited a deadline")
				}
				var controlled context.Context
				controlled, expire = context.WithCancel(parent)
				budget = &serviceShutdownDeadlineContext{Context: controlled, deadline: deadline}
				return budget, func() { canceled = true; expire() }
			}
			assertBudget := func(opCtx context.Context) {
				t.Helper()
				gotDeadline, bounded := opCtx.Deadline()
				if opCtx != budget || !bounded || gotDeadline != deadline || opCtx.Err() != nil {
					t.Fatal("shutdown repair and stop did not share the unchanged parent budget")
				}
			}
			h.health = func(context.Context) error { cancel(); return nil }
			h.repair = func(opCtx context.Context) error {
				if ctx.Err() == nil {
					return nil // Startup repair does not consume the shutdown budget.
				}
				repairs++
				assertBudget(opCtx)
				if tc.exhaust {
					expire() // Deterministically consume the whole shared deadline in repair.
					if tc.repairReturns {
						return opCtx.Err()
					}
				}
				return nil
			}
			h.stop = func(opCtx context.Context) error {
				if ctx.Err() == nil {
					return nil // Startup stop.
				}
				stops++
				assertBudget(opCtx)
				return nil
			}
			err := c.Run(ctx, SupervisorPolicy{hooks: &h, ShutdownTimeout: tc.timeout})
			if factories != 1 || repairs != 1 || !canceled {
				t.Fatal("shutdown budget was reset, skipped, or left uncanceled")
			}
			if tc.exhaust {
				if stops != 0 || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "context deadline exceeded") {
					t.Fatal("repair exhausted the shared deadline without a visible deadline failure or admitted stop afterward")
				}
			} else if err != nil || stops != 1 {
				t.Fatal("shutdown with remaining shared budget did not complete repair then stop")
			}
		})
	}
}

// This source contract exposes the Windows selection to Linux, without making
// the native shared-context fixture pretend to execute Windows APIs.
func TestServiceRunShutdownDeadlinePolicySourceContract(t *testing.T) {
	source, err := os.ReadFile("service_supervisor.go")
	if err != nil {
		t.Fatal("cannot read supervisor shutdown budget source")
	}
	normalized := strings.ReplaceAll(string(source), "\r\n", "\n")
	if !serviceRunShutdownDeadlinePolicySourceContract(normalized) {
		t.Fatal("supervisor shutdown budget lost platform cap or independent parent")
	}
	for _, tc := range []struct{ name, from, to string }{
		{"windows_cap", "maximumShutdown = ServiceShutdownTimeout", "maximumShutdown = 30 * time.Second"},
		{"unix_cap", "maximumShutdown := 30 * time.Second", "maximumShutdown := ServiceShutdownTimeout"},
		{"platform_selector", `runtime.GOOS == "windows"`, `runtime.GOOS == "linux"`},
		{"oversized_policy", "shutdown > maximumShutdown", "shutdown < maximumShutdown"},
		{"caller_parent", "s.hooks.shutdownContext(context.Background(), shutdown)", "s.hooks.shutdownContext(ctx, shutdown)"},
		{"reset_stop_budget", "stopErr := retryServiceContention(stopCtx, s.hooks.stop)", "stopErr := retryServiceContention(context.Background(), s.hooks.stop)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutant := strings.Replace(normalized, tc.from, tc.to, 1)
			if mutant == normalized {
				t.Fatal("supervisor shutdown budget mutation did not apply")
			}
			if serviceRunShutdownDeadlinePolicySourceContract(mutant) {
				t.Fatal("supervisor shutdown budget contract accepted a broken policy")
			}
		})
	}
}

func serviceRunShutdownDeadlinePolicySourceContract(source string) bool {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "service_supervisor.go", source, 0)
	if err != nil {
		return false
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "Run" || function.Body == nil {
			continue
		}
		body := source[fset.Position(function.Body.Pos()).Offset:fset.Position(function.Body.End()).Offset]
		const policy = `shutdown := policy.ShutdownTimeout
	maximumShutdown := 30 * time.Second
	if runtime.GOOS == "windows" {
		maximumShutdown = ServiceShutdownTimeout
	}
	if shutdown <= 0 || shutdown > maximumShutdown {
		shutdown = maximumShutdown
	}`
		return strings.Contains(body, policy) &&
			strings.Count(body, "s.hooks.shutdownContext(") == 1 &&
			strings.Contains(body, "stopCtx, cancel := s.hooks.shutdownContext(context.Background(), shutdown)") &&
			strings.Contains(body, "repairErr := retryServiceContention(stopCtx, s.hooks.repair)") &&
			strings.Contains(body, "stopErr := retryServiceContention(stopCtx, s.hooks.stop)")
	}
	return false
}
