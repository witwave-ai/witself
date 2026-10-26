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
