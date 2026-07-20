package store

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Portable archives preserve only terminal enrollment and rotation history.
// Live transfer capsules and staging workspaces are cell-local authority and
// ExportAccount refuses to stream them.
type vaultEnrollmentImportScope struct {
	id         string
	realmID    string
	agentID    string
	state      string
	rowVersion int64
	createdAt  time.Time
	approved   bool
}

type vaultRotationImportScope struct {
	id          string
	realmID     string
	agentID     string
	state       string
	rowVersion  int64
	createdAt   time.Time
	stagedCount int64
}

func (ic *importCtx) validateImportedVaultEnrollment(obj map[string]any) (vaultEnrollmentImportScope, error) {
	id, err := requireStringField(obj, "id")
	if err != nil || !validImportedGeneratedID(id, "enr") {
		return vaultEnrollmentImportScope{}, fmt.Errorf("id is invalid")
	}
	realmID, realmErr := requireStringField(obj, "realm_id")
	agentID, agentErr := requireStringField(obj, "owner_agent_id")
	if realmErr != nil || agentErr != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID {
		return vaultEnrollmentImportScope{}, fmt.Errorf("owner is outside its realm")
	}
	keyID, keyErr := requireStringField(obj, "vault_key_id")
	keyVersion, versionOK := importedPositiveInteger(obj["vault_key_version"])
	key, keyExists := ic.importedVaultKey(keyID, keyVersion)
	if keyErr != nil || !versionOK || !keyExists || key.version != keyVersion ||
		key.realmID != realmID || key.agentID != agentID {
		return vaultEnrollmentImportScope{}, fmt.Errorf("vault key is outside enrollment scope")
	}
	targetLocationID, err := requireStringField(obj, "target_location_id")
	if err != nil || !validImportedGeneratedID(targetLocationID, "loc") {
		return vaultEnrollmentImportScope{}, fmt.Errorf("target_location_id is invalid")
	}
	targetLocationName, ok := obj["target_location_name"].(string)
	if !ok || !utf8.ValidString(targetLocationName) || len(targetLocationName) > 256 || strings.ContainsRune(targetLocationName, '\x00') {
		return vaultEnrollmentImportScope{}, fmt.Errorf("target_location_name is invalid")
	}
	targetPublicKey, err := requireStringField(obj, "target_public_key")
	if err != nil || !validVaultEnrollmentPublicKey(targetPublicKey) {
		return vaultEnrollmentImportScope{}, fmt.Errorf("target_public_key is invalid")
	}
	targetAlgorithm, err := requireStringField(obj, "target_key_algorithm")
	if err != nil || targetAlgorithm != VaultEnrollmentTargetKeyAlgorithm {
		return vaultEnrollmentImportScope{}, fmt.Errorf("target_key_algorithm is invalid")
	}
	pairingCommitment, err := requireStringField(obj, "pairing_commitment")
	if err != nil || !validFactSHA256(pairingCommitment) {
		return vaultEnrollmentImportScope{}, fmt.Errorf("pairing_commitment is invalid")
	}
	state, err := requireStringField(obj, "lifecycle_state")
	if err != nil || (state != VaultEnrollmentStateConsumed &&
		state != VaultEnrollmentStateCancelled && state != VaultEnrollmentStateExpired) {
		return vaultEnrollmentImportScope{}, fmt.Errorf("lifecycle_state is not terminal")
	}
	for _, field := range []string{"source_ephemeral_public_key", "transfer_ciphertext", "transfer_algorithm", "consume_commitment"} {
		value, present := obj[field]
		if !present || value != nil {
			return vaultEnrollmentImportScope{}, fmt.Errorf("terminal row carries %s", field)
		}
	}
	sourceLocationID, sourcePresent, err := importedNullableString(obj, "source_location_id")
	if err != nil || sourcePresent && (!validImportedGeneratedID(sourceLocationID, "loc") || sourceLocationID == targetLocationID) {
		return vaultEnrollmentImportScope{}, fmt.Errorf("source_location_id is invalid")
	}
	revision, ok := importedPositiveInteger(obj["row_version"])
	if !ok {
		return vaultEnrollmentImportScope{}, fmt.Errorf("row_version is invalid")
	}
	createdAt, err := requireImportedTimestamp(obj, "created_at")
	expiresAt, expiryErr := requireImportedTimestamp(obj, "expires_at")
	if err != nil || expiryErr != nil || !expiresAt.After(*createdAt) ||
		ic.requireTimestampAtOrBeforeExport("created_at", *createdAt) != nil {
		return vaultEnrollmentImportScope{}, fmt.Errorf("base timestamps are invalid")
	}
	approvedAt, approved, err := importedOptionalTimestamp(obj, "approved_at")
	if err != nil || approved != sourcePresent || approved && approvedAt.Before(*createdAt) ||
		approved && ic.requireTimestampAtOrBeforeExport("approved_at", *approvedAt) != nil {
		return vaultEnrollmentImportScope{}, fmt.Errorf("approval state is invalid")
	}
	consumedAt, consumed, consumeErr := importedOptionalTimestamp(obj, "consumed_at")
	cancelledAt, cancelled, cancelErr := importedOptionalTimestamp(obj, "cancelled_at")
	expiredAt, expired, expireErr := importedOptionalTimestamp(obj, "expired_at")
	if consumeErr != nil || cancelErr != nil || expireErr != nil {
		return vaultEnrollmentImportScope{}, fmt.Errorf("terminal timestamp is invalid")
	}
	if consumed && (consumedAt.Before(*createdAt) || !approved || consumedAt.Before(*approvedAt) ||
		ic.requireTimestampAtOrBeforeExport("consumed_at", *consumedAt) != nil) {
		return vaultEnrollmentImportScope{}, fmt.Errorf("consumed_at is invalid")
	}
	if cancelled && (cancelledAt.Before(*createdAt) ||
		ic.requireTimestampAtOrBeforeExport("cancelled_at", *cancelledAt) != nil) {
		return vaultEnrollmentImportScope{}, fmt.Errorf("cancelled_at is invalid")
	}
	if expired && (expiredAt.Before(*createdAt) || expiredAt.Before(*expiresAt) ||
		ic.requireTimestampAtOrBeforeExport("expired_at", *expiredAt) != nil) {
		return vaultEnrollmentImportScope{}, fmt.Errorf("expired_at is invalid")
	}
	if state == VaultEnrollmentStateConsumed && (!consumed || cancelled || expired) ||
		state == VaultEnrollmentStateCancelled && (consumed || !cancelled || expired) ||
		state == VaultEnrollmentStateExpired && (consumed || cancelled || !expired) {
		return vaultEnrollmentImportScope{}, fmt.Errorf("terminal lifecycle fields are inconsistent")
	}
	return vaultEnrollmentImportScope{
		id: id, realmID: realmID, agentID: agentID, state: state,
		rowVersion: revision, createdAt: *createdAt, approved: approved,
	}, nil
}

