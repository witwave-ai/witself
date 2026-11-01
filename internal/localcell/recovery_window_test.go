package localcell

import (
	"math"
	"testing"
	"time"
)

func TestPostgresRecoveryStartWindow(t *testing.T) {
	const epoch int64 = 1000
	for _, test := range []struct {
		name    string
		epoch   int64
		created int64
		want    bool
	}{
		{name: "before_lower_boundary", epoch: epoch, created: epoch - 61},
		{name: "at_lower_boundary", epoch: epoch, created: epoch - 60, want: true},
		{name: "at_upper_boundary", epoch: epoch, created: epoch + 1, want: true},
		{name: "after_upper_boundary", epoch: epoch, created: epoch + 2},
		{name: "maximum_epoch", epoch: math.MaxInt64, created: math.MaxInt64, want: true},
		{name: "maximum_epoch_before_lower_boundary", epoch: math.MaxInt64, created: math.MaxInt64 - 61},
		{name: "minimum_creation_time", epoch: 1, created: math.MinInt64},
		{name: "invalid_epoch", epoch: 0, created: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := postgresRecoveryStartMatches(time.Unix(test.created, 0), test.epoch); got != test.want {
				t.Errorf("recovery window acceptance: got %v, want %v", got, test.want)
			}
		})
	}
}
