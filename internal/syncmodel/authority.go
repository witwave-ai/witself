package syncmodel

import (
	"errors"
	"maps"
	"slices"
)

// GraphMutationKind includes indirect mutations, not just successful API calls.
type GraphMutationKind string

// Graph mutation kinds cover direct and indirect request-graph writers.
const (
	GraphClaim            GraphMutationKind = "claim"
	GraphRenew            GraphMutationKind = "renew"
	GraphRelease          GraphMutationKind = "release"
	GraphOffer            GraphMutationKind = "offer"
	GraphDecline          GraphMutationKind = "decline"
	GraphSelect           GraphMutationKind = "select"
	GraphComplete         GraphMutationKind = "complete"
	GraphCancel           GraphMutationKind = "cancel"
	GraphExpiry           GraphMutationKind = "expiry"
	GraphAgentDelete      GraphMutationKind = "agent_delete"
	GraphMaintenance      GraphMutationKind = "maintenance"
	GraphFailureReconcile GraphMutationKind = "failure_path_reconcile"
	GraphLinkedMessage    GraphMutationKind = "linked_protocol_message"
)

// GraphMutationKinds returns the closed SC-3.7/3.10 fixture inventory.
func GraphMutationKinds() []GraphMutationKind {
	return []GraphMutationKind{GraphClaim, GraphRenew, GraphRelease, GraphOffer, GraphDecline, GraphSelect, GraphComplete, GraphCancel, GraphExpiry, GraphAgentDelete, GraphMaintenance, GraphFailureReconcile, GraphLinkedMessage}
}

// AuthorityIdentity is the complete independently verified graph authority scope.
type AuthorityIdentity struct {
	Incarnation    Incarnation
	RequestID      string
	InstallationID string
	Epoch          GraphEpoch
}

func (a AuthorityIdentity) valid() bool {
	return a.Incarnation.Validate() == nil && validModelReference(a.RequestID) && validModelReference(a.InstallationID) && a.Epoch.Validate() == nil
}

// ExecutionDisposition keeps historical completion, unexecuted reservation and
// ambiguous provider-started work distinct through transfer and recovery.
type ExecutionDisposition string

// Execution dispositions never imply a new logical action after transfer.
const (
	ExecutionCompleted        ExecutionDisposition = "completed"
	ExecutionReserved         ExecutionDisposition = "reserved"
	ExecutionAmbiguousStarted ExecutionDisposition = "ambiguous_started"
)

// TransferredExecution retains the original logical identity and lease bound.
// It cannot mint a fresh action or infer that expiry stopped an external effect.
type TransferredExecution struct {
	Action       OperationID
	Disposition  ExecutionDisposition
	LeaseThrough Ordinal
	OutcomeRef   string
}

// ReassignmentAllowed authorizes only an unexecuted reservation; any future
// action still uses Action. Ambiguous work requires explicit outcome recovery.
func (e TransferredExecution) ReassignmentAllowed() bool { return e.Disposition == ExecutionReserved }

// TransferBinding is immutable after freeze begins, including when the target
// disappears or the client loses an acknowledgement.
type TransferBinding struct {
	ID          TransferID
	Predecessor AuthorityIdentity
	Successor   AuthorityIdentity
}

// TransferFenceProof is synthetic evidence of the committed final graph. A pin
// is modeled separately: possession of this value alone confers no authority.
type TransferFenceProof struct {
	Version        uint32
	Binding        TransferBinding
	FinalRevision  GraphRevision
	JournalThrough Ordinal
	Manifest       Commitment
}

// TransferStatus is value-free lifecycle information, never a runtime proof.
type TransferStatus struct {
	Freezing                     bool
	Fenced                       bool
	Staged                       bool
	Installed                    bool
	PublicationCommitted         bool
	PublicationPinned            bool
	Aborted                      bool
	AbortPinned                  bool
	RecoveryInstalled            bool
	RecoveryInstallationPinned   bool
	RecoveryPublicationCommitted bool
	RecoveryPublicationPinned    bool
	FinalRevision                GraphRevision
	PendingMutations             int
	Outbox                       []string
}

type graphMutation struct {
	kind     GraphMutationKind
	identity AuthorityIdentity
	revision GraphRevision
}

