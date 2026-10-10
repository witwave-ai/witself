package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/memlimit"
)

// ImportInfo identifies a caller-authorized import, never an archive identity.
type ImportInfo struct {
	Purpose, AccountID, BackupID, EvacuationID string
}

// ImportObserver creates independent state for each import, including concurrent ones.
type ImportObserver interface {
	Begin(ImportInfo) ImportTracker
}

// ImportTracker receives only trusted identity, allowlisted table names, sizes,
// and outcome classes. No row content or error text crosses this boundary.
type ImportTracker interface {
	EntryStart(table string, chunk int, entryBytes int64)
	Row(table string, rowBytes int)
	RowDone(table string, rowBytes int)
	Entry(export.EntryStats)
	Finish(outcome string)
}

func (s *Store) beginImport(accountID string, options accountImportOptions) ImportTracker {
	if s.importObserver == nil {
		return noopImportTracker{}
	}
	purpose := "import"
	if options.backupValidation {
		purpose = "backup_validation"
	} else if options.evacuationID != "" {
		purpose = "evacuation_import"
	}
	tracker := s.importObserver.Begin(ImportInfo{
		Purpose: purpose, AccountID: accountID,
		BackupID: options.backupID, EvacuationID: options.evacuationID,
	})
	if tracker == nil {
		return noopImportTracker{}
	}
	return tracker
}

func importOutcome(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "canceled"
	}
	return "error"
}

func importTelemetryTable(table string) string {
	for _, known := range canonicalArchiveTables {
		if table == known.name {
			return table
		}
	}
	return "unknown"
}

// wrapImportRow samples before large allocations and after successful row work,
// preserving the original row bytes, table and error seen by the importer.
func wrapImportRow(tracker ImportTracker, inner func(string, []byte) error) func(string, []byte) error {
	return func(table string, row []byte) error {
		knownTable := importTelemetryTable(table)
		tracker.Row(knownTable, len(row))
		if err := inner(table, row); err != nil {
			return err
		}
		tracker.RowDone(knownTable, len(row))
		return nil
	}
}

type noopImportTracker struct{}

func (noopImportTracker) EntryStart(string, int, int64) {}
func (noopImportTracker) Row(string, int)               {}
func (noopImportTracker) RowDone(string, int)           {}
func (noopImportTracker) Entry(export.EntryStats)       {}
func (noopImportTracker) Finish(string)                 {}

type importTelemetryLogger struct {
	mu                     sync.Mutex
	w                      io.Writer
	prefix                 string
	sample                 func() memlimit.Snapshot
	sampleBytes, lineBytes int
}

// NewImportTelemetryLogger records memory at completed entries and large rows.
// Each line is written atomically under a logger-owned lock, so callers can
// share an arbitrary writer between concurrent imports.
func NewImportTelemetryLogger(w io.Writer, prefix string, sample func() memlimit.Snapshot) ImportObserver {
	return newImportTelemetryLogger(w, prefix, sample, 1<<20, 4<<20)
}

func newImportTelemetryLogger(w io.Writer, prefix string, sample func() memlimit.Snapshot, sampleBytes, lineBytes int) ImportObserver {
	if w == nil {
		w = io.Discard
	}
	if sample == nil {
		sample = memlimit.Sample
	}
	return &importTelemetryLogger{w: w, prefix: prefix, sample: sample, sampleBytes: sampleBytes, lineBytes: lineBytes}
}

type importMemoryMaximum struct {
	value int64
	table string
	chunk int
}

func emptyImportMaximum() importMemoryMaximum {
	return importMemoryMaximum{value: -1, table: "none", chunk: -1}
}

func (m *importMemoryMaximum) include(value int64, table string, chunk int) {
	if value > m.value {
		m.value, m.table, m.chunk = value, table, chunk
	}
}

type importTelemetryTracker struct {
	logger             *importTelemetryLogger
	info               ImportInfo
	started            time.Time
	chunk              int
	entryGo, entryAnon int64
	entryRSS           int64
	entries, rows      int
	largestRowBytes    int
	largestRowTable    string
	maxGo, maxAnon     importMemoryMaximum
	maxLive, maxRSS    int64
}

func (l *importTelemetryLogger) Begin(info ImportInfo) ImportTracker {
	return &importTelemetryTracker{
		logger: l, info: info, started: time.Now(), chunk: -1,
		entryGo: -1, entryAnon: -1, entryRSS: -1,
		largestRowTable: "none", maxGo: emptyImportMaximum(), maxAnon: emptyImportMaximum(),
		maxLive: -1, maxRSS: -1,
	}
}

