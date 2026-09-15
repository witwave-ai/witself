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
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/jsonstrict"
	"github.com/witwave-ai/witself/internal/signuplegal"
	"github.com/witwave-ai/witself/internal/token"
)

const (
	accountProvisionJournalSchema      = "witself.account-provision-journal.v1"
	accountProvisionLegalJournalSchema = "witself.account-provision-journal.v2"
	maxAccountProvisionJournalBytes    = 16 * 1024
	maxAccountProvisionConfigBytes     = 4 * 1024 * 1024
	accountProvisionJournalLockFile    = ".lock"
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

	accountProvisionFingerprintPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	accountProvisionIDPattern             = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	accountProvisionAccountIDPattern      = regexp.MustCompile(`^acc_[a-z2-7]{16}$`)
	accountProvisionConsentVersionPattern = regexp.MustCompile(
		`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`,
	)
)

// AccountProvisionJournal is the private, crash-safe handoff for one local
// account signup. RequestFingerprint binds the endpoint, local name, and
// normalized signup request without retaining their plaintext; the only
// request values retained are the non-sensitive accepted legal version labels.
// AccountID and OperatorToken are added only after the one-shot bootstrap
// exchange succeeds, so a crash cannot strand the consumed credential.
// Refused v2 journals retain one bounded refusal and optional candidate; promotion
// or a credential winner returns to v1 without carrying recursive lineage.
type AccountProvisionJournal struct {
	SchemaVersion          string               `json:"schema_version"`
	RequestFingerprint     string               `json:"request_fingerprint"`
	ProvisionID            string               `json:"provision_id"`
	AcceptedTermsVersion   string               `json:"accepted_terms_version,omitempty"`
	AcceptedPrivacyVersion string               `json:"accepted_privacy_version,omitempty"`
	AccountID              string               `json:"account_id,omitempty"`
	OperatorToken          string               `json:"operator_token,omitempty"`
	LegalRefusal           *signuplegal.Refusal `json:"legal_refusal,omitempty"`
	Reconsent              *signuplegal.Pending `json:"reconsent,omitempty"`
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
	return BeginAccountProvisionJournalWithConsent(
		localName, requestFingerprint, "", "",
	)
}

// BeginAccountProvisionJournalWithConsent is BeginAccountProvisionJournal for
// a signup that also needs to retain the exact non-sensitive legal version
// labels accepted by the user. Keeping the labels beside the fingerprint lets
// a later CLI resume the same request after its compiled-in legal versions have
// changed. The fields are omitted for consentless signups, so v1 journals from
// older clients remain valid and byte-compatible in shape.
func BeginAccountProvisionJournalWithConsent(
	localName, requestFingerprint, acceptedTermsVersion,
	acceptedPrivacyVersion string,
) (AccountProvisionJournal, bool, error) {
	return beginAccountProvisionJournalWithConsent(localName, requestFingerprint, acceptedTermsVersion, acceptedPrivacyVersion, syncAccountProvisionJournalDirectory)
}

