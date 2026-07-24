package store

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	importedSecretFieldNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	importedSecretTemplatePattern  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	importedSecretTagPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
)

type secretVaultKeyVersionImportKey struct {
	agentID string
	version int64
}

type secretVaultKeyIdentityImportKey struct {
	id      string
	version int64
}

type secretVaultKeyImportScope struct {
	id          string
	realmID     string
	agentID     string
	version     int64
	rowVersion  int64
	state       string
	fingerprint string
}

type secretImportScope struct {
	realmID    string
	agentID    string
	archived   bool
	deleted    bool
	createdAt  time.Time
	rowVersion int64
}

type secretFieldImportScope struct {
	secretID      string
	realmID       string
	agentID       string
	sensitive     bool
	dekID         string
	dekGeneration int64
	valueVersion  int64
	rowVersion    int64
}

type secretDEKGenerationImportKey struct {
	fieldID    string
	generation int64
}

type secretDEKImportScope struct {
	secretID        string
	fieldID         string
	realmID         string
	agentID         string
	generation      int64
	wrappingKeyID   string
	wrappingVersion int64
	retired         bool
	rowVersion      int64
}

func (ic *importCtx) validateImportedVaultKey(obj map[string]any) (secretVaultKeyImportScope, error) {
	id, err := requireStringField(obj, "id")
	if err != nil || !validImportedGeneratedID(id, "avk") {
		return secretVaultKeyImportScope{}, fmt.Errorf("id is invalid")
	}
	realmID, err := requireStringField(obj, "realm_id")
	agentID, agentErr := requireStringField(obj, "owner_agent_id")
	if err != nil || agentErr != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID {
		return secretVaultKeyImportScope{}, fmt.Errorf("owner is outside its realm")
	}
	version, ok := importedPositiveInteger(obj["key_version"])
	if !ok {
		return secretVaultKeyImportScope{}, fmt.Errorf("key_version is invalid")
	}
	algorithm, err := requireStringField(obj, "algorithm")
	if err != nil || algorithm != SecretAEADAlgorithm {
		return secretVaultKeyImportScope{}, fmt.Errorf("algorithm is invalid")
	}
	fingerprint, err := requireStringField(obj, "fingerprint")
	if err != nil || !validFactSHA256(fingerprint) {
		return secretVaultKeyImportScope{}, fmt.Errorf("fingerprint is invalid")
	}
	state, err := requireStringField(obj, "lifecycle_state")
	// Portable archives are frozen snapshots. A pending AVK is authority owned
	// by an open rotation, and ExportAccount refuses every open rotation. Accepting
	// an orphan pending row would import an unusable live-version fence with no
	// lifecycle operation capable of completing or cancelling it.
	if err != nil || (state != "current" && state != "retired") {
		return secretVaultKeyImportScope{}, fmt.Errorf("lifecycle_state is invalid")
	}
	revision, ok := importedPositiveInteger(obj["row_version"])
	if !ok || revision < 1 {
		return secretVaultKeyImportScope{}, fmt.Errorf("row_version is invalid")
	}
	createdAt, err := requireImportedTimestamp(obj, "created_at")
	if err != nil || ic.requireTimestampAtOrBeforeExport("created_at", *createdAt) != nil {
		return secretVaultKeyImportScope{}, fmt.Errorf("created_at is invalid")
	}
	retiredAt, retired, err := importedOptionalTimestamp(obj, "retired_at")
	if err != nil || retired != (state == "retired") {
		return secretVaultKeyImportScope{}, fmt.Errorf("retired_at is inconsistent")
	}
	if retired && (retiredAt.Before(*createdAt) || ic.requireTimestampAtOrBeforeExport("retired_at", *retiredAt) != nil) {
		return secretVaultKeyImportScope{}, fmt.Errorf("retired_at is invalid")
	}
	return secretVaultKeyImportScope{
		id: id, realmID: realmID, agentID: agentID, version: version, rowVersion: revision,
		state: state, fingerprint: fingerprint,
	}, nil
}

