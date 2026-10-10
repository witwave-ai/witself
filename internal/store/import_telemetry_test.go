package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/memlimit"
)

func TestImportRowWrapLogsBeforeFailingRow(t *testing.T) {
	var output bytes.Buffer
	samples := 0
	logger := newImportTelemetryLogger(&output, "test", func() memlimit.Snapshot {
		samples++
		return memlimit.Snapshot{GoTotal: 123, RSS: 456}
	}, 1024, 1024)
	tracker := logger.Begin(ImportInfo{Purpose: "import", AccountID: "acc_test"})
	tracker.EntryStart("transcript_entries", 7, 2049)
	wantErr := errors.New("private row error")
	row := []byte(strings.Repeat("private row content", 128))
	wrapped := wrapImportRow(tracker, func(table string, got []byte) error {
		if table != "transcript_entries" || !bytes.Equal(got, row) {
			t.Fatal("wrapper changed row input")
		}
		if !strings.Contains(output.String(), "account import large row") {
			t.Fatal("large-row line missing before inner callback")
		}
		return wantErr
	})
	if err := wrapped("transcript_entries", row); err != wantErr {
		t.Fatalf("wrapper did not preserve error: %v", err)
	}
	if samples != 1 {
		t.Fatalf("failed row sampled %d times, want only pre-row sample", samples)
	}
	if !strings.Contains(output.String(), fmt.Sprintf("table=\"transcript_entries\" chunk=7 row_bytes=%d", len(row))) {
		t.Fatalf("wrong large-row coordinates: %s", output.String())
	}
	if strings.Contains(output.String(), "private") {
		t.Fatal("row content or error escaped into telemetry")
	}
}

type importRowRecordingTracker struct {
	noopImportTracker
	events []string
}

func (r *importRowRecordingTracker) Row(table string, rowBytes int) {
	r.events = append(r.events, fmt.Sprintf("before:%s:%d", table, rowBytes))
}

func (r *importRowRecordingTracker) RowDone(table string, rowBytes int) {
	r.events = append(r.events, fmt.Sprintf("after:%s:%d", table, rowBytes))
}

func TestImportRowWrapSanitizesOnlyObserverTable(t *testing.T) {
	tracker := &importRowRecordingTracker{}
	wrapped := wrapImportRow(tracker, func(table string, row []byte) error {
		if table != "private-table" || string(row) != "row" {
			t.Fatal("wrapper changed validation inputs")
		}
		tracker.events = append(tracker.events, "inner")
		return nil
	})
	if err := wrapped("private-table", []byte("row")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tracker.events, ","); got != "before:unknown:3,inner,after:unknown:3" {
		t.Fatalf("events = %s", got)
	}
}

func TestImportTelemetryLoggerEntryAndImportMaxima(t *testing.T) {
	var output bytes.Buffer
	samples := []memlimit.Snapshot{
		{GoLive: 10, GoTotal: 30, Anon: 50, RSS: 100}, // before row
		{GoLive: 20, GoTotal: 70, Anon: 40, RSS: 80},  // after row
		{GoLive: 15, GoTotal: 60, Anon: 60, RSS: 90, GoGoal: 25, GoLimit: 200, File: 3, RSSHWM: 110},
		{GoLive: 12, GoTotal: 40, Anon: -1, RSS: 75, GoGoal: 24, GoLimit: 200, File: 4, RSSHWM: 110},
		{GoTotal: 999, Anon: 999, RSS: 999, RSSHWM: 111, MemoryPeak: 222, File: 5, GoLimit: 200}, // final only
	}
	n := 0
	logger := newImportTelemetryLogger(&output, "test", func() memlimit.Snapshot {
		if n == len(samples) {
			t.Fatal("unexpected extra sample")
		}
		s := samples[n]
		n++
		return s
	}, 1024, 1024)
	tracker := logger.Begin(ImportInfo{Purpose: "backup_validation", AccountID: "acc_test", BackupID: "backup_test"})
	tracker.EntryStart("transcript_entries", 2, 4097)
	tracker.Row("transcript_entries", 2048)
	tracker.RowDone("transcript_entries", 2048)
	tracker.Entry(export.EntryStats{Table: "transcript_entries", Chunk: 2, EntryBytes: 4097, Rows: 2, LargestRowBytes: 2048})
	tracker.EntryStart("memories", 1, 501)
	tracker.Row("memories", 500)
	tracker.RowDone("memories", 500)
	tracker.Entry(export.EntryStats{Table: "memories", Chunk: 1, EntryBytes: 501, Rows: 1, LargestRowBytes: 500})
	tracker.Finish("ok")
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 || n != len(samples) {
		t.Fatalf("lines=%d samples=%d", len(lines), n)
	}
	for _, want := range []string{
		`go_live=15 go_goal=25 go_total=70 go_limit=200 anon=60 file=3 rss=100 rss_hwm=110`,
	} {
		if !strings.Contains(lines[1], want) {
			t.Fatalf("entry maxima missing %q: %s", want, lines[1])
		}
	}
	if !strings.Contains(lines[2], "go_total=40 go_limit=200 anon=-1 file=4 rss=75") {
		t.Fatalf("entry maxima did not reset: %s", lines[2])
	}
	for _, want := range []string{
		`purpose="backup_validation" account_id="acc_test" backup_id="backup_test" outcome="ok" entries=2 rows=3`,
		`largest_row_bytes=2048 largest_row_table="transcript_entries"`,
		`max_go_total=70 max_go_total_table="transcript_entries" max_go_total_chunk=2`,
		`max_anon=60 max_anon_table="transcript_entries" max_anon_chunk=2`,
		`max_go_live=20 max_rss=100 rss_hwm=111 memory_peak=222 file=5 go_limit=200 duration=`,
	} {
		if !strings.Contains(lines[3], want) {
			t.Fatalf("summary missing %q: %s", want, lines[3])
		}
	}
}

