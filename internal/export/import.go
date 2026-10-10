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
	"hash"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/jsonstrict"
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

// EntryStart identifies a checked chunk header before its contents are read.
type EntryStart struct {
	Table      string
	Chunk      int
	EntryBytes int64
}

// EntryStats describes a completed chunk's archived, pre-upgrade rows.
type EntryStats struct {
	Table           string
	Chunk           int
	EntryBytes      int64
	Rows            int
	LargestRowBytes int
	LargeRows       int
}

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
	// OnEntryStart runs after chunk name/order checks and before reading it.
	// OnEntry runs only after all rows in that chunk complete successfully.
	// Neither callback receives row contents or changes validation decisions.
	OnEntryStart func(EntryStart)
	OnEntry      func(EntryStats)
	// Row receives each table row, post-upgrade, in archive order. The
	// archive writes tables in foreign-key dependency order, so inserting
	// rows as they arrive satisfies references without buffering.
	// The row slice is valid only until Row returns: Read reuses its memory
	// for later rows. A callback retaining any part must copy it (bytes.Clone).
	// Rows arrive while their entry streams in, before its checksum is known.
	// Before ErrCorrupt for a truncated or damaged entry, Row may have received
	// that entry's complete rows: only rows ending in a newline within the bytes
	// the archive delivered, in order. A trailing fragment is never delivered.
	// If Row returns an error, Read returns it unchanged and reads no further.
	Row func(table string, row []byte) error
}

// Read streams an archive from r, verifying its structure and trailing
// checksums. Rows are delivered to opts.Row as they are decoded; integrity
// is only fully proven at the end, so callers MUST stage everything in a
// transaction and commit only when Read returns nil — nothing may be
// considered landed before that. Chunk-reading memory is a fixed read buffer
// plus the largest row, not the entry size. A row longer than the read buffer
// briefly needs a second copy while it is assembled.
func Read(ctx context.Context, r io.Reader, opts ImportOptions) (Manifest, error) {
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
	raw, err := readEntry(tr, hdr)
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
	entryReader := newEntryRowReader()
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
		rows := 0
		stats := EntryStats{Table: table, Chunk: chunkNo, EntryBytes: hdr.Size}
		err = entryReader.readEntry(tr, hdr, func(row []byte) error {
			rows++
			if len(row) > stats.LargestRowBytes {
				stats.LargestRowBytes = len(row)
			}
			if len(row) >= 1<<20 {
				stats.LargeRows++
			}
			row, err := upgradeRow(table, row, m.SchemaVersion, opts.CurrentSchema)
			if err != nil {
				return err
			}
			if row == nil {
				return nil // upgrader dropped the row
			}
			if opts.Row != nil {
				if err := opts.Row(table, row); err != nil {
					return err
				}
			}
			return ctx.Err()
		})
		if err != nil {
			return m, err
		}
		if opts.OnEntry != nil {
			stats.Rows = rows
			opts.OnEntry(stats)
		}
		seen[hdr.Name] = ChunkSum{
			Name:   hdr.Name,
			SHA256: hex.EncodeToString(entryReader.hasher.Sum(nil)),
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

const entryReadBufferBytes = 256 << 10

// entrySizeReader preserves readEntry's io.ReadFull boundary semantics while
// streaming: a short entry fails, but an error accompanying its final bytes is
// left for tar.Reader.Next to report after the entry completes.
type entrySizeReader struct {
	reader io.Reader
	hdr    *tar.Header
	got    int64
}

func (r *entrySizeReader) Read(p []byte) (int, error) {
	if r.got == r.hdr.Size {
		return 0, io.EOF
	}
	if remaining := r.hdr.Size - r.got; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.reader.Read(p)
	r.got += int64(n)
	if r.got == r.hdr.Size {
		return n, nil
	}
	if err == io.EOF && r.got < r.hdr.Size {
		if r.got > 0 {
			err = io.ErrUnexpectedEOF
		}
		return n, fmt.Errorf("%w: %s: %v", ErrCorrupt, r.hdr.Name, err)
	}
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("%w: %s: %v", ErrCorrupt, r.hdr.Name, err)
	}
	return n, err
}

// entryRowReader belongs to one Read call. Only assembled rows use rowBuf;
// ordinary rows borrow the fixed bufio buffer for the duration of the callback.
type entryRowReader struct {
	reader *bufio.Reader
	rowBuf []byte
	pieces [][]byte
	hasher hash.Hash
}

func newEntryRowReader() *entryRowReader {
	return &entryRowReader{
		reader: bufio.NewReaderSize(nil, entryReadBufferBytes),
		hasher: sha256.New(),
	}
}

func (r *entryRowReader) readEntry(tr *tar.Reader, hdr *tar.Header, row func([]byte) error) error {
	defer func() {
		r.pieces = nil
		r.rowBuf = r.rowBuf[:0]
		if cap(r.rowBuf) > 1<<20 {
			r.rowBuf = nil
		}
	}()
	if hdr.Size > maxChunkBytes {
		return fmt.Errorf("%w: entry %s claims %d bytes", ErrCorrupt, hdr.Name, hdr.Size)
	}
	r.hasher.Reset()
	r.reader.Reset(io.TeeReader(&entrySizeReader{reader: tr, hdr: hdr}, r.hasher))
	for {
		segment, err := r.reader.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			if len(r.pieces) == 0 && len(r.rowBuf)+len(segment) <= cap(r.rowBuf) {
				r.rowBuf = append(r.rowBuf, segment...)
			} else {
				piece := make([]byte, len(segment))
				copy(piece, segment)
				r.pieces = append(r.pieces, piece)
			}
			continue
		}
		if err != nil {
			// Bytes returned with an error are a fragment, never another row.
			if err == io.EOF {
				if len(segment) == 0 && len(r.rowBuf) == 0 && len(r.pieces) == 0 {
					return nil
				}
				return fmt.Errorf("%w: %s: unterminated row", ErrCorrupt, hdr.Name)
			}
			if errors.Is(err, ErrCorrupt) {
				return err // entrySizeReader already adds the readEntry diagnostic.
			}
			return fmt.Errorf("%w: %s: %v", ErrCorrupt, hdr.Name, err)
		}
		tail := segment[:len(segment)-1]
		if len(r.rowBuf) == 0 && len(r.pieces) == 0 {
			if err := row(tail); err != nil {
				return err
			}
			continue
		}
		if len(r.pieces) == 0 && len(r.rowBuf)+len(tail) <= cap(r.rowBuf) {
			r.rowBuf = append(r.rowBuf, tail...)
		} else {
			size := len(r.rowBuf) + len(tail)
			for _, piece := range r.pieces {
				size += len(piece)
			}
			assembled := make([]byte, size)
			offset := copy(assembled, r.rowBuf)
			for _, piece := range r.pieces {
				offset += copy(assembled[offset:], piece)
			}
			copy(assembled[offset:], tail)
			r.rowBuf = assembled
			r.pieces = nil
		}
		if err := row(r.rowBuf); err != nil {
			return err
		}
		r.rowBuf = r.rowBuf[:0]
	}
}

