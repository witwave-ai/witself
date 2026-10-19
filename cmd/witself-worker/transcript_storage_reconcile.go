package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/worker"
)

const transcriptStorageReconcileJob = "transcript_storage_reconcile"
const transcriptStorageReconcileEnv = "WITSELF_TRANSCRIPT_STORAGE_RECONCILE_"

func transcriptStorageReconcileFromEnv(lookup func(string) (string, bool)) (bool, store.TranscriptStorageReconcileConfig, error) {
	cfg := store.DefaultTranscriptStorageReconcileConfig()
	enabled, err := boolEnv(lookup, transcriptStorageReconcileEnv+"ENABLED", false)
	if err != nil {
		return false, cfg, err
	}
	for _, setting := range []struct {
		name   string
		target *time.Duration
	}{{"INTERVAL", &cfg.Interval}, {"UNIT_TIMEOUT", &cfg.UnitTimeout}} {
		if raw, ok := lookup(transcriptStorageReconcileEnv + setting.name); ok {
			*setting.target, err = parseDurationEnv(transcriptStorageReconcileEnv+setting.name, raw)
			if err != nil {
				return false, cfg, err
			}
		}
	}
	if raw, ok := lookup(transcriptStorageReconcileEnv + "PAGE_ENTRIES"); ok {
		cfg.PageEntries, err = parseIntEnv(transcriptStorageReconcileEnv+"PAGE_ENTRIES", raw)
		if err != nil {
			return false, cfg, err
		}
	}
	if raw, ok := lookup(transcriptStorageReconcileEnv + "DUTY_CYCLE"); ok {
		cfg.DutyCycle, err = strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return false, cfg, fmt.Errorf("%sDUTY_CYCLE must be a number", transcriptStorageReconcileEnv)
		}
	}
	if err := cfg.Validate(); err != nil {
		return false, cfg, fmt.Errorf("%s configuration: %w", transcriptStorageReconcileEnv, err)
	}
	return enabled, cfg, nil
}

func registerTranscriptStorageReconcile(registry *worker.Registry, st *store.Store, jobs jobConfig, output io.Writer) error {
	if !jobs.transcriptStorageReconcileEnabled {
		return nil
	}
	cfg := jobs.transcriptStorageReconcile
	metrics := registry.Metrics()
	if err := registry.Register(worker.Job{
		Name: transcriptStorageReconcileJob,
		Run: func(ctx context.Context) error {
			return st.RunTranscriptStorageReconcileWorker(ctx, cfg,
				func(result store.TranscriptStorageReconcilePassResult) {
					reportTranscriptStorageReconcile(metrics, output, result)
				},
				func(status store.TranscriptStorageReconcileStatus) {
					metrics.ObserveTranscriptStorageReconcileStatus(status.UnverifiedEligible, status.UnverifiedDeferred, status.Stuck)
				})
		},
	}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(output, "witself-worker: transcript storage reconcile enabled (interval %s, duty %.2f, page %d, unit timeout %s)\n", cfg.Interval, cfg.DutyCycle, cfg.PageEntries, cfg.UnitTimeout)
	return nil
}

func reportTranscriptStorageReconcile(metrics *worker.Metrics, output io.Writer, result store.TranscriptStorageReconcilePassResult) {
	counts := worker.TranscriptStorageReconcileCounts{
		Corrected: result.Corrected, Confirmed: result.Confirmed, Raced: result.Raced, Busy: result.Busy,
		Anomaly: result.Anomaly, Timeout: result.Timeout, Error: result.Error, Skipped: result.Skipped,
		Entries: result.Entries, Stuck: result.Stuck,
	}
	metrics.ObserveTranscriptStorageReconcilePass(counts)
	if result.Timeout > 0 || result.Error > 0 || result.Anomaly > 0 || result.Err != nil {
		metrics.RecordJobFailure(transcriptStorageReconcileJob)
	}
	if result.Corrected+result.Confirmed+result.Raced+result.Busy+result.Anomaly+result.Timeout+result.Error+result.Skipped > 0 {
		_, _ = fmt.Fprintf(output, "witself-worker: transcript storage reconcile: corrected=%d confirmed=%d raced=%d busy=%d anomaly=%d timeout=%d error=%d skipped=%d entries=%d stuck=%d elapsed=%s\n", result.Corrected, result.Confirmed, result.Raced, result.Busy, result.Anomaly, result.Timeout, result.Error, result.Skipped, result.Entries, result.Stuck, result.Elapsed)
	}
}
