package syncharness

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func runtimeFilesCopy(t *testing.T) fstest.MapFS {
	t.Helper()
	files := fstest.MapFS{}
	err := fs.WalkDir(runtimeResultFiles, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data, readErr := runtimeResultFiles.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			files[path] = &fstest.MapFile{Data: data}
		}
		return nil
	})
	if err != nil {
		t.Fatal("runtime evidence fixture unavailable")
	}
	return files
}

func writeRuntimeInventory(t *testing.T, files fstest.MapFS, results RuntimeResults) {
	t.Helper()
	data, err := json.Marshal(results)
	if err != nil {
		t.Fatal("runtime inventory encoding failed")
	}
	files["testdata/runtime_results/results.v1.json"] = &fstest.MapFile{Data: data}
}

func TestRuntimeResultsInventoryAndBoundaries(t *testing.T) {
	results, err := EmbeddedRuntimeResults()
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	fixturePasses, capturePasses, pending := 0, 0, 0
	for _, r := range results.Records {
		covered[r.Obligation+":"+r.Capability] = true
		switch r.Outcome {
		case Pass:
			switch r.EvidenceClass {
			case FixtureEvidence:
				fixturePasses++
				if r.DatabaseClass != "none" {
					t.Fatal("runtime evidence: fixture inventory claimed database execution")
				}
			case RealPostgresEvidence:
				capturePasses++
				if r.Adapter.Package != "store" || r.Executable != "lc_s6c1_go_test_v1" || r.DatabaseClass != "postgresql" {
					t.Fatal("runtime evidence: production capture allocation changed")
				}
			default:
				t.Fatal("runtime evidence: unexecuted broader evidence class claimed")
			}
		case NotRun:
			pending++
		default:
			t.Fatal("unexpected retained outcome")
		}
	}
	for _, m := range manifests {
		for _, capability := range m.MissingCapabilities {
			if !covered[m.ID+":"+capability] {
				t.Fatal("capability missing from runtime inventory")
			}
		}
	}
	// These two model capabilities are complete allocations, so they no longer
	// appear in the edited manifests' missing-capability lists.
	for _, pair := range []string{
		"H03:legacy_projection_backfill_model",
		"H05:ordered_evidence_membership_model",
	} {
		if !covered[pair] {
			t.Fatal("completed model capability missing from runtime inventory")
		}
	}
	if fixturePasses != 12 || capturePasses != 7 || pending != 74 || len(results.Records) != 93 {
		t.Fatal("runtime evidence inventory changed without review")
	}
}

func TestRuntimeResultValidateRequiresExecutedEvidence(t *testing.T) {
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal(err)
	}
	base, _, ok := fixtureRuntimeRecord("H03")
	if !ok || base.Validate(manifests) != nil {
		t.Fatal("executed fixture record must validate before removing evidence")
	}
	for _, test := range []struct {
		name   string
		mutate func(*RuntimeResultRecord)
	}{
		{"artifact_empty", func(r *RuntimeResultRecord) { r.Artifact = "" }},
		{"adapter_package_empty", func(r *RuntimeResultRecord) { r.Adapter.Package = "" }},
		{"adapter_symbol_empty", func(r *RuntimeResultRecord) { r.Adapter.Symbol = "" }},
		{"evidence_class_empty", func(r *RuntimeResultRecord) { r.EvidenceClass = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := base
			test.mutate(&r)
			// Exercise the public record boundary without a runtime-results
			// loader or retained-artifact lookup supplying a second refusal.
			if !errors.Is(r.Validate(manifests), ErrRuntimeResult) {
				t.Fatal("public record validator accepted missing execution evidence")
			}
		})
	}
}

