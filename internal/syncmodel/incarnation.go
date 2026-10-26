package syncmodel

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Incarnation identifies an account history; it never rewrites an operation's
// original incarnation or an immutable receipt's acceptance incarnation.
type Incarnation struct {
	AccountID string         `json:"account_id"`
	Lifecycle LifecycleEpoch `json:"lifecycle_epoch"`
	Restore   RestoreMarker  `json:"restore_marker"`
}

// Validate checks the account reference and lifecycle interoperability bound.
func (i Incarnation) Validate() error {
	if !validModelReference(i.AccountID) {
		return errors.New("invalid incarnation account")
	}
	return i.Lifecycle.Validate()
}

func validModelReference(s string) bool {
	if s == "" || strings.TrimSpace(s) != s || !utf8.ValidString(s) || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// IncarnationTransition selects the two distinct SC-6.6 allocation rules.
type IncarnationTransition string

const (
	// ProvenMove advances the lifecycle epoch while preserving the restore marker.
	ProvenMove IncarnationTransition = "proven_move"
	// BackupRestore allocates a new epoch and a previously unused restore marker.
	BackupRestore IncarnationTransition = "backup_restore"
)

// IncarnationAllocation is consumed even if installation/publication is aborted.
// Keeping allocation separate from authority is essential: this model allocates
// history but confers no readiness, grant, or active-authority capability.
type IncarnationAllocation struct {
	AllocationID string
	Kind         IncarnationTransition
	Predecessor  Incarnation
	Successor    Incarnation
}

// IncarnationHistory must live outside the account's modeled restore set. The
// caller persists the returned state before using an allocation; this pure model
// does not supply durability or compare-and-swap across processes.
type IncarnationHistory struct {
	Current       Incarnation
	LastAllocated LifecycleEpoch
	Allocations   []IncarnationAllocation
}

// IncarnationRequest supplies an allocation identity and fixture transition evidence.
type IncarnationRequest struct {
	AllocationID string
	Kind         IncarnationTransition
	MoveProven   bool
	Restore      RestoreMarker // supplied bytes for deterministic fixtures
}

// AllocateIncarnation is the SC-6.6 transition oracle. It leaves Current alone
// and returns fresh history, so discarding/aborting an allocation does not make
// its epoch reusable. Exact retries return their durable original allocation.
func AllocateIncarnation(h IncarnationHistory, request IncarnationRequest) (IncarnationHistory, IncarnationAllocation, error) {
	fail := func() (IncarnationHistory, IncarnationAllocation, error) {
		return h, IncarnationAllocation{}, errors.New("invalid incarnation allocation")
	}
	if h.Current.Validate() != nil || h.LastAllocated.Validate() != nil || h.LastAllocated < h.Current.Lifecycle || !validModelReference(request.AllocationID) {
		return fail()
	}
	if request.Kind != ProvenMove && request.Kind != BackupRestore {
		return fail()
	}
	if request.Kind == ProvenMove && !request.MoveProven {
		return fail()
	}
	seenIDs := make(map[string]bool, len(h.Allocations))
	seenEpochs := make(map[LifecycleEpoch]bool, len(h.Allocations))
	for _, old := range h.Allocations {
		if !validModelReference(old.AllocationID) || seenIDs[old.AllocationID] || seenEpochs[old.Successor.Lifecycle] ||
			old.Predecessor.Validate() != nil || old.Successor.Validate() != nil || old.Predecessor.AccountID != h.Current.AccountID ||
			old.Successor.AccountID != h.Current.AccountID || old.Successor.Lifecycle <= old.Predecessor.Lifecycle || old.Successor.Lifecycle > h.LastAllocated ||
			(old.Kind != ProvenMove && old.Kind != BackupRestore) ||
			(old.Kind == ProvenMove && old.Predecessor.Restore != old.Successor.Restore) ||
			(old.Kind == BackupRestore && old.Predecessor.Restore == old.Successor.Restore) {
			return fail()
		}
		seenIDs[old.AllocationID], seenEpochs[old.Successor.Lifecycle] = true, true
	}
	for _, old := range h.Allocations {
		if old.AllocationID == request.AllocationID {
			if old.Kind != request.Kind || (request.Kind == BackupRestore && old.Successor.Restore != request.Restore) {
				return fail()
			}
			return h, old, nil
		}
	}
	if h.LastAllocated == LifecycleEpoch(MaxLifecycleEpoch) {
		return fail()
	}
	marker := h.Current.Restore
	if request.Kind == BackupRestore {
		marker = request.Restore
		if marker == h.Current.Restore {
			return fail()
		}
		for _, old := range h.Allocations {
			if marker == old.Predecessor.Restore || marker == old.Successor.Restore {
				return fail()
			}
		}
	}
	allocation := IncarnationAllocation{
		AllocationID: request.AllocationID, Kind: request.Kind, Predecessor: h.Current,
		Successor: Incarnation{AccountID: h.Current.AccountID, Lifecycle: h.LastAllocated + 1, Restore: marker},
	}
	next := h
	next.LastAllocated = allocation.Successor.Lifecycle
	next.Allocations = append(append([]IncarnationAllocation(nil), h.Allocations...), allocation)
	return next, allocation, nil
}

// AllocateRandomRestore mints a production-candidate cryptographic marker only
// for a new restore intent. Retrying an existing intent reuses its exact marker.
func AllocateRandomRestore(h IncarnationHistory, allocationID string) (IncarnationHistory, IncarnationAllocation, error) {
	for _, old := range h.Allocations {
		if old.AllocationID == allocationID {
			return AllocateIncarnation(h, IncarnationRequest{AllocationID: allocationID, Kind: BackupRestore, Restore: old.Successor.Restore})
		}
	}
	marker, err := NewRestoreMarker()
	if err != nil {
		return h, IncarnationAllocation{}, errors.New("restore marker generation failed")
	}
	return AllocateIncarnation(h, IncarnationRequest{AllocationID: allocationID, Kind: BackupRestore, Restore: marker})
}
