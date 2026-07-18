package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

var importedAvatarStyleIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,127}$`)

const importedLegacyBuiltInAvatarDescription = "A consistent flat-vector portrait system for human, animal, insect, and other agent forms."

var importedLegacyBuiltInAvatarStyleSpec = json.RawMessage(`{
  "canvas":{"width":512,"height":512,"view_box":"0 0 512 512"},
  "crop":"head_and_shoulders",
  "pose":"front_three_quarter",
  "palette":{"maximum_colors":12},
  "layers":["background","base-identity","expression","attire","experience"],
  "subject_forms":["human","animal","insect","anthropomorphic","hybrid","robot","symbolic"]
}`)

type avatarStyleHeadImportKey struct {
	realmID     string
	stylePackID string
}

type avatarStyleVersionImportKey struct {
	realmID     string
	stylePackID string
	version     int64
}

type avatarStyleHeadImportScope struct {
	currentVersion int64
	revision       int64
}

type avatarStyleVersionImportScope struct {
	pack            avatardomain.StylePack
	previousVersion int64
}

type realmAvatarStyleImportScope struct {
	style    avatarStyleVersionImportKey
	revision int64
}

type avatarProfileImportScope struct {
	agentID                   string
	realmID                   string
	lineage                   int64
	style                     avatarStyleVersionImportKey
	status                    avatardomain.Status
	policy                    avatardomain.AutonomyPolicy
	subjectForm               avatardomain.SubjectForm
	latestVersion             int64
	proposedVersion           int64
	activeVersion             int64
	revision                  int64
	retainedPayloadCountLimit int64
	retainedPayloadByteLimit  int64
}

type avatarVersionImportKey struct {
	agentID string
	version int64
}

type avatarVersionImportScope struct {
	id                    string
	realmID               string
	lineage               int64
	style                 avatarStyleVersionImportKey
	parentVersion         int64
	subjectForm           avatardomain.SubjectForm
	svg                   string
	proposedByKind        string
	proposedAt            time.Time
	payloadState          avatardomain.PayloadState
	payloadBytes          int64
	payloadCompactedAt    *time.Time
	lockedLayersSHA256    string
	continuityFingerprint bool
}

func (ic *importCtx) normalizeLegacyImportedAvatarPayloadFields(table string, obj map[string]any) error {
	if ic.schemaVersion >= 51 {
		return nil
	}
	switch table {
	case "agent_avatar_profiles":
		obj["retained_payload_count_limit"] = AvatarDefaultRetainedPayloadCountLimit
		obj["retained_payload_byte_limit"] = AvatarDefaultRetainedPayloadByteLimit
	case "agent_avatar_versions":
		svg, svgOK := obj["svg"].(string)
		description, descriptionOK := obj["description"].(string)
		visualSpec, specOK := obj["visual_spec"].(map[string]any)
		if !svgOK || !descriptionOK || !specOK {
			return fmt.Errorf("%w: legacy avatar version payload is incomplete", ErrArchiveContent)
		}
		rawSpec, err := json.Marshal(visualSpec)
		if err != nil {
			return fmt.Errorf("%w: legacy avatar visual_spec is invalid", ErrArchiveContent)
		}
		payloadBytes, err := avatarCreativePayloadBytes(svg, description, rawSpec)
		if err != nil {
			return fmt.Errorf("%w: legacy avatar payload byte count is invalid", ErrArchiveContent)
		}
		obj["payload_state"] = string(avatardomain.PayloadFull)
		obj["payload_bytes"] = payloadBytes
		obj["payload_compacted_at"] = nil
		obj["payload_compaction_reason"] = nil
	}
	return nil
}

type avatarLineageImportKey struct {
	agentID    string
	generation int64
}

type avatarResetImportScope struct {
	sequence        int64
	retiredLineage  int64
	newLineage      int64
	retiredActive   int64
	retiredProposed int64
	actorKind       string
	actorID         string
	resetAt         time.Time
}

func (ic *importCtx) recordAvatarLineageTime(key avatarLineageImportKey, at time.Time) {
	if current := ic.avatarLineageEarliestAt[key]; current.IsZero() || at.Before(current) {
		ic.avatarLineageEarliestAt[key] = at
	}
	if current := ic.avatarLineageLatestAt[key]; current.IsZero() || at.After(current) {
		ic.avatarLineageLatestAt[key] = at
	}
}

func importedAvatarOptionalPositiveInteger(obj map[string]any, field string) (int64, bool, error) {
	raw, present := obj[field]
	if !present || raw == nil {
		return 0, false, nil
	}
	value, ok := importedPositiveInteger(raw)
	if !ok {
		return 0, false, fmt.Errorf("%s must be a positive integer or null", field)
	}
	return value, true, nil
}

func (ic *importCtx) validateImportedAvatarTimes(obj map[string]any, fields ...string) error {
	for _, field := range fields {
		value, err := requireImportedTimestamp(obj, field)
		if err != nil {
			return fmt.Errorf("%s is invalid", field)
		}
		if err := ic.requireTimestampAtOrBeforeExport(field, *value); err != nil {
			return err
		}
	}
	return nil
}

func (ic *importCtx) validateImportedAvatarStyleHead(obj map[string]any) (avatarStyleHeadImportKey, avatarStyleHeadImportScope, error) {
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || !ic.realms[realmID] {
		return avatarStyleHeadImportKey{}, avatarStyleHeadImportScope{}, fmt.Errorf("references realm %q not present in this archive", realmID)
	}
	stylePackID, err := requireStringField(obj, "id")
	if err != nil || !importedAvatarStyleIDPattern.MatchString(stylePackID) {
		return avatarStyleHeadImportKey{}, avatarStyleHeadImportScope{}, fmt.Errorf("style pack id is invalid")
	}
	currentVersion, ok := importedPositiveInteger(obj["current_version"])
	if !ok {
		return avatarStyleHeadImportKey{}, avatarStyleHeadImportScope{}, fmt.Errorf("current_version is invalid")
	}
	revision, ok := importedPositiveInteger(obj["revision"])
	if !ok {
		return avatarStyleHeadImportKey{}, avatarStyleHeadImportScope{}, fmt.Errorf("revision is invalid")
	}
	if err := ic.validateImportedAvatarTimes(obj, "created_at", "updated_at"); err != nil {
		return avatarStyleHeadImportKey{}, avatarStyleHeadImportScope{}, err
	}
	return avatarStyleHeadImportKey{realmID: realmID, stylePackID: stylePackID}, avatarStyleHeadImportScope{
		currentVersion: currentVersion, revision: revision,
	}, nil
}

func (ic *importCtx) validateImportedAvatarStyleVersion(obj map[string]any) (avatarStyleVersionImportKey, avatarStyleVersionImportScope, error) {
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || !ic.realms[realmID] {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("references realm %q not present in this archive", realmID)
	}
	stylePackID, err := requireStringField(obj, "style_pack_id")
	if err != nil || !importedAvatarStyleIDPattern.MatchString(stylePackID) {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("style_pack_id is invalid")
	}
	version, ok := importedPositiveInteger(obj["version"])
	if !ok {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("version is invalid")
	}
	headKey := avatarStyleHeadImportKey{realmID: realmID, stylePackID: stylePackID}
	if _, exists := ic.avatarStyleHeads[headKey]; !exists {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("style head is not present")
	}
	previousVersion, hasPrevious, err := importedAvatarOptionalPositiveInteger(obj, "previous_version")
	if err != nil {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, err
	}
	if version == 1 && hasPrevious {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("first version has a previous_version")
	}
	if version > 1 {
		if !hasPrevious || previousVersion != version-1 {
			return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("version chain is not contiguous")
		}
		prior := avatarStyleVersionImportKey{realmID: realmID, stylePackID: stylePackID, version: previousVersion}
		if _, exists := ic.avatarStyleVersions[prior]; !exists {
			return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("previous style version is not present")
		}
	}
	name, err := requireStringField(obj, "name")
	if err != nil || strings.TrimSpace(name) == "" || len(name) > 256 {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("name is invalid")
	}
	description, err := requireStringField(obj, "description")
	if err != nil || strings.TrimSpace(description) == "" || len(description) > 8192 {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("description is invalid")
	}
	styleSpec, ok := obj["style_spec"].(map[string]any)
	if !ok {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("style_spec must be an object")
	}
	references, ok := obj["reference_examples"].([]any)
	if !ok {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("reference_examples must be an array")
	}
	if _, ok := obj["provenance"].(map[string]any); !ok {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("provenance must be an object")
	}
	createdByKind, err := requireStringField(obj, "created_by_kind")
	if err != nil || (createdByKind != PrincipalOperator && createdByKind != ActorSystem) {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("created_by_kind is invalid")
	}
	createdByID, _ := obj["created_by_id"].(string)
	if createdByKind == ActorSystem {
		if createdByID != "" {
			return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("system style version carries created_by_id")
		}
	} else if createdByID == "" || !ic.operators[createdByID] {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("created_by_id is outside the imported scope")
	}
	if err := ic.validateImportedAvatarTimes(obj, "created_at"); err != nil {
		return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, err
	}

	var pack avatardomain.StylePack
	if stylePackID == avatardomain.DefaultStylePackID && version == avatardomain.BuiltInStylePackVersion {
		if createdByKind != ActorSystem {
			return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("built-in style must be system-authored")
		}
		pack = avatardomain.BuiltInFlatVectorStylePack()
		if err := validateImportedBuiltInAvatarStyle(styleSpec, references, obj, pack); err != nil {
			return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, err
		}
	} else {
		if createdByKind != PrincipalOperator {
			return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("custom style must be operator-authored")
		}
		raw, err := json.Marshal(styleSpec)
		if err != nil || json.Unmarshal(raw, &pack) != nil {
			return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("style_spec cannot decode as a style pack")
		}
		if pack.ID != stylePackID || int64(pack.Version) != version || pack.Name != name || pack.Description != description {
			return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("style pack identity metadata disagrees")
		}
		if err := pack.Validate(); err != nil {
			return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("style pack is invalid: %v", err)
		}
		rawReferences, _ := json.Marshal(references)
		packReferences, _ := json.Marshal(pack.References)
		if !jsonValuesEqual(rawReferences, packReferences) {
			return avatarStyleVersionImportKey{}, avatarStyleVersionImportScope{}, fmt.Errorf("reference_examples disagree with style_spec")
		}
	}
	key := avatarStyleVersionImportKey{realmID: realmID, stylePackID: stylePackID, version: version}
	return key, avatarStyleVersionImportScope{pack: pack, previousVersion: previousVersion}, nil
}

// validateImportedBuiltInAvatarStyle permits the two representations that
// schema 0050 can legitimately have written: the compact migration backfill
// used for realms that already existed, and the complete domain style pack
// written by CreateRealm after that migration. Treating the built-in ID as a
// blanket bypass would let a checksummed but hostile archive persist arbitrary
// style metadata under Witself's trusted built-in identity.
func validateImportedBuiltInAvatarStyle(styleSpec map[string]any, references []any, obj map[string]any, pack avatardomain.StylePack) error {
	provenance, _ := obj["provenance"].(map[string]any)
	wantProvenance := json.RawMessage(`{"revision":"1","source":"witself.builtin"}`)
	rawProvenance, _ := json.Marshal(provenance)
	if !jsonValuesEqual(rawProvenance, wantProvenance) {
		return fmt.Errorf("built-in style provenance is not canonical")
	}

	rawSpec, _ := json.Marshal(styleSpec)
	rawReferences, _ := json.Marshal(references)
	canonicalPack, _ := json.Marshal(pack)
	canonicalReferences, _ := json.Marshal(pack.References)
	if jsonValuesEqual(rawSpec, canonicalPack) && jsonValuesEqual(rawReferences, canonicalReferences) {
		if obj["name"] != pack.Name || obj["description"] != pack.Description {
			return fmt.Errorf("built-in style identity metadata disagrees")
		}
		return nil
	}

	if !jsonValuesEqual(rawSpec, importedLegacyBuiltInAvatarStyleSpec) || len(references) != 0 ||
		obj["name"] != pack.Name || obj["description"] != importedLegacyBuiltInAvatarDescription {
		return fmt.Errorf("built-in style payload is not a recognized canonical representation")
	}
	return nil
}

func (ic *importCtx) validateImportedRealmAvatarStyle(obj map[string]any) (realmAvatarStyleImportScope, error) {
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || !ic.realms[realmID] {
		return realmAvatarStyleImportScope{}, fmt.Errorf("references realm %q not present in this archive", realmID)
	}
	stylePackID, err := requireStringField(obj, "style_pack_id")
	if err != nil {
		return realmAvatarStyleImportScope{}, fmt.Errorf("style_pack_id is required")
	}
	version, ok := importedPositiveInteger(obj["style_pack_version"])
	if !ok {
		return realmAvatarStyleImportScope{}, fmt.Errorf("style_pack_version is invalid")
	}
	style := avatarStyleVersionImportKey{realmID: realmID, stylePackID: stylePackID, version: version}
	if _, exists := ic.avatarStyleVersions[style]; !exists {
		return realmAvatarStyleImportScope{}, fmt.Errorf("selected style version is not present")
	}
	revision, ok := importedPositiveInteger(obj["revision"])
	if !ok {
		return realmAvatarStyleImportScope{}, fmt.Errorf("revision is invalid")
	}
	if err := ic.validateImportedAvatarTimes(obj, "created_at", "updated_at"); err != nil {
		return realmAvatarStyleImportScope{}, err
	}
	return realmAvatarStyleImportScope{style: style, revision: revision}, nil
}

func (ic *importCtx) validateImportedAvatarProfile(obj map[string]any) (avatarProfileImportScope, error) {
	agentID, err := requireStringField(obj, "agent_id")
	if err != nil || !ic.agents[agentID] {
		return avatarProfileImportScope{}, fmt.Errorf("references agent %q not present in this archive", agentID)
	}
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || ic.agentRealms[agentID] != realmID {
		return avatarProfileImportScope{}, fmt.Errorf("agent realm does not match")
	}
	stylePackID, err := requireStringField(obj, "style_pack_id")
	if err != nil {
		return avatarProfileImportScope{}, fmt.Errorf("style_pack_id is required")
	}
	styleVersion, ok := importedPositiveInteger(obj["style_pack_version"])
	if !ok {
		return avatarProfileImportScope{}, fmt.Errorf("style_pack_version is invalid")
	}
	style := avatarStyleVersionImportKey{realmID: realmID, stylePackID: stylePackID, version: styleVersion}
	if _, exists := ic.avatarStyleVersions[style]; !exists {
		return avatarProfileImportScope{}, fmt.Errorf("profile style version is not present")
	}
	statusText, err := requireStringField(obj, "status")
	status := avatardomain.Status(statusText)
	if err != nil || status.Validate() != nil {
		return avatarProfileImportScope{}, fmt.Errorf("status is invalid")
	}
	lineage, ok := importedPositiveInteger(obj["lineage_generation"])
	if !ok {
		return avatarProfileImportScope{}, fmt.Errorf("lineage_generation is invalid")
	}
	policyText, err := requireStringField(obj, "autonomy_policy")
	policy := avatardomain.AutonomyPolicy(policyText)
	if err != nil || policy.Validate() != nil {
		return avatarProfileImportScope{}, fmt.Errorf("autonomy_policy is invalid")
	}
	formText, err := requireStringField(obj, "subject_form")
	form := avatardomain.SubjectForm(formText)
	if err != nil || form.Validate() != nil {
		return avatarProfileImportScope{}, fmt.Errorf("subject_form is invalid")
	}
	latest, _, err := importedAvatarOptionalPositiveInteger(obj, "latest_avatar_version")
	if err != nil {
		return avatarProfileImportScope{}, err
	}
	proposed, _, err := importedAvatarOptionalPositiveInteger(obj, "proposed_avatar_version")
	if err != nil {
		return avatarProfileImportScope{}, err
	}
	active, _, err := importedAvatarOptionalPositiveInteger(obj, "active_avatar_version")
	if err != nil {
		return avatarProfileImportScope{}, err
	}
	attemptCount, ok := importedNonnegativeInteger(obj["attempt_count"])
	if !ok || attemptCount > 1000000 {
		return avatarProfileImportScope{}, fmt.Errorf("attempt_count is invalid")
	}
	fallbackSeed, err := requireStringField(obj, "fallback_seed")
	if err != nil || fallbackSeed != agentID {
		return avatarProfileImportScope{}, fmt.Errorf("fallback_seed must equal agent_id")
	}
	failureCode, _ := obj["failure_code"].(string)
	if failureCode != "" {
		if _, err := normalizeAvatarReasonCode(failureCode, true); err != nil {
			return avatarProfileImportScope{}, fmt.Errorf("failure_code is invalid")
		}
	}
	retryAfter, hasRetry, err := importedOptionalTimestamp(obj, "retry_after")
	if err != nil {
		return avatarProfileImportScope{}, fmt.Errorf("retry_after is invalid")
	}
	if status == avatardomain.StatusGenerationFailed {
		if failureCode == "" || !hasRetry || attemptCount < 1 {
			return avatarProfileImportScope{}, fmt.Errorf("generation_failed lifecycle is incomplete")
		}
		// retry_after is live scheduling authority, not historical metadata.
		// Bound it to the destination database clock captured before archive
		// streaming. The only allowance is the same five-minute cross-cell skew
		// accepted for a source manifest; checksums do not make archive-authored
		// timestamps trusted.
		if ic.importedAt.IsZero() {
			return avatarProfileImportScope{}, fmt.Errorf("generation_failed retry baseline is unavailable")
		}
		latestRetry := ic.importedAt.Add(maxAvatarGenerationBackoff + maxArchiveManifestFutureSkew)
		// A manifest timestamp is still untrusted, so it may only tighten the
		// destination-clock ceiling, never extend it.
		if !ic.exportedAt.IsZero() {
			manifestLatest := ic.exportedAt.Add(maxAvatarGenerationBackoff)
			if manifestLatest.Before(latestRetry) {
				latestRetry = manifestLatest
			}
		}
		if retryAfter == nil || retryAfter.After(latestRetry) {
			return avatarProfileImportScope{}, fmt.Errorf("retry_after exceeds destination import time plus maximum generation backoff and clock skew")
		}
	} else if failureCode != "" || hasRetry {
		return avatarProfileImportScope{}, fmt.Errorf("non-failed profile carries failure state")
	}
	revision, ok := importedPositiveInteger(obj["revision"])
	if !ok {
		return avatarProfileImportScope{}, fmt.Errorf("revision is invalid")
	}
	if err := ic.validateImportedAvatarTimes(obj, "created_at", "updated_at"); err != nil {
		return avatarProfileImportScope{}, err
	}
	retainedCountLimit, ok := importedPositiveInteger(obj["retained_payload_count_limit"])
	if !ok || retainedCountLimit < AvatarMinRetainedPayloadCountLimit ||
		retainedCountLimit > AvatarMaxRetainedPayloadCountLimit {
		return avatarProfileImportScope{}, fmt.Errorf("retained_payload_count_limit is invalid")
	}
	retainedByteLimit, ok := importedPositiveInteger(obj["retained_payload_byte_limit"])
	if !ok || retainedByteLimit < AvatarMinRetainedPayloadByteLimit ||
		retainedByteLimit > AvatarMaxRetainedPayloadByteLimit {
		return avatarProfileImportScope{}, fmt.Errorf("retained_payload_byte_limit is invalid")
	}
	return avatarProfileImportScope{
		agentID: agentID, realmID: realmID, lineage: lineage,
		style: style, status: status,
		policy: policy, subjectForm: form, latestVersion: latest,
		proposedVersion: proposed, activeVersion: active, revision: revision,
		retainedPayloadCountLimit: retainedCountLimit,
		retainedPayloadByteLimit:  retainedByteLimit,
	}, nil
}

func (ic *importCtx) validateImportedAvatarVersion(obj map[string]any) (avatarVersionImportKey, avatarVersionImportScope, error) {
	agentID, err := requireStringField(obj, "agent_id")
	profile, exists := ic.avatarProfiles[agentID]
	if err != nil || !exists {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("profile is not present")
	}
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || realmID != profile.realmID {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("agent realm does not match")
	}
	version, ok := importedPositiveInteger(obj["version"])
	if !ok {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("version is invalid")
	}
	key := avatarVersionImportKey{agentID: agentID, version: version}
	lineage, ok := importedPositiveInteger(obj["lineage_generation"])
	if !ok || lineage > profile.lineage {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("lineage_generation is invalid")
	}
	if version > 1 {
		if _, priorExists := ic.avatarVersions[avatarVersionImportKey{agentID: agentID, version: version - 1}]; !priorExists {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("version history is not contiguous")
		}
	}
	parent, hasParent, err := importedAvatarOptionalPositiveInteger(obj, "parent_version")
	if err != nil || (hasParent && parent >= version) {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("parent_version is invalid")
	}
	if hasParent {
		parentScope, exists := ic.avatarVersions[avatarVersionImportKey{agentID: agentID, version: parent}]
		if !exists {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("parent version is not present")
		}
		if parentScope.lineage != lineage {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("parent version crosses avatar lineages")
		}
	}
	id, err := requireStringField(obj, "id")
	if err != nil || !validImportedGeneratedID(id, "avver") {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("id is invalid")
	}
	stylePackID, err := requireStringField(obj, "style_pack_id")
	if err != nil {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("style_pack_id is required")
	}
	styleVersion, ok := importedPositiveInteger(obj["style_pack_version"])
	if !ok {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("style_pack_version is invalid")
	}
	style := avatarStyleVersionImportKey{realmID: realmID, stylePackID: stylePackID, version: styleVersion}
	styleScope, exists := ic.avatarStyleVersions[style]
	if !exists {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("style version is not present")
	}
	formText, err := requireStringField(obj, "subject_form")
	form := avatardomain.SubjectForm(formText)
	if err != nil || form.Validate() != nil || !styleScope.pack.HasSubjectForm(form) {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("subject_form is invalid for the style")
	}
	payloadStateText, err := requireStringField(obj, "payload_state")
	payloadState := avatardomain.PayloadState(payloadStateText)
	if err != nil || payloadState.Validate() != nil {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("payload_state is invalid")
	}
	payloadBytes, ok := importedPositiveInteger(obj["payload_bytes"])
	if !ok || payloadBytes > maxAvatarPayloadBytes {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("payload_bytes is invalid")
	}
	storedDigest, err := requireStringField(obj, "svg_sha256")
	if err != nil || !validFactSHA256(storedDigest) {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("svg_sha256 is invalid")
	}
	lockedLayersSHA256, lockedDigestErr := requireStringField(obj, "locked_layers_sha256")
	legacyLockedDigest := ic.schemaVersion < 51 && lockedDigestErr != nil
	if !legacyLockedDigest && (lockedDigestErr != nil || !validFactSHA256(lockedLayersSHA256)) {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("locked_layers_sha256 is invalid")
	}
	continuityFingerprint, err := importedAvatarContinuityFingerprint(obj)
	if err != nil {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, err
	}
	compactedAt, hasCompactedAt, err := importedOptionalTimestamp(obj, "payload_compacted_at")
	if err != nil {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("payload_compacted_at is invalid")
	}
	compactionReason, hasCompactionReason := optionalStringField(obj, "payload_compaction_reason")
	var svg string
	switch payloadState {
	case avatardomain.PayloadFull:
		if hasCompactedAt || hasCompactionReason {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("full payload carries compaction metadata")
		}
		description, descriptionErr := requireStringField(obj, "description")
		normalizedDescription, normalizeErr := avatardomain.NormalizeDescription(description)
		if descriptionErr != nil || normalizeErr != nil || normalizedDescription != description {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("description is not canonical")
		}
		visualSpec, specOK := obj["visual_spec"].(map[string]any)
		if !specOK {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("visual_spec must be an object")
		}
		rawSpec, _ := json.Marshal(visualSpec)
		canonicalSpec, normalizeSpecErr := avatardomain.NormalizeSpecJSON(rawSpec)
		if normalizeSpecErr != nil || !jsonValuesEqual(rawSpec, canonicalSpec) {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("visual_spec is invalid")
		}
		svgValue, svgErr := requireStringField(obj, "svg")
		if svgErr != nil {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("svg is required")
		}
		canonicalSVG, sanitizeErr := avatardomain.SanitizeSVGForStylePack([]byte(svgValue), styleScope.pack)
		if sanitizeErr != nil || string(canonicalSVG) != svgValue {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("svg is not canonical and style-valid")
		}
		digest := sha256.Sum256([]byte(svgValue))
		if storedDigest != hex.EncodeToString(digest[:]) {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("svg_sha256 mismatch")
		}
		derivedLockedDigest, digestErr := avatardomain.LockedLayersSHA256(canonicalSVG, styleScope.pack)
		if digestErr != nil {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("locked-layer projection is invalid")
		}
		if legacyLockedDigest {
			lockedLayersSHA256 = derivedLockedDigest
			obj["locked_layers_sha256"] = derivedLockedDigest
		} else if lockedLayersSHA256 != derivedLockedDigest {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("locked_layers_sha256 mismatch")
		}
		minimumBytes := int64(len(svgValue) + len(description) + len(canonicalSpec))
		if payloadBytes < minimumBytes {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("payload_bytes understates retained creative data")
		}
		svg = svgValue
	case avatardomain.PayloadCompacted:
		if legacyLockedDigest {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("legacy archive cannot contain compacted avatar payloads")
		}
		if obj["svg"] != nil || obj["description"] != nil || obj["visual_spec"] != nil {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("compacted payload retains creative data")
		}
		if !hasCompactedAt || !hasCompactionReason || compactionReason != "quota" {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("compacted payload metadata is incomplete")
		}
		if err := ic.requireTimestampAtOrBeforeExport("payload_compacted_at", *compactedAt); err != nil {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, err
		}
	}
	provenance, ok := obj["provenance"].(map[string]any)
	if !ok {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("provenance must be an object")
	}
	for field := range provenance {
		if field != "runtime" && field != "model" && field != "recipe" && field != "recipe_version" {
			return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("provenance contains unknown field %q", field)
		}
	}
	rawProvenance, _ := json.Marshal(provenance)
	var client AvatarClientProvenance
	if json.Unmarshal(rawProvenance, &client) != nil {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("provenance is invalid")
	}
	normalizedClient, err := normalizeAvatarClient(client)
	if err != nil || !reflect.DeepEqual(client, normalizedClient) {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("provenance is not canonical")
	}
	actorKind, err := requireStringField(obj, "proposed_by_kind")
	actorID, actorErr := requireStringField(obj, "proposed_by_id")
	if err != nil || actorErr != nil || !ic.validImportedAvatarActor(actorKind, actorID, realmID) {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("proposer is outside the imported scope")
	}
	if actorKind == PrincipalAgent && actorID != agentID {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("agent proposer does not own the avatar")
	}
	if actorKind == PrincipalAgent && hasParent {
		parentScope := ic.avatarVersions[avatarVersionImportKey{agentID: agentID, version: parent}]
		if parentScope.style == style {
			if parentScope.subjectForm != form {
				return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("same-style agent-authored evolution changes subject_form")
			}
			if parentScope.payloadState == avatardomain.PayloadFull && payloadState == avatardomain.PayloadFull {
				if err := avatardomain.ValidateLockedLayerContinuity(
					[]byte(parentScope.svg), []byte(svg), styleScope.pack); err != nil {
					return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("same-style agent-authored evolution violates locked-layer continuity: %v", err)
				}
			}
			if parentScope.lockedLayersSHA256 != lockedLayersSHA256 {
				return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("same-style agent-authored evolution violates retained locked-layer continuity")
			}
			if err := avatardomain.ValidatePerceptualContinuity(
				[]byte(parentScope.svg), []byte(svg), styleScope.pack); err != nil {
				return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("same-style agent-authored evolution violates perceptual continuity: %v", err)
			}
		}
	}
	if err := ic.validateImportedAvatarTimes(obj, "proposed_at"); err != nil {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, err
	}
	proposedAt, _ := requireImportedTimestamp(obj, "proposed_at")
	if hasCompactedAt && compactedAt.Before(*proposedAt) {
		return avatarVersionImportKey{}, avatarVersionImportScope{}, fmt.Errorf("payload was compacted before it was proposed")
	}
	return key, avatarVersionImportScope{
		id: id, realmID: realmID, lineage: lineage, style: style, parentVersion: parent,
		subjectForm: form, svg: svg, proposedByKind: actorKind, proposedAt: proposedAt.UTC(),
		payloadState: payloadState, payloadBytes: payloadBytes,
		payloadCompactedAt:    compactedAt,
		lockedLayersSHA256:    lockedLayersSHA256,
		continuityFingerprint: continuityFingerprint,
	}, nil
}

func importedAvatarContinuityFingerprint(obj map[string]any) (bool, error) {
	raw, present := obj["continuity_fingerprint"]
	if !present || raw == nil {
		return false, nil
	}
	encoded, ok := raw.(string)
	if !ok || !strings.HasPrefix(encoded, `\x`) || len(encoded) <= 2 || len(encoded)%2 != 0 {
		return false, fmt.Errorf("continuity_fingerprint is invalid")
	}
	decoded, err := hex.DecodeString(encoded[2:])
	if err != nil || len(decoded) < 1 || len(decoded) > 38*1024 {
		return false, fmt.Errorf("continuity_fingerprint is invalid")
	}
	return true, nil
}

func (ic *importCtx) validImportedAvatarActor(kind, id, realmID string) bool {
	switch kind {
	case PrincipalOperator:
		return ic.operators[id]
	case PrincipalAgent:
		return ic.agents[id] && ic.agentRealms[id] == realmID
	default:
		return false
	}
}

func (ic *importCtx) validateImportedAvatarActivation(obj map[string]any) error {
	id, err := requireStringField(obj, "id")
	if err != nil || !validImportedGeneratedID(id, "avact") || ic.avatarLedgerIDs[id] {
		return fmt.Errorf("id is invalid or duplicated")
	}
	agentID, err := requireStringField(obj, "agent_id")
	profile, exists := ic.avatarProfiles[agentID]
	if err != nil || !exists {
		return fmt.Errorf("profile is not present")
	}
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || realmID != profile.realmID {
		return fmt.Errorf("agent realm does not match")
	}
	version, ok := importedPositiveInteger(obj["avatar_version"])
	if !ok {
		return fmt.Errorf("avatar_version is invalid")
	}
	versionKey := avatarVersionImportKey{agentID: agentID, version: version}
	versionScope, exists := ic.avatarVersions[versionKey]
	if !exists {
		return fmt.Errorf("avatar version is not present")
	}
	lineage, ok := importedPositiveInteger(obj["lineage_generation"])
	if !ok || lineage != versionScope.lineage || lineage > profile.lineage {
		return fmt.Errorf("lineage_generation does not match the avatar version")
	}
	sequence, ok := importedPositiveInteger(obj["sequence"])
	if !ok || sequence != ic.avatarActivationSequence[agentID]+1 {
		return fmt.Errorf("activation sequence is invalid or non-contiguous")
	}
	prior, hasPrior, err := importedAvatarOptionalPositiveInteger(obj, "prior_active_version")
	if err != nil {
		return err
	}
	lineageKey := avatarLineageImportKey{agentID: agentID, generation: lineage}
	count := ic.avatarActivationCount[lineageKey]
	if count == 0 && hasPrior {
		return fmt.Errorf("first activation carries prior_active_version in its lineage")
	}
	if count > 0 && (!hasPrior || prior != ic.avatarActivationHeads[lineageKey]) {
		return fmt.Errorf("activation ledger prior pointer is inconsistent")
	}
	action, err := requireStringField(obj, "action")
	if err != nil || (action != "activated" && action != "rolled_back") {
		return fmt.Errorf("action is invalid")
	}
	if action == "activated" && ic.avatarActivatedVersions[versionKey] {
		return fmt.Errorf("activation repeats a previously active version without rollback semantics")
	}
	if action == "activated" && count > 0 && version <= prior {
		return fmt.Errorf("ordinary activation does not advance to a new avatar version")
	}
	if action == "activated" {
		if count == 0 && versionScope.parentVersion != 0 {
			return fmt.Errorf("first ordinary activation version carries a parent in its lineage")
		}
		if count > 0 && versionScope.parentVersion != prior {
			return fmt.Errorf("ordinary activation version parent disagrees with prior_active_version")
		}
	}
	if action == "rolled_back" && (count == 0 || version == prior || !ic.avatarActivatedVersions[versionKey]) {
		return fmt.Errorf("rollback does not target a different previously active version")
	}
	actorKind, err := requireStringField(obj, "activated_by_kind")
	actorID, actorErr := requireStringField(obj, "activated_by_id")
	if err != nil || actorErr != nil || !ic.validImportedAvatarActor(actorKind, actorID, realmID) {
		return fmt.Errorf("activator is outside the imported scope")
	}
	if actorKind == PrincipalAgent && actorID != agentID {
		return fmt.Errorf("agent activator does not own the avatar")
	}
	if err := ic.validateImportedAvatarTimes(obj, "activated_at"); err != nil {
		return err
	}
	activatedAt, _ := requireImportedTimestamp(obj, "activated_at")
	ic.avatarLedgerIDs[id] = true
	ic.avatarActivationHeads[lineageKey] = version
	ic.avatarActivationCount[lineageKey] = count + 1
	ic.avatarActivationSequence[agentID] = sequence
	ic.avatarActivatedVersions[versionKey] = true
	if previous := ic.avatarLastActivatedAt[versionKey]; previous.IsZero() || activatedAt.After(previous) {
		ic.avatarLastActivatedAt[versionKey] = activatedAt.UTC()
	}
	ic.recordAvatarLineageTime(lineageKey, activatedAt.UTC())
	return nil
}

func (ic *importCtx) validateImportedAvatarRejection(obj map[string]any) (avatarVersionImportKey, error) {
	id, err := requireStringField(obj, "id")
	if err != nil || !validImportedGeneratedID(id, "avrej") || ic.avatarLedgerIDs[id] {
		return avatarVersionImportKey{}, fmt.Errorf("id is invalid or duplicated")
	}
	agentID, err := requireStringField(obj, "agent_id")
	profile, exists := ic.avatarProfiles[agentID]
	if err != nil || !exists {
		return avatarVersionImportKey{}, fmt.Errorf("profile is not present")
	}
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || realmID != profile.realmID {
		return avatarVersionImportKey{}, fmt.Errorf("agent realm does not match")
	}
	version, ok := importedPositiveInteger(obj["avatar_version"])
	key := avatarVersionImportKey{agentID: agentID, version: version}
	if !ok {
		return avatarVersionImportKey{}, fmt.Errorf("avatar_version is invalid")
	}
	if _, exists := ic.avatarVersions[key]; !exists {
		return avatarVersionImportKey{}, fmt.Errorf("avatar version is not present")
	}
	if ic.avatarActivatedVersions[key] {
		return avatarVersionImportKey{}, fmt.Errorf("an activated avatar version cannot be rejected")
	}
	reasonCode, _ := obj["reason_code"].(string)
	if reasonCode != "" {
		if _, err := normalizeAvatarReasonCode(reasonCode, true); err != nil {
			return avatarVersionImportKey{}, fmt.Errorf("reason_code is invalid")
		}
	}
	actorKind, err := requireStringField(obj, "rejected_by_kind")
	actorID, actorErr := requireStringField(obj, "rejected_by_id")
	if err != nil || actorErr != nil || actorKind != PrincipalOperator || !ic.operators[actorID] {
		return avatarVersionImportKey{}, fmt.Errorf("rejector is outside the imported scope")
	}
	if err := ic.validateImportedAvatarTimes(obj, "rejected_at"); err != nil {
		return avatarVersionImportKey{}, err
	}
	rejectedAt, _ := requireImportedTimestamp(obj, "rejected_at")
	ic.avatarLedgerIDs[id] = true
	ic.avatarRejectedAt[key] = rejectedAt.UTC()
	ic.recordAvatarLineageTime(avatarLineageImportKey{
		agentID: agentID, generation: ic.avatarVersions[key].lineage,
	}, rejectedAt.UTC())
	return key, nil
}

func (ic *importCtx) validateImportedAvatarReset(obj map[string]any) error {
	id, err := requireStringField(obj, "id")
	if err != nil || !validImportedGeneratedID(id, "avrst") || ic.avatarLedgerIDs[id] {
		return fmt.Errorf("id is invalid or duplicated")
	}
	agentID, err := requireStringField(obj, "agent_id")
	profile, exists := ic.avatarProfiles[agentID]
	if err != nil || !exists {
		return fmt.Errorf("profile is not present")
	}
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || realmID != profile.realmID {
		return fmt.Errorf("agent realm does not match")
	}
	sequence, ok := importedPositiveInteger(obj["sequence"])
	if !ok || sequence != ic.avatarResetCount[agentID]+1 {
		return fmt.Errorf("reset sequence is invalid or non-contiguous")
	}
	retiredLineage, ok := importedPositiveInteger(obj["retired_lineage_generation"])
	if !ok || retiredLineage != sequence {
		return fmt.Errorf("retired_lineage_generation is invalid or non-contiguous")
	}
	newLineage, ok := importedPositiveInteger(obj["new_lineage_generation"])
	if !ok || newLineage != retiredLineage+1 || newLineage > profile.lineage {
		return fmt.Errorf("new_lineage_generation is invalid")
	}
	retiredActive, hasActive, err := importedAvatarOptionalPositiveInteger(obj, "retired_active_version")
	if err != nil {
		return err
	}
	retiredProposed, hasProposed, err := importedAvatarOptionalPositiveInteger(obj, "retired_proposed_version")
	if err != nil {
		return err
	}
	if !hasActive && !hasProposed {
		return fmt.Errorf("reset retires neither an active nor proposed avatar")
	}
	if hasActive && hasProposed && retiredActive == retiredProposed {
		return fmt.Errorf("reset active and proposed pointers cannot be identical")
	}
	lineageKey := avatarLineageImportKey{agentID: agentID, generation: retiredLineage}
	if hasActive {
		key := avatarVersionImportKey{agentID: agentID, version: retiredActive}
		version, exists := ic.avatarVersions[key]
		if !exists || version.lineage != retiredLineage || !ic.avatarActivatedVersions[key] ||
			ic.avatarActivationHeads[lineageKey] != retiredActive {
			return fmt.Errorf("retired_active_version is not the retired lineage head")
		}
	} else if ic.avatarActivationCount[lineageKey] != 0 {
		return fmt.Errorf("reset omits the retired lineage active head")
	}
	if hasProposed {
		key := avatarVersionImportKey{agentID: agentID, version: retiredProposed}
		version, exists := ic.avatarVersions[key]
		if !exists || version.lineage != retiredLineage ||
			ic.avatarActivatedVersions[key] || ic.avatarRejectedVersions[key] {
			return fmt.Errorf("retired_proposed_version is not a pending avatar in the retired lineage")
		}
	}
	reasonCode, _ := obj["reason_code"].(string)
	if reasonCode != "" {
		if _, err := normalizeAvatarReasonCode(reasonCode, true); err != nil {
			return fmt.Errorf("reason_code is invalid")
		}
	}
	actorKind, err := requireStringField(obj, "reset_by_kind")
	actorID, actorErr := requireStringField(obj, "reset_by_id")
	if err != nil || actorErr != nil || !ic.validImportedAvatarActor(actorKind, actorID, realmID) {
		return fmt.Errorf("reset actor is outside the imported scope")
	}
	if actorKind == PrincipalAgent && actorID != agentID {
		return fmt.Errorf("agent reset actor does not own the avatar")
	}
	if err := ic.validateImportedAvatarTimes(obj, "reset_at"); err != nil {
		return err
	}
	resetAt, _ := requireImportedTimestamp(obj, "reset_at")
	resetAtValue := resetAt.UTC()
	if latest := ic.avatarLineageLatestAt[lineageKey]; !latest.IsZero() && resetAtValue.Before(latest) {
		return fmt.Errorf("reset_at precedes lifecycle activity in the retired lineage")
	}
	newLineageKey := avatarLineageImportKey{agentID: agentID, generation: newLineage}
	if earliest := ic.avatarLineageEarliestAt[newLineageKey]; !earliest.IsZero() && earliest.Before(resetAtValue) {
		return fmt.Errorf("new avatar lineage activity precedes its reset boundary")
	}
	if priorReset := ic.avatarLastResetAt[agentID]; !priorReset.IsZero() && resetAtValue.Before(priorReset) {
		return fmt.Errorf("reset timestamps are out of sequence")
	}
	if _, duplicate := ic.avatarResets[lineageKey]; duplicate {
		return fmt.Errorf("retired avatar lineage is reset more than once")
	}
	ic.avatarLedgerIDs[id] = true
	ic.avatarResets[lineageKey] = avatarResetImportScope{
		sequence: sequence, retiredLineage: retiredLineage, newLineage: newLineage,
		retiredActive: retiredActive, retiredProposed: retiredProposed,
		actorKind: actorKind, actorID: actorID, resetAt: resetAtValue,
	}
	ic.avatarResetCount[agentID] = sequence
	ic.avatarLastResetAt[agentID] = resetAtValue
	return nil
}

func (ic *importCtx) validateImportedAvatarReceipt(obj map[string]any) error {
	realmID, err := requireStringField(obj, "realm_id")
	if err != nil || !ic.realms[realmID] {
		return fmt.Errorf("realm is outside the imported scope")
	}
	targetKind, err := requireStringField(obj, "target_kind")
	targetID, targetErr := requireStringField(obj, "target_id")
	if err != nil || targetErr != nil {
		return fmt.Errorf("target is required")
	}
	switch targetKind {
	case "avatar":
		profile, exists := ic.avatarProfiles[targetID]
		if !exists || profile.realmID != realmID {
			return fmt.Errorf("avatar target is outside the imported scope")
		}
	case "style_pack":
		if targetID != realmID {
			return fmt.Errorf("style target must equal realm_id")
		}
	default:
		return fmt.Errorf("target_kind is invalid")
	}
	actorKind, err := requireStringField(obj, "actor_kind")
	actorID, actorErr := requireStringField(obj, "actor_id")
	if err != nil || actorErr != nil || !ic.validImportedAvatarActor(actorKind, actorID, realmID) {
		return fmt.Errorf("actor is outside the imported scope")
	}
	operation, err := requireStringField(obj, "operation")
	allowed := map[string]bool{
		"propose": true, "activate": true, "reject": true, "rollback": true,
		"reset": true, "set_policy": true, "set_quota": true,
		"set_style": true, "fail": true,
	}
	if err != nil || !allowed[operation] {
		return fmt.Errorf("operation is invalid")
	}
	if targetKind == "style_pack" && operation != "set_style" {
		return fmt.Errorf("style receipt operation is invalid")
	}
	if targetKind == "avatar" && operation == "set_style" {
		return fmt.Errorf("avatar receipt operation is invalid")
	}
	if targetKind == "avatar" && actorKind == PrincipalAgent && actorID != targetID {
		return fmt.Errorf("agent receipt actor does not own the avatar")
	}
	switch operation {
	case "reject", "set_policy", "set_quota", "set_style":
		if actorKind != PrincipalOperator {
			return fmt.Errorf("operation requires an operator actor")
		}
	case "fail":
		if actorKind != PrincipalAgent || actorID != targetID {
			return fmt.Errorf("failure receipt requires its target agent actor")
		}
	}
	key, err := requireStringField(obj, "idempotency_key")
	if err != nil {
		return fmt.Errorf("idempotency_key is required")
	}
	if normalized, err := normalizeAvatarIdempotencyKey(key); err != nil || normalized != key {
		return fmt.Errorf("idempotency_key is not canonical")
	}
	requestHash, err := requireStringField(obj, "request_hash")
	if err != nil || !validFactSHA256(requestHash) {
		return fmt.Errorf("request_hash is invalid")
	}
	resultRevision, ok := importedPositiveInteger(obj["result_revision"])
	if !ok || resultRevision < 1 {
		return fmt.Errorf("result_revision is invalid")
	}
	resultVersion, hasResultVersion, err := importedAvatarOptionalPositiveInteger(obj, "result_version")
	if err != nil {
		return err
	}
	wantsResultVersion := operation == "propose" || operation == "activate" || operation == "reject" ||
		operation == "rollback" || operation == "set_style"
	if wantsResultVersion != hasResultVersion || (hasResultVersion && resultVersion < 1) {
		return fmt.Errorf("result_version is inconsistent with operation")
	}
	resultLineage, hasResultLineage, err := importedAvatarOptionalPositiveInteger(obj, "result_lineage_generation")
	if err != nil {
		return err
	}
	if (operation == "reset") != hasResultLineage {
		return fmt.Errorf("result_lineage_generation is inconsistent with operation")
	}
	if targetKind == "avatar" {
		profile := ic.avatarProfiles[targetID]
		if resultRevision > profile.revision {
			return fmt.Errorf("result_revision exceeds the avatar profile head")
		}
		if hasResultVersion {
			versionKey := avatarVersionImportKey{agentID: targetID, version: resultVersion}
			if _, exists := ic.avatarVersions[versionKey]; !exists {
				return fmt.Errorf("result_version is outside the avatar history")
			}
			if operation == "reject" && !ic.avatarRejectedVersions[versionKey] {
				return fmt.Errorf("reject receipt does not name a rejected version")
			}
			if (operation == "activate" || operation == "rollback") && !ic.avatarActivatedVersions[versionKey] {
				return fmt.Errorf("activation receipt does not name an activated version")
			}
		}
		if operation == "reset" {
			resetKey := avatarLineageImportKey{
				agentID: targetID, generation: resultLineage - 1,
			}
			reset, exists := ic.avatarResets[resetKey]
			if !exists || reset.newLineage != resultLineage {
				return fmt.Errorf("reset receipt does not identify an exact reset lifecycle row")
			}
			if reset.actorKind != actorKind || reset.actorID != actorID {
				return fmt.Errorf("reset receipt actor does not match the reset lifecycle row")
			}
			if ic.avatarResetReceipts[resetKey] {
				return fmt.Errorf("reset lifecycle row has more than one receipt")
			}
			ic.avatarResetReceipts[resetKey] = true
		}
	} else {
		selected := ic.realmAvatarStyles[realmID]
		if resultRevision > selected.revision || resultVersion != selected.style.version {
			return fmt.Errorf("style receipt result exceeds the selected style head")
		}
	}
	return ic.validateImportedAvatarTimes(obj, "created_at")
}

func (ic *importCtx) validateImportedAvatarGraph() error {
	for key, head := range ic.avatarStyleHeads {
		current := avatarStyleVersionImportKey{realmID: key.realmID, stylePackID: key.stylePackID, version: head.currentVersion}
		if _, exists := ic.avatarStyleVersions[current]; !exists {
			return fmt.Errorf("style head %s/%s points to missing version %d", key.realmID, key.stylePackID, head.currentVersion)
		}
	}
	for realmID := range ic.realms {
		selected, exists := ic.realmAvatarStyles[realmID]
		if !exists {
			return fmt.Errorf("realm %q has no selected avatar style", realmID)
		}
		head := ic.avatarStyleHeads[avatarStyleHeadImportKey{realmID: realmID, stylePackID: selected.style.stylePackID}]
		if selected.style.version != head.currentVersion {
			return fmt.Errorf("realm %q does not select its style pack head", realmID)
		}
	}
	for agentID := range ic.agents {
		profile, exists := ic.avatarProfiles[agentID]
		if !exists {
			return fmt.Errorf("agent %q has no avatar profile", agentID)
		}
		selected := ic.realmAvatarStyles[profile.realmID]
		if ic.liveAgents[agentID] && profile.style != selected.style {
			return fmt.Errorf("agent %q profile does not use the selected realm style", agentID)
		}
		if got, want := ic.avatarResetCount[agentID], profile.lineage-1; got != want {
			return fmt.Errorf("agent %q reset count %d does not establish lineage %d", agentID, got, profile.lineage)
		}
		for generation := int64(1); generation < profile.lineage; generation++ {
			resetKey := avatarLineageImportKey{agentID: agentID, generation: generation}
			reset, exists := ic.avatarResets[resetKey]
			if !exists || reset.sequence != generation || reset.retiredLineage != generation ||
				reset.newLineage != generation+1 {
				return fmt.Errorf("agent %q avatar lineage %d has no contiguous reset", agentID, generation)
			}
			if !ic.avatarResetReceipts[resetKey] {
				return fmt.Errorf("agent %q avatar lineage %d reset has no receipt", agentID, generation)
			}
		}
		maxVersion := int64(0)
		var retainedPayloadCount, retainedPayloadBytes int64
		firstActivatedVersion := map[int64]int64{}
		for key, version := range ic.avatarVersions {
			if key.agentID == agentID && key.version > maxVersion {
				maxVersion = key.version
			}
			if key.agentID != agentID {
				continue
			}
			if version.payloadState == avatardomain.PayloadFull {
				retainedPayloadCount++
				retainedPayloadBytes += version.payloadBytes
			}
			if ic.avatarActivatedVersions[key] &&
				(firstActivatedVersion[version.lineage] == 0 || key.version < firstActivatedVersion[version.lineage]) {
				firstActivatedVersion[version.lineage] = key.version
			}
			if version.parentVersion > 0 {
				parentKey := avatarVersionImportKey{agentID: agentID, version: version.parentVersion}
				parent := ic.avatarVersions[parentKey]
				if parent.lineage != version.lineage {
					return fmt.Errorf("agent %q avatar version %d names a parent in another lineage", agentID, key.version)
				}
				if !ic.avatarActivatedVersions[parentKey] {
					return fmt.Errorf("agent %q avatar version %d names a parent that was never activated", agentID, key.version)
				}
			}
			if version.payloadState == avatardomain.PayloadCompacted && version.continuityFingerprint {
				needed := false
				for childKey, child := range ic.avatarVersions {
					if childKey.agentID == agentID && child.parentVersion == key.version &&
						child.lineage == version.lineage && child.payloadState == avatardomain.PayloadFull &&
						child.proposedByKind == PrincipalAgent && child.style == version.style {
						needed = true
						break
					}
				}
				if !needed {
					return fmt.Errorf("agent %q compacted avatar version %d retains an obsolete continuity_fingerprint", agentID, key.version)
				}
			}
			if version.payloadState == avatardomain.PayloadCompacted {
				compactedAt := *version.payloadCompactedAt
				if activatedAt := ic.avatarLastActivatedAt[key]; !activatedAt.IsZero() && compactedAt.Before(activatedAt) {
					return fmt.Errorf("agent %q avatar version %d was activated after payload compaction", agentID, key.version)
				}
				if rejectedAt := ic.avatarRejectedAt[key]; !rejectedAt.IsZero() && compactedAt.Before(rejectedAt) {
					return fmt.Errorf("agent %q avatar version %d was rejected after payload compaction", agentID, key.version)
				}
				if reset, exists := ic.avatarResets[avatarLineageImportKey{
					agentID: agentID, generation: version.lineage,
				}]; exists && (reset.retiredActive == key.version || reset.retiredProposed == key.version) &&
					compactedAt.Before(reset.resetAt) {
					return fmt.Errorf("agent %q avatar version %d remained protected until after its reset boundary", agentID, key.version)
				}
			}
		}
		// Schema-51 writers enforce this invariant on every quota change and
		// proposal. Pre-51 archives had no quota contract and are allowed through
		// with migration defaults so the first subsequent proposal can compact
		// their legacy history deterministically.
		if ic.schemaVersion >= 51 &&
			(retainedPayloadCount > profile.retainedPayloadCountLimit ||
				retainedPayloadBytes > profile.retainedPayloadByteLimit) {
			return fmt.Errorf("agent %q retained avatar payloads exceed the configured quota", agentID)
		}
		for key, version := range ic.avatarVersions {
			if key.agentID != agentID {
				continue
			}
			if first := firstActivatedVersion[version.lineage]; first > 0 && key.version > first && version.parentVersion == 0 {
				return fmt.Errorf("agent %q avatar version %d omits a parent after activation in lineage %d", agentID, key.version, version.lineage)
			}
		}
		type importedRollbackPayload struct {
			version int64
			state   avatardomain.PayloadState
			at      time.Time
		}
		rollbackPayloads := make([]importedRollbackPayload, 0)
		for key, version := range ic.avatarVersions {
			if key.agentID != agentID || version.lineage != profile.lineage ||
				key.version == profile.activeVersion || !ic.avatarActivatedVersions[key] ||
				ic.avatarRejectedVersions[key] {
				continue
			}
			rollbackPayloads = append(rollbackPayloads, importedRollbackPayload{
				version: key.version, state: version.payloadState,
				at: ic.avatarLastActivatedAt[key],
			})
		}
		sort.Slice(rollbackPayloads, func(i, j int) bool {
			if !rollbackPayloads[i].at.Equal(rollbackPayloads[j].at) {
				return rollbackPayloads[i].at.After(rollbackPayloads[j].at)
			}
			return rollbackPayloads[i].version > rollbackPayloads[j].version
		})
		for i := 0; i < len(rollbackPayloads) && i < AvatarRollbackPayloadFloor; i++ {
			if rollbackPayloads[i].state != avatardomain.PayloadFull {
				return fmt.Errorf("agent %q compacted protected rollback avatar version %d", agentID, rollbackPayloads[i].version)
			}
		}
		if profile.latestVersion != maxVersion {
			return fmt.Errorf("agent %q latest avatar version %d does not match history head %d", agentID, profile.latestVersion, maxVersion)
		}
		var active avatarVersionImportScope
		if profile.activeVersion > 0 {
			var exists bool
			active, exists = ic.avatarVersions[avatarVersionImportKey{agentID: agentID, version: profile.activeVersion}]
			if !exists || active.lineage != profile.lineage ||
				active.payloadState != avatardomain.PayloadFull ||
				ic.avatarRejectedVersions[avatarVersionImportKey{agentID: agentID, version: profile.activeVersion}] {
				return fmt.Errorf("agent %q active avatar is missing or rejected", agentID)
			}
			lineageKey := avatarLineageImportKey{agentID: agentID, generation: profile.lineage}
			if ic.avatarActivationCount[lineageKey] == 0 || ic.avatarActivationHeads[lineageKey] != profile.activeVersion {
				return fmt.Errorf("agent %q active pointer disagrees with activation ledger", agentID)
			}
		} else if ic.avatarActivationCount[avatarLineageImportKey{agentID: agentID, generation: profile.lineage}] != 0 {
			return fmt.Errorf("agent %q current lineage has activation history but no active pointer", agentID)
		}
		hasActive := profile.activeVersion > 0
		hasProposed := profile.proposedVersion > 0
		switch profile.status {
		case avatardomain.StatusActive, avatardomain.StatusEvolutionDue:
			if !hasActive || hasProposed {
				return fmt.Errorf("agent %q status %q requires an active avatar and no proposal", agentID, profile.status)
			}
			if profile.status == avatardomain.StatusActive && active.style != profile.style {
				return fmt.Errorf("agent %q active avatar style does not match the selected profile style", agentID)
			}
		case avatardomain.StatusPlaceholder, avatardomain.StatusGenerationDue, avatardomain.StatusRejected:
			if hasActive || hasProposed {
				return fmt.Errorf("agent %q status %q requires no active avatar or proposal", agentID, profile.status)
			}
		case avatardomain.StatusProposed:
			if !hasProposed {
				return fmt.Errorf("agent %q proposed status has no proposal", agentID)
			}
		case avatardomain.StatusGenerationFailed:
			if hasProposed {
				return fmt.Errorf("agent %q generation failure cannot retain a proposal", agentID)
			}
		case avatardomain.StatusArchived:
			if ic.liveAgents[agentID] || hasProposed {
				return fmt.Errorf("agent %q archived avatar requires a deleted agent and no proposal", agentID)
			}
		}
		if profile.proposedVersion > 0 {
			proposed, exists := ic.avatarVersions[avatarVersionImportKey{agentID: agentID, version: profile.proposedVersion}]
			if !exists || proposed.lineage != profile.lineage || proposed.style != profile.style ||
				proposed.payloadState != avatardomain.PayloadFull ||
				ic.avatarRejectedVersions[avatarVersionImportKey{agentID: agentID, version: profile.proposedVersion}] {
				return fmt.Errorf("agent %q proposed avatar is missing, rejected, or off-style", agentID)
			}
			if profile.status != avatardomain.StatusProposed {
				return fmt.Errorf("agent %q proposed pointer has status %q", agentID, profile.status)
			}
			if profile.proposedVersion != profile.latestVersion || proposed.subjectForm != profile.subjectForm {
				return fmt.Errorf("agent %q proposed avatar disagrees with profile projection", agentID)
			}
			if proposed.parentVersion != profile.activeVersion {
				return fmt.Errorf("agent %q proposed avatar parent does not match the active avatar", agentID)
			}
		} else if profile.activeVersion > 0 && active.subjectForm != profile.subjectForm {
			return fmt.Errorf("agent %q subject form disagrees with active avatar", agentID)
		}
	}
	return nil
}

// synthesizeLegacyImportedAvatars supplies the model-free defaults that
// migration 0050 would have backfilled on the source cell. A pre-50 archive
// cannot contain avatar streams, and the destination database was migrated
// before this account existed, so the import transaction must create those
// rows after realms and agents land. No synthetic account events are added:
// an archive restore preserves its audit ledger exactly.
func synthesizeLegacyImportedAvatars(ctx context.Context, tx pgx.Tx, ic *importCtx) error {
	for realmID := range ic.realms {
		if err := createDefaultRealmAvatarStyleTx(ctx, tx, ic.accountID, realmID); err != nil {
			return err
		}
	}
	for agentID := range ic.agents {
		realmID := ic.agentRealms[agentID]
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_avatar_profiles
			       (account_id, realm_id, agent_id, style_pack_id,
			        style_pack_version, fallback_seed)
			SELECT $1, $2, $3, style_pack_id, style_pack_version, $3
			  FROM realm_avatar_styles
			 WHERE account_id=$1 AND realm_id=$2`, ic.accountID, realmID, agentID); err != nil {
			return fmt.Errorf("create legacy imported avatar profile: %w", err)
		}
	}
	return nil
}
