package testenv

import (
	"testing"
	"time"
)

func TestMigrationBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		work time.Duration
		want time.Duration
	}{
		{"zero", 0, time.Minute},
		{"below_floor", 3 * time.Second, time.Minute},
		{"proportional", 12 * time.Second, 2 * time.Minute},
		{"above_cap", 90 * time.Second, 10 * time.Minute},
		{"huge", 10 * time.Minute, 10 * time.Minute},
		{"negative", -time.Second, time.Minute},
		{"maximum_duration", time.Duration(1<<63 - 1), 10 * time.Minute},
		{"minimum_duration", time.Duration(-1 << 63), time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MigrationBudget(tc.work); got != tc.want {
				t.Fatalf("migration budget = %s, want %s", got, tc.want)
			}
		})
	}
}
