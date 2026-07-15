package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	// MemoryCurationPlanSchemaV1 is the only accepted curation-plan language.
	// It is deliberately independent from the HTTP and archive schema versions.
	MemoryCurationPlanSchemaV1 = "witself.memory-plan.v1"

	// MemoryCurationOperationCreate identifies a create action payload.
	MemoryCurationOperationCreate = "create"
	// MemoryCurationOperationReplace identifies a replacement action payload.
	MemoryCurationOperationReplace = "replace"
	// MemoryCurationOperationSupersede identifies a supersession action payload.
	MemoryCurationOperationSupersede = "supersede"
	// MemoryCurationOperationRelate identifies a relation action payload.
	MemoryCurationOperationRelate = "relate"
	// MemoryCurationOperationProposeFact identifies a fact-candidate action payload.
	MemoryCurationOperationProposeFact = "propose_fact"

	// MemoryCurationRelationDerivedFrom identifies a derivation lineage edge.
	MemoryCurationRelationDerivedFrom = "derived_from"
	// MemoryCurationRelationSummarizes identifies a summary lineage edge.
	MemoryCurationRelationSummarizes = "summarizes"
	// MemoryCurationRelationMergedFrom identifies a merge lineage edge.
	MemoryCurationRelationMergedFrom = "merged_from"
	// MemoryCurationRelationSplitFrom identifies a split lineage edge.
	MemoryCurationRelationSplitFrom = "split_from"
	// MemoryCurationRelationConflictsWith identifies a conflicting-memory edge.
	MemoryCurationRelationConflictsWith = "conflicts_with"

	// MaxMemoryCurationPlanActions bounds actions in one accepted plan.
	MaxMemoryCurationPlanActions = 128
	// MaxMemoryCurationSupersedeReplacements bounds replacements in one supersession.
	MaxMemoryCurationSupersedeReplacements = 32
	// MaxMemoryCurationRelationsPerNewMemory bounds lineage edges on one new memory.
	MaxMemoryCurationRelationsPerNewMemory = 32
	// MaxMemoryCurationPlanEvidenceRows bounds evidence across one plan.
	MaxMemoryCurationPlanEvidenceRows = 1024
	// MaxMemoryCurationPlanRelationRows bounds relation rows across one plan.
	MaxMemoryCurationPlanRelationRows = 1024
	// MaxMemoryCurationPlanContentBytes bounds aggregate memory content in one plan.
	MaxMemoryCurationPlanContentBytes = 8 * 1024 * 1024
	// MaxMemoryCurationPlanArtifactBytes bounds aggregate inline artifacts in one plan.
	MaxMemoryCurationPlanArtifactBytes = 8 * 1024 * 1024
	// MaxMemoryCurationPlanFactValueBytes bounds aggregate fact-candidate values.
	MaxMemoryCurationPlanFactValueBytes = 1024 * 1024
	// MaxMemoryCurationPlanCanonicalJSONBytes bounds canonical accepted-plan JSON.
	MaxMemoryCurationPlanCanonicalJSONBytes = 32 * 1024 * 1024
)

var memoryCurationLocalRefPattern = regexpMustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// MemoryCurationPlanDraft is caller-authored. DraftRevision is caller-local
// optimistic state; it is not carried into the immutable accepted-plan hash.
type MemoryCurationPlanDraft struct {
	Schema        string                     `json:"schema"`
	DraftRevision int64                      `json:"draft_revision"`
	Actions       []MemoryCurationPlanAction `json:"actions"`
}

// MemoryCurationPlan is the normalized immutable payload stored and hashed by
// the server. It contains no server clock values or caller-supplied audit data.
type MemoryCurationPlan struct {
	Schema       string                     `json:"schema"`
	PlanRevision int64                      `json:"plan_revision"`
	Actions      []MemoryCurationPlanAction `json:"actions"`
}

// MemoryCurationPlanAction is a tagged union. Exactly one payload pointer must
// be present and its field name must match Operation.
type MemoryCurationPlanAction struct {
	Ordinal     int64                            `json:"ordinal"`
	Operation   string                           `json:"operation"`
	Create      *MemoryCurationCreateAction      `json:"create,omitempty"`
	Replace     *MemoryCurationReplaceAction     `json:"replace,omitempty"`
	Supersede   *MemoryCurationSupersedeAction   `json:"supersede,omitempty"`
	Relate      *MemoryCurationRelateAction      `json:"relate,omitempty"`
	ProposeFact *MemoryCurationProposeFactAction `json:"propose_fact,omitempty"`
}

// MemoryCurationCreateAction creates one full memory. MemoryID is forbidden in
// a draft and populated by plan acceptance. LocalRef remains as a correlation
// label; references elsewhere in the accepted plan contain only MemoryID.
type MemoryCurationCreateAction struct {
	LocalRef  string                          `json:"local_ref"`
	MemoryID  string                          `json:"memory_id,omitempty"`
	Snapshot  MemoryCurationMemorySnapshot    `json:"snapshot"`
	Relations []MemoryCurationLineageRelation `json:"relations,omitempty"`
}

// MemoryCurationMemorySnapshot is a full desired value-bearing snapshot.
// Occurrence bounds are semantic event intervals, not server/audit timestamps.
type MemoryCurationMemorySnapshot struct {
	Content         string                   `json:"content"`
	ContentEncoding string                   `json:"content_encoding,omitempty"`
	Kind            string                   `json:"kind,omitempty"`
	Tags            []string                 `json:"tags,omitempty"`
	Links           []string                 `json:"links,omitempty"`
	Salience        *float64                 `json:"salience,omitempty"`
	Sensitive       bool                     `json:"sensitive,omitempty"`
	OccurredFrom    *time.Time               `json:"occurred_from,omitempty"`
	OccurredUntil   *time.Time               `json:"occurred_until,omitempty"`
	Evidence        []MemoryCurationEvidence `json:"evidence,omitempty"`
}

// MemoryCurationTargetReference is an optimistic reference to a mutable head.
// A draft contains exactly one of MemoryID or LocalRef. Accepted references
// always contain MemoryID and a positive ExpectedVersion.
type MemoryCurationTargetReference struct {
	MemoryID        string `json:"memory_id,omitempty"`
	LocalRef        string `json:"local_ref,omitempty"`
	ExpectedVersion int64  `json:"expected_version"`
}

// MemoryCurationVersionReference pins one immutable version for provenance or
// a graph edge. A local new-memory reference may name only version one.
type MemoryCurationVersionReference struct {
	MemoryID string `json:"memory_id,omitempty"`
	LocalRef string `json:"local_ref,omitempty"`
	Version  int64  `json:"version"`
}

