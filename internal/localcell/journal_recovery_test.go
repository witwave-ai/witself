package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func journalRecord(t *testing.T, operation, step, state string) []byte {
	t.Helper()
	data, err := json.Marshal(journalEntry{operation, step, state, time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func TestJournalAppendSync(t *testing.T) {
	for _, state := range []string{"started", "done"} {
		for _, outcome := range []string{"success", "failure"} {
			t.Run(state+"/"+outcome, func(t *testing.T) {
				c, _ := unitCell(t)
				originalSync := syncFile
				t.Cleanup(func() { syncFile = originalSync })
				calls := 0
				syncFile = func(f *os.File) error {
					calls++
					if f.Name() != c.path("journal", "operations.jsonl") {
						t.Fatal("append synced the wrong file")
					}
					data, err := os.ReadFile(f.Name())
					if err != nil || !bytes.HasSuffix(data, []byte{'\n'}) || bytes.Count(data, []byte{'\n'}) != 1 {
						t.Fatal("append sync preceded the complete newline-terminated record")
					}
					var entry journalEntry
					if err := json.Unmarshal(data, &entry); err != nil || entry.Operation != "stop" || entry.Step != "operation" || entry.State != state {
						t.Fatal("append sync did not cover the requested journal record")
					}
					if outcome == "failure" {
						return errors.New("synthetic private sync details")
					}
					return originalSync(f)
				}
				err := c.journal("stop", "operation", state)
				if calls != 1 {
					t.Fatalf("journal append synced %d times; want exactly one", calls)
				}
				if outcome == "failure" {
					if err == nil || err.Error() != "cannot sync local cell journal" {
						t.Fatal("append sync failure did not return the value-free journal error")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestJournalTailTruncationSync(t *testing.T) {
	for _, outcome := range []string{"success", "failure"} {
		t.Run(outcome, func(t *testing.T) {
			c, _ := unitCell(t)
			path := c.path("journal", "operations.jsonl")
			prefix := journalRecord(t, "stop", "operation", "started")
			if err := testWriteFile(path, append(bytes.Clone(prefix), []byte(`{"operation":`)...), 0o600); err != nil {
				t.Fatal(err)
			}
			originalSync := syncFile
			t.Cleanup(func() { syncFile = originalSync })
			want := []struct{ step, state string }{
				{"operation", "started"}, {"revert_stop", "started"},
				{"revert_stop", "done"}, {"operation", "done"},
			}
			calls := 0
			syncFile = func(f *os.File) error {
				if f.Name() != path {
					return originalSync(f) // Atomic manifest publication shares the seam.
				}
				calls++
				data, err := os.ReadFile(path)
				if err != nil || calls > len(want) || bytes.Count(data, []byte{'\n'}) != calls {
					t.Fatal("Repair did not sync the truncation once before syncing each recovery append")
				}
				if calls == 1 && !bytes.Equal(data, prefix) {
					t.Fatal("first recovery sync did not cover exactly the truncated durable prefix")
				}
				lines := bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
				var entry journalEntry
				if err := json.Unmarshal(lines[len(lines)-1], &entry); err != nil || entry.Operation != "stop" || entry.Step != want[calls-1].step || entry.State != want[calls-1].state {
					t.Fatal("recovery journal sync order changed")
				}
				if outcome == "failure" {
					return errors.New("synthetic private sync details")
				}
				return originalSync(f)
			}
			err := c.Repair(t.Context())
			if outcome == "failure" {
				if err == nil || err.Error() != "cannot sync repaired local cell journal" || calls != 1 {
					t.Fatal("truncation sync failure did not stop Repair with the value-free journal error")
				}
				assertFileBytes(t, path, prefix, "Repair appended after a failed truncation sync")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if calls != len(want) {
				t.Fatalf("Repair synced journal %d times; want one truncation and three appends", calls)
			}
			if err := c.Repair(t.Context()); err != nil || calls != len(want) {
				t.Fatal("repeated Repair performed another journal sync")
			}
		})
	}
}

func TestJournalTornTailRepair(t *testing.T) {
	for _, boundary := range []string{"operation_started", "step_started", "step_done", "operation_done", "first_record"} {
		for _, signature := range []string{"partial", "zero_fill", "partial_zero_fill", "complete_without_newline", "long_zero_fill", "partial_long_zero_fill"} {
			t.Run(boundary+"/"+signature, func(t *testing.T) {
				c, _ := unitCell(t)
				if err := testMkdir(c.path("pg"), 0o700); err != nil {
					t.Fatal(err)
				}
				const retained = "retained synthetic PostgreSQL bytes"
				if err := testWriteFile(c.path("pg", "retained-data"), []byte(retained), 0o600); err != nil {
					t.Fatal(err)
				}
				// Recover from a settled prior operation or the empty durable prefix,
				// without inventing an operation identity from the unacknowledged tail.
				data := journalRecord(t, "stop", "operation", "started")
				data = append(data, journalRecord(t, "stop", "operation", "done")...)
				entries := []struct{ step, state string }{
					{"operation", "started"}, {"postgres", "started"},
					{"postgres", "done"}, {"operation", "done"},
				}
				cut := map[string]int{"operation_started": 0, "step_started": 1, "step_done": 2, "operation_done": 3}[boundary]
				for _, entry := range entries[:cut] {
					data = append(data, journalRecord(t, "start", entry.step, entry.state)...)
				}
				if boundary == "first_record" {
					data = nil
				}
				prefix := bytes.Clone(data)
				last := journalRecord(t, "start", entries[cut].step, entries[cut].state)
				switch signature {
				case "partial":
					data = append(data, last[:len(last)/2]...)
				case "zero_fill":
					data = append(data, make([]byte, 32)...)
				case "long_zero_fill":
					data = append(data, make([]byte, 128*1024)...)
				case "partial_long_zero_fill":
					data = append(data, last[:len(last)/2]...)
					data = append(data, make([]byte, 128*1024)...)
				case "partial_zero_fill":
					data = append(data, last[:len(last)/2]...)
					data = append(data, make([]byte, 32)...)
				case "complete_without_newline":
					data = append(data, bytes.TrimSuffix(last, []byte{'\n'})...)
				}
				path := c.path("journal", "operations.jsonl")
				if err := testWriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				pgBefore, err := os.Stat(c.path("pg", "retained-data"))
				if err != nil {
					t.Fatal(err)
				}
				inspection, err := c.Inspect(t.Context())
				if err != nil || !inspection.JournalTailIncomplete || (inspection.Interrupted != nil) != (cut > 0) {
					t.Fatalf("Inspect did not recover the validated prefix: %v", err)
				}
				assertFileBytes(t, path, data, "Inspect changed the journal")
				lock, err := c.own(false)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Repair(t.Context()); !errors.Is(err, ErrLocked) {
					t.Fatal("Repair reconciled a tail without exclusive ownership")
				}
				assertFileBytes(t, path, data, "locked Repair changed the journal")
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
				if err := c.Repair(t.Context()); err != nil {
					t.Fatalf("Repair rejected a torn final append: %v", err)
				}
				repaired, err := os.ReadFile(path)
				if err != nil || !bytes.HasPrefix(repaired, prefix) {
					t.Fatal("Repair changed the durable journal prefix")
				}
				if cut == 0 && !bytes.Equal(repaired, prefix) {
					t.Fatal("Repair did not truncate to the exact durable prefix")
				}
				for _, line := range bytes.Split(bytes.TrimSuffix(repaired, []byte{'\n'}), []byte{'\n'}) {
					if len(repaired) != 0 && !json.Valid(line) {
						t.Fatal("Repair appended after an unreconciled torn tail")
					}
				}
				inspection, err = c.Inspect(t.Context())
				if err != nil || inspection.Interrupted != nil || inspection.JournalTailIncomplete {
					t.Fatalf("Repair left unsettled journal state: %v", err)
				}
				manifest, err := os.ReadFile(c.path("manifest.json"))
				if err != nil {
					t.Fatal(err)
				}
				journalBefore, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				manifestBefore, err := os.Stat(c.path("manifest.json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Repair(t.Context()); err != nil {
					t.Fatal(err)
				}
				for _, before := range []struct {
					path string
					info os.FileInfo
				}{{path, journalBefore}, {c.path("manifest.json"), manifestBefore}, {c.path("pg", "retained-data"), pgBefore}} {
					after, err := os.Stat(before.path)
					if err != nil || !os.SameFile(before.info, after) || !before.info.ModTime().Equal(after.ModTime()) {
						t.Fatal("Repair rewrote retained data or second Repair changed a durable file")
					}
				}
				assertFileBytes(t, path, repaired, "second Repair wrote journal records")
				assertFileBytes(t, c.path("manifest.json"), manifest, "second Repair rewrote manifest")
				assertFileBytes(t, c.path("pg", "retained-data"), []byte(retained), "Repair changed PGDATA")
			})
		}
	}
}

func assertFileBytes(t *testing.T, path string, want []byte, failure string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal(failure)
	}
}

func TestJournalCorruptionFailsClosed(t *testing.T) {
	begin := journalRecord(t, "start", "operation", "started")
	complete := journalRecord(t, "start", "postgres", "started")
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"interior", append(append(bytes.Clone(begin), []byte("{\"operation\":\n")...), complete...)},
		{"committed-final", append(bytes.Clone(begin), []byte("{\"operation\":\n")...)},
		{"invalid-final-syntax", append(bytes.Clone(begin), []byte("{\"operation\":!")...)},
		{"interior-nul", append(append(bytes.Clone(begin), bytes.Replace(complete, []byte("postgres"), []byte("post\x00gres"), 1)...), complete...)},
		{"committed-zero-fill", append(append(bytes.Clone(begin), make([]byte, 32)...), '\n')},
		{"committed-long-zero-fill", append(append(bytes.Clone(begin), make([]byte, 128*1024)...), '\n')},
		{"invalid-prefix-long-zero-fill", append(append(bytes.Clone(begin), []byte("{\"operation\":!")...), make([]byte, 128*1024)...)},
		{"long-zero-fill-followed-by-garbage", append(append(bytes.Clone(begin), make([]byte, 128*1024)...), 'x')},
		{"newline-terminated-garbage", append(bytes.Clone(begin), []byte("garbage\n")...)},
		{"value-followed-by-garbage", append(append(bytes.Clone(begin), bytes.TrimSuffix(complete, []byte{'\n'})...), []byte("garbage")...)},
		{"value-followed-by-partial-value", append(append(bytes.Clone(begin), bytes.TrimSuffix(complete, []byte{'\n'})...), '{')},
		{"two-values-without-delimiter", append(append(bytes.Clone(begin), bytes.TrimSuffix(complete, []byte{'\n'})...), bytes.TrimSuffix(complete, []byte{'\n'})...)},
		{"first-record-garbage", []byte("garbage")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := unitCell(t)
			path := c.path("journal", "operations.jsonl")
			if err := testWriteFile(path, test.data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Inspect(t.Context()); err == nil || !strings.Contains(err.Error(), "manual recovery required") {
				t.Fatal("Inspect accepted committed or ambiguous corruption")
			}
			if err := c.Repair(t.Context()); err == nil || !strings.Contains(err.Error(), "manual recovery required") {
				t.Fatal("Repair accepted committed or ambiguous corruption")
			}
			assertFileBytes(t, path, test.data, "Repair changed corrupt journal evidence")
		})
	}
}
