package testenv

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type observerRecordingTB struct {
	testing.TB
	fatals []string
}

func (t *observerRecordingTB) Fatal(args ...any) {
	t.fatals = append(t.fatals, fmt.Sprint(args...))
}

func (t *observerRecordingTB) Fatalf(format string, args ...any) {
	t.fatals = append(t.fatals, fmt.Sprintf(format, args...))
}

// Seam swaps are deliberately serial, like postgresTransactionGuardsEnabled.
func stubObserverConnect(t *testing.T, connect func(context.Context, string) (*pgx.Conn, error)) *[]time.Duration {
	t.Helper()
	previousConnect, previousSleep := postgresObserverConnect, postgresObserverSleep
	t.Cleanup(func() {
		postgresObserverConnect, postgresObserverSleep = previousConnect, previousSleep
	})
	var pauses []time.Duration
	postgresObserverConnect = connect
	postgresObserverSleep = func(pause time.Duration) { pauses = append(pauses, pause) }
	return &pauses
}

func TestObserverConnectRetriesTransientFailure(t *testing.T) {
	standIn := &pgx.Conn{}
	var contexts []context.Context
	pauses := stubObserverConnect(t, func(ctx context.Context, _ string) (*pgx.Conn, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second || ctx.Err() != nil {
			t.Fatal("observer attempt lacks a live five-second context budget")
		}
		for _, previous := range contexts {
			if ctx == previous || !errors.Is(previous.Err(), context.Canceled) {
				t.Fatal("observer attempt reused a context or did not cancel its predecessor")
			}
		}
		contexts = append(contexts, ctx)
		if len(contexts) < 3 {
			return nil, context.DeadlineExceeded
		}
		return standIn, nil
	})
	recorder := &observerRecordingTB{TB: t}
	guard := &postgresTransactionGuard{}
	ctx, cancel, ok := guard.connectObserver(recorder, "synthetic")
	if !ok || guard.observer != standIn || len(contexts) != 3 || len(recorder.fatals) != 0 {
		t.Fatal("observer did not succeed with the returned connection after three attempts")
	}
	defer cancel()
	if contexts[2].Err() != nil || ctx != contexts[2] {
		t.Fatal("successful observer attempt context was not returned live")
	}
	cancel()
	if !errors.Is(contexts[2].Err(), context.Canceled) {
		t.Fatal("successful observer attempt context was not canceled by caller")
	}
	if len(*pauses) != 2 {
		t.Fatal("observer retries did not pause exactly twice")
	}
	// Use the specification's clamp-bounds option: elapsed uses the real clock,
	// and no sleep or timing-sensitive injected elapsed is needed in this test.
	for _, pause := range *pauses {
		if pause < 200*time.Millisecond || pause > 2*time.Second {
			t.Fatal("observer retry pause outside clamp bounds")
		}
	}
}

func TestObserverBaselineSharesConnectBudget(t *testing.T) {
	standIn := &pgx.Conn{}
	var connectedContext context.Context
	previousConnect := postgresObserverConnect
	t.Cleanup(func() { postgresObserverConnect = previousConnect })
	postgresObserverConnect = func(ctx context.Context, _ string) (*pgx.Conn, error) {
		connectedContext = ctx
		return standIn, nil
	}
	recorder := &observerRecordingTB{TB: t}
	guard := &postgresTransactionGuard{}
	ctx, cancel, ok := guard.connectObserver(recorder, "synthetic")
	if !ok || guard.observer != standIn || len(recorder.fatals) != 0 {
		t.Fatal("observer did not connect for the shared baseline budget")
	}
	defer cancel()
	if ctx != connectedContext || ctx.Err() != nil {
		t.Fatal("baseline did not receive the live successful connect context")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
		t.Fatal("shared observer context lacks a live five-second budget")
	}
	cancel()
	if !errors.Is(connectedContext.Err(), context.Canceled) {
		t.Fatal("shared observer context was not canceled by caller")
	}
}

