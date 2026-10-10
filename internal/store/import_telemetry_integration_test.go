package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/memlimit"
	"github.com/witwave-ai/witself/internal/testenv"
)

const importTelemetrySentinel = "S108SENTINEL-0123456789abcdef0123456789abcdef"

type importTelemetryFixture struct {
	info      ImportInfo
	archive   []byte
	checksums archiveexport.Checksums
}

func TestImportTelemetryEntryPointsPostgres(t *testing.T) {
	for _, entrypoint := range []string{"ValidateAccountBackup", "BackupValidationLease.Validate", "ImportAccountEvacuation", "AccountImportLease.Import", "ImportAccount"} {
		t.Run(entrypoint, func(t *testing.T) {
			var logs bytes.Buffer
			ctx, st := openImportTelemetryTestStore(t, &logs)
			purpose := "import"
			switch entrypoint {
			case "ValidateAccountBackup", "BackupValidationLease.Validate":
				purpose = "backup_validation"
			case "ImportAccountEvacuation", "AccountImportLease.Import":
				purpose = "evacuation_import"
			}
			fixture := seedImportTelemetryFixture(ctx, t, st, purpose)
			var err error
			switch entrypoint {
			case "ValidateAccountBackup":
				_, err = st.ValidateAccountBackup(ctx, fixture.info.AccountID, fixture.info.BackupID, bytes.NewReader(fixture.archive))
			case "BackupValidationLease.Validate":
				lease, acquireErr := st.AcquireBackupValidationLease(ctx, fixture.info.AccountID, fixture.info.BackupID)
				if acquireErr != nil {
					t.Fatal(acquireErr)
				}
				defer lease.Close()
				_, err = lease.Validate(ctx, bytes.NewReader(fixture.archive))
			case "ImportAccountEvacuation":
				_, _, err = st.ImportAccountEvacuation(ctx, fixture.info.AccountID, fixture.info.EvacuationID, bytes.NewReader(fixture.archive))
			case "AccountImportLease.Import":
				lease, acquireErr := st.AcquireAccountImportLease(ctx, fixture.info.AccountID, fixture.info.EvacuationID)
				if acquireErr != nil {
					t.Fatal(acquireErr)
				}
				defer lease.Close()
				_, _, err = lease.Import(ctx, bytes.NewReader(fixture.archive))
			case "ImportAccount":
				_, err = st.ImportAccount(ctx, fixture.info.AccountID, bytes.NewReader(fixture.archive))
			}
			if err != nil {
				t.Fatalf("entrypoint failed: %v", err)
			}
			assertImportTelemetryLines(t, logs.String(), fixture, "ok", true)
		})
	}
}

func TestImportTelemetryHostileArchivePostgres(t *testing.T) {
	var logs bytes.Buffer
	ctx, st := openImportTelemetryTestStore(t, &logs)
	fixture := seedImportTelemetryFixture(ctx, t, st, "backup_validation")
	baseline, err := Open(ctx, testenv.RequirePostgres(t))
	if err != nil {
		t.Fatal(err)
	}
	defer baseline.Close()

	for _, hostile := range []string{"unknown_column", "manifest_account_id"} {
		t.Run(hostile, func(t *testing.T) {
			logs.Reset()
			manifest, rows := readAvatarArchiveRows(t, fixture.archive, SchemaVersion())
			var want string
			var wantClass error
			switch hostile {
			case "unknown_column":
				var account map[string]any
				if err := json.Unmarshal(rows["accounts"][0], &account); err != nil {
					t.Fatal(err)
				}
				account[importTelemetrySentinel] = "synthetic unknown value"
				rows["accounts"][0], err = json.Marshal(account)
				if err != nil {
					t.Fatal(err)
				}
				wantClass = ErrArchiveContent
				want = fmt.Sprintf("%v: accounts row has unknown column %q", wantClass, importTelemetrySentinel)
			case "manifest_account_id":
				manifest.AccountID = importTelemetrySentinel
				wantClass = ErrArchiveAccountMismatch
				want = fmt.Sprintf("%v: archive is for %q", wantClass, importTelemetrySentinel)
			}
			archive := writeAvatarArchiveRows(t, manifest, manifest.Tables, rows)
			_, baselineErr := baseline.ValidateAccountBackup(ctx, fixture.info.AccountID, fixture.info.BackupID, bytes.NewReader(archive))
			_, observedErr := st.ValidateAccountBackup(ctx, fixture.info.AccountID, fixture.info.BackupID, bytes.NewReader(archive))
			// Assert the established diagnostic, as well as observer-on/off parity.
			// Do not print the error: its text deliberately contains hostile data.
			if !errors.Is(baselineErr, wantClass) || baselineErr.Error() != want {
				t.Fatal("baseline import error changed class or text")
			}
			if !errors.Is(observedErr, wantClass) || observedErr.Error() != want {
				t.Fatal("observed import error changed class or text")
			}
			failed := fixture
			failed.checksums = archiveexport.Checksums{}
			assertImportTelemetryLines(t, logs.String(), failed, "error", false)
		})
	}
}

