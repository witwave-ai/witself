package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/memlimit"
)

const (
	importPeakContainerLimit = int64(268435456)
	importPeakSoftLimit      = int64(201326592)
	importPeakBound          = int64(251658240)
)

type importPeakResult struct {
	PeakGoTotal int64 `json:"peak_go_total"`
	SoftLimit   int64 `json:"soft_limit"`
	VMHWM       int64 `json:"vm_hwm"`
}

func TestImportPeakMemory(t *testing.T) {
	if importPeakRaceEnabled {
		message := "NOT RUN TestImportPeakMemory: race detector changes memory accounting; proven by the no-race gate"
		if os.Getenv("WITSELF_TEST_REQUIRE_PEAK_MEMORY") == "1" {
			t.Fatal(message)
		}
		t.Skip(message)
	}
	if mode := os.Getenv("WITSELF_IMPORT_PEAK_CHILD"); mode != "" {
		result := runImportPeakChild(t, mode)
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			t.Fatal(err)
		}
		// Emit exactly the measurement, without the testing package's PASS line.
		os.Exit(0)
	}
	archive := filepath.Join(t.TempDir(), "peak.tar.gz")
	writeImportPeakFixture(t, archive)
	results := make(map[string]importPeakResult, 2)
	for _, mode := range []string{"control", "limited"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestImportPeakMemory$")
		for _, env := range os.Environ() {
			key, _, _ := strings.Cut(env, "=")
			switch key {
			case "GOMEMLIMIT", "GOGC", "GODEBUG", "WITSELF_IMPORT_PEAK_CHILD", "WITSELF_IMPORT_PEAK_ARCHIVE":
				continue
			}
			command.Env = append(command.Env, env)
		}
		command.Env = append(command.Env, "WITSELF_IMPORT_PEAK_CHILD="+mode, "WITSELF_IMPORT_PEAK_ARCHIVE="+archive)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		ctxErr := ctx.Err()
		cancel()
		if err != nil {
			t.Fatalf("%s child failed: %v (context=%v): %s%s", mode, err, ctxErr, output, stderr.String())
		}
		var result importPeakResult
		if err := json.Unmarshal(bytes.TrimSpace(output), &result); err != nil {
			t.Fatalf("%s child measurement invalid: %v", mode, err)
		}
		results[mode] = result
		t.Logf("%s peak_go_total=%d soft_limit=%d vm_hwm=%d bound=%d", mode, result.PeakGoTotal, result.SoftLimit, result.VMHWM, importPeakBound)
	}
	control, limited := results["control"], results["limited"]
	if control.PeakGoTotal <= importPeakContainerLimit {
		t.Errorf("fixture too weak: control peak_go_total=%d must exceed %d", control.PeakGoTotal, importPeakContainerLimit)
	}
	if limited.SoftLimit != importPeakSoftLimit {
		t.Errorf("limited soft_limit=%d, want %d", limited.SoftLimit, importPeakSoftLimit)
	}
	if limited.PeakGoTotal > importPeakBound {
		t.Errorf("limited peak_go_total=%d exceeds %d", limited.PeakGoTotal, importPeakBound)
	}
	t.Run("linux_rss", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skipf("NOT RUN TestImportPeakMemory/linux_rss: needs Linux /proc/self/status; proven by the go-store CI step (Linux, no race)")
		}
		if control.VMHWM < 0 || limited.VMHWM < 0 {
			message := "NOT RUN TestImportPeakMemory/linux_rss: Linux /proc/self/status unavailable"
			if os.Getenv("WITSELF_TEST_REQUIRE_PEAK_MEMORY") == "1" {
				t.Fatal(message)
			}
			t.Skip(message)
		}
		t.Logf("limited vm_hwm=%d container_limit=%d within_container_limit=%t", limited.VMHWM, importPeakContainerLimit, limited.VMHWM <= importPeakContainerLimit)
		if control.VMHWM <= importPeakContainerLimit {
			t.Errorf("control vm_hwm=%d must exceed %d", control.VMHWM, importPeakContainerLimit)
		}
		if limited.VMHWM > control.VMHWM-(48<<20) {
			t.Errorf("limited vm_hwm=%d must be at least 48 MiB below control=%d", limited.VMHWM, control.VMHWM)
		}
	})
}

type importPeakRows struct {
	table string
	next  func() ([]byte, error)
}

func (s *importPeakRows) Table() string { return s.table }
func (s *importPeakRows) Next(context.Context) ([]byte, error) {
	return s.next()
}

