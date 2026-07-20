package local

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/witwave-ai/witself/internal/sealed"
)

var (
	// ErrAgentVaultKeyRecoveryUnavailable means the selected recovery artifact
	// does not exist. It does not imply that the underlying AVK is unavailable.
	ErrAgentVaultKeyRecoveryUnavailable = errors.New("agent vault key recovery artifact is unavailable")

	// ErrAgentVaultKeyRecoveryExists means immutable publication found an
	// existing owner-only regular file. Existing bytes are never replaced.
	ErrAgentVaultKeyRecoveryExists = errors.New("agent vault key recovery artifact already exists")

	// ErrAgentVaultKeyRecoveryUnsafe means a path contains a symlink, an unsafe
	// file type, or permissions that could expose the recovery artifact.
	ErrAgentVaultKeyRecoveryUnsafe = errors.New("agent vault key recovery artifact storage is unsafe")

	// ErrAgentVaultKeyRecoveryInvalid deliberately hides malformed artifact
	// contents and public metadata mismatches.
	ErrAgentVaultKeyRecoveryInvalid = errors.New("agent vault key recovery artifact is invalid")

	// ErrAgentVaultKeyRecoveryStorage deliberately hides OS paths and syscall
	// details from errors that may be rendered by an AI client.
	ErrAgentVaultKeyRecoveryStorage = errors.New("agent vault key recovery artifact storage failed")

	// ErrAgentVaultKeyRecoveryScope identifies invalid local selectors, epoch
	// metadata, or an empty explicit path without echoing those values.
	ErrAgentVaultKeyRecoveryScope = errors.New("agent vault key recovery artifact scope is invalid")
)

// AgentVaultKeyRecoveryPath returns the immutable managed path for one exact
// AVK recovery epoch. Local selector names are not authenticated owner IDs;
// those stable IDs remain inside the artifact's authenticated recovery scope.
func AgentVaultKeyRecoveryPath(account, realm, agent, keyID string, keyVersion uint64) (string, error) {
	_, _, path, err := agentVaultKeyRecoveryLocation(account, realm, agent, keyID, keyVersion)
	return path, err
}

// CreateAgentVaultKeyRecovery validates and durably publishes one recovery
// artifact in managed local storage. The AVK epoch is derived from the
// artifact, and an existing path is never replaced.
func CreateAgentVaultKeyRecovery(account, realm, agent string, artifact []byte) (string, error) {
	metadata, err := inspectLocalRecoveryArtifact(artifact)
	if err != nil {
		return "", err
	}
	home, directory, path, err := agentVaultKeyRecoveryLocation(
		account, realm, agent, metadata.AVK.ID, metadata.AVK.Version,
	)
	if err != nil {
		return "", err
	}
	if err := ensureAgentVaultKeyRecoveryDirectories(home, directory); err != nil {
		return "", err
	}
	if err := publishAgentVaultKeyRecoveryArtifact(home, directory, path, artifact); err != nil {
		return "", err
	}
	return path, nil
}

// ReadAgentVaultKeyRecovery securely reads one exact managed recovery epoch.
// It rejects file/path metadata disagreement instead of accepting a copied or
// renamed artifact under a different epoch name.
func ReadAgentVaultKeyRecovery(account, realm, agent, keyID string, keyVersion uint64) ([]byte, sealed.AVKRecoveryMetadata, error) {
	home, directory, path, err := agentVaultKeyRecoveryLocation(account, realm, agent, keyID, keyVersion)
	if err != nil {
		return nil, sealed.AVKRecoveryMetadata{}, err
	}
	if err := validateAgentVaultKeyRecoveryDirectories(home, directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, sealed.AVKRecoveryMetadata{}, ErrAgentVaultKeyRecoveryUnavailable
		}
		return nil, sealed.AVKRecoveryMetadata{}, err
	}
	raw, err := readAgentVaultKeyRecoveryArtifact(home, directory, path)
	if err != nil {
		return nil, sealed.AVKRecoveryMetadata{}, err
	}
	metadata, err := inspectLocalRecoveryArtifact(raw)
	if err != nil || metadata.AVK.ID != keyID || metadata.AVK.Version != keyVersion {
		clear(raw)
		return nil, sealed.AVKRecoveryMetadata{}, ErrAgentVaultKeyRecoveryInvalid
	}
	return raw, metadata, nil
}

