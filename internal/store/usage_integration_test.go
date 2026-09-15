package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestUsagePostgresRoundTrip is opt-in because it needs a disposable real
// Postgres database. It covers metering, retry idempotency, authorization,
// rollups, and account archive portability as one lifecycle.
func TestUsagePostgresRoundTrip(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx, "usage-test@witwave.ai", "usage test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "recorder")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}

	tr, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID, CreateTranscriptInput{
		ExternalID: "usage-round-trip", Title: "Usage test",
	})
	if err != nil {
		t.Fatal(err)
	}
	retried, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID, CreateTranscriptInput{
		ExternalID: "usage-round-trip", Title: "Usage test",
	})
	if err != nil || retried.ID != tr.ID {
		t.Fatalf("retry create = %#v / %v", retried, err)
	}
	inputs := []AppendTranscriptEntryInput{
		{ExternalID: "prompt-1", Role: TranscriptRoleUser, Body: "hello"},
		{ExternalID: "reply-1", Role: TranscriptRoleAssistant, Body: "hi", ReplyToExternalID: "prompt-1"},
	}
	if _, err := st.AppendTranscriptEntries(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, inputs); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendTranscriptEntries(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, inputs); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetTranscriptPageObservational(ctx, p, tr.ID, TranscriptPageOptions{Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetTranscriptPageObservational(ctx, p, tr.ID, TranscriptPageOptions{Limit: 1, Tail: true}); err != nil {
		t.Fatal(err)
	}
	var observationalReads, observationalReadRollups int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM usage_events
		WHERE account_id=$1 AND dimension=$2`, p.AccountID, UsageDimensionTranscriptEntryRead).Scan(&observationalReads); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM usage_rollups
		WHERE account_id=$1 AND dimension=$2`, p.AccountID, UsageDimensionTranscriptEntryRead).Scan(&observationalReadRollups); err != nil {
		t.Fatal(err)
	}
	if observationalReads != 0 || observationalReadRollups != 0 {
		t.Fatalf("observational transcript reads wrote %d usage events / %d rollups", observationalReads, observationalReadRollups)
	}
	if _, err := st.GetTranscriptPage(ctx, p, tr.ID, TranscriptPageOptions{Limit: 10}); err != nil {
		t.Fatal(err)
	}
	operator := Principal{Kind: PrincipalOperator, ID: "opr_ignored", AccountID: p.AccountID, AccountStatus: "active"}
	if _, err := st.GetTranscriptPage(ctx, operator, tr.ID, TranscriptPageOptions{Limit: 10}); err != nil {
		t.Fatal(err)
	}

	query := UsageQuery{Since: time.Now().Add(-24 * time.Hour), Until: time.Now().Add(time.Hour), Bucket: UsageBucketDay}
	before, err := st.GetAgentUsage(ctx, p, query)
	if err != nil {
		t.Fatal(err)
	}
	totals := usageTotalsByDimension(before.Totals)
	if totals[UsageDimensionTranscriptCreated].Quantity != 1 || totals[UsageDimensionTranscriptCreated].EventCount != 1 {
		t.Fatalf("create total = %+v", totals[UsageDimensionTranscriptCreated])
	}
	if totals[UsageDimensionTranscriptEntryWrite].Quantity != 2 || totals[UsageDimensionTranscriptEntryWrite].EventCount != 1 {
		t.Fatalf("write total = %+v", totals[UsageDimensionTranscriptEntryWrite])
	}
	if totals[UsageDimensionTranscriptEntryRead].Quantity != 2 || totals[UsageDimensionTranscriptEntryRead].EventCount != 1 {
		t.Fatalf("read total = %+v", totals[UsageDimensionTranscriptEntryRead])
	}
	if totals[UsageDimensionTranscriptStorage].Quantity <= 0 {
		t.Fatalf("storage total = %+v", totals[UsageDimensionTranscriptStorage])
	}
	filtered, err := st.GetAgentUsage(ctx, p, UsageQuery{
		Since: query.Since, Until: query.Until, Bucket: query.Bucket,
		Dimensions: []string{UsageDimensionTranscriptEntryWrite},
	})
	if err != nil || len(filtered.Totals) != 1 || filtered.Totals[0].Dimension != UsageDimensionTranscriptEntryWrite {
		t.Fatalf("filtered usage = %#v / %v", filtered.Totals, err)
	}
	if _, err := st.GetAgentUsage(ctx, operator, query); !errors.Is(err, ErrUsageForbidden) {
		t.Fatalf("operator usage = %v, want ErrUsageForbidden", err)
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE usage_rollups SET quantity = quantity + 1
		WHERE account_id = $1 AND bucket = 'day' AND dimension = $2`,
		p.AccountID, UsageDimensionTranscriptCreated); err != nil {
		t.Fatal(err)
	}
	if err := validateImportedUsageRollups(ctx, tx, p.AccountID); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("stale rollup validation = %v, want ErrArchiveContent", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	if err := st.SuspendAccountSystem(ctx, p.AccountID, "evacuation", "usage archive round trip"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, p.AccountID, "test-source", "test", &archive); err != nil {
		t.Fatal(err)
	}
	if err := deleteAccountForIntegrationTest(ctx, st, p.AccountID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"usage_events", "usage_rollups"} {
		t.Run("hostile_import_"+table, func(t *testing.T) {
			manifest, rows := readAvatarArchiveRows(t, archive.Bytes(), SchemaVersion())
			if len(rows[table]) == 0 {
				t.Fatal("usage archive fixture has no rows to corrupt")
			}
			var row map[string]json.RawMessage
			if err := json.Unmarshal(rows[table][0], &row); err != nil {
				t.Fatal(err)
			}
			row["dimension"] = json.RawMessage(`"malformed-dimension"`)
			rows[table][0], err = json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			hostile := writeAvatarArchiveRows(t, manifest, manifest.Tables, rows)
			if _, err := st.ImportAccount(ctx, p.AccountID, bytes.NewReader(hostile)); !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), "invalid usage dimension") {
				t.Fatalf("hostile archive import = %v, want invalid usage dimension / ErrArchiveContent", err)
			}
			var remaining int
			if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE id=$1`, p.AccountID).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if remaining != 0 {
				t.Fatal("hostile usage import did not roll back the account")
			}
		})
	}
	if _, err := st.ImportAccount(ctx, p.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	after, err := st.GetAgentUsage(ctx, p, query)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Points, after.Points) || !reflect.DeepEqual(before.Totals, after.Totals) {
		t.Fatalf("usage changed across archive\nbefore: %#v / %#v\nafter:  %#v / %#v", before.Points, before.Totals, after.Points, after.Totals)
	}
	t.Run("hostile_import_high_cardinality", func(t *testing.T) {
		manifest, rows := readAvatarArchiveRows(t, archive.Bytes(), SchemaVersion())
		// Keep a valid transcript subject and matching ledger projections while
		// inflating unit cardinality beyond the report limit. This exercises the
		// real archive boundary rather than only directly inserted legacy rows.
		var event map[string]any
		if err := json.Unmarshal(rows["usage_events"][0], &event); err != nil {
			t.Fatal(err)
		}
		occurredAt, err := time.Parse(time.RFC3339Nano, event["occurred_at"].(string))
		if err != nil {
			t.Fatal(err)
		}
		for n := 0; n <= UsageReportPointLimit; n++ {
			unit := fmt.Sprintf("unit_%06d", n)
			event["id"] = fmt.Sprintf("usg_cardinality_%06d", n)
			event["idempotency_key"] = fmt.Sprintf("cardinality:%d", n)
			event["dimension"] = UsageDimensionTranscriptCreated
			event["unit"] = unit
			event["quantity"] = 2
			raw, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			rows["usage_events"] = append(rows["usage_events"], raw)
			for _, bucket := range []string{UsageBucketHour, UsageBucketDay} {
				rollup := map[string]any{
					"account_id": p.AccountID, "realm_id": p.RealmID, "agent_id": p.ID,
					"dimension": UsageDimensionTranscriptCreated, "unit": unit, "bucket": bucket,
					"bucket_start": usageBucketStart(occurredAt, bucket), "quantity": 2, "event_count": 1,
					"updated_at": occurredAt,
				}
				raw, err := json.Marshal(rollup)
				if err != nil {
					t.Fatal(err)
				}
				rows["usage_rollups"] = append(rows["usage_rollups"], raw)
			}
		}
		hostile := writeAvatarArchiveRows(t, manifest, manifest.Tables, rows)
		if err := deleteAccountForIntegrationTest(ctx, st, p.AccountID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ImportAccount(ctx, p.AccountID, bytes.NewReader(hostile)); err != nil {
			t.Fatal(err)
		}
		if report, err := st.GetAgentUsage(ctx, p, query); !errors.Is(err, ErrUsageQueryTooLarge) || len(report.Points) != 0 || len(report.Totals) != 0 {
			t.Fatalf("unnegotiated imported usage = %d points / %d totals / %v", len(report.Points), len(report.Totals), err)
		}
		partialQuery := query
		partialQuery.AllowTruncation = true
		report, err := st.GetAgentUsage(ctx, p, partialQuery)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Truncated || len(report.Points) != UsageReportPointLimit || len(report.Totals) > UsageReportPointLimit {
			t.Fatalf("imported report points/totals/truncated = %d/%d/%v", len(report.Points), len(report.Totals), report.Truncated)
		}
		var quantity, count int64
		for _, point := range report.Points {
			quantity += point.Quantity
			count += point.EventCount
		}
		for _, total := range report.Totals {
			quantity -= total.Quantity
			count -= total.EventCount
		}
		if quantity != 0 || count != 0 {
			t.Fatal("imported report totals include omitted points")
		}
		for _, bucket := range []string{UsageBucketHour, UsageBucketDay} {
			_, err := st.GetAgentUsage(ctx, p, UsageQuery{
				Since: occurredAt.Add(-6 * 366 * 24 * time.Hour), Until: occurredAt, Bucket: bucket,
			})
			if !errors.Is(err, ErrUsageInputInvalid) {
				t.Fatalf("oversize %s window over imported usage = %v, want ErrUsageInputInvalid", bucket, err)
			}
		}
	})
}

