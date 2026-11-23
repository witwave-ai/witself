package main

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

// Each stopped-state action gets one bounded subprocess invocation.
func windowsStoppedCommand(parent context.Context, allowance time.Duration, action string, invoke func(context.Context, string) string) string {
	ctx, cancel := context.WithTimeout(parent, allowance)
	defer cancel()
	if ctx.Err() != nil {
		return "timeout"
	}
	return invoke(ctx, action)
}

// The subprocess keeps its raw output here. The test logs only these literals.
func classifyWindowsStoppedCommand(ctxErr error, output []byte, err error) string {
	var exit *exec.ExitError
	nonzero := errors.As(err, &exit)
	var status localcell.ServiceStatus
	switch {
	case (err != nil && ctxErr != nil) || errors.Is(err, exec.ErrWaitDelay):
		return "timeout"
	case nonzero:
		return "exit_nonzero"
	case err != nil:
		return "command_error"
	case json.Unmarshal(output, &status) != nil:
		return "bad_json"
	case status.DesiredState != "stopped" || status.State != "stopped" || status.Healthy || status.Generation == "":
		return "wrong_state"
	default:
		return ""
	}
}

func TestWindowsForegroundStoppedCommand(t *testing.T) {
	const stopped = `{"desired_state":"stopped","state":"stopped","generation":"g"}`
	const gap = "native service host site=control_transport\nlocalcell: service_control_refused: endpoint:record\n"
	nonzero := &exec.ExitError{}
	for _, row := range []struct {
		name, output string
		err          error
		class        string
	}{
		{"stopped", stopped, nil, ""},
		{"endpoint_refresh", gap, nonzero, "exit_nonzero"},
		{"endpoint_read", strings.Replace(gap, "control_transport", "endpoint_read", 1), nonzero, "exit_nonzero"},
		{"endpoint_descriptor_crlf", strings.ReplaceAll(strings.Replace(gap, "control_transport", "endpoint_descriptor", 1), "\n", "\r\n"), nonzero, "exit_nonzero"},
		{"other_refusal", strings.Replace(gap, "endpoint:record", "endpoint:host_identity", 1), nonzero, "exit_nonzero"},
		{"extra_output", gap + "extra\n", nonzero, "exit_nonzero"},
		{"wrong_site", strings.Replace(gap, "control_transport", "unknown", 1), nonzero, "exit_nonzero"},
		{"refusal_with_zero_exit", gap, nil, "bad_json"},
		{"bad_json", "{", nil, "bad_json"},
		{"wrong_desired", strings.Replace(stopped, "stopped", "running", 1), nil, "wrong_state"},
		{"wrong_state", strings.Replace(stopped, `"state":"stopped"`, `"state":"running"`, 1), nil, "wrong_state"},
		{"healthy", strings.Replace(stopped, "{", `{"healthy":true,`, 1), nil, "wrong_state"},
		{"missing_generation", strings.Replace(stopped, `"g"`, `""`, 1), nil, "wrong_state"},
		{"wait_delay", gap, exec.ErrWaitDelay, "timeout"},
		{"spawn_error", gap, errors.New("spawn failed"), "command_error"},
	} {
		t.Run(row.name, func(t *testing.T) {
			calls := 0
			class := windowsStoppedCommand(t.Context(), time.Minute, "stop", func(ctx context.Context, action string) string {
				calls++
				if action != "stop" || calls != 1 {
					t.Fatal("stopped command repeated or changed its action")
				}
				return classifyWindowsStoppedCommand(ctx.Err(), []byte(row.output), row.err)
			})
			if class != row.class || calls != 1 {
				t.Fatal("stopped command lost its failure class or retried the wrong outcome")
			}
		})
	}
	// A child can finish before its deadline while the caller resumes later.
	// Only a failed invocation may be reclassified by its expired context.
	t.Run("canceled_at_return", func(t *testing.T) {
		for _, row := range []struct {
			output string
			err    error
			class  string
		}{
			{stopped, nil, ""},
			{"{", nil, "bad_json"},
			{"{}", nil, "wrong_state"},
			{"", nonzero, "timeout"},
		} {
			parent, cancel := context.WithCancel(t.Context())
			class := windowsStoppedCommand(parent, time.Minute, "status", func(ctx context.Context, _ string) string {
				cancel()
				return classifyWindowsStoppedCommand(ctx.Err(), []byte(row.output), row.err)
			})
			cancel()
			if class != row.class {
				t.Fatal("command deadline masked a completed subprocess result")
			}
		}
	})
	t.Run("shared_allowance", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			const allowance = 2500 * time.Microsecond
			class := windowsStoppedCommand(t.Context(), allowance, "observe", func(ctx context.Context, _ string) string {
				if deadline, _ := ctx.Deadline(); !deadline.Equal(start.Add(allowance)) {
					t.Fatal("stopped command gave a retry a fresh allowance")
				}
				<-ctx.Done()
				return classifyWindowsStoppedCommand(ctx.Err(), nil, nonzero)
			})
			if class != "timeout" || time.Since(start) != allowance {
				t.Fatal("endpoint refusals must stop at the original allowance")
			}
		})
	})
}
