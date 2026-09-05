package transcriptcapture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ReleaseUpgradeBarrierError refuses a release until the runtime's previous
// uploader has finished and all remaining events have submission provenance.
type ReleaseUpgradeBarrierError struct {
	Runtime string
	Reason  string
	Err     error
}

func (err *ReleaseUpgradeBarrierError) Error() string {
	return fmt.Sprintf("transcript release refused: %s; let `witself transcript flush --runtime %s` finish in the foreground, then retry", err.Reason, err.Runtime)
}

func (err *ReleaseUpgradeBarrierError) Unwrap() error { return err.Err }

// VerifyReleaseUpgradeBarrier runs before taking the runtime flush lock. In
// particular, a live pre-upgrade uploader must never be displaced because its
// lock happens to be old. Unknown owners also fail closed.
func VerifyReleaseUpgradeBarrier(runtime string) error {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return err
	}
	dir, err := outboxDir(runtime)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, ".flush.lock")
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return &ReleaseUpgradeBarrierError{Runtime: runtime, Reason: "cannot inspect the runtime flush lock", Err: err}
	}
	if err == nil {
		if !info.Mode().IsRegular() {
			return &ReleaseUpgradeBarrierError{Runtime: runtime, Reason: "runtime flush lock owner is unknown"}
		}
		running, known := flushLockOwnerRunning(path)
		if !known {
			return &ReleaseUpgradeBarrierError{Runtime: runtime, Reason: "runtime flush lock owner is unknown"}
		}
		if running {
			return &ReleaseUpgradeBarrierError{Runtime: runtime, Reason: "a runtime transcript flush is active"}
		}
	}
	return nil
}

// VerifyReleaseSubmissionProvenance must run again with the flush lock held,
// before release mutates state. The barrier covers every event for the runtime,
// including other sessions and already-redacted events.
func VerifyReleaseSubmissionProvenance(runtime string) error {
	runtime, err := NormalizeRuntime(runtime)
	if err != nil {
		return err
	}
	pending, err := Pending(runtime)
	if err != nil {
		return &ReleaseUpgradeBarrierError{Runtime: runtime, Reason: "cannot verify outbox submission provenance", Err: err}
	}
	for _, item := range pending {
		if !item.tracked {
			return &ReleaseUpgradeBarrierError{Runtime: runtime, Reason: "outbox contains an event without a submission sidecar (legacy submission history is unknown)", Err: errLegacySubmissionUnknown}
		}
	}
	return nil
}
