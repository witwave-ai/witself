package worker

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestTranscriptStorageReconcileMetrics(t *testing.T) {
	r := NewRegistry()
	m := r.Metrics()
	var output bytes.Buffer
	m.ObserveTranscriptStorageReconcilePass(TranscriptStorageReconcileCounts{Corrected: 99})
	m.ObserveTranscriptStorageReconcileStatus(99, 99, 99)
	m.writePrometheus(&output)
	if strings.Contains(output.String(), "witself_worker_transcript_storage_") {
		t.Fatal("disabled job exposed metric families")
	}
	if err := r.Register(Job{Name: "transcript_storage_reconcile", Run: func(context.Context) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	m.writePrometheus(&output)
	for _, expected := range []string{
		`witself_worker_transcript_storage_reconcile_units_total{result="corrected"} 0`,
		`witself_worker_transcript_storage_reconcile_entries_total 0`,
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing registration sample %s", expected)
		}
	}
	if strings.Contains(output.String(), "witself_worker_transcript_storage_unverified_shards") {
		t.Fatal("status gauge appeared before successful status")
	}
	m.ObserveTranscriptStorageReconcilePass(TranscriptStorageReconcileCounts{Corrected: 1, Confirmed: 2, Raced: 3, Busy: 4, Anomaly: 5, Timeout: 6, Error: 7, Skipped: 8, Entries: 23, Stuck: 1})
	m.ObserveTranscriptStorageReconcileStatus(31, 16, 1)
	output.Reset()
	m.writePrometheus(&output)
	for _, expected := range []string{
		`witself_worker_transcript_storage_reconcile_units_total{result="corrected"} 1`,
		`witself_worker_transcript_storage_reconcile_units_total{result="confirmed"} 2`,
		`witself_worker_transcript_storage_reconcile_units_total{result="raced"} 3`,
		`witself_worker_transcript_storage_reconcile_units_total{result="busy"} 4`,
		`witself_worker_transcript_storage_reconcile_units_total{result="anomaly"} 5`,
		`witself_worker_transcript_storage_reconcile_units_total{result="timeout"} 6`,
		`witself_worker_transcript_storage_reconcile_units_total{result="error"} 7`,
		`witself_worker_transcript_storage_reconcile_units_total{result="skipped"} 8`,
		`witself_worker_transcript_storage_reconcile_entries_total 23`,
		`witself_worker_transcript_storage_unverified_shards{state="eligible"} 31`,
		`witself_worker_transcript_storage_unverified_shards{state="deferred"} 16`,
		`witself_worker_transcript_storage_reconcile_stuck_units 1`,
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing sample %s", expected)
		}
	}
	m.ObserveTranscriptStorageReconcilePass(TranscriptStorageReconcileCounts{Confirmed: 1})
	output.Reset()
	m.writePrometheus(&output)
	if !strings.Contains(output.String(), "witself_worker_transcript_storage_reconcile_stuck_units 0") {
		t.Fatal("success did not clear stuck gauge")
	}
}
