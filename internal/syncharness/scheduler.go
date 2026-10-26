package syncharness

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/fnv"
	"slices"
	"time"
)

// PRNGVersion freezes the integer algorithm and actor stream derivation.
const PRNGVersion = "splitmix64-fnv1a-v1"

// TraceVersion fixes the replay and future minimizer record shape.
const TraceVersion = 1

// Random is a frozen, non-cryptographic synthetic stream. Its 53-bit output is
// recorded as bounded decimal JSON numbers. Actor identity and name domain-
// separate streams, so another actor's draws cannot perturb this actor.
type Random struct{ state uint64 }

// NewActorRandom constructs an independent stream from public synthetic identity bytes.
func NewActorRandom(seed uint64, actor string, identity []byte) (*Random, error) {
	if seed > MaxSeed || !validSymbol(actor) || len(identity) == 0 || len(identity) > 64 {
		return nil, ErrInvalidScenario
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(PRNGVersion))
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(actor)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write([]byte(actor))
	_, _ = hash.Write(identity)
	return &Random{state: seed ^ hash.Sum64()}, nil
}

// Uint64 returns the next deterministic, exact-JSON-range random draw.
func (r *Random) Uint64() uint64 {
	r.state += 0x9e3779b97f4a7c15
	value := r.state
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return (value ^ (value >> 31)) >> 11
}

// Event declares causal prerequisites by insertion ordinal. Named barriers sort
// simultaneous ready events before insertion ordinal; neither random draws nor
// real time select a conflict winner. Alternatives are symbolic model choices.
type Event struct {
	Actor        string        `json:"actor"`
	Node         NodeID        `json:"node"`
	Channel      string        `json:"channel"`
	Operation    string        `json:"operation"`
	Checkpoint   Checkpoint    `json:"checkpoint"`
	Barrier      string        `json:"barrier"`
	At           time.Duration `json:"at_ns"`
	Advance      time.Duration `json:"advance_ns"`
	Alternatives []string      `json:"alternatives"`
	Requires     []uint64      `json:"requires,omitempty"`
}

// ScheduledEvent retains insertion identity independently of execution order.
type ScheduledEvent struct {
	Ordinal uint64 `json:"ordinal"`
	Event   Event  `json:"event"`
}

// ScenarioRecord contains symbolic replay inputs but no private identity bytes.
type ScenarioRecord struct {
	Version           int       `json:"scenario_version"`
	ID                string    `json:"scenario"`
	Seed              uint64    `json:"seed"`
	PRNG              string    `json:"prng_version"`
	ClockStart        time.Time `json:"clock_start"`
	Inputs            []string  `json:"symbolic_inputs"`
	Actors            []string  `json:"actors"`
	Capabilities      []string  `json:"capabilities"`
	ExpectedInvariant string    `json:"expected_invariant"`
}

// Step contains no actor identity bytes or data payload. The ordinal, barrier,
// and prerequisites allow a later minimizer to preserve causal dependencies.
type Step struct {
	ScheduledEvent
	Now            time.Time `json:"now"`
	ClockUncertain bool      `json:"clock_uncertain"`
	Draw           uint64    `json:"draw"`
	Choice         string    `json:"choice"`
	Faults         []Fault   `json:"faults,omitempty"`
}

// Observation states the checked invariant and each selected fault's observable.
type Observation struct {
	Invariant  string         `json:"invariant"`
	Outcome    EvidenceStatus `json:"outcome"`
	Assertions []string       `json:"assertions,omitempty"`
}

// TraceStep pairs one selected event with its independently evaluated observation.
type TraceStep struct {
	Step
	Observation Observation `json:"observation"`
}

// Trace is a portable schedule plus observations, not synchronization proof.
type Trace struct {
	Version   int              `json:"trace_version"`
	Scenario  ScenarioRecord   `json:"scenario"`
	FaultPlan FaultPlan        `json:"fault_plan"`
	Events    []ScheduledEvent `json:"events"`
	Steps     []TraceStep      `json:"steps"`
	Outcome   EvidenceStatus   `json:"outcome"`
}

// Bytes uses struct/slice ordering only. There are no maps or private inputs in
// this portable record; consumers must treat a loaded trace as untrusted data.
func (t Trace) Bytes() ([]byte, error) { return json.MarshalIndent(t, "", "  ") }

// Observer checks model state after a selected event and returns value-free evidence.
type Observer func(Step) (Observation, error)