func (ic *importCtx) importedVaultKey(id string, version int64) (secretVaultKeyImportScope, bool) {
	key, exists := ic.vaultKeyIdentities[secretVaultKeyIdentityImportKey{id: id, version: version}]
	return key, exists
}

func (ic *importCtx) validateImportedSecret(obj map[string]any) (secretImportScope, string, error) {
	id, err := requireStringField(obj, "id")
	if err != nil || !validImportedGeneratedID(id, "sec") {
		return secretImportScope{}, "", fmt.Errorf("id is invalid")
	}
	realmID, err := requireStringField(obj, "realm_id")
	agentID, agentErr := requireStringField(obj, "owner_agent_id")
	if err != nil || agentErr != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID {
		return secretImportScope{}, "", fmt.Errorf("owner is outside its realm")
	}
	name, err := requireStringField(obj, "name")
	if err != nil || len(name) < 1 || strings.TrimSpace(name) != name || len(name) > 256 || strings.ContainsAny(name, "\x00\r\n") {
		return secretImportScope{}, "", fmt.Errorf("name is invalid")
	}
	description, ok := obj["description"].(string)
	if !ok || len(description) > 4096 || strings.ContainsRune(description, '\x00') {
		return secretImportScope{}, "", fmt.Errorf("description is invalid")
	}
	template, err := requireStringField(obj, "template")
	if err != nil || !importedSecretTemplatePattern.MatchString(template) {
		return secretImportScope{}, "", fmt.Errorf("template is invalid")
	}
	tags, ok := obj["tags"].([]any)
	if !ok || len(tags) > 64 {
		return secretImportScope{}, "", fmt.Errorf("tags are invalid")
	}
	seenTags := make(map[string]bool, len(tags))
	for _, raw := range tags {
		tag, ok := raw.(string)
		if !ok || !importedSecretTagPattern.MatchString(tag) || seenTags[tag] {
			return secretImportScope{}, "", fmt.Errorf("tags are invalid")
		}
		seenTags[tag] = true
	}
	revision, ok := importedPositiveInteger(obj["row_version"])
	if !ok || revision < 1 {
		return secretImportScope{}, "", fmt.Errorf("row_version is invalid")
	}
	createdAt, err := requireImportedTimestamp(obj, "created_at")
	updatedAt, updateErr := requireImportedTimestamp(obj, "updated_at")
	if err != nil || updateErr != nil || updatedAt.Before(*createdAt) ||
		ic.requireTimestampAtOrBeforeExport("created_at", *createdAt) != nil ||
		ic.requireTimestampAtOrBeforeExport("updated_at", *updatedAt) != nil {
		return secretImportScope{}, "", fmt.Errorf("timestamps are invalid")
	}
	archivedAt, archived, err := importedOptionalTimestamp(obj, "archived_at")
	if err != nil || archived && (archivedAt.Before(*createdAt) || ic.requireTimestampAtOrBeforeExport("archived_at", *archivedAt) != nil) {
		return secretImportScope{}, "", fmt.Errorf("archived_at is invalid")
	}
	deletedAt, deleted, err := importedOptionalTimestamp(obj, "deleted_at")
	if err != nil || deleted && (deletedAt.Before(*createdAt) || ic.requireTimestampAtOrBeforeExport("deleted_at", *deletedAt) != nil) {
		return secretImportScope{}, "", fmt.Errorf("deleted_at is invalid")
	}
	if deleted && (archived || name != id || description != "" ||
		template != "generic" || len(tags) != 0) {
		return secretImportScope{}, "", fmt.Errorf("deleted tombstone metadata is invalid")
	}
	return secretImportScope{
		realmID: realmID, agentID: agentID, archived: archived,
		deleted: deleted, createdAt: *createdAt, rowVersion: revision,
	}, name, nil
}

