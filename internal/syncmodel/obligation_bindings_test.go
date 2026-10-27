package syncmodel

import (
	"slices"
	"testing"

	"github.com/witwave-ai/witself/internal/syncharness"
)

// Direct function bindings execute the exact scoped test; a string or source
// inventory match alone cannot satisfy an implemented manifest. Pure tests only.
func TestImplementedObligationBindings(t *testing.T) {
	bindings := map[string]func(*testing.T){
		"TestAbortPublicationRestoreRace":                               TestAbortPublicationRestoreRace,
		"TestAdoptionJoinEnrollment":                                    TestAdoptionJoinEnrollment,
		"TestAuthorityControlPinsAreSeparateAndRestartRollsBackWriters": TestAuthorityControlPinsAreSeparateAndRestartRollsBackWriters,
		"TestAuthorityTransferCommitPinCrashMatrix":                     TestAuthorityTransferCommitPinCrashMatrix,
		"TestAuthorityTransferImmutableBindingAndProof":                 TestAuthorityTransferImmutableBindingAndProof,
		"TestBoundedMalformedAdmission":                                 TestBoundedMalformedAdmission,
		"TestBoundedResourceAdmission":                                  TestBoundedResourceAdmission,
		"TestCanonicalEnvelopeGoldenVectors":                            TestCanonicalEnvelopeGoldenVectors,
		"TestCaptureOmittedSemanticJournal":                             TestCaptureOmittedSemanticJournal,
		"TestCompoundAuthorityDependenciesAndAlteredFragmentsRefuse":    TestCompoundAuthorityDependenciesAndAlteredFragmentsRefuse,
		"TestCompoundNotificationBeforeLink":                            TestCompoundNotificationBeforeLink,
		"TestDatabaseVsWholeDiskRestoreDomain":                          TestDatabaseVsWholeDiskRestoreDomain,
		"TestDestinationPolicyModes":                                    TestDestinationPolicyModes,
		"TestEnvelopeAdmissionNegative":                                 TestEnvelopeAdmissionNegative,
		"TestGrantDeletionClockHorizon":                                 TestGrantDeletionClockHorizon,
		"TestGraphFreezeMutationMatrix":                                 TestGraphFreezeMutationMatrix,
		"TestIndependentCompleteEnvelopeGoldenVectors":                  TestIndependentCompleteEnvelopeGoldenVectors,
		"TestIndistinguishableWholeSystemRollback":                      TestIndistinguishableWholeSystemRollback,
		"TestLogicalAccountingIdentityProvenanceAndScope":               TestLogicalAccountingIdentityProvenanceAndScope,
		"TestLogicalAccountingReplayAndRebuild":                         TestLogicalAccountingReplayAndRebuild,
		"TestPayloadRetirementCommitmentFloorModel":                     TestPayloadRetirementCommitmentFloorModel,
		"TestReceiptChainSealedMembership":                              TestReceiptChainSealedMembership,
		"TestReceiptExactSignedBytesAndPayloadFreeBasis":                TestReceiptExactSignedBytesAndPayloadFreeBasis,
		"TestReceiptExpectedAcceptanceAndAuthorityCoordinates":          TestReceiptExpectedAcceptanceAndAuthorityCoordinates,
		"TestRecoveryIntervalCoverageAndAtomicHoles":                    TestRecoveryIntervalCoverageAndAtomicHoles,
		"TestRecoveryIntervalsOverridePrecedence":                       TestRecoveryIntervalsOverridePrecedence,
		"TestRecoveryMandatoryOverrides":                                TestRecoveryMandatoryOverrides,
		"TestRecoveryRJPTruthTable":                                     TestRecoveryRJPTruthTable,
		"TestRecoveryReplicaOutboxAndInputOnlyReceipt":                  TestRecoveryReplicaOutboxAndInputOnlyReceipt,
		"TestRestoreAbortedAllocationsAndExternalAuthority":             TestRestoreAbortedAllocationsAndExternalAuthority,
		"TestRestoreMarkerCrashMatrix":                                  TestRestoreMarkerCrashMatrix,
		"TestSignedCursorFieldMutation":                                 TestSignedCursorFieldMutation,
		"TestTransferredExecutionDisposition":                           TestTransferredExecutionDisposition,
		"TestTransitionCrashResumeAndPartialCopy":                       TestTransitionCrashResumeAndPartialCopy,
		"TestUnavailableTargetNeverTimesIntoAuthority":                  TestUnavailableTargetNeverTimesIntoAuthority,
		"TestUnresolvedBeyondHorizon":                                   TestUnresolvedBeyondHorizon,
	}
	// Reviewed per-obligation bindings prevent a real but unrelated test from
	// being substituted for the required model invariant.
	expected := map[string][]string{
		"H11": {"TestAdoptionJoinEnrollment", "TestTransitionCrashResumeAndPartialCopy"},
		"H14": {"TestCompoundNotificationBeforeLink"},
		"H15": {"TestGraphFreezeMutationMatrix"},
		"H16": {"TestAuthorityTransferImmutableBindingAndProof"},
		"H17": {"TestAuthorityTransferCommitPinCrashMatrix"},
		"H18": {"TestCompoundAuthorityDependenciesAndAlteredFragmentsRefuse"},
		"H20": {"TestUnavailableTargetNeverTimesIntoAuthority"},
		"H21": {"TestAbortPublicationRestoreRace"},
		"H22": {"TestTransferredExecutionDisposition"},
		"H23": {"TestEnvelopeAdmissionNegative"},
		"H25": {"TestCanonicalEnvelopeGoldenVectors", "TestIndependentCompleteEnvelopeGoldenVectors"},
		"H27": {"TestReceiptExactSignedBytesAndPayloadFreeBasis"},
		"H28": {"TestPayloadRetirementCommitmentFloorModel"},
		"H31": {"TestBoundedMalformedAdmission", "TestBoundedResourceAdmission"},
		"H41": {"TestCaptureOmittedSemanticJournal"},
		"H46": {"TestRecoveryRJPTruthTable"},
		"H50": {"TestRestoreMarkerCrashMatrix", "TestRestoreAbortedAllocationsAndExternalAuthority"},
		"H54": {"TestSignedCursorFieldMutation", "TestReceiptExpectedAcceptanceAndAuthorityCoordinates"},
		"H55": {"TestReceiptChainSealedMembership"},
		"H56": {"TestAuthorityControlPinsAreSeparateAndRestartRollsBackWriters"},
		"H57": {"TestRecoveryRJPTruthTable", "TestRecoveryIntervalCoverageAndAtomicHoles", "TestRecoveryReplicaOutboxAndInputOnlyReceipt", "TestRecoveryIntervalsOverridePrecedence"},
		"H59": {"TestIndistinguishableWholeSystemRollback"},
		"H60": {"TestRecoveryIntervalCoverageAndAtomicHoles", "TestRecoveryMandatoryOverrides"},
		"H61": {"TestUnresolvedBeyondHorizon"},
		"H62": {"TestLogicalAccountingReplayAndRebuild"},
		"H63": {"TestDatabaseVsWholeDiskRestoreDomain"},
		"H64": {"TestLogicalAccountingReplayAndRebuild", "TestLogicalAccountingIdentityProvenanceAndScope"},
		"H66": {"TestDestinationPolicyModes"},
		"H68": {"TestGrantDeletionClockHorizon"},
	}
	manifests, err := syncharness.EmbeddedObligations()
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	seenIDs := map[string]bool{}
	for _, manifest := range manifests {
		if manifest.Disposition != "I" {
			continue
		}
		var names []string
		for _, test := range manifest.Tests {
			if test.Package == "syncmodel" {
				names = append(names, test.Name)
			}
		}
		if len(names) > 0 {
			if !slices.Equal(names, expected[manifest.ID]) {
				t.Fatalf("%s implementation binding differs from reviewed scoped evidence", manifest.ID)
			}
			seenIDs[manifest.ID] = true
		}
		for _, test := range manifest.Tests {
			if test.Package != "syncmodel" {
				continue
			}
			run, ok := bindings[test.Name]
			if !ok {
				t.Fatalf("%s lacks compiled execution binding", manifest.ID)
			}
			used[test.Name] = true
			t.Run(manifest.ID+"/"+test.Name, run)
		}
	}
	if len(used) != len(bindings) || len(seenIDs) != len(expected) {
		t.Fatal("orphaned obligation test binding")
	}
}
