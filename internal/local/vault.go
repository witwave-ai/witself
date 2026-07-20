package local

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/witwave-ai/witself/internal/sealed"
)

const maxAgentVaultKeyFileBytes = 4 * 1024

var (
	// ErrAgentVaultKeyUnavailable means no local AVK exists for the selected
	// agent. Callers must reconcile the authenticated server binding before
	// deciding whether generation is allowed.
	ErrAgentVaultKeyUnavailable = errors.New("agent vault key is unavailable")

	// ErrAgentVaultKeyExists means exclusive creation found an existing regular
	// key file. Existing key material is never replaced.
	ErrAgentVaultKeyExists = errors.New("agent vault key already exists")

	// ErrAgentVaultKeyUnsafe means the local path is not an owner-only regular
	// key file rooted below owner-only directories.
	ErrAgentVaultKeyUnsafe = errors.New("agent vault key storage is unsafe")

	// ErrAgentVaultKeyInvalid deliberately hides malformed record contents and
	// codec details.
	ErrAgentVaultKeyInvalid = errors.New("agent vault key file is invalid")

	// ErrAgentVaultKeyStorage deliberately hides OS paths and syscall details.
	ErrAgentVaultKeyStorage = errors.New("agent vault key storage failed")

	// ErrAgentVaultKeyScope identifies invalid local selector components without
	// echoing them into an error.
	ErrAgentVaultKeyScope = errors.New("agent vault key scope is invalid")
)

// AgentVaultKeyPath returns the canonical client-custody path for one named
// agent. Scope components are local selectors, not authenticated identities.
func AgentVaultKeyPath(account, realm, agent string) (string, error) {
	_, path, err := agentVaultKeyLocation(account, realm, agent)
	return path, err
}

// AgentVaultKeyEpochPath returns the immutable path for one exact AVK epoch.
// The legacy AgentVaultKeyPath remains stable for v1 callers; new callers that
// have reconciled the backend's current public binding should use this path so
// multiple key epochs can coexist without replacement.
func AgentVaultKeyEpochPath(account, realm, agent, keyID string, keyVersion uint64) (string, error) {
	_, legacyPath, err := agentVaultKeyLocation(account, realm, agent)
	if err != nil {
		return "", err
	}
	if !validAgentVaultKeyEpoch(keyID, keyVersion) {
		return "", ErrAgentVaultKeyScope
	}
	return filepath.Join(
		filepath.Dir(legacyPath), agent, "epochs",
		strconv.FormatUint(keyVersion, 10)+"-"+keyID+".key",
	), nil
}

// ReadAgentVaultKey reads one existing, private AVK record. It never generates
// or replaces a key. Callers own server-binding reconciliation.
func ReadAgentVaultKey(account, realm, agent string) (*sealed.AgentVaultKey, error) {
	home, path, err := agentVaultKeyLocation(account, realm, agent)
	if err != nil {
		return nil, err
	}
	return readAgentVaultKeyFile(home, path)
}

// ReadAgentVaultKeyEpoch reads the exact AVK selected by a reconciled backend
// binding. The immutable epoch path wins. When it is absent, a matching v1
// legacy record is accepted in place without copying, deleting, or rewriting
// that record. A malformed or unsafe epoch path never falls back.
func ReadAgentVaultKeyEpoch(account, realm, agent, keyID string, keyVersion uint64) (*sealed.AgentVaultKey, error) {
	home, _, err := agentVaultKeyLocation(account, realm, agent)
	if err != nil {
		return nil, err
	}
	path, err := AgentVaultKeyEpochPath(account, realm, agent, keyID, keyVersion)
	if err != nil {
		return nil, err
	}
	key, err := readAgentVaultKeyFile(home, path)
	if err == nil {
		if key.ID() != keyID || key.Version() != keyVersion {
			key.Clear()
			return nil, ErrAgentVaultKeyInvalid
		}
		return key, nil
	}
	if !errors.Is(err, ErrAgentVaultKeyUnavailable) {
		return nil, err
	}

	legacy, legacyErr := ReadAgentVaultKey(account, realm, agent)
	if legacyErr != nil {
		return nil, legacyErr
	}
	if legacy.ID() != keyID || legacy.Version() != keyVersion {
		legacy.Clear()
		return nil, ErrAgentVaultKeyUnavailable
	}
	return legacy, nil
}

