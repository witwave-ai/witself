package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

const (
	// MemoryVectorMetricCosine selects cosine similarity ranking.
	MemoryVectorMetricCosine = "cosine"
	// MemoryVectorMetricDot selects dot-product ranking.
	MemoryVectorMetricDot = "dot"
	// MemoryVectorMetricEuclidean selects Euclidean-distance ranking.
	MemoryVectorMetricEuclidean = "euclidean"

	// MemoryVectorNormalizationNone declares unnormalized vector components.
	MemoryVectorNormalizationNone = "none"
	// MemoryVectorNormalizationL2 declares unit-length vector components.
	MemoryVectorNormalizationL2 = "l2"

	maxMemoryVectorDimensions = 4096
	maxMemoryVectorProfiles   = 100
	maxMemoryVectorComponent  = 1_000_000.0
	memoryVectorNormTolerance = 1e-6
)

// MemoryVectorProfile is an immutable, portable declaration of the exact
// client-side vector recipe. Provider and model are identifiers, never backend
// credentials or a request to run inference.
type MemoryVectorProfile struct {
	ID             string    `json:"id"`
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	Recipe         string    `json:"recipe"`
	RecipeVersion  string    `json:"recipe_version"`
	Dimensions     int       `json:"dimensions"`
	DistanceMetric string    `json:"distance_metric"`
	Normalization  string    `json:"normalization"`
	ContractHash   string    `json:"contract_hash"`
	CreatedAt      time.Time `json:"created_at"`
}

// CreateMemoryVectorProfileInput declares a portable client-side vector recipe.
type CreateMemoryVectorProfileInput struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Recipe         string `json:"recipe"`
	RecipeVersion  string `json:"recipe_version"`
	Dimensions     int    `json:"dimensions"`
	DistanceMetric string `json:"distance_metric"`
	Normalization  string `json:"normalization"`
}

// MemoryVectorReceipt deliberately excludes vector components. It is safe for
// ordinary responses, logs, and audit metadata.
type MemoryVectorReceipt struct {
	ProfileID     string    `json:"profile_id"`
	MemoryID      string    `json:"memory_id"`
	MemoryVersion int64     `json:"memory_version"`
	ContentHash   string    `json:"content_hash"`
	VectorHash    string    `json:"vector_hash"`
	Dimensions    int       `json:"dimensions"`
	CreatedAt     time.Time `json:"created_at"`
	Replayed      bool      `json:"replayed,omitempty"`
}

// PutMemoryVectorInput attaches client-generated vector components to one exact
// content-hash-pinned memory version.
type PutMemoryVectorInput struct {
	ProfileID     string    `json:"profile_id"`
	MemoryID      string    `json:"memory_id"`
	MemoryVersion int64     `json:"memory_version"`
	ContentHash   string    `json:"content_hash"`
	Vector        []float64 `json:"vector"`
}

type memoryVectorProfileContract struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Recipe         string `json:"recipe"`
	RecipeVersion  string `json:"recipe_version"`
	Dimensions     int    `json:"dimensions"`
	DistanceMetric string `json:"distance_metric"`
	Normalization  string `json:"normalization"`
}

func normalizeMemoryVectorProfileInput(in CreateMemoryVectorProfileInput) (CreateMemoryVectorProfileInput, string, error) {
	in.Provider = strings.TrimSpace(in.Provider)
	in.Model = strings.TrimSpace(in.Model)
	in.Recipe = strings.TrimSpace(in.Recipe)
	in.RecipeVersion = strings.TrimSpace(in.RecipeVersion)
	in.DistanceMetric = strings.ToLower(strings.TrimSpace(in.DistanceMetric))
	in.Normalization = strings.ToLower(strings.TrimSpace(in.Normalization))
	if len(in.Provider) < 1 || len(in.Provider) > 128 || len(in.Model) < 1 || len(in.Model) > 256 ||
		len(in.Recipe) < 1 || len(in.Recipe) > 128 || len(in.RecipeVersion) < 1 || len(in.RecipeVersion) > 128 {
		return CreateMemoryVectorProfileInput{}, "", fmt.Errorf("%w: vector profile identity fields are required and bounded", ErrMemoryInputInvalid)
	}
	for _, field := range []struct{ label, value string }{
		{label: "provider", value: in.Provider},
		{label: "model", value: in.Model},
		{label: "recipe", value: in.Recipe},
		{label: "recipe_version", value: in.RecipeVersion},
	} {
		if strings.IndexFunc(field.value, func(char rune) bool { return char < 0x20 || char == 0x7f }) >= 0 {
			return CreateMemoryVectorProfileInput{}, "", fmt.Errorf("%w: vector profile %s contains a control character", ErrMemoryInputInvalid, field.label)
		}
	}
	if in.Dimensions < 1 || in.Dimensions > maxMemoryVectorDimensions {
		return CreateMemoryVectorProfileInput{}, "", fmt.Errorf("%w: vector dimensions must be 1-%d", ErrMemoryInputInvalid, maxMemoryVectorDimensions)
	}
	if in.DistanceMetric != MemoryVectorMetricCosine && in.DistanceMetric != MemoryVectorMetricDot && in.DistanceMetric != MemoryVectorMetricEuclidean {
		return CreateMemoryVectorProfileInput{}, "", fmt.Errorf("%w: vector distance_metric must be cosine, dot, or euclidean", ErrMemoryInputInvalid)
	}
	if in.Normalization != MemoryVectorNormalizationNone && in.Normalization != MemoryVectorNormalizationL2 {
		return CreateMemoryVectorProfileInput{}, "", fmt.Errorf("%w: vector normalization must be none or l2", ErrMemoryInputInvalid)
	}
	if in.DistanceMetric == MemoryVectorMetricDot && in.Normalization != MemoryVectorNormalizationL2 {
		return CreateMemoryVectorProfileInput{}, "", fmt.Errorf("%w: dot profiles require l2 normalization", ErrMemoryInputInvalid)
	}
	contract := memoryVectorProfileContract(in)
	raw, err := json.Marshal(contract)
	if err != nil {
		return CreateMemoryVectorProfileInput{}, "", err
	}
	sum := sha256.Sum256(raw)
	return in, hex.EncodeToString(sum[:]), nil
}

