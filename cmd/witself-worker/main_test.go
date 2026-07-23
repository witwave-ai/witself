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
	if defaults.retentionEnabled || defaults.retention != store.DefaultTranscriptRetentionWorkerConfig() {
		t.Fatalf("retention defaults = enabled %t config %#v", defaults.retentionEnabled, defaults.retention)
	}
	if defaults.retention.BatchTimeout != 2*time.Minute {
		t.Fatalf("retention batch timeout default = %s, want 2m", defaults.retention.BatchTimeout)
	}

	configured, err := jobConfigFromEnv(mapLookup(map[string]string{
		avatarStyleRolloutEnabledEnv:       "false",
		avatarStyleRolloutBatchSizeEnv:     "17",
		avatarStyleRolloutIntervalEnv:      "750ms",
		avatarStyleRolloutBatchTimeoutEnv:  "3s",
		transcriptRetentionEnabledEnv:      "true",
		transcriptRetentionModeEnv:         "ENFORCE",
		transcriptRetentionBatchSizeEnv:    "250",
		transcriptRetentionIntervalEnv:     "15m",
		transcriptRetentionBatchTimeoutEnv: "90s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if configured.avatarEnabled || configured.avatar.BatchSize != 17 ||
		configured.avatar.Interval != 750*time.Millisecond ||
		configured.avatar.BatchTimeout != 3*time.Second {
		t.Fatalf("configured avatar = enabled %t config %#v", configured.avatarEnabled, configured.avatar)
	}
	if !configured.retentionEnabled ||
		configured.retention.Mode != store.TranscriptRetentionModeEnforce ||
		configured.retention.BatchSize != 250 ||
		configured.retention.Interval != 15*time.Minute ||
		configured.retention.BatchTimeout != 90*time.Second {
		t.Fatalf("configured retention = enabled %t config %#v", configured.retentionEnabled, configured.retention)
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
		{transcriptRetentionEnabledEnv, "sometimes"},
		{transcriptRetentionModeEnv, "destructive"},
		{transcriptRetentionBatchSizeEnv, "0"},
		{transcriptRetentionIntervalEnv, "30s"},
		{transcriptRetentionBatchTimeoutEnv, "999ms"},
		{transcriptRetentionBatchTimeoutEnv, "6m"},
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
