package export

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"testing"
)

type bufferTestSource struct {
	table string
	rows  [][]byte
	index int
}

func (s *bufferTestSource) Table() string { return s.table }
func (s *bufferTestSource) Next(context.Context) ([]byte, error) {
	if s.index == len(s.rows) {
		return nil, nil
	}
	row := s.rows[s.index]
	s.index++
	return row, nil
}

func TestWriteMatchesOracle(t *testing.T) {
	small := []byte(`{"id":"small"}`)
	big := bytes.Repeat([]byte("x"), 33<<20)
	half := bytes.Repeat([]byte("y"), 16<<20)
	for _, tc := range []struct {
		name   string
		tables [][][]byte
	}{
		{"three_small_tables", [][][]byte{{small}, {small, small}, {small}}},
		{"oversized_then_ordinary", [][][]byte{{big}, {small, small}}},
		{"two_chunks", [][][]byte{{half, half}}},
		{"interspersed_empty_tables", [][][]byte{nil, {small}, nil, {small, small}, nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sources := func() []RowSource {
				var out []RowSource
				for i, rows := range tc.tables {
					out = append(out, &bufferTestSource{table: fmt.Sprintf("table_%d", i), rows: rows})
				}
				return out
			}
			var got, want bytes.Buffer
			m := Manifest{SchemaVersion: 1, AccountID: "writer-oracle"}
			if err := Write(context.Background(), &got, m, sources()); err != nil {
				t.Fatal(err)
			}
			if err := writeOracle(context.Background(), &want, m, sources()); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatalf("archive bytes differ: Write=%d oracle=%d", got.Len(), want.Len())
			}
		})
	}
}

func TestWriteReusesChunkBuffer(t *testing.T) {
	row := bytes.Repeat([]byte("x"), 1024)
	measure := func(tables int, nonempty bool) uint64 {
		best := ^uint64(0)
		for run := 0; run < 3; run++ {
			sources := make([]RowSource, tables)
			for i := range sources {
				source := &bufferTestSource{table: fmt.Sprintf("table_%d", i)}
				if nonempty {
					source.rows = [][]byte{row}
				}
				sources[i] = source
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			err := Write(context.Background(), io.Discard, Manifest{SchemaVersion: 1}, sources)
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatal(err)
			}
			best = min(best, after.TotalAlloc-before.TotalAlloc)
		}
		return best
	}
	baseline := measure(0, false)
	empty := measure(74, false)
	ordinary := measure(3, true)
	t.Logf("minimum TotalAlloc: zero_sources=%d empty_74=%d ordinary_3=%d empty_delta=%d ordinary_delta=%d", baseline, empty, ordinary, int64(empty)-int64(baseline), int64(ordinary)-int64(baseline))
	if int64(empty)-int64(baseline) >= 1<<20 {
		t.Error("empty tables allocated at least 1 MiB above baseline")
	}
	if int64(ordinary)-int64(baseline) >= chunkSize+(1<<20) {
		t.Error("three ordinary tables allocated at least one chunk plus 1 MiB above baseline")
	}
	t.Run("oversized_then_small_tables", func(t *testing.T) {
		// TotalAlloc cannot detect this regression: retaining the oversized
		// buffer avoids a later allocation. Check the shared flush helper's
		// retained capacity directly after each single-row table instead.
		var buf []byte
		for table, rowBytes := range []int{chunkSize + 1024, 1024, 1024, 1024} {
			if cap(buf) < rowBytes+1 {
				buf = make([]byte, rowBytes+1, max(chunkSize, rowBytes+1))
			} else {
				buf = buf[:rowBytes+1]
			}
			buf = resetChunkBuffer(buf)
			if len(buf) != 0 || cap(buf) > chunkSize {
				t.Fatalf("table %d retained len=%d cap=%d after flush; want empty buffer with cap <= %d", table, len(buf), cap(buf), chunkSize)
			}
		}
	})
}