// archiveCompletionReader forwards unchanged until completion arms ctx.
// Checks between compressed reads also cover empty-member loops inside gzip;
// they cannot interrupt an underlying Read that is already blocked.
type archiveCompletionReader struct {
	reader flate.Reader
	ctx    context.Context
}

func (r *archiveCompletionReader) Read(p []byte) (int, error) {
	if r.ctx != nil {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
	}
	return r.reader.Read(p)
}

func (r *archiveCompletionReader) ReadByte() (byte, error) {
	if r.ctx != nil {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
	}
	return r.reader.ReadByte()
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
	// An identity upgrader still decodes and re-encodes the row. Reject
	// ambiguous JSON first so that process cannot erase duplicate member names
	// or normalize invalid Unicode escapes before the store's hostile-content
	// validator sees them.
	if err := rejectAmbiguousArchiveJSON(row); err != nil {
		return nil, fmt.Errorf("%w: %s row: %v", ErrCorrupt, table, err)
	}
	var obj map[string]any
	decoder := json.NewDecoder(bytes.NewReader(row))
	decoder.UseNumber()
	if err := decoder.Decode(&obj); err != nil {
		return nil, fmt.Errorf("%w: %s row: %v", ErrCorrupt, table, err)
	}
	if err := jsonstrict.RequireEOF(decoder); err != nil {
		if errors.Is(err, jsonstrict.ErrTrailingValue) {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("%w: %s row: %v", ErrCorrupt, table, err)
	}
	if obj == nil {
		return nil, fmt.Errorf("%w: %s row: expected JSON object", ErrCorrupt, table)
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

// rejectAmbiguousArchiveJSON rejects JSON spellings that encoding/json would
// otherwise normalize while decoding. Archive control records and rows that
// cross an upgrader must have one unambiguous interpretation before checksums
// or semantic validation are trusted.
func rejectAmbiguousArchiveJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("invalid UTF-8 in JSON")
	}
	if err := rejectUnpairedArchiveJSONSurrogates(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeUniqueArchiveJSONValue(decoder); err != nil {
		return err
	}
	err := jsonstrict.RequireEOF(decoder)
	if errors.Is(err, jsonstrict.ErrTrailingValue) {
		return errors.New("multiple JSON values")
	}
	return err
}

func rejectUnpairedArchiveJSONSurrogates(raw []byte) error {
	for index := 0; index < len(raw); index++ {
		if raw[index] != '"' {
			continue
		}
		index++
		for index < len(raw) && raw[index] != '"' {
			if raw[index] != '\\' {
				index++
				continue
			}
			if index+1 >= len(raw) {
				return errors.New("unterminated JSON escape")
			}
			if raw[index+1] != 'u' {
				index += 2
				continue
			}
			codeUnit, ok := parseArchiveJSONHexCodeUnit(raw, index+2)
			if !ok {
				return errors.New("invalid JSON Unicode escape")
			}
			switch {
			case codeUnit >= 0xd800 && codeUnit <= 0xdbff:
				if index+12 > len(raw) || raw[index+6] != '\\' || raw[index+7] != 'u' {
					return errors.New("unpaired high surrogate escape")
				}
				low, validLow := parseArchiveJSONHexCodeUnit(raw, index+8)
				if !validLow || low < 0xdc00 || low > 0xdfff {
					return errors.New("unpaired high surrogate escape")
				}
				index += 12
			case codeUnit >= 0xdc00 && codeUnit <= 0xdfff:
				return errors.New("unpaired low surrogate escape")
			default:
				index += 6
			}
		}
	}
	return nil
}

func parseArchiveJSONHexCodeUnit(raw []byte, offset int) (uint16, bool) {
	if offset < 0 || offset+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, character := range raw[offset : offset+4] {
		value <<= 4
		switch {
		case character >= '0' && character <= '9':
			value |= uint16(character - '0')
		case character >= 'a' && character <= 'f':
			value |= uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			value |= uint16(character-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func consumeUniqueArchiveJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object member name is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object member %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeUniqueArchiveJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("object did not terminate")
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueArchiveJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("array did not terminate")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
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
