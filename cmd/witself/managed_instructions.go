package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// managedInstructionsSpec describes one marker-delimited instruction block in
// a runtime-owned Markdown file. The filename and temporary-file pattern are
// kept explicit so diagnostics and atomic writes use the runtime's own names.
type managedInstructionsSpec struct {
	path        string
	fileName    string
	tempPattern string
	beginMarker string
	endMarker   string
	block       []byte

	// removeEmpty is for dedicated Witself-owned files. Shared instruction
	// files such as AGENTS.md retain an empty file after removing the block.
	removeEmpty bool
}

type managedInstructionsSnapshot struct {
	path             string
	sourcePath       string
	data             []byte
	mode             fs.FileMode
	existed          bool
	fileName         string
	tempPattern      string
	viaSymlink       bool
	sourceInfo       fs.FileInfo
	sourceLinkTarget string
	originalInfo     fs.FileInfo
	mutated          bool
	expected         []byte
	expectedInfo     fs.FileInfo
	expectedSet      bool
}

func installManagedInstructions(spec managedInstructionsSpec) (managedInstructionsSnapshot, error) {
	spec, err := normalizeManagedInstructionsSpec(spec)
	if err != nil {
		return managedInstructionsSnapshot{}, err
	}
	snapshot, err := readManagedInstructionsSnapshot(spec)
	if err != nil {
		return managedInstructionsSnapshot{}, err
	}
	updated, changed, err := upsertManagedInstructionsBlock(snapshot.data, spec)
	if err != nil {
		return managedInstructionsSnapshot{}, err
	}
	if !changed {
		return snapshot, nil
	}
	mode := snapshot.mode
	if !snapshot.existed {
		mode = 0o600
	}
	expectedInfo, err := writeManagedInstructionsFile(
		snapshot.path,
		updated,
		mode,
		spec.fileName,
		spec.tempPattern,
		snapshot.verifyOriginalState,
		snapshot.verifySourceSymlink,
		snapshot.data,
		snapshot.originalInfo,
		snapshot.existed,
	)
	if err != nil {
		return managedInstructionsSnapshot{}, err
	}
	snapshot.mutated = true
	snapshot.expected = bytes.Clone(updated)
	snapshot.expectedInfo = expectedInfo
	snapshot.expectedSet = true
	return snapshot, nil
}

func removeManagedInstructions(spec managedInstructionsSpec) (managedInstructionsSnapshot, error) {
	spec, err := normalizeManagedInstructionsSpec(spec)
	if err != nil {
		return managedInstructionsSnapshot{}, err
	}
	snapshot, err := readManagedInstructionsSnapshot(spec)
	if err != nil {
		return managedInstructionsSnapshot{}, err
	}
	if !snapshot.existed {
		return snapshot, nil
	}
	updated, changed, err := removeManagedInstructionsBlock(snapshot.data, spec)
	if err != nil {
		return managedInstructionsSnapshot{}, err
	}
	if !changed {
		return snapshot, nil
	}
	// A symlink is user-owned structure. Keep its resolved target present rather
	// than turning the link into a dangling one, even for a dedicated file.
	if spec.removeEmpty && len(updated) == 0 && !snapshot.viaSymlink {
		if err := removeManagedInstructionsFile(
			snapshot.path,
			spec.fileName,
			spec.tempPattern,
			snapshot.verifyOriginalState,
			snapshot.data,
			snapshot.originalInfo,
		); err != nil {
			return managedInstructionsSnapshot{}, err
		}
		snapshot.mutated = true
		return snapshot, nil
	}
	expectedInfo, err := writeManagedInstructionsFile(
		snapshot.path,
		updated,
		snapshot.mode,
		spec.fileName,
		spec.tempPattern,
		snapshot.verifyOriginalState,
		snapshot.verifySourceSymlink,
		snapshot.data,
		snapshot.originalInfo,
		true,
	)
	if err != nil {
		return managedInstructionsSnapshot{}, err
	}
	snapshot.mutated = true
	snapshot.expected = bytes.Clone(updated)
	snapshot.expectedInfo = expectedInfo
	snapshot.expectedSet = true
	return snapshot, nil
}