func TestImportTelemetryLoggerEmptyAndUnknown(t *testing.T) {
	var output bytes.Buffer
	logger := NewImportTelemetryLogger(&output, "test", func() memlimit.Snapshot { return memlimit.Snapshot{} })
	tracker := logger.Begin(ImportInfo{Purpose: "evacuation_import", AccountID: "acc_test", EvacuationID: "evac_test"})
	tracker.Finish("error")
	for _, want := range []string{
		`evacuation_id="evac_test" outcome="error" entries=0 rows=0 largest_row_bytes=0 largest_row_table="none"`,
		`max_go_total=-1 max_go_total_table="none" max_go_total_chunk=-1`,
		`max_anon=-1 max_anon_table="none" max_anon_chunk=-1 max_go_live=-1 max_rss=-1`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("empty summary missing %q: %s", want, output.String())
		}
	}
	output.Reset()
	tracker = logger.Begin(ImportInfo{Purpose: "import", AccountID: "acc_test"})
	tracker.EntryStart("private", 1, 2)
	tracker.Entry(export.EntryStats{Table: "private", Chunk: 1, EntryBytes: 2, Rows: 1, LargestRowBytes: 1})
	tracker.Finish("ok")
	if strings.Contains(output.String(), "private") || !strings.Contains(output.String(), `table="unknown"`) {
		t.Fatal("unknown table escaped telemetry allowlist")
	}
}

func TestImportTelemetryLoggerConcurrentTrackers(t *testing.T) {
	var output bytes.Buffer
	logger := NewImportTelemetryLogger(&output, "test", func() memlimit.Snapshot { return memlimit.Snapshot{} })
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker := logger.Begin(ImportInfo{Purpose: "import", AccountID: fmt.Sprintf("acc_%d", i)})
			for chunk := 1; chunk <= i+1; chunk++ {
				tracker.EntryStart("accounts", chunk, 2)
				tracker.Entry(export.EntryStats{Table: "accounts", Chunk: chunk, EntryBytes: 2, Rows: 1, LargestRowBytes: 1})
			}
			tracker.Finish("ok")
		}()
	}
	wg.Wait()
	for i := range 8 {
		want := fmt.Sprintf(`account_id="acc_%d" outcome="ok" entries=%d rows=%d`, i, i+1, i+1)
		if strings.Count(output.String(), want) != 1 {
			t.Fatalf("missing independent summary %q", want)
		}
	}
}

func TestImportTelemetryLoggerOutcomeClasses(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "ok"},
		{context.Canceled, "canceled"},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "canceled"},
		{errors.New("private error text"), "error"},
	} {
		if got := importOutcome(tc.err); got != tc.want {
			t.Fatalf("outcome=%q, want %q", got, tc.want)
		}
	}
}

