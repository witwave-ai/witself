package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/token"
)

const (
	accountProvisionJournalSchema   = "witself.account-provision-journal.v1"
	maxAccountProvisionJournalBytes = 16 * 1024
	maxAccountProvisionConfigBytes  = 4 * 1024 * 1024
	accountProvisionJournalLockFile = ".lock"
)

var (
	// ErrAccountProvisionJournalUnavailable means no resumable entry exists.
	ErrAccountProvisionJournalUnavailable = errors.New("account provision journal entry is unavailable")
	// ErrAccountProvisionJournalConflict means another request owns the entry.
	ErrAccountProvisionJournalConflict = errors.New("account provision journal request conflicts with pending signup")
	// ErrAccountProvisionJournalUnsafe means filesystem safety checks failed.
	ErrAccountProvisionJournalUnsafe = errors.New("account provision journal storage is unsafe")
	// ErrAccountProvisionJournalInvalid means the durable entry is malformed.
	ErrAccountProvisionJournalInvalid = errors.New("account provision journal entry is invalid")
	// ErrAccountProvisionJournalStorage means durable journal I/O failed.
	ErrAccountProvisionJournalStorage = errors.New("account provision journal storage failed")

	accountProvisionFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	accountProvisionIDPattern          = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	accountProvisionAccountIDPattern   = regexp.MustCompile(`^acc_[a-z2-7]{16}$`)
)

// AccountProvisionJournal is the private, crash-safe handoff for one local
// account signup. RequestFingerprint binds the endpoint, local name, and
// normalized signup request without retaining their plaintext. AccountID and
// OperatorToken are added only after the one-shot bootstrap exchange succeeds,
// so a crash cannot strand the consumed credential.
type AccountProvisionJournal struct {
	SchemaVersion      string `json:"schema_version"`
	RequestFingerprint string `json:"request_fingerprint"`
	ProvisionID        string `json:"provision_id"`
	AccountID          string `json:"account_id,omitempty"`
	OperatorToken      string `json:"operator_token,omitempty"`
}

// AccountProvisionJournalPath returns the canonical private journal path for a
// local account name.
func AccountProvisionJournalPath(localName string) (string, error) {
	_, path, err := accountProvisionJournalLocation(localName)
	return path, err
}

// BeginAccountProvisionJournal creates a durable provision id before the first
// remote POST. Concurrent matching callers serialize on a stable owner-only
// lock and reuse the winner's id. A different request for the same local name
// fails closed.
func BeginAccountProvisionJournal(
	localName, requestFingerprint string,
) (AccountProvisionJournal, bool, error) {
	if !accountProvisionFingerprintPattern.MatchString(requestFingerprint) {
		return AccountProvisionJournal{}, false, ErrAccountProvisionJournalInvalid
	}
	home, path, err := accountProvisionJournalLocation(localName)
	if err != nil {
		return AccountProvisionJournal{}, false, err
	}
	directory := filepath.Dir(path)
	if err := ensureAccountProvisionJournalDirectories(home, directory); err != nil {
		return AccountProvisionJournal{}, false, err
	}
	lock, err := acquireAccountProvisionJournalLock(home, directory)
	if err != nil {
		return AccountProvisionJournal{}, false, err
	}
	defer lock.release()

	current, err := readAccountProvisionJournalLocked(home, path)
	if err == nil {
		if current.RequestFingerprint != requestFingerprint {
			clearAccountProvisionJournal(&current)
			return AccountProvisionJournal{}, false, ErrAccountProvisionJournalConflict
		}
		return current, false, nil
	}
	if !errors.Is(err, ErrAccountProvisionJournalUnavailable) {
		return AccountProvisionJournal{}, false, err
	}

	provisionID, err := id.New("prv")
	if err != nil {
		return AccountProvisionJournal{}, false, ErrAccountProvisionJournalStorage
	}
	record := AccountProvisionJournal{
		SchemaVersion:      accountProvisionJournalSchema,
		RequestFingerprint: requestFingerprint,
		ProvisionID:        provisionID,
	}
	if err := publishAccountProvisionJournal(home, path, record, nil); err != nil {
		return AccountProvisionJournal{}, false, err
	}
	return record, true, nil
}

