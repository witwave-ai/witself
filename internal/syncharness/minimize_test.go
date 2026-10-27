package syncharness

import (
	"errors"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"
)

type reducerModel struct{ prepared, repaired, closed bool }

func (m *reducerModel) Apply(step Step, payloads []PayloadShape) (Observation, error) {
	o := Observation{Invariant: "no_fresh_action", Outcome: ModelPass}
	for _, fault := range step.Faults {
		o.Assertions = append(o.Assertions, fault.Observable)
	}
	if m.closed {
		return o, errors.New("closed adapter")
	}
	if step.Event.Operation == "prepare" {
		m.prepared = true
	}
	shape := slices.ContainsFunc(payloads, func(p PayloadShape) bool {
		return p.Input == "request" && slices.Contains(p.Fields, "effect") && p.Elements > 0
	})
	lost := slices.ContainsFunc(step.Faults, func(f Fault) bool { return f.Class == DelayedLostACK })
	if step.Event.Operation == "retry" && m.prepared && shape && lost && !m.repaired {
		o.Outcome = Fail
	}
	return o, nil
}

func (m *reducerModel) Close() error { m.closed = true; return nil }

func reducerTrace(t *testing.T) (Trace, []PayloadShape) {
	t.Helper()
	scenario := modelScenario(29)
	scenario.ID, scenario.ExpectedInvariant = "B04", "no_fresh_action"
	scenario.Inputs = []string{"request", "irrelevant"}
	first, noise, last := modelEvent("a"), modelEvent("b"), modelEvent("c")
	first.Operation, noise.Operation, last.Operation = "prepare", "noise", "retry"
	noise.Actor = "actor-R"
	last.Requires, last.Checkpoint = []uint64{1}, ReturnedACK
	first.Alternatives, noise.Alternatives, last.Alternatives = []string{"fixed"}, []string{"fixed"}, []string{"fixed"}
	observable, _ := FaultContract(DelayedLostACK)
	unused, _ := FaultContract(Partition)
	faults := FaultPlan{{Class: DelayedLostACK, Node: L1, Channel: "model", Operation: "retry", Checkpoint: ReturnedACK, Observable: observable}, {Class: Partition, Node: L1, Channel: "model", Operation: "retry", Checkpoint: ReturnedACK, Observable: unused}}
	payloads := []PayloadShape{{Input: "request", Fields: []string{"effect", "padding"}, Elements: 3}, {Input: "irrelevant", Fields: []string{"padding"}, Elements: 2}}
	model := &reducerModel{}
	trace := runModel(t, scenario, faults, []Event{first, noise, last}, func(s Step) (Observation, error) { return model.Apply(s, payloads) })
	return trace, payloads
}

