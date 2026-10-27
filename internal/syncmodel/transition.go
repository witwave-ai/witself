package syncmodel

import (
	"errors"
	"slices"
)

// NamespaceTransitionKind keeps the three SC-2 transitions distinct.
type NamespaceTransitionKind string

// Namespace transition kinds are closed; joining and enrollment are not adoption.
const (
	TransitionAdoption   NamespaceTransitionKind = "adoption"
	TransitionJoin       NamespaceTransitionKind = "namespace_join"
	TransitionEnrollment NamespaceTransitionKind = "device_enrollment"
)

// TransitionCheckpoint is durable progress, not a claim of runtime convergence.
type TransitionCheckpoint string

// Transition checkpoints separate reviewed identity, complete copy and publication.
const (
	TransitionIntent             TransitionCheckpoint = "intent"
	TransitionAdmissionReview    TransitionCheckpoint = "admission_review"
	TransitionSnapshotStaged     TransitionCheckpoint = "snapshot_staged"
	TransitionCatchupPending     TransitionCheckpoint = "catchup_pending"
	TransitionIdentityVerified   TransitionCheckpoint = "identity_verified"
	TransitionPublicationPending TransitionCheckpoint = "publication_pending"
	TransitionComplete           TransitionCheckpoint = "complete"
	TransitionBlocked            TransitionCheckpoint = "blocked"
	TransitionRebootstrap        TransitionCheckpoint = "rebootstrap"
)

// TransitionReview is an explicitly identified collision/ownership/sealed item.
// A caller cannot replace the required set with a blanket "reviewed" boolean.
type TransitionReview struct {
	Kind      string
	Reference string
}

// NamespaceTransitionInput freezes both data sets and existing work. Symbolic
// entries model populated data; bindings, completed outcomes and lease bounds
// are carried unchanged. Enrollment resolves local data without merging names.
type NamespaceTransitionInput struct {
	ID                   string
	Kind                 NamespaceTransitionKind
	SourceNamespace      string
	DestinationNamespace string
	BindingID            string
	SourceEntries        []string
	DestinationEntries   []string
	Pending              []OperationID
	Completed            []OperationID
	LeaseThrough         Ordinal
	RequiredReviews      []TransitionReview
}

// TransitionEvidence supplies one checkpoint's reviewed model evidence. These
// are fixture assertions, never authenticated runtime proofs.
type TransitionEvidence struct {
	CapacityAvailable bool
	Reviewed          []TransitionReview
	PopulatedVerified bool
	CopyComplete      bool
	Watermark         Ordinal
	WatermarkVerified bool
	IdentityVerified  bool
	PublicationPinned bool
}

// NamespaceTransition is a pure, explicitly synthetic transition oracle.
// Original data and pending work are never overwritten by snapshot replacement.
type NamespaceTransition struct {
	input       NamespaceTransitionInput
	checkpoint  TransitionCheckpoint
	resume      TransitionCheckpoint
	watermark   Ordinal
	latest      Ordinal
	verified    bool
	copyReady   bool
	pending     []OperationID
	blockReason string
}

func cloneTransitionInput(in NamespaceTransitionInput) NamespaceTransitionInput {
	in.SourceEntries = slices.Clone(in.SourceEntries)
	in.DestinationEntries = slices.Clone(in.DestinationEntries)
	in.Pending = slices.Clone(in.Pending)
	in.Completed = slices.Clone(in.Completed)
	in.RequiredReviews = slices.Clone(in.RequiredReviews)
	return in
}