// ReadAccountProvisionJournal reads and validates one private pending signup.
func ReadAccountProvisionJournal(localName string) (AccountProvisionJournal, error) {
	home, path, err := accountProvisionJournalLocation(localName)
	if err != nil {
		return AccountProvisionJournal{}, err
	}
	directory := filepath.Dir(path)
	if err := validateAccountProvisionJournalDirectories(home, directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AccountProvisionJournal{}, ErrAccountProvisionJournalUnavailable
		}
		return AccountProvisionJournal{}, err
	}
	lock, err := acquireAccountProvisionJournalLock(home, directory)
	if err != nil {
		return AccountProvisionJournal{}, err
	}
	defer lock.release()
	return readAccountProvisionJournalLocked(home, path)
}

// SaveAccountProvisionCredential durably adds the consumed bootstrap exchange
// result. It never replaces a different account or operator token.
func SaveAccountProvisionCredential(
	localName, requestFingerprint, provisionID, accountID, operatorToken string,
) error {
	if !accountProvisionFingerprintPattern.MatchString(requestFingerprint) ||
		!accountProvisionIDPattern.MatchString(provisionID) ||
		!accountProvisionAccountIDPattern.MatchString(accountID) {
		return ErrAccountProvisionJournalInvalid
	}
	kind, _, err := token.Parse(operatorToken)
	if err != nil || kind != token.KindOperator || strings.TrimSpace(operatorToken) != operatorToken {
		return ErrAccountProvisionJournalInvalid
	}
	home, path, err := accountProvisionJournalLocation(localName)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := validateAccountProvisionJournalDirectories(home, directory); err != nil {
		return classifyAccountProvisionJournalDirectoryError(err)
	}
	lock, err := acquireAccountProvisionJournalLock(home, directory)
	if err != nil {
		return err
	}
	defer lock.release()

	current, err := readAccountProvisionJournalLocked(home, path)
	if err != nil {
		return err
	}
	defer clearAccountProvisionJournal(&current)
	if current.RequestFingerprint != requestFingerprint ||
		current.ProvisionID != provisionID {
		return ErrAccountProvisionJournalConflict
	}
	if current.AccountID != "" || current.OperatorToken != "" {
		if current.AccountID == accountID && current.OperatorToken == operatorToken {
			return nil
		}
		return ErrAccountProvisionJournalConflict
	}
	current.AccountID = accountID
	current.OperatorToken = operatorToken
	expected := current
	expected.AccountID = ""
	expected.OperatorToken = ""
	return publishAccountProvisionJournal(home, path, current, &expected)
}

// DeleteAccountProvisionJournal removes only the exact completed credential
// handoff. Callers invoke it after local.Save has durably stored the matching
// account and operator token.
func DeleteAccountProvisionJournal(
	localName, requestFingerprint, provisionID, accountID, operatorToken string,
) error {
	home, path, err := accountProvisionJournalLocation(localName)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := validateAccountProvisionJournalDirectories(home, directory); err != nil {
		return classifyAccountProvisionJournalDirectoryError(err)
	}
	lock, err := acquireAccountProvisionJournalLock(home, directory)
	if err != nil {
		return err
	}
	defer lock.release()

	current, err := readAccountProvisionJournalLocked(home, path)
	if err != nil {
		return err
	}
	defer clearAccountProvisionJournal(&current)
	if current.RequestFingerprint != requestFingerprint ||
		current.ProvisionID != provisionID ||
		current.AccountID != accountID ||
		current.OperatorToken != operatorToken {
		return ErrAccountProvisionJournalConflict
	}
	fenced, err := readAccountProvisionJournalLocked(home, path)
	if err != nil {
		return err
	}
	defer clearAccountProvisionJournal(&fenced)
	if !equalAccountProvisionJournal(current, fenced) {
		return ErrAccountProvisionJournalConflict
	}
	info, err := os.Lstat(path)
	if err != nil || !privateRegularAccountProvisionJournalFile(info) {
		return ErrAccountProvisionJournalUnsafe
	}
	if err := os.Remove(path); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	if err := syncAccountProvisionJournalDirectory(directory); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	return nil
}

