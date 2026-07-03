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
	"strconv"
	"strings"
)

// ErrArchiveTooNew is returned when the archive's schema version is newer
// than the destination's — restoring forward-written data would need a
// downgrader, which we refuse to improvise.
var ErrArchiveTooNew = errors.New("archive schema is newer than this cell")

// ErrCorrupt wraps every structural failure of the archive itself — bad
// layout, checksum mismatch, truncation. Errors from the caller's callbacks
// pass through unwrapped, so a database failure is distinguishable from a
// damaged archive.
var ErrCorrupt = errors.New("corrupt archive")

// maxChunkBytes caps how much one tar entry may claim. The writer bounds
// chunks at chunkSize plus at most one oversized row; anything past this is
// a hostile or damaged archive, refused before allocation.
const maxChunkBytes = 1 << 30 // 1 GiB

// ImportOptions parameterizes Read.
type ImportOptions struct {
	// CurrentSchema is the destination's schema version. Rows written at an
	// older schema are lifted through the upgrader chain; a newer archive
	// refuses with ErrArchiveTooNew.
	CurrentSchema int
	// OnManifest, when set, runs as soon as the manifest is decoded — before
	// any row is delivered. Returning an error aborts the read; use it for
	// cheap preconditions (account collision, id mismatch) so a bad import
	// stops before streaming gigabytes.
	OnManifest func(Manifest) error
	// Row receives each table row, post-upgrade, in archive order. The
	// archive writes tables in foreign-key dependency order, so inserting
	// rows as they arrive satisfies references without buffering.
	Row func(table string, row []byte) error
}

// Read streams an archive from r, verifying its structure and trailing
// checksums. Rows are delivered to opts.Row as they are decoded; integrity
// is only fully proven at the end, so callers MUST stage everything in a
// transaction and commit only when Read returns nil — nothing may be
// considered landed before that.
func Read(ctx context.Context, r io.Reader, opts ImportOptions) (Manifest, error) {
	var m Manifest

	gz, err := gzip.NewReader(r)
	if err != nil {
		return m, fmt.Errorf("%w: not a gzip stream: %v", ErrCorrupt, err)
	}
	tr := tar.NewReader(gz)

	// The manifest must lead.
	hdr, err := tr.Next()
	if err != nil {
		return m, fmt.Errorf("%w: missing manifest: %v", ErrCorrupt, err)
	}
	if hdr.Name != "manifest.json" {
		return m, fmt.Errorf("%w: first entry is %q, want manifest.json", ErrCorrupt, hdr.Name)
	}
	raw, err := readEntry(tr, hdr)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("%w: manifest: %v", ErrCorrupt, err)
	}
	if m.FormatVersion != FormatVersion {
		return m, fmt.Errorf("%w: format version %d, this reader speaks %d", ErrCorrupt, m.FormatVersion, FormatVersion)
	}
	if m.SchemaVersion > opts.CurrentSchema {
		return m, fmt.Errorf("%w: archive schema %d > cell schema %d", ErrArchiveTooNew, m.SchemaVersion, opts.CurrentSchema)
	}
	if opts.OnManifest != nil {
		if err := opts.OnManifest(m); err != nil {
			return m, err
		}
	}

	tables := map[string]bool{}
	for _, t := range m.Tables {
		tables[t] = true
	}

	// Walk chunks, hashing each for the trailing-checksum comparison. Tables
	// must arrive in manifest order without interleaving, chunks numbered
	// from 1 — the writer's exact shape, pinned strictly.
	seen := map[string]ChunkSum{}
	rowsPerTable := map[string]int{}
	tableIdx := 0 // position in m.Tables of the table currently streaming
	nextChunk := 0
	var sums *Checksums
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		if hdr.Name == "checksums.json" {
			raw, err := readEntry(tr, hdr)
			if err != nil {
				return m, err
			}
			sums = &Checksums{}
			if err := json.Unmarshal(raw, sums); err != nil {
				return m, fmt.Errorf("%w: checksums: %v", ErrCorrupt, err)
			}
			if _, err := tr.Next(); !errors.Is(err, io.EOF) {
				return m, fmt.Errorf("%w: entries after checksums.json", ErrCorrupt)
			}
			break
		}

		table, chunkNo, err := parseChunkName(hdr.Name)
		if err != nil {
			return m, err
		}
		if !tables[table] {
			return m, fmt.Errorf("%w: chunk for table %q not in manifest", ErrCorrupt, table)
		}
		// Advance through the manifest's table order; going backwards or
		// jumping to an unlisted position means an interleaved or reordered
		// archive.
		for tableIdx < len(m.Tables) && m.Tables[tableIdx] != table {
			tableIdx++
			nextChunk = 0
		}
		if tableIdx == len(m.Tables) {
			return m, fmt.Errorf("%w: table %q out of manifest order", ErrCorrupt, table)
		}
		nextChunk++
		if chunkNo != nextChunk {
			return m, fmt.Errorf("%w: chunk %s out of sequence (want %06d)", ErrCorrupt, hdr.Name, nextChunk)
		}

		data, err := readEntry(tr, hdr)
		if err != nil {
			return m, err
		}
		sum := sha256.Sum256(data)
		rows := 0
		for len(data) > 0 {
			nl := bytes.IndexByte(data, '\n')
			if nl < 0 {
				return m, fmt.Errorf("%w: %s: unterminated row", ErrCorrupt, hdr.Name)
			}
			row := data[:nl]
			data = data[nl+1:]
			rows++
			row, err := upgradeRow(table, row, m.SchemaVersion, opts.CurrentSchema)
			if err != nil {
				return m, err
			}
			if row == nil {
				continue // upgrader dropped the row
			}
			if opts.Row != nil {
				if err := opts.Row(table, row); err != nil {
					return m, err
				}
			}
			if err := ctx.Err(); err != nil {
				return m, err
			}
		}
		seen[hdr.Name] = ChunkSum{
			Name:   hdr.Name,
			SHA256: hex.EncodeToString(sum[:]),
			Bytes:  int(hdr.Size),
			Rows:   rows,
		}
		rowsPerTable[table] += rows
	}
	if sums == nil {
		return m, fmt.Errorf("%w: truncated — no checksums.json", ErrCorrupt)
	}

	// Every chunk the trailer describes must have been seen byte-identical,
	// and every chunk the archive carried must be described. Track which
	// names in `seen` have actually been reconciled: a checksums.json that
	// lists one chunk twice would otherwise pass a naive count-equality
	// check while leaving a different real chunk unverified.
	verified := make(map[string]bool, len(seen))
	for _, want := range sums.Chunks {
		if verified[want.Name] {
			return m, fmt.Errorf("%w: checksummed chunk %s listed twice", ErrCorrupt, want.Name)
		}
		got, ok := seen[want.Name]
		if !ok {
			return m, fmt.Errorf("%w: checksummed chunk %s missing", ErrCorrupt, want.Name)
		}
		if got.SHA256 != want.SHA256 || got.Bytes != want.Bytes || got.Rows != want.Rows {
			return m, fmt.Errorf("%w: chunk %s does not match its checksum", ErrCorrupt, want.Name)
		}
		verified[want.Name] = true
	}
	for name := range seen {
		if !verified[name] {
			return m, fmt.Errorf("%w: chunk %s carried but not checksummed", ErrCorrupt, name)
		}
	}
	for table, want := range sums.TableRows {
		if !tables[table] {
			return m, fmt.Errorf("%w: checksums count rows for unknown table %q", ErrCorrupt, table)
		}
		if rowsPerTable[table] != want {
			return m, fmt.Errorf("%w: table %s has %d rows, checksums say %d", ErrCorrupt, table, rowsPerTable[table], want)
		}
	}
	for _, table := range m.Tables {
		if _, ok := sums.TableRows[table]; !ok {
			return m, fmt.Errorf("%w: table %s missing from checksums", ErrCorrupt, table)
		}
	}
	return m, nil
}

