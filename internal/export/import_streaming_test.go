package export

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

type streamingTestSource struct {
	table string
	rows  [][]byte
	next  int
}

func (s *streamingTestSource) Table() string { return s.table }
func (s *streamingTestSource) Next(context.Context) ([]byte, error) {
	if s.next == len(s.rows) {
		return nil, nil
	}
	row := s.rows[s.next]
	s.next++
	return row, nil
}

func streamingTestRow(size int) []byte {
	return []byte(`{"body":"` + strings.Repeat("x", size-len(`{"body":""}`)) + `"}`)
}

func writeStreamingTestArchive(t *testing.T, m Manifest, sources ...RowSource) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := Write(context.Background(), &out, m, sources); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type streamingRecordedRow struct {
	table string
	row   []byte
}

type streamingReadRecord struct {
	manifest  Manifest
	observed  []Manifest
	starts    []EntryStart
	startRows []int
	rows      []streamingRecordedRow
	stats     []EntryStats
	err       error
}

type streamingReadCase struct {
	name       string
	archive    []byte
	schema     int
	fragmented bool
	cut        bool
}

func recordStreamingRead(tc streamingReadCase, reader func(context.Context, io.Reader, ImportOptions) (Manifest, error), mode string) streamingReadRecord {
	var got streamingReadRecord
	opts := ImportOptions{
		CurrentSchema: tc.schema,
		OnManifest: func(m Manifest) error {
			got.observed = append(got.observed, m)
			return nil
		},
		OnEntryStart: func(s EntryStart) {
			got.starts = append(got.starts, s)
			got.startRows = append(got.startRows, len(got.rows))
		},
		OnEntry: func(s EntryStats) { got.stats = append(got.stats, s) },
		Row: func(table string, row []byte) error {
			got.rows = append(got.rows, streamingRecordedRow{table, bytes.Clone(row)})
			return nil
		},
	}
	if mode == "row_nil" {
		opts.Row = nil
	}
	if mode == "observers_nil" {
		opts.OnManifest = nil
		opts.OnEntryStart, opts.OnEntry = nil, nil
	}
	var input io.Reader = bytes.NewReader(tc.archive)
	if tc.fragmented {
		input = &gzipCompletionFragmentReader{reader: bytes.NewReader(tc.archive)}
	}
	got.manifest, got.err = reader(context.Background(), input, opts)
	return got
}

func requireStreamingRowsEqual(t *testing.T, got, want []streamingRecordedRow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("row count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].table != want[i].table || !bytes.Equal(got[i].row, want[i].row) {
			t.Fatalf("row %d differs (table or bytes)", i)
		}
	}
}

func requireStreamingMetadataEqual(t *testing.T, got, want streamingReadRecord) {
	t.Helper()
	if !reflect.DeepEqual(got.manifest, want.manifest) || !reflect.DeepEqual(got.observed, want.observed) {
		t.Fatal("returned or callback manifest differs from whole-entry oracle")
	}
	if !reflect.DeepEqual(got.starts, want.starts) {
		t.Fatal("EntryStart sequence differs from whole-entry oracle")
	}
	if !reflect.DeepEqual(got.stats, want.stats) {
		t.Fatal("EntryStats sequence differs from whole-entry oracle")
	}
}

