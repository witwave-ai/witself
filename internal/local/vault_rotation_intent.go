package local

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/sealed"
	"golang.org/x/sys/unix"
)

const (
	agentVaultKeyRotationIntentSchemaV1 uint32 = 1
	agentVaultKeyRotationIntentFile            = "intent.state"
	agentVaultKeyRotationLockFile              = ".intent.lock"
	agentVaultKeyRotationIntentDomain          = "witself/local/avk-rotation-intent/v1\x00"
	maxAgentVaultKeyRotationIntentBytes        = 8 * 1024
)

var (
	// ErrAgentVaultKeyRotationIntentUnavailable means no local intent exists.
	ErrAgentVaultKeyRotationIntentUnavailable = errors.New("agent vault key rotation intent is unavailable")
	// ErrAgentVaultKeyRotationIntentExists means exclusive creation found an intent.
	ErrAgentVaultKeyRotationIntentExists = errors.New("agent vault key rotation intent already exists")
	// ErrAgentVaultKeyRotationIntentConflict means the exact durable intent changed.
	ErrAgentVaultKeyRotationIntentConflict = errors.New("agent vault key rotation intent conflicts")
	// ErrAgentVaultKeyRotationIntentUnsafe means the intent path is not owner-only.
	ErrAgentVaultKeyRotationIntentUnsafe = errors.New("agent vault key rotation intent storage is unsafe")
	// ErrAgentVaultKeyRotationIntentInvalid means the durable record failed validation.
	ErrAgentVaultKeyRotationIntentInvalid = errors.New("agent vault key rotation intent is invalid")
	// ErrAgentVaultKeyRotationIntentStorage hides OS-specific storage failures.
	ErrAgentVaultKeyRotationIntentStorage = errors.New("agent vault key rotation intent storage failed")
	// ErrAgentVaultKeyRotationIntentScope identifies invalid local selectors.
	ErrAgentVaultKeyRotationIntentScope = errors.New("agent vault key rotation intent scope is invalid")
)

// AgentVaultKeyRotationIntent is the public, value-free retry fence written
// before Start. It never contains AVK bytes, DEKs, secret values, or tokens.
// Start fields are both populated for a locally prepared Start and both empty
// for a safely adopted already-open server rotation.
type AgentVaultKeyRotationIntent struct {
	RotationID                  string                       `json:"rotation_id"`
	AccountID                   string                       `json:"account_id"`
	RealmID                     string                       `json:"realm_id"`
	OwnerAgentID                string                       `json:"owner_agent_id"`
	Source                      sealed.AgentVaultKeyMetadata `json:"source"`
	Target                      sealed.AgentVaultKeyMetadata `json:"target"`
	ExpectedSourceKeyRowVersion int64                        `json:"expected_source_key_row_version,omitempty"`
	StartIdempotencyKey         string                       `json:"start_idempotency_key,omitempty"`
}

type agentVaultKeyRotationIntentRecord struct {
	SchemaVersion               uint32                       `json:"schema_version"`
	RotationID                  string                       `json:"rotation_id"`
	AccountID                   string                       `json:"account_id"`
	RealmID                     string                       `json:"realm_id"`
	OwnerAgentID                string                       `json:"owner_agent_id"`
	Source                      sealed.AgentVaultKeyMetadata `json:"source"`
	Target                      sealed.AgentVaultKeyMetadata `json:"target"`
	ExpectedSourceKeyRowVersion int64                        `json:"expected_source_key_row_version,omitempty"`
	StartIdempotencyKey         string                       `json:"start_idempotency_key,omitempty"`
	Checksum                    string                       `json:"checksum"`
}

type agentVaultKeyRotationIntentBody struct {
	SchemaVersion               uint32                       `json:"schema_version"`
	RotationID                  string                       `json:"rotation_id"`
	AccountID                   string                       `json:"account_id"`
	RealmID                     string                       `json:"realm_id"`
	OwnerAgentID                string                       `json:"owner_agent_id"`
	Source                      sealed.AgentVaultKeyMetadata `json:"source"`
	Target                      sealed.AgentVaultKeyMetadata `json:"target"`
	ExpectedSourceKeyRowVersion int64                        `json:"expected_source_key_row_version,omitempty"`
	StartIdempotencyKey         string                       `json:"start_idempotency_key,omitempty"`
}

