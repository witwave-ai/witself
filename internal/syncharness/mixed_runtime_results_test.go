package syncharness

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"go.yaml.in/yaml/v3"
)

func mixedIdentityFixture() MixedBuildIdentity {
	binary := func(value string) BinaryBuildIdentity {
		return BinaryBuildIdentity{Digest: strings.Repeat(value, 64), GoVersion: "go1.26.3", GOOS: "linux", GOARCH: "amd64"}
	}
	return MixedBuildIdentity{PredecessorCommit: strings.Repeat("a", 40), CurrentServer: binary("1"), CurrentWorker: binary("2"), PredecessorServer: binary("3"), PredecessorWorker: binary("4")}
}

func mixedJobEvidenceFixture(capability string) []MixedJobEvidence {
	if capability != "legacy_writer_refusal" {
		return nil
	}
	return []MixedJobEvidence{
		{Job: "avatar_style_rollout", Class: GuardCauseExact, RowsUnchanged: true},
		{Job: "message_rate_bucket_cleanup", Class: GuardCauseExact, RowsUnchanged: true},
		{Job: "agent_email_rate_bucket_cleanup", Class: GuardCauseExact, RowsUnchanged: true},
		{Job: "agent_email_outbound", Class: GuardCauseExact, RowsUnchanged: true},
		{Job: "transcript_retention", Class: GuardCauseExact, RowsUnchanged: true},
		{Job: "message_retention", Class: GuardCauseExact, RowsUnchanged: true},
		{Job: "agent_email_retention", Class: GuardCauseExact, RowsUnchanged: true},
		{Job: "support_ticket_age_out", Class: GuardCauseHidden, RowsUnchanged: true},
		{Job: "transcript_storage_reconcile", Class: GuardCauseHidden, RowsUnchanged: true},
		{Job: "account_purge", Class: PreguardRefusal, RowsUnchanged: true},
	}
}

func TestMixedRuntimeResultsRequireCompletePrivateIdentity(t *testing.T) {
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal("mixed evidence obligation inventory unavailable")
	}
	for _, allocation := range mixedRuntimeAllocations {
		t.Run(allocation.capability, func(t *testing.T) {
			for _, count := range []int{0, 1, 3, 5} {
				if _, err := VerifyMixedRuntimeArtifact(allocation.capability, count, mixedIdentityFixture(), mixedJobEvidenceFixture(allocation.capability)...); !errors.Is(err, ErrRuntimeResult) {
					t.Fatal("mixed evidence accepted incomplete or invented matrix")
				}
			}
			artifact, err := VerifyMixedRuntimeArtifact(allocation.capability, 4, mixedIdentityFixture(), mixedJobEvidenceFixture(allocation.capability)...)
			if err != nil || artifact.Result.Validate(manifests) != nil || artifact.Result.Adapter.Symbol != "TestCaptureMixMatrix" || artifact.Result.EvidenceClass != MixedBinariesEvidence {
				t.Fatal("mixed evidence rejected reviewed receipt shape")
			}
			// The synthetic receipt is not retained as executed evidence. Verify
			// private identity serialization survives the strict artifact reader.
			data, err := json.Marshal(artifact)
			if err != nil {
				t.Fatal("mixed evidence private encoding failed")
			}
			files := fstest.MapFS{"testdata/runtime_results/artifacts/" + artifact.ID + ".v1.json": &fstest.MapFile{Data: data}}
			decoded, err := loadRuntimeArtifact(files, artifact.ID)
			if err != nil || !reflect.DeepEqual(decoded, artifact) {
				t.Fatal("mixed evidence private identity round trip failed")
			}
			for _, mutate := range []func(*RuntimeResultRecord){
				func(r *RuntimeResultRecord) { r.EvidenceClass = RealPostgresEvidence },
				func(r *RuntimeResultRecord) { r.Adapter.Symbol = "TestSyncCaptureLateFirstWrite" },
				func(r *RuntimeResultRecord) { r.DatabaseClass = "postgresql" },
				func(r *RuntimeResultRecord) { r.BuildIdentity = nil },
				func(r *RuntimeResultRecord) { r.Obligation = "H01" },
			} {
				changed := artifact.Result
				mutate(&changed)
				if !errors.Is(changed.Validate(manifests), ErrRuntimeResult) {
					t.Fatal("mixed evidence accepted changed scope or identity")
				}
			}
		})
	}
	for _, mutate := range []func(*MixedBuildIdentity){
		func(i *MixedBuildIdentity) { i.PredecessorCommit = "unknown" },
		func(i *MixedBuildIdentity) { i.CurrentServer.Digest = "missing" },
		func(i *MixedBuildIdentity) { i.CurrentWorker = i.PredecessorWorker },
		func(i *MixedBuildIdentity) { i.PredecessorServer.GoVersion = "unknown" },
		func(i *MixedBuildIdentity) { i.PredecessorWorker.GOOS = "" },
		func(i *MixedBuildIdentity) { i.CurrentServer.GOARCH = "private/value" },
	} {
		identity := mixedIdentityFixture()
		mutate(&identity)
		if _, err := VerifyMixedRuntimeArtifact("mixed_executable_versions", 4, identity); !errors.Is(err, ErrRuntimeResult) {
			t.Fatal("mixed evidence accepted incomplete binary provenance")
		}
	}
	if _, err := VerifyMixedRuntimeArtifact("unknown_capability", 4, mixedIdentityFixture()); !errors.Is(err, ErrRuntimeResult) {
		t.Fatal("mixed evidence accepted unbound capability")
	}
}

