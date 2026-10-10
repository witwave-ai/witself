package export

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// readWholeEntryOracle is the verbatim Read implementation from b4d952af.
// Keep this oracle unchanged; only its name and readEntry references differ.
func readWholeEntryOracle(ctx context.Context, r io.Reader, opts ImportOptions) (Manifest, error) {
	var m Manifest

	// Preserve gzip's buffering choice, but allow completion to check context
	// between compressed reads, including loops over empty gzip members.
	compressed, ok := r.(flate.Reader)
	if !ok {
		compressed = bufio.NewReader(r)
	}
	input := &archiveCompletionReader{reader: compressed}
	gz, err := gzip.NewReader(input)
	if err != nil {
		return m, fmt.Errorf("%w: not a gzip stream: %v", ErrCorrupt, err)
	}
	defer func() { _ = gz.Close() }() // Close cannot verify integrity; preserve the read/callback error.
	tr := tar.NewReader(gz)

	// The manifest must lead.
	hdr, err := tr.Next()
	if err != nil {
		return m, fmt.Errorf("%w: missing manifest: %v", ErrCorrupt, err)
	}
	if hdr.Name != "manifest.json" {
		return m, fmt.Errorf("%w: first entry is %q, want manifest.json", ErrCorrupt, hdr.Name)
	}
	raw, err := readEntryOracle(tr, hdr)
	if err != nil {
		return m, err
	}
	if err := rejectAmbiguousArchiveJSON(raw); err != nil {
		return m, fmt.Errorf("%w: manifest: %v", ErrCorrupt, err)
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
			raw, err := readEntryOracle(tr, hdr)
			if err != nil {
				return m, err
			}
			sums = &Checksums{}
			if err := rejectAmbiguousArchiveJSON(raw); err != nil {
				return m, fmt.Errorf("%w: checksums: %v", ErrCorrupt, err)
			}
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

		if opts.OnEntryStart != nil {
			opts.OnEntryStart(EntryStart{Table: table, Chunk: chunkNo, EntryBytes: hdr.Size})
		}
		data, err := readEntryOracle(tr, hdr)
		if err != nil {
			return m, err
		}
		sum := sha256.Sum256(data)
		rows := 0
		stats := EntryStats{Table: table, Chunk: chunkNo, EntryBytes: hdr.Size}
		for len(data) > 0 {
			nl := bytes.IndexByte(data, '\n')
			if nl < 0 {
				return m, fmt.Errorf("%w: %s: unterminated row", ErrCorrupt, hdr.Name)
			}
			row := data[:nl]
			data = data[nl+1:]
			rows++
			if len(row) > stats.LargestRowBytes {
				stats.LargestRowBytes = len(row)
			}
			if len(row) >= 1<<20 {
				stats.LargeRows++
			}
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
		if opts.OnEntry != nil {
			stats.Rows = rows
			opts.OnEntry(stats)
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
	// TAR's end blocks do not prove that gzip's footer is valid. Finish the
	// compressed stream, retaining default multistream and ignored TAR tails.
	// Arm cancellation only here so earlier callback/structural errors keep
	// their existing precedence. Memory is fixed; work scales with the tail.
	input.ctx = ctx
	var tail [32 << 10]byte
	for {
		if err := ctx.Err(); err != nil {
			return m, err
		}
		_, err := gz.Read(tail[:])
		if ctxErr := ctx.Err(); ctxErr != nil {
			return m, ctxErr
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, fmt.Errorf("%w: gzip completion: %v", ErrCorrupt, err)
		}
	}
	return m, nil
}

// readEntryOracle is the verbatim readEntry implementation from b4d952af.
func readEntryOracle(tr *tar.Reader, hdr *tar.Header) ([]byte, error) {
	if hdr.Size > maxChunkBytes {
		return nil, fmt.Errorf("%w: entry %s claims %d bytes", ErrCorrupt, hdr.Name, hdr.Size)
	}
	data := make([]byte, hdr.Size)
	if _, err := io.ReadFull(tr, data); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, hdr.Name, err)
	}
	return data, nil
}
