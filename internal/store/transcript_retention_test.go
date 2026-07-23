package store

import (
	"testing"
	"time"
)

func TestTranscriptRetentionWorkerConfigBounds(t *testing.T) {
	defaults := DefaultTranscriptRetentionWorkerConfig()
	if defaults.BatchSize != 100 || defaults.Interval != 5*time.Minute ||
		defaults.BatchTimeout != 2*time.Minute ||
		defaults.LaneCount != defaultTranscriptRetentionWorkerLaneCount ||
		defaults.Mode != TranscriptRetentionModePreview {
		t.Fatalf("defaults = %+v", defaults)
	}
	if err := defaults.Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}
	for _, invalid := range []TranscriptRetentionWorkerConfig{
		{BatchSize: 0, Interval: time.Minute, BatchTimeout: time.Second, LaneCount: defaultTranscriptRetentionWorkerLaneCount, Mode: TranscriptRetentionModePreview},
		{BatchSize: maxTranscriptRetentionBatchSize + 1, Interval: time.Minute, BatchTimeout: time.Second, LaneCount: defaultTranscriptRetentionWorkerLaneCount, Mode: TranscriptRetentionModePreview},
		{BatchSize: 1, Interval: time.Minute - time.Nanosecond, BatchTimeout: time.Second, LaneCount: defaultTranscriptRetentionWorkerLaneCount, Mode: TranscriptRetentionModePreview},
		{BatchSize: 1, Interval: 24*time.Hour + time.Nanosecond, BatchTimeout: time.Second, LaneCount: defaultTranscriptRetentionWorkerLaneCount, Mode: TranscriptRetentionModePreview},
		{BatchSize: 1, Interval: time.Minute, BatchTimeout: minTranscriptRetentionBatchTimeout - time.Nanosecond, LaneCount: defaultTranscriptRetentionWorkerLaneCount, Mode: TranscriptRetentionModePreview},
		{BatchSize: 1, Interval: time.Minute, BatchTimeout: maxTranscriptRetentionBatchTimeout + time.Nanosecond, LaneCount: defaultTranscriptRetentionWorkerLaneCount, Mode: TranscriptRetentionModePreview},
		{BatchSize: 1, Interval: time.Minute, BatchTimeout: time.Second, LaneCount: defaultTranscriptRetentionWorkerLaneCount - 1, Mode: TranscriptRetentionModePreview},
		{BatchSize: 1, Interval: time.Minute, BatchTimeout: time.Second, LaneCount: defaultTranscriptRetentionWorkerLaneCount + 1, Mode: TranscriptRetentionModePreview},
		{BatchSize: 1, Interval: time.Minute, BatchTimeout: time.Second, LaneCount: defaultTranscriptRetentionWorkerLaneCount, Mode: "delete-everything"},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("config %+v unexpectedly valid", invalid)
		}
	}
}
