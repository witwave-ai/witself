package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/export"
)

type archiveByteReader struct {
	r io.Reader
	n int64
}

func (r *archiveByteReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

type archiveRows struct{ n int }

func (r *archiveRows) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			r.n++
		}
	}
	return len(p), nil
}

type archivePadding struct{ nonzero bool }

func (p *archivePadding) Write(data []byte) (int, error) {
	for _, b := range data {
		if b != 0 {
			p.nonzero = true
		}
	}
	return len(data), nil
}

type checkedArchive struct {
	manifest export.Manifest
	ndjson   int64
	rows     int
}

var archiveChunkName = regexp.MustCompile(`^[a-z_]+/[0-9]{6}\.ndjson$`)
var archiveCellName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func measureArchive(ctx context.Context, c config, d deps, s session, r readings, estimate int64, k, maximum int, during bool) (int64, error) {
	if err := quietCheck(d.now(), during, true); err != nil {
		return 0, err
	}
	if err := checkFilesystemSpace(r, estimate, c.limits.fs, during); err != nil {
		return 0, err
	}
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(20*time.Minute, cancel)
	defer timer.Stop()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, strings.TrimRight(s.endpoint, "/")+"/v1/export", nil)
	if err != nil {
		return 0, httpFailure("export", 0, err)
	}
	req.Header.Set("Authorization", "Bearer "+s.operatorToken)
	start := d.now()
	resp, err := d.client.Do(req)
	if err != nil {
		if ctx.Err() == nil && requestCtx.Err() != nil {
			err = context.DeadlineExceeded
		}
		return 0, httpFailure("export", 0, err)
	}
	defer func() { _ = resp.Body.Close() }()
	headers := d.now()
	timer.Reset(30 * time.Minute)
	if resp.StatusCode != http.StatusOK {
		return 0, httpFailure("export", resp.StatusCode, nil)
	}
	if resp.Header.Get("X-Witself-Export-Purpose") != "self" || resp.ContentLength < 0 {
		return 0, failure(5, "stopped: unexpected response from export")
	}
	counter := &archiveByteReader{r: resp.Body}
	checked, problem := inspectArchive(counter, c.account)
	// Drain to the declared end even after an integrity failure. In particular,
	// never abort a valid self export merely because its manifest is unexpected.
	_, drainErr := io.Copy(io.Discard, counter)
	if counter.n != resp.ContentLength {
		problem = "content-length"
	} else if drainErr != nil && problem == "" {
		problem = "trailer"
	}
	if ctx.Err() != nil {
		return 0, failure(130, "stopped: interrupted")
	}
	if problem != "" {
		return 0, failure(5, "stopped: archive check failed (%s)", problem)
	}
	kv(d, "measure:", fmt.Sprintf("%d of %d", k, maximum))
	kv(d, "archive bytes:", counter.n)
	kv(d, "archive purpose:", "self")
	kv(d, "manifest cell:", responseWord(checked.manifest.Cell))
	kv(d, "manifest schema:", checked.manifest.SchemaVersion)
	kv(d, "transcript_entries rows:", checked.rows)
	kv(d, "ndjson bytes:", checked.ndjson)
	kv(d, "compression ratio:", fmt.Sprintf("%.2f", float64(checked.ndjson)/float64(counter.n)))
	kv(d, "checksums:", "ok")
	kv(d, "export seconds:", int64(headers.Sub(start).Seconds()))
	kv(d, "download seconds:", int64(d.now().Sub(headers).Seconds()))
	return counter.n, nil
}

func inspectArchive(input io.Reader, account string) (checkedArchive, string) {
	var result checkedArchive
	gz, err := gzip.NewReader(input)
	if err != nil {
		return result, "trailer"
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	chunks := make(map[string]export.ChunkSum)
	rows := make(map[string]int)
	var trailer export.Checksums
	manifestSeen, trailerSeen := false, false
	problem := ""
	for {
		h, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			problem = "trailer"
			break
		}
		if trailerSeen || h.Typeflag != tar.TypeReg {
			problem = "trailer"
			break
		}
		switch h.Name {
		case "manifest.json":
			if manifestSeen || len(chunks) != 0 || h.Size > 1<<20 {
				problem = "manifest"
				break
			}
			manifestSeen = true
			data, readErr := io.ReadAll(tr)
			if readErr != nil || json.Unmarshal(data, &result.manifest) != nil || result.manifest.AccountID != account || (result.manifest.Cell != "" && !archiveCellName.MatchString(result.manifest.Cell)) {
				problem = "manifest"
			}
		case "checksums.json":
			if !manifestSeen || h.Size > 16<<20 {
				problem = "trailer"
				break
			}
			trailerSeen = true
			data, readErr := io.ReadAll(tr)
			if readErr != nil || json.Unmarshal(data, &trailer) != nil {
				problem = "trailer"
			}
		default:
			if !manifestSeen || !archiveChunkName.MatchString(h.Name) {
				problem = "checksums"
				break
			}
			if _, exists := chunks[h.Name]; exists {
				problem = "checksums"
				break
			}
			hash := sha256.New()
			counter := &archiveRows{}
			n, copyErr := io.Copy(io.MultiWriter(hash, counter), tr)
			if copyErr != nil {
				problem = "trailer"
				break
			}
			chunks[h.Name] = export.ChunkSum{Name: h.Name, SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: int(n), Rows: counter.n}
			table := strings.SplitN(h.Name, "/", 2)[0]
			rows[table] += counter.n
			result.ndjson += n
		}
		if problem != "" {
			break
		}
	}
	// Reading gzip to EOF verifies its checksum and requires its trailer.
	padding := &archivePadding{}
	if _, err := io.Copy(padding, gz); (err != nil || padding.nonzero) && problem == "" {
		problem = "trailer"
	}
	if problem != "" {
		return result, problem
	}
	if !manifestSeen {
		return result, "manifest"
	}
	if !trailerSeen {
		return result, "trailer"
	}
	if len(chunks) != len(trailer.Chunks) {
		return result, "checksums"
	}
	for _, sum := range trailer.Chunks {
		actual, ok := chunks[sum.Name]
		if !ok || actual != sum {
			return result, "checksums"
		}
		delete(chunks, sum.Name)
	}
	if len(chunks) != 0 {
		return result, "checksums"
	}
	for table, count := range rows {
		if actual, ok := trailer.TableRows[table]; !ok || actual != count {
			return result, "checksums"
		}
	}
	for table, count := range trailer.TableRows {
		if rows[table] != count {
			return result, "checksums"
		}
	}
	result.rows = rows["transcript_entries"]
	return result, ""
}
