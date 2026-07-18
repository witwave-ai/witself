package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

type avatarPayloadCandidate struct {
	version                 int64
	lineage                 int64
	bytes                   int64
	qualifyingParentVersion int64
	wasActivated            bool
	rejected                bool
	lastActivatedAt         *time.Time
}

type avatarPayloadCompactionPlan struct {
	versions              []int64
	count                 int
	compactedPayloadBytes int64
	netReclaimedBytes     int64
	retainedCount         int
	retainedBytes         int64
}

type avatarPayloadQuotaVersion struct {
	avatarPayloadCandidate
	parentVersion    int64
	stylePackID      string
	stylePackVersion int
	subjectForm      avatardomain.SubjectForm
	proposedByKind   string
	proposedByID     string
	payloadState     avatardomain.PayloadState
	rendererProfile  avatardomain.RendererProfile
	fingerprintBytes int64
}

type avatarCompactionFingerprintSource struct {
	version          int64
	stylePackID      string
	stylePackVersion int
	svg              string
	lockedDigest     string
}

type avatarCompactionFingerprintChild struct {
	parentVersion int64
	version       int64
	svg           string
	lockedDigest  string
}

type avatarCompactionSubjectMismatch struct {
	parentVersion int64
	childVersion  int64
}

// planAvatarPayloadCompaction is deterministic and deliberately separates
// eligibility from SQL. Protected pointers and the rollback floor are never
// returned, even when the configured quota cannot accommodate them. The
// lifecycle-ordered min-delta index finds the first candidate that fits the
// cleanup's non-growth headroom in O(log F), with only local parent/child point
// updates, so a pre-quota legacy history does not turn planning quadratic.
func planAvatarPayloadCompaction(candidates []avatarPayloadCandidate,
	existingFingerprintBytes map[int64]int64, currentLineage,
	activeVersion, proposedVersion int64, incomingCount int, incomingBytes int64,
	countLimit int, byteLimit int64) (avatarPayloadCompactionPlan, error) {
	plan := avatarPayloadCompactionPlan{
		retainedCount: len(candidates) + incomingCount,
		retainedBytes: incomingBytes,
	}
	for _, candidate := range candidates {
		plan.retainedBytes += candidate.bytes
	}
	workingFingerprints := make(map[int64]int64, len(existingFingerprintBytes))
	for version, size := range existingFingerprintBytes {
		if size > 0 {
			workingFingerprints[version] = size
			plan.retainedBytes += size
		}
	}
	preCleanupBytes := plan.retainedBytes
	qualifyingChildCount := make(map[int64]int)
	qualifyingChildren := make(map[int64][]int64)
	for _, candidate := range candidates {
		if candidate.qualifyingParentVersion == 0 {
			continue
		}
		qualifyingChildCount[candidate.qualifyingParentVersion]++
		qualifyingChildren[candidate.qualifyingParentVersion] = append(
			qualifyingChildren[candidate.qualifyingParentVersion], candidate.version)
	}
	// Obsolete fingerprints are independently reclaimable metadata. Prune them
	// in the projection before considering irreversible SVG compaction so a
	// protected-only history can reconcile a mixed-writer boundary without
	// destroying another payload merely to begin cleanup.
	for parent, size := range workingFingerprints {
		if qualifyingChildCount[parent] == 0 {
			plan.retainedBytes -= size
			delete(workingFingerprints, parent)
		}
	}
	plan.netReclaimedBytes = preCleanupBytes - plan.retainedBytes
	if plan.retainedCount <= countLimit && plan.retainedBytes <= byteLimit {
		return plan, nil
	}

	protectedRollback := make(map[int64]bool, AvatarRollbackPayloadFloor)
	rollbackCandidates := make([]avatarPayloadCandidate, 0)
	for _, candidate := range candidates {
		if candidate.version == activeVersion || candidate.version == proposedVersion ||
			candidate.lineage != currentLineage || candidate.rejected ||
			!candidate.wasActivated {
			continue
		}
		rollbackCandidates = append(rollbackCandidates, candidate)
	}
	sort.Slice(rollbackCandidates, func(i, j int) bool {
		left, right := rollbackCandidates[i], rollbackCandidates[j]
		if left.lastActivatedAt != nil && right.lastActivatedAt != nil &&
			!left.lastActivatedAt.Equal(*right.lastActivatedAt) {
			return left.lastActivatedAt.After(*right.lastActivatedAt)
		}
		if left.lastActivatedAt != nil && right.lastActivatedAt == nil {
			return true
		}
		if left.lastActivatedAt == nil && right.lastActivatedAt != nil {
			return false
		}
		return left.version > right.version
	})
	for i := 0; i < len(rollbackCandidates) && i < AvatarRollbackPayloadFloor; i++ {
		protectedRollback[rollbackCandidates[i].version] = true
	}

	type eligiblePayload struct {
		avatarPayloadCandidate
		priority int
	}
	eligible := make([]eligiblePayload, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.version == activeVersion || candidate.version == proposedVersion ||
			protectedRollback[candidate.version] {
			continue
		}
		priority := 3 // old current-lineage rollback payload beyond the floor
		switch {
		case candidate.lineage != currentLineage:
			priority = 0 // retired lineage
		case candidate.rejected:
			priority = 1
		case !candidate.wasActivated:
			priority = 2 // otherwise non-rollbackable
		}
		eligible = append(eligible, eligiblePayload{candidate, priority})
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].priority != eligible[j].priority {
			return eligible[i].priority < eligible[j].priority
		}
		return eligible[i].version < eligible[j].version
	})

	eligibleIndex := make(map[int64]int, len(eligible))
	selected := make(map[int64]bool, len(eligible))
	deltas := make([]int64, len(eligible))
	for i := range eligible {
		eligibleIndex[eligible[i].version] = i
		deltas[i] = avatarPayloadCandidateDelta(eligible[i].avatarPayloadCandidate,
			qualifyingChildCount, workingFingerprints)
	}
	deltaIndex := newAvatarPayloadDeltaIndex(deltas)
	updateCandidate := func(version int64) {
		index, ok := eligibleIndex[version]
		if !ok || selected[version] {
			return
		}
		deltaIndex.update(index, avatarPayloadCandidateDelta(
			eligible[index].avatarPayloadCandidate, qualifyingChildCount,
			workingFingerprints))
	}
	remainingQualifyingChild := func(parent int64) int64 {
		for _, child := range qualifyingChildren[parent] {
			if !selected[child] {
				return child
			}
		}
		return 0
	}

	for plan.retainedCount > countLimit || plan.retainedBytes > byteLimit {
		headroom := preCleanupBytes - plan.retainedBytes
		index := deltaIndex.firstAtMost(headroom)
		if index < 0 {
			return avatarPayloadCompactionPlan{}, ErrAvatarPayloadQuotaExceeded
		}
		candidate := eligible[index].avatarPayloadCandidate
		delta := avatarPayloadCandidateDelta(candidate, qualifyingChildCount,
			workingFingerprints)
		plan.retainedBytes += delta
		plan.versions = append(plan.versions, candidate.version)
		plan.count++
		plan.compactedPayloadBytes += candidate.bytes
		plan.retainedCount--
		selected[candidate.version] = true
		deltaIndex.update(index, avatarPayloadDisabledDelta)

		if parent := candidate.qualifyingParentVersion; parent > 0 {
			before := qualifyingChildCount[parent]
			qualifyingChildCount[parent] = before - 1
			switch before {
			case 1:
				delete(workingFingerprints, parent)
				updateCandidate(parent)
			case 2:
				if _, retained := workingFingerprints[parent]; retained {
					updateCandidate(remainingQualifyingChild(parent))
				}
			}
		}
		if qualifyingChildCount[candidate.version] > 0 {
			workingFingerprints[candidate.version] = avatardomain.PerceptualContinuityFingerprintBytes
			if qualifyingChildCount[candidate.version] == 1 {
				updateCandidate(remainingQualifyingChild(candidate.version))
			}
		}
	}
	if plan.retainedCount > countLimit || plan.retainedBytes > byteLimit {
		return avatarPayloadCompactionPlan{}, ErrAvatarPayloadQuotaExceeded
	}
	plan.netReclaimedBytes = preCleanupBytes - plan.retainedBytes
	return plan, nil
}

