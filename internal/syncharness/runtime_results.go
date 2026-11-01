package syncharness

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"reflect"
	"regexp"
	"slices"
)

// EvidenceClass records the experiment's actual boundary, never its ambition.
type EvidenceClass string

// Evidence classes and outcomes remain distinct from legacy MODEL_PASS and
// POSTGRES_PASS statuses used by the preexisting miniature harness.
const (
	FixtureEvidence                  EvidenceClass  = "fixture"
	RealPostgresEvidence             EvidenceClass  = "real_postgres"
	MixedBinariesEvidence            EvidenceClass  = "mixed_binaries"
	NativeLifecycleEvidence          EvidenceClass  = "native_lifecycle"
	IndependentFailureDomainEvidence EvidenceClass  = "independent_failure_domain"
	Pass                             EvidenceStatus = "PASS"
	UnsupportedScope                 EvidenceStatus = "UNSUPPORTED_SCOPE"
)

// RuntimeAdapterSymbol names a package-local compiled execution binding.
// Package is a symbolic package name, not a filesystem path.
type RuntimeAdapterSymbol struct {
	Package string `json:"package"`
	Symbol  string `json:"symbol"`
}

// RuntimeResultRecord is separate from the obligation allocation inventory.
// Artifact is an opaque retained artifact ID, never a path, URL or raw digest.
// An unexecuted capability has no adapter, evidence class or artifact.
type RuntimeResultRecord struct {
	Version           int                  `json:"version"`
	Obligation        string               `json:"obligation"`
	Capability        string               `json:"capability"`
	EvidenceClass     EvidenceClass        `json:"evidence_class,omitempty"`
	Adapter           RuntimeAdapterSymbol `json:"adapter"`
	Scenario          string               `json:"scenario"`
	Executable        string               `json:"executable,omitempty"`
	DatabaseClass     string               `json:"database_class,omitempty"`
	PlatformClass     string               `json:"platform_class,omitempty"`
	Artifact          string               `json:"artifact,omitempty"`
	Outcome           EvidenceStatus       `json:"outcome"`
	MissingCapability string               `json:"missing_capability,omitempty"`
	Reason            string               `json:"reason,omitempty"`
}

// RuntimeArtifact retains only symbolic experiment provenance and an assertion
// count. The binding suites execute the adapter again and compare this record.
type RuntimeArtifact struct {
	Version int                 `json:"version"`
	ID      string              `json:"id"`
	Result  RuntimeResultRecord `json:"result"`
	Checks  int                 `json:"checks"`
}

// RuntimeResults is the versioned, value-free runtime evidence inventory.
type RuntimeResults struct {
	Version int                   `json:"version"`
	Kind    string                `json:"kind"`
	Records []RuntimeResultRecord `json:"records"`
}

// ErrRuntimeResult avoids reflecting rejected metadata into diagnostics.
var ErrRuntimeResult = errors.New("sync harness: invalid or unbound runtime evidence")

