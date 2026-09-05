package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
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

func TestUsageQueryWindowBoundaries(t *testing.T) {
	since := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		bucket string
		window time.Duration
	}{
		{"hour", UsageBucketHour, 90 * 24 * time.Hour},
		{"day", UsageBucketDay, 5 * 366 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, extra := range []time.Duration{0, time.Nanosecond} {
				_, err := normalizeUsageQuery(UsageQuery{Since: since, Until: since.Add(tc.window + extra), Bucket: tc.bucket})
				if extra == 0 && err != nil {
					t.Fatalf("exact maximum window: %v", err)
				}
				if extra != 0 && !errors.Is(err, ErrUsageInputInvalid) {
					t.Fatalf("oversize window = %v, want ErrUsageInputInvalid", err)
				}
			}
			_, err := normalizeUsageQuery(UsageQuery{Since: since, Until: since, Bucket: tc.bucket})
			if !errors.Is(err, ErrUsageInputInvalid) {
				t.Fatalf("empty window = %v, want ErrUsageInputInvalid", err)
			}
		})
	}
}

func TestUsageDimensionVocabularyIsClosed(t *testing.T) {
	want := []string{
		"email_address", "email_received", "email_sent", "email_storage_byte",
		"encrypted_storage_byte", "fact_returned", "message_delivered", "message_sent",
		"runtime_injection", "secret_read", "stored_secret", "totp_code",
		"transcript_created", "transcript_entry_read", "transcript_entry_write", "transcript_storage_byte",
	}
	query, err := normalizeUsageQuery(UsageQuery{})
	if err != nil || !reflect.DeepEqual(query.Dimensions, want) {
		t.Fatalf("default dimension vocabulary = %#v / %v", query.Dimensions, err)
	}
	for _, dimension := range want {
		query, err := normalizeUsageQuery(UsageQuery{Dimensions: []string{dimension, dimension}})
		if err != nil || len(query.Dimensions) != 1 || query.Dimensions[0] != dimension {
			t.Fatalf("accepted dimension %s = %#v / %v", dimension, query.Dimensions, err)
		}
	}
	for _, dimension := range []string{"unknown_dimension", "Transcript_created", "bad.dimension", ""} {
		_, err := normalizeUsageQuery(UsageQuery{Dimensions: []string{dimension}})
		if !errors.Is(err, ErrUsageInputInvalid) {
			t.Fatalf("unknown dimension = %v, want ErrUsageInputInvalid", err)
		}
		if dimension != "" && strings.Contains(err.Error(), dimension) {
			t.Fatalf("query error echoed untrusted dimension: %v", err)
		}
		in := usageEventInput{
			AccountID: "acc_1", RealmID: "rlm_1", AgentID: "agt_1", Dimension: dimension,
			Unit: UsageUnitEntry, SubjectType: "transcript", SubjectID: "trn_1", IdempotencyKey: "write:1", Quantity: 1,
		}
		if err := validateUsageEventInput(&in); err == nil {
			t.Fatal("unknown event dimension was accepted")
		}
	}
	// Duplicated filters never increase the SQL dimension-array cardinality.
	query, err = normalizeUsageQuery(UsageQuery{Dimensions: strings.Split(strings.Repeat("message_sent,", 10000)+"message_sent", ",")})
	if err != nil || !reflect.DeepEqual(query.Dimensions, []string{"message_sent"}) {
		t.Fatalf("high-cardinality filter = %#v / %v", query.Dimensions, err)
	}
}

func TestImportedUsagePreservesHistoricalDimensionSyntax(t *testing.T) {
	tests := []struct {
		name      string
		dimension any
		valid     bool
	}{
		{name: "legacy custom", dimension: "legacy_custom", valid: true},
		{name: "single letter", dimension: "a", valid: true},
		{name: "maximum length", dimension: "a" + strings.Repeat("_", 63), valid: true},
		{name: "digits and underscores", dimension: "a_09", valid: true},
		{name: "too long", dimension: strings.Repeat("a", 65)},
		{name: "uppercase", dimension: "Legacy_custom"},
		{name: "leading digit", dimension: "1legacy"},
		{name: "hyphen", dimension: "legacy-custom"},
		{name: "newline", dimension: "legacy_custom\n"},
		{name: "non ascii", dimension: "légacy_custom"},
		{name: "empty", dimension: ""},
		{name: "null", dimension: nil},
		{name: "number", dimension: float64(1)},
	}
	for _, table := range []string{"usage_events", "usage_rollups"} {
		for _, test := range tests {
			t.Run(table+"/"+test.name, func(t *testing.T) {
				ic := newImportCtx("acc_1")
				ic.realms["rlm_1"] = true
				ic.agents["agt_1"] = true
				ic.agentRealms["agt_1"] = "rlm_1"
				row := map[string]any{
					"account_id": "acc_1", "realm_id": "rlm_1", "agent_id": "agt_1",
					"dimension": test.dimension, "unit": "entry",
				}
				err := ic.validateAndRecord(table, row)
				if test.valid {
					if err != nil {
						t.Fatalf("historically valid imported dimension rejected: %v", err)
					}
					return
				}
				if !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), "invalid usage dimension") {
					t.Fatalf("malformed imported dimension = %v, want ErrArchiveContent", err)
				}
				if dimension, ok := test.dimension.(string); ok && dimension != "" && strings.Contains(err.Error(), dimension) {
					t.Fatalf("archive error echoed untrusted dimension: %v", err)
				}
			})
		}
	}
}
