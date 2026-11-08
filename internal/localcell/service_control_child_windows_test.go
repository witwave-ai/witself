//go:build windows

package localcell_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

const serviceControlChildFlag = "WITSELF_TEST_SERVICE_CONTROL_CHILD"

type serviceControlChildRequest struct {
	Root, Action string
	Deadline     time.Time
}

func TestServiceControlChildSyntheticBudget(t *testing.T) {
	if serviceControlChildSyntheticBudgetMirror != localcell.NativeServiceChildSyntheticBudget {
		t.Fatal("portable child readiness budget mirror differs from the Windows fixture")
	}
}

// This is deliberately the native proof's client shape: a fresh test process
// whose first control operation uses the exact native environment constructor.
// Each row owns a fresh synthetic host; no PostgreSQL or manager is involved.
func TestServiceControlChildProcessClient(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	rows := []struct {
		name        string
		environment func() []string
	}{
		{"minimal", func() []string { return nativeServiceChildEnvironment(os.LookupEnv) }},
		{"full", os.Environ},
	}
	testContext, testCancel := context.WithTimeout(t.Context(), time.Duration(len(rows))*localcell.NativeServiceChildSyntheticBudget)
	defer testCancel()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), localcell.NativeServiceChildSyntheticBudget)
			defer cancel()
			// The whole-test bound can cancel a row, but never sizes its allowance.
			stopOuter := context.AfterFunc(testContext, cancel)
			defer stopOuter()
			deadline, _ := ctx.Deadline()
			workContext, workCancel := context.WithDeadline(ctx, deadline.Add(-localcell.NativeServiceChildSyntheticBudget/20))
			defer workCancel()
			prepared := time.Now()
			cell, root, policy := localcell.NativeServiceChildFixture(t)
			allowance := serviceControlChildReadinessAllowance(localcell.NativeServiceChildSyntheticBudget, time.Since(prepared))
			hostContext, stopHost := context.WithCancel(workContext)
			stderr, err := os.CreateTemp(t.TempDir(), "host-stderr")
			if err != nil {
				t.Fatal("cannot create synthetic host diagnostic capture")
			}
			originalStderr := os.Stderr
			os.Stderr = stderr
			done := make(chan error, 1)
			finished := false
			logHost := func() {
				data, readErr := os.ReadFile(stderr.Name())
				if readErr != nil {
					t.Log("synthetic host diagnostic capture unavailable")
					return
				}
				serviceControlChildLog(t, data)
			}
			// Run before t.Context is canceled and before the row deadline is released.
			defer func() {
				stopHost()
				if !finished {
					select {
					case <-done:
					case <-ctx.Done():
						t.Error("synthetic host cleanup exceeded shared budget")
					}
				}
				if t.Failed() {
					logHost()
				}
				os.Stderr = originalStderr
				_ = stderr.Close()
			}()
			readinessStarted := time.Now()
			go func() { done <- cell.Run(hostContext, policy) }()
			// Readiness comes from client A, matching the native proof. The child
			// still makes its first ever connection separately for every action.
			readyContext, readyCancel := context.WithTimeout(workContext, allowance)
			defer readyCancel()
			lastControl := "none"
			for {
				status, controlErr := cell.Control(readyContext, "health")
				if controlErr != nil {
					switch serviceControlChildDiagnostic(controlErr) {
					case "localcell: service_control_unavailable: transport:dial":
						lastControl = "dial"
					case "localcell: service_control_unavailable: transport:deadline":
						lastControl = "deadline"
					case "localcell: service_control_unavailable: transport:eof":
						lastControl = "eof"
					case "localcell: service_control_unavailable: transport:response":
						lastControl = "response"
					default:
						lastControl = "unclassified"
					}
				} else {
					lastControl = "none"
					if status.Healthy {
						break
					}
					lastControl = "unhealthy"
				}
				select {
				case <-readyContext.Done():
					logHost()
					workDeadline, _ := workContext.Deadline()
					t.Fatalf("synthetic host did not reach authenticated readiness: row=%s elapsed_ms=%d allowance_ms=%d work_remaining_ms=%d last_control=%s", row.name, time.Since(readinessStarted).Milliseconds(), allowance.Milliseconds(), max(time.Until(workDeadline).Milliseconds(), 0), lastControl)
				case <-time.After(time.Millisecond):
				}
			}
			allowance = max(allowance, 10*time.Since(readinessStarted))
			for _, action := range []string{"health", "status", "observe", "stop"} {
				started := time.Now()
				commandAllowance := allowance
				if action == "health" {
					// Measure cold process startup before sizing later commands.
					workDeadline, _ := workContext.Deadline()
					commandAllowance = time.Until(workDeadline)
				}
				commandContext, commandCancel := context.WithTimeout(workContext, commandAllowance)
				deadline, _ := commandContext.Deadline()
				waitDelay := localcell.NativeServiceChildSyntheticBudget / 30
				// Let Control print its deadline classification before os/exec's
				// process deadline can kill the helper and discard that evidence.
				request, marshalErr := json.Marshal(serviceControlChildRequest{Root: root, Action: action, Deadline: deadline.Add(-waitDelay)})
				if marshalErr != nil {
					commandCancel()
					t.Fatal("cannot construct child control request")
				}
				command := exec.CommandContext(commandContext, os.Args[0], "-test.run", "^TestServiceControlChildProcessHelper$")
				command.Env = append(row.environment(), serviceControlChildFlag+"=1")
				command.Stdin = bytes.NewReader(request)
				var captured bytes.Buffer
				command.Stdout, command.Stderr = io.Discard, &captured
				command.WaitDelay = waitDelay
				runErr := command.Run()
				commandCancel()
				if runErr != nil {
					serviceControlChildLog(t, captured.Bytes())
					logHost()
					t.Fatalf("child control action failed: %s", action)
				}
				if captured.Len() != 0 {
					serviceControlChildLog(t, captured.Bytes())
					t.Fatal("successful child control emitted unexpected stderr")
				}
				allowance = max(allowance, 10*time.Since(started))
			}
			// Stop changes desired state and stops children; the foreground host
			// deliberately stays alive and successful control emits no diagnostic.
			observeContext, observeCancel := context.WithTimeout(workContext, allowance)
			status, err := cell.Control(observeContext, "observe")
			observeCancel()
			if err != nil || status.State != "stopped" || status.DesiredState != "stopped" || status.Healthy {
				t.Fatal("child stop did not leave a serving foreground host with a durable stop fence")
			}
			before, err := os.ReadFile(stderr.Name())
			if err != nil {
				t.Fatal("synthetic host diagnostic capture unavailable")
			}
			if len(before) != 0 {
				logHost()
				t.Fatal("successful child control left unexpected host diagnostics")
			}
			select {
			case <-done:
				finished = true
				t.Fatal("control stop unexpectedly terminated the foreground host")
			default:
			}
			stopHost()
			select {
			case runErr := <-done:
				finished = true
				if runErr != nil {
					t.Fatal("synthetic foreground host shutdown failed")
				}
			case <-ctx.Done():
				t.Fatal("synthetic foreground host shutdown exceeded shared budget")
			}
			after, err := os.ReadFile(stderr.Name())
			if err != nil {
				t.Fatal("synthetic foreground shutdown capture unavailable")
			}
			count := nativeServiceShutdownReceiptCount(before, after)
			if count == 0 {
				t.Fatal("synthetic foreground shutdown receipt missing after stop response")
			}
			if count != 1 {
				t.Fatal("synthetic foreground shutdown emitted more than one receipt")
			}
		})
	}
}

