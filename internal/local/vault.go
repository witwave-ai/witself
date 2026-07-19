package local

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
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

// ReadAgentVaultKey reads one existing, private AVK record. It never generates
// or replaces a key. Callers own server-binding reconciliation.
func ReadAgentVaultKey(account, realm, agent string) (*sealed.AgentVaultKey, error) {
	home, path, err := agentVaultKeyLocation(account, realm, agent)
	if err != nil {
		return nil, err
	}
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

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return classifyAgentVaultKeyCollision(path)
		}
		return ErrAgentVaultKeyStorage
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(path)
		}
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
	if err := syncAgentVaultKeyDirectory(directory); err != nil {
		return ErrAgentVaultKeyStorage
	}
	keep = true
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
