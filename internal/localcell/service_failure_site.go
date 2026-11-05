package localcell

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"
)

// Site metadata preserves the original error text and identity. Only the closed
// site label is printed separately by the foreground command on failure.
type serviceFailure struct {
	cause    error
	site     string
	reported atomic.Bool
}

func (e *serviceFailure) Error() string { return e.cause.Error() }
func (e *serviceFailure) Unwrap() error { return e.cause }

func serviceFailureSite(site string) string {
	switch site {
	case "host_config", "host_descriptor", "host_prepare", "host_persisted_config",
		"initial_descriptor", "lease_directory", "lease_record", "lease_write",
		"control_secret", "control_transport", "control_cancelled", "endpoint_descriptor", "endpoint_read", "endpoint_write", "endpoint_lease", "endpoint_remove",
		"supervisor_load", "supervisor_traversal", "supervisor_save", "supervisor_start", "supervisor_stop", "supervisor_repair", "supervisor_reconcile", "supervisor_health", "supervisor_command",
		"health_lock", "health_tree", "health_database", "health_credential", "health_probe", "health_ready",
		"startup_reconcile", "startup_repair", "startup_stop", "shutdown_repair", "shutdown_stop", "boot_binding", "descriptor_write":
		return site
	default:
		return "unknown"
	}
}

func serviceFailureAt(err error, site string) error {
	if err == nil {
		return nil
	}
	return &serviceFailure{cause: err, site: serviceFailureSite(site)}
}

// A boundary supplies a fallback only when a more precise operation has not
// already attached a site.
func serviceFailureFallback(err error, site string) error {
	var located *serviceFailure
	if errors.As(err, &located) {
		return err
	}
	return serviceFailureAt(err, site)
}

// Operation names are internal control flow, but only this fixed mapping may
// attach them to an externally visible failure receipt.
func serviceFailureOperation(err error, operation string) error {
	switch operation {
	case "start":
		return serviceFailureFallback(err, "supervisor_start")
	case "stop":
		return serviceFailureFallback(err, "supervisor_stop")
	case "repair":
		return serviceFailureFallback(err, "supervisor_repair")
	case "reconcile":
		return serviceFailureFallback(err, "supervisor_reconcile")
	case "health":
		return serviceFailureFallback(err, "supervisor_health")
	default:
		return serviceFailureFallback(err, "unknown")
	}
}

// WriteServiceHostFailure emits bounded, value-free receipts, including errors
// swallowed by status translation or backoff. The innermost site owns the receipt
// across all later boundaries. Joined failures are visited through ordinary
// wrappers too, so an already reported branch cannot hide an unreported sibling.
// Raw errors remain the caller's responsibility and never enter this line.
func WriteServiceHostFailure(output io.Writer, err error, fallback string) {
	if err == nil {
		return
	}
	var visit func(error, *serviceFailure)
	visit = func(err error, located *serviceFailure) {
		if err == nil {
			return
		}
		if failure, ok := err.(*serviceFailure); ok {
			located = failure
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, cause := range joined.Unwrap() {
				visit(cause, located)
			}
			return
		}
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			visit(wrapped.Unwrap(), located)
			return
		}
		site := serviceFailureSite(fallback)
		if located != nil {
			if !located.reported.CompareAndSwap(false, true) {
				return
			}
			site = serviceFailureSite(located.site)
		}
		_, _ = fmt.Fprintf(output, "native service host site=%s\n", site)
	}
	visit(err, nil)
}
