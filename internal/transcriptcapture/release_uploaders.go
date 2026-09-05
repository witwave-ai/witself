package transcriptcapture

import (
	"bytes"
	"errors"
)

// VerifyManagedHookExecutable checks the exact owned policy and runner, then
// binds that runner to the executable whose release capability was probed.
// A matching runner digest alone does not establish its executable target.
func VerifyManagedHookExecutable(runtimeName string, ownership ManagedHookOwnership, executable string) error {
	if err := VerifyManagedHooksOwned(runtimeName, ownership); err != nil {
		return err
	}
	raw, exists, err := readManagedRegularFile(ownership.RunnerPath)
	if err != nil {
		return err
	}
	if !exists || !bytes.Equal(raw, managedRunnerBytes(executable)) {
		return errors.New("managed hook runner does not target the persisted uploader executable")
	}
	return nil
}
