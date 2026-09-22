package store

import (
	"testing"

	"github.com/witwave-ai/witself/internal/activity"
)

func TestActivityEventUnitsAndQuantities(t *testing.T) {
	for _, dimension := range append(activity.Dimensions(), "activity_tracking_started") {
		t.Run(dimension, func(t *testing.T) {
			base := usageEventInput{AccountID: "acc_1", RealmID: "rlm_1", AgentID: "agt_1", Dimension: dimension, Unit: activity.Unit(dimension), Quantity: 1, SubjectType: "activity", SubjectID: "opaque", IdempotencyKey: "opaque-key"}
			for _, tc := range []struct {
				unit     string
				quantity int64
				valid    bool
			}{{base.Unit, 1, true}, {base.Unit, 0, false}, {base.Unit, -1, false}, {"byte", 1, false}, {base.Unit, 2, base.Unit == "record"}} {
				in := base
				in.Unit = tc.unit
				in.Quantity = tc.quantity
				if err := validateUsageEventInput(&in); (err == nil) != tc.valid {
					t.Fatalf("unit=%s quantity=%d: %v", tc.unit, tc.quantity, err)
				}
			}
		})
	}
}