// SaveProvisionedAccountDurable commits the journaled credential to the
// ordinary local account layout. The token is published first with
// no-overwrite semantics and the complete config is then atomically replaced;
// both files and containing directories are synced. An exact partial or
// completed prior attempt is resumed, while any different binding or token
// fails closed.
func SaveProvisionedAccountDurable(
	localName string,
	account Account,
	operatorToken string,
) error {
	if !namePattern.MatchString(localName) ||
		!accountProvisionAccountIDPattern.MatchString(account.ID) {
		return ErrAccountProvisionJournalInvalid
	}
	kind, _, err := token.Parse(operatorToken)
	if err != nil || kind != token.KindOperator ||
		strings.TrimSpace(operatorToken) != operatorToken {
		return ErrAccountProvisionJournalInvalid
	}
	home, journalPath, err := accountProvisionJournalLocation(localName)
	if err != nil {
		return err
	}
	journalDirectory := filepath.Dir(journalPath)
	if err := validateAccountProvisionJournalDirectories(
		home, journalDirectory,
	); err != nil {
		return classifyAccountProvisionJournalDirectoryError(err)
	}
	lock, err := acquireAccountProvisionJournalLock(home, journalDirectory)
	if err != nil {
		return err
	}
	defer lock.release()

	// Require the credential handoff itself to be present and exact before
	// writing either ordinary local file.
	journal, err := readAccountProvisionJournalLocked(home, journalPath)
	if err != nil {
		return err
	}
	defer clearAccountProvisionJournal(&journal)
	if journal.AccountID != account.ID || journal.OperatorToken != operatorToken {
		return ErrAccountProvisionJournalConflict
	}

	config, configExists, err := readProvisionConfig(home)
	if err != nil {
		return err
	}
	existingAccount, bindingExists := config.Accounts[localName]
	if bindingExists && (existingAccount.ID != account.ID ||
		(existingAccount.Email != "" && account.Email != "" &&
			existingAccount.Email != account.Email)) {
		return ErrNameTaken
	}

	tokenPath, err := TokenPath(localName)
	if err != nil {
		return ErrAccountProvisionJournalStorage
	}
	if legacyPath, legacyErr := legacyTokenPath(localName); legacyErr == nil {
		if _, legacyErr = os.Lstat(legacyPath); legacyErr == nil {
			return ErrNameTaken
		} else if !errors.Is(legacyErr, os.ErrNotExist) {
			return ErrAccountProvisionJournalStorage
		}
	}
	tokenRaw, tokenExists, err := readPrivateProvisionFile(
		tokenPath, maxAccountProvisionJournalBytes,
	)
	if err != nil {
		return err
	}
	if tokenExists {
		defer clear(tokenRaw)
		if strings.TrimSpace(string(tokenRaw)) != operatorToken {
			return ErrNameTaken
		}
	} else {
		if err := ensurePrivateProvisionDirectory(home, filepath.Dir(tokenPath)); err != nil {
			return err
		}
		raw := []byte(operatorToken + "\n")
		if err := publishPrivateProvisionFileNoReplace(tokenPath, raw); err != nil {
			clear(raw)
			return err
		}
		clear(raw)
	}
	if err := syncPrivateProvisionFile(tokenPath); err != nil {
		return err
	}
	if err := syncAccountProvisionJournalDirectory(filepath.Dir(tokenPath)); err != nil {
		return ErrAccountProvisionJournalStorage
	}

	if bindingExists && existingAccount.ID == account.ID {
		// Exact config + token is already the durable result of a prior attempt.
		configPath := filepath.Join(home, "config.json")
		if err := syncPrivateProvisionFile(configPath); err != nil {
			return err
		}
		if err := syncAccountProvisionJournalDirectory(home); err != nil {
			return ErrAccountProvisionJournalStorage
		}
		return nil
	}
	if !configExists && config.Accounts == nil {
		config.Accounts = map[string]Account{}
	}
	if config.Accounts == nil {
		config.Accounts = map[string]Account{}
	}
	config.Accounts[localName] = account
	if err := replaceProvisionConfig(home, config); err != nil {
		return err
	}
	return nil
}

type accountProvisionJournalLock struct {
	file *os.File
}

