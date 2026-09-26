package export

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type streamingSource struct {
	next func(context.Context) ([]byte, error)
}

func (s streamingSource) Table() string                            { return "transcript_entries" }
func (s streamingSource) Next(ctx context.Context) ([]byte, error) { return s.next(ctx) }

func TestWriteFlushesReadableChunksBeforeSourceExhaustion(t *testing.T) {
	const total = 1100
	row := []byte(`{"body":"` + strings.Repeat("x", 64<<10) + `"}`)
	produced, flushes, priorBytes := 0, 0, 0
	var archive bytes.Buffer
	src := streamingSource{next: func(context.Context) ([]byte, error) {
		if produced == total {
			return nil, nil
		}
		produced++
		return row, nil
	}}
	err := Write(context.Background(), &archive, Manifest{}, []RowSource{src}, WriteOptions{Flush: func() error {
		flushes++
		if archive.Len() <= priorBytes {
			t.Fatal("flush emitted no compressed bytes")
		}
		priorBytes = archive.Len()
		gz, err := gzip.NewReader(bytes.NewReader(archive.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		// Every flushed entry must already be fully readable without the gzip footer.
		for i := 0; i < flushes; i++ {
			if _, err := tr.Next(); err != nil {
				t.Fatalf("flushed tar header: %v", err)
			}
			if _, err := io.Copy(io.Discard, tr); err != nil {
				t.Fatalf("flushed tar body: %v", err)
			}
		}
		if flushes == 2 && produced >= total {
			t.Fatal("first chunk waited for all rows")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if flushes != 4 {
		t.Fatalf("flushes = %d, want manifest + 3 chunks", flushes)
	}
	rows := 0
	if _, err := Read(context.Background(), bytes.NewReader(archive.Bytes()), ImportOptions{
		CurrentSchema: 0, Row: func(string, []byte) error { rows++; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if rows != total {
		t.Fatalf("round-trip rows = %d", rows)
	}
}

func TestWriteFlushFailureKeepsSourceName(t *testing.T) {
	sentinel := errors.New("transport stopped")
	calls := 0
	err := Write(context.Background(), io.Discard, Manifest{}, []RowSource{newSource("transcript_entries", 1)}, WriteOptions{Flush: func() error {
		calls++
		if calls == 2 {
			return sentinel
		}
		return nil
	}})
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "transcript_entries") {
		t.Fatalf("error = %v", err)
	}
}

func TestWriteSlowSourceWarningAndValueFreeProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs streamingLogBuffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		calls := 0
		src := streamingSource{next: func(context.Context) ([]byte, error) {
			calls++
			if calls == 1 {
				time.Sleep(29 * time.Second)
				synctest.Wait()
				if logs.Len() != 0 {
					t.Fatal("early slow-source warning")
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if !strings.Contains(logs.String(), "account export slow source") {
					t.Fatal("no warning during blocked Next")
				}
				return []byte(`{"body":"private-row-marker"}`), nil
			}
			return nil, nil
		}}
		if err := Write(context.Background(), io.Discard, Manifest{}, []RowSource{src, newSource("empty", 0)}, WriteOptions{Logger: logger}); err != nil {
			t.Fatal(err)
		}
		text := logs.String()
		if strings.Contains(text, "private-row-marker") {
			t.Fatal("row value leaked")
		}
		if !strings.Contains(text, "table=transcript_entries chunks=1 rows=1 elapsed=") || !strings.Contains(text, "table=empty chunks=0 rows=0 elapsed=") {
			t.Fatalf("missing progress: %s", text)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if logs.String() != text {
			t.Fatal("watchdog survived export")
		}
	})
}

func TestWriteSlowSourceCancellationStopsWatchdog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var logs streamingLogBuffer
		src := streamingSource{next: func(ctx context.Context) ([]byte, error) { cancel(); return nil, ctx.Err() }}
		err := Write(ctx, io.Discard, Manifest{}, []RowSource{src}, WriteOptions{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "transcript_entries") {
			t.Fatalf("error = %v", err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if logs.Len() != 0 {
			t.Fatal("failed table emitted completion or late warning")
		}
	})
}

// Logging runs in the watchdog callback; fake time alone does not establish
// synchronization between buffer reads and a later callback write.
type streamingLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *streamingLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *streamingLogBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }
func (b *streamingLogBuffer) Len() int       { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.Len() }