// TestReadMatchesWholeEntryOracle freezes both successful imports and refusals
// against b4d952af. Corrupt-entry rows have a separate byte-derived oracle:
// neither reader decides which complete lines a truncated TAR delivered.
func TestReadMatchesWholeEntryOracle(t *testing.T) {
	valid := []struct {
		name  string
		build func(*testing.T) streamingReadCase
	}{
		{"round_trip_multichunk", func(t *testing.T) streamingReadCase {
			return streamingReadCase{archive: writeStreamingTestArchive(t, Manifest{
				SchemaVersion: 13, ServerVersion: "0.0.83", Purpose: PurposeBackup,
				BackupID: "bkp_roundtrip", AccountID: "acc_rt", Status: "active",
			}, newSource("realms", 3), newSource("audit", 300_000)), schema: 13}
		}},
		{"solo_33_mib", func(t *testing.T) streamingReadCase {
			return streamingReadCase{archive: writeStreamingTestArchive(t, Manifest{SchemaVersion: 13},
				&streamingTestSource{table: "realms", rows: [][]byte{streamingTestRow(33 << 20)}}), schema: 13}
		}},
		{"buffer_boundaries_and_reuse", func(t *testing.T) streamingReadCase {
			var rows [][]byte
			for _, size := range []int{17, entryReadBufferBytes - 1, 23, entryReadBufferBytes, 31, entryReadBufferBytes + 1, (1 << 20) + 3, 600 << 10, 2 << 20, 29} {
				rows = append(rows, streamingTestRow(size))
			}
			return streamingReadCase{archive: writeStreamingTestArchive(t, Manifest{SchemaVersion: 13},
				&streamingTestSource{table: "realms", rows: rows}), schema: 13}
		}},
		{"empty_rows", func(t *testing.T) streamingReadCase {
			return streamingReadCase{archive: writeStreamingTestArchive(t, Manifest{SchemaVersion: 13},
				&streamingTestSource{table: "realms", rows: [][]byte{{}, {}, []byte(`{}`), {}}}), schema: 13}
		}},
		{"schema_13_to_14", func(t *testing.T) streamingReadCase {
			previous, hadPrevious := upgraders[13]
			upgraders[13] = func(_ string, row map[string]any) (map[string]any, error) {
				if i, ok := row["i"].(json.Number); ok && i.String() == "0" {
					return nil, nil
				}
				row["lifted"] = true
				return row, nil
			}
			t.Cleanup(func() {
				if hadPrevious {
					upgraders[13] = previous
				} else {
					delete(upgraders, 13)
				}
			})
			return streamingReadCase{archive: buildArchive(t, 13, "acc_up"), schema: 14}
		}},
	}
	checkValid := func(t *testing.T, tc streamingReadCase) {
		for _, mode := range []string{"callbacks", "row_nil", "observers_nil"} {
			t.Run(mode, func(t *testing.T) {
				old := recordStreamingRead(tc, readWholeEntryOracle, mode)
				got := recordStreamingRead(tc, Read, mode)
				if old.err != nil || got.err != nil {
					t.Fatalf("valid archive: oracle error=%v; stream error=%v", old.err, got.err)
				}
				requireStreamingMetadataEqual(t, got, old)
				requireStreamingRowsEqual(t, got.rows, old.rows)
				if !reflect.DeepEqual(got.startRows, old.startRows) {
					t.Fatal("EntryStart row indices differ")
				}
			})
		}
	}
	for _, tc := range valid {
		t.Run("valid/"+tc.name, func(t *testing.T) { checkValid(t, tc.build(t)) })
	}
	validGzip, corruptGzip := streamingGzipCases(t)
	for _, tc := range validGzip {
		t.Run("valid/gzip/"+tc.name, func(t *testing.T) { checkValid(t, tc) })
	}
	validBoundary, corruptBoundary := streamingBoundaryCases(t)
	for _, tc := range validBoundary {
		t.Run("valid/"+tc.name, func(t *testing.T) { checkValid(t, tc) })
	}
	corrupt := append(streamingCorruptCases(t), corruptGzip...)
	corrupt = append(corrupt, corruptBoundary...)
	for _, tc := range corrupt {
		t.Run("corrupt/"+tc.name, func(t *testing.T) {
			old := recordStreamingRead(tc, readWholeEntryOracle, "callbacks")
			got := recordStreamingRead(tc, Read, "callbacks")
			if old.err == nil || got.err == nil {
				t.Fatal("corrupt archive was accepted by a reader")
			}
			for _, class := range []error{ErrCorrupt, ErrArchiveTooNew} {
				if errors.Is(got.err, class) != errors.Is(old.err, class) {
					t.Fatalf("error class differs for %v", class)
				}
			}
			if got.err.Error() != old.err.Error() {
				t.Fatalf("error text differs: stream=%v; oracle=%v", got.err, old.err)
			}
			requireStreamingMetadataEqual(t, got, old)
			if len(got.starts) == 0 || len(got.stats) == len(got.starts) {
				requireStreamingRowsEqual(t, got.rows, old.rows)
				return
			}
			k := len(got.starts) - 1
			requireStreamingRowsEqual(t, got.rows[:got.startRows[k]], old.rows[:old.startRows[k]])
			expected := streamingDeliveredCompleteRows(t, tc.archive, got.starts)
			if tc.cut && len(expected) == 0 {
				t.Fatal("cut fixture has no complete row in its failing entry")
			}
			requireStreamingRowsEqual(t, got.rows[got.startRows[k]:], expected)
			oldEntry := old.rows[old.startRows[k]:]
			if len(oldEntry) > len(expected) {
				t.Fatal("whole-entry oracle delivered more rows than the independent complete-line inventory")
			}
			requireStreamingRowsEqual(t, oldEntry, expected[:len(oldEntry)])
		})
	}
}

