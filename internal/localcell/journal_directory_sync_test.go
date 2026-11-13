package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"testing"
)

// journalDirectorySyncTrace records, in order, each completed sync of the
// ordinary journal file ("file"), each injected failure of that sync
// ("file_failed") and each call of the journal directory hook ("directory").
// Other files share the syncFile seam and pass through unrecorded.
type journalDirectorySyncTrace struct {
	events            []string
	directoryPaths    []string
	directoryContents [][]byte
	fileErr           error // returned instead of syncing the journal file
	directoryErr      error // returned instead of syncing the journal directory
}

// traceJournalDirectorySync replaces both hooks for the rest of the test and
// restores them when it ends. The real syncs run unless an error is injected.
func traceJournalDirectorySync(t *testing.T, c *Cell) *journalDirectorySyncTrace {
	t.Helper()
	realSync, realDirectorySync := syncFile, syncJournalDirectory
	t.Cleanup(func() { syncFile, syncJournalDirectory = realSync, realDirectorySync })
	trace := &journalDirectorySyncTrace{}
	journalPath := c.path("journal", "operations.jsonl")
	syncFile = func(f *os.File) error {
		if f.Name() != journalPath {
			return realSync(f) // Atomic manifest publication shares the seam.
		}
		if trace.fileErr != nil {
			trace.events = append(trace.events, "file_failed")
			return trace.fileErr
		}
		if err := realSync(f); err != nil {
			return err
		}
		trace.events = append(trace.events, "file")
		return nil
	}
	syncJournalDirectory = func(path string) error {
		trace.events = append(trace.events, "directory")
		trace.directoryPaths = append(trace.directoryPaths, path)
		// Capture the file as it stands when the directory sync starts.
		data, err := os.ReadFile(journalPath)
		if err != nil {
			data = nil
		}
		trace.directoryContents = append(trace.directoryContents, data)
		if trace.directoryErr != nil {
			return trace.directoryErr
		}
		return realDirectorySync(path)
	}
	return trace
}

// journalDirectorySyncLines decodes data as complete, newline-terminated
// ordinary journal records. It reports false for a torn, padded or invalid line.
func journalDirectorySyncLines(data []byte) ([]journalEntry, bool) {
	if len(data) == 0 || !bytes.HasSuffix(data, []byte{'\n'}) {
		return nil, false
	}
	var entries []journalEntry
	for _, line := range bytes.SplitAfter(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var entry journalEntry
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&entry) != nil || decoder.Decode(new(any)) != io.EOF || entry.At.IsZero() {
			return nil, false
		}
		entries = append(entries, entry)
	}
	return entries, true
}

// journalDirectorySyncSingleRecord reports whether data is exactly one
// complete record for the requested operation, step and state without a cause.
func journalDirectorySyncSingleRecord(data []byte, operation, step, state string) bool {
	entries, ok := journalDirectorySyncLines(data)
	return ok && len(entries) == 1 && entries[0].Operation == operation && entries[0].Step == step &&
		entries[0].State == state && entries[0].Cause == ""
}

// Every other test replaces the seam, so only this one can observe a default
// that silently skips the production directory sync.
func TestJournalDirectorySyncDefaultsToSyncDirectory(t *testing.T) {
	if reflect.ValueOf(syncJournalDirectory).Pointer() != reflect.ValueOf(syncDirectory).Pointer() {
		t.Fatal("ordinary journal directory sync no longer defaults to syncDirectory")
	}
}

func TestJournalDirectorySyncFirstAppendAfterFileSync(t *testing.T) {
	c, _ := unitCell(t)
	path := c.path("journal", "operations.jsonl")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fresh cell fixture already has an ordinary journal")
	}
	trace := traceJournalDirectorySync(t, c)
	if err := c.journal("stop", "operation", "started"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(trace.events, []string{"file", "directory"}) {
		t.Fatalf("first append sync trace = %v; want one completed file sync, then one directory sync", trace.events)
	}
	if !slices.Equal(trace.directoryPaths, []string{c.path("journal")}) {
		t.Fatal("first append did not sync exactly the journal directory")
	}
	if !journalDirectorySyncSingleRecord(trace.directoryContents[0], "stop", "operation", "started") {
		t.Fatal("journal directory sync preceded the complete first record")
	}
}

