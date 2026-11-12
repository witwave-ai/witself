package localcell

import (
	"context"
	"go/ast"
	"os"
	"strings"
	"testing"
	"time"
)

// The ordering contract makes the guarantee independent of goroutine scheduling:
// both fields reset under a live-host guard immediately before publishing a
// non-exit reply; exit-host retains its old reset after finishHostExitCommand.
func TestSupervisorCommandReplyOrder(t *testing.T) {
	data, err := os.ReadFile("service_supervisor.go")
	if err != nil {
		t.Fatal("cannot read supervisor reply ordering source")
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, source := range []string{source, strings.ReplaceAll(source, "\n", "\r\n")} {
		fn := serviceAdmissionFunction(t, "service_supervisor.go", source, "Run")
		found := 0
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			clause, ok := node.(*ast.CommClause)
			if !ok || admissionNodeText(clause.Comm) != "command := <-commands" {
				return true
			}
			for index, statement := range clause.Body {
				if admissionNodeText(statement) != "command.reply <- serviceReply{status: status, err: err}" {
					continue
				}
				found++
				const reset = `if command.action != "exit-host" && ctx.Err() == nil { s.phase, s.operation = "idle", "none" }`
				const exitReset = `if command.action == "exit-host" && ctx.Err() == nil { s.phase, s.operation = "idle", "none" }`
				if index == 0 || admissionNodeText(clause.Body[index-1]) != admissionStatements(reset)[0] ||
					admissionNodeText(clause.Body[len(clause.Body)-1]) != admissionStatements(exitReset)[0] {
					t.Fatal("supervisor must reset live ordinary commands before replying and exit commands after finishing")
				}
			}
			return true
		})
		if found != 1 {
			t.Fatal("supervisor reply ordering lost its single command publication boundary")
		}
	}
}

// No sleeps or retries decide the receipt. A completed command's caller cancels
// immediately on receipt; Run and the caller are joined before capture ends.
// The source contract above deterministically excludes the old racy ordering.
func TestSupervisorCommandReplyCancellationReceipt(t *testing.T) {
	for _, action := range []string{"observe", "health"} {
		t.Run(action, func(t *testing.T) {
			cell := serviceCancellationReceiptCell(t)
			prepareSupervisorTest(t, cell)
			hooks, _, _, _, _ := supervisorTestHooks()
			hooks.ticks = make(chan time.Time)
			ready := make(chan struct{})
			healthCalls := 0
			hooks.health = func(context.Context) error {
				healthCalls++
				if healthCalls == 1 {
					close(ready)
				}
				return nil
			}
			budget, budgetCancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer budgetCancel()
			host, cancel := context.WithCancel(budget)
			defer cancel()
			completed := make(chan serviceReply, 1)
			go func() {
				select {
				case <-ready:
					status, err := cell.Control(budget, action)
					cancel()
					completed <- serviceReply{status: status, err: err}
				case <-host.Done():
					completed <- serviceReply{err: host.Err()}
				}
			}()
			var reply serviceReply
			receipt, runErr := serviceMaintenanceCaptureReceipt(t, func() error {
				err := cell.Run(host, SupervisorPolicy{hooks: &hooks, ShutdownTimeout: time.Second})
				cancel()
				reply = <-completed
				return err
			})
			if runErr != nil || budget.Err() != nil || reply.err != nil || !reply.status.Healthy || reply.status.State != "running" {
				t.Fatal("reply cancellation fixture did not finish a healthy command and clean host shutdown")
			}
			wantHealthCalls := 1
			if action == "health" {
				wantHealthCalls++
			}
			if healthCalls != wantHealthCalls {
				t.Fatal("reply cancellation fixture did not execute exactly the requested command")
			}
			if string(receipt) != "service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true\n" {
				t.Fatal("completed control reply did not retain an idle shutdown receipt")
			}
		})
	}
}