func readAgentVaultKeyFile(home, path string) (*sealed.AgentVaultKey, error) {
	if err := validateAgentVaultKeyDirectories(home, filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrAgentVaultKeyUnavailable
		}
		if errors.Is(err, ErrAgentVaultKeyUnsafe) {
			return nil, ErrAgentVaultKeyUnsafe
		}
		return nil, ErrAgentVaultKeyStorage
	}

	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrAgentVaultKeyUnavailable
	}
	if err != nil {
		return nil, ErrAgentVaultKeyStorage
	}
	if !privateRegularAgentVaultKeyFile(before) {
		return nil, ErrAgentVaultKeyUnsafe
	}

	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrAgentVaultKeyUnavailable
	}
	if err != nil {
		return nil, ErrAgentVaultKeyStorage
	}
	defer func() { _ = file.Close() }()

	after, err := file.Stat()
	if err != nil {
		return nil, ErrAgentVaultKeyStorage
	}
	current, err := os.Lstat(path)
	if err != nil {
		return nil, ErrAgentVaultKeyUnsafe
	}
	// Lstat followed by fstat prevents a final-path symlink or replacement race
	// from being read after validation.
	if !os.SameFile(before, after) || !os.SameFile(after, current) ||
		!privateRegularAgentVaultKeyFile(after) || !privateRegularAgentVaultKeyFile(current) {
		return nil, ErrAgentVaultKeyUnsafe
	}
	if after.Size() <= 0 || after.Size() > maxAgentVaultKeyFileBytes {
		return nil, ErrAgentVaultKeyInvalid
	}

	raw, err := io.ReadAll(io.LimitReader(file, maxAgentVaultKeyFileBytes+1))
	if err != nil {
		return nil, ErrAgentVaultKeyStorage
	}
	defer clear(raw)
	if len(raw) == 0 || len(raw) > maxAgentVaultKeyFileBytes {
		return nil, ErrAgentVaultKeyInvalid
	}
	finalFile, fileErr := file.Stat()
	finalPath, pathErr := os.Lstat(path)
	if fileErr != nil || pathErr != nil || !os.SameFile(after, finalFile) || !os.SameFile(finalFile, finalPath) ||
		!privateRegularAgentVaultKeyFile(finalFile) || !privateRegularAgentVaultKeyFile(finalPath) {
		return nil, ErrAgentVaultKeyUnsafe
	}
	key, err := sealed.ParseAgentVaultKey(raw)
	if err != nil {
		return nil, ErrAgentVaultKeyInvalid
	}
	return key, nil
}

// CreateAgentVaultKey durably creates one private AVK record with O_EXCL
// semantics. It never generates a key and never loads or replaces an existing
// one; callers must reconcile server binding state before calling it.
func CreateAgentVaultKey(account, realm, agent string, key *sealed.AgentVaultKey) error {
	home, path, err := agentVaultKeyLocation(account, realm, agent)
	if err != nil {
		return err
	}
	return createAgentVaultKeyFile(home, path, key)
}

// CreateAgentVaultKeyEpoch durably publishes one exact immutable AVK epoch. A
// safe legacy record, including an exact match used for crash-safe bootstrap
// staging, remains untouched while the canonical no-replace epoch is added.
// A different legacy epoch may coexist with the new canonical record.
func CreateAgentVaultKeyEpoch(account, realm, agent string, key *sealed.AgentVaultKey) error {
	if key == nil {
		return ErrAgentVaultKeyInvalid
	}
	home, _, err := agentVaultKeyLocation(account, realm, agent)
	if err != nil {
		return err
	}
	path, err := AgentVaultKeyEpochPath(account, realm, agent, key.ID(), key.Version())
	if err != nil {
		return err
	}

	legacy, legacyErr := ReadAgentVaultKey(account, realm, agent)
	switch {
	case legacyErr == nil:
		legacy.Clear()
	case errors.Is(legacyErr, ErrAgentVaultKeyUnavailable):
		// No legacy record participates in this epoch.
	default:
		// Unsafe or malformed legacy custody state is never ignored while adding
		// another private key beneath the same agent scope.
		return legacyErr
	}
	return createAgentVaultKeyFile(home, path, key)
}