// AgentVaultKeyRotationIntentPath returns the one managed intent path for a
// local agent selector. Only one unacknowledged rotation may occupy it.
func AgentVaultKeyRotationIntentPath(account, realm, agent string) (string, error) {
	_, _, path, err := agentVaultKeyRotationIntentLocation(account, realm, agent)
	return path, err
}

// CreateAgentVaultKeyRotationIntent publishes one immutable owner-only intent
// with atomic no-replace semantics. Callers must persist the target AVK epoch
// before this function and must persist this intent before remote Start.
func CreateAgentVaultKeyRotationIntent(account, realm, agent string, intent AgentVaultKeyRotationIntent) error {
	home, directory, path, err := agentVaultKeyRotationIntentLocation(account, realm, agent)
	if err != nil {
		return err
	}
	raw, err := marshalAgentVaultKeyRotationIntent(account, realm, agent, intent)
	if err != nil {
		return err
	}
	defer clear(raw)
	lock, err := acquireAgentVaultKeyRotationIntentLock(home, directory)
	if err != nil {
		return mapAgentVaultKeyRotationIntentStorageError(err)
	}
	defer lock.release()
	if err := publishAgentVaultKeyEnrollmentFile(home, directory, path, raw); err != nil {
		if errors.Is(err, errAgentVaultKeyEnrollmentCollision) {
			info, statErr := os.Lstat(path)
			if statErr != nil || !privateAgentVaultKeyEnrollmentFile(info) {
				return ErrAgentVaultKeyRotationIntentUnsafe
			}
			return ErrAgentVaultKeyRotationIntentExists
		}
		return mapAgentVaultKeyRotationIntentStorageError(err)
	}
	return nil
}

// ReadAgentVaultKeyRotationIntent verifies the owner-only path, canonical JSON,
// local-scope checksum, exact algorithms, IDs, versions, and retry fence.
func ReadAgentVaultKeyRotationIntent(account, realm, agent string) (*AgentVaultKeyRotationIntent, error) {
	home, directory, path, err := agentVaultKeyRotationIntentLocation(account, realm, agent)
	if err != nil {
		return nil, err
	}
	if err := validateAgentVaultKeyEnrollmentDirectories(home, directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrAgentVaultKeyRotationIntentUnavailable
		}
		return nil, mapAgentVaultKeyRotationIntentStorageError(err)
	}
	raw, err := readAgentVaultKeyEnrollmentFile(home, directory, path)
	if err != nil {
		if errors.Is(err, ErrAgentVaultKeyEnrollmentUnavailable) {
			return nil, ErrAgentVaultKeyRotationIntentUnavailable
		}
		return nil, mapAgentVaultKeyRotationIntentStorageError(err)
	}
	defer clear(raw)
	return parseAgentVaultKeyRotationIntent(account, realm, agent, raw)
}

// DeleteAgentVaultKeyRotationIntentAfterAcknowledge deletes only the exact
// rotation named by the caller and syncs the directory. Missing state is an
// idempotent success. This is intentionally separate from Rotate: a committed
// intent survives process failure and repeated Rotate calls until the caller
// explicitly acknowledges the already-returned terminal outcome.
func DeleteAgentVaultKeyRotationIntentAfterAcknowledge(account, realm, agent, rotationID string) error {
	if !validLocalPrefixedID(rotationID, "vkr") {
		return ErrAgentVaultKeyRotationIntentScope
	}
	home, directory, path, err := agentVaultKeyRotationIntentLocation(account, realm, agent)
	if err != nil {
		return err
	}
	lock, err := acquireAgentVaultKeyRotationIntentLock(home, directory)
	if err != nil {
		return mapAgentVaultKeyRotationIntentStorageError(err)
	}
	defer lock.release()
	intent, err := ReadAgentVaultKeyRotationIntent(account, realm, agent)
	if errors.Is(err, ErrAgentVaultKeyRotationIntentUnavailable) {
		return nil
	}
	if err != nil {
		return err
	}
	if intent.RotationID != rotationID {
		return ErrAgentVaultKeyRotationIntentConflict
	}
	if err := removeAgentVaultKeyEnrollmentFile(path); err != nil {
		return mapAgentVaultKeyRotationIntentStorageError(err)
	}
	if err := syncAgentVaultKeyEnrollmentDirectory(directory); err != nil {
		return ErrAgentVaultKeyRotationIntentStorage
	}
	return nil
}

