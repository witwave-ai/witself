package export

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// TestReadRoundTrip pins the import half of the format contract: every row
// written comes back, in table order, with the manifest intact. The row
// count is deliberately past chunkSize (32 MiB / ~116-byte helper rows =
// 287 741 rows) so the same-table chunk-sequencing branch — the strictest
// part of the reader — actually runs.
func TestReadRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	m := Manifest{SchemaVersion: 13, ServerVersion: "0.0.83", AccountID: "acc_rt", Status: "suspended"}
	if err := Write(context.Background(), &buf, m,
		[]RowSource{newSource("realms", 3), newSource("audit", 300_000)}); err != nil {
		t.Fatal(err)
	}

	var manifestSeen bool
	rows := map[string]int{}
	var order []string
	got, err := Read(context.Background(), &buf, ImportOptions{
		CurrentSchema: 13,
		OnManifest: func(mm Manifest) error {
			manifestSeen = true
			if rows["realms"]+rows["audit"] != 0 {
				t.Error("OnManifest ran after rows were delivered")
			}
			if mm.AccountID != "acc_rt" || mm.SchemaVersion != 13 {
				t.Errorf("manifest = %+v", mm)
			}
			return nil
		},
		Row: func(table string, row []byte) error {
			if len(order) == 0 || order[len(order)-1] != table {
				order = append(order, table)
			}
			var obj map[string]any
			if err := json.Unmarshal(row, &obj); err != nil {
				return fmt.Errorf("row not JSON: %w", err)
			}
			rows[table]++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !manifestSeen {
		t.Error("OnManifest never ran")
	}
	if rows["realms"] != 3 || rows["audit"] != 300_000 {
		t.Errorf("row counts = %v", rows)
	}
	if len(order) != 2 || order[0] != "realms" || order[1] != "audit" {
		t.Errorf("table order = %v, want [realms audit]", order)
	}
	if got.Status != "suspended" {
		t.Errorf("manifest status = %q", got.Status)
	}
}

// TestReadDetectsTamper — flip one byte inside a chunk and the trailing
// checksums must catch it before Read returns success.
func TestReadDetectsTamper(t *testing.T) {
	archive := buildArchive(t, 13, "acc_tamper")

	// Rebuild the archive with one chunk byte flipped: decompress, patch the
	// raw tar bytes inside a chunk's payload, recompress. The tar structure
	// stays valid; only the content lies.
	raw := gunzip(t, archive)
	i := bytes.Index(raw, []byte(`"i":1`))
	if i < 0 {
		t.Fatal("marker row not found in tar bytes")
	}
	raw[i+4] = '7' // row now claims i=7; chunk hash no longer matches
	var err error
	_, err = Read(context.Background(), bytes.NewReader(regzip(t, raw)), ImportOptions{CurrentSchema: 13})
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("tampered archive error = %v, want ErrCorrupt", err)
	}
}

// TestReadDetectsTruncation — a stream cut before the trailing entry must
// refuse, whatever layer notices first.
func TestReadDetectsTruncation(t *testing.T) {
	archive := buildArchive(t, 13, "acc_trunc")
	for _, cut := range []int{100, len(archive) / 2} {
		_, err := Read(context.Background(), bytes.NewReader(archive[:len(archive)-cut]),
			ImportOptions{CurrentSchema: 13})
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("truncated by %d: error = %v, want ErrCorrupt", cut, err)
		}
	}
}

// TestReadRefusesNewerSchema — an archive from the future needs a newer cell,
// not a lossy improvisation.
func TestReadRefusesNewerSchema(t *testing.T) {
	archive := buildArchive(t, 14, "acc_new")
	_, err := Read(context.Background(), bytes.NewReader(archive), ImportOptions{CurrentSchema: 13})
	if !errors.Is(err, ErrArchiveTooNew) {
		t.Errorf("error = %v, want ErrArchiveTooNew", err)
	}
}