func (snapshot managedInstructionsSnapshot) restore() error {
	if snapshot.path == "" || !snapshot.mutated {
		return nil
	}
	if !snapshot.existed {
		return removeManagedInstructionsFile(
			snapshot.path,
			snapshot.fileName,
			snapshot.tempPattern,
			snapshot.verifyExpectedState,
			snapshot.expected,
			snapshot.expectedInfo,
		)
	}
	fileName := snapshot.fileName
	if fileName == "" {
		fileName = filepath.Base(snapshot.path)
	}
	tempPattern := snapshot.tempPattern
	if tempPattern == "" {
		tempPattern = "." + fileName + ".witself-*"
	}
	_, err := writeManagedInstructionsFile(
		snapshot.path,
		snapshot.data,
		snapshot.mode,
		fileName,
		tempPattern,
		snapshot.verifyExpectedState,
		snapshot.verifySourceSymlink,
		snapshot.expected,
		snapshot.expectedInfo,
		snapshot.expectedSet,
	)
	return err
}

func (snapshot managedInstructionsSnapshot) verifyExpectedState() error {
	if err := snapshot.verifySourceSymlink(); err != nil {
		return fmt.Errorf("refuse to roll back %s because its symlink changed after Witself updated it: %w", snapshot.sourcePath, err)
	}
	if !snapshot.expectedSet {
		if _, err := os.Lstat(snapshot.path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return fmt.Errorf("inspect %s before rollback: %w", snapshot.path, err)
		}
		return fmt.Errorf("refuse to roll back %s because it was recreated after Witself removed it", snapshot.path)
	}
	err := verifyManagedInstructionsFileState(snapshot.path, snapshot.expected, snapshot.expectedInfo)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("refuse to roll back %s because it was removed after Witself updated it", snapshot.path)
	}
	if err != nil {
		return fmt.Errorf("refuse to roll back %s because it changed after Witself updated it: %w", snapshot.path, err)
	}
	return nil
}

func (snapshot managedInstructionsSnapshot) verifyOriginalState() error {
	if err := snapshot.verifySourceSymlink(); err != nil {
		return fmt.Errorf("refuse to update %s because its symlink changed after Witself read it: %w", snapshot.sourcePath, err)
	}
	if !snapshot.existed {
		if _, err := os.Lstat(snapshot.path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return fmt.Errorf("inspect %s before update: %w", snapshot.path, err)
		}
		return fmt.Errorf("refuse to update %s because it was created after Witself read it", snapshot.path)
	}
	if err := verifyManagedInstructionsFileState(snapshot.path, snapshot.data, snapshot.originalInfo); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("refuse to update %s because it was removed after Witself read it", snapshot.path)
		}
		return fmt.Errorf("refuse to update %s because it changed after Witself read it: %w", snapshot.path, err)
	}
	return nil
}