// WriteRecoveryArtifact validates and atomically writes a portable recovery
// artifact to an explicit user-selected path. The parent must already exist,
// every spelled parent component must be a real directory rather than a
// symlink, and an existing final path is never replaced. The parent need not
// be private (for example, ~/Downloads is supported), but the file is always
// created mode 0600.
func WriteRecoveryArtifact(path string, artifact []byte) error {
	if _, err := inspectLocalRecoveryArtifact(artifact); err != nil {
		return err
	}
	absPath, directory, directoryInfo, err := explicitAgentVaultKeyRecoveryLocation(path)
	if err != nil {
		return err
	}
	return publishExplicitAgentVaultKeyRecoveryArtifact(directory, absPath, directoryInfo, artifact)
}

// ReadRecoveryArtifact securely reads and validates a portable recovery
// artifact from an explicit path. It never follows a final or parent symlink
// and never performs passphrase derivation or decryption.
func ReadRecoveryArtifact(path string) ([]byte, sealed.AVKRecoveryMetadata, error) {
	absPath, directory, directoryInfo, err := explicitAgentVaultKeyRecoveryLocation(path)
	if err != nil {
		return nil, sealed.AVKRecoveryMetadata{}, err
	}
	raw, err := readExplicitAgentVaultKeyRecoveryArtifact(directory, absPath, directoryInfo)
	if err != nil {
		return nil, sealed.AVKRecoveryMetadata{}, err
	}
	metadata, err := inspectLocalRecoveryArtifact(raw)
	if err != nil {
		clear(raw)
		return nil, sealed.AVKRecoveryMetadata{}, err
	}
	return raw, metadata, nil
}

func agentVaultKeyRecoveryLocation(account, realm, agent, keyID string, keyVersion uint64) (home, directory, path string, err error) {
	for _, value := range []string{account, realm, agent} {
		if !namePattern.MatchString(value) {
			return "", "", "", ErrAgentVaultKeyRecoveryScope
		}
	}
	if !validAgentVaultKeyEpoch(keyID, keyVersion) {
		return "", "", "", ErrAgentVaultKeyRecoveryScope
	}
	home, err = root()
	if err != nil {
		return "", "", "", ErrAgentVaultKeyRecoveryStorage
	}
	directory = filepath.Join(home, "keys", "accounts", account, "realms", realm,
		"agents", agent, "recovery")
	path = filepath.Join(directory, strconv.FormatUint(keyVersion, 10)+"-"+keyID+".recovery")
	return home, directory, path, nil
}

func inspectLocalRecoveryArtifact(artifact []byte) (sealed.AVKRecoveryMetadata, error) {
	if len(artifact) == 0 || len(artifact) > sealed.MaxAVKRecoveryPackageBytes {
		return sealed.AVKRecoveryMetadata{}, ErrAgentVaultKeyRecoveryInvalid
	}
	metadata, err := sealed.InspectAgentVaultKeyRecovery(artifact)
	if err != nil || !validAgentVaultKeyEpoch(metadata.AVK.ID, metadata.AVK.Version) {
		return sealed.AVKRecoveryMetadata{}, ErrAgentVaultKeyRecoveryInvalid
	}
	return metadata, nil
}

func ensureAgentVaultKeyRecoveryDirectories(home, directory string) error {
	err := ensureAgentVaultKeyEnrollmentDirectories(home, directory)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrAgentVaultKeyEnrollmentUnsafe):
		return ErrAgentVaultKeyRecoveryUnsafe
	case errors.Is(err, ErrAgentVaultKeyEnrollmentScope):
		return ErrAgentVaultKeyRecoveryScope
	default:
		return ErrAgentVaultKeyRecoveryStorage
	}
}