func (ic *importCtx) validateImportedVaultEnrollmentReceipt(obj map[string]any) (string, error) {
	realmID, realmErr := requireStringField(obj, "realm_id")
	agentID, agentErr := requireStringField(obj, "owner_agent_id")
	if realmErr != nil || agentErr != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID {
		return "", fmt.Errorf("owner is outside its realm")
	}
	operation, err := requireStringField(obj, "operation")
	if err != nil {
		return "", fmt.Errorf("operation is invalid")
	}
	switch operation {
	case "enrollment_request", "enrollment_approve", "enrollment_consume", "enrollment_cancel":
	default:
		return "", fmt.Errorf("operation is invalid")
	}
	keyHash, keyErr := requireStringField(obj, "idempotency_key_hash")
	requestHash, requestErr := requireStringField(obj, "request_hash")
	if keyErr != nil || requestErr != nil || !validFactSHA256(keyHash) || !validFactSHA256(requestHash) {
		return "", fmt.Errorf("request hashes are invalid")
	}
	enrollmentID, err := requireStringField(obj, "enrollment_id")
	enrollment, exists := ic.vaultEnrollments[enrollmentID]
	if err != nil || !exists || enrollment.realmID != realmID || enrollment.agentID != agentID {
		return "", fmt.Errorf("enrollment is outside receipt scope")
	}
	revision, ok := importedPositiveInteger(obj["result_revision"])
	if !ok || revision > enrollment.rowVersion {
		return "", fmt.Errorf("result_revision is invalid")
	}
	if operation == "enrollment_approve" && !enrollment.approved ||
		operation == "enrollment_consume" && enrollment.state != VaultEnrollmentStateConsumed ||
		operation == "enrollment_cancel" && enrollment.state != VaultEnrollmentStateCancelled {
		return "", fmt.Errorf("operation does not match terminal enrollment")
	}
	createdAt, err := requireImportedTimestamp(obj, "created_at")
	if err != nil || createdAt.Before(enrollment.createdAt) ||
		ic.requireTimestampAtOrBeforeExport("created_at", *createdAt) != nil {
		return "", fmt.Errorf("created_at is invalid")
	}
	return strings.Join([]string{realmID, agentID, operation, keyHash}, "\x00"), nil
}