// NewNamespaceTransition records intent with independent populated namespaces.
func NewNamespaceTransition(in NamespaceTransitionInput) (*NamespaceTransition, error) {
	if !validModelReference(in.ID) || !validModelReference(in.SourceNamespace) || !validModelReference(in.DestinationNamespace) || !validModelReference(in.BindingID) || in.LeaseThrough.Validate() != nil {
		return nil, errors.New("invalid transition identity")
	}
	switch in.Kind {
	case TransitionAdoption, TransitionJoin, TransitionEnrollment:
	default:
		return nil, errors.New("invalid transition kind")
	}
	for _, entries := range [][]string{in.SourceEntries, in.DestinationEntries} {
		seenEntries := make(map[string]bool)
		for _, entry := range entries {
			if !validModelReference(entry) || seenEntries[entry] {
				return nil, errors.New("invalid transition data inventory")
			}
			seenEntries[entry] = true
		}
	}
	seenWork := make(map[OperationID]bool)
	for _, work := range [][]OperationID{in.Pending, in.Completed} {
		for _, op := range work {
			if seenWork[op] {
				return nil, errors.New("invalid transition work inventory")
			}
			seenWork[op] = true
		}
	}
	seen := make(map[TransitionReview]bool)
	for _, r := range in.RequiredReviews {
		if (r.Kind != "identity" && r.Kind != "ownership" && r.Kind != "sealed_reference") || !validModelReference(r.Reference) || seen[r] {
			return nil, errors.New("invalid transition review inventory")
		}
		seen[r] = true
	}
	if in.Kind == TransitionJoin {
		for _, entry := range in.SourceEntries {
			if slices.Contains(in.DestinationEntries, entry) && !seen[TransitionReview{Kind: "identity", Reference: entry}] {
				return nil, errors.New("join collision omitted from review inventory")
			}
		}
	}
	return &NamespaceTransition{input: cloneTransitionInput(in), checkpoint: TransitionIntent, pending: slices.Clone(in.Pending)}, nil
}

// Input returns a defensive copy of preserved identities, data and work.
func (t *NamespaceTransition) Input() NamespaceTransitionInput { return cloneTransitionInput(t.input) }

// Namespace returns the selected namespace without rewriting either original
// namespace or silently merging data. Adoption always retains its source;
// enrollment references the existing destination and never merges the source.
func (t *NamespaceTransition) Namespace() string {
	if t.input.Kind == TransitionAdoption {
		return t.input.SourceNamespace
	}
	return t.input.DestinationNamespace
}

// Checkpoint returns progress and a value-free refusal reason.
func (t *NamespaceTransition) Checkpoint() (TransitionCheckpoint, string) {
	return t.checkpoint, t.blockReason
}

// Pending includes writes made while a transition was interrupted or blocked.
func (t *NamespaceTransition) Pending() []OperationID { return slices.Clone(t.pending) }

// RecordPending captures ongoing local work without treating it as acceptance.
func (t *NamespaceTransition) RecordPending(op OperationID) error {
	if t == nil || t.checkpoint == TransitionComplete || uint64(t.latest) == MaxCounter {
		return errors.New("transition write refused")
	}
	if slices.Contains(t.pending, op) || slices.Contains(t.input.Completed, op) {
		return nil
	}
	t.pending = append(t.pending, op)
	t.latest++
	if t.checkpoint == TransitionIdentityVerified || t.checkpoint == TransitionPublicationPending {
		t.checkpoint = TransitionCatchupPending
	}
	return nil
}

func (t *NamespaceTransition) block(reason string) error {
	t.resume = t.checkpoint
	t.checkpoint = TransitionBlocked
	t.blockReason = reason
	return errors.New(reason)
}

func reviewsEqual(required, got []TransitionReview) bool {
	if len(required) != len(got) {
		return false
	}
	seen := make(map[TransitionReview]bool)
	for _, r := range got {
		if seen[r] || !slices.Contains(required, r) {
			return false
		}
		seen[r] = true
	}
	return true
}