func TestSeedTraceReducerReproduces(t *testing.T) {
	trace, payloads := reducerTrace(t)
	original := traceBytes(t, trace)
	var adapters []*reducerModel
	factory := func() (ReplayAdapter, error) { m := &reducerModel{}; adapters = append(adapters, m); return m, nil }
	reduced, err := Minimize(trace, payloads, factory)
	if err != nil {
		t.Fatal(err)
	}
	if len(reduced.Schedule.Steps) != 2 || reduced.Schedule.Steps[0].Ordinal != 1 || reduced.Schedule.Steps[1].Ordinal != 3 || !slices.Equal(reduced.Schedule.Steps[1].Event.Requires, []uint64{1}) || len(reduced.Schedule.Scenario.Actors) != 1 || len(reduced.Schedule.Scenario.Inputs) != 1 || len(reduced.Schedule.Payloads) != 1 || reduced.Schedule.Payloads[0].Elements != 1 || !slices.Equal(reduced.Schedule.Payloads[0].Fields, []string{"effect"}) || len(reduced.Schedule.Steps[1].Faults) != 1 {
		t.Fatal("reducer did not minimize all four dimensions with causal closure")
	}
	if reduced.Attempts != len(adapters) {
		t.Fatal("reduction reused topology")
	}
	for _, adapter := range adapters {
		if !adapter.closed {
			t.Fatal("replay leaked adapter")
		}
	}
	if !reflect.DeepEqual(original, traceBytes(t, trace)) {
		t.Fatal("reducer mutated original trace")
	}
	repaired, err := ReplayRecorded(reduced.Schedule, func() (ReplayAdapter, error) { return &reducerModel{repaired: true}, nil })
	if err != nil || repaired.Outcome != ModelPass {
		t.Fatal("repaired model failed reduced regression")
	}
	raw, err := reduced.Schedule.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile("testdata/reduced/B04.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := DecodeRecordedSchedule(committed)
	if err != nil {
		t.Fatal("committed reduced fixture invalid")
	}
	loadedBytes, _ := loaded.Bytes()
	if !reflect.DeepEqual(raw, loadedBytes) {
		t.Fatal("reviewed reduced fixture drifted")
	}
}

type replayFunc struct {
	apply func(Step, []PayloadShape) (Observation, error)
}

func (r replayFunc) Apply(s Step, p []PayloadShape) (Observation, error) { return r.apply(s, p) }
func (replayFunc) Close() error                                          { return nil }

func TestTraceReducerRejectsTimeoutAndChangedInvariant(t *testing.T) {
	trace, payloads := reducerTrace(t)
	for _, mode := range []string{"timeout", "different_invariant", "different_observation", "candidate_timeout"} {
		t.Run(mode, func(t *testing.T) {
			attempts := 0
			factory := func() (ReplayAdapter, error) {
				attempts++
				attempt := attempts
				m := &reducerModel{}
				return replayFunc{apply: func(s Step, p []PayloadShape) (Observation, error) {
					o, err := m.Apply(s, p)
					if mode == "timeout" || (mode == "candidate_timeout" && attempt > 1) {
						return o, errors.New("timeout")
					}
					if mode == "different_invariant" {
						o.Invariant = "another_invariant"
					}
					if mode == "different_observation" && s.Event.Operation == "noise" {
						o.Outcome = Fail
					}
					return o, err
				}}, nil
			}
			if _, err := Minimize(trace, payloads, factory); err == nil {
				t.Fatal("non-reproducer accepted")
			}
		})
	}
}

func TestRecordedScheduleValidation(t *testing.T) {
	trace, payloads := reducerTrace(t)
	changedPlan := copyTrace(t, trace)
	changedPlan.FaultPlan = nil
	if _, err := RecordTrace(changedPlan, payloads); err == nil {
		t.Fatal("recorded fault plan disagreed with selected faults")
	}
	s, err := RecordTrace(trace, payloads)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*RecordedSchedule){
		func(s *RecordedSchedule) { s.Steps[0].Event.Channel = "postgres://private" },
		func(s *RecordedSchedule) {
			s.Scenario.Inputs[0] = "postgres://private"
			s.Payloads[0].Input = "postgres://private"
		},
		func(s *RecordedSchedule) { s.Steps[2].Event.Requires = []uint64{999} },
		func(s *RecordedSchedule) { s.Steps[0].Ordinal = s.Steps[1].Ordinal },
		func(s *RecordedSchedule) { s.Scenario.Inputs = []string{"postgres://private"} },
		func(s *RecordedSchedule) { s.Steps[0].Choice = "unselected" },
		func(s *RecordedSchedule) { s.Payloads[0].Fields = []string{"value with private spaces"} },
		func(s *RecordedSchedule) { s.Steps[2].Faults[0].Operation = "wrong" },
		func(s *RecordedSchedule) { s.Scenario.PRNG = "unknown" },
		func(s *RecordedSchedule) { s.Steps[0].Now = time.Time{} },
	} {
		changed := cloneRecorded(s)
		mutate(&changed)
		if _, err := changed.Bytes(); err == nil {
			t.Fatal("invalid or value-bearing trace accepted")
		}
	}
	raw, _ := s.Bytes()
	if _, err := DecodeRecordedSchedule(append(raw, []byte(" {}")...)); err == nil {
		t.Fatal("trailing record accepted")
	}
}

func TestReducedRegressionFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/reduced/B04.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := DecodeRecordedSchedule(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, repaired := range []bool{false, true} {
		result, err := ReplayRecorded(s, func() (ReplayAdapter, error) { return &reducerModel{repaired: repaired}, nil })
		if err != nil || (result.Outcome == Fail) == repaired {
			t.Fatal("reduced fixture failed to distinguish repaired model")
		}
	}
}