func TestRuntimeResultsRejectFabricatedEvidence(t *testing.T) {
	for _, mutation := range []string{"fabricated_pass", "adapter", "unrelated_adapter", "package", "artifact_empty", "artifact_unknown", "artifact_deleted", "artifact_outcome", "artifact_checks", "class_empty", "class_upgrade", "unknown_class", "unknown_h", "unknown_capability", "wrong_capability_pair", "scenario", "build", "database", "platform", "duplicate", "conflicting_duplicate", "duplicate_not_run", "duplicate_not_run_reverse", "conflicting_pending_duplicate", "conflicting_pending_duplicate_reverse", "missing_capability", "not_run_proof", "outcome", "version", "orphan_artifact"} {
		t.Run(mutation, func(t *testing.T) {
			files := runtimeFilesCopy(t)
			results, err := EmbeddedRuntimeResults()
			if err != nil {
				t.Fatal(err)
			}
			index := slices.IndexFunc(results.Records, func(r RuntimeResultRecord) bool { return r.Outcome == Pass })
			r := &results.Records[index]
			switch mutation {
			case "fabricated_pass":
				results.Records[0].Outcome = Pass
				results.Records[0].MissingCapability = ""
			case "adapter":
				r.Adapter.Symbol = "NonexistentAdapter"
			case "unrelated_adapter":
				r.Adapter.Symbol = "RunCoverageScheduleModel"
			case "package":
				r.Adapter.Package = "syncharness"
			case "artifact_empty":
				r.Artifact = ""
			case "artifact_unknown":
				r.Artifact = "unretained_artifact"
			case "artifact_deleted":
				delete(files, "testdata/runtime_results/artifacts/"+r.Artifact+".v1.json")
			case "artifact_outcome", "artifact_checks":
				path := "testdata/runtime_results/artifacts/" + r.Artifact + ".v1.json"
				var a RuntimeArtifact
				if json.Unmarshal(files[path].Data, &a) != nil {
					t.Fatal("fixture decoding failed")
				}
				if mutation == "artifact_outcome" {
					a.Result.Outcome = Fail
				} else {
					a.Checks++
				}
				data, _ := json.Marshal(a)
				files[path] = &fstest.MapFile{Data: data}
			case "class_empty":
				r.EvidenceClass = ""
			case "class_upgrade":
				r.EvidenceClass = RealPostgresEvidence
			case "unknown_class":
				r.EvidenceClass = "unreviewed"
			case "unknown_h":
				r.Obligation = "H70"
			case "unknown_capability":
				r.Capability = "fabricated_capability"
			case "wrong_capability_pair":
				r.Capability = "ordered_evidence_membership_model"
			case "scenario":
				r.Scenario = "OperationMatrix"
			case "build":
				r.Executable = "unexecuted_build"
			case "database":
				r.DatabaseClass = "postgres_16"
			case "platform":
				r.PlatformClass = "native_windows"
			case "duplicate":
				results.Records = append(results.Records, *r)
			case "conflicting_duplicate":
				other := *r
				other.Outcome = Fail
				results.Records = append(results.Records, other)
			case "duplicate_not_run", "duplicate_not_run_reverse", "conflicting_pending_duplicate", "conflicting_pending_duplicate_reverse":
				pendingIndex := slices.IndexFunc(results.Records, func(record RuntimeResultRecord) bool { return record.Outcome == NotRun })
				if pendingIndex < 0 {
					t.Fatal("pending runtime fixture missing")
				}
				other := results.Records[pendingIndex]
				if strings.HasPrefix(mutation, "conflicting_pending_duplicate") {
					other.Outcome = UnsupportedScope
					other.MissingCapability = other.Capability
				}
				if strings.HasSuffix(mutation, "_reverse") {
					results.Records = append([]RuntimeResultRecord{other}, results.Records...)
				} else {
					results.Records = append(results.Records, other)
				}
			case "missing_capability":
				results.Records[0].MissingCapability = ""
			case "not_run_proof":
				results.Records[0].Adapter = r.Adapter
			case "outcome":
				r.Outcome = PostgresPass
			case "version":
				r.Version = 2
			case "orphan_artifact":
				files["testdata/runtime_results/artifacts/orphan.v1.json"] = &fstest.MapFile{Data: []byte("{}")}
			}
			writeRuntimeInventory(t, files, results)
			if _, err := LoadRuntimeResults(files); err == nil {
				t.Fatal("fabricated or incomplete evidence accepted")
			}
		})
	}
}

func TestRuntimeResultValueFreeFields(t *testing.T) {
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal(err)
	}
	base, _, _ := fixtureRuntimeRecord("H03")
	for _, value := range []string{strings.Repeat("a", 64), "x" + strings.Repeat("A", 64), "/private/file", "relative/file", `C:\private\file`, "https://example.invalid", "file.json", "parent..child"} {
		for field := 0; field < 10; field++ {
			r := base
			fields := []*string{&r.Obligation, &r.Capability, &r.Adapter.Package, &r.Adapter.Symbol, &r.Scenario, &r.Executable, &r.DatabaseClass, &r.PlatformClass, &r.Artifact, &r.MissingCapability}
			*fields[field] = value
			if r.Validate(manifests) == nil {
				t.Fatal("value-bearing runtime field accepted")
			}
		}
	}
}

