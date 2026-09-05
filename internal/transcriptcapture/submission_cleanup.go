package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SweepOrphanSubmissions finishes acknowledged event cleanup and retries snapshot
// cleanup interrupted after an older uploader removed the outbox event. Flush and
// release apply callers hold the runtime flush lock. Previews can also sweep:
// acknowledged cleanup uses session locking and rechecks its evidence, while
// other snapshots are removed only when their event is absent. Queued publication
// records are preserved. Callers log a returned error once without making it
// fatal. All entries are considered even after a failure and retried later.
func SweepOrphanSubmissions(runtime string) error {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return err
	}
	outbox, err := outboxDir(runtime)
	if err != nil {
		return err
	}
	dir := filepath.Join(outbox, ".submissions")
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || !trustedPathIdentity(dir, info) {
		return errors.New("capture submissions directory is not a trusted directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var firstErr error
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json"+pendingAcknowledgementSuffix) {
			path := filepath.Join(outbox, strings.TrimSuffix(entry.Name(), pendingAcknowledgementSuffix))
			if err := sweepPendingAcknowledgement(path, runtime); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("finish capture acknowledgement: %w", err)
			}
			continue
		}
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(outbox, entry.Name())); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			if firstErr == nil {
				firstErr = fmt.Errorf("inspect orphan capture event: %w", err)
			}
			continue
		}
		path := filepath.Join(dir, entry.Name())
		// Queued provenance is value-free and is published before its event.
		// Do not race that publication by treating it as a plaintext snapshot.
		// Invalid records still need cleanup: they may retain event content.
		if orphanSubmissionQueued(path) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = fmt.Errorf("remove orphan capture submission: %w", err)
		}
	}
	return firstErr
}

func orphanSubmissionQueued(path string) bool {
	file, _, err := openTrustedRegularFile(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(file)
	if err != nil {
		return false
	}
	var submission pendingSubmission
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(&submission) == nil && decoder.Decode(new(any)) == io.EOF &&
		submission.State == "queued" && submission.Event == nil && len(submission.Entries) == 0
}
