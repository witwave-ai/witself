package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
	"github.com/witwave-ai/witself/internal/id"
)

type avatarLockedProfile struct {
	status            avatardomain.Status
	policy            avatardomain.AutonomyPolicy
	stylePackID       string
	stylePackVersion  int
	lineageGeneration int64
	retryReady        bool
	revision          int64
	latestVersion     *int64
	proposedVersion   *int64
	activeVersion     *int64
}

// ProposeAvatar stores an immutable avatar proposal for the authenticated agent.
func (s *Store) ProposeAvatar(ctx context.Context, p Principal, in ProposeAvatarInput) (AvatarMutationResult, error) {
	target, err := requireSelfAvatarPrincipal(p)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	return s.proposeAvatar(ctx, p, target, in, false)
}

// ProposeAgentAvatar stores an immutable avatar proposal for an operator-selected agent.
func (s *Store) ProposeAgentAvatar(ctx context.Context, p Principal, agentID string, in ProposeAvatarInput) (AvatarMutationResult, error) {
	target, err := resolveOperatorAvatarTarget(ctx, s.pool, p, agentID)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	return s.proposeAvatar(ctx, p, target, in, true)
}

func (s *Store) proposeAvatar(ctx context.Context, p Principal, target avatarTarget, in ProposeAvatarInput, operator bool) (AvatarMutationResult, error) {
	if in.ExpectedProfileRevision < 1 || in.ParentVersion < 0 || in.StylePackVersion < 1 {
		return AvatarMutationResult{}, fmt.Errorf("%w: expected revision and style version must be positive", ErrAvatarInputInvalid)
	}
	stylePackID := strings.TrimSpace(in.StylePackID)
	if err := (avatardomain.StylePackRef{RealmID: target.realmID, StylePackID: stylePackID, Version: in.StylePackVersion}).Validate(); err != nil {
		return AvatarMutationResult{}, fmt.Errorf("%w: %v", ErrAvatarInputInvalid, err)
	}
	if err := in.SubjectForm.Validate(); err != nil {
		return AvatarMutationResult{}, fmt.Errorf("%w: %v", ErrAvatarInputInvalid, err)
	}
	description, err := avatardomain.NormalizeDescription(in.Description)
	if err != nil {
		return AvatarMutationResult{}, fmt.Errorf("%w: %v", ErrAvatarInputInvalid, err)
	}
	visualSpec, err := avatardomain.NormalizeSpecJSON(in.VisualSpec)
	if err != nil {
		return AvatarMutationResult{}, fmt.Errorf("%w: %v", ErrAvatarInputInvalid, err)
	}
	provenance, err := normalizeAvatarClient(in.Provenance)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	key, err := normalizeAvatarIdempotencyKey(in.IdempotencyKey)
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
		target.realmID, "avatar", target.agentID, p.Kind, p.ID, "propose", key); err != nil {
		return AvatarMutationResult{}, err
	}
	profile, err := lockAvatarProfileTx(ctx, tx, target)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	pack, err := loadAvatarStylePackVersion(ctx, tx, target.accountID,
		target.realmID, stylePackID, in.StylePackVersion)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if !pack.HasSubjectForm(in.SubjectForm) {
		return AvatarMutationResult{}, fmt.Errorf("%w: selected style does not support subject form", ErrAvatarInputInvalid)
	}
	sanitizedSVG, err := avatardomain.SanitizeSVGForStylePack([]byte(in.SVG), pack)
	if err != nil {
		return AvatarMutationResult{}, fmt.Errorf("%w: %v", ErrAvatarInputInvalid, err)
	}
	svgDigest := sha256.Sum256(sanitizedSVG)
	svgSHA256 := hex.EncodeToString(svgDigest[:])
	fingerprint, err := avatarFingerprint(struct {
		ExpectedRevision int64                    `json:"expected_revision"`
		ParentVersion    int64                    `json:"parent_version"`
		StylePackID      string                   `json:"style_pack_id"`
		StyleVersion     int                      `json:"style_version"`
		SubjectForm      avatardomain.SubjectForm `json:"subject_form"`
		Description      string                   `json:"description"`
		VisualSpec       json.RawMessage          `json:"visual_spec"`
		SVGSHA256        string                   `json:"svg_sha256"`
		Provenance       AvatarClientProvenance   `json:"provenance"`
	}{in.ExpectedProfileRevision, in.ParentVersion, stylePackID,
		in.StylePackVersion, in.SubjectForm, description, visualSpec, svgSHA256,
		provenance})
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if receipt, replayed, err := replayAvatarMutationTx(ctx, tx, p, target,
		"avatar", target.agentID, "propose", key, fingerprint); err != nil {
		return AvatarMutationResult{}, err
	} else if replayed {
		view, err := getAvatarView(ctx, tx, target)
		if err != nil {
			return AvatarMutationResult{}, err
		}
		return AvatarMutationResult{Avatar: view, Receipt: receipt}, nil
	}
	if !operator && profile.policy == avatardomain.AutonomyOperatorOnly {
		return AvatarMutationResult{}, ErrAvatarForbidden
	}
	if !operator && profile.status == avatardomain.StatusGenerationFailed && !profile.retryReady {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	if profile.status == avatardomain.StatusArchived {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	if profile.proposedVersion != nil {
		return AvatarMutationResult{}, fmt.Errorf("%w: pending avatar proposal must be activated or rejected first", ErrAvatarConflict)
	}
	if profile.stylePackID != stylePackID || profile.stylePackVersion != in.StylePackVersion {
		return AvatarMutationResult{}, fmt.Errorf("%w: proposal must use the profile's selected style", ErrAvatarConflict)
	}
	if profile.revision != in.ExpectedProfileRevision {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	if profile.activeVersion == nil {
		if in.ParentVersion != 0 {
			return AvatarMutationResult{}, fmt.Errorf("%w: initial avatar cannot name a parent", ErrAvatarConflict)
		}
	} else if in.ParentVersion != *profile.activeVersion {
		return AvatarMutationResult{}, fmt.Errorf("%w: evolution parent must be the current active version", ErrAvatarConflict)
	}
	if profile.activeVersion != nil {
		parentInfo, err := getAvatarVersionForMutationTx(ctx, tx, target, *profile.activeVersion)
		if err != nil {
			return AvatarMutationResult{}, err
		}
		if parentInfo.lineageGeneration != profile.lineageGeneration {
			return AvatarMutationResult{}, fmt.Errorf("%w: active avatar is outside the current lineage", ErrAvatarConflict)
		}
		if !operator {
			sameStyle := parentInfo.stylePackID == stylePackID &&
				parentInfo.stylePackVersion == in.StylePackVersion
			if sameStyle {
				if parentInfo.subjectForm != in.SubjectForm {
					return AvatarMutationResult{}, fmt.Errorf("%w: same-style self evolution must preserve subject_form", ErrAvatarInputInvalid)
				}
				if err := avatardomain.ValidateLockedLayerContinuity(
					[]byte(parentInfo.svg), sanitizedSVG, pack); err != nil {
					return AvatarMutationResult{}, fmt.Errorf("%w: %v", ErrAvatarInputInvalid, err)
				}
				if err := avatardomain.ValidatePerceptualContinuity(
					[]byte(parentInfo.svg), sanitizedSVG, pack); err != nil {
					return AvatarMutationResult{}, fmt.Errorf("%w: %v", ErrAvatarInputInvalid, err)
				}
			}
		}
	}

	version := int64(1)
	if profile.latestVersion != nil {
		version = *profile.latestVersion + 1
	}
	versionID, err := id.New("avver")
	if err != nil {
		return AvatarMutationResult{}, err
	}
	var parent any
	if in.ParentVersion > 0 {
		parent = in.ParentVersion
	}
	provenanceJSON, err := json.Marshal(provenance)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_avatar_versions
		       (account_id, realm_id, agent_id, id, version, parent_version,
		        lineage_generation, style_pack_id, style_pack_version, subject_form, svg,
		        description, visual_spec, svg_sha256, provenance,
		        proposed_by_kind, proposed_by_id, proposed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,
		        clock_timestamp())`,
		target.accountID, target.realmID, target.agentID, versionID, version,
		parent, profile.lineageGeneration, stylePackID, in.StylePackVersion,
		string(in.SubjectForm),
		string(sanitizedSVG), description, visualSpec, svgSHA256, provenanceJSON,
		p.Kind, p.ID); err != nil {
		return AvatarMutationResult{}, fmt.Errorf("insert avatar version: %w", err)
	}
	var resultRevision int64
	err = tx.QueryRow(ctx, `
		UPDATE agent_avatar_profiles
		   SET latest_avatar_version=$4, proposed_avatar_version=$4,
		       status='proposed', subject_form=$5, attempt_count=0,
		       retry_after=NULL, failure_code='', revision=revision+1,
		       updated_at=clock_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3 AND revision=$6
		 RETURNING revision`, target.accountID, target.realmID, target.agentID,
		version, string(in.SubjectForm), in.ExpectedProfileRevision).Scan(&resultRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	if err != nil {
		return AvatarMutationResult{}, fmt.Errorf("advance avatar proposal: %w", err)
	}
	receipt, err := insertAvatarMutationReceiptTx(ctx, tx, p, target, "avatar",
		target.agentID, "propose", key, fingerprint, resultRevision, version)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	metadata := avatarVersionEventMetadata(target.agentID, version,
		in.ParentVersion, 0, stylePackID, in.StylePackVersion,
		string(avatardomain.StatusProposed))
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: target.accountID, ActorKind: avatarAuditActor(p), ActorID: p.ID,
		Verb: VerbAvatarProposed, Metadata: metadata,
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

// ActivateAvatar activates the authenticated agent's pending avatar proposal.
func (s *Store) ActivateAvatar(ctx context.Context, p Principal, in ActivateAvatarInput) (AvatarMutationResult, error) {
	target, err := requireSelfAvatarPrincipal(p)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	return s.activateAvatar(ctx, p, target, in, false)
}

// ActivateAgentAvatar activates a pending avatar proposal for an operator-selected agent.
func (s *Store) ActivateAgentAvatar(ctx context.Context, p Principal, agentID string, in ActivateAvatarInput) (AvatarMutationResult, error) {
	target, err := resolveOperatorAvatarTarget(ctx, s.pool, p, agentID)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	return s.activateAvatar(ctx, p, target, in, true)
}

func (s *Store) activateAvatar(ctx context.Context, p Principal, target avatarTarget, in ActivateAvatarInput, operator bool) (AvatarMutationResult, error) {
	if in.Version < 1 || in.ExpectedProfileRevision < 1 {
		return AvatarMutationResult{}, fmt.Errorf("%w: version and expected revision are required", ErrAvatarInputInvalid)
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
		target.realmID, "avatar", target.agentID, p.Kind, p.ID, "activate", key); err != nil {
		return AvatarMutationResult{}, err
	}
	profile, err := lockAvatarProfileTx(ctx, tx, target)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if receipt, replayed, err := replayAvatarMutationTx(ctx, tx, p, target,
		"avatar", target.agentID, "activate", key, fingerprint); err != nil {
		return AvatarMutationResult{}, err
	} else if replayed {
		view, err := getAvatarView(ctx, tx, target)
		return AvatarMutationResult{Avatar: view, Receipt: receipt}, err
	}
	if !operator && profile.policy != avatardomain.AutonomyAgentSelfManaged {
		return AvatarMutationResult{}, ErrAvatarForbidden
	}
	if profile.revision != in.ExpectedProfileRevision ||
		profile.proposedVersion == nil || *profile.proposedVersion != in.Version {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	versionInfo, err := getAvatarVersionForMutationTx(ctx, tx, target, in.Version)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if versionInfo.lineageGeneration != profile.lineageGeneration {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	activationID, err := id.New("avact")
	if err != nil {
		return AvatarMutationResult{}, err
	}
	var prior any
	var priorNumber int64
	if profile.activeVersion != nil {
		prior, priorNumber = *profile.activeVersion, *profile.activeVersion
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_avatar_activations
		       (id, account_id, realm_id, agent_id, sequence,
		        lineage_generation, avatar_version,
		        prior_active_version, action, activated_by_kind, activated_by_id,
		        activated_at)
		VALUES ($1,$2,$3,$4,
		        (SELECT COALESCE(MAX(sequence),0)+1
		           FROM agent_avatar_activations
		          WHERE account_id=$2 AND realm_id=$3 AND agent_id=$4),
		        $5,$6,$7,'activated',$8,$9,clock_timestamp())`, activationID,
		target.accountID, target.realmID, target.agentID,
		profile.lineageGeneration, in.Version, prior,
		p.Kind, p.ID); err != nil {
		return AvatarMutationResult{}, fmt.Errorf("record avatar activation: %w", err)
	}
	resultRevision, err := updateActiveAvatarProfileTx(ctx, tx, target,
		in.ExpectedProfileRevision, in.Version, versionInfo)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	receipt, err := insertAvatarMutationReceiptTx(ctx, tx, p, target, "avatar",
		target.agentID, "activate", key, fingerprint, resultRevision, in.Version)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	metadata := avatarVersionEventMetadata(target.agentID, in.Version,
		versionInfo.parentVersion, priorNumber, versionInfo.stylePackID,
		versionInfo.stylePackVersion, string(avatardomain.StatusActive))
	if err := logEventTx(ctx, tx, EventInput{AccountID: target.accountID,
		ActorKind: avatarAuditActor(p), ActorID: p.ID,
		Verb: VerbAvatarActivated, Metadata: metadata}); err != nil {
		return AvatarMutationResult{}, err
	}
	if versionInfo.parentVersion > 0 {
		if err := logEventTx(ctx, tx, EventInput{AccountID: target.accountID,
			ActorKind: avatarAuditActor(p), ActorID: p.ID,
			Verb: VerbAvatarEvolved, Metadata: metadata}); err != nil {
			return AvatarMutationResult{}, err
		}
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

// RollbackAvatar reactivates an earlier avatar version for the authenticated agent.
func (s *Store) RollbackAvatar(ctx context.Context, p Principal, in RollbackAvatarInput) (AvatarMutationResult, error) {
	target, err := requireSelfAvatarPrincipal(p)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	return s.rollbackAvatar(ctx, p, target, in, false)
}

// RollbackAgentAvatar reactivates an earlier avatar version for an operator-selected agent.
func (s *Store) RollbackAgentAvatar(ctx context.Context, p Principal, agentID string, in RollbackAvatarInput) (AvatarMutationResult, error) {
	target, err := resolveOperatorAvatarTarget(ctx, s.pool, p, agentID)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	return s.rollbackAvatar(ctx, p, target, in, true)
}

func (s *Store) rollbackAvatar(ctx context.Context, p Principal, target avatarTarget, in RollbackAvatarInput, operator bool) (AvatarMutationResult, error) {
	if in.Version < 1 || in.ExpectedProfileRevision < 1 {
		return AvatarMutationResult{}, fmt.Errorf("%w: version and expected revision are required", ErrAvatarInputInvalid)
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
		target.realmID, "avatar", target.agentID, p.Kind, p.ID, "rollback", key); err != nil {
		return AvatarMutationResult{}, err
	}
	profile, err := lockAvatarProfileTx(ctx, tx, target)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if receipt, replayed, err := replayAvatarMutationTx(ctx, tx, p, target,
		"avatar", target.agentID, "rollback", key, fingerprint); err != nil {
		return AvatarMutationResult{}, err
	} else if replayed {
		view, err := getAvatarView(ctx, tx, target)
		return AvatarMutationResult{Avatar: view, Receipt: receipt}, err
	}
	if profile.proposedVersion != nil {
		return AvatarMutationResult{}, fmt.Errorf("%w: pending avatar proposal must be activated or rejected before rollback", ErrAvatarConflict)
	}
	if !operator && profile.policy != avatardomain.AutonomyAgentSelfManaged {
		return AvatarMutationResult{}, ErrAvatarForbidden
	}
	if profile.revision != in.ExpectedProfileRevision || profile.activeVersion == nil ||
		in.Version == *profile.activeVersion {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	versionInfo, err := getAvatarVersionForMutationTx(ctx, tx, target, in.Version)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if versionInfo.lineageGeneration != profile.lineageGeneration {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	var previouslyActive bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM agent_avatar_activations
		   WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3
		     AND avatar_version=$4 AND lineage_generation=$5
		)`, target.accountID, target.realmID, target.agentID,
		in.Version, profile.lineageGeneration).Scan(&previouslyActive); err != nil {
		return AvatarMutationResult{}, fmt.Errorf("verify avatar rollback target: %w", err)
	}
	if !previouslyActive {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	activationID, err := id.New("avact")
	if err != nil {
		return AvatarMutationResult{}, err
	}
	prior := *profile.activeVersion
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_avatar_activations
		       (id, account_id, realm_id, agent_id, sequence,
		        lineage_generation, avatar_version,
		        prior_active_version, action, activated_by_kind, activated_by_id,
		        activated_at)
		VALUES ($1,$2,$3,$4,
		        (SELECT COALESCE(MAX(sequence),0)+1
		           FROM agent_avatar_activations
		          WHERE account_id=$2 AND realm_id=$3 AND agent_id=$4),
		        $5,$6,$7,'rolled_back',$8,$9,clock_timestamp())`, activationID,
		target.accountID, target.realmID, target.agentID,
		profile.lineageGeneration, in.Version, prior,
		p.Kind, p.ID); err != nil {
		return AvatarMutationResult{}, fmt.Errorf("record avatar rollback: %w", err)
	}
	resultRevision, err := updateActiveAvatarProfileTx(ctx, tx, target,
		in.ExpectedProfileRevision, in.Version, versionInfo)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	receipt, err := insertAvatarMutationReceiptTx(ctx, tx, p, target, "avatar",
		target.agentID, "rollback", key, fingerprint, resultRevision, in.Version)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	resultStatus := avatardomain.StatusActive
	if versionInfo.stylePackID != profile.stylePackID ||
		versionInfo.stylePackVersion != profile.stylePackVersion {
		resultStatus = avatardomain.StatusEvolutionDue
	}
	metadata := avatarVersionEventMetadata(target.agentID, in.Version,
		versionInfo.parentVersion, prior, versionInfo.stylePackID,
		versionInfo.stylePackVersion, string(resultStatus))
	if err := logEventTx(ctx, tx, EventInput{AccountID: target.accountID,
		ActorKind: avatarAuditActor(p), ActorID: p.ID,
		Verb: VerbAvatarRolledBack, Metadata: metadata}); err != nil {
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

// ResetAvatar retires the authenticated agent's current avatar lineage without
// deleting any immutable version or lifecycle history.
func (s *Store) ResetAvatar(ctx context.Context, p Principal, in ResetAvatarInput) (AvatarMutationResult, error) {
	target, err := requireSelfAvatarPrincipal(p)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	return s.resetAvatar(ctx, p, target, in, false)
}

// ResetAgentAvatar is the operator-target counterpart. Operator authority is
// independent of the target's autonomy policy.
func (s *Store) ResetAgentAvatar(ctx context.Context, p Principal, agentID string, in ResetAvatarInput) (AvatarMutationResult, error) {
	target, err := resolveOperatorAvatarTarget(ctx, s.pool, p, agentID)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	return s.resetAvatar(ctx, p, target, in, true)
}

func (s *Store) resetAvatar(ctx context.Context, p Principal, target avatarTarget, in ResetAvatarInput, operator bool) (AvatarMutationResult, error) {
	if in.ExpectedProfileRevision < 1 {
		return AvatarMutationResult{}, fmt.Errorf("%w: expected revision is required", ErrAvatarInputInvalid)
	}
	reasonCode, err := normalizeAvatarReasonCode(in.ReasonCode, false)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	key, err := normalizeAvatarIdempotencyKey(in.IdempotencyKey)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	in.ReasonCode, in.IdempotencyKey = reasonCode, ""
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
		target.realmID, "avatar", target.agentID, p.Kind, p.ID, "reset", key); err != nil {
		return AvatarMutationResult{}, err
	}
	profile, err := lockAvatarProfileTx(ctx, tx, target)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if receipt, replayed, err := replayAvatarMutationTx(ctx, tx, p, target,
		"avatar", target.agentID, "reset", key, fingerprint); err != nil {
		return AvatarMutationResult{}, err
	} else if replayed {
		view, err := getAvatarView(ctx, tx, target)
		return AvatarMutationResult{Avatar: view, Receipt: receipt}, err
	}
	if !operator && profile.policy != avatardomain.AutonomyAgentSelfManaged {
		return AvatarMutationResult{}, ErrAvatarForbidden
	}
	if profile.revision != in.ExpectedProfileRevision {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	if profile.status == avatardomain.StatusArchived {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	// A reset retires durable avatar state; it is not a retry/backoff escape or
	// a way to churn generations on an already-empty lineage. Pending-only state
	// is sufficient and is intentionally retired by this operation.
	if profile.activeVersion == nil && profile.proposedVersion == nil {
		return AvatarMutationResult{}, fmt.Errorf("%w: avatar reset requires an active or proposed version", ErrAvatarConflict)
	}
	if profile.lineageGeneration >= 2147483647 {
		return AvatarMutationResult{}, fmt.Errorf("%w: avatar lineage generation exhausted", ErrAvatarConflict)
	}

	resetID, err := id.New("avrst")
	if err != nil {
		return AvatarMutationResult{}, err
	}
	newLineage := profile.lineageGeneration + 1
	var retiredActive, retiredProposed any
	if profile.activeVersion != nil {
		retiredActive = *profile.activeVersion
	}
	if profile.proposedVersion != nil {
		retiredProposed = *profile.proposedVersion
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO agent_avatar_resets
		       (id, account_id, realm_id, agent_id, sequence,
		        retired_lineage_generation, new_lineage_generation,
		        retired_active_version, retired_proposed_version,
		        reset_by_kind, reset_by_id, reason_code, reset_at)
		VALUES ($1,$2,$3,$4,
		        (SELECT COALESCE(MAX(sequence),0)+1
		           FROM agent_avatar_resets
		          WHERE account_id=$2 AND realm_id=$3 AND agent_id=$4),
		        $5,$6,$7,$8,$9,$10,$11,clock_timestamp())`, resetID, target.accountID, target.realmID,
		target.agentID, profile.lineageGeneration, newLineage, retiredActive,
		retiredProposed, p.Kind, p.ID, reasonCode)
	if err != nil {
		return AvatarMutationResult{}, fmt.Errorf("record avatar reset: %w", err)
	}

	var resultRevision int64
	err = tx.QueryRow(ctx, `
		UPDATE agent_avatar_profiles
		   SET lineage_generation=$4, active_avatar_version=NULL,
		       proposed_avatar_version=NULL, status='generation_due',
		       subject_form='human', attempt_count=0, retry_after=NULL,
		       failure_code='', revision=revision+1,
		       updated_at=clock_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3
		   AND revision=$5 AND lineage_generation=$6
		 RETURNING revision`, target.accountID, target.realmID, target.agentID,
		newLineage, in.ExpectedProfileRevision,
		profile.lineageGeneration).Scan(&resultRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	if err != nil {
		return AvatarMutationResult{}, fmt.Errorf("reset avatar profile: %w", err)
	}
	receipt, err := insertAvatarMutationReceiptWithLineageTx(ctx, tx, p, target,
		"avatar", target.agentID, "reset", key, fingerprint, resultRevision, 0,
		newLineage)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	metadata := map[string]any{
		"agent_id": target.agentID, "status": string(avatardomain.StatusGenerationDue),
		"retired_lineage_generation": strconv.FormatInt(profile.lineageGeneration, 10),
		"new_lineage_generation":     strconv.FormatInt(newLineage, 10),
	}
	if profile.activeVersion != nil {
		metadata["retired_active_version"] = strconv.FormatInt(*profile.activeVersion, 10)
	}
	if profile.proposedVersion != nil {
		metadata["retired_proposed_version"] = strconv.FormatInt(*profile.proposedVersion, 10)
	}
	if reasonCode != "" {
		metadata["reason_code"] = reasonCode
	}
	if err := logEventTx(ctx, tx, EventInput{AccountID: target.accountID,
		ActorKind: avatarAuditActor(p), ActorID: p.ID,
		Verb: VerbAvatarReset, Metadata: metadata}); err != nil {
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

// RejectAgentAvatar rejects the pending avatar proposal for an operator-selected agent.
func (s *Store) RejectAgentAvatar(ctx context.Context, p Principal, agentID string, in RejectAvatarInput) (AvatarMutationResult, error) {
	target, err := resolveOperatorAvatarTarget(ctx, s.pool, p, agentID)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if in.Version < 1 || in.ExpectedProfileRevision < 1 {
		return AvatarMutationResult{}, fmt.Errorf("%w: version and expected revision are required", ErrAvatarInputInvalid)
	}
	reasonCode, err := normalizeAvatarReasonCode(in.ReasonCode, false)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	key, err := normalizeAvatarIdempotencyKey(in.IdempotencyKey)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	in.ReasonCode, in.IdempotencyKey = reasonCode, ""
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
		target.realmID, "avatar", target.agentID, p.Kind, p.ID, "reject", key); err != nil {
		return AvatarMutationResult{}, err
	}
	profile, err := lockAvatarProfileTx(ctx, tx, target)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if receipt, replayed, err := replayAvatarMutationTx(ctx, tx, p, target,
		"avatar", target.agentID, "reject", key, fingerprint); err != nil {
		return AvatarMutationResult{}, err
	} else if replayed {
		view, err := getAvatarView(ctx, tx, target)
		return AvatarMutationResult{Avatar: view, Receipt: receipt}, err
	}
	if profile.revision != in.ExpectedProfileRevision ||
		profile.proposedVersion == nil || *profile.proposedVersion != in.Version {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	rejectionID, err := id.New("avrej")
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_avatar_rejections
		       (id, account_id, realm_id, agent_id, avatar_version,
		        reason_code, rejected_by_kind, rejected_by_id, rejected_at)
		VALUES ($1,$2,$3,$4,$5,$6,'operator',$7,clock_timestamp())`, rejectionID,
		target.accountID, target.realmID, target.agentID, in.Version,
		reasonCode, p.ID); err != nil {
		return AvatarMutationResult{}, fmt.Errorf("record avatar rejection: %w", err)
	}
	status := avatardomain.StatusRejected
	if profile.activeVersion != nil {
		status = avatardomain.StatusActive
		active, err := getAvatarVersionForMutationTx(ctx, tx, target, *profile.activeVersion)
		if err != nil {
			return AvatarMutationResult{}, err
		}
		// Rejecting a proposal must restore the work state implied by the
		// surviving active version. If the realm selected a newer style, the old
		// active portrait is still usable but evolution remains due; otherwise a
		// rejection would permanently clear the only foreground checkpoint.
		if active.stylePackID != profile.stylePackID ||
			active.stylePackVersion != profile.stylePackVersion {
			status = avatardomain.StatusEvolutionDue
		}
	}
	var resultRevision int64
	err = tx.QueryRow(ctx, `
		UPDATE agent_avatar_profiles
		   SET proposed_avatar_version=NULL, status=$4,
		       subject_form=COALESCE((
		         SELECT v.subject_form FROM agent_avatar_versions v
		          WHERE v.account_id=agent_avatar_profiles.account_id
		            AND v.realm_id=agent_avatar_profiles.realm_id
		            AND v.agent_id=agent_avatar_profiles.agent_id
		            AND v.version=agent_avatar_profiles.active_avatar_version
		       ), 'human'),
		       revision=revision+1,
		       updated_at=clock_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3 AND revision=$5
		 RETURNING revision`, target.accountID, target.realmID, target.agentID,
		string(status), in.ExpectedProfileRevision).Scan(&resultRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	if err != nil {
		return AvatarMutationResult{}, fmt.Errorf("reject avatar proposal: %w", err)
	}
	receipt, err := insertAvatarMutationReceiptTx(ctx, tx, p, target, "avatar",
		target.agentID, "reject", key, fingerprint, resultRevision, in.Version)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	metadata := map[string]any{
		"agent_id": target.agentID, "avatar_version": strconv.FormatInt(in.Version, 10),
		"status": string(status),
	}
	if reasonCode != "" {
		metadata["reason_code"] = reasonCode
	}
	if err := logEventTx(ctx, tx, EventInput{AccountID: target.accountID,
		ActorKind: ActorOperator, ActorID: p.ID,
		Verb: VerbAvatarRejected, Metadata: metadata}); err != nil {
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

// ReportAvatarGenerationFailure records a failed avatar generation attempt for the authenticated agent.
func (s *Store) ReportAvatarGenerationFailure(ctx context.Context, p Principal, in AvatarGenerationFailureInput) (AvatarMutationResult, error) {
	target, err := requireSelfAvatarPrincipal(p)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if in.ExpectedProfileRevision < 1 {
		return AvatarMutationResult{}, fmt.Errorf("%w: expected revision is required", ErrAvatarInputInvalid)
	}
	reasonCode, err := normalizeAvatarReasonCode(in.ReasonCode, true)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	key, err := normalizeAvatarIdempotencyKey(in.IdempotencyKey)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	in.ReasonCode, in.IdempotencyKey = reasonCode, ""
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
		target.realmID, "avatar", target.agentID, p.Kind, p.ID, "fail", key); err != nil {
		return AvatarMutationResult{}, err
	}
	profile, err := lockAvatarProfileTx(ctx, tx, target)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if receipt, replayed, err := replayAvatarMutationTx(ctx, tx, p, target,
		"avatar", target.agentID, "fail", key, fingerprint); err != nil {
		return AvatarMutationResult{}, err
	} else if replayed {
		view, err := getAvatarView(ctx, tx, target)
		return AvatarMutationResult{Avatar: view, Receipt: receipt}, err
	}
	if profile.policy == avatardomain.AutonomyOperatorOnly {
		return AvatarMutationResult{}, ErrAvatarForbidden
	}
	if profile.status == avatardomain.StatusGenerationFailed && !profile.retryReady {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	if profile.revision != in.ExpectedProfileRevision {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	if profile.proposedVersion != nil {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	switch profile.status {
	case avatardomain.StatusPlaceholder,
		avatardomain.StatusGenerationDue,
		avatardomain.StatusEvolutionDue,
		avatardomain.StatusRejected,
		avatardomain.StatusGenerationFailed:
	default:
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	var attemptCount int
	if err := tx.QueryRow(ctx, `SELECT attempt_count FROM agent_avatar_profiles
		WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3`, target.accountID,
		target.realmID, target.agentID).Scan(&attemptCount); err != nil {
		return AvatarMutationResult{}, err
	}
	attemptCount++
	shift := attemptCount - 1
	if shift > 6 {
		shift = 6
	}
	retrySeconds := 60 * (1 << shift)
	if retrySeconds > int(maxAvatarGenerationBackoff/time.Second) {
		retrySeconds = int(maxAvatarGenerationBackoff / time.Second)
	}
	var resultRevision int64
	err = tx.QueryRow(ctx, `
		UPDATE agent_avatar_profiles
		   SET status='generation_failed', attempt_count=$4,
		       retry_after=clock_timestamp()+($5 * interval '1 second'),
		       failure_code=$6, revision=revision+1,
		       updated_at=clock_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3 AND revision=$7
		 RETURNING revision`, target.accountID, target.realmID, target.agentID,
		attemptCount, retrySeconds, reasonCode,
		in.ExpectedProfileRevision).Scan(&resultRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	if err != nil {
		return AvatarMutationResult{}, fmt.Errorf("record avatar generation failure: %w", err)
	}
	receipt, err := insertAvatarMutationReceiptTx(ctx, tx, p, target, "avatar",
		target.agentID, "fail", key, fingerprint, resultRevision, 0)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if err := logEventTx(ctx, tx, EventInput{AccountID: target.accountID,
		ActorKind: ActorAgent, ActorID: p.ID, Verb: VerbAvatarGenerationFailed,
		Metadata: map[string]any{
			"agent_id":      target.agentID,
			"status":        string(avatardomain.StatusGenerationFailed),
			"attempt_count": strconv.Itoa(attemptCount), "reason_code": reasonCode,
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

// SetAvatarPolicy updates the autonomy policy for an operator-selected agent's avatar.
func (s *Store) SetAvatarPolicy(ctx context.Context, p Principal, agentID string, in UpdateAvatarPolicyInput) (AvatarMutationResult, error) {
	target, err := resolveOperatorAvatarTarget(ctx, s.pool, p, agentID)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if in.ExpectedProfileRevision < 1 {
		return AvatarMutationResult{}, fmt.Errorf("%w: expected revision is required", ErrAvatarInputInvalid)
	}
	if err := in.Policy.Validate(); err != nil {
		return AvatarMutationResult{}, fmt.Errorf("%w: %v", ErrAvatarInputInvalid, err)
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
		target.realmID, "avatar", target.agentID, p.Kind, p.ID, "set_policy", key); err != nil {
		return AvatarMutationResult{}, err
	}
	profile, err := lockAvatarProfileTx(ctx, tx, target)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if receipt, replayed, err := replayAvatarMutationTx(ctx, tx, p, target,
		"avatar", target.agentID, "set_policy", key, fingerprint); err != nil {
		return AvatarMutationResult{}, err
	} else if replayed {
		view, err := getAvatarView(ctx, tx, target)
		return AvatarMutationResult{Avatar: view, Receipt: receipt}, err
	}
	if profile.revision != in.ExpectedProfileRevision {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	var resultRevision int64
	err = tx.QueryRow(ctx, `
		UPDATE agent_avatar_profiles
		   SET autonomy_policy=$4, revision=revision+1,
		       updated_at=clock_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3 AND revision=$5
		 RETURNING revision`, target.accountID, target.realmID, target.agentID,
		string(in.Policy), in.ExpectedProfileRevision).Scan(&resultRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return AvatarMutationResult{}, ErrAvatarConflict
	}
	if err != nil {
		return AvatarMutationResult{}, fmt.Errorf("set avatar policy: %w", err)
	}
	receipt, err := insertAvatarMutationReceiptTx(ctx, tx, p, target, "avatar",
		target.agentID, "set_policy", key, fingerprint, resultRevision, 0)
	if err != nil {
		return AvatarMutationResult{}, err
	}
	if err := logEventTx(ctx, tx, EventInput{AccountID: target.accountID,
		ActorKind: ActorOperator, ActorID: p.ID, Verb: VerbAvatarPolicyChanged,
		Metadata: map[string]any{
			"agent_id": target.agentID, "policy_from": string(profile.policy),
			"policy_to": string(in.Policy), "status": string(profile.status),
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

func lockAvatarProfileTx(ctx context.Context, tx pgx.Tx, target avatarTarget) (avatarLockedProfile, error) {
	var out avatarLockedProfile
	var status, policy string
	err := tx.QueryRow(ctx, `
		SELECT p.status, p.autonomy_policy, p.style_pack_id,
		       p.style_pack_version, p.lineage_generation,
		       (p.retry_after IS NULL OR p.retry_after <= clock_timestamp()),
		       p.revision, p.latest_avatar_version,
		       p.proposed_avatar_version, p.active_avatar_version
		  FROM agent_avatar_profiles p
		  JOIN agents a ON a.id=p.agent_id AND a.realm_id=p.realm_id
		  JOIN realms r ON r.id=p.realm_id AND r.account_id=p.account_id
		 WHERE p.account_id=$1 AND p.realm_id=$2 AND p.agent_id=$3
		   AND a.deleted_at IS NULL AND r.deleted_at IS NULL
		 FOR UPDATE OF p`, target.accountID, target.realmID, target.agentID).Scan(
		&status, &policy, &out.stylePackID, &out.stylePackVersion,
		&out.lineageGeneration, &out.retryReady,
		&out.revision, &out.latestVersion, &out.proposedVersion,
		&out.activeVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return avatarLockedProfile{}, ErrAvatarNotFound
	}
	if err != nil {
		return avatarLockedProfile{}, fmt.Errorf("lock avatar profile: %w", err)
	}
	out.status = avatardomain.Status(status)
	out.policy = avatardomain.AutonomyPolicy(policy)
	return out, nil
}

type avatarVersionMutationInfo struct {
	parentVersion     int64
	lineageGeneration int64
	stylePackID       string
	stylePackVersion  int
	subjectForm       avatardomain.SubjectForm
	svg               string
}

func getAvatarVersionForMutationTx(ctx context.Context, tx pgx.Tx, target avatarTarget, version int64) (avatarVersionMutationInfo, error) {
	var out avatarVersionMutationInfo
	var parent *int64
	var subjectForm string
	err := tx.QueryRow(ctx, `
		SELECT v.parent_version, v.lineage_generation,
		       v.style_pack_id, v.style_pack_version,
		       v.subject_form, v.svg
		  FROM agent_avatar_versions v
		  LEFT JOIN agent_avatar_rejections rejection
		    ON rejection.account_id=v.account_id AND rejection.realm_id=v.realm_id
		   AND rejection.agent_id=v.agent_id AND rejection.avatar_version=v.version
		 WHERE v.account_id=$1 AND v.realm_id=$2 AND v.agent_id=$3
		   AND v.version=$4 AND rejection.id IS NULL`, target.accountID,
		target.realmID, target.agentID, version).Scan(&parent,
		&out.lineageGeneration, &out.stylePackID,
		&out.stylePackVersion, &subjectForm, &out.svg)
	if errors.Is(err, pgx.ErrNoRows) {
		return avatarVersionMutationInfo{}, ErrAvatarVersionNotFound
	}
	if err != nil {
		return avatarVersionMutationInfo{}, fmt.Errorf("get avatar mutation version: %w", err)
	}
	if parent != nil {
		out.parentVersion = *parent
	}
	out.subjectForm = avatardomain.SubjectForm(subjectForm)
	return out, nil
}

func updateActiveAvatarProfileTx(ctx context.Context, tx pgx.Tx, target avatarTarget, expectedRevision, version int64, info avatarVersionMutationInfo) (int64, error) {
	var revision int64
	err := tx.QueryRow(ctx, `
		UPDATE agent_avatar_profiles
		   SET active_avatar_version=$4, proposed_avatar_version=NULL,
		       status=CASE WHEN style_pack_id=$5 AND style_pack_version=$6
		                   THEN 'active' ELSE 'evolution_due' END,
		       subject_form=$7, attempt_count=0, retry_after=NULL,
		       failure_code='', revision=revision+1,
		       updated_at=clock_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3 AND revision=$8
		   AND lineage_generation=$9
		 RETURNING revision`, target.accountID, target.realmID, target.agentID,
		version, info.stylePackID, info.stylePackVersion,
		string(info.subjectForm), expectedRevision,
		info.lineageGeneration).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrAvatarConflict
	}
	if err != nil {
		return 0, fmt.Errorf("activate avatar profile: %w", err)
	}
	return revision, nil
}

func loadAvatarStylePackVersion(ctx context.Context, q avatarRowQuerier, accountID, realmID, stylePackID string, version int) (avatardomain.StylePack, error) {
	var raw json.RawMessage
	err := q.QueryRow(ctx, `
		SELECT style_spec FROM avatar_style_pack_versions
		 WHERE account_id=$1 AND realm_id=$2 AND style_pack_id=$3 AND version=$4`,
		accountID, realmID, stylePackID, version).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return avatardomain.StylePack{}, ErrAvatarStyleNotFound
	}
	if err != nil {
		return avatardomain.StylePack{}, fmt.Errorf("load avatar style version: %w", err)
	}
	if stylePackID == avatardomain.DefaultStylePackID && version == avatardomain.BuiltInStylePackVersion {
		return avatardomain.BuiltInFlatVectorStylePack(), nil
	}
	var pack avatardomain.StylePack
	if err := json.Unmarshal(raw, &pack); err != nil {
		return avatardomain.StylePack{}, fmt.Errorf("decode avatar style version: %w", err)
	}
	if pack.ID != stylePackID || pack.Version != version {
		return avatardomain.StylePack{}, ErrAvatarStyleNotFound
	}
	if err := pack.Validate(); err != nil {
		return avatardomain.StylePack{}, fmt.Errorf("%w: %v", ErrAvatarStyleNotFound, err)
	}
	return pack, nil
}

func avatarVersionEventMetadata(agentID string, version, parentVersion, priorVersion int64, stylePackID string, stylePackVersion int, status string) map[string]any {
	metadata := map[string]any{
		"agent_id": agentID, "avatar_version": strconv.FormatInt(version, 10),
		"style_pack_id":      stylePackID,
		"style_pack_version": strconv.Itoa(stylePackVersion), "status": status,
	}
	if parentVersion > 0 {
		metadata["parent_version"] = strconv.FormatInt(parentVersion, 10)
	}
	if priorVersion > 0 {
		metadata["prior_active_version"] = strconv.FormatInt(priorVersion, 10)
	}
	return metadata
}
