package localcell

import "testing"

func TestServiceFaultEvidenceReset(t *testing.T) {
	for _, row := range []string{"repair", "retirement"} {
		t.Run(row, func(t *testing.T) {
			f := newFaultCommitGuardFixture(t)
			if err := f.step(t, nil, func() {}); err != nil {
				t.Fatal(faultPersistenceLost)
			}
			before := f.descriptor(t)
			if !before.Faulted || f.s.shutdownFaultCommit.revision != before.Revision {
				t.Fatal(faultPersistenceLost)
			}
			if row == "repair" {
				if _, err := f.s.command(t.Context(), "repair"); err != nil {
					t.Fatal(faultPersistenceLost)
				}
				after := f.descriptor(t)
				if after.Faulted || after.RepairRevision != before.RepairRevision+1 || f.s.shutdownBlocked ||
					f.s.shutdownFaultCommit != (serviceFaultCommit{}) {
					t.Fatal(faultPersistenceLost)
				}
				return
			}

			// Run releases this supervisor at retirement. Retain the fixture
			// reference and exercise the normal writer on an identical snapshot:
			// even the old supervisor can no longer claim its previous commit.
			f.s.retireShutdownFault()
			if !f.s.shutdownBlocked || f.s.fault(t.Context(), "service_stop_refused") != nil {
				t.Fatal(faultPersistenceLost)
			}
			f.settled(t, before.Revision+1)
		})
	}
}