func (snapshot managedInstructionsSnapshot) verifySourceSymlink() error {
	if !snapshot.viaSymlink {
		return nil
	}
	info, err := os.Lstat(snapshot.sourcePath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 || !sameManagedInstructionsFileIdentity(info, snapshot.sourceInfo) {
		return errors.New("path is no longer the original symlink")
	}
	target, err := os.Readlink(snapshot.sourcePath)
	if err != nil {
		return err
	}
	if target != snapshot.sourceLinkTarget {
		return errors.New("symlink target changed")
	}
	resolved, err := filepath.EvalSymlinks(snapshot.sourcePath)
	if err != nil {
		return err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return err
	}
	if resolved != snapshot.path {
		return fmt.Errorf("symlink now resolves to %s instead of %s", resolved, snapshot.path)
	}
	return nil
}

func verifyManagedInstructionsFileState(path string, expected []byte, expectedInfo fs.FileInfo) error {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("%s is no longer a regular file", path)
	}
	if !sameManagedInstructionsFileIdentity(before, expectedInfo) {
		return fmt.Errorf("%s was replaced", path)
	}
	if before.Mode().Perm() != expectedInfo.Mode().Perm() {
		return fmt.Errorf("%s permissions changed from %04o to %04o", path, expectedInfo.Mode().Perm(), before.Mode().Perm())
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !sameManagedInstructionsFileIdentity(before, opened) {
		return fmt.Errorf("%s changed while it was opened", path)
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !after.Mode().IsRegular() || !sameManagedInstructionsFileIdentity(before, after) {
		return fmt.Errorf("%s changed while it was read", path)
	}
	if after.Mode().Perm() != expectedInfo.Mode().Perm() {
		return fmt.Errorf("%s permissions changed while it was read", path)
	}
	if !bytes.Equal(raw, expected) {
		return fmt.Errorf("%s contents changed", path)
	}
	return nil
}

func normalizeManagedInstructionsSpec(spec managedInstructionsSpec) (managedInstructionsSpec, error) {
	if strings.TrimSpace(spec.path) == "" {
		return managedInstructionsSpec{}, errors.New("managed instructions path is empty")
	}
	if spec.fileName == "" {
		spec.fileName = filepath.Base(spec.path)
	}
	if spec.tempPattern == "" {
		spec.tempPattern = "." + spec.fileName + ".witself-*"
	}
	if spec.beginMarker == "" || spec.endMarker == "" {
		return managedInstructionsSpec{}, fmt.Errorf("%s managed instruction markers must not be empty", spec.fileName)
	}
	if spec.beginMarker == spec.endMarker {
		return managedInstructionsSpec{}, fmt.Errorf("%s managed instruction markers must be distinct", spec.fileName)
	}
	begin := []byte(spec.beginMarker)
	end := []byte(spec.endMarker)
	if !bytes.HasPrefix(spec.block, begin) || !bytes.HasSuffix(spec.block, end) ||
		bytes.Count(spec.block, begin) != 1 || bytes.Count(spec.block, end) != 1 {
		return managedInstructionsSpec{}, fmt.Errorf("%s managed instruction block must contain exactly one matching marker pair", spec.fileName)
	}
	return spec, nil
}

func readManagedInstructionsSnapshot(spec managedInstructionsSpec) (managedInstructionsSnapshot, error) {
	snapshot := managedInstructionsSnapshot{
		path:        spec.path,
		sourcePath:  spec.path,
		mode:        0o600,
		fileName:    spec.fileName,
		tempPattern: spec.tempPattern,
	}
	info, err := os.Lstat(spec.path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return managedInstructionsSnapshot{}, fmt.Errorf("inspect %s: %w", spec.path, err)
	}
	path := spec.path
	if info.Mode()&os.ModeSymlink != 0 {
		snapshot.sourceInfo = info
		snapshot.sourceLinkTarget, err = os.Readlink(path)
		if err != nil {
			return managedInstructionsSnapshot{}, fmt.Errorf("read link %s: %w", path, err)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return managedInstructionsSnapshot{}, fmt.Errorf("resolve %s: %w", path, err)
		}
		path, err = filepath.Abs(resolved)
		if err != nil {
			return managedInstructionsSnapshot{}, fmt.Errorf("resolve %s: %w", resolved, err)
		}
		info, err = os.Stat(path)
		if err != nil {
			return managedInstructionsSnapshot{}, fmt.Errorf("inspect %s: %w", path, err)
		}
		snapshot.path = path
		snapshot.viaSymlink = true
	}
	if !info.Mode().IsRegular() {
		return managedInstructionsSnapshot{}, fmt.Errorf("%s is not a regular file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return managedInstructionsSnapshot{}, fmt.Errorf("read %s: %w", path, err)
	}
	snapshot.data = raw
	snapshot.mode = info.Mode()
	snapshot.existed = true
	snapshot.originalInfo = info
	return snapshot, nil
}

func upsertManagedInstructionsBlock(raw []byte, spec managedInstructionsSpec) ([]byte, bool, error) {
	start, end, found, err := managedInstructionsBlockRange(raw, spec)
	if err != nil {
		return nil, false, err
	}
	if found {
		if bytes.Equal(raw[start:end], spec.block) {
			return raw, false, nil
		}
		updated := make([]byte, 0, len(raw)-(end-start)+len(spec.block))
		updated = append(updated, raw[:start]...)
		updated = append(updated, spec.block...)
		updated = append(updated, raw[end:]...)
		return updated, true, nil
	}
	updated := make([]byte, 0, len(spec.block)+2+len(raw))
	updated = append(updated, spec.block...)
	if len(raw) == 0 {
		updated = append(updated, '\n')
	} else {
		updated = append(updated, '\n', '\n')
		updated = append(updated, raw...)
	}
	return updated, true, nil
}

func removeManagedInstructionsBlock(raw []byte, spec managedInstructionsSpec) ([]byte, bool, error) {
	start, end, found, err := managedInstructionsBlockRange(raw, spec)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return raw, false, nil
	}
	// The installer owns the separator immediately following its managed block.
	// Removing it restores unrelated content byte-for-byte for prefix installs.
	if bytes.HasPrefix(raw[end:], []byte("\n\n")) {
		end += 2
	} else if bytes.HasPrefix(raw[end:], []byte("\n")) {
		end++
	}
	updated := make([]byte, 0, len(raw)-(end-start))
	updated = append(updated, raw[:start]...)
	updated = append(updated, raw[end:]...)
	return updated, true, nil
}

func managedInstructionsBlockRange(raw []byte, spec managedInstructionsSpec) (int, int, bool, error) {
	begin := []byte(spec.beginMarker)
	endMarker := []byte(spec.endMarker)
	beginCount := bytes.Count(raw, begin)
	endCount := bytes.Count(raw, endMarker)
	if beginCount == 0 && endCount == 0 {
		return 0, 0, false, nil
	}
	if beginCount > 1 || endCount > 1 {
		return 0, 0, false, fmt.Errorf("%s contains multiple Witself managed memory routing markers", spec.fileName)
	}
	start := bytes.Index(raw, begin)
	endStart := bytes.Index(raw, endMarker)
	if start == -1 || endStart == -1 || endStart < start {
		return 0, 0, false, fmt.Errorf("%s contains an incomplete Witself managed memory routing block", spec.fileName)
	}
	return start, endStart + len(endMarker), true, nil
}

func writeManagedInstructionsFile(
	path string,
	data []byte,
	mode fs.FileMode,
	fileName string,
	tempPattern string,
	verifyCurrent func() error,
	verifyBinding func() error,
	expected []byte,
	expectedInfo fs.FileInfo,
	expectedExists bool,
) (fs.FileInfo, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), tempPattern)
	if err != nil {
		return nil, fmt.Errorf("create temporary %s: %w", fileName, err)
	}
	tempPath := temp.Name()
	cleanupTemp := true
	var tempInfo fs.FileInfo
	defer func() {
		if cleanupTemp && tempInfo != nil {
			_, _ = removeManagedInstructionsEntryIfSame(tempPath, tempInfo)
		}
	}()
	if err := temp.Chmod(mode.Perm()); err != nil {
		_ = temp.Close()
		return nil, fmt.Errorf("set temporary %s permissions: %w", fileName, err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return nil, fmt.Errorf("write temporary %s: %w", fileName, err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return nil, fmt.Errorf("sync temporary %s: %w", fileName, err)
	}
	tempInfo, err = temp.Stat()
	if err != nil {
		_ = temp.Close()
		return nil, fmt.Errorf("inspect temporary %s: %w", fileName, err)
	}
	if err := temp.Close(); err != nil {
		return nil, fmt.Errorf("close temporary %s: %w", fileName, err)
	}
	if verifyCurrent != nil {
		if err := verifyCurrent(); err != nil {
			return nil, err
		}
	}
	runManagedInstructionsBeforeMutationForTest()
	if !expectedExists {
		// Linking a same-directory temporary file into an absent destination is
		// an atomic no-overwrite create. A file that appears after verification
		// wins; Witself refuses the update instead of replacing it.
		if err := os.Link(tempPath, path); err != nil {
			return nil, fmt.Errorf("refuse to create %s because it changed during update: %w", path, err)
		}
		stateErr := verifyManagedInstructionsFileState(path, data, tempInfo)
		if stateErr == nil && verifyBinding != nil {
			stateErr = verifyBinding()
		}
		if stateErr == nil {
			if err := managedInstructionsSyncDirectory(path); err != nil {
				stateErr = fmt.Errorf("sync %s after create: %w", filepath.Dir(path), err)
			}
		}
		if stateErr != nil {
			preserved, recoveryErr := recoverManagedInstructionsCreate(path, tempPath, tempInfo)
			if recoveryErr != nil {
				return nil, fmt.Errorf(
					"refuse to create %s because it changed during update (%v); recovery failed: %w; preserved state: %s",
					path,
					stateErr,
					recoveryErr,
					strings.Join(preserved, ", "),
				)
			}
			if len(preserved) != 0 {
				return nil, fmt.Errorf(
					"refuse to create %s because it changed during update (%v); preserved state: %s",
					path,
					stateErr,
					strings.Join(preserved, ", "),
				)
			}
			return nil, fmt.Errorf("refuse to create %s because it changed during update: %w", path, stateErr)
		}
		if removed, _ := removeManagedInstructionsEntryIfSame(tempPath, tempInfo); removed {
			cleanupTemp = false
			_ = managedInstructionsSyncDirectory(path)
		}
		return tempInfo, nil
	}
	// Exchange retains the exact file that occupied path at the mutation
	// instant under tempPath. Verify that displaced inode, not a stale pre-check,
	// before committing the replacement.
	if err := exchangeManagedInstructionFiles(tempPath, path); err != nil {
		return nil, fmt.Errorf("replace %s atomically: %w", path, err)
	}
	// tempPath now contains a user-owned displaced destination, so the generic
	// temporary-file cleanup must never unlink it by name.
	cleanupTemp = false
	stateErr := verifyManagedInstructionsFileState(tempPath, expected, expectedInfo)
	if stateErr == nil {
		stateErr = verifyManagedInstructionsFileState(path, data, tempInfo)
	}
	if stateErr == nil && verifyBinding != nil {
		stateErr = verifyBinding()
	}
	if stateErr == nil {
		if err := managedInstructionsSyncDirectory(path); err != nil {
			stateErr = fmt.Errorf("sync %s after replacement: %w", filepath.Dir(path), err)
		} else {
			if removed, _ := removeManagedInstructionsEntryIfSame(tempPath, expectedInfo); removed {
				_ = managedInstructionsSyncDirectory(path)
			}
			return tempInfo, nil
		}
	}

	// The destination changed after the pre-check. Put the displaced version
	// back without overwriting any still-newer edit. Recovery preserves every
	// inode under a named temporary path if another race prevents restoration.
	recoveryPaths, recoveryErr := recoverManagedInstructionsExchange(path, tempPath, tempInfo, tempPattern)
	if recoveryErr != nil {
		return nil, fmt.Errorf(
			"refuse to replace %s because it changed during update (%v); recovery failed: %w; preserved state: %s",
			path,
			stateErr,
			recoveryErr,
			strings.Join(recoveryPaths, ", "),
		)
	}
	if len(recoveryPaths) != 0 {
		return nil, fmt.Errorf(
			"refuse to replace %s because it changed during update (%v); preserved state: %s",
			path,
			stateErr,
			strings.Join(recoveryPaths, ", "),
		)
	}
	return nil, fmt.Errorf("refuse to replace %s because it changed during update: %w", path, stateErr)
}

// managedInstructionsBeforeMutationForTest lets deterministic tests replace a
// destination in the otherwise tiny interval between preflight and mutation.
// Production code leaves it nil.
var managedInstructionsBeforeMutationForTest func()

// managedInstructionsBeforeRecoveryForTest lets deterministic tests replace a
// recovery temporary entry immediately before it participates in an exchange.
// Production code leaves it nil.
var managedInstructionsBeforeRecoveryForTest func()

var managedInstructionsBeforeDeleteMutationForTest func(string)

// managedInstructionsSyncDirectory is replaceable only so tests can prove
// that a failed durability barrier restores the pre-mutation destination.
var managedInstructionsSyncDirectory = syncManagedInstructionsDirectory

func runManagedInstructionsBeforeMutationForTest() {
	if managedInstructionsBeforeMutationForTest != nil {
		managedInstructionsBeforeMutationForTest()
	}
}

func runManagedInstructionsBeforeRecoveryForTest() {
	if managedInstructionsBeforeRecoveryForTest != nil {
		managedInstructionsBeforeRecoveryForTest()
	}
}

func runManagedInstructionsBeforeDeleteMutationForTest(path string) {
	if managedInstructionsBeforeDeleteMutationForTest != nil {
		managedInstructionsBeforeDeleteMutationForTest(path)
	}
}

func recoverManagedInstructionsExchange(
	path string,
	displacedPath string,
	replacementInfo fs.FileInfo,
	tempPattern string,
) ([]string, error) {
	dir := filepath.Dir(path)
	capture, err := os.CreateTemp(dir, tempPattern+"-recovery-*")
	if err != nil {
		return []string{displacedPath}, err
	}
	capturePath := capture.Name()
	captureInfo, err := capture.Stat()
	if err != nil {
		_ = capture.Close()
		return []string{displacedPath, capturePath}, err
	}
	if err := capture.Close(); err != nil {
		return []string{displacedPath, capturePath}, err
	}
	runManagedInstructionsBeforeRecoveryForTest()
	if err := exchangeManagedInstructionFiles(capturePath, path); err != nil {
		_, _ = removeManagedInstructionsEntryIfSame(capturePath, captureInfo)
		return []string{displacedPath}, err
	}
	if err := exchangeManagedInstructionFiles(displacedPath, path); err != nil {
		// Undo the first exchange when possible. If that fails, every inode still
		// has a name and the caller receives all preservation paths.
		rollbackErr := exchangeManagedInstructionFiles(capturePath, path)
		_ = managedInstructionsSyncDirectory(path)
		if rollbackErr != nil {
			return existingManagedInstructionsPaths(path, displacedPath, capturePath), errors.Join(err, rollbackErr)
		}
		_, _ = removeManagedInstructionsEntryIfSame(capturePath, captureInfo)
		return existingManagedInstructionsPaths(displacedPath, capturePath), err
	}
	if err := managedInstructionsSyncDirectory(path); err != nil {
		return existingManagedInstructionsPaths(displacedPath, capturePath), err
	}
	preserved := make([]string, 0, 2)
	if removed, err := removeManagedInstructionsEntryIfSame(displacedPath, captureInfo); err != nil {
		preserved = append(preserved, displacedPath)
	} else if !removed {
		preserved = append(preserved, displacedPath)
	}
	if removed, err := removeManagedInstructionsEntryIfSame(capturePath, replacementInfo); err != nil {
		preserved = append(preserved, capturePath)
	} else if !removed {
		preserved = append(preserved, capturePath)
	}
	_ = managedInstructionsSyncDirectory(path)
	return preserved, nil
}

func recoverManagedInstructionsCreate(path, tempPath string, tempInfo fs.FileInfo) ([]string, error) {
	pathInfo, pathErr := os.Lstat(path)
	tempCurrent, tempErr := os.Lstat(tempPath)
	linkedByWitself := pathErr == nil && sameManagedInstructionsFileIdentity(pathInfo, tempInfo)
	if !linkedByWitself && pathErr == nil && tempErr == nil {
		linkedByWitself = sameManagedInstructionsFileIdentity(pathInfo, tempCurrent)
	}
	if !linkedByWitself {
		return preservedManagedInstructionsCreatePaths(path, tempPath, tempInfo), errors.New("new destination could not be identified safely")
	}
	removed, err := removeManagedInstructionsEntryIfSame(path, pathInfo)
	if err != nil || !removed {
		if err == nil {
			err = errors.New("new destination changed during recovery")
		}
		return preservedManagedInstructionsCreatePaths(path, tempPath, tempInfo), err
	}
	if err := managedInstructionsSyncDirectory(path); err != nil {
		return existingManagedInstructionsPaths(tempPath), err
	}
	if tempErr == nil && !sameManagedInstructionsFileIdentity(tempCurrent, tempInfo) {
		return []string{tempPath}, nil
	}
	return nil, nil
}

func preservedManagedInstructionsCreatePaths(path, tempPath string, tempInfo fs.FileInfo) []string {
	preserved := existingManagedInstructionsPaths(path)
	current, err := os.Lstat(tempPath)
	if err == nil && !sameManagedInstructionsFileIdentity(current, tempInfo) {
		preserved = append(preserved, tempPath)
	}
	return preserved
}

func removeManagedInstructionsFile(
	path string,
	fileName string,
	tempPattern string,
	verifyCurrent func() error,
	expected []byte,
	expectedInfo fs.FileInfo,
) error {
	if fileName == "" {
		fileName = filepath.Base(path)
	}
	if tempPattern == "" {
		tempPattern = "." + fileName + ".witself-*"
	}
	tomb, err := os.CreateTemp(filepath.Dir(path), tempPattern+"-delete-*")
	if err != nil {
		return fmt.Errorf("create temporary %s deletion: %w", fileName, err)
	}
	tombPath := tomb.Name()
	tombInfo, err := tomb.Stat()
	if err != nil {
		_ = tomb.Close()
		_ = os.Remove(tombPath)
		return fmt.Errorf("inspect temporary %s deletion: %w", fileName, err)
	}
	if err := tomb.Close(); err != nil {
		_ = os.Remove(tombPath)
		return fmt.Errorf("close temporary %s deletion: %w", fileName, err)
	}
	removed, err := removeManagedInstructionsEntryIfSame(tombPath, tombInfo)
	if err != nil || !removed {
		if err == nil {
			err = errors.New("temporary deletion entry was replaced")
		}
		return fmt.Errorf("prepare temporary %s deletion without overwriting another entry: %w", fileName, err)
	}
	if verifyCurrent != nil {
		if err := verifyCurrent(); err != nil {
			return err
		}
	}
	runManagedInstructionsBeforeMutationForTest()
	runManagedInstructionsBeforeDeleteMutationForTest(tombPath)
	// A no-replace rename makes the destination absent in one namespace
	// operation and refuses to overwrite a tomb entry that appeared meanwhile.
	if err := renameManagedInstructionFileNoReplace(path, tombPath); err != nil {
		return fmt.Errorf("refuse to remove %s because it changed during update: %w", path, err)
	}
	stateErr := verifyManagedInstructionsFileState(tombPath, expected, expectedInfo)
	if stateErr != nil {
		preserved, recoveryErr := restoreManagedInstructionsRenamedDeletion(path, tombPath)
		if recoveryErr != nil {
			return fmt.Errorf(
				"refuse to remove %s because it changed during update (%v); recovery failed: %w; preserved state: %s",
				path,
				stateErr,
				recoveryErr,
				strings.Join(preserved, ", "),
			)
		}
		if len(preserved) != 0 {
			return fmt.Errorf(
				"refuse to remove %s because it changed during update (%v); preserved state: %s",
				path,
				stateErr,
				strings.Join(preserved, ", "),
			)
		}
		return fmt.Errorf("refuse to remove %s because it changed during update: %w", path, stateErr)
	}
	// Persist the absent destination while its old inode remains recoverable at
	// tombPath. A failed barrier can therefore restore the exact prior entry.
	if err := managedInstructionsSyncDirectory(path); err != nil {
		preserved, recoveryErr := restoreManagedInstructionsRenamedDeletion(path, tombPath)
		if recoveryErr != nil {
			return fmt.Errorf("sync %s after removal: %v; recovery failed: %w; preserved state: %s", filepath.Dir(path), err, recoveryErr, strings.Join(preserved, ", "))
		}
		return fmt.Errorf("sync %s after removal: %w", filepath.Dir(path), err)
	}
	if removed, _ := removeManagedInstructionsEntryIfSame(tombPath, expectedInfo); removed {
		_ = managedInstructionsSyncDirectory(path)
	}
	return nil
}

func restoreManagedInstructionsRenamedDeletion(path, tombPath string) ([]string, error) {
	if err := renameManagedInstructionFileNoReplace(tombPath, path); err != nil {
		return existingManagedInstructionsPaths(path, tombPath), err
	}
	if err := managedInstructionsSyncDirectory(path); err != nil {
		return existingManagedInstructionsPaths(path, tombPath), err
	}
	return nil, nil
}

func removeManagedInstructionsEntryIfSame(path string, expectedInfo fs.FileInfo) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sameManagedInstructionsFileIdentity(info, expectedInfo) {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

// os.SameFile compares only device and inode. Linux may immediately reuse an
// inode after a remove-and-recreate boundary race, which made a replacement
// look identical to the snapshot in fast tests and, more importantly, in real
// concurrent edits. Size and modification time are stable across the hard-link
// and rename operations used here but change on ordinary replacement writes,
// so include them in the optimistic identity token.
func sameManagedInstructionsFileIdentity(current, expected fs.FileInfo) bool {
	return current != nil && expected != nil &&
		os.SameFile(current, expected) &&
		current.Size() == expected.Size() &&
		current.ModTime().Equal(expected.ModTime())
}

func existingManagedInstructionsPaths(paths ...string) []string {
	existing := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, err := os.Lstat(path); err == nil {
			existing = append(existing, path)
		}
	}
	return existing
}
