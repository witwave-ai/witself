package store

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestMemoryVectorProfileAndValues(t *testing.T) {
	in := CreateMemoryVectorProfileInput{
		Provider: " local ", Model: "model-v1", Recipe: "content",
		RecipeVersion: "1", Dimensions: 2,
		DistanceMetric: MemoryVectorMetricCosine,
		Normalization:  MemoryVectorNormalizationL2,
	}
	normalized, contractHash, err := normalizeMemoryVectorProfileInput(in)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Provider != "local" || !isSHA256Hex(contractHash) {
		t.Fatalf("normalized profile/hash = %#v / %q", normalized, contractHash)
	}
	_, again, err := normalizeMemoryVectorProfileInput(normalized)
	if err != nil || again != contractHash {
		t.Fatalf("profile contract is not deterministic = %q / %v", again, err)
	}
	invalid := normalized
	invalid.DistanceMetric = MemoryVectorMetricDot
	invalid.Normalization = MemoryVectorNormalizationNone
	if _, _, err := normalizeMemoryVectorProfileInput(invalid); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("unnormalized dot profile error = %v", err)
	}
	invalid = normalized
	invalid.Model = "model-v1\nforged-output"
	if _, _, err := normalizeMemoryVectorProfileInput(invalid); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("control-character profile error = %v", err)
	}
	profile := MemoryVectorProfile{
		Dimensions: 2, DistanceMetric: MemoryVectorMetricCosine,
		Normalization: MemoryVectorNormalizationL2,
	}
	vector, vectorHash, err := normalizeMemoryVector([]float64{1, 0}, profile)
	if err != nil || !isSHA256Hex(vectorHash) {
		t.Fatalf("normalize vector = %#v / %q / %v", vector, vectorHash, err)
	}
	if _, _, err := normalizeMemoryVector([]float64{0.5, 0}, profile); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("non-unit vector error = %v", err)
	}
	if _, _, err := normalizeMemoryVector([]float64{math.NaN(), 0}, profile); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("non-finite vector error = %v", err)
	}
	similarity, err := memoryVectorSimilarity(profile, []float64{1, 0}, []float64{0, 1})
	if err != nil || math.Abs(similarity-0.5) > 1e-12 {
		t.Fatalf("orthogonal cosine similarity = %v / %v", similarity, err)
	}
}

func TestMemoryHybridCandidateBudgetIsExplicit(t *testing.T) {
	items := make([]hybridRankedMemory, maxMemoryHybridCandidates+1)
	bounded, truncated := applyMemoryHybridCandidateBudget(items)
	if !truncated || len(bounded) != maxMemoryHybridCandidates {
		t.Fatalf("bounded candidates = %d, truncated=%t", len(bounded), truncated)
	}
	bounded, truncated = applyMemoryHybridCandidateBudget(items[:maxMemoryHybridCandidates])
	if truncated || len(bounded) != maxMemoryHybridCandidates {
		t.Fatalf("at-limit candidates = %d, truncated=%t", len(bounded), truncated)
	}
}

func TestCorruptArchiveRejectsBackdatedMemoryVector(t *testing.T) {
	createdAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	owner := memoryOwnerImportKey{realmID: "realm_archive", ownerKind: "agent", ownerID: "agent_archive"}
	profile := MemoryVectorProfile{ID: "mvp_abcdefghijklmnop", Provider: "fixture",
		Model: "model", Recipe: "content", RecipeVersion: "1", Dimensions: 2,
		DistanceMetric: MemoryVectorMetricCosine, Normalization: MemoryVectorNormalizationL2,
		CreatedAt: createdAt}
	_, vectorHash, err := normalizeMemoryVector([]float64{1, 0}, profile)
	if err != nil {
		t.Fatal(err)
	}
	ic := newImportCtx("acc_archive")
	ic.exportedAt = createdAt.Add(time.Minute)
	ic.realms[owner.realmID] = true
	ic.agents[owner.ownerID] = true
	ic.agentRealms[owner.ownerID] = owner.realmID
	ic.memoryVectorProfiles[profile.ID] = memoryVectorProfileImportScope{owner: owner, profile: profile}
	ic.memoryVersions[memoryVersionImportKey{memoryID: "mem_abcdefghijklmnop", version: 1}] = memoryVersionImportScope{
		owner: owner, contentHash: strings.Repeat("a", 64), createdAt: createdAt,
	}
	row := map[string]any{
		"profile_id": profile.ID, "memory_id": "mem_abcdefghijklmnop",
		"memory_version": int64(1), "account_id": "acc_archive",
		"realm_id": owner.realmID, "owner_kind": owner.ownerKind,
		"owner_id": owner.ownerID, "content_hash": strings.Repeat("a", 64),
		"vector": []any{float64(1), float64(0)}, "vector_hash": vectorHash,
		"created_by_agent_id": owner.ownerID,
		"created_at":          createdAt.Add(-time.Second).Format(time.RFC3339Nano),
	}
	err = ic.validateAndRecord("memory_vectors", row)
	if !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), "created_at") {
		t.Fatalf("backdated vector archive error = %v", err)
	}
}

