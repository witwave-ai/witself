package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

func TestAutomationHealthStateClassification(t *testing.T) {
	tests := []struct {
		name string
		got  automationHealthState
		want automationHealthState
	}{
		{name: "no error", got: classifyHealthError(nil), want: automationHealthOK},
		{name: "deadline", got: classifyHealthError(context.DeadlineExceeded), want: automationHealthTimeout},
		{name: "wrapped deadline", got: classifyHealthError(fmt.Errorf("probe: %w", context.DeadlineExceeded)), want: automationHealthTimeout},
		{name: "canceled", got: classifyHealthError(context.Canceled), want: automationHealthDown},
		{name: "transport error", got: classifyHealthError(errors.New("connection refused")), want: automationHealthDown},
		{name: "cell ok", got: classifyCellProbe(fleet.ProbeResult{OK: true}, nil), want: automationHealthOK},
		{name: "cell degraded", got: classifyCellProbe(fleet.ProbeResult{OK: false}, nil), want: automationHealthDegraded},
		{name: "control-plane upstream timeout", got: classifyCellProbe(fleet.ProbeResult{Reason: "The operation timed out"}, nil), want: automationHealthTimeout},
		{name: "gateway timeout", got: classifyCellProbe(fleet.ProbeResult{CellStatus: http.StatusGatewayTimeout, Reason: "HTTP 504"}, nil), want: automationHealthTimeout},
		{name: "cell timeout", got: classifyCellProbe(fleet.ProbeResult{}, context.DeadlineExceeded), want: automationHealthTimeout},
		{name: "cell down", got: classifyCellProbe(fleet.ProbeResult{}, errors.New("unreachable")), want: automationHealthDown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("state = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestProbeAutomationHealthConcurrentFake(t *testing.T) {
	hungStarted := make(chan struct{})
	fastFinished := make(chan struct{})
	results := probeAutomationHealth(context.Background(), []automationHealthTarget{
		{
			name: "hung",
			probe: func(ctx context.Context) automationHealthState {
				close(hungStarted)
				<-ctx.Done()
				return classifyHealthError(ctx.Err())
			},
		},
		{
			name: "fast",
			probe: func(context.Context) automationHealthState {
				<-hungStarted
				close(fastFinished)
				return automationHealthOK
			},
		},
	}, 25*time.Millisecond, time.Now)

	select {
	case <-fastFinished:
	default:
		t.Fatal("fast sibling did not complete")
	}
	if results[0].State != automationHealthTimeout {
		t.Fatalf("hung state = %q", results[0].State)
	}
	if results[1].State != automationHealthOK {
		t.Fatalf("fast state = %q", results[1].State)
	}
}

func TestProbeAutomationHealthTimeoutDoesNotBlockSibling(t *testing.T) {
	hangStarted := make(chan struct{})
	hangEnded := make(chan struct{})
	fastObservedHung := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hang":
			close(hangStarted)
			<-r.Context().Done()
			close(hangEnded)
		case "/fast":
			<-hangStarted
			select {
			case <-hangEnded:
				fastObservedHung <- false
			default:
				fastObservedHung <- true
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	httpProbe := func(path string) func(context.Context) automationHealthState {
		return func(ctx context.Context) automationHealthState {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
			if err != nil {
				return classifyHealthError(err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				return classifyHealthError(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return automationHealthDegraded
			}
			return automationHealthOK
		}
	}

	const timeout = 100 * time.Millisecond
	started := time.Now()
	results := probeAutomationHealth(context.Background(), []automationHealthTarget{
		{name: "hung", probe: httpProbe("/hang")},
		{name: "fast", probe: httpProbe("/fast")},
	}, timeout, time.Now)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("probe batch took %s with a %s per-target timeout", elapsed, timeout)
	}
	if !<-fastObservedHung {
		t.Fatal("fast sibling did not complete while the hung probe was still in flight")
	}
	if got, want := results[0].State, automationHealthTimeout; got != want {
		t.Fatalf("hung state = %q, want %q", got, want)
	}
	if got, want := results[1].State, automationHealthOK; got != want {
		t.Fatalf("fast state = %q, want %q", got, want)
	}
	if results[0].CheckedAt.IsZero() || results[1].CheckedAt.IsZero() {
		t.Fatal("every probe result must carry checked_at")
	}
}

func TestEmitAutomationHealthNDJSONShape(t *testing.T) {
	results := []automationHealthResult{
		{
			Name:      "cell-a",
			State:     automationHealthOK,
			LatencyMS: 7,
			CheckedAt: time.Date(2026, 8, 28, 12, 34, 56, 0, time.UTC),
		},
		{
			Name:      "cell-b",
			State:     automationHealthTimeout,
			LatencyMS: 100,
			CheckedAt: time.Date(2026, 8, 28, 12, 34, 57, 123000000, time.UTC),
		},
	}
	var out bytes.Buffer
	if err := emitAutomationHealth(results, true, &out); err != nil {
		t.Fatalf("emitAutomationHealth: %v", err)
	}

	want := "" +
		"{\"name\":\"cell-a\",\"state\":\"ok\",\"latency_ms\":7,\"checked_at\":\"2026-08-28T12:34:56Z\"}\n" +
		"{\"name\":\"cell-b\",\"state\":\"timeout\",\"latency_ms\":100,\"checked_at\":\"2026-08-28T12:34:57.123Z\"}\n"
	if got := out.String(); got != want {
		t.Fatalf("NDJSON mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRunHealthCommandExitContract(t *testing.T) {
	for _, tt := range []struct {
		name     string
		probeOK  bool
		wantCode int
		wantErr  bool
	}{
		{name: "all ok", probeOK: true, wantCode: 0},
		{name: "degraded is nonzero", probeOK: false, wantCode: 1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/cells":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"cells": []fleet.Cell{{Name: "cell-a"}},
					})
				case r.Method == http.MethodPost && r.URL.Path == "/v1/cells/cell-a:probe":
					_ = json.NewEncoder(w).Encode(fleet.ProbeResult{OK: tt.probeOK})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			configPath := writeHealthProbeConfig(t, srv.URL)
			var out bytes.Buffer
			err := runHealthCommand(context.Background(), configPath, "", time.Second, true, &out)
			if got := commandExitCode(err); got != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (err %v)", got, tt.wantCode, err)
			}
			if tt.wantErr {
				if !errors.Is(err, errHealthNotOK) {
					t.Fatalf("error = %v, want errHealthNotOK", err)
				}
			} else if err != nil {
				t.Fatalf("runHealthCommand: %v", err)
			}
			if lines := bytes.Count(out.Bytes(), []byte{'\n'}); lines != 2 {
				t.Fatalf("NDJSON records = %d, want 2; output %q", lines, out.String())
			}
		})
	}
}

func TestCommandExitCodeHealthContract(t *testing.T) {
	if got := commandExitCode(nil); got != 0 {
		t.Fatalf("healthy exit code = %d", got)
	}
	if got := commandExitCode(errHealthNotOK); got == 0 {
		t.Fatal("non-ok health result must map to a nonzero exit code")
	}
}

func writeHealthProbeConfig(t *testing.T, controlPlane string) string {
	t.Helper()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "fleet.token")
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "infra.yaml")
	config := fmt.Sprintf("version: 1\ndefaults:\n  control_plane: %s\n  fleet_token_file: %s\ncells:\n  cell-a: {}\n", controlPlane, tokenPath)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}