func validateAgentVaultKeyRecoveryDirectories(home, directory string) error {
	err := validateAgentVaultKeyEnrollmentDirectories(home, directory)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrNotExist):
		return os.ErrNotExist
	case errors.Is(err, ErrAgentVaultKeyEnrollmentUnsafe):
		return ErrAgentVaultKeyRecoveryUnsafe
	default:
		return ErrAgentVaultKeyRecoveryStorage
	}
}

func publishAgentVaultKeyRecoveryArtifact(home, directory, path string, artifact []byte) error {
	if len(artifact) == 0 || len(artifact) > sealed.MaxAVKRecoveryPackageBytes {
		return ErrAgentVaultKeyRecoveryInvalid
	}
	if err := validateAgentVaultKeyRecoveryDirectories(home, directory); err != nil {
		return err
	}
	directoryBefore, err := os.Lstat(directory)
	if err != nil || !privateAgentVaultKeyEnrollmentDirectory(directoryBefore) {
		return ErrAgentVaultKeyRecoveryUnsafe
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		return classifyAgentVaultKeyRecoveryCollision(info)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return ErrAgentVaultKeyRecoveryStorage
	}

	temporary, err := os.CreateTemp(directory, ".recovery-write-*.tmp")
	if err != nil {
		return ErrAgentVaultKeyRecoveryStorage
	}
	temporaryPath := temporary.Name()
	temporaryExists := true
	defer func() {
		_ = temporary.Close()
		if temporaryExists {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := writeAndSyncRecoveryTemporary(temporary, artifact); err != nil {
		return err
	}
	temporaryInfo, err := os.Lstat(temporaryPath)
	if err != nil || !privateAgentVaultKeyEnrollmentFile(temporaryInfo) {
		return ErrAgentVaultKeyRecoveryStorage
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return classifyAgentVaultKeyRecoveryPathCollision(path)
		}
		return ErrAgentVaultKeyRecoveryStorage
	}
	published := true
	defer func() {
		if published {
			removeAgentVaultKeyEnrollmentIfSame(path, temporaryInfo)
		}
	}()
	finalInfo, finalErr := os.Lstat(path)
	directoryAfter, directoryErr := os.Lstat(directory)
	if finalErr != nil || directoryErr != nil || !os.SameFile(temporaryInfo, finalInfo) ||
		!privateAgentVaultKeyEnrollmentFile(finalInfo) || !os.SameFile(directoryBefore, directoryAfter) ||
		!privateAgentVaultKeyEnrollmentDirectory(directoryAfter) {
		return ErrAgentVaultKeyRecoveryUnsafe
	}
	if err := validateAgentVaultKeyRecoveryDirectories(home, directory); err != nil {
		return err
	}
	if err := syncAgentVaultKeyEnrollmentDirectory(directory); err != nil {
		return ErrAgentVaultKeyRecoveryStorage
	}
	if err := os.Remove(temporaryPath); err != nil {
		return ErrAgentVaultKeyRecoveryStorage
	}
	temporaryExists = false
	if err := syncAgentVaultKeyEnrollmentDirectory(directory); err != nil {
		return ErrAgentVaultKeyRecoveryStorage
	}
	published = false
	return nil
}

func publishExplicitAgentVaultKeyRecoveryArtifact(directory, path string, directoryBefore os.FileInfo, artifact []byte) error {
	if info, err := os.Lstat(path); err == nil {
		return classifyAgentVaultKeyRecoveryCollision(info)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrAgentVaultKeyRecoveryStorage
	}
	temporary, err := os.CreateTemp(directory, ".recovery-write-*.tmp")
	if err != nil {
		return ErrAgentVaultKeyRecoveryStorage
	}
	temporaryPath := temporary.Name()
	temporaryExists := true
	defer func() {
		_ = temporary.Close()
		if temporaryExists {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := writeAndSyncRecoveryTemporary(temporary, artifact); err != nil {
		return err
	}
	temporaryInfo, err := os.Lstat(temporaryPath)
	if err != nil || !privateAgentVaultKeyEnrollmentFile(temporaryInfo) {
		return ErrAgentVaultKeyRecoveryStorage
	}
	if err := validateExplicitAgentVaultKeyRecoveryDirectory(directory, directoryBefore); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return classifyAgentVaultKeyRecoveryPathCollision(path)
		}
		return ErrAgentVaultKeyRecoveryStorage
	}
	published := true
	defer func() {
		if published {
			removeAgentVaultKeyEnrollmentIfSame(path, temporaryInfo)
		}
	}()
	finalInfo, finalErr := os.Lstat(path)
	if finalErr != nil || !os.SameFile(temporaryInfo, finalInfo) ||
		!privateAgentVaultKeyEnrollmentFile(finalInfo) {
		return ErrAgentVaultKeyRecoveryUnsafe
	}
	if err := validateExplicitAgentVaultKeyRecoveryDirectory(directory, directoryBefore); err != nil {
		return err
	}
	if err := syncAgentVaultKeyEnrollmentDirectory(directory); err != nil {
		return ErrAgentVaultKeyRecoveryStorage
	}
	if err := os.Remove(temporaryPath); err != nil {
		return ErrAgentVaultKeyRecoveryStorage
	}
	temporaryExists = false
	if err := syncAgentVaultKeyEnrollmentDirectory(directory); err != nil {
		return ErrAgentVaultKeyRecoveryStorage
	}
	published = false
	return nil
}

func writeAndSyncRecoveryTemporary(file *os.File, artifact []byte) error {
	if err := file.Chmod(0o600); err != nil {
		return ErrAgentVaultKeyRecoveryStorage
	}
	info, err := file.Stat()
	if err != nil || !privateAgentVaultKeyEnrollmentFile(info) {
		return ErrAgentVaultKeyRecoveryStorage
	}
	written, err := io.Copy(file, bytes.NewReader(artifact))
	if err != nil || written != int64(len(artifact)) {
		return ErrAgentVaultKeyRecoveryStorage
	}
	if err := file.Sync(); err != nil {
		return ErrAgentVaultKeyRecoveryStorage
	}
	finalInfo, err := file.Stat()
	if err != nil || !privateAgentVaultKeyEnrollmentFile(finalInfo) || finalInfo.Size() != int64(len(artifact)) {
		return ErrAgentVaultKeyRecoveryStorage
	}
	if err := file.Close(); err != nil {
		return ErrAgentVaultKeyRecoveryStorage
	}
	return nil
}

func readAgentVaultKeyRecoveryArtifact(home, directory, path string) ([]byte, error) {
	directoryBefore, err := os.Lstat(directory)
	if err != nil || !privateAgentVaultKeyEnrollmentDirectory(directoryBefore) {
		return nil, ErrAgentVaultKeyRecoveryUnsafe
	}
	raw, err := readRecoveryArtifactFile(path)
	if err != nil {
		return nil, err
	}
	directoryAfter, directoryErr := os.Lstat(directory)
	if directoryErr != nil || !os.SameFile(directoryBefore, directoryAfter) ||
		!privateAgentVaultKeyEnrollmentDirectory(directoryAfter) {
		clear(raw)
		return nil, ErrAgentVaultKeyRecoveryUnsafe
	}
	if err := validateAgentVaultKeyRecoveryDirectories(home, directory); err != nil {
		clear(raw)
		return nil, err
	}
	return raw, nil
}

func readExplicitAgentVaultKeyRecoveryArtifact(directory, path string, directoryBefore os.FileInfo) ([]byte, error) {
	raw, err := readRecoveryArtifactFile(path)
	if err != nil {
		return nil, err
	}
	if err := validateExplicitAgentVaultKeyRecoveryDirectory(directory, directoryBefore); err != nil {
		clear(raw)
		return nil, err
	}
	return raw, nil
}

func readRecoveryArtifactFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrAgentVaultKeyRecoveryUnavailable
	}
	if err != nil {
		return nil, ErrAgentVaultKeyRecoveryStorage
	}
	if !privateAgentVaultKeyEnrollmentFile(before) {
		return nil, ErrAgentVaultKeyRecoveryUnsafe
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrAgentVaultKeyRecoveryUnavailable
	}
	if err != nil {
		return nil, ErrAgentVaultKeyRecoveryStorage
	}
	defer func() { _ = file.Close() }()
	after, statErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !os.SameFile(before, after) || !os.SameFile(after, current) ||
		!privateAgentVaultKeyEnrollmentFile(after) || !privateAgentVaultKeyEnrollmentFile(current) {
		return nil, ErrAgentVaultKeyRecoveryUnsafe
	}
	if after.Size() <= 0 || after.Size() > sealed.MaxAVKRecoveryPackageBytes {
		return nil, ErrAgentVaultKeyRecoveryInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(file, sealed.MaxAVKRecoveryPackageBytes+1))
	if err != nil {
		return nil, ErrAgentVaultKeyRecoveryStorage
	}
	if len(raw) == 0 || len(raw) > sealed.MaxAVKRecoveryPackageBytes {
		clear(raw)
		return nil, ErrAgentVaultKeyRecoveryInvalid
	}
	finalFile, fileErr := file.Stat()
	finalPath, finalPathErr := os.Lstat(path)
	if fileErr != nil || finalPathErr != nil || !os.SameFile(after, finalFile) ||
		!os.SameFile(finalFile, finalPath) || !privateAgentVaultKeyEnrollmentFile(finalFile) ||
		!privateAgentVaultKeyEnrollmentFile(finalPath) {
		clear(raw)
		return nil, ErrAgentVaultKeyRecoveryUnsafe
	}
	return raw, nil
}

