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

	"golang.org/x/sys/unix"
)

// MaxSecretCreateJournalBytes bounds one serialized secret-create request.
// The server accepts up to one MiB of decoded mutation material; two MiB
// leaves room for JSON and base64 expansion without permitting unbounded
// local reads or writes.
const MaxSecretCreateJournalBytes = 2 * 1024 * 1024

var (
	// ErrSecretCreateJournalUnavailable means the selected journal entry does
	// not exist locally.
	ErrSecretCreateJournalUnavailable = errors.New("secret create journal entry is unavailable")

	// ErrSecretCreateJournalExists means exclusive publication found an
	// existing private regular entry. Existing bytes are never replaced.
	ErrSecretCreateJournalExists = errors.New("secret create journal entry already exists")

	// ErrSecretCreateJournalConflict means a compare-and-swap replacement did
	// not find the exact journal bytes its caller had authenticated. The current
	// entry is left untouched.
	ErrSecretCreateJournalConflict = errors.New("secret create journal entry changed")

	// ErrSecretCreateJournalUnsafe means a journal path is not an owner-only
	// regular file rooted below owner-only directories.
	ErrSecretCreateJournalUnsafe = errors.New("secret create journal storage is unsafe")

	// ErrSecretCreateJournalInvalid deliberately hides malformed request bytes
	// and invalid scoped hashes.
	ErrSecretCreateJournalInvalid = errors.New("secret create journal entry is invalid")

	// ErrSecretCreateJournalStorage deliberately hides OS paths and syscall
	// details.
	ErrSecretCreateJournalStorage = errors.New("secret create journal storage failed")

	// ErrSecretCreateJournalScope identifies invalid local selector components
	// without echoing them into an error.
	ErrSecretCreateJournalScope = errors.New("secret create journal scope is invalid")
)

var secretCreateJournalHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const secretCreateJournalLockFile = ".lock"

// SecretCreateJournalPath returns the canonical path for one serialized
// secret-create request. Scope components are local selectors; scopedHash is
// a lowercase hex digest computed by the caller over its authenticated scope
// and operation key.
func SecretCreateJournalPath(account, realm, agent, scopedHash string) (string, error) {
	_, path, err := secretCreateJournalLocation(account, realm, agent, scopedHash)
	return path, err
}

// ReadSecretCreateJournal reads one existing serialized secret-create request.
// It preserves the exact bytes written by CreateSecretCreateJournal.
func ReadSecretCreateJournal(account, realm, agent, scopedHash string) ([]byte, error) {
	home, path, err := secretCreateJournalLocation(account, realm, agent, scopedHash)
	if err != nil {
		return nil, err
	}
	directory := filepath.Dir(path)
	if err := validateSecretCreateJournalDirectories(home, directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrSecretCreateJournalUnavailable
		}
		if errors.Is(err, ErrSecretCreateJournalUnsafe) {
			return nil, ErrSecretCreateJournalUnsafe
		}
		return nil, ErrSecretCreateJournalStorage
	}
	directoryBefore, err := os.Lstat(directory)
	if err != nil || !privateSecretCreateJournalDirectory(directoryBefore) {
		return nil, ErrSecretCreateJournalUnsafe
	}

	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrSecretCreateJournalUnavailable
	}
	if err != nil {
		return nil, ErrSecretCreateJournalStorage
	}
	if !privateRegularSecretCreateJournalFile(before) {
		return nil, ErrSecretCreateJournalUnsafe
	}

	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrSecretCreateJournalUnavailable
	}
	if err != nil {
		return nil, ErrSecretCreateJournalStorage
	}
	defer func() { _ = file.Close() }()

	after, err := file.Stat()
	if err != nil {
		return nil, ErrSecretCreateJournalStorage
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || !os.SameFile(after, current) ||
		!privateRegularSecretCreateJournalFile(after) || !privateRegularSecretCreateJournalFile(current) {
		return nil, ErrSecretCreateJournalUnsafe
	}
	if after.Size() <= 0 || after.Size() > MaxSecretCreateJournalBytes {
		return nil, ErrSecretCreateJournalInvalid
	}

	raw, err := io.ReadAll(io.LimitReader(file, MaxSecretCreateJournalBytes+1))
	if err != nil {
		return nil, ErrSecretCreateJournalStorage
	}
	if !validSecretCreateJournalRaw(raw) {
		clear(raw)
		return nil, ErrSecretCreateJournalInvalid
	}

	finalFile, fileErr := file.Stat()
	finalPath, pathErr := os.Lstat(path)
	finalDirectory, directoryErr := os.Lstat(directory)
	if fileErr != nil || pathErr != nil || !os.SameFile(after, finalFile) || !os.SameFile(finalFile, finalPath) ||
		!privateRegularSecretCreateJournalFile(finalFile) || !privateRegularSecretCreateJournalFile(finalPath) ||
		directoryErr != nil || !os.SameFile(directoryBefore, finalDirectory) || !privateSecretCreateJournalDirectory(finalDirectory) {
		clear(raw)
		return nil, ErrSecretCreateJournalUnsafe
	}
	if err := validateSecretCreateJournalDirectories(home, directory); err != nil {
		clear(raw)
		if errors.Is(err, ErrSecretCreateJournalUnsafe) || errors.Is(err, os.ErrNotExist) {
			return nil, ErrSecretCreateJournalUnsafe
		}
		return nil, ErrSecretCreateJournalStorage
	}
	return raw, nil
}

