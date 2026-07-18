package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

const (
	maxAvatarIdempotencyKeyBytes = 512
	maxAvatarReasonCodeBytes     = 128
	defaultAvatarHistoryLimit    = 20
	maxAvatarHistoryLimit        = 100
	maxAvatarGenerationBackoff   = time.Hour
	// AvatarRollbackPayloadFloor preserves the two most recently activated,
	// inactive current-lineage payloads as usable rollback targets.
	AvatarRollbackPayloadFloor = 2
	// AvatarDefaultRetainedPayloadCountLimit is the per-agent full-payload default.
	AvatarDefaultRetainedPayloadCountLimit = 20
	// AvatarMinRetainedPayloadCountLimit leaves room for active, proposed, and
	// the documented rollback floor.
	AvatarMinRetainedPayloadCountLimit = AvatarRollbackPayloadFloor + 2
	// AvatarMaxRetainedPayloadCountLimit bounds operator configuration.
	AvatarMaxRetainedPayloadCountLimit = 1000
	// AvatarDefaultRetainedPayloadByteLimit is the per-agent retained-content
	// default over full creative payloads plus continuity fingerprints.
	AvatarDefaultRetainedPayloadByteLimit int64 = 2 * 1024 * 1024
	// AvatarMinRetainedPayloadByteLimit is exactly four maximum creative
	// payloads: active, proposed, and the two documented rollback targets.
	AvatarMinRetainedPayloadByteLimit int64 = 512 * 1024
	// AvatarMaxRetainedPayloadByteLimit bounds per-agent storage configuration.
	AvatarMaxRetainedPayloadByteLimit int64 = 64 * 1024 * 1024
	maxAvatarPayloadBytes             int64 = 128 * 1024
)

var (
	avatarReasonCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
)

var (
	// ErrAvatarForbidden reports a principal or autonomy-policy boundary.
	ErrAvatarForbidden = errors.New("avatar access forbidden")
	// ErrAvatarNotFound hides missing, deleted, and cross-tenant targets behind
	// one result so forged ids cannot become an account/realm oracle.
	ErrAvatarNotFound = errors.New("avatar not found")
	// ErrAvatarVersionNotFound reports an unavailable version inside an already
	// authorized avatar profile.
	ErrAvatarVersionNotFound = errors.New("avatar version not found")
	// ErrAvatarStyleNotFound reports an unavailable realm style version.
	ErrAvatarStyleNotFound = errors.New("avatar style not found")
	// ErrAvatarInputInvalid reports malformed or unbounded client input.
	ErrAvatarInputInvalid = errors.New("invalid avatar input")
	// ErrAvatarConflict reports optimistic-concurrency or lifecycle conflicts.
	ErrAvatarConflict = errors.New("avatar changed concurrently")
	// ErrAvatarIdempotencyConflict reports reuse of one retry key for different
	// normalized request semantics.
	ErrAvatarIdempotencyConflict = errors.New("avatar idempotency key conflict")
	// ErrAvatarPayloadQuotaExceeded reports that protected payloads plus the
	// incoming proposal cannot fit after every eligible payload is compacted.
	ErrAvatarPayloadQuotaExceeded = errors.New("avatar payload quota exceeded")
	// ErrAvatarPayloadCompactionDisabled reports that the requested mutation
	// would require irreversible cleanup while the rollout activation gate is
	// still disabled. It is a stable retryable conflict, not a hard quota
	// refusal: retry after every writer has converged and the gate is enabled.
	ErrAvatarPayloadCompactionDisabled = errors.New("avatar payload compaction is temporarily disabled")
)

// AvatarActor identifies the authenticated principal that proposed or applied
// a lifecycle transition. Client provenance never substitutes for this actor.
type AvatarActor struct {
	Kind string
	ID   string
	Name string
}

