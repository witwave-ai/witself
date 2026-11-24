package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"runtime"
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
	fileErr           error  // returned instead of syncing the journal file
	failDirectory     string // directory whose sync fails
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
		stage := "directory"
		if path == c.cfg.Root {
			stage = "root"
		}
		trace.events = append(trace.events, stage)
		trace.directoryPaths = append(trace.directoryPaths, path)
		// Capture the file as it stands when the directory sync starts.
		data, err := os.ReadFile(journalPath)
		if err != nil {
			data = nil
		}
		trace.directoryContents = append(trace.directoryContents, data)
		if path == trace.failDirectory {
			return os.ErrInvalid
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

func firstJournalSyncs() []string {
	if runtime.GOOS == "windows" {
		return []string{"file", "directory"}
	}
	return []string{"file", "directory", "root"}
}

func TestJournalDirectorySyncFirstAppendAfterFileSync(t *testing.T) {
	c, _ := unitCell(t)
	path := c.path("journal", "operations.jsonl")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fresh cell fixture already has an ordinary journal")
	}
	trace := traceJournalDirectorySync(t, c)
	if err := c.journal(context.Background(), "stop", "operation", "started"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(trace.events, firstJournalSyncs()) {
		t.Fatal("first append did not sync file, journal directory and supported root in order")
	}
	paths := []string{c.path("journal")}
	if runtime.GOOS != "windows" {
		paths = append(paths, c.cfg.Root)
	}
	if !slices.Equal(trace.directoryPaths, paths) {
		t.Fatal("first append synced the wrong directory")
	}
	if !journalDirectorySyncSingleRecord(trace.directoryContents[0], "stop", "operation", "started") {
		t.Fatal("journal directory sync preceded the complete first record")
	}
}

func TestJournalDirectorySyncSkipsExistingFile(t *testing.T) {
	t.Run("second_append", func(t *testing.T) {
		c, _ := unitCell(t)
		trace := traceJournalDirectorySync(t, c)
		if err := c.journal(context.Background(), "stop", "operation", "started"); err != nil {
			t.Fatal(err)
		}
		if err := c.journal(context.Background(), "stop", "stop", "started"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(trace.events, append(firstJournalSyncs(), "file")) {
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
		if err := c.journal(context.Background(), "stop", "operation", "started"); err != nil {
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
		first := firstJournalSyncs()
		if len(trace.events) <= len(first) || !slices.Equal(trace.events[:len(first)], first) ||
			slices.ContainsFunc(trace.events[len(first):], func(stage string) bool { return stage != "file" }) {
			t.Fatal("Stop synced outside the expected publication and append sequence")
		}
	})
}

func TestJournalDirectorySyncFailureKeepsRecordForRepair(t *testing.T) {
	t.Run("journal", func(t *testing.T) { checkPublicationFailure(t, false) })
	if runtime.GOOS != "windows" {
		t.Run("root", func(t *testing.T) { checkPublicationFailure(t, true) })
	}
}

func checkPublicationFailure(t *testing.T, failRoot bool) {
	t.Helper()
	c, _ := unitCell(t)
	path := c.path("journal", "operations.jsonl")
	trace := traceJournalDirectorySync(t, c)
	trace.failDirectory = c.path("journal")
	want := []string{"file", "directory"}
	if failRoot {
		trace.failDirectory = c.cfg.Root
		want = append(want, "root")
	}
	// Stop's first append is begin's operation record, written before any step.
	err := c.Stop(t.Context())
	if err == nil || err.Error() != "cannot sync local cell journal directory" {
		t.Fatal("first-append directory sync failure did not return the fixed value-free journal error")
	}
	if !slices.Equal(trace.events, want) {
		t.Fatal("first publication continued past the failed directory sync")
	}
	data, err := os.ReadFile(path)
	if err != nil || !journalDirectorySyncSingleRecord(data, "stop", "operation", "started") {
		t.Fatal("directory sync failure removed, rewrote or extended the file-synced first record")
	}
	progress, err := c.readJournal(context.Background())
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
	if len(trace.events) != len(want) {
		t.Fatal("a refused lifecycle call synced the journal")
	}
	assertFileBytes(t, path, data, "a refused lifecycle call changed the journal")
	trace.failDirectory = ""
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
	progress, err = c.readJournal(context.Background())
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
	err := c.journal(context.Background(), "stop", "operation", "started")
	if err == nil || err.Error() != "cannot sync local cell journal" {
		t.Fatal("first-append file sync failure did not return the existing value-free journal error")
	}
	if !slices.Equal(trace.events, []string{"file_failed"}) {
		t.Fatalf("file sync failure trace = %v; want one failed file sync and no directory sync", trace.events)
	}
}