// CreateSecretCreateJournal durably publishes one serialized secret-create
// request with no-overwrite semantics. A fully written temporary inode is
// hard-linked into place, so readers can never observe a partial request.
func CreateSecretCreateJournal(account, realm, agent, scopedHash string, raw []byte) error {
	home, path, err := secretCreateJournalLocation(account, realm, agent, scopedHash)
	if err != nil {
		return err
	}
	if !validSecretCreateJournalRaw(raw) {
		return ErrSecretCreateJournalInvalid
	}

	directory := filepath.Dir(path)
	if err := ensureSecretCreateJournalDirectories(home, directory); err != nil {
		return err
	}
	directoryBefore, err := os.Lstat(directory)
	if err != nil || !privateSecretCreateJournalDirectory(directoryBefore) {
		return ErrSecretCreateJournalUnsafe
	}
	if info, err := os.Lstat(path); err == nil {
		if !privateRegularSecretCreateJournalFile(info) {
			return ErrSecretCreateJournalUnsafe
		}
		return ErrSecretCreateJournalExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrSecretCreateJournalStorage
	}

	file, err := os.CreateTemp(directory, ".secret-create-*.tmp")
	if err != nil {
		return ErrSecretCreateJournalStorage
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
		return ErrSecretCreateJournalStorage
	}
	info, err := file.Stat()
	if err != nil || !privateRegularSecretCreateJournalFile(info) {
		return ErrSecretCreateJournalStorage
	}
	written, err := io.Copy(file, bytes.NewReader(raw))
	if err != nil || written != int64(len(raw)) {
		return ErrSecretCreateJournalStorage
	}
	if err := file.Sync(); err != nil {
		return ErrSecretCreateJournalStorage
	}
	if err := file.Close(); err != nil {
		return ErrSecretCreateJournalStorage
	}

	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return classifySecretCreateJournalCollision(path)
		}
		return ErrSecretCreateJournalStorage
	}
	published := true
	defer func() {
		if published {
			removeSecretCreateJournalIfSame(path, info)
		}
	}()

	publishedInfo, err := os.Lstat(path)
	currentDirectory, directoryErr := os.Lstat(directory)
	if err != nil || directoryErr != nil || !os.SameFile(info, publishedInfo) ||
		!privateRegularSecretCreateJournalFile(publishedInfo) ||
		!os.SameFile(directoryBefore, currentDirectory) || !privateSecretCreateJournalDirectory(currentDirectory) {
		return ErrSecretCreateJournalUnsafe
	}
	if err := validateSecretCreateJournalDirectories(home, directory); err != nil {
		if errors.Is(err, ErrSecretCreateJournalUnsafe) || errors.Is(err, os.ErrNotExist) {
			return ErrSecretCreateJournalUnsafe
		}
		return ErrSecretCreateJournalStorage
	}
	if err := os.Remove(temporaryPath); err != nil {
		return ErrSecretCreateJournalStorage
	}
	temporaryExists = false
	if err := syncSecretCreateJournalDirectory(directory); err != nil {
		return ErrSecretCreateJournalStorage
	}
	published = false
	return nil
}

