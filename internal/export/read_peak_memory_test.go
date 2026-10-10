package export

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"testing"
	"time"
)

type readPeakResult struct {
	PeakGoTotal int64  `json:"peak_go_total"`
	Rows        int    `json:"rows"`
	LargestRow  int    `json:"largest_row"`
	Sum         uint64 `json:"sum"`
}

func TestReadPeakMemory(t *testing.T) {
	if readPeakRaceEnabled {
		message := "NOT RUN TestReadPeakMemory: race detector changes memory accounting; proven by the no-race gate"
		if os.Getenv("WITSELF_TEST_REQUIRE_PEAK_MEMORY") == "1" {
			t.Fatal(message)
		}
		t.Skip(message)
	}
	if mode := os.Getenv("WITSELF_READ_PEAK_CHILD"); mode != "" {
		result := runReadPeakChild(t, mode)
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			t.Fatal(err)
		}
		// Emit exactly the measurement, without the testing package's PASS line.
		os.Exit(0)
	}
	dir := t.TempDir()
	bulkRows, bulkRowBytes, bigRowBytes := writeReadPeakFixtures(t, dir)
	results := make(map[string]readPeakResult, 5)
	for _, mode := range []string{"empty", "stream_bulk", "oracle_bulk", "stream_big", "oracle_big"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReadPeakMemory$")
		for _, env := range os.Environ() {
			key, _, _ := strings.Cut(env, "=")
			switch key {
			case "GOMEMLIMIT", "GOGC", "GODEBUG", "WITSELF_READ_PEAK_CHILD", "WITSELF_READ_PEAK_DIR":
				continue
			}
			command.Env = append(command.Env, env)
		}
		command.Env = append(command.Env, "GOGC=100", "WITSELF_READ_PEAK_CHILD="+mode, "WITSELF_READ_PEAK_DIR="+dir)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		ctxErr := ctx.Err()
		cancel()
		if err != nil {
			t.Fatalf("%s child failed: %v (context=%v): %s%s", mode, err, ctxErr, output, stderr.String())
		}
		var result readPeakResult
		if err := json.Unmarshal(bytes.TrimSpace(output), &result); err != nil {
			t.Fatalf("%s child measurement invalid: %v", mode, err)
		}
		results[mode] = result
		t.Logf("%s peak_go_total=%d rows=%d largest_row=%d sum=%d", mode, result.PeakGoTotal, result.Rows, result.LargestRow, result.Sum)
	}
	for _, mode := range []string{"empty", "stream_bulk", "oracle_bulk", "stream_big", "oracle_big"} {
		wantRows, wantLargest := 0, 0
		if strings.HasSuffix(mode, "_bulk") {
			wantRows, wantLargest = bulkRows, bulkRowBytes
		} else if strings.HasSuffix(mode, "_big") {
			wantRows, wantLargest = 1, bigRowBytes
		}
		if got := results[mode]; got.Rows != wantRows || got.LargestRow != wantLargest {
			t.Errorf("%s rows=%d largest_row=%d, want rows=%d largest_row=%d", mode, got.Rows, got.LargestRow, wantRows, wantLargest)
		}
	}
	for _, fixture := range []string{"bulk", "big"} {
		if results["stream_"+fixture].Sum != results["oracle_"+fixture].Sum {
			t.Errorf("%s row byte sums differ between stream and oracle", fixture)
		}
	}
	// Subtract the matching runtime/test-binary floor, including Go's minimum
	// heap at GOGC=100. A whole bulk entry alone is nearly 32 MiB; streaming
	// retains a 256 KiB reader and sub-buffer rows. A big row transiently holds
	// both its pieces and its final exact-size buffer, so it needs about 2*R.
	const oracleBulkFloor = 30 << 20
	const streamBulkBound = 16 << 20
	empty := results["empty"].PeakGoTotal
	oracleBulk := results["oracle_bulk"].PeakGoTotal - empty
	streamBulk := results["stream_bulk"].PeakGoTotal - empty
	streamBig := results["stream_big"].PeakGoTotal - empty
	bigBound := int64(bigRowBytes) * 5 / 2
	t.Logf("E=%d R=%d oracle_bulk-E=%d floor=%d stream_bulk-E=%d bound=%d stream_big-E=%d bound=%d", empty, bigRowBytes, oracleBulk, oracleBulkFloor, streamBulk, streamBulkBound, streamBig, bigBound)
	if oracleBulk < oracleBulkFloor {
		t.Errorf("fixture too weak: oracle_bulk-E=%d must be at least %d", oracleBulk, oracleBulkFloor)
	}
	if streamBulk > streamBulkBound {
		t.Errorf("stream_bulk-E=%d exceeds %d", streamBulk, streamBulkBound)
	}
	if streamBig > bigBound {
		t.Errorf("stream_big-E=%d exceeds 2.5*R=%d", streamBig, bigBound)
	}
}

