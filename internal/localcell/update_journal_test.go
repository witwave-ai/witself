package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func updateRecordFixture(t *testing.T) (*Cell, *updateRecord) {
	t.Helper()
	c, m := unitCell(t)
	return c, &updateRecord{
		OperationID: "update_test", InstallationID: m.InstallationID, MaintenanceGeneration: 1,
		Target:  updateComponentSetIdentity{ManifestDigest: strings.Repeat("a", 64), Channel: "stable", Sequence: 1, Tag: "v1.0.0"},
		Channel: "unmanaged", SchemaFrom: 1, SchemaTo: 1, LayoutVersion: LayoutVersion, PGMajor: 16, State: updatePrepared,
	}
}

// beginUpdateRecordFixture creates test-owned immutable intent. Production has no
// first-intent writer until distribution custody can authenticate every identity.
func beginUpdateRecordFixture(t *testing.T, c *Cell, record *updateRecord) error {
	t.Helper()
	if err := c.authenticateUpdateInstallation(record); err != nil || record.State != updatePrepared || record.AckedGeneration != 0 {
		return errUpdatePartialState
	}
	progress, err := c.readUpdateJournal()
	if err != nil {
		return err
	}
	if progress.pending != nil || progress.tornTail {
		return errUpdatePartialState
	}
	if _, err := os.Lstat(c.updateRecordPath(record.OperationID)); !errors.Is(err, os.ErrNotExist) {
		return errUpdatePartialState
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return errUpdatePartialState
	}
	f, err := openPrivateFile(c.updateIntentPath(record.OperationID), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errUpdatePartialState
	}
	data = append(data, '\n')
	n, writeErr := f.Write(data)
	if writeErr == nil && n == len(data) {
		writeErr = syncFile(f)
	}
	closeErr := f.Close()
	if writeErr != nil || n != len(data) || closeErr != nil || syncDirectory(c.path("journal")) != nil {
		return errUpdatePartialState
	}
	return c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: "operation", State: "started", UpdateID: record.OperationID})
}