// ReplaceSecretCreateJournalAfterVaultKeyAdvance durably replaces one exact
// serialized request after its wrapping-key epoch has advanced. The caller
// must supply the precise bytes it previously authenticated. A stable
// owner-only advisory lock serializes cooperating replacers, and the old
// request remains authoritative unless a complete replacement has been
// written and synced before the atomic rename.
func ReplaceSecretCreateJournalAfterVaultKeyAdvance(account, realm, agent, scopedHash string, expectedRaw, replacementRaw []byte) error {
	if !validSecretCreateJournalRaw(expectedRaw) || !validSecretCreateJournalRaw(replacementRaw) {
		return ErrSecretCreateJournalInvalid
	}
	home, path, err := secretCreateJournalLocation(account, realm, agent, scopedHash)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := validateSecretCreateJournalDirectories(home, directory); err != nil {
		return classifySecretCreateJournalDirectoryError(err)
	}
	lock, err := acquireSecretCreateJournalLock(home, directory)
	if err != nil {
		return err
	}
	defer lock.release()

	current, err := ReadSecretCreateJournal(account, realm, agent, scopedHash)
	if err != nil {
		return err
	}
	match := bytes.Equal(current, expectedRaw)
	clear(current)
	if !match {
		return ErrSecretCreateJournalConflict
	}

	directoryBefore, err := os.Lstat(directory)
	if err != nil || !privateSecretCreateJournalDirectory(directoryBefore) {
		return ErrSecretCreateJournalUnsafe
	}
	file, err := os.CreateTemp(directory, ".secret-create-replace-*.tmp")
	if err != nil {
		return ErrSecretCreateJournalStorage
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
		return ErrSecretCreateJournalStorage
	}
	temporaryInfo, err := file.Stat()
	if err != nil || !privateRegularSecretCreateJournalFile(temporaryInfo) {
		return ErrSecretCreateJournalUnsafe
	}
	written, err := io.Copy(file, bytes.NewReader(replacementRaw))
	if err != nil || written != int64(len(replacementRaw)) {
		return ErrSecretCreateJournalStorage
	}
	if err := file.Sync(); err != nil {
		return ErrSecretCreateJournalStorage
	}
	if err := file.Close(); err != nil {
		return ErrSecretCreateJournalStorage
	}

	// Re-read under the stable lock immediately before publication. This exact
	// fence also detects a non-cooperating local writer instead of overwriting
	// bytes the caller did not authenticate.
	current, err = ReadSecretCreateJournal(account, realm, agent, scopedHash)
	if err != nil {
		return err
	}
	match = bytes.Equal(current, expectedRaw)
	clear(current)
	if !match {
		return ErrSecretCreateJournalConflict
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return ErrSecretCreateJournalStorage
	}
	temporaryExists = false

	finalInfo, finalErr := os.Lstat(path)
	directoryAfter, directoryErr := os.Lstat(directory)
	if finalErr != nil || directoryErr != nil || !os.SameFile(temporaryInfo, finalInfo) ||
		!privateRegularSecretCreateJournalFile(finalInfo) || !os.SameFile(directoryBefore, directoryAfter) ||
		!privateSecretCreateJournalDirectory(directoryAfter) {
		return ErrSecretCreateJournalUnsafe
	}
	if err := validateSecretCreateJournalDirectories(home, directory); err != nil {
		return classifySecretCreateJournalDirectoryError(err)
	}
	if err := syncSecretCreateJournalDirectory(directory); err != nil {
		return ErrSecretCreateJournalStorage
	}
	return nil
}

type secretCreateJournalLock struct {
	file *os.File
}