func normalizeMemoryVector(vector []float64, profile MemoryVectorProfile) ([]float64, string, error) {
	if len(vector) != profile.Dimensions || len(vector) < 1 || len(vector) > maxMemoryVectorDimensions {
		return nil, "", fmt.Errorf("%w: vector has %d dimensions; profile requires %d", ErrMemoryInputInvalid, len(vector), profile.Dimensions)
	}
	out := append([]float64(nil), vector...)
	normSquared := 0.0
	for i, component := range out {
		if math.IsNaN(component) || math.IsInf(component, 0) || math.Abs(component) > maxMemoryVectorComponent {
			return nil, "", fmt.Errorf("%w: vector component %d is not finite and bounded", ErrMemoryInputInvalid, i)
		}
		if component == 0 {
			out[i] = 0 // canonicalize negative zero before hashing.
		}
		normSquared += out[i] * out[i]
		if math.IsInf(normSquared, 0) {
			return nil, "", fmt.Errorf("%w: vector norm overflow", ErrMemoryInputInvalid)
		}
	}
	norm := math.Sqrt(normSquared)
	if profile.DistanceMetric == MemoryVectorMetricCosine && norm == 0 {
		return nil, "", fmt.Errorf("%w: cosine vector must be non-zero", ErrMemoryInputInvalid)
	}
	if profile.Normalization == MemoryVectorNormalizationL2 && math.Abs(norm-1) > memoryVectorNormTolerance {
		return nil, "", fmt.Errorf("%w: vector must have l2 norm 1 within tolerance", ErrMemoryInputInvalid)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return out, hex.EncodeToString(sum[:]), nil
}

// CreateMemoryVectorProfile returns the immutable profile for a normalized
// vector contract, creating it when necessary.
func (s *Store) CreateMemoryVectorProfile(ctx context.Context, p Principal, in CreateMemoryVectorProfileInput) (MemoryVectorProfile, error) {
	if p.Kind != PrincipalAgent {
		return MemoryVectorProfile{}, ErrMemoryForbidden
	}
	normalized, contractHash, err := normalizeMemoryVectorProfileInput(in)
	if err != nil {
		return MemoryVectorProfile{}, err
	}
	profileID, err := id.New("mvp")
	if err != nil {
		return MemoryVectorProfile{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MemoryVectorProfile{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MemoryVectorProfile{}, err
	}
	// Serialize profile creation per owner so the hard response/storage ceiling
	// cannot be exceeded by concurrent distinct contracts. Exact retries still
	// return the existing immutable profile at the ceiling.
	lockKey := p.AccountID + "|" + p.RealmID + "|" + p.ID + "|memory-vector-profiles"
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return MemoryVectorProfile{}, fmt.Errorf("lock memory vector profiles: %w", err)
	}
	var out MemoryVectorProfile
	err = tx.QueryRow(ctx, `
		SELECT id,provider,model,recipe,recipe_version,dimensions,
		       distance_metric,normalization,contract_hash,created_at
		FROM memory_vector_profiles
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND contract_hash=$4`, p.AccountID, p.RealmID, p.ID, contractHash).
		Scan(&out.ID, &out.Provider, &out.Model, &out.Recipe, &out.RecipeVersion,
			&out.Dimensions, &out.DistanceMetric, &out.Normalization,
			&out.ContractHash, &out.CreatedAt)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return MemoryVectorProfile{}, err
		}
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return MemoryVectorProfile{}, err
	}
	var profileCount int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM memory_vector_profiles
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3`,
		p.AccountID, p.RealmID, p.ID).Scan(&profileCount); err != nil {
		return MemoryVectorProfile{}, err
	}
	if profileCount >= maxMemoryVectorProfiles {
		return MemoryVectorProfile{}, fmt.Errorf("%w: memory vector profile limit is %d", ErrMemoryInputInvalid, maxMemoryVectorProfiles)
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO memory_vector_profiles
		  (id,account_id,realm_id,owner_kind,owner_id,provider,model,recipe,
		   recipe_version,dimensions,distance_metric,normalization,contract_hash,
		   created_by_agent_id)
		VALUES ($1,$2,$3,'agent',$4,$5,$6,$7,$8,$9,$10,$11,$12,$4)
		RETURNING id,provider,model,recipe,recipe_version,dimensions,
		          distance_metric,normalization,contract_hash,created_at`,
		profileID, p.AccountID, p.RealmID, p.ID, normalized.Provider,
		normalized.Model, normalized.Recipe, normalized.RecipeVersion,
		normalized.Dimensions, normalized.DistanceMetric, normalized.Normalization,
		contractHash).Scan(&out.ID, &out.Provider, &out.Model, &out.Recipe,
		&out.RecipeVersion, &out.Dimensions, &out.DistanceMetric,
		&out.Normalization, &out.ContractHash, &out.CreatedAt)
	if err != nil {
		return MemoryVectorProfile{}, fmt.Errorf("create memory vector profile: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MemoryVectorProfile{}, err
	}
	return out, nil
}

