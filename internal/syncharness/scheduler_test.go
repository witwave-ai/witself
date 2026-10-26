package syncharness

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

func modelScenario(seed uint64) Scenario {
	return Scenario{Version: ScenarioVersion, ID: "B02", Seed: seed, ClockStart: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), Inputs: []string{"object-a", "account-a"}, Actors: []Actor{{Name: "actor-L1", Identity: []byte{1, 2, 3}}, {Name: "actor-R", Identity: []byte{4, 5, 6}}}, Capabilities: []string{"model-scheduler"}, ExpectedInvariant: "schedule-replays"}
}

func modelEvent(barrier string) Event {
	return Event{Actor: "actor-L1", Node: L1, Channel: "model", Operation: "sentinel", Checkpoint: DomainWrite, Barrier: barrier, Alternatives: []string{"left", "right", "hold"}}
}

func modelObserver(step Step) (Observation, error) {
	result := Observation{Invariant: "schedule-replays", Outcome: ModelPass}
	for _, fault := range step.Faults {
		result.Assertions = append(result.Assertions, fault.Observable)
	}
	return result, nil
}

func runModel(t *testing.T, scenario Scenario, faults FaultPlan, events []Event, observer Observer) Trace {
	t.Helper()
	scheduler, err := NewScheduler(scenario, faults)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if _, err := scheduler.Enqueue(event); err != nil {
			t.Fatal(err)
		}
	}
	trace, err := scheduler.Run(observer)
	if err != nil {
		t.Fatal(err)
	}
	return trace
}

