package syncmodel

import "errors"

// TranscriptEntryID preserves an existing opaque entry identity. Numeric
// presentation coordinates and operation IDs are separate identity domains.
type TranscriptEntryID string

// LegacyCoordinateKind separates transcript sequences from memory versions.
type LegacyCoordinateKind string

// Legacy coordinate kinds identify separate numeric presentation namespaces.
const (
	LegacyTranscriptSequence LegacyCoordinateKind = "transcript_sequence"
	LegacyMemoryVersion      LegacyCoordinateKind = "memory_version"
)

// Legacy projection errors are value-free refusals of invalid or ambiguous work.
var (
	ErrLegacyProjectionInvalid   = errors.New("invalid legacy projection mapping")
	ErrLegacyProjectionConflict  = errors.New("legacy projection mapping conflict")
	ErrLegacyProjectionAmbiguous = errors.New("ambiguous legacy coordinate")
	ErrLegacyProjectionMissing   = errors.New("legacy coordinate not found")
	ErrLegacyProjectionDeleted   = errors.New("legacy identity deleted")
)

// LegacyProjectionScope qualifies every old integer by its immutable history,
// ownership, object generation and projection. ExternalName is a retained alias;
// it is never sufficient to select a replacement object after name reuse.
type LegacyProjectionScope struct {
	Incarnation  Incarnation
	RealmID      string
	OwnerID      string
	ObjectID     string
	Generation   GenerationID
	ProjectionID string
	ExternalName string
}

func (s LegacyProjectionScope) valid() bool {
	return s.Incarnation.Validate() == nil && validLabel(s.RealmID) && validLabel(s.OwnerID) &&
		validLabel(s.ObjectID) && s.Generation != (GenerationID{}) &&
		validLabel(s.ProjectionID) && validLabel(s.ExternalName)
}

// LegacyCoordinate is a projection-qualified numeric presentation alias.
type LegacyCoordinate struct {
	Kind   LegacyCoordinateKind
	Scope  LegacyProjectionScope
	Number Ordinal
}

func (c LegacyCoordinate) valid() bool {
	return c.Scope.valid() && c.Number > 0 && c.Number.Validate() == nil &&
		(c.Kind == LegacyTranscriptSequence || c.Kind == LegacyMemoryVersion)
}

// LegacyIdentity is a typed, payload-free target. Exactly the identity selected
// by the coordinate kind is populated; an integer is never a causal token.
type LegacyIdentity struct {
	EntryID   TranscriptEntryID
	VersionID RevisionID
}

func (i LegacyIdentity) valid(kind LegacyCoordinateKind) bool {
	if kind == LegacyTranscriptSequence {
		return validLabel(string(i.EntryID)) && i.VersionID == (RevisionID{})
	}
	return kind == LegacyMemoryVersion && i.EntryID == "" && i.VersionID != (RevisionID{})
}

// LegacyProjectionRecord retains the original numeric alias and its exact
// target even after deletion. Deleted never means the identity can be reused.
type LegacyProjectionRecord struct {
	Coordinate LegacyCoordinate
	Identity   LegacyIdentity
	Deleted    bool
}

type legacyObject struct {
	kind        LegacyCoordinateKind
	incarnation Incarnation
	realm       string
	owner       string
	object      string
	generation  GenerationID
}

func (s LegacyProjectionScope) object(kind LegacyCoordinateKind) legacyObject {
	return legacyObject{kind, s.Incarnation, s.RealmID, s.OwnerID, s.ObjectID, s.Generation}
}

type legacyTarget struct {
	object   legacyObject
	identity LegacyIdentity
}

// LegacyProjectionBackfill is a pure migration oracle, not a production schema
// or durable adapter. Callers supply reviewed identities; this model never mints
// a replacement identity on retry and exposes no runtime-readiness claim.
type LegacyProjectionBackfill struct {
	records     map[LegacyCoordinate]LegacyIdentity
	deleted     map[legacyTarget]bool
	generations map[legacyObject]LegacyProjectionScope
}

