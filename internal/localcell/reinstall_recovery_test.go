package localcell

import (
	"bytes"
	"errors"
	"testing"
)

// The sequence, verification-failure and crash tests already protect the marker
// and run/ custody. These rows cover the retained material and service state
// that survived the recovery-path mutation audit.
func TestRetainedReinstallRecovery(t *testing.T) {
	rows := []string{"failed_proof", "failed_stop", "crash_verified"}

	t.Run("service_stopped", func(t *testing.T) {
		for _, row := range rows {
			t.Run(row, func(t *testing.T) {
				f, r := reinstallFixture(t, "legacy", true)
				c := f.cell
				descriptor := retainedRead(t, c, "service/descriptor.json")
				launches := len(f.launches)
				check := func() {
					t.Helper()
					d, err := c.ServiceDescriptor()
					if err != nil || d.DesiredState != "stopped" ||
						!bytes.Equal(descriptor, retainedRead(t, c, "service/descriptor.json")) {
						t.Fatal("retained reinstall changed service state")
					}
					if len(f.launches) != launches {
						t.Fatal("retained reinstall launched a component")
					}
				}

				// Success-path service retention already has its own test. Here the
				// observations are immediately after failure and after Repair.
				if !retainedReinstallRecoveryRow(t, f, r, row, false, check) {
					t.Fatal("retained reinstall changed service state")
				}
			})
		}
	})

	t.Run("pending_bytes", func(t *testing.T) {
		for _, row := range rows {
			t.Run(row, func(t *testing.T) {
				f, r := reinstallFixture(t, "legacy", false)
				check := func() {
					t.Helper()
					after := retainedSnapshot(t, f.cell)
					for _, name := range []string{
						"journal/private-recovery",
						"payload/sentinel",
						"trust/sentinel",
						"bystander",
					} {
						// retainedEntry compares the same bytes, size and mode as
						// reinstallChanged, including removal of a labelled entry.
						if after[name] != f.material[name] {
							t.Fatal("retained reinstall changed pending recovery bytes")
						}
					}
				}

				// A retry must preserve the original Removal snapshot too, rather
				// than accepting any damage left by the preceding failure/Repair.
				if !retainedReinstallRecoveryRow(t, f, r, row, true, check) {
					t.Fatal("retained reinstall changed pending recovery bytes")
				}
			})
		}
	})
}

// retainedReinstallRecoveryRow exercises one failure, settles it and optionally
// retries. The callback observes each completed boundary before another operation
// can conceal a change. A false result means the requested row did not complete;
// callers report only their fixed, value-free child diagnostic.
func retainedReinstallRecoveryRow(
	t *testing.T,
	f *retainedFixture,
	r *reinstallRecorder,
	row string,
	retry bool,
	check func(),
) bool {
	t.Helper()
	c := f.cell
	originalFailpoint, originalFailures := c.cfg.Failpoint, r.fail
	t.Cleanup(func() {
		c.cfg.Failpoint, r.fail = originalFailpoint, originalFailures
	})
	r.fail = map[string]error{}
	fired := false
	switch row {
	case "failed_proof":
		// A legacy query refusal reaches verified's rollback branch.
		r.fail["query"] = errors.New("synthetic recovery proof failure")
	case "failed_stop":
		// The proof succeeds, but stop leaves a pending Reinstall for Repair.
		r.fail["stop"] = errors.New("synthetic recovery stop failure")
	case "crash_verified":
		// Cut after the verified record, while Removal's marker still holds.
		c.cfg.Failpoint = func(operation, step string) error {
			if operation == "reinstall" && step == "verified" {
				fired = true
				return errors.New("synthetic recovery crash")
			}
			return nil
		}
	default:
		return false
	}

	err := c.Reinstall(t.Context(), reinstallOptions(f))
	switch row {
	case "failed_proof":
		if !errors.Is(err, errReinstallIdentity) {
			return false
		}
	case "failed_stop":
		if err == nil || err.Error() != "local cell reinstall: stop failed" {
			return false
		}
	case "crash_verified":
		if !fired || !errors.Is(err, errInterrupted) {
			return false
		}
	}
	check()

	// Restore both injections before Repair and keep cleanup for early failures.
	c.cfg.Failpoint, r.fail = originalFailpoint, originalFailures
	if c.Repair(t.Context()) != nil {
		return false
	}
	check()
	if retry {
		if c.Reinstall(t.Context(), reinstallOptions(f)) != nil {
			return false
		}
		check()
	}
	return true
}
