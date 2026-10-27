package syncharness

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
)

// PayloadShape describes public synthetic structure only. No payload values,
// identities, connection strings or keys belong in a regression artifact.
type PayloadShape struct {
	Input    string   `json:"input"`
	Fields   []string `json:"fields"`
	Elements uint32   `json:"elements"`
}

// RecordedSchedule retains selected choices and original insertion ordinals.
// Replay executes this order directly, without drawing another random schedule.
type RecordedSchedule struct {
	Version  int            `json:"version"`
	Scenario ScenarioRecord `json:"scenario"`
	Steps    []Step         `json:"steps"`
	Payloads []PayloadShape `json:"payload_shapes"`
}

// ReplayResult identifies an invariant failure separately from an adapter error
// or timeout. Only a completed run with the same invariant can be minimized.
type ReplayResult struct {
	Observations []Observation
	Invariant    string
	Outcome      EvidenceStatus
}

// ReplayAdapter is owned by one replay. Factories must allocate fresh state;
// Close releases that state even when replay or validation fails.
type ReplayAdapter interface {
	Apply(Step, []PayloadShape) (Observation, error)
	Close() error
}

// ReplayFactory creates a fresh topology for every attempted reduction.
type ReplayFactory func() (ReplayAdapter, error)

var (
	// ErrInvalidTrace refuses malformed or value-bearing portable artifacts.
	ErrInvalidTrace = errors.New("sync harness: invalid recorded schedule")
	// ErrNotReproducer distinguishes changed invariants/timeouts from reproduction.
	ErrNotReproducer = errors.New("sync harness: same invariant did not reproduce")
	// ErrReplayAdapter is deliberately value-free, even for database failures.
	ErrReplayAdapter = errors.New("sync harness: replay adapter failed")
)

// RecordTrace converts a completed trace without recomputing its choices.
func RecordTrace(trace Trace, payloads []PayloadShape) (RecordedSchedule, error) {
	if trace.Version != TraceVersion || len(trace.Events) != len(trace.Steps) || trace.FaultPlan.Validate() != nil {
		return RecordedSchedule{}, ErrInvalidTrace
	}
	s := RecordedSchedule{Version: 1, Scenario: cloneScenarioRecord(trace.Scenario), Payloads: clonePayloads(payloads)}
	reached := make(map[Fault]bool)
	for _, step := range trace.Steps {
		if !slices.ContainsFunc(trace.Events, func(e ScheduledEvent) bool { return reflect.DeepEqual(e, step.ScheduledEvent) }) {
			return RecordedSchedule{}, ErrInvalidTrace
		}
		if !reflect.DeepEqual(trace.FaultPlan.matching(step.Event), step.Faults) {
			return RecordedSchedule{}, ErrInvalidTrace
		}
		for _, fault := range step.Faults {
			reached[fault] = true
		}
		s.Steps = append(s.Steps, cloneStep(step.Step))
	}
	if len(reached) != len(trace.FaultPlan) {
		return RecordedSchedule{}, ErrInvalidTrace
	}
	return s, s.Validate()
}

// Validate checks causal closure, symbolic metadata and exact selected faults.
// Removed events keep their original ordinal; renumbering would lose provenance.
func (s RecordedSchedule) Validate() error {
	r := s.Scenario
	if s.Version != 1 || r.Version != ScenarioVersion || r.PRNG != PRNGVersion || r.Seed > MaxSeed || r.ClockStart.IsZero() || !validRecordedSymbol(r.ID) || !validRecordedSymbol(r.ExpectedInvariant) || len(s.Steps) == 0 || len(s.Steps) > MaxSteps || len(r.Actors) == 0 {
		return ErrInvalidTrace
	}
	for _, values := range [][]string{r.Actors, r.Inputs, r.Capabilities} {
		for i, value := range values {
			if !validRecordedSymbol(value) || slices.Contains(values[:i], value) {
				return ErrInvalidTrace
			}
		}
	}
	seen := make(map[uint64]bool)
	for _, step := range s.Steps {
		e := step.Event
		if step.Ordinal == 0 || step.Ordinal > MaxSteps || seen[step.Ordinal] || !slices.Contains(r.Actors, e.Actor) || !validNode(e.Node) || !validRecordedSymbol(e.Channel) || !validRecordedSymbol(e.Operation) || !validRecordedSymbol(e.Barrier) || !slices.Contains(Checkpoints(), e.Checkpoint) || e.At < 0 || step.Now.IsZero() || step.Draw > MaxSeed || !slices.Contains(e.Alternatives, step.Choice) {
			return ErrInvalidTrace
		}
		for i, alternative := range e.Alternatives {
			if !validRecordedSymbol(alternative) || slices.Contains(e.Alternatives[:i], alternative) {
				return ErrInvalidTrace
			}
		}
		for i, prerequisite := range e.Requires {
			if !seen[prerequisite] || prerequisite >= step.Ordinal || slices.Contains(e.Requires[:i], prerequisite) {
				return ErrInvalidTrace
			}
		}
		if FaultPlan(step.Faults).Validate() != nil || !reflect.DeepEqual(FaultPlan(step.Faults).matching(e), step.Faults) {
			return ErrInvalidTrace
		}
		seen[step.Ordinal] = true
	}
	inputs := make(map[string]bool)
	for _, payload := range s.Payloads {
		if !slices.Contains(r.Inputs, payload.Input) || inputs[payload.Input] || payload.Elements > 1024 || len(payload.Fields) > 64 {
			return ErrInvalidTrace
		}
		inputs[payload.Input] = true
		for i, field := range payload.Fields {
			if !validRecordedSymbol(field) || slices.Contains(payload.Fields[:i], field) {
				return ErrInvalidTrace
			}
		}
	}
	return nil
}

