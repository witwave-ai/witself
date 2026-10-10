package export

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"testing"
)

// streamingBoundaryCases exercises the distinction between an entry's claimed
// size and the bytes TAR actually delivers, including errors accompanying its
// final bytes. These cases use the same error-text and EntryStats comparisons
// with readWholeEntryOracle as the rest of the differential corpus.
func streamingBoundaryCases(t *testing.T) (valid, corrupt []streamingReadCase) {
	t.Helper()
	const name = "realms/000001.ndjson"
	m := Manifest{FormatVersion: FormatVersion, SchemaVersion: 13, Tables: []string{"realms"}}
	for _, typeflag := range []byte{tar.TypeLink, tar.TypeSymlink, tar.TypeDir, tar.TypeFifo} {
		for _, claimed := range []int{0, 7} {
			// Use a regular zero-byte entry then patch its header: tar.Writer
			// normalizes header-only sizes and would hide the malformed claim.
			sums := Checksums{
				Chunks:    []ChunkSum{{Name: name, SHA256: sha256Hex(nil), Bytes: claimed, Rows: 0}},
				TableRows: map[string]int{"realms": 0},
			}
			raw := gunzip(t, buildHandArchive(t, m, []tarEntry{{name: name}}, sums))
			header, _, _ := streamingTarOffsets(t, raw, name)
			raw[header+156] = typeflag
			streamingSetTarSize(raw[header:header+512], int64(claimed))
			archive := regzip(t, raw)
			streamingCheckHeaderOnlyEntry(t, archive, name, typeflag, claimed)
			tc := streamingReadCase{
				name: fmt.Sprintf("header_only_type_%c_claimed_%d", typeflag, claimed), archive: archive, schema: 13,
			}
			if claimed == 0 {
				valid = append(valid, tc)
			} else {
				corrupt = append(corrupt, tc)
			}
		}
	}

	rows := [][]byte{[]byte(`{"id":1}`), []byte(`{"id":2}`), []byte(`{"id":3}`)}
	canonical := writeStreamingTestArchive(t, Manifest{SchemaVersion: 13},
		&streamingTestSource{table: "realms", rows: rows})
	raw := gunzip(t, canonical)
	_, payload, size := streamingTarOffsets(t, raw, name)
	end := payload + size
	first := regzip(t, raw[:end])
	second := regzip(t, raw[end:])
	badCRC := bytes.Clone(first)
	badCRC[len(badCRC)-8] ^= 1
	join := func(a, b []byte) []byte { return append(bytes.Clone(a), b...) }
	memberCases := []struct {
		name    string
		archive []byte
		wantErr error
	}{
		{"member_boundary_footer_dropped", first[:len(first)-8], io.ErrUnexpectedEOF},
		{"member_boundary_bad_crc_then_valid_member", join(badCRC, second), gzip.ErrChecksum},
		{"member_boundary_garbage_after_member", join(first, bytes.Repeat([]byte{'x'}, 16)), gzip.ErrHeader},
	}
	for _, tc := range memberCases {
		// Pin the reproduction itself: the error must accompany the chunk's
		// final bytes, rather than an earlier row or later TAR padding read.
		streamingCheckFinalBytesError(t, tc.archive, name, size, tc.wantErr)
		corrupt = append(corrupt, streamingReadCase{name: tc.name, archive: tc.archive, schema: 13})
	}

	// Unlike the header-only cases, this regular-file header has a real data
	// section. Its first row is complete, but the TAR ends inside row two.
	cut := payload + len(rows[0]) + 1 + len(rows[1])/2
	corrupt = append(corrupt, streamingReadCase{
		name: "regular_entry_tar_cut_mid_data", archive: regzip(t, raw[:cut]), schema: 13, cut: true,
	})
	return valid, corrupt
}

func streamingCheckHeaderOnlyEntry(t *testing.T, archive []byte, name string, typeflag byte, claimed int) {
	t.Helper()
	tr := streamingBoundaryTarReader(t, archive)
	hdr := streamingBoundaryFindEntry(t, tr, name)
	if hdr.Typeflag != typeflag || hdr.Size != int64(claimed) {
		t.Fatal("header-only fixture lost its typeflag or claimed size")
	}
	var one [1]byte
	if n, err := tr.Read(one[:]); n != 0 || err != io.EOF {
		t.Fatalf("header-only fixture delivered %d bytes with error %v, want zero bytes and EOF", n, err)
	}
}

func streamingCheckFinalBytesError(t *testing.T, archive []byte, name string, size int, wantErr error) {
	t.Helper()
	tr := streamingBoundaryTarReader(t, archive)
	streamingBoundaryFindEntry(t, tr, name)
	n, err := tr.Read(make([]byte, size))
	if n != size || !errors.Is(err, wantErr) {
		t.Fatalf("member-boundary fixture delivered %d bytes with error %v, want %d bytes and %v", n, err, size, wantErr)
	}
}

func streamingBoundaryTarReader(t *testing.T, archive []byte) *tar.Reader {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	})
	return tar.NewReader(gz)
}

func streamingBoundaryFindEntry(t *testing.T, tr *tar.Reader, name string) *tar.Header {
	t.Helper()
	for {
		hdr, err := tr.Next()
		if err != nil {
			t.Fatalf("boundary fixture is missing %s: %v", name, err)
		}
		if hdr.Name == name {
			return hdr
		}
	}
}