// AuthorityTransfer models a single request graph. Internal state prevents a
// caller from reactivating a copied old epoch by changing a row's state label.
// Every durable control commit has a separate witness-pin checkpoint. There is
// no clock input and no timeout path that creates authority.
type AuthorityTransfer struct {
	current        AuthorityIdentity
	revision       GraphRevision
	publicationPin bool
	work           []TransferredExecution
	mutations      map[string]graphMutation
	binding        *TransferBinding
	proof          *TransferFenceProof
	status         TransferStatus
	fencePinned    bool
	installPinned  bool
	abortOwner     *AuthorityIdentity
	retired        map[GraphEpoch]bool
}

// NewAuthorityTransfer starts an independently admitted synthetic authority.
func NewAuthorityTransfer(a AuthorityIdentity, revision GraphRevision, publicationPinned bool, work []TransferredExecution) (*AuthorityTransfer, error) {
	if !a.valid() || revision.Validate() != nil {
		return nil, errors.New("invalid graph authority")
	}
	seen := make(map[OperationID]bool)
	for _, e := range work {
		if e.LeaseThrough.Validate() != nil || seen[e.Action] {
			return nil, errors.New("invalid transferred execution")
		}
		switch e.Disposition {
		case ExecutionCompleted:
			if !validModelReference(e.OutcomeRef) {
				return nil, errors.New("completion outcome missing")
			}
		case ExecutionReserved, ExecutionAmbiguousStarted:
			if e.OutcomeRef != "" {
				return nil, errors.New("unsettled execution has outcome")
			}
		default:
			return nil, errors.New("unknown execution disposition")
		}
		seen[e.Action] = true
	}
	return &AuthorityTransfer{current: a, revision: revision, publicationPin: publicationPinned, work: slices.Clone(work), mutations: make(map[string]graphMutation), retired: make(map[GraphEpoch]bool)}, nil
}

// Work returns unchanged historical execution and original lease bounds.
func (a *AuthorityTransfer) Work() []TransferredExecution { return slices.Clone(a.work) }

// ReservationExecutable checks an existing unexecuted reservation's original
// action and lease. An unresolved started action in this graph blocks any
// reassignment; neither a completed nor an unknown action is executable.
func (a *AuthorityTransfer) ReservationExecutable(identity AuthorityIdentity, action OperationID, at Ordinal) bool {
	if !a.CanMutate(identity) || at.Validate() != nil {
		return false
	}
	for _, work := range a.work {
		if work.Disposition == ExecutionAmbiguousStarted {
			return false
		}
	}
	for _, work := range a.work {
		if work.Action == action {
			return work.Disposition == ExecutionReserved && at <= work.LeaseThrough
		}
	}
	return false
}

// Status returns a defensive snapshot, including pending publication outboxes.
func (a *AuthorityTransfer) Status() TransferStatus {
	s := a.status
	s.PendingMutations = len(a.mutations)
	s.Outbox = slices.Clone(s.Outbox)
	return s
}

// CanMutate checks the current complete scope and publication pin every time.
func (a *AuthorityTransfer) CanMutate(identity AuthorityIdentity) bool {
	return a != nil && a.current == identity && a.publicationPin && !a.retired[identity.Epoch] && !a.status.Freezing
}

// BeginMutation admits an exact revision before the freeze boundary.
func (a *AuthorityTransfer) BeginMutation(id string, kind GraphMutationKind, identity AuthorityIdentity, revision GraphRevision) error {
	if !validModelReference(id) || !slices.Contains(GraphMutationKinds(), kind) || !a.CanMutate(identity) || revision != a.revision {
		return errors.New("authority_epoch_stale")
	}
	if _, exists := a.mutations[id]; exists {
		return errors.New("graph mutation already pending")
	}
	a.mutations[id] = graphMutation{kind: kind, identity: identity, revision: revision}
	return nil
}