// MemoryCurationReplaceAction replaces one optimistic target with a full snapshot.
type MemoryCurationReplaceAction struct {
	Target   MemoryCurationTargetReference `json:"target"`
	Snapshot MemoryCurationMemorySnapshot  `json:"snapshot"`
	Reason   string                        `json:"reason,omitempty"`
}

// MemoryCurationSupersedeAction supersedes one optimistic target with one or
// more immutable replacement versions.
type MemoryCurationSupersedeAction struct {
	Target       MemoryCurationTargetReference    `json:"target"`
	Replacements []MemoryCurationVersionReference `json:"replacements"`
	Reason       string                           `json:"reason,omitempty"`
}

// MemoryCurationLineageRelation attaches a typed lineage edge to a new memory.
type MemoryCurationLineageRelation struct {
	RelationType string                         `json:"relation_type"`
	To           MemoryCurationVersionReference `json:"to"`
}

// MemoryCurationRelateAction creates a typed edge between immutable versions.
type MemoryCurationRelateAction struct {
	RelationType string                         `json:"relation_type"`
	From         MemoryCurationVersionReference `json:"from"`
	To           MemoryCurationVersionReference `json:"to"`
}

// MemoryCurationEvidence mirrors capture evidence but represents memory
// provenance with a version-reference object so client-local references can be
// resolved before plan hashing.
type MemoryCurationEvidence struct {
	InputEvidenceID     string                          `json:"input_evidence_id,omitempty"`
	Type                string                          `json:"type"`
	Role                string                          `json:"role,omitempty"`
	ResolutionState     string                          `json:"resolution_state"`
	ExternalLocator     string                          `json:"external_locator,omitempty"`
	ResolvedKind        string                          `json:"resolved_kind,omitempty"`
	SourceTranscriptID  string                          `json:"source_transcript_id,omitempty"`
	SourceSequenceFrom  int64                           `json:"source_sequence_from,omitempty"`
	SourceSequenceUntil int64                           `json:"source_sequence_until,omitempty"`
	SourceMemory        *MemoryCurationVersionReference `json:"source_memory,omitempty"`
	SourceMessageID     string                          `json:"source_message_id,omitempty"`
	SourceImportLocator string                          `json:"source_import_locator,omitempty"`
	ArtifactExcerpt     []byte                          `json:"artifact_excerpt,omitempty"`
	ArtifactSensitive   bool                            `json:"artifact_sensitive,omitempty"`
	TerminalReasonCode  string                          `json:"terminal_reason_code,omitempty"`
	SourceDigest        string                          `json:"source_digest,omitempty"`
}

// MemoryCurationProposeFactAction can only create a review candidate. It does
// not expose FactSourceKind, fact.set authority, idempotency fields, audit
// timestamps, confirmation timestamps, or deleted-fact recreation.
type MemoryCurationProposeFactAction struct {
	Subject     string                   `json:"subject,omitempty"`
	Predicate   string                   `json:"predicate"`
	ValueType   string                   `json:"value_type,omitempty"`
	Value       json.RawMessage          `json:"value"`
	Recurrence  string                   `json:"recurrence,omitempty"`
	Cardinality string                   `json:"cardinality,omitempty"`
	Sensitive   bool                     `json:"sensitive,omitempty"`
	Confidence  *float64                 `json:"confidence,omitempty"`
	ValidFrom   *time.Time               `json:"valid_from,omitempty"`
	ValidUntil  *time.Time               `json:"valid_until,omitempty"`
	Reason      string                   `json:"reason,omitempty"`
	Evidence    []MemoryCurationEvidence `json:"evidence"`
}

// MemoryCurationMemoryIDAllocator is injected by the persistence layer. Plan
// code invokes it once per create local ref in action order.
type MemoryCurationMemoryIDAllocator interface {
	AllocateMemoryID(localRef string) (string, error)
}

// MemoryCurationMemoryIDAllocatorFunc adapts a function to
// MemoryCurationMemoryIDAllocator.
type MemoryCurationMemoryIDAllocatorFunc func(localRef string) (string, error)

// AllocateMemoryID delegates allocation to f.
func (f MemoryCurationMemoryIDAllocatorFunc) AllocateMemoryID(localRef string) (string, error) {
	return f(localRef)
}

// MemoryCurationPlanAcceptOptions supplies the immutable revision and exactly
// one ID source. PreallocatedMemoryIDs is the retry/import path; Allocator is
// the first-acceptance path.
type MemoryCurationPlanAcceptOptions struct {
	PlanRevision          int64
	PreallocatedMemoryIDs map[string]string
	Allocator             MemoryCurationMemoryIDAllocator
}

// MemoryCurationPreallocatedMemoryID binds a draft-local reference to its
// server-allocated stable memory ID.
type MemoryCurationPreallocatedMemoryID struct {
	LocalRef string `json:"local_ref"`
	MemoryID string `json:"memory_id"`
}

// MemoryCurationImpactPreview intentionally contains counts only. It can be
// logged and returned without leaking content, fact values, locators, or ids.
type MemoryCurationImpactPreview struct {
	ActionCount           int `json:"action_count"`
	CreateActions         int `json:"create_actions"`
	ReplaceActions        int `json:"replace_actions"`
	SupersedeActions      int `json:"supersede_actions"`
	RelateActions         int `json:"relate_actions"`
	ProposeFactActions    int `json:"propose_fact_actions"`
	NewMemories           int `json:"new_memories"`
	MemoryVersionWrites   int `json:"memory_version_writes"`
	EvidenceRows          int `json:"evidence_rows"`
	RelationRows          int `json:"relation_rows"`
	ExpectedVersionChecks int `json:"expected_version_checks"`
	FactCandidates        int `json:"fact_candidates"`
}

// MemoryCurationPlanAcceptance is the pure result consumed by the later plan
// persistence method. CanonicalJSON hashes exactly Plan and is returned as a
// defensive copy by CanonicalBytes.
type MemoryCurationPlanAcceptance struct {
	Plan                  MemoryCurationPlan                   `json:"plan"`
	PlanHash              string                               `json:"plan_hash"`
	PreallocatedMemoryIDs []MemoryCurationPreallocatedMemoryID `json:"preallocated_memory_ids,omitempty"`
	Preview               MemoryCurationImpactPreview          `json:"preview"`
	canonicalJSON         []byte
}

// CanonicalBytes returns a defensive copy of the canonical accepted-plan JSON.
func (a MemoryCurationPlanAcceptance) CanonicalBytes() []byte {
	return append([]byte(nil), a.canonicalJSON...)
}