var (
	// ErrInvalidEvent rejects malformed work or impossible causal prerequisites.
	ErrInvalidEvent = errors.New("sync harness: invalid event or causal prerequisite")
	// ErrStepBudget bounds model scheduling without silent truncation.
	ErrStepBudget = errors.New("sync harness: scheduler step budget exceeded")
	// ErrSchedulerUsed prevents accidental reuse of consumed random streams.
	ErrSchedulerUsed = errors.New("sync harness: scheduler already ran")
	// ErrMissingObserver prevents execution without an invariant check.
	ErrMissingObserver = errors.New("sync harness: model observer missing")
	// ErrObservation rejects missing invariants or fault assertions.
	ErrObservation = errors.New("sync harness: invariant or fault observable not asserted")
	// ErrReplayMismatch means recomputed metadata, events, or observations differ.
	ErrReplayMismatch = errors.New("sync harness: recorded schedule replay mismatch")
	// ErrFaultNotReached prevents a planned but unexercised fault from passing.
	ErrFaultNotReached = errors.New("sync harness: selected fault checkpoint not reached")
)

// Scheduler owns logical time, actor streams, and a bounded causal event queue.
type Scheduler struct {
	scenario ScenarioRecord
	clock    *Clock
	streams  map[string]*Random
	faults   FaultPlan
	events   []ScheduledEvent
	ran      bool
}

// NewScheduler snapshots validated inputs and creates independent actor streams.
func NewScheduler(scenario Scenario, faults FaultPlan) (*Scheduler, error) {
	if err := scenario.validate(); err != nil {
		return nil, err
	}
	if err := faults.Validate(); err != nil {
		return nil, err
	}
	scheduler := &Scheduler{
		scenario: ScenarioRecord{Version: scenario.Version, ID: scenario.ID, Seed: scenario.Seed, PRNG: PRNGVersion, ClockStart: scenario.ClockStart.UTC(), Inputs: slices.Clone(scenario.Inputs), Capabilities: slices.Clone(scenario.Capabilities), ExpectedInvariant: scenario.ExpectedInvariant},
		clock:    NewClock(scenario.ClockStart), streams: make(map[string]*Random), faults: slices.Clone(faults),
	}
	for _, actor := range scenario.Actors {
		stream, err := NewActorRandom(scenario.Seed, actor.Name, actor.Identity)
		if err != nil {
			return nil, err
		}
		scheduler.streams[actor.Name] = stream
		scheduler.scenario.Actors = append(scheduler.scenario.Actors, actor.Name)
	}
	return scheduler, nil
}

// Enqueue copies one event and allocates its stable insertion ordinal.
func (s *Scheduler) Enqueue(event Event) (uint64, error) {
	if s.ran {
		return 0, ErrSchedulerUsed
	}
	if len(s.events) >= MaxSteps {
		return 0, ErrStepBudget
	}
	if _, found := s.streams[event.Actor]; !found {
		return 0, ErrInvalidEvent
	}
	if !validNode(event.Node) || !validSymbol(event.Channel) || !validSymbol(event.Operation) || !slices.Contains(Checkpoints(), event.Checkpoint) || !validSymbol(event.Barrier) || event.At < 0 || len(event.Alternatives) == 0 {
		return 0, ErrInvalidEvent
	}
	for index, alternative := range event.Alternatives {
		if !validSymbol(alternative) || slices.Contains(event.Alternatives[:index], alternative) {
			return 0, ErrInvalidEvent
		}
	}
	ordinal := uint64(len(s.events) + 1)
	for index, prerequisite := range event.Requires {
		if prerequisite == 0 || prerequisite >= ordinal || slices.Contains(event.Requires[:index], prerequisite) {
			return 0, ErrInvalidEvent
		}
	}
	event.Alternatives = slices.Clone(event.Alternatives)
	event.Requires = slices.Clone(event.Requires)
	s.events = append(s.events, ScheduledEvent{Ordinal: ordinal, Event: event})
	return ordinal, nil
}

