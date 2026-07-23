package main

import "path/filepath"

const copilotOperationLockFile = ".witself-copilot-operation.lock"

func acquireCopilotOperationLock() (func(), error) {
	configRoot, err := copilotOperationLockRoot()
	if err != nil {
		return nil, err
	}
	return acquireIntegrationOperationLock(
		filepath.Join(configRoot, copilotOperationLockFile),
		"GitHub Copilot",
		"another GitHub Copilot install, uninstall, or routing refresh is already running",
	)
}
