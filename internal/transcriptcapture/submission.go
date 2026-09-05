package transcriptcapture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// New capture writes value-free queued provenance before publishing an event.
// Before its first append attempt this becomes an immutable Event+Entries
// snapshot: even a failed request may have committed server entries. Keep both
// states outside the legacy outbox glob. An absent record is unknown history,
// not evidence that an older uploader never submitted the event. Queued
// provenance is also uncertain: pre-upgrade uploaders ignore these records.
type pendingSubmission struct {
	State                 string            `json:"state"`
	EventID               string            `json:"event_id,omitempty"`
	Runtime               string            `json:"runtime,omitempty"`
	TranscriptExternalID  string            `json:"transcript_external_id,omitempty"`
	Event                 *Event            `json:"event,omitempty"`
	Entries               []Entry           `json:"entries,omitempty"`
	ReleaseID             string            `json:"release_id,omitempty"`
	SupersededExternalIDs []string          `json:"superseded_external_ids,omitempty"`
	ReplyEventIDs         map[string]string `json:"reply_event_ids,omitempty"`
}

var errLegacySubmissionUnknown = errors.New("legacy capture submission history is unknown; use a compatible transcript flush to reconcile upload-ready events before release")

func submissionPath(path string) string {
	return filepath.Join(filepath.Dir(path), ".submissions", filepath.Base(path))
}

// Entries returns the immutable attempted projection, if one exists.
func (pending PendingEvent) Entries() []Entry {
	if pending.submission != nil {
		return pending.submission.Entries
	}
	return pending.Event.Entries()
}

// The replacement namespace lives in the event as well as the sidecar so it
// survives acknowledgement's snapshot-first unlink and remains retryable.
func (event Event) submissionProjection() Event {
	if event.SubmissionReleaseID == "" {
		return event
	}
	event.ID = releasedSubmissionEventID(event.ID, event.SubmissionReleaseID)
	if replacement := event.SubmissionReplyEventIDs[event.ReplyToEventID]; replacement != "" {
		event.ReplyToEventID = replacement
	}
	event.RecoveredMessages = append([]RecoveredMessage(nil), event.RecoveredMessages...)
	for i := range event.RecoveredMessages {
		message := &event.RecoveredMessages[i]
		message.ID = releasedSubmissionEventID(message.ID, event.SubmissionReleaseID)
		if replacement := event.SubmissionReplyEventIDs[message.ReplyToEventID]; replacement != "" {
			message.ReplyToEventID = replacement
		}
	}
	event.SubmissionReleaseID, event.SubmissionReplyEventIDs = "", nil
	return event
}

func releasedSubmissionEventID(eventID, releaseID string) string {
	sum := sha256.Sum256([]byte(eventID + "\x00" + releaseID))
	return "evt_release_" + hex.EncodeToString(sum[:])
}

// Persist the value-free replacement identity before changing the event. A
// crash at either write keeps the release hold and the same retry namespace.
// An old uploader may have committed every original chunk despite queued state.
func prepareOperatorReleaseSubmission(item PendingEvent, pending []PendingEvent, matches func(Event) bool, releaseID string) (Event, error) {
	// Multiple turns may revisit a turn-less event through an older pending
	// slice. The first durable replacement namespace always wins.
	current, err := loadPendingSubmission(item)
	if err != nil {
		return item.Event, err
	}
	item.queuedSubmission = current.queuedSubmission
	if item.queuedSubmission != nil && item.queuedSubmission.ReleaseID != "" {
		item.Event.SubmissionReleaseID = item.queuedSubmission.ReleaseID
		item.Event.SubmissionReplyEventIDs = item.queuedSubmission.ReplyEventIDs
		return item.Event, nil
	}
	if !item.tracked || releaseID == "" {
		return item.Event, errors.New("held: prior submission unknown; release replacement identity is unavailable")
	}
	// Each event only needs identities for its own reply references. Copying
	// the complete turn map into every sidecar and event grows quadratically.
	replyTargets := make(map[string]bool)
	if item.Event.ReplyToEventID != "" {
		replyTargets[item.Event.ReplyToEventID] = true
	}
	for _, message := range item.Event.RecoveredMessages {
		if message.ReplyToEventID != "" {
			replyTargets[message.ReplyToEventID] = true
		}
	}
	record := pendingSubmission{
		State: "queued", EventID: item.Event.ID, Runtime: item.Event.Runtime,
		TranscriptExternalID: item.Event.TranscriptExternalID(), ReleaseID: releaseID,
		ReplyEventIDs: make(map[string]string, len(replyTargets)),
	}
	for _, entry := range item.Event.Entries() {
		record.SupersededExternalIDs = append(record.SupersededExternalIDs, entry.ExternalID)
	}
	for _, other := range pending {
		if len(replyTargets) == 0 {
			break
		}
		if !matches(other.Event) || other.submission != nil {
			continue
		}
		otherReleaseID := releaseID
		if other.queuedSubmission != nil && other.queuedSubmission.ReleaseID != "" {
			otherReleaseID = other.queuedSubmission.ReleaseID
		} else if operatorReleasedEventSafe(other.Event) {
			continue
		}
		if replyTargets[other.Event.ID] {
			record.ReplyEventIDs[other.Event.ID] = releasedSubmissionEventID(other.Event.ID, otherReleaseID)
		}
		for _, message := range other.Event.RecoveredMessages {
			if replyTargets[message.ID] {
				record.ReplyEventIDs[message.ID] = releasedSubmissionEventID(message.ID, otherReleaseID)
			}
		}
	}
	if err := writeJSONAtomic(submissionPath(item.Path), record); err != nil {
		return item.Event, err
	}
	item.Event.SubmissionReleaseID = record.ReleaseID
	item.Event.SubmissionReplyEventIDs = record.ReplyEventIDs
	return item.Event, nil
}