// Run executes ready events by time, named barrier, and ordinal on one owner.
// Empty work is NOT_RUN; an unexercised fault or unasserted observable fails.
func (s *Scheduler) Run(observer Observer) (Trace, error) {
	if s.ran {
		return Trace{}, ErrSchedulerUsed
	}
	if observer == nil {
		return Trace{}, ErrMissingObserver
	}
	s.ran = true
	trace := Trace{Version: TraceVersion, Scenario: cloneScenarioRecord(s.scenario), FaultPlan: slices.Clone(s.faults), Events: cloneEvents(s.events), Outcome: NotRun}
	pending := cloneEvents(s.events)
	if len(pending) > 0 {
		trace.Outcome = ModelPass
	}
	reached := make(map[Fault]bool)
	completed := make(map[uint64]bool)
	for len(pending) > 0 {
		index := nextEvent(pending, completed)
		if index < 0 {
			trace.Outcome = Fail
			return trace, ErrInvalidEvent
		}
		selected := pending[index]
		pending = append(pending[:index], pending[index+1:]...)
		at := s.scenario.ClockStart.Add(selected.Event.At)
		if at.After(s.clock.Now()) {
			if err := s.clock.Advance(at.Sub(s.clock.Now())); err != nil {
				return trace, err
			}
		}
		if err := s.clock.Advance(selected.Event.Advance); err != nil && !errors.Is(err, ErrClockRollback) {
			return trace, err
		}
		draw := s.streams[selected.Event.Actor].Uint64()
		step := Step{ScheduledEvent: selected, Now: s.clock.Now(), ClockUncertain: s.clock.Uncertain(), Draw: draw, Choice: selected.Event.Alternatives[draw%uint64(len(selected.Event.Alternatives))], Faults: s.faults.matching(selected.Event)}
		for _, fault := range step.Faults {
			reached[fault] = true
		}
		observation, err := observer(cloneStep(step))
		observation.Assertions = slices.Clone(observation.Assertions)
		if err != nil {
			trace.Outcome = Fail
			return trace, errors.New("sync harness: model observer failed")
		}
		trace.Steps = append(trace.Steps, TraceStep{Step: step, Observation: observation})
		if !validObservation(s.scenario.ExpectedInvariant, step.Faults, observation) {
			trace.Outcome = Fail
			return trace, ErrObservation
		}
		if observation.Outcome == Fail {
			trace.Outcome = Fail
		} else if trace.Outcome != Fail && observation.Outcome != ModelPass {
			trace.Outcome = ScaffoldOnly
		}
		completed[selected.Ordinal] = true
	}
	for _, fault := range s.faults {
		if !reached[fault] {
			trace.Outcome = Fail
			return trace, ErrFaultNotReached
		}
	}
	return trace, nil
}

func cloneEvent(event Event) Event {
	event.Alternatives = slices.Clone(event.Alternatives)
	event.Requires = slices.Clone(event.Requires)
	return event
}

func cloneEvents(events []ScheduledEvent) []ScheduledEvent {
	result := slices.Clone(events)
	for index := range result {
		result[index].Event = cloneEvent(result[index].Event)
	}
	return result
}

func cloneStep(step Step) Step {
	step.Event = cloneEvent(step.Event)
	step.Faults = slices.Clone(step.Faults)
	return step
}

func cloneScenarioRecord(record ScenarioRecord) ScenarioRecord {
	record.Inputs = slices.Clone(record.Inputs)
	record.Actors = slices.Clone(record.Actors)
	record.Capabilities = slices.Clone(record.Capabilities)
	return record
}

func validObservation(invariant string, faults []Fault, observation Observation) bool {
	if observation.Invariant != invariant || (observation.Outcome != ModelPass && observation.Outcome != ScaffoldOnly && observation.Outcome != NotRun && observation.Outcome != Fail) || len(observation.Assertions) != len(faults) {
		return false
	}
	for index, fault := range faults {
		if observation.Assertions[index] != fault.Observable {
			return false
		}
	}
	return true
}

func nextEvent(events []ScheduledEvent, completed map[uint64]bool) int {
	best := -1
	for index, event := range events {
		ready := true
		for _, prerequisite := range event.Event.Requires {
			if !completed[prerequisite] {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		if best < 0 || eventLess(event, events[best]) {
			best = index
		}
	}
	return best
}

func eventLess(a, b ScheduledEvent) bool {
	if a.Event.At != b.Event.At {
		return a.Event.At < b.Event.At
	}
	if a.Event.Barrier != b.Event.Barrier {
		return a.Event.Barrier < b.Event.Barrier
	}
	return a.Ordinal < b.Ordinal
}

// Replay executes the recorded symbolic schedule with independently supplied
// scenario/identity inputs and fault plan, then compares the complete record.
// Changed events, choices, versions, identities, faults, or outcomes fail closed.
// It performs no minimization and never silently substitutes a new random run.
func Replay(scenario Scenario, faults FaultPlan, observer Observer, recorded Trace) (Trace, error) {
	if recorded.Version != TraceVersion || len(recorded.Events) > MaxSteps {
		return Trace{}, ErrReplayMismatch
	}
	scheduler, err := NewScheduler(scenario, faults)
	if err != nil {
		return Trace{}, err
	}
	for _, event := range recorded.Events {
		ordinal, err := scheduler.Enqueue(event.Event)
		if err != nil || ordinal != event.Ordinal {
			return Trace{}, ErrReplayMismatch
		}
	}
	trace, err := scheduler.Run(observer)
	if err != nil {
		return trace, err
	}
	actual, err := trace.Bytes()
	if err != nil {
		return trace, err
	}
	expected, err := recorded.Bytes()
	if err != nil {
		return trace, err
	}
	if !bytes.Equal(actual, expected) {
		return trace, ErrReplayMismatch
	}
	return trace, nil
}
