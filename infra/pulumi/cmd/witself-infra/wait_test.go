package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

// TestWaitForCellHealthy pins the six stop conditions of the readiness
// poll: return cleanly on first OK, retry until OK arrives, tolerate ALB
// warmup 404s, time out with the last-seen diagnostic tail intact, honor
// context cancellation immediately, and fail fast when the cell isn't
// registered (no amount of waiting fixes that).
//
// This is the gate between registerCell and restoreCell — a wrong-way
// retry would either surface a benign transient as a hard failure, spin
// forever on a cell that never came up, or wait 20 minutes on a cell the
// operator already removed from the fleet.
func TestWaitForCellHealthy(t *testing.T) {
	tests := []struct {
		name       string
		responses  []probeResponse // one entry per successive Probe call
		ctxTimeout time.Duration   // 0 = no ctx cancellation
		maxWait    time.Duration
		wantErr    string // substring; empty means expect no error
		wantCalls  int    // 0 = don't check
	}{
		{
			name:      "healthy on first poll",
			responses: []probeResponse{{result: fleet.ProbeResult{OK: true, CellVersion: "0.0.86"}}},
			maxWait:   500 * time.Millisecond,
			wantErr:   "",
			wantCalls: 1,
		},
		{
			name: "healthy after three not-yet-ready polls",
			responses: []probeResponse{
				{result: fleet.ProbeResult{OK: false, Reason: "dial tcp: lookup <host>: no such host"}},
				{result: fleet.ProbeResult{OK: false, Reason: "HTTP 502"}},
				{result: fleet.ProbeResult{OK: false, Reason: "HTTP 503"}},
				{result: fleet.ProbeResult{OK: true, CellVersion: "0.0.86"}},
			},
			maxWait:   2 * time.Second,
			wantErr:   "",
			wantCalls: 4,
		},
		{
			name: "healthy after ALB warmup returns 404",
			responses: []probeResponse{
				{result: fleet.ProbeResult{OK: false, Reason: "HTTP 404: default backend", CellStatus: 404}},
				{result: fleet.ProbeResult{OK: true, CellVersion: "0.0.86"}},
			},
			maxWait:   2 * time.Second,
			wantErr:   "",
			wantCalls: 2,
		},
		{
			name: "times out with the last-seen diagnostic tail intact",
			responses: []probeResponse{
				{result: fleet.ProbeResult{OK: false, Reason: "dial tcp: lookup api.aws-sandbox-usw2-dev.cells.witself.witwave.ai: no such host"}},
			},
			maxWait: 100 * time.Millisecond,
			// The Reason string must survive to the final error unchanged
			// so the operator sees the actual failure mode, not a
			// truncated blob.
			wantErr: "no such host",
		},
		{
			name:       "context cancellation returns immediately",
			responses:  []probeResponse{{result: fleet.ProbeResult{OK: false, Reason: "not yet"}}},
			ctxTimeout: 50 * time.Millisecond,
			maxWait:    5 * time.Second,
			wantErr:    "context",
		},
		{
			name: "deregistered cell fails fast without waiting",
			responses: []probeResponse{
				{err: fleet.ErrNotRegistered},
			},
			maxWait:   5 * time.Second,
			wantErr:   "not registered",
			wantCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prober := &fakeProber{responses: tc.responses}

			ctx := context.Background()
			if tc.ctxTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.ctxTimeout)
				defer cancel()
			}

			// Poll interval much shorter than maxWait so the retry loop
			// gets several iterations if the test's response list has
			// several transients.
			pollEvery := max(tc.maxWait/5, 5*time.Millisecond)
			err := waitForCellHealthy(ctx, prober, "aws-sandbox-usw2-dev", tc.maxWait, pollEvery)

			if tc.wantCalls > 0 && int(prober.calls.Load()) != tc.wantCalls {
				t.Errorf("calls = %d, want %d", prober.calls.Load(), tc.wantCalls)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// probeResponse is one scripted answer for the fake prober: either a
// ProbeResult (probe succeeded, cell may or may not be ready) or an err
// (the probe itself failed — network, auth, unknown cell).
type probeResponse struct {
	result fleet.ProbeResult
	err    error
}

// fakeProber answers Probe() calls from a scripted list. The last entry
// repeats for any extra calls — useful for "cell never becomes ready"
// timeout tests.
type fakeProber struct {
	responses []probeResponse
	calls     atomic.Int32
}

func (f *fakeProber) Probe(_ context.Context, _ string) (fleet.ProbeResult, error) {
	idx := int(f.calls.Add(1)) - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	r := f.responses[idx]
	return r.result, r.err
}