// AvatarClientProvenance is untrusted generation metadata retained for audit
// and reproducibility. It is never an authorization input.
type AvatarClientProvenance struct {
	Runtime       string `json:"runtime,omitempty"`
	Model         string `json:"model,omitempty"`
	Recipe        string `json:"recipe,omitempty"`
	RecipeVersion string `json:"recipe_version,omitempty"`
}

// AvatarProfile is one agent's mutable policy and version-pointer projection.
type AvatarProfile struct {
	AccountID                 string
	RealmID                   string
	AgentID                   string
	SubjectForm               avatardomain.SubjectForm
	AutonomyPolicy            avatardomain.AutonomyPolicy
	Status                    avatardomain.Status
	Style                     avatardomain.StylePackRef
	LineageGeneration         int64
	ProfileRevision           int64
	LatestVersion             int64
	ActiveVersion             int64
	ProposedVersion           int64
	AttemptCount              int
	RetryAfter                *time.Time
	FallbackSeed              string
	FailureCode               string
	RetainedPayloadCountLimit int
	RetainedPayloadByteLimit  int64
	RetainedPayloadCount      int
	RetainedPayloadBytes      int64
	RollbackPayloadFloor      int
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

// AvatarVersion is one immutable sanitized avatar payload plus lifecycle
// timestamps projected from append-only activation/rejection rows.
type AvatarVersion struct {
	ID                      string
	AccountID               string
	RealmID                 string
	AgentID                 string
	Version                 int64
	ParentVersion           *int64
	LineageGeneration       int64
	SubjectForm             avatardomain.SubjectForm
	Description             string
	VisualSpec              json.RawMessage
	SVG                     string
	SVGSHA256               string
	LockedLayersSHA256      string
	RendererProfile         avatardomain.RendererProfile
	Style                   avatardomain.StylePackRef
	Provenance              AvatarClientProvenance
	ProposedBy              AvatarActor
	ProposedAt              time.Time
	IsActive                bool
	IsProposed              bool
	WasActivated            bool
	RollbackEligible        bool
	Rejected                bool
	LastActivatedAt         *time.Time
	RejectedAt              *time.Time
	PayloadState            avatardomain.PayloadState
	PayloadBytes            int64
	PayloadCompactedAt      *time.Time
	PayloadCompactionReason string
}

// AvatarVersionSummary is the bounded metadata and lifecycle projection used
// by history listings. Creative payloads remain available only through an
// exact version read.
type AvatarVersionSummary struct {
	ID                      string
	AccountID               string
	RealmID                 string
	AgentID                 string
	Version                 int64
	ParentVersion           *int64
	LineageGeneration       int64
	SubjectForm             avatardomain.SubjectForm
	SVGSHA256               string
	LockedLayersSHA256      string
	RendererProfile         avatardomain.RendererProfile
	Style                   avatardomain.StylePackRef
	ProposedBy              AvatarActor
	ProposedAt              time.Time
	IsActive                bool
	IsProposed              bool
	WasActivated            bool
	RollbackEligible        bool
	Rejected                bool
	LastActivatedAt         *time.Time
	RejectedAt              *time.Time
	PayloadState            avatardomain.PayloadState
	PayloadBytes            int64
	PayloadCompactedAt      *time.Time
	PayloadCompactionReason string
}

// AvatarView combines the exact profile with active and pending payloads. A
// deterministic non-persisted placeholder is returned as Active while the
// durable active pointer is empty.
type AvatarView struct {
	Profile  AvatarProfile
	Active   *AvatarVersion
	Proposed *AvatarVersion
}

// AvatarHistoryPage is a bounded newest-first immutable version page.
type AvatarHistoryPage struct {
	Versions          []AvatarVersionSummary
	NextBeforeVersion int64
}

// AvatarHistoryOptions selects one bounded newest-first page. BeforeVersion
// is exclusive; zero starts from the newest immutable version.
type AvatarHistoryOptions struct {
	Limit         int
	BeforeVersion int64
}

// AvatarStyleView is the realm's selected immutable style pack.
type AvatarStyleView struct {
	RealmID       string
	StyleRevision int64
	StylePack     avatardomain.StylePack
	Rollout       *AvatarStyleRollout
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// AvatarStyleRollout is value-free durable progress for asynchronously
// projecting one selected realm style onto every live agent profile. Creative
// payloads remain in immutable style/avatar versions and never enter the job.
type AvatarStyleRollout struct {
	StyleRevision         int64
	StylePackID           string
	StylePackVersion      int
	Status                string
	TargetProfileCount    *int64
	ProcessedProfileCount int64
	BatchCount            int64
	LastBatchSize         int
	FailureCount          int
	RetryAfter            *time.Time
	LastFailureCode       string
	CreatedAt             time.Time
	StartedAt             *time.Time
	UpdatedAt             time.Time
	CompletedAt           *time.Time
	SupersededAt          *time.Time
}

// AvatarMutationReceipt is the durable value-free retry receipt for one
// mutation. RequestHash is a canonical SHA-256 fingerprint, never SVG content.
type AvatarMutationReceipt struct {
	Operation               string
	Actor                   AvatarActor
	RequestHash             string
	ResultRevision          int64
	ResultVersion           int64
	ResultLineageGeneration int64
	Replayed                bool
	CreatedAt               time.Time
}

// AvatarMutationResult returns the post-mutation profile and its retry receipt.
type AvatarMutationResult struct {
	Avatar  AvatarView
	Receipt AvatarMutationReceipt
}

// AvatarStyleMutationResult is the style counterpart of AvatarMutationResult.
type AvatarStyleMutationResult struct {
	Style   AvatarStyleView
	Receipt AvatarMutationReceipt
}

// ProposeAvatarInput submits one client-generated candidate. Scope is always
// derived from the authenticated self principal or an explicit operator target.
type ProposeAvatarInput struct {
	ExpectedProfileRevision int64
	ParentVersion           int64
	StylePackID             string
	StylePackVersion        int
	SubjectForm             avatardomain.SubjectForm
	Description             string
	VisualSpec              json.RawMessage
	SVG                     string
	Provenance              AvatarClientProvenance
	IdempotencyKey          string
}

// ActivateAvatarInput identifies the proposed avatar version to activate.
type ActivateAvatarInput struct {
	Version                 int64
	ExpectedProfileRevision int64
	IdempotencyKey          string
}

// RollbackAvatarInput identifies the prior avatar version to restore.
type RollbackAvatarInput = ActivateAvatarInput

// ResetAvatarInput starts a fresh non-destructive avatar lineage. Existing
// versions and lifecycle ledgers remain immutable and globally numbered.
type ResetAvatarInput struct {
	ExpectedProfileRevision int64
	ReasonCode              string
	IdempotencyKey          string
}

// RejectAvatarInput identifies the proposed avatar version to reject.
type RejectAvatarInput struct {
	Version                 int64
	ExpectedProfileRevision int64
	ReasonCode              string
	IdempotencyKey          string
}

// AvatarGenerationFailureInput records a failed avatar generation attempt.
type AvatarGenerationFailureInput struct {
	ExpectedProfileRevision int64
	ReasonCode              string
	IdempotencyKey          string
}

// UpdateAvatarPolicyInput changes an avatar profile's autonomy policy.
type UpdateAvatarPolicyInput struct {
	Policy                  avatardomain.AutonomyPolicy
	ExpectedProfileRevision int64
	IdempotencyKey          string
}

// UpdateAvatarQuotaInput changes one agent's retained-content limits.
// Any required compaction is applied atomically with the limit change.
type UpdateAvatarQuotaInput struct {
	RetainedPayloadCountLimit int
	RetainedPayloadByteLimit  int64
	ExpectedProfileRevision   int64
	IdempotencyKey            string
}

// CreateAvatarStyleVersionInput creates a new version of a realm style pack.
type CreateAvatarStyleVersionInput struct {
	ExpectedStyleRevision int64
	StylePack             avatardomain.StylePack
	IdempotencyKey        string
}

type avatarTarget struct {
	accountID string
	realmID   string
	agentID   string
	agentName string
}

func requireSelfAvatarPrincipal(p Principal) (avatarTarget, error) {
	if p.Kind != PrincipalAgent ||
		(strings.TrimSpace(p.AccessProfile) != "" && p.AccessProfile != AccessProfileFull) ||
		p.AccountID == "" || p.RealmID == "" || p.ID == "" {
		return avatarTarget{}, ErrAvatarForbidden
	}
	return avatarTarget{accountID: p.AccountID, realmID: p.RealmID, agentID: p.ID, agentName: p.AgentName}, nil
}

func resolveOperatorAvatarTarget(ctx context.Context, q avatarRowQuerier, p Principal, agentID string) (avatarTarget, error) {
	if p.Kind != PrincipalOperator ||
		(strings.TrimSpace(p.AccessProfile) != "" && p.AccessProfile != AccessProfileFull) ||
		p.AccountID == "" || p.ID == "" || strings.TrimSpace(agentID) == "" {
		return avatarTarget{}, ErrAvatarForbidden
	}
	target := avatarTarget{accountID: p.AccountID, agentID: strings.TrimSpace(agentID)}
	err := q.QueryRow(ctx, `
		SELECT a.realm_id, a.name
		  FROM agents a
		  JOIN realms r ON r.id=a.realm_id
		 WHERE a.id=$1 AND r.account_id=$2
		   AND a.deleted_at IS NULL AND r.deleted_at IS NULL`, target.agentID,
		target.accountID).Scan(&target.realmID, &target.agentName)
	if errors.Is(err, pgx.ErrNoRows) {
		return avatarTarget{}, ErrAvatarNotFound
	}
	if err != nil {
		return avatarTarget{}, fmt.Errorf("resolve operator avatar target: %w", err)
	}
	return target, nil
}

func avatarActor(p Principal) AvatarActor {
	name := ""
	if p.Kind == PrincipalAgent {
		name = p.AgentName
	}
	return AvatarActor{Kind: p.Kind, ID: p.ID, Name: name}
}

func avatarAuditActor(p Principal) string {
	if p.Kind == PrincipalAgent {
		return ActorAgent
	}
	return ActorOperator
}

func normalizeAvatarIdempotencyKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > maxAvatarIdempotencyKeyBytes || !utf8.ValidString(key) {
		return "", fmt.Errorf("%w: idempotency key must contain 1-%d bytes", ErrAvatarInputInvalid, maxAvatarIdempotencyKeyBytes)
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: idempotency key contains a control character", ErrAvatarInputInvalid)
		}
	}
	return key, nil
}