func TestMixedRuntimeArtifactRequiresCompleteJobEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]MixedJobEvidence) []MixedJobEvidence
	}{
		{"absent", func([]MixedJobEvidence) []MixedJobEvidence { return nil }},
		{"incomplete", func(e []MixedJobEvidence) []MixedJobEvidence { return e[:len(e)-1] }},
		{"duplicate", func(e []MixedJobEvidence) []MixedJobEvidence { e[1] = e[0]; return e }},
		{"unknown_job", func(e []MixedJobEvidence) []MixedJobEvidence { e[0].Job = "unknown_job"; return e }},
		{"unknown_class", func(e []MixedJobEvidence) []MixedJobEvidence { e[0].Class = "generic_failure"; return e }},
		{"hidden_exact_cause", func(e []MixedJobEvidence) []MixedJobEvidence { e[6].Class = GuardCauseHidden; return e }},
		{"hidden_preguard_refusal", func(e []MixedJobEvidence) []MixedJobEvidence { e[9].Class = GuardCauseHidden; return e }},
		{"hidden_rows_changed", func(e []MixedJobEvidence) []MixedJobEvidence { e[7].RowsUnchanged = false; return e }},
		{"preguard_rows_changed", func(e []MixedJobEvidence) []MixedJobEvidence { e[9].RowsUnchanged = false; return e }},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence := test.mutate(mixedJobEvidenceFixture("legacy_writer_refusal"))
			if _, err := VerifyMixedRuntimeArtifact("legacy_writer_refusal", 4, mixedIdentityFixture(), evidence...); !errors.Is(err, ErrRuntimeResult) {
				t.Fatal("mixed evidence accepted incomplete or misqualified job evidence")
			}
			artifact, err := VerifyMixedRuntimeArtifact("legacy_writer_refusal", 4, mixedIdentityFixture(), mixedJobEvidenceFixture("legacy_writer_refusal")...)
			if err != nil {
				t.Fatal("mixed evidence fixture rejected")
			}
			artifact.JobEvidence = evidence
			data, err := json.Marshal(artifact)
			if err != nil {
				t.Fatal("mixed evidence altered artifact encoding failed")
			}
			files := fstest.MapFS{"testdata/runtime_results/artifacts/" + artifact.ID + ".v1.json": &fstest.MapFile{Data: data}}
			if _, err := loadRuntimeArtifact(files, artifact.ID); !errors.Is(err, ErrRuntimeResult) {
				t.Fatal("mixed evidence replay accepted incomplete or misqualified job evidence")
			}
		})
	}
	for _, allocation := range mixedRuntimeAllocations {
		if allocation.capability == "legacy_writer_refusal" {
			continue
		}
		if _, err := VerifyMixedRuntimeArtifact(allocation.capability, 4, mixedIdentityFixture(), mixedJobEvidenceFixture("legacy_writer_refusal")...); !errors.Is(err, ErrRuntimeResult) {
			t.Fatal("mixed evidence attached job qualifications to unrelated capability")
		}
	}
	evidence := mixedJobEvidenceFixture("legacy_writer_refusal")
	artifact, err := VerifyMixedRuntimeArtifact("legacy_writer_refusal", 4, mixedIdentityFixture(), evidence...)
	if err != nil {
		t.Fatal("mixed evidence fixture rejected")
	}
	evidence[0].Class = GuardCauseHidden
	if artifact.JobEvidence[0].Class != GuardCauseExact {
		t.Fatal("mixed evidence receipt aliases mutable input")
	}
}

