package localcell

import (
	"testing"
)

func TestUpdateFixedCauses(t *testing.T) {
	for cause, want := range map[fixedCause]string{
		errMaintenanceFenceActive: "local_maintenance_fence_active",
		errUpdatePartialState:     "local_update_partial_state",
	} {
		if cause.Error() != want {
			t.Fatal("update fixed cause changed")
		}
	}
}
