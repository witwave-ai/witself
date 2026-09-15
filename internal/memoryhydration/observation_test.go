package memoryhydration

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestObservationCarriesNoPromptQueryOrContext(t *testing.T) {
	result := Result{Attempted: true, Injected: true, Context: "private context canary", Query: "private query canary", Reason: "private prompt token canary", Delivery: "private delivery canary", Elapsed: 125 * time.Millisecond, ContextBytes: 22, Elided: true, Outcome: OutcomeInjected}
	observation := NewObservation(result)
	raw, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private", "query", "prompt", "token", "reason", "delivery", `"context"`} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Fatalf("observation includes %q: %s", forbidden, raw)
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 7 || observation.Timestamp.IsZero() || !observation.Attempted || !observation.Injected || observation.ElapsedMS != 125 || observation.ContextBytes != 22 || !observation.Elided || observation.Outcome != OutcomeInjected {
		t.Fatalf("observation = %+v / %s", observation, raw)
	}
}

func TestTimeoutObservationOutcome(t *testing.T) {
	source := &hydrationSourceStub{selfFn: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	result, err := Execute(context.Background(), Config{Timeout: 5 * time.Millisecond}, exactBinding(), Request{Runtime: transcriptcapture.RuntimeCodex, Event: EventSessionStart}, source)
	if !errors.Is(err, context.DeadlineExceeded) || result.Outcome != OutcomeTimeout || !result.Attempted || result.Injected || result.ContextBytes != 0 || result.Elapsed < 5*time.Millisecond {
		t.Fatalf("timeout = %+v, %v", result, err)
	}
	observation := NewObservation(result)
	if observation.Outcome != OutcomeTimeout || observation.Injected || observation.ElapsedMS < 5 {
		t.Fatalf("timeout observation = %+v", observation)
	}
}

func TestHydrationObservationOutcomesAndElision(t *testing.T) {
	nilResult, err := Execute(context.Background(), Config{}, exactBinding(), Request{Runtime: transcriptcapture.RuntimeCodex, Event: EventSessionStart}, nil)
	if err == nil || nilResult.Outcome != OutcomeConfigError || nilResult.Elapsed <= 0 {
		t.Fatalf("nil source = %+v, %v", nilResult, err)
	}
	bindingResult, err := Execute(context.Background(), Config{}, Binding{}, Request{Runtime: transcriptcapture.RuntimeCodex, Event: EventSessionStart}, &hydrationSourceStub{})
	if err == nil || bindingResult.Outcome != OutcomeBindingMismatch || bindingResult.Elapsed <= 0 {
		t.Fatalf("invalid binding = %+v, %v", bindingResult, err)
	}
	tests := []struct {
		name             string
		cfg              Config
		binding          Binding
		request          Request
		source           *hydrationSourceStub
		outcome          Outcome
		injected, elided bool
	}{
		{name: "injected", outcome: OutcomeInjected, injected: true},
		{name: "server elision", source: &hydrationSourceStub{self: client.SelfDigest{Identity: exactIdentity(), Elided: true}}, outcome: OutcomeInjected, injected: true, elided: true},
		{name: "renderer elision", cfg: Config{MaximumBytes: 1024}, source: &hydrationSourceStub{self: client.SelfDigest{Identity: exactIdentity(), PrimaryFacts: []client.SelfFact{{Value: strings.Repeat("canary", 1024)}}}}, outcome: OutcomeInjected, injected: true, elided: true},
		{name: "record elision", source: &hydrationSourceStub{self: client.SelfDigest{Identity: exactIdentity(), SalientMemories: []client.SelfMemory{{ID: "mem_1", Snippet: strings.Repeat("canary", 1024)}}}}, outcome: OutcomeInjected, injected: true, elided: true},
		{name: "no context", request: Request{Runtime: transcriptcapture.RuntimeCodex, Event: EventUserPromptSubmit, Prompt: "write a parser"}, outcome: OutcomeNoContext},
		{name: "unsupported", request: Request{Runtime: transcriptcapture.RuntimeCursor, Event: EventSessionStart}, outcome: OutcomeNoContext},
		{name: "config error", cfg: Config{MaximumBytes: 1}, outcome: OutcomeConfigError},
		{name: "self error", source: &hydrationSourceStub{selfFn: func(context.Context) error { return errors.New("private backend canary") }}, outcome: OutcomeSelfError},
		{name: "binding mismatch", source: &hydrationSourceStub{self: client.SelfDigest{}}, outcome: OutcomeBindingMismatch},
		{name: "recall timeout", request: Request{Runtime: transcriptcapture.RuntimeCodex, Event: EventUserPromptSubmit, Prompt: "resume our prior plan"}, source: &hydrationSourceStub{self: client.SelfDigest{Identity: exactIdentity()}, recallErr: context.DeadlineExceeded}, outcome: OutcomeTimeout, injected: true},
		{name: "recall degraded", request: Request{Runtime: transcriptcapture.RuntimeCodex, Event: EventUserPromptSubmit, Prompt: "resume our prior plan"}, source: &hydrationSourceStub{self: client.SelfDigest{Identity: exactIdentity()}, recallErr: errors.New("private recall canary")}, outcome: OutcomeRecallDegraded, injected: true},
		{name: "degraded page", request: Request{Runtime: transcriptcapture.RuntimeCodex, Event: EventUserPromptSubmit, Prompt: "resume our prior plan"}, source: &hydrationSourceStub{self: client.SelfDigest{Identity: exactIdentity()}, recall: client.MemoryRecallPage{Degraded: true, DegradedReason: "unavailable"}}, outcome: OutcomeRecallDegraded, injected: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.request.Runtime == "" {
				tt.request = Request{Runtime: transcriptcapture.RuntimeCodex, Event: EventSessionStart}
			}
			if tt.binding.AccountID == "" {
				tt.binding = exactBinding()
			}
			if tt.source == nil {
				tt.source = &hydrationSourceStub{self: client.SelfDigest{Identity: exactIdentity()}}
			}
			result, _ := Execute(context.Background(), tt.cfg, tt.binding, tt.request, tt.source)
			if result.Outcome != tt.outcome || result.Injected != tt.injected || result.Elided != tt.elided || result.ContextBytes != len(result.Context) || result.Elapsed <= 0 {
				t.Fatalf("result = %+v; want outcome=%s injected=%t elided=%t", result, tt.outcome, tt.injected, tt.elided)
			}
		})
	}
}

