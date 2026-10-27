package syncmodel

import (
	"reflect"
	"slices"
	"testing"

	"github.com/witwave-ai/witself/internal/syncharness"
)

// Direct function bindings execute the exact scoped test; a string or source
// inventory match alone cannot satisfy an implemented manifest. Pure tests only.
func TestImplementedObligationBindings(t *testing.T) {
	bindings := map[string]func(*testing.T){
		"TestLegacyProjectionTypedScopeIsolation":            TestLegacyProjectionTypedScopeIsolation,
		"TestLegacyProjectionBackfillScenario":               TestLegacyProjectionBackfillScenario,
		"TestLegacyProjectionBackfillRetry":                  TestLegacyProjectionBackfillRetry,
		"TestLegacyProjectionBackfillAtomicConflict":         TestLegacyProjectionBackfillAtomicConflict,
		"TestLegacyProjectionDeletionAndNameReuse":           TestLegacyProjectionDeletionAndNameReuse,
		"TestLegacyProjectionDeletionBeforeBackfill":         TestLegacyProjectionDeletionBeforeBackfill,
		"TestLegacyProjectionAmbiguousCoordinatesAndScope":   TestLegacyProjectionAmbiguousCoordinatesAndScope,
		"TestLegacyProjectionInvalidMappings":                TestLegacyProjectionInvalidMappings,
		"TestOrderedEvidenceMembershipScenario":              TestOrderedEvidenceMembershipScenario,
		"TestEvidenceMembershipInterleaving":                 TestEvidenceMembershipInterleaving,
		"TestEvidenceMembershipRetryBackfillDeletion":        TestEvidenceMembershipRetryBackfillDeletion,
		"TestEvidenceMembershipRejectsForgedProof":           TestEvidenceMembershipRejectsForgedProof,
		"TestEvidenceMembershipProjectionAppendAndOwnership": TestEvidenceMembershipProjectionAppendAndOwnership,
		"TestEvidenceMembershipValueFreeBounds":              TestEvidenceMembershipValueFreeBounds,

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
		"H03": {"TestLegacyProjectionBackfillScenario", "TestLegacyProjectionBackfillRetry", "TestLegacyProjectionBackfillAtomicConflict", "TestLegacyProjectionDeletionAndNameReuse", "TestLegacyProjectionDeletionBeforeBackfill", "TestLegacyProjectionAmbiguousCoordinatesAndScope", "TestLegacyProjectionInvalidMappings", "TestLegacyProjectionTypedScopeIsolation"},
		"H05": {"TestOrderedEvidenceMembershipScenario", "TestEvidenceMembershipInterleaving", "TestEvidenceMembershipRetryBackfillDeletion", "TestEvidenceMembershipRejectsForgedProof", "TestEvidenceMembershipProjectionAppendAndOwnership", "TestEvidenceMembershipValueFreeBounds"},

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

// These expected resolutions are frozen independently of the adapter body and
// model fixture helpers. Counts increase only for completed matching assertions.
func observeLegacyProjectionScenario(run func() (LegacyProjectionObservations, error)) (int, error) {
	observed, err := run()
	if err != nil {
		return 0, err
	}
	scope := LegacyProjectionScope{
		Incarnation: Incarnation{AccountID: "account_fixture", Lifecycle: 1, Restore: RestoreMarker{1}},
		RealmID:     "realm_fixture", OwnerID: "owner_fixture", ObjectID: "conversation_fixture",
		Generation: GenerationID{1}, ProjectionID: "projection_legacy", ExternalName: "conversation_name",
	}
	original := LegacyProjectionRecord{Coordinate: LegacyCoordinate{LegacyTranscriptSequence, scope, 1}, Identity: LegacyIdentity{EntryID: "entry_original"}}
	memory := LegacyProjectionRecord{Coordinate: LegacyCoordinate{LegacyMemoryVersion, LegacyProjectionScope{
		Incarnation: scope.Incarnation, RealmID: "realm_fixture", OwnerID: "owner_fixture", ObjectID: "memory_fixture",
		Generation: GenerationID{1}, ProjectionID: "projection_legacy", ExternalName: "memory_name",
	}, 1}, Identity: LegacyIdentity{VersionID: RevisionID{1}}}
	deleted := original
	deleted.Deleted = true
	reused := LegacyProjectionRecord{Coordinate: LegacyCoordinate{LegacyTranscriptSequence, LegacyProjectionScope{
		Incarnation: scope.Incarnation, RealmID: "realm_fixture", OwnerID: "owner_fixture", ObjectID: "conversation_new",
		Generation: GenerationID{2}, ProjectionID: "projection_legacy", ExternalName: "conversation_name",
	}, 1}, Identity: LegacyIdentity{EntryID: "entry_new"}}
	if len(observed.OriginalAliases) != 2 || len(observed.RetriedAliases) != 2 {
		return 0, syncharness.ErrRuntimeResult
	}
	return completedModelAssertions(
		observed.OriginalAliases[0] == original, observed.OriginalAliases[1] == memory,
		observed.RetriedAliases[0] == original, observed.RetriedAliases[1] == memory,
		observed.DeletedAlias == deleted, observed.ReusedNameAlias == reused, observed.AmbiguityRefused,
	)
}

func observeEvidenceMembershipScenario(run func() (EvidenceMembershipObservations, error)) (int, error) {
	observed, err := run()
	if err != nil {
		return 0, err
	}
	expected := EvidenceMembershipProof{
		Scope:      EvidenceScope{Incarnation: Incarnation{AccountID: "account", Lifecycle: 1}, RealmID: "realm", OwnerID: "owner", ConversationID: "conversation", Generation: GenerationID{1}},
		EvidenceID: RevisionID{3}, SourceProjection: RevisionID{1}, First: 10, Last: 12,
		Members: []TranscriptEntryID{"entry_A", "entry_B", "entry_C"},
	}
	return completedModelAssertions(
		reflect.DeepEqual(observed.SealedMembership, expected),
		slices.Equal(observed.DestinationPositions, []Ordinal{20, 22, 24}),
		reflect.DeepEqual(observed.RetriedMembership, expected),
		slices.Equal(observed.UnavailableMembers, []TranscriptEntryID{"entry_B"}),
		observed.ForgedProofRefused, observed.OriginalProofValid,
	)
}

func completedModelAssertions(matches ...bool) (int, error) {
	checks := 0
	for _, match := range matches {
		if !match {
			return checks, syncharness.ErrRuntimeResult
		}
		checks++
	}
	if checks == 0 {
		return 0, syncharness.ErrRuntimeResult
	}
	return checks, nil
}

func observeModelRuntimeResult(id string, run func() (int, error)) (syncharness.RuntimeResultRecord, int) {
	observed := syncharness.RuntimeResultRecord{
		Version: 1, Obligation: id, Capability: "legacy_projection_backfill_model",
		EvidenceClass: syncharness.FixtureEvidence, Adapter: syncharness.RuntimeAdapterSymbol{Package: "syncmodel", Symbol: "RunLegacyProjectionBackfillScenario"},
		Scenario: "LegacyProjectionMigration", Executable: "lc_s6a_go_test_v1", DatabaseClass: "none", PlatformClass: "portable_go", Artifact: "H03_fixture_v1", Outcome: syncharness.Fail,
	}
	switch id {
	case "H03":
	case "H05":
		observed.Capability = "ordered_evidence_membership_model"
		observed.Adapter.Symbol = "RunOrderedEvidenceMembershipScenario"
		observed.Scenario = "EvidenceMembershipInterleaving"
		observed.Artifact = "H05_fixture_v1"
	default:
		return observed, 0
	}
	checks, err := run()
	if err == nil && checks > 0 {
		observed.Outcome = syncharness.Pass
	}
	return observed, checks
}

func TestRuntimeResultAdapterBindings(t *testing.T) {
	bindings := map[string]func() (int, error){
		"RunLegacyProjectionBackfillScenario": func() (int, error) {
			return observeLegacyProjectionScenario(RunLegacyProjectionBackfillScenario)
		},
		"RunOrderedEvidenceMembershipScenario": func() (int, error) {
			return observeEvidenceMembershipScenario(RunOrderedEvidenceMembershipScenario)
		},
	}
	results, err := syncharness.EmbeddedRuntimeResults()
	if err != nil {
		t.Fatal(err)
	}
	executed := 0
	for _, record := range results.Records {
		if record.Adapter.Package != "syncmodel" {
			continue
		}
		run, ok := bindings[record.Adapter.Symbol]
		if !ok {
			t.Fatal("runtime result lacks a compiled model adapter")
		}
		t.Run(record.Obligation, func(t *testing.T) {
			observed, checks := observeModelRuntimeResult(record.Obligation, run)
			if observed != record || syncharness.VerifyRuntimeArtifact(observed, checks) != nil {
				t.Fatal("executed model differs from retained runtime evidence")
			}
		})
		executed++
	}
	if executed != len(bindings) {
		t.Fatal("model runtime execution bindings incomplete")
	}
}

func TestRuntimeResultAdapterRejectsEmptyObservations(t *testing.T) {
	for id, run := range map[string]func() (int, error){
		"H03": func() (int, error) {
			return observeLegacyProjectionScenario(func() (LegacyProjectionObservations, error) {
				return LegacyProjectionObservations{}, nil
			})
		},
		"H05": func() (int, error) {
			return observeEvidenceMembershipScenario(func() (EvidenceMembershipObservations, error) {
				return EvidenceMembershipObservations{}, nil
			})
		},
	} {
		t.Run(id, func(t *testing.T) {
			observed, checks := observeModelRuntimeResult(id, run)
			if observed.Outcome != syncharness.Fail || checks != 0 || syncharness.VerifyRuntimeArtifact(observed, checks) == nil {
				t.Fatal("no-op adapter earned runtime evidence without observations")
			}
		})
	}
}
