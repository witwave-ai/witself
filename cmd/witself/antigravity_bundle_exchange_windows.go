//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

var antigravityWindowsAfterBundleQuarantineForTest func(live, staged, quarantine string)

func renameAntigravityBundleDirectoryNoReplace(source, destination string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	source = filepath.Clean(source)
	destination = filepath.Clean(destination)
	if strings.EqualFold(source, destination) {
		return errors.New("Antigravity bundle rename source and destination are the same Windows path")
	}
	sourceDirectory := filepath.Dir(source)
	destinationDirectory := filepath.Dir(destination)
	sourceDirectoryHandle, err := openManagedInstructionWindowsDirectory(
		sourceDirectory,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
	)
	if err != nil {
		return err
	}
	defer sourceDirectoryHandle.Close()
	if !strings.EqualFold(sourceDirectory, destinationDirectory) {
		destinationDirectoryHandle, err := openManagedInstructionWindowsDirectory(
			destinationDirectory,
			windows.FILE_READ_ATTRIBUTES,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		)
		if err != nil {
			return err
		}
		defer destinationDirectoryHandle.Close()
		var sourceInformation, destinationInformation windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(
			windows.Handle(sourceDirectoryHandle.Fd()),
			&sourceInformation,
		); err != nil {
			return err
		}
		if err := windows.GetFileInformationByHandle(
			windows.Handle(destinationDirectoryHandle.Fd()),
			&destinationInformation,
		); err != nil {
			return err
		}
		if sourceInformation.VolumeSerialNumber != destinationInformation.VolumeSerialNumber {
			return errors.New("Antigravity bundle Windows rename requires one volume")
		}
	}
	file, info, err := openManagedInstructionWindowsRenameSource(source)
	if err != nil {
		return err
	}
	defer file.Close()
	if !info.IsDir() {
		return errors.New("Antigravity bundle Windows rename source must be a directory")
	}
	return renameOpenManagedInstructionWindowsFileNoReplace(file, destination)
}

// Windows cannot atomically exchange non-empty directories. Keep both halves
// outside Antigravity's live plugin discovery directory, move the validated
// current bundle to its journal-owned quarantine, and then publish the staged
// replacement with a handle-fenced no-replace rename. The Antigravity
// transaction recovery path recognizes both interruption layouts.
func exchangeAntigravityBundleDirectories(
	live string,
	staged string,
	current antigravityPluginBundle,
) (string, error) {
	quarantine := antigravityBundleRemovalPath(live, current)
	if _, err := os.Lstat(quarantine); err == nil {
		return "", errors.New("an interrupted Antigravity plugin exchange requires recovery before replacement")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := renameAntigravityBundleDirectoryNoReplace(live, quarantine); err != nil {
		return "", err
	}
	if antigravityWindowsAfterBundleQuarantineForTest != nil {
		antigravityWindowsAfterBundleQuarantineForTest(live, staged, quarantine)
	}
	if err := renameAntigravityBundleDirectoryNoReplace(staged, live); err != nil {
		cause := fmt.Errorf("publish staged Antigravity plugin: %w", err)
		if _, statErr := os.Lstat(live); errors.Is(statErr, os.ErrNotExist) {
			if restoreErr := renameAntigravityBundleDirectoryNoReplace(quarantine, live); restoreErr != nil {
				cause = errors.Join(cause, fmt.Errorf("restore prior Antigravity plugin: %w", restoreErr))
			}
		} else if statErr != nil {
			cause = errors.Join(cause, fmt.Errorf("inspect Antigravity plugin after failed publish: %w", statErr))
		}
		return "", cause
	}
	return quarantine, nil
}
