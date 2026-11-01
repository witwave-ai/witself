package syncharness

import (
	"encoding/json"
	"errors"
	"testing"
	"testing/fstest"
)

// These tests check evidence validation only; the named store tests provide the
// compiled execution bindings and actual PostgreSQL case counts.
func TestCaptureRuntimeResultsRejectScopeAndIdentityChanges(t *testing.T) {
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal("capture evidence: obligation inventory unavailable")
	}
	for _, name := range []string{
		"TestSyncCaptureInstallationDrain", "TestSyncCaptureLateFirstWrite",
		"TestSyncCaptureStaleSnapshots", "TestSyncCaptureAccountGate",
		"TestSyncCaptureSnapshotSession", "TestSyncCaptureForgottenJournal",
		"TestSyncCaptureCommittedPrefix",
	} {
		t.Run(name, func(t *testing.T) {
			base, checks, ok := captureRuntimeRecord(name)
			if !ok || base.Validate(manifests) != nil {
				t.Fatal("capture evidence: reviewed production allocation rejected")
			}
			for _, mutate := range []func(*RuntimeResultRecord){
				func(r *RuntimeResultRecord) { r.EvidenceClass = MixedBinariesEvidence },
				func(r *RuntimeResultRecord) { r.EvidenceClass = NativeLifecycleEvidence },
				func(r *RuntimeResultRecord) { r.Adapter.Symbol = "TestSyncCaptureUnexecuted" },
				func(r *RuntimeResultRecord) { r.Executable = "unexecuted_build" },
				func(r *RuntimeResultRecord) { r.DatabaseClass = "none" },
				func(r *RuntimeResultRecord) { r.PlatformClass = "native_windows" },
				func(r *RuntimeResultRecord) { r.Artifact = "unretained_artifact" },
				func(r *RuntimeResultRecord) { r.Reason = "skipped" },
			} {
				changed := base
				mutate(&changed)
				if !errors.Is(changed.Validate(manifests), ErrRuntimeResult) {
					t.Fatal("capture evidence: changed execution scope or identity accepted")
				}
			}
			if _, err := VerifyCaptureRuntimeArtifact(name, checks-1); !errors.Is(err, ErrRuntimeResult) {
				t.Fatal("capture evidence: incomplete scenario count accepted")
			}
			files := runtimeFilesCopy(t)
			path := "testdata/runtime_results/artifacts/" + base.Artifact + ".v1.json"
			var artifact RuntimeArtifact
			if json.Unmarshal(files[path].Data, &artifact) != nil {
				t.Fatal("capture evidence: retained artifact unavailable")
			}
			artifact.Checks--
			data, err := json.Marshal(artifact)
			if err != nil {
				t.Fatal("capture evidence: artifact encoding failed")
			}
			files[path] = &fstest.MapFile{Data: data}
			if _, err := LoadRuntimeResults(files); !errors.Is(err, ErrRuntimeResult) {
				t.Fatal("capture evidence: retained incomplete artifact accepted")
			}
		})
	}
	if _, err := VerifyCaptureRuntimeArtifact("TestSyncCaptureUnexecuted", 1); !errors.Is(err, ErrRuntimeResult) {
		t.Fatal("capture evidence: unregistered execution binding accepted")
	}
}

func TestCaptureRuntimePreparedCaseRemainsNotRun(t *testing.T) {
	results, err := EmbeddedRuntimeResults()
	if err != nil {
		t.Fatal("capture evidence: runtime inventory unavailable")
	}
	found := false
	for _, r := range results.Records {
		if r.Obligation != "H35" || r.Capability != "prepared_transactions" {
			continue
		}
		found = true
		if r.Outcome != NotRun || r.Reason != "max_prepared_transactions_0" || r.Adapter != (RuntimeAdapterSymbol{}) || r.Artifact != "" || r.EvidenceClass != "" {
			t.Fatal("capture evidence: disabled prepared transactions claimed execution")
		}
	}
	if !found {
		t.Fatal("capture evidence: prepared transaction limitation omitted")
	}
}
