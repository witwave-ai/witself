package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAcceptMemoryCurationPlanNormalizesResolvesHashesAndPreviews(t *testing.T) {
	draft := completeMemoryCurationPlanDraft()
	accepted, err := AcceptMemoryCurationPlan(draft, MemoryCurationPlanAcceptOptions{
		PlanRevision: 7,
		Allocator: MemoryCurationMemoryIDAllocatorFunc(func(localRef string) (string, error) {
			if localRef != "summary" {
				t.Fatalf("allocator localRef = %q, want summary", localRef)
			}
			return "mem_plan_summary", nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if draft.Actions[0].Create.MemoryID != "" {
		t.Fatal("acceptance mutated caller's draft")
	}
	if accepted.Plan.Schema != MemoryCurationPlanSchemaV1 || accepted.Plan.PlanRevision != 7 {
		t.Fatalf("accepted plan header = %#v", accepted.Plan)
	}
	if got, want := accepted.PreallocatedMemoryIDs, []MemoryCurationPreallocatedMemoryID{{
		LocalRef: "summary", MemoryID: "mem_plan_summary",
	}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("preallocated ids = %#v, want %#v", got, want)
	}
	create := accepted.Plan.Actions[0].Create
	if create.LocalRef != "summary" || create.MemoryID != "mem_plan_summary" {
		t.Fatalf("accepted create = %#v", create)
	}
	if got, want := create.Snapshot.Tags, []string{"alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized tags = %#v, want %#v", got, want)
	}
	if got, want := create.Snapshot.Links, []string{"https://a.example", "https://z.example"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized links = %#v, want %#v", got, want)
	}
	for _, replacement := range accepted.Plan.Actions[2].Supersede.Replacements {
		if replacement.LocalRef != "" {
			t.Fatalf("supersede replacement retained local ref: %#v", replacement)
		}
	}
	if accepted.Plan.Actions[3].Relate.From.MemoryID != "mem_plan_summary" ||
		accepted.Plan.Actions[3].Relate.From.LocalRef != "" {
		t.Fatalf("relate reference was not resolved: %#v", accepted.Plan.Actions[3].Relate.From)
	}
	factEvidence := accepted.Plan.Actions[4].ProposeFact.Evidence[0]
	if factEvidence.SourceMemory == nil || factEvidence.SourceMemory.MemoryID != "mem_plan_summary" ||
		factEvidence.SourceMemory.LocalRef != "" {
		t.Fatalf("fact evidence reference was not resolved: %#v", factEvidence.SourceMemory)
	}
	if got, want := string(accepted.Plan.Actions[4].ProposeFact.Value), `{"a":"x","z":1}`; got != want {
		t.Fatalf("canonical fact value = %s, want %s", got, want)
	}
	wantPreview := MemoryCurationImpactPreview{
		ActionCount: 5, CreateActions: 1, ReplaceActions: 1,
		SupersedeActions: 1, RelateActions: 1, ProposeFactActions: 1,
		NewMemories: 1, MemoryVersionWrites: 3, EvidenceRows: 2,
		RelationRows: 4, ExpectedVersionChecks: 2, FactCandidates: 1,
	}
	if !reflect.DeepEqual(accepted.Preview, wantPreview) {
		t.Fatalf("preview = %#v, want %#v", accepted.Preview, wantPreview)
	}
	canonical := accepted.CanonicalBytes()
	if !json.Valid(canonical) || bytes.Contains(canonical, []byte("draft_revision")) ||
		bytes.Contains(canonical, []byte(`"local_ref":"summary","version"`)) {
		t.Fatalf("unexpected canonical JSON: %s", canonical)
	}
	sum := sha256.Sum256(canonical)
	if accepted.PlanHash != hex.EncodeToString(sum[:]) || !isSHA256Hex(accepted.PlanHash) {
		t.Fatalf("plan hash = %q", accepted.PlanHash)
	}
	mutated := accepted.CanonicalBytes()
	mutated[0] = '['
	if bytes.Equal(mutated, accepted.CanonicalBytes()) {
		t.Fatal("CanonicalBytes did not return a defensive copy")
	}

	// Draft revision, input set ordering, whitespace, and allocator mechanism
	// do not alter the accepted plan when the immutable plan revision and ids
	// are the same.
	retry := completeMemoryCurationPlanDraft()
	retry.DraftRevision = 99
	retry.Actions[0].Create.Snapshot.Tags = []string{"alpha", "zeta"}
	retry.Actions[0].Create.Snapshot.Links = []string{"https://a.example", "https://z.example"}
	reaccepted, err := AcceptMemoryCurationPlan(retry, MemoryCurationPlanAcceptOptions{
		PlanRevision:          7,
		PreallocatedMemoryIDs: map[string]string{"summary": "mem_plan_summary"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.PlanHash != reaccepted.PlanHash || !bytes.Equal(accepted.CanonicalBytes(), reaccepted.CanonicalBytes()) {
		t.Fatalf("equivalent normalized plans differ:\n%s\n%s", accepted.CanonicalBytes(), reaccepted.CanonicalBytes())
	}
	revisionEight, err := AcceptMemoryCurationPlan(retry, MemoryCurationPlanAcceptOptions{
		PlanRevision: 8, PreallocatedMemoryIDs: map[string]string{"summary": "mem_plan_summary"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if revisionEight.PlanHash == accepted.PlanHash {
		t.Fatal("plan revision was not bound into plan hash")
	}
}

func TestMemoryCurationPlanEmptyActionsHaveOneCanonicalForm(t *testing.T) {
	for _, actions := range [][]MemoryCurationPlanAction{nil, {}} {
		accepted, err := AcceptMemoryCurationPlan(MemoryCurationPlanDraft{
			Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1, Actions: actions,
		}, MemoryCurationPlanAcceptOptions{PlanRevision: 1})
		if err != nil {
			t.Fatal(err)
		}
		const want = `{"actions":[],"plan_revision":1,"schema":"witself.memory-plan.v1"}`
		if got := string(accepted.CanonicalBytes()); got != want {
			t.Fatalf("canonical empty plan = %s, want %s", got, want)
		}
		const wantHash = "26a28c18319d65dffe876dc925c7a53c98e3ee3f8d8a790f91c7cf991d5e7a54"
		if accepted.PlanHash != wantHash {
			t.Fatalf("empty plan hash = %s, want %s", accepted.PlanHash, wantHash)
		}
	}
}

func TestDecodeMemoryCurationPlanDraftIsStrict(t *testing.T) {
	valid := `{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}`
	if _, err := DecodeMemoryCurationPlanDraft([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  []byte
	}{
		{"empty", nil},
		{"unknown top-level", []byte(`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[],"created_at":"2025-01-01T00:00:00Z"}`)},
		{"unknown action", []byte(`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[{"ordinal":1,"operation":"create","created_at":"x","create":{}}]}`)},
		{"origin spoof", []byte(`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[{"ordinal":1,"operation":"create","create":{"local_ref":"x","origin":"self","snapshot":{}}}]}`)},
		{"client timestamp", []byte(`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[{"ordinal":1,"operation":"propose_fact","propose_fact":{"predicate":"x","value":"y","observed_at":"2025-01-01T00:00:00Z","evidence":[]}}]}`)},
		{"duplicate name", []byte(`{"schema":"witself.memory-plan.v1","schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}`)},
		{"nested duplicate name", []byte(`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[{"ordinal":1,"ordinal":1,"operation":"relate","relate":{}}]}`)},
		{"trailing", []byte(valid + `{}`)},
		{"invalid utf8", append([]byte(valid[:len(valid)-1]), 0xff, '}')},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeMemoryCurationPlanDraft(test.raw); !errors.Is(err, ErrMemoryInputInvalid) {
				t.Fatalf("error = %v, want ErrMemoryInputInvalid", err)
			}
		})
	}
}

func TestAcceptMemoryCurationPlanRejectsInvalidEnvelopesAndAllocation(t *testing.T) {
	validCreate := func(localRef string) MemoryCurationPlanAction {
		return MemoryCurationPlanAction{Ordinal: 1, Operation: MemoryCurationOperationCreate,
			Create: &MemoryCurationCreateAction{LocalRef: localRef, Snapshot: validCurationSnapshot("content")}}
	}
	errorAllocator := errors.New("allocator unavailable")
	tests := []struct {
		name    string
		draft   MemoryCurationPlanDraft
		options MemoryCurationPlanAcceptOptions
	}{
		{"schema", MemoryCurationPlanDraft{Schema: "v2", DraftRevision: 1}, MemoryCurationPlanAcceptOptions{PlanRevision: 1}},
		{"draft revision", MemoryCurationPlanDraft{Schema: MemoryCurationPlanSchemaV1}, MemoryCurationPlanAcceptOptions{PlanRevision: 1}},
		{"plan revision", validCurationDraft(validCreate("new")), MemoryCurationPlanAcceptOptions{}},
		{"ordinal zero", validCurationDraft(func() MemoryCurationPlanAction { a := validCreate("new"); a.Ordinal = 0; return a }()), MemoryCurationPlanAcceptOptions{PlanRevision: 1}},
		{"operation payload mismatch", validCurationDraft(MemoryCurationPlanAction{Ordinal: 1, Operation: MemoryCurationOperationReplace, Create: validCreate("new").Create}), MemoryCurationPlanAcceptOptions{PlanRevision: 1}},
		{"two payloads", validCurationDraft(func() MemoryCurationPlanAction {
			a := validCreate("new")
			a.Relate = &MemoryCurationRelateAction{}
			return a
		}()), MemoryCurationPlanAcceptOptions{PlanRevision: 1}},
		{"unsupported operation", validCurationDraft(MemoryCurationPlanAction{Ordinal: 1, Operation: "delete", Relate: &MemoryCurationRelateAction{}}), MemoryCurationPlanAcceptOptions{PlanRevision: 1}},
		{"invalid local ref", validCurationDraft(validCreate("BAD REF")), MemoryCurationPlanAcceptOptions{PlanRevision: 1}},
		{"client memory id", validCurationDraft(func() MemoryCurationPlanAction { a := validCreate("new"); a.Create.MemoryID = "mem_spoof"; return a }()), MemoryCurationPlanAcceptOptions{PlanRevision: 1}},
		{"missing id source", validCurationDraft(validCreate("new")), MemoryCurationPlanAcceptOptions{PlanRevision: 1}},
		{"both id sources", validCurationDraft(validCreate("new")), MemoryCurationPlanAcceptOptions{PlanRevision: 1, PreallocatedMemoryIDs: map[string]string{}, Allocator: fixedCurationAllocator("mem_new")}},
		{"mapping missing", validCurationDraft(validCreate("new")), MemoryCurationPlanAcceptOptions{PlanRevision: 1, PreallocatedMemoryIDs: map[string]string{}}},
		{"mapping extra", validCurationDraft(validCreate("new")), MemoryCurationPlanAcceptOptions{PlanRevision: 1, PreallocatedMemoryIDs: map[string]string{"new": "mem_new", "other": "mem_other"}}},
		{"mapping invalid id", validCurationDraft(validCreate("new")), MemoryCurationPlanAcceptOptions{PlanRevision: 1, PreallocatedMemoryIDs: map[string]string{"new": "bad"}}},
		{"allocator invalid id", validCurationDraft(validCreate("new")), MemoryCurationPlanAcceptOptions{PlanRevision: 1, Allocator: fixedCurationAllocator("bad")}},
		{"allocator error", validCurationDraft(validCreate("new")), MemoryCurationPlanAcceptOptions{PlanRevision: 1, Allocator: MemoryCurationMemoryIDAllocatorFunc(func(string) (string, error) { return "", errorAllocator })}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := AcceptMemoryCurationPlan(test.draft, test.options)
			if test.name == "allocator error" {
				if !errors.Is(err, errorAllocator) {
					t.Fatalf("error = %v, want allocator error", err)
				}
				return
			}
			if !errors.Is(err, ErrMemoryInputInvalid) {
				t.Fatalf("error = %v, want ErrMemoryInputInvalid", err)
			}
		})
	}

	actions := make([]MemoryCurationPlanAction, MaxMemoryCurationPlanActions+1)
	for index := range actions {
		actions[index] = MemoryCurationPlanAction{Ordinal: int64(index + 1), Operation: MemoryCurationOperationRelate, Relate: &MemoryCurationRelateAction{}}
	}
	if _, err := AcceptMemoryCurationPlan(MemoryCurationPlanDraft{
		Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1, Actions: actions,
	}, MemoryCurationPlanAcceptOptions{PlanRevision: 1}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("too many actions error = %v", err)
	}
}

func TestMemoryCurationPlanRelationsAndSupersedeSets(t *testing.T) {
	allowed := []string{
		MemoryCurationRelationDerivedFrom,
		MemoryCurationRelationSummarizes,
		MemoryCurationRelationMergedFrom,
		MemoryCurationRelationSplitFrom,
		MemoryCurationRelationConflictsWith,
	}
	for _, relationType := range allowed {
		t.Run(relationType, func(t *testing.T) {
			draft := validCurationDraft(MemoryCurationPlanAction{
				Ordinal: 1, Operation: MemoryCurationOperationRelate,
				Relate: &MemoryCurationRelateAction{RelationType: relationType,
					From: MemoryCurationVersionReference{MemoryID: "mem_from", Version: 1},
					To:   MemoryCurationVersionReference{MemoryID: "mem_to", Version: 2}},
			})
			if _, err := AcceptMemoryCurationPlan(draft, MemoryCurationPlanAcceptOptions{PlanRevision: 1}); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, relationType := range []string{"supersedes", "related", ""} {
		t.Run("reject_"+relationType, func(t *testing.T) {
			draft := validCurationDraft(MemoryCurationPlanAction{
				Ordinal: 1, Operation: MemoryCurationOperationRelate,
				Relate: &MemoryCurationRelateAction{RelationType: relationType,
					From: MemoryCurationVersionReference{MemoryID: "mem_from", Version: 1},
					To:   MemoryCurationVersionReference{MemoryID: "mem_to", Version: 2}},
			})
			if _, err := AcceptMemoryCurationPlan(draft, MemoryCurationPlanAcceptOptions{PlanRevision: 1}); !errors.Is(err, ErrMemoryInputInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	duplicate := validCurationDraft(
		MemoryCurationPlanAction{Ordinal: 1, Operation: MemoryCurationOperationRelate,
			Relate: &MemoryCurationRelateAction{RelationType: MemoryCurationRelationDerivedFrom,
				From: MemoryCurationVersionReference{MemoryID: "mem_from", Version: 1},
				To:   MemoryCurationVersionReference{MemoryID: "mem_to", Version: 2}}},
		MemoryCurationPlanAction{Ordinal: 2, Operation: MemoryCurationOperationRelate,
			Relate: &MemoryCurationRelateAction{RelationType: MemoryCurationRelationDerivedFrom,
				From: MemoryCurationVersionReference{MemoryID: "mem_from", Version: 1},
				To:   MemoryCurationVersionReference{MemoryID: "mem_to", Version: 2}}},
	)
	if _, err := AcceptMemoryCurationPlan(duplicate, MemoryCurationPlanAcceptOptions{PlanRevision: 1}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("duplicate relation error = %v", err)
	}

	self := validCurationDraft(MemoryCurationPlanAction{Ordinal: 1, Operation: MemoryCurationOperationRelate,
		Relate: &MemoryCurationRelateAction{RelationType: MemoryCurationRelationDerivedFrom,
			From: MemoryCurationVersionReference{MemoryID: "mem_same", Version: 1},
			To:   MemoryCurationVersionReference{MemoryID: "mem_same", Version: 1}}})
	if _, err := AcceptMemoryCurationPlan(self, MemoryCurationPlanAcceptOptions{PlanRevision: 1}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("self relation error = %v", err)
	}

	create := MemoryCurationPlanAction{Ordinal: 1, Operation: MemoryCurationOperationCreate,
		Create: &MemoryCurationCreateAction{LocalRef: "output", Snapshot: validCurationSnapshot("output"),
			Relations: []MemoryCurationLineageRelation{
				{RelationType: MemoryCurationRelationDerivedFrom, To: MemoryCurationVersionReference{MemoryID: "mem_source", Version: 1}},
				{RelationType: MemoryCurationRelationDerivedFrom, To: MemoryCurationVersionReference{MemoryID: "mem_source", Version: 1}},
			}}}
	supersede := MemoryCurationPlanAction{Ordinal: 2, Operation: MemoryCurationOperationSupersede,
		Supersede: &MemoryCurationSupersedeAction{
			Target: MemoryCurationTargetReference{MemoryID: "mem_source", ExpectedVersion: 1},
			Replacements: []MemoryCurationVersionReference{
				{LocalRef: "output", Version: 1}, {LocalRef: "output", Version: 1},
				{MemoryID: "mem_existing_output", Version: 4},
			},
		}}
	accepted, err := AcceptMemoryCurationPlan(validCurationDraft(create, supersede), MemoryCurationPlanAcceptOptions{
		PlanRevision: 1, PreallocatedMemoryIDs: map[string]string{"output": "mem_output"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted.Plan.Actions[0].Create.Relations) != 1 || len(accepted.Plan.Actions[1].Supersede.Replacements) != 2 {
		t.Fatalf("set normalization failed: %#v", accepted.Plan.Actions)
	}
	if accepted.Preview.NewMemories != 1 || accepted.Preview.RelationRows != 3 {
		t.Fatalf("preview = %#v", accepted.Preview)
	}

	badSupersedes := []MemoryCurationSupersedeAction{
		{Target: MemoryCurationTargetReference{MemoryID: "mem_source", ExpectedVersion: 1}},
		{Target: MemoryCurationTargetReference{MemoryID: "mem_source", ExpectedVersion: 1}, Replacements: make([]MemoryCurationVersionReference, 33)},
		{Target: MemoryCurationTargetReference{MemoryID: "mem_source", ExpectedVersion: 1}, Replacements: []MemoryCurationVersionReference{{MemoryID: "mem_source", Version: 1}}},
		{Target: MemoryCurationTargetReference{LocalRef: "unknown", ExpectedVersion: 1}, Replacements: []MemoryCurationVersionReference{{MemoryID: "mem_output", Version: 1}}},
		{Target: MemoryCurationTargetReference{MemoryID: "mem_source", ExpectedVersion: 1}, Replacements: []MemoryCurationVersionReference{{LocalRef: "unknown", Version: 1}}},
	}
	for index, input := range badSupersedes {
		action := MemoryCurationPlanAction{Ordinal: 1, Operation: MemoryCurationOperationSupersede, Supersede: &input}
		if _, err := AcceptMemoryCurationPlan(validCurationDraft(action), MemoryCurationPlanAcceptOptions{PlanRevision: 1}); !errors.Is(err, ErrMemoryInputInvalid) {
			t.Errorf("bad supersede %d error = %v", index, err)
		}
	}
}

func TestMemoryCurationPlanSupportsMergeAndSplitCompilation(t *testing.T) {
	actions := []MemoryCurationPlanAction{
		{Ordinal: 1, Operation: MemoryCurationOperationCreate, Create: &MemoryCurationCreateAction{LocalRef: "merged", Snapshot: validCurationSnapshot("merged")}},
		{Ordinal: 2, Operation: MemoryCurationOperationCreate, Create: &MemoryCurationCreateAction{LocalRef: "split_a", Snapshot: validCurationSnapshot("split a")}},
		{Ordinal: 3, Operation: MemoryCurationOperationCreate, Create: &MemoryCurationCreateAction{LocalRef: "split_b", Snapshot: validCurationSnapshot("split b")}},
		{Ordinal: 4, Operation: MemoryCurationOperationSupersede, Supersede: &MemoryCurationSupersedeAction{Target: MemoryCurationTargetReference{MemoryID: "mem_merge_a", ExpectedVersion: 2}, Replacements: []MemoryCurationVersionReference{{LocalRef: "merged", Version: 1}}}},
		{Ordinal: 5, Operation: MemoryCurationOperationSupersede, Supersede: &MemoryCurationSupersedeAction{Target: MemoryCurationTargetReference{MemoryID: "mem_merge_b", ExpectedVersion: 3}, Replacements: []MemoryCurationVersionReference{{LocalRef: "merged", Version: 1}}}},
		{Ordinal: 6, Operation: MemoryCurationOperationSupersede, Supersede: &MemoryCurationSupersedeAction{Target: MemoryCurationTargetReference{MemoryID: "mem_split_source", ExpectedVersion: 4}, Replacements: []MemoryCurationVersionReference{{LocalRef: "split_a", Version: 1}, {LocalRef: "split_b", Version: 1}}}},
	}
	allocated := []string{}
	accepted, err := AcceptMemoryCurationPlan(validCurationDraft(actions...), MemoryCurationPlanAcceptOptions{
		PlanRevision: 1,
		Allocator: MemoryCurationMemoryIDAllocatorFunc(func(localRef string) (string, error) {
			allocated = append(allocated, localRef)
			return "mem_" + localRef, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := allocated, []string{"merged", "split_a", "split_b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("allocation order = %#v, want %#v", got, want)
	}
	if accepted.Preview.NewMemories != 3 || accepted.Preview.SupersedeActions != 3 ||
		accepted.Preview.RelationRows != 4 || accepted.Preview.MemoryVersionWrites != 6 {
		t.Fatalf("merge/split preview = %#v", accepted.Preview)
	}
}

func TestMemoryCurationPlanValidatesSnapshotsEvidenceTargetsAndFacts(t *testing.T) {
	replace := func(snapshot MemoryCurationMemorySnapshot, target MemoryCurationTargetReference) MemoryCurationPlanDraft {
		return validCurationDraft(MemoryCurationPlanAction{Ordinal: 1, Operation: MemoryCurationOperationReplace,
			Replace: &MemoryCurationReplaceAction{Target: target, Snapshot: snapshot}})
	}
	// Replacement evidence is additive and optional; create evidence is not.
	if _, err := AcceptMemoryCurationPlan(replace(MemoryCurationMemorySnapshot{Content: "replacement"},
		MemoryCurationTargetReference{MemoryID: "mem_target", ExpectedVersion: 1}), MemoryCurationPlanAcceptOptions{PlanRevision: 1}); err != nil {
		t.Fatal(err)
	}
	createWithoutEvidence := validCurationDraft(MemoryCurationPlanAction{Ordinal: 1, Operation: MemoryCurationOperationCreate,
		Create: &MemoryCurationCreateAction{LocalRef: "new", Snapshot: MemoryCurationMemorySnapshot{Content: "new"}}})
	if _, err := AcceptMemoryCurationPlan(createWithoutEvidence, MemoryCurationPlanAcceptOptions{PlanRevision: 1, Allocator: fixedCurationAllocator("mem_new")}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("missing create evidence error = %v", err)
	}

	invalidSnapshots := []MemoryCurationMemorySnapshot{
		{Content: ""},
		{Content: "not canonical base64", ContentEncoding: "base64"},
		{Content: "x", Salience: floatPointer(math.NaN())},
		{Content: "x", OccurredFrom: timePointer(time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)), OccurredUntil: timePointer(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))},
	}
	for index, snapshot := range invalidSnapshots {
		if _, err := AcceptMemoryCurationPlan(replace(snapshot,
			MemoryCurationTargetReference{MemoryID: "mem_target", ExpectedVersion: 1}), MemoryCurationPlanAcceptOptions{PlanRevision: 1}); !errors.Is(err, ErrMemoryInputInvalid) {
			t.Errorf("invalid snapshot %d error = %v", index, err)
		}
	}

	invalidTargets := []MemoryCurationTargetReference{
		{},
		{MemoryID: "mem_target", LocalRef: "new", ExpectedVersion: 1},
		{MemoryID: "bad", ExpectedVersion: 1},
		{MemoryID: "mem_target", ExpectedVersion: 0},
		{LocalRef: "unknown", ExpectedVersion: 1},
	}
	for index, target := range invalidTargets {
		if _, err := AcceptMemoryCurationPlan(replace(MemoryCurationMemorySnapshot{Content: "x"}, target),
			MemoryCurationPlanAcceptOptions{PlanRevision: 1}); !errors.Is(err, ErrMemoryInputInvalid) {
			t.Errorf("invalid target %d error = %v", index, err)
		}
	}

	badEvidence := []MemoryCurationEvidence{
		{Type: "conversation", ResolutionState: MemoryEvidenceUnresolvable, TerminalReasonCode: "missing"},
		{Type: "conversation", ResolutionState: MemoryEvidenceResolved, ResolvedKind: "memory", SourceMemory: &MemoryCurationVersionReference{LocalRef: "unknown", Version: 1}},
		{Type: "conversation", ResolutionState: MemoryEvidenceResolved, ResolvedKind: "memory", SourceMemory: &MemoryCurationVersionReference{MemoryID: "mem_source", Version: 0}},
	}
	for index, evidence := range badEvidence {
		snapshot := MemoryCurationMemorySnapshot{Content: "x", Evidence: []MemoryCurationEvidence{evidence}}
		if _, err := AcceptMemoryCurationPlan(replace(snapshot,
			MemoryCurationTargetReference{MemoryID: "mem_target", ExpectedVersion: 1}), MemoryCurationPlanAcceptOptions{PlanRevision: 1}); !errors.Is(err, ErrMemoryInputInvalid) {
			t.Errorf("invalid evidence %d error = %v", index, err)
		}
	}

	fact := MemoryCurationProposeFactAction{Predicate: "profile/name", Value: json.RawMessage(`"Ada"`),
		Evidence: []MemoryCurationEvidence{unavailableCurationEvidence()}}
	accepted, err := AcceptMemoryCurationPlan(validCurationDraft(MemoryCurationPlanAction{
		Ordinal: 1, Operation: MemoryCurationOperationProposeFact, ProposeFact: &fact,
	}), MemoryCurationPlanAcceptOptions{PlanRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	proposal := accepted.Plan.Actions[0].ProposeFact
	if proposal.Subject != "self" || proposal.ValueType != "string" || proposal.Cardinality != FactCardinalityOne ||
		proposal.Confidence == nil || *proposal.Confidence != 0.5 {
		t.Fatalf("normalized proposal = %#v", proposal)
	}
	fact.Evidence = nil
	if _, err := AcceptMemoryCurationPlan(validCurationDraft(MemoryCurationPlanAction{
		Ordinal: 1, Operation: MemoryCurationOperationProposeFact, ProposeFact: &fact,
	}), MemoryCurationPlanAcceptOptions{PlanRevision: 1}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("fact without evidence error = %v", err)
	}
}

func TestMemoryCurationPlanAggregateBudgets(t *testing.T) {
	// Thirty-three maximum-size memory payloads exceed the 8 MiB plan content
	// budget while each remains individually valid.
	contentActions := make([]MemoryCurationPlanAction, 33)
	content := strings.Repeat("x", maxMemoryContentBytes)
	ids := make(map[string]string, len(contentActions))
	for index := range contentActions {
		localRef := fmt.Sprintf("new_%02d", index)
		contentActions[index] = MemoryCurationPlanAction{Ordinal: int64(index + 1), Operation: MemoryCurationOperationCreate,
			Create: &MemoryCurationCreateAction{LocalRef: localRef, Snapshot: validCurationSnapshot(content)}}
		ids[localRef] = "mem_" + localRef
	}
	if _, err := AcceptMemoryCurationPlan(validCurationDraft(contentActions...), MemoryCurationPlanAcceptOptions{
		PlanRevision: 1, PreallocatedMemoryIDs: ids,
	}); !errors.Is(err, ErrMemoryInputInvalid) || !strings.Contains(err.Error(), "content") {
		t.Fatalf("content budget error = %v", err)
	}

	// Supersession edges are real relation rows and count against the global
	// bound even though they are not generic relate primitives.
	relationActions := make([]MemoryCurationPlanAction, 33)
	for index := range relationActions {
		replacements := make([]MemoryCurationVersionReference, 32)
		for replacement := range replacements {
			replacements[replacement] = MemoryCurationVersionReference{
				MemoryID: fmt.Sprintf("mem_output_%02d_%02d", index, replacement), Version: 1,
			}
		}
		relationActions[index] = MemoryCurationPlanAction{Ordinal: int64(index + 1), Operation: MemoryCurationOperationSupersede,
			Supersede: &MemoryCurationSupersedeAction{
				Target:       MemoryCurationTargetReference{MemoryID: fmt.Sprintf("mem_source_%02d", index), ExpectedVersion: 1},
				Replacements: replacements,
			}}
	}
	if _, err := AcceptMemoryCurationPlan(validCurationDraft(relationActions...), MemoryCurationPlanAcceptOptions{
		PlanRevision: 1,
	}); !errors.Is(err, ErrMemoryInputInvalid) || !strings.Contains(err.Error(), "relation") {
		t.Fatalf("relation budget error = %v", err)
	}

	evidenceActions := make([]MemoryCurationPlanAction, 33)
	for index := range evidenceActions {
		evidence := make([]MemoryCurationEvidence, 32)
		for evidenceIndex := range evidence {
			evidence[evidenceIndex] = unavailableCurationEvidence()
		}
		evidenceActions[index] = MemoryCurationPlanAction{Ordinal: int64(index + 1), Operation: MemoryCurationOperationReplace,
			Replace: &MemoryCurationReplaceAction{
				Target:   MemoryCurationTargetReference{MemoryID: fmt.Sprintf("mem_evidence_%02d", index), ExpectedVersion: 1},
				Snapshot: MemoryCurationMemorySnapshot{Content: "x", Evidence: evidence},
			}}
	}
	if _, err := AcceptMemoryCurationPlan(validCurationDraft(evidenceActions...), MemoryCurationPlanAcceptOptions{
		PlanRevision: 1,
	}); !errors.Is(err, ErrMemoryInputInvalid) || !strings.Contains(err.Error(), "evidence rows") {
		t.Fatalf("evidence budget error = %v", err)
	}

	artifactActions := make([]MemoryCurationPlanAction, 5)
	artifact := bytes.Repeat([]byte{'x'}, 65536)
	for index := range artifactActions {
		evidence := make([]MemoryCurationEvidence, 32)
		for evidenceIndex := range evidence {
			evidence[evidenceIndex] = MemoryCurationEvidence{
				Type: "artifact", ResolutionState: MemoryEvidenceResolved,
				ResolvedKind: "artifact", ArtifactExcerpt: artifact,
			}
		}
		artifactActions[index] = MemoryCurationPlanAction{Ordinal: int64(index + 1), Operation: MemoryCurationOperationReplace,
			Replace: &MemoryCurationReplaceAction{
				Target:   MemoryCurationTargetReference{MemoryID: fmt.Sprintf("mem_artifact_%02d", index), ExpectedVersion: 1},
				Snapshot: MemoryCurationMemorySnapshot{Content: "x", Evidence: evidence},
			}}
	}
	if _, err := AcceptMemoryCurationPlan(validCurationDraft(artifactActions...), MemoryCurationPlanAcceptOptions{
		PlanRevision: 1,
	}); !errors.Is(err, ErrMemoryInputInvalid) || !strings.Contains(err.Error(), "artifacts") {
		t.Fatalf("artifact budget error = %v", err)
	}

	factActions := make([]MemoryCurationPlanAction, 17)
	maxFactValue := json.RawMessage(`"` + strings.Repeat("x", 65534) + `"`)
	for index := range factActions {
		factActions[index] = MemoryCurationPlanAction{Ordinal: int64(index + 1), Operation: MemoryCurationOperationProposeFact,
			ProposeFact: &MemoryCurationProposeFactAction{
				Predicate: fmt.Sprintf("profile/value_%02d", index), ValueType: "string", Value: maxFactValue,
				Evidence: []MemoryCurationEvidence{unavailableCurationEvidence()},
			}}
	}
	if _, err := AcceptMemoryCurationPlan(validCurationDraft(factActions...), MemoryCurationPlanAcceptOptions{
		PlanRevision: 1,
	}); !errors.Is(err, ErrMemoryInputInvalid) || !strings.Contains(err.Error(), "fact values") {
		t.Fatalf("fact-value budget error = %v", err)
	}
}

func TestMemoryCurationCanonicalJSONMatchesRFC8785RestrictedProfile(t *testing.T) {
	numbers, err := canonicalizeMemoryCurationRawJSON(json.RawMessage(
		`[333333333.33333329,1E30,4.50,2e-3,0.000000000000000000000000001,-0,0.000001,0.0000001,1e20,1e21]`,
	))
	if err != nil {
		t.Fatal(err)
	}
	const wantNumbers = `[333333333.3333333,1e+30,4.5,0.002,1e-27,0,0.000001,1e-7,100000000000000000000,1e+21]`
	if string(numbers) != wantNumbers {
		t.Fatalf("canonical numbers = %s, want %s", numbers, wantNumbers)
	}
	// UTF-16 ordering differs from Unicode code-point/UTF-8 ordering here:
	// the surrogate pair for the emoji sorts before U+FFFD.
	stringsValue, err := canonicalizeMemoryCurationRawJSON(json.RawMessage(`{"�":"replacement","😀":"emoji","line\u2028separator":"<ok>"}`))
	if err != nil {
		t.Fatal(err)
	}
	const wantStrings = `{"line separator":"<ok>","😀":"emoji","�":"replacement"}`
	if string(stringsValue) != wantStrings {
		t.Fatalf("canonical strings = %s, want %s", stringsValue, wantStrings)
	}
	controls, err := canonicalizeMemoryCurationRawJSON(json.RawMessage("{\"x\":\"\\b\\t\\n\\f\\r\\\"\\\\\\u0000/<>\"}"))
	if err != nil {
		t.Fatal(err)
	}
	const wantControls = `{"x":"\b\t\n\f\r\"\\\u0000/<>"}`
	if string(controls) != wantControls {
		t.Fatalf("canonical control escaping = %s, want %s", controls, wantControls)
	}
	paired, err := canonicalizeMemoryCurationRawJSON(json.RawMessage(`"\ud834\udd1e"`))
	if err != nil || string(paired) != `"𝄞"` {
		t.Fatalf("canonical surrogate pair = %s, error %v", paired, err)
	}
	for _, raw := range []json.RawMessage{json.RawMessage(`"\ud800"`), json.RawMessage(`"\udc00"`), json.RawMessage(`"\ud800\u0041"`)} {
		if _, err := canonicalizeMemoryCurationRawJSON(raw); !errors.Is(err, ErrMemoryInputInvalid) {
			t.Errorf("lone surrogate %s error = %v", raw, err)
		}
	}
}

func TestMemoryCurationPlanRejectsInvalidUTF8AndDuplicateFactMembers(t *testing.T) {
	draft := validCurationDraft(MemoryCurationPlanAction{Ordinal: 1, Operation: MemoryCurationOperationProposeFact,
		ProposeFact: &MemoryCurationProposeFactAction{Predicate: "profile/name",
			Value: json.RawMessage(`{"name":"Ada","name":"Grace"}`), Evidence: []MemoryCurationEvidence{unavailableCurationEvidence()}}})
	if _, err := AcceptMemoryCurationPlan(draft, MemoryCurationPlanAcceptOptions{PlanRevision: 1}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("duplicate fact member error = %v", err)
	}
	invalid := validCurationDraft(MemoryCurationPlanAction{Ordinal: 1, Operation: MemoryCurationOperationReplace,
		Replace: &MemoryCurationReplaceAction{Target: MemoryCurationTargetReference{MemoryID: "mem_target", ExpectedVersion: 1},
			Snapshot: MemoryCurationMemorySnapshot{Content: string([]byte{0xff})}}})
	if _, err := AcceptMemoryCurationPlan(invalid, MemoryCurationPlanAcceptOptions{PlanRevision: 1}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
	oversizedFact := validCurationDraft(MemoryCurationPlanAction{Ordinal: 1, Operation: MemoryCurationOperationProposeFact,
		ProposeFact: &MemoryCurationProposeFactAction{Predicate: "profile/name", ValueType: "string",
			Value: json.RawMessage(`"` + strings.Repeat("x", 65535) + `"`), Evidence: []MemoryCurationEvidence{unavailableCurationEvidence()}}})
	if _, err := AcceptMemoryCurationPlan(oversizedFact, MemoryCurationPlanAcceptOptions{PlanRevision: 1}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("oversized fact value error = %v", err)
	}
}

func FuzzDecodeMemoryCurationPlanDraftNeverPanics(f *testing.F) {
	f.Add([]byte(`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}`))
	f.Add([]byte(`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[{"ordinal":1,"operation":"create","create":{"local_ref":"x","snapshot":{"content":"x","evidence":[{"type":"conversation","resolution_state":"unavailable","terminal_reason_code":"not_recorded"}]}}}]}`))
	f.Add([]byte{0xff, 0x00, '{', '}'})
	f.Fuzz(func(_ *testing.T, raw []byte) {
		draft, err := DecodeMemoryCurationPlanDraft(raw)
		if err != nil {
			return
		}
		ids := make(map[string]string)
		for _, action := range draft.Actions {
			if action.Create != nil && memoryCurationLocalRefPattern.MatchString(strings.TrimSpace(action.Create.LocalRef)) {
				ids[strings.TrimSpace(action.Create.LocalRef)] = "mem_fuzz_" + fmt.Sprint(action.Ordinal)
			}
		}
		_, _ = AcceptMemoryCurationPlan(draft, MemoryCurationPlanAcceptOptions{PlanRevision: 1, PreallocatedMemoryIDs: ids})
	})
}

func FuzzMemoryCurationCanonicalRawJSONNeverPanics(f *testing.F) {
	f.Add([]byte(`{"b":2,"a":1}`))
	f.Add([]byte(`[1e30,-0,0.000001]`))
	f.Add([]byte(`"\ud800"`))
	f.Fuzz(func(_ *testing.T, raw []byte) {
		_, _ = canonicalizeMemoryCurationRawJSON(json.RawMessage(raw))
	})
}

func completeMemoryCurationPlanDraft() MemoryCurationPlanDraft {
	when := time.Date(2025, 2, 3, 4, 5, 6, 7, time.FixedZone("offset", -7*60*60))
	return validCurationDraft(
		MemoryCurationPlanAction{Ordinal: 1, Operation: MemoryCurationOperationCreate,
			Create: &MemoryCurationCreateAction{LocalRef: " summary ",
				Snapshot: MemoryCurationMemorySnapshot{Content: "Summary", Kind: "decision",
					Tags:         []string{"zeta", "alpha", "zeta"},
					Links:        []string{"https://z.example", "https://a.example"},
					OccurredFrom: &when, Evidence: []MemoryCurationEvidence{unavailableCurationEvidence()}},
				Relations: []MemoryCurationLineageRelation{{RelationType: MemoryCurationRelationDerivedFrom,
					To: MemoryCurationVersionReference{MemoryID: "mem_source", Version: 2}}}}},
		MemoryCurationPlanAction{Ordinal: 2, Operation: MemoryCurationOperationReplace,
			Replace: &MemoryCurationReplaceAction{Target: MemoryCurationTargetReference{MemoryID: " mem_replace ", ExpectedVersion: 3},
				Snapshot: MemoryCurationMemorySnapshot{Content: "Revised", Kind: "decision", Salience: floatPointer(0.75)}, Reason: " hone wording "}},
		MemoryCurationPlanAction{Ordinal: 3, Operation: MemoryCurationOperationSupersede,
			Supersede: &MemoryCurationSupersedeAction{Target: MemoryCurationTargetReference{MemoryID: "mem_source", ExpectedVersion: 2},
				Replacements: []MemoryCurationVersionReference{{LocalRef: "summary", Version: 1}, {MemoryID: "mem_existing_output", Version: 4}}, Reason: " merge "}},
		MemoryCurationPlanAction{Ordinal: 4, Operation: MemoryCurationOperationRelate,
			Relate: &MemoryCurationRelateAction{RelationType: MemoryCurationRelationConflictsWith,
				From: MemoryCurationVersionReference{LocalRef: "summary", Version: 1},
				To:   MemoryCurationVersionReference{MemoryID: "mem_source", Version: 2}}},
		MemoryCurationPlanAction{Ordinal: 5, Operation: MemoryCurationOperationProposeFact,
			ProposeFact: &MemoryCurationProposeFactAction{Predicate: "profile/preference", ValueType: "object",
				Value: json.RawMessage(`{"z":1,"a":"x"}`), Reason: " candidate only ",
				Evidence: []MemoryCurationEvidence{{Type: "memory", ResolutionState: MemoryEvidenceResolved,
					ResolvedKind: "memory", SourceMemory: &MemoryCurationVersionReference{LocalRef: "summary", Version: 1}}}}},
	)
}

func validCurationDraft(actions ...MemoryCurationPlanAction) MemoryCurationPlanDraft {
	return MemoryCurationPlanDraft{Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1, Actions: actions}
}

func validCurationSnapshot(content string) MemoryCurationMemorySnapshot {
	return MemoryCurationMemorySnapshot{Content: content, Evidence: []MemoryCurationEvidence{unavailableCurationEvidence()}}
}

func unavailableCurationEvidence() MemoryCurationEvidence {
	return MemoryCurationEvidence{Type: "conversation", ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "not_recorded"}
}

func fixedCurationAllocator(memoryID string) MemoryCurationMemoryIDAllocator {
	return MemoryCurationMemoryIDAllocatorFunc(func(string) (string, error) { return memoryID, nil })
}

func floatPointer(value float64) *float64 { return &value }

func timePointer(value time.Time) *time.Time { return &value }
