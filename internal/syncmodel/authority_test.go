package syncmodel

import (
	"reflect"
	"testing"
)

func authorityFixture(t *testing.T) (*AuthorityTransfer, TransferBinding) {
	t.Helper()
	predecessor := AuthorityIdentity{Incarnation: fixtureIncarnationHistory().Current, RequestID: "request", InstallationID: "predecessor", Epoch: 4}
	successor := predecessor
	successor.InstallationID, successor.Epoch = "successor", 5
	work := []TransferredExecution{
		{Action: OperationID{1}, Disposition: ExecutionCompleted, LeaseThrough: 11, OutcomeRef: "original-completion"},
		{Action: OperationID{2}, Disposition: ExecutionReserved, LeaseThrough: 12},
		{Action: OperationID{3}, Disposition: ExecutionAmbiguousStarted, LeaseThrough: 13},
	}
	a, err := NewAuthorityTransfer(predecessor, 7, true, work)
	if err != nil {
		t.Fatal(err)
	}
	return a, TransferBinding{ID: TransferID{1}, Predecessor: predecessor, Successor: successor}
}

func fenceAuthority(t *testing.T, a *AuthorityTransfer, binding TransferBinding) TransferFenceProof {
	t.Helper()
	if err := a.BeginFreeze(binding); err != nil {
		t.Fatal(err)
	}
	proof, err := a.CommitFence(9, Commitment{1})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.PinFence(proof); err != nil {
		t.Fatal(err)
	}
	return proof
}

func installAuthority(t *testing.T, a *AuthorityTransfer, proof TransferFenceProof) {
	t.Helper()
	if err := a.Stage(proof, true); err != nil {
		t.Fatal(err)
	}
	if err := a.Install(); err != nil {
		t.Fatal(err)
	}
	if err := a.PinInstall(); err != nil {
		t.Fatal(err)
	}
}

func TestGraphFreezeMutationMatrix(t *testing.T) {
	for _, kind := range GraphMutationKinds() {
		for _, commit := range []bool{true, false} {
			name := string(kind) + "/refuse"
			if commit {
				name = string(kind) + "/drain"
			}
			t.Run(name, func(t *testing.T) {
				a, binding := authorityFixture(t)
				if err := a.BeginMutation("before", kind, binding.Predecessor, 7); err != nil {
					t.Fatal(err)
				}
				if err := a.BeginFreeze(binding); err != nil {
					t.Fatal(err)
				}
				if _, err := a.CommitFence(9, Commitment{1}); err == nil {
					t.Fatal("freeze committed before admitted writer drained")
				}
				if err := a.BeginMutation("after", kind, binding.Predecessor, 7); err == nil {
					t.Fatal("new writer passed freeze")
				}
				if err := a.FinishMutation("before", commit); err != nil {
					t.Fatal(err)
				}
				proof, err := a.CommitFence(9, Commitment{1})
				want := GraphRevision(7)
				if commit {
					want++
				}
				if err != nil || proof.FinalRevision != want || !reflect.DeepEqual(a.Status().Outbox, []string{"fence"}) {
					t.Fatal("final revision or atomic transfer outbox lost drained effect")
				}
				if err := a.BeginMutation("stale", kind, binding.Predecessor, want); err == nil {
					t.Fatal("old epoch passed permanent fence")
				}
				if err := a.FinishMutation("before", true); err == nil {
					t.Fatal("drained transaction committed a later unrecorded effect")
				}
			})
		}
	}
}

