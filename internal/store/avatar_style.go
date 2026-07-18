package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

// createDefaultRealmAvatarStyleTx installs the model-free built-in style for a
// newly created realm. It intentionally lives inside CreateRealm's transaction
// rather than a database trigger so archive import can stream exact rows
// without trigger-created conflicts.
func createDefaultRealmAvatarStyleTx(ctx context.Context, tx pgx.Tx, accountID, realmID string) error {
	pack := avatardomain.BuiltInFlatVectorStylePack()
	packJSON, err := json.Marshal(pack)
	if err != nil {
		return fmt.Errorf("marshal built-in avatar style: %w", err)
	}
	referencesJSON, err := json.Marshal(pack.References)
	if err != nil {
		return fmt.Errorf("marshal built-in avatar references: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO avatar_style_packs
		       (account_id, realm_id, id, current_version)
		VALUES ($1,$2,$3,$4)`, accountID, realmID, pack.ID, pack.Version); err != nil {
		return fmt.Errorf("create default avatar style head: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO avatar_style_pack_versions
		       (account_id, realm_id, style_pack_id, version, name,
		        description, style_spec, reference_examples, provenance,
		        created_by_kind)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,
		        '{"source":"witself.builtin","revision":"1"}'::jsonb,
		        'system')`, accountID, realmID, pack.ID, pack.Version, pack.Name,
		pack.Description, packJSON, referencesJSON); err != nil {
		return fmt.Errorf("create default avatar style version: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO realm_avatar_styles
		       (account_id, realm_id, style_pack_id, style_pack_version)
		VALUES ($1,$2,$3,$4)`, accountID, realmID, pack.ID, pack.Version); err != nil {
		return fmt.Errorf("select default realm avatar style: %w", err)
	}
	return nil
}

// GetRealmAvatarStyle returns the authenticated agent's own realm style when
// realmID is empty, or one explicit account realm for an operator.
func (s *Store) GetRealmAvatarStyle(ctx context.Context, p Principal, realmID string) (AvatarStyleView, error) {
	var accountID string
	switch p.Kind {
	case PrincipalAgent:
		if _, err := requireSelfAvatarPrincipal(p); err != nil {
			return AvatarStyleView{}, err
		}
		if realmID != "" && realmID != p.RealmID {
			return AvatarStyleView{}, ErrAvatarForbidden
		}
		accountID, realmID = p.AccountID, p.RealmID
	case PrincipalOperator:
		if strings.TrimSpace(p.AccessProfile) != "" && p.AccessProfile != AccessProfileFull {
			return AvatarStyleView{}, ErrAvatarForbidden
		}
		accountID, realmID = p.AccountID, strings.TrimSpace(realmID)
		if accountID == "" || realmID == "" {
			return AvatarStyleView{}, ErrAvatarForbidden
		}
	default:
		return AvatarStyleView{}, ErrAvatarForbidden
	}
	return getRealmAvatarStyle(ctx, s.pool, accountID, realmID)
}

// SetRealmAvatarStyle publishes and selects one immutable realm style version.
// Only a full operator principal can change the team-wide visual grammar.
func (s *Store) SetRealmAvatarStyle(ctx context.Context, p Principal, realmID string, in CreateAvatarStyleVersionInput) (AvatarStyleMutationResult, error) {
	if p.Kind != PrincipalOperator || p.AccountID == "" || p.ID == "" ||
		(strings.TrimSpace(p.AccessProfile) != "" && p.AccessProfile != AccessProfileFull) {
		return AvatarStyleMutationResult{}, ErrAvatarForbidden
	}
	realmID = strings.TrimSpace(realmID)
	if realmID == "" || in.ExpectedStyleRevision < 1 {
		return AvatarStyleMutationResult{}, fmt.Errorf("%w: realm and expected style revision are required", ErrAvatarInputInvalid)
	}
	if err := in.StylePack.Validate(); err != nil {
		return AvatarStyleMutationResult{}, fmt.Errorf("%w: %v", ErrAvatarInputInvalid, err)
	}
	key, err := normalizeAvatarIdempotencyKey(in.IdempotencyKey)
	if err != nil {
		return AvatarStyleMutationResult{}, err
	}
	in.IdempotencyKey = ""
	fingerprint, err := avatarFingerprint(in)
	if err != nil {
		return AvatarStyleMutationResult{}, err
	}
	target := avatarTarget{accountID: p.AccountID, realmID: realmID, agentID: realmID}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AvatarStyleMutationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return AvatarStyleMutationResult{}, err
	}
	if err := lockAvatarIdempotencyKey(ctx, tx, p.AccountID, realmID,
		"style_pack", realmID, p.Kind, p.ID, "set_style", key); err != nil {
		return AvatarStyleMutationResult{}, err
	}
	if receipt, replayed, err := replayAvatarMutationTx(ctx, tx, p, target,
		"style_pack", realmID, "set_style", key, fingerprint); err != nil {
		return AvatarStyleMutationResult{}, err
	} else if replayed {
		style, err := getRealmAvatarStyle(ctx, tx, p.AccountID, realmID)
		if err != nil {
			return AvatarStyleMutationResult{}, err
		}
		return AvatarStyleMutationResult{Style: style, Receipt: receipt}, nil
	}

	var currentRevision int64
	err = tx.QueryRow(ctx, `
		SELECT ras.revision
		  FROM realm_avatar_styles ras
		  JOIN realms r ON r.id=ras.realm_id AND r.account_id=ras.account_id
		 WHERE ras.account_id=$1 AND ras.realm_id=$2
		   AND r.deleted_at IS NULL
		 FOR UPDATE`, p.AccountID, realmID).Scan(&currentRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return AvatarStyleMutationResult{}, ErrAvatarStyleNotFound
	}
	if err != nil {
		return AvatarStyleMutationResult{}, fmt.Errorf("lock realm avatar style: %w", err)
	}
	if currentRevision != in.ExpectedStyleRevision {
		return AvatarStyleMutationResult{}, ErrAvatarConflict
	}

	var currentPackVersion int
	err = tx.QueryRow(ctx, `
		SELECT current_version FROM avatar_style_packs
		 WHERE account_id=$1 AND realm_id=$2 AND id=$3
		 FOR UPDATE`, p.AccountID, realmID, in.StylePack.ID).Scan(&currentPackVersion)
	newPack := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !newPack {
		return AvatarStyleMutationResult{}, fmt.Errorf("lock avatar style pack: %w", err)
	}
	var previousVersion any
	if newPack {
		if in.StylePack.Version != 1 {
			return AvatarStyleMutationResult{}, fmt.Errorf("%w: a new style pack must start at version 1", ErrAvatarInputInvalid)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO avatar_style_packs
			       (account_id, realm_id, id, current_version)
			VALUES ($1,$2,$3,$4)`, p.AccountID, realmID, in.StylePack.ID,
			in.StylePack.Version); err != nil {
			return AvatarStyleMutationResult{}, fmt.Errorf("create avatar style pack: %w", err)
		}
	} else {
		if in.StylePack.Version != currentPackVersion+1 {
			return AvatarStyleMutationResult{}, fmt.Errorf("%w: style version must follow current version %d", ErrAvatarInputInvalid, currentPackVersion)
		}
		previousVersion = currentPackVersion
	}

	packJSON, err := json.Marshal(in.StylePack)
	if err != nil {
		return AvatarStyleMutationResult{}, fmt.Errorf("marshal avatar style pack: %w", err)
	}
	referencesJSON, err := json.Marshal(in.StylePack.References)
	if err != nil {
		return AvatarStyleMutationResult{}, fmt.Errorf("marshal avatar style references: %w", err)
	}
	provenance, _ := json.Marshal(map[string]string{"source": "operator"})
	if _, err := tx.Exec(ctx, `
		INSERT INTO avatar_style_pack_versions
		       (account_id, realm_id, style_pack_id, version, previous_version,
		        name, description, style_spec, reference_examples, provenance,
		        created_by_kind, created_by_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'operator',$11)`,
		p.AccountID, realmID, in.StylePack.ID, in.StylePack.Version,
		previousVersion, in.StylePack.Name, in.StylePack.Description, packJSON,
		referencesJSON, provenance, p.ID); err != nil {
		return AvatarStyleMutationResult{}, fmt.Errorf("create avatar style version: %w", err)
	}
	if !newPack {
		if _, err := tx.Exec(ctx, `
			UPDATE avatar_style_packs
			   SET current_version=$4, revision=revision+1, updated_at=clock_timestamp()
			 WHERE account_id=$1 AND realm_id=$2 AND id=$3`, p.AccountID,
			realmID, in.StylePack.ID, in.StylePack.Version); err != nil {
			return AvatarStyleMutationResult{}, fmt.Errorf("advance avatar style head: %w", err)
		}
	}
	var resultRevision int64
	if err := tx.QueryRow(ctx, `
		UPDATE realm_avatar_styles
		   SET style_pack_id=$3, style_pack_version=$4,
		       revision=revision+1, updated_at=clock_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND revision=$5
		 RETURNING revision`, p.AccountID, realmID, in.StylePack.ID,
		in.StylePack.Version, in.ExpectedStyleRevision).Scan(&resultRevision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AvatarStyleMutationResult{}, ErrAvatarConflict
		}
		return AvatarStyleMutationResult{}, fmt.Errorf("select realm avatar style: %w", err)
	}
	// Every live team member follows the newly selected grammar for its next
	// generation. Historical active versions keep their original style link;
	// only the profile's generation target advances. Any stale proposal remains
	// immutable history but is no longer activation-eligible.
	if _, err := tx.Exec(ctx, `
		UPDATE agent_avatar_profiles p
		   SET style_pack_id=$3, style_pack_version=$4,
		       proposed_avatar_version=NULL,
		       subject_form=COALESCE((
		         SELECT v.subject_form FROM agent_avatar_versions v
		          WHERE v.account_id=p.account_id AND v.realm_id=p.realm_id
		            AND v.agent_id=p.agent_id
		            AND v.version=p.active_avatar_version
		       ), 'human'),
		       status=CASE WHEN p.active_avatar_version IS NULL
		                   THEN 'generation_due' ELSE 'evolution_due' END,
		       attempt_count=0, retry_after=NULL, failure_code='',
		       revision=p.revision+1, updated_at=clock_timestamp()
		 WHERE p.account_id=$1 AND p.realm_id=$2
		   AND EXISTS (
		       SELECT 1 FROM agents a
		        WHERE a.id=p.agent_id AND a.realm_id=p.realm_id
		          AND a.deleted_at IS NULL
		   )`, p.AccountID, realmID, in.StylePack.ID,
		in.StylePack.Version); err != nil {
		return AvatarStyleMutationResult{}, fmt.Errorf("propagate realm avatar style: %w", err)
	}
	receipt, err := insertAvatarMutationReceiptTx(ctx, tx, p, target,
		"style_pack", realmID, "set_style", key, fingerprint, resultRevision,
		int64(in.StylePack.Version))
	if err != nil {
		return AvatarStyleMutationResult{}, err
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: p.AccountID, ActorKind: ActorOperator, ActorID: p.ID,
		Verb: VerbAvatarStyleChanged,
		Metadata: map[string]any{
			"realm_id": realmID, "style_pack_id": in.StylePack.ID,
			"style_pack_version": strconv.Itoa(in.StylePack.Version),
			"style_revision":     strconv.FormatInt(resultRevision, 10),
		},
	}); err != nil {
		return AvatarStyleMutationResult{}, err
	}
	style, err := getRealmAvatarStyle(ctx, tx, p.AccountID, realmID)
	if err != nil {
		return AvatarStyleMutationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AvatarStyleMutationResult{}, err
	}
	return AvatarStyleMutationResult{Style: style, Receipt: receipt}, nil
}

type avatarRowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getRealmAvatarStyle(ctx context.Context, q avatarRowQuerier, accountID, realmID string) (AvatarStyleView, error) {
	var out AvatarStyleView
	var packID string
	var version int
	var raw json.RawMessage
	err := q.QueryRow(ctx, `
		SELECT ras.realm_id, ras.revision, ras.style_pack_id,
		       ras.style_pack_version, spv.style_spec, ras.created_at,
		       ras.updated_at
		  FROM realm_avatar_styles ras
		  JOIN realms r ON r.id=ras.realm_id AND r.account_id=ras.account_id
		  JOIN avatar_style_pack_versions spv
		    ON spv.account_id=ras.account_id AND spv.realm_id=ras.realm_id
		   AND spv.style_pack_id=ras.style_pack_id
		   AND spv.version=ras.style_pack_version
		 WHERE ras.account_id=$1 AND ras.realm_id=$2
		   AND r.deleted_at IS NULL`, accountID, realmID).Scan(&out.RealmID,
		&out.StyleRevision, &packID, &version, &raw, &out.CreatedAt,
		&out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AvatarStyleView{}, ErrAvatarStyleNotFound
	}
	if err != nil {
		return AvatarStyleView{}, fmt.Errorf("get realm avatar style: %w", err)
	}
	if packID == avatardomain.DefaultStylePackID && version == avatardomain.BuiltInStylePackVersion {
		out.StylePack = avatardomain.BuiltInFlatVectorStylePack()
		return out, nil
	}
	if err := json.Unmarshal(raw, &out.StylePack); err != nil {
		return AvatarStyleView{}, fmt.Errorf("decode realm avatar style: %w", err)
	}
	if out.StylePack.ID != packID || out.StylePack.Version != version {
		return AvatarStyleView{}, fmt.Errorf("%w: persisted style identity mismatch", ErrAvatarStyleNotFound)
	}
	if err := out.StylePack.Validate(); err != nil {
		return AvatarStyleView{}, fmt.Errorf("%w: persisted style invalid: %v", ErrAvatarStyleNotFound, err)
	}
	return out, nil
}