// FinishMutation drains admitted writers. A stale revision refuses its effects;
// a successful pre-freeze commit belongs to the final captured graph revision.
func (a *AuthorityTransfer) FinishMutation(id string, commit bool) error {
	m, ok := a.mutations[id]
	if !ok {
		return errors.New("graph mutation missing")
	}
	delete(a.mutations, id)
	if !commit {
		return nil
	}
	if a.retired[m.identity.Epoch] || !a.publicationPin || m.identity != a.current || m.revision != a.revision || uint64(a.revision) == MaxCounter {
		return errors.New("graph revision conflict")
	}
	a.revision++
	return nil
}

// BeginFreeze records one immutable transfer intent and refuses new writers.
// Existing admitted writers must drain or refuse before CommitFence succeeds.
func (a *AuthorityTransfer) BeginFreeze(binding TransferBinding) error {
	if a.binding != nil {
		if *a.binding == binding {
			return nil
		}
		return errors.New("transfer binding conflict")
	}
	if !binding.Predecessor.valid() || !binding.Successor.valid() || binding.Predecessor != a.current || !a.CanMutate(a.current) ||
		binding.Successor.Incarnation.AccountID != a.current.Incarnation.AccountID || binding.Successor.RequestID != a.current.RequestID ||
		binding.Successor.Epoch <= a.current.Epoch || binding.Successor.InstallationID == a.current.InstallationID {
		return errors.New("invalid transfer binding")
	}
	a.binding = &binding
	a.status.Freezing = true
	return nil
}

// CommitFence atomically records final graph and transfer outbox in the model.
// The predecessor epoch is permanently retired, even if transfer later aborts.
func (a *AuthorityTransfer) CommitFence(journal Ordinal, manifest Commitment) (TransferFenceProof, error) {
	if a.proof != nil {
		if a.proof.JournalThrough == journal && a.proof.Manifest == manifest {
			return *a.proof, nil
		}
		return TransferFenceProof{}, errors.New("fence replay conflict")
	}
	if a.binding == nil || !a.status.Freezing || len(a.mutations) != 0 || journal.Validate() != nil {
		return TransferFenceProof{}, errors.New("graph freeze drain pending")
	}
	proof := TransferFenceProof{Version: 1, Binding: *a.binding, FinalRevision: a.revision, JournalThrough: journal, Manifest: manifest}
	a.proof, a.publicationPin, a.status.Fenced = &proof, false, true
	a.retired[a.current.Epoch] = true
	a.status.FinalRevision = a.revision
	a.status.Outbox = append(a.status.Outbox, "fence")
	return proof, nil
}

// PinFence accepts only the exact committed proof, never a numeric cutoff.
func (a *AuthorityTransfer) PinFence(proof TransferFenceProof) error {
	if a.proof == nil || *a.proof != proof {
		return errors.New("fence proof conflict")
	}
	a.fencePinned = true
	return nil
}

// Stage keeps complete imported graph state invisible and nonexecutable.
func (a *AuthorityTransfer) Stage(proof TransferFenceProof, complete bool) error {
	if a.proof == nil || *a.proof != proof || !a.fencePinned || !complete || a.status.Aborted {
		return errors.New("authority_transfer_pending")
	}
	a.status.Staged = true
	return nil
}

// Install durably installs successor epoch separately from publication.
func (a *AuthorityTransfer) Install() error {
	if !a.status.Staged || a.status.Aborted {
		return errors.New("authority_transfer_pending")
	}
	if !a.status.Installed {
		a.status.Installed = true
		a.status.Outbox = append(a.status.Outbox, "install")
	}
	return nil
}

// PinInstall makes the installation checkpoint usable for publication.
func (a *AuthorityTransfer) PinInstall() error {
	if !a.status.Installed || a.status.Aborted {
		return errors.New("authority_transfer_pending")
	}
	a.installPinned = true
	return nil
}

// Publish commits successor publication and its outbox. Commit already excludes
// abort, but execution remains prohibited until the separate publication pin.
func (a *AuthorityTransfer) Publish() error {
	if !a.status.Installed || !a.installPinned || a.status.Aborted {
		return errors.New("authority_transfer_pending")
	}
	if !a.status.PublicationCommitted {
		a.status.PublicationCommitted = true
		a.status.Outbox = append(a.status.Outbox, "publication")
	}
	return nil
}