func TestJournalDirectorySyncSkipsExistingFile(t *testing.T) {
	t.Run("second_append", func(t *testing.T) {
		c, _ := unitCell(t)
		trace := traceJournalDirectorySync(t, c)
		if err := c.journal("stop", "operation", "started"); err != nil {
			t.Fatal(err)
		}
		if err := c.journal("stop", "stop", "started"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(trace.events, []string{"file", "directory", "file"}) {
			t.Fatalf("append sync trace = %v; want the directory synced only by the creating append", trace.events)
		}
	})
	t.Run("existing_empty_file", func(t *testing.T) {
		// An existing pathname was not created by this append, even when it is
		// empty, as after Repair truncates a torn first record to byte zero.
		c, _ := unitCell(t)
		if err := testWriteFile(c.path("journal", "operations.jsonl"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		trace := traceJournalDirectorySync(t, c)
		if err := c.journal("stop", "operation", "started"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(trace.events, []string{"file"}) {
			t.Fatalf("append sync trace = %v; want only the file sync for an existing journal", trace.events)
		}
	})
	t.Run("lifecycle_stop", func(t *testing.T) {
		// Stop on a fresh cell appends several records; only begin's creating
		// append syncs the directory, never one fsync per line.
		c, _ := unitCell(t)
		trace := traceJournalDirectorySync(t, c)
		if err := c.Stop(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(trace.events) < 3 || !slices.Equal(trace.events[:2], []string{"file", "directory"}) ||
			slices.Contains(trace.events[2:], "directory") {
			t.Fatalf("Stop sync trace = %v; want one directory sync, right after the first file sync only", trace.events)
		}
	})
}

func TestJournalDirectorySyncFailureKeepsRecordForRepair(t *testing.T) {
	c, _ := unitCell(t)
	path := c.path("journal", "operations.jsonl")
	trace := traceJournalDirectorySync(t, c)
	trace.directoryErr = errors.New("synthetic private directory sync details")
	// Stop's first append is begin's operation record, written before any step.
	err := c.Stop(t.Context())
	if err == nil || err.Error() != "cannot sync local cell journal directory" {
		t.Fatal("first-append directory sync failure did not return the fixed value-free journal error")
	}
	if !slices.Equal(trace.events, []string{"file", "directory"}) {
		t.Fatalf("failed first append sync trace = %v; want the file sync, then the failed directory sync", trace.events)
	}
	data, err := os.ReadFile(path)
	if err != nil || !journalDirectorySyncSingleRecord(data, "stop", "operation", "started") {
		t.Fatal("directory sync failure removed, rewrote or extended the file-synced first record")
	}
	progress, err := c.readJournal()
	if err != nil || progress.tornTail || progress.cleanupOnly || progress.pending == nil ||
		*progress.pending != (Interruption{Operation: "stop", Step: "operation"}) || len(progress.done) != 0 ||
		progress.durableBytes != int64(len(data)) {
		t.Fatal("readJournal did not report the complete first record as a pending operation")
	}
	// The failed operation stays fenced: a later lifecycle call refuses without
	// appending, so only Repair may settle the record the caller saw fail.
	if err := c.Stop(t.Context()); err == nil || err.Error() != "local cell operation interrupted; run repair first" {
		t.Fatal("a lifecycle call ran on top of the unsettled first record")
	}
	if len(trace.events) != 2 {
		t.Fatal("a refused lifecycle call synced the journal")
	}
	assertFileBytes(t, path, data, "a refused lifecycle call changed the journal")
	trace.directoryErr = nil
	if err := c.Repair(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(after, data) {
		t.Fatal("Repair did not keep the retained first record as its durable prefix")
	}
	entries, ok := journalDirectorySyncLines(after)
	if !ok || len(entries) < 2 || entries[len(entries)-1].Operation != "stop" ||
		entries[len(entries)-1].Step != "operation" || entries[len(entries)-1].State != "done" {
		t.Fatal("Repair did not settle the operation whose first record it retained")
	}
	progress, err = c.readJournal()
	if err != nil || progress.pending != nil || progress.tornTail {
		t.Fatal("journal remained unsettled after Repair")
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestJournalDirectorySyncNotCalledAfterFileSyncFailure(t *testing.T) {
	c, _ := unitCell(t)
	trace := traceJournalDirectorySync(t, c)
	trace.fileErr = errors.New("synthetic private sync details")
	err := c.journal("stop", "operation", "started")
	if err == nil || err.Error() != "cannot sync local cell journal" {
		t.Fatal("first-append file sync failure did not return the existing value-free journal error")
	}
	if !slices.Equal(trace.events, []string{"file_failed"}) {
		t.Fatalf("file sync failure trace = %v; want one failed file sync and no directory sync", trace.events)
	}
}
