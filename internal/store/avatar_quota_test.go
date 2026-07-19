package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

func TestPlanAvatarPayloadCompactionPrioritizesEligibilityAndPreservesProtectedPayloads(t *testing.T) {
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	at := func(offset time.Duration) *time.Time {
		value := base.Add(offset)
		return &value
	}
	candidates := []avatarPayloadCandidate{
		{version: 6, lineage: 2, bytes: 100, wasActivated: true, lastActivatedAt: at(2 * time.Hour)},
		{version: 2, lineage: 2, bytes: 100, rejected: true},
		{version: 8, lineage: 2, bytes: 100}, // proposed pointer
		{version: 1, lineage: 1, bytes: 100, wasActivated: true, lastActivatedAt: at(time.Hour)},
		{version: 5, lineage: 2, bytes: 100, wasActivated: true, lastActivatedAt: at(3 * time.Hour)},
		{version: 4, lineage: 2, bytes: 100, wasActivated: true, lastActivatedAt: at(time.Hour)},
		{version: 7, lineage: 2, bytes: 100, wasActivated: true, lastActivatedAt: at(4 * time.Hour)}, // active pointer
		{version: 3, lineage: 2, bytes: 100},
	}
	plan, err := planAvatarPayloadCompaction(candidates, nil, 2, 7, 8,
		1, 100, 5, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{1, 2, 3, 4}; !reflect.DeepEqual(plan.versions, want) {
		t.Fatalf("compaction order = %v, want %v", plan.versions, want)
	}
	if plan.count != 4 || plan.compactedPayloadBytes != 400 || plan.netReclaimedBytes != 400 ||
		plan.retainedCount != 5 || plan.retainedBytes != 500 {
		t.Fatalf("plan accounting = %#v", plan)
	}
	for _, protected := range []int64{5, 6, 7, 8} {
		for _, compacted := range plan.versions {
			if compacted == protected {
				t.Fatalf("protected version %d was compacted", protected)
			}
		}
	}
}

func TestPlanAvatarPayloadCompactionUsesByteQuotaAndStableTieBreaks(t *testing.T) {
	candidates := []avatarPayloadCandidate{
		{version: 3, lineage: 4, bytes: 60},
		{version: 2, lineage: 4, bytes: 70, rejected: true},
		{version: 1, lineage: 3, bytes: 80},
	}
	plan, err := planAvatarPayloadCompaction(candidates, nil, 4, 0, 0,
		1, 50, 10, 130)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{1, 2}; !reflect.DeepEqual(plan.versions, want) {
		t.Fatalf("byte-triggered order = %v, want %v", plan.versions, want)
	}
	if plan.retainedCount != 2 || plan.retainedBytes != 110 {
		t.Fatalf("byte-triggered accounting = %#v", plan)
	}
}

func TestPlanAvatarPayloadCompactionSupportsImmediateQuotaLowering(t *testing.T) {
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	at := func(offset time.Duration) *time.Time {
		value := base.Add(offset)
		return &value
	}
	candidates := []avatarPayloadCandidate{
		{version: 1, lineage: 1, bytes: 100},
		{version: 2, lineage: 2, bytes: 100, rejected: true},
		{version: 3, lineage: 2, bytes: 100, wasActivated: true, lastActivatedAt: at(time.Hour)},
		{version: 4, lineage: 2, bytes: 100, wasActivated: true, lastActivatedAt: at(2 * time.Hour)},
		{version: 5, lineage: 2, bytes: 100, wasActivated: true, lastActivatedAt: at(3 * time.Hour)},
	}
	plan, err := planAvatarPayloadCompaction(candidates, nil, 2, 5, 0,
		0, 0, 4, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{1}; !reflect.DeepEqual(plan.versions, want) {
		t.Fatalf("quota-lowering order = %v, want %v", plan.versions, want)
	}
}

func TestPlanAvatarPayloadCompactionFailsClosedWhenOnlyProtectedPayloadsRemain(t *testing.T) {
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	at := func(offset time.Duration) *time.Time {
		value := base.Add(offset)
		return &value
	}
	candidates := []avatarPayloadCandidate{
		{version: 1, lineage: 1, bytes: 100, wasActivated: true, lastActivatedAt: at(time.Hour)},
		{version: 2, lineage: 1, bytes: 100, wasActivated: true, lastActivatedAt: at(2 * time.Hour)},
		{version: 3, lineage: 1, bytes: 100, wasActivated: true, lastActivatedAt: at(3 * time.Hour)},
		{version: 4, lineage: 1, bytes: 100},
	}
	plan, err := planAvatarPayloadCompaction(candidates, nil, 1, 3, 4,
		1, 100, 4, 10_000)
	if !errors.Is(err, ErrAvatarPayloadQuotaExceeded) {
		t.Fatalf("error = %v, want ErrAvatarPayloadQuotaExceeded", err)
	}
	if len(plan.versions) != 0 || plan.count != 0 || plan.compactedPayloadBytes != 0 {
		t.Fatalf("failed plan exposed partial compaction: %#v", plan)
	}
}

func TestPlanAvatarPayloadCompactionNoOpInsideQuota(t *testing.T) {
	plan, err := planAvatarPayloadCompaction([]avatarPayloadCandidate{
		{version: 1, lineage: 1, bytes: 100},
	}, nil, 1, 0, 0, 1, 50, 4, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.versions) != 0 || plan.retainedCount != 2 || plan.retainedBytes != 150 {
		t.Fatalf("no-op plan = %#v", plan)
	}
}

func TestPlanAvatarPayloadCompactionSkipsGrowingBoundaryUntilOffset(t *testing.T) {
	candidates := []avatarPayloadCandidate{
		{version: 1, lineage: 1, bytes: 1_500},
		{version: 2, lineage: 1, bytes: 1_500, qualifyingParentVersion: 1},
		{version: 3, lineage: 1, bytes: 50_000},
	}
	plan, err := planAvatarPayloadCompaction(candidates, nil, 2, 2, 0,
		0, 0, 2, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{3}; !reflect.DeepEqual(plan.versions, want) {
		t.Fatalf("single cleanup plan = %v, want safe alternative %v", plan.versions, want)
	}
	if plan.retainedBytes != 3_000 || plan.netReclaimedBytes != 50_000 {
		t.Fatalf("single cleanup accounting = %#v", plan)
	}

	plan, err = planAvatarPayloadCompaction(candidates, nil, 2, 2, 0,
		0, 0, 1, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{3, 1}; !reflect.DeepEqual(plan.versions, want) {
		t.Fatalf("offset cleanup plan = %v, want %v", plan.versions, want)
	}
	wantBytes := int64(1_500 + avatardomain.PerceptualContinuityFingerprintBytes)
	if plan.retainedBytes != wantBytes || plan.netReclaimedBytes != 53_000-wantBytes {
		t.Fatalf("offset cleanup accounting = %#v, want retained %d", plan, wantBytes)
	}
}

func TestPlanAvatarPayloadCompactionRefusesUnfundedGrowingBoundary(t *testing.T) {
	plan, err := planAvatarPayloadCompaction([]avatarPayloadCandidate{
		{version: 1, lineage: 1, bytes: 1_500},
		{version: 2, lineage: 1, bytes: 1_500, qualifyingParentVersion: 1},
	}, nil, 1, 2, 0, 0, 0, 1, 1_000_000)
	if !errors.Is(err, ErrAvatarPayloadQuotaExceeded) {
		t.Fatalf("error = %v, want unfunded-boundary quota refusal", err)
	}
	if len(plan.versions) != 0 || plan.retainedBytes != 0 {
		t.Fatalf("failed growing-boundary plan exposed partial cleanup: %#v", plan)
	}
}

func TestPlanAvatarPayloadCompactionPrunesLastChildFingerprint(t *testing.T) {
	plan, err := planAvatarPayloadCompaction([]avatarPayloadCandidate{
		{version: 2, lineage: 1, bytes: 1_500, qualifyingParentVersion: 1},
	}, map[int64]int64{1: avatardomain.PerceptualContinuityFingerprintBytes},
		2, 0, 0, 0, 0, 0, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	wantReclaimed := int64(1_500 + avatardomain.PerceptualContinuityFingerprintBytes)
	if plan.retainedBytes != 0 || plan.netReclaimedBytes != wantReclaimed {
		t.Fatalf("last-child cleanup accounting = %#v, want reclaimed %d", plan, wantReclaimed)
	}
}

func TestPlanAvatarPayloadCompactionPrunesObsoleteFingerprintWithoutPayloadCleanup(t *testing.T) {
	const payloadBytes = int64(50_000)
	plan, err := planAvatarPayloadCompaction([]avatarPayloadCandidate{
		{version: 2, lineage: 1, bytes: payloadBytes, wasActivated: true},
	}, map[int64]int64{1: avatardomain.PerceptualContinuityFingerprintBytes},
		1, 2, 0, 0, 0, 1, payloadBytes)
	if err != nil {
		t.Fatal(err)
	}
	if plan.count != 0 || len(plan.versions) != 0 || plan.retainedCount != 1 ||
		plan.retainedBytes != payloadBytes ||
		plan.netReclaimedBytes != avatardomain.PerceptualContinuityFingerprintBytes {
		t.Fatalf("obsolete-only cleanup = %#v", plan)
	}
}

func TestPlanAvatarPayloadCompactionHandlesLargeLegacyHistory(t *testing.T) {
	const versions = 20_000
	candidates := make([]avatarPayloadCandidate, 0, versions)
	for version := int64(1); version <= versions; version++ {
		candidates = append(candidates, avatarPayloadCandidate{
			version: version, lineage: 1, bytes: 100,
		})
	}
	plan, err := planAvatarPayloadCompaction(candidates, nil, 2, 0, 0,
		0, 0, 1_000, 10_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if plan.count != versions-1_000 || len(plan.versions) != versions-1_000 ||
		plan.versions[0] != 1 || plan.versions[len(plan.versions)-1] != versions-1_000 {
		t.Fatalf("large legacy cleanup = count:%d first:%d last:%d",
			plan.count, plan.versions[0], plan.versions[len(plan.versions)-1])
	}
	if plan.retainedCount != 1_000 || plan.retainedBytes != 100_000 {
		t.Fatalf("large legacy accounting = %#v", plan)
	}
}