// RetireAgentVaultKeyRotationIntentAfterCanonicalAdvance removes only an exact
// pristine prepared intent after the authenticated backend has proved that its
// current epoch advanced without ever creating that rotation ID. The caller
// supplies the full expected record so a stale process cannot retire a newer
// journal that happens to occupy the same path.
func RetireAgentVaultKeyRotationIntentAfterCanonicalAdvance(account, realm, agent string, expected AgentVaultKeyRotationIntent) error {
	if !validAgentVaultKeyRotationIntent(expected) || expected.ExpectedSourceKeyRowVersion < 1 ||
		expected.StartIdempotencyKey == "" {
		return ErrAgentVaultKeyRotationIntentConflict
	}
	home, directory, path, err := agentVaultKeyRotationIntentLocation(account, realm, agent)
	if err != nil {
		return err
	}
	lock, err := acquireAgentVaultKeyRotationIntentLock(home, directory)
	if err != nil {
		return mapAgentVaultKeyRotationIntentStorageError(err)
	}
	defer lock.release()
	current, err := ReadAgentVaultKeyRotationIntent(account, realm, agent)
	if err != nil {
		return err
	}
	if *current != expected {
		return ErrAgentVaultKeyRotationIntentConflict
	}
	if err := removeAgentVaultKeyEnrollmentFile(path); err != nil {
		return mapAgentVaultKeyRotationIntentStorageError(err)
	}
	if err := syncAgentVaultKeyEnrollmentDirectory(directory); err != nil {
		return ErrAgentVaultKeyRotationIntentStorage
	}
	return nil
}

// ReplaceAgentVaultKeyRotationIntentAfterCanonicalConflict atomically
// supersedes one pristine locally prepared intent with an adopted canonical
// server rotation. This narrow transition exists for two installations that
// concurrently prepared different targets: only the server winner may replace
// the never-started loser. Both intents must name the same owner scope, and the
// canonical source must be either identical or strictly newer; it can never
// move backward. Neither ordinary mutation nor terminal cleanup uses this
// function.
func ReplaceAgentVaultKeyRotationIntentAfterCanonicalConflict(account, realm, agent string, expected, replacement AgentVaultKeyRotationIntent) error {
	if !validAgentVaultKeyRotationIntent(expected) || expected.ExpectedSourceKeyRowVersion < 1 ||
		expected.StartIdempotencyKey == "" || !validAgentVaultKeyRotationIntent(replacement) ||
		replacement.ExpectedSourceKeyRowVersion != 0 || replacement.StartIdempotencyKey != "" ||
		expected.RotationID == replacement.RotationID || expected.AccountID != replacement.AccountID ||
		expected.RealmID != replacement.RealmID || expected.OwnerAgentID != replacement.OwnerAgentID ||
		(expected.Source != replacement.Source && replacement.Source.Version <= expected.Source.Version) {
		return ErrAgentVaultKeyRotationIntentConflict
	}
	home, directory, path, err := agentVaultKeyRotationIntentLocation(account, realm, agent)
	if err != nil {
		return err
	}
	lock, err := acquireAgentVaultKeyRotationIntentLock(home, directory)
	if err != nil {
		return mapAgentVaultKeyRotationIntentStorageError(err)
	}
	defer lock.release()
	current, err := ReadAgentVaultKeyRotationIntent(account, realm, agent)
	if err != nil {
		return err
	}
	if *current != expected {
		return ErrAgentVaultKeyRotationIntentConflict
	}
	raw, err := marshalAgentVaultKeyRotationIntent(account, realm, agent, replacement)
	if err != nil {
		return err
	}
	defer clear(raw)
	if err := replaceAgentVaultKeyRotationIntentFile(home, directory, path, expected, account, realm, agent, raw); err != nil {
		if errors.Is(err, ErrAgentVaultKeyRotationIntentConflict) ||
			errors.Is(err, ErrAgentVaultKeyRotationIntentUnsafe) ||
			errors.Is(err, ErrAgentVaultKeyRotationIntentInvalid) ||
			errors.Is(err, ErrAgentVaultKeyRotationIntentStorage) {
			return err
		}
		return mapAgentVaultKeyRotationIntentStorageError(err)
	}
	return nil
}

type agentVaultKeyRotationIntentLock struct {
	file *os.File
}

