package localcell

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

var syncUpdateJournalDirectory = syncDirectory
var openUpdateJournalFile = openPrivateFile

func validUpdateOperationID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, ch := range value {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
			continue
		default:
			return false
		}
	}
	return true
}

func updateStateIndex(state updateState) int {
	for i, candidate := range []updateState{updatePrepared, updateMaintenanceRequested, updateAcknowledged,
		updateQuiesced, updateRecoveryVerified, updateMigrating, updateActivated, updateHealthVerified, updateCommitted} {
		if state == candidate {
			return i
		}
	}
	return -1
}

func validUpdateJournalTransition(from, to updateState) bool {
	// A lost acknowledgement must be requested again; it cannot authorize a drain.
	return (from == "" || from.valid()) && to.valid() && (to == from || updateStateIndex(to) == updateStateIndex(from)+1 ||
		from == updateAcknowledged && to == updateMaintenanceRequested)
}

func validUpdateSetIdentity(identity updateComponentSetIdentity, allowEmpty bool) bool {
	if allowEmpty && identity == (updateComponentSetIdentity{}) {
		return true
	}
	digest, err := hex.DecodeString(identity.ManifestDigest)
	return err == nil && len(digest) == 32 && identity.ManifestDigest == strings.ToLower(identity.ManifestDigest) &&
		identity.Sequence > 0 && safeUpdateLabel(identity.Channel, 64) && safeUpdateLabel(identity.Tag, 128)
}

func safeUpdateLabel(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, ch := range value {
		if ch < 0x21 || ch > 0x7e || ch == '/' || ch == '\\' {
			return false
		}
	}
	return true
}

func validateUpdateRecord(record *updateRecord) error {
	if record == nil || !validUpdateOperationID(record.OperationID) || !validUpdateOperationID(record.InstallationID) ||
		record.MaintenanceGeneration <= 0 || !record.State.valid() ||
		!validUpdateSetIdentity(record.Source, true) || !validUpdateSetIdentity(record.Target, false) ||
		record.LayoutVersion != LayoutVersion || record.PGMajor != 16 || record.SchemaFrom < 0 || record.SchemaTo < record.SchemaFrom ||
		record.AckedGeneration != 0 && record.AckedGeneration != record.MaintenanceGeneration || record.RetainedGenerations < 0 {
		return errUpdatePartialState
	}
	switch record.Channel {
	case "shell", "homebrew", "linux_package", "msi", "unmanaged":
	default:
		return errUpdatePartialState
	}
	if record.ChannelAction != "" && !validUpdateOperationID(record.ChannelAction) ||
		record.RecoveryPoint != "" && !validUpdateOperationID(record.RecoveryPoint) {
		return errUpdatePartialState
	}
	return nil
}

func (c *Cell) updateRecordPath(id string) string {
	return c.path("journal", "update-"+id+".json")
}

func (c *Cell) updateIntentPath(id string) string {
	return c.path("journal", "update-"+id+".intent.json")
}

// authenticateUpdateInstallation binds an existing private intent to the current
// installation. Initial intent creation requires the future distribution-custody
// seam; no production helper in these foundations creates an intent.
func (c *Cell) authenticateUpdateInstallation(record *updateRecord) error {
	if validateUpdateRecord(record) != nil || c.validateRoot(false) != nil ||
		privateDir(c.path("journal"), false) != nil || servicePrivateRecord(c.path("manifest.json")) != nil {
		return errUpdatePartialState
	}
	manifest, err := readManifest(c.cfg.Root)
	if err != nil || !manifest.Installed || manifest.InstallationID != record.InstallationID || manifest.LayoutVersion != record.LayoutVersion {
		return errUpdatePartialState
	}
	return nil
}