func beginAccountProvisionJournalWithConsent(
	localName, requestFingerprint, acceptedTermsVersion, acceptedPrivacyVersion string,
	syncReplayDirectory func(string) error,
) (AccountProvisionJournal, bool, error) {
	if !accountProvisionFingerprintPattern.MatchString(requestFingerprint) ||
		!validAccountProvisionConsentVersions(
			acceptedTermsVersion, acceptedPrivacyVersion,
		) {
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
		if current.RequestFingerprint != requestFingerprint ||
			(current.AcceptedTermsVersion != "" &&
				(current.AcceptedTermsVersion != acceptedTermsVersion ||
					current.AcceptedPrivacyVersion != acceptedPrivacyVersion)) {
			clearAccountProvisionJournal(&current)
			return AccountProvisionJournal{}, false, ErrAccountProvisionJournalConflict
		}
		if current.AcceptedTermsVersion == "" && acceptedTermsVersion != "" {
			expected := current
			current.AcceptedTermsVersion = acceptedTermsVersion
			current.AcceptedPrivacyVersion = acceptedPrivacyVersion
			if err := publishAccountProvisionJournal(
				home, path, current, &expected,
			); err != nil {
				clearAccountProvisionJournal(&current)
				return AccountProvisionJournal{}, false, err
			}
		}
		// A prior initial publication or reconsent promotion may have renamed
		// successfully but failed its directory sync. A visible record alone
		// cannot authorize the next remote mutation until replay sync succeeds.
		if err := syncReplayDirectory(directory); err != nil {
			clearAccountProvisionJournal(&current)
			return AccountProvisionJournal{}, false, ErrAccountProvisionJournalStorage
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
		SchemaVersion:          accountProvisionJournalSchema,
		RequestFingerprint:     requestFingerprint,
		ProvisionID:            provisionID,
		AcceptedTermsVersion:   acceptedTermsVersion,
		AcceptedPrivacyVersion: acceptedPrivacyVersion,
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
	expected := current
	current.AccountID = accountID
	current.OperatorToken = operatorToken
	// The original exact credential wins over a racing refusal or transition.
	// A stale promotion must never replace this completed handoff.
	current.SchemaVersion = accountProvisionJournalSchema
	current.LegalRefusal = nil
	current.Reconsent = nil
	return publishAccountProvisionJournal(home, path, current, &expected)
}

// RecordAccountProvisionLegalRefusal retains a typed, exact not-admitted
// receipt without changing the original request. An identical lost local
// acknowledgement can replay; any different full-record owner conflicts.
func RecordAccountProvisionLegalRefusal(
	localName string, expected AccountProvisionJournal, refusal signuplegal.Refusal,
) (AccountProvisionJournal, error) {
	if !validAccountProvisionJournal(expected) || expected.AccountID != "" ||
		refusal.ValidateFor(expected.ProvisionID, expected.AcceptedTermsVersion, expected.AcceptedPrivacyVersion) != nil {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalInvalid
	}
	if expected.LegalRefusal != nil && *expected.LegalRefusal != refusal {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalConflict
	}
	next := expected
	next.SchemaVersion = accountProvisionLegalJournalSchema
	next.LegalRefusal = &refusal
	return updateAccountProvisionLegalJournal(localName, func(current AccountProvisionJournal) (AccountProvisionJournal, error) {
		if equalAccountProvisionJournal(current, next) {
			return current, nil
		}
		if !equalAccountProvisionJournal(current, expected) {
			return AccountProvisionJournal{}, ErrAccountProvisionJournalConflict
		}
		return next, nil
	})
}

// BeginAccountProvisionReconsent durably elects one candidate before remote
// registration. Retries with the same requested pair and local fingerprint
// reuse the winner; they never choose another candidate after an ambiguous ack.
func BeginAccountProvisionReconsent(
	localName string, expected AccountProvisionJournal, newLocalFingerprint, terms, privacy string,
) (AccountProvisionJournal, error) {
	if !validAccountProvisionJournal(expected) || expected.LegalRefusal == nil ||
		expected.AccountID != "" || !accountProvisionFingerprintPattern.MatchString(newLocalFingerprint) ||
		terms == "" || !validAccountProvisionConsentVersions(terms, privacy) {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalInvalid
	}
	return updateAccountProvisionLegalJournal(localName, func(current AccountProvisionJournal) (AccountProvisionJournal, error) {
		// A concurrently persisted candidate is replayable only when the entire
		// predecessor still matches and the requested local identity/pair agrees.
		owner := current
		owner.Reconsent = expected.Reconsent
		if !equalAccountProvisionJournal(owner, expected) {
			return AccountProvisionJournal{}, ErrAccountProvisionJournalConflict
		}
		if current.Reconsent != nil {
			if (expected.Reconsent != nil && *current.Reconsent != *expected.Reconsent) ||
				current.Reconsent.RequestFingerprint != newLocalFingerprint ||
				current.Reconsent.Candidate.ConsentTermsVersion != terms ||
				current.Reconsent.Candidate.ConsentPrivacyVersion != privacy {
				return AccountProvisionJournal{}, ErrAccountProvisionJournalConflict
			}
			return current, nil
		}
		if expected.Reconsent != nil {
			return AccountProvisionJournal{}, ErrAccountProvisionJournalConflict
		}
		candidateID, err := id.New("prv")
		if err != nil {
			return AccountProvisionJournal{}, ErrAccountProvisionJournalStorage
		}
		transitionID, err := id.New("trn")
		if err != nil {
			return AccountProvisionJournal{}, ErrAccountProvisionJournalStorage
		}
		current.Reconsent = &signuplegal.Pending{
			Candidate:    signuplegal.Candidate{ProvisionID: candidateID, ConsentTermsVersion: terms, ConsentPrivacyVersion: privacy},
			TransitionID: transitionID, RequestFingerprint: newLocalFingerprint,
		}
		return current, nil
	})
}

// PromoteAccountProvisionReconsent replaces the predecessor only after an
// exact registered acknowledgement. The transport additionally binds the ack
// to the actual request core; the journal never retains email/name/invite.
func PromoteAccountProvisionReconsent(
	localName string, expected AccountProvisionJournal, ack signuplegal.Ack,
) (AccountProvisionJournal, error) {
	if !validAccountProvisionJournal(expected) || expected.LegalRefusal == nil ||
		expected.Reconsent == nil || expected.AccountID != "" || ack.Validate() != nil {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalInvalid
	}
	pending := *expected.Reconsent
	if ack.Refusal != *expected.LegalRefusal || ack.Candidate != pending.Candidate || ack.TransitionID != pending.TransitionID {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalConflict
	}
	// The CP fingerprint is opaque and is not the local fingerprint.
	next := AccountProvisionJournal{
		SchemaVersion:          accountProvisionJournalSchema,
		RequestFingerprint:     pending.RequestFingerprint,
		ProvisionID:            pending.Candidate.ProvisionID,
		AcceptedTermsVersion:   pending.Candidate.ConsentTermsVersion,
		AcceptedPrivacyVersion: pending.Candidate.ConsentPrivacyVersion,
	}
	return updateAccountProvisionLegalJournal(localName, func(current AccountProvisionJournal) (AccountProvisionJournal, error) {
		if equalAccountProvisionJournal(current, next) {
			return current, nil
		}
		if !equalAccountProvisionJournal(current, expected) {
			return AccountProvisionJournal{}, ErrAccountProvisionJournalConflict
		}
		return next, nil
	})
}

func updateAccountProvisionLegalJournal(
	localName string, next func(AccountProvisionJournal) (AccountProvisionJournal, error),
) (AccountProvisionJournal, error) {
	home, path, err := accountProvisionJournalLocation(localName)
	if err != nil {
		return AccountProvisionJournal{}, err
	}
	directory := filepath.Dir(path)
	if err := validateAccountProvisionJournalDirectories(home, directory); err != nil {
		return AccountProvisionJournal{}, classifyAccountProvisionJournalDirectoryError(err)
	}
	lock, err := acquireAccountProvisionJournalLock(home, directory)
	if err != nil {
		return AccountProvisionJournal{}, err
	}
	defer lock.release()
	current, err := readAccountProvisionJournalLocked(home, path)
	if err != nil {
		return AccountProvisionJournal{}, err
	}
	defer clearAccountProvisionJournal(&current)
	record, err := next(current)
	if err != nil {
		return AccountProvisionJournal{}, err
	}
	if !validAccountProvisionJournal(record) {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalInvalid
	}
	if equalAccountProvisionJournal(record, current) {
		// An earlier rename may have completed before its directory sync failed.
		// Replay does not authorize remote work until that boundary is durable.
		if err := syncAccountProvisionJournalDirectory(directory); err != nil {
			return AccountProvisionJournal{}, ErrAccountProvisionJournalStorage
		}
		return record, nil
	}
	if err := publishAccountProvisionJournal(home, path, record, &current); err != nil {
		clearAccountProvisionJournal(&record)
		return AccountProvisionJournal{}, err
	}
	return record, nil
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
	if err := removeAccountProvisionPrivateFile(path, info); err != nil {
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

	tokenPath := filepath.Join(home, "tokens", "accounts", localName, "owner.token")
	legacyPath := filepath.Join(home, "tokens", "accounts", localName+".token")
	if _, legacyErr := os.Lstat(legacyPath); legacyErr == nil {
		return ErrNameTaken
	} else if !errors.Is(legacyErr, os.ErrNotExist) {
		return ErrAccountProvisionJournalStorage
	}
	if err := lock.pinCredentialDirectory(home, filepath.Dir(tokenPath)); err != nil {
		return err
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
	pins []*accountProvisionDirectoryPins
}

func acquireAccountProvisionJournalLock(
	home, directory string,
) (*accountProvisionJournalLock, error) {
	if err := validateAccountProvisionJournalDirectories(home, directory); err != nil {
		return nil, classifyAccountProvisionJournalDirectoryError(err)
	}
	path := filepath.Join(directory, accountProvisionJournalLockFile)
	pins, err := pinAccountProvisionDirectories(home, directory, false)
	if err != nil {
		return nil, classifyAccountProvisionJournalDirectoryError(err)
	}
	pinsOwned := true
	defer func() {
		if pinsOwned {
			_ = pins.close()
		}
	}()
	file, created, err := openAccountProvisionPrivateFile(path, true)
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
	if err := validateAccountProvisionPrivateFileHandle(file, path, opened); err != nil {
		return nil, err
	}
	if err := syncAccountProvisionLockPublication(file, directory, created); err != nil {
		return nil, err
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
	if err := validateAccountProvisionPrivateFileHandle(file, path, opened); err != nil {
		_ = unlockLocalFile(file)
		return nil, err
	}
	cleanup = false
	pinsOwned = false
	return &accountProvisionJournalLock{file: file, pins: []*accountProvisionDirectoryPins{pins}}, nil
}

func (lock *accountProvisionJournalLock) release() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = unlockLocalFile(lock.file)
	_ = lock.file.Close()
	lock.file = nil
	for i := len(lock.pins) - 1; i >= 0; i-- {
		_ = lock.pins[i].close()
	}
	lock.pins = nil
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
	file, _, err := openAccountProvisionPrivateFile(path, false)
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
	if err := validateAccountProvisionPrivateFileHandle(file, path, opened); err != nil {
		return AccountProvisionJournal{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxAccountProvisionJournalBytes+1))
	if err != nil {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalStorage
	}
	defer clear(raw)
	if len(raw) > maxAccountProvisionJournalBytes {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalInvalid
	}
	unique := json.NewDecoder(bytes.NewReader(raw))
	if !utf8.Valid(raw) || jsonstrict.ConsumeUniqueValue(unique) != nil || jsonstrict.RequireEOF(unique) != nil {
		return AccountProvisionJournal{}, ErrAccountProvisionJournalInvalid
	}
	var record AccountProvisionJournal
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil || !validAccountProvisionJournal(record) || !validAccountProvisionJournalJSON(raw, record) {
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
	if err := validateAccountProvisionPrivateFileHandle(file, path, opened); err != nil {
		clearAccountProvisionJournal(&record)
		return AccountProvisionJournal{}, err
	}
	return record, nil
}

func publishAccountProvisionJournal(
	home, path string,
	record AccountProvisionJournal,
	expected *AccountProvisionJournal,
) error {
	return publishAccountProvisionJournalWithIO(home, path, record, expected, accountProvisionJournalIO{
		syncFile: (*os.File).Sync, rename: os.Rename, syncDirectory: syncAccountProvisionJournalDirectory,
	})
}

// Per-call operations keep deterministic publication fault tests isolated;
// every production caller uses the real file sync, atomic rename and dir sync.
type accountProvisionJournalIO struct {
	syncFile      func(*os.File) error
	rename        func(string, string) error
	syncDirectory func(string) error
}

func publishAccountProvisionJournalWithIO(
	home, path string, record AccountProvisionJournal, expected *AccountProvisionJournal, fs accountProvisionJournalIO,
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
	file, err := createAccountProvisionPrivateTemp(directory, ".account-provision-*.tmp")
	if err != nil {
		return ErrAccountProvisionJournalStorage
	}
	temporaryPath := file.Name()
	temporaryExists := true
	cleanupIdentity, _ := file.Stat()
	defer func() {
		_ = file.Close()
		if temporaryExists {
			_ = removeAccountProvisionPrivateFile(temporaryPath, cleanupIdentity)
		}
	}()
	if err := prepareAccountProvisionPrivateTemp(file); err != nil {
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
	if err := fs.syncFile(file); err != nil {
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
		if err := fs.rename(temporaryPath, path); err != nil {
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
		if err := removeAccountProvisionPrivateFile(temporaryPath, cleanupIdentity); err != nil {
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
	if err := validateAccountProvisionPrivatePath(path, temporaryInfo); err != nil {
		return err
	}
	if err := fs.syncDirectory(directory); err != nil {
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
	home, err = accountProvisionHome(home)
	if err != nil {
		return "", "", err
	}
	return home, filepath.Join(home, "journal", "account-provision", localName+".json"), nil
}

func accountProvisionJournalDirectories(home, directory string) []string {
	journal := filepath.Join(home, "journal")
	if directory != filepath.Join(journal, "account-provision") {
		return []string{directory}
	}
	return []string{home, journal, directory}
}

func validAccountProvisionJournal(record AccountProvisionJournal) bool {
	if (record.SchemaVersion != accountProvisionJournalSchema && record.SchemaVersion != accountProvisionLegalJournalSchema) ||
		!accountProvisionFingerprintPattern.MatchString(record.RequestFingerprint) ||
		!accountProvisionIDPattern.MatchString(record.ProvisionID) ||
		!validAccountProvisionConsentVersions(
			record.AcceptedTermsVersion, record.AcceptedPrivacyVersion,
		) {
		return false
	}
	if record.SchemaVersion == accountProvisionLegalJournalSchema {
		if record.AccountID != "" || record.OperatorToken != "" || record.LegalRefusal == nil ||
			record.LegalRefusal.ValidateFor(record.ProvisionID, record.AcceptedTermsVersion, record.AcceptedPrivacyVersion) != nil {
			return false
		}
		return record.Reconsent == nil || (record.Reconsent.Validate() == nil && record.Reconsent.Candidate.ProvisionID != record.ProvisionID)
	}
	if record.LegalRefusal != nil || record.Reconsent != nil {
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

// Reject case aliases and null fields in addition to duplicate/trailing JSON.
// Nested protocol shapes are checked before any record can authorize a write.
func validAccountProvisionJournalJSON(raw []byte, record AccountProvisionJournal) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for key, value := range fields {
		switch key {
		case "schema_version", "request_fingerprint", "provision_id", "accepted_terms_version", "accepted_privacy_version", "account_id", "operator_token", "legal_refusal", "reconsent":
		default:
			return false
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	if record.LegalRefusal != nil {
		refusal, err := signuplegal.DecodeRefusal(fields["legal_refusal"])
		if err != nil || refusal != *record.LegalRefusal {
			return false
		}
	}
	if record.Reconsent != nil {
		var pending, candidate map[string]json.RawMessage
		if json.Unmarshal(fields["reconsent"], &pending) != nil || len(pending) != 3 ||
			len(pending["candidate"]) == 0 || len(pending["transition_id"]) == 0 || len(pending["request_fingerprint"]) == 0 ||
			json.Unmarshal(pending["candidate"], &candidate) != nil || len(candidate) != 3 ||
			len(candidate["provision_id"]) == 0 || len(candidate["consent_terms_version"]) == 0 || len(candidate["consent_privacy_version"]) == 0 {
			return false
		}
	}
	return true
}

func validAccountProvisionConsentVersions(termsVersion, privacyVersion string) bool {
	if termsVersion == "" || privacyVersion == "" {
		return termsVersion == "" && privacyVersion == ""
	}
	return accountProvisionConsentVersionPattern.MatchString(termsVersion) &&
		accountProvisionConsentVersionPattern.MatchString(privacyVersion)
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
		left.AcceptedTermsVersion == right.AcceptedTermsVersion &&
		left.AcceptedPrivacyVersion == right.AcceptedPrivacyVersion &&
		left.AccountID == right.AccountID &&
		left.OperatorToken == right.OperatorToken &&
		(left.LegalRefusal == nil) == (right.LegalRefusal == nil) &&
		(left.LegalRefusal == nil || *left.LegalRefusal == *right.LegalRefusal) &&
		(left.Reconsent == nil) == (right.Reconsent == nil) &&
		(left.Reconsent == nil || *left.Reconsent == *right.Reconsent)
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
	file, _, err := openAccountProvisionPrivateFile(path, false)
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
	if err := validateAccountProvisionPrivateFileHandle(file, path, opened); err != nil {
		return nil, false, err
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
	if err := validateAccountProvisionPrivateFileHandle(file, path, opened); err != nil {
		clear(raw)
		return nil, false, err
	}
	return raw, true, nil
}

func publishPrivateProvisionFileNoReplace(path string, raw []byte) error {
	directory := filepath.Dir(path)
	file, err := createAccountProvisionPrivateTemp(directory, ".account-token-*.tmp")
	if err != nil {
		return ErrAccountProvisionJournalStorage
	}
	temporaryPath := file.Name()
	temporaryExists := true
	cleanupIdentity, _ := file.Stat()
	defer func() {
		_ = file.Close()
		if temporaryExists {
			_ = removeAccountProvisionPrivateFile(temporaryPath, cleanupIdentity)
		}
	}()
	if err := prepareAccountProvisionPrivateTemp(file); err != nil {
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
	if err := removeAccountProvisionPrivateFile(temporaryPath, cleanupIdentity); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	temporaryExists = false
	published, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, published) ||
		!privateRegularAccountProvisionJournalFile(published) {
		return ErrAccountProvisionJournalUnsafe
	}
	if err := validateAccountProvisionPrivatePath(path, info); err != nil {
		return err
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
	file, err := createAccountProvisionPrivateTemp(home, ".config-*.tmp")
	if err != nil {
		return ErrAccountProvisionJournalStorage
	}
	temporaryPath := file.Name()
	temporaryExists := true
	cleanupIdentity, _ := file.Stat()
	defer func() {
		_ = file.Close()
		if temporaryExists {
			_ = removeAccountProvisionPrivateFile(temporaryPath, cleanupIdentity)
		}
	}()
	if err := prepareAccountProvisionPrivateTemp(file); err != nil {
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
	if existing, err := os.Lstat(path); err == nil {
		if !privateRegularAccountProvisionJournalFile(existing) {
			return ErrAccountProvisionJournalUnsafe
		}
		if err := validateAccountProvisionPrivatePath(path, existing); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
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
	if err := validateAccountProvisionPrivatePath(path, info); err != nil {
		return err
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
	file, _, err := openAccountProvisionPrivateFile(path, false)
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
	if err := validateAccountProvisionPrivateFileHandle(file, path, opened); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	return validateAccountProvisionPrivateFileHandle(file, path, opened)
}

func (lock *accountProvisionJournalLock) pinCredentialDirectory(home, directory string) error {
	pins, err := pinAccountProvisionCredentialDirectories(home, directory)
	if err != nil {
		return err
	}
	lock.pins = append(lock.pins, pins)
	return nil
}