func (ic *importCtx) validateImportedSecretField(obj map[string]any) (secretFieldImportScope, string, error) {
	id, err := requireStringField(obj, "id")
	if err != nil || !validImportedGeneratedID(id, "fld") {
		return secretFieldImportScope{}, "", fmt.Errorf("id is invalid")
	}
	secretID, err := requireStringField(obj, "secret_id")
	secret, exists := ic.secrets[secretID]
	if err != nil || !exists {
		return secretFieldImportScope{}, "", fmt.Errorf("secret is not present in this archive")
	}
	realmID, _ := requireStringField(obj, "realm_id")
	agentID, _ := requireStringField(obj, "owner_agent_id")
	if realmID != secret.realmID || agentID != secret.agentID {
		return secretFieldImportScope{}, "", fmt.Errorf("scope does not match secret")
	}
	name, err := requireStringField(obj, "name")
	if err != nil || !importedSecretFieldNamePattern.MatchString(name) {
		return secretFieldImportScope{}, "", fmt.Errorf("name is invalid")
	}
	kind, err := requireStringField(obj, "field_kind")
	if err != nil || !validSecretFieldKind(kind) {
		return secretFieldImportScope{}, "", fmt.Errorf("field_kind is invalid")
	}
	sensitive, ok := obj["sensitive"].(bool)
	if !ok || !sensitive && secretFieldKindRequiresProtection(kind) {
		return secretFieldImportScope{}, "", fmt.Errorf("sensitivity is invalid")
	}
	encoding, err := requireStringField(obj, "value_encoding")
	if err != nil || (encoding != "utf8" && encoding != "json" && encoding != "binary") || !sensitive && encoding != "utf8" {
		return secretFieldImportScope{}, "", fmt.Errorf("value_encoding is invalid")
	}
	valueVersion, ok := importedPositiveInteger(obj["value_version"])
	if !ok {
		return secretFieldImportScope{}, "", fmt.Errorf("value_version is invalid")
	}
	revision, ok := importedPositiveInteger(obj["row_version"])
	if !ok || revision < 1 {
		return secretFieldImportScope{}, "", fmt.Errorf("row_version is invalid")
	}
	createdAt, err := requireImportedTimestamp(obj, "created_at")
	updatedAt, updateErr := requireImportedTimestamp(obj, "updated_at")
	if err != nil || updateErr != nil || updatedAt.Before(*createdAt) ||
		createdAt.Before(secret.createdAt) ||
		ic.requireTimestampAtOrBeforeExport("created_at", *createdAt) != nil ||
		ic.requireTimestampAtOrBeforeExport("updated_at", *updatedAt) != nil {
		return secretFieldImportScope{}, "", fmt.Errorf("timestamps are invalid")
	}
	publicValue, publicIsString := obj["public_value"].(string)
	envelopeVersion, envelopePresent, envelopeOK := importedOptionalPositiveInteger(obj, "envelope_version")
	ciphertextBytes, ciphertextOK := importedByteaLength(obj["ciphertext"])
	algorithm, algorithmPresent := optionalStringField(obj, "aead_algorithm")
	aadVersion, aadPresent, aadOK := importedOptionalPositiveInteger(obj, "aad_version")
	dekID, dekPresent := optionalStringField(obj, "dek_id")
	dekGeneration, generationPresent, generationOK := importedOptionalPositiveInteger(obj, "dek_generation")
	if !sensitive {
		if !publicIsString || len(publicValue) > 65536 || strings.ContainsRune(publicValue, '\x00') || obj["envelope_version"] != nil ||
			obj["ciphertext"] != nil ||
			obj["aead_algorithm"] != nil || obj["aad_version"] != nil ||
			obj["dek_id"] != nil || obj["dek_generation"] != nil ||
			envelopePresent || ciphertextOK || algorithmPresent || aadPresent || dekPresent || generationPresent {
			return secretFieldImportScope{}, "", fmt.Errorf("public field branch is invalid")
		}
		return secretFieldImportScope{secretID: secretID, realmID: realmID, agentID: agentID, valueVersion: valueVersion, rowVersion: revision}, name, nil
	}
	if publicIsString || obj["public_value"] != nil || !envelopePresent || !envelopeOK || envelopeVersion != 1 ||
		!ciphertextOK || ciphertextBytes < 29 || ciphertextBytes > 65564 ||
		!algorithmPresent || algorithm != SecretAEADAlgorithm || !aadPresent || !aadOK || aadVersion != 1 ||
		!dekPresent || !validImportedGeneratedID(dekID, "dek") || !generationPresent || !generationOK {
		return secretFieldImportScope{}, "", fmt.Errorf("sensitive field envelope is invalid")
	}
	return secretFieldImportScope{
		secretID: secretID, realmID: realmID, agentID: agentID,
		sensitive: true, dekID: dekID, dekGeneration: dekGeneration,
		valueVersion: valueVersion, rowVersion: revision,
	}, name, nil
}