// authenticateUpdateRecord checks every immutable field against the private
// intent and proves ownership of any existing derived record. Torn bytes cannot
// prove their identity and are retained for future supported recovery.
func (c *Cell) authenticateUpdateRecord(record *updateRecord) error {
	if err := c.authenticateUpdateInstallation(record); err != nil {
		return err
	}
	intent, err := c.readUpdateIntent(record.OperationID)
	if err != nil || !sameUpdateMetadata(record, intent) {
		return errUpdatePartialState
	}
	previous, missing, err := c.readDerivedUpdateRecord(record.OperationID)
	if err != nil || !missing && !sameUpdateMetadata(previous, intent) {
		return errUpdatePartialState
	}
	return nil
}

func (c *Cell) writeUpdateRecord(record *updateRecord) error {
	if err := c.authenticateUpdateRecord(record); err != nil {
		return err
	}
	if err := writeAtomic(c.updateRecordPath(record.OperationID), record, func() error {
		return c.authenticateUpdateRecord(record)
	}); err != nil {
		return errUpdatePartialState
	}
	return nil
}

// appendUpdateJournalEntry requires the root operation lock. Identity and the
// complete durable prefix are checked before creating or appending the journal;
// a foreign, unsafe, torn or nonsequential prefix is never modified.
func (c *Cell) appendUpdateJournalEntry(record *updateRecord, entry updateJournalEntry) error {
	if c.authenticateUpdateRecord(record) != nil || entry.Operation != "update" ||
		!validUpdateJournalStep(entry.Operation, entry.Step) || entry.Cause != "" || entry.State != "started" && entry.State != "done" {
		return errUpdatePartialState
	}
	progress, err := c.readUpdateJournal()
	if err != nil || progress.tornTail {
		return errUpdatePartialState
	}
	if entry.Step == "operation" && entry.State == "started" {
		if progress.pending != nil || entry.UpdateID != record.OperationID {
			return errUpdatePartialState
		}
	} else if progress.pending == nil || progress.pending.Operation != "update" || progress.updateID != record.OperationID || entry.UpdateID != "" ||
		updateState(entry.Step).valid() && !validUpdateJournalTransition(progress.updateState, updateState(entry.Step)) {
		return errUpdatePartialState
	}
	path := c.path("journal", "operations.jsonl")
	info, err := os.Lstat(path)
	flags := os.O_WRONLY | os.O_APPEND
	if errors.Is(err, os.ErrNotExist) {
		flags |= os.O_CREATE | os.O_EXCL
	} else if err != nil || !os.SameFile(info, info) || servicePrivateRecord(path) != nil {
		// Windows FileInfo resolves its file ID lazily from the pathname. The
		// self-comparison pins it before the open, so a replacement that takes
		// the pathname afterwards fails the identity check below.
		return errUpdatePartialState
	}
	f, err := openUpdateJournalFile(path, flags, 0o600)
	if err != nil {
		return errUpdatePartialState
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !privatePassword(path, opened) || info != nil && !os.SameFile(info, opened) ||
		c.authenticateUpdateRecord(record) != nil {
		return errUpdatePartialState
	}
	entry.At = time.Now().UTC()
	data, err := json.Marshal(entry)
	if err != nil {
		return errUpdatePartialState
	}
	data = append(data, '\n')
	n, err := f.Write(data)
	if err != nil || n != len(data) || syncFile(f) != nil {
		return errUpdatePartialState
	}
	if syncUpdateJournalDirectory(c.path("journal")) != nil {
		return errUpdatePartialState
	}
	return nil
}

func (c *Cell) readUpdateIntent(id string) (*updateRecord, error) {
	if !validUpdateOperationID(id) {
		return nil, errUpdatePartialState
	}
	var intent updateRecord
	if servicePrivateRecord(c.updateIntentPath(id)) != nil || readJSON(c.updateIntentPath(id), &intent) != nil ||
		c.authenticateUpdateInstallation(&intent) != nil || intent.OperationID != id || intent.State != updatePrepared || intent.AckedGeneration != 0 {
		return nil, errUpdatePartialState
	}
	return &intent, nil
}

func sameUpdateMetadata(record, intent *updateRecord) bool {
	metadata := *record
	metadata.State, metadata.AckedGeneration = intent.State, intent.AckedGeneration
	return metadata == *intent
}

// updateStep requires the root lock. The started journal line is durable before
// the derived record or action; a crash therefore always repeats observations.
func (c *Cell) updateStep(record *updateRecord, state updateState, action func() error) error {
	if !state.valid() || record == nil {
		return errUpdatePartialState
	}
	intent, err := c.readUpdateIntent(record.OperationID)
	if err != nil || c.authenticateUpdateRecord(record) != nil || !sameUpdateMetadata(record, intent) {
		return errUpdatePartialState
	}
	if updateStateIndex(state) < updateStateIndex(updateAcknowledged) && record.AckedGeneration != 0 ||
		updateStateIndex(state) >= updateStateIndex(updateAcknowledged) && record.AckedGeneration != record.MaintenanceGeneration {
		return errUpdatePartialState
	}
	progress, err := c.readUpdateJournal()
	if err != nil {
		return err
	}
	if progress.pending == nil || progress.pending.Operation != "update" || progress.updateID != record.OperationID ||
		!validUpdateJournalTransition(progress.updateState, state) {
		return errUpdatePartialState
	}
	if err := c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: string(state), State: "started"}); err != nil {
		return err
	}
	record.State = state
	if err := c.writeUpdateRecord(record); err != nil {
		return err
	}
	if action != nil {
		if err := action(); err != nil {
			return err
		}
	}
	return c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: string(state), State: "done"})
}

