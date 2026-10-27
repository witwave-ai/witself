package syncmodel

import (
	"errors"
	"slices"
)

// MaxEvidenceMembers bounds the disposable model's projection and proof sizes.
const MaxEvidenceMembers = 4096

// EvidenceScope keeps conversation generations and account histories separate.
// Its labels are opaque fixture identifiers, never external names or content.
type EvidenceScope struct {
	Incarnation    Incarnation  `json:"incarnation"`
	RealmID        string       `json:"realm_id"`
	OwnerID        string       `json:"owner_id"`
	ConversationID string       `json:"conversation_id"`
	Generation     GenerationID `json:"generation"`
}

func (s EvidenceScope) validate() error {
	if s.Incarnation.Validate() != nil || s.Generation == (GenerationID{}) {
		return errors.New("invalid evidence scope")
	}
	for _, label := range []string{s.Incarnation.AccountID, s.RealmID, s.OwnerID, s.ConversationID} {
		if !evidenceLabel(label) {
			return errors.New("invalid evidence scope label")
		}
	}
	return nil
}

// EvidenceMembershipProof freezes the complete ordered membership of one
// inclusive source range. Source coordinates are aliases, not a query over a
// later merged projection. No payload, external name or content hash is stored.
// Verification is against this model's retained seal, not a cryptographic or
// independently durable proof.
type EvidenceMembershipProof struct {
	Scope            EvidenceScope       `json:"scope"`
	EvidenceID       RevisionID          `json:"evidence_id"`
	SourceProjection RevisionID          `json:"source_projection"`
	First            Ordinal             `json:"first"`
	Last             Ordinal             `json:"last"`
	Members          []TranscriptEntryID `json:"members"`
}

type evidenceProjectionKey struct {
	scope EvidenceScope
	id    RevisionID
}

type evidenceProjection struct {
	first   Ordinal
	members []TranscriptEntryID
}

type evidenceEntryKey struct {
	scope EvidenceScope
	id    TranscriptEntryID
}

// OrderedEvidenceModel is a pure, single-threaded oracle for SC-1.17. It has no
// production adapter, storage, payload recovery or authority implications.
type OrderedEvidenceModel struct {
	projections map[evidenceProjectionKey]evidenceProjection
	proofs      map[evidenceProjectionKey]EvidenceMembershipProof
	entries     map[evidenceEntryKey]bool // true means permanently unavailable
}

// AppendProjection accepts only an exact replay or contiguous append. Existing
// coordinates cannot be rewritten by capture arrival order or retry. New
// projections can present the same entries with interleaved unrelated entries.
func (m *OrderedEvidenceModel) AppendProjection(scope EvidenceScope, projectionID RevisionID, first Ordinal, entries []TranscriptEntryID) error {
	if scope.validate() != nil || projectionID == (RevisionID{}) || first == 0 || first.Validate() != nil || len(entries) == 0 || len(entries) > MaxEvidenceMembers || uint64(len(entries)-1) > MaxCounter-uint64(first) {
		return errors.New("invalid evidence projection")
	}
	seen := make(map[TranscriptEntryID]bool, len(entries))
	for _, entry := range entries {
		if !evidenceLabel(string(entry)) || seen[entry] {
			return errors.New("invalid evidence projection member")
		}
		seen[entry] = true
	}
	key := evidenceProjectionKey{scope, projectionID}
	projection, exists := m.projections[key]
	if exists {
		if first < projection.first || uint64(first-projection.first) > uint64(len(projection.members)) {
			return errors.New("evidence projection gap")
		}
		offset := int(first - projection.first)
		overlap := min(len(entries), len(projection.members)-offset)
		if !slices.Equal(entries[:overlap], projection.members[offset:offset+overlap]) {
			return errors.New("evidence projection coordinate reuse")
		}
		if overlap == len(entries) {
			return nil
		}
		entries = entries[overlap:]
		if len(projection.members)+len(entries) > MaxEvidenceMembers {
			return errors.New("evidence projection capacity")
		}
		for _, entry := range projection.members {
			if slices.Contains(entries, entry) {
				return errors.New("duplicate evidence projection member")
			}
		}
	} else {
		projection.first = first
	}
	if m.projections == nil {
		m.projections = make(map[evidenceProjectionKey]evidenceProjection)
		m.entries = make(map[evidenceEntryKey]bool)
	}
	projection.members = append(projection.members, entries...)
	m.projections[key] = projection
	for _, entry := range entries {
		entryKey := evidenceEntryKey{scope, entry}
		if _, known := m.entries[entryKey]; !known {
			m.entries[entryKey] = false
		}
	}
	return nil
}

