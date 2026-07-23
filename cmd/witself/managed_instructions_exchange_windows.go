//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const managedInstructionWindowsExchangePrefix = ".witself-windows-replace-"

var (
	replaceFileW                                 = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")
	replaceManagedInstructionWindowsFileCall     = callReplaceManagedInstructionWindowsFile
	managedInstructionWindowsAfterReplaceForTest func(replaced, replacement, backup string, callErr error)
	managedInstructionWindowsAfterFinishForTest  func(first, second, backup string)
)

type managedInstructionWindowsRenameInfo struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [windows.MAX_LONG_PATH]uint16
}

// exchangeManagedInstructionFiles has a bounded semantic difference from the
// Darwin and Linux implementations: Windows exposes no atomic two-name swap.
// ReplaceFileW normally installs first at the user-visible second path while
// retaining the displaced second file under a unique same-directory backup.
// Windows can also report a documented partial-failure layout where first and
// the backup remain but second is absent. Inspect exact file identities after
// every call, reconcile that layout before returning, and never overwrite an
// unexpected concurrent entry. Once second contains the validated replacement,
// move the backup to first with a handle-fenced no-replace rename to complete
// the swap.
func exchangeManagedInstructionFiles(first, second string) error {
	first, second, directory, err := managedInstructionWindowsPair(first, second)
	if err != nil {
		return err
	}
	directoryHandle, err := openManagedInstructionWindowsDirectory(
		directory,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
	)
	if err != nil {
		return err
	}
	defer directoryHandle.Close()

	firstFile, firstInfo, err := openManagedInstructionWindowsReplaceSource(first)
	if err != nil {
		return err
	}
	defer firstFile.Close()
	secondFile, secondInfo, err := openManagedInstructionWindowsReplaceSource(second)
	if err != nil {
		_ = firstFile.Close()
		return err
	}
	defer secondFile.Close()
	if os.SameFile(firstInfo, secondInfo) {
		return errors.New("managed-instruction exchange paths identify the same Windows file")
	}
	if firstInfo.IsDir() || secondInfo.IsDir() {
		return errors.New("Windows managed-instruction exchange supports regular files only")
	}

	preservationPath, err := vacantManagedInstructionWindowsPath(directory)
	if err != nil {
		return fmt.Errorf("reserve Windows managed-instruction exchange preservation path: %w", err)
	}
	// ReplaceFileW opens the replacement with no sharing mode. Release our
	// validation handle immediately before the call, then verify that the exact
	// validated identity became the destination. If a racer changed first in
	// this bounded window, the actual displaced destination is restored below.
	if err := firstFile.Close(); err != nil {
		return fmt.Errorf("close validated Windows replacement before exchange: %w", err)
	}
	replaceErr := replaceManagedInstructionWindowsFileCall(second, first, preservationPath)
	if managedInstructionWindowsAfterReplaceForTest != nil {
		managedInstructionWindowsAfterReplaceForTest(second, first, preservationPath, replaceErr)
	}

	firstState, err := inspectManagedInstructionWindowsPath(first, firstInfo)
	if err != nil {
		return managedInstructionWindowsExchangeFailure(errors.Join(replaceErr, err), first, second, preservationPath)
	}
	secondState, err := inspectManagedInstructionWindowsPath(second, firstInfo)
	if err != nil {
		return managedInstructionWindowsExchangeFailure(errors.Join(replaceErr, err), first, second, preservationPath)
	}
	preservationState, err := inspectManagedInstructionWindowsPath(preservationPath, secondInfo)
	if err != nil {
		return managedInstructionWindowsExchangeFailure(errors.Join(replaceErr, err), first, second, preservationPath)
	}

	// Normal success, and any Win32 error that nevertheless reached the same
	// exact durable layout, are safe to finish as a completed exchange.
	if !firstState.exists && secondState.matches && preservationState.matches {
		return finishManagedInstructionWindowsExchange(
			secondFile,
			firstInfo,
			first,
			second,
			preservationPath,
			replaceErr,
		)
	}

	// ERROR_UNABLE_TO_MOVE_REPLACEMENT_2 leaves the validated replacement at
	// first, the validated old destination at the requested backup, and second
	// absent. Complete that interrupted move without replacing a racing entry.
	if firstState.matches && !secondState.exists && preservationState.matches {
		if moveErr := renameManagedInstructionFileNoReplace(first, second); moveErr == nil {
			return finishManagedInstructionWindowsExchange(
				secondFile,
				firstInfo,
				first,
				second,
				preservationPath,
				replaceErr,
			)
		} else {
			cause := errors.Join(replaceErr, fmt.Errorf("complete partial Windows replacement: %w", moveErr))
			currentSecond, inspectErr := inspectManagedInstructionWindowsPath(second, secondInfo)
			cause = errors.Join(cause, inspectErr)
			if inspectErr == nil && !currentSecond.exists {
				// No racer supplied a shared-path entry. Restore the exact displaced
				// destination so a reported failure never knowingly returns with the
				// shared instruction path absent.
				if restoreErr := renameOpenManagedInstructionWindowsFileNoReplace(secondFile, second); restoreErr != nil {
					cause = errors.Join(cause, fmt.Errorf("restore shared Windows instruction path: %w", restoreErr))
				}
			}
			return managedInstructionWindowsExchangeFailure(cause, first, second, preservationPath)
		}
	}

	// A concurrent writer may have replaced second after ReplaceFileW returned.
	// Never roll an expected old file over that newer entry. If second is absent
	// and the exact displaced destination is available, restore it; otherwise
	// leave every existing path untouched and disclose all recovery locations.
	cause := errors.Join(replaceErr, fmt.Errorf(
		"unexpected Windows replacement layout (first=%s second=%s backup=%s)",
		firstState, secondState, preservationState,
	))
	if !secondState.exists && preservationState.matches {
		if restoreErr := renameOpenManagedInstructionWindowsFileNoReplace(secondFile, second); restoreErr != nil {
			cause = errors.Join(cause, fmt.Errorf("restore shared Windows instruction path: %w", restoreErr))
		}
	}
	return managedInstructionWindowsExchangeFailure(cause, first, second, preservationPath)
}