// PinPublication enables only the exact successor and retains every old fence.
func (a *AuthorityTransfer) PinPublication() error {
	if !a.status.PublicationCommitted || a.status.Aborted {
		return errors.New("authority_transfer_pending")
	}
	a.status.PublicationPinned, a.status.Freezing = true, false
	a.current, a.publicationPin = a.binding.Successor, true
	return nil
}

// Abort commits a terminal successor abort naming a strictly newer recovery
// owner. It never reactivates either abandoned epoch, including while unpinned.
func (a *AuthorityTransfer) Abort(owner AuthorityIdentity) error {
	if a.status.Aborted {
		if a.abortOwner != nil && *a.abortOwner == owner {
			return nil
		}
		return errors.New("abort replay conflict")
	}
	if !owner.valid() || a.binding == nil || !a.status.Fenced || !a.fencePinned || a.status.PublicationCommitted ||
		owner.Incarnation.AccountID != a.current.Incarnation.AccountID || owner.RequestID != a.current.RequestID || owner.Epoch <= a.binding.Successor.Epoch {
		return errors.New("transfer abort refused")
	}
	a.abortOwner, a.status.Aborted = &owner, true
	a.retired[a.binding.Successor.Epoch] = true
	a.status.Outbox = append(a.status.Outbox, "abort")
	return nil
}

// PinAbort permits explicit installation of the recorded newer recovery owner;
// neither restoring a predecessor row nor merely waiting invokes that path.
func (a *AuthorityTransfer) PinAbort() error {
	if !a.status.Aborted || a.abortOwner == nil || a.status.PublicationCommitted {
		return errors.New("transfer abort pending")
	}
	a.status.AbortPinned = true
	return nil
}

// RecoverAborted installs only the exact pinned owner at its fresh epoch. Its
// own installation/publication checkpoints remain necessary before execution.
func (a *AuthorityTransfer) RecoverAborted(owner AuthorityIdentity) error {
	if !a.status.AbortPinned || a.abortOwner == nil || *a.abortOwner != owner || a.retired[owner.Epoch] {
		return errors.New("transfer recovery pending")
	}
	if !a.status.RecoveryInstalled {
		a.status.RecoveryInstalled = true
		a.status.Outbox = append(a.status.Outbox, "recovery_install")
	}
	return nil
}

// PinRecoveryInstall witnesses the new owner installation independently.
func (a *AuthorityTransfer) PinRecoveryInstall() error {
	if !a.status.RecoveryInstalled {
		return errors.New("transfer recovery pending")
	}
	a.status.RecoveryInstallationPinned = true
	return nil
}

// PublishRecovery records fresh-epoch publication without reactivating an old
// epoch. An abort proof alone is not an executable grant.
func (a *AuthorityTransfer) PublishRecovery() error {
	if !a.status.RecoveryInstallationPinned {
		return errors.New("transfer recovery pending")
	}
	if !a.status.RecoveryPublicationCommitted {
		a.status.RecoveryPublicationCommitted = true
		a.status.Outbox = append(a.status.Outbox, "recovery_publication")
	}
	return nil
}

// PinRecoveryPublication makes only the recorded new recovery epoch usable.
func (a *AuthorityTransfer) PinRecoveryPublication() error {
	if !a.status.RecoveryPublicationCommitted || a.abortOwner == nil {
		return errors.New("transfer recovery pending")
	}
	a.status.RecoveryPublicationPinned = true
	a.current, a.publicationPin, a.status.Freezing = *a.abortOwner, true, false
	return nil
}

// Restart retains committed checkpoints and immutable work. Uncommitted
// mutations are rolled back. This is model recovery, not physical durability.
func (a *AuthorityTransfer) Restart() *AuthorityTransfer {
	next := *a
	next.work = slices.Clone(a.work)
	next.mutations = make(map[string]graphMutation)
	next.retired = maps.Clone(a.retired)
	next.status.Outbox = slices.Clone(a.status.Outbox)
	if a.binding != nil {
		binding := *a.binding
		next.binding = &binding
	}
	if a.proof != nil {
		proof := *a.proof
		next.proof = &proof
	}
	if a.abortOwner != nil {
		owner := *a.abortOwner
		next.abortOwner = &owner
	}
	return &next
}