func TestUpdateRecordAndJournal(t *testing.T) {
	states := []updateState{updatePrepared, updateMaintenanceRequested, updateAcknowledged, updateQuiesced,
		updateRecoveryVerified, updateMigrating, updateActivated, updateHealthVerified, updateCommitted}
	t.Run("closed_set", func(t *testing.T) {
		for _, state := range states {
			if !state.valid() || !validUpdateJournalStep("update", string(state)) {
				t.Fatal("declared update state is rejected")
			}
		}
		for _, invalid := range []string{"", "prepare", "health", "done", "PREPARED", "prepared\n", "rollback"} {
			if updateState(invalid).valid() || validUpdateJournalStep("update", invalid) {
				t.Fatal("undeclared update state is accepted")
			}
		}
		if !validUpdateJournalStep("update", "operation") || !validUpdateJournalStep("update", "revert_stop") {
			t.Fatal("update journal operation boundary is rejected")
		}
	})
	t.Run("journal_before_record_and_action", func(t *testing.T) {
		beforeSync := syncFile
		t.Cleanup(func() { syncFile = beforeSync })
		c, record := updateRecordFixture(t)
		if err := beginUpdateRecordFixture(t, c, record); err != nil {
			t.Fatal(err)
		}
		for _, state := range states {
			if updateStateIndex(state) >= updateStateIndex(updateAcknowledged) {
				record.AckedGeneration = record.MaintenanceGeneration
			}
			originalSync := syncFile
			observed := false
			syncFile = func(f *os.File) error {
				if f.Name() != c.path("journal", "operations.jsonl") {
					progress, err := c.readUpdateJournal()
					if err != nil || progress.updateState != state || progress.done[string(state)] {
						t.Fatal("derived record was flushed before the started journal boundary")
					}
					observed = true
				}
				return originalSync(f)
			}
			err := c.updateStep(record, state, func() error {
				var current updateRecord
				if readJSON(c.updateRecordPath(record.OperationID), &current) != nil || current.State != state {
					t.Fatal("action ran before derived record publication")
				}
				return nil
			})
			syncFile = originalSync
			if err != nil || !observed {
				t.Fatal("durable update boundary was not observed")
			}
			progress, err := c.readUpdateJournal()
			if err != nil || progress.pending == nil || progress.pending.Operation != "update" || progress.updateState != state || !progress.done[string(state)] {
				t.Fatal("journal did not reconstruct the durable update step")
			}

		}
		if err := c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: "operation", State: "done"}); err != nil {
			t.Fatal(err)
		}
		progress, err := c.readUpdateJournal()
		if err != nil || progress.pending != nil || !progress.updateComplete {
			t.Fatal("completed update was reopened")
		}
	})
	for _, kind := range []string{"missing", "stale"} {
		t.Run(kind+"_record_repaired_from_journal", func(t *testing.T) {
			beforeSync := syncFile
			t.Cleanup(func() { syncFile = beforeSync })
			c, record := updateRecordFixture(t)
			if err := beginUpdateRecordFixture(t, c, record); err != nil {
				t.Fatal(err)
			}
			if kind == "stale" {
				if err := c.updateStep(record, updatePrepared, nil); err != nil {
					t.Fatal(err)
				}
			} else if err := c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: "prepared", State: "started"}); err != nil {
				t.Fatal(err)
			}
			if err := c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: "maintenance_requested", State: "started"}); err != nil {
				t.Fatal(err)
			}

			first, err := c.reconcileUpdateRecord()
			if err != nil || first.State != updateMaintenanceRequested || first.AckedGeneration != 0 || first.Target != record.Target {
				t.Fatal("durable journal did not repair the incomplete record")
			}
			var durable updateRecord
			if readJSON(c.updateRecordPath(record.OperationID), &durable) != nil || durable != *first {
				t.Fatal("reconstructed update record was not durably published")
			}
			originalSync := syncFile
			syncFile = func(*os.File) error { return errors.New("unexpected repeat write") }
			second, err := c.reconcileUpdateRecord()
			syncFile = originalSync
			if err != nil || *second != *first {
				t.Fatal("record reconciliation was not idempotent")
			}
		})
	}
	t.Run("record_ahead_is_corruption", func(t *testing.T) {
		c, record := updateRecordFixture(t)
		if err := beginUpdateRecordFixture(t, c, record); err != nil {
			t.Fatal(err)
		}
		if err := c.updateStep(record, updatePrepared, nil); err != nil {
			t.Fatal(err)
		}
		record.State = updateMaintenanceRequested
		if err := c.writeUpdateRecord(record); err != nil {
			t.Fatal(err)
		}
		if _, err := c.reconcileUpdateRecord(); !errors.Is(err, errUpdatePartialState) {
			t.Fatal("a record ahead of its journal was accepted")
		}
	})
	t.Run("reopened_request_discards_stale_ack", func(t *testing.T) {
		c, record := updateRecordFixture(t)
		if err := beginUpdateRecordFixture(t, c, record); err != nil {
			t.Fatal(err)
		}
		for _, state := range []updateState{updatePrepared, updateMaintenanceRequested, updateAcknowledged} {
			if state == updateAcknowledged {
				record.AckedGeneration = record.MaintenanceGeneration
			}
			if err := c.updateStep(record, state, nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: string(updateMaintenanceRequested), State: "started"}); err != nil {
			t.Fatal(err)
		}
		repaired, err := c.reconcileUpdateRecord()
		if err != nil || repaired.State != updateMaintenanceRequested || repaired.AckedGeneration != 0 {
			t.Fatal("reopened maintenance request retained stale acknowledgement")
		}
	})
	t.Run("foreign_intent_preserved", func(t *testing.T) {
		c, record := updateRecordFixture(t)
		foreign := []byte("foreign retained bytes")
		if err := testWriteFile(c.updateIntentPath(record.OperationID), foreign, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: "operation", State: "started", UpdateID: record.OperationID}); !errors.Is(err, errUpdatePartialState) {
			t.Fatal("foreign intent authorized a journal append")
		}
		assertFileBytes(t, c.updateIntentPath(record.OperationID), foreign, "foreign intent bytes changed")
		if _, err := os.Stat(c.path("journal", "operations.jsonl")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed intent creation began an update operation")
		}
	})
	t.Run("zero_fields_preserve_legacy_encoding", func(t *testing.T) {
		_, manifest := unitCell(t)
		data, err := json.Marshal(manifest)
		if err != nil || bytes.Contains(data, []byte("active_generation")) || bytes.Contains(data, []byte("maintenance_generation")) {
			t.Fatal("zero update fields changed legacy manifest encoding")
		}
		entry := updateJournalEntry{Operation: "stop", Step: "operation", State: "started", At: manifest.CreatedAt}
		data, err = json.Marshal(entry)
		if err != nil || bytes.Contains(data, []byte("update_id")) {
			t.Fatal("legacy journal entry gained update metadata")
		}
	})
}

