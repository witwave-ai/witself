package capturemix

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestWorkerOutcomeBudgetCoversEveryFixtureInterval(t *testing.T) {
	tests := []struct {
		prefix string
		want   time.Duration
	}{
		{"AVATAR_STYLE_ROLLOUT", 61 * time.Second},
		{"MESSAGE_RATE_BUCKET_CLEANUP", time.Minute},
		{"AGENT_EMAIL_RATE_BUCKET_CLEANUP", time.Minute},
		{"AGENT_EMAIL_OUTBOUND", time.Minute},
		{"TRANSCRIPT_RETENTION", time.Minute},
		{"MESSAGE_RETENTION", 62 * time.Second},
		{"AGENT_EMAIL_RETENTION", 63 * time.Second},
		{"ACCOUNT_PURGE", 65 * time.Second},
		{"SUPPORT_TICKET_AGE_OUT", 67 * time.Second},
		{"TRANSCRIPT_STORAGE_RECONCILE", time.Minute},
		{"UNRECOGNIZED_WORKER", time.Minute},
	}
	var configured []string
	budget := proofWorkerOutcomeBudget()
	for _, test := range tests {
		t.Run(test.prefix, func(t *testing.T) {
			interval := proofWorkerInterval(test.prefix)
			if interval != test.want {
				t.Fatalf("capturemix fixture interval changed: got=%s want=%s", interval, test.want)
			}
			if budget < 2*interval {
				t.Fatalf("capturemix worker outcome budget cannot cover two fixture intervals: budget=%s interval=%s", budget, interval)
			}
		})
		if test.prefix != "UNRECOGNIZED_WORKER" {
			configured = append(configured, test.prefix)
		}
	}
	if !reflect.DeepEqual(proofWorkerPrefixes, configured) {
		t.Fatal("capturemix worker budget interval table differs from configured worker prefixes")
	}
	if budget <= 0 || budget > workerOutcomeBudgetCap || workerOutcomeBudgetCap >= 24*time.Minute {
		t.Fatal("capturemix worker outcome budget exceeds its cap or enclosing test context")
	}
}

func TestWorkerOutcomeBudgetUsesLargestConfiguredInterval(t *testing.T) {
	// Compute the maximum independently from the complete fixture-prefix table.
	// An allowance large enough to cover two intervals could otherwise hide a
	// mutant that derives the budget from the smallest configured interval.
	var largest time.Duration
	for _, prefix := range []string{
		"AVATAR_STYLE_ROLLOUT", "MESSAGE_RATE_BUCKET_CLEANUP", "AGENT_EMAIL_RATE_BUCKET_CLEANUP",
		"AGENT_EMAIL_OUTBOUND", "TRANSCRIPT_RETENTION", "MESSAGE_RETENTION", "AGENT_EMAIL_RETENTION",
		"ACCOUNT_PURGE", "SUPPORT_TICKET_AGE_OUT", "TRANSCRIPT_STORAGE_RECONCILE", "UNRECOGNIZED_WORKER",
	} {
		if interval := proofWorkerInterval(prefix); interval > largest {
			largest = interval
		}
	}
	want := min(4*largest, workerOutcomeBudgetCap)
	if got := proofWorkerOutcomeBudget(); got != want {
		t.Fatalf("capturemix worker outcome budget must use the largest configured interval: got=%s want=%s", got, want)
	}
}

func TestWorkerOutcomeBudgetIsUsedByWorkerObservation(t *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "process.go", nil, 0)
	if err != nil {
		t.Fatal("capturemix worker observation source unavailable")
	}
	polls := 0
	for _, declaration := range source.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "assertWorker" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee, ok := call.Fun.(*ast.Ident)
			if !ok || callee.Name != "pollWithBudget" {
				return true
			}
			polls++
			if len(call.Args) != 3 {
				t.Fatal("capturemix worker outcome poll signature changed")
			}
			budget, ok := call.Args[1].(*ast.CallExpr)
			if !ok {
				t.Fatal("capturemix worker outcome poll bypasses proportional budget helper")
			}
			helper, ok := budget.Fun.(*ast.Ident)
			if !ok || helper.Name != "proofWorkerOutcomeBudget" || len(budget.Args) != 0 {
				t.Fatal("capturemix worker outcome poll bypasses proportional budget helper")
			}
			return true
		})
	}
	if polls != 1 {
		t.Fatal("capturemix worker outcome observation must use exactly one proportional budget poll")
	}
}

