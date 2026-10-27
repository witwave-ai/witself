package syncmodel

import (
	"bytes"
	"errors"
)

// CapturedEffect identifies one semantic effect, including repeated writes to an
// object and their dependencies. It is miniature harness data, not a writer hook.
type CapturedEffect struct {
	Reference    string              `json:"reference"`
	Operation    OperationID         `json:"operation"`
	AtomicGroup  string              `json:"atomic_group"`
	ObjectID     string              `json:"object_id"`
	Value        CanonicalTypedValue `json:"value"`
	Dependencies []Dependency        `json:"dependencies"`
}

func (e CapturedEffect) canonical(account string) ([]byte, error) {
	if !validLabel(e.Reference) || !validLabel(e.AtomicGroup) || !validLabel(e.ObjectID) || e.Dependencies == nil || len(e.Dependencies) > MaxDependencies {
		return nil, errors.New("invalid captured effect")
	}
	if err := e.Value.Validate(); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, d := range e.Dependencies {
		if err := d.Validate(); err != nil || d.AccountID != account {
			return nil, errors.New("invalid capture dependency")
		}
		b, err := MarshalCanonical(d)
		if err != nil || seen[string(b)] {
			return nil, errors.New("invalid capture dependency")
		}
		seen[string(b)] = true
	}
	// Dependency order is a set; semantic effect order is retained.
	e.Dependencies = (CausalBasis{Dependencies: e.Dependencies}).canonical().Dependencies
	return MarshalCanonical(e)
}

// JournalBundle is a sealed, provisional local capture. Finalized is allocated
// by the SQL counter adapter; it is never an authority acceptance receipt.
type JournalBundle struct {
	AccountID         string
	Incarnation       Incarnation
	Capture           CaptureEpoch
	BundleID          string
	Finalized         Ordinal
	LogicalOperations []OperationID
	AtomicGroups      []string
	SemanticEffects   []CanonicalTypedValue
	Effects           []CapturedEffect
	OriginClass       string
	Sealed            bool
}

// CaptureJournal checks a declared journal against every miniature observed
// effect. SQL tests separately prove admission locks and deferred rollback.
type CaptureJournal struct {
	incarnation Incarnation
	capture     CaptureEpoch
	bundleID    string
	effects     [][]byte
	references  map[string]bool
	sealed      bool
}

// AdmitCapture explicitly admits a capable writer under active fixture capture.
// The SQL adapter must obtain these booleans through locking control-row reads.
func AdmitCapture(inc Incarnation, capture CaptureEpoch, bundleID string, capabilityEnabled, captureActive bool) (*CaptureJournal, error) {
	if !capabilityEnabled || !captureActive || !validLabel(bundleID) {
		return nil, errors.New("capture admission refused")
	}
	if err := inc.Validate(); err != nil {
		return nil, err
	}
	if err := capture.Validate(); err != nil {
		return nil, err
	}
	return &CaptureJournal{incarnation: inc, capture: capture, bundleID: bundleID, references: make(map[string]bool)}, nil
}

// RecordEffect observes one effect before sealing; distinct references retain
// repeated changes to the same object rather than collapsing them to row count.
func (j *CaptureJournal) RecordEffect(e CapturedEffect) error {
	if j == nil || j.sealed || j.references == nil {
		return errors.New("capture mutation refused")
	}
	b, err := e.canonical(j.incarnation.AccountID)
	if err != nil {
		return err
	}
	if j.references[e.Reference] {
		return errors.New("duplicate semantic effect")
	}
	j.effects = append(j.effects, b)
	j.references[e.Reference] = true
	return nil
}