func acquireAgentVaultKeyRotationIntentLock(home, directory string) (*agentVaultKeyRotationIntentLock, error) {
	if err := ensureAgentVaultKeyEnrollmentDirectories(home, directory); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, agentVaultKeyRotationLockFile)
	if err := publishAgentVaultKeyEnrollmentFile(home, directory, path, []byte("witself-avk-rotation-lock-v1\n")); err != nil {
		if !errors.Is(err, errAgentVaultKeyEnrollmentCollision) {
			return nil, err
		}
		info, statErr := os.Lstat(path)
		if statErr != nil || !privateAgentVaultKeyEnrollmentFile(info) {
			return nil, ErrAgentVaultKeyRotationIntentUnsafe
		}
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrAgentVaultKeyRotationIntentUnsafe
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, ErrAgentVaultKeyRotationIntentStorage
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
		!privateAgentVaultKeyEnrollmentFile(opened) || !privateAgentVaultKeyEnrollmentFile(linked) {
		return nil, ErrAgentVaultKeyRotationIntentUnsafe
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return nil, ErrAgentVaultKeyRotationIntentStorage
	}
	// Revalidate the stable inode and its owner-only directory chain after the
	// blocking lock acquisition. A lock-path replacement cannot silently split
	// cooperating CLI processes across different lock inodes.
	linked, linkErr = os.Lstat(path)
	if linkErr != nil || !os.SameFile(opened, linked) || !privateAgentVaultKeyEnrollmentFile(linked) {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, ErrAgentVaultKeyRotationIntentUnsafe
	}
	if err := validateAgentVaultKeyEnrollmentDirectories(home, directory); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, err
	}
	cleanup = false
	return &agentVaultKeyRotationIntentLock{file: file}, nil
}

func (lock *agentVaultKeyRotationIntentLock) release() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	_ = lock.file.Close()
	lock.file = nil
}

func replaceAgentVaultKeyRotationIntentFile(home, directory, path string, expected AgentVaultKeyRotationIntent, account, realm, agent string, raw []byte) error {
	if len(raw) == 0 || len(raw) > maxAgentVaultKeyRotationIntentBytes {
		return ErrAgentVaultKeyRotationIntentInvalid
	}
	if err := validateAgentVaultKeyEnrollmentDirectories(home, directory); err != nil {
		return err
	}
	directoryBefore, err := os.Lstat(directory)
	if err != nil || !privateAgentVaultKeyEnrollmentDirectory(directoryBefore) {
		return ErrAgentVaultKeyRotationIntentUnsafe
	}
	file, err := os.CreateTemp(directory, ".rotation-reconcile-*.tmp")
	if err != nil {
		return ErrAgentVaultKeyRotationIntentStorage
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
		return ErrAgentVaultKeyRotationIntentStorage
	}
	temporaryInfo, err := file.Stat()
	if err != nil || !privateAgentVaultKeyEnrollmentFile(temporaryInfo) {
		return ErrAgentVaultKeyRotationIntentUnsafe
	}
	written, err := io.Copy(file, bytes.NewReader(raw))
	if err != nil || written != int64(len(raw)) {
		return ErrAgentVaultKeyRotationIntentStorage
	}
	if err := file.Sync(); err != nil {
		return ErrAgentVaultKeyRotationIntentStorage
	}
	if err := file.Close(); err != nil {
		return ErrAgentVaultKeyRotationIntentStorage
	}
	// Re-read immediately before the atomic rename. A competing reconciler may
	// only converge on the same authenticated canonical open rotation; an exact
	// mismatch fails closed rather than overwriting unrelated intent state.
	current, err := ReadAgentVaultKeyRotationIntent(account, realm, agent)
	if err != nil || *current != expected {
		return ErrAgentVaultKeyRotationIntentConflict
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return ErrAgentVaultKeyRotationIntentStorage
	}
	temporaryExists = false
	finalInfo, finalErr := os.Lstat(path)
	directoryAfter, directoryErr := os.Lstat(directory)
	if finalErr != nil || directoryErr != nil || !os.SameFile(temporaryInfo, finalInfo) ||
		!privateAgentVaultKeyEnrollmentFile(finalInfo) || !os.SameFile(directoryBefore, directoryAfter) ||
		!privateAgentVaultKeyEnrollmentDirectory(directoryAfter) {
		return ErrAgentVaultKeyRotationIntentUnsafe
	}
	if err := validateAgentVaultKeyEnrollmentDirectories(home, directory); err != nil {
		return err
	}
	if err := syncAgentVaultKeyEnrollmentDirectory(directory); err != nil {
		return ErrAgentVaultKeyRotationIntentStorage
	}
	return nil
}

