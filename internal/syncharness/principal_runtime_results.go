package syncharness

// principalRuntimeAllocation binds only the five C2a experiments. These are
// narrower than H43: no mixed binaries, installer provisioning or hostile
// administrator boundary is certified. Counts come from completed store cases.
type principalRuntimeAllocation struct {
	testName   string
	capability string
	checks     int
	disposable bool
}

var principalRuntimeAllocations = [...]principalRuntimeAllocation{
	{"TestSyncPrincipalSeparation", "production_principal_separation", 6, true},
	{"TestSyncPrincipalBypassDenied", "production_bypass_denial", 20, true},
	{"TestSyncPrincipalDowngradeDenied", "production_downgrade_denial", 4, true},
	{"TestSyncPrincipalCapabilityRevocation", "production_capability_revocation", 2, false},
	{"TestSyncPrincipalActivationTopology", "production_activation_topology", 2, false},
}

func principalRuntimeRecord(testName string) (RuntimeResultRecord, int, bool) {
	for _, allocation := range principalRuntimeAllocations {
		if allocation.testName != testName {
			continue
		}
		return RuntimeResultRecord{
			Version: 1, Obligation: "H43", Capability: allocation.capability,
			EvidenceClass: RealPostgresEvidence,
			Adapter:       RuntimeAdapterSymbol{Package: "store", Symbol: testName},
			Scenario:      "WriterGuardBypassAndDowngrade",
			Executable:    "lc_s6c2a_go_test_v1", DatabaseClass: "postgresql",
			PlatformClass: "go_test_host", Artifact: "H43_" + allocation.capability + "_v1",
			Outcome: Pass,
		}, allocation.checks, true
	}
	return RuntimeResultRecord{}, 0, false
}

func principalRuntimeCapability(obligation, capability string) bool {
	if obligation != "H43" {
		return false
	}
	for _, allocation := range principalRuntimeAllocations {
		if allocation.capability == capability {
			return true
		}
	}
	return false
}

func principalRuntimePendingReason(capability string) string {
	for _, allocation := range principalRuntimeAllocations {
		if allocation.capability == capability {
			if allocation.disposable {
				return "disposable_cluster_unavailable"
			}
			return "not_executed"
		}
	}
	return ""
}

// VerifyPrincipalRuntimeArtifact checks the named store adapter's complete
// observed case count and returns its value-free runtime receipt for retention
// in test output. The caller must execute every PostgreSQL assertion first;
// skipped, filtered or failed tests cannot emit this receipt. No committed CI
// PASS artifact is manufactured before the disposable experiment executes.
func VerifyPrincipalRuntimeArtifact(testName string, checks int) (RuntimeArtifact, error) {
	r, expectedChecks, ok := principalRuntimeRecord(testName)
	manifests, err := EmbeddedObligations()
	if !ok || checks != expectedChecks || err != nil || r.Validate(manifests) != nil {
		return RuntimeArtifact{}, ErrRuntimeResult
	}
	return RuntimeArtifact{Version: 1, ID: r.Artifact, Result: r, Checks: checks}, nil
}

// PrincipalRuntimeNotRun records an unavailable disposable-cluster experiment.
// The executing selector test logs its own name with this record; the record
// itself follows v1's rule that NOT_RUN carries no execution class or artifact.
// Owner-path cases cannot use cluster availability to excuse missing execution.
func PrincipalRuntimeNotRun(testName string) (RuntimeResultRecord, error) {
	for _, allocation := range principalRuntimeAllocations {
		if allocation.testName != testName || !allocation.disposable {
			continue
		}
		return RuntimeResultRecord{
			Version: 1, Obligation: "H43", Capability: allocation.capability,
			Scenario: "WriterGuardBypassAndDowngrade", Outcome: NotRun,
			MissingCapability: allocation.capability, Reason: "disposable_cluster_unavailable",
		}, nil
	}
	return RuntimeResultRecord{}, ErrRuntimeResult
}