// Bytes validates before serialization so callers cannot accidentally commit a
// raw adapter error, DSN, payload value or secret-shaped hex string.
func (s RecordedSchedule) Bytes() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(s, "", "  ")
}

// DecodeRecordedSchedule rejects unknown fields and trailing documents. The
// canonical round-trip also refuses duplicate fields and alternate spellings.
func DecodeRecordedSchedule(raw []byte) (RecordedSchedule, error) {
	if len(raw) > 8<<20 {
		return RecordedSchedule{}, ErrInvalidTrace
	}
	var s RecordedSchedule
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&s) != nil || d.Decode(new(any)) != io.EOF || s.Validate() != nil {
		return RecordedSchedule{}, ErrInvalidTrace
	}
	canonical, _ := s.Bytes()
	var compactRaw, compactCanonical bytes.Buffer
	if json.Compact(&compactRaw, raw) != nil || json.Compact(&compactCanonical, canonical) != nil || !bytes.Equal(compactRaw.Bytes(), compactCanonical.Bytes()) {
		return RecordedSchedule{}, ErrInvalidTrace
	}
	return s, nil
}

func clonePayloads(in []PayloadShape) []PayloadShape {
	out := slices.Clone(in)
	for i := range out {
		out[i].Fields = slices.Clone(out[i].Fields)
	}
	return out
}

func cloneRecorded(in RecordedSchedule) RecordedSchedule {
	out := in
	out.Scenario = cloneScenarioRecord(in.Scenario)
	out.Payloads = clonePayloads(in.Payloads)
	out.Steps = slices.Clone(in.Steps)
	for i := range out.Steps {
		out.Steps[i] = cloneStep(out.Steps[i])
	}
	return out
}

// ReplayRecorded always allocates a new adapter and runs recorded ordinals in
// order. A timeout/error cannot be represented as a completed failing invariant.
func ReplayRecorded(s RecordedSchedule, factory ReplayFactory) (result ReplayResult, err error) {
	if s.Validate() != nil || factory == nil {
		return result, ErrInvalidTrace
	}
	adapter, err := factory()
	if err != nil || adapter == nil {
		return result, ErrReplayAdapter
	}
	defer func() {
		if adapter.Close() != nil {
			err = ErrReplayAdapter
		}
	}()
	result.Outcome = ModelPass
	for _, step := range s.Steps {
		observation, applyErr := adapter.Apply(cloneStep(step), clonePayloads(s.Payloads))
		if applyErr != nil {
			return result, ErrReplayAdapter
		}
		if !validObservation(s.Scenario.ExpectedInvariant, step.Faults, observation) {
			return result, ErrObservation
		}
		if observation.Outcome != ModelPass && observation.Outcome != Fail {
			return result, ErrCapabilityMissing
		}
		result.Observations = append(result.Observations, observation)
		if observation.Outcome == Fail {
			result.Outcome, result.Invariant = Fail, observation.Invariant
		} else if result.Outcome != Fail && observation.Outcome != ModelPass {
			result.Outcome = observation.Outcome
		}
	}
	return result, nil
}

// Recorded symbols deliberately exclude URI and path delimiters.
func validRecordedSymbol(value string) bool {
	return validSymbol(value) && !strings.ContainsAny(value, ":/\\")
}