func TestImportTelemetryConcurrentLeasesPostgres(t *testing.T) {
	var logs bytes.Buffer
	ctx, st := openImportTelemetryTestStore(t, &logs)
	validation := seedImportTelemetryFixture(ctx, t, st, "backup_validation")
	importing := seedImportTelemetryFixture(ctx, t, st, "evacuation_import")
	validationLease, err := st.AcquireBackupValidationLease(ctx, validation.info.AccountID, validation.info.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	defer validationLease.Close()
	importLease, err := st.AcquireAccountImportLease(ctx, importing.info.AccountID, importing.info.EvacuationID)
	if err != nil {
		t.Fatal(err)
	}
	defer importLease.Close()

	// Both Begin calls have completed before either archive's first Read.
	// This makes a shared-tracker mutation fail even on a single-core runner.
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		_, err := validationLease.Validate(ctx, &importTelemetryReadGate{
			Reader: bytes.NewReader(validation.archive), ctx: ctx, ready: ready, release: release,
		})
		results <- err
	}()
	go func() {
		_, _, err := importLease.Import(ctx, &importTelemetryReadGate{
			Reader: bytes.NewReader(importing.archive), ctx: ctx, ready: ready, release: release,
		})
		results <- err
	}()
	started := 0
	for started < 2 {
		select {
		case <-ready:
			started++
		case <-ctx.Done():
			close(release)
			// Drain the workers before reading the non-concurrent bytes.Buffer.
			<-results
			<-results
			t.Fatal("concurrent imports did not both reach their archive readers")
		}
	}
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			// A second worker may still be writing, so defer all log inspection.
			t.Errorf("concurrent lease operation failed: %v", err)
		}
	}
	if t.Failed() {
		return
	}
	byAccount := map[string]*strings.Builder{
		validation.info.AccountID: {},
		importing.info.AccountID:  {},
	}
	for _, line := range strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n") {
		matched := 0
		for accountID, buffer := range byAccount {
			if strings.Contains(line, " account_id="+strconv.Quote(accountID)+" ") {
				buffer.WriteString(line + "\n")
				matched++
			}
		}
		if matched != 1 {
			t.Fatal("concurrent telemetry line does not belong to exactly one expected account")
		}
	}
	assertImportTelemetryLines(t, byAccount[validation.info.AccountID].String(), validation, "ok", true)
	assertImportTelemetryLines(t, byAccount[importing.info.AccountID].String(), importing, "ok", true)
}