func avatarPayloadCandidateDelta(candidate avatarPayloadCandidate,
	qualifyingChildCount map[int64]int, fingerprints map[int64]int64) int64 {
	delta := -candidate.bytes
	if qualifyingChildCount[candidate.version] > 0 {
		delta += avatardomain.PerceptualContinuityFingerprintBytes
	}
	if parent := candidate.qualifyingParentVersion; parent > 0 &&
		qualifyingChildCount[parent] == 1 {
		delta -= fingerprints[parent]
	}
	return delta
}

const avatarPayloadDisabledDelta int64 = 1 << 62

type avatarPayloadDeltaIndex struct {
	size int
	min  []int64
}

func newAvatarPayloadDeltaIndex(values []int64) *avatarPayloadDeltaIndex {
	size := 1
	for size < len(values) {
		size *= 2
	}
	index := &avatarPayloadDeltaIndex{size: size, min: make([]int64, size*2)}
	for i := range index.min {
		index.min[i] = avatarPayloadDisabledDelta
	}
	copy(index.min[size:], values)
	for i := size - 1; i > 0; i-- {
		index.min[i] = min(index.min[i*2], index.min[i*2+1])
	}
	return index
}

func (i *avatarPayloadDeltaIndex) update(position int, value int64) {
	if position < 0 || position >= i.size {
		return
	}
	position += i.size
	i.min[position] = value
	for position /= 2; position > 0; position /= 2 {
		i.min[position] = min(i.min[position*2], i.min[position*2+1])
	}
}

func (i *avatarPayloadDeltaIndex) firstAtMost(limit int64) int {
	if len(i.min) < 2 || i.min[1] > limit {
		return -1
	}
	position := 1
	for position < i.size {
		if i.min[position*2] <= limit {
			position *= 2
		} else {
			position = position*2 + 1
		}
	}
	return position - i.size
}

