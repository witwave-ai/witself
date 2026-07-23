package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	runtimepkg "runtime"
)

const integrationTransactionJournalReadLimit = 16 * 1024 * 1024

// integrationTransactionJournalFile is an exact optimistic-ownership token for
// one durable provider transaction journal. Callers validate the decoded
// journal separately, then pass this snapshot to the exact removal helper.
type integrationTransactionJournalFile struct {
	path        string
	displayName string
	raw         []byte
	info        fs.FileInfo
}

func loadIntegrationTransactionJournalFile(path, displayName string) (integrationTransactionJournalFile, error) {
	snapshot := integrationTransactionJournalFile{path: path, displayName: displayName}
	linked, err := os.Lstat(path)
	if err != nil {
		return snapshot, err
	}
	if !linked.Mode().IsRegular() || linked.Mode()&os.ModeSymlink != 0 ||
		!integrationFileModeMatches(linked.Mode(), 0o600) {
		return snapshot, fmt.Errorf("%s must be a real 0600 regular file", displayName)
	}
	if linked.Size() > integrationTransactionJournalReadLimit {
		return snapshot, fmt.Errorf("%s is too large", displayName)
	}

	file, err := os.Open(path)
	if err != nil {
		return snapshot, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return snapshot, err
	}
	if !sameManagedInstructionsFileIdentity(opened, linked) {
		return snapshot, fmt.Errorf("%s identity changed while it was opened", displayName)
	}
	raw, err := io.ReadAll(io.LimitReader(file, integrationTransactionJournalReadLimit+1))
	if err != nil {
		return snapshot, err
	}
	if len(raw) > integrationTransactionJournalReadLimit {
		return snapshot, fmt.Errorf("%s is too large", displayName)
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return snapshot, err
	}
	linkedAfter, err := os.Lstat(path)
	if err != nil {
		return snapshot, err
	}
	if linkedAfter.Mode()&os.ModeSymlink != 0 ||
		!sameManagedInstructionsFileIdentity(opened, openedAfter) ||
		!sameManagedInstructionsFileIdentity(openedAfter, linkedAfter) ||
		int64(len(raw)) != openedAfter.Size() {
		return snapshot, fmt.Errorf("%s identity changed while it was read", displayName)
	}
	snapshot.raw = raw
	snapshot.info = linkedAfter
	return snapshot, nil
}

func removeIntegrationTransactionJournalFile(expected integrationTransactionJournalFile) error {
	if expected.path == "" || expected.info == nil {
		return errors.New("transaction journal removal is missing its exact file snapshot")
	}
	verifyCurrent := func() error {
		current, err := loadIntegrationTransactionJournalFile(expected.path, expected.displayName)
		if err != nil {
			return err
		}
		if !sameManagedInstructionsFileIdentity(current.info, expected.info) ||
			!bytes.Equal(current.raw, expected.raw) {
			return fmt.Errorf("%s changed; refusing to clear it", expected.displayName)
		}
		return nil
	}
	return removeManagedInstructionsFile(
		expected.path,
		filepath.Base(expected.path),
		"",
		verifyCurrent,
		expected.raw,
		expected.info,
	)
}

func syncIntegrationTransactionFileState(path, displayName string) error {
	linked, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return syncIntegrationTransactionNearestDirectory(filepath.Dir(path))
	}
	if err != nil {
		return err
	}
	if !linked.Mode().IsRegular() || linked.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real regular file before transaction commit", displayName)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err == nil && !sameManagedInstructionsFileIdentity(opened, linked) {
		err = fmt.Errorf("%s identity changed before transaction commit", displayName)
	}
	if err == nil {
		err = file.Sync()
	}
	openedAfter, statErr := file.Stat()
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if statErr != nil {
		return statErr
	}
	if closeErr != nil {
		return closeErr
	}
	linkedAfter, err := os.Lstat(path)
	if err != nil || linkedAfter.Mode()&os.ModeSymlink != 0 ||
		!sameManagedInstructionsFileIdentity(opened, openedAfter) ||
		!sameManagedInstructionsFileIdentity(openedAfter, linkedAfter) {
		if err == nil {
			err = fmt.Errorf("%s identity changed while transaction state was committed", displayName)
		}
		return err
	}
	return syncIntegrationTransactionNearestDirectory(filepath.Dir(path))
}

func syncIntegrationTransactionNearestDirectory(path string) error {
	if runtimepkg.GOOS == "windows" {
		return nil
	}
	for {
		linked, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			parent := filepath.Dir(path)
			if parent == path {
				return err
			}
			path = parent
			continue
		}
		if err != nil {
			return err
		}
		if !linked.IsDir() || linked.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("transaction state directory %s must be a real directory", path)
		}
		directory, err := os.Open(path)
		if err != nil {
			return err
		}
		opened, statErr := directory.Stat()
		if statErr == nil && !os.SameFile(opened, linked) {
			statErr = errors.New("transaction state directory identity changed while it was opened")
		}
		if statErr == nil {
			statErr = directory.Sync()
		}
		closeErr := directory.Close()
		if statErr != nil {
			return statErr
		}
		if closeErr != nil {
			return closeErr
		}
		linkedAfter, err := os.Lstat(path)
		if err != nil || linkedAfter.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, linkedAfter) {
			if err == nil {
				err = errors.New("transaction state directory identity changed while it was committed")
			}
			return err
		}
		return nil
	}
}