// DecodeMemoryCurationPlanDraft decodes the public draft contract. It rejects
// unknown fields, duplicate object member names, trailing JSON, and oversized
// input before any ID allocation can occur.
func DecodeMemoryCurationPlanDraft(raw []byte) (MemoryCurationPlanDraft, error) {
	if len(raw) == 0 || len(raw) > MaxMemoryCurationPlanCanonicalJSONBytes {
		return MemoryCurationPlanDraft{}, memoryCurationInvalidf("plan JSON must contain 1-%d bytes", MaxMemoryCurationPlanCanonicalJSONBytes)
	}
	if !utf8.Valid(raw) {
		return MemoryCurationPlanDraft{}, memoryCurationInvalidf("plan JSON must be valid UTF-8")
	}
	if err := rejectDuplicateJSONNames(raw); err != nil {
		return MemoryCurationPlanDraft{}, memoryCurationInvalidf("invalid plan JSON: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var draft MemoryCurationPlanDraft
	if err := decoder.Decode(&draft); err != nil {
		return MemoryCurationPlanDraft{}, memoryCurationInvalidf("decode plan JSON: %v", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return MemoryCurationPlanDraft{}, memoryCurationInvalidf("decode plan JSON: %v", err)
	}
	return draft, nil
}

// AcceptMemoryCurationPlan normalizes and validates a draft without reading or
// writing a database. Authorization, run-input membership, and live-version
// checks remain the responsibility of the transactional plan method.
func AcceptMemoryCurationPlan(
	draft MemoryCurationPlanDraft,
	options MemoryCurationPlanAcceptOptions,
) (MemoryCurationPlanAcceptance, error) {
	if options.PlanRevision < 1 {
		return MemoryCurationPlanAcceptance{}, memoryCurationInvalidf("plan revision must be positive")
	}
	if options.Allocator != nil && options.PreallocatedMemoryIDs != nil {
		return MemoryCurationPlanAcceptance{}, memoryCurationInvalidf("provide an allocator or a preallocated id mapping, not both")
	}

	copyOfDraft, err := cloneMemoryCurationPlanDraft(draft)
	if err != nil {
		return MemoryCurationPlanAcceptance{}, err
	}
	copyOfDraft.Schema = strings.TrimSpace(copyOfDraft.Schema)
	if copyOfDraft.Schema != MemoryCurationPlanSchemaV1 {
		return MemoryCurationPlanAcceptance{}, memoryCurationInvalidf("schema must be %q", MemoryCurationPlanSchemaV1)
	}
	if copyOfDraft.DraftRevision < 1 {
		return MemoryCurationPlanAcceptance{}, memoryCurationInvalidf("draft revision must be positive")
	}
	if len(copyOfDraft.Actions) > MaxMemoryCurationPlanActions {
		return MemoryCurationPlanAcceptance{}, memoryCurationInvalidf("plan contains more than %d actions", MaxMemoryCurationPlanActions)
	}
	if copyOfDraft.Actions == nil {
		copyOfDraft.Actions = make([]MemoryCurationPlanAction, 0)
	}

	newMemories, err := validateMemoryCurationActionEnvelope(copyOfDraft.Actions)
	if err != nil {
		return MemoryCurationPlanAcceptance{}, err
	}
	assigned, mapping, err := allocateMemoryCurationIDs(newMemories, options)
	if err != nil {
		return MemoryCurationPlanAcceptance{}, err
	}

	budget := memoryCurationPlanBudget{}
	preview := MemoryCurationImpactPreview{ActionCount: len(copyOfDraft.Actions)}
	seenRelations := make(map[string]struct{})
	for index := range copyOfDraft.Actions {
		if err := normalizeMemoryCurationAction(&copyOfDraft.Actions[index], assigned, &budget, &preview, seenRelations); err != nil {
			return MemoryCurationPlanAcceptance{}, fmt.Errorf("action %d: %w", index+1, err)
		}
	}
	if err := budget.validate(); err != nil {
		return MemoryCurationPlanAcceptance{}, err
	}

	plan := MemoryCurationPlan{
		Schema:       MemoryCurationPlanSchemaV1,
		PlanRevision: options.PlanRevision,
		Actions:      copyOfDraft.Actions,
	}
	canonical, err := canonicalMemoryCurationJSON(plan)
	if err != nil {
		return MemoryCurationPlanAcceptance{}, err
	}
	if len(canonical) > MaxMemoryCurationPlanCanonicalJSONBytes {
		return MemoryCurationPlanAcceptance{}, memoryCurationInvalidf("canonical plan exceeds %d bytes", MaxMemoryCurationPlanCanonicalJSONBytes)
	}
	sum := sha256.Sum256(canonical)
	return MemoryCurationPlanAcceptance{
		Plan:                  plan,
		PlanHash:              hex.EncodeToString(sum[:]),
		PreallocatedMemoryIDs: mapping,
		Preview:               preview,
		canonicalJSON:         append([]byte(nil), canonical...),
	}, nil
}

func cloneMemoryCurationPlanDraft(draft MemoryCurationPlanDraft) (MemoryCurationPlanDraft, error) {
	if err := validateMemoryCurationUTF8(reflect.ValueOf(draft)); err != nil {
		return MemoryCurationPlanDraft{}, err
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		return MemoryCurationPlanDraft{}, memoryCurationInvalidf("encode draft: %v", err)
	}
	var out MemoryCurationPlanDraft
	if err := json.Unmarshal(raw, &out); err != nil {
		return MemoryCurationPlanDraft{}, memoryCurationInvalidf("copy draft: %v", err)
	}
	return out, nil
}

func validateMemoryCurationActionEnvelope(actions []MemoryCurationPlanAction) ([]*MemoryCurationCreateAction, error) {
	newMemories := make([]*MemoryCurationCreateAction, 0)
	seenLocalRefs := make(map[string]struct{})
	for index := range actions {
		action := &actions[index]
		if action.Ordinal != int64(index+1) {
			return nil, memoryCurationInvalidf("action ordinals must be contiguous and start at 1")
		}
		action.Operation = strings.TrimSpace(action.Operation)
		payloads := 0
		if action.Create != nil {
			payloads++
		}
		if action.Replace != nil {
			payloads++
		}
		if action.Supersede != nil {
			payloads++
		}
		if action.Relate != nil {
			payloads++
		}
		if action.ProposeFact != nil {
			payloads++
		}
		if payloads != 1 {
			return nil, memoryCurationInvalidf("action %d must contain exactly one operation payload", index+1)
		}
		switch action.Operation {
		case MemoryCurationOperationCreate:
			if action.Create == nil {
				return nil, memoryCurationInvalidf("action %d operation does not match its payload", index+1)
			}
			newMemories = append(newMemories, action.Create)
		case MemoryCurationOperationReplace:
			if action.Replace == nil {
				return nil, memoryCurationInvalidf("action %d operation does not match its payload", index+1)
			}
		case MemoryCurationOperationSupersede:
			if action.Supersede == nil {
				return nil, memoryCurationInvalidf("action %d operation does not match its payload", index+1)
			}
			if len(action.Supersede.Replacements) < 1 || len(action.Supersede.Replacements) > MaxMemoryCurationSupersedeReplacements {
				return nil, memoryCurationInvalidf("action %d supersede replacements must contain 1-%d memories", index+1, MaxMemoryCurationSupersedeReplacements)
			}
		case MemoryCurationOperationRelate:
			if action.Relate == nil {
				return nil, memoryCurationInvalidf("action %d operation does not match its payload", index+1)
			}
		case MemoryCurationOperationProposeFact:
			if action.ProposeFact == nil {
				return nil, memoryCurationInvalidf("action %d operation does not match its payload", index+1)
			}
		default:
			return nil, memoryCurationInvalidf("action %d has unsupported operation %q", index+1, action.Operation)
		}
	}
	for _, memory := range newMemories {
		memory.LocalRef = strings.TrimSpace(memory.LocalRef)
		if !memoryCurationLocalRefPattern.MatchString(memory.LocalRef) {
			return nil, memoryCurationInvalidf("invalid new-memory local_ref")
		}
		if strings.TrimSpace(memory.MemoryID) != "" {
			return nil, memoryCurationInvalidf("drafts may not assign memory_id")
		}
		if _, exists := seenLocalRefs[memory.LocalRef]; exists {
			return nil, memoryCurationInvalidf("duplicate new-memory local_ref %q", memory.LocalRef)
		}
		seenLocalRefs[memory.LocalRef] = struct{}{}
	}
	return newMemories, nil
}

func allocateMemoryCurationIDs(
	newMemories []*MemoryCurationCreateAction,
	options MemoryCurationPlanAcceptOptions,
) (map[string]string, []MemoryCurationPreallocatedMemoryID, error) {
	if len(newMemories) > 0 && options.Allocator == nil && options.PreallocatedMemoryIDs == nil {
		return nil, nil, memoryCurationInvalidf("an allocator or complete preallocated id mapping is required")
	}
	if len(newMemories) == 0 && len(options.PreallocatedMemoryIDs) > 0 {
		return nil, nil, memoryCurationInvalidf("preallocated id mapping contains unknown local refs")
	}

	normalizedSupplied := make(map[string]string, len(options.PreallocatedMemoryIDs))
	for localRef, memoryID := range options.PreallocatedMemoryIDs {
		localRef = strings.TrimSpace(localRef)
		memoryID = strings.TrimSpace(memoryID)
		if !memoryCurationLocalRefPattern.MatchString(localRef) || !validMemoryID(memoryID) {
			return nil, nil, memoryCurationInvalidf("invalid preallocated memory id mapping")
		}
		if _, exists := normalizedSupplied[localRef]; exists {
			return nil, nil, memoryCurationInvalidf("preallocated id mapping has duplicate normalized local refs")
		}
		normalizedSupplied[localRef] = memoryID
	}

	assigned := make(map[string]string, len(newMemories))
	mapping := make([]MemoryCurationPreallocatedMemoryID, 0, len(newMemories))
	seenMemoryIDs := make(map[string]struct{}, len(newMemories))
	for _, memory := range newMemories {
		var memoryID string
		if options.PreallocatedMemoryIDs != nil {
			var ok bool
			memoryID, ok = normalizedSupplied[memory.LocalRef]
			if !ok {
				return nil, nil, memoryCurationInvalidf("preallocated id mapping is missing local ref %q", memory.LocalRef)
			}
			delete(normalizedSupplied, memory.LocalRef)
		} else if options.Allocator != nil {
			var err error
			memoryID, err = options.Allocator.AllocateMemoryID(memory.LocalRef)
			if err != nil {
				return nil, nil, fmt.Errorf("allocate memory id for %q: %w", memory.LocalRef, err)
			}
			memoryID = strings.TrimSpace(memoryID)
		}
		if !validMemoryID(memoryID) {
			return nil, nil, memoryCurationInvalidf("allocator returned an invalid memory id for %q", memory.LocalRef)
		}
		if _, exists := seenMemoryIDs[memoryID]; exists {
			return nil, nil, memoryCurationInvalidf("allocator returned a duplicate memory id")
		}
		seenMemoryIDs[memoryID] = struct{}{}
		assigned[memory.LocalRef] = memoryID
		memory.MemoryID = memoryID
		mapping = append(mapping, MemoryCurationPreallocatedMemoryID{LocalRef: memory.LocalRef, MemoryID: memoryID})
	}
	if len(normalizedSupplied) != 0 {
		return nil, nil, memoryCurationInvalidf("preallocated id mapping contains unknown local refs")
	}
	return assigned, mapping, nil
}

type memoryCurationPlanBudget struct {
	contentBytes   int
	artifactBytes  int
	factValueBytes int
	evidenceRows   int
	relationRows   int
}

func (b memoryCurationPlanBudget) validate() error {
	switch {
	case b.contentBytes > MaxMemoryCurationPlanContentBytes:
		return memoryCurationInvalidf("plan memory content exceeds %d bytes", MaxMemoryCurationPlanContentBytes)
	case b.artifactBytes > MaxMemoryCurationPlanArtifactBytes:
		return memoryCurationInvalidf("plan evidence artifacts exceed %d bytes", MaxMemoryCurationPlanArtifactBytes)
	case b.factValueBytes > MaxMemoryCurationPlanFactValueBytes:
		return memoryCurationInvalidf("plan fact values exceed %d bytes", MaxMemoryCurationPlanFactValueBytes)
	case b.evidenceRows > MaxMemoryCurationPlanEvidenceRows:
		return memoryCurationInvalidf("plan contains more than %d evidence rows", MaxMemoryCurationPlanEvidenceRows)
	case b.relationRows > MaxMemoryCurationPlanRelationRows:
		return memoryCurationInvalidf("plan contains more than %d relation rows", MaxMemoryCurationPlanRelationRows)
	default:
		return nil
	}
}

func normalizeMemoryCurationAction(
	action *MemoryCurationPlanAction,
	assigned map[string]string,
	budget *memoryCurationPlanBudget,
	preview *MemoryCurationImpactPreview,
	seenRelations map[string]struct{},
) error {
	switch action.Operation {
	case MemoryCurationOperationCreate:
		preview.CreateActions++
		preview.NewMemories++
		preview.MemoryVersionWrites++
		return normalizeMemoryCurationNewMemory(action.Create, assigned, budget, preview, seenRelations)
	case MemoryCurationOperationReplace:
		preview.ReplaceActions++
		preview.MemoryVersionWrites++
		preview.ExpectedVersionChecks++
		if err := normalizeMemoryCurationTarget(&action.Replace.Target, assigned); err != nil {
			return err
		}
		action.Replace.Reason = strings.TrimSpace(action.Replace.Reason)
		if len(action.Replace.Reason) > 2048 {
			return memoryCurationInvalidf("replace reason is too large")
		}
		return normalizeMemoryCurationSnapshot(&action.Replace.Snapshot, assigned, false, budget, preview)
	case MemoryCurationOperationSupersede:
		preview.SupersedeActions++
		preview.ExpectedVersionChecks++
		preview.MemoryVersionWrites++ // source's superseded version
		if err := normalizeMemoryCurationTarget(&action.Supersede.Target, assigned); err != nil {
			return err
		}
		action.Supersede.Reason = strings.TrimSpace(action.Supersede.Reason)
		if len(action.Supersede.Reason) > 2048 {
			return memoryCurationInvalidf("supersede reason is too large")
		}
		for index := range action.Supersede.Replacements {
			if err := normalizeMemoryCurationVersionReference(&action.Supersede.Replacements[index], assigned); err != nil {
				return fmt.Errorf("replacement %d: %w", index, err)
			}
			if action.Supersede.Replacements[index].MemoryID == action.Supersede.Target.MemoryID {
				return memoryCurationInvalidf("a memory cannot supersede itself")
			}
		}
		sort.Slice(action.Supersede.Replacements, func(i, j int) bool {
			a, b := action.Supersede.Replacements[i], action.Supersede.Replacements[j]
			if a.MemoryID != b.MemoryID {
				return a.MemoryID < b.MemoryID
			}
			return a.Version < b.Version
		})
		unique := action.Supersede.Replacements[:0]
		for _, replacement := range action.Supersede.Replacements {
			if len(unique) > 0 && sameMemoryCurationVersion(unique[len(unique)-1], replacement) {
				continue
			}
			unique = append(unique, replacement)
		}
		action.Supersede.Replacements = unique
		preview.RelationRows += len(unique)
		budget.relationRows += len(unique)
		return nil
	case MemoryCurationOperationRelate:
		preview.RelateActions++
		preview.RelationRows++
		budget.relationRows++
		relation := action.Relate
		relation.RelationType = strings.TrimSpace(relation.RelationType)
		if !validMemoryCurationRelationType(relation.RelationType) {
			return memoryCurationInvalidf("invalid generic relation type %q", relation.RelationType)
		}
		if err := normalizeMemoryCurationVersionReference(&relation.From, assigned); err != nil {
			return fmt.Errorf("from: %w", err)
		}
		if err := normalizeMemoryCurationVersionReference(&relation.To, assigned); err != nil {
			return fmt.Errorf("to: %w", err)
		}
		if sameMemoryCurationVersion(relation.From, relation.To) {
			return memoryCurationInvalidf("a relation may not point to itself")
		}
		key := memoryCurationRelationKey(relation.RelationType, relation.From, relation.To)
		if _, duplicate := seenRelations[key]; duplicate {
			return memoryCurationInvalidf("duplicate relation in plan")
		}
		seenRelations[key] = struct{}{}
		return nil
	case MemoryCurationOperationProposeFact:
		preview.ProposeFactActions++
		preview.FactCandidates++
		return normalizeMemoryCurationFactProposal(action.ProposeFact, assigned, budget, preview)
	default:
		return memoryCurationInvalidf("unsupported operation %q", action.Operation)
	}
}

func normalizeMemoryCurationNewMemory(
	memory *MemoryCurationCreateAction,
	assigned map[string]string,
	budget *memoryCurationPlanBudget,
	preview *MemoryCurationImpactPreview,
	seenRelations map[string]struct{},
) error {
	if assigned[memory.LocalRef] != memory.MemoryID {
		return memoryCurationInvalidf("new-memory id allocation changed during normalization")
	}
	if err := normalizeMemoryCurationSnapshot(&memory.Snapshot, assigned, true, budget, preview); err != nil {
		return err
	}
	if len(memory.Relations) > MaxMemoryCurationRelationsPerNewMemory {
		return memoryCurationInvalidf("new memory contains more than %d lineage relations", MaxMemoryCurationRelationsPerNewMemory)
	}
	for index := range memory.Relations {
		relation := &memory.Relations[index]
		relation.RelationType = strings.TrimSpace(relation.RelationType)
		if !validMemoryCurationRelationType(relation.RelationType) {
			return memoryCurationInvalidf("invalid generic relation type %q", relation.RelationType)
		}
		if err := normalizeMemoryCurationVersionReference(&relation.To, assigned); err != nil {
			return fmt.Errorf("lineage relation %d: %w", index, err)
		}
		if relation.To.MemoryID == memory.MemoryID && relation.To.Version == 1 {
			return memoryCurationInvalidf("a lineage relation may not point to itself")
		}
	}
	sort.Slice(memory.Relations, func(i, j int) bool {
		a, b := memory.Relations[i], memory.Relations[j]
		if a.RelationType != b.RelationType {
			return a.RelationType < b.RelationType
		}
		if a.To.MemoryID != b.To.MemoryID {
			return a.To.MemoryID < b.To.MemoryID
		}
		return a.To.Version < b.To.Version
	})
	unique := memory.Relations[:0]
	for _, relation := range memory.Relations {
		from := MemoryCurationVersionReference{MemoryID: memory.MemoryID, Version: 1}
		key := memoryCurationRelationKey(relation.RelationType, from, relation.To)
		if len(unique) > 0 {
			previous := unique[len(unique)-1]
			if previous.RelationType == relation.RelationType && sameMemoryCurationVersion(previous.To, relation.To) {
				continue
			}
		}
		if _, duplicate := seenRelations[key]; duplicate {
			return memoryCurationInvalidf("duplicate relation in plan")
		}
		seenRelations[key] = struct{}{}
		unique = append(unique, relation)
	}
	memory.Relations = unique
	budget.relationRows += len(unique)
	preview.RelationRows += len(unique)
	return nil
}

func normalizeMemoryCurationSnapshot(
	snapshot *MemoryCurationMemorySnapshot,
	assigned map[string]string,
	requireEvidence bool,
	budget *memoryCurationPlanBudget,
	preview *MemoryCurationImpactPreview,
) error {
	if snapshot.ContentEncoding == "" {
		snapshot.ContentEncoding = "plain"
	} else {
		snapshot.ContentEncoding = strings.TrimSpace(snapshot.ContentEncoding)
	}
	if snapshot.Kind == "" {
		snapshot.Kind = "episodic"
	} else {
		snapshot.Kind = strings.TrimSpace(snapshot.Kind)
	}
	var err error
	snapshot.Tags, err = normalizeMemoryStrings(snapshot.Tags, 64, 128, "tag")
	if err != nil {
		return err
	}
	snapshot.Links, err = normalizeMemoryStrings(snapshot.Links, 256, 2048, "link")
	if err != nil {
		return err
	}
	if snapshot.Salience == nil {
		value := 0.5
		snapshot.Salience = &value
	}
	if math.IsNaN(*snapshot.Salience) || math.IsInf(*snapshot.Salience, 0) {
		return memoryCurationInvalidf("salience must be finite")
	}
	if snapshot.OccurredFrom != nil {
		value := snapshot.OccurredFrom.UTC()
		snapshot.OccurredFrom = &value
	}
	if snapshot.OccurredUntil != nil {
		value := snapshot.OccurredUntil.UTC()
		snapshot.OccurredUntil = &value
	}
	if err := validateMemorySnapshot(Memory{
		Content: snapshot.Content, ContentEncoding: snapshot.ContentEncoding,
		Kind: snapshot.Kind, Salience: *snapshot.Salience,
		OccurredFrom: snapshot.OccurredFrom, OccurredUntil: snapshot.OccurredUntil,
	}); err != nil {
		return err
	}
	if requireEvidence && len(snapshot.Evidence) == 0 {
		return memoryCurationInvalidf("new memories require at least one evidence row")
	}
	if len(snapshot.Evidence) > 32 {
		return memoryCurationInvalidf("a memory snapshot may contain at most 32 evidence rows")
	}
	for index := range snapshot.Evidence {
		if err := normalizeMemoryCurationEvidence(&snapshot.Evidence[index], assigned); err != nil {
			return fmt.Errorf("evidence %d: %w", index, err)
		}
		budget.artifactBytes += len(snapshot.Evidence[index].ArtifactExcerpt)
	}
	budget.contentBytes += len(snapshot.Content)
	budget.evidenceRows += len(snapshot.Evidence)
	preview.EvidenceRows += len(snapshot.Evidence)
	return nil
}

func normalizeMemoryCurationTarget(target *MemoryCurationTargetReference, assigned map[string]string) error {
	target.MemoryID = strings.TrimSpace(target.MemoryID)
	target.LocalRef = strings.TrimSpace(target.LocalRef)
	if target.ExpectedVersion < 1 || (target.MemoryID == "") == (target.LocalRef == "") {
		return memoryCurationInvalidf("target requires exactly one memory_id or local_ref and a positive expected_version")
	}
	if target.LocalRef != "" {
		memoryID, ok := assigned[target.LocalRef]
		if !ok {
			return memoryCurationInvalidf("target names unknown local ref %q", target.LocalRef)
		}
		if target.ExpectedVersion != 1 {
			return memoryCurationInvalidf("a local target must expect version 1")
		}
		target.MemoryID = memoryID
		target.LocalRef = ""
	} else if !validMemoryID(target.MemoryID) {
		return memoryCurationInvalidf("target contains an invalid memory id")
	}
	return nil
}

func normalizeMemoryCurationVersionReference(reference *MemoryCurationVersionReference, assigned map[string]string) error {
	reference.MemoryID = strings.TrimSpace(reference.MemoryID)
	reference.LocalRef = strings.TrimSpace(reference.LocalRef)
	if reference.Version < 1 || (reference.MemoryID == "") == (reference.LocalRef == "") {
		return memoryCurationInvalidf("version reference requires exactly one memory_id or local_ref and a positive version")
	}
	if reference.LocalRef != "" {
		memoryID, ok := assigned[reference.LocalRef]
		if !ok {
			return memoryCurationInvalidf("version reference names unknown local ref %q", reference.LocalRef)
		}
		if reference.Version != 1 {
			return memoryCurationInvalidf("a local version reference must name version 1")
		}
		reference.MemoryID = memoryID
		reference.LocalRef = ""
	} else if !validMemoryID(reference.MemoryID) {
		return memoryCurationInvalidf("version reference contains an invalid memory id")
	}
	return nil
}

func normalizeMemoryCurationEvidence(evidence *MemoryCurationEvidence, assigned map[string]string) error {
	evidence.InputEvidenceID = strings.TrimSpace(evidence.InputEvidenceID)
	if evidence.InputEvidenceID != "" && !validCurationID(evidence.InputEvidenceID, "mev") {
		return memoryCurationInvalidf("input_evidence_id is invalid")
	}
	var sourceMemoryID string
	var sourceMemoryVersion int64
	if evidence.SourceMemory != nil {
		if err := normalizeMemoryCurationVersionReference(evidence.SourceMemory, assigned); err != nil {
			return fmt.Errorf("source_memory: %w", err)
		}
		sourceMemoryID = evidence.SourceMemory.MemoryID
		sourceMemoryVersion = evidence.SourceMemory.Version
	}
	normalized, err := normalizeMemoryEvidenceInput(MemoryEvidenceInput{
		Type: evidence.Type, Role: evidence.Role,
		ResolutionState: evidence.ResolutionState,
		ExternalLocator: evidence.ExternalLocator, ResolvedKind: evidence.ResolvedKind,
		SourceTranscriptID:  evidence.SourceTranscriptID,
		SourceSequenceFrom:  evidence.SourceSequenceFrom,
		SourceSequenceUntil: evidence.SourceSequenceUntil,
		SourceMemoryID:      sourceMemoryID, SourceMemoryVersion: sourceMemoryVersion,
		SourceMessageID:     evidence.SourceMessageID,
		SourceImportLocator: evidence.SourceImportLocator,
		ArtifactExcerpt:     append([]byte(nil), evidence.ArtifactExcerpt...),
		ArtifactSensitive:   evidence.ArtifactSensitive,
		TerminalReasonCode:  evidence.TerminalReasonCode,
		SourceDigest:        evidence.SourceDigest,
	})
	if err != nil {
		return err
	}
	if normalized.ResolutionState == MemoryEvidenceUnresolvable {
		return memoryCurationInvalidf("unresolvable evidence is only valid when resolving a pending row")
	}
	evidence.Type = normalized.Type
	evidence.Role = normalized.Role
	evidence.ResolutionState = normalized.ResolutionState
	evidence.ExternalLocator = normalized.ExternalLocator
	evidence.ResolvedKind = normalized.ResolvedKind
	evidence.SourceTranscriptID = normalized.SourceTranscriptID
	evidence.SourceSequenceFrom = normalized.SourceSequenceFrom
	evidence.SourceSequenceUntil = normalized.SourceSequenceUntil
	evidence.SourceMessageID = normalized.SourceMessageID
	evidence.SourceImportLocator = normalized.SourceImportLocator
	evidence.ArtifactExcerpt = append([]byte(nil), normalized.ArtifactExcerpt...)
	evidence.ArtifactSensitive = normalized.ArtifactSensitive
	evidence.TerminalReasonCode = normalized.TerminalReasonCode
	evidence.SourceDigest = normalized.SourceDigest
	if normalized.SourceMemoryID == "" {
		evidence.SourceMemory = nil
	} else {
		evidence.SourceMemory = &MemoryCurationVersionReference{
			MemoryID: normalized.SourceMemoryID, Version: normalized.SourceMemoryVersion,
		}
	}
	return nil
}

func normalizeMemoryCurationFactProposal(
	proposal *MemoryCurationProposeFactAction,
	assigned map[string]string,
	budget *memoryCurationPlanBudget,
	preview *MemoryCurationImpactPreview,
) error {
	proposal.Reason = strings.TrimSpace(proposal.Reason)
	if len(proposal.Reason) > 1024 {
		return memoryCurationInvalidf("fact proposal reason is too large")
	}
	if proposal.Confidence != nil && (math.IsNaN(*proposal.Confidence) || math.IsInf(*proposal.Confidence, 0)) {
		return memoryCurationInvalidf("fact proposal confidence must be finite")
	}
	if proposal.ValidFrom != nil {
		value := proposal.ValidFrom.UTC()
		proposal.ValidFrom = &value
	}
	if proposal.ValidUntil != nil {
		value := proposal.ValidUntil.UTC()
		proposal.ValidUntil = &value
	}
	input, err := normalizeSetFactInputAt(SetFactInput{
		Subject: proposal.Subject, Predicate: proposal.Predicate,
		ValueType: proposal.ValueType, Value: append(json.RawMessage(nil), proposal.Value...),
		Recurrence: proposal.Recurrence, Cardinality: proposal.Cardinality,
		Sensitive: proposal.Sensitive, SourceKind: FactSourceInference,
		Confidence: proposal.Confidence, ValidFrom: proposal.ValidFrom,
		ValidUntil: proposal.ValidUntil,
	}, time.Unix(0, 0).UTC())
	if err != nil {
		return fmt.Errorf("%w: invalid fact proposal: %v", ErrMemoryInputInvalid, err)
	}
	if input.Confidence == nil {
		value := 0.5
		input.Confidence = &value
	}
	proposal.Subject = input.Subject
	proposal.Predicate = input.Predicate
	proposal.ValueType = input.ValueType
	canonicalValue, err := canonicalizeMemoryCurationRawJSON(input.Value)
	if err != nil {
		return fmt.Errorf("%w: invalid fact proposal value: %v", ErrMemoryInputInvalid, err)
	}
	proposal.Value = canonicalValue
	proposal.Recurrence = input.Recurrence
	proposal.Cardinality = input.Cardinality
	proposal.Sensitive = input.Sensitive
	proposal.Confidence = input.Confidence
	proposal.ValidFrom = input.ValidFrom
	proposal.ValidUntil = input.ValidUntil
	if len(proposal.Evidence) < 1 || len(proposal.Evidence) > 32 {
		return memoryCurationInvalidf("fact proposal evidence must contain 1-32 rows")
	}
	for index := range proposal.Evidence {
		if err := normalizeMemoryCurationEvidence(&proposal.Evidence[index], assigned); err != nil {
			return fmt.Errorf("fact evidence %d: %w", index, err)
		}
		budget.artifactBytes += len(proposal.Evidence[index].ArtifactExcerpt)
	}
	budget.factValueBytes += len(proposal.Value)
	budget.evidenceRows += len(proposal.Evidence)
	preview.EvidenceRows += len(proposal.Evidence)
	return nil
}

func validMemoryCurationRelationType(value string) bool {
	switch value {
	case MemoryCurationRelationDerivedFrom,
		MemoryCurationRelationSummarizes,
		MemoryCurationRelationMergedFrom,
		MemoryCurationRelationSplitFrom,
		MemoryCurationRelationConflictsWith:
		return true
	default:
		return false
	}
}

func sameMemoryCurationVersion(a, b MemoryCurationVersionReference) bool {
	return a.MemoryID == b.MemoryID && a.Version == b.Version
}

func memoryCurationRelationKey(
	relationType string,
	from, to MemoryCurationVersionReference,
) string {
	return relationType + "\x00" + from.MemoryID + "\x00" + strconv.FormatInt(from.Version, 10) +
		"\x00" + to.MemoryID + "\x00" + strconv.FormatInt(to.Version, 10)
}

func memoryCurationInvalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMemoryInputInvalid, fmt.Sprintf(format, args...))
}

// canonicalMemoryCurationJSON implements RFC 8785 ordering and primitive
// serialization for the deliberately restricted accepted-plan schema. JSON
// numbers are interpreted as IEEE-754 binary64, as required by I-JSON/JCS.
func canonicalMemoryCurationJSON(value any) ([]byte, error) {
	if err := validateMemoryCurationUTF8(reflect.ValueOf(value)); err != nil {
		return nil, err
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, memoryCurationInvalidf("encode canonical plan: %v", err)
	}
	raw := bytes.TrimSpace(encoded.Bytes())
	if err := rejectDuplicateJSONNames(raw); err != nil {
		return nil, memoryCurationInvalidf("canonical plan contains invalid JSON: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, memoryCurationInvalidf("decode canonical plan: %v", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, memoryCurationInvalidf("decode canonical plan: %v", err)
	}
	var canonical bytes.Buffer
	if err := writeCanonicalJSON(&canonical, decoded); err != nil {
		return nil, err
	}
	return canonical.Bytes(), nil
}

func canonicalizeMemoryCurationRawJSON(raw json.RawMessage) (json.RawMessage, error) {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, memoryCurationInvalidf("fact value must be valid UTF-8 JSON")
	}
	if err := rejectDuplicateJSONNames(raw); err != nil {
		return nil, memoryCurationInvalidf("fact value contains invalid JSON: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var canonical bytes.Buffer
	if err := writeCanonicalJSON(&canonical, value); err != nil {
		return nil, err
	}
	return json.RawMessage(append([]byte(nil), canonical.Bytes()...)), nil
}

func writeCanonicalJSON(out *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if value {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		if !utf8.ValidString(value) {
			return memoryCurationInvalidf("canonical JSON contains invalid UTF-8")
		}
		writeCanonicalJSONString(out, value)
	case json.Number:
		number, err := formatCanonicalJSONNumber(value)
		if err != nil {
			return err
		}
		out.WriteString(number)
	case []any:
		out.WriteByte('[')
		for index, item := range value {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonicalJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			if !utf8.ValidString(key) {
				return memoryCurationInvalidf("canonical JSON contains invalid UTF-8 object key")
			}
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			writeCanonicalJSONString(out, key)
			out.WriteByte(':')
			if err := writeCanonicalJSON(out, value[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return memoryCurationInvalidf("canonical JSON contains unsupported value type %T", value)
	}
	return nil
}

func writeCanonicalJSONString(out *bytes.Buffer, value string) {
	const hexDigits = "0123456789abcdef"
	out.WriteByte('"')
	start := 0
	for index := 0; index < len(value); index++ {
		character := value[index]
		var escape string
		switch character {
		case '"':
			escape = `\"`
		case '\\':
			escape = `\\`
		case '\b':
			escape = `\b`
		case '\t':
			escape = `\t`
		case '\n':
			escape = `\n`
		case '\f':
			escape = `\f`
		case '\r':
			escape = `\r`
		default:
			if character < 0x20 {
				if start < index {
					out.WriteString(value[start:index])
				}
				out.WriteString(`\u00`)
				out.WriteByte(hexDigits[character>>4])
				out.WriteByte(hexDigits[character&0x0f])
				start = index + 1
			}
			continue
		}
		if start < index {
			out.WriteString(value[start:index])
		}
		out.WriteString(escape)
		start = index + 1
	}
	if start < len(value) {
		out.WriteString(value[start:])
	}
	out.WriteByte('"')
}

func formatCanonicalJSONNumber(value json.Number) (string, error) {
	number, err := strconv.ParseFloat(string(value), 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return "", memoryCurationInvalidf("canonical JSON number %q is not finite binary64", value)
	}
	if number == 0 {
		return "0", nil
	}
	negative := number < 0
	if negative {
		number = -number
	}
	scientific := strconv.FormatFloat(number, 'e', -1, 64)
	exponentOffset := strings.LastIndexByte(scientific, 'e')
	if exponentOffset < 1 {
		return "", memoryCurationInvalidf("cannot canonicalize JSON number %q", value)
	}
	coefficient := scientific[:exponentOffset]
	exponent, err := strconv.Atoi(scientific[exponentOffset+1:])
	if err != nil {
		return "", memoryCurationInvalidf("cannot canonicalize JSON number %q", value)
	}
	digits := strings.ReplaceAll(coefficient, ".", "")
	decimalPosition := exponent + 1
	var result string
	switch {
	case decimalPosition > 0 && decimalPosition <= 21:
		if decimalPosition >= len(digits) {
			result = digits + strings.Repeat("0", decimalPosition-len(digits))
		} else {
			result = digits[:decimalPosition] + "." + digits[decimalPosition:]
		}
	case decimalPosition <= 0 && decimalPosition > -6:
		result = "0." + strings.Repeat("0", -decimalPosition) + digits
	default:
		result = digits[:1]
		if len(digits) > 1 {
			result += "." + digits[1:]
		}
		if exponent >= 0 {
			result += "e+" + strconv.Itoa(exponent)
		} else {
			result += "e" + strconv.Itoa(exponent)
		}
	}
	if negative {
		result = "-" + result
	}
	return result, nil
}

func lessUTF16(a, b string) bool {
	aUnits := utf16.Encode([]rune(a))
	bUnits := utf16.Encode([]rune(b))
	for index := 0; index < len(aUnits) && index < len(bUnits); index++ {
		if aUnits[index] != bUnits[index] {
			return aUnits[index] < bUnits[index]
		}
	}
	return len(aUnits) < len(bUnits)
}

func validateMemoryCurationUTF8(value reflect.Value) error {
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		return validateMemoryCurationUTF8(value.Elem())
	}
	if value.Type() == reflect.TypeOf(json.RawMessage{}) {
		raw := value.Bytes()
		if !utf8.Valid(raw) || !json.Valid(raw) {
			return memoryCurationInvalidf("fact value must be valid UTF-8 JSON")
		}
		return nil
	}
	switch value.Kind() {
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return memoryCurationInvalidf("plan strings must be valid UTF-8")
		}
	case reflect.Struct:
		typeOfValue := value.Type()
		for index := 0; index < value.NumField(); index++ {
			if typeOfValue.Field(index).PkgPath != "" {
				continue
			}
			if err := validateMemoryCurationUTF8(value.Field(index)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for index := 0; index < value.Len(); index++ {
			if err := validateMemoryCurationUTF8(value.Index(index)); err != nil {
				return err
			}
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			if err := validateMemoryCurationUTF8(iterator.Key()); err != nil {
				return err
			}
			if err := validateMemoryCurationUTF8(iterator.Value()); err != nil {
				return err
			}
		}
	}
	return nil
}

func rejectDuplicateJSONNames(raw []byte) error {
	if err := rejectUnpairedJSONSurrogates(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeUniqueJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

// encoding/json deliberately replaces lone UTF-16 surrogate escapes with the
// Unicode replacement character. JCS instead requires Unicode scalar values,
// so reject the lossy form before decoding.
func rejectUnpairedJSONSurrogates(raw []byte) error {
	for index := 0; index < len(raw); index++ {
		if raw[index] != '"' {
			continue
		}
		index++
		for index < len(raw) && raw[index] != '"' {
			if raw[index] != '\\' {
				index++
				continue
			}
			if index+1 >= len(raw) {
				return errors.New("unterminated JSON escape")
			}
			if raw[index+1] != 'u' {
				index += 2
				continue
			}
			codeUnit, ok := parseJSONHexCodeUnit(raw, index+2)
			if !ok {
				return errors.New("invalid JSON Unicode escape")
			}
			switch {
			case codeUnit >= 0xd800 && codeUnit <= 0xdbff:
				if index+12 > len(raw) || raw[index+6] != '\\' || raw[index+7] != 'u' {
					return errors.New("unpaired high surrogate escape")
				}
				low, validLow := parseJSONHexCodeUnit(raw, index+8)
				if !validLow || low < 0xdc00 || low > 0xdfff {
					return errors.New("unpaired high surrogate escape")
				}
				index += 12
			case codeUnit >= 0xdc00 && codeUnit <= 0xdfff:
				return errors.New("unpaired low surrogate escape")
			default:
				index += 6
			}
		}
	}
	return nil
}

func parseJSONHexCodeUnit(raw []byte, offset int) (uint16, bool) {
	if offset < 0 || offset+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, character := range raw[offset : offset+4] {
		value <<= 4
		switch {
		case character >= '0' && character <= '9':
			value |= uint16(character - '0')
		case character >= 'a' && character <= 'f':
			value |= uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			value |= uint16(character-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func consumeUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object member name is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object member %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("object did not terminate")
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("array did not terminate")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("unexpected trailing JSON value")
}

// regexpMustCompile is kept local so this file does not add mutable package
// initialization or an external schema dependency.
func regexpMustCompile(expression string) *regexp.Regexp {
	return regexp.MustCompile(expression)
}
