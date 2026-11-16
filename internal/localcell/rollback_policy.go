package localcell

// rollbackEvidence is a caller assertion, not authentication. This decision
// neither verifies component sets nor manufactures evidence from their data.
type rollbackEvidence string

const (
	rollbackEvidenceMissing  rollbackEvidence = ""
	rollbackEvidenceVerified rollbackEvidence = "verified_complete_component_set"
)

type rollbackTarget struct {
	rollbackEvidence               rollbackEvidence
	rollbackSequence               uint64
	rollbackOS, rollbackArch       string
	rollbackCoreSchema             int
	rollbackSyncSelected           bool
	rollbackSyncSchema             int
	rollbackLayout                 int
	rollbackPrincipalLayout        int
	rollbackPostgreSQLMajor        string
	rollbackABI                    string
	rollbackServer, rollbackWorker bool
	rollbackPostgreSQL             bool
}

type rollbackApplied struct {
	rollbackOS, rollbackArch string
	rollbackCoreSchema       int
	rollbackSyncSelected     bool
	rollbackSyncSchema       int
	rollbackLayout           int
	rollbackPrincipalLayout  int
	rollbackPostgreSQLMajor  string
	rollbackABI              string
	rollbackHighWater        uint64
}

type rollbackPolicy struct {
	rollbackPermitted bool
	rollbackSequence  uint64
	rollbackTargets   []uint64
}

type rollbackReason string

const (
	rollbackAllowed       rollbackReason = ""
	rollbackIncomplete    rollbackReason = "incomplete"
	rollbackHighWater     rollbackReason = "high_water"
	rollbackCurrentPolicy rollbackReason = "current_policy"
	rollbackPlatform      rollbackReason = "platform"
	rollbackPGMajor       rollbackReason = "pg_major"
	rollbackABI           rollbackReason = "abi"
	rollbackLayout        rollbackReason = "layout"
	rollbackSchemaOlder   rollbackReason = "schema_older"
	rollbackSchemaNewer   rollbackReason = "schema_newer"
	rollbackSyncSchema    rollbackReason = "sync_schema"
)

type rollbackDecision struct {
	rollbackAllowed   bool
	rollbackReason    rollbackReason
	rollbackHighWater uint64
}

// rollbackEvaluate is a dark, effect-free compatibility decision. The policy
// must belong to the recorded high-water release; neither a target's policy nor
// a future release's policy can authorize the return. Schema equality is strict.
func rollbackEvaluate(rollbackCandidate rollbackTarget, rollbackState rollbackApplied, rollbackView rollbackPolicy) rollbackDecision {
	rollbackResult := rollbackDecision{rollbackHighWater: rollbackState.rollbackHighWater}
	rollbackListed := false
	// Bound work before scanning the caller-owned list. No reference escapes.
	if len(rollbackView.rollbackTargets) <= 1024 {
		for rollbackIndex := range rollbackView.rollbackTargets {
			if rollbackView.rollbackTargets[rollbackIndex] == rollbackCandidate.rollbackSequence {
				rollbackListed = true
				break
			}
		}
	}
	switch {
	case rollbackCandidate.rollbackEvidence != rollbackEvidenceVerified ||
		!rollbackCandidate.rollbackServer || !rollbackCandidate.rollbackWorker || !rollbackCandidate.rollbackPostgreSQL ||
		rollbackCandidate.rollbackOS == "" || rollbackCandidate.rollbackArch == "" || rollbackState.rollbackOS == "" || rollbackState.rollbackArch == "" ||
		len(rollbackCandidate.rollbackABI) == 0 || len(rollbackCandidate.rollbackABI) > 128 || len(rollbackState.rollbackABI) == 0 || len(rollbackState.rollbackABI) > 128 ||
		rollbackCandidate.rollbackCoreSchema < 1 || rollbackState.rollbackCoreSchema < 1 ||
		rollbackCandidate.rollbackLayout < 1 || rollbackState.rollbackLayout < 1 || rollbackState.rollbackHighWater < 1 ||
		rollbackCandidate.rollbackPrincipalLayout < 0 || rollbackState.rollbackPrincipalLayout < 0 ||
		rollbackCandidate.rollbackPostgreSQLMajor == "" || rollbackState.rollbackPostgreSQLMajor == "" || len(rollbackView.rollbackTargets) > 1024:
		rollbackResult.rollbackReason = rollbackIncomplete
	case rollbackView.rollbackSequence < rollbackState.rollbackHighWater:
		rollbackResult.rollbackReason = rollbackHighWater
	case rollbackView.rollbackSequence > rollbackState.rollbackHighWater || !rollbackView.rollbackPermitted ||
		rollbackCandidate.rollbackSequence >= rollbackState.rollbackHighWater || !rollbackListed:
		rollbackResult.rollbackReason = rollbackCurrentPolicy
	case rollbackCandidate.rollbackOS != rollbackState.rollbackOS || rollbackCandidate.rollbackArch != rollbackState.rollbackArch:
		rollbackResult.rollbackReason = rollbackPlatform
	case rollbackCandidate.rollbackPostgreSQLMajor != "16" || rollbackState.rollbackPostgreSQLMajor != "16":
		rollbackResult.rollbackReason = rollbackPGMajor
	case rollbackCandidate.rollbackABI != rollbackState.rollbackABI:
		rollbackResult.rollbackReason = rollbackABI
	case rollbackCandidate.rollbackLayout != rollbackState.rollbackLayout || rollbackCandidate.rollbackPrincipalLayout != rollbackState.rollbackPrincipalLayout:
		rollbackResult.rollbackReason = rollbackLayout
	case rollbackCandidate.rollbackCoreSchema < rollbackState.rollbackCoreSchema:
		rollbackResult.rollbackReason = rollbackSchemaOlder
	case rollbackCandidate.rollbackCoreSchema > rollbackState.rollbackCoreSchema:
		rollbackResult.rollbackReason = rollbackSchemaNewer
	case rollbackCandidate.rollbackSyncSelected != rollbackState.rollbackSyncSelected ||
		(rollbackCandidate.rollbackSyncSelected && (rollbackCandidate.rollbackSyncSchema < 1 || rollbackState.rollbackSyncSchema < 1 || rollbackCandidate.rollbackSyncSchema != rollbackState.rollbackSyncSchema)) ||
		(!rollbackCandidate.rollbackSyncSelected && (rollbackCandidate.rollbackSyncSchema != 0 || rollbackState.rollbackSyncSchema != 0)):
		rollbackResult.rollbackReason = rollbackSyncSchema
	default:
		rollbackResult.rollbackAllowed = true
	}
	return rollbackResult
}