func TestMixedRuntimeJobEvidenceVocabulary(t *testing.T) {
	if !reflect.DeepEqual([]MixedJobEvidenceClass{GuardCauseExact, GuardCauseHidden, PreguardRefusal}, []MixedJobEvidenceClass{"guard_cause_exact", "guard_cause_hidden", "preguard_refusal"}) {
		t.Fatal("mixed evidence qualification vocabulary changed")
	}
	if len(mixedJobEvidenceClasses) != 10 {
		t.Fatal("mixed evidence job qualification inventory changed")
	}
	for _, evidence := range mixedJobEvidenceFixture("legacy_writer_refusal") {
		if mixedJobEvidenceClasses[evidence.Job] != evidence.Class {
			t.Fatal("mixed evidence job qualification mapping changed")
		}
		if evidence.Check() != "capturemix job="+evidence.Job+" evidence="+string(evidence.Class) {
			t.Fatal("mixed evidence public qualification line changed")
		}
	}
	if _, err := MixedRuntimeNotRun("legacy_writer_refusal", "mixed_binaries_guard_cause_unavailable"); !errors.Is(err, ErrRuntimeResult) {
		t.Fatal("mixed evidence accepted retired job-wide guard qualification")
	}
}

func mixedRuntimeClassDeclarationsMatch(source []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "mixed_runtime_results.go", source, 0)
	if err != nil {
		return false
	}
	classes := map[string]string{}
	for _, declaration := range file.Decls {
		group, ok := declaration.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		var previousType ast.Expr
		var previousValues []ast.Expr
		for _, specification := range group.Specs {
			value, ok := specification.(*ast.ValueSpec)
			if !ok {
				return false
			}
			kind, values := value.Type, value.Values
			if len(values) == 0 {
				kind, values = previousType, previousValues
			} else {
				previousType, previousValues = kind, values
			}
			identifier, ok := kind.(*ast.Ident)
			if !ok || identifier.Name != "MixedJobEvidenceClass" {
				continue
			}
			if len(value.Names) != 1 || len(values) != 1 {
				return false
			}
			literal, ok := values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return false
			}
			text, err := strconv.Unquote(literal.Value)
			if err != nil || classes[value.Names[0].Name] != "" {
				return false
			}
			classes[value.Names[0].Name] = text
		}
	}
	return reflect.DeepEqual(classes, map[string]string{
		"GuardCauseExact":  "guard_cause_exact",
		"GuardCauseHidden": "guard_cause_hidden",
		"PreguardRefusal":  "preguard_refusal",
	})
}

func TestMixedRuntimeJobEvidenceClassDeclarations(t *testing.T) {
	source, err := os.ReadFile("mixed_runtime_results.go")
	if err != nil {
		t.Fatal("mixed evidence class declarations unavailable")
	}
	normalized := bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
	for _, source := range [][]byte{normalized, bytes.ReplaceAll(normalized, []byte("\n"), []byte("\r\n"))} {
		if !mixedRuntimeClassDeclarationsMatch(source) {
			t.Fatal("mixed evidence class declarations changed")
		}
		mutated := append(bytes.Clone(source), []byte("\nconst UnusedGuardCause MixedJobEvidenceClass = \"guard_cause_other\"\n")...)
		if mixedRuntimeClassDeclarationsMatch(mutated) {
			t.Fatal("mixed evidence class declaration pin accepted fourth class")
		}
	}
}