func agentVaultKeyRotationIntentLocation(account, realm, agent string) (home, directory, path string, err error) {
	for _, value := range []string{account, realm, agent} {
		if !namePattern.MatchString(value) {
			return "", "", "", ErrAgentVaultKeyRotationIntentScope
		}
	}
	home, err = root()
	if err != nil {
		return "", "", "", ErrAgentVaultKeyRotationIntentStorage
	}
	directory = filepath.Join(home, "keys", "accounts", account, "realms", realm,
		"agents", agent, "rotation")
	path = filepath.Join(directory, agentVaultKeyRotationIntentFile)
	return home, directory, path, nil
}

func marshalAgentVaultKeyRotationIntent(account, realm, agent string, intent AgentVaultKeyRotationIntent) ([]byte, error) {
	if !validAgentVaultKeyRotationIntent(intent) {
		return nil, ErrAgentVaultKeyRotationIntentInvalid
	}
	body := agentVaultKeyRotationIntentBody{
		SchemaVersion: agentVaultKeyRotationIntentSchemaV1,
		RotationID:    intent.RotationID, AccountID: intent.AccountID, RealmID: intent.RealmID,
		OwnerAgentID: intent.OwnerAgentID, Source: intent.Source, Target: intent.Target,
		ExpectedSourceKeyRowVersion: intent.ExpectedSourceKeyRowVersion,
		StartIdempotencyKey:         intent.StartIdempotencyKey,
	}
	bodyRaw, err := json.Marshal(body)
	if err != nil {
		return nil, ErrAgentVaultKeyRotationIntentInvalid
	}
	defer clear(bodyRaw)
	record := agentVaultKeyRotationIntentRecord{
		SchemaVersion: body.SchemaVersion,
		RotationID:    body.RotationID, AccountID: body.AccountID, RealmID: body.RealmID,
		OwnerAgentID: body.OwnerAgentID, Source: body.Source, Target: body.Target,
		ExpectedSourceKeyRowVersion: body.ExpectedSourceKeyRowVersion,
		StartIdempotencyKey:         body.StartIdempotencyKey,
		Checksum:                    agentVaultKeyRotationIntentChecksum(account, realm, agent, bodyRaw),
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) == 0 || len(raw) > maxAgentVaultKeyRotationIntentBytes {
		clear(raw)
		return nil, ErrAgentVaultKeyRotationIntentInvalid
	}
	return raw, nil
}