func loadPendingSubmission(pending PendingEvent) (PendingEvent, error) {
	pending.tracked, pending.submission, pending.queuedSubmission = false, nil, nil
	path := submissionPath(pending.Path)
	file, _, err := openTrustedRegularFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pending, nil
	}
	if err != nil {
		return pending, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(file)
	if err != nil {
		return pending, err
	}
	var submission pendingSubmission
	if err := json.Unmarshal(raw, &submission); err != nil {
		return pending, fmt.Errorf("parse capture submission: %w", err)
	}
	if submission.State == "queued" {
		if submission.Event != nil || len(submission.Entries) != 0 || submission.EventID != pending.Event.ID ||
			submission.Runtime != pending.Event.Runtime || submission.TranscriptExternalID != pending.Event.TranscriptExternalID() {
			return pending, errors.New("capture queued provenance does not match pending event")
		}
		if (submission.ReleaseID == "") != (len(submission.SupersededExternalIDs) == 0) ||
			(submission.ReleaseID == "" && len(submission.ReplyEventIDs) != 0) {
			return pending, errors.New("capture queued release provenance is incomplete")
		}
		if pending.Event.SubmissionReleaseID != "" && pending.Event.SubmissionReleaseID != submission.ReleaseID {
			return pending, errors.New("capture event replacement identity differs from its queued provenance")
		}
		pending.tracked = true
		pending.queuedSubmission = &submission
		return pending, nil
	}
	if submission.State != "attempted" || submission.Event == nil ||
		submission.Event.ID != pending.Event.ID || submission.Event.Runtime != pending.Event.Runtime ||
		submission.Event.TranscriptExternalID() != pending.Event.TranscriptExternalID() || len(submission.Entries) == 0 {
		return pending, errors.New("capture submission identity does not match pending event")
	}
	pending.Event = *submission.Event
	pending.tracked = true
	pending.submission = &submission
	return pending, nil
}

// registerPendingQueued never retroactively labels an existing legacy event
// unsubmitted, and never replaces an attempted envelope on a fence retry.
func registerPendingQueued(path string, event Event) error {
	pending, err := loadPendingSubmission(PendingEvent{Path: path, Event: event})
	if err != nil || pending.tracked {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeJSONAtomic(submissionPath(path), pendingSubmission{
		State: "queued", EventID: event.ID, Runtime: event.Runtime, TranscriptExternalID: event.TranscriptExternalID(),
	})
}

func operatorReleaseRewriteAllowed(pending PendingEvent) error {
	if pending.tracked || pending.submission != nil {
		return nil
	}
	return errLegacySubmissionUnknown
}

// MarkPendingSubmitted freezes each event before any append request can be
// sent. The caller holds the flush lock; per-session locking excludes a hook
// rewrite between checking the prepared snapshot and persisting its attempt.
func MarkPendingSubmitted(pending PendingEvent) error {
	release, err := acquireSessionStateLock(pending.Event.Runtime, pending.Event.SessionID)
	if err != nil {
		return err
	}
	defer release()
	if err := validatePendingRewritePath(pending.Path, pending.Event); err != nil {
		return err
	}
	existing, err := loadPendingSubmission(pending)
	if err != nil {
		return err
	}
	want, err := json.Marshal(pending.Event)
	if err != nil {
		return err
	}
	if existing.submission != nil {
		gotEntries, err := json.Marshal(existing.Entries())
		wantEntries, wantErr := json.Marshal(pending.Entries())
		if err != nil || wantErr != nil || !bytes.Equal(gotEntries, wantEntries) {
			return errors.New("prepared capture entries differ from their immutable submission")
		}
		got, err := json.Marshal(existing.Event)
		if err != nil || !bytes.Equal(want, got) {
			return errors.New("prepared capture event differs from its immutable submission")
		}
		return nil
	}
	raw, err := os.ReadFile(pending.Path)
	if err != nil {
		return err
	}
	var current Event
	if err := json.Unmarshal(raw, &current); err != nil {
		return err
	}
	got, err := json.Marshal(current)
	if err != nil || !bytes.Equal(want, got) {
		return errors.New("capture event changed before submission; retry flush")
	}
	wantEntries, err := json.Marshal(pending.Entries())
	if err != nil {
		return err
	}
	gotEntries, err := json.Marshal(existing.Entries())
	if err != nil || !bytes.Equal(wantEntries, gotEntries) {
		return errors.New("capture submission identity changed before submission; retry flush")
	}
	record := pendingSubmission{State: "attempted", Event: &pending.Event, Entries: pending.Entries()}
	if existing.queuedSubmission != nil {
		record.ReleaseID = existing.queuedSubmission.ReleaseID
		record.SupersededExternalIDs = existing.queuedSubmission.SupersededExternalIDs
		record.ReplyEventIDs = existing.queuedSubmission.ReplyEventIDs
	}
	return writeJSONAtomic(submissionPath(pending.Path), record)
}