func streamingDeliveredCompleteRows(t *testing.T, archive []byte, starts []EntryStart) []streamingRecordedRow {
	t.Helper()
	last := starts[len(starts)-1]
	if last.EntryBytes > maxChunkBytes {
		return nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil
	}
	decoded, _ := io.ReadAll(gz) // Partial output is the independent evidence.
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(decoded))
	chunks := 0
	for {
		hdr, err := tr.Next()
		if err != nil {
			t.Fatal("independent TAR walk did not reach the failing entry")
		}
		if hdr.Name == "manifest.json" || hdr.Name == "checksums.json" {
			continue
		}
		chunks++
		if chunks != len(starts) {
			continue
		}
		data, _ := io.ReadAll(tr) // Preserve bytes returned with a TAR error.
		var rows []streamingRecordedRow
		for {
			nl := bytes.IndexByte(data, '\n')
			if nl < 0 {
				return rows
			}
			rows = append(rows, streamingRecordedRow{last.Table, bytes.Clone(data[:nl])})
			data = data[nl+1:]
		}
	}
}

// This reproduces every finite archive case from TestReadGzipCompletion,
// including both fragmented transports. Callback/cancellation faults have
// separate precedence tests because those are not corrupt archive inputs.
func streamingGzipCases(t *testing.T) (valid, corrupt []streamingReadCase) {
	t.Helper()
	canonical := buildArchive(t, 13, "acc_gzip_completion")
	raw := gunzip(t, canonical)
	join := func(a, b []byte) []byte { return append(bytes.Clone(a), b...) }
	crc := bytes.Clone(canonical)
	crc[len(crc)-8] ^= 1
	isize := bytes.Clone(canonical)
	isize[len(isize)-4] ^= 1
	split := join(regzip(t, raw[:37]), regzip(t, raw[37:]))
	dataTail := bytes.Repeat([]byte("ignored decoded archive tail\n"), 257)
	emptyMember, dataMember := regzip(t, nil), regzip(t, dataTail)
	badLaterCRC := bytes.Clone(dataMember)
	badLaterCRC[len(badLaterCRC)-8] ^= 1
	valid = []streamingReadCase{
		{name: "canonical", archive: canonical},
		{name: "fragmented", archive: canonical, fragmented: true},
		{name: "multistream_split", archive: split},
		{name: "fragmented_multistream_split", archive: split, fragmented: true},
		{name: "decoded_zero_tail", archive: regzip(t, join(raw, make([]byte, 8192)))},
		{name: "decoded_nonzero_tail", archive: regzip(t, join(raw, dataTail))},
		{name: "empty_member_tail", archive: join(canonical, emptyMember)},
		{name: "nonempty_member_tail", archive: join(canonical, dataMember)},
		// These archives also back the cancellation tests. Without a canceled
		// context their decoded and repeated-empty-member tails are valid.
		{name: "cancellation_decoded_tail_without_cancel", archive: join(canonical, regzip(t, bytes.Repeat([]byte{'d'}, 64<<10)))},
		{name: "cancellation_empty_tail_without_cancel", archive: join(canonical, bytes.Repeat(emptyMember, 64))},
	}
	corrupt = []streamingReadCase{
		{name: "crc_mismatch", archive: crc},
		{name: "fragmented_crc_mismatch", archive: crc, fragmented: true},
		{name: "isize_mismatch", archive: isize},
	}
	for cut := 1; cut <= 8; cut++ {
		corrupt = append(corrupt, streamingReadCase{name: fmt.Sprintf("footer_truncated_%d", cut), archive: canonical[:len(canonical)-cut]})
	}
	corrupt = append(corrupt,
		streamingReadCase{name: "later_member_crc_mismatch", archive: join(canonical, badLaterCRC)},
		streamingReadCase{name: "later_member_truncated", archive: join(canonical, emptyMember[:len(emptyMember)-1])},
		streamingReadCase{name: "trailing_raw_junk", archive: join(canonical, bytes.Repeat([]byte{'x'}, 16))},
		streamingReadCase{name: "trailing_partial_header", archive: join(canonical, []byte{0x1f, 0x8b})},
	)
	for i := range valid {
		valid[i].schema = 13
	}
	for i := range corrupt {
		corrupt[i].schema = 13
	}
	return valid, corrupt
}

