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
	version         int64
	lineage         int64
	bytes           int64
	wasActivated    bool
	rejected        bool
	lastActivatedAt *time.Time
}

type avatarPayloadCompactionPlan struct {
	versions      []int64
	count         int
	bytes         int64
	retainedCount int
	retainedBytes int64
}

type avatarCompactionFingerprintSource struct {
	version          int64
	stylePackID      string
	stylePackVersion int
	svg              string
	needed           bool
}

// planAvatarPayloadCompaction is deterministic and deliberately separates
// eligibility from SQL. Protected pointers and the rollback floor are never
// returned, even when the configured quota cannot accommodate them.
func planAvatarPayloadCompaction(candidates []avatarPayloadCandidate, currentLineage,
	activeVersion, proposedVersion int64, incomingCount int, incomingBytes int64, countLimit int,
	byteLimit int64) (avatarPayloadCompactionPlan, error) {
	plan := avatarPayloadCompactionPlan{
		retainedCount: len(candidates) + incomingCount,
		retainedBytes: incomingBytes,
	}
	for _, candidate := range candidates {
		plan.retainedBytes += candidate.bytes
	}
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
	for _, candidate := range eligible {
		if plan.retainedCount <= countLimit && plan.retainedBytes <= byteLimit {
			break
		}
		plan.versions = append(plan.versions, candidate.version)
		plan.count++
		plan.bytes += candidate.bytes
		plan.retainedCount--
		plan.retainedBytes -= candidate.bytes
	}
	if plan.retainedCount > countLimit || plan.retainedBytes > byteLimit {
		return avatarPayloadCompactionPlan{}, ErrAvatarPayloadQuotaExceeded
	}
	return plan, nil
}

func compactAvatarPayloadsTx(ctx context.Context, tx pgx.Tx,
	target avatarTarget, profile avatarLockedProfile, incomingCount int,
	incomingBytes int64, countLimit int, byteLimit int64) (avatarPayloadCompactionPlan, error) {
	rows, err := tx.Query(ctx, `
		SELECT v.version, v.lineage_generation, v.payload_bytes,
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
		   AND v.payload_state='full'`, target.accountID, target.realmID,
		target.agentID)
	if err != nil {
		return avatarPayloadCompactionPlan{}, fmt.Errorf("list avatar payload quota state: %w", err)
	}
	candidates := make([]avatarPayloadCandidate, 0)
	for rows.Next() {
		var candidate avatarPayloadCandidate
		if err := rows.Scan(&candidate.version, &candidate.lineage,
			&candidate.bytes, &candidate.wasActivated, &candidate.rejected,
			&candidate.lastActivatedAt); err != nil {
			return avatarPayloadCompactionPlan{}, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return avatarPayloadCompactionPlan{}, err
	}
	rows.Close()
	var activeVersion, proposedVersion int64
	if profile.activeVersion != nil {
		activeVersion = *profile.activeVersion
	}
	if profile.proposedVersion != nil {
		proposedVersion = *profile.proposedVersion
	}
	plan, err := planAvatarPayloadCompaction(candidates,
		profile.lineageGeneration, activeVersion, proposedVersion, incomingCount,
		incomingBytes, countLimit, byteLimit)
	if err != nil || plan.count == 0 {
		return plan, err
	}
	fingerprints, err := buildAvatarCompactionFingerprintsTx(ctx, tx, target, plan)
	if err != nil {
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
		        AND child.proposed_by_kind='agent'
		        AND child.proposed_by_id=child.agent_id
		        AND child.style_pack_id=parent.style_pack_id
		        AND child.style_pack_version=parent.style_pack_version
		   )`, target.accountID, target.realmID, target.agentID); err != nil {
		return avatarPayloadCompactionPlan{}, fmt.Errorf("prune avatar continuity fingerprints: %w", err)
	}
	return plan, nil
}

// buildAvatarCompactionFingerprintsTx snapshots the exact bounded perceptual
// boundary before planned parent SVGs are cleared. A compacted parent retains
// the projection only while a full direct child outside this plan still needs
// it for same-style, owner-authored continuity validation.
func buildAvatarCompactionFingerprintsTx(ctx context.Context, tx pgx.Tx,
	target avatarTarget, plan avatarPayloadCompactionPlan) (map[int64][]byte, error) {
	rows, err := tx.Query(ctx, `
		SELECT parent.version, parent.style_pack_id, parent.style_pack_version,
		       parent.svg,
		       EXISTS (
		         SELECT 1 FROM agent_avatar_versions child
		          WHERE child.account_id=parent.account_id
		            AND child.realm_id=parent.realm_id
		            AND child.agent_id=parent.agent_id
		            AND child.parent_version=parent.version
		            AND child.lineage_generation=parent.lineage_generation
		            AND child.style_pack_id=parent.style_pack_id
		            AND child.style_pack_version=parent.style_pack_version
		            AND child.payload_state='full'
		            AND NOT (child.version=ANY($4::bigint[]))
		            AND child.proposed_by_kind='agent'
		            AND child.proposed_by_id=parent.agent_id
		       )
		  FROM agent_avatar_versions parent
		 WHERE parent.account_id=$1 AND parent.realm_id=$2 AND parent.agent_id=$3
		   AND parent.version=ANY($4::bigint[]) AND parent.payload_state='full'
		 ORDER BY parent.version`, target.accountID, target.realmID, target.agentID,
		plan.versions)
	if err != nil {
		return nil, fmt.Errorf("load avatar compaction fingerprint sources: %w", err)
	}
	sources := make([]avatarCompactionFingerprintSource, 0, plan.count)
	for rows.Next() {
		var source avatarCompactionFingerprintSource
		if err := rows.Scan(&source.version, &source.stylePackID,
			&source.stylePackVersion, &source.svg, &source.needed); err != nil {
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
	if len(sources) != plan.count {
		return nil, ErrAvatarConflict
	}
	type styleKey struct {
		id      string
		version int
	}
	packs := make(map[styleKey]avatardomain.StylePack)
	fingerprints := make(map[int64][]byte)
	for _, source := range sources {
		if !source.needed {
			continue
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
		fingerprints[source.version] = fingerprint
	}
	return fingerprints, nil
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
			"compacted_bytes":        strconv.FormatInt(compaction.bytes, 10),
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
	compaction, err := compactAvatarPayloadsTx(ctx, tx, target,
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
		       revision=revision+1, updated_at=clock_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3 AND revision=$6
		 RETURNING revision`, target.accountID, target.realmID, target.agentID,
		in.RetainedPayloadCountLimit, in.RetainedPayloadByteLimit,
		in.ExpectedProfileRevision).Scan(&resultRevision)
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
