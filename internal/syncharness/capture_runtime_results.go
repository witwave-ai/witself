package syncharness

// captureRuntimeRecord binds only the seven production experiments in C1.
// Their callers are the named store tests, which verify their artifact after
// all cases execute against PostgreSQL. This is single-executable evidence;
// no principal separation, snapshot materialization, native lifecycle, or
// complete H obligation is certified by these records.
func captureRuntimeRecord(testName string) (RuntimeResultRecord, int, bool) {
	r := RuntimeResultRecord{
		Version: 1, EvidenceClass: RealPostgresEvidence,
		Adapter:    RuntimeAdapterSymbol{Package: "store", Symbol: testName},
		Executable: "lc_s6c1_go_test_v1", DatabaseClass: "postgresql",
		PlatformClass: "go_test_host", Outcome: Pass,
	}
	checks := 0
	switch testName {
	case "TestSyncCaptureInstallationDrain":
		r.Obligation, r.Capability, r.Scenario, checks = "H35", "production_relation_guards", "GuardInstallRelationDrain", 2
	case "TestSyncCaptureLateFirstWrite":
		r.Obligation, r.Capability, r.Scenario, checks = "H36", "production_writer_admission", "PreBeginLateFirstWrite", 1
	case "TestSyncCaptureStaleSnapshots":
		r.Obligation, r.Capability, r.Scenario, checks = "H37", "production_locking_admission", "OldSnapshotLockingAdmission", 4
	case "TestSyncCaptureAccountGate":
		r.Obligation, r.Capability, r.Scenario, checks = "H38", "production_account_gate", "AccountGateDrain", 2
	case "TestSyncCaptureSnapshotSession":
		r.Obligation, r.Capability, r.Scenario, checks = "H39", "same_physical_session_snapshot", "SnapshotPinSameSessionAck", 3
	case "TestSyncCaptureForgottenJournal":
		r.Obligation, r.Capability, r.Scenario, checks = "H41", "complete_production_writer_inventory", "OmittedSemanticJournal", 5
	case "TestSyncCaptureCommittedPrefix":
		r.Obligation, r.Capability, r.Scenario, checks = "H42", "production_committed_prefix", "CommittedPrefixCounter", 2
	default:
		return RuntimeResultRecord{}, 0, false
	}
	r.Artifact = r.Obligation + "_capture_postgres_v1"
	return r, checks, true
}

func expectedRuntimeRecord(r RuntimeResultRecord) (RuntimeResultRecord, int, bool) {
	if r.Adapter.Package == "store" {
		return captureRuntimeRecord(r.Adapter.Symbol)
	}
	return fixtureRuntimeRecord(r.Obligation)
}

// VerifyCaptureRuntimeArtifact is called by the exact named store test only
// after its PostgreSQL cases pass. checks is the number of completed scenario
// cases, not a caller-supplied claim of table or operation coverage. A skipped
// database test must not call this function or emit a passing record.
func VerifyCaptureRuntimeArtifact(testName string, checks int) (RuntimeResultRecord, error) {
	r, expectedChecks, ok := captureRuntimeRecord(testName)
	if !ok || checks != expectedChecks || VerifyRuntimeArtifact(r, checks) != nil {
		return RuntimeResultRecord{}, ErrRuntimeResult
	}
	return r, nil
}