// SealRange captures complete membership before further capture/interleaving.
// Reusing an evidence identity is allowed only for its exact original range.
func (m *OrderedEvidenceModel) SealRange(scope EvidenceScope, evidenceID, projectionID RevisionID, first, last Ordinal) (EvidenceMembershipProof, error) {
	projection, exists := m.projections[evidenceProjectionKey{scope, projectionID}]
	if scope.validate() != nil || evidenceID == (RevisionID{}) || !exists || first < projection.first || last < first || last.Validate() != nil || uint64(last-projection.first) >= uint64(len(projection.members)) {
		return EvidenceMembershipProof{}, errors.New("unavailable evidence source range")
	}
	proof := EvidenceMembershipProof{Scope: scope, EvidenceID: evidenceID, SourceProjection: projectionID, First: first, Last: last,
		Members: slices.Clone(projection.members[int(first-projection.first) : int(last-projection.first)+1])}
	key := evidenceProjectionKey{scope, evidenceID}
	if previous, ok := m.proofs[key]; ok {
		if !sameEvidenceProof(previous, proof) {
			return EvidenceMembershipProof{}, errors.New("evidence identity reuse")
		}
		return cloneEvidenceProof(previous), nil
	}
	if m.proofs == nil {
		m.proofs = make(map[evidenceProjectionKey]EvidenceMembershipProof)
	}
	m.proofs[key] = cloneEvidenceProof(proof)
	return proof, nil
}

// BackfillRange preserves legacy coordinates in their explicit original
// projection. Missing entries refuse atomically; deletion does not erase IDs.
func (m *OrderedEvidenceModel) BackfillRange(scope EvidenceScope, evidenceID, projectionID RevisionID, first, last Ordinal) (EvidenceMembershipProof, error) {
	return m.SealRange(scope, evidenceID, projectionID, first, last)
}

// Verify requires the exact retained order, source, scope, bounds and seal ID.
func (m *OrderedEvidenceModel) Verify(proof EvidenceMembershipProof) error {
	expected, exists := m.proofs[evidenceProjectionKey{proof.Scope, proof.EvidenceID}]
	if !exists || !sameEvidenceProof(expected, proof) {
		return errors.New("evidence membership mismatch")
	}
	return nil
}

// Delete makes a known entry unavailable without changing any sealed membership.
func (m *OrderedEvidenceModel) Delete(scope EvidenceScope, entry TranscriptEntryID) error {
	key := evidenceEntryKey{scope, entry}
	if _, known := m.entries[key]; !known {
		return errors.New("unknown evidence entry")
	}
	m.entries[key] = true
	return nil
}

// Unavailable lists deleted members in their sealed order, independently of
// proof validity. Captures and backfill can never clear deletion knowledge.
func (m *OrderedEvidenceModel) Unavailable(proof EvidenceMembershipProof) ([]TranscriptEntryID, error) {
	if err := m.Verify(proof); err != nil {
		return nil, err
	}
	unavailable := make([]TranscriptEntryID, 0)
	for _, entry := range proof.Members {
		if m.entries[evidenceEntryKey{proof.Scope, entry}] {
			unavailable = append(unavailable, entry)
		}
	}
	return unavailable, nil
}