func (ic *importCtx) validateImportedSecretDEK(obj map[string]any) (secretDEKImportScope, error) {
	id, err := requireStringField(obj, "id")
	if err != nil || !validImportedGeneratedID(id, "dek") {
		return secretDEKImportScope{}, fmt.Errorf("id is invalid")
	}
	fieldID, err := requireStringField(obj, "field_id")
	field, exists := ic.secretFields[fieldID]
	if err != nil || !exists || !field.sensitive {
		return secretDEKImportScope{}, fmt.Errorf("field is not a sensitive field in this archive")
	}
	secretID, _ := requireStringField(obj, "secret_id")
	realmID, _ := requireStringField(obj, "realm_id")
	agentID, _ := requireStringField(obj, "owner_agent_id")
	if secretID != field.secretID || realmID != field.realmID || agentID != field.agentID {
		return secretDEKImportScope{}, fmt.Errorf("scope does not match field")
	}
	generation, ok := importedPositiveInteger(obj["dek_generation"])
	if !ok {
		return secretDEKImportScope{}, fmt.Errorf("dek_generation is invalid")
	}
	if wrappedBytes, ok := importedByteaLength(obj["wrapped_dek"]); !ok || wrappedBytes != 60 {
		return secretDEKImportScope{}, fmt.Errorf("wrapped_dek is invalid")
	}
	algorithm, err := requireStringField(obj, "wrap_algorithm")
	if err != nil || algorithm != SecretAEADAlgorithm {
		return secretDEKImportScope{}, fmt.Errorf("wrap_algorithm is invalid")
	}
	aadVersion, ok := importedPositiveInteger(obj["aad_version"])
	if !ok || aadVersion != 1 {
		return secretDEKImportScope{}, fmt.Errorf("aad_version is invalid")
	}
	if wrapRevision, ok := importedPositiveInteger(obj["wrap_revision"]); !ok || wrapRevision < 1 {
		return secretDEKImportScope{}, fmt.Errorf("wrap_revision is invalid")
	}
	keyID, err := requireStringField(obj, "wrapping_key_id")
	keyVersion, versionOK := importedPositiveInteger(obj["wrapping_key_version"])
	key, keyExists := ic.importedVaultKey(keyID, keyVersion)
	if err != nil || !versionOK || !keyExists || key.version != keyVersion || key.realmID != realmID || key.agentID != agentID {
		return secretDEKImportScope{}, fmt.Errorf("wrapping key is outside field scope")
	}
	revision, ok := importedPositiveInteger(obj["row_version"])
	if !ok || revision < 1 {
		return secretDEKImportScope{}, fmt.Errorf("row_version is invalid")
	}
	createdAt, err := requireImportedTimestamp(obj, "created_at")
	if err != nil || ic.requireTimestampAtOrBeforeExport("created_at", *createdAt) != nil {
		return secretDEKImportScope{}, fmt.Errorf("created_at is invalid")
	}
	retiredAt, retired, err := importedOptionalTimestamp(obj, "retired_at")
	if err != nil || retired && (retiredAt.Before(*createdAt) || ic.requireTimestampAtOrBeforeExport("retired_at", *retiredAt) != nil) {
		return secretDEKImportScope{}, fmt.Errorf("retired_at is invalid")
	}
	return secretDEKImportScope{
		secretID: secretID, fieldID: fieldID, realmID: realmID, agentID: agentID,
		generation: generation, wrappingKeyID: keyID,
		wrappingVersion: keyVersion, retired: retired, rowVersion: revision,
	}, nil
}

