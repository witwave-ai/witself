package transcriptcapture

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const pendingAcknowledgementSuffix = ".ack"

// Acknowledgement outlives the plaintext snapshot until the event is removed.
// It carries only identity, so a failed event unlink cannot lose provenance or
// require keeping a second copy of the acknowledged event's content.
type pendingAcknowledgement struct {
	EventID              string `json:"event_id"`
	Runtime              string `json:"runtime"`
	SessionID            string `json:"session_id"`
	TranscriptExternalID string `json:"transcript_external_id"`
}

func pendingAcknowledgementPath(path string) string {
	return submissionPath(path) + pendingAcknowledgementSuffix
}

func pendingAcknowledgementForEvent(path string) (pendingAcknowledgement, error) {
	file, _, err := openTrustedRegularFile(path)
	if err != nil {
		return pendingAcknowledgement{}, err
	}
	defer func() { _ = file.Close() }()
	var event Event
	if err := json.NewDecoder(file).Decode(&event); err != nil {
		return pendingAcknowledgement{}, err
	}
	if err := validatePendingRewritePath(path, event); err != nil {
		return pendingAcknowledgement{}, err
	}
	return pendingAcknowledgement{
		EventID: event.ID, Runtime: event.Runtime, SessionID: event.SessionID,
		TranscriptExternalID: event.TranscriptExternalID(),
	}, nil
}

func removePendingWithAcknowledgement(path string) error {
	acknowledgement, err := pendingAcknowledgementForEvent(path)
	if err != nil {
		return err
	}
	release, err := acquireSessionStateLock(acknowledgement.Runtime, acknowledgement.SessionID)
	if err != nil {
		return err
	}
	defer release()
	current, err := pendingAcknowledgementForEvent(path)
	if errors.Is(err, os.ErrNotExist) {
		// A concurrent preview may already have completed this acknowledgement.
		return nil
	}
	if err != nil {
		return err
	}
	if current != acknowledgement {
		return errors.New("capture event identity changed before acknowledgement")
	}
	if err := writeJSONAtomic(pendingAcknowledgementPath(path), acknowledgement); err != nil {
		return err
	}
	return removeAcknowledgedPendingFiles(path)
}

func removeAcknowledgedPendingFiles(path string) error {
	// The plaintext snapshot must disappear before its discoverable event.
	// Retain acknowledgement through either failed unlink for the next sweep.
	if err := os.Remove(submissionPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(pendingAcknowledgementPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func readPendingAcknowledgement(path, runtime string) (pendingAcknowledgement, error) {
	file, _, err := openTrustedRegularFile(pendingAcknowledgementPath(path))
	if err != nil {
		return pendingAcknowledgement{}, err
	}
	defer func() { _ = file.Close() }()
	var acknowledgement pendingAcknowledgement
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&acknowledgement) != nil || decoder.Decode(new(any)) != io.EOF ||
		acknowledgement.Runtime != runtime || acknowledgement.EventID == "" ||
		acknowledgement.SessionID == "" || acknowledgement.TranscriptExternalID == "" ||
		strings.ContainsAny(acknowledgement.EventID, `/\`) ||
		!strings.HasSuffix(filepath.Base(path), "-"+acknowledgement.EventID+".json") {
		return pendingAcknowledgement{}, errors.New("capture acknowledgement identity is invalid")
	}
	return acknowledgement, nil
}

func sweepPendingAcknowledgement(path, runtime string) error {
	acknowledgement, err := readPendingAcknowledgement(path, runtime)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Previews need not own the flush lock. Serialize with acknowledgement and
	// hook publication, then recheck evidence so a stale sweep cannot remove a
	// same-path event republished after the acknowledged event was removed.
	release, err := acquireSessionStateLock(acknowledgement.Runtime, acknowledgement.SessionID)
	if err != nil {
		return err
	}
	defer release()
	current, err := readPendingAcknowledgement(path, runtime)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if current != acknowledgement {
		return errors.New("capture acknowledgement changed before cleanup")
	}
	event, err := pendingAcknowledgementForEvent(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && event != acknowledgement {
		return errors.New("capture acknowledgement does not match pending event")
	}
	return removeAcknowledgedPendingFiles(path)
}
