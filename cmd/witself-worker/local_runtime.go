package main

import (
	"strconv"

	"github.com/witwave-ai/witself/internal/envconfig"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

func workerListenerAddresses(runtime runtimeconfig.Config, lookup func(string) (string, bool)) (string, string) {
	if runtime.Local {
		return runtime.HealthAddr, runtime.MetricsAddr
	}
	return envconfig.TrimmedOr(lookup, "WITSELF_HEALTH_ADDR", ":8081"), envconfig.TrimmedOr(lookup, "WITSELF_METRICS_ADDR", ":9090")
}

// workerConfigFromEnv applies local job selection before parsing job settings so
// hosted enable flags cannot turn on an excluded job or a default local job.
func workerConfigFromEnv(lookup func(string) (string, bool), environ []string) (runtimeconfig.Config, jobConfig, error) {
	runtime, err := runtimeconfig.FromEnv("worker", lookup, environ)
	if err != nil {
		return runtimeconfig.Config{}, jobConfig{}, err
	}
	if runtime.Local {
		enable := map[string]bool{
			avatarStyleRolloutEnabledEnv:              runtime.Jobs[avatarStyleRolloutJob],
			messageRateBucketCleanupEnabledEnv:        runtime.Jobs[messageRateBucketCleanupJob],
			agentEmailRateBucketCleanupEnabledEnv:     runtime.Jobs[agentEmailRateBucketCleanupJob],
			transcriptStorageReconcileEnv + "ENABLED": runtime.Jobs[transcriptStorageReconcileJob],
			agentEmailOutboundEnabledEnv:              false,
			transcriptRetentionEnabledEnv:             false,
			messageRetentionEnabledEnv:                false,
			agentEmailRetentionEnabledEnv:             false,
			accountPurgeEnabledEnv:                    false,
			"WITSELF_SUPPORT_TICKET_AGE_OUT_ENABLED":  false,
		}
		original := lookup
		lookup = func(name string) (string, bool) {
			if enabled, present := enable[name]; present {
				return strconv.FormatBool(enabled), true
			}
			return original(name)
		}
	}
	jobs, err := jobConfigFromEnv(lookup)
	return runtime, jobs, err
}
