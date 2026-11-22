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

// Only the Windows subprocess proof calls this helper. That host publishes its
// descriptor and endpoint separately (LC-HOST-ENDPOINT-1). Count the exact
// refusal and share the original command allowance across every child process.
func windowsStoppedCommand(parent context.Context, allowance time.Duration, action string, invoke func(context.Context, string) string) (string, int) {
	ctx, cancel := context.WithTimeout(parent, allowance)
	defer cancel()
	gaps := 0
	for ctx.Err() == nil {
		class := invoke(ctx, action)
		if class != "endpoint_record_retry" {
			return class, gaps
		}
		gaps++
		select {
		case <-ctx.Done():
			return "timeout", gaps
		case <-time.After(time.Millisecond):
		}
	}
	return "timeout", gaps
}

// The subprocess keeps its raw output here. The test logs only these literals.
func classifyWindowsStoppedCommand(ctxErr error, output []byte, err error) string {
	var exit *exec.ExitError
	nonzero := errors.As(err, &exit)
	var status localcell.ServiceStatus
	switch {
	case nonzero && windowsEndpointRecordRefused(output):
		return "endpoint_record_retry"
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

func windowsEndpointRecordRefused(output []byte) bool {
	site, cause, _ := strings.Cut(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n")
	switch site {
	case "native service host site=control_transport", "native service host site=endpoint_read", "native service host site=endpoint_descriptor":
		return cause == localcell.ErrServiceControlRefused.Error()+": endpoint:record\n"
	default:
		return false
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
		gaps         int
	}{
		{"stopped", stopped, nil, "", 0},
		{"endpoint_refresh", gap, nonzero, "", 1},
		{"endpoint_read", strings.Replace(gap, "control_transport", "endpoint_read", 1), nonzero, "", 1},
		{"endpoint_descriptor_crlf", strings.ReplaceAll(strings.Replace(gap, "control_transport", "endpoint_descriptor", 1), "\n", "\r\n"), nonzero, "", 1},
		{"other_refusal", strings.Replace(gap, "endpoint:record", "endpoint:host_identity", 1), nonzero, "exit_nonzero", 0},
		{"extra_output", gap + "extra\n", nonzero, "exit_nonzero", 0},
		{"wrong_site", strings.Replace(gap, "control_transport", "unknown", 1), nonzero, "exit_nonzero", 0},
		{"refusal_with_zero_exit", gap, nil, "bad_json", 0},
		{"bad_json", "{", nil, "bad_json", 0},
		{"wrong_desired", strings.Replace(stopped, "stopped", "running", 1), nil, "wrong_state", 0},
		{"wrong_state", strings.Replace(stopped, `"state":"stopped"`, `"state":"running"`, 1), nil, "wrong_state", 0},
		{"healthy", strings.Replace(stopped, "{", `{"healthy":true,`, 1), nil, "wrong_state", 0},
		{"missing_generation", strings.Replace(stopped, `"g"`, `""`, 1), nil, "wrong_state", 0},
		{"wait_delay", gap, exec.ErrWaitDelay, "timeout", 0},
		{"spawn_error", gap, errors.New("spawn failed"), "command_error", 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			calls := 0
			class, gaps := windowsStoppedCommand(t.Context(), time.Minute, "stop", func(ctx context.Context, action string) string {
				calls++
				if action != "stop" || calls > 2 {
					t.Fatal("stopped command repeated or changed its action")
				}
				if calls == 1 {
					return classifyWindowsStoppedCommand(ctx.Err(), []byte(row.output), row.err)
				}
				return classifyWindowsStoppedCommand(ctx.Err(), []byte(stopped), nil)
			})
			if class != row.class || gaps != row.gaps || calls != row.gaps+1 {
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
			class, gaps := windowsStoppedCommand(parent, time.Minute, "status", func(ctx context.Context, _ string) string {
				cancel()
				return classifyWindowsStoppedCommand(ctx.Err(), []byte(row.output), row.err)
			})
			cancel()
			if class != row.class || gaps != 0 {
				t.Fatal("command deadline masked a completed subprocess result")
			}
		}
	})
	t.Run("shared_allowance", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			const allowance = 2500 * time.Microsecond
			calls := 0
			class, gaps := windowsStoppedCommand(t.Context(), allowance, "observe", func(ctx context.Context, _ string) string {
				calls++
				if deadline, _ := ctx.Deadline(); !deadline.Equal(start.Add(allowance)) || calls > 3 {
					t.Fatal("stopped command gave a retry a fresh allowance")
				}
				return classifyWindowsStoppedCommand(ctx.Err(), []byte(gap), nonzero)
			})
			if class != "timeout" || gaps != 3 || calls != 3 || time.Since(start) != allowance {
				t.Fatal("endpoint refusals must stop at the original allowance")
			}
		})
	})
}