func TestAuthorityTransferImmutableBindingAndProof(t *testing.T) {
	a, binding := authorityFixture(t)
	proof := fenceAuthority(t, a, binding)
	for _, mutate := range []func(*TransferBinding){
		func(b *TransferBinding) { b.ID = TransferID{2} },
		func(b *TransferBinding) { b.Successor.InstallationID = "other-target" },
		func(b *TransferBinding) { b.Successor.Epoch++ },
		func(b *TransferBinding) { b.Successor.Incarnation.Restore = RestoreMarker{9} },
	} {
		changed := binding
		mutate(&changed)
		if a.BeginFreeze(changed) == nil {
			t.Fatal("competing target or altered transfer reuse accepted")
		}
	}
	for _, mutate := range []func(*TransferFenceProof){
		func(p *TransferFenceProof) { p.Version++ },
		func(p *TransferFenceProof) { p.FinalRevision++ },
		func(p *TransferFenceProof) { p.JournalThrough++ },
		func(p *TransferFenceProof) { p.Manifest = Commitment{2} },
		func(p *TransferFenceProof) { p.Binding.Successor.Incarnation.AccountID = "foreign" },
		func(p *TransferFenceProof) { p.Binding.Successor.Incarnation.Restore = RestoreMarker{9} },
		func(p *TransferFenceProof) { p.Binding.Successor.InstallationID = "foreign" },
		func(p *TransferFenceProof) { p.Binding.Successor.RequestID = "foreign" },
	} {
		changed := proof
		mutate(&changed)
		if a.PinFence(changed) == nil || a.Stage(changed, true) == nil {
			t.Fatal("altered fence proof authorized successor")
		}
	}
	if a.Stage(proof, false) == nil || a.Install() == nil || a.Publish() == nil {
		t.Fatal("incomplete staged transfer installed or published")
	}
	if !reflect.DeepEqual(a.Status().Outbox, []string{"fence"}) {
		t.Fatal("invalid proof changed durable checkpoint")
	}
}

func TestAuthorityTransferCommitPinCrashMatrix(t *testing.T) {
	for _, crash := range []string{"intent", "fence", "fence_pin", "stage", "install", "install_pin", "publication", "publication_pin"} {
		t.Run(crash, func(t *testing.T) {
			a, binding := authorityFixture(t)
			originalWork := a.Work()
			var proof TransferFenceProof
			steps := []struct {
				name string
				run  func() error
			}{
				{"intent", func() error { return a.BeginFreeze(binding) }},
				{"fence", func() error { var err error; proof, err = a.CommitFence(9, Commitment{1}); return err }},
				{"fence_pin", func() error { return a.PinFence(proof) }},
				{"stage", func() error { return a.Stage(proof, true) }},
				{"install", func() error { return a.Install() }},
				{"install_pin", func() error { return a.PinInstall() }},
				{"publication", func() error { return a.Publish() }},
				{"publication_pin", func() error { return a.PinPublication() }},
			}
			for _, step := range steps {
				if err := step.run(); err != nil {
					t.Fatal(err)
				}
				if step.name == crash {
					before := a.Status()
					a = a.Restart()
					if err := step.run(); err != nil || !reflect.DeepEqual(before, a.Status()) {
						t.Fatal("checkpoint restart/replay changed durable transition")
					}
				}
				if a.CanMutate(binding.Predecessor) || a.CanMutate(binding.Successor) != (step.name == "publication_pin") {
					t.Fatal("unpublished or stale graph acquired execution authority")
				}
				if !reflect.DeepEqual(a.Work(), originalWork) {
					t.Fatal("transfer rewrote historical execution")
				}
			}
			if a.BeginMutation("successor-write", GraphMaintenance, binding.Successor, 7) != nil || a.FinishMutation("successor-write", true) != nil {
				t.Fatal("published successor could not commit under its new epoch")
			}
			if a.BeginMutation("retired-write", GraphMaintenance, binding.Predecessor, 8) == nil {
				t.Fatal("successor commit removed predecessor fence")
			}
			if !reflect.DeepEqual(a.Status().Outbox, []string{"fence", "install", "publication"}) {
				t.Fatal("lost ACK duplicated control outbox")
			}
		})
	}
}

