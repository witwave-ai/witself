package syncharness

import (
	"errors"
	"reflect"
	"slices"
)

// Reduction describes a locally minimal synthetic reproducer and its evidence.
// Attempts includes exact initial replay and the final fresh-topology replay.
type Reduction struct {
	Schedule RecordedSchedule
	Attempts int
}

// MaxReductionAttempts bounds work without accepting an unfinished reduction.
const MaxReductionAttempts = 2048

// ErrReductionBudget reports an explicit resource limit, never a reproducer.
var ErrReductionBudget = errors.New("sync harness: reduction attempt budget exceeded")

// Minimize first replays every recorded event and compares every observation.
// Only then does it delta-debug causally closed event sets, actors, symbolic
// payload shape and fault choices. Each attempt and final verification gets a
// fresh adapter; an error, different invariant or timeout cannot qualify.
func Minimize(trace Trace, payloads []PayloadShape, factory ReplayFactory) (Reduction, error) {
	var out Reduction
	if trace.Outcome != Fail {
		return out, ErrNotReproducer
	}
	s, err := RecordTrace(trace, payloads)
	if err != nil {
		return out, err
	}
	initial, err := ReplayRecorded(s, factory)
	out.Attempts++
	if err != nil || initial.Outcome != Fail || initial.Invariant != s.Scenario.ExpectedInvariant || len(initial.Observations) != len(trace.Steps) {
		return out, ErrNotReproducer
	}
	for i := range trace.Steps {
		if !reflect.DeepEqual(initial.Observations[i], trace.Steps[i].Observation) {
			return out, ErrReplayMismatch
		}
	}
	accept := func(candidate RecordedSchedule) (bool, error) {
		if candidate.Validate() != nil {
			return false, nil
		}
		if out.Attempts >= MaxReductionAttempts {
			return false, ErrReductionBudget
		}
		out.Attempts++
		result, replayErr := ReplayRecorded(candidate, factory)
		return replayErr == nil && result.Outcome == Fail && result.Invariant == initial.Invariant, nil
	}
	for {
		previous := cloneRecorded(s)
		// Chunk removal closes over descendants, never drops prerequisites from a
		// retained event, and leaves original ordinals unchanged for review.
		for granularity := 2; len(s.Steps) > 1; {
			width := (len(s.Steps) + granularity - 1) / granularity
			reduced := false
			for start := 0; start < len(s.Steps); start += width {
				remove := make(map[uint64]bool)
				for _, step := range s.Steps[start:min(start+width, len(s.Steps))] {
					remove[step.Ordinal] = true
				}
				candidate := removeRecordedEvents(s, remove)
				ok, err := accept(candidate)
				if err != nil {
					return out, err
				}
				if ok {
					s, reduced = candidate, true
					break
				}
			}
			if reduced {
				granularity = max(2, granularity-1)
				continue
			}
			if granularity >= len(s.Steps) {
				break
			}
			granularity = min(len(s.Steps), granularity*2)
		}
		// Remove actors with all their dependent events; unused actors disappear too.
		for i := 0; i < len(s.Scenario.Actors); {
			actor := s.Scenario.Actors[i]
			remove := make(map[uint64]bool)
			for _, step := range s.Steps {
				if step.Event.Actor == actor {
					remove[step.Ordinal] = true
				}
			}
			candidate := removeRecordedEvents(s, remove)
			candidate.Scenario.Actors = slices.Delete(candidate.Scenario.Actors, i, i+1)
			ok, err := accept(candidate)
			if err != nil {
				return out, err
			}
			if ok {
				s = candidate
			} else {
				i++
			}
		}
		for i := 0; i < len(s.Scenario.Inputs); {
			candidate := cloneRecorded(s)
			input := candidate.Scenario.Inputs[i]
			candidate.Scenario.Inputs = slices.Delete(candidate.Scenario.Inputs, i, i+1)
			candidate.Payloads = slices.DeleteFunc(candidate.Payloads, func(p PayloadShape) bool { return p.Input == input })
			ok, err := accept(candidate)
			if err != nil {
				return out, err
			}
			if ok {
				s = candidate
			} else {
				i++
			}
		}
		for i := range s.Payloads {
			for j := 0; j < len(s.Payloads[i].Fields); {
				candidate := cloneRecorded(s)
				candidate.Payloads[i].Fields = slices.Delete(candidate.Payloads[i].Fields, j, j+1)
				ok, err := accept(candidate)
				if err != nil {
					return out, err
				}
				if ok {
					s = candidate
				} else {
					j++
				}
			}
			// Bounded cardinalities permit exact one-element minimality.
			for s.Payloads[i].Elements > 0 {
				candidate := cloneRecorded(s)
				candidate.Payloads[i].Elements--
				ok, err := accept(candidate)
				if err != nil {
					return out, err
				}
				if !ok {
					break
				}
				s = candidate
			}
		}
		for i := range s.Steps {
			for j := 0; j < len(s.Steps[i].Faults); {
				candidate := cloneRecorded(s)
				candidate.Steps[i].Faults = slices.Delete(candidate.Steps[i].Faults, j, j+1)
				ok, err := accept(candidate)
				if err != nil {
					return out, err
				}
				if ok {
					s = candidate
				} else {
					j++
				}
			}
			candidate := cloneRecorded(s)
			candidate.Steps[i].Event.Alternatives = []string{candidate.Steps[i].Choice}
			ok, err := accept(candidate)
			if err != nil {
				return out, err
			}
			if ok {
				s = candidate
			}
		}
		if reflect.DeepEqual(previous, s) {
			break
		}
	}
	// Prove the final artifact afresh, not from a retained/mutated adapter.
	ok, err := accept(s)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, ErrNotReproducer
	}
	out.Schedule = s
	return out, nil
}

func removeRecordedEvents(s RecordedSchedule, remove map[uint64]bool) RecordedSchedule {
	out := cloneRecorded(s)
	out.Steps = nil
	for _, step := range s.Steps {
		for _, prerequisite := range step.Event.Requires {
			if remove[prerequisite] {
				remove[step.Ordinal] = true
			}
		}
		if !remove[step.Ordinal] {
			out.Steps = append(out.Steps, cloneStep(step))
		}
	}
	return out
}