func acquireSecretCreateJournalLock(home, directory string) (*secretCreateJournalLock, error) {
	if err := validateSecretCreateJournalDirectories(home, directory); err != nil {
		return nil, classifySecretCreateJournalDirectoryError(err)
	}
	path := filepath.Join(directory, secretCreateJournalLockFile)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_CREAT|unix.O_EXCL, 0o600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return nil, ErrSecretCreateJournalUnsafe
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, ErrSecretCreateJournalStorage
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
		!privateRegularSecretCreateJournalFile(opened) || !privateRegularSecretCreateJournalFile(linked) {
		return nil, ErrSecretCreateJournalUnsafe
	}
	if created {
		if err := file.Sync(); err != nil {
			return nil, ErrSecretCreateJournalStorage
		}
		if err := syncSecretCreateJournalDirectory(directory); err != nil {
			return nil, ErrSecretCreateJournalStorage
		}
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return nil, ErrSecretCreateJournalStorage
	}
	linked, linkErr = os.Lstat(path)
	if linkErr != nil || !os.SameFile(opened, linked) || !privateRegularSecretCreateJournalFile(linked) {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, ErrSecretCreateJournalUnsafe
	}
	if err := validateSecretCreateJournalDirectories(home, directory); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, classifySecretCreateJournalDirectoryError(err)
	}
	cleanup = false
	return &secretCreateJournalLock{file: file}, nil
}

func (lock *secretCreateJournalLock) release() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	_ = lock.file.Close()
	lock.file = nil
}

func classifySecretCreateJournalDirectoryError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ErrSecretCreateJournalUnavailable
	case errors.Is(err, ErrSecretCreateJournalUnsafe):
		return ErrSecretCreateJournalUnsafe
	default:
		return ErrSecretCreateJournalStorage
	}
}

func secretCreateJournalLocation(account, realm, agent, scopedHash string) (home, path string, err error) {
	for _, value := range []string{account, realm, agent} {
		if !namePattern.MatchString(value) {
			return "", "", ErrSecretCreateJournalScope
		}
	}
	if !secretCreateJournalHashPattern.MatchString(scopedHash) {
		return "", "", ErrSecretCreateJournalInvalid
	}
	home, err = root()
	if err != nil {
		return "", "", ErrSecretCreateJournalStorage
	}
	return home, filepath.Join(home, "journal", "accounts", account, "realms", realm,
		"agents", agent, "secret-create", scopedHash+".json"), nil
}

func validSecretCreateJournalRaw(raw []byte) bool {
	return len(raw) > 0 && len(raw) <= MaxSecretCreateJournalBytes && json.Valid(raw)
}

func ensureSecretCreateJournalDirectories(home, directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return ErrSecretCreateJournalStorage
	}
	for _, path := range secretCreateJournalDirectories(home, directory) {
		info, err := os.Lstat(path)
		if err != nil {
			return ErrSecretCreateJournalStorage
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrSecretCreateJournalUnsafe
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return ErrSecretCreateJournalStorage
		}
		info, err = os.Lstat(path)
		if err != nil || !privateSecretCreateJournalDirectory(info) {
			return ErrSecretCreateJournalUnsafe
		}
	}
	return nil
}

func validateSecretCreateJournalDirectories(home, directory string) error {
	for _, path := range secretCreateJournalDirectories(home, directory) {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !privateSecretCreateJournalDirectory(info) {
			return ErrSecretCreateJournalUnsafe
		}
	}
	return nil
}

func secretCreateJournalDirectories(home, directory string) []string {
	journalRoot := filepath.Join(home, "journal")
	relative, err := filepath.Rel(journalRoot, directory)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return []string{directory}
	}
	paths := []string{journalRoot}
	current := journalRoot
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		paths = append(paths, current)
	}
	return paths
}

func privateSecretCreateJournalDirectory(info os.FileInfo) bool {
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0o700
}

func privateRegularSecretCreateJournalFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0o600
}

func classifySecretCreateJournalCollision(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !privateRegularSecretCreateJournalFile(info) {
		return ErrSecretCreateJournalUnsafe
	}
	return ErrSecretCreateJournalExists
}

func removeSecretCreateJournalIfSame(path string, published os.FileInfo) {
	current, err := os.Lstat(path)
	if err == nil && os.SameFile(published, current) {
		_ = os.Remove(path)
	}
}

func syncSecretCreateJournalDirectory(directory string) error {
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
