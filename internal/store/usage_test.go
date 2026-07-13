package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestNormalizeUsageQuery(t *testing.T) {
	until := time.Date(2026, 7, 12, 18, 37, 0, 0, time.UTC)
	query, err := normalizeUsageQuery(UsageQuery{
		Since:      until.Add(-48 * time.Hour),
		Until:      until,
		Bucket:     UsageBucketDay,
		Dimensions: []string{"transcript_entry_write", "transcript_created", "transcript_entry_write"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSince := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	if !query.Since.Equal(wantSince) || !query.Until.Equal(until) {
		t.Fatalf("window = %s..%s", query.Since, query.Until)
	}
	if len(query.Dimensions) != 2 || query.Dimensions[0] != "transcript_created" || query.Dimensions[1] != "transcript_entry_write" {
		t.Fatalf("dimensions = %#v", query.Dimensions)
	}

	_, err = normalizeUsageQuery(UsageQuery{
		Since: until.Add(-91 * 24 * time.Hour), Until: until, Bucket: UsageBucketHour,
	})
	if !errors.Is(err, ErrUsageInputInvalid) {
		t.Fatalf("91-day hourly query = %v, want ErrUsageInputInvalid", err)
	}
	_, err = normalizeUsageQuery(UsageQuery{
		Since: until.Add(-time.Hour), Until: until, Bucket: UsageBucketDay,
		Dimensions: []string{"bad.dimension"},
	})
	if !errors.Is(err, ErrUsageInputInvalid) {
		t.Fatalf("bad dimension = %v, want ErrUsageInputInvalid", err)
	}
}

func TestValidateUsageEventInput(t *testing.T) {
	in := usageEventInput{
		AccountID: "acc_1", RealmID: "rlm_1", AgentID: "agt_1",
		Dimension: UsageDimensionTranscriptEntryWrite, Quantity: 2, Unit: UsageUnitEntry,
		SubjectType: "transcript", SubjectID: "trn_1", IdempotencyKey: "write:1",
	}
	if err := validateUsageEventInput(&in); err != nil {
		t.Fatal(err)
	}
	if string(in.Metadata) != `{}` || in.OccurredAt.IsZero() {
		t.Fatalf("defaults = metadata:%s occurred:%s", in.Metadata, in.OccurredAt)
	}

	in.Metadata = json.RawMessage(`[]`)
	if err := validateUsageEventInput(&in); err == nil {
		t.Fatal("array metadata was accepted")
	}
}

func TestUsageBatchKeyIsOrderIndependent(t *testing.T) {
	a := usageBatchKey([]string{"ent_2", "ent_1"})
	b := usageBatchKey([]string{"ent_1", "ent_2"})
	if a != b || len(a) != 64 {
		t.Fatalf("keys = %q / %q", a, b)
	}
}