func (l *importTelemetryLogger) write(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write([]byte(line))
}

func (t *importTelemetryTracker) identity() string {
	identity := fmt.Sprintf("purpose=%q account_id=%q", t.info.Purpose, t.info.AccountID)
	switch t.info.Purpose {
	case "backup_validation":
		identity += fmt.Sprintf(" backup_id=%q", t.info.BackupID)
	case "evacuation_import":
		identity += fmt.Sprintf(" evacuation_id=%q", t.info.EvacuationID)
	}
	return identity
}

func (t *importTelemetryTracker) EntryStart(_ string, chunk int, _ int64) {
	t.chunk = chunk
	t.entryGo, t.entryAnon, t.entryRSS = -1, -1, -1
}

func (t *importTelemetryTracker) takeSample(table string) memlimit.Snapshot {
	s := t.logger.sample()
	t.entryGo = max(t.entryGo, s.GoTotal)
	t.entryAnon = max(t.entryAnon, s.Anon)
	t.entryRSS = max(t.entryRSS, s.RSS)
	t.maxGo.include(s.GoTotal, table, t.chunk)
	t.maxAnon.include(s.Anon, table, t.chunk)
	t.maxLive = max(t.maxLive, s.GoLive)
	t.maxRSS = max(t.maxRSS, s.RSS)
	return s
}

func (t *importTelemetryTracker) Row(table string, rowBytes int) {
	if rowBytes < t.logger.lineBytes {
		return
	}
	table = importTelemetryTable(table)
	s := t.takeSample(table)
	t.logger.write(fmt.Sprintf("%s: account import large row %s table=%q chunk=%d row_bytes=%d go_live=%d go_total=%d go_limit=%d anon=%d rss=%d\n",
		t.logger.prefix, t.identity(), table, t.chunk, rowBytes, s.GoLive, s.GoTotal, s.GoLimit, s.Anon, s.RSS))
}

func (t *importTelemetryTracker) RowDone(table string, rowBytes int) {
	if rowBytes >= t.logger.sampleBytes {
		t.takeSample(importTelemetryTable(table))
	}
}

func (t *importTelemetryTracker) Entry(entry export.EntryStats) {
	entry.Table = importTelemetryTable(entry.Table)
	s := t.takeSample(entry.Table)
	t.entries++
	t.rows += entry.Rows
	if entry.LargestRowBytes > t.largestRowBytes {
		t.largestRowBytes, t.largestRowTable = entry.LargestRowBytes, entry.Table
	}
	t.logger.write(fmt.Sprintf("%s: account import entry %s table=%q chunk=%d entry_bytes=%d rows=%d largest_row_bytes=%d large_rows=%d go_live=%d go_goal=%d go_total=%d go_limit=%d anon=%d file=%d rss=%d rss_hwm=%d\n",
		t.logger.prefix, t.identity(), entry.Table, entry.Chunk, entry.EntryBytes, entry.Rows, entry.LargestRowBytes, entry.LargeRows,
		s.GoLive, s.GoGoal, t.entryGo, s.GoLimit, t.entryAnon, s.File, t.entryRSS, s.RSSHWM))
}

func (t *importTelemetryTracker) Finish(outcome string) {
	// Final resident/page-cache values are useful even before the first entry;
	// they must not manufacture entry/large-row maxima for an early refusal.
	s := t.logger.sample()
	t.logger.write(fmt.Sprintf("%s: account import memory %s outcome=%q entries=%d rows=%d largest_row_bytes=%d largest_row_table=%q max_go_total=%d max_go_total_table=%q max_go_total_chunk=%d max_anon=%d max_anon_table=%q max_anon_chunk=%d max_go_live=%d max_rss=%d rss_hwm=%d memory_peak=%d file=%d go_limit=%d duration=%s\n",
		t.logger.prefix, t.identity(), outcome, t.entries, t.rows, t.largestRowBytes, t.largestRowTable,
		t.maxGo.value, t.maxGo.table, t.maxGo.chunk, t.maxAnon.value, t.maxAnon.table, t.maxAnon.chunk,
		t.maxLive, t.maxRSS, s.RSSHWM, s.MemoryPeak, s.File, s.GoLimit, time.Since(t.started)))
}