func (ic *importCtx) validateImportedVaultRotation(obj map[string]any) (vaultRotationImportScope, error) {
	id, err := requireStringField(obj, "id")
	if err != nil || !validImportedGeneratedID(id, "vkr") {
		return vaultRotationImportScope{}, fmt.Errorf("id is invalid")
	}
	realmID, realmErr := requireStringField(obj, "realm_id")
	agentID, agentErr := requireStringField(obj, "owner_agent_id")
	if realmErr != nil || agentErr != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID {
		return vaultRotationImportScope{}, fmt.Errorf("owner is outside its realm")
	}
	sourceID, sourceErr := requireStringField(obj, "source_key_id")
	targetID, targetErr := requireStringField(obj, "target_key_id")
	sourceVersion, sourceVersionOK := importedPositiveInteger(obj["source_key_version"])
	targetVersion, targetVersionOK := importedPositiveInteger(obj["target_key_version"])
	source, sourceExists := ic.importedVaultKey(sourceID, sourceVersion)
	target, targetExists := ic.importedVaultKey(targetID, targetVersion)
	if sourceErr != nil || targetErr != nil || !sourceVersionOK || !targetVersionOK ||
		!sourceExists || !targetExists || sourceID == targetID ||
		sourceVersion == int64(^uint64(0)>>1) || targetVersion != sourceVersion+1 ||
		source.version != sourceVersion || target.version != targetVersion ||
		source.realmID != realmID || target.realmID != realmID ||
		source.agentID != agentID || target.agentID != agentID {
		return vaultRotationImportScope{}, fmt.Errorf("key transition is invalid")
	}
	state, err := requireStringField(obj, "lifecycle_state")
	if err != nil || (state != VaultKeyRotationCommitted && state != VaultKeyRotationCancelled) {
		return vaultRotationImportScope{}, fmt.Errorf("lifecycle_state is not terminal")
	}
	dispositionMode, dispositionPresent, dispositionErr := importedNullableString(obj, "recovery_disposition_mode")
	artifactSHA256, artifactPresent, artifactErr := importedNullableString(obj, "recovery_artifact_sha256")
	if dispositionErr != nil || artifactErr != nil {
		return vaultRotationImportScope{}, fmt.Errorf("recovery disposition is invalid")
	}
	switch state {
	case VaultKeyRotationCommitted:
		if !dispositionPresent || !validVaultKeyRotationRecoveryDisposition(VaultKeyRotationRecoveryDisposition{
			Mode: dispositionMode, ArtifactSHA256: artifactSHA256,
		}) || dispositionMode == VaultKeyRotationRecoveryArtifact != artifactPresent {
			return vaultRotationImportScope{}, fmt.Errorf("recovery disposition is invalid")
		}
	case VaultKeyRotationCancelled:
		if dispositionPresent || artifactPresent {
			return vaultRotationImportScope{}, fmt.Errorf("cancelled rotation carries a recovery disposition")
		}
	}
	if source.state == "pending" || state == VaultKeyRotationCommitted && target.state == "pending" ||
		state == VaultKeyRotationCancelled && target.state != "retired" {
		return vaultRotationImportScope{}, fmt.Errorf("key lifecycle does not match transition")
	}
	itemCount, itemOK := importedNonnegativeInteger(obj["item_count"])
	stagedCount, stagedOK := importedNonnegativeInteger(obj["staged_count"])
	if !itemOK || !stagedOK || stagedCount > itemCount ||
		state == VaultKeyRotationCommitted && stagedCount != itemCount {
		return vaultRotationImportScope{}, fmt.Errorf("item counts are invalid")
	}
	revision, ok := importedPositiveInteger(obj["row_version"])
	if !ok {
		return vaultRotationImportScope{}, fmt.Errorf("row_version is invalid")
	}
	createdAt, err := requireImportedTimestamp(obj, "created_at")
	updatedAt, updateErr := requireImportedTimestamp(obj, "updated_at")
	if err != nil || updateErr != nil || updatedAt.Before(*createdAt) ||
		ic.requireTimestampAtOrBeforeExport("created_at", *createdAt) != nil ||
		ic.requireTimestampAtOrBeforeExport("updated_at", *updatedAt) != nil {
		return vaultRotationImportScope{}, fmt.Errorf("base timestamps are invalid")
	}
	committedAt, committed, commitErr := importedOptionalTimestamp(obj, "committed_at")
	cancelledAt, cancelled, cancelErr := importedOptionalTimestamp(obj, "cancelled_at")
	if commitErr != nil || cancelErr != nil ||
		committed && (committedAt.Before(*createdAt) || committedAt.After(*updatedAt) ||
			ic.requireTimestampAtOrBeforeExport("committed_at", *committedAt) != nil) ||
		cancelled && (cancelledAt.Before(*createdAt) || cancelledAt.After(*updatedAt) ||
			ic.requireTimestampAtOrBeforeExport("cancelled_at", *cancelledAt) != nil) ||
		state == VaultKeyRotationCommitted && (!committed || cancelled) ||
		state == VaultKeyRotationCancelled && (committed || !cancelled) {
		return vaultRotationImportScope{}, fmt.Errorf("terminal timestamps are inconsistent")
	}
	return vaultRotationImportScope{
		id: id, realmID: realmID, agentID: agentID, state: state,
		rowVersion: revision, createdAt: *createdAt, stagedCount: stagedCount,
	}, nil
}

