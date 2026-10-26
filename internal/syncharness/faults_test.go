package syncharness

import (
	"errors"
	"testing"
)

func TestAllFaultClassesRequireObservableAssertion(t *testing.T) {
	expectedContracts := map[FaultClass]string{
		Partition:                  "Independent local work proceeds; remote ownership and transfer remain pending",
		DuplicateReorderedDelivery: "Stable operation/fragment identity; no duplicate logical action or partial graph",
		CrashBetweenWrites:         "Before commit: none of the atomic group; after commit: complete group and original outcome",
		DelayedLostACK:             "Retry original operation; no fresh ID or repeat external effect",
		ExpiredHistory:             "Unresolved work remains pinned; missing acceptance/deletion/frontier evidence blocks convergence",
		IncompatibleVersions:       "Explicit refusal or pending compatibility recovery; no implicit acceptance",
		WitnessRestore:             "Distinct publication-pending, proven-gap, evidence-conflict and continuity-unproven results",
	}
	classes := FaultClasses()
	if len(classes) != 7 {
		t.Fatal("fault class inventory incomplete")
	}
	for _, class := range classes {
		t.Run(string(class), func(t *testing.T) {
			contract, ok := FaultContract(class)
			if !ok || contract == "" || contract != expectedContracts[class] {
				t.Fatal("fault lacks observable contract")
			}
			plan := FaultPlan{{Class: class, Node: L1, Channel: "model", Operation: "sentinel", Checkpoint: DomainWrite, Observable: contract}}
			trace := runModel(t, modelScenario(1), plan, []Event{modelEvent("barrier")}, modelObserver)
			if len(trace.Steps[0].Faults) != 1 || trace.Steps[0].Observation.Assertions[0] != contract {
				t.Fatal("selected contract not asserted")
			}
			if _, err := Replay(modelScenario(1), plan, modelObserver, trace); err != nil {
				t.Fatal("fault selection did not replay")
			}
			scheduler, err := NewScheduler(modelScenario(1), plan)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := scheduler.Enqueue(modelEvent("barrier")); err != nil {
				t.Fatal(err)
			}
			missing := func(Step) (Observation, error) {
				return Observation{Invariant: "schedule-replays", Outcome: ModelPass}, nil
			}
			if got, err := scheduler.Run(missing); !errors.Is(err, ErrObservation) || got.Outcome != Fail {
				t.Fatal("unasserted fault passed")
			}
		})
	}
}

func TestFaultSelectorRequiresEveryCoordinate(t *testing.T) {
	contract, _ := FaultContract(Partition)
	original := Fault{Class: Partition, Node: L1, Channel: "model", Operation: "sentinel", Checkpoint: DomainWrite, Observable: contract}
	mutations := []struct {
		name   string
		mutate func(*Fault)
	}{
		{"node", func(fault *Fault) { fault.Node = R }},
		{"channel", func(fault *Fault) { fault.Channel = "other" }},
		{"operation", func(fault *Fault) { fault.Operation = "other" }},
		{"checkpoint", func(fault *Fault) { fault.Checkpoint = AfterAdmission }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			fault := original
			mutation.mutate(&fault)
			scheduler, err := NewScheduler(modelScenario(1), FaultPlan{fault})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := scheduler.Enqueue(modelEvent("barrier")); err != nil {
				t.Fatal(err)
			}
			trace, err := scheduler.Run(modelObserver)
			if !errors.Is(err, ErrFaultNotReached) || trace.Outcome != Fail || len(trace.Steps[0].Faults) != 0 {
				t.Fatal("nonmatching fault silently passed or matched")
			}
		})
	}
	changed := original
	changed.Observable = "invented"
	if err := (FaultPlan{changed}).Validate(); !errors.Is(err, ErrInvalidFault) {
		t.Fatal("wrong observable accepted")
	}
	if err := (FaultPlan{original, original}).Validate(); !errors.Is(err, ErrInvalidFault) {
		t.Fatal("duplicate fault accepted")
	}
}

func TestCheckpointInventory(t *testing.T) {
	expected := []Checkpoint{BeforeAdmission, AfterAdmission, DomainWrite, JournalSeal, CaptureCommit, FragmentPersistence, InboundMutationCommit, ReceiptCommit, PublicationOutbox, WitnessCAS, ReturnedACK, SourceTerminalPersistence, GraphFence, GraphInstall, GraphPublish, GraphAbort, SnapshotGate, SnapshotPin, SnapshotAcknowledgment}
	seen := make(map[Checkpoint]bool)
	for _, checkpoint := range Checkpoints() {
		if seen[checkpoint] {
			t.Fatal("duplicate checkpoint")
		}
		seen[checkpoint] = true
	}
	for _, checkpoint := range expected {
		if !seen[checkpoint] {
			t.Fatal("design checkpoint absent")
		}
	}
}