// Backfill atomically installs one batch. A conflicting final row cannot leave
// earlier rows partly installed. Existing aliases are immutable, including
// aliases whose payload has since been deleted. Missing deleted objects require
// explicit historical tombstones instead of resurrecting a stale live row.
func (m *LegacyProjectionBackfill) Backfill(records []LegacyProjectionRecord) error {
	staged := make(map[LegacyCoordinate]LegacyIdentity, len(m.records)+len(records))
	for coordinate, identity := range m.records {
		staged[coordinate] = identity
	}
	for _, record := range records {
		c := record.Coordinate
		if !c.valid() || !record.Identity.valid(c.Kind) {
			return ErrLegacyProjectionInvalid
		}
		if old, exists := staged[c]; exists {
			if old != record.Identity {
				return ErrLegacyProjectionConflict
			}
			continue
		}
		if _, deleted := m.generations[c.Scope.object(c.Kind)]; deleted && !record.Deleted {
			return ErrLegacyProjectionDeleted
		}
		for oldCoordinate, identity := range staged {
			if oldCoordinate.Scope.object(oldCoordinate.Kind) == c.Scope.object(c.Kind) && oldCoordinate.Scope.ProjectionID == c.Scope.ProjectionID &&
				oldCoordinate.Kind == c.Kind && oldCoordinate.Scope.ExternalName != c.Scope.ExternalName {
				return ErrLegacyProjectionConflict
			}
			// A projection cannot contain the same immutable target twice.
			if oldCoordinate.Scope == c.Scope && oldCoordinate.Kind == c.Kind && identity == record.Identity {
				return ErrLegacyProjectionConflict
			}
		}
		staged[c] = record.Identity
	}
	if m.deleted == nil {
		m.deleted = make(map[legacyTarget]bool)
	}
	for _, record := range records {
		if record.Deleted {
			m.deleted[legacyTarget{record.Coordinate.Scope.object(record.Coordinate.Kind), record.Identity}] = true
		}
	}
	m.records = staged
	return nil
}

// Resolve returns a retained identity and its availability independently. A
// deleted entry keeps its exact alias; no later generation can replace it.
func (m *LegacyProjectionBackfill) Resolve(c LegacyCoordinate) (LegacyProjectionRecord, error) {
	if !c.valid() {
		return LegacyProjectionRecord{}, ErrLegacyProjectionInvalid
	}
	identity, exists := m.records[c]
	if !exists {
		return LegacyProjectionRecord{}, ErrLegacyProjectionMissing
	}
	_, generationDeleted := m.generations[c.Scope.object(c.Kind)]
	deleted := generationDeleted || m.deleted[legacyTarget{c.Scope.object(c.Kind), identity}]
	return LegacyProjectionRecord{Coordinate: c, Identity: identity, Deleted: deleted}, nil
}

// Delete retires availability across every projection of this exact immutable
// target. It preserves aliases and cannot affect another generation or account.
func (m *LegacyProjectionBackfill) Delete(c LegacyCoordinate) error {
	record, err := m.Resolve(c)
	if err != nil {
		return err
	}
	m.deleted[legacyTarget{c.Scope.object(c.Kind), record.Identity}] = true
	return nil
}

// DeleteGeneration records a deletion floor even before backfill. This models
// retention/deletion racing migration without erasing identity knowledge.
func (m *LegacyProjectionBackfill) DeleteGeneration(kind LegacyCoordinateKind, scope LegacyProjectionScope) error {
	if !scope.valid() || (kind != LegacyTranscriptSequence && kind != LegacyMemoryVersion) {
		return ErrLegacyProjectionInvalid
	}
	if m.generations == nil {
		m.generations = make(map[legacyObject]LegacyProjectionScope)
	}
	if old, exists := m.generations[scope.object(kind)]; exists && old.ExternalName != scope.ExternalName {
		return ErrLegacyProjectionConflict
	}
	m.generations[scope.object(kind)] = scope
	return nil
}