func compactAvatarPayloadsTx(ctx context.Context, tx pgx.Tx,
	target avatarTarget, profile avatarLockedProfile, incomingCount int,
	incomingBytes int64, countLimit int, byteLimit int64) (avatarPayloadCompactionPlan, error) {
	rows, err := tx.Query(ctx, `
		SELECT v.version, v.lineage_generation, v.payload_bytes,
		       v.parent_version, v.style_pack_id, v.style_pack_version,
		       v.subject_form, v.proposed_by_kind, v.proposed_by_id, v.payload_state,
		       v.renderer_profile,
		       COALESCE(octet_length(v.continuity_fingerprint),0),
		       EXISTS (
		         SELECT 1 FROM agent_avatar_activations activation
		          WHERE activation.account_id=v.account_id
		            AND activation.realm_id=v.realm_id
		            AND activation.agent_id=v.agent_id
		            AND activation.avatar_version=v.version
		            AND activation.lineage_generation=v.lineage_generation
		       ),
		       EXISTS (
		         SELECT 1 FROM agent_avatar_rejections rejection
		          WHERE rejection.account_id=v.account_id
		            AND rejection.realm_id=v.realm_id
		            AND rejection.agent_id=v.agent_id
		            AND rejection.avatar_version=v.version
		       ),
		       (SELECT MAX(activation.activated_at)
		          FROM agent_avatar_activations activation
		         WHERE activation.account_id=v.account_id
		           AND activation.realm_id=v.realm_id
		           AND activation.agent_id=v.agent_id
		           AND activation.avatar_version=v.version
		           AND activation.lineage_generation=v.lineage_generation)
		  FROM agent_avatar_versions v
		 WHERE v.account_id=$1 AND v.realm_id=$2 AND v.agent_id=$3
		 ORDER BY v.version`, target.accountID, target.realmID, target.agentID)
	if err != nil {
		return avatarPayloadCompactionPlan{}, fmt.Errorf("list avatar payload quota state: %w", err)
	}
	versions := make([]avatarPayloadQuotaVersion, 0)
	versionsByNumber := make(map[int64]avatarPayloadQuotaVersion)
	for rows.Next() {
		var version avatarPayloadQuotaVersion
		var parentVersion *int64
		var payloadState, rendererProfile string
		if err := rows.Scan(&version.version, &version.lineage,
			&version.bytes, &parentVersion, &version.stylePackID,
			&version.stylePackVersion, &version.subjectForm, &version.proposedByKind,
			&version.proposedByID, &payloadState, &rendererProfile,
			&version.fingerprintBytes,
			&version.wasActivated, &version.rejected,
			&version.lastActivatedAt); err != nil {
			rows.Close()
			return avatarPayloadCompactionPlan{}, err
		}
		if parentVersion != nil {
			version.parentVersion = *parentVersion
		}
		version.payloadState = avatardomain.PayloadState(payloadState)
		version.rendererProfile = avatardomain.RendererProfile(rendererProfile)
		versions = append(versions, version)
		versionsByNumber[version.version] = version
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return avatarPayloadCompactionPlan{}, err
	}
	rows.Close()
	candidates := make([]avatarPayloadCandidate, 0, len(versions))
	existingFingerprintBytes := make(map[int64]int64)
	subjectMismatches := make([]avatarCompactionSubjectMismatch, 0)
	for _, version := range versions {
		if version.fingerprintBytes > 0 {
			existingFingerprintBytes[version.version] = version.fingerprintBytes
		}
		var qualifyingParentVersion int64
		if version.parentVersion > 0 && version.proposedByKind == PrincipalAgent &&
			version.proposedByID == target.agentID {
			if parent, exists := versionsByNumber[version.parentVersion]; exists &&
				parent.lineage == version.lineage &&
				parent.stylePackID == version.stylePackID &&
				parent.stylePackVersion == version.stylePackVersion {
				if parent.subjectForm != version.subjectForm {
					subjectMismatches = append(subjectMismatches,
						avatarCompactionSubjectMismatch{
							parentVersion: version.parentVersion,
							childVersion:  version.version,
						})
				} else if parent.rendererProfile == avatardomain.RendererProfilePerceptualV1 &&
					version.rendererProfile == avatardomain.RendererProfilePerceptualV1 {
					qualifyingParentVersion = version.parentVersion
				}
			}
		}
		if version.payloadState != avatardomain.PayloadFull {
			continue
		}
		candidate := version.avatarPayloadCandidate
		candidate.qualifyingParentVersion = qualifyingParentVersion
		candidates = append(candidates, candidate)
	}
	var activeVersion, proposedVersion int64
	if profile.activeVersion != nil {
		activeVersion = *profile.activeVersion
	}
	if profile.proposedVersion != nil {
		proposedVersion = *profile.proposedVersion
	}
	plan, err := planAvatarPayloadCompaction(candidates, existingFingerprintBytes,
		profile.lineageGeneration, activeVersion, proposedVersion, incomingCount,
		incomingBytes, countLimit, byteLimit)
	if err != nil {
		return plan, err
	}
	selected := make(map[int64]bool, plan.count)
	for _, version := range plan.versions {
		selected[version] = true
	}
	for _, mismatch := range subjectMismatches {
		if selected[mismatch.parentVersion] || selected[mismatch.childVersion] {
			return avatarPayloadCompactionPlan{}, fmt.Errorf(
				"%w: avatar %d child %d changes subject_form across same-style self evolution",
				ErrAvatarConflict, mismatch.parentVersion, mismatch.childVersion)
		}
	}
	if err := validateAvatarNonPerceptualCompactionEdgesTx(
		ctx, tx, target); err != nil {
		return avatarPayloadCompactionPlan{}, err
	}
	if plan.count > 0 {
		fingerprints, err := buildAvatarCompactionFingerprintsTx(ctx, tx, target, plan)
		if err != nil {
			return avatarPayloadCompactionPlan{}, err
		}
		if err := validateAvatarPrunedContinuityBoundariesTx(ctx, tx, target, plan); err != nil {
			return avatarPayloadCompactionPlan{}, err
		}
		command, err := tx.Exec(ctx, `
			UPDATE agent_avatar_versions
			   SET payload_state='compacted', svg=NULL, description=NULL,
			       visual_spec=NULL, payload_compacted_at=clock_timestamp(),
			       payload_compaction_reason='quota', continuity_fingerprint=NULL
			 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3
			   AND version=ANY($4::bigint[]) AND payload_state='full'
			   AND ($5::bigint=0 OR version<>$5)
			   AND ($6::bigint=0 OR version<>$6)`, target.accountID, target.realmID,
			target.agentID, plan.versions, activeVersion, proposedVersion)
		if err != nil {
			return avatarPayloadCompactionPlan{}, fmt.Errorf("compact avatar payloads: %w", err)
		}
		if command.RowsAffected() != int64(plan.count) {
			return avatarPayloadCompactionPlan{}, ErrAvatarConflict
		}
		for _, version := range plan.versions {
			fingerprint, needed := fingerprints[version]
			if !needed {
				continue
			}
			command, err := tx.Exec(ctx, `
				UPDATE agent_avatar_versions
				   SET continuity_fingerprint=$5
				 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3
				   AND version=$4 AND payload_state='compacted'
				   AND continuity_fingerprint IS NULL`, target.accountID, target.realmID,
				target.agentID, version, fingerprint)
			if err != nil {
				return avatarPayloadCompactionPlan{}, fmt.Errorf("retain avatar continuity fingerprint: %w", err)
			}
			if command.RowsAffected() != 1 {
				return avatarPayloadCompactionPlan{}, ErrAvatarConflict
			}
		}
	}
	// A perceptual continuity fingerprint is useful on a compacted parent only
	// while a retained full, same-style, agent-authored direct child still
	// depends on that boundary proof. Clear every obsolete copy in the same
	// transaction so future fingerprint storage stays bounded by full payloads.
	if _, err := tx.Exec(ctx, `
		UPDATE agent_avatar_versions parent
		   SET continuity_fingerprint=NULL
		 WHERE parent.account_id=$1 AND parent.realm_id=$2 AND parent.agent_id=$3
		   AND parent.payload_state='compacted'
		   AND parent.continuity_fingerprint IS NOT NULL
		   AND NOT EXISTS (
		     SELECT 1 FROM agent_avatar_versions child
		      WHERE child.account_id=parent.account_id
		        AND child.realm_id=parent.realm_id
		        AND child.agent_id=parent.agent_id
		        AND child.parent_version=parent.version
		        AND child.lineage_generation=parent.lineage_generation
		        AND child.payload_state='full'
		        AND child.renderer_profile='perceptual-v1'
		        AND child.proposed_by_kind='agent'
		        AND child.proposed_by_id=child.agent_id
		        AND child.style_pack_id=parent.style_pack_id
		        AND child.style_pack_version=parent.style_pack_version
		        AND child.subject_form=parent.subject_form
		   )`, target.accountID, target.realmID, target.agentID); err != nil {
		return avatarPayloadCompactionPlan{}, fmt.Errorf("prune avatar continuity fingerprints: %w", err)
	}
	var retainedFullCount int
	var retainedContentBytes int64
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE payload_state='full'),
		       COALESCE(SUM(CASE WHEN payload_state='full' THEN payload_bytes ELSE 0 END),0) +
		       COALESCE(SUM(octet_length(continuity_fingerprint)),0)
		  FROM agent_avatar_versions
		 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3`, target.accountID,
		target.realmID, target.agentID).Scan(&retainedFullCount, &retainedContentBytes); err != nil {
		return avatarPayloadCompactionPlan{}, fmt.Errorf("verify avatar payload compaction: %w", err)
	}
	if retainedFullCount+incomingCount != plan.retainedCount ||
		retainedContentBytes+incomingBytes != plan.retainedBytes {
		return avatarPayloadCompactionPlan{}, ErrAvatarConflict
	}
	return plan, nil
}

// validateAvatarNonPerceptualCompactionEdgesTx proves every same-style,
// owner-authored edge that is not perceptual-v1 on both sides before an enabled
// quota pass succeeds. Those quarantined edges never create or consume WAPF.
// Their durable continuity authority is the exact locked-layer digest; while
// both SVGs are still present we additionally compare their normalized locked
// projections. Paging keeps legacy histories bounded in memory.
func validateAvatarNonPerceptualCompactionEdgesTx(ctx context.Context, tx pgx.Tx,
	target avatarTarget) error {
	type styleKey struct {
		id      string
		version int
	}
	type boundary struct {
		parentVersion, childVersion int64
		stylePackID                 string
		stylePackVersion            int
		parentSubject, childSubject avatardomain.SubjectForm
		parentState, childState     avatardomain.PayloadState
		parentProfile, childProfile avatardomain.RendererProfile
		parentSVG, childSVG         *string
		parentLocked, childLocked   string
	}
	var afterParent, afterChild int64
	for {
		rows, err := tx.Query(ctx, `
			SELECT parent.version,child.version,
			       parent.style_pack_id,parent.style_pack_version,
			       parent.subject_form,child.subject_form,
			       parent.payload_state,parent.renderer_profile,parent.svg,
			       parent.locked_layers_sha256,
			       child.payload_state,child.renderer_profile,child.svg,
			       child.locked_layers_sha256
			  FROM agent_avatar_versions parent
			  JOIN agent_avatar_versions child
			    ON child.account_id=parent.account_id
			   AND child.realm_id=parent.realm_id
			   AND child.agent_id=parent.agent_id
			   AND child.parent_version=parent.version
			   AND child.lineage_generation=parent.lineage_generation
			   AND child.style_pack_id=parent.style_pack_id
			   AND child.style_pack_version=parent.style_pack_version
			   AND child.proposed_by_kind='agent'
			   AND child.proposed_by_id=parent.agent_id
			 WHERE parent.account_id=$1 AND parent.realm_id=$2 AND parent.agent_id=$3
			   AND NOT (parent.renderer_profile='perceptual-v1' AND
			            child.renderer_profile='perceptual-v1')
			   AND (parent.version,child.version)>($4::bigint,$5::bigint)
			 ORDER BY parent.version,child.version
			 LIMIT $6`, target.accountID, target.realmID, target.agentID,
			afterParent, afterChild,
			avatarContinuityValidationBatchSize)
		if err != nil {
			return fmt.Errorf("load quarantined avatar continuity boundaries: %w", err)
		}
		batch := make([]boundary, 0, avatarContinuityValidationBatchSize)
		for rows.Next() {
			var item boundary
			var parentSubject, childSubject string
			var parentState, childState, parentProfile, childProfile string
			if err := rows.Scan(&item.parentVersion, &item.childVersion,
				&item.stylePackID, &item.stylePackVersion,
				&parentSubject, &childSubject,
				&parentState, &parentProfile, &item.parentSVG, &item.parentLocked,
				&childState, &childProfile, &item.childSVG, &item.childLocked); err != nil {
				rows.Close()
				return err
			}
			item.parentState = avatardomain.PayloadState(parentState)
			item.childState = avatardomain.PayloadState(childState)
			item.parentSubject = avatardomain.SubjectForm(parentSubject)
			item.childSubject = avatardomain.SubjectForm(childSubject)
			item.parentProfile = avatardomain.RendererProfile(parentProfile)
			item.childProfile = avatardomain.RendererProfile(childProfile)
			batch = append(batch, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}
		packs := make(map[styleKey]avatardomain.StylePack)
		for _, item := range batch {
			if item.parentSubject != item.childSubject {
				return fmt.Errorf("%w: avatar %d child %d changes subject form under the same style",
					ErrAvatarConflict, item.parentVersion, item.childVersion)
			}
			if item.parentProfile == avatardomain.RendererProfileLegacy &&
				item.childProfile == avatardomain.RendererProfilePerceptualV1 {
				return fmt.Errorf("%w: avatar %d child %d promotes a legacy renderer without an explicit baseline",
					ErrAvatarConflict, item.parentVersion, item.childVersion)
			}
			if item.parentLocked != item.childLocked {
				return fmt.Errorf("%w: avatar %d child %d violates retained locked-layer continuity",
					ErrAvatarConflict, item.parentVersion, item.childVersion)
			}
			key := styleKey{id: item.stylePackID, version: item.stylePackVersion}
			pack, ok := packs[key]
			if !ok {
				pack, err = loadAvatarStylePackVersion(ctx, tx, target.accountID,
					target.realmID, item.stylePackID, item.stylePackVersion)
				if err != nil {
					return err
				}
				packs[key] = pack
			}
			if item.parentState == avatardomain.PayloadFull {
				if item.parentSVG == nil {
					return ErrAvatarConflict
				}
				derived, digestErr := avatardomain.LockedLayersSHA256([]byte(*item.parentSVG), pack)
				if digestErr != nil || derived != item.parentLocked {
					return fmt.Errorf("%w: avatar %d retained locked-layer digest is invalid",
						ErrAvatarConflict, item.parentVersion)
				}
			}
			if item.childState == avatardomain.PayloadFull {
				if item.childSVG == nil {
					return ErrAvatarConflict
				}
				derived, digestErr := avatardomain.LockedLayersSHA256([]byte(*item.childSVG), pack)
				if digestErr != nil || derived != item.childLocked {
					return fmt.Errorf("%w: avatar %d child %d retained locked-layer digest is invalid",
						ErrAvatarConflict, item.parentVersion, item.childVersion)
				}
			}
			if item.parentState == avatardomain.PayloadFull &&
				item.childState == avatardomain.PayloadFull {
				if err := avatardomain.ValidateLockedLayerContinuity(
					[]byte(*item.parentSVG), []byte(*item.childSVG), pack); err != nil {
					return fmt.Errorf("%w: avatar %d child %d violates locked-layer continuity: %v",
						ErrAvatarConflict, item.parentVersion, item.childVersion, err)
				}
			}
		}
		afterParent = batch[len(batch)-1].parentVersion
		afterChild = batch[len(batch)-1].childVersion
	}
}

// buildAvatarCompactionFingerprintsTx snapshots the exact bounded perceptual
// boundary before planned parent SVGs are cleared. A compacted parent retains
// the projection only while a full direct child outside this plan still needs
// it for same-style, owner-authored continuity validation.
func buildAvatarCompactionFingerprintsTx(ctx context.Context, tx pgx.Tx,
	target avatarTarget, plan avatarPayloadCompactionPlan) (map[int64][]byte, error) {
	sources, err := loadAvatarCompactionFingerprintSourcesTx(ctx, tx, target, plan)
	if err != nil {
		return nil, err
	}
	childRows, err := tx.Query(ctx, `
		SELECT child.parent_version, child.version, child.svg,
		       child.locked_layers_sha256
		  FROM agent_avatar_versions child
		  JOIN agent_avatar_versions parent
		    ON parent.account_id=child.account_id
		   AND parent.realm_id=child.realm_id
		   AND parent.agent_id=child.agent_id
		   AND parent.version=child.parent_version
		 WHERE parent.account_id=$1 AND parent.realm_id=$2 AND parent.agent_id=$3
		   AND parent.version=ANY($4::bigint[]) AND parent.payload_state='full'
		   AND parent.renderer_profile='perceptual-v1'
		   AND child.lineage_generation=parent.lineage_generation
		   AND child.style_pack_id=parent.style_pack_id
		   AND child.style_pack_version=parent.style_pack_version
		   AND child.subject_form=parent.subject_form
		   AND child.payload_state='full'
		   AND child.renderer_profile='perceptual-v1'
		   AND NOT (child.version=ANY($4::bigint[]))
		   AND child.proposed_by_kind='agent'
		   AND child.proposed_by_id=parent.agent_id
		 ORDER BY child.parent_version, child.version`, target.accountID,
		target.realmID, target.agentID, plan.versions)
	if err != nil {
		return nil, fmt.Errorf("load retained avatar continuity children: %w", err)
	}
	children := make(map[int64][]avatarCompactionFingerprintChild)
	for childRows.Next() {
		var child avatarCompactionFingerprintChild
		if err := childRows.Scan(&child.parentVersion, &child.version,
			&child.svg, &child.lockedDigest); err != nil {
			childRows.Close()
			return nil, err
		}
		children[child.parentVersion] = append(children[child.parentVersion], child)
	}
	if err := childRows.Err(); err != nil {
		childRows.Close()
		return nil, err
	}
	childRows.Close()
	type styleKey struct {
		id      string
		version int
	}
	packs := make(map[styleKey]avatardomain.StylePack)
	fingerprints := make(map[int64][]byte)
	for _, source := range sources {
		retainedChildren := children[source.version]
		if len(retainedChildren) == 0 {
			return nil, ErrAvatarConflict
		}
		key := styleKey{id: source.stylePackID, version: source.stylePackVersion}
		pack, ok := packs[key]
		if !ok {
			pack, err = loadAvatarStylePackVersion(ctx, tx, target.accountID,
				target.realmID, source.stylePackID, source.stylePackVersion)
			if err != nil {
				return nil, err
			}
			packs[key] = pack
		}
		fingerprint, err := avatardomain.BuildPerceptualContinuityFingerprint(
			[]byte(source.svg), pack)
		if err != nil {
			return nil, fmt.Errorf("build avatar %d continuity fingerprint: %w", source.version, err)
		}
		if len(fingerprint) != avatardomain.PerceptualContinuityFingerprintBytes {
			return nil, fmt.Errorf("build avatar %d continuity fingerprint: exact length mismatch", source.version)
		}
		if err := avatardomain.ValidatePerceptualContinuityFingerprintForStyle(
			fingerprint, pack); err != nil {
			return nil, fmt.Errorf("validate avatar %d continuity fingerprint: %w", source.version, err)
		}
		parentLockedDigest, err := avatardomain.LockedLayersSHA256([]byte(source.svg), pack)
		if err != nil || parentLockedDigest != source.lockedDigest {
			return nil, fmt.Errorf("%w: avatar %d retained locked-layer digest is invalid",
				ErrAvatarConflict, source.version)
		}
		for _, child := range retainedChildren {
			childLockedDigest, digestErr := avatardomain.LockedLayersSHA256(
				[]byte(child.svg), pack)
			if digestErr != nil || childLockedDigest != child.lockedDigest ||
				child.lockedDigest != source.lockedDigest {
				return nil, fmt.Errorf("%w: avatar %d child %d violates retained locked-layer continuity",
					ErrAvatarConflict, source.version, child.version)
			}
			if err := avatardomain.ValidateLockedLayerContinuity(
				[]byte(source.svg), []byte(child.svg), pack); err != nil {
				return nil, fmt.Errorf("%w: avatar %d child %d violates locked-layer continuity: %v",
					ErrAvatarConflict, source.version, child.version, err)
			}
			if err := avatardomain.ValidatePerceptualContinuityFromFingerprint(
				fingerprint, []byte(child.svg), pack); err != nil {
				return nil, fmt.Errorf("%w: avatar %d child %d violates perceptual continuity: %v",
					ErrAvatarConflict, source.version, child.version, err)
			}
		}
		fingerprints[source.version] = fingerprint
	}
	return fingerprints, nil
}

// loadAvatarCompactionFingerprintSourcesTx loads SVGs only for selected full
// parents that retain a qualifying child outside the plan. The selected
// history itself may be much larger than the retained boundary set.
func loadAvatarCompactionFingerprintSourcesTx(ctx context.Context, tx pgx.Tx,
	target avatarTarget, plan avatarPayloadCompactionPlan) ([]avatarCompactionFingerprintSource, error) {
	rows, err := tx.Query(ctx, `
		SELECT parent.version, parent.style_pack_id, parent.style_pack_version,
		       parent.svg, parent.locked_layers_sha256
		  FROM agent_avatar_versions parent
		 WHERE parent.account_id=$1 AND parent.realm_id=$2 AND parent.agent_id=$3
		   AND parent.version=ANY($4::bigint[]) AND parent.payload_state='full'
		   AND parent.renderer_profile='perceptual-v1'
		   AND EXISTS (
		     SELECT 1 FROM agent_avatar_versions child
		      WHERE child.account_id=parent.account_id
		        AND child.realm_id=parent.realm_id
		        AND child.agent_id=parent.agent_id
		        AND child.parent_version=parent.version
		        AND child.lineage_generation=parent.lineage_generation
		        AND child.style_pack_id=parent.style_pack_id
		        AND child.style_pack_version=parent.style_pack_version
		        AND child.subject_form=parent.subject_form
		        AND child.payload_state='full'
		        AND child.renderer_profile='perceptual-v1'
		        AND NOT (child.version=ANY($4::bigint[]))
		        AND child.proposed_by_kind='agent'
		        AND child.proposed_by_id=parent.agent_id
		   )
		 ORDER BY parent.version`, target.accountID, target.realmID, target.agentID,
		plan.versions)
	if err != nil {
		return nil, fmt.Errorf("load avatar compaction fingerprint sources: %w", err)
	}
	sources := make([]avatarCompactionFingerprintSource, 0)
	for rows.Next() {
		var source avatarCompactionFingerprintSource
		if err := rows.Scan(&source.version, &source.stylePackID,
			&source.stylePackVersion, &source.svg, &source.lockedDigest); err != nil {
			rows.Close()
			return nil, err
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return sources, nil
}

const avatarContinuityValidationBatchSize = 64

// validateAvatarPrunedContinuityBoundariesTx fails closed before cleanup
// clears every qualifying full child of a compacted parent. The parent's exact
// stored fingerprint remains the authority for the final child comparison,
// and batches keep legacy over-quota histories from being materialized at once.
func validateAvatarPrunedContinuityBoundariesTx(ctx context.Context, tx pgx.Tx,
	target avatarTarget, plan avatarPayloadCompactionPlan) error {
	type styleKey struct {
		id      string
		version int
	}
	type boundary struct {
		parentVersion    int64
		stylePackID      string
		stylePackVersion int
		fingerprint      []byte
		parentLocked     string
		childVersion     int64
		childSVG         string
		childLocked      string
	}
	var afterParent, afterChild int64
	for {
		rows, err := tx.Query(ctx, `
			SELECT parent.version, parent.style_pack_id, parent.style_pack_version,
			       parent.continuity_fingerprint, parent.locked_layers_sha256,
			       child.version, child.svg, child.locked_layers_sha256
			  FROM agent_avatar_versions parent
			  JOIN agent_avatar_versions child
			    ON child.account_id=parent.account_id
			   AND child.realm_id=parent.realm_id
			   AND child.agent_id=parent.agent_id
			   AND child.parent_version=parent.version
			   AND child.lineage_generation=parent.lineage_generation
			   AND child.style_pack_id=parent.style_pack_id
			   AND child.style_pack_version=parent.style_pack_version
			   AND child.subject_form=parent.subject_form
			   AND child.payload_state='full'
			   AND child.proposed_by_kind='agent'
			   AND child.proposed_by_id=parent.agent_id
			 WHERE parent.account_id=$1 AND parent.realm_id=$2 AND parent.agent_id=$3
			   AND parent.payload_state='compacted'
			   AND parent.renderer_profile='perceptual-v1'
			   AND parent.continuity_fingerprint IS NOT NULL
			   AND child.renderer_profile='perceptual-v1'
			   AND child.version=ANY($4::bigint[])
			   AND (parent.version, child.version) > ($5::bigint, $6::bigint)
			   AND NOT EXISTS (
			     SELECT 1 FROM agent_avatar_versions sibling
			      WHERE sibling.account_id=parent.account_id
			        AND sibling.realm_id=parent.realm_id
			        AND sibling.agent_id=parent.agent_id
			        AND sibling.parent_version=parent.version
			        AND sibling.lineage_generation=parent.lineage_generation
			        AND sibling.style_pack_id=parent.style_pack_id
			        AND sibling.style_pack_version=parent.style_pack_version
			        AND sibling.subject_form=parent.subject_form
			        AND sibling.payload_state='full'
			        AND sibling.renderer_profile='perceptual-v1'
			        AND sibling.proposed_by_kind='agent'
			        AND sibling.proposed_by_id=parent.agent_id
			        AND NOT (sibling.version=ANY($4::bigint[]))
			   )
			 ORDER BY parent.version, child.version
			 LIMIT $7`, target.accountID, target.realmID, target.agentID,
			plan.versions, afterParent, afterChild, avatarContinuityValidationBatchSize)
		if err != nil {
			return fmt.Errorf("load pruned avatar continuity boundaries: %w", err)
		}
		batch := make([]boundary, 0, avatarContinuityValidationBatchSize)
		for rows.Next() {
			var item boundary
			if err := rows.Scan(&item.parentVersion, &item.stylePackID,
				&item.stylePackVersion, &item.fingerprint, &item.parentLocked,
				&item.childVersion, &item.childSVG, &item.childLocked); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}
		packs := make(map[styleKey]avatardomain.StylePack)
		for _, item := range batch {
			key := styleKey{id: item.stylePackID, version: item.stylePackVersion}
			pack, ok := packs[key]
			if !ok {
				pack, err = loadAvatarStylePackVersion(ctx, tx, target.accountID,
					target.realmID, item.stylePackID, item.stylePackVersion)
				if err != nil {
					return err
				}
				packs[key] = pack
			}
			if err := avatardomain.ValidatePerceptualContinuityFingerprintForStyle(
				item.fingerprint, pack); err != nil {
				return fmt.Errorf("%w: avatar %d retained continuity fingerprint is invalid: %v",
					ErrAvatarConflict, item.parentVersion, err)
			}
			childLocked, digestErr := avatardomain.LockedLayersSHA256(
				[]byte(item.childSVG), pack)
			if digestErr != nil || childLocked != item.childLocked ||
				item.childLocked != item.parentLocked {
				return fmt.Errorf("%w: avatar %d child %d violates retained locked-layer continuity",
					ErrAvatarConflict, item.parentVersion, item.childVersion)
			}
			if err := avatardomain.ValidatePerceptualContinuityFromFingerprint(
				item.fingerprint, []byte(item.childSVG), pack); err != nil {
				return fmt.Errorf("%w: avatar %d child %d violates perceptual continuity: %v",
					ErrAvatarConflict, item.parentVersion, item.childVersion, err)
			}
		}
		afterParent = batch[len(batch)-1].parentVersion
		afterChild = batch[len(batch)-1].childVersion
	}
}

func logAvatarPayloadCompactionTx(ctx context.Context, tx pgx.Tx,
	target avatarTarget, compaction avatarPayloadCompactionPlan,
	countLimit int, byteLimit int64) error {
	if compaction.count == 0 {
		return nil
	}
	versions := make([]string, len(compaction.versions))
	for i, version := range compaction.versions {
		versions[i] = strconv.FormatInt(version, 10)
	}
	return logEventTx(ctx, tx, EventInput{
		AccountID: target.accountID, ActorKind: ActorSystem,
		Verb: VerbAvatarPayloadCompacted,
		Metadata: map[string]any{
			"agent_id":               target.agentID,
			"compacted_versions":     strings.Join(versions, ","),
			"compacted_count":        strconv.Itoa(compaction.count),
			"net_reclaimed_bytes":    strconv.FormatInt(compaction.netReclaimedBytes, 10),
			"retained_payload_count": strconv.Itoa(compaction.retainedCount),
			"retained_payload_bytes": strconv.FormatInt(compaction.retainedBytes, 10),
			"count_limit":            strconv.Itoa(countLimit),
			"byte_limit":             strconv.FormatInt(byteLimit, 10),
			"rollback_floor":         strconv.Itoa(AvatarRollbackPayloadFloor),
		},
	})
}

func avatarCreativePayloadBytes(svg, description string, visualSpec []byte) (int64, error) {
	size := int64(len(svg) + len(description) + len(visualSpec))
	if size < 1 || size > maxAvatarPayloadBytes {
		return 0, fmt.Errorf("%w: avatar creative payload byte count is invalid", ErrAvatarInputInvalid)
	}
	return size, nil
}

func avatarRetainedPayloadUsageTx(ctx context.Context, tx pgx.Tx,
	target avatarTarget) (int, int64, error) {
	var count int
	var bytes int64
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE payload_state='full'),
		       COALESCE(SUM(CASE WHEN payload_state='full' THEN payload_bytes ELSE 0 END),0) +
		       COALESCE(SUM(octet_length(continuity_fingerprint)),0)
		  FROM agent_avatar_versions
		 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3`, target.accountID,
		target.realmID, target.agentID).Scan(&count, &bytes); err != nil {
		return 0, 0, fmt.Errorf("read avatar retained payload usage: %w", err)
	}
	return count, bytes, nil
}