func TestMixedRuntimeRehearsalCannotClaimExecution(t *testing.T) {
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal("mixed evidence obligation inventory unavailable")
	}
	for _, allocation := range mixedRuntimeAllocations {
		for _, reason := range []string{"mixed_binaries_roles_unavailable", "proof_profile_database_unavailable", "predecessor_build_unavailable", "not_executed"} {
			record, err := MixedRuntimeNotRun(allocation.capability, reason)
			if err != nil || record.Validate(manifests) != nil {
				t.Fatal("mixed evidence rejected pending rehearsal receipt")
			}
			for _, mutate := range []func(*RuntimeResultRecord){
				func(r *RuntimeResultRecord) { r.Outcome = Pass },
				func(r *RuntimeResultRecord) { r.EvidenceClass = MixedBinariesEvidence },
				func(r *RuntimeResultRecord) { identity := mixedIdentityFixture(); r.BuildIdentity = &identity },
				func(r *RuntimeResultRecord) { r.Reason = "ignored_failure" },
				func(r *RuntimeResultRecord) { r.Reason = "" },
			} {
				changed := record
				mutate(&changed)
				if !errors.Is(changed.Validate(manifests), ErrRuntimeResult) {
					t.Fatal("mixed evidence rehearsal claimed executable proof")
				}
			}
		}
	}
	// Read the inventory directly so identity checks remain independent of
	// retained artifact resolution, which the inventory boundary test covers.
	data, err := runtimeResultFiles.ReadFile("testdata/runtime_results/results.v1.json")
	var results RuntimeResults
	if err != nil || decodeRuntimeJSON(data, &results) != nil {
		t.Fatal("mixed evidence retained inventory unavailable")
	}
	predecessor := mixedRuntimeWorkflowPredecessor(t)
	var identity *MixedBuildIdentity
	found := 0
	for _, record := range results.Records {
		if mixedRuntimeCapability(record.Obligation, record.Capability) && record.Outcome == NotRun {
			t.Fatal("mixed evidence retained pending capability allocation")
		}
		if record.EvidenceClass != MixedBinariesEvidence {
			continue
		}
		expected, _, ok := mixedRuntimeRecord(record.Capability)
		expected.BuildIdentity = record.BuildIdentity
		if !ok || record != expected || record.Validate(manifests) != nil || record.BuildIdentity == nil || !record.BuildIdentity.valid() {
			t.Fatal("mixed evidence retained record differs from reviewed constructor")
		}
		if record.BuildIdentity.PredecessorCommit != predecessor {
			t.Fatal("mixed evidence retained predecessor differs from workflow pin")
		}
		if identity != nil && *record.BuildIdentity != *identity {
			t.Fatal("mixed evidence retained build identities differ")
		}
		identity = record.BuildIdentity
		found++
		t.Run(record.Capability, func(t *testing.T) {
			withoutIdentity := record
			withoutIdentity.BuildIdentity = nil
			if !errors.Is(withoutIdentity.Validate(manifests), ErrRuntimeResult) {
				t.Fatal("mixed evidence retained record accepted missing identity")
			}
			malformedPredecessor := record
			changedIdentity := *record.BuildIdentity
			changedIdentity.PredecessorCommit = "unreviewed_predecessor"
			malformedPredecessor.BuildIdentity = &changedIdentity
			if !errors.Is(malformedPredecessor.Validate(manifests), ErrRuntimeResult) {
				t.Fatal("mixed evidence retained record accepted malformed predecessor")
			}
		})
	}
	if found != 5 {
		t.Fatal("mixed evidence retained pass allocation count changed")
	}
}

func mixedRuntimeWorkflowPredecessor(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal("mixed evidence workflow unavailable")
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				With struct {
					Ref string `yaml:"ref"`
				} `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if yaml.Unmarshal(data, &workflow) != nil {
		t.Fatal("mixed evidence workflow invalid")
	}
	var predecessor string
	for _, step := range workflow.Jobs["go-capture-mixed"].Steps {
		if step.Name == "Check out the reviewed immutable predecessor" {
			if predecessor != "" || !mixedCommit.MatchString(step.With.Ref) {
				t.Fatal("mixed evidence workflow predecessor pin invalid or duplicated")
			}
			predecessor = step.With.Ref
		}
	}
	if predecessor == "" {
		t.Fatal("mixed evidence workflow predecessor pin missing")
	}
	return predecessor
}
