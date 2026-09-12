package main

import "path/filepath"

const dshOperationLockFile = ".witself-dsh-operation.lock"

func acquireDSHOperationLock() (func(), error) {
	configRoot, err := dshOperationLockRoot()
	if err != nil {
		return nil, err
	}
	return acquireIntegrationOperationLock(
		filepath.Join(configRoot, dshOperationLockFile),
		"DeepSeek Harness",
		"another DeepSeek Harness install, uninstall, or routing refresh is already running",
	)
}