func TestCorruptArchiveRejectsVectorTimestampsAfterManifest(t *testing.T) {
	exportedAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	owner := memoryOwnerImportKey{realmID: "realm_archive", ownerKind: "agent", ownerID: "agent_archive"}
	profileInput := CreateMemoryVectorProfileInput{
		Provider: "fixture", Model: "model", Recipe: "content", RecipeVersion: "1",
		Dimensions: 2, DistanceMetric: MemoryVectorMetricCosine,
		Normalization: MemoryVectorNormalizationL2,
	}
	_, contractHash, err := normalizeMemoryVectorProfileInput(profileInput)
	if err != nil {
		t.Fatal(err)
	}
	newContext := func() *importCtx {
		ic := newImportCtx("acc_archive")
		ic.exportedAt = exportedAt
		ic.realms[owner.realmID] = true
		ic.agents[owner.ownerID] = true
		ic.agentRealms[owner.ownerID] = owner.realmID
		return ic
	}
	profileRow := func(createdAt time.Time) map[string]any {
		return map[string]any{
			"id": "mvp_abcdefghijklmnop", "account_id": "acc_archive",
			"realm_id": owner.realmID, "owner_kind": owner.ownerKind, "owner_id": owner.ownerID,
			"provider": profileInput.Provider, "model": profileInput.Model,
			"recipe": profileInput.Recipe, "recipe_version": profileInput.RecipeVersion,
			"dimensions": float64(profileInput.Dimensions), "distance_metric": profileInput.DistanceMetric,
			"normalization": profileInput.Normalization, "contract_hash": contractHash,
			"created_by_agent_id": owner.ownerID, "created_at": createdAt.Format(time.RFC3339Nano),
		}
	}

	ic := newContext()
	err = ic.validateAndRecord("memory_vector_profiles", profileRow(exportedAt.Add(time.Nanosecond)))
	if !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), "created_at") {
		t.Fatalf("future-dated vector profile error = %v", err)
	}

	ic = newContext()
	if err := ic.validateAndRecord("memory_vector_profiles", profileRow(exportedAt.Add(-time.Minute))); err != nil {
		t.Fatalf("valid vector profile: %v", err)
	}
	profile := ic.memoryVectorProfiles["mvp_abcdefghijklmnop"].profile
	_, vectorHash, err := normalizeMemoryVector([]float64{1, 0}, profile)
	if err != nil {
		t.Fatal(err)
	}
	memoryID := "mem_abcdefghijklmnop"
	ic.memoryVersions[memoryVersionImportKey{memoryID: memoryID, version: 1}] = memoryVersionImportScope{
		owner: owner, contentHash: strings.Repeat("a", 64), createdAt: exportedAt.Add(-time.Minute),
	}
	vectorRow := map[string]any{
		"profile_id": profile.ID, "memory_id": memoryID, "memory_version": float64(1),
		"account_id": "acc_archive", "realm_id": owner.realmID,
		"owner_kind": owner.ownerKind, "owner_id": owner.ownerID,
		"content_hash": strings.Repeat("a", 64), "vector": []any{float64(1), float64(0)},
		"vector_hash": vectorHash, "created_by_agent_id": owner.ownerID,
		"created_at": exportedAt.Add(time.Nanosecond).Format(time.RFC3339Nano),
	}
	err = ic.validateAndRecord("memory_vectors", vectorRow)
	if !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), "created_at") {
		t.Fatalf("future-dated vector error = %v", err)
	}
}

func TestArchiveManifestExportedAtValidation(t *testing.T) {
	importedAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		exportedAt time.Time
		wantErr    bool
	}{
		{name: "missing", wantErr: true},
		{name: "within clock skew", exportedAt: importedAt.Add(maxArchiveManifestFutureSkew), wantErr: false},
		{name: "materially future", exportedAt: importedAt.Add(maxArchiveManifestFutureSkew + time.Nanosecond), wantErr: true},
		{name: "past", exportedAt: importedAt.Add(-time.Hour), wantErr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArchiveExportedAt(tc.exportedAt, importedAt)
			if tc.wantErr != errors.Is(err, ErrArchiveContent) {
				t.Fatalf("validateArchiveExportedAt() error = %v, want ErrArchiveContent=%t", err, tc.wantErr)
			}
		})
	}
}