func TestAuthorityControlPinsAreSeparateAndRestartRollsBackWriters(t *testing.T) {
	a, binding := authorityFixture(t)
	if err := a.BeginMutation("writer", GraphMaintenance, binding.Predecessor, 7); err != nil {
		t.Fatal(err)
	}
	if err := a.BeginFreeze(binding); err != nil {
		t.Fatal(err)
	}
	a = a.Restart()
	proof, err := a.CommitFence(9, Commitment{1})
	if err != nil || proof.FinalRevision != 7 {
		t.Fatal("restart retained uncommitted writer effect")
	}
	if a.Stage(proof, true) == nil {
		t.Fatal("unpin promise used as predecessor fence proof")
	}
	if err := a.PinFence(proof); err != nil {
		t.Fatal(err)
	}
	if err := a.Stage(proof, true); err != nil {
		t.Fatal(err)
	}
	if err := a.Install(); err != nil {
		t.Fatal(err)
	}
	if a.Publish() == nil {
		t.Fatal("unpinned installation published")
	}
	if err := a.PinInstall(); err != nil {
		t.Fatal(err)
	}
	if err := a.Publish(); err != nil {
		t.Fatal(err)
	}
	if a.CanMutate(binding.Successor) {
		t.Fatal("committed unpinned publication enabled execution")
	}
}

func TestTransferredExecutionDisposition(t *testing.T) {
	a, binding := authorityFixture(t)
	original := a.Work()
	proof := fenceAuthority(t, a, binding)
	installAuthority(t, a, proof)
	if err := a.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := a.PinPublication(); err != nil {
		t.Fatal(err)
	}
	a = a.Restart()
	if !reflect.DeepEqual(original, a.Work()) || len(a.Work()) != 3 {
		t.Fatal("transferred execution was flattened or became a fresh action")
	}
	for _, work := range a.Work() {
		if work.ReassignmentAllowed() != (work.Disposition == ExecutionReserved) {
			t.Fatal("completed or ambiguous execution reassigned")
		}
	}
	if a.ReservationExecutable(binding.Successor, OperationID{2}, 1) || a.ReservationExecutable(binding.Successor, OperationID{99}, 1) {
		t.Fatal("ambiguous execution permitted reassignment or a fresh action")
	}
	unambiguous, err := NewAuthorityTransfer(binding.Successor, 7, true, original[:2])
	if err != nil || !unambiguous.ReservationExecutable(binding.Successor, OperationID{2}, 12) || unambiguous.ReservationExecutable(binding.Successor, OperationID{2}, 13) || unambiguous.ReservationExecutable(binding.Successor, OperationID{1}, 1) {
		t.Fatal("reserved continuation changed action identity, completion or original lease bound")
	}
	// Neither inspection nor a caller's edited result may revive a completion
	// or extend a lease. Recovery uses the original action identity.
	copyOfWork := a.Work()
	copyOfWork[0].Disposition, copyOfWork[2].LeaseThrough = ExecutionReserved, 999
	if !reflect.DeepEqual(original, a.Work()) {
		t.Fatal("inspection changed execution or lease")
	}
}

func TestAbortPublicationRestoreRace(t *testing.T) {
	for _, publicationFirst := range []bool{false, true} {
		name := "abort_first"
		if publicationFirst {
			name = "publication_commit_first"
		}
		t.Run(name, func(t *testing.T) {
			a, binding := authorityFixture(t)
			proof := fenceAuthority(t, a, binding)
			installAuthority(t, a, proof)
			owner := binding.Predecessor
			owner.Epoch = 6
			if publicationFirst {
				if err := a.Publish(); err != nil {
					t.Fatal(err)
				}
				if a.Abort(owner) == nil {
					t.Fatal("abort won against committed publication awaiting pin")
				}
			} else {
				if err := a.Abort(owner); err != nil {
					t.Fatal(err)
				}
				if a.Publish() == nil || a.RecoverAborted(owner) == nil {
					t.Fatal("publication raced past abort or unpinned abort conferred authority")
				}
				if err := a.PinAbort(); err != nil {
					t.Fatal(err)
				}
				a = a.Restart()
				if a.RecoverAborted(binding.Predecessor) == nil || a.RecoverAborted(binding.Successor) == nil {
					t.Fatal("restored abandoned epoch became recovery authority")
				}
				if err := a.RecoverAborted(owner); err != nil || a.CanMutate(owner) {
					t.Fatal("pinned abort did not install its new owner invisibly")
				}
				if a.PublishRecovery() == nil {
					t.Fatal("unwitnessed recovery installation published")
				}
				if err := a.PinRecoveryInstall(); err != nil {
					t.Fatal(err)
				}
				if err := a.PublishRecovery(); err != nil || a.CanMutate(owner) {
					t.Fatal("unpinned recovery publication enabled execution")
				}
				if err := a.PinRecoveryPublication(); err != nil || !a.CanMutate(owner) {
					t.Fatal("newly pinned recovery epoch not activated")
				}
				if a.BeginMutation("recovery-write", GraphMaintenance, owner, 7) != nil || a.FinishMutation("recovery-write", true) != nil {
					t.Fatal("published recovery owner could not commit under its new epoch")
				}
			}
			if a.CanMutate(binding.Predecessor) || a.CanMutate(binding.Successor) {
				t.Fatal("retired epoch reactivated")
			}
		})
	}
}

