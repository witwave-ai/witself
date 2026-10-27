package syncharness

import (
	"slices"
	"time"
)

// ScheduleBarrier is an explicit event dependency, not a timing delay.
type ScheduleBarrier struct {
	Role, Action string
	After        []uint64
}

// CoverageSchedule registers each SC-5 expansion with required runtime evidence.
// The miniature model checks interleavings; it does not supply database roles,
// production guards, mixed binaries, or native restore adapters.
type CoverageSchedule struct {
	ID, Name string
	Required []string
	Barriers []ScheduleBarrier
}

func schedule(id, name string, required []string, pairs ...string) CoverageSchedule {
	s := CoverageSchedule{ID: id, Name: name, Required: required}
	for i := 0; i < len(pairs); i += 2 {
		b := ScheduleBarrier{Role: pairs[i], Action: pairs[i+1]}
		if i > 0 {
			b.After = []uint64{uint64(i / 2)}
		}
		s.Barriers = append(s.Barriers, b)
	}
	return s
}

// CoverageSchedules returns the ten named schedules, each with controlled role
// barriers. These are executable miniature schedules, not runtime test passes.
func CoverageSchedules() []CoverageSchedule {
	return []CoverageSchedule{
		schedule("H35", "GuardInstallRelationDrain", []string{"production_relation_guards", "prepared_transactions"}, "U", "writer_hold", "I", "install_wait", "U", "writer_commit", "I", "guard_install"),
		schedule("H36", "PreBeginLateFirstWrite", []string{"mixed_executable_versions", "production_writer_admission"}, "V", "old_begin", "A", "guard_install", "V", "legacy_refuse", "V", "fresh_admit"),
		schedule("H37", "OldSnapshotLockingAdmission", []string{"production_locking_admission"}, "V", "old_snapshot", "A", "guard_install", "V", "stale_refuse", "V", "fresh_admit"),
		schedule("H38", "AccountGateDrain", []string{"production_account_gate"}, "T", "writer_hold", "A", "gate_wait", "T", "writer_commit", "A", "gate_acquire"),
		schedule("H39", "SnapshotPinSameSessionAck", []string{"same_physical_session_snapshot"}, "A", "gate_acquire", "S", "snapshot_pin", "A", "gate_death", "A", "gate_reconnect", "S", "ack_refuse"),
		schedule("H40", "IncompleteSnapshotRestart", []string{"native_restore", "cross_store_snapshot"}, "A", "gate_acquire", "S", "snapshot_pin", "S", "snapshot_partial", "S", "snapshot_death", "S", "publish_refuse", "A", "fresh_boundary", "S", "snapshot_complete"),
		schedule("H41", "OmittedSemanticJournal", []string{"complete_production_writer_inventory"}, "T", "fresh_admit", "T", "domain_mutate", "T", "journal_partial", "T", "mutation_abort", "T", "complete_commit"),
		schedule("H42", "CommittedPrefixCounter", []string{"production_committed_prefix"}, "T", "counter_hold", "U", "counter_wait", "T", "counter_rollback", "U", "counter_finalize", "V", "prefix_consume"),
		schedule("H43", "WriterGuardBypassAndDowngrade", []string{"real_database_roles", "mixed_executable_versions", "production_guard_bypass_proof"}, "V", "old_snapshot", "A", "guard_install", "V", "bypass_refuse", "A", "capability_revoke", "V", "revoked_snapshot_refuse"),
		schedule("H44", "ActivationCrashAndReplayAccounting", []string{"native_restore", "cross_store_apply", "production_logical_accounting"}, "A", "gate_acquire", "S", "snapshot_pin", "A", "gate_death", "S", "ack_refuse", "A", "fresh_boundary", "S", "snapshot_complete", "T", "replay_no_action"),
	}
}

// RuntimeScheduleAdapter returns the evidence actually executed. A fixture
// adapter cannot upgrade its experiment by claiming production capabilities.
type RuntimeScheduleAdapter interface {
	RunCoverageSchedule(CoverageSchedule) (RuntimeResultRecord, error)
}

// CoverageScheduleResult preserves missing capabilities alongside the class of
// an executed observation. PASS with fixture evidence is never a runtime H pass.
type CoverageScheduleResult struct {
	Obligation    string
	Status        EvidenceStatus
	EvidenceClass EvidenceClass
	Missing       []string
	Record        *RuntimeResultRecord
}