func (ic *importCtx) validateImportedSecretReceipt(obj map[string]any) (string, error) {
	realmID, err := requireStringField(obj, "realm_id")
	agentID, agentErr := requireStringField(obj, "owner_agent_id")
	if err != nil || agentErr != nil || !ic.agents[agentID] || ic.agentRealms[agentID] != realmID {
		return "", fmt.Errorf("owner is outside its realm")
	}
	actorKind, err := requireStringField(obj, "actor_kind")
	actorID, actorErr := requireStringField(obj, "actor_id")
	if err != nil || actorErr != nil ||
		(actorKind == "agent" && actorID != agentID) ||
		(actorKind == "operator" && !ic.operators[actorID]) ||
		(actorKind != "agent" && actorKind != "operator") {
		return "", fmt.Errorf("actor is invalid")
	}
	operation, err := requireStringField(obj, "operation")
	if err != nil || !validImportedSecretOperation(operation) {
		return "", fmt.Errorf("operation is invalid")
	}
	keyHash, err := requireStringField(obj, "idempotency_key_hash")
	requestHash, requestErr := requireStringField(obj, "request_hash")
	if err != nil || requestErr != nil || !validFactSHA256(keyHash) || !validFactSHA256(requestHash) {
		return "", fmt.Errorf("request hashes are invalid")
	}
	targetKind, err := requireStringField(obj, "target_kind")
	targetID, targetErr := requireStringField(obj, "target_id")
	if err != nil || targetErr != nil || !ic.validImportedSecretReceiptTarget(targetKind, targetID, realmID, agentID) {
		return "", fmt.Errorf("target is invalid")
	}
	if !validImportedSecretOperationTarget(operation, targetKind) {
		return "", fmt.Errorf("operation target is invalid")
	}
	revision, ok := importedPositiveInteger(obj["result_revision"])
	if !ok || revision < 1 {
		return "", fmt.Errorf("result_revision is invalid")
	}
	valueVersion, valueVersionPresent, ok := importedOptionalPositiveInteger(obj, "result_value_version")
	if !ok {
		return "", fmt.Errorf("result_value_version is invalid")
	}
	if err := ic.validateImportedSecretReceiptResult(operation, targetKind, targetID,
		revision, valueVersion, valueVersionPresent); err != nil {
		return "", err
	}
	createdAt, err := requireImportedTimestamp(obj, "created_at")
	if err != nil || ic.requireTimestampAtOrBeforeExport("created_at", *createdAt) != nil {
		return "", fmt.Errorf("created_at is invalid")
	}
	return strings.Join([]string{realmID, agentID, actorKind, actorID, operation, keyHash}, "\x00"), nil
}

func (ic *importCtx) validImportedSecretReceiptTarget(kind, id, realmID, agentID string) bool {
	switch kind {
	case "key_epoch":
		target, ok := ic.vaultKeys[id]
		return ok && target.realmID == realmID && target.agentID == agentID
	case "secret":
		target, ok := ic.secrets[id]
		return ok && target.realmID == realmID && target.agentID == agentID
	case "field":
		target, ok := ic.secretFields[id]
		return ok && target.realmID == realmID && target.agentID == agentID
	case "dek":
		target, ok := ic.secretDEKs[id]
		return ok && target.realmID == realmID && target.agentID == agentID
	default:
		return false
	}
}