// Advance commits precisely the next checkpoint. Retrying its lost ACK is
// idempotent. No snapshot-only branch can reach complete.
func (t *NamespaceTransition) Advance(next TransitionCheckpoint, e TransitionEvidence) error {
	if t == nil {
		return errors.New("transition unavailable")
	}
	if next == t.checkpoint {
		return nil
	}
	switch t.checkpoint {
	case TransitionIntent:
		if next != TransitionAdmissionReview {
			break
		}
	case TransitionAdmissionReview:
		if next != TransitionSnapshotStaged {
			break
		}
		if !e.CapacityAvailable {
			return t.block("capacity_refused")
		}
		if t.input.Kind == TransitionAdoption && t.input.SourceNamespace != t.input.DestinationNamespace && len(t.input.DestinationEntries) > 0 {
			return t.block("namespace_collision")
		}
		if !reviewsEqual(t.input.RequiredReviews, e.Reviewed) {
			return t.block("identity_review_pending")
		}
		if t.input.Kind == TransitionEnrollment && len(t.input.DestinationEntries) > 0 && !e.PopulatedVerified {
			return t.block("populated_replica_pending")
		}
		t.copyReady = false
	case TransitionSnapshotStaged:
		if next != TransitionCatchupPending {
			break
		}
		if !e.CopyComplete || !e.WatermarkVerified || e.Watermark.Validate() != nil || e.Watermark < t.latest {
			return errors.New("snapshot_incomplete")
		}
		t.copyReady, t.verified, t.watermark = true, true, e.Watermark
		t.latest = e.Watermark
	case TransitionCatchupPending:
		if next != TransitionIdentityVerified {
			break
		}
		if !t.copyReady || !e.WatermarkVerified || !e.IdentityVerified || e.Watermark.Validate() != nil || e.Watermark < t.latest {
			return errors.New("catchup_pending")
		}
		t.verified, t.watermark, t.latest = true, e.Watermark, e.Watermark
	case TransitionIdentityVerified:
		if next != TransitionPublicationPending {
			break
		}
	case TransitionPublicationPending:
		if next != TransitionComplete {
			break
		}
		if !e.PublicationPinned || !t.verified || t.watermark != t.latest || !t.copyReady {
			return errors.New("publication_pending")
		}
	default:
		return errors.New("transition requires resume")
	}
	allowed := map[TransitionCheckpoint]TransitionCheckpoint{
		TransitionIntent: TransitionAdmissionReview, TransitionAdmissionReview: TransitionSnapshotStaged,
		TransitionSnapshotStaged: TransitionCatchupPending, TransitionCatchupPending: TransitionIdentityVerified,
		TransitionIdentityVerified: TransitionPublicationPending, TransitionPublicationPending: TransitionComplete,
	}
	if allowed[t.checkpoint] != next {
		return errors.New("transition checkpoint out of order")
	}
	t.checkpoint = next
	return nil
}

// Interrupt models a crash without modifying preserved namespaces or work.
// A resume must present the exact durable, verified boundary; otherwise it
// explicitly requests a fresh bootstrap and never continues a partial copy.
func (t *NamespaceTransition) Interrupt(watermark Ordinal, verified bool) {
	if t.checkpoint == TransitionComplete {
		return
	}
	if !verified || !t.verified || watermark != t.watermark {
		t.checkpoint, t.copyReady, t.verified = TransitionRebootstrap, false, false
		return
	}
	if t.checkpoint == TransitionBlocked {
		t.checkpoint = t.resume
	}
	if t.checkpoint == TransitionIdentityVerified || t.checkpoint == TransitionPublicationPending {
		t.checkpoint = TransitionCatchupPending
	}
	t.blockReason = ""
}

// Rebootstrap restarts admission and snapshot work while retaining every local
// operation, original namespace, immutable outcome and original lease bound.
func (t *NamespaceTransition) Rebootstrap() error {
	if t.checkpoint != TransitionRebootstrap && t.checkpoint != TransitionBlocked {
		return errors.New("rebootstrap not requested")
	}
	t.checkpoint, t.blockReason, t.copyReady, t.verified = TransitionAdmissionReview, "", false, false
	return nil
}

// RestoreStage enumerates SC-6.7's durable boundaries; readiness exists only at
// published. Intent and allocation history remain outside the restored dataset.
type RestoreStage string

// Restore checkpoints are committed in order with a fresh marker before readiness.
const (
	RestoreIntent    RestoreStage = "intent"
	RestoreMarkerSet RestoreStage = "marker"
	RestoreImported  RestoreStage = "import"
	RestoreCAS       RestoreStage = "cas"
	RestoreInstalled RestoreStage = "install"
	RestorePublished RestoreStage = "publish"
	RestoreAborted   RestoreStage = "aborted"
)