func TestServiceControlChildProcessHelper(t *testing.T) {
	if os.Getenv(serviceControlChildFlag) != "1" {
		return
	}
	var request serviceControlChildRequest
	if json.NewDecoder(io.LimitReader(os.Stdin, 4096)).Decode(&request) != nil {
		fmt.Fprintln(os.Stderr, "localcell: service_control_child_request_invalid")
		os.Exit(2)
	}
	ctx, cancel := context.WithDeadline(t.Context(), request.Deadline)
	defer cancel()
	cell, err := localcell.New(localcell.Config{Root: request.Root, Timeout: time.Second})
	if err == nil {
		var status localcell.ServiceStatus
		status, err = cell.Control(ctx, request.Action)
		if err == nil && ((request.Action == "stop" && (status.State != "stopped" || status.DesiredState != "stopped")) || (request.Action != "stop" && !status.Healthy)) {
			fmt.Fprintln(os.Stderr, "localcell: service_control_child_status_invalid")
			os.Exit(3)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, serviceControlChildDiagnostic(err))
		os.Exit(1)
	}
	os.Exit(0)
}

func serviceControlChildLog(t *testing.T, data []byte) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if len(line) <= 160 && (serviceControlChildDiagnosticLine.MatchString(line) || nativeServiceShutdownLine.MatchString(line)) {
			t.Log(line)
		} else if line != "" {
			t.Log("child diagnostic omitted outside value-free vocabulary")
		}
	}
}