func (ic *importCtx) validateImportedSecretGraph() error {
	initializedAgents := map[string]bool{}
	for _, key := range ic.vaultKeys {
		initializedAgents[key.agentID] = true
	}
	for agentID := range initializedAgents {
		if ic.liveAgents[agentID] && ic.vaultCurrentKeys[agentID] != 1 {
			return fmt.Errorf("live agent %q has %d current vault keys", agentID, ic.vaultCurrentKeys[agentID])
		}
	}
	for secretID, secret := range ic.secrets {
		count := ic.secretFieldCounts[secretID]
		if secret.deleted {
			if count != 0 {
				return fmt.Errorf("deleted secret %q retains %d fields", secretID, count)
			}
			continue
		}
		if count < 1 || count > maxSecretFields {
			return fmt.Errorf("secret %q has %d fields", secretID, count)
		}
	}
	for fieldID, field := range ic.secretFields {
		if !field.sensitive {
			if ic.secretCurrentDEKs[fieldID] != 0 {
				return fmt.Errorf("public field %q has a DEK", fieldID)
			}
			continue
		}
		dek, ok := ic.secretDEKs[field.dekID]
		if !ok || dek.fieldID != fieldID || dek.generation != field.dekGeneration || dek.retired {
			return fmt.Errorf("sensitive field %q does not reference its current DEK", fieldID)
		}
		if key, ok := ic.importedVaultKey(dek.wrappingKeyID, dek.wrappingVersion); !ok ||
			key.state != "current" {
			return fmt.Errorf("sensitive field %q is not wrapped by the current vault key", fieldID)
		}
		if ic.secretCurrentDEKs[fieldID] != 1 {
			return fmt.Errorf("sensitive field %q has %d current DEKs", fieldID, ic.secretCurrentDEKs[fieldID])
		}
	}
	return nil
}

func (ic *importCtx) validateImportedSecretReceiptResult(operation, targetKind, targetID string, revision, valueVersion int64, valueVersionPresent bool) error {
	var currentRevision int64
	switch targetKind {
	case "key_epoch":
		currentRevision = ic.vaultKeys[targetID].rowVersion
	case "secret":
		currentRevision = ic.secrets[targetID].rowVersion
	case "field":
		currentRevision = ic.secretFields[targetID].rowVersion
	case "dek":
		currentRevision = ic.secretDEKs[targetID].rowVersion
	}
	if currentRevision < 1 || revision > currentRevision {
		return fmt.Errorf("result_revision exceeds target revision")
	}
	switch operation {
	case "secret_delete":
		target := ic.secrets[targetID]
		if !target.deleted || revision != currentRevision {
			return fmt.Errorf("secret delete result does not match the deleted target")
		}
		if valueVersionPresent {
			return fmt.Errorf("result_value_version is not valid for operation")
		}
	case "secret_update":
		if valueVersionPresent && (valueVersion < 1 || valueVersion > ic.secretMaxValueVersions[targetID]) {
			return fmt.Errorf("result_value_version exceeds the secret field versions")
		}
	case "field_access":
		field := ic.secretFields[targetID]
		if !valueVersionPresent || valueVersion > field.valueVersion {
			return fmt.Errorf("field access result_value_version is invalid")
		}
	default:
		if valueVersionPresent {
			return fmt.Errorf("result_value_version is not valid for operation")
		}
	}
	return nil
}

func importedByteaLength(raw any) (int, bool) {
	value, ok := raw.(string)
	if !ok || !strings.HasPrefix(value, `\x`) || (len(value)-2)%2 != 0 {
		return 0, false
	}
	for _, char := range value[2:] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return 0, false
		}
	}
	return (len(value) - 2) / 2, true
}

func validImportedSecretOperation(operation string) bool {
	switch operation {
	case "key_register", "secret_create", "secret_update", "secret_archive",
		"secret_restore", "secret_delete", "dek_rewrap", "field_access":
		return true
	default:
		return false
	}
}

func validImportedSecretOperationTarget(operation, targetKind string) bool {
	switch operation {
	case "key_register":
		return targetKind == "key_epoch"
	case "secret_create", "secret_update", "secret_archive", "secret_restore",
		"secret_delete":
		return targetKind == "secret"
	case "dek_rewrap":
		return targetKind == "dek"
	case "field_access":
		return targetKind == "field"
	default:
		return false
	}
}