func usageTotalsByDimension(totals []UsageTotal) map[string]UsageTotal {
	out := make(map[string]UsageTotal, len(totals))
	for _, total := range totals {
		out[total.Dimension] = total
	}
	return out
}

func TestUsagePostgresRowLimitAndHostileImportedCardinality(t *testing.T) {
	ctx, st, p := newUsageBoundsFixture(t)
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	query := UsageQuery{Since: since, Until: since.Add(time.Hour), Bucket: UsageBucketHour}
	empty, err := st.GetAgentUsage(ctx, p, query)
	if err != nil || empty.Truncated || len(empty.Points) != 0 || len(empty.Totals) != 0 {
		t.Fatalf("empty report = %#v / %v", empty, err)
	}
	// Archives historically accepted arbitrary syntactically valid dimensions
	// and units. Seed that stored shape directly, including an unknown dimension
	// that must never become part of today's response vocabulary.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO usage_rollups
		  (account_id, realm_id, agent_id, dimension, unit, bucket, bucket_start, quantity, event_count)
		SELECT $1, $2, $3, $4, 'unit_' || lpad(n::text, 6, '0'), 'hour', $5, 2, 3
		FROM generate_series(1, $6::int) n`,
		p.AccountID, p.RealmID, p.ID, UsageDimensionTranscriptCreated, since, UsageReportPointLimit); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO usage_rollups
		  (account_id, realm_id, agent_id, dimension, unit, bucket, bucket_start, quantity, event_count)
		VALUES ($1, $2, $3, 'aaa_unknown_import', 'transcript', 'hour', $4, 99, 99)`,
		p.AccountID, p.RealmID, p.ID, since); err != nil {
		t.Fatal(err)
	}
	exact, err := st.GetAgentUsage(ctx, p, query)
	if err != nil {
		t.Fatal(err)
	}
	assertUsageBoundsReport(t, exact, UsageReportPointLimit, UsageReportPointLimit, false)
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO usage_rollups
		  (account_id, realm_id, agent_id, dimension, unit, bucket, bucket_start, quantity, event_count)
		SELECT $1, $2, $3, $4, 'unit_' || lpad(n::text, 6, '0'), 'hour', $5, 999, 999
		FROM generate_series($6::int, $7::int) n`,
		p.AccountID, p.RealmID, p.ID, UsageDimensionTranscriptCreated, since,
		UsageReportPointLimit+1, UsageReportPointLimit+1); err != nil {
		t.Fatal(err)
	}
	refused, err := st.GetAgentUsage(ctx, p, query)
	if !errors.Is(err, ErrUsageQueryTooLarge) || len(refused.Points) != 0 || len(refused.Totals) != 0 {
		t.Fatalf("cap+1 without opt-in = %d points / %d totals / %v", len(refused.Points), len(refused.Totals), err)
	}
	query.AllowTruncation = true
	truncated, err := st.GetAgentUsage(ctx, p, query)
	if err != nil {
		t.Fatal(err)
	}
	assertUsageBoundsReport(t, truncated, UsageReportPointLimit, UsageReportPointLimit, true)
	if !reflect.DeepEqual(exact.Points, truncated.Points) || !reflect.DeepEqual(exact.Totals, truncated.Totals) {
		t.Fatal("truncated totals or points include rows beyond the deterministic limit")
	}
	query.Dimensions = []string{"aaa_unknown_import"}
	if _, err := st.GetAgentUsage(ctx, p, query); !errors.Is(err, ErrUsageInputInvalid) {
		t.Fatalf("unknown imported dimension query = %v, want ErrUsageInputInvalid", err)
	}
	query.Dimensions = nil
	query.Until = since.Add(90*24*time.Hour + time.Nanosecond)
	if _, err := st.GetAgentUsage(ctx, p, query); !errors.Is(err, ErrUsageInputInvalid) {
		t.Fatalf("oversize imported usage query = %v, want ErrUsageInputInvalid", err)
	}
}

func TestUsagePostgresHighCardinality(t *testing.T) {
	ctx, st, p := newUsageBoundsFixture(t)
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dimensions := []string{
		UsageDimensionTranscriptCreated, UsageDimensionTranscriptEntryWrite,
		UsageDimensionTranscriptEntryRead, UsageDimensionTranscriptStorage,
		UsageDimensionMessageSent, UsageDimensionMessageDelivered,
	}
	units := []string{UsageUnitTranscript, UsageUnitEntry, UsageUnitEntry, UsageUnitByte, UsageUnitMessage, UsageUnitDelivery}
	// Six ordinary dimensions across the maximum hourly window exceed the row
	// cap even with canonical units and correctly aligned UTC buckets.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO usage_rollups
		  (account_id, realm_id, agent_id, dimension, unit, bucket, bucket_start, quantity, event_count)
		SELECT $1, $2, $3, d.dimension, d.unit, 'hour', $4::timestamptz + n * interval '1 hour', 2, 3
		FROM unnest($5::text[], $6::text[]) d(dimension, unit)
		CROSS JOIN generate_series(0, 90 * 24 - 1) n`,
		p.AccountID, p.RealmID, p.ID, since, dimensions, units); err != nil {
		t.Fatal(err)
	}
	query := UsageQuery{Since: since, Until: since.Add(90 * 24 * time.Hour), Bucket: UsageBucketHour}
	if _, err := st.GetAgentUsage(ctx, p, query); !errors.Is(err, ErrUsageQueryTooLarge) {
		t.Fatalf("high cardinality without opt-in = %v, want ErrUsageQueryTooLarge", err)
	}
	query.AllowTruncation = true
	report, err := st.GetAgentUsage(ctx, p, query)
	if err != nil {
		t.Fatal(err)
	}
	assertUsageBoundsReport(t, report, UsageReportPointLimit, len(dimensions), true)
	query.Dimensions = []string{UsageDimensionTranscriptCreated}
	filtered, err := st.GetAgentUsage(ctx, p, query)
	if err != nil {
		t.Fatal(err)
	}
	assertUsageBoundsReport(t, filtered, 90*24, 1, false)
	if !filtered.Points[0].BucketStart.Equal(since) || !filtered.Points[len(filtered.Points)-1].BucketStart.Equal(query.Until.Add(-time.Hour)) {
		t.Fatal("hourly report does not preserve its inclusive-since, exclusive-until bounds")
	}
}

