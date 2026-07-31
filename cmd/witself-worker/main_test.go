package main

import (
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/worker"
)

func TestJobConfigFromEnvDefaultsAndOverrides(t *testing.T) {
	defaults, err := jobConfigFromEnv(mapLookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !defaults.avatarEnabled || defaults.avatar != store.DefaultAvatarStyleRolloutWorkerConfig() {
		t.Fatalf("avatar defaults = enabled %t config %#v", defaults.avatarEnabled, defaults.avatar)
	}
	if !defaults.messageRateBucketCleanupEnabled ||
		defaults.messageRateBucketCleanup != store.DefaultMessageRateBucketCleanupWorkerConfig() {
		t.Fatalf(
			"message rate bucket cleanup defaults = enabled %t config %#v",
			defaults.messageRateBucketCleanupEnabled,
			defaults.messageRateBucketCleanup,
		)
	}
	if defaults.retentionEnabled || defaults.retention != store.DefaultTranscriptRetentionWorkerConfig() {
		t.Fatalf("retention defaults = enabled %t config %#v", defaults.retentionEnabled, defaults.retention)
	}
	if defaults.retention.BatchTimeout != 2*time.Minute {
		t.Fatalf("retention batch timeout default = %s, want 2m", defaults.retention.BatchTimeout)
	}
	if defaults.messageRetentionEnabled ||
		defaults.messageRetention != store.DefaultMessageRetentionWorkerConfig() {
		t.Fatalf(
			"message retention defaults = enabled %t config %#v",
			defaults.messageRetentionEnabled,
			defaults.messageRetention,
		)
	}
	if defaults.agentEmailRetentionEnabled ||
		defaults.agentEmailRetention != store.DefaultAgentEmailRetentionWorkerConfig() {
		t.Fatalf(
			"agent-email retention defaults = enabled %t config %#v",
			defaults.agentEmailRetentionEnabled,
			defaults.agentEmailRetention,
		)
	}

	configured, err := jobConfigFromEnv(mapLookup(map[string]string{
		avatarStyleRolloutEnabledEnv:            "false",
		avatarStyleRolloutBatchSizeEnv:          "17",
		avatarStyleRolloutIntervalEnv:           "750ms",
		avatarStyleRolloutBatchTimeoutEnv:       "3s",
		messageRateBucketCleanupEnabledEnv:      "false",
		messageRateBucketCleanupBatchSizeEnv:    "5000",
		messageRateBucketCleanupIntervalEnv:     "10m",
		messageRateBucketCleanupBatchTimeoutEnv: "1m",
		transcriptRetentionEnabledEnv:           "true",
		transcriptRetentionModeEnv:              "ENFORCE",
		transcriptRetentionBatchSizeEnv:         "250",
		transcriptRetentionIntervalEnv:          "15m",
		transcriptRetentionBatchTimeoutEnv:      "90s",
		messageRetentionEnabledEnv:              "true",
		messageRetentionModeEnv:                 "ENFORCE",
		messageRetentionBatchSizeEnv:            "50",
		messageRetentionIntervalEnv:             "10m",
		messageRetentionBatchTimeoutEnv:         "75s",
		agentEmailRetentionEnabledEnv:           "true",
		agentEmailRetentionModeEnv:              "ENFORCE",
		agentEmailRetentionBatchSizeEnv:         "40",
		agentEmailRetentionIntervalEnv:          "12m",
		agentEmailRetentionBatchTimeoutEnv:      "80s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if configured.avatarEnabled || configured.avatar.BatchSize != 17 ||
		configured.avatar.Interval != 750*time.Millisecond ||
		configured.avatar.BatchTimeout != 3*time.Second {
		t.Fatalf("configured avatar = enabled %t config %#v", configured.avatarEnabled, configured.avatar)
	}
	if configured.messageRateBucketCleanupEnabled ||
		configured.messageRateBucketCleanup.BatchSize != 5000 ||
		configured.messageRateBucketCleanup.Interval != 10*time.Minute ||
		configured.messageRateBucketCleanup.BatchTimeout != time.Minute {
		t.Fatalf(
			"configured message rate bucket cleanup = enabled %t config %#v",
			configured.messageRateBucketCleanupEnabled,
			configured.messageRateBucketCleanup,
		)
	}
	if !configured.retentionEnabled ||
		configured.retention.Mode != store.TranscriptRetentionModeEnforce ||
		configured.retention.BatchSize != 250 ||
		configured.retention.Interval != 15*time.Minute ||
		configured.retention.BatchTimeout != 90*time.Second {
		t.Fatalf("configured retention = enabled %t config %#v", configured.retentionEnabled, configured.retention)
	}
	if !configured.messageRetentionEnabled ||
		configured.messageRetention.Mode != store.MessageRetentionModeEnforce ||
		configured.messageRetention.BatchSize != 50 ||
		configured.messageRetention.Interval != 10*time.Minute ||
		configured.messageRetention.BatchTimeout != 75*time.Second {
		t.Fatalf(
			"configured message retention = enabled %t config %#v",
			configured.messageRetentionEnabled,
			configured.messageRetention,
		)
	}
	if !configured.agentEmailRetentionEnabled ||
		configured.agentEmailRetention.Mode != store.AgentEmailRetentionModeEnforce ||
		configured.agentEmailRetention.BatchSize != 40 ||
		configured.agentEmailRetention.Interval != 12*time.Minute ||
		configured.agentEmailRetention.BatchTimeout != 80*time.Second {
		t.Fatalf(
			"configured agent-email retention = enabled %t config %#v",
			configured.agentEmailRetentionEnabled,
			configured.agentEmailRetention,
		)
	}
}

func TestJobConfigFromEnvRejectsNamedInvalidValues(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{avatarStyleRolloutEnabledEnv, "sometimes"},
		{avatarStyleRolloutBatchSizeEnv, "0"},
		{avatarStyleRolloutIntervalEnv, "99ms"},
		{avatarStyleRolloutBatchTimeoutEnv, "6m"},
		{messageRateBucketCleanupEnabledEnv, "sometimes"},
		{messageRateBucketCleanupBatchSizeEnv, "0"},
		{messageRateBucketCleanupBatchSizeEnv, "10001"},
		{messageRateBucketCleanupIntervalEnv, "59s"},
		{messageRateBucketCleanupIntervalEnv, "25h"},
		{messageRateBucketCleanupBatchTimeoutEnv, "999ms"},
		{messageRateBucketCleanupBatchTimeoutEnv, "6m"},
		{transcriptRetentionEnabledEnv, "sometimes"},
		{transcriptRetentionModeEnv, "destructive"},
		{transcriptRetentionBatchSizeEnv, "0"},
		{transcriptRetentionIntervalEnv, "30s"},
		{transcriptRetentionBatchTimeoutEnv, "999ms"},
		{transcriptRetentionBatchTimeoutEnv, "6m"},
		{messageRetentionEnabledEnv, "sometimes"},
		{messageRetentionModeEnv, "destructive"},
		{messageRetentionBatchSizeEnv, "0"},
		{messageRetentionBatchSizeEnv, "101"},
		{messageRetentionIntervalEnv, "30s"},
		{messageRetentionBatchTimeoutEnv, "9s"},
		{messageRetentionBatchTimeoutEnv, "6m"},
		{agentEmailRetentionEnabledEnv, "sometimes"},
		{agentEmailRetentionModeEnv, "destructive"},
		{agentEmailRetentionBatchSizeEnv, "0"},
		{agentEmailRetentionBatchSizeEnv, "101"},
		{agentEmailRetentionIntervalEnv, "30s"},
		{agentEmailRetentionBatchTimeoutEnv, "9s"},
		{agentEmailRetentionBatchTimeoutEnv, "6m"},
	}
	for _, test := range tests {
		t.Run(test.key+"="+test.value, func(t *testing.T) {
			_, err := jobConfigFromEnv(mapLookup(map[string]string{test.key: test.value}))
			if err == nil || !strings.Contains(err.Error(), test.key) {
				t.Fatalf("error = %v, want validation naming %s", err, test.key)
			}
		})
	}
}

func TestMessageRateBucketCleanupMetricResult(t *testing.T) {
	if got := messageRateBucketCleanupMetricResult(0); got != worker.RetentionResultNoWork {
		t.Fatalf("zero deleted rows metric = %q", got)
	}
	if got := messageRateBucketCleanupMetricResult(7); got != worker.RetentionResultSuccess {
		t.Fatalf("non-zero deleted rows metric = %q", got)
	}
}

func TestDatabaseDSNPreferenceAndListenerDefaults(t *testing.T) {
	lookup := mapLookup(map[string]string{
		"WITSELF_DATABASE_URL": " postgres://preferred ",
		"DATABASE_URL":         "postgres://fallback",
	})
	if got := dbDSN(lookup); got != "postgres://preferred" {
		t.Fatalf("dbDSN = %q", got)
	}
	if got := dbDSN(mapLookup(map[string]string{"DATABASE_URL": "postgres://fallback"})); got != "postgres://fallback" {
		t.Fatalf("fallback dbDSN = %q", got)
	}
	if got := envOr(mapLookup(nil), "WITSELF_HEALTH_ADDR", ":8081"); got != ":8081" {
		t.Fatalf("health default = %q", got)
	}
}

func TestRetentionMetricMappingContainsNoIdentifiers(t *testing.T) {
	result := store.TranscriptRetentionBatchResult{
		Scanned:                10,
		SkippedLocked:          2,
		ScanCapped:             true,
		Eligible:               7,
		EligibleScanCapped:     true,
		Deleted:                6,
		DeferredEvidence:       1,
		DeferredCuration:       2,
		DeferredScanCapped:     true,
		ReleasedCurationInputs: 3,
		DeletedCurationCursors: 4,
	}
	if got := retentionMetricResult(store.TranscriptRetentionBatchResult{}); got != worker.RetentionResultNoWork {
		t.Fatalf("zero result metric = %q", got)
	}
	if got := retentionMetricResult(result); got != worker.RetentionResultSuccess {
		t.Fatalf("non-zero result metric = %q", got)
	}
	counts := retentionMetricCounts(result)
	if counts.Scanned != 10 || counts.Deleted != 6 || counts.ReleasedCurationInputs != 3 ||
		!counts.ScanCapped || !counts.EligibleScanCapped || !counts.DeferredScanCapped {
		t.Fatalf("mapped counts = %#v", counts)
	}
}

func TestMessageRetentionMetricMappingContainsNoIdentifiers(t *testing.T) {
	result := store.MessageRetentionBatchResult{
		Scanned:          8,
		SkippedLocked:    1,
		ScanCapped:       true,
		EligibleThreads:  6,
		DeletedThreads:   5,
		DeletedMessages:  13,
		DeferredEvidence: 1,
		DeferredActive:   2,
		DeferredLocked:   3,
		DeferredOversize: 4,
		DeferredBudget:   5,
		RepairedActivity: 6,
		LaneAdvanced:     true,
	}
	if got := messageRetentionMetricResult(store.MessageRetentionBatchResult{
		LaneAdvanced: true,
	}); got != worker.RetentionResultNoWork {
		t.Fatalf("empty message result metric = %q", got)
	}
	if got := messageRetentionMetricResult(result); got != worker.RetentionResultSuccess {
		t.Fatalf("non-empty message result metric = %q", got)
	}
	counts := messageRetentionMetricCounts(result)
	if counts.Scanned != 8 || counts.DeletedThreads != 5 ||
		counts.DeletedMessages != 13 || counts.DeferredActive != 2 ||
		counts.DeferredLocked != 3 || counts.DeferredOversize != 4 ||
		counts.DeferredBudget != 5 || counts.RepairedActivity != 6 ||
		!counts.ScanCapped {
		t.Fatalf("mapped message counts = %#v", counts)
	}
}

func TestAgentEmailRetentionMetricMappingContainsNoIdentifiers(t *testing.T) {
	result := store.AgentEmailRetentionBatchResult{
		Scanned:               8,
		SkippedLocked:         1,
		Eligible:              6,
		Deleted:               5,
		DeletedRawBytes:       8192,
		DeferredActive:        2,
		DeferredLocked:        3,
		DeferredOversize:      4,
		DeferredBudget:        5,
		ClearedDuplicateLinks: 6,
		DeletedCanaryProofs:   1,
		ScanCapped:            true,
		LaneAdvanced:          true,
	}
	if got := agentEmailRetentionMetricResult(
		store.AgentEmailRetentionBatchResult{LaneAdvanced: true},
	); got != worker.RetentionResultNoWork {
		t.Fatalf("empty agent-email result metric = %q", got)
	}
	if got := agentEmailRetentionMetricResult(result); got != worker.RetentionResultSuccess {
		t.Fatalf("non-empty agent-email result metric = %q", got)
	}
	counts := agentEmailRetentionMetricCounts(result)
	if counts.Scanned != 8 ||
		counts.Deleted != 5 ||
		counts.DeletedRawBytes != 8192 ||
		counts.DeferredActive != 2 ||
		counts.DeferredLocked != 3 ||
		counts.DeferredOversize != 4 ||
		counts.DeferredBudget != 5 ||
		counts.ClearedDuplicateLinks != 6 ||
		counts.DeletedCanaryProofs != 1 ||
		!counts.ScanCapped {
		t.Fatalf("mapped agent-email counts = %#v", counts)
	}
}

func TestRunCommandSurface(t *testing.T) {
	if code := run([]string{"not-a-command"}); code != 2 {
		t.Fatalf("unknown command exit = %d, want 2", code)
	}
	if code := run([]string{"help"}); code != 0 {
		t.Fatalf("help exit = %d, want 0", code)
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