func finishManagedInstructionWindowsExchange(
	displacedFile *os.File,
	replacementInfo os.FileInfo,
	first string,
	second string,
	preservationPath string,
	replaceErr error,
) error {
	displacedInfo, err := displacedFile.Stat()
	if err != nil {
		return managedInstructionWindowsExchangeFailure(
			errors.Join(replaceErr, fmt.Errorf("inspect displaced Windows instruction before finish: %w", err)),
			first,
			second,
			preservationPath,
		)
	}
	if err := renameOpenManagedInstructionWindowsFileNoReplace(displacedFile, first); err != nil {
		return managedInstructionWindowsExchangeFailure(
			errors.Join(replaceErr, fmt.Errorf("finish Windows managed-instruction exchange: %w", err)),
			first,
			second,
			preservationPath,
		)
	}
	if managedInstructionWindowsAfterFinishForTest != nil {
		managedInstructionWindowsAfterFinishForTest(first, second, preservationPath)
	}
	firstState, firstErr := inspectManagedInstructionWindowsPath(first, displacedInfo)
	secondState, secondErr := inspectManagedInstructionWindowsPath(second, replacementInfo)
	preservationState, preservationErr := inspectManagedInstructionWindowsPath(preservationPath, displacedInfo)
	if firstErr != nil || secondErr != nil || preservationErr != nil ||
		!firstState.matches || !secondState.matches || preservationState.exists {
		cause := errors.Join(
			replaceErr,
			firstErr,
			secondErr,
			preservationErr,
			fmt.Errorf(
				"Windows exchange changed while finishing (first=%s second=%s backup=%s)",
				firstState,
				secondState,
				preservationState,
			),
		)
		return managedInstructionWindowsExchangeFailure(cause, first, second, preservationPath)
	}
	return nil
}

type managedInstructionWindowsPathState struct {
	exists  bool
	matches bool
}

func (state managedInstructionWindowsPathState) String() string {
	if !state.exists {
		return "absent"
	}
	if state.matches {
		return "expected"
	}
	return "other"
}

func inspectManagedInstructionWindowsPath(path string, expected os.FileInfo) (managedInstructionWindowsPathState, error) {
	actual, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return managedInstructionWindowsPathState{}, nil
	}
	if err != nil {
		return managedInstructionWindowsPathState{}, fmt.Errorf("inspect Windows managed-instruction path %s: %w", path, err)
	}
	return managedInstructionWindowsPathState{
		exists: true,
		matches: actual.Mode().IsRegular() && actual.Mode()&os.ModeSymlink == 0 &&
			os.SameFile(actual, expected),
	}, nil
}

func callReplaceManagedInstructionWindowsFile(replaced, replacement, backup string) error {
	replacedPointer, err := windows.UTF16PtrFromString(replaced)
	if err != nil {
		return err
	}
	replacementPointer, err := windows.UTF16PtrFromString(replacement)
	if err != nil {
		return err
	}
	backupPointer, err := windows.UTF16PtrFromString(backup)
	if err != nil {
		return err
	}
	result, _, callErr := replaceFileW.Call(
		uintptr(unsafe.Pointer(replacedPointer)),
		uintptr(unsafe.Pointer(replacementPointer)),
		uintptr(unsafe.Pointer(backupPointer)),
		0,
		0,
		0,
	)
	if result != 0 {
		return nil
	}
	if callErr != windows.ERROR_SUCCESS {
		return callErr
	}
	return errors.New("ReplaceFileW failed without a Windows error code")
}

func managedInstructionWindowsExchangeFailure(cause error, paths ...string) error {
	preserved := existingManagedInstructionsPaths(paths...)
	if len(preserved) == 0 {
		return fmt.Errorf("recoverable Windows managed-instruction exchange failed: %w", cause)
	}
	return fmt.Errorf(
		"recoverable Windows managed-instruction exchange failed: %w; preserved state: %s",
		cause,
		strings.Join(preserved, ", "),
	)
}

