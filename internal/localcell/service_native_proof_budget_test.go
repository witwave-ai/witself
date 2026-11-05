package localcell_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const nativeServiceProofRowTimeout = 15 * time.Minute
const nativeServiceCleanupReserve = 75 * time.Second
const nativeServiceProofBound = nativeServiceProofRowTimeout - nativeServiceCleanupReserve - nativeServiceHostBound

// One absolute deadline bounds the composition, even when phase caps sum to
// more than the row permits. Cleanup keeps its independent reserved parent.
type nativeServiceProofBudget struct {
	ctx      context.Context
	started  time.Time
	deadline time.Time
	bound    time.Duration
	once     sync.Once
}

func newNativeServiceProofBudget(ctx context.Context, bound time.Duration) *nativeServiceProofBudget {
	deadline, _ := ctx.Deadline()
	return &nativeServiceProofBudget{ctx: ctx, started: deadline.Add(-bound), deadline: deadline, bound: bound}
}

func (b *nativeServiceProofBudget) phase(parent context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	deadline := b.deadline
	if capped := time.Now().Add(limit); capped.Before(deadline) {
		deadline = capped
	}
	return context.WithDeadline(parent, deadline)
}

func (d *nativeServiceFailureDiagnostic) logProofBudget() {
	if d.proof == nil || d.proof.ctx.Err() != context.DeadlineExceeded {
		return
	}
	d.proof.once.Do(func() {
		d.logf("proof_budget_exhausted phase=%s elapsed=%s bound=%s", nativeServiceStep(d.step), time.Since(d.proof.started), d.proof.bound)
	})
}

// A context-unaware hook finishes beyond the proof deadline but below its own
// ten-second limit. A detached parent deliberately tests the budget's absolute
// deadline independently of context's automatic parent-deadline inheritance.
func TestNativeServiceProofBudgetExhaustion(t *testing.T) {
	const bound = time.Second
	proofCtx, cancel := context.WithTimeout(t.Context(), bound)
	defer cancel()
	proof := newNativeServiceProofBudget(proofCtx, bound)
	phaseCtx, cancelPhase := proof.phase(t.Context(), 10*time.Second)
	defer cancelPhase()
	// Wait until the known absolute deadline, then spend a small proportional
	// amount beyond it. This does not assume how quickly the scheduler ran us.
	hook := func(ctx context.Context) error {
		<-proofCtx.Done()
		time.Sleep(bound / 10)
		return ctx.Err()
	}
	phaseErr := hook(phaseCtx)
	deadline, ok := phaseCtx.Deadline()
	if !ok || deadline.After(proof.deadline) {
		t.Fatal("proof phase deadline exceeded the whole-proof deadline")
	}
	if phaseErr != context.DeadlineExceeded {
		t.Fatal("context-unaware hook escaped the whole-proof deadline")
	}
	var lines []string
	diagnostic := nativeServiceFailureDiagnostic{proof: proof, logf: func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}}
	diagnostic.enter("write_witness")
	diagnostic.logProofBudget()
	diagnostic.logProofBudget()
	if len(lines) != 1 {
		t.Fatal("proof exhaustion must emit exactly one diagnostic line")
	}
	prefix, suffix := "proof_budget_exhausted phase=write_witness elapsed=", " bound=1s"
	if !strings.HasPrefix(lines[0], prefix) || !strings.HasSuffix(lines[0], suffix) {
		t.Fatal("proof exhaustion diagnostic lost its exact phase elapsed bound line")
	}
	elapsed, err := time.ParseDuration(strings.TrimSuffix(strings.TrimPrefix(lines[0], prefix), suffix))
	if err != nil || elapsed < bound || lines[0] != fmt.Sprintf("proof_budget_exhausted phase=write_witness elapsed=%s bound=1s", elapsed) {
		t.Fatal("proof exhaustion diagnostic lost its exact phase elapsed bound line")
	}
}

func TestNativeServiceProofBudgetPhaseCaps(t *testing.T) {
	proofCtx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	proof := newNativeServiceProofBudget(proofCtx, time.Minute)
	for _, limit := range []time.Duration{time.Second, 10 * time.Second, time.Hour} {
		started := time.Now()
		ctx, stop := proof.phase(proofCtx, limit)
		deadline, ok := ctx.Deadline()
		stop()
		if !ok || deadline.After(proof.deadline) || deadline.After(time.Now().Add(limit)) || deadline.Before(started) {
			t.Fatal("proof phase did not retain its limit and remaining deadline")
		}
	}
	var lines []string
	d := nativeServiceFailureDiagnostic{proof: proof, logf: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }}
	d.enter("ready")
	d.logProofBudget()
	if len(lines) != 0 {
		t.Fatal("live proof emitted a budget exhaustion diagnostic")
	}
}