type importTelemetryReadGate struct {
	io.Reader
	ctx     context.Context
	ready   chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *importTelemetryReadGate) Read(p []byte) (int, error) {
	r.once.Do(func() {
		r.ready <- struct{}{}
		select {
		case <-r.release:
		case <-r.ctx.Done():
		}
	})
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func openImportTelemetryTestStore(t *testing.T, logs io.Writer) (context.Context, *Store) {
	t.Helper()
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	logger := newImportTelemetryLogger(logs, "witself-server", memlimit.Sample, 1024, 1024)
	st, err := Open(ctx, dsn, WithImportObserver(logger))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return ctx, st
}

func seedImportTelemetryFixture(ctx context.Context, t *testing.T, st *Store, purpose string) importTelemetryFixture {
	t.Helper()
	account := provisionActiveEvacuationTestAccount(ctx, t, st, "import-telemetry")
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	// Agent.Name is its display name; there is no separate display_name column.
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, importTelemetrySentinel)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := st.CreateTranscript(ctx, account.AccountID, realm.ID, agent.ID, CreateTranscriptInput{ExternalID: "telemetry-transcript"})
	if err != nil {
		t.Fatal(err)
	}
	for i, body := range []string{"small synthetic transcript row", strings.Repeat(importTelemetrySentinel, 256)} {
		if _, err := st.AppendTranscriptEntry(ctx, account.AccountID, realm.ID, agent.ID, transcript.ID, AppendTranscriptEntryInput{
			ExternalID: fmt.Sprintf("telemetry-row-%d", i), Role: TranscriptRoleUser, Body: body,
		}); err != nil {
			t.Fatal(err)
		}
	}
	fixture := importTelemetryFixture{info: ImportInfo{Purpose: purpose, AccountID: account.AccountID}}
	var archive bytes.Buffer
	switch purpose {
	case "backup_validation":
		fixture.info.BackupID = "backup_telemetry_" + account.AccountID
		err = st.ExportAccountBackup(ctx, account.AccountID, fixture.info.BackupID, "telemetry-source", "test", &archive)
	case "evacuation_import":
		fixture.info.EvacuationID = "evac_telemetry_" + account.AccountID
		if _, err := st.BeginAccountEvacuation(ctx, account.AccountID, fixture.info.EvacuationID, "telemetry fixture"); err != nil {
			t.Fatal(err)
		}
		err = st.ExportAccountEvacuation(ctx, account.AccountID, fixture.info.EvacuationID, "telemetry-source", "test", &archive)
		if _, abortErr := st.AbortAccountEvacuation(ctx, account.AccountID, fixture.info.EvacuationID); abortErr != nil {
			t.Fatal(abortErr)
		}
	default:
		if err := st.SuspendAccountSystem(ctx, account.AccountID, "evacuation", "telemetry fixture"); err != nil {
			t.Fatal(err)
		}
		err = st.ExportAccount(ctx, account.AccountID, "telemetry-source", "test", &archive)
	}
	if err != nil {
		t.Fatal(err)
	}
	fixture.archive = bytes.Clone(archive.Bytes())
	fixture.checksums, err = memoryArchiveLoadChecksums(fixture.archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.checksums.Chunks) == 0 {
		t.Fatal("telemetry fixture has no chunks")
	}
	// Check that the privacy canary occupies the largest transcript row and
	// starts early enough to catch even a short row-preview regression.
	_, rows := readAvatarArchiveRows(t, fixture.archive, SchemaVersion())
	var largest []byte
	for _, row := range rows["transcript_entries"] {
		if len(row) > len(largest) {
			largest = row
		}
	}
	if len(largest) < 1024 || !bytes.Contains(largest[:min(256, len(largest))], []byte(importTelemetrySentinel)) {
		t.Fatal("largest transcript row does not carry the early privacy canary")
	}
	if err := deleteAccountForIntegrationTest(ctx, st, account.AccountID); err != nil {
		t.Fatal(err)
	}
	return fixture
}

var importTelemetryField = regexp.MustCompile(` ([a-z_]+)=("(?:[^"\\]|\\.)*"|[^ ]+)`)
var importTelemetryInteger = regexp.MustCompile(`^(?:-1|0|[1-9][0-9]*)$`)

