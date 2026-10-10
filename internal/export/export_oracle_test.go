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

// writeOracle is the verbatim Write implementation from b4d952af.
// Keep this oracle unchanged; only its name differs.
func writeOracle(ctx context.Context, w io.Writer, m Manifest, sources []RowSource, options ...WriteOptions) error {
	var opts WriteOptions
	if len(options) > 0 {
		opts = options[0]
	}
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

	flushStream := func() error {
		if err := tw.Flush(); err != nil {
			return err
		}
		if err := gz.Flush(); err != nil {
			return err
		}
		if opts.Flush != nil {
			return opts.Flush()
		}
		return nil
	}

	sums := Checksums{TableRows: map[string]int{}}

	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := writeEntry(tw, "manifest.json", manifestJSON); err != nil {
		return err
	}

	if err := flushStream(); err != nil {
		return err
	}

	for _, src := range sources {
		table := src.Table()
		started := time.Now()
		next, stop := watchSource(ctx, src, opts.Logger)
		defer stop()
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
				return fmt.Errorf("export %s: %w", table, err)
			}
			if err := flushStream(); err != nil {
				return fmt.Errorf("export %s: %w", table, err)
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
			row, err := next()
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
		stop()
		if opts.Logger != nil {
			opts.Logger.InfoContext(ctx, "account export table complete", "table", table,
				"chunks", chunkNo, "rows", sums.TableRows[table], "elapsed", time.Since(started))
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