func renameManagedInstructionFileNoReplace(source, destination string) error {
	source, destination, directory, err := managedInstructionWindowsPair(source, destination)
	if err != nil {
		return err
	}
	directoryHandle, err := openManagedInstructionWindowsDirectory(
		directory,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
	)
	if err != nil {
		return err
	}
	defer directoryHandle.Close()
	file, _, err := openManagedInstructionWindowsRenameSource(source)
	if err != nil {
		return err
	}
	defer file.Close()
	return renameOpenManagedInstructionWindowsFileNoReplace(file, destination)
}

func managedInstructionWindowsPair(first, second string) (string, string, string, error) {
	first, err := filepath.Abs(first)
	if err != nil {
		return "", "", "", err
	}
	second, err = filepath.Abs(second)
	if err != nil {
		return "", "", "", err
	}
	first = filepath.Clean(first)
	second = filepath.Clean(second)
	if strings.EqualFold(first, second) {
		return "", "", "", errors.New("managed-instruction rename source and destination are the same Windows path")
	}
	firstDirectory := filepath.Dir(first)
	secondDirectory := filepath.Dir(second)
	if !strings.EqualFold(firstDirectory, secondDirectory) {
		return "", "", "", errors.New("managed-instruction Windows rename requires one directory and volume")
	}
	return first, second, firstDirectory, nil
}

func openManagedInstructionWindowsRenameSource(path string) (*os.File, os.FileInfo, error) {
	return openManagedInstructionWindowsSource(path, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
}

func openManagedInstructionWindowsReplaceSource(path string) (*os.File, os.FileInfo, error) {
	return openManagedInstructionWindowsSource(
		path,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
}

func openManagedInstructionWindowsSource(path string, share uint32) (*os.File, os.FileInfo, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, nil, err
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		share,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, nil, errors.New("Windows managed-instruction rename handle is unavailable")
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	linked, err := os.Lstat(path)
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		opened.Mode()&os.ModeSymlink != 0 || linked.Mode()&os.ModeSymlink != 0 {
		_ = file.Close()
		return nil, nil, fmt.Errorf("refuse to rename Windows reparse point %s", path)
	}
	openedSupported := opened.Mode().IsRegular() || opened.IsDir()
	linkedSupported := linked.Mode().IsRegular() || linked.IsDir()
	if !openedSupported || !linkedSupported || opened.IsDir() != linked.IsDir() || !os.SameFile(opened, linked) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("Windows managed-instruction source %s changed while opening", path)
	}
	return file, opened, nil
}

func renameOpenManagedInstructionWindowsFileNoReplace(file *os.File, destination string) error {
	destination, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	encoded, err := windows.UTF16FromString(filepath.Clean(destination))
	if err != nil {
		return err
	}
	nameUnits := len(encoded) - 1
	if nameUnits > windows.MAX_LONG_PATH {
		return errors.New("Windows managed-instruction destination exceeds the long-path limit")
	}
	var information managedInstructionWindowsRenameInfo
	information.FileNameLength = uint32(nameUnits * 2)
	copy(information.FileName[:], encoded[:nameUnits])
	bufferSize := unsafe.Offsetof(information.FileName) + uintptr(information.FileNameLength)
	return windows.SetFileInformationByHandle(
		windows.Handle(file.Fd()),
		windows.FileRenameInfo,
		(*byte)(unsafe.Pointer(&information)),
		uint32(bufferSize),
	)
}

func vacantManagedInstructionWindowsPath(directory string) (string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		path := filepath.Join(directory, managedInstructionWindowsExchangePrefix+hex.EncodeToString(random[:]))
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			// Do not pre-create this path. A placeholder would publish the exact
			// backup name to a directory watcher before ReplaceFileW uses it.
			// The unannounced 128-bit random name makes an independent collision
			// infeasible; ReplaceFileW remains responsible for creating it.
			return path, nil
		} else if err != nil {
			return path, err
		}
	}
	return "", errors.New("could not choose an unused Windows managed-instruction preservation path")
}

func syncManagedInstructionsDirectory(path string) error {
	directory, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return err
	}
	file, err := openManagedInstructionWindowsDirectory(
		directory,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
	if err != nil {
		return err
	}
	// Win32 exposes no portable POSIX-style directory fsync, and
	// FlushFileBuffers on a directory handle is unsupported on common local
	// filesystems including NTFS. The generic transaction has already synced all
	// replacement file bytes before mutation. A successful ReplaceFileW keeps
	// the destination present; the exchange code reconciles its documented
	// partial-failure layout before returning. Validate and pin the exact
	// non-reparse directory through this boundary, but do not turn the absence
	// of a directory-flush primitive into a deterministic install failure.
	return file.Close()
}

func openManagedInstructionWindowsDirectory(path string, access, share uint32) (*os.File, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPointer,
		access,
		share,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("Windows managed-instruction directory handle is unavailable")
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	linked, err := os.Lstat(path)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		_ = file.Close()
		return nil, err
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		!opened.IsDir() || !linked.IsDir() ||
		opened.Mode()&os.ModeSymlink != 0 || linked.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, linked) {
		_ = file.Close()
		return nil, fmt.Errorf("refuse to use unsafe Windows managed-instruction directory %s", path)
	}
	return file, nil
}