// RestoreIdentity preserves both pending operation identity and historical
// receipt acceptance incarnation. A restore never relabels either field.
type RestoreIdentity struct {
	Operation           OperationID
	AcceptedIncarnation Incarnation
}

// RestoreRecord is one durable intent, including its immutable restored work.
type RestoreRecord struct {
	ID         string
	Stage      RestoreStage
	Allocation IncarnationAllocation
	Identities []RestoreIdentity
}

// RestoreState belongs outside the simulated restore set. Immutable-return
// operations make checkpoints explicit for crash/reopen tests.
type RestoreState struct {
	History IncarnationHistory
	Records []RestoreRecord
}

func cloneRestore(s RestoreState) RestoreState {
	s.History.Allocations = slices.Clone(s.History.Allocations)
	s.Records = slices.Clone(s.Records)
	for i := range s.Records {
		s.Records[i].Identities = slices.Clone(s.Records[i].Identities)
	}
	return s
}

// BeginRestore records or replays exact intent before allocating a marker.
func BeginRestore(s RestoreState, id string, identities []RestoreIdentity) (RestoreState, error) {
	if !validModelReference(id) || s.History.Current.Validate() != nil {
		return s, errors.New("invalid restore intent")
	}
	seen := make(map[OperationID]bool)
	for _, item := range identities {
		if item.AcceptedIncarnation.Validate() != nil || item.AcceptedIncarnation.AccountID != s.History.Current.AccountID || seen[item.Operation] {
			return s, errors.New("invalid restore identity")
		}
		seen[item.Operation] = true
	}
	for _, r := range s.Records {
		if r.ID == id {
			if !slices.Equal(r.Identities, identities) {
				return s, errors.New("restore intent reuse conflict")
			}
			return cloneRestore(s), nil
		}
	}
	next := cloneRestore(s)
	next.Records = append(next.Records, RestoreRecord{ID: id, Stage: RestoreIntent, Identities: slices.Clone(identities)})
	return next, nil
}

// AdvanceRestore commits one durable boundary. CAS/install/publication require
// current external single-writer evidence, which this model cannot itself prove.
func AdvanceRestore(s RestoreState, id string, stage RestoreStage, marker RestoreMarker, externalAuthority bool) (RestoreState, error) {
	next := cloneRestore(s)
	for i := range next.Records {
		r := &next.Records[i]
		if r.ID != id {
			continue
		}
		if r.Stage == stage {
			if r.Stage != RestoreIntent && r.Allocation.Successor.Restore != marker {
				return s, errors.New("restore marker reuse conflict")
			}
			return next, nil
		}
		order := map[RestoreStage]RestoreStage{RestoreIntent: RestoreMarkerSet, RestoreMarkerSet: RestoreImported, RestoreImported: RestoreCAS, RestoreCAS: RestoreInstalled, RestoreInstalled: RestorePublished}
		if order[r.Stage] != stage || (r.Stage != RestoreIntent && r.Allocation.Successor.Restore != marker) {
			return s, errors.New("restore checkpoint conflict")
		}
		if stage == RestoreMarkerSet {
			h, allocation, err := AllocateIncarnation(next.History, IncarnationRequest{AllocationID: id, Kind: BackupRestore, Restore: marker})
			if err != nil {
				return s, err
			}
			next.History, r.Allocation = h, allocation
		}
		if stage == RestoreCAS || stage == RestoreInstalled || stage == RestorePublished {
			if !externalAuthority || next.History.Current != r.Allocation.Predecessor {
				return s, errors.New("restore authority pending")
			}
		}
		if stage == RestorePublished {
			next.History.Current = r.Allocation.Successor
		}
		r.Stage = stage
		return next, nil
	}
	return s, errors.New("restore intent missing")
}

// AbortRestore consumes an allocated epoch/marker without activating it.
func AbortRestore(s RestoreState, id string) (RestoreState, error) {
	next := cloneRestore(s)
	for i := range next.Records {
		if next.Records[i].ID == id {
			if next.Records[i].Stage == RestorePublished {
				return s, errors.New("published restore cannot abort")
			}
			next.Records[i].Stage = RestoreAborted
			return next, nil
		}
	}
	return s, errors.New("restore intent missing")
}
