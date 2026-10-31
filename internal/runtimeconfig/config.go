// Package runtimeconfig validates the explicitly selected local runtime configuration.
// An unset or empty selector leaves the hosted configuration untouched.
package runtimeconfig

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultMigrationWait bounds migration waits in the selected local runtime.
	DefaultMigrationWait = 120 * time.Second
	// JobRestartNever preserves the worker's exit-on-return behavior.
	JobRestartNever = "never"
	// JobRestartOnFailure restarts unexpected worker job returns.
	JobRestartOnFailure = "on-failure"
)

// Config contains local-only overrides. Hosted callers receive its zero value.
type Config struct {
	Local                            bool
	SyncSchema                       bool
	APIAddr, HealthAddr, MetricsAddr string
	DatabaseURL, SpoolDir, AccountID string
	Jobs                             map[string]bool
	MigrationWait                    time.Duration
	JobRestartPolicy                 string
}

// This is the complete ambient configuration refusal inventory. A trailing star
// matches every variable with that prefix, including future dispatch settings.
var forbiddenLocalEnvironment = [...]string{
	"WITSELF_PROVISION_TOKEN",
	"WITSELF_BACKUP_TOKEN",
	"WITSELF_BACKUP_VALIDATION_ENABLED",
	"WITSELF_BILLING_ENDPOINT",
	"WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED",
	"WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED",
	"WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_*",
	"WITSELF_AGENT_EMAIL_PROVIDER_EVENT_TOKEN",
}

var localJobEligibility = map[string]bool{
	"avatar_style_rollout":            true,
	"message_rate_bucket_cleanup":     true,
	"agent_email_rate_bucket_cleanup": true,
	"transcript_storage_reconcile":    true,
	"agent_email_outbound":            false,
	"account_purge":                   false,
	"transcript_retention":            false,
	"message_retention":               false,
	"agent_email_retention":           false,
	"support_ticket_age_out":          false,
}

