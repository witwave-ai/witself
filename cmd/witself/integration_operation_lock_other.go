//go:build !darwin && !linux && !windows

package main

import (
	"errors"
	"os"
)

var errIntegrationOperationLockUnsupported = errors.New("atomic integration operation locking is not supported on this platform")

func openIntegrationOperationLockFile(string) (*os.File, error) {
	return nil, errIntegrationOperationLockUnsupported
}

func validateIntegrationOperationLockIdentity(*os.File) error {
	return errIntegrationOperationLockUnsupported
}

func secureIntegrationOperationLockFile(*os.File) error {
	return errIntegrationOperationLockUnsupported
}

func tryIntegrationOperationLock(*os.File) (bool, error) {
	return false, errIntegrationOperationLockUnsupported
}

func unlockIntegrationOperationLock(*os.File) error {
	return errIntegrationOperationLockUnsupported
}
