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
)

// SelfAvatarCheckpoint is the authenticated, value-free foreground lifecycle
// hint exposed by self hydration. It never contains SVG, descriptions, visual
// specifications, prompts, provenance, or hashes.
type SelfAvatarCheckpoint struct {
	Pending           bool
	Status            string
	Reason            string
	ProfileRevision   int64
	StylePackID       string
	StylePackVersion  int64
	LineageGeneration int64
	ActiveVersion     int64
	ProposedVersion   int64
	AttemptCount      int
	RetryAfter        *time.Time
}

func createAgentAvatarProfileTx(ctx context.Context, tx pgx.Tx, accountID, realmID, agentID string) error {
	var stylePackID string
	var stylePackVersion int
	if err := tx.QueryRow(ctx, `
		SELECT style_pack_id, style_pack_version
		  FROM realm_avatar_styles
		 WHERE account_id=$1 AND realm_id=$2`, accountID, realmID).Scan(
		&stylePackID, &stylePackVersion); err != nil {
		return fmt.Errorf("resolve initial avatar style: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_avatar_profiles
		       (account_id, realm_id, agent_id, style_pack_id,
		        style_pack_version, fallback_seed)
		VALUES ($1,$2,$3,$4,$5,$3)`, accountID, realmID, agentID,
		stylePackID, stylePackVersion); err != nil {
		return fmt.Errorf("create agent avatar profile: %w", err)
	}
	return logEventTx(ctx, tx, EventInput{
		AccountID: accountID, ActorKind: ActorSystem,
		Verb: VerbAvatarGenerationRequested,
		Metadata: map[string]any{
			"agent_id": agentID, "status": string(avatardomain.StatusGenerationDue),
			"style_pack_id":      stylePackID,
			"style_pack_version": strconv.Itoa(stylePackVersion),
		},
	})
}

// GetAvatar returns the authenticated full agent's exact avatar state.
func (s *Store) GetAvatar(ctx context.Context, p Principal) (AvatarView, error) {
	target, err := requireSelfAvatarPrincipal(p)
	if err != nil {
		return AvatarView{}, err
	}
	return getAvatarView(ctx, s.pool, target)
}

// GetSelfAvatarCheckpoint returns only value-free lifecycle state for the
// authenticated full agent. Generation failures remain quiet until their
// server-stamped retry time is due, preventing a hook loop on every turn.
func (s *Store) GetSelfAvatarCheckpoint(ctx context.Context, p Principal) (SelfAvatarCheckpoint, error) {
	target, err := requireSelfAvatarPrincipal(p)
	if err != nil {
		return SelfAvatarCheckpoint{}, err
	}
	var out SelfAvatarCheckpoint
	var status, policy string
	var activeVersion, proposedVersion *int64
	var retryDue bool
	err = s.pool.QueryRow(ctx, `
		SELECT p.status, p.autonomy_policy, p.revision, p.style_pack_id,
		       p.style_pack_version, p.active_avatar_version,
		       p.proposed_avatar_version, p.lineage_generation,
		       p.attempt_count, p.retry_after,
		       (p.retry_after IS NOT NULL AND p.retry_after <= clock_timestamp())
		  FROM agent_avatar_profiles p
		  JOIN agents a ON a.id=p.agent_id AND a.realm_id=p.realm_id
		  JOIN realms r ON r.id=p.realm_id AND r.account_id=p.account_id
		 WHERE p.account_id=$1 AND p.realm_id=$2 AND p.agent_id=$3
		   AND a.deleted_at IS NULL AND r.deleted_at IS NULL`, target.accountID,
		target.realmID, target.agentID).Scan(&status, &policy,
		&out.ProfileRevision, &out.StylePackID, &out.StylePackVersion,
		&activeVersion, &proposedVersion, &out.LineageGeneration,
		&out.AttemptCount, &out.RetryAfter,
		&retryDue)
	if errors.Is(err, pgx.ErrNoRows) {
		return SelfAvatarCheckpoint{}, ErrAvatarNotFound
	}
	if err != nil {
		return SelfAvatarCheckpoint{}, fmt.Errorf("get avatar checkpoint: %w", err)
	}
	out.Status = status
	if activeVersion != nil {
		out.ActiveVersion = *activeVersion
	}
	if proposedVersion != nil {
		out.ProposedVersion = *proposedVersion
	}
	out.Pending, out.Reason = classifySelfAvatarCheckpoint(
		avatardomain.Status(status), avatardomain.AutonomyPolicy(policy), retryDue,
		out.LineageGeneration,
	)
	return out, nil
}

func classifySelfAvatarCheckpoint(status avatardomain.Status, policy avatardomain.AutonomyPolicy, retryDue bool, lineageGeneration int64) (bool, string) {
	// An operator-only agent has no authorized self mutation that can satisfy
	// creative or activation work. Keep the lifecycle state visible through
	// self.show, but never inject a foreground action that would fail and repeat
	// on every prompt. Account operators resolve it through the explicit target
	// routes.
	if policy == avatardomain.AutonomyOperatorOnly {
		switch status {
		case avatardomain.StatusPlaceholder,
			avatardomain.StatusGenerationDue,
			avatardomain.StatusEvolutionDue,
			avatardomain.StatusRejected,
			avatardomain.StatusGenerationFailed,
			avatardomain.StatusProposed:
			return false, "awaiting_operator"
		default:
			return false, ""
		}
	}

	switch status {
	case avatardomain.StatusPlaceholder:
		return true, "initial_avatar"
	case avatardomain.StatusGenerationDue:
		if lineageGeneration > 1 {
			return true, "avatar_reset"
		}
		return true, "initial_avatar"
	case avatardomain.StatusEvolutionDue:
		return true, "style_changed"
	case avatardomain.StatusRejected:
		return true, "proposal_rejected"
	case avatardomain.StatusGenerationFailed:
		if retryDue {
			return true, "retry_due"
		}
	case avatardomain.StatusProposed:
		if policy == avatardomain.AutonomyAgentSelfManaged {
			return true, "activation_due"
		}
		return false, "awaiting_operator"
	}
	return false, ""
}

// GetAgentAvatar returns one exact same-account target to a full operator.
func (s *Store) GetAgentAvatar(ctx context.Context, p Principal, agentID string) (AvatarView, error) {
	target, err := resolveOperatorAvatarTarget(ctx, s.pool, p, agentID)
	if err != nil {
		return AvatarView{}, err
	}
	return getAvatarView(ctx, s.pool, target)
}

// GetAvatarVersion returns one exact immutable creative payload for the
// authenticated full agent. History remains payload-free.
func (s *Store) GetAvatarVersion(ctx context.Context, p Principal, version int64) (AvatarVersion, error) {
	target, err := requireSelfAvatarPrincipal(p)
	if err != nil {
		return AvatarVersion{}, err
	}
	return getAvatarVersionDetail(ctx, s.pool, target, version)
}

// GetAgentAvatarVersion is the operator-target counterpart for one exact
// immutable creative payload.
func (s *Store) GetAgentAvatarVersion(ctx context.Context, p Principal, agentID string, version int64) (AvatarVersion, error) {
	target, err := resolveOperatorAvatarTarget(ctx, s.pool, p, agentID)
	if err != nil {
		return AvatarVersion{}, err
	}
	return getAvatarVersionDetail(ctx, s.pool, target, version)
}

// GetAvatarHistory returns the authenticated full agent's immutable history.
func (s *Store) GetAvatarHistory(ctx context.Context, p Principal, limit int) (AvatarHistoryPage, error) {
	return s.GetAvatarHistoryPage(ctx, p, AvatarHistoryOptions{Limit: limit})
}

// GetAvatarHistoryPage returns one cursor-bounded payload-free history page.
func (s *Store) GetAvatarHistoryPage(ctx context.Context, p Principal, opts AvatarHistoryOptions) (AvatarHistoryPage, error) {
	target, err := requireSelfAvatarPrincipal(p)
	if err != nil {
		return AvatarHistoryPage{}, err
	}
	return getAvatarHistory(ctx, s.pool, target, opts)
}

// GetAgentAvatarHistory is the operator-target counterpart used by review UIs.
func (s *Store) GetAgentAvatarHistory(ctx context.Context, p Principal, agentID string, limit int) (AvatarHistoryPage, error) {
	return s.GetAgentAvatarHistoryPage(ctx, p, agentID, AvatarHistoryOptions{Limit: limit})
}

// GetAgentAvatarHistoryPage is the paginated operator-target counterpart.
func (s *Store) GetAgentAvatarHistoryPage(ctx context.Context, p Principal, agentID string, opts AvatarHistoryOptions) (AvatarHistoryPage, error) {
	target, err := resolveOperatorAvatarTarget(ctx, s.pool, p, agentID)
	if err != nil {
		return AvatarHistoryPage{}, err
	}
	return getAvatarHistory(ctx, s.pool, target, opts)
}

func getAvatarView(ctx context.Context, q avatarRowQuerier, target avatarTarget) (AvatarView, error) {
	var out AvatarView
	var activeVersion, proposedVersion, latestVersion *int64
	var retryAfter *time.Time
	var status string
	var policy string
	var subjectForm string
	var stylePackID string
	var agentName string
	err := q.QueryRow(ctx, `
		SELECT p.account_id, p.realm_id, p.agent_id, p.status,
		       p.autonomy_policy, p.style_pack_id, p.style_pack_version,
		       p.latest_avatar_version, p.proposed_avatar_version,
		       p.active_avatar_version, p.lineage_generation,
		       p.subject_form, p.attempt_count,
		       p.retry_after, p.fallback_seed, p.failure_code, p.revision,
		       p.created_at, p.updated_at, a.name
		  FROM agent_avatar_profiles p
		  JOIN agents a ON a.id=p.agent_id AND a.realm_id=p.realm_id
		  JOIN realms r ON r.id=p.realm_id AND r.account_id=p.account_id
		 WHERE p.account_id=$1 AND p.realm_id=$2 AND p.agent_id=$3
		   AND a.deleted_at IS NULL AND r.deleted_at IS NULL`, target.accountID,
		target.realmID, target.agentID).Scan(&out.Profile.AccountID,
		&out.Profile.RealmID, &out.Profile.AgentID, &status, &policy,
		&stylePackID, &out.Profile.Style.Version, &latestVersion,
		&proposedVersion, &activeVersion, &out.Profile.LineageGeneration,
		&subjectForm,
		&out.Profile.AttemptCount, &retryAfter, &out.Profile.FallbackSeed,
		&out.Profile.FailureCode, &out.Profile.ProfileRevision,
		&out.Profile.CreatedAt, &out.Profile.UpdatedAt, &agentName)
	if errors.Is(err, pgx.ErrNoRows) {
		return AvatarView{}, ErrAvatarNotFound
	}
	if err != nil {
		return AvatarView{}, fmt.Errorf("get avatar profile: %w", err)
	}
	if latestVersion != nil {
		out.Profile.LatestVersion = *latestVersion
	}
	out.Profile.Status = avatardomain.Status(status)
	out.Profile.AutonomyPolicy = avatardomain.AutonomyPolicy(policy)
	out.Profile.SubjectForm = avatardomain.SubjectForm(subjectForm)
	out.Profile.Style.RealmID = out.Profile.RealmID
	out.Profile.Style.StylePackID = stylePackID
	out.Profile.RetryAfter = retryAfter
	if activeVersion != nil {
		out.Profile.ActiveVersion = *activeVersion
		version, err := getAvatarVersion(ctx, q, target, *activeVersion,
			activeVersion, proposedVersion, out.Profile.LineageGeneration)
		if err != nil {
			return AvatarView{}, err
		}
		out.Active = &version
	} else {
		pack, err := loadAvatarStylePackVersion(ctx, q, target.accountID,
			target.realmID, out.Profile.Style.StylePackID,
			out.Profile.Style.Version)
		if err != nil {
			return AvatarView{}, err
		}
		placeholder, err := deterministicAvatarPlaceholder(target, agentName,
			out.Profile.Style, pack, out.Profile.LineageGeneration,
			out.Profile.UpdatedAt)
		if err != nil {
			return AvatarView{}, err
		}
		out.Active = &placeholder
	}
	if proposedVersion != nil {
		out.Profile.ProposedVersion = *proposedVersion
		version, err := getAvatarVersion(ctx, q, target, *proposedVersion,
			activeVersion, proposedVersion, out.Profile.LineageGeneration)
		if err != nil {
			return AvatarView{}, err
		}
		out.Proposed = &version
	}
	return out, nil
}

func getAvatarHistory(ctx context.Context, q avatarRowQuerier, target avatarTarget, opts AvatarHistoryOptions) (AvatarHistoryPage, error) {
	if opts.Limit == 0 {
		opts.Limit = defaultAvatarHistoryLimit
	}
	if opts.Limit < 1 || opts.Limit > maxAvatarHistoryLimit {
		return AvatarHistoryPage{}, fmt.Errorf("%w: history limit must be 1-%d", ErrAvatarInputInvalid, maxAvatarHistoryLimit)
	}
	if opts.BeforeVersion < 0 {
		return AvatarHistoryPage{}, fmt.Errorf("%w: before_version cannot be negative", ErrAvatarInputInvalid)
	}
	if _, _, _, err := getAvatarVersionPointers(ctx, q, target); err != nil {
		return AvatarHistoryPage{}, err
	}
	rows, err := queryAvatarRows(ctx, q, `
		SELECT v.id, v.account_id, v.realm_id, v.agent_id, v.version,
		       v.parent_version, v.lineage_generation,
		       v.style_pack_id, v.style_pack_version,
		       v.subject_form, v.svg_sha256, v.proposed_by_kind,
		       v.proposed_by_id, v.proposed_at,
		       COALESCE(v.version=p.active_avatar_version, FALSE),
		       COALESCE(v.version=p.proposed_avatar_version, FALSE),
		       EXISTS (
		         SELECT 1 FROM agent_avatar_activations activation
		          WHERE activation.account_id=v.account_id
		            AND activation.realm_id=v.realm_id
		            AND activation.agent_id=v.agent_id
		            AND activation.avatar_version=v.version
		            AND activation.lineage_generation=v.lineage_generation
		       ),
		       (SELECT MAX(activation.activated_at)
		          FROM agent_avatar_activations activation
		         WHERE activation.account_id=v.account_id
		           AND activation.realm_id=v.realm_id
		           AND activation.agent_id=v.agent_id
		           AND activation.avatar_version=v.version
		           AND activation.lineage_generation=v.lineage_generation),
		       p.lineage_generation,
		       (SELECT MAX(rejection.rejected_at)
		          FROM agent_avatar_rejections rejection
		         WHERE rejection.account_id=v.account_id
		           AND rejection.realm_id=v.realm_id
		           AND rejection.agent_id=v.agent_id
		           AND rejection.avatar_version=v.version)
		  FROM agent_avatar_versions v
		  JOIN agent_avatar_profiles p
		    ON p.account_id=v.account_id AND p.realm_id=v.realm_id
		   AND p.agent_id=v.agent_id
		  JOIN agents a ON a.id=p.agent_id AND a.realm_id=p.realm_id
		  JOIN realms r ON r.id=p.realm_id AND r.account_id=p.account_id
		 WHERE v.account_id=$1 AND v.realm_id=$2 AND v.agent_id=$3
		   AND a.deleted_at IS NULL AND r.deleted_at IS NULL
		   AND ($4::bigint=0 OR v.version < $4)
		 ORDER BY v.version DESC LIMIT $5`, target.accountID, target.realmID,
		target.agentID, opts.BeforeVersion, opts.Limit+1)
	if err != nil {
		return AvatarHistoryPage{}, fmt.Errorf("list avatar history: %w", err)
	}
	defer rows.Close()
	out := AvatarHistoryPage{Versions: make([]AvatarVersionSummary, 0, opts.Limit+1)}
	for rows.Next() {
		var version AvatarVersionSummary
		var currentLineage int64
		var subjectForm, actorKind string
		if err := rows.Scan(&version.ID, &version.AccountID,
			&version.RealmID, &version.AgentID, &version.Version,
			&version.ParentVersion, &version.LineageGeneration,
			&version.Style.StylePackID,
			&version.Style.Version, &subjectForm, &version.SVGSHA256,
			&actorKind, &version.ProposedBy.ID, &version.ProposedAt,
			&version.IsActive, &version.IsProposed, &version.WasActivated,
			&version.LastActivatedAt, &currentLineage,
			&version.RejectedAt); err != nil {
			return AvatarHistoryPage{}, err
		}
		version.Style.RealmID = version.RealmID
		version.SubjectForm = avatardomain.SubjectForm(subjectForm)
		version.ProposedBy.Kind = actorKind
		version.Rejected = version.RejectedAt != nil
		version.RollbackEligible = version.WasActivated && !version.IsActive &&
			!version.Rejected && version.LineageGeneration == currentLineage
		out.Versions = append(out.Versions, version)
	}
	if err := rows.Err(); err != nil {
		return AvatarHistoryPage{}, err
	}
	if len(out.Versions) > opts.Limit {
		out.Versions = out.Versions[:opts.Limit]
		out.NextBeforeVersion = out.Versions[len(out.Versions)-1].Version
	}
	return out, nil
}

func getAvatarVersionDetail(ctx context.Context, q avatarRowQuerier, target avatarTarget, version int64) (AvatarVersion, error) {
	if version < 1 {
		return AvatarVersion{}, fmt.Errorf("%w: avatar version must be positive", ErrAvatarInputInvalid)
	}
	activeVersion, proposedVersion, currentLineage, err := getAvatarVersionPointers(ctx, q, target)
	if err != nil {
		return AvatarVersion{}, err
	}
	return getAvatarVersion(ctx, q, target, version, activeVersion,
		proposedVersion, currentLineage)
}

func getAvatarVersionPointers(ctx context.Context, q avatarRowQuerier, target avatarTarget) (*int64, *int64, int64, error) {
	var activeVersion, proposedVersion *int64
	var lineageGeneration int64
	err := q.QueryRow(ctx, `
		SELECT p.active_avatar_version, p.proposed_avatar_version,
		       p.lineage_generation
		  FROM agent_avatar_profiles p
		  JOIN agents a ON a.id=p.agent_id AND a.realm_id=p.realm_id
		  JOIN realms r ON r.id=p.realm_id AND r.account_id=p.account_id
		 WHERE p.account_id=$1 AND p.realm_id=$2 AND p.agent_id=$3
		   AND a.deleted_at IS NULL AND r.deleted_at IS NULL`, target.accountID,
		target.realmID, target.agentID).Scan(&activeVersion, &proposedVersion,
		&lineageGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, 0, ErrAvatarNotFound
	}
	if err != nil {
		return nil, nil, 0, fmt.Errorf("get avatar version head: %w", err)
	}
	return activeVersion, proposedVersion, lineageGeneration, nil
}

// Query is not part of pgx.Row's narrow interface, so history accepts either a
// pool or transaction through this small local adapter.
type avatarRowsQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func queryAvatarRows(ctx context.Context, q avatarRowQuerier, sql string, args ...any) (pgx.Rows, error) {
	querier, ok := q.(avatarRowsQuerier)
	if !ok {
		return nil, errors.New("avatar query backend does not support rows")
	}
	return querier.Query(ctx, sql, args...)
}

func getAvatarVersion(ctx context.Context, q avatarRowQuerier, target avatarTarget, version int64, activeVersion, proposedVersion *int64, currentLineage int64) (AvatarVersion, error) {
	var out AvatarVersion
	var parentVersion *int64
	var subjectForm, actorKind string
	var spec, provenance json.RawMessage
	err := q.QueryRow(ctx, `
		SELECT v.id, v.account_id, v.realm_id, v.agent_id, v.version,
		       v.parent_version, v.lineage_generation,
		       v.style_pack_id, v.style_pack_version,
		       v.subject_form, v.svg, v.description, v.visual_spec,
		       v.svg_sha256, v.provenance, v.proposed_by_kind,
		       v.proposed_by_id, v.proposed_at,
		       EXISTS (
		         SELECT 1 FROM agent_avatar_activations activation
		          WHERE activation.account_id=v.account_id
		            AND activation.realm_id=v.realm_id
		            AND activation.agent_id=v.agent_id
		            AND activation.avatar_version=v.version
		            AND activation.lineage_generation=v.lineage_generation
		       ),
		       (SELECT MAX(activation.activated_at)
		          FROM agent_avatar_activations activation
		         WHERE activation.account_id=v.account_id
		           AND activation.realm_id=v.realm_id
		           AND activation.agent_id=v.agent_id
		           AND activation.avatar_version=v.version
		           AND activation.lineage_generation=v.lineage_generation),
		       (SELECT MAX(rejection.rejected_at)
		          FROM agent_avatar_rejections rejection
		         WHERE rejection.account_id=v.account_id
		           AND rejection.realm_id=v.realm_id
		           AND rejection.agent_id=v.agent_id
		           AND rejection.avatar_version=v.version)
		  FROM agent_avatar_versions v
		 WHERE v.account_id=$1 AND v.realm_id=$2 AND v.agent_id=$3
		   AND v.version=$4`, target.accountID, target.realmID, target.agentID,
		version).Scan(&out.ID, &out.AccountID, &out.RealmID, &out.AgentID,
		&out.Version, &parentVersion, &out.LineageGeneration,
		&out.Style.StylePackID,
		&out.Style.Version, &subjectForm, &out.SVG, &out.Description, &spec,
		&out.SVGSHA256, &provenance, &actorKind, &out.ProposedBy.ID,
		&out.ProposedAt, &out.WasActivated, &out.LastActivatedAt,
		&out.RejectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AvatarVersion{}, ErrAvatarVersionNotFound
	}
	if err != nil {
		return AvatarVersion{}, fmt.Errorf("get avatar version: %w", err)
	}
	out.ParentVersion = parentVersion
	out.IsActive = activeVersion != nil && *activeVersion == out.Version
	out.IsProposed = proposedVersion != nil && *proposedVersion == out.Version
	out.Rejected = out.RejectedAt != nil
	out.RollbackEligible = out.WasActivated && !out.IsActive && !out.Rejected &&
		out.LineageGeneration == currentLineage
	out.SubjectForm = avatardomain.SubjectForm(subjectForm)
	out.VisualSpec = append(json.RawMessage(nil), spec...)
	out.Style.RealmID = out.RealmID
	out.ProposedBy.Kind = actorKind
	if len(provenance) > 0 {
		_ = json.Unmarshal(provenance, &out.Provenance)
	}
	return out, nil
}

func deterministicAvatarPlaceholder(target avatarTarget, agentName string, style avatardomain.StylePackRef, pack avatardomain.StylePack, lineageGeneration int64, createdAt time.Time) (AvatarVersion, error) {
	if pack.ID != style.StylePackID || pack.Version != style.Version {
		return AvatarVersion{}, fmt.Errorf("generate avatar placeholder: style identity mismatch")
	}
	seedName := strings.TrimSpace(agentName)
	svg, err := avatardomain.GeneratePlaceholderSVGForStylePack(target.agentID, seedName, pack)
	if errors.Is(err, avatardomain.ErrInvalidPlaceholderSeed) {
		// Older creation surfaces could persist names outside the current seed
		// contract. The stable agent ID remains a deterministic, non-secret seed
		// name so those live agents still receive a placeholder.
		seedName = strings.TrimSpace(target.agentID)
		svg, err = avatardomain.GeneratePlaceholderSVGForStylePack(target.agentID, seedName, pack)
	}
	if err != nil {
		return AvatarVersion{}, fmt.Errorf("generate avatar placeholder: %w", err)
	}
	description := "A deterministic model-free flat-vector placeholder awaiting the agent's initial avatar."
	visualSpec := json.RawMessage(`{"placeholder":true,"deterministic":true,"source":"seeded_builtin"}`)
	if pack.ID != avatardomain.DefaultStylePackID || pack.Version != avatardomain.BuiltInStylePackVersion {
		description = "A deterministic model-free placeholder derived from the selected style pack's human reference."
		referenceID := ""
		for _, reference := range pack.References {
			if reference.SubjectForm == avatardomain.SubjectHuman {
				referenceID = reference.ID
				break
			}
		}
		encoded, err := json.Marshal(map[string]any{
			"placeholder": true, "deterministic": true,
			"source": "style_reference", "style_reference_id": referenceID,
		})
		if err != nil {
			return AvatarVersion{}, fmt.Errorf("generate avatar placeholder spec: %w", err)
		}
		visualSpec = encoded
	}
	digest := sha256.Sum256(svg)
	digestText := hex.EncodeToString(digest[:])
	idHash := sha256.New()
	for _, part := range []string{
		strings.TrimSpace(target.agentID), seedName, style.RealmID,
		style.StylePackID, strconv.Itoa(style.Version),
		strconv.FormatInt(lineageGeneration, 10), digestText,
	} {
		_, _ = idHash.Write([]byte(part))
		_, _ = idHash.Write([]byte{0})
	}
	idDigestText := hex.EncodeToString(idHash.Sum(nil))
	return AvatarVersion{
		ID: "placeholder-" + idDigestText[:16], AccountID: target.accountID,
		RealmID: target.realmID, AgentID: target.agentID, Version: 0,
		LineageGeneration: lineageGeneration,
		SubjectForm:       avatardomain.SubjectHuman,
		Description:       description,
		VisualSpec:        visualSpec,
		SVG:               string(svg), SVGSHA256: digestText, Style: style,
		ProposedBy: AvatarActor{Kind: ActorSystem}, ProposedAt: createdAt,
		IsActive: true,
	}, nil
}
