package main

import (
	"encoding/base64"
	"io"
	"os"
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
	if !defaults.agentEmailRateBucketCleanupEnabled ||
		defaults.agentEmailRateBucketCleanup != store.DefaultAgentEmailRateBucketCleanupWorkerConfig() {
		t.Fatalf(
			"agent-email rate bucket cleanup defaults = enabled %t config %#v",
			defaults.agentEmailRateBucketCleanupEnabled,
			defaults.agentEmailRateBucketCleanup,
		)
	}
	if defaults.agentEmailOutboundEnabled ||
		defaults.agentEmailOutbound != defaultAgentEmailOutboundWorkerConfig() {
		t.Fatalf(
			"outbound agent-email defaults = enabled %t config %#v",
			defaults.agentEmailOutboundEnabled,
			defaults.agentEmailOutbound,
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
	if defaults.accountPurgeEnabled ||
		defaults.accountPurge != store.DefaultAccountPurgeWorkerConfig() {
		t.Fatalf(
			"account purge defaults = enabled %t config %#v",
			defaults.accountPurgeEnabled,
			defaults.accountPurge,
		)
	}

	configured, err := jobConfigFromEnv(mapLookup(map[string]string{
		avatarStyleRolloutEnabledEnv:               "false",
		avatarStyleRolloutBatchSizeEnv:             "17",
		avatarStyleRolloutIntervalEnv:              "750ms",
		avatarStyleRolloutBatchTimeoutEnv:          "3s",
		messageRateBucketCleanupEnabledEnv:         "false",
		messageRateBucketCleanupBatchSizeEnv:       "5000",
		messageRateBucketCleanupIntervalEnv:        "10m",
		messageRateBucketCleanupBatchTimeoutEnv:    "1m",
		agentEmailRateBucketCleanupEnabledEnv:      "false",
		agentEmailRateBucketCleanupBatchSizeEnv:    "4000",
		agentEmailRateBucketCleanupIntervalEnv:     "12m",
		agentEmailRateBucketCleanupBatchTimeoutEnv: "2m",
		agentEmailOutboundEnabledEnv:               "true",
		agentEmailOutboundDispatchEndpointEnv:      "https://send.example.test/v1/dispatch",
		agentEmailOutboundDispatchAudienceEnv:      "witself-agent-email-send",
		agentEmailOutboundDispatchKeyIDEnv:         "founder-cell",
		agentEmailOutboundDispatchPrivateKeyEnv:    base64.StdEncoding.EncodeToString(make([]byte, 64)),
		agentEmailOutboundBatchSizeEnv:             "9",
		agentEmailOutboundIntervalEnv:              "3s",
		agentEmailOutboundBatchTimeoutEnv:          "40s",
		agentEmailOutboundProviderTimeoutEnv:       "15s",
		transcriptRetentionEnabledEnv:              "true",
		transcriptRetentionModeEnv:                 "ENFORCE",
		transcriptRetentionBatchSizeEnv:            "250",
		transcriptRetentionIntervalEnv:             "15m",
		transcriptRetentionBatchTimeoutEnv:         "90s",
		messageRetentionEnabledEnv:                 "true",
		messageRetentionModeEnv:                    "ENFORCE",
		messageRetentionBatchSizeEnv:               "50",
		messageRetentionIntervalEnv:                "10m",
		messageRetentionBatchTimeoutEnv:            "75s",
		agentEmailRetentionEnabledEnv:              "true",
		agentEmailRetentionModeEnv:                 "ENFORCE",
		agentEmailRetentionBatchSizeEnv:            "40",
		agentEmailRetentionIntervalEnv:             "12m",
		agentEmailRetentionBatchTimeoutEnv:         "80s",
		accountPurgeEnabledEnv:                     "true",
		accountPurgeModeEnv:                        "ENFORCE",
		accountPurgeBatchSizeEnv:                   "35",
		accountPurgeIntervalEnv:                    "20m",
		accountPurgeBatchTimeoutEnv:                "70s",
		accountPurgeGraceEnv:                       "1080h",
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
	if configured.agentEmailRateBucketCleanupEnabled ||
		configured.agentEmailRateBucketCleanup.BatchSize != 4000 ||
		configured.agentEmailRateBucketCleanup.Interval != 12*time.Minute ||
		configured.agentEmailRateBucketCleanup.BatchTimeout != 2*time.Minute {
		t.Fatalf(
			"configured agent-email rate bucket cleanup = enabled %t config %#v",
			configured.agentEmailRateBucketCleanupEnabled,
			configured.agentEmailRateBucketCleanup,
		)
	}
	if !configured.agentEmailOutboundEnabled ||
		configured.agentEmailOutbound.BatchSize != 9 ||
		configured.agentEmailOutbound.Interval != 3*time.Second ||
		configured.agentEmailOutbound.BatchTimeout != 40*time.Second ||
		configured.agentEmailOutbound.ProviderTimeout != 15*time.Second ||
		configured.agentEmailOutboundClient.Endpoint != "https://send.example.test/v1/dispatch" ||
		configured.agentEmailOutboundClient.KeyID != "founder-cell" ||
		len(configured.agentEmailOutboundClient.PrivateKey) != 64 {
		t.Fatalf(
			"outbound agent-email configured = enabled %t config %#v client endpoint=%q key_id=%q key_bytes=%d",
			configured.agentEmailOutboundEnabled,
			configured.agentEmailOutbound,
			configured.agentEmailOutboundClient.Endpoint,
			configured.agentEmailOutboundClient.KeyID,
			len(configured.agentEmailOutboundClient.PrivateKey),
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
	if !configured.accountPurgeEnabled ||
		configured.accountPurge.Mode != store.AccountPurgeModeEnforce ||
		configured.accountPurge.BatchSize != 35 ||
		configured.accountPurge.Interval != 20*time.Minute ||
		configured.accountPurge.BatchTimeout != 70*time.Second ||
		configured.accountPurge.Grace != 1080*time.Hour {
		t.Fatalf(
			"configured account purge = enabled %t config %#v",
			configured.accountPurgeEnabled,
			configured.accountPurge,
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
		{agentEmailRateBucketCleanupEnabledEnv, "sometimes"},
		{agentEmailRateBucketCleanupBatchSizeEnv, "0"},
		{agentEmailRateBucketCleanupBatchSizeEnv, "10001"},
		{agentEmailRateBucketCleanupIntervalEnv, "59s"},
		{agentEmailRateBucketCleanupIntervalEnv, "25h"},
		{agentEmailRateBucketCleanupBatchTimeoutEnv, "999ms"},
		{agentEmailRateBucketCleanupBatchTimeoutEnv, "6m"},
		{agentEmailOutboundEnabledEnv, "sometimes"},
		{agentEmailOutboundBatchSizeEnv, "0"},
		{agentEmailOutboundBatchSizeEnv, "101"},
		{agentEmailOutboundIntervalEnv, "99ms"},
		{agentEmailOutboundIntervalEnv, "6m"},
		{agentEmailOutboundBatchTimeoutEnv, "999ms"},
		{agentEmailOutboundBatchTimeoutEnv, "6m"},
		{agentEmailOutboundProviderTimeoutEnv, "999ms"},
		{agentEmailOutboundProviderTimeoutEnv, "61s"},
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
		{accountPurgeEnabledEnv, "sometimes"},
		{accountPurgeModeEnv, "destructive"},
		{accountPurgeBatchSizeEnv, "0"},
		{accountPurgeBatchSizeEnv, "1001"},
		{accountPurgeIntervalEnv, "30s"},
		{accountPurgeIntervalEnv, "25h"},
		{accountPurgeBatchTimeoutEnv, "999ms"},
		{accountPurgeBatchTimeoutEnv, "6m"},
		{accountPurgeGraceEnv, "59s"},
		{accountPurgeGraceEnv, "8761h"},
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

func TestJobConfigFromEnvRequiresCompleteOutboundDispatchIdentity(t *testing.T) {
	base := map[string]string{
		agentEmailOutboundEnabledEnv:            "true",
		agentEmailOutboundDispatchEndpointEnv:   "https://send.example.test/v1/dispatch",
		agentEmailOutboundDispatchAudienceEnv:   "witself-agent-email-send",
		agentEmailOutboundDispatchKeyIDEnv:      "founder-cell",
		agentEmailOutboundDispatchPrivateKeyEnv: base64.StdEncoding.EncodeToString(make([]byte, 64)),
	}
	for _, missing := range []string{
		agentEmailOutboundDispatchEndpointEnv,
		agentEmailOutboundDispatchKeyIDEnv,
		agentEmailOutboundDispatchPrivateKeyEnv,
	} {
		t.Run(missing, func(t *testing.T) {
			values := make(map[string]string, len(base))
			for key, value := range base {
				values[key] = value
			}
			delete(values, missing)
			_, err := jobConfigFromEnv(mapLookup(values))
			if err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("error = %v, want missing key %s", err, missing)
			}
		})
	}

	secret := "this-is-not-a-private-key"
	values := make(map[string]string, len(base))
	for key, value := range base {
		values[key] = value
	}
	values[agentEmailOutboundDispatchPrivateKeyEnv] = secret
	_, err := jobConfigFromEnv(mapLookup(values))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("invalid private-key error leaked input: %v", err)
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

func TestAgentEmailRateBucketCleanupMetricResult(t *testing.T) {
	if got := agentEmailRateBucketCleanupMetricResult(0); got != worker.RetentionResultNoWork {
		t.Fatalf("zero deleted rows metric = %q", got)
	}
	if got := agentEmailRateBucketCleanupMetricResult(7); got != worker.RetentionResultSuccess {
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
		ScannedOutbound:       7,
		EligibleOutbound:      6,
		DeletedOutbound:       5,
		DeletedOutboundBytes:  4096,
		ScannedProviderEvents: 4,
		DeletedProviderEvents: 3,
		ScannedSuppressions:   2,
		EligibleSuppressions:  2,
		DeletedSuppressions:   1,
		ScanCapped:            true,
		LaneAdvanced:          true,
	}
	if got := agentEmailRetentionMetricResult(
		store.AgentEmailRetentionBatchResult{LaneAdvanced: true},
	); got != worker.RetentionResultNoWork {
		t.Fatalf("empty agent-email result metric = %q", got)
	}
	for name, outboundOnly := range map[string]store.AgentEmailRetentionBatchResult{
		"outbound message":  {ScannedOutbound: 1},
		"outbound bytes":    {DeletedOutboundBytes: 1},
		"provider receipt":  {ScannedProviderEvents: 1},
		"recipient cleanup": {ScannedSuppressions: 1},
	} {
		if got := agentEmailRetentionMetricResult(outboundOnly); got != worker.RetentionResultSuccess {
			t.Errorf("%s-only agent-email result metric = %q", name, got)
		}
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
		counts.ScannedOutbound != 7 ||
		counts.EligibleOutbound != 6 ||
		counts.DeletedOutbound != 5 ||
		counts.DeletedOutboundBytes != 4096 ||
		counts.ScannedProviderEvents != 4 ||
		counts.DeletedProviderEvents != 3 ||
		counts.ScannedSuppressions != 2 ||
		counts.EligibleSuppressions != 2 ||
		counts.DeletedSuppressions != 1 ||
		!counts.ScanCapped {
		t.Fatalf("mapped agent-email counts = %#v", counts)
	}
}

func TestAccountPurgeMetricMappingAggregatesTables(t *testing.T) {
	empty := store.AccountPurgeBatchResult{
		DeletedByTable: map[string]int64{"account_private": 0},
	}
	if got := accountPurgeMetricResult(empty); got != worker.RetentionResultNoWork {
		t.Fatalf("empty account purge result metric = %q", got)
	}
	deletedOnly := store.AccountPurgeBatchResult{
		DeletedByTable: map[string]int64{"memories": 1},
	}
	if got := accountPurgeMetricResult(deletedOnly); got != worker.RetentionResultSuccess {
		t.Fatalf("deleted-only account purge result metric = %q", got)
	}
	result := store.AccountPurgeBatchResult{
		Scanned:                     8,
		SkippedLocked:               1,
		Eligible:                    6,
		PurgedAccounts:              4,
		DeletedByTable:              map[string]int64{"memories": 7, "facts": 11, "ignored": -1},
		DeferredVaultLifecycle:      2,
		AttachmentInvariantFailures: 3,
		ProvisionReceiptScrubs:      1,
	}
	if got := accountPurgeMetricResult(result); got != worker.RetentionResultSuccess {
		t.Fatalf("non-empty account purge result metric = %q", got)
	}
	counts := accountPurgeMetricCounts(result)
	if counts.Scanned != 8 ||
		counts.SkippedLocked != 1 ||
		counts.Eligible != 6 ||
		counts.PurgedAccounts != 4 ||
		counts.DeletedRows != 18 ||
		counts.DeferredVaultLifecycle != 2 ||
		counts.AttachmentInvariantFailures != 3 ||
		counts.ProvisionReceiptScrubs != 1 {
		t.Fatalf("mapped account purge counts = %#v", counts)
	}
}

func TestLogAgentEmailRetentionResultIncludesOutboundCleanup(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStderr := os.Stderr
	os.Stderr = writeEnd
	logAgentEmailRetentionResult(store.AgentEmailRetentionModeEnforce,
		store.AgentEmailRetentionBatchResult{
			DeletedOutbound:       2,
			DeletedOutboundBytes:  2048,
			DeletedProviderEvents: 3,
			DeletedSuppressions:   4,
		})
	os.Stderr = originalStderr
	if err := writeEnd.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(readEnd)
	if err != nil {
		t.Fatal(err)
	}
	if err := readEnd.Close(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"deleted_outbound=2",
		"deleted_outbound_bytes=2048",
		"deleted_provider_events=3",
		"deleted_suppressions=4",
	} {
		if !strings.Contains(string(output), want) {
			t.Errorf("agent-email retention log missing %q: %s", want, output)
		}
	}
}

func TestLogAccountPurgeResultContainsCountsOnly(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStderr := os.Stderr
	os.Stderr = writeEnd
	logAccountPurgeResult(store.AccountPurgeModeEnforce, store.AccountPurgeBatchResult{
		Scanned:                     3,
		PurgedAccounts:              1,
		DeletedByTable:              map[string]int64{"account_private_table": 4},
		DeferredVaultLifecycle:      2,
		AttachmentInvariantFailures: 3,
		ProvisionReceiptScrubs:      1,
	})
	os.Stderr = originalStderr
	if err := writeEnd.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(readEnd)
	if err != nil {
		t.Fatal(err)
	}
	if err := readEnd.Close(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"mode=enforce",
		"scanned=3",
		"purged_accounts=1",
		"deleted_rows=4",
		"deferred_vault_lifecycle=2",
		"attachment_invariant_failures=3",
		"provision_receipt_scrubs=1",
	} {
		if !strings.Contains(string(output), want) {
			t.Errorf("account purge log missing %q: %s", want, output)
		}
	}
	for _, forbidden := range []string{"account_private_table", "account_id", "realm_id"} {
		if strings.Contains(string(output), forbidden) {
			t.Errorf("account purge log exposed %q: %s", forbidden, output)
		}
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