func TestObserverBaselineContextPlumbingContract(t *testing.T) {
	source, err := os.ReadFile("postgres_transactions.go")
	if err != nil {
		t.Fatal("observer baseline source contract could not read source")
	}
	normalized := strings.ReplaceAll(string(source), "\r\n", "\n")
	file, err := parser.ParseFile(token.NewFileSet(), "postgres_transactions.go", normalized, 0)
	if err != nil {
		t.Fatal("observer baseline source contract could not parse source")
	}
	var statements []ast.Stmt
	var collect func([]ast.Stmt)
	collect = func(list []ast.Stmt) {
		for _, statement := range list {
			branch, ok := statement.(*ast.IfStmt)
			if ok && branch.Init != nil {
				collect([]ast.Stmt{branch.Init})
			}
			statements = append(statements, statement)
			if ok {
				collect(branch.Body.List)
				switch alternate := branch.Else.(type) {
				case *ast.BlockStmt:
					collect(alternate.List)
				case *ast.IfStmt:
					collect([]ast.Stmt{alternate})
				}
			}
		}
	}
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "registerPostgresTransactionGuard" && function.Body != nil {
			collect(function.Body.List)
		}
	}
	isIdentifier := func(expression ast.Expr, name string) bool {
		identifier, ok := expression.(*ast.Ident)
		return ok && identifier.Name == name
	}
	isCancel := func(call *ast.CallExpr) bool {
		return isIdentifier(call.Fun, "cancel") && len(call.Args) == 0 && !call.Ellipsis.IsValid()
	}
	connectIndex, connectCount := -1, 0
	for index, statement := range statements {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 3 || len(assignment.Rhs) != 1 {
			continue
		}
		if !isIdentifier(assignment.Lhs[0], "ctx") || !isIdentifier(assignment.Lhs[1], "cancel") || !isIdentifier(assignment.Lhs[2], "ok") {
			continue
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok {
			continue
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && isIdentifier(selector.X, "guard") && selector.Sel.Name == "connectObserver" {
			connectIndex, connectCount = index, connectCount+1
		}
	}
	if connectCount != 1 {
		t.Fatal("observer baseline clause (a): expected ctx, cancel, ok assigned from guard.connectObserver")
	}
	baselineIndex, baselineCount, queryCount := -1, 0, 0
	for index, statement := range statements {
		ast.Inspect(statement, func(node ast.Node) bool {
			// Walk expressions here; only collect descends into statement bodies.
			// In particular, Cleanup closures own separate contexts and queries.
			if _, ok := node.(*ast.FuncLit); ok {
				return false
			}
			if nested, ok := node.(ast.Stmt); ok && nested != statement {
				return false
			}
			call, ok := node.(*ast.CallExpr)
			if ok && isIdentifier(call.Fun, "postgresOpenTransactions") {
				queryCount++
				if len(call.Args) == 2 && isIdentifier(call.Args[0], "ctx") && isIdentifier(call.Args[1], "observer") && !call.Ellipsis.IsValid() {
					baselineIndex, baselineCount = index, baselineCount+1
				}
			}
			return true
		})
	}
	if baselineCount != 1 || baselineIndex <= connectIndex {
		t.Fatal("observer baseline clause (d): expected exactly one postgresOpenTransactions(ctx, observer) after connect")
	}
	deferredCancel := false
	for _, statement := range statements[connectIndex+1 : baselineIndex] {
		replacedOrCanceled := false
		switch statement := statement.(type) {
		case *ast.AssignStmt:
			for _, left := range statement.Lhs {
				if isIdentifier(left, "ctx") || isIdentifier(left, "cancel") {
					replacedOrCanceled = true
				}
			}
		case *ast.ExprStmt:
			if call, ok := statement.X.(*ast.CallExpr); ok && isCancel(call) {
				replacedOrCanceled = true
			}
		case *ast.DeferStmt:
			if isCancel(statement.Call) {
				deferredCancel = true
			}
		}
		if replacedOrCanceled {
			t.Fatal("observer baseline clause (c): ctx or cancel reassigned or canceled before baseline")
		}
	}
	if !deferredCancel {
		t.Fatal("observer baseline clause (b): expected defer cancel() between connect and baseline")
	}
	if queryCount != 1 {
		t.Fatal("observer baseline clause (e): postgresOpenTransactions called outside the single baseline")
	}
}

func TestObserverConnectExhaustsAttempts(t *testing.T) {
	attempts := 0
	pauses := stubObserverConnect(t, func(context.Context, string) (*pgx.Conn, error) {
		attempts++
		return nil, syscall.ECONNREFUSED
	})
	recorder := &observerRecordingTB{TB: t}
	registerPostgresTransactionGuard(recorder, "synthetic")
	if attempts != 3 || len(*pauses) != 2 {
		t.Fatal("observer refusal did not exhaust three attempts with two pauses")
	}
	if len(recorder.fatals) != 1 || recorder.fatals[0] != "postgres transaction guard observer connection failed kind=refused attempts=3" {
		t.Fatal("observer refusal fatal differs from the fixed diagnostic")
	}
}

func TestObserverConnectDoesNotRetryAuth(t *testing.T) {
	attempts := 0
	pauses := stubObserverConnect(t, func(context.Context, string) (*pgx.Conn, error) {
		attempts++
		return nil, &pgconn.PgError{Code: "28P01"}
	})
	recorder := &observerRecordingTB{TB: t}
	registerPostgresTransactionGuard(recorder, "synthetic")
	if attempts != 1 || len(*pauses) != 0 {
		t.Fatal("observer authentication failure was retried")
	}
	if len(recorder.fatals) != 1 || recorder.fatals[0] != "postgres transaction guard observer connection failed kind=auth attempts=1" {
		t.Fatal("observer authentication fatal differs from the fixed diagnostic")
	}
}

type observerTimeoutError struct{}

func (observerTimeoutError) Error() string   { return "synthetic timeout" }
func (observerTimeoutError) Timeout() bool   { return true }
func (observerTimeoutError) Temporary() bool { return false }

func TestObserverConnectClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		kind string
	}{
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"net_timeout", observerTimeoutError{}, "timeout"},
		{"timeout_outranks_wrapped_auth", errors.Join(observerTimeoutError{}, &pgconn.PgError{Code: "28P01"}), "timeout"},
		{"connection_limit", &pgconn.PgError{Code: "53300"}, "limit"},
		{"configuration_limit", &pgconn.PgError{Code: "53400"}, "limit"},
		{"authorization", &pgconn.PgError{Code: "28000"}, "auth"},
		{"password", &pgconn.PgError{Code: "28P01"}, "auth"},
		{"refused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, "refused"},
		{"reset", syscall.ECONNRESET, "refused"},
		{"other", errors.New("x"), "other"},
		{"wrapped_limit", fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "53300"}), "limit"},
		{"pipe", syscall.EPIPE, "refused"},
		{"wrapped_refused", fmt.Errorf("wrapped: %w", syscall.ECONNREFUSED), "refused"},
		{"wrapped_deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "timeout"},
		{"wrapped_timeout", fmt.Errorf("wrapped: %w", observerTimeoutError{}), "timeout"},
		{"wrapped_auth", fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "28P01"}), "auth"},
		{"auth_class", &pgconn.PgError{Code: "28ZZZ"}, "auth"},
		{"other_postgres", &pgconn.PgError{Code: "23505"}, "other"},
		{"canceled", context.Canceled, "other"},
		{"other_network", &net.OpError{Err: errors.New("x")}, "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if postgresObserverConnectKind(test.err) != test.kind {
				t.Fatal("observer connection classification mismatch")
			}
		})
	}
}