// streamingCutCases builds a real Write archive, then cuts decoded TAR bytes
// at complete-row and fragment boundaries on each side of the read buffer.
func streamingCutCases(t *testing.T) []streamingReadCase {
	t.Helper()
	realms := [][]byte{[]byte(`{"id":1}`), []byte(`{"id":2}`), []byte(`{"id":3}`), []byte(`{"id":4}`), []byte(`{"id":5}`)}
	notes := [][]byte{[]byte(`{"id":6}`), streamingTestRow(300 << 10), []byte(`{"id":7}`), []byte(`{"id":8}`)}
	archive := writeStreamingTestArchive(t, Manifest{SchemaVersion: 13},
		&streamingTestSource{table: "realms", rows: realms},
		&streamingTestSource{table: "notes", rows: notes},
		&streamingTestSource{table: "tokens"})
	raw := gunzip(t, archive)
	realmsHeader, realmsStart, realmsSize := streamingTarOffsets(t, raw, "realms/000001.ndjson")
	_, notesStart, _ := streamingTarOffsets(t, raw, "notes/000001.ndjson")
	secondEnd := realmsStart + len(realms[0]) + 1 + len(realms[1]) + 1
	largeStart := notesStart + len(notes[0]) + 1
	cases := []streamingReadCase{
		{name: "C1_complete_second_row", archive: regzip(t, raw[:secondEnd]), cut: true},
		{name: "C2_mid_third_row", archive: regzip(t, raw[:secondEnd+len(realms[2])/2]), cut: true},
		{name: "C3_mid_first_large_segment", archive: regzip(t, raw[:largeStart+(100<<10)]), cut: true},
		{name: "C4_mid_slow_path", archive: regzip(t, raw[:largeStart+(290<<10)]), cut: true},
	}
	for _, edit := range []struct {
		name string
		size int
	}{
		{"header_size_plus_512", realmsSize + 512},
		{"header_size_minus_1", realmsSize - 1},
		{"header_size_minus_last_row", realmsSize - len(realms[len(realms)-1]) - 1},
	} {
		edited := bytes.Clone(raw)
		streamingSetTarSize(edited[realmsHeader:realmsHeader+512], int64(edit.size))
		cases = append(cases, streamingReadCase{name: edit.name, archive: regzip(t, edited)})
	}
	claimed := bytes.Clone(raw[:realmsStart])
	streamingSetTarSize(claimed[realmsHeader:realmsHeader+512], maxChunkBytes+1)
	cases = append(cases, streamingReadCase{name: "oversized_header_without_data", archive: regzip(t, claimed)})
	for i := range cases {
		cases[i].schema = 13
	}
	return cases
}

func streamingTarOffsets(t *testing.T, raw []byte, name string) (header, payload, size int) {
	t.Helper()
	r := bytes.NewReader(raw)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err != nil {
			t.Fatalf("TAR fixture is missing %s", name)
		}
		if hdr.Name == name {
			payload = len(raw) - r.Len()
			return payload - 512, payload, int(hdr.Size)
		}
	}
}

func streamingSetTarSize(header []byte, size int64) {
	copy(header[124:136], fmt.Sprintf("%011o\x00", size))
	for i := 148; i < 156; i++ {
		header[i] = ' '
	}
	var sum int
	for _, b := range header {
		sum += int(b)
	}
	copy(header[148:156], fmt.Sprintf("%06o\x00 ", sum))
}