func normalizeAvatarReasonCode(code string, required bool) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" && !required {
		return "", nil
	}
	if code == "" || len(code) > maxAvatarReasonCodeBytes || !avatarReasonCodePattern.MatchString(code) {
		return "", fmt.Errorf("%w: invalid reason code", ErrAvatarInputInvalid)
	}
	return code, nil
}

func normalizeAvatarClient(in AvatarClientProvenance) (AvatarClientProvenance, error) {
	values := []*string{&in.Runtime, &in.Model, &in.Recipe, &in.RecipeVersion}
	for _, value := range values {
		normalized, err := avatardomain.NormalizeClientProvenanceLabel(*value)
		if err != nil {
			return AvatarClientProvenance{}, fmt.Errorf("%w: invalid client provenance", ErrAvatarInputInvalid)
		}
		*value = normalized
	}
	return in, nil
}

func avatarFingerprint(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("%w: canonicalize request", ErrAvatarInputInvalid)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func lockAvatarIdempotencyKey(ctx context.Context, tx pgx.Tx, accountID, realmID, targetKind, targetID, actorKind, actorID, operation, key string) error {
	name := strings.Join([]string{accountID, realmID, targetKind, targetID, actorKind, actorID, operation, key}, "\x00")
	sum := sha256.Sum256([]byte(name))
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(binary.BigEndian.Uint64(sum[:8])))
	return err
}

