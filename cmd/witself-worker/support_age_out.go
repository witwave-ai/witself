package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/worker"
)

const supportTicketAgeOutJob = "support_ticket_age_out"

func registerSupportTicketAgeOut(registry *worker.Registry, st *store.Store, jobs jobConfig) error {
	if !jobs.supportTicketAgeOutEnabled {
		return nil
	}
	return registry.Register(worker.Job{
		Name: supportTicketAgeOutJob,
		Run: func(ctx context.Context) error {
			return st.RunSupportTicketAgeOutWorker(ctx, jobs.supportTicketAgeOut,
				func(resolved int64) {
					if resolved > 0 {
						fmt.Fprintf(os.Stderr, "witself-worker: support ticket age-out: resolved=%d\n", resolved)
					}
				},
				func(error) {
					registry.Metrics().RecordJobFailure(supportTicketAgeOutJob)
					fmt.Fprintln(os.Stderr, "witself-worker: support ticket age-out batch failed")
				})
		},
	})
}

func supportTicketAgeOutFromEnv(lookup func(string) (string, bool)) (bool, store.SupportTicketAgeOutWorkerConfig, error) {
	cfg := store.DefaultSupportTicketAgeOutWorkerConfig()
	enabled, err := boolEnv(lookup, "WITSELF_SUPPORT_TICKET_AGE_OUT_ENABLED", false)
	if err != nil {
		return false, cfg, err
	}
	if raw, ok := lookup("WITSELF_SUPPORT_TICKET_AGE_OUT_BATCH_SIZE"); ok {
		cfg.BatchSize, err = parseIntEnv("WITSELF_SUPPORT_TICKET_AGE_OUT_BATCH_SIZE", raw)
		if err != nil {
			return false, cfg, err
		}
	}
	for key, target := range map[string]*time.Duration{
		"WITSELF_SUPPORT_TICKET_AGE_OUT_AFTER":         &cfg.After,
		"WITSELF_SUPPORT_TICKET_AGE_OUT_INTERVAL":      &cfg.Interval,
		"WITSELF_SUPPORT_TICKET_AGE_OUT_BATCH_TIMEOUT": &cfg.BatchTimeout,
	} {
		if raw, ok := lookup(key); ok {
			*target, err = parseDurationEnv(key, raw)
			if err != nil {
				return false, cfg, err
			}
		}
	}
	if err := cfg.Validate(); err != nil {
		return false, cfg, err
	}
	return enabled, cfg, nil
}