func acquireAccountProvisionJournalLock(
	home, directory string,
) (*accountProvisionJournalLock, error) {
	if err := validateAccountProvisionJournalDirectories(home, directory); err != nil {
		return nil, classifyAccountProvisionJournalDirectoryError(err)
	}
	path := filepath.Join(directory, accountProvisionJournalLockFile)
	file, created, err := openLocalLockFileNoFollow(path, true)
	if err != nil {
		if errors.Is(err, errLocalLockFileStorage) {
			return nil, ErrAccountProvisionJournalStorage
		}
		return nil, ErrAccountProvisionJournalUnsafe
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = file.Close()
		}
	}()
	opened, statErr := file.Stat()
	linked, linkErr := os.Lstat(path)
	if statErr != nil || linkErr != nil || !os.SameFile(opened, linked) ||
		!privateRegularAccountProvisionJournalFile(opened) ||
		!privateRegularAccountProvisionJournalFile(linked) {
		return nil, ErrAccountProvisionJournalUnsafe
	}
	if created {
		if err := file.Sync(); err != nil {
			return nil, ErrAccountProvisionJournalStorage
		}
		if err := syncAccountProvisionJournalDirectory(directory); err != nil {
			return nil, ErrAccountProvisionJournalStorage
		}
	}
	if err := lockLocalFile(file); err != nil {
		return nil, ErrAccountProvisionJournalStorage
	}
	linked, linkErr = os.Lstat(path)
	if linkErr != nil || !os.SameFile(opened, linked) ||
		!privateRegularAccountProvisionJournalFile(linked) {
		_ = unlockLocalFile(file)
		return nil, ErrAccountProvisionJournalUnsafe
	}
	if err := validateAccountProvisionJournalDirectories(home, directory); err != nil {
		_ = unlockLocalFile(file)
		return nil, classifyAccountProvisionJournalDirectoryError(err)
	}
	cleanup = false
	return &accountProvisionJournalLock{file: file}, nil
}

func (lock *accountProvisionJournalLock) release() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = unlockLocalFile(lock.file)
	_ = lock.file.Close()
	lock.file = nil
}

func readAccountProvisionJournalLocked(
	home, path string,
) (AccountProvisionJournal, error) {
	directory := filepath.Dir(path)
	directoryBefore, err := os.Lstat(directory)
	if err != nil || !privateAccountProvisionJournalDirectory(directoryBefore) {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalUnsafe
	}
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalUnavailable
	}
	if err != nil {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalStorage
	}
	if !privateRegularAccountProvisionJournalFile(before) {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalUnsafe
	}
	file, _, err := openLocalLockFileNoFollow(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalUnavailable
	}
	if err != nil {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalUnsafe
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	linked, linkErr := os.Lstat(path)
	if err != nil || linkErr != nil || !os.SameFile(before, opened) ||
		!os.SameFile(opened, linked) ||
		!privateRegularAccountProvisionJournalFile(opened) ||
		!privateRegularAccountProvisionJournalFile(linked) ||
		opened.Size() <= 0 || opened.Size() > maxAccountProvisionJournalBytes {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalUnsafe
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxAccountProvisionJournalBytes+1))
	if err != nil {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalStorage
	}
	defer clear(raw)
	if len(raw) > maxAccountProvisionJournalBytes {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalInvalid
	}
	var record AccountProvisionJournal
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil || !validAccountProvisionJournal(record) {
		clearAccountProvisionJournal(&record)
		return AccountProvisionJournal{}, ErrAccountProvisionJournalInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		clearAccountProvisionJournal(&record)
		return AccountProvisionJournal{}, ErrAccountProvisionJournalInvalid
	}
	finalFile, fileErr := file.Stat()
	finalPath, pathErr := os.Lstat(path)
	finalDirectory, directoryErr := os.Lstat(directory)
	if fileErr != nil || pathErr != nil || directoryErr != nil ||
		!os.SameFile(opened, finalFile) || !os.SameFile(finalFile, finalPath) ||
		!os.SameFile(directoryBefore, finalDirectory) ||
		!privateRegularAccountProvisionJournalFile(finalFile) ||
		!privateRegularAccountProvisionJournalFile(finalPath) ||
		!privateAccountProvisionJournalDirectory(finalDirectory) {
		clearAccountProvisionJournal(&record)
		return AccountProvisionJournal{}, ErrAccountProvisionJournalUnsafe
	}
	if err := validateAccountProvisionJournalDirectories(home, directory); err != nil {
		clearAccountProvisionJournal(&record)
		return AccountProvisionJournal{}, classifyAccountProvisionJournalDirectoryError(err)
	}
	return record, nil
}