// upgradeRow lifts one row from the archive's schema to the destination's.
// The chunk hash was computed over the raw bytes, so upgrades apply strictly
// after integrity; a decode round-trip only happens when some step in the
// range actually registered an upgrader.
func upgradeRow(table string, row []byte, from, to int) ([]byte, error) {
	needed := false
	for v := from; v < to; v++ {
		if UpgraderFor(v) != nil {
			needed = true
			break
		}
	}
	if !needed {
		return row, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(row, &obj); err != nil {
		return nil, fmt.Errorf("%w: %s row: %v", ErrCorrupt, table, err)
	}
	for v := from; v < to; v++ {
		up := UpgraderFor(v)
		if up == nil {
			continue
		}
		var err error
		obj, err = up(table, obj)
		if err != nil {
			return nil, fmt.Errorf("upgrade %s row from schema %d: %w", table, v, err)
		}
		if obj == nil {
			return nil, nil
		}
	}
	return json.Marshal(obj)
}

func readEntry(tr *tar.Reader, hdr *tar.Header) ([]byte, error) {
	if hdr.Size > maxChunkBytes {
		return nil, fmt.Errorf("%w: entry %s claims %d bytes", ErrCorrupt, hdr.Name, hdr.Size)
	}
	data := make([]byte, hdr.Size)
	if _, err := io.ReadFull(tr, data); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, hdr.Name, err)
	}
	return data, nil
}

func parseChunkName(name string) (table string, chunkNo int, err error) {
	i := strings.IndexByte(name, '/')
	if i <= 0 || !strings.HasSuffix(name, ".ndjson") {
		return "", 0, fmt.Errorf("%w: unexpected entry %q", ErrCorrupt, name)
	}
	table = name[:i]
	numPart := strings.TrimSuffix(name[i+1:], ".ndjson")
	n, aerr := strconv.Atoi(numPart)
	if aerr != nil || n < 1 || len(numPart) != 6 {
		return "", 0, fmt.Errorf("%w: unexpected entry %q", ErrCorrupt, name)
	}
	return table, n, nil
}