func assertUsageBoundsReport(t *testing.T, report UsageReport, points, totals int, truncated bool) {
	t.Helper()
	if len(report.Points) != points || len(report.Totals) != totals || report.Truncated != truncated {
		t.Fatalf("report points/totals/truncated = %d/%d/%v, want %d/%d/%v",
			len(report.Points), len(report.Totals), report.Truncated, points, totals, truncated)
	}
	var quantity, events int64
	for i, point := range report.Points {
		if !validUsageDimension(point.Dimension) {
			t.Fatal("report included an unknown imported dimension")
		}
		if i > 0 {
			previous := report.Points[i-1]
			if point.BucketStart.Before(previous.BucketStart) ||
				(point.BucketStart.Equal(previous.BucketStart) && point.Dimension+"\x00"+point.Unit <= previous.Dimension+"\x00"+previous.Unit) {
				t.Fatal("report points are not in deterministic bucket, dimension, unit order")
			}
		}
		quantity += point.Quantity
		events += point.EventCount
	}
	if quantity != int64(points)*2 || events != int64(points)*3 {
		t.Fatalf("point sums = %d/%d, want %d/%d", quantity, events, points*2, points*3)
	}
	for _, total := range report.Totals {
		quantity -= total.Quantity
		events -= total.EventCount
	}
	if quantity != 0 || events != 0 {
		t.Fatal("totals do not cover exactly the returned points")
	}
}

func newUsageBoundsFixture(t *testing.T) (context.Context, *Store, Principal) {
	t.Helper()
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx, fmt.Sprintf("usage-bounds-%d@witwave.ai", time.Now().UnixNano()), "usage bounds", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID); err != nil {
			t.Error(err)
		}
	})
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "usage-bounds")
	if err != nil {
		t.Fatal(err)
	}
	return ctx, st, Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID, RealmID: realm.ID, AccountStatus: "active"}
}