func publishAccountProvisionJournal(
	home, path string,
	record AccountProvisionJournal,
	expected *AccountProvisionJournal,
) error {
	if !validAccountProvisionJournal(record) {
		return ErrAccountProvisionJournalInvalid
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > maxAccountProvisionJournalBytes {
		clear(raw)
		return ErrAccountProvisionJournalInvalid
	}
	raw = append(raw, '\n')
	defer clear(raw)

	directory := filepath.Dir(path)
	directoryBefore, err := os.Lstat(directory)
	if err != nil || !privateAccountProvisionJournalDirectory(directoryBefore) {
		return ErrAccountProvisionJournalUnsafe
	}
	if expected != nil {
		current, err := os.Lstat(path)
		if err != nil || !privateRegularAccountProvisionJournalFile(current) {
			return ErrAccountProvisionJournalUnsafe
		}
	}
	file, err := os.CreateTemp(directory, ".account-provision-*.tmp")
	if err != nil {
		return ErrAccountProvisionJournalStorage
	}
	temporaryPath := file.Name()
	temporaryExists := true
	defer func() {
		_ = file.Close()
		if temporaryExists {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	temporaryInfo, err := file.Stat()
	if err != nil || !privateRegularAccountProvisionJournalFile(temporaryInfo) {
		return ErrAccountProvisionJournalUnsafe
	}
	written, err := io.Copy(file, bytes.NewReader(raw))
	if err != nil || written != int64(len(raw)) {
		return ErrAccountProvisionJournalStorage
	}
	if err := file.Sync(); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	if err := file.Close(); err != nil {
		return ErrAccountProvisionJournalStorage
	}

	if expected != nil {
		current, err := readAccountProvisionJournalLocked(home, path)
		if err != nil {
			return err
		}
		matches := equalAccountProvisionJournal(current, *expected)
		clearAccountProvisionJournal(&current)
		if !matches {
			return ErrAccountProvisionJournalConflict
		}
		if err := os.Rename(temporaryPath, path); err != nil {
			return ErrAccountProvisionJournalStorage
		}
		temporaryExists = false
	} else {
		if err := os.Link(temporaryPath, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return ErrAccountProvisionJournalConflict
			}
			return ErrAccountProvisionJournalStorage
		}
		if err := os.Remove(temporaryPath); err != nil {
			return ErrAccountProvisionJournalStorage
		}
		temporaryExists = false
	}
	finalInfo, finalErr := os.Lstat(path)
	directoryAfter, directoryErr := os.Lstat(directory)
	if finalErr != nil || directoryErr != nil ||
		!os.SameFile(temporaryInfo, finalInfo) ||
		!os.SameFile(directoryBefore, directoryAfter) ||
		!privateRegularAccountProvisionJournalFile(finalInfo) ||
		!privateAccountProvisionJournalDirectory(directoryAfter) {
		return ErrAccountProvisionJournalUnsafe
	}
	if err := validateAccountProvisionJournalDirectories(home, directory); err != nil {
		return classifyAccountProvisionJournalDirectoryError(err)
	}
	if err := syncAccountProvisionJournalDirectory(directory); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	return nil
}

func accountProvisionJournalLocation(localName string) (home, path string, err error) {
	if !namePattern.MatchString(localName) {
		return "", "", ErrAccountProvisionJournalInvalid
	}
	home, err = root()
	if err != nil {
		return "", "", ErrAccountProvisionJournalStorage
	}
	return home, filepath.Join(home, "journal", "account-provision", localName+".json"), nil
}

func ensureAccountProvisionJournalDirectories(home, directory string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	for _, path := range accountProvisionJournalDirectories(home, directory) {
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return ErrAccountProvisionJournalStorage
			}
			info, err = os.Lstat(path)
			if err != nil {
				return ErrAccountProvisionJournalStorage
			}
		case err != nil:
			return ErrAccountProvisionJournalStorage
		}
		if !privateAccountProvisionJournalDirectory(info) {
			return ErrAccountProvisionJournalUnsafe
		}
	}
	return nil
}

