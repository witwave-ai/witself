//go:build !darwin && !linux

package main

import "errors"

func acquireCopilotOperationLock() (func(), error) {
	return nil, errors.New("atomic GitHub Copilot integration locking is not supported on this platform")
}
