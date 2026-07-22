//go:build !darwin && !linux

package main

import "errors"

func acquireAntigravityOperationLock() (func(), error) {
	return nil, errors.New("atomic Antigravity integration locking is not supported on this platform")
}