func validateAccountProvisionJournalDirectories(home, directory string) error {
	for _, path := range accountProvisionJournalDirectories(home, directory) {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !privateAccountProvisionJournalDirectory(info) {
			return ErrAccountProvisionJournalUnsafe
		}
	}
	return nil
}

func accountProvisionJournalDirectories(home, directory string) []string {
	journal := filepath.Join(home, "journal")
	if directory != filepath.Join(journal, "account-provision") {
		return []string{directory}
	}
	return []string{home, journal, directory}
}

func validAccountProvisionJournal(record AccountProvisionJournal) bool {
	if record.SchemaVersion != accountProvisionJournalSchema ||
		!accountProvisionFingerprintPattern.MatchString(record.RequestFingerprint) ||
		!accountProvisionIDPattern.MatchString(record.ProvisionID) {
		return false
	}
	if record.AccountID == "" && record.OperatorToken == "" {
		return true
	}
	if !accountProvisionAccountIDPattern.MatchString(record.AccountID) {
		return false
	}
	kind, _, err := token.Parse(record.OperatorToken)
	return err == nil && kind == token.KindOperator &&
		strings.TrimSpace(record.OperatorToken) == record.OperatorToken
}

func clearAccountProvisionJournal(record *AccountProvisionJournal) {
	if record == nil {
		return
	}
	clear([]byte(record.OperatorToken))
	record.OperatorToken = ""
}

func equalAccountProvisionJournal(left, right AccountProvisionJournal) bool {
	return left.SchemaVersion == right.SchemaVersion &&
		left.RequestFingerprint == right.RequestFingerprint &&
		left.ProvisionID == right.ProvisionID &&
		left.AccountID == right.AccountID &&
		left.OperatorToken == right.OperatorToken
}

func privateAccountProvisionJournalDirectory(info os.FileInfo) bool {
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0 &&
		info.Mode().Perm() == 0o700
}

func privateRegularAccountProvisionJournalFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 &&
		info.Mode().Perm() == 0o600
}

func classifyAccountProvisionJournalDirectoryError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ErrAccountProvisionJournalUnavailable
	case errors.Is(err, ErrAccountProvisionJournalUnsafe):
		return ErrAccountProvisionJournalUnsafe
	default:
		return ErrAccountProvisionJournalStorage
	}
}

func syncAccountProvisionJournalDirectory(directory string) error {
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func readProvisionConfig(home string) (*Config, bool, error) {
	path := filepath.Join(home, "config.json")
	raw, exists, err := readPrivateProvisionFile(path, maxAccountProvisionConfigBytes)
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return &Config{Accounts: map[string]Account{}}, false, nil
	}
	defer clear(raw)
	var config Config
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, false, ErrAccountProvisionJournalInvalid
	}
	if config.Accounts == nil {
		config.Accounts = map[string]Account{}
	}
	return &config, true, nil
}

func readPrivateProvisionFile(path string, maximum int64) ([]byte, bool, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, ErrAccountProvisionJournalStorage
	}
	if !privateRegularAccountProvisionJournalFile(before) {
		return nil, false, ErrAccountProvisionJournalUnsafe
	}
	file, _, err := openLocalLockFileNoFollow(path, false)
	if err != nil {
		return nil, false, ErrAccountProvisionJournalUnsafe
	}
	defer func() { _ = file.Close() }()
	opened, statErr := file.Stat()
	linked, linkErr := os.Lstat(path)
	if statErr != nil || linkErr != nil || !os.SameFile(before, opened) ||
		!os.SameFile(opened, linked) ||
		!privateRegularAccountProvisionJournalFile(opened) ||
		opened.Size() < 0 || opened.Size() > maximum {
		return nil, false, ErrAccountProvisionJournalUnsafe
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, false, ErrAccountProvisionJournalStorage
	}
	if int64(len(raw)) > maximum {
		clear(raw)
		return nil, false, ErrAccountProvisionJournalInvalid
	}
	finalFile, fileErr := file.Stat()
	finalPath, pathErr := os.Lstat(path)
	if fileErr != nil || pathErr != nil ||
		!os.SameFile(opened, finalFile) ||
		!os.SameFile(finalFile, finalPath) ||
		!privateRegularAccountProvisionJournalFile(finalFile) ||
		!privateRegularAccountProvisionJournalFile(finalPath) {
		clear(raw)
		return nil, false, ErrAccountProvisionJournalUnsafe
	}
	return raw, true, nil
}

