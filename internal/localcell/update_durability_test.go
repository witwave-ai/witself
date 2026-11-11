package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Observe completed sync calls, rather than treating readable journal bytes as
// evidence of durability. Both the started and done lines need their own syncs.
func TestUpdateStepDurabilityOrder(t *testing.T) {
	c, record := updateRecordFixture(t)
	if err := beginUpdateRecordFixture(t, c, record); err != nil {
		t.Fatal(err)
	}
	realSync, realDirectorySync := syncFile, syncUpdateJournalDirectory
	t.Cleanup(func() {
		syncFile, syncUpdateJournalDirectory = realSync, realDirectorySync
	})
	for _, state := range []updateState{updatePrepared, updateMaintenanceRequested, updateAcknowledged,
		updateQuiesced, updateRecoveryVerified, updateMigrating, updateActivated, updateHealthVerified, updateCommitted} {
		t.Run(string(state), func(t *testing.T) {
			if state != updatePrepared && state != updateMaintenanceRequested {
				record.AckedGeneration = record.MaintenanceGeneration
			}
			before := *record
			recordPath := c.updateRecordPath(record.OperationID)
			beforeBytes, beforeErr := os.ReadFile(recordPath)
			if beforeErr != nil && !errors.Is(beforeErr, os.ErrNotExist) {
				t.Fatal(beforeErr)
			}
			assertUnadvanced := func() {
				t.Helper()
				afterBytes, afterErr := os.ReadFile(recordPath)
				if *record != before || !bytes.Equal(beforeBytes, afterBytes) ||
					(beforeErr == nil) != (afterErr == nil) || afterErr != nil && !errors.Is(afterErr, os.ErrNotExist) {
					t.Fatal("record advanced before journal durability completed")
				}
				pending, err := filepath.Glob(c.path("journal", ".pending-*"))
				if err != nil || len(pending) != 0 {
					t.Fatal("record write began before journal durability completed")
				}
			}
			var events []string
			fileSyncs, directorySyncs := 0, 0
			syncFile = func(f *os.File) error {
				if f.Name() != c.path("journal", "operations.jsonl") {
					if !reflect.DeepEqual(events, []string{"journal_file", "journal_directory"}) {
						t.Fatal("record write reached sync before the journal file and directory syncs completed")
					}
					if err := realSync(f); err != nil {
						return err
					}
					events = append(events, "record_file")
					return nil
				}
				if fileSyncs == 0 {
					assertUnadvanced()
				}
				data, err := os.ReadFile(f.Name())
				if err != nil {
					t.Fatal(err)
				}
				lines := bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
				var entry updateJournalEntry
				wantStatus := "started"
				if fileSyncs == 1 {
					wantStatus = "done"
				}
				if json.Unmarshal(lines[len(lines)-1], &entry) != nil || entry.Step != string(state) || entry.State != wantStatus {
					t.Fatal("journal sync did not cover the appended step line")
				}
				if err := realSync(f); err != nil {
					return err
				}
				fileSyncs++
				events = append(events, "journal_file")
				return nil
			}
			syncUpdateJournalDirectory = func(path string) error {
				if path != c.path("journal") || fileSyncs != directorySyncs+1 || len(events) == 0 || events[len(events)-1] != "journal_file" {
					t.Fatal("journal directory sync used the wrong path or preceded the completed file sync")
				}
				if directorySyncs == 0 {
					assertUnadvanced()
				}
				if err := realDirectorySync(path); err != nil {
					return err
				}
				directorySyncs++
				events = append(events, "journal_directory")
				return nil
			}
			err := c.updateStep(record, state, func() error {
				if !reflect.DeepEqual(events, []string{"journal_file", "journal_directory", "record_file"}) {
					t.Fatal("action began before journal durability and record sync completed")
				}
				var current updateRecord
				if readJSON(recordPath, &current) != nil || current.State != state {
					t.Fatal("action began before derived record publication")
				}
				events = append(events, "action")
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"journal_file", "journal_directory", "record_file", "action", "journal_file", "journal_directory"}
			if !reflect.DeepEqual(events, want) || fileSyncs != 2 || directorySyncs != 2 {
				t.Fatalf("durability event order = %v, want %v", events, want)
			}
		})
	}
}

func TestUpdateJournalDirectorySyncEveryAppend(t *testing.T) {
	c, record := updateRecordFixture(t)
	realSync, realDirectorySync := syncFile, syncUpdateJournalDirectory
	t.Cleanup(func() {
		syncFile, syncUpdateJournalDirectory = realSync, realDirectorySync
	})
	fileSyncs, directorySyncs := 0, 0
	syncFile = func(f *os.File) error {
		if err := realSync(f); err != nil {
			return err
		}
		if f.Name() == c.path("journal", "operations.jsonl") {
			fileSyncs++
		}
		return nil
	}
	syncUpdateJournalDirectory = func(path string) error {
		if path != c.path("journal") || fileSyncs != directorySyncs+1 {
			t.Fatal("journal directory sync did not follow its file sync at the exact directory")
		}
		if err := realDirectorySync(path); err != nil {
			return err
		}
		directorySyncs++
		return nil
	}
	if err := beginUpdateRecordFixture(t, c, record); err != nil {
		t.Fatal(err)
	}
	if fileSyncs != 1 || directorySyncs != 1 {
		t.Fatal("new journal pathname was not synced")
	}
	if err := c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: "prepared", State: "started"}); err != nil {
		t.Fatal(err)
	}
	if fileSyncs != 2 || directorySyncs != 2 {
		t.Fatal("existing journal pathname was not synced on the second append")
	}
}