var runtimeToken = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,95}$`)

func validRuntimeToken(value string) bool {
	return runtimeToken.MatchString(value) && !longHex.MatchString(value)
}

func validEvidenceClass(class EvidenceClass) bool {
	return slices.Contains([]EvidenceClass{FixtureEvidence, RealPostgresEvidence, MixedBinariesEvidence, NativeLifecycleEvidence, IndependentFailureDomainEvidence}, class)
}

// fixtureRuntimeRecord is a reviewed adapter allocation, not an execution.
// LC-S6A registers no real database, binary, lifecycle or witness adapters.
// LC-S6C1's separate production allocation is in capture_runtime_results.go.
func fixtureRuntimeRecord(id string) (RuntimeResultRecord, int, bool) {
	r := RuntimeResultRecord{Version: 1, Obligation: id, EvidenceClass: FixtureEvidence, Executable: "lc_s6a_go_test_v1", DatabaseClass: "none", PlatformClass: "portable_go", Outcome: Pass}
	// These are reviewed expected counts; bindings supply counts from execution.
	checks := 0
	switch id {
	case "H03":
		checks = 7
		r.Capability = "legacy_projection_backfill_model"
		r.Adapter = RuntimeAdapterSymbol{"syncmodel", "RunLegacyProjectionBackfillScenario"}
		r.Scenario = "LegacyProjectionMigration"
	case "H05":
		checks = 6
		r.Capability = "ordered_evidence_membership_model"
		r.Adapter = RuntimeAdapterSymbol{"syncmodel", "RunOrderedEvidenceMembershipScenario"}
		r.Scenario = "EvidenceMembershipInterleaving"
	default:
		found := false
		for _, s := range CoverageSchedules() {
			if s.ID == id {
				r.Capability = "runtime_schedule_adapter"
				r.Adapter = RuntimeAdapterSymbol{"syncharness", "RunCoverageScheduleModel"}
				r.Scenario = s.Name
				checks = len(s.Barriers)
				found = true
				break
			}
		}
		if !found {
			return RuntimeResultRecord{}, 0, false
		}
	}
	r.Artifact = id + "_fixture_v1"
	return r, checks, true
}

type runtimeCapabilityPair struct {
	Obligation string
	Capability string
}

// completedModelCapabilities freezes the two reviewed LC-S6A model completions.
// Missing capabilities still come from the manifests; completion removes only
// these model gaps, not their runtime-result identities or production gaps.
var completedModelCapabilities = [...]runtimeCapabilityPair{
	{"H03", "legacy_projection_backfill_model"},
	{"H05", "ordered_evidence_membership_model"},
}

// Validate checks exact obligation/capability pairing and the reviewed adapter's
// executed class. General evidence classes are versioned now; absent adapters
// remain NOT_RUN or UNSUPPORTED_SCOPE rather than being inferred from a label.
func (r RuntimeResultRecord) Validate(manifests []ObligationManifest) error {
	if r.Version != 1 || !validRuntimeToken(r.Obligation) || !validRuntimeToken(r.Capability) || !validRuntimeToken(r.Scenario) {
		return ErrRuntimeResult
	}
	for _, value := range []string{r.Adapter.Package, r.Adapter.Symbol, r.Executable, r.DatabaseClass, r.PlatformClass, r.Artifact, r.MissingCapability, r.Reason} {
		if value != "" && !validRuntimeToken(value) {
			return ErrRuntimeResult
		}
	}
	known := false
	for _, m := range manifests {
		if m.ID == r.Obligation && m.Name == r.Scenario {
			known = slices.Contains(m.MissingCapabilities, r.Capability) || slices.ContainsFunc(completedModelCapabilities[:], func(pair runtimeCapabilityPair) bool {
				return pair.Obligation == m.ID && pair.Capability == r.Capability
			})
			if r.Capability == "runtime_schedule_adapter" {
				known = slices.ContainsFunc(CoverageSchedules(), func(s CoverageSchedule) bool { return s.ID == m.ID })
			}
		}
	}
	if !known {
		return ErrRuntimeResult
	}
	if r.Outcome == NotRun || r.Outcome == UnsupportedScope {
		if r.MissingCapability != r.Capability || r.EvidenceClass != "" || r.Adapter != (RuntimeAdapterSymbol{}) || r.Artifact != "" || r.Executable != "" || r.DatabaseClass != "" || r.PlatformClass != "" {
			return ErrRuntimeResult
		}
		return nil
	}
	if (r.Outcome != Pass && r.Outcome != Fail) || !validEvidenceClass(r.EvidenceClass) || r.MissingCapability != "" || r.Reason != "" {
		return ErrRuntimeResult
	}
	expected, _, ok := expectedRuntimeRecord(r)
	expected.Outcome = r.Outcome
	if !ok || r != expected {
		return ErrRuntimeResult
	}
	return nil
}

//go:embed testdata/runtime_results/*.v1.json testdata/runtime_results/artifacts/*.v1.json
var runtimeResultFiles embed.FS

// EmbeddedRuntimeResults loads records and verifies every retained artifact.
// It does not change any obligation manifest or promote partial evidence.
func EmbeddedRuntimeResults() (RuntimeResults, error) { return LoadRuntimeResults(runtimeResultFiles) }

func decodeRuntimeJSON(data []byte, target any) error {
	if len(data) > 1<<20 {
		return ErrRuntimeResult
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil || d.Decode(new(any)) != io.EOF {
		return ErrRuntimeResult
	}
	// Canonical field order also rejects duplicate keys and alternate spellings.
	canonical, err := json.Marshal(target)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, data) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return ErrRuntimeResult
	}
	return nil
}

// LoadRuntimeResults resolves opaque artifact IDs through the supplied retained
// filesystem. Duplicate identity is obligation/capability/scenario/class/build/
// database/platform: changing outcome or artifact cannot create another result.
func LoadRuntimeResults(fsys fs.FS) (RuntimeResults, error) {
	var results RuntimeResults
	data, err := fs.ReadFile(fsys, "testdata/runtime_results/results.v1.json")
	if err != nil || decodeRuntimeJSON(data, &results) != nil || results.Version != 1 || results.Kind != "runtime_results" || len(results.Records) == 0 {
		return RuntimeResults{}, ErrRuntimeResult
	}
	manifests, err := EmbeddedObligations()
	if err != nil {
		return RuntimeResults{}, ErrRuntimeResult
	}
	seen := map[[7]string]bool{}
	artifacts := map[string]bool{}
	for _, r := range results.Records {
		key := [7]string{r.Obligation, r.Capability, r.Scenario, string(r.EvidenceClass), r.Executable, r.DatabaseClass, r.PlatformClass}
		if r.Validate(manifests) != nil || seen[key] {
			return RuntimeResults{}, ErrRuntimeResult
		}
		seen[key] = true
		if r.Outcome == NotRun || r.Outcome == UnsupportedScope {
			continue
		}
		a, err := loadRuntimeArtifact(fsys, r.Artifact)
		_, checks, _ := expectedRuntimeRecord(r)
		if err != nil || a.Result != r || a.Checks != checks || artifacts[r.Artifact] {
			return RuntimeResults{}, ErrRuntimeResult
		}
		artifacts[r.Artifact] = true
	}
	files, err := fs.Glob(fsys, "testdata/runtime_results/artifacts/*.v1.json")
	if err != nil || len(files) != len(artifacts) {
		return RuntimeResults{}, ErrRuntimeResult
	}
	return results, nil
}

func loadRuntimeArtifact(fsys fs.FS, id string) (RuntimeArtifact, error) {
	var artifact RuntimeArtifact
	if !validRuntimeToken(id) {
		return artifact, ErrRuntimeResult
	}
	data, err := fs.ReadFile(fsys, "testdata/runtime_results/artifacts/"+id+".v1.json")
	if err != nil || decodeRuntimeJSON(data, &artifact) != nil || artifact.Version != 1 || artifact.ID != id {
		return RuntimeArtifact{}, ErrRuntimeResult
	}
	return artifact, nil
}

// VerifyRuntimeArtifact compares a fresh execution with the retained evidence.
// Callers must invoke their compiled adapter before supplying its observed result.
func VerifyRuntimeArtifact(observed RuntimeResultRecord, checks int) error {
	manifests, err := EmbeddedObligations()
	if err != nil || observed.Validate(manifests) != nil || (observed.Outcome != Pass && observed.Outcome != Fail) {
		return ErrRuntimeResult
	}
	artifact, err := loadRuntimeArtifact(runtimeResultFiles, observed.Artifact)
	if err != nil || !reflect.DeepEqual(artifact.Result, observed) || artifact.Checks != checks {
		return ErrRuntimeResult
	}
	return nil
}