func (ic *importCtx) validateImportedVaultRotationReceipt(obj map[string]any) (string, error) {
	realmID, realmErr := requireStringField(obj, "realm_id")
	agentID, agentErr := requireStringField(obj, "owner_agent_id")
	if realmErr != nil || agentErr != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID {
		return "", fmt.Errorf("owner is outside its realm")
	}
	operation, err := requireStringField(obj, "operation")
	if err != nil {
		return "", fmt.Errorf("operation is invalid")
	}
	switch operation {
	case "rotation_start", "rotation_stage", "rotation_commit", "rotation_cancel":
	default:
		return "", fmt.Errorf("operation is invalid")
	}
	keyHash, keyErr := requireStringField(obj, "idempotency_key_hash")
	requestHash, requestErr := requireStringField(obj, "request_hash")
	if keyErr != nil || requestErr != nil || !validFactSHA256(keyHash) || !validFactSHA256(requestHash) {
		return "", fmt.Errorf("request hashes are invalid")
	}
	rotationID, err := requireStringField(obj, "rotation_id")
	rotation, exists := ic.vaultRotations[rotationID]
	if err != nil || !exists || rotation.realmID != realmID || rotation.agentID != agentID {
		return "", fmt.Errorf("rotation is outside receipt scope")
	}
	revision, ok := importedPositiveInteger(obj["result_revision"])
	if !ok || revision > rotation.rowVersion {
		return "", fmt.Errorf("result_revision is invalid")
	}
	if operation == "rotation_stage" && rotation.stagedCount == 0 ||
		operation == "rotation_commit" && rotation.state != VaultKeyRotationCommitted ||
		operation == "rotation_cancel" && rotation.state != VaultKeyRotationCancelled {
		return "", fmt.Errorf("operation does not match terminal rotation")
	}
	createdAt, err := requireImportedTimestamp(obj, "created_at")
	if err != nil || createdAt.Before(rotation.createdAt) ||
		ic.requireTimestampAtOrBeforeExport("created_at", *createdAt) != nil {
		return "", fmt.Errorf("created_at is invalid")
	}
	return strings.Join([]string{realmID, agentID, operation, keyHash}, "\x00"), nil
}

func importedNullableString(obj map[string]any, field string) (string, bool, error) {
	raw, present := obj[field]
	if !present {
		return "", false, fmt.Errorf("%s is required", field)
	}
	if raw == nil {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok || value == "" {
		return "", false, fmt.Errorf("%s is invalid", field)
	}
	return value, true, nil
}
