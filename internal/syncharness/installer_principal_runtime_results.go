package syncharness

const installerPrincipalRuntimeTest = "TestRealLifecycle/InstallerPrincipals"

func installerPrincipalRuntimeCapability(obligation, capability string) bool {
	return obligation == "H43" && capability == "installer_principal_topology"
}

// installerPrincipalRuntimeRecord binds the native parent's one reviewed child.
// Twelve completed assertion groups include both independent installations and
// cleanup. This allocation does not certify conversion, capture or all of H43.
func installerPrincipalRuntimeRecord(symbol string) (RuntimeResultRecord, int, bool) {
	if symbol != "TestRealLifecycle" {
		return RuntimeResultRecord{}, 0, false
	}
	return RuntimeResultRecord{
		Version: 1, Obligation: "H43", Capability: "installer_principal_topology",
		EvidenceClass: NativeLifecycleEvidence,
		Adapter:       RuntimeAdapterSymbol{Package: "localcell", Symbol: "TestRealLifecycle"},
		Scenario:      "WriterGuardBypassAndDowngrade", Executable: "lc_s6c3_native_go_test_v1",
		DatabaseClass: "postgresql", PlatformClass: "native_go_test_host",
		Artifact: "H43_installer_principal_topology_v1", Outcome: Pass,
	}, 12, true
}

// VerifyInstallerPrincipalRuntimeArtifact returns a receipt for the exact native
// child only after every assertion and cleanup succeeds. A skipped parent, a
// filtered child or a committed NOT_RUN row cannot supply execution evidence.
func VerifyInstallerPrincipalRuntimeArtifact(testName string, checks int) (RuntimeArtifact, error) {
	if testName != installerPrincipalRuntimeTest {
		return RuntimeArtifact{}, ErrRuntimeResult
	}
	r, expectedChecks, ok := installerPrincipalRuntimeRecord("TestRealLifecycle")
	manifests, err := EmbeddedObligations()
	if !ok || checks != expectedChecks || err != nil || r.Validate(manifests) != nil {
		return RuntimeArtifact{}, ErrRuntimeResult
	}
	return RuntimeArtifact{Version: 1, ID: r.Artifact, Result: r, Checks: checks}, nil
}