// appendUpdateFixtureEntry deliberately bypasses the writer to construct invalid
// durable input for parser tests; every path is confined to unitCell's TempDir.
func appendUpdateFixtureEntry(t *testing.T, c *Cell, entry updateJournalEntry) {
	t.Helper()
	entry.At = time.Now().UTC()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	path := c.path("journal", "operations.jsonl")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := testWriteFile(path, append(old, append(data, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateJournalRejectsUnboundOrSkippedState(t *testing.T) {
	for _, kind := range []string{"unbound", "skipped", "wrong_identity"} {
		t.Run(kind, func(t *testing.T) {
			c, record := updateRecordFixture(t)
			switch kind {
			case "unbound":
				appendUpdateFixtureEntry(t, c, updateJournalEntry{Operation: "update", Step: "operation", State: "started"})
			case "skipped":
				if err := beginUpdateRecordFixture(t, c, record); err != nil {
					t.Fatal(err)
				}
				appendUpdateFixtureEntry(t, c, updateJournalEntry{Operation: "update", Step: "activated", State: "started"})
			case "wrong_identity":
				appendUpdateFixtureEntry(t, c, updateJournalEntry{Operation: "stop", Step: "operation", State: "started", UpdateID: record.OperationID})
			}
			if _, err := c.readUpdateJournal(); !errors.Is(err, errUpdatePartialState) {
				t.Fatal("unbound or nonsequential update journal was accepted")
			}
		})
	}
}

func TestUpdateRecordCustody(t *testing.T) {
	defects := []string{"symlink"}
	if runtime.GOOS != "windows" {
		defects = append(defects, "mode")
	}
	for _, kind := range []string{"intent", "derived"} {
		for _, defect := range defects {
			t.Run(kind+"/"+defect, func(t *testing.T) {
				c, record := updateRecordFixture(t)
				if err := beginUpdateRecordFixture(t, c, record); err != nil {
					t.Fatal(err)
				}
				if err := c.updateStep(record, updatePrepared, nil); err != nil {
					t.Fatal(err)
				}
				path := c.updateRecordPath(record.OperationID)
				if kind == "intent" {
					path = c.updateIntentPath(record.OperationID)
				}
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				target := path
				switch defect {
				case "mode":
					if os.Chmod(path, 0o644) != nil {
						t.Fatal("record custody fixture could not change its own file mode")
					}
				case "symlink":
					target = path + ".retained"
					if os.Rename(path, target) != nil {
						t.Fatal("record custody fixture could not retain its own record")
					}
					if err := os.Symlink(target, path); err != nil {
						if runtime.GOOS == "windows" {
							t.Skip("LC-S2-PW3: Windows symlink fixture privileges unavailable")
						}
						t.Fatal("record custody fixture could not install symlink")
					}
				}
				if _, err := c.reconcileUpdateRecord(); !errors.Is(err, errUpdatePartialState) {
					t.Fatal("unsafe record custody was accepted")
				}
				assertFileBytes(t, target, original, "unsafe record custody changed retained bytes")
			})
		}
	}
}

func TestUpdateStepRejectsMetadataDriftBeforeAction(t *testing.T) {
	mutations := map[string]func(*updateRecord){
		"target":         func(record *updateRecord) { record.Target.Sequence++ },
		"source":         func(record *updateRecord) { record.Source = record.Target },
		"installation":   func(record *updateRecord) { record.InstallationID = "another_installation" },
		"generation":     func(record *updateRecord) { record.MaintenanceGeneration++ },
		"channel":        func(record *updateRecord) { record.Channel = "shell" },
		"channel_action": func(record *updateRecord) { record.ChannelAction = "changed" },
		"operation":      func(record *updateRecord) { record.OperationID = "changed" },
		"layout":         func(record *updateRecord) { record.LayoutVersion++ },
		"pg_major":       func(record *updateRecord) { record.PGMajor++ },
		"schema":         func(record *updateRecord) { record.SchemaTo++ },
		"recovery":       func(record *updateRecord) { record.RecoveryPoint = "changed" },
		"retained":       func(record *updateRecord) { record.RetainedGenerations++ },
		"stale_ack":      func(record *updateRecord) { record.AckedGeneration = record.MaintenanceGeneration },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			c, record := updateRecordFixture(t)
			if err := beginUpdateRecordFixture(t, c, record); err != nil {
				t.Fatal(err)
			}
			if err := c.updateStep(record, updatePrepared, nil); err != nil {
				t.Fatal(err)
			}
			journal, err := os.ReadFile(c.path("journal", "operations.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			derivedPath := c.updateRecordPath(record.OperationID)
			derived, err := os.ReadFile(derivedPath)
			if err != nil {
				t.Fatal(err)
			}
			mutate(record)
			called := false
			err = c.updateStep(record, updateMaintenanceRequested, func() error { called = true; return nil })
			if !errors.Is(err, errUpdatePartialState) || called {
				t.Fatal("changed update authority reached an action or journal boundary")
			}
			assertFileBytes(t, c.path("journal", "operations.jsonl"), journal, "metadata refusal changed the journal")
			assertFileBytes(t, derivedPath, derived, "metadata refusal changed the derived record")
		})
	}
}

// TestUpdateForeignRecordPreserved is the permanent form of the review probes.
// Both the intent and stale/torn derived bytes claim another installation; each
// mutator must refuse before replacing or appending any existing evidence.
func TestUpdateForeignRecordPreserved(t *testing.T) {
	for _, kind := range []string{"stale", "torn"} {
		for _, method := range []string{"reconcile", "write", "step", "append"} {
			t.Run(kind+"/"+method, func(t *testing.T) {
				c, record := updateRecordFixture(t)
				if err := beginUpdateRecordFixture(t, c, record); err != nil {
					t.Fatal(err)
				}
				if err := c.updateStep(record, updatePrepared, nil); err != nil {
					t.Fatal(err)
				}
				appendUpdateFixtureEntry(t, c, updateJournalEntry{Operation: "update", Step: "maintenance_requested", State: "started"})
				record.InstallationID = "foreign_installation"
				if err := writeAtomic(c.updateIntentPath(record.OperationID), record, nil); err != nil {
					t.Fatal(err)
				}
				if kind == "stale" {
					if err := writeAtomic(c.updateRecordPath(record.OperationID), record, nil); err != nil {
						t.Fatal(err)
					}
				} else if err := testWriteFile(c.updateRecordPath(record.OperationID), []byte(`{"operation_id":"update_test","installation_id":"foreign_installation"`), 0o600); err != nil {
					t.Fatal(err)
				}
				before := updateJournalSnapshot(t, c)
				var err error
				switch method {
				case "reconcile":
					_, err = c.reconcileUpdateRecord()
				case "write":
					record.State = updateMaintenanceRequested
					err = c.writeUpdateRecord(record)
				case "step":
					err = c.updateStep(record, updateMaintenanceRequested, func() error {
						t.Fatal("foreign identity reached update action")
						return nil
					})
				case "append":
					err = c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: "maintenance_requested", State: "done"})
				}
				if !errors.Is(err, errUpdatePartialState) {
					t.Fatal("foreign installation did not return the fixed update refusal")
				}
				assertUpdateJournalSnapshot(t, c, before)
			})
		}
	}
}

func TestUpdateMissingIntentRefusesCreation(t *testing.T) {
	for _, method := range []string{"write", "append"} {
		t.Run(method, func(t *testing.T) {
			c, record := updateRecordFixture(t)
			before := updateJournalSnapshot(t, c)
			var err error
			if method == "write" {
				err = c.writeUpdateRecord(record)
			} else {
				err = c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: "operation", State: "started", UpdateID: record.OperationID})
			}
			if !errors.Is(err, errUpdatePartialState) {
				t.Fatal("missing immutable intent authorized an update write")
			}
			assertUpdateJournalSnapshot(t, c, before)
		})
	}
}

