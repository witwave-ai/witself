package main

import "errors"

// providerPreflightChangedError reports that provider state no longer matches
// the exact snapshot established before a mutation. The caller must not claim
// or remove the new state. If an earlier phase already mutated provider state,
// the surrounding transaction must be retained for explicit recovery.
type providerPreflightChangedError struct {
	err error
}

func (e *providerPreflightChangedError) Error() string { return e.err.Error() }
func (e *providerPreflightChangedError) Unwrap() error { return e.err }

// providerMutationUncertainError reports that a provider CLI may have committed
// before returning an error, or that the committed state changed before it
// could be verified. Automatic rollback cannot safely attribute the live bytes
// to this process, so durable recovery state must be preserved.
type providerMutationUncertainError struct {
	err error
}

func (e *providerMutationUncertainError) Error() string { return e.err.Error() }
func (e *providerMutationUncertainError) Unwrap() error { return e.err }

func providerPreflightChanged(err error) bool {
	var target *providerPreflightChangedError
	return errors.As(err, &target)
}

func providerMutationUncertain(err error) bool {
	var target *providerMutationUncertainError
	return errors.As(err, &target)
}