// enforceAvatarPayloadQuotaTx is the rollout activation boundary. Disabled
// mode retains exact accounting and permits only mutations that already fit;
// it never clears creative payloads. Enabled mode first repairs any final
// legacy NULL digest under the profile fence and then applies normal cleanup.
func (s *Store) enforceAvatarPayloadQuotaTx(ctx context.Context, tx pgx.Tx,
	target avatarTarget, profile avatarLockedProfile, incomingCount int,
	incomingBytes int64, countLimit int,
	byteLimit int64) (avatarPayloadCompactionPlan, error) {
	if s.avatarPayloadCompactionEnabled {
		if _, err := backfillAvatarLockedLayerDigestsTx(ctx, tx,
			avatarLockedLayerDigestBackfillFilter{
				accountID: target.accountID,
				realmID:   target.realmID,
				agentID:   target.agentID,
			}); err != nil {
			return avatarPayloadCompactionPlan{}, fmt.Errorf(
				"repair avatar digests before payload compaction: %w", err)
		}
		return compactAvatarPayloadsTx(ctx, tx, target, profile, incomingCount,
			incomingBytes, countLimit, byteLimit)
	}
	retainedCount, retainedBytes, err := avatarRetainedPayloadUsageTx(ctx, tx, target)
	if err != nil {
		return avatarPayloadCompactionPlan{}, err
	}
	retainedCount += incomingCount
	retainedBytes += incomingBytes
	if retainedCount > countLimit || retainedBytes > byteLimit {
		return avatarPayloadCompactionPlan{}, fmt.Errorf(
			"%w: enable avatar payload compaction and retry",
			ErrAvatarPayloadCompactionDisabled)
	}
	return avatarPayloadCompactionPlan{
		retainedCount: retainedCount,
		retainedBytes: retainedBytes,
	}, nil
}

