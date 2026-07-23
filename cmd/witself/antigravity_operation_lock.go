package main

import (
	"path/filepath"
)

const antigravityOperationLockFile = ".witself-antigravity-operation.lock"

func acquireAntigravityOperationLock() (func(), error) {
	configRoot, err := antigravityOperationLockRoot()
	if err != nil {
		return nil, err
	}
	return acquireIntegrationOperationLock(
		filepath.Join(configRoot, antigravityOperationLockFile),
		"Antigravity",
		"another Antigravity install or uninstall is already running",
	)
}