func createAgentVaultKeyFile(home, path string, key *sealed.AgentVaultKey) error {
	raw, err := sealed.EncodeAgentVaultKey(key)
	if err != nil || len(raw) == 0 || len(raw) > maxAgentVaultKeyFileBytes {
		clear(raw)
		return ErrAgentVaultKeyInvalid
	}
	defer clear(raw)

	directory := filepath.Dir(path)
	if err := ensureAgentVaultKeyDirectories(home, directory); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !privateRegularAgentVaultKeyFile(info) {
			return ErrAgentVaultKeyUnsafe
		}
		return ErrAgentVaultKeyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrAgentVaultKeyStorage
	}

	// Build and sync the complete record under an unguessable owner-only name,
	// then publish it with a same-directory hard link. Link is an atomic,
	// no-replace operation: the canonical name is never visible with a partial
	// record, and concurrent creators cannot replace one another.
	file, err := os.CreateTemp(directory, ".avk-write-*.tmp")
	if err != nil {
		return ErrAgentVaultKeyStorage
	}
	temporaryPath := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(temporaryPath)
	}()

	if err := file.Chmod(0o600); err != nil {
		return ErrAgentVaultKeyStorage
	}
	info, err := file.Stat()
	if err != nil || !privateRegularAgentVaultKeyFile(info) {
		return ErrAgentVaultKeyStorage
	}
	written, err := io.Copy(file, bytes.NewReader(raw))
	if err != nil || written != int64(len(raw)) {
		return ErrAgentVaultKeyStorage
	}
	if err := file.Sync(); err != nil {
		return ErrAgentVaultKeyStorage
	}
	if err := file.Close(); err != nil {
		return ErrAgentVaultKeyStorage
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return classifyAgentVaultKeyCollision(path)
		}
		return ErrAgentVaultKeyStorage
	}
	temporaryInfo, temporaryErr := os.Lstat(temporaryPath)
	finalInfo, finalErr := os.Lstat(path)
	if temporaryErr != nil || finalErr != nil || !os.SameFile(temporaryInfo, finalInfo) ||
		!privateRegularAgentVaultKeyFile(temporaryInfo) || !privateRegularAgentVaultKeyFile(finalInfo) {
		return ErrAgentVaultKeyStorage
	}
	// First make the completed canonical link durable. Only then remove the
	// temporary link and sync that directory change. A crash at any point leaves
	// either no canonical file or one complete, parseable record.
	if err := syncAgentVaultKeyDirectory(directory); err != nil {
		return ErrAgentVaultKeyStorage
	}
	if err := os.Remove(temporaryPath); err != nil {
		return ErrAgentVaultKeyStorage
	}
	if err := syncAgentVaultKeyDirectory(directory); err != nil {
		return ErrAgentVaultKeyStorage
	}
	return nil
}

func agentVaultKeyLocation(account, realm, agent string) (home, path string, err error) {
	for _, value := range []string{account, realm, agent} {
		if !namePattern.MatchString(value) {
			return "", "", ErrAgentVaultKeyScope
		}
	}
	home, err = root()
	if err != nil {
		return "", "", ErrAgentVaultKeyStorage
	}
	return home, filepath.Join(home, "keys", "accounts", account, "realms", realm, "agents", agent+".key"), nil
}

func validAgentVaultKeyEpoch(keyID string, keyVersion uint64) bool {
	if keyVersion == 0 || len(keyID) != len("avk_")+16 || !strings.HasPrefix(keyID, "avk_") {
		return false
	}
	for _, character := range keyID[len("avk_"):] {
		if (character < 'a' || character > 'z') && (character < '2' || character > '7') {
			return false
		}
	}
	return true
}

func ensureAgentVaultKeyDirectories(home, directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return ErrAgentVaultKeyStorage
	}
	for _, path := range agentVaultKeyDirectories(home, directory) {
		info, err := os.Lstat(path)
		if err != nil {
			return ErrAgentVaultKeyStorage
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrAgentVaultKeyUnsafe
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return ErrAgentVaultKeyStorage
		}
		info, err = os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			return ErrAgentVaultKeyUnsafe
		}
	}
	return nil
}

func validateAgentVaultKeyDirectories(home, directory string) error {
	for _, path := range agentVaultKeyDirectories(home, directory) {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			return ErrAgentVaultKeyUnsafe
		}
	}
	return nil
}

func agentVaultKeyDirectories(home, directory string) []string {
	root := filepath.Join(home, "keys")
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return []string{directory}
	}
	paths := []string{root}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		paths = append(paths, current)
	}
	return paths
}

func privateRegularAgentVaultKeyFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0o600
}

func classifyAgentVaultKeyCollision(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !privateRegularAgentVaultKeyFile(info) {
		return ErrAgentVaultKeyUnsafe
	}
	return ErrAgentVaultKeyExists
}

func syncAgentVaultKeyDirectory(directory string) error {
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