func assertImportTelemetryLines(t *testing.T, output string, fixture importTelemetryFixture, outcome string, wantLarge bool) {
	t.Helper()
	if strings.Contains(output, importTelemetrySentinel) {
		t.Fatal("telemetry leaked the archive privacy sentinel")
	}
	if output == "" || !strings.HasSuffix(output, "\n") {
		t.Fatal("telemetry must contain complete newline-terminated records")
	}
	allowedTables := map[string]bool{"unknown": true, "none": true}
	for _, table := range canonicalArchiveTables {
		allowedTables[table.name] = true
	}
	refKeys := ""
	switch fixture.info.Purpose {
	case "backup_validation":
		refKeys = " backup_id"
	case "evacuation_import":
		refKeys = " evacuation_id"
	}
	wantedChunks := make(map[string]archiveexport.ChunkSum)
	wantedRows := 0
	for _, chunk := range fixture.checksums.Chunks {
		wantedChunks[chunk.Name] = chunk
		wantedRows += chunk.Rows
	}
	entries, summaries, largeRows := 0, 0, 0
	for lineNumber, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		var kind, keys string
		switch {
		case strings.HasPrefix(line, "witself-server: account import entry "):
			kind = "entry"
			keys = "purpose account_id" + refKeys + " table chunk entry_bytes rows largest_row_bytes large_rows go_live go_goal go_total go_limit anon file rss rss_hwm"
		case strings.HasPrefix(line, "witself-server: account import large row "):
			kind = "large row"
			keys = "purpose account_id" + refKeys + " table chunk row_bytes go_live go_total go_limit anon rss"
		case strings.HasPrefix(line, "witself-server: account import memory "):
			kind = "memory"
			keys = "purpose account_id" + refKeys + " outcome entries rows largest_row_bytes largest_row_table max_go_total max_go_total_table max_go_total_chunk max_anon max_anon_table max_anon_chunk max_go_live max_rss rss_hwm memory_peak file go_limit duration"
		default:
			t.Fatalf("telemetry line %d has an unexpected record type", lineNumber)
		}
		rest := strings.TrimPrefix(line, "witself-server: account import "+kind)
		matches := importTelemetryField.FindAllStringSubmatch(rest, -1)
		wantKeys := strings.Fields(keys)
		if len(matches) != len(wantKeys) {
			t.Fatalf("telemetry line %d has an unexpected field count", lineNumber)
		}
		fields := make(map[string]string, len(matches))
		consumed := 0
		for i, match := range matches {
			consumed += len(match[0])
			key, value := match[1], match[2]
			if key != wantKeys[i] {
				t.Fatalf("telemetry line %d field %d has an unexpected key", lineNumber, i)
			}
			if strings.HasPrefix(value, "\"") {
				decoded, err := strconv.Unquote(value)
				if err != nil {
					t.Fatal("invalid quoted telemetry string")
				}
				switch key {
				case "purpose":
					if decoded != fixture.info.Purpose {
						t.Fatal("telemetry purpose mismatch")
					}
				case "account_id":
					if decoded != fixture.info.AccountID {
						t.Fatal("telemetry account identity mismatch")
					}
				case "backup_id":
					if decoded != fixture.info.BackupID {
						t.Fatal("telemetry backup identity mismatch")
					}
				case "evacuation_id":
					if decoded != fixture.info.EvacuationID {
						t.Fatal("telemetry evacuation identity mismatch")
					}
				case "outcome":
					if decoded != outcome {
						t.Fatal("telemetry outcome mismatch")
					}
				case "table", "largest_row_table", "max_go_total_table", "max_anon_table":
					if !allowedTables[decoded] {
						t.Fatal("telemetry table is outside the canonical vocabulary")
					}
				default:
					t.Fatal("telemetry string in a numeric field")
				}
				fields[key] = decoded
			} else if key == "duration" {
				duration, err := time.ParseDuration(value)
				if err != nil || duration < 0 || duration.String() != value {
					t.Fatal("invalid telemetry duration")
				}
				fields[key] = value
			} else {
				if !importTelemetryInteger.MatchString(value) {
					t.Fatal("telemetry numeric field is not an integer or unavailable marker")
				}
				switch key {
				case "purpose", "account_id", "backup_id", "evacuation_id", "outcome", "table", "largest_row_table", "max_go_total_table", "max_anon_table":
					t.Fatal("telemetry string field is not quoted")
				}
				fields[key] = value
			}
		}
		if consumed != len(rest) {
			t.Fatal("telemetry record contains unparsed text")
		}
		switch kind {
		case "entry":
			entries++
			chunk, err := strconv.Atoi(fields["chunk"])
			if err != nil {
				t.Fatal(err)
			}
			name := fmt.Sprintf("%s/%06d.ndjson", fields["table"], chunk)
			want, found := wantedChunks[name]
			if !found || fields["entry_bytes"] != strconv.Itoa(want.Bytes) || fields["rows"] != strconv.Itoa(want.Rows) {
				t.Fatal("telemetry entry differs from checksums.json chunk")
			}
			delete(wantedChunks, name)
		case "large row":
			largeRows++
			rowBytes, err := strconv.Atoi(fields["row_bytes"])
			if err != nil || rowBytes < 1024 {
				t.Fatal("large-row telemetry did not use the test threshold")
			}
		case "memory":
			summaries++
			if fields["entries"] != strconv.Itoa(len(fixture.checksums.Chunks)) || fields["rows"] != strconv.Itoa(wantedRows) {
				t.Fatal("telemetry summary totals differ from this import's checksums.json")
			}
			if len(fixture.checksums.Chunks) == 0 {
				if fields["largest_row_bytes"] != "0" || fields["largest_row_table"] != "none" {
					t.Fatal("import without completed entries recorded a largest row")
				}
			}
			if len(fixture.checksums.Chunks) == 0 && largeRows == 0 {
				for key, want := range map[string]string{
					"max_go_total": "-1", "max_go_total_table": "none", "max_go_total_chunk": "-1",
					"max_anon": "-1", "max_anon_table": "none", "max_anon_chunk": "-1", "max_go_live": "-1",
				} {
					if fields[key] != want {
						t.Fatalf("empty-import telemetry has incorrect %s", key)
					}
				}
			}
		}
	}
	if entries != len(fixture.checksums.Chunks) || len(wantedChunks) != 0 || summaries != 1 {
		t.Fatalf("telemetry count mismatch: entries=%d summaries=%d", entries, summaries)
	}
	if wantLarge && largeRows == 0 {
		t.Fatal("sentinel-bearing rows produced no large-row telemetry")
	}
}
