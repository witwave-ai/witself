//go:build !darwin && !linux

package main

import "errors"

func exchangeManagedInstructionFiles(_, _ string) error {
	return errors.New("atomic managed-instruction exchange is not supported on this operating system")
}

func renameManagedInstructionFileNoReplace(_, _ string) error {
	return errors.New("atomic managed-instruction no-replace rename is not supported on this operating system")
}

func syncManagedInstructionsDirectory(_ string) error {
	return nil
}