func TestObserverConnectRetriesOtherAndLimit(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"other", errors.New("synthetic")},
		{"limit", &pgconn.PgError{Code: "53300"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			pauses := stubObserverConnect(t, func(context.Context, string) (*pgx.Conn, error) {
				attempts++
				return nil, test.err
			})
			recorder := &observerRecordingTB{TB: t}
			guard := &postgresTransactionGuard{}
			_, _, ok := guard.connectObserver(recorder, "synthetic")
			if ok || guard.observer != nil || attempts != 3 || len(*pauses) != 2 {
				t.Fatal("observer transient kind did not exhaust the retry budget")
			}
			if len(recorder.fatals) != 1 || recorder.fatals[0] != "postgres transaction guard observer connection failed kind="+test.name+" attempts=3" {
				t.Fatal("observer final transient kind diagnostic mismatch")
			}
		})
	}
}

func TestObserverConnectFatalIsValueFree(t *testing.T) {
	const marker = "observer-fixture-4c95266543cd433ba94f62f83412d949"
	dsn := "postgres://fixture.invalid/" + marker
	seamError := errors.New("synthetic driver message " + marker)
	stubObserverConnect(t, func(_ context.Context, got string) (*pgx.Conn, error) {
		if got != dsn {
			t.Fatal("observer seam did not receive the synthetic DSN")
		}
		return nil, seamError
	})
	recorder := &observerRecordingTB{TB: t}
	registerPostgresTransactionGuard(recorder, dsn)
	if len(recorder.fatals) != 1 {
		t.Fatal("observer value-free check did not record exactly one fatal")
	}
	fatal := recorder.fatals[0]
	if strings.Contains(fatal, marker) || strings.Contains(fatal, seamError.Error()) || strings.Contains(fatal, dsn) {
		t.Fatal("observer fatal exposed a synthetic connection value")
	}
	if fatal != "postgres transaction guard observer connection failed kind=other attempts=3" {
		t.Fatal("observer value-free fatal differs from the fixed diagnostic")
	}
}

func TestObserverConnectFatalLiteral(t *testing.T) {
	source, err := os.ReadFile("postgres_transactions.go")
	if err != nil {
		t.Fatal("observer source contract could not read source")
	}
	normalized := strings.ReplaceAll(string(source), "\r\n", "\n")
	if strings.Count(normalized, "postgres transaction guard observer connection failed kind=") != 1 {
		t.Fatal("observer source must contain exactly one classified fatal prefix")
	}
	if strings.Count(normalized, `"postgres transaction guard observer connection failed"`) != 0 {
		t.Fatal("observer source still contains the old bare fatal literal")
	}
}