// readDerivedUpdateRecord distinguishes absence from existing bytes. An existing
// incomplete record cannot prove ownership and is never overwritten.
func (c *Cell) readDerivedUpdateRecord(id string) (*updateRecord, bool, error) {
	if !validUpdateOperationID(id) || c.validateRoot(false) != nil {
		return nil, false, errUpdatePartialState
	}
	if err := servicePrivateRecord(c.updateRecordPath(id)); errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	} else if err != nil {
		return nil, false, errUpdatePartialState
	}
	f, err := openPrivateFile(c.updateRecordPath(id), os.O_RDONLY, 0)
	if err != nil {
		return nil, false, errUpdatePartialState
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, false, errUpdatePartialState
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record updateRecord
	if err := decoder.Decode(&record); err != nil || decoder.Decode(new(any)) != io.EOF || validateUpdateRecord(&record) != nil || record.OperationID != id {
		return nil, false, errUpdatePartialState
	}
	return &record, false, nil
}

// reconcileUpdateRecord requires the root lock and re-reads the durable journal
// itself. A record can lag the journal, but can never authorize progress.
func (c *Cell) reconcileUpdateRecord() (*updateRecord, error) {
	progress, err := c.readUpdateJournal()
	if err != nil || progress.tornTail || !validUpdateOperationID(progress.updateID) {
		return nil, errUpdatePartialState
	}
	storedIntent, err := c.readUpdateIntent(progress.updateID)
	if err != nil {
		return nil, errUpdatePartialState
	}
	intent := *storedIntent
	record, missing, err := c.readDerivedUpdateRecord(progress.updateID)
	if err != nil {
		return nil, err
	}
	if !missing {
		// A reopened maintenance request may lag a previously durable ACK in
		// the derived record. Only states witnessed in this operation's journal
		// may be reconciled; an invented future state remains corruption.
		if !sameUpdateMetadata(record, &intent) || updateStateIndex(record.State) > updateStateIndex(progress.updateMaxState) {
			return nil, errUpdatePartialState
		}
	}
	if progress.updateState == "" {
		intent.State = ""
		return &intent, nil
	}
	intent.State = progress.updateState
	if updateStateIndex(intent.State) >= updateStateIndex(updateAcknowledged) {
		intent.AckedGeneration = intent.MaintenanceGeneration
	}
	if missing || *record != intent {
		if err := c.writeUpdateRecord(&intent); err != nil {
			return nil, err
		}
	}
	return &intent, nil
}
