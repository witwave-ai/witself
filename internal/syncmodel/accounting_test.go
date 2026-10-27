package syncmodel

import (
	"testing"
	"time"
)

func TestLogicalAccountingReplayAndRebuild(t *testing.T) {
	var ledger LogicalLedger
	a := LogicalAction{AccountID: "account", Operation: OperationID{1}, ActionOrdinal: 0, Actor: "actor", OccurredAt: time.Date(2026, 1, 1, 23, 30, 0, 0, time.FixedZone("fixture", 3600)), Class: "fixture_mutation", Dimension: "actions", Quantity: 3}
	if added, err := ledger.Record(a); err != nil || !added {
		t.Fatal("original action not recorded")
	}
	for i, event := range []string{"snapshot", "replay", "duplicate", "lost_ack", "regroup"} {
		if added, err := ledger.Record(a); err != nil || added {
			t.Fatal("replay counted original action twice")
		}
		if err := ledger.RecordReplay("account", "node", event); err != nil {
			t.Fatal(err)
		}
		ledger.Rebuild()
		want := AccountingCounts{OriginalEvents: 1, HourCount: 1, DayCount: 1, ReplayWork: uint64(i + 1)}
		for range 2 {
			if ledger.Counts("account") != want {
				t.Fatal("replay, rebuild or passive read changed logical count")
			}
		}
	}
	read := a
	read.Operation = OperationID{2}
	read.Class = "fixture_audited_read"
	if _, err := ledger.Record(read); err != nil {
		t.Fatal(err)
	}
	compound := a
	compound.Operation = OperationID{3}
	compound.Class = "fixture_compound"
	if _, err := ledger.Record(compound); err != nil {
		t.Fatal(err)
	}
	want := AccountingCounts{OriginalEvents: 3, HourCount: 3, DayCount: 3, ReplayWork: 5}
	if ledger.Counts("account") != want {
		t.Fatal("fresh audited read or compound missing own action")
	}
	key := RollupKey{AccountID: "account", Actor: "actor", Dimension: "actions", Bucket: "hour", Start: time.Date(2026, 1, 1, 22, 0, 0, 0, time.UTC)}
	if got := ledger.Aggregate(key); got.Count != 3 || got.Quantity != 9 {
		t.Fatal("original UTC time, actor or quantity lost")
	}
	ledger.Rebuild()
	if ledger.Counts("account") != want {
		t.Fatal("rebuild generated a source event")
	}
}

func TestLogicalAccountingIdentityProvenanceAndScope(t *testing.T) {
	var ledger LogicalLedger
	a := LogicalAction{AccountID: "account", Operation: OperationID{1}, ActionOrdinal: 0, Actor: "actor", OccurredAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Class: "fixture_mutation", Dimension: "actions", Quantity: 1}
	if _, err := ledger.Record(a); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*LogicalAction){
		func(a *LogicalAction) { a.Actor = "other" },
		func(a *LogicalAction) { a.OccurredAt = a.OccurredAt.Add(time.Hour) },
		func(a *LogicalAction) { a.Class = "fixture_audited_read" },
		func(a *LogicalAction) { a.Quantity++ },
	} {
		changed := a
		mutate(&changed)
		if _, err := ledger.Record(changed); err == nil {
			t.Fatal("changed action provenance accepted")
		}
	}
	b := a
	b.ActionOrdinal++
	if added, err := ledger.Record(b); err != nil || !added {
		t.Fatal("second action in operation not counted")
	}
	b = a
	b.AccountID = "other-account"
	if added, err := ledger.Record(b); err != nil || !added {
		t.Fatal("action identity crossed accounts")
	}
	if ledger.Counts("account").OriginalEvents != 2 || ledger.Counts("other-account").OriginalEvents != 1 {
		t.Fatal("account ledger isolation failed")
	}
	b.Operation = OperationID{4}
	b.Class = "unknown"
	if _, err := ledger.Record(b); err == nil {
		t.Fatal("uncataloged action class accepted")
	}
}
