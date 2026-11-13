package localcell

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

const (
	serviceLeasePublishBudget         = 2 * time.Second
	serviceLeasePublishInitialBackoff = 25 * time.Millisecond
	serviceLeasePublishMaxBackoff     = 200 * time.Millisecond
)

// publishError keeps the public record error value-free while retaining the
// platform classification for the shared publication retry decision.
type publishError struct{ cause error }

func (e *publishError) Error() string { return "cannot publish local cell record" }
func (e *publishError) Unwrap() error { return e.cause }

type leasePublishHooks struct {
	write func(string, any, func() error) error
	sleep func(time.Duration)
	now   func() time.Time
}

// Like beforePasswordWrite, these hooks are nil outside tests. Each nil field
// selects the corresponding production operation; write is one complete
// publication attempt, never a retrying one.
var serviceLeasePublishTestHooks leasePublishHooks

func (h leasePublishHooks) defaults() leasePublishHooks {
	if h.write == nil {
		h.write = writeAtomicOnce
	}
	if h.sleep == nil {
		h.sleep = time.Sleep
	}
	if h.now == nil {
		h.now = time.Now
	}
	return h
}

func retryPublish(attempt func() error, retryable func(error) bool, h leasePublishHooks) (attempts int, err error) {
	h = h.defaults()
	start := h.now()
	backoff := serviceLeasePublishInitialBackoff
	for {
		attempts++
		err = attempt()
		if err == nil || !retryable(err) || h.now().Sub(start) >= serviceLeasePublishBudget {
			return attempts, err
		}
		h.sleep(backoff)
		backoff = min(backoff*2, serviceLeasePublishMaxBackoff)
	}
}

// The lease relies on the shared publication retry. It only adds its value-free
// failure diagnostic, which needs the attempt count.
func (c *Cell) publishServiceLease(path string, record serviceLeaseRecord) error {
	h := serviceLeasePublishTestHooks.defaults()
	start := h.now()
	attempts, err := publishAtomic(func() error {
		return h.write(path, record, nil)
	}, h)
	if err != nil {
		writeLeasePublishDiagnostic(os.Stderr, runtimeconfig.WindowsPrivateFailureClass(err), attempts, h.now().Sub(start))
	}
	return err
}

func writeLeasePublishDiagnostic(w io.Writer, class string, attempts int, elapsed time.Duration) {
	switch class {
	case "sharing_violation", "access_denied", "validation", "other":
	default:
		class = "other"
	}
	_, _ = fmt.Fprintf(w, "native service host lease_publish class=%s attempts=%d elapsed_ms=%d\n", class, attempts, elapsed.Milliseconds())
}
