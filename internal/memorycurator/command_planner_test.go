package memorycurator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

func TestCommandPlannerUsesArgvSanitizedEnvironmentAndSessionMarker(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wouldCreate := filepath.Join(t.TempDir(), "shell-injection")
	dangerousArgument := "; touch " + wouldCreate
	t.Setenv("CURATOR_HELPER_MODE", "valid")
	t.Setenv("CURATOR_EXPECT_ARG", dangerousArgument)
	t.Setenv("CURATOR_EXTRA", "inherited")
	t.Setenv("WITSELF_TOKEN", "must-not-reach-child")
	t.Setenv("WITSELF_TOKEN_FILE", "/must/not/reach/child")
	t.Setenv("WITSELF_ENDPOINT", "https://must-not-reach-child.invalid")

	planner := CommandPlanner{
		Path: executable,
		Args: []string{"-test.run=^TestCommandPlannerHelperProcess$", "--", dangerousArgument},
		Env: []string{
			"CURATOR_EXTRA=overlay",
			"WITSELF_TOKEN=also-must-not-reach-child",
			"WITSELF_CURATOR_SESSION=0",
		},
	}
	draft, err := planner.Plan(context.Background(), testPlannerEnvelope())
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if string(draft) != string(emptyPlan) {
		t.Fatalf("Plan() = %s", draft)
	}
	if _, err := os.Stat(wouldCreate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("argument was interpreted as shell syntax: stat error = %v", err)
	}
}

func TestCommandPlannerBoundsAndValidatesStreams(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		mode       string
		maxOutput  int
		maxStderr  int
		want       error
		withCancel bool
	}{
		{name: "malformed", mode: "malformed", want: ErrInvalidPlannerOutput},
		{name: "stdout limit", mode: "stdout-limit", maxOutput: 32, want: ErrPlannerOutputLimit},
		{name: "stderr limit", mode: "stderr-limit", maxStderr: 32, want: ErrPlannerStderrLimit},
		{name: "context cancellation", mode: "sleep", want: context.DeadlineExceeded, withCancel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CURATOR_HELPER_MODE", test.mode)
			ctx := context.Background()
			if test.withCancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 25*time.Millisecond)
				defer cancel()
			}
			planner := CommandPlanner{
				Path: executable, Args: []string{"-test.run=^TestCommandPlannerHelperProcess$"},
				MaxOutputBytes: test.maxOutput, MaxStderrBytes: test.maxStderr,
			}
			_, err := planner.Plan(ctx, testPlannerEnvelope())
			if !errors.Is(err, test.want) {
				t.Fatalf("Plan() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCommandPlannerHelperProcess(_ *testing.T) {
	mode := os.Getenv("CURATOR_HELPER_MODE")
	if mode == "" {
		return
	}
	fail := func(format string, values ...any) {
		_, _ = fmt.Fprintf(os.Stderr, format+"\n", values...)
		os.Exit(42)
	}
	switch mode {
	case "valid":
		if got := os.Getenv("WITSELF_CURATOR_SESSION"); got != "1" {
			fail("WITSELF_CURATOR_SESSION=%q", got)
		}
		for _, name := range []string{"WITSELF_TOKEN", "WITSELF_TOKEN_FILE", "WITSELF_ENDPOINT"} {
			if _, present := os.LookupEnv(name); present {
				fail("%s reached child", name)
			}
		}
		if got := os.Getenv("CURATOR_EXTRA"); got != "overlay" {
			fail("CURATOR_EXTRA=%q", got)
		}
		separator := -1
		for index, argument := range os.Args {
			if argument == "--" {
				separator = index
				break
			}
		}
		if separator < 0 || separator+1 >= len(os.Args) || os.Args[separator+1] != os.Getenv("CURATOR_EXPECT_ARG") {
			fail("argument was not preserved: %#v", os.Args)
		}
		var envelope PlannerEnvelope
		if err := json.NewDecoder(os.Stdin).Decode(&envelope); err != nil {
			fail("decode envelope: %v", err)
		}
		if envelope.Schema != PlannerEnvelopeSchemaV1 || envelope.RunID != "mrun_command" || len(envelope.MaterializedInputs) != 1 {
			fail("bad envelope: %#v", envelope)
		}
		_, _ = os.Stdout.Write(emptyPlan)
		os.Exit(0)
	case "malformed":
		_, _ = os.Stdout.WriteString("model commentary before JSON")
		os.Exit(0)
	case "stdout-limit":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 256))
		os.Exit(0)
	case "stderr-limit":
		_, _ = os.Stderr.WriteString(strings.Repeat("x", 256))
		os.Exit(0)
	case "sleep":
		time.Sleep(5 * time.Second)
		os.Exit(0)
	default:
		fail("unknown helper mode %q", mode)
	}
}

func testPlannerEnvelope() PlannerEnvelope {
	expires := time.Now().UTC().Add(time.Minute)
	return PlannerEnvelope{
		Schema: PlannerEnvelopeSchemaV1, RequestID: "mcrq_command", RequestGeneration: 1,
		RunID: "mrun_command", FencingGeneration: 1, LeaseExpiresAt: &expires,
		Policy: PlannerPolicy{
			PlanSchema:        MemoryPlanSchemaV1,
			AllowedOperations: []string{"create", "replace", "supersede", "relate", "propose_fact"},
			MaximumActions:    32,
		},
		MaterializedInputs: []client.MemoryCurationRunInput{{RunID: "mrun_command", Ordinal: 1, Kind: "cursor"}},
	}
}

func TestValidatePlanDraftIsStrict(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "duplicate member", raw: `{"schema":"witself.memory-plan.v1","schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}`},
		{name: "unknown member", raw: `{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[],"commentary":"hi"}`},
		{name: "trailing value", raw: `{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]} {}`},
		{name: "wrong payload", raw: `{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[{"ordinal":1,"operation":"relate","create":{}}]}`},
		{name: "noncontiguous ordinal", raw: `{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[{"ordinal":2,"operation":"relate","relate":{}}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validatePlanDraft([]byte(test.raw)); !errors.Is(err, ErrInvalidPlannerOutput) {
				t.Fatalf("validatePlanDraft() error = %v", err)
			}
		})
	}
	if err := validatePlanDraft(emptyPlan); err != nil {
		t.Fatalf("valid empty plan rejected: %v", err)
	}
}
