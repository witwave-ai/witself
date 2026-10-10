package export

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestReadEntryHooks(t *testing.T) {
	rowOfSize := func(size int) string {
		return `{"body":"` + strings.Repeat("x", size-len(`{"body":""}`)) + `"}`
	}
	chunks := []tarEntry{
		{"realms/000001.ndjson", []byte(rowOfSize((1<<20)-1) + "\n" + rowOfSize(1<<20) + "\n")},
		{"realms/000002.ndjson", []byte(rowOfSize((1<<20)+9) + "\n")},
		{"tokens/000001.ndjson", []byte("{}\n")},
	}
	sums := Checksums{TableRows: map[string]int{"realms": 3, "tokens": 1}}
	for i, chunk := range chunks {
		rows := 1
		if i == 0 {
			rows = 2
		}
		sums.Chunks = append(sums.Chunks, ChunkSum{Name: chunk.name, SHA256: sha256Hex(chunk.data), Bytes: len(chunk.data), Rows: rows})
	}
	manifest := Manifest{FormatVersion: FormatVersion, SchemaVersion: 13, Tables: []string{"realms", "tokens"}}
	archive := buildHandArchive(t, manifest, chunks, sums)
	wantStats := []EntryStats{
		{Table: "realms", Chunk: 1, EntryBytes: int64(len(chunks[0].data)), Rows: 2, LargestRowBytes: 1 << 20, LargeRows: 1},
		{Table: "realms", Chunk: 2, EntryBytes: int64(len(chunks[1].data)), Rows: 1, LargestRowBytes: (1 << 20) + 9, LargeRows: 1},
		{Table: "tokens", Chunk: 1, EntryBytes: 3, Rows: 1, LargestRowBytes: 2},
	}
	// Changing every delivered row proves byte counts are taken before upgrade.
	upgraders[13] = func(_ string, _ map[string]any) (map[string]any, error) {
		return map[string]any{"upgraded": true}, nil
	}
	t.Cleanup(func() { delete(upgraders, 13) })
	var starts []EntryStart
	var stats []EntryStats
	rowsSeen := 0
	_, err := Read(context.Background(), bytes.NewReader(archive), ImportOptions{
		CurrentSchema: 14,
		OnEntryStart: func(start EntryStart) {
			if len(starts) != len(stats) {
				t.Fatal("new entry started before previous entry completed")
			}
			starts = append(starts, start)
			rowsSeen = 0
		},
		Row: func(table string, row []byte) error {
			if len(starts) != len(stats)+1 || starts[len(starts)-1].Table != table {
				t.Fatal("row arrived before its entry start")
			}
			if string(row) != `{"upgraded":true}` {
				t.Fatal("fixture upgrader did not run")
			}
			rowsSeen++
			return nil
		},
		OnEntry: func(entry EntryStats) {
			if entry.Rows != rowsSeen {
				t.Fatal("entry completed before all its rows")
			}
			start := starts[len(starts)-1]
			if start != (EntryStart{Table: entry.Table, Chunk: entry.Chunk, EntryBytes: entry.EntryBytes}) {
				t.Fatal("start/end coordinates differ")
			}
			stats = append(stats, entry)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stats, wantStats) || len(starts) != len(chunks) {
		t.Fatalf("entry stats = %+v; want %+v; starts=%d", stats, wantStats, len(starts))
	}
}

func TestReadEntryHooksNoCompletionOnRowError(t *testing.T) {
	archive := buildArchive(t, 13, "acc_entry_error")
	wantErr := errors.New("sink failed")
	starts, completed := 0, 0
	_, err := Read(context.Background(), bytes.NewReader(archive), ImportOptions{
		CurrentSchema: 13,
		OnEntryStart:  func(EntryStart) { starts++ },
		OnEntry:       func(EntryStats) { completed++ },
		Row:           func(string, []byte) error { return wantErr },
	})
	if err != wantErr || starts != 1 || completed != 0 {
		t.Fatalf("err=%v starts=%d completed=%d", err, starts, completed)
	}
}

// assertEntryObserverParity replays existing round-trip and corrupt fixtures
// both ways, comparing the returned manifest, every delivered row and exact
// diagnostic, including error classification.
func assertEntryObserverParity(t *testing.T, archive []byte, schema int) {
	t.Helper()
	read := func(observe bool) (Manifest, []string, error) {
		var rows []string
		opts := ImportOptions{CurrentSchema: schema, Row: func(table string, row []byte) error {
			rows = append(rows, table+":"+string(row))
			return nil
		}}
		if observe {
			opts.OnEntryStart = func(EntryStart) {}
			opts.OnEntry = func(EntryStats) {}
		}
		m, err := Read(context.Background(), bytes.NewReader(archive), opts)
		return m, rows, err
	}
	m, rows, err := read(false)
	observedManifest, observedRows, observedErr := read(true)
	if !reflect.DeepEqual(m, observedManifest) || !reflect.DeepEqual(rows, observedRows) {
		t.Fatal("entry observers changed manifest or row sequence")
	}
	if fmt.Sprint(err) != fmt.Sprint(observedErr) || errors.Is(err, ErrCorrupt) != errors.Is(observedErr, ErrCorrupt) ||
		errors.Is(err, ErrArchiveTooNew) != errors.Is(observedErr, ErrArchiveTooNew) {
		t.Fatalf("entry observers changed error: %v => %v", err, observedErr)
	}
}