func TestUnavailableTargetNeverTimesIntoAuthority(t *testing.T) {
	a, binding := authorityFixture(t)
	fenceAuthority(t, a, binding)
	before := a.Status()
	for range 100 {
		a = a.Restart()
		if a.Publish() == nil || a.PinPublication() == nil || a.CanMutate(binding.Predecessor) || a.CanMutate(binding.Successor) {
			t.Fatal("unavailable successor turned waiting or replay into authority")
		}
	}
	if !reflect.DeepEqual(before, a.Status()) {
		t.Fatal("pending transfer discarded work")
	}
	independent := binding.Predecessor
	independent.RequestID = "independent-local-graph"
	other, err := NewAuthorityTransfer(independent, 1, true, nil)
	if err != nil || other.BeginMutation("local", GraphClaim, independent, 1) != nil || other.FinishMutation("local", true) != nil {
		t.Fatal("remote outage blocked independent local authority")
	}
}

func TestAuthorityMutationScopeRequiresCurrentPinAndRevision(t *testing.T) {
	a, binding := authorityFixture(t)
	for _, mutate := range []func(*AuthorityIdentity){
		func(i *AuthorityIdentity) { i.Incarnation.AccountID = "foreign" },
		func(i *AuthorityIdentity) { i.Incarnation.Lifecycle++ },
		func(i *AuthorityIdentity) { i.Incarnation.Restore = RestoreMarker{2} },
		func(i *AuthorityIdentity) { i.RequestID = "foreign" },
		func(i *AuthorityIdentity) { i.InstallationID = "foreign" },
		func(i *AuthorityIdentity) { i.Epoch++ },
	} {
		identity := binding.Predecessor
		mutate(&identity)
		if a.BeginMutation("foreign", GraphClaim, identity, 7) == nil {
			t.Fatal("mutation accepted wrong graph authority scope")
		}
	}
	if a.BeginMutation("wrong-revision", GraphClaim, binding.Predecessor, 8) == nil || a.BeginMutation("unknown-kind", "unknown", binding.Predecessor, 7) == nil {
		t.Fatal("unknown writer or stale revision accepted")
	}
	unpinned, err := NewAuthorityTransfer(binding.Predecessor, 7, false, a.Work())
	if err != nil {
		t.Fatal(err)
	}
	if unpinned.BeginMutation("unpinned", GraphClaim, binding.Predecessor, 7) == nil || unpinned.BeginFreeze(binding) == nil {
		t.Fatal("unpublished authority mutated or produced a fence")
	}
	if err := a.BeginMutation("first", GraphMaintenance, binding.Predecessor, 7); err != nil {
		t.Fatal(err)
	}
	if err := a.BeginMutation("racing", GraphFailureReconcile, binding.Predecessor, 7); err != nil {
		t.Fatal(err)
	}
	if err := a.FinishMutation("first", true); err != nil {
		t.Fatal(err)
	}
	if a.FinishMutation("racing", true) == nil {
		t.Fatal("racing stale revision committed an effect")
	}
	if a.Status().PendingMutations != 0 {
		t.Fatal("refused writer did not drain")
	}
}