func ensurePrivateProvisionDirectory(home, directory string) error {
	relative, err := filepath.Rel(home, directory)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ErrAccountProvisionJournalUnsafe
	}
	current := home
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(current, 0o700); err != nil &&
				!errors.Is(err, os.ErrExist) {
				return ErrAccountProvisionJournalStorage
			}
			info, err = os.Lstat(current)
			if err != nil {
				return ErrAccountProvisionJournalStorage
			}
		case err != nil:
			return ErrAccountProvisionJournalStorage
		}
		if !privateAccountProvisionJournalDirectory(info) {
			return ErrAccountProvisionJournalUnsafe
		}
	}
	return nil
}

func publishPrivateProvisionFileNoReplace(path string, raw []byte) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".account-token-*.tmp")
	if err != nil {
		return ErrAccountProvisionJournalStorage
	}
	temporaryPath := file.Name()
	temporaryExists := true
	defer func() {
		_ = file.Close()
		if temporaryExists {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	info, err := file.Stat()
	if err != nil || !privateRegularAccountProvisionJournalFile(info) {
		return ErrAccountProvisionJournalUnsafe
	}
	written, err := io.Copy(file, bytes.NewReader(raw))
	if err != nil || written != int64(len(raw)) {
		return ErrAccountProvisionJournalStorage
	}
	if err := file.Sync(); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	if err := file.Close(); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrNameTaken
		}
		return ErrAccountProvisionJournalStorage
	}
	if err := os.Remove(temporaryPath); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	temporaryExists = false
	published, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, published) ||
		!privateRegularAccountProvisionJournalFile(published) {
		return ErrAccountProvisionJournalUnsafe
	}
	if err := syncAccountProvisionJournalDirectory(directory); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	return nil
}

func replaceProvisionConfig(home string, config *Config) error {
	raw, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return ErrAccountProvisionJournalInvalid
	}
	raw = append(raw, '\n')
	defer clear(raw)
	file, err := os.CreateTemp(home, ".config-*.tmp")
	if err != nil {
		return ErrAccountProvisionJournalStorage
	}
	temporaryPath := file.Name()
	temporaryExists := true
	defer func() {
		_ = file.Close()
		if temporaryExists {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	info, err := file.Stat()
	if err != nil || !privateRegularAccountProvisionJournalFile(info) {
		return ErrAccountProvisionJournalUnsafe
	}
	written, err := io.Copy(file, bytes.NewReader(raw))
	if err != nil || written != int64(len(raw)) {
		return ErrAccountProvisionJournalStorage
	}
	if err := file.Sync(); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	if err := file.Close(); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	path := filepath.Join(home, "config.json")
	if existing, err := os.Lstat(path); err == nil &&
		!privateRegularAccountProvisionJournalFile(existing) {
		return ErrAccountProvisionJournalUnsafe
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrAccountProvisionJournalStorage
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	temporaryExists = false
	published, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, published) ||
		!privateRegularAccountProvisionJournalFile(published) {
		return ErrAccountProvisionJournalUnsafe
	}
	if err := syncAccountProvisionJournalDirectory(home); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	return nil
}

func syncPrivateProvisionFile(path string) error {
	before, err := os.Lstat(path)
	if err != nil || !privateRegularAccountProvisionJournalFile(before) {
		return ErrAccountProvisionJournalUnsafe
	}
	file, _, err := openLocalLockFileNoFollow(path, false)
	if err != nil {
		return ErrAccountProvisionJournalUnsafe
	}
	defer func() { _ = file.Close() }()
	opened, statErr := file.Stat()
	linked, linkErr := os.Lstat(path)
	if statErr != nil || linkErr != nil || !os.SameFile(before, opened) ||
		!os.SameFile(opened, linked) ||
		!privateRegularAccountProvisionJournalFile(opened) {
		return ErrAccountProvisionJournalUnsafe
	}
	if err := file.Sync(); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	return nil
}
