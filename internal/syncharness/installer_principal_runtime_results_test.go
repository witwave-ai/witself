package syncharness

import (
	"errors"
	"testing"
)

func TestInstallerPrincipalRuntimeResults(t *testing.T) {
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal("installer evidence obligation inventory unavailable")
	}
	base, checks, ok := installerPrincipalRuntimeRecord("TestRealLifecycle")
	if !ok || base.Validate(manifests) != nil {
		t.Fatal("installer evidence reviewed native allocation rejected")
	}
	for _, mutate := range []func(*RuntimeResultRecord){
		func(r *RuntimeResultRecord) { r.Obligation = "H34" },
		func(r *RuntimeResultRecord) { r.Capability = "production_principal_separation" },
		func(r *RuntimeResultRecord) { r.Scenario = "CoverageInventoryAndSchedule" },
		func(r *RuntimeResultRecord) { r.Adapter.Package = "store" },
		func(r *RuntimeResultRecord) { r.Adapter.Symbol = "InstallerPrincipals" },
		func(r *RuntimeResultRecord) { r.Adapter.Symbol = "TestRealLifecycleOther" },
		func(r *RuntimeResultRecord) { r.EvidenceClass = RealPostgresEvidence },
		func(r *RuntimeResultRecord) { r.EvidenceClass = FixtureEvidence },
		func(r *RuntimeResultRecord) { r.EvidenceClass = MixedBinariesEvidence },
		func(r *RuntimeResultRecord) { r.Executable = "unexecuted_build" },
		func(r *RuntimeResultRecord) { r.DatabaseClass = "none" },
		func(r *RuntimeResultRecord) { r.PlatformClass = "portable_go" },
		func(r *RuntimeResultRecord) { r.Artifact = "unretained_artifact" },
		func(r *RuntimeResultRecord) { r.Reason = "native_lifecycle_not_executed" },
	} {
		changed := base
		mutate(&changed)
		if !errors.Is(changed.Validate(manifests), ErrRuntimeResult) {
			t.Fatal("installer evidence fabricated binding or execution scope accepted")
		}
	}
	for _, name := range []string{"TestRealLifecycle", "InstallerPrincipals", "TestRealLifecycle/InstallerPrincipals/partial", "TestRealLifecycle/Other"} {
		if _, err := VerifyInstallerPrincipalRuntimeArtifact(name, checks); !errors.Is(err, ErrRuntimeResult) {
			t.Fatal("installer evidence missing or filtered native child accepted")
		}
	}
	for _, count := range []int{0, checks - 1, checks + 1} {
		if _, err := VerifyInstallerPrincipalRuntimeArtifact(installerPrincipalRuntimeTest, count); !errors.Is(err, ErrRuntimeResult) {
			t.Fatal("installer evidence incomplete or invented count accepted")
		}
	}
	// This is receipt-shape validation, never a retained native execution claim.
	artifact, err := VerifyInstallerPrincipalRuntimeArtifact(installerPrincipalRuntimeTest, checks)
	if err != nil || artifact.Version != 1 || artifact.Result != base || artifact.ID != base.Artifact || artifact.Checks != checks {
		t.Fatal("installer evidence native receipt differs from allocation")
	}
	if !errors.Is(VerifyRuntimeArtifact(base, checks), ErrRuntimeResult) {
		t.Fatal("installer evidence unexecuted native artifact was manufactured")
	}
}

func TestInstallerPrincipalRuntimeResultsRemainPending(t *testing.T) {
	results, err := EmbeddedRuntimeResults()
	if err != nil {
		t.Fatal("installer evidence runtime inventory unavailable")
	}
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal("installer evidence obligation inventory unavailable")
	}
	found := 0
	for _, r := range results.Records {
		if !installerPrincipalRuntimeCapability(r.Obligation, r.Capability) {
			continue
		}
		found++
		if r.Outcome != NotRun || r.Reason != "native_lifecycle_not_executed" || r.MissingCapability != r.Capability || r.Validate(manifests) != nil {
			t.Fatal("installer evidence pending baseline claimed native execution")
		}
		for _, mutate := range []func(*RuntimeResultRecord){
			func(r *RuntimeResultRecord) { r.Outcome = Pass },
			func(r *RuntimeResultRecord) { r.EvidenceClass = NativeLifecycleEvidence },
			func(r *RuntimeResultRecord) { r.Adapter = RuntimeAdapterSymbol{"localcell", "TestRealLifecycle"} },
			func(r *RuntimeResultRecord) { r.Reason = "not_executed" },
			func(r *RuntimeResultRecord) { r.Artifact = "H43_installer_principal_topology_v1" },
		} {
			changed := r
			mutate(&changed)
			if !errors.Is(changed.Validate(manifests), ErrRuntimeResult) {
				t.Fatal("installer evidence pending baseline accepted fabricated receipt")
			}
		}
	}
	if found != 1 {
		t.Fatal("installer evidence exact native allocation missing or duplicated")
	}
}