type importTelemetryFailedBeginner struct{ err error }

func (b importTelemetryFailedBeginner) Begin(context.Context) (pgx.Tx, error) {
	return nil, b.err
}

func TestImportTelemetryLoggerBeforeTransactionFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options accountImportOptions
		err     error
		want    string
	}{
		{"plain", accountImportOptions{}, errors.New("private database error"), `purpose="import" account_id="acc_expected" outcome="error"`},
		{"backup", accountImportOptions{backupValidation: true, backupID: "backup_expected"}, context.Canceled, `purpose="backup_validation" account_id="acc_expected" backup_id="backup_expected" outcome="canceled"`},
		{"evacuation", accountImportOptions{evacuationID: "evac_expected"}, context.DeadlineExceeded, `purpose="evacuation_import" account_id="acc_expected" evacuation_id="evac_expected" outcome="canceled"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			st := &Store{importObserver: NewImportTelemetryLogger(&output, "test", func() memlimit.Snapshot { return memlimit.Snapshot{} })}
			_, _, err := st.importAccount(context.Background(), importTelemetryFailedBeginner{tc.err}, "acc_expected", tc.options, nil)
			if err != tc.err {
				t.Fatal("observer changed transaction error")
			}
			if strings.Count(output.String(), "\n") != 1 || !strings.Contains(output.String(), tc.want+" entries=0") || strings.Contains(output.String(), "private") {
				t.Fatal("first transaction failure did not produce one value-free summary")
			}
		})
	}
}

type importTelemetryFinishTracker struct {
	noopImportTracker
	outcomes []string
}

func (r *importTelemetryFinishTracker) Begin(ImportInfo) ImportTracker { return r }
func (r *importTelemetryFinishTracker) Finish(outcome string) {
	r.outcomes = append(r.outcomes, outcome)
}

type importTelemetryScan func(...any) error

func (scan importTelemetryScan) Scan(dest ...any) error { return scan(dest...) }

type importTelemetryPanicTx struct {
	pgx.Tx
	t          *testing.T
	now        time.Time
	panicValue any
	rolledBack bool
}

func (tx *importTelemetryPanicTx) Begin(context.Context) (pgx.Tx, error) { return tx, nil }
func (tx *importTelemetryPanicTx) Rollback(context.Context) error {
	tx.rolledBack = true
	return nil
}
func (tx *importTelemetryPanicTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return importTelemetryScan(func(dest ...any) error {
		switch {
		case sql == `SELECT clock_timestamp()`:
			*dest[0].(*time.Time) = tx.now
			return nil
		case strings.Contains(sql, "FROM accounts"):
			return pgx.ErrNoRows
		default:
			tx.t.Fatal("unexpected query before row callback")
			return nil
		}
	})
}
func (tx *importTelemetryPanicTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if !strings.HasPrefix(sql, "INSERT INTO accounts ") {
		tx.t.Fatal("unexpected statement before row insert")
	}
	panic(tx.panicValue)
}

func TestImportTelemetryRowPanicFinishesWithError(t *testing.T) {
	const accountID = "acc_panic"
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	tables := canonicalArchiveTableNamesForSchema(SchemaVersion())
	archive := writeAvatarArchiveRows(t, export.Manifest{
		SchemaVersion: SchemaVersion(), AccountID: accountID,
		Status: "suspended", ExportedAt: now.Add(-time.Minute),
	}, tables, map[string][][]byte{
		"accounts": {[]byte(`{"id":"acc_panic","status":"suspended"}`)},
	})
	tracker := &importTelemetryFinishTracker{}
	st := &Store{importObserver: tracker}
	wantPanic := errors.New("synthetic row callback panic")
	tx := &importTelemetryPanicTx{t: t, now: now, panicValue: wantPanic}
	func() {
		defer func() {
			if got := recover(); got != wantPanic {
				t.Fatal("row callback panic did not propagate unchanged")
			}
		}()
		_, _, _ = st.importAccount(context.Background(), tx, accountID, accountImportOptions{}, bytes.NewReader(archive))
		t.Fatal("panicking import returned normally")
	}()
	if len(tracker.outcomes) != 1 || tracker.outcomes[0] != "error" {
		t.Fatalf("panic finish outcomes = %v, want [error]", tracker.outcomes)
	}
	if !tx.rolledBack {
		t.Fatal("panicking import did not roll back its transaction")
	}
}
