//go:build !windows

package localcell

import "testing"

// Exercise the Windows target comparison portably with two issued records for
// the same process. Unix exit transport itself keeps its stable socket protocol.
func bindingOriginalExitActivation(t *testing.T) {
	t.Run("original_exit_activation", func(t *testing.T) {
		original, _, _ := endpointFixture(t)
		next := original
		var err error
		next.PipeName, err = newServiceControlPipeName(next.InstallationID)
		if err != nil || next.PipeName == original.PipeName {
			t.Fatal(bindingExitFailure)
		}
		target := serviceHostExitActivation{Activation: &original}
		fresh := serviceHostExitActivation{Activation: &next}
		if !target.matches(original) || target.matches(next) || !fresh.matches(next) || (serviceHostExitActivation{}).matches(next) {
			t.Fatal(bindingExitFailure)
		}
	})
}