// Positions locates exactly the sealed members in another projection. It does
// not expand their endpoints into a range and refuses absent or reordered IDs.
func (m *OrderedEvidenceModel) Positions(proof EvidenceMembershipProof, projectionID RevisionID) ([]Ordinal, error) {
	if err := m.Verify(proof); err != nil {
		return nil, err
	}
	projection, exists := m.projections[evidenceProjectionKey{proof.Scope, projectionID}]
	if !exists {
		return nil, errors.New("unknown evidence destination projection")
	}
	positions := make([]Ordinal, 0, len(proof.Members))
	for _, entry := range proof.Members {
		index := slices.Index(projection.members, entry)
		if index < 0 {
			return nil, errors.New("evidence member absent from projection")
		}
		position := projection.first + Ordinal(index)
		if len(positions) > 0 && position <= positions[len(positions)-1] {
			return nil, errors.New("evidence member order changed")
		}
		positions = append(positions, position)
	}
	return positions, nil
}

func cloneEvidenceProof(proof EvidenceMembershipProof) EvidenceMembershipProof {
	proof.Members = slices.Clone(proof.Members)
	return proof
}

func sameEvidenceProof(a, b EvidenceMembershipProof) bool {
	return a.Scope == b.Scope && a.EvidenceID == b.EvidenceID && a.SourceProjection == b.SourceProjection && a.First == b.First && a.Last == b.Last && slices.Equal(a.Members, b.Members)
}

func evidenceLabel(label string) bool {
	if len(label) == 0 || len(label) > 128 {
		return false
	}
	hexRun := 0
	for _, char := range label {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' && char != '-' {
			return false
		}
		if (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F') || (char >= '0' && char <= '9') {
			hexRun++
			if hexRun >= 64 {
				return false
			}
		} else {
			hexRun = 0
		}
	}
	return true
}

// EvidenceMembershipObservations retains operation-returned membership,
// destination coordinates, tombstones and verification outcomes, without payload.
type EvidenceMembershipObservations struct {
	SealedMembership     EvidenceMembershipProof
	DestinationPositions []Ordinal
	RetriedMembership    EvidenceMembershipProof
	UnavailableMembers   []TranscriptEntryID
	ForgedProofRefused   bool
	OriginalProofValid   bool
}

// RunOrderedEvidenceMembershipScenario executes a compiled, value-free fixture.
// The binding independently validates observations before claiming model evidence.
func RunOrderedEvidenceMembershipScenario() (EvidenceMembershipObservations, error) {
	var observed EvidenceMembershipObservations
	var model OrderedEvidenceModel
	scope := EvidenceScope{Incarnation: Incarnation{AccountID: "account", Lifecycle: 1}, RealmID: "realm", OwnerID: "owner", ConversationID: "conversation", Generation: GenerationID{1}}
	source, merged, evidence := RevisionID{1}, RevisionID{2}, RevisionID{3}
	if err := model.AppendProjection(scope, source, 10, []TranscriptEntryID{"entry_A", "entry_B", "entry_C"}); err != nil {
		return observed, err
	}
	var err error
	observed.SealedMembership, err = model.SealRange(scope, evidence, source, 10, 12)
	if err != nil {
		return observed, err
	}
	if err := model.AppendProjection(scope, merged, 20, []TranscriptEntryID{"entry_A", "entry_X", "entry_B", "entry_Y", "entry_C"}); err != nil {
		return observed, err
	}
	observed.DestinationPositions, err = model.Positions(observed.SealedMembership, merged)
	if err != nil {
		return observed, err
	}
	if err := model.Delete(scope, "entry_B"); err != nil {
		return observed, err
	}
	observed.RetriedMembership, err = model.BackfillRange(scope, evidence, source, 10, 12)
	if err != nil {
		return observed, err
	}
	observed.UnavailableMembers, err = model.Unavailable(observed.RetriedMembership)
	if err != nil {
		return observed, err
	}
	forged := cloneEvidenceProof(observed.SealedMembership)
	forged.Members[1] = "entry_X"
	observed.ForgedProofRefused = model.Verify(forged) != nil
	observed.OriginalProofValid = model.Verify(observed.SealedMembership) == nil
	return observed, nil
}