type readPeakRows struct {
	table string
	next  func() ([]byte, error)
}

func (s *readPeakRows) Table() string { return s.table }
func (s *readPeakRows) Next(context.Context) ([]byte, error) {
	return s.next()
}

func writeReadPeakFixtures(t *testing.T, dir string) (bulkRows, bulkRowBytes, bigRowBytes int) {
	t.Helper()
	write := func(name string, source RowSource) {
		t.Helper()
		f, err := os.Create(filepath.Join(dir, name+".tar.gz"))
		if err != nil {
			t.Fatal(err)
		}
		writeErr := Write(context.Background(), f, Manifest{SchemaVersion: 1, AccountID: "acct-peak-memory"}, []RowSource{source})
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("write %s peak fixture: write=%v close=%v", name, writeErr, closeErr)
		}
	}
	write("empty", &readPeakRows{table: "transcript_entries", next: func() ([]byte, error) { return nil, nil }})
	// Replicate the store's writeImportPeakFixture generator without importing
	// store (which imports export). Fixed-width fields keep every row equal in
	// size, so this produces exactly eight nearly 32 MiB chunk entries.
	random := rand.NewChaCha8([32]byte{111})
	row := func(index int) ([]byte, error) {
		var content [2400]byte
		_, _ = random.Read(content[:])
		return json.Marshal(map[string]any{
			"id": fmt.Sprintf("entry-%09d", index), "account_id": "acct-peak-memory",
			"agent_id": "agent-peak-memory", "session_id": "session-peak-memory",
			"entry_index": fmt.Sprintf("%09d", index), "role": "user",
			"content":    base64.StdEncoding.EncodeToString(content[:])[:3200],
			"created_at": "2026-10-09T00:00:00Z", "metadata": map[string]any{},
		})
	}
	first, err := row(0)
	if err != nil {
		t.Fatal(err)
	}
	bulkRowBytes = len(first)
	bulkRows = 8 * (chunkSize / (bulkRowBytes + 1))
	index := 0
	write("bulk", &readPeakRows{table: "transcript_entries", next: func() ([]byte, error) {
		if index == bulkRows {
			return nil, nil
		}
		index++
		return row(index)
	}})
	emitted := false
	write("big", &readPeakRows{table: "agent_email_messages", next: func() ([]byte, error) {
		if emitted {
			return nil, nil
		}
		emitted = true
		raw := make([]byte, 10<<20)
		_, _ = random.Read(raw)
		encoded, err := json.Marshal(map[string]any{"id": "email-big", "raw_mime": "\\x" + hex.EncodeToString(raw)})
		bigRowBytes = len(encoded)
		return encoded, err
	}})
	return bulkRows, bulkRowBytes, bigRowBytes
}

func runReadPeakChild(t *testing.T, mode string) readPeakResult {
	t.Helper()
	var fixture string
	reader := Read
	switch mode {
	case "empty":
		fixture = "empty"
	case "stream_bulk":
		fixture = "bulk"
	case "oracle_bulk":
		fixture, reader = "bulk", readWholeEntryOracle
	case "stream_big":
		fixture = "big"
	case "oracle_big":
		fixture, reader = "big", readWholeEntryOracle
	default:
		t.Fatal("invalid peak child mode")
	}
	var mu sync.Mutex
	var result readPeakResult
	values := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	sample := func() {
		mu.Lock()
		defer mu.Unlock()
		metrics.Read(values)
		total := int64(values[0].Value.Uint64() - values[1].Value.Uint64())
		result.PeakGoTotal = max(result.PeakGoTotal, total)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sample()
			case <-stop:
				return
			}
		}
	}()
	sample()
	f, err := os.Open(filepath.Join(os.Getenv("WITSELF_READ_PEAK_DIR"), fixture+".tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := reader(context.Background(), f, ImportOptions{CurrentSchema: 1, Row: func(_ string, row []byte) error {
		result.Rows++
		result.LargestRow = max(result.LargestRow, len(row))
		if len(row) > 0 {
			result.Sum = result.Sum*33 + uint64(row[len(row)/2])
		}
		sample()
		runtime.KeepAlive(row)
		return nil
	}})
	closeErr := f.Close()
	sample()
	close(stop)
	<-done
	if readErr != nil || closeErr != nil {
		t.Fatalf("read peak fixture: read=%v close=%v", readErr, closeErr)
	}
	return result
}