func TestCompanionQueryFailureClassesAreClosed(t *testing.T) {
	deadlineContext, cancelDeadline := context.WithDeadline(context.Background(), time.Time{})
	defer cancelDeadline()
	canceledContext, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	plain := errors.New("database observation failed")
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"database_error", context.Background(), plain, "query_failed:other"},
		{"nil_error", context.Background(), nil, "query_failed:other"},
		{"closed_connection", context.Background(), pgconn.ErrConnClosed, "query_failed:connection_closed"},
		{"wrapped_closed_connection", context.Background(), fmt.Errorf("query: %w", pgconn.ErrConnClosed), "query_failed:connection_closed"},
		{"sql_closed_connection", context.Background(), sql.ErrConnDone, "query_failed:connection_closed"},
		{"transport_eof", context.Background(), io.EOF, "query_failed:connection_closed"},
		{"wrapped_transport_eof", context.Background(), fmt.Errorf("query: %w", io.EOF), "query_failed:connection_closed"},
		{"transport_unexpected_eof", context.Background(), io.ErrUnexpectedEOF, "query_failed:connection_closed"},
		{"wrapped_transport_unexpected_eof", context.Background(), fmt.Errorf("query: %w", io.ErrUnexpectedEOF), "query_failed:connection_closed"},
		{"transport_closed", context.Background(), net.ErrClosed, "query_failed:connection_closed"},
		{"wrapped_transport_closed", context.Background(), fmt.Errorf("query: %w", net.ErrClosed), "query_failed:connection_closed"},
		{"network_operation_closed", context.Background(), &net.OpError{Op: "read", Net: "tcp", Err: net.ErrClosed}, "query_failed:connection_closed"},
		{"eof_text_is_not_transport_identity", context.Background(), errors.New("EOF"), "query_failed:other"},
		{"unexpected_eof_text_is_not_transport_identity", context.Background(), errors.New("unexpected EOF"), "query_failed:other"},
		{"closed_text_is_not_driver_identity", context.Background(), errors.New("conn closed"), "query_failed:other"},
		{"no_rows", context.Background(), pgx.ErrNoRows, "query_failed:no_rows"},
		{"wrapped_no_rows", context.Background(), fmt.Errorf("query: %w", pgx.ErrNoRows), "query_failed:no_rows"},
		{"sql_no_rows", context.Background(), sql.ErrNoRows, "query_failed:no_rows"},
		{"sqlstate_connection", context.Background(), &pgconn.PgError{Code: "08006", Message: "private provider text"}, "query_failed:sqlstate_08"},
		{"wrapped_sqlstate_schema", context.Background(), fmt.Errorf("query: %w", &pgconn.PgError{Code: "3F000", Detail: "private schema detail"}), "query_failed:sqlstate_3F"},
		{"sqlstate_same_class", context.Background(), &pgconn.PgError{Code: "08003"}, "query_failed:sqlstate_08"},
		{"short_sqlstate", context.Background(), &pgconn.PgError{Code: "08"}, "query_failed:other"},
		{"long_sqlstate", context.Background(), &pgconn.PgError{Code: "080006"}, "query_failed:other"},
		{"lowercase_sqlstate", context.Background(), &pgconn.PgError{Code: "3f000"}, "query_failed:other"},
		{"malformed_sqlstate_class", context.Background(), &pgconn.PgError{Code: "8:000"}, "query_failed:other"},
		{"malformed_sqlstate_subclass", context.Background(), &pgconn.PgError{Code: "0800:"}, "query_failed:other"},
		{"typed_nil_sqlstate", context.Background(), (*pgconn.PgError)(nil), "query_failed:other"},
		{"deadline_error", context.Background(), context.DeadlineExceeded, "query_deadline"},
		{"wrapped_deadline_error", context.Background(), fmt.Errorf("query: %w", context.DeadlineExceeded), "query_deadline"},
		{"canceled_error", context.Background(), context.Canceled, "query_canceled"},
		{"wrapped_canceled_error", context.Background(), fmt.Errorf("query: %w", context.Canceled), "query_canceled"},
		{"expired_observation", deadlineContext, plain, "query_deadline"},
		{"canceled_caller", canceledContext, plain, "query_canceled"},
		{"deadline_context_overrides_canceled_error", deadlineContext, context.Canceled, "query_deadline"},
		{"canceled_context_overrides_deadline_error", canceledContext, context.DeadlineExceeded, "query_canceled"},
		{"deadline_context_overrides_closed_connection", deadlineContext, pgconn.ErrConnClosed, "query_deadline"},
		{"deadline_context_overrides_transport_eof", deadlineContext, io.EOF, "query_deadline"},
		{"deadline_context_overrides_transport_unexpected_eof", deadlineContext, io.ErrUnexpectedEOF, "query_deadline"},
		{"deadline_context_overrides_transport_closed", deadlineContext, net.ErrClosed, "query_deadline"},
		{"canceled_context_overrides_transport_eof", canceledContext, io.EOF, "query_canceled"},
		{"canceled_context_overrides_transport_unexpected_eof", canceledContext, io.ErrUnexpectedEOF, "query_canceled"},
		{"canceled_context_overrides_transport_closed", canceledContext, net.ErrClosed, "query_canceled"},
		{"canceled_context_overrides_sqlstate", canceledContext, &pgconn.PgError{Code: "08006"}, "query_canceled"},
		{"joined_deadline_overrides_closed_connection", context.Background(), errors.Join(pgconn.ErrConnClosed, context.DeadlineExceeded), "query_deadline"},
		{"joined_canceled_overrides_sqlstate", context.Background(), errors.Join(&pgconn.PgError{Code: "08006"}, context.Canceled), "query_canceled"},
		{"joined_deadline_overrides_transport_eof", context.Background(), errors.Join(io.EOF, context.DeadlineExceeded), "query_deadline"},
		{"joined_canceled_overrides_transport_closed", context.Background(), errors.Join(net.ErrClosed, context.Canceled), "query_canceled"},
	}
	seen := map[string]bool{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := companionQueryFailureClass(test.ctx, test.err)
			if got != test.want {
				t.Fatalf("capturemix companion query class mismatch: got=%s want=%s", got, test.want)
			}
			seen[got] = true
		})
	}
	if !reflect.DeepEqual(seen, map[string]bool{
		"query_failed:other": true, "query_failed:connection_closed": true,
		"query_failed:no_rows": true, "query_failed:sqlstate_08": true, "query_failed:sqlstate_3F": true,
		"query_deadline": true, "query_canceled": true,
	}) {
		t.Fatal("capturemix companion query failure vocabulary changed")
	}
}