// Passed reports an executed observation within its explicit evidence class.
func (r CoverageScheduleResult) Passed() bool {
	return r.Status == Pass && r.Record != nil && r.Record.Outcome == Pass && r.EvidenceClass == r.Record.EvidenceClass && validEvidenceClass(r.EvidenceClass)
}

// RunCoverageSchedule requires the registered schedule, every runtime capability
// and an executing adapter. Missing capabilities return NOT_RUN explicitly.
func RunCoverageSchedule(s CoverageSchedule, available []string, adapter RuntimeScheduleAdapter) CoverageScheduleResult {
	result := CoverageScheduleResult{Obligation: s.ID, Status: NotRun}
	if !registeredCoverageSchedule(s) {
		result.Status = Fail
		return result
	}
	for _, cap := range s.Required {
		if !slices.Contains(available, cap) {
			result.Missing = append(result.Missing, cap)
		}
	}
	if adapter == nil {
		result.Missing = append(result.Missing, "runtime_schedule_adapter")
	}
	slices.Sort(result.Missing)
	if len(result.Missing) > 0 {
		return result
	}
	// A caller-supplied implementation could copy a retained PASS without doing
	// any work. Only reviewed concrete execution bindings may supply evidence.
	// Later slices must register their actual adapters here as well as in the
	// versioned result validator and package-local compiled binding tests.
	switch a := adapter.(type) {
	case FixtureCoverageAdapter:
	case *FixtureCoverageAdapter:
		if a == nil {
			result.Status = Fail
			return result
		}
	default:
		result.Status = Fail
		return result
	}
	record, err := adapter.RunCoverageSchedule(s)
	if err != nil || record.Obligation != s.ID || record.Scenario != s.Name || VerifyRuntimeArtifact(record, len(s.Barriers)) != nil {
		result.Status = Fail
	} else {
		result.Status = record.Outcome
		result.EvidenceClass = record.EvidenceClass
		result.Record = &record
		if record.EvidenceClass == FixtureEvidence {
			// Executing the barrier model does not discharge production gaps,
			// even if the caller supplied every capability name above.
			result.Missing = slices.Clone(s.Required)
			slices.Sort(result.Missing)
		}
	}
	return result
}

func registeredCoverageSchedule(s CoverageSchedule) bool {
	return slices.ContainsFunc(CoverageSchedules(), func(candidate CoverageSchedule) bool {
		return candidate.ID == s.ID && candidate.Name == s.Name && slices.Equal(candidate.Required, s.Required) && slices.EqualFunc(candidate.Barriers, s.Barriers, func(a, b ScheduleBarrier) bool {
			return a.Role == b.Role && a.Action == b.Action && slices.Equal(a.After, b.After)
		})
	})
}

// FixtureCoverageAdapter executes only the model barriers. The evidence class,
// executable and retained artifact are fixed by this adapter, not its caller.
type FixtureCoverageAdapter struct{}

// RunCoverageSchedule executes exactly the reviewed model barrier sequence.
func (FixtureCoverageAdapter) RunCoverageSchedule(s CoverageSchedule) (RuntimeResultRecord, error) {
	if !registeredCoverageSchedule(s) {
		return RuntimeResultRecord{}, ErrRuntimeResult
	}
	trace, err := RunCoverageScheduleModel(s)
	if err != nil || trace.Outcome != ModelPass || len(trace.Steps) != len(s.Barriers) {
		return RuntimeResultRecord{}, ErrRuntimeResult
	}
	record, checks, ok := fixtureRuntimeRecord(s.ID)
	if !ok || record.Scenario != s.Name || checks != len(trace.Steps) {
		return RuntimeResultRecord{}, ErrRuntimeResult
	}
	return record, nil
}

