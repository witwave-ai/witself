package store

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

var importEntryTestEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func TestImportEntryIndexDifferential(t *testing.T) {
	for _, maximum := range []uint32{0, 2} {
		t.Run(fmt.Sprintf("max_%d", maximum), func(t *testing.T) {
			index := importEntryIndex{maxOrdinals: maximum}
			reference := make(map[string]string)
			random := rand.New(rand.NewChaCha8([32]byte{112}))
			ids := []string{"", "ent_éaaaaaaaaaaaaaa", "trn_aaaaaaaaaaaaaaaa", "ent_aaaaaaaaaaaaaaa", "ent_aaaaaaaaaaaaaaaaa"}
			for _, invalid := range "0189" {
				ids = append(ids, "ent_"+string(invalid)+strings.Repeat("a", 15))
			}
			for i := 0; i < 1024; i++ {
				var raw [10]byte
				for j := range raw {
					raw[j] = byte(random.Uint64())
				}
				compact := "ent_" + importEntryTestEncoding.EncodeToString(raw[:])
				if key, ok := importEntryKey(compact); !ok || key != raw {
					t.Fatal("compact ID did not decode to its original 80-bit payload")
				}
				ids = append(ids, compact, "ent_"+strings.ToUpper(compact[4:]))
			}
			assertEqual := func(id string) {
				t.Helper()
				got, ok := index.transcriptOf(id)
				want, found := reference[id]
				if got != want || ok != found {
					t.Fatalf("lookup %q = (%q,%t), want (%q,%t)", id, got, ok, want, found)
				}
			}
			for operation := 0; operation < 200000; operation++ {
				entryID := ids[random.IntN(len(ids))]
				if random.IntN(3) != 0 {
					transcriptID := fmt.Sprintf("trn_%04d", random.IntN(1000))
					index.put(entryID, transcriptID)
					reference[entryID] = transcriptID
				}
				assertEqual(entryID)
				assertEqual(ids[random.IntN(len(ids))])
			}
			for _, id := range ids {
				assertEqual(id)
			}
		})
	}
	t.Run("form_transitions", func(t *testing.T) {
		index := importEntryIndex{maxOrdinals: 2}
		const entryID = "ent_aaaaaaaaaaaaaaaa"
		index.put(entryID, "trn_first")
		index.put("ent_bbbbbbbbbbbbbbbb", "trn_second")
		index.put(entryID, "trn_overflow")
		key, _ := importEntryKey(entryID)
		if _, found := index.compact[key]; found {
			t.Fatal("overflow put retained the old compact form")
		}
		if got, ok := index.transcriptOf(entryID); !ok || got != "trn_overflow" {
			t.Fatal("overflow put did not replace the old transcript")
		}
		index.maxOrdinals = 3
		index.put(entryID, "trn_first")
		if _, found := index.fallback[entryID]; found {
			t.Fatal("compact put retained the old fallback form")
		}
		index.put("ent_AAAAAAAAAAAAAAAA", "trn_uppercase")
		if got, _ := index.transcriptOf(entryID); got != "trn_first" {
			t.Fatal("uppercase ID collided with a lowercase ID")
		}
	})
}

type importEntryBytesResult struct {
	Mode          string  `json:"mode"`
	Entries       int     `json:"entries"`
	HeapBytes     int64   `json:"heap_bytes"`
	BytesPerEntry float64 `json:"bytes_per_entry"`
}

func TestImportEntryIndexBytes(t *testing.T) {
	if importPeakRaceEnabled {
		message := "NOT RUN TestImportEntryIndexBytes: race detector"
		if os.Getenv("WITSELF_TEST_REQUIRE_PEAK_MEMORY") == "1" {
			t.Fatal(message)
		}
		t.Skip(message)
	}
	if mode := os.Getenv("WITSELF_IMPORT_ENTRY_INDEX_CHILD"); mode != "" {
		result := measureImportEntryIndexBytes(t, mode)
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	for _, mode := range []string{"new", "legacy"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestImportEntryIndexBytes$")
		for _, env := range os.Environ() {
			key, _, _ := strings.Cut(env, "=")
			switch key {
			case "GOGC", "GOMEMLIMIT", "GODEBUG", "WITSELF_IMPORT_ENTRY_INDEX_CHILD":
				continue
			}
			command.Env = append(command.Env, env)
		}
		command.Env = append(command.Env, "WITSELF_IMPORT_ENTRY_INDEX_CHILD="+mode)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		cancel()
		if err != nil {
			t.Fatalf("%s child failed: %v: %s%s", mode, err, output, stderr.String())
		}
		var result importEntryBytesResult
		if err := json.Unmarshal(bytes.TrimSpace(output), &result); err != nil {
			t.Fatalf("%s child measurement: %v", mode, err)
		}
		t.Logf("%s entries=%d heap_bytes=%d bytes_per_entry=%.6f", result.Mode, result.Entries, result.HeapBytes, result.BytesPerEntry)
		// A 16-byte interleaved slot plus one control byte costs about 38.9
		// bytes at the worst 7/16 load. 48 leaves margin for map and intern
		// overhead; the memo's 40-byte estimate leaves almost none. The legacy
		// fixture must retain both fresh strings, at least 80 bytes per entry.
		if mode == "new" && result.BytesPerEntry > 48 {
			t.Errorf("new bytes_per_entry=%.6f exceeds 48", result.BytesPerEntry)
		}
		if mode == "legacy" && result.BytesPerEntry < 80 {
			t.Errorf("legacy bytes_per_entry=%.6f below fixture-strength bound 80", result.BytesPerEntry)
		}
	}
}

func measureImportEntryIndexBytes(t *testing.T, mode string) importEntryBytesResult {
	t.Helper()
	const generated, fallback = 1000000, 1000
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	retained := buildImportEntryIndexFixture(t, mode, generated, fallback)
	// The helper's source slices and all unretained decoder-like strings are
	// out of scope before either collection.
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(retained)
	delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	return importEntryBytesResult{Mode: mode, Entries: generated + fallback, HeapBytes: delta, BytesPerEntry: float64(delta) / float64(generated+fallback)}
}

func buildImportEntryIndexFixture(t *testing.T, mode string, generated, fallback int) any {
	t.Helper()
	random := rand.NewChaCha8([32]byte{112})
	ids, transcripts := make([]string, generated+fallback), make([]string, generated+fallback)
	for i := range ids {
		if i < generated {
			var raw [10]byte
			_, _ = random.Read(raw[:])
			ids[i] = "ent_" + importEntryTestEncoding.EncodeToString(raw[:])
		} else {
			ids[i] = fmt.Sprintf("ent_legacy_%07d", i-generated)
		}
		// Each formatted value is a fresh allocation, even for a repeated ID.
		transcripts[i] = fmt.Sprintf("trn_%016d", i%1000)
	}
	if mode == "legacy" {
		index := make(map[string]string)
		for i, entryID := range ids {
			index[entryID] = transcripts[i]
		}
		return index
	}
	if mode != "new" {
		t.Fatalf("unknown entry index child mode %q", mode)
	}
	index := &importEntryIndex{}
	for i, entryID := range ids {
		index.put(entryID, transcripts[i])
	}
	return index
}