func replayAvatarMutationTx(ctx context.Context, tx pgx.Tx, p Principal, target avatarTarget, targetKind, targetID, operation, key, requestHash string) (AvatarMutationReceipt, bool, error) {
	var receipt AvatarMutationReceipt
	var storedHash string
	err := tx.QueryRow(ctx, `
		SELECT request_hash, result_revision, COALESCE(result_version, 0),
		       COALESCE(result_lineage_generation, 0), created_at
		  FROM avatar_mutation_receipts
		 WHERE account_id=$1 AND realm_id=$2 AND target_kind=$3 AND target_id=$4
		   AND actor_kind=$5 AND actor_id=$6 AND operation=$7 AND idempotency_key=$8`,
		target.accountID, target.realmID, targetKind, targetID, p.Kind, p.ID,
		operation, key).Scan(&storedHash, &receipt.ResultRevision,
		&receipt.ResultVersion, &receipt.ResultLineageGeneration,
		&receipt.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AvatarMutationReceipt{}, false, nil
	}
	if err != nil {
		return AvatarMutationReceipt{}, false, fmt.Errorf("replay avatar mutation: %w", err)
	}
	if storedHash != requestHash {
		return AvatarMutationReceipt{}, false, ErrAvatarIdempotencyConflict
	}
	receipt.Operation = operation
	receipt.Actor = avatarActor(p)
	receipt.RequestHash = storedHash
	receipt.Replayed = true
	return receipt, true, nil
}