// ResolveUnqualified models a legacy external-name lookup. All historical
// projections and generations participate in ambiguity, including deleted ones;
// filtering to live rows would silently remap reused external names.
func (m *LegacyProjectionBackfill) ResolveUnqualified(kind LegacyCoordinateKind, incarnation Incarnation, realm, owner, name string, number Ordinal) (LegacyProjectionRecord, error) {
	if incarnation.Validate() != nil || !validLabel(realm) || !validLabel(owner) || !validLabel(name) || number == 0 || number.Validate() != nil ||
		(kind != LegacyTranscriptSequence && kind != LegacyMemoryVersion) {
		return LegacyProjectionRecord{}, ErrLegacyProjectionInvalid
	}
	matchesName := func(scope LegacyProjectionScope) bool {
		return scope.Incarnation == incarnation && scope.RealmID == realm && scope.OwnerID == owner && scope.ExternalName == name
	}
	objects := make(map[legacyObject]bool)
	var found LegacyCoordinate
	count := 0
	for c := range m.records {
		if c.Kind != kind || !matchesName(c.Scope) {
			continue
		}
		objects[c.Scope.object(c.Kind)] = true
		if c.Number == number {
			found = c
			count++
		}
	}
	for object, scope := range m.generations {
		if object.kind == kind && matchesName(scope) {
			objects[object] = true
		}
	}
	if len(objects) > 1 || count > 1 {
		return LegacyProjectionRecord{}, ErrLegacyProjectionAmbiguous
	}
	if count == 0 {
		return LegacyProjectionRecord{}, ErrLegacyProjectionMissing
	}
	return m.Resolve(found)
}

// LegacyProjectionObservations contains only the typed resolutions and refusal
// observed during H03 execution. Empty observations do not establish success.
type LegacyProjectionObservations struct {
	OriginalAliases  []LegacyProjectionRecord
	RetriedAliases   []LegacyProjectionRecord
	DeletedAlias     LegacyProjectionRecord
	ReusedNameAlias  LegacyProjectionRecord
	AmbiguityRefused bool
}

// RunLegacyProjectionBackfillScenario executes the deterministic H03 fixture.
// Observations and errors are value-free; their expected values are independently
// frozen by the runtime binding, not certified by this adapter.
func RunLegacyProjectionBackfillScenario() (LegacyProjectionObservations, error) {
	var observed LegacyProjectionObservations
	scope := LegacyProjectionScope{
		Incarnation: Incarnation{AccountID: "account_fixture", Lifecycle: 1, Restore: RestoreMarker{1}},
		RealmID:     "realm_fixture", OwnerID: "owner_fixture", ObjectID: "conversation_fixture",
		Generation: GenerationID{1}, ProjectionID: "projection_legacy", ExternalName: "conversation_name",
	}
	first := LegacyProjectionRecord{Coordinate: LegacyCoordinate{LegacyTranscriptSequence, scope, 1}, Identity: LegacyIdentity{EntryID: "entry_original"}}
	memory := first
	memory.Coordinate.Kind = LegacyMemoryVersion
	memory.Coordinate.Scope.ObjectID, memory.Coordinate.Scope.ExternalName = "memory_fixture", "memory_name"
	memory.Identity = LegacyIdentity{VersionID: RevisionID{1}}
	var model LegacyProjectionBackfill
	for _, aliases := range []*[]LegacyProjectionRecord{&observed.OriginalAliases, &observed.RetriedAliases} {
		if err := model.Backfill([]LegacyProjectionRecord{first, memory}); err != nil {
			return observed, err
		}
		for _, coordinate := range []LegacyCoordinate{first.Coordinate, memory.Coordinate} {
			resolved, err := model.Resolve(coordinate)
			if err != nil {
				return observed, err
			}
			*aliases = append(*aliases, resolved)
		}
	}
	if err := model.DeleteGeneration(LegacyTranscriptSequence, scope); err != nil {
		return observed, err
	}
	if err := model.Backfill([]LegacyProjectionRecord{first}); err != nil {
		return observed, err
	}
	var err error
	observed.DeletedAlias, err = model.Resolve(first.Coordinate)
	if err != nil {
		return observed, err
	}
	reused := first
	reused.Coordinate.Scope.Generation, reused.Coordinate.Scope.ObjectID = GenerationID{2}, "conversation_new"
	reused.Identity.EntryID = "entry_new"
	if err := model.Backfill([]LegacyProjectionRecord{reused}); err != nil {
		return observed, err
	}
	observed.ReusedNameAlias, err = model.Resolve(reused.Coordinate)
	if err != nil {
		return observed, err
	}
	_, err = model.ResolveUnqualified(LegacyTranscriptSequence, scope.Incarnation, scope.RealmID, scope.OwnerID, scope.ExternalName, 1)
	observed.AmbiguityRefused = errors.Is(err, ErrLegacyProjectionAmbiguous)
	return observed, nil
}
