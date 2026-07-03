package export

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"testing"
)

// TestRoundTripAndIntegrity pins the archive contract: manifest first, chunks
// in named order, checksums last, chunks split at the size bound, integrity
// verifiable from the trailing entry alone.
func TestRoundTripAndIntegrity(t *testing.T) {
	// Two tables — one small enough for a single chunk, one big enough to
	// straddle at least two chunks so the splitting logic is exercised.
	small := newSource("realms", 3)
	big := newSource("audit", 400_000) // ~400k rows -> multi-chunk (~48MB)

	var buf bytes.Buffer
	m := Manifest{SchemaVersion: 13, ServerVersion: "0.0.80", AccountID: "acc_x", Status: "suspended"}
	if err := Write(context.Background(), &buf, m, []RowSource{small, big}); err != nil {
		t.Fatal(err)
	}

	entries := readAll(t, &buf)
	if entries[0].name != "manifest.json" {
		t.Fatalf("first entry = %q, want manifest.json", entries[0].name)
	}
	if entries[len(entries)-1].name != "checksums.json" {
		t.Fatalf("last entry = %q, want checksums.json", entries[len(entries)-1].name)
	}

	var manifest Manifest
	if err := json.Unmarshal(entries[0].data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.FormatVersion != FormatVersion || manifest.SchemaVersion != 13 || manifest.AccountID != "acc_x" {
		t.Errorf("manifest = %+v", manifest)
	}
	if len(manifest.Tables) != 2 || manifest.Tables[0] != "realms" || manifest.Tables[1] != "audit" {
		t.Errorf("tables = %v", manifest.Tables)
	}

	var sums Checksums
	if err := json.Unmarshal(entries[len(entries)-1].data, &sums); err != nil {
		t.Fatal(err)
	}
	if sums.TableRows["realms"] != 3 || sums.TableRows["audit"] != 400_000 {
		t.Errorf("row counts = %v", sums.TableRows)
	}
	// The big table must have split into multiple chunks; the small one is one chunk.
	auditChunks := 0
	for _, c := range sums.Chunks {
		if c.Name[:5] == "audit" {
			auditChunks++
		}
	}
	if auditChunks < 2 {
		t.Errorf("audit chunks = %d, want >=2 (chunking not exercised)", auditChunks)
	}

	// Recompute each chunk's checksum from the entry payload — the trailing
	// checksums block MUST match, byte for byte.
	byName := map[string][]byte{}
	for _, e := range entries[1 : len(entries)-1] {
		byName[e.name] = e.data
	}
	for _, c := range sums.Chunks {
		data, ok := byName[c.Name]
		if !ok {
			t.Errorf("chunk %s missing from archive", c.Name)
			continue
		}
		if len(data) != c.Bytes {
			t.Errorf("%s length = %d, want %d", c.Name, len(data), c.Bytes)
		}
		if c.Bytes > chunkSize {
			t.Errorf("%s exceeds chunkSize %d", c.Name, chunkSize)
		}
		got := sha256.Sum256(data)
		if hex.EncodeToString(got[:]) != c.SHA256 {
			t.Errorf("%s sha256 mismatch", c.Name)
		}
	}
}

// TestEmptyTableStillRecorded — an empty table shows up in TableRows so a
// reader can distinguish "no rows" from "dropped from the format."
func TestEmptyTableStillRecorded(t *testing.T) {
	var buf bytes.Buffer
	src := newSource("tokens", 0)
	if err := Write(context.Background(), &buf, Manifest{AccountID: "acc_e", Status: "closed"}, []RowSource{src}); err != nil {
		t.Fatal(err)
	}
	entries := readAll(t, &buf)
	var sums Checksums
	_ = json.Unmarshal(entries[len(entries)-1].data, &sums)
	if _, ok := sums.TableRows["tokens"]; !ok {
		t.Error("empty table missing from TableRows")
	}
	if len(sums.Chunks) != 0 {
		t.Errorf("empty table produced %d chunks", len(sums.Chunks))
	}
}

// TestTruncationDetected — cut the archive short and the trailing checksums
// entry disappears, so any reader realizes something is wrong.
func TestTruncationDetected(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, Manifest{AccountID: "acc_t"},
		[]RowSource{newSource("realms", 10)}); err != nil {
		t.Fatal(err)
	}
	full := buf.Bytes()
	truncated := full[:len(full)-100] // eat the last archive bytes
	// tar/gzip readers won't just gracefully return — one of them will
	// error, or the checksums entry will be missing. Both count as detected.
	gzr, err := gzip.NewReader(bytes.NewReader(truncated))
	if err != nil {
		return // gzip already caught it
	}
	tr := tar.NewReader(gzr)
	seenChecksums := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return // tar reader caught it
		}
		if h.Name == "checksums.json" {
			seenChecksums = true
		}
	}
	if seenChecksums {
		t.Error("truncated archive still had checksums.json — integrity signal lost")
	}
}

// --- test helpers ---

type entry struct {
	name string
	data []byte
}

func readAll(t *testing.T, r io.Reader) []entry {
	t.Helper()
	gzr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gzr)
	var out []entry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		buf, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, entry{name: h.Name, data: buf})
	}
	return out
}

type source struct {
	table string
	n, i  int
}

func newSource(table string, n int) *source { return &source{table: table, n: n} }
func (s *source) Table() string             { return s.table }
func (s *source) Next(_ context.Context) ([]byte, error) {
	if s.i >= s.n {
		return nil, nil
	}
	// ~120-byte rows; 200k of them = ~24MB, comfortably crossing chunkSize.
	row := fmt.Sprintf(`{"table":%q,"i":%d,"payload":"%s"}`, s.table, s.i,
		"padpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpad")
	s.i++
	return []byte(row), nil
}