func TestUpdateStepSyncFailurePreservesRecord(t *testing.T) {
	for _, failure := range []string{"journal_file", "journal_directory"} {
		t.Run(failure, func(t *testing.T) {
			c, record := updateRecordFixture(t)
			if err := beginUpdateRecordFixture(t, c, record); err != nil {
				t.Fatal(err)
			}
			if err := c.updateStep(record, updatePrepared, nil); err != nil {
				t.Fatal(err)
			}
			before := *record
			beforeBytes, err := os.ReadFile(c.updateRecordPath(record.OperationID))
			if err != nil {
				t.Fatal(err)
			}
			realSync, realDirectorySync := syncFile, syncUpdateJournalDirectory
			t.Cleanup(func() {
				syncFile, syncUpdateJournalDirectory = realSync, realDirectorySync
			})
			fileSyncs, directorySyncs := 0, 0
			syncFile = func(f *os.File) error {
				if f.Name() == c.path("journal", "operations.jsonl") {
					fileSyncs++
					if failure == "journal_file" {
						return errors.New("injected journal sync failure")
					}
				}
				return realSync(f)
			}
			syncUpdateJournalDirectory = func(path string) error {
				directorySyncs++
				if path != c.path("journal") {
					t.Fatal("directory sync did not target the journal")
				}
				return errors.New("injected journal directory sync failure")
			}
			ran := false
			err = c.updateStep(record, updateMaintenanceRequested, func() error { ran = true; return nil })
			if err != errUpdatePartialState || ran || *record != before {
				t.Fatal("failed sync did not return the exact refusal before action and record advancement")
			}
			afterBytes, err := os.ReadFile(c.updateRecordPath(record.OperationID))
			if err != nil || !bytes.Equal(beforeBytes, afterBytes) {
				t.Fatal("failed sync changed the persisted derived record")
			}
			wantDirectorySyncs := 0
			if failure == "journal_directory" {
				wantDirectorySyncs = 1
			}
			if fileSyncs != 1 || directorySyncs != wantDirectorySyncs {
				t.Fatal("sync failure injection was not reached in file-before-directory order")
			}
		})
	}
}

func TestUpdateJournalReplacementBeforeOpenPreservesBothFiles(t *testing.T) {
	c, record := updateRecordFixture(t)
	if err := beginUpdateRecordFixture(t, c, record); err != nil {
		t.Fatal(err)
	}
	path := c.path("journal", "operations.jsonl")
	aside := c.path("journal", "inspected.jsonl")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := []byte("replacement evidence\n")
	realOpen := openUpdateJournalFile
	t.Cleanup(func() { openUpdateJournalFile = realOpen })
	opened := false
	openUpdateJournalFile = func(name string, flags int, mode os.FileMode) (*os.File, error) {
		if name != path || opened {
			t.Fatal("unexpected journal open")
		}
		opened = true
		if err := os.Rename(path, aside); err != nil {
			t.Fatal(err)
		}
		f, err := openPrivateFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		n, writeErr := f.Write(replacement)
		closeErr := f.Close()
		if writeErr != nil || n != len(replacement) || closeErr != nil || servicePrivateRecord(path) != nil {
			t.Fatal("could not create a distinct private regular replacement")
		}
		return realOpen(name, flags, mode)
	}
	if err := c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: "prepared", State: "started"}); err != errUpdatePartialState {
		t.Fatal("replaced journal did not return the exact refusal")
	}
	if !opened {
		t.Fatal("journal replacement seam was not reached")
	}
	for name, want := range map[string][]byte{aside: original, path: replacement} {
		got, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("refused append changed inspected or replacement journal bytes")
		}
	}
}