func parseAgentVaultKeyRotationIntent(account, realm, agent string, raw []byte) (*AgentVaultKeyRotationIntent, error) {
	if len(raw) == 0 || len(raw) > maxAgentVaultKeyRotationIntentBytes || !json.Valid(raw) {
		return nil, ErrAgentVaultKeyRotationIntentInvalid
	}
	var record agentVaultKeyRotationIntentRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil || record.SchemaVersion != agentVaultKeyRotationIntentSchemaV1 ||
		!validAgentVaultKeyRotationIntentChecksum(record.Checksum) {
		return nil, ErrAgentVaultKeyRotationIntentInvalid
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(canonical, raw) {
		clear(canonical)
		return nil, ErrAgentVaultKeyRotationIntentInvalid
	}
	clear(canonical)
	body := agentVaultKeyRotationIntentBody{
		SchemaVersion: record.SchemaVersion,
		RotationID:    record.RotationID, AccountID: record.AccountID, RealmID: record.RealmID,
		OwnerAgentID: record.OwnerAgentID, Source: record.Source, Target: record.Target,
		ExpectedSourceKeyRowVersion: record.ExpectedSourceKeyRowVersion,
		StartIdempotencyKey:         record.StartIdempotencyKey,
	}
	bodyRaw, err := json.Marshal(body)
	if err != nil {
		return nil, ErrAgentVaultKeyRotationIntentInvalid
	}
	defer clear(bodyRaw)
	want := agentVaultKeyRotationIntentChecksum(account, realm, agent, bodyRaw)
	if len(want) != len(record.Checksum) || subtle.ConstantTimeCompare([]byte(want), []byte(record.Checksum)) != 1 {
		return nil, ErrAgentVaultKeyRotationIntentInvalid
	}
	intent := &AgentVaultKeyRotationIntent{
		RotationID: record.RotationID, AccountID: record.AccountID, RealmID: record.RealmID,
		OwnerAgentID: record.OwnerAgentID, Source: record.Source, Target: record.Target,
		ExpectedSourceKeyRowVersion: record.ExpectedSourceKeyRowVersion,
		StartIdempotencyKey:         record.StartIdempotencyKey,
	}
	if !validAgentVaultKeyRotationIntent(*intent) {
		return nil, ErrAgentVaultKeyRotationIntentInvalid
	}
	return intent, nil
}

func validAgentVaultKeyRotationIntent(intent AgentVaultKeyRotationIntent) bool {
	prepared := intent.ExpectedSourceKeyRowVersion > 0 && validLocalPrefixedID(intent.StartIdempotencyKey, "op")
	adopted := intent.ExpectedSourceKeyRowVersion == 0 && intent.StartIdempotencyKey == ""
	return validLocalPrefixedID(intent.RotationID, "vkr") &&
		validLocalPrefixedID(intent.AccountID, "acc") && validLocalPrefixedID(intent.RealmID, "realm") &&
		validLocalPrefixedID(intent.OwnerAgentID, "agent") &&
		validAgentVaultKeyEpoch(intent.Source.ID, intent.Source.Version) &&
		validAgentVaultKeyEpoch(intent.Target.ID, intent.Target.Version) &&
		intent.Source.ID != intent.Target.ID && intent.Source.Version < uint64(^uint64(0)>>1) &&
		intent.Target.Version == intent.Source.Version+1 &&
		intent.Source.Algorithm == sealed.AES256GCMAlgorithm &&
		intent.Target.Algorithm == sealed.AES256GCMAlgorithm &&
		validLocalSHA256(intent.Source.Fingerprint) && validLocalSHA256(intent.Target.Fingerprint) &&
		(prepared || adopted) && validRotationIntentString(intent.StartIdempotencyKey)
}

func validRotationIntentString(value string) bool {
	if len(value) > 512 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validLocalPrefixedID(value, prefix string) bool {
	body := strings.TrimPrefix(value, prefix+"_")
	if body == value || len(body) != 16 {
		return false
	}
	for _, character := range body {
		if (character < 'a' || character > 'z') && (character < '2' || character > '7') {
			return false
		}
	}
	return true
}

func validLocalSHA256(value string) bool {
	if len(value) != 2*sha256.Size || strings.ToLower(value) != value {
		return false
	}
	raw, err := hex.DecodeString(value)
	valid := err == nil && len(raw) == sha256.Size
	clear(raw)
	return valid
}

func agentVaultKeyRotationIntentChecksum(account, realm, agent string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(agentVaultKeyRotationIntentDomain))
	for _, value := range []string{account, realm, agent} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(value))
	}
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func validAgentVaultKeyRotationIntentChecksum(value string) bool {
	return validLocalSHA256(value)
}

func mapAgentVaultKeyRotationIntentStorageError(err error) error {
	switch {
	case errors.Is(err, ErrAgentVaultKeyRotationIntentUnavailable):
		return ErrAgentVaultKeyRotationIntentUnavailable
	case errors.Is(err, ErrAgentVaultKeyRotationIntentExists):
		return ErrAgentVaultKeyRotationIntentExists
	case errors.Is(err, ErrAgentVaultKeyRotationIntentConflict):
		return ErrAgentVaultKeyRotationIntentConflict
	case errors.Is(err, ErrAgentVaultKeyRotationIntentUnsafe):
		return ErrAgentVaultKeyRotationIntentUnsafe
	case errors.Is(err, ErrAgentVaultKeyRotationIntentInvalid):
		return ErrAgentVaultKeyRotationIntentInvalid
	case errors.Is(err, ErrAgentVaultKeyRotationIntentStorage):
		return ErrAgentVaultKeyRotationIntentStorage
	case errors.Is(err, ErrAgentVaultKeyRotationIntentScope):
		return ErrAgentVaultKeyRotationIntentScope
	case errors.Is(err, ErrAgentVaultKeyEnrollmentUnsafe):
		return ErrAgentVaultKeyRotationIntentUnsafe
	case errors.Is(err, ErrAgentVaultKeyEnrollmentInvalid):
		return ErrAgentVaultKeyRotationIntentInvalid
	case errors.Is(err, ErrAgentVaultKeyEnrollmentScope):
		return ErrAgentVaultKeyRotationIntentScope
	default:
		return ErrAgentVaultKeyRotationIntentStorage
	}
}
