package store

import (
	"testing"
	"time"
)

func TestTranscriptRetentionWorkerConfigBounds(t *testing.T) {
	defaults := DefaultTranscriptRetentionWorkerConfig()
	if defaults.BatchSize != 100 || defaults.Interval != 5*time.Minute ||
		defaults.Mode != TranscriptRetentionModePreview {
		t.Fatalf("defaults = %+v", defaults)
	}
	if err := defaults.Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}
	for _, invalid := range []TranscriptRetentionWorkerConfig{
		{BatchSize: 0, Interval: time.Minute, Mode: TranscriptRetentionModePreview},
		{BatchSize: maxTranscriptRetentionBatchSize + 1, Interval: time.Minute, Mode: TranscriptRetentionModePreview},
		{BatchSize: 1, Interval: time.Minute - time.Nanosecond, Mode: TranscriptRetentionModePreview},
		{BatchSize: 1, Interval: 24*time.Hour + time.Nanosecond, Mode: TranscriptRetentionModePreview},
		{BatchSize: 1, Interval: time.Minute, Mode: "delete-everything"},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("config %+v unexpectedly valid", invalid)
		}
	}
}
