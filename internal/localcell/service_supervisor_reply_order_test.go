package localcell

import (
	"context"
	"go/ast"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// The ordering contract makes the guarantee independent of goroutine scheduling:
// the loop publishes a reply once, after the worker returned and the live idle
// reset, also at shutdown (it watches cancellation only while idle); exit-host
// resets after it, and an accepted one replies on the worker before its drain.
func TestSupervisorCommandReplyOrder(t *testing.T) {
	data, err := os.ReadFile("service_supervisor.go")
	if err != nil {
		t.Fatal("cannot read supervisor reply ordering source")
	}
	const publish = "done.reply <- serviceReply{status: done.status, err: done.err}"
	pinned := admissionStatements(`inFlight = false
		if done.reply != nil { if done.action != "exit-host" && ctx.Err() == nil { s.phase, s.operation = "idle", "none" }; ` + publish + ` }
		if done.action == "exit-host" && ctx.Err() == nil { s.phase, s.operation = "idle", "none" }
		var runDone <-chan struct{}; if !inFlight { runDone = ctx.Done() }
		return nil
		if command.action != "exit-host" || err != nil { return supervisorWork{action: command.action, reply: command.reply, status: status, err: err} }
		command.reply <- serviceReply{status: status, err: err}
		exited := s.finishHostExitCommand(ctx, command)`)
	texts := func(list []ast.Stmt) []string {
		body := make([]string, len(list))
		for index, statement := range list {
			body[index] = admissionNodeText(statement)
		}
		return body
	}
	sends := func(node ast.Node) map[string]int {
		found := map[string]int{}
		ast.Inspect(node, func(node ast.Node) bool {
			if send, ok := node.(*ast.SendStmt); ok {
				found[admissionNodeText(send)]++
			}
			return true
		})
		return found
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, source := range []string{source, strings.ReplaceAll(source, "\n", "\r\n")} {
		ordered := 0
		run, serve := serviceAdmissionFunction(t, "service_supervisor.go", source, "Run"), serviceAdmissionFunction(t, "service_supervisor.go", source, "serveCommand")
		loop, _ := run.Body.List[len(run.Body.List)-1].(*ast.ForStmt)
		if loop == nil {
			t.Fatal("supervisor Run lost its closing control loop")
		}
		ast.Inspect(run.Body, func(node ast.Node) bool {
			clause, ok := node.(*ast.CommClause)
			if body := []string(nil); ok && admissionNodeText(clause.Comm) == "done := <-work" {
				body, ordered = texts(clause.Body), ordered+1
				if published := slices.Index(body, pinned[1]); body[0] != pinned[0] || published < 0 || slices.Index(body, pinned[2]) <= published {
					t.Fatal("supervisor must reset live ordinary commands before replying and exit commands after the worker")
				}
			} else if ok && admissionNodeText(clause.Comm) == "<-runDone" {
				if body, ordered = texts(clause.Body), ordered+1; !slices.Equal(body, pinned[5:6]) || len(loop.Body.List) < 2 || !slices.Equal(texts(loop.Body.List[:2]), pinned[3:5]) {
					t.Fatal("supervisor shutdown must wait for the worker before it returns")
				}
			}
			return true
		})
		if ordered != 2 || !maps.Equal(sends(run.Body), map[string]int{publish: 1, "command.reply <- deferServiceCommand(command.action, held)": 1, "work <- run()": 1}) {
			t.Fatal("supervisor reply ordering lost its single command publication boundary")
		}
		body := texts(serve.Body.List)
		accepted := slices.Index(body, pinned[7])
		if !maps.Equal(sends(serve.Body), map[string]int{pinned[7]: 1, "command.completed <- disposition": 1}) || accepted < 1 || body[accepted-1] != pinned[6] || slices.Index(body, pinned[8]) != accepted+1 {
			t.Fatal("supervisor exit-host must reply once on the worker before finishing its drain")
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