func traceBytes(t *testing.T, trace Trace) []byte {
	t.Helper()
	data, err := trace.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func copyTrace(t *testing.T, trace Trace) Trace {
	t.Helper()
	var result Trace
	if err := json.Unmarshal(traceBytes(t, trace), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDeterministicScheduleReplay(t *testing.T) {
	scenario := modelScenario(17)
	events := []Event{modelEvent("second"), modelEvent("first"), modelEvent("first")}
	events[0].At = time.Second
	events[1].Advance = -time.Millisecond
	events[2].Actor = "actor-R"
	events[2].Requires = []uint64{1}
	trace := runModel(t, scenario, nil, events, modelObserver)
	again := runModel(t, scenario, nil, events, modelObserver)
	if !bytes.Equal(traceBytes(t, trace), traceBytes(t, again)) {
		t.Fatal("same seed/inputs changed trace")
	}
	replayed, err := Replay(scenario, nil, modelObserver, trace)
	if err != nil || !bytes.Equal(traceBytes(t, trace), traceBytes(t, replayed)) {
		t.Fatal("recorded schedule failed exact replay")
	}
	other := runModel(t, modelScenario(18), nil, events, modelObserver)
	if bytes.Equal(traceBytes(t, trace), traceBytes(t, other)) || trace.Steps[0].Draw == other.Steps[0].Draw {
		t.Fatal("different seed did not change model choices")
	}
	var ordinals []uint64
	for _, step := range trace.Steps {
		ordinals = append(ordinals, step.Ordinal)
	}
	if !slices.Equal(ordinals, []uint64{2, 1, 3}) {
		t.Fatal("barrier/time/dependency order incorrect")
	}
	if !trace.Steps[0].ClockUncertain || !trace.Steps[2].ClockUncertain || trace.Steps[0].Now != scenario.ClockStart.Add(-time.Millisecond) {
		t.Fatal("rollback missing from replay record")
	}
}

func TestSchedulerBarrierAndOrdinalTieBreak(t *testing.T) {
	trace := runModel(t, modelScenario(1), nil, []Event{modelEvent("z"), modelEvent("a"), modelEvent("a")}, modelObserver)
	var ordinals []uint64
	for _, step := range trace.Steps {
		ordinals = append(ordinals, step.Ordinal)
	}
	if !slices.Equal(ordinals, []uint64{2, 3, 1}) {
		t.Fatal("named barrier then ordinal tie-break changed")
	}
}

func TestReplayRejectsChangedRecordOrInputs(t *testing.T) {
	scenario := modelScenario(7)
	trace := runModel(t, scenario, nil, []Event{modelEvent("barrier")}, modelObserver)
	mutations := []struct {
		name   string
		mutate func(*Trace)
	}{
		{"trace-version", func(trace *Trace) { trace.Version++ }},
		{"scenario-version", func(trace *Trace) { trace.Scenario.Version++ }},
		{"prng-version", func(trace *Trace) { trace.Scenario.PRNG = "unknown" }},
		{"ordinal", func(trace *Trace) { trace.Events[0].Ordinal++ }},
		{"event", func(trace *Trace) { trace.Events[0].Event.Operation = "other" }},
		{"draw", func(trace *Trace) { trace.Steps[0].Draw++ }},
		{"choice", func(trace *Trace) { trace.Steps[0].Choice = "forged" }},
		{"outcome", func(trace *Trace) { trace.Outcome = PostgresPass }},
		{"observation", func(trace *Trace) { trace.Steps[0].Observation.Outcome = ScaffoldOnly }},
		{"inputs", func(trace *Trace) { trace.Scenario.Inputs[0] = "other" }},
		{"capabilities", func(trace *Trace) { trace.Scenario.Capabilities = nil }},
		{"clock", func(trace *Trace) { trace.Steps[0].Now = trace.Steps[0].Now.Add(time.Second) }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := copyTrace(t, trace)
			mutation.mutate(&changed)
			if _, err := Replay(scenario, nil, modelObserver, changed); !errors.Is(err, ErrReplayMismatch) {
				t.Fatal("changed record accepted")
			}
		})
	}
	changed := modelScenario(7)
	changed.Actors[0].Identity[0]++
	if _, err := Replay(changed, nil, modelObserver, trace); !errors.Is(err, ErrReplayMismatch) {
		t.Fatal("changed supplied identity accepted")
	}
	failing := func(step Step) (Observation, error) {
		observation, err := modelObserver(step)
		observation.Outcome = Fail
		return observation, err
	}
	if _, err := Replay(scenario, nil, failing, trace); !errors.Is(err, ErrReplayMismatch) {
		t.Fatal("changed observer result accepted")
	}
	failed := runModel(t, scenario, nil, []Event{modelEvent("barrier")}, failing)
	if failed.Outcome != Fail {
		t.Fatal("failure lost")
	}
	if replay, err := Replay(scenario, nil, failing, failed); err != nil || replay.Outcome != Fail {
		t.Fatal("same failed invariant did not replay")
	}
}

func TestActorStreamsIndependentAndVersioned(t *testing.T) {
	a, err := NewActorRandom(42, "actor-a", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := NewActorRandom(42, "actor-a", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewActorRandom(42, "actor-b", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if a.Uint64() != baseline.Uint64() {
			t.Fatal("unrelated actor perturbed stream")
		}
		for range 7 {
			_ = b.Uint64()
		}
	}
	other, err := NewActorRandom(42, "actor-a", []byte{1, 2, 4})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err = NewActorRandom(42, "actor-a", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if other.Uint64() == baseline.Uint64() || PRNGVersion != "splitmix64-fnv1a-v1" {
		t.Fatal("identity/version separation missing")
	}
}

func TestSchedulerActorStreamIndependence(t *testing.T) {
	type drawChoice struct {
		draw   uint64
		choice string
	}
	run := func(t *testing.T, reverseActors bool, remoteDraws int) []drawChoice {
		t.Helper()
		scenario := modelScenario(42)
		if reverseActors {
			slices.Reverse(scenario.Actors)
		}
		// Equal times and barriers preserve insertion order: L1, R draws, L1.
		events := []Event{modelEvent("same-barrier")}
		for range remoteDraws {
			event := modelEvent("same-barrier")
			event.Actor = "actor-R"
			event.Node = R
			events = append(events, event)
		}
		events = append(events, modelEvent("same-barrier"))
		trace := runModel(t, scenario, nil, events, modelObserver)
		if len(trace.Steps) != len(events) {
			t.Fatal("scheduler did not execute every actor event")
		}
		var local []drawChoice
		for index, step := range trace.Steps {
			if step.Event.Actor != events[index].Actor {
				t.Fatal("actor interleaving changed")
			}
			if step.Event.Actor == "actor-L1" {
				local = append(local, drawChoice{draw: step.Draw, choice: step.Choice})
			}
		}
		if len(local) != 2 {
			t.Fatal("scheduler did not execute both L1 events")
		}
		return local
	}
	baseline := run(t, false, 0)
	for _, order := range []struct {
		name    string
		reverse bool
	}{
		{name: "L1-registered-first"},
		{name: "R-registered-first", reverse: true},
	} {
		t.Run(order.name, func(t *testing.T) {
			for _, interference := range []struct {
				name  string
				draws int
			}{
				{name: "no-R-draws"},
				{name: "one-R-draw", draws: 1},
				{name: "extra-R-draws", draws: 7},
			} {
				t.Run(interference.name, func(t *testing.T) {
					if got := run(t, order.reverse, interference.draws); !slices.Equal(got, baseline) {
						t.Fatal("L1 draw/choice sequence changed with actor registration order or R draws")
					}
				})
			}
		})
	}
}

// These decimal vectors were calculated with an independent integer-only
// reference, freezing stream compatibility for recorded PRNG version v1.
func TestActorRandomGoldenVector(t *testing.T) {
	stream, err := NewActorRandom(42, "actor-a", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []uint64{6698651657328414, 8673621219387231, 6385016414867458, 5796902068687697} {
		if got := stream.Uint64(); got != expected {
			t.Fatal("PRNG version v1 vector changed")
		}
	}
}

func TestSchedulerRejectsInvalidOrUnobservedWork(t *testing.T) {
	scheduler, err := NewScheduler(modelScenario(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	invalid := modelEvent("barrier")
	invalid.Requires = []uint64{1}
	if _, err := scheduler.Enqueue(invalid); !errors.Is(err, ErrInvalidEvent) {
		t.Fatal("future/self prerequisite accepted")
	}
	if _, err := scheduler.Run(nil); !errors.Is(err, ErrMissingObserver) {
		t.Fatal("missing observer passed")
	}
	trace, err := scheduler.Run(modelObserver)
	if err != nil || trace.Outcome != NotRun {
		t.Fatal("empty schedule manufactured proof")
	}
	if _, err := scheduler.Run(modelObserver); !errors.Is(err, ErrSchedulerUsed) {
		t.Fatal("consumed random streams reused")
	}
	if _, err := scheduler.Enqueue(modelEvent("barrier")); !errors.Is(err, ErrSchedulerUsed) {
		t.Fatal("enqueue after run accepted")
	}
	scheduler, err = NewScheduler(modelScenario(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	for range MaxSteps {
		if _, err := scheduler.Enqueue(modelEvent("barrier")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := scheduler.Enqueue(modelEvent("barrier")); !errors.Is(err, ErrStepBudget) {
		t.Fatal("step budget exceeded")
	}
}

func TestObserverCannotMutateSchedule(t *testing.T) {
	contract, _ := FaultContract(Partition)
	faults := FaultPlan{{Class: Partition, Node: L1, Channel: "model", Operation: "sentinel", Checkpoint: DomainWrite, Observable: contract}}
	observer := func(step Step) (Observation, error) {
		result, err := modelObserver(step)
		step.Event.Alternatives[0] = "tampered"
		step.Faults[0].Observable = "tampered"
		return result, err
	}
	trace := runModel(t, modelScenario(1), faults, []Event{modelEvent("barrier")}, observer)
	if trace.Events[0].Event.Alternatives[0] != "left" || trace.Steps[0].Event.Alternatives[0] != "left" || trace.Steps[0].Faults[0].Observable != contract {
		t.Fatal("observer mutated record")
	}
	if _, err := Replay(modelScenario(1), faults, modelObserver, trace); err != nil {
		t.Fatal("observer aliasing changed replay")
	}
}

func TestSchedulerDoesNotPromoteModelEvidence(t *testing.T) {
	for _, status := range []EvidenceStatus{ScaffoldOnly, NotRun, Fail, PostgresPass} {
		t.Run(string(status), func(t *testing.T) {
			scheduler, err := NewScheduler(modelScenario(1), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := scheduler.Enqueue(modelEvent("barrier")); err != nil {
				t.Fatal(err)
			}
			observer := func(Step) (Observation, error) {
				return Observation{Invariant: "schedule-replays", Outcome: status}, nil
			}
			trace, err := scheduler.Run(observer)
			if status == PostgresPass {
				if !errors.Is(err, ErrObservation) || trace.Outcome != Fail {
					t.Fatal("model claimed PostgreSQL evidence")
				}
				return
			}
			if err != nil || trace.Outcome == ModelPass || trace.Outcome == PostgresPass {
				t.Fatal("unexecuted or failed obligation became a pass")
			}
		})
	}
}
