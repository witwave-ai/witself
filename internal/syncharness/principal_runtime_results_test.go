package syncharness

import (
	"encoding/json"
	"errors"
	"testing"
	"testing/fstest"
)

func TestPrincipalRuntimeResultsRejectScopeAndIdentityChanges(t *testing.T) {
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal("principal evidence: obligation inventory unavailable")
	}
	for _, allocation := range principalRuntimeAllocations {
		t.Run(allocation.testName, func(t *testing.T) {
			base, checks, ok := principalRuntimeRecord(allocation.testName)
			if !ok || base.Validate(manifests) != nil {
				t.Fatal("principal evidence: reviewed production allocation rejected")
			}
			for _, mutate := range []func(*RuntimeResultRecord){
				func(r *RuntimeResultRecord) { r.Obligation = "H34" },
				func(r *RuntimeResultRecord) { r.Capability = "real_database_roles" },
				func(r *RuntimeResultRecord) { r.Scenario = "CoverageInventoryAndSchedule" },
				func(r *RuntimeResultRecord) { r.EvidenceClass = FixtureEvidence },
				func(r *RuntimeResultRecord) { r.EvidenceClass = MixedBinariesEvidence },
				func(r *RuntimeResultRecord) { r.EvidenceClass = NativeLifecycleEvidence },
				func(r *RuntimeResultRecord) { r.Adapter.Package = "syncharness" },
				func(r *RuntimeResultRecord) { r.Adapter.Symbol = "TestSyncPrincipalUnexecuted" },
				func(r *RuntimeResultRecord) { r.Executable = "unexecuted_build" },
				func(r *RuntimeResultRecord) { r.DatabaseClass = "none" },
				func(r *RuntimeResultRecord) { r.PlatformClass = "native_windows" },
				func(r *RuntimeResultRecord) { r.Artifact = "unretained_artifact" },
				func(r *RuntimeResultRecord) { r.Reason = "disposable_cluster_unavailable" },
			} {
				changed := base
				mutate(&changed)
				if !errors.Is(changed.Validate(manifests), ErrRuntimeResult) {
					t.Fatal("principal evidence: changed execution scope or identity accepted")
				}
			}
			for _, count := range []int{0, checks - 1, checks + 1} {
				if _, err := VerifyPrincipalRuntimeArtifact(allocation.testName, count); !errors.Is(err, ErrRuntimeResult) {
					t.Fatal("principal evidence: incomplete or invented case count accepted")
				}
			}
			// This verifies the receipt shape, not PostgreSQL behavior. Only the
			// named store test may retain its result as executed evidence.
			artifact, err := VerifyPrincipalRuntimeArtifact(allocation.testName, checks)
			if err != nil || artifact.Version != 1 || artifact.Result != base || artifact.ID != base.Artifact || artifact.Checks != checks {
				t.Fatal("principal evidence: runtime receipt differs from allocation")
			}
		})
	}
	for _, name := range []string{"TestSyncPrincipalUnexecuted", "TestSyncPrincipalSeparation/worker"} {
		if _, err := VerifyPrincipalRuntimeArtifact(name, 6); !errors.Is(err, ErrRuntimeResult) {
			t.Fatal("principal evidence: unregistered or filtered binding accepted")
		}
	}
}

func TestPrincipalRuntimeNotRunCannotClaimExecution(t *testing.T) {
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal("principal evidence: obligation inventory unavailable")
	}
	for _, allocation := range principalRuntimeAllocations {
		r, err := PrincipalRuntimeNotRun(allocation.testName)
		if !allocation.disposable {
			if !errors.Is(err, ErrRuntimeResult) {
				t.Fatal("principal evidence: owner path escaped through disposable refusal")
			}
			continue
		}
		if err != nil || r.Validate(manifests) != nil || r.Outcome != NotRun || r.Reason != "disposable_cluster_unavailable" || r.MissingCapability != allocation.capability {
			t.Fatal("principal evidence: disposable refusal record changed")
		}
		for _, mutate := range []func(*RuntimeResultRecord){
			func(r *RuntimeResultRecord) { r.Outcome = Pass },
			func(r *RuntimeResultRecord) { r.EvidenceClass = RealPostgresEvidence },
			func(r *RuntimeResultRecord) { r.Adapter = RuntimeAdapterSymbol{"store", allocation.testName} },
			func(r *RuntimeResultRecord) { r.Executable = "lc_s6c2a_go_test_v1" },
			func(r *RuntimeResultRecord) { r.Artifact = "unexecuted_artifact" },
			func(r *RuntimeResultRecord) { r.MissingCapability = "" },
			func(r *RuntimeResultRecord) { r.Reason = "not_executed" },
		} {
			changed := r
			mutate(&changed)
			if !errors.Is(changed.Validate(manifests), ErrRuntimeResult) {
				t.Fatal("principal evidence: unavailable experiment claimed execution")
			}
		}
	}
	if _, err := PrincipalRuntimeNotRun("TestSyncPrincipalUnexecuted"); !errors.Is(err, ErrRuntimeResult) {
		t.Fatal("principal evidence: unknown unavailable binding accepted")
	}
}

func TestPrincipalRuntimeRetainsUnexecutedCIAsNotRun(t *testing.T) {
	results, err := EmbeddedRuntimeResults()
	if err != nil {
		t.Fatal("principal evidence: runtime inventory unavailable")
	}
	found := 0
	for _, r := range results.Records {
		if !principalRuntimeCapability(r.Obligation, r.Capability) {
			continue
		}
		found++
		for _, allocation := range principalRuntimeAllocations {
			if allocation.capability != r.Capability {
				continue
			}
			if allocation.disposable {
				expected, err := PrincipalRuntimeNotRun(allocation.testName)
				if err != nil || r != expected {
					t.Fatal("principal evidence: retained CI result claims unavailable execution")
				}
				continue
			}
			expected, checks, ok := principalRuntimeRecord(allocation.testName)
			if !ok || r != expected || VerifyRuntimeArtifact(r, checks) != nil {
				t.Fatal("principal evidence: retained owner execution receipt changed")
			}
		}
	}
	if found != len(principalRuntimeAllocations) {
		t.Fatal("principal evidence: runtime capability allocation incomplete")
	}
}

func TestPrincipalRuntimeRetainedArtifactsRejectChangedChecks(t *testing.T) {
	for _, name := range []string{"TestSyncPrincipalActivationTopology", "TestSyncPrincipalCapabilityRevocation"} {
		t.Run(name, func(t *testing.T) {
			r, _, ok := principalRuntimeRecord(name)
			if !ok {
				t.Fatal("principal evidence: retained allocation unavailable")
			}
			files := runtimeFilesCopy(t)
			path := "testdata/runtime_results/artifacts/" + r.Artifact + ".v1.json"
			var artifact RuntimeArtifact
			if json.Unmarshal(files[path].Data, &artifact) != nil {
				t.Fatal("principal evidence: retained artifact unavailable")
			}
			artifact.Checks--
			data, err := json.Marshal(artifact)
			if err != nil {
				t.Fatal("principal evidence: retained artifact encoding failed")
			}
			files[path] = &fstest.MapFile{Data: data}
			if _, err := LoadRuntimeResults(files); !errors.Is(err, ErrRuntimeResult) {
				t.Fatal("principal evidence: incomplete retained owner execution accepted")
			}
		})
	}
}