func insertAvatarMutationReceiptTx(ctx context.Context, tx pgx.Tx, p Principal, target avatarTarget, targetKind, targetID, operation, key, requestHash string, revision int64, version int64) (AvatarMutationReceipt, error) {
	return insertAvatarMutationReceiptResultTx(ctx, tx, p, target, targetKind,
		targetID, operation, key, requestHash, revision, version, 0)
}

func insertAvatarMutationReceiptWithLineageTx(ctx context.Context, tx pgx.Tx, p Principal, target avatarTarget, targetKind, targetID, operation, key, requestHash string, revision, version, lineageGeneration int64) (AvatarMutationReceipt, error) {
	return insertAvatarMutationReceiptResultTx(ctx, tx, p, target, targetKind,
		targetID, operation, key, requestHash, revision, version,
		lineageGeneration)
}

func insertAvatarMutationReceiptResultTx(ctx context.Context, tx pgx.Tx, p Principal, target avatarTarget, targetKind, targetID, operation, key, requestHash string, revision, version, lineageGeneration int64) (AvatarMutationReceipt, error) {
	var receipt AvatarMutationReceipt
	var nullableVersion any
	if version > 0 {
		nullableVersion = version
	}
	var nullableLineage any
	if lineageGeneration > 0 {
		nullableLineage = lineageGeneration
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO avatar_mutation_receipts
		       (account_id, realm_id, target_kind, target_id, actor_kind,
		        actor_id, operation, idempotency_key, request_hash,
		        result_revision, result_version, result_lineage_generation)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING created_at`, target.accountID, target.realmID, targetKind,
		targetID, p.Kind, p.ID, operation, key, requestHash, revision,
		nullableVersion, nullableLineage).Scan(&receipt.CreatedAt)
	if err != nil {
		return AvatarMutationReceipt{}, fmt.Errorf("record avatar mutation receipt: %w", err)
	}
	receipt.Operation = operation
	receipt.Actor = avatarActor(p)
	receipt.RequestHash = requestHash
	receipt.ResultRevision = revision
	receipt.ResultVersion = version
	receipt.ResultLineageGeneration = lineageGeneration
	return receipt, nil
}