func streamingCorruptCases(t *testing.T) []streamingReadCase {
	t.Helper()
	cases := streamingCutCases(t)
	tamper := buildArchive(t, 13, "acc_tamper")
	raw := gunzip(t, tamper)
	i := bytes.Index(raw, []byte(`"i":1`))
	if i < 0 {
		t.Fatal("tamper fixture has no marker")
	}
	raw[i+4] = '7'
	cases = append(cases, streamingReadCase{name: "tampered_chunk", archive: regzip(t, raw)})
	truncated := buildArchive(t, 13, "acc_trunc")
	for _, cut := range []int{100, len(truncated) / 2} {
		cases = append(cases, streamingReadCase{name: fmt.Sprintf("compressed_cut_%d", cut), archive: truncated[:len(truncated)-cut]})
	}
	cases = append(cases, streamingChecksumCases(t)...)
	cases = append(cases, streamingShapeCases(t)...)
	// The absent trailer is intentional: stray.txt must refuse first, exactly
	// as in TestReadRejectsStrayEntries.
	var stray bytes.Buffer
	gz := gzip.NewWriter(&stray)
	tw := tar.NewWriter(gz)
	manifest, err := json.Marshal(Manifest{FormatVersion: FormatVersion, SchemaVersion: 13, AccountID: "acc_stray", Status: "suspended", Tables: []string{"realms"}})
	if err != nil {
		t.Fatal(err)
	}
	writeTarEntry(t, tw, "manifest.json", manifest)
	writeTarEntry(t, tw, "stray.txt", []byte("who put this here"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	cases = append(cases, streamingReadCase{name: "stray_entry", archive: stray.Bytes()})
	for i := range cases {
		cases[i].schema = 13
	}
	return cases
}

func streamingChecksumCases(t *testing.T) []streamingReadCase {
	t.Helper()
	data := []byte(`{"table":"realms","id":1}` + "\n" + `{"table":"realms","id":2}` + "\n")
	m := Manifest{FormatVersion: FormatVersion, SchemaVersion: 13, AccountID: "acc_tc", Status: "suspended", Tables: []string{"realms"}}
	sum := ChunkSum{Name: "realms/000001.ndjson", SHA256: sha256Hex(data), Bytes: len(data), Rows: 2}
	bytesLie, rowsLie := sum, sum
	bytesLie.Bytes++
	rowsLie.Rows++
	fixtures := []struct {
		name  string
		sums  Checksums
		extra []tarEntry
	}{
		{"unchecked_extra_chunk", Checksums{Chunks: []ChunkSum{sum}, TableRows: map[string]int{"realms": 2}}, []tarEntry{{"realms/000002.ndjson", data}}},
		{"checksummed_chunk_missing", Checksums{Chunks: []ChunkSum{sum, {Name: "realms/000002.ndjson", SHA256: sha256Hex(data), Bytes: len(data), Rows: 2}}, TableRows: map[string]int{"realms": 4}}, nil},
		{"listed_twice", Checksums{Chunks: []ChunkSum{sum, sum}, TableRows: map[string]int{"realms": 4}}, []tarEntry{{"realms/000002.ndjson", []byte(`{"tampered":true}` + "\n")}}},
		{"bytes_lie", Checksums{Chunks: []ChunkSum{bytesLie}, TableRows: map[string]int{"realms": 2}}, nil},
		{"row_count_lies", Checksums{Chunks: []ChunkSum{rowsLie}, TableRows: map[string]int{"realms": 3}}, nil},
		{"table_total_disagrees", Checksums{Chunks: []ChunkSum{sum}, TableRows: map[string]int{"realms": 99}}, nil},
		{"unknown_table_count", Checksums{Chunks: []ChunkSum{sum}, TableRows: map[string]int{"realms": 2, "audit": 0}}, nil},
		{"missing_table_count", Checksums{Chunks: []ChunkSum{sum}, TableRows: map[string]int{}}, nil},
	}
	var cases []streamingReadCase
	for _, f := range fixtures {
		cases = append(cases, streamingReadCase{name: "checksum/" + f.name,
			archive: buildHandArchive(t, m, append([]tarEntry{{sum.Name, data}}, f.extra...), f.sums)})
	}
	return cases
}

func streamingShapeCases(t *testing.T) []streamingReadCase {
	t.Helper()
	realms, tokens, unterminated := []byte("{\"id\":1}\n"), []byte("{\"id\":2}\n"), []byte(`{"id":1}`)
	fixtures := []struct {
		name    string
		tables  []string
		entries []tarEntry
		counts  map[string]int
	}{
		{"table_absent", []string{"realms"}, []tarEntry{{"realms/000001.ndjson", realms}, {"tokens/000001.ndjson", tokens}}, map[string]int{"realms": 1, "tokens": 1}},
		{"table_order", []string{"realms", "tokens"}, []tarEntry{{"tokens/000001.ndjson", tokens}, {"realms/000001.ndjson", realms}}, map[string]int{"realms": 1, "tokens": 1}},
		{"chunk_restarts", []string{"realms"}, []tarEntry{{"realms/000001.ndjson", realms}, {"realms/000001.ndjson", realms}}, map[string]int{"realms": 2}},
		{"chunk_skips", []string{"realms"}, []tarEntry{{"realms/000001.ndjson", realms}, {"realms/000003.ndjson", realms}}, map[string]int{"realms": 2}},
		{"unterminated_row", []string{"realms"}, []tarEntry{{"realms/000001.ndjson", unterminated}}, map[string]int{"realms": 1}},
		// The original shape fixture has no complete rows before its fragment.
		// This extra case pins the oracle's nonempty prefix at a clean EOF.
		{"complete_then_unterminated_row", []string{"realms"}, []tarEntry{{"realms/000001.ndjson", append(bytes.Clone(realms), unterminated...)}}, map[string]int{"realms": 2}},
	}
	var cases []streamingReadCase
	for _, f := range fixtures {
		sums := Checksums{TableRows: f.counts}
		for _, e := range f.entries {
			sums.Chunks = append(sums.Chunks, ChunkSum{Name: e.name, SHA256: sha256Hex(e.data), Bytes: len(e.data), Rows: 1})
		}
		m := Manifest{FormatVersion: FormatVersion, SchemaVersion: 13, AccountID: "acc_s", Status: "suspended", Tables: f.tables}
		cases = append(cases, streamingReadCase{name: "shape/" + f.name, archive: buildHandArchive(t, m, f.entries, sums)})
	}
	return cases
}

func TestReadCallbackErrorPrecedesLaterCorruption(t *testing.T) {
	archive := streamingCutCases(t)[0].archive
	for _, fail := range []bool{true, false} {
		t.Run(fmt.Sprintf("callback_error_%t", fail), func(t *testing.T) {
			sentinel := errors.New("synthetic database callback failure")
			calls, completions := 0, 0
			_, err := Read(context.Background(), bytes.NewReader(archive), ImportOptions{
				CurrentSchema: 13,
				Row: func(table string, _ []byte) error {
					if table != "realms" {
						t.Fatal("later entry reached after truncated realms entry")
					}
					calls++
					if fail {
						return sentinel
					}
					return nil
				},
				OnEntry: func(EntryStats) { completions++ },
			})
			if fail {
				if err != sentinel || errors.Is(err, ErrCorrupt) || calls != 1 {
					t.Fatalf("callback precedence: err=%v calls=%d", err, calls)
				}
			} else if !errors.Is(err, ErrCorrupt) || calls != 2 {
				t.Fatalf("truncated entry: err=%v calls=%d, want ErrCorrupt and two complete rows", err, calls)
			}
			if completions != 0 {
				t.Fatal("failing entry emitted OnEntry")
			}
		})
	}
	largeArchive := streamingStopArchive(t, true)
	for _, stop := range []int{1, 2} {
		t.Run(fmt.Sprintf("no_further_read/row_%d", stop), func(t *testing.T) {
			input := &streamingCountingReader{reader: bytes.NewReader(largeArchive)}
			sentinel := errors.New("synthetic database callback failure")
			calls, completions, consumedAtStop := 0, 0, 0
			_, err := Read(context.Background(), input, ImportOptions{
				CurrentSchema: 13,
				Row: func(string, []byte) error {
					calls++
					if calls == stop {
						consumedAtStop = input.bytes
						return sentinel
					}
					return nil
				},
				OnEntry: func(EntryStats) { completions++ },
			})
			if err != sentinel || errors.Is(err, ErrCorrupt) || calls != stop || completions != 0 {
				t.Fatalf("callback row %d: err=%v delivered=%d completed=%d", stop, err, calls, completions)
			}
			requireStreamingStoppedReading(t, input.bytes, consumedAtStop, len(largeArchive))
		})
	}
}

type streamingCountingReader struct {
	reader io.Reader
	bytes  int
}

func (r *streamingCountingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += n
	return n, err
}

// Keep several MiB of incompressible rows beyond both the fast and assembled
// stop rows. A drain must consume compressed input, not just buffered output.
func streamingStopArchive(t *testing.T, truncated bool) []byte {
	t.Helper()
	var rows [][]byte
	for _, size := range []int{23, entryReadBufferBytes + 3, 25, 27, entryReadBufferBytes + 5, 29} {
		rows = append(rows, streamingTestRow(size))
	}
	random := rand.NewChaCha8([32]byte{111})
	var content [48 << 10]byte
	for i := 0; i < 96; i++ {
		_, _ = random.Read(content[:])
		rows = append(rows, []byte(`{"body":"`+base64.StdEncoding.EncodeToString(content[:])+`"}`))
	}
	archive := writeStreamingTestArchive(t, Manifest{SchemaVersion: 13}, &streamingTestSource{table: "realms", rows: rows})
	if truncated {
		raw := gunzip(t, archive)
		_, start, size := streamingTarOffsets(t, raw, "realms/000001.ndjson")
		archive = regzip(t, raw[:start+size-len(rows[len(rows)-1])/2])
	}
	if len(archive) < 4<<20 {
		t.Fatal("stop fixture must retain at least 4 MiB of compressed data")
	}
	return archive
}

func requireStreamingStoppedReading(t *testing.T, consumed, atStop, archiveSize int) {
	t.Helper()
	t.Logf("compressed bytes: at_stop=%d returned=%d archive=%d", atStop, consumed, archiveSize)
	if consumed != atStop {
		t.Errorf("read after callback stopped the entry: consumed=%d, at callback=%d", consumed, atStop)
	}
	if consumed >= archiveSize/2 {
		t.Errorf("consumed %d compressed bytes, must stay below half of %d", consumed, archiveSize)
	}
}

func TestReadRowCancellationStopsEntry(t *testing.T) {
	archive := streamingStopArchive(t, false)
	for _, stop := range []int{1, 2} {
		t.Run(fmt.Sprintf("row_%d", stop), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls, completed, consumedAtStop := 0, 0, 0
			input := &streamingCountingReader{reader: bytes.NewReader(archive)}
			_, err := Read(ctx, input, ImportOptions{
				CurrentSchema: 13,
				Row: func(string, []byte) error {
					calls++
					if calls == stop {
						consumedAtStop = input.bytes
						cancel()
					}
					return nil
				},
				OnEntry: func(EntryStats) { completed++ },
			})
			if !errors.Is(err, context.Canceled) || errors.Is(err, ErrCorrupt) || calls != stop || completed != 0 {
				t.Fatalf("cancel row %d: err=%v delivered=%d completed=%d", stop, err, calls, completed)
			}
			requireStreamingStoppedReading(t, input.bytes, consumedAtStop, len(archive))
		})
	}
}

func TestEntryRowReaderRetainsOnlyLargestRow(t *testing.T) {
	r := newEntryRowReader()
	for _, tc := range []struct {
		name    string
		sizes   []int
		wantCap int
	}{
		{"retain_exact_largest", []int{300 << 10, 700 << 10, 500 << 10}, 700 << 10},
		{"release_large_row", []int{1536 << 10}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rows [][]byte
			var content bytes.Buffer
			for _, size := range tc.sizes {
				row := streamingTestRow(size)
				rows = append(rows, row)
				content.Write(row)
				content.WriteByte('\n')
			}
			var raw bytes.Buffer
			tw := tar.NewWriter(&raw)
			writeTarEntry(t, tw, "realms/000001.ndjson", content.Bytes())
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			tr := tar.NewReader(&raw)
			hdr, err := tr.Next()
			if err != nil {
				t.Fatal(err)
			}
			i := 0
			err = r.readEntry(tr, hdr, func(row []byte) error {
				if i >= len(rows) || !bytes.Equal(row, rows[i]) {
					t.Fatal("assembled row differs from source")
				}
				i++
				return nil
			})
			if err != nil || i != len(rows) {
				t.Fatalf("entry err=%v rows=%d want=%d", err, i, len(rows))
			}
			if cap(r.rowBuf) != tc.wantCap || len(r.rowBuf) != 0 {
				t.Fatalf("retained row buffer len=%d cap=%d, want len=0 cap=%d", len(r.rowBuf), cap(r.rowBuf), tc.wantCap)
			}
			if tc.wantCap == 0 && r.rowBuf != nil {
				t.Fatal("large row buffer was not released")
			}
		})
	}
}
