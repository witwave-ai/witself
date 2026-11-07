package localcell

import "errors"

// ReadinessFailureForTest exposes the observation from before startup rollback
// to the external lifecycle proof tests, without adding a production API.
func ReadinessFailureForTest(err error) (component, cause string, state ProcessState, observationFailed, ok bool) {
	var failure *readinessError
	if !errors.As(err, &failure) {
		return "", "", ProcessState{}, false, false
	}
	return failure.component, failure.Error(), failure.state, failure.observationFailed, true
}

// ReadinessExitDiagnosticForTest shares the production value-free renderer with
// the external lifecycle diagnostic contract, without exporting a runtime API.
func ReadinessExitDiagnosticForTest(err error) string {
	var exit *readinessExitError
	if !errors.As(err, &exit) {
		return ""
	}
	return exit.observationDiagnostic()
}

func ReadinessExitForTest(waitCompleted, recoveredAlive bool) error {
	return &readinessExitError{
		observation: &readinessError{component: "server"}, component: "server",
		exitStatus: "unavailable", diagnostic: "value-free diagnostic unavailable",
		waitCompleted: waitCompleted, recoveredAlive: recoveredAlive,
	}
}