// SetAvatarQuota updates one operator-selected agent's retained-payload limits.
func (s *Store) SetAvatarQuota(ctx context.Context, p Principal, agentID string,
	in UpdateAvatarQuotaInput) (AvatarMutationResult, error) {
	target, err := resolveOperatorAvatarTarget(ctx, s.pool, p, agentID)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if in.ExpectedProfileRevision < 1 ||
		in.RetainedPayloadCountLimit < AvatarMinRetainedPayloadCountLimit ||
		in.RetainedPayloadCountLimit > AvatarMaxRetainedPayloadCountLimit ||
		in.RetainedPayloadByteLimit < AvatarMinRetainedPayloadByteLimit ||
		in.RetainedPayloadByteLimit > AvatarMaxRetainedPayloadByteLimit {
		return AvatarMutationResult{}, fmt.Errorf("%w: avatar payload quota is outside supported bounds", ErrAvatarInputInvalid)
	}
	key, err := normalizeAvatarIdempotencyKey(in.IdempotencyKey)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	in.IdempotencyKey = ""
	fingerprint, err := avatarFingerprint(in)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, target.accountID, false); err != nil {
		return AvatarMutationResult{}, err
	}
	if err := lockAvatarIdempotencyKey(ctx, tx, target.accountID,
		target.realmID, "avatar", target.agentID, p.Kind, p.ID, "set_quota", key); err != nil {
		return AvatarMutationResult{}, err
	}
	profile, err := lockAvatarProfileTx(ctx, tx, target)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if receipt, replayed, err := replayAvatarMutationTx(ctx, tx, p, target,
		"avatar", target.agentID, "set_quota", key, fingerprint); err != nil {
		return AvatarMutationResult{}, err
	} else if replayed {
		view, err := getAvatarView(ctx, tx, target)
		return AvatarMutationResult{Avatar: view, Receipt: receipt}, err
	}
	if profile.revision != in.ExpectedProfileRevision {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	compaction, err := s.enforceAvatarPayloadQuotaTx(ctx, tx, target,
		profile, 0, 0, in.RetainedPayloadCountLimit,
		in.RetainedPayloadByteLimit)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	var resultRevision int64
	err = tx.QueryRow(ctx, `
		UPDATE agent_avatar_profiles
		   SET retained_payload_count_limit=$4,
		       retained_payload_byte_limit=$5,
		       payload_quota_reconciliation_required=
		         CASE WHEN $7 THEN false ELSE payload_quota_reconciliation_required END,
		       revision=revision+1, updated_at=clock_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3 AND revision=$6
		 RETURNING revision`, target.accountID, target.realmID, target.agentID,
		in.RetainedPayloadCountLimit, in.RetainedPayloadByteLimit,
		in.ExpectedProfileRevision, s.avatarPayloadCompactionEnabled).Scan(&resultRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	if err != nil {
		return AvatarMutationResult{}, fmt.Errorf("set avatar payload quota: %w", err)
	}
	receipt, err := insertAvatarMutationReceiptTx(ctx, tx, p, target, "avatar",
		target.agentID, "set_quota", key, fingerprint, resultRevision, 0)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if err := logAvatarPayloadCompactionTx(ctx, tx, target, compaction,
		in.RetainedPayloadCountLimit, in.RetainedPayloadByteLimit); err != nil {
		return AvatarMutationResult{}, err
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: target.accountID, ActorKind: ActorOperator, ActorID: p.ID,
		Verb: VerbAvatarQuotaChanged,
		Metadata: map[string]any{
			"agent_id":         target.agentID,
			"count_limit_from": strconv.Itoa(profile.retainedPayloadCountLimit),
			"count_limit_to":   strconv.Itoa(in.RetainedPayloadCountLimit),
			"byte_limit_from":  strconv.FormatInt(profile.retainedPayloadByteLimit, 10),
			"byte_limit_to":    strconv.FormatInt(in.RetainedPayloadByteLimit, 10),
			"rollback_floor":   strconv.Itoa(AvatarRollbackPayloadFloor),
		},
	}); err != nil {
		return AvatarMutationResult{}, err
	}
	view, err := getAvatarView(ctx, tx, target)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AvatarMutationResult{}, err
	}
	return AvatarMutationResult{Avatar: view, Receipt: receipt}, nil
}
