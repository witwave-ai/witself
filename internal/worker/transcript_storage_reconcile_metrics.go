package worker

import (
	"fmt"
	"io"
)

// TranscriptStorageReconcileCounts contains bounded outcomes without identifiers.
type TranscriptStorageReconcileCounts struct {
	Corrected, Confirmed, Raced, Busy, Anomaly, Timeout, Error, Skipped int64
	Entries, Stuck                                                      int64
}

type transcriptStorageReconcileMetrics struct {
	units                     [8]uint64
	entries                   uint64
	eligible, deferred, stuck int64
	hasStatus                 bool
}

// ObserveTranscriptStorageReconcilePass accumulates unit outcomes for a registered job.
func (m *Metrics) ObserveTranscriptStorageReconcilePass(counts TranscriptStorageReconcileCounts) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs["transcript_storage_reconcile"] == nil {
		return
	}
	for i, count := range [...]int64{counts.Corrected, counts.Confirmed, counts.Raced, counts.Busy, counts.Anomaly, counts.Timeout, counts.Error, counts.Skipped} {
		if count > 0 {
			m.transcriptStorageReconcile.units[i] += uint64(count)
		}
	}
	if counts.Entries > 0 {
		m.transcriptStorageReconcile.entries += uint64(counts.Entries)
	}
	m.transcriptStorageReconcile.stuck = counts.Stuck
}

// ObserveTranscriptStorageReconcileStatus publishes a successful per-replica status read.
func (m *Metrics) ObserveTranscriptStorageReconcileStatus(eligible, deferred, stuck int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs["transcript_storage_reconcile"] == nil {
		return
	}
	m.transcriptStorageReconcile.eligible = eligible
	m.transcriptStorageReconcile.deferred = deferred
	m.transcriptStorageReconcile.stuck = stuck
	m.transcriptStorageReconcile.hasStatus = true
}

func (m *Metrics) writeTranscriptStorageReconcilePrometheus(w io.Writer) {
	m.mu.Lock()
	registered := m.jobs["transcript_storage_reconcile"] != nil
	state := m.transcriptStorageReconcile
	m.mu.Unlock()
	if !registered {
		return
	}
	_, _ = fmt.Fprintln(w, "# HELP witself_worker_transcript_storage_reconcile_units_total Transcript storage reconciliation units by bounded result.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_transcript_storage_reconcile_units_total counter")
	for i, label := range [...]string{"corrected", "confirmed", "raced", "busy", "anomaly", "timeout", "error", "skipped"} {
		_, _ = fmt.Fprintf(w, "witself_worker_transcript_storage_reconcile_units_total{result=%q} %d\n", label, state.units[i])
	}
	_, _ = fmt.Fprintln(w, "# HELP witself_worker_transcript_storage_reconcile_entries_total Entries measured by transcript storage reconciliation.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_transcript_storage_reconcile_entries_total counter")
	_, _ = fmt.Fprintf(w, "witself_worker_transcript_storage_reconcile_entries_total %d\n", state.entries)
	_, _ = fmt.Fprintln(w, "# HELP witself_worker_transcript_storage_reconcile_stuck_units Units with at least three consecutive timeouts in this runner.")
	_, _ = fmt.Fprintln(w, "# TYPE witself_worker_transcript_storage_reconcile_stuck_units gauge")
	_, _ = fmt.Fprintf(w, "witself_worker_transcript_storage_reconcile_stuck_units %d\n", state.stuck)
	if state.hasStatus {
		_, _ = fmt.Fprintln(w, "# HELP witself_worker_transcript_storage_unverified_shards Non-exact transcript storage shards from the last successful status read.")
		_, _ = fmt.Fprintln(w, "# TYPE witself_worker_transcript_storage_unverified_shards gauge")
		_, _ = fmt.Fprintf(w, "witself_worker_transcript_storage_unverified_shards{state=\"eligible\"} %d\n", state.eligible)
		_, _ = fmt.Fprintf(w, "witself_worker_transcript_storage_unverified_shards{state=\"deferred\"} %d\n", state.deferred)
	}
}
