//go:build !windows

package main

func renameAntigravityBundleDirectoryNoReplace(source, destination string) error {
	return renameManagedInstructionFileNoReplace(source, destination)
}

func exchangeAntigravityBundleDirectories(
	live string,
	staged string,
	_ antigravityPluginBundle,
) (string, error) {
	if err := exchangeManagedInstructionFiles(live, staged); err != nil {
		return "", err
	}
	return staged, nil
}