// ListMemoryVectorProfiles returns the caller's bounded immutable profile set.
func (s *Store) ListMemoryVectorProfiles(ctx context.Context, p Principal) ([]MemoryVectorProfile, error) {
	if p.Kind != PrincipalAgent {
		return nil, ErrMemoryForbidden
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT id,provider,model,recipe,recipe_version,dimensions,
		       distance_metric,normalization,contract_hash,created_at
		FROM memory_vector_profiles
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		ORDER BY created_at,id LIMIT $4`, p.AccountID, p.RealmID, p.ID, maxMemoryVectorProfiles)
	if err != nil {
		return nil, fmt.Errorf("list memory vector profiles: %w", err)
	}
	defer rows.Close()
	out := []MemoryVectorProfile{}
	for rows.Next() {
		var profile MemoryVectorProfile
		if err := rows.Scan(&profile.ID, &profile.Provider, &profile.Model,
			&profile.Recipe, &profile.RecipeVersion, &profile.Dimensions,
			&profile.DistanceMetric, &profile.Normalization,
			&profile.ContractHash, &profile.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// PutMemoryVector stores client-generated components for one exact memory
// version and returns a value-free receipt.
func (s *Store) PutMemoryVector(ctx context.Context, p Principal, in PutMemoryVectorInput) (MemoryVectorReceipt, error) {
	if p.Kind != PrincipalAgent {
		return MemoryVectorReceipt{}, ErrMemoryForbidden
	}
	in.ProfileID = strings.TrimSpace(in.ProfileID)
	in.MemoryID = strings.TrimSpace(in.MemoryID)
	in.ContentHash = strings.TrimSpace(in.ContentHash)
	if in.ProfileID == "" || in.MemoryID == "" || in.MemoryVersion < 1 || !isSHA256Hex(in.ContentHash) {
		return MemoryVectorReceipt{}, fmt.Errorf("%w: profile_id, memory_id, positive memory_version, and content_hash are required", ErrMemoryInputInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MemoryVectorReceipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MemoryVectorReceipt{}, err
	}
	profile, err := getMemoryVectorProfileTx(ctx, tx, p, in.ProfileID)
	if err != nil {
		return MemoryVectorReceipt{}, err
	}
	vector, vectorHash, err := normalizeMemoryVector(in.Vector, profile)
	if err != nil {
		return MemoryVectorReceipt{}, err
	}
	var storedContentHash string
	err = tx.QueryRow(ctx, `
		SELECT content_hash FROM memory_versions
		WHERE memory_id=$1 AND version=$2 AND account_id=$3 AND realm_id=$4
		  AND owner_kind='agent' AND owner_id=$5`, in.MemoryID, in.MemoryVersion,
		p.AccountID, p.RealmID, p.ID).Scan(&storedContentHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryVectorReceipt{}, ErrMemoryNotFound
	}
	if err != nil {
		return MemoryVectorReceipt{}, err
	}
	if storedContentHash != in.ContentHash {
		return MemoryVectorReceipt{}, fmt.Errorf("%w: vector content_hash does not match the exact memory version", ErrMemoryConflict)
	}
	rawVector, _ := json.Marshal(vector)
	var out MemoryVectorReceipt
	err = tx.QueryRow(ctx, `
		INSERT INTO memory_vectors
		  (profile_id,memory_id,memory_version,account_id,realm_id,owner_kind,
		   owner_id,content_hash,vector,vector_hash,created_by_agent_id)
		VALUES ($1,$2,$3,$4,$5,'agent',$6,$7,$8::jsonb,$9,$6)
		ON CONFLICT (profile_id,memory_id,memory_version) DO NOTHING
		RETURNING profile_id,memory_id,memory_version,content_hash,vector_hash,
		          jsonb_array_length(vector),created_at`, in.ProfileID, in.MemoryID,
		in.MemoryVersion, p.AccountID, p.RealmID, p.ID, in.ContentHash,
		string(rawVector), vectorHash).Scan(&out.ProfileID, &out.MemoryID,
		&out.MemoryVersion, &out.ContentHash, &out.VectorHash, &out.Dimensions,
		&out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			SELECT profile_id,memory_id,memory_version,content_hash,vector_hash,
			       jsonb_array_length(vector),created_at
			FROM memory_vectors
			WHERE profile_id=$1 AND memory_id=$2 AND memory_version=$3
			  AND account_id=$4 AND realm_id=$5 AND owner_kind='agent' AND owner_id=$6`,
			in.ProfileID, in.MemoryID, in.MemoryVersion, p.AccountID, p.RealmID,
			p.ID).Scan(&out.ProfileID, &out.MemoryID, &out.MemoryVersion,
			&out.ContentHash, &out.VectorHash, &out.Dimensions, &out.CreatedAt)
		if err != nil {
			return MemoryVectorReceipt{}, err
		}
		if out.ContentHash != in.ContentHash || out.VectorHash != vectorHash {
			return MemoryVectorReceipt{}, fmt.Errorf("%w: exact memory vector is immutable", ErrMemoryConflict)
		}
		out.Replayed = true
	} else if err != nil {
		return MemoryVectorReceipt{}, fmt.Errorf("put memory vector: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MemoryVectorReceipt{}, err
	}
	return out, nil
}

type memoryVectorQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getMemoryVectorProfileTx(ctx context.Context, q memoryVectorQueryer, p Principal, profileID string) (MemoryVectorProfile, error) {
	var out MemoryVectorProfile
	err := q.QueryRow(ctx, `
		SELECT id,provider,model,recipe,recipe_version,dimensions,
		       distance_metric,normalization,contract_hash,created_at
		FROM memory_vector_profiles
		WHERE id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4`, profileID, p.AccountID,
		p.RealmID, p.ID).Scan(&out.ID, &out.Provider, &out.Model, &out.Recipe,
		&out.RecipeVersion, &out.Dimensions, &out.DistanceMetric,
		&out.Normalization, &out.ContractHash, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryVectorProfile{}, ErrMemoryNotFound
	}
	if err != nil {
		return MemoryVectorProfile{}, err
	}
	return out, nil
}

func memoryVectorSimilarity(profile MemoryVectorProfile, left, right []float64) (float64, error) {
	if len(left) != profile.Dimensions || len(right) != profile.Dimensions {
		return 0, fmt.Errorf("%w: incompatible vector dimensions", ErrMemoryInputInvalid)
	}
	dot, leftNorm, rightNorm, distanceSquared := 0.0, 0.0, 0.0, 0.0
	for i := range left {
		dot += left[i] * right[i]
		leftNorm += left[i] * left[i]
		rightNorm += right[i] * right[i]
		delta := left[i] - right[i]
		distanceSquared += delta * delta
	}
	var similarity float64
	switch profile.DistanceMetric {
	case MemoryVectorMetricCosine:
		if leftNorm == 0 || rightNorm == 0 {
			return 0, fmt.Errorf("%w: cosine vector is zero", ErrMemoryInputInvalid)
		}
		similarity = (dot/math.Sqrt(leftNorm*rightNorm) + 1) / 2
	case MemoryVectorMetricDot:
		similarity = (dot + 1) / 2
	case MemoryVectorMetricEuclidean:
		similarity = 1 / (1 + math.Sqrt(distanceSquared))
	default:
		return 0, fmt.Errorf("%w: unknown vector metric", ErrMemoryInputInvalid)
	}
	if similarity < 0 {
		return 0, nil
	}
	if similarity > 1 {
		return 1, nil
	}
	return similarity, nil
}
