// Package export defines Witself's account archive format and its streaming
// writer. An archive is a LOGICAL artifact — named-field NDJSON rows, never a
// database dump — so it can be restored into whatever database a future cell
// runs, through the version upgrader chain (see upgrade.go).
//
// Layout: a gzip-compressed tar stream.
//
//	manifest.json            first entry — version coordinates + account id
//	<table>/000001.ndjson    bounded chunks (~chunkSize) of one row per line
//	<table>/000002.ndjson    ...
//	checksums.json           LAST entry — per-chunk sha256 + row counts
//
// The trailing checksums entry is the integrity contract: it is written after
// everything it describes, so a truncated or corrupted stream is detectable
// by any reader without out-of-band metadata. Import verifies it before a
// single row is COMMITTED — rows stream into a transaction that only commits
// once the trailer proves the archive whole. Memory stays bounded regardless
// of account size: rows stream from database cursors, and only one chunk is
// buffered at a time.
package export

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// FormatVersion identifies the archive layout itself (tar structure, manifest
// fields, chunking scheme). Bump only when the LAYOUT changes; schema changes
// ride the SchemaVersion + upgrader chain instead.
const FormatVersion = 1

// chunkSize bounds how many NDJSON bytes are buffered per tar entry. Tar
// needs each entry's size up front, so rows accumulate to at most ~this many
// bytes and then flush as one numbered chunk.
const chunkSize = 32 << 20 // 32 MiB

// Manifest is the archive's leading entry: everything a future importer needs
// to decide whether and how it can restore this archive.
type Manifest struct {
	FormatVersion int       `json:"format_version"`
	SchemaVersion int       `json:"schema_version"`
	ServerVersion string    `json:"server_version"`
	Compression   string    `json:"compression"` // of chunk content semantics; outer stream is gzip
	AccountID     string    `json:"account_id"`
	Cell          string    `json:"cell,omitempty"`
	Status        string    `json:"status"` // account status at export time (suspended/closed)
	ExportedAt    time.Time `json:"exported_at"`
	Tables        []string  `json:"tables"`
}

// ChunkSum records one chunk's integrity data in the trailing checksums entry.
type ChunkSum struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
	Rows   int    `json:"rows"`
}

// Checksums is the archive's trailing entry.
type Checksums struct {
	Chunks    []ChunkSum     `json:"chunks"`
	TableRows map[string]int `json:"table_rows"`
}

// RowSource yields one table's rows as marshaled JSON objects. Implementations
// stream from database cursors; Next returns nil when exhausted, and err is
// checked after exhaustion.
type RowSource interface {
	// Table names the table these rows belong to.
	Table() string
	// Next returns the next row as a JSON object, or nil at end of rows.
	Next(ctx context.Context) ([]byte, error)
}

// Write streams a complete archive to w: manifest, chunked table rows, then
// the trailing checksums. It never buffers more than one chunk.
func Write(ctx context.Context, w io.Writer, m Manifest, sources []RowSource) error {
	m.FormatVersion = FormatVersion
	if m.Compression == "" {
		m.Compression = "gzip"
	}
	m.Tables = m.Tables[:0]
	for _, s := range sources {
		m.Tables = append(m.Tables, s.Table())
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	sums := Checksums{TableRows: map[string]int{}}

	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := writeEntry(tw, "manifest.json", manifestJSON); err != nil {
		return err
	}

	for _, src := range sources {
		table := src.Table()
		chunkNo := 0
		buf := make([]byte, 0, chunkSize)
		rows := 0
		flush := func() error {
			if rows == 0 {
				return nil
			}
			chunkNo++
			name := fmt.Sprintf("%s/%06d.ndjson", table, chunkNo)
			if err := writeEntry(tw, name, buf); err != nil {
				return err
			}
			sum := sha256.Sum256(buf)
			sums.Chunks = append(sums.Chunks, ChunkSum{
				Name:   name,
				SHA256: hex.EncodeToString(sum[:]),
				Bytes:  len(buf),
				Rows:   rows,
			})
			sums.TableRows[table] += rows
			buf = buf[:0]
			rows = 0
			return nil
		}
		for {
			row, err := src.Next(ctx)
			if err != nil {
				return fmt.Errorf("export %s: %w", table, err)
			}
			if row == nil {
				break
			}
			// Flush BEFORE appending when this row would overflow the
			// chunk, so no chunk ever exceeds chunkSize. A single row
			// larger than the chunk still gets its own chunk (the buf is
			// empty when we get here after the flush).
			if len(buf) > 0 && len(buf)+len(row)+1 > chunkSize {
				if err := flush(); err != nil {
					return err
				}
			}
			buf = append(buf, row...)
			buf = append(buf, '\n')
			rows++
		}
		if err := flush(); err != nil {
			return err
		}
		if _, ok := sums.TableRows[table]; !ok {
			sums.TableRows[table] = 0 // empty tables are recorded, not omitted
		}
	}

	sumsJSON, err := json.Marshal(sums)
	if err != nil {
		return err
	}
	if err := writeEntry(tw, "checksums.json", sumsJSON); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func writeEntry(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o600,
		Size: int64(len(data)),
	}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}