func TestHydrationLedgerPermissionsRotationAndConcurrency(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	observation := NewObservation(Result{Attempted: true, Injected: true, Outcome: OutcomeInjected, Elapsed: time.Millisecond})
	const workers = 12
	var wg sync.WaitGroup
	errorsCh := make(chan error, workers)
	for range workers {
		wg.Go(func() { errorsCh <- AppendObservation(transcriptcapture.RuntimeCodex, observation) })
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(home, "capture", "hydration", "codex.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "\n") != workers {
		t.Fatalf("ledger has %d lines", strings.Count(string(raw), "\n"))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	// Seed just below the cap with valid complete records, then force rotation.
	line := raw[:strings.IndexByte(string(raw), '\n')+1]
	if err := os.WriteFile(path, []byte(strings.Repeat(string(line), (512*1024)/len(line))), 0600); err != nil {
		t.Fatal(err)
	}
	latest := observation
	latest.Timestamp = observation.Timestamp.Add(time.Second)
	if err := AppendObservation(transcriptcapture.RuntimeCodex, latest); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 512*1024 || len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("rotated ledger size = %d", len(raw))
	}
	observations, err := ReadObservations(transcriptcapture.RuntimeCodex, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) < 2 || !observations[len(observations)-1].Timestamp.Equal(latest.Timestamp) {
		t.Fatalf("latest record lost, count=%d", len(observations))
	}
	for _, record := range observations {
		if record.Outcome != OutcomeInjected {
			t.Fatalf("corrupt record = %+v", record)
		}
	}
}

func TestHydrationLedgerValidationAndWindowSummary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	missing, err := ReadObservations(transcriptcapture.RuntimeClaudeCode, now, now)
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing ledger = %v, %v", missing, err)
	}
	for i, outcome := range []Outcome{OutcomeInjected, OutcomeSelfError, OutcomeRecallDegraded, OutcomeOutputRejected, OutcomeNoContext} {
		record := Observation{Timestamp: now.Add(time.Duration(i) * time.Second), Attempted: true, Injected: outcome == OutcomeInjected || outcome == OutcomeRecallDegraded, ElapsedMS: float64((i + 1) * 100), Elided: outcome == OutcomeRecallDegraded, Outcome: outcome}
		if err := AppendObservation(transcriptcapture.RuntimeCodex, record); err != nil {
			t.Fatal(err)
		}
	}
	records, err := ReadObservations(transcriptcapture.RuntimeCodex, now.Add(time.Second), now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	summary := SummarizeObservations(records)
	if summary.Attempts != 3 || summary.Injected != 1 || summary.Failures != 3 || summary.ElidedCount != 1 || summary.HookOutputRejected != 1 || summary.P95LatencyMS != 400 || summary.MaxLatencyMS != 400 {
		t.Fatalf("summary = %+v", summary)
	}
	invalid := NewObservation(Result{Outcome: Outcome("private prompt token canary")})
	invalid.Outcome = Outcome("private prompt token canary")
	if err := AppendObservation(transcriptcapture.RuntimeCodex, invalid); err == nil {
		t.Fatal("arbitrary outcome accepted")
	}
	invalid.Outcome = OutcomeInjected
	invalid.ElapsedMS = math.Inf(1)
	if err := AppendObservation(transcriptcapture.RuntimeCodex, invalid); err == nil {
		t.Fatal("nonfinite latency accepted")
	}
	if err := AppendObservation("../escape", NewObservation(Result{Outcome: OutcomeInjected})); err == nil {
		t.Fatal("path traversal accepted")
	}
	// Corrupt or foreign ledger content must never become acceptance evidence.
	path := filepath.Join(home, "capture", "hydration", "codex.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("not json\n{\"timestamp\":\"2026-09-05T12:00:00Z\",\"outcome\":\"injected\",\"prompt\":\"private canary\"}\n")
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	records, err = ReadObservations(transcriptcapture.RuntimeCodex, time.Time{}, time.Time{})
	if err != nil || len(records) != 5 {
		t.Fatalf("corrupt lines affected evidence: count=%d err=%v", len(records), err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private prompt token canary") {
		t.Fatal("invalid observation persisted")
	}
	// A process interruption can leave a partial line. The next complete event
	// must remain readable instead of being joined to that fragment.
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"timestamp":`)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendObservation(transcriptcapture.RuntimeCodex, NewObservation(Result{Outcome: OutcomeNoContext})); err != nil {
		t.Fatal(err)
	}
	records, err = ReadObservations(transcriptcapture.RuntimeCodex, time.Time{}, time.Time{})
	if err != nil || len(records) != 6 {
		t.Fatalf("partial tail lost next event: count=%d err=%v", len(records), err)
	}
}

func TestHydrationLedgerRejectsNonRegularFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	dir := filepath.Join(home, "capture", "hydration")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "untouched")
	if err := os.WriteFile(target, []byte("canary"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "codex.jsonl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := AppendObservation(transcriptcapture.RuntimeCodex, NewObservation(Result{Outcome: OutcomeInjected})); err == nil {
		t.Fatal("symlink accepted")
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "canary" {
		t.Fatalf("symlink target modified: %q, %v", raw, err)
	}
}

func TestHydrationLedgerContentionIsBounded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	observation := NewObservation(Result{Outcome: OutcomeInjected})
	if err := AppendObservation(transcriptcapture.RuntimeCodex, observation); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "capture", "hydration", "codex.jsonl")
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if err := lockObservationFile(file); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := AppendObservation(transcriptcapture.RuntimeCodex, observation); err == nil {
		t.Fatal("contending writer did not fail open")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("ledger contention blocked hook for %v", elapsed)
	}
}