// TestReadOnManifestAborts — a precondition failure stops the read before
// any row is delivered (the cheap-abort contract for collision checks).
func TestReadOnManifestAborts(t *testing.T) {
	archive := buildArchive(t, 13, "acc_abort")
	sentinel := errors.New("account already exists")
	rowsDelivered := 0
	_, err := Read(context.Background(), bytes.NewReader(archive), ImportOptions{
		CurrentSchema: 13,
		OnManifest:    func(Manifest) error { return sentinel },
		Row:           func(string, []byte) error { rowsDelivered++; return nil },
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the OnManifest sentinel", err)
	}
	if rowsDelivered != 0 {
		t.Errorf("%d rows delivered after OnManifest refused", rowsDelivered)
	}
}

// TestReadAppliesUpgraders — rows written at an older schema are lifted
// through the chain; hashes still verify because integrity is checked on the
// raw bytes before transformation.
func TestReadAppliesUpgraders(t *testing.T) {
	archive := buildArchive(t, 13, "acc_up")

	upgraders[13] = func(_ string, row map[string]any) (map[string]any, error) {
		if i, ok := row["i"].(float64); ok && int(i) == 0 {
			return nil, nil // drop row 0 — exercises the drop contract
		}
		row["lifted"] = true
		return row, nil
	}
	t.Cleanup(func() { delete(upgraders, 13) })

	var kept int
	_, err := Read(context.Background(), bytes.NewReader(archive), ImportOptions{
		CurrentSchema: 14,
		Row: func(_ string, row []byte) error {
			var obj map[string]any
			if err := json.Unmarshal(row, &obj); err != nil {
				return err
			}
			if obj["lifted"] != true {
				return fmt.Errorf("row not lifted: %s", row)
			}
			kept++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if kept != 4 { // 5 rows written, row 0 dropped
		t.Errorf("kept = %d, want 4", kept)
	}
}

// TestReadRejectsStrayEntries — an entry that is neither the manifest, a
// well-formed chunk, nor the trailing checksums is a foreign object. Built
// by hand with the tar writer so the header checksums stay valid and the
// name check itself is what refuses.
func TestReadRejectsStrayEntries(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	manifest, _ := json.Marshal(Manifest{
		FormatVersion: FormatVersion, SchemaVersion: 13,
		AccountID: "acc_stray", Status: "suspended", Tables: []string{"realms"},
	})
	for _, e := range []struct {
		name string
		data []byte
	}{
		{"manifest.json", manifest},
		{"stray.txt", []byte("who put this here")},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(e.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), ImportOptions{CurrentSchema: 13})
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("stray entry error = %v, want ErrCorrupt", err)
	}
}

// TestReadRowErrorPassesThrough — sink failures (a database refusing a row)
// must surface unwrapped, distinguishable from archive corruption.
func TestReadRowErrorPassesThrough(t *testing.T) {
	archive := buildArchive(t, 13, "acc_sink")
	dbErr := errors.New("duplicate key value violates unique constraint")
	_, err := Read(context.Background(), bytes.NewReader(archive), ImportOptions{
		CurrentSchema: 13,
		Row:           func(string, []byte) error { return dbErr },
	})
	if !errors.Is(err, dbErr) {
		t.Errorf("error = %v, want the sink error", err)
	}
	if errors.Is(err, ErrCorrupt) {
		t.Error("sink error was wrapped as archive corruption")
	}
}

// TestReadDetectsChecksumTrailerTamperClasses pins every refusal path in
// the trailing-checksums verification: it is the sole integrity gate the
// import transaction leans on, and until now only the SHA256 half of one
// path was tested. Each subtest hand-builds a MINIMAL archive whose
// non-integrity structure is valid, then makes exactly one integrity
// invariant lie — so a regression that drops any of these checks would
// silently commit hostile rows on the caller side.
func TestReadDetectsChecksumTrailerTamperClasses(t *testing.T) {
	// Base: one realms chunk with two rows. Every case forks from this
	// pair (manifest, chunk-bytes, chunkSum) and rewrites the trailer.
	realmsChunk := []byte(`{"table":"realms","id":1}` + "\n" + `{"table":"realms","id":2}` + "\n")
	manifest := Manifest{
		FormatVersion: FormatVersion, SchemaVersion: 13,
		AccountID: "acc_tc", Status: "suspended", Tables: []string{"realms"},
	}
	trueChunkSum := ChunkSum{
		Name: "realms/000001.ndjson", SHA256: sha256Hex(realmsChunk),
		Bytes: len(realmsChunk), Rows: 2,
	}

	tests := []struct {
		name  string
		sums  Checksums
		extra []tarEntry // extra chunk entries beyond realms/000001
		want  string
	}{
		{
			name:  "extra chunk carried, checksums cover only the first",
			sums:  Checksums{Chunks: []ChunkSum{trueChunkSum}, TableRows: map[string]int{"realms": 2}},
			extra: []tarEntry{{"realms/000002.ndjson", realmsChunk}},
			want:  "carried but not checksummed",
		},
		{
			name: "checksummed chunk missing from the tar",
			sums: Checksums{Chunks: []ChunkSum{trueChunkSum,
				{Name: "realms/000002.ndjson", SHA256: sha256Hex(realmsChunk), Bytes: len(realmsChunk), Rows: 2}},
				TableRows: map[string]int{"realms": 4}},
			want: "missing",
		},
		{
			name: "checksummed chunk listed twice — one real chunk escapes verification",
			sums: Checksums{Chunks: []ChunkSum{trueChunkSum, trueChunkSum},
				TableRows: map[string]int{"realms": 4}},
			extra: []tarEntry{{"realms/000002.ndjson", []byte(`{"tampered":true}` + "\n")}},
			want:  "listed twice",
		},
		{
			name: "bytes lie in the trailer",
			sums: Checksums{
				Chunks:    []ChunkSum{{Name: trueChunkSum.Name, SHA256: trueChunkSum.SHA256, Bytes: trueChunkSum.Bytes + 1, Rows: trueChunkSum.Rows}},
				TableRows: map[string]int{"realms": 2},
			},
			want: "does not match its checksum",
		},
		{
			name: "row count lies in the trailer",
			sums: Checksums{
				Chunks:    []ChunkSum{{Name: trueChunkSum.Name, SHA256: trueChunkSum.SHA256, Bytes: trueChunkSum.Bytes, Rows: trueChunkSum.Rows + 1}},
				TableRows: map[string]int{"realms": 3},
			},
			want: "does not match its checksum",
		},
		{
			name: "TableRows total disagrees with sum of chunk rows",
			sums: Checksums{Chunks: []ChunkSum{trueChunkSum}, TableRows: map[string]int{"realms": 99}},
			want: "checksums say",
		},
		{
			name: "TableRows names a table absent from the manifest",
			sums: Checksums{Chunks: []ChunkSum{trueChunkSum},
				TableRows: map[string]int{"realms": 2, "audit": 0}},
			want: "unknown table",
		},
		{
			name: "manifest lists a table missing from TableRows",
			sums: Checksums{Chunks: []ChunkSum{trueChunkSum},
				TableRows: map[string]int{}}, // realms zero-count omitted
			want: "missing from checksums",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			archive := buildHandArchive(t, manifest,
				append([]tarEntry{{trueChunkSum.Name, realmsChunk}}, tc.extra...), tc.sums)
			_, err := Read(context.Background(), bytes.NewReader(archive), ImportOptions{CurrentSchema: 13})
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("error = %v, want ErrCorrupt", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestReadEnforcesShape covers the ordering/interleaving/duplication and
// unterminated-row refusals. Together with the trailer test they pin the
// full "pinned strictly" claim in the reader's own doc comment.
func TestReadEnforcesShape(t *testing.T) {
	realmsChunk := []byte(`{"id":1}` + "\n")
	tokensChunk := []byte(`{"id":2}` + "\n")
	unterm := []byte(`{"id":1}`) // no trailing newline
	sumOne := func(name string, data []byte, rows int) ChunkSum {
		return ChunkSum{Name: name, SHA256: sha256Hex(data), Bytes: len(data), Rows: rows}
	}

	tests := []struct {
		name     string
		manifest Manifest
		entries  []tarEntry
		sums     Checksums
		want     string
	}{
		{
			name:     "chunk for table not in manifest",
			manifest: Manifest{FormatVersion: FormatVersion, SchemaVersion: 13, AccountID: "acc_s", Status: "suspended", Tables: []string{"realms"}},
			entries:  []tarEntry{{"realms/000001.ndjson", realmsChunk}, {"tokens/000001.ndjson", tokensChunk}},
			sums:     Checksums{TableRows: map[string]int{"realms": 1, "tokens": 1}},
			want:     "not in manifest",
		},
		{
			name:     "tables out of manifest order",
			manifest: Manifest{FormatVersion: FormatVersion, SchemaVersion: 13, AccountID: "acc_s", Status: "suspended", Tables: []string{"realms", "tokens"}},
			entries:  []tarEntry{{"tokens/000001.ndjson", tokensChunk}, {"realms/000001.ndjson", realmsChunk}},
			sums:     Checksums{TableRows: map[string]int{"realms": 1, "tokens": 1}},
			want:     "out of manifest order",
		},
		{
			name:     "chunk numbering restarts within a table (duplicate chunk 1)",
			manifest: Manifest{FormatVersion: FormatVersion, SchemaVersion: 13, AccountID: "acc_s", Status: "suspended", Tables: []string{"realms"}},
			entries:  []tarEntry{{"realms/000001.ndjson", realmsChunk}, {"realms/000001.ndjson", realmsChunk}},
			sums:     Checksums{TableRows: map[string]int{"realms": 2}},
			want:     "out of sequence",
		},
		{
			name:     "chunk numbering skips",
			manifest: Manifest{FormatVersion: FormatVersion, SchemaVersion: 13, AccountID: "acc_s", Status: "suspended", Tables: []string{"realms"}},
			entries:  []tarEntry{{"realms/000001.ndjson", realmsChunk}, {"realms/000003.ndjson", realmsChunk}},
			sums:     Checksums{TableRows: map[string]int{"realms": 2}},
			want:     "out of sequence",
		},
		{
			name:     "unterminated row in a chunk",
			manifest: Manifest{FormatVersion: FormatVersion, SchemaVersion: 13, AccountID: "acc_s", Status: "suspended", Tables: []string{"realms"}},
			entries:  []tarEntry{{"realms/000001.ndjson", unterm}},
			sums:     Checksums{Chunks: []ChunkSum{sumOne("realms/000001.ndjson", unterm, 1)}, TableRows: map[string]int{"realms": 1}},
			want:     "unterminated row",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.sums.Chunks) == 0 {
				for _, e := range tc.entries {
					// A stray-chunk case will refuse before checksum
					// verification, so only include those the reader
					// actually reaches. Real chunks get real hashes.
					table, _, err := parseChunkName(e.name)
					if err == nil {
						_ = table
						tc.sums.Chunks = append(tc.sums.Chunks, sumOne(e.name, e.data, 1))
					}
				}
			}
			archive := buildHandArchive(t, tc.manifest, tc.entries, tc.sums)
			_, err := Read(context.Background(), bytes.NewReader(archive), ImportOptions{CurrentSchema: 13})
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("error = %v, want ErrCorrupt", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestReadRefusesUnknownFormatVersion — the format-compat boundary for
// long-lived cold-storage archives. A v2 layout must refuse in a v1 reader
// rather than being interpreted through v1 assumptions.
func TestReadRefusesUnknownFormatVersion(t *testing.T) {
	manifest := Manifest{
		FormatVersion: FormatVersion + 1, SchemaVersion: 13,
		AccountID: "acc_fv", Status: "suspended", Tables: []string{},
	}
	archive := buildHandArchive(t, manifest, nil,
		Checksums{TableRows: map[string]int{}})
	_, err := Read(context.Background(), bytes.NewReader(archive), ImportOptions{CurrentSchema: 13})
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("error = %v, want ErrCorrupt", err)
	}
	if !strings.Contains(err.Error(), "format version") {
		t.Errorf("error message does not mention format version: %v", err)
	}
}

// sha256Hex returns the hex digest of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type tarEntry struct {
	name string
	data []byte
}

// buildHandArchive writes a valid gzip+tar with the caller's exact manifest,
// chunk entries, and checksums, in the exact order the reader expects
// (manifest first, chunks in the order given, checksums last). Used by
// every "tamper this one thing" test that would otherwise need to fake
// integrity too.
func buildHandArchive(t *testing.T, manifest Manifest, chunks []tarEntry, sums Checksums) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTarEntry(t, tw, "manifest.json", manifestJSON)
	for _, c := range chunks {
		writeTarEntry(t, tw, c.name, c.data)
	}
	sumsJSON, err := json.Marshal(sums)
	if err != nil {
		t.Fatal(err)
	}
	writeTarEntry(t, tw, "checksums.json", sumsJSON)

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeTarEntry(t *testing.T, tw *tar.Writer, name string, data []byte) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
}

// --- helpers ---

// buildArchive writes a small two-table archive at the given schema version.
// realms has 5 rows, tokens is empty (recorded but chunkless).
func buildArchive(t *testing.T, schema int, accountID string) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := Manifest{SchemaVersion: schema, AccountID: accountID, Status: "suspended"}
	if err := Write(context.Background(), &buf, m,
		[]RowSource{newSource("realms", 5), newSource("tokens", 0)}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gunzip(t *testing.T, data []byte) []byte {
	t.Helper()
	gzr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(gzr)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func regzip(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	if _, err := gzw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