// Seal requires exact semantic coverage, including dependency identity. An
// empty, partial, substituted or reordered journal cannot seal a mutation.
func (j *CaptureJournal) Seal(finalized Ordinal, effects []CapturedEffect) (JournalBundle, error) {
	if j == nil || j.sealed || len(j.effects) == 0 || len(effects) != len(j.effects) || finalized == 0 || finalized.Validate() != nil {
		return JournalBundle{}, errors.New("capture coverage incomplete")
	}
	b := JournalBundle{AccountID: j.incarnation.AccountID, Incarnation: j.incarnation, Capture: j.capture, BundleID: j.bundleID, Finalized: finalized, OriginClass: "original", Sealed: true}
	ops, groups := make(map[OperationID]bool), make(map[string]bool)
	for i, e := range effects {
		raw, err := e.canonical(j.incarnation.AccountID)
		if err != nil || !bytes.Equal(raw, j.effects[i]) {
			return JournalBundle{}, errors.New("capture coverage incomplete")
		}
		var copyEffect CapturedEffect
		if err := UnmarshalStrict(raw, &copyEffect); err != nil {
			return JournalBundle{}, err
		}
		b.Effects = append(b.Effects, copyEffect)
		b.SemanticEffects = append(b.SemanticEffects, copyEffect.Value)
		if !ops[e.Operation] {
			b.LogicalOperations = append(b.LogicalOperations, e.Operation)
			ops[e.Operation] = true
		}
		if !groups[e.AtomicGroup] {
			b.AtomicGroups = append(b.AtomicGroups, e.AtomicGroup)
			groups[e.AtomicGroup] = true
		}
	}
	j.sealed = true
	return b, nil
}

// O-8 limits are test profile limits, not production policy.
const (
	MaxPendingOperations = 256
	MaxTransportBytes    = 8 * 1024 * 1024
	MaxCompoundBytes     = 1024 * 1024
)

// ErrResourceCapacity is an explicit resource refusal; it is not a receipt.
var ErrResourceCapacity = errors.New("sync model: resource capacity refused")

// ResourceUsage is the retained size/count reservation for one operation.
type ResourceUsage struct {
	EnvelopeBytes   int
	DependencyCount int
	FragmentCount   int
	CompoundBytes   int
	TransportBytes  int
}

// ResourceQueue is a single-node queue with independent per-account budgets.
// It reserves before adding work and never evicts existing work on refusal.
type ResourceQueue struct {
	accounts map[string]map[OperationID]ResourceUsage
}

// Admit atomically reserves all six fixture resource limits. Exact retries do
// not consume capacity twice; an altered reservation is refused.
func (q *ResourceQueue) Admit(account string, op OperationID, u ResourceUsage) error {
	if !validLabel(account) || u.EnvelopeBytes < 0 || u.DependencyCount < 0 || u.FragmentCount < 0 || u.CompoundBytes < 0 || u.TransportBytes < u.EnvelopeBytes || u.TransportBytes < u.CompoundBytes {
		return errors.New("invalid resource reservation")
	}
	if u.EnvelopeBytes > MaxEnvelopeBytes || u.DependencyCount > MaxDependencies || u.FragmentCount > MaxFragments || u.CompoundBytes > MaxCompoundBytes {
		return ErrResourceCapacity
	}
	if old, ok := q.accounts[account][op]; ok {
		if old != u {
			return errors.New("resource reservation changed")
		}
		return nil
	}
	n, retained := q.Usage(account)
	if n >= MaxPendingOperations || u.TransportBytes > MaxTransportBytes-retained {
		return ErrResourceCapacity
	}
	if q.accounts == nil {
		q.accounts = make(map[string]map[OperationID]ResourceUsage)
	}
	if q.accounts[account] == nil {
		q.accounts[account] = make(map[OperationID]ResourceUsage)
	}
	q.accounts[account][op] = u
	return nil
}

// Usage returns the retained reservation totals without inspecting payloads.
func (q *ResourceQueue) Usage(account string) (operations, retainedBytes int) {
	for _, u := range q.accounts[account] {
		operations++
		retainedBytes += u.TransportBytes
	}
	return operations, retainedBytes
}