func TestRuntimeResultStrictJSON(t *testing.T) {
	for _, mutation := range []string{"unknown", "duplicate", "trailing", "artifact_unknown"} {
		t.Run(mutation, func(t *testing.T) {
			files := runtimeFilesCopy(t)
			path := "testdata/runtime_results/results.v1.json"
			switch mutation {
			case "unknown":
				files[path].Data = bytes.Replace(files[path].Data, []byte(`"version": 1`), []byte(`"unexpected": true, "version": 1`), 1)
			case "duplicate":
				files[path].Data = bytes.Replace(files[path].Data, []byte(`"version": 1`), []byte(`"version": 1, "version": 1`), 1)
			case "trailing":
				files[path].Data = append(files[path].Data, []byte("{}")...)
			case "artifact_unknown":
				path = "testdata/runtime_results/artifacts/H03_fixture_v1.v1.json"
				files[path].Data = bytes.Replace(files[path].Data, []byte(`"version": 1`), []byte(`"unexpected": true, "version": 1`), 1)
			}
			if _, err := LoadRuntimeResults(files); err == nil {
				t.Fatal("malformed runtime evidence JSON accepted")
			}
		})
	}
}

func TestRuntimeResultOutcomeAndClassVocabulary(t *testing.T) {
	for _, class := range []EvidenceClass{FixtureEvidence, RealPostgresEvidence, MixedBinariesEvidence, NativeLifecycleEvidence, IndependentFailureDomainEvidence} {
		if !validEvidenceClass(class) {
			t.Fatal("versioned evidence class missing")
		}
	}
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal(err)
	}
	r, checks, _ := fixtureRuntimeRecord("H03")
	for _, class := range []EvidenceClass{RealPostgresEvidence, MixedBinariesEvidence, NativeLifecycleEvidence, IndependentFailureDomainEvidence} {
		upgraded := r
		upgraded.EvidenceClass = class
		if upgraded.Validate(manifests) == nil {
			t.Fatal("known evidence class accepted without an executing adapter")
		}
	}
	r.Outcome = Fail
	if r.Validate(manifests) != nil {
		t.Fatal("executed failure is not representable")
	}
	if VerifyRuntimeArtifact(r, checks) == nil {
		t.Fatal("executed failure matched a retained pass")
	}
	r = RuntimeResultRecord{Version: 1, Obligation: "H03", Capability: "production_domain_adapter", Scenario: "LegacyProjectionMigration", Outcome: UnsupportedScope, MissingCapability: "production_domain_adapter"}
	if r.Validate(manifests) != nil {
		t.Fatal("unsupported scope is not representable")
	}
}

func TestRuntimeResultsPreserveUnknownAccountingInventory(t *testing.T) {
	catalog := mustCatalog(t)
	expected := map[string]int{
		"accounts_operators_agents": 48, "avatars": 22, "backups_exports_imports": 8,
		"facts_subjects_candidates_aliases": 16, "inbound_outbound_email": 33,
		"memories_capture_curation_vectors_deletion": 32, "messages_complete_requests": 29,
		"preferences_consent": 5, "realms": 3, "secrets_vault": 27, "transcripts": 8, "usage_billing": 2,
	}
	counts := map[string]int{}
	for _, vector := range catalog.Accounting {
		counts[vector.Family]++
		if vector.OriginalEvents != nil || vector.HourCount != nil || vector.DayCount != nil || vector.ReplayWork != nil || vector.Evidence != ScaffoldOnly {
			t.Fatal("runtime evidence assigned unproved production accounting quantities")
		}
	}
	if len(catalog.Accounting) != 233 || !reflect.DeepEqual(counts, expected) {
		t.Fatal("production accounting inventory changed without reviewed expectations")
	}
}

