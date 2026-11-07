package syncharness

import (
	"regexp"
	"slices"
)

// MixedJobEvidenceClass qualifies what the pinned predecessor actually exposed.
// A hidden guard cause or an earlier purge refusal never claims an exact cause.
type MixedJobEvidenceClass string

// The three classes describe only the refusal cause actually observed.
const (
	GuardCauseExact  MixedJobEvidenceClass = "guard_cause_exact"
	GuardCauseHidden MixedJobEvidenceClass = "guard_cause_hidden"
	PreguardRefusal  MixedJobEvidenceClass = "preguard_refusal"
)

// MixedJobEvidence is the value-free, per-job qualification retained beside the
// private binary identities. Every accepted refusal also leaves its rows intact.
type MixedJobEvidence struct {
	Job           string                `json:"job"`
	Class         MixedJobEvidenceClass `json:"class"`
	RowsUnchanged bool                  `json:"rows_unchanged"`
}

// Check is the fixed public evidence line; binary identities never enter it.
func (evidence MixedJobEvidence) Check() string {
	return "capturemix job=" + evidence.Job + " evidence=" + string(evidence.Class)
}

var mixedJobEvidenceClasses = map[string]MixedJobEvidenceClass{
	"avatar_style_rollout":            GuardCauseExact,
	"message_rate_bucket_cleanup":     GuardCauseExact,
	"agent_email_rate_bucket_cleanup": GuardCauseExact,
	"agent_email_outbound":            GuardCauseExact,
	"transcript_retention":            GuardCauseExact,
	"message_retention":               GuardCauseExact,
	"agent_email_retention":           GuardCauseExact,
	"support_ticket_age_out":          GuardCauseHidden,
	"transcript_storage_reconcile":    GuardCauseHidden,
	"account_purge":                   PreguardRefusal,
}

func validMixedJobEvidence(capability string, evidence []MixedJobEvidence) bool {
	if capability != "legacy_writer_refusal" {
		return len(evidence) == 0
	}
	if len(evidence) != len(mixedJobEvidenceClasses) {
		return false
	}
	seen := make(map[string]bool, len(evidence))
	for _, item := range evidence {
		class, known := mixedJobEvidenceClasses[item.Job]
		if !known || seen[item.Job] || class != item.Class || !item.RowsUnchanged {
			return false
		}
		seen[item.Job] = true
	}
	return true
}

// BinaryBuildIdentity is private artifact data. Never log or format this value:
// exact executable digests belong only in the retained job artifact.
type BinaryBuildIdentity struct {
	Digest    string `json:"digest"`
	GoVersion string `json:"go_version"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
}

// MixedBuildIdentity binds the four independently built executables. The driver
// verifies the reviewed predecessor pin before computing this private record.
type MixedBuildIdentity struct {
	PredecessorCommit string              `json:"predecessor_commit"`
	CurrentServer     BinaryBuildIdentity `json:"current_server"`
	CurrentWorker     BinaryBuildIdentity `json:"current_worker"`
	PredecessorServer BinaryBuildIdentity `json:"predecessor_server"`
	PredecessorWorker BinaryBuildIdentity `json:"predecessor_worker"`
}

var mixedDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var mixedCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)
var mixedGoVersion = regexp.MustCompile(`^go1\.[0-9]+(?:\.[0-9]+)?$`)

func (identity MixedBuildIdentity) valid() bool {
	if !mixedCommit.MatchString(identity.PredecessorCommit) || identity.CurrentServer.Digest == identity.PredecessorServer.Digest || identity.CurrentWorker.Digest == identity.PredecessorWorker.Digest {
		return false
	}
	for _, binary := range []BinaryBuildIdentity{identity.CurrentServer, identity.CurrentWorker, identity.PredecessorServer, identity.PredecessorWorker} {
		if !mixedDigest.MatchString(binary.Digest) || !mixedGoVersion.MatchString(binary.GoVersion) || !validRuntimeToken(binary.GOOS) || !validRuntimeToken(binary.GOARCH) {
			return false
		}
	}
	return true
}

var mixedRuntimeAllocations = [...]struct{ obligation, capability, scenario string }{
	{"H36", "production_writer_admission", "PreBeginLateFirstWrite"},
	{"H36", "mixed_executable_versions", "PreBeginLateFirstWrite"},
	{"H36", "legacy_writer_refusal", "PreBeginLateFirstWrite"},
	{"H34", "proof_profile_registration", "CoverageInventoryAndSchedule"},
	{"H43", "production_downgrade_denial_mixed", "WriterGuardBypassAndDowngrade"},
}

func mixedRuntimeRecord(capability string) (RuntimeResultRecord, int, bool) {
	for _, allocation := range mixedRuntimeAllocations {
		if allocation.capability == capability {
			return RuntimeResultRecord{
				Version: 1, Obligation: allocation.obligation, Capability: capability,
				EvidenceClass: MixedBinariesEvidence,
				Adapter:       RuntimeAdapterSymbol{Package: "store", Symbol: "TestCaptureMixMatrix"},
				Scenario:      allocation.scenario, Executable: "lc_s6c2b_go_test_v1",
				DatabaseClass: "disposable_postgresql", PlatformClass: "go_test_host",
				Artifact: allocation.obligation + "_mixed_" + capability + "_v1", Outcome: Pass,
			}, 4, true
		}
	}
	return RuntimeResultRecord{}, 0, false
}

func mixedRuntimeCapability(obligation, capability string) bool {
	r, _, ok := mixedRuntimeRecord(capability)
	return ok && r.Obligation == obligation
}

func validMixedPendingReason(reason string) bool {
	switch reason {
	case "mixed_binaries_roles_unavailable", "proof_profile_database_unavailable", "predecessor_build_unavailable", "not_executed":
		return true
	default:
		return false
	}
}

// VerifyMixedRuntimeArtifact is called only after all four executable matrix
// rows have completed the capability's assertions under the isolated roles.
// Rehearsal, partial, failed and skipped rows must never call it. Its return
// includes private identities and must be written to an artifact, never a log.
// It does not promote any H obligation or claim installer conversion.
func VerifyMixedRuntimeArtifact(capability string, checks int, identity MixedBuildIdentity, jobEvidence ...MixedJobEvidence) (RuntimeArtifact, error) {
	r, expectedChecks, ok := mixedRuntimeRecord(capability)
	r.BuildIdentity = &identity
	manifests, err := EmbeddedObligations()
	if !ok || checks != expectedChecks || err != nil || r.Validate(manifests) != nil || !validMixedJobEvidence(capability, jobEvidence) {
		return RuntimeArtifact{}, ErrRuntimeResult
	}
	return RuntimeArtifact{Version: 1, ID: r.Artifact, Result: r, Checks: checks, JobEvidence: slices.Clone(jobEvidence)}, nil
}

// MixedRuntimeNotRun is a value-free receipt. No executable identity or class
// may turn a rehearsal into evidence of the role-isolated production matrix.
func MixedRuntimeNotRun(capability, reason string) (RuntimeResultRecord, error) {
	r, _, ok := mixedRuntimeRecord(capability)
	if !ok || !validMixedPendingReason(reason) {
		return RuntimeResultRecord{}, ErrRuntimeResult
	}
	return RuntimeResultRecord{
		Version: 1, Obligation: r.Obligation, Capability: capability, Scenario: r.Scenario,
		Outcome: NotRun, MissingCapability: capability, Reason: reason,
	}, nil
}
