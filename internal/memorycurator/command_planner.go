package memorycurator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/witwave-ai/witself/internal/client"
)

// ErrPlannerOutputLimit and ErrPlannerStderrLimit report bounded child-stream
// overflow.
var (
	ErrPlannerOutputLimit = errors.New("curator planner stdout exceeded its limit")
	ErrPlannerStderrLimit = errors.New("curator planner stderr exceeded its limit")
)

// CommandPlanner invokes a trusted local planner with an argv vector. It never
// invokes a shell and never expands planner arguments. The child receives only
// PlannerEnvelope on stdin; credentials remain in the parent runner.
type CommandPlanner struct {
	Path string
	Args []string
	Dir  string
	// Env overlays the inherited non-Witself environment. WITSELF_* entries
	// are always stripped and WITSELF_CURATOR_SESSION is always forced to 1.
	Env            []string
	MaxOutputBytes int
	MaxStderrBytes int
}

// Plan invokes the configured command and validates its returned plan draft.
func (p CommandPlanner) Plan(ctx context.Context, envelope PlannerEnvelope) (json.RawMessage, error) {
	if strings.TrimSpace(p.Path) == "" {
		return nil, errors.New("curator planner executable is required")
	}
	if envelope.Schema != PlannerEnvelopeSchemaV1 || envelope.RequestID == "" || envelope.RunID == "" || envelope.FencingGeneration < 1 || envelope.Policy.PlanSchema != MemoryPlanSchemaV1 {
		return nil, errors.New("curator planner envelope is incomplete")
	}
	if envelope.MaterializedInputs == nil {
		envelope.MaterializedInputs = []client.MemoryCurationRunInput{}
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode curator planner envelope: %w", err)
	}
	maxOutput := p.MaxOutputBytes
	if maxOutput == 0 {
		maxOutput = DefaultMaxPlannerOutputBytes
	}
	maxStderr := p.MaxStderrBytes
	if maxStderr == 0 {
		maxStderr = DefaultMaxPlannerStderrBytes
	}
	if maxOutput < 1 || maxOutput > DefaultMaxPlannerOutputBytes || maxStderr < 1 || maxStderr > DefaultMaxPlannerStderrBytes {
		return nil, errors.New("curator planner stream limits are invalid")
	}

	command := exec.CommandContext(ctx, p.Path, p.Args...)
	command.Stdin = bytes.NewReader(payload)
	command.Dir = p.Dir
	// Provider credentials, HOME, PATH, and other runtime settings survive, but
	// Witself variables are withheld from the inference child. The forced
	// session marker lets provider hooks avoid recording the curator itself and
	// recursively scheduling more curation.
	command.Env = plannerEnvironment(os.Environ(), p.Env)
	stdout := &cappedBuffer{limit: maxOutput, limitErr: ErrPlannerOutputLimit}
	stderr := &cappedBuffer{limit: maxStderr, limitErr: ErrPlannerStderrLimit}
	command.Stdout = stdout
	command.Stderr = stderr
	runErr := command.Run()
	if stdout.exceeded {
		return nil, ErrPlannerOutputLimit
	}
	if stderr.exceeded {
		return nil, ErrPlannerStderrLimit
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if runErr != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("curator planner command failed: %w: %s", runErr, detail)
		}
		return nil, fmt.Errorf("curator planner command failed: %w", runErr)
	}
	raw := append(json.RawMessage(nil), stdout.Bytes()...)
	if err := validatePlanDraftForLimit(raw, envelope.Policy.MaximumActions); err != nil {
		return nil, err
	}
	return raw, nil
}

type cappedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	limitErr error
}

func (b *cappedBuffer) Write(value []byte) (int, error) {
	if b.exceeded {
		return 0, b.limitErr
	}
	remaining := b.limit - b.buffer.Len()
	if len(value) > remaining {
		if remaining > 0 {
			_, _ = b.buffer.Write(value[:remaining])
		}
		b.exceeded = true
		return remaining, b.limitErr
	}
	return b.buffer.Write(value)
}

func (b *cappedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *cappedBuffer) String() string { return b.buffer.String() }

func plannerEnvironment(inherited, overlay []string) []string {
	values := make(map[string]string, len(inherited)+len(overlay)+1)
	order := make([]string, 0, len(inherited)+len(overlay)+1)
	add := func(entry string) {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.HasPrefix(strings.ToUpper(name), "WITSELF_") {
			return
		}
		if _, exists := values[name]; !exists {
			order = append(order, name)
		}
		values[name] = value
	}
	for _, entry := range inherited {
		add(entry)
	}
	for _, entry := range overlay {
		add(entry)
	}
	result := make([]string, 0, len(order)+1)
	for _, name := range order {
		result = append(result, name+"="+values[name])
	}
	return append(result, "WITSELF_CURATOR_SESSION=1")
}