func TestRuntimeResultsCannotPromoteObligations(t *testing.T) {
	before, err := EmbeddedObligations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EmbeddedRuntimeResults(); err != nil {
		t.Fatal(err)
	}
	after, err := EmbeddedObligations()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("runtime inventory changed obligation state")
	}
	for _, m := range after {
		for _, mutation := range []string{"runtime", "remaining"} {
			changed := m
			if mutation == "runtime" {
				changed.RuntimeStatus = Pass
			} else {
				changed.Remaining = Pass
			}
			if changed.Validate() == nil {
				t.Fatal("runtime promotion accepted without a separate reviewed implementation")
			}
		}
	}
}

func TestModelAllocationManifestsPreserveRuntimeScaffold(t *testing.T) {
	// Frozen from the LC-S5D manifests: model completion may change only the
	// two reviewed allocations, never the runtime status of any obligation.
	priorGroups := map[string]string{
		"I":             "H01 H10 H11 H14 H15 H16 H17 H18 H20 H21 H22 H23 H25 H27 H28 H31 H33 H34 H41 H42 H46 H47 H50 H54 H55 H56 H57 H59 H60 H61 H62 H63 H64 H66 H68",
		"NOT_RUN":       "H12 H65 H67 H69",
		"S":             "H02 H04 H06 H07 H08 H09 H13 H19 H32 H35 H36 H37 H38 H39 H40 H43 H44 H45 H52 H53",
		"SCAFFOLD_ONLY": "H03 H05 H24 H26 H29 H30 H48 H49 H51 H58",
	}
	prior := map[string]string{}
	for disposition, ids := range priorGroups {
		for _, id := range strings.Fields(ids) {
			if _, exists := prior[id]; exists {
				t.Fatal("duplicate obligation in frozen LC-S5D inventory")
			}
			prior[id] = disposition
		}
	}
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal(err)
	}
	if len(prior) != 69 || len(manifests) != 69 {
		t.Fatal("obligation allocation inventory incomplete")
	}
	changed := []string{}
	seen := map[string]bool{}
	for _, m := range manifests {
		if prior[m.ID] == "" || seen[m.ID] {
			t.Fatal("unexpected or duplicate obligation in allocation inventory")
		}
		seen[m.ID] = true
		if m.Disposition != prior[m.ID] {
			changed = append(changed, m.ID)
			if m.Disposition != "I" {
				t.Fatal("model completion changed to an unreviewed disposition")
			}
		}
		t.Run(m.ID, func(t *testing.T) {
			if m.RuntimeStatus != NotRun || m.Remaining != ScaffoldOnly {
				t.Fatal("model allocation promoted runtime scaffold state")
			}
			if m.ID == "H03" || m.ID == "H05" {
				if !slices.Equal(m.MissingCapabilities, []string{"production_domain_adapter"}) || len(m.Tests) == 0 {
					t.Fatal("completed model allocation did not retain its production gap and test bindings")
				}
			}
			data, err := obligationFiles.ReadFile("testdata/obligations/" + m.ID + ".v1.json")
			if err != nil {
				t.Fatal("obligation manifest fixture unavailable")
			}
			var decoded ObligationManifest
			if json.Unmarshal(data, &decoded) != nil || !reflect.DeepEqual(decoded, m) {
				t.Fatal("loaded allocation differs from the manifest itself")
			}
			var document map[string]json.RawMessage
			if json.Unmarshal(data, &document) != nil {
				t.Fatal("obligation manifest fixture decoding failed")
			}
			for _, status := range []EvidenceStatus{"", Pass, Fail, UnsupportedScope, ScaffoldOnly, "UNREVIEWED"} {
				name := string(status)
				if name == "" {
					name = "empty"
				}
				t.Run(name, func(t *testing.T) {
					statusJSON, err := json.Marshal(status)
					if err != nil {
						t.Fatal("runtime status encoding failed")
					}
					document["runtime_status"] = statusJSON
					edited, err := json.Marshal(document)
					if err != nil || json.Unmarshal(edited, &decoded) != nil {
						t.Fatal("edited obligation manifest decoding failed")
					}
					if !errors.Is(decoded.Validate(), ErrObligationManifest) {
						t.Fatal("edited manifest runtime status accepted without explicit reviewed promotion")
					}
				})
			}
		})
	}
	if !slices.Equal(changed, []string{"H03", "H05"}) {
		t.Fatal("model completion changed dispositions beyond the two reviewed allocations")
	}
}