func explicitAgentVaultKeyRecoveryLocation(path string) (absolute, directory string, directoryInfo os.FileInfo, err error) {
	if path == "" {
		return "", "", nil, ErrAgentVaultKeyRecoveryScope
	}
	absolute, err = filepath.Abs(path)
	if err != nil {
		return "", "", nil, ErrAgentVaultKeyRecoveryStorage
	}
	absolute = filepath.Clean(absolute)
	directory = filepath.Dir(absolute)
	if directory == absolute {
		return "", "", nil, ErrAgentVaultKeyRecoveryScope
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil, ErrAgentVaultKeyRecoveryUnavailable
	}
	if err != nil {
		return "", "", nil, ErrAgentVaultKeyRecoveryStorage
	}
	if filepath.Clean(resolved) != directory {
		return "", "", nil, ErrAgentVaultKeyRecoveryUnsafe
	}
	directoryInfo, err = os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil, ErrAgentVaultKeyRecoveryUnavailable
	}
	if err != nil {
		return "", "", nil, ErrAgentVaultKeyRecoveryStorage
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", nil, ErrAgentVaultKeyRecoveryUnsafe
	}
	return absolute, directory, directoryInfo, nil
}

func validateExplicitAgentVaultKeyRecoveryDirectory(directory string, before os.FileInfo) error {
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return ErrAgentVaultKeyRecoveryUnsafe
	}
	current, err := os.Lstat(directory)
	if err != nil || filepath.Clean(resolved) != directory || !current.IsDir() ||
		current.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, current) {
		return ErrAgentVaultKeyRecoveryUnsafe
	}
	return nil
}

func classifyAgentVaultKeyRecoveryCollision(info os.FileInfo) error {
	if !privateAgentVaultKeyEnrollmentFile(info) {
		return ErrAgentVaultKeyRecoveryUnsafe
	}
	return ErrAgentVaultKeyRecoveryExists
}

func classifyAgentVaultKeyRecoveryPathCollision(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return ErrAgentVaultKeyRecoveryUnsafe
	}
	return classifyAgentVaultKeyRecoveryCollision(info)
}