func writeImportPeakFixture(t *testing.T, path string) {
	t.Helper()
	random := rand.NewChaCha8([32]byte{108})
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
	rows := 8 * ((32 << 20) / (len(first) + 1))
	index := 0
	transcripts := &importPeakRows{table: "transcript_entries", next: func() ([]byte, error) {
		if index == rows {
			return nil, nil
		}
		index++
		return row(index)
	}}
	emitted := false
	email := &importPeakRows{table: "agent_email_messages", next: func() ([]byte, error) {
		if emitted {
			return nil, nil
		}
		emitted = true
		attachment := make([]byte, (6<<20)*3/4)
		_, _ = random.Read(attachment)
		encoded := base64.StdEncoding.EncodeToString(attachment)
		var mime strings.Builder
		mime.WriteString("From: sender@example.test\r\nTo: receiver@example.test\r\nSubject: peak fixture\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=peak-boundary\r\n\r\n--peak-boundary\r\nContent-Type: text/plain\r\n\r\nPeak memory fixture.\r\n--peak-boundary\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=fixture.bin\r\nContent-Transfer-Encoding: base64\r\n\r\n")
		for len(encoded) > 0 {
			n := min(76, len(encoded))
			mime.WriteString(encoded[:n])
			mime.WriteString("\r\n")
			encoded = encoded[n:]
		}
		mime.WriteString("--peak-boundary--\r\n")
		raw := []byte(mime.String())
		digest := sha256.Sum256(raw)
		return json.Marshal(map[string]any{
			"id": "email-peak-memory", "account_id": "acct-peak-memory",
			"raw_mime": "\\x" + hex.EncodeToString(raw), "raw_sha256": hex.EncodeToString(digest[:]),
		})
	}}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := export.Write(context.Background(), f, export.Manifest{SchemaVersion: 1, AccountID: "acct-peak-memory"}, []export.RowSource{transcripts, email})
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("write peak fixture: write=%v close=%v", writeErr, closeErr)
	}
}

func runImportPeakChild(t *testing.T, mode string) importPeakResult {
	t.Helper()
	if mode != "control" && mode != "limited" {
		t.Fatal("invalid peak child mode")
	}
	ballast := make([]byte, 35<<20)
	for i := range ballast {
		ballast[i] = byte(i + 1)
	}
	validation := make(map[string]string, 40<<10)
	for i := 0; i < 40<<10; i++ {
		validation[strconv.Itoa(i)] = strings.Repeat("v", 1024)
	}
	// The parent owns this tree: os.Exit below bypasses child test cleanups.
	root := filepath.Join(filepath.Dir(os.Getenv("WITSELF_IMPORT_PEAK_ARCHIVE")), "cgroup-"+mode)
	cgroup := filepath.Join(root, "sys", "fs", "cgroup")
	if err := os.MkdirAll(cgroup, 0o700); err != nil {
		t.Fatal(err)
	}
	limit := "max"
	if mode == "limited" {
		limit = "268435456"
	}
	if err := os.WriteFile(filepath.Join(cgroup, "memory.max"), []byte(limit), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration := memlimit.ConfigureFrom(root, os.LookupEnv, debug.SetMemoryLimit, io.Discard, "peak-child")
	var mu sync.Mutex
	var peak int64
	values := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	sample := func() {
		mu.Lock()
		defer mu.Unlock()
		metrics.Read(values)
		total := int64(values[0].Value.Uint64() - values[1].Value.Uint64())
		peak = max(peak, total)
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
	f, err := os.Open(os.Getenv("WITSELF_IMPORT_PEAK_ARCHIVE"))
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := export.Read(context.Background(), f, export.ImportOptions{CurrentSchema: 1, Row: func(table string, row []byte) error {
		obj, err := decodeImportRow(row)
		if err != nil {
			return err
		}
		var raw []byte
		var parsed agentemail.ParsedMessage
		if table == "agent_email_messages" {
			raw, err = hex.DecodeString(obj["raw_mime"].(string)[2:])
			if err != nil {
				return err
			}
			digest := sha256.Sum256(raw)
			if hex.EncodeToString(digest[:]) != obj["raw_sha256"].(string) {
				return fmt.Errorf("fixture raw MIME digest mismatch")
			}
			parsed, err = agentemail.ParseMessage(raw, true)
			if err != nil {
				return err
			}
			if _, _, err := agentemail.RetryCanaryChallenge(raw); err != nil {
				return err
			}
		}
		encoded, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		params, wire := make([]byte, 0, 256), make([]byte, 0, 1024)
		for offset := 0; offset < len(encoded); offset += 4096 {
			end := min(offset+4096, len(encoded))
			params = append(params, encoded[offset:end]...)
			wire = append(wire, encoded[offset:end]...)
		}
		sample()
		runtime.KeepAlive(obj)
		runtime.KeepAlive(raw)
		runtime.KeepAlive(parsed)
		runtime.KeepAlive(encoded)
		runtime.KeepAlive(params)
		runtime.KeepAlive(wire)
		return nil
	}})
	closeErr := f.Close()
	sample()
	close(stop)
	<-done
	runtime.KeepAlive(ballast)
	runtime.KeepAlive(validation)
	if readErr != nil || closeErr != nil {
		t.Fatalf("read peak fixture: read=%v close=%v", readErr, closeErr)
	}
	vmHWM := int64(-1)
	if runtime.GOOS == "linux" {
		vmHWM = memlimit.SampleFrom("/", "").RSSHWM
	}
	return importPeakResult{PeakGoTotal: peak, SoftLimit: configuration.SoftLimit, VMHWM: vmHWM}
}
