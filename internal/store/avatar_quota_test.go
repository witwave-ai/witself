package store

import (
	"errors"
	"reflect"
	"testing"
	"time"
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
	plan, err := planAvatarPayloadCompaction(candidates, 2, 7, 8,
		1, 100, 5, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{1, 2, 3, 4}; !reflect.DeepEqual(plan.versions, want) {
		t.Fatalf("compaction order = %v, want %v", plan.versions, want)
	}
	if plan.count != 4 || plan.bytes != 400 || plan.retainedCount != 5 || plan.retainedBytes != 500 {
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
	plan, err := planAvatarPayloadCompaction(candidates, 4, 0, 0,
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
	plan, err := planAvatarPayloadCompaction(candidates, 2, 5, 0,
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
	plan, err := planAvatarPayloadCompaction(candidates, 1, 3, 4,
		1, 100, 4, 10_000)
	if !errors.Is(err, ErrAvatarPayloadQuotaExceeded) {
		t.Fatalf("error = %v, want ErrAvatarPayloadQuotaExceeded", err)
	}
	if len(plan.versions) != 0 || plan.count != 0 || plan.bytes != 0 {
		t.Fatalf("failed plan exposed partial compaction: %#v", plan)
	}
}

func TestPlanAvatarPayloadCompactionNoOpInsideQuota(t *testing.T) {
	plan, err := planAvatarPayloadCompaction([]avatarPayloadCandidate{
		{version: 1, lineage: 1, bytes: 100},
	}, 1, 0, 0, 1, 50, 4, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.versions) != 0 || plan.retainedCount != 2 || plan.retainedBytes != 150 {
		t.Fatalf("no-op plan = %#v", plan)
	}
}