// FromEnv validates a server or worker startup without consulting the process
// environment itself. environ discovers variable names for case-insensitive and
// wildcard refusals; its values never appear in diagnostics.
func FromEnv(role string, lookup func(string) (string, bool), environ []string) (Config, error) {
	runtime, _ := lookup("WITSELF_RUNTIME")
	const syncVariable = "WITSELF_LOCAL_SYNC_SCHEMA"
	syncValue, syncPresent := lookup(syncVariable)
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, syncVariable) && (runtime != "local" || name != syncVariable) {
			return Config{}, fmt.Errorf("%s is forbidden without explicit local runtime selection", name)
		}
	}
	if syncPresent && runtime != "local" {
		return Config{}, fmt.Errorf("%s is forbidden without explicit local runtime selection", syncVariable)
	}
	switch runtime {
	case "":
		return Config{}, nil
	case "local":
	default:
		return Config{}, fmt.Errorf("WITSELF_RUNTIME has an unsupported value")
	}
	if role != "server" && role != "worker" {
		return Config{}, fmt.Errorf("invalid runtime process role")
	}
	for _, forbidden := range forbiddenLocalEnvironment {
		if prefix, wildcard := strings.CutSuffix(forbidden, "*"); wildcard {
			for _, entry := range environ {
				name, _, _ := strings.Cut(entry, "=")
				if strings.HasPrefix(strings.ToUpper(name), prefix) {
					return Config{}, fmt.Errorf("%s is forbidden under the local runtime configuration", name)
				}
			}
		} else if _, present := lookup(forbidden); present {
			return Config{}, fmt.Errorf("%s is forbidden under the local runtime configuration", forbidden)
		} else {
			for _, entry := range environ {
				name, _, _ := strings.Cut(entry, "=")
				if strings.EqualFold(name, forbidden) {
					return Config{}, fmt.Errorf("%s is forbidden under the local runtime configuration", name)
				}
			}
		}
	}
	cfg := Config{Local: true, MigrationWait: DefaultMigrationWait, JobRestartPolicy: JobRestartNever}
	if syncPresent {
		if syncValue != "1" {
			return Config{}, fmt.Errorf("%s must be 1 when explicitly selected", syncVariable)
		}
		cfg.SyncSchema = true
	}
	if raw := trimmed(lookup, "WITSELF_LOCAL_MIGRATION_WAIT", ""); raw != "" {
		var err error
		cfg.MigrationWait, err = time.ParseDuration(raw)
		if err != nil || cfg.MigrationWait <= 0 {
			return Config{}, fmt.Errorf("WITSELF_LOCAL_MIGRATION_WAIT must be a positive duration")
		}
	}
	cfg.JobRestartPolicy = trimmed(lookup, "WITSELF_LOCAL_JOB_RESTART_POLICY", JobRestartNever)
	if cfg.JobRestartPolicy != JobRestartNever && cfg.JobRestartPolicy != JobRestartOnFailure {
		return Config{}, fmt.Errorf("WITSELF_LOCAL_JOB_RESTART_POLICY has an unsupported value")
	}
	cfg.DatabaseURL = trimmed(lookup, "WITSELF_DATABASE_URL", "")
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("WITSELF_DATABASE_URL is required under the local runtime configuration")
	}
	cfg.SpoolDir = trimmed(lookup, "WITSELF_LOCAL_SPOOL_DIR", "")
	if err := ValidateSpoolDir(cfg.SpoolDir); err != nil {
		return Config{}, err
	}
	if id, present := lookup("WITSELF_LOCAL_ACCOUNT_ID"); present {
		if strings.TrimSpace(id) == "" {
			return Config{}, fmt.Errorf("WITSELF_LOCAL_ACCOUNT_ID must identify the local bootstrap account")
		}
		cfg.AccountID = id
	}
	healthDefault, metricsDefault := "127.0.0.1:8081", "127.0.0.1:9090"
	if role == "worker" {
		healthDefault, metricsDefault = "127.0.0.1:8082", "127.0.0.1:9091"
	}
	if role == "server" {
		cfg.APIAddr = trimmed(lookup, "WITSELF_API_ADDR", "127.0.0.1:8080")
	}
	cfg.HealthAddr = trimmed(lookup, "WITSELF_HEALTH_ADDR", healthDefault)
	cfg.MetricsAddr = trimmed(lookup, "WITSELF_METRICS_ADDR", metricsDefault)
	for _, listener := range []struct{ name, addr string }{
		{"WITSELF_API_ADDR", cfg.APIAddr},
		{"WITSELF_HEALTH_ADDR", cfg.HealthAddr},
		{"WITSELF_METRICS_ADDR", cfg.MetricsAddr},
	} {
		if listener.name == "WITSELF_API_ADDR" && role == "worker" {
			continue
		}
		if !loopbackAddress(listener.addr) {
			return Config{}, fmt.Errorf("%s must be a loopback listener under the local runtime configuration", listener.name)
		}
	}
	if role == "worker" {
		raw, _ := lookup("WITSELF_LOCAL_JOBS")
		var err error
		cfg.Jobs, err = ParseJobs(raw)
		if err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

func trimmed(lookup func(string) (string, bool), name, fallback string) string {
	value, _ := lookup(name)
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func loopbackAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ParseJobs permits only explicitly selected jobs whose local role is known.
func ParseJobs(raw string) (map[string]bool, error) {
	jobs := make(map[string]bool)
	if strings.TrimSpace(raw) == "" {
		return jobs, nil
	}
	for _, entry := range strings.Split(raw, ",") {
		name := strings.TrimSpace(entry)
		allowed, known := localJobEligibility[name]
		if !known {
			return nil, fmt.Errorf("WITSELF_LOCAL_JOBS contains an unknown job")
		}
		if !allowed {
			return nil, fmt.Errorf("WITSELF_LOCAL_JOBS contains a job forbidden under the local runtime configuration")
		}
		jobs[name] = true
	}
	return jobs, nil
}
