package store

import (
	"testing"
	"time"
)

func TestAvatarStyleRolloutWorkerConfigBounds(t *testing.T) {
	defaults := DefaultAvatarStyleRolloutWorkerConfig()
	if defaults.BatchSize != 100 || defaults.Interval != 2*time.Second ||
		defaults.BatchTimeout != 30*time.Second {
		t.Fatalf("defaults = %#v", defaults)
	}
	if err := defaults.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []AvatarStyleRolloutWorkerConfig{
		{BatchSize: 0, Interval: time.Second, BatchTimeout: time.Second},
		{BatchSize: maxAvatarStyleRolloutBatchSize + 1, Interval: time.Second, BatchTimeout: time.Second},
		{BatchSize: 1, Interval: minAvatarStyleRolloutInterval - time.Nanosecond, BatchTimeout: time.Second},
		{BatchSize: 1, Interval: maxAvatarStyleRolloutInterval + time.Nanosecond, BatchTimeout: time.Second},
		{BatchSize: 1, Interval: time.Second, BatchTimeout: minAvatarStyleRolloutBatchTimeout - time.Nanosecond},
		{BatchSize: 1, Interval: time.Second, BatchTimeout: maxAvatarStyleRolloutBatchTimeout + time.Nanosecond},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid config accepted: %#v", invalid)
		}
	}
}