// CompileCoverageSchedule uses the existing causal scheduler, with all six SC-5
// session roles registered independently of which pair a particular case uses.
func CompileCoverageSchedule(s CoverageSchedule) (*Scheduler, error) {
	scenario := Scenario{Version: 1, ID: s.Name, Seed: 1, ClockStart: time.Unix(1, 0).UTC(), ExpectedInvariant: "coverage_boundary", Inputs: []string{s.ID}}
	for _, role := range []string{"U", "I", "V", "T", "A", "S"} {
		scenario.Actors = append(scenario.Actors, Actor{Name: role, Identity: []byte(role)})
	}
	scheduler, err := NewScheduler(scenario, nil)
	if err != nil {
		return nil, err
	}
	for _, barrier := range s.Barriers {
		_, err = scheduler.Enqueue(Event{Actor: barrier.Role, Node: R, Channel: "coverage", Operation: barrier.Action, Checkpoint: BeforeAdmission, Barrier: barrier.Action, Alternatives: []string{"execute"}, Requires: barrier.After})
		if err != nil {
			return nil, err
		}
	}
	return scheduler, nil
}

type coverageState struct {
	writer, guard, old, admitted, gate, pin, partial, dead, mutated, journal, counter, revoked bool
	session, pinSession, pending, actions, finalized, consumed                                 int
}

func (m *coverageState) apply(action string) bool {
	switch action {
	case "writer_hold":
		if m.writer || m.gate {
			return false
		}
		m.writer = true
	case "install_wait", "gate_wait":
		return m.writer && !m.guard && !m.gate
	case "writer_commit":
		if !m.writer {
			return false
		}
		m.writer = false
	case "guard_install":
		if m.writer {
			return false
		}
		m.guard = true
	case "old_begin", "old_snapshot":
		m.old = true
	case "legacy_refuse", "bypass_refuse":
		return m.old && m.guard && !m.admitted && !m.mutated
	case "stale_refuse":
		return m.old && m.guard && !m.mutated
	case "revoked_snapshot_refuse":
		return m.old && m.guard && m.revoked && !m.mutated
	case "fresh_admit":
		m.old = false
		m.admitted = true
	case "gate_acquire":
		if m.writer || m.gate {
			return false
		}
		m.gate = true
		m.session++
	case "snapshot_pin":
		if !m.gate {
			return false
		}
		m.pin = true
		m.pinSession = m.session
		m.pending = 1
	case "gate_death":
		if !m.gate {
			return false
		}
		m.gate = false
		m.dead = true
	case "gate_reconnect":
		m.session++
	case "ack_refuse":
		return m.pin && (!m.gate || m.pinSession != m.session)
	case "snapshot_partial":
		if !m.pin {
			return false
		}
		m.partial = true
	case "snapshot_death":
		m.dead = true
	case "publish_refuse":
		return m.partial && m.dead && m.pending == 1
	case "fresh_boundary":
		if !m.dead {
			return false
		}
		m.session++
		m.pinSession = m.session
		m.gate = true
		m.pin = true
		m.partial = false
		m.dead = false
	case "snapshot_complete":
		return m.pin && m.gate && !m.partial && !m.dead && m.session == m.pinSession && m.pending == 1
	case "domain_mutate":
		if !m.admitted {
			return false
		}
		m.mutated = true
	case "journal_partial":
		if !m.mutated {
			return false
		}
		m.journal = false
	case "mutation_abort":
		if !m.mutated || m.journal {
			return false
		}
		m.mutated = false
		m.admitted = false
	case "complete_commit":
		if m.mutated {
			return false
		}
		m.actions++
		m.journal = true
	case "counter_hold":
		m.counter = true
	case "counter_wait":
		return m.counter && m.finalized == 0
	case "counter_rollback":
		if !m.counter {
			return false
		}
		m.counter = false
	case "counter_finalize":
		if m.counter {
			return false
		}
		m.finalized++
	case "prefix_consume":
		if m.counter || m.finalized != 1 {
			return false
		}
		m.consumed = m.finalized
	case "capability_revoke":
		m.revoked = true
	case "replay_no_action":
		return m.actions == 0 && m.pending == 1
	default:
		return false
	}
	return true
}

// RunCoverageScheduleModel executes every barrier with a small guard model. Its
// MODEL_PASS is recorded separately from RunCoverageSchedule's runtime NOT_RUN.
func RunCoverageScheduleModel(s CoverageSchedule) (Trace, error) {
	scheduler, err := CompileCoverageSchedule(s)
	if err != nil {
		return Trace{}, err
	}
	state := coverageState{}
	return scheduler.Run(func(step Step) (Observation, error) {
		outcome := ModelPass
		if !state.apply(step.Event.Operation) {
			outcome = Fail
		}
		return Observation{Invariant: "coverage_boundary", Outcome: outcome}, nil
	})
}
