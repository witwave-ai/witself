package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
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
}

func usageTotalsByDimension(totals []UsageTotal) map[string]UsageTotal {
	out := make(map[string]UsageTotal, len(totals))
	for _, total := range totals {
		out[total.Dimension] = total
	}
	return out
}