func TestUpdateTornRecordPreserved(t *testing.T) {
	for _, method := range []string{"reconcile", "write", "step", "append"} {
		t.Run(method, func(t *testing.T) {
			c, record := updateRecordFixture(t)
			if err := beginUpdateRecordFixture(t, c, record); err != nil {
				t.Fatal(err)
			}
			appendUpdateFixtureEntry(t, c, updateJournalEntry{Operation: "update", Step: "prepared", State: "started"})
			if err := testWriteFile(c.updateRecordPath(record.OperationID), []byte(`{"operation_id":`), 0o600); err != nil {
				t.Fatal(err)
			}
			before := updateJournalSnapshot(t, c)
			var err error
			switch method {
			case "reconcile":
				_, err = c.reconcileUpdateRecord()
			case "write":
				err = c.writeUpdateRecord(record)
			case "step":
				err = c.updateStep(record, updatePrepared, nil)
			case "append":
				err = c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: "prepared", State: "done"})
			}
			if !errors.Is(err, errUpdatePartialState) {
				t.Fatal("existing torn record was treated as owned or absent")
			}
			assertUpdateJournalSnapshot(t, c, before)
		})
	}
}

func TestUpdateWriteAuthenticatesAllMetadataBeforeWrite(t *testing.T) {
	for _, field := range []string{"source", "target", "generation", "installation", "operation", "channel", "channel_action", "schema_from", "schema_to", "layout", "pg_major", "recovery", "retained"} {
		t.Run(field, func(t *testing.T) {
			c, record := updateRecordFixture(t)
			if err := beginUpdateRecordFixture(t, c, record); err != nil {
				t.Fatal(err)
			}
			if err := c.updateStep(record, updatePrepared, nil); err != nil {
				t.Fatal(err)
			}
			before := updateJournalSnapshot(t, c)
			switch field {
			case "source":
				record.Source = record.Target
			case "target":
				record.Target.Sequence++
			case "generation":
				record.MaintenanceGeneration++
			case "installation":
				record.InstallationID = "foreign_installation"
			case "operation":
				record.OperationID = "other_operation"
			case "channel":
				record.Channel = "shell"
			case "channel_action":
				record.ChannelAction = "changed"
			case "schema_from":
				record.SchemaFrom--
			case "schema_to":
				record.SchemaTo++
			case "layout":
				record.LayoutVersion++
			case "pg_major":
				record.PGMajor++
			case "recovery":
				record.RecoveryPoint = "changed"
			case "retained":
				record.RetainedGenerations++
			}
			if err := c.writeUpdateRecord(record); !errors.Is(err, errUpdatePartialState) {
				t.Fatal("changed identity metadata reached the record writer")
			}
			assertUpdateJournalSnapshot(t, c, before)
		})
	}
}

func updateJournalSnapshot(t *testing.T, c *Cell) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(c.path("journal"))
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		data, err := os.ReadFile(c.path("journal", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = data
	}
	return result
}

func assertUpdateJournalSnapshot(t *testing.T, c *Cell, before map[string][]byte) {
	t.Helper()
	after := updateJournalSnapshot(t, c)
	if len(after) != len(before) {
		t.Fatal("identity refusal created or removed journal evidence or left a temporary file")
	}
	for name, data := range before {
		if !bytes.Equal(after[name], data) {
			t.Fatal("identity refusal changed exact prior journal or record bytes")
		}
	}
}
