package main

import (
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/worker"
)

func TestSupportTicketAgeOutConfigFromEnv(t *testing.T) {
	jobs, err := jobConfigFromEnv(mapLookup(nil))
	if err != nil || jobs.supportTicketAgeOutEnabled || jobs.supportTicketAgeOut != store.DefaultSupportTicketAgeOutWorkerConfig() {
		t.Fatalf("default jobs=%+v err=%v", jobs, err)
	}
	if err := registerSupportTicketAgeOut(nil, nil, jobs); err != nil {
		t.Fatalf("disabled job touched registry: %v", err)
	}
	values := map[string]string{
		"WITSELF_SUPPORT_TICKET_AGE_OUT_ENABLED": "true", "WITSELF_SUPPORT_TICKET_AGE_OUT_AFTER": "48h",
		"WITSELF_SUPPORT_TICKET_AGE_OUT_BATCH_SIZE": "2", "WITSELF_SUPPORT_TICKET_AGE_OUT_INTERVAL": "2h",
		"WITSELF_SUPPORT_TICKET_AGE_OUT_BATCH_TIMEOUT": "20s",
	}
	jobs, err = jobConfigFromEnv(mapLookup(values))
	if err != nil || !jobs.supportTicketAgeOutEnabled || jobs.supportTicketAgeOut.After != 48*time.Hour || jobs.supportTicketAgeOut.BatchSize != 2 || jobs.supportTicketAgeOut.Interval != 2*time.Hour || jobs.supportTicketAgeOut.BatchTimeout != 20*time.Second {
		t.Fatalf("configured jobs=%+v err=%v", jobs, err)
	}
	registry := worker.NewRegistry()
	if err := registerSupportTicketAgeOut(registry, nil, jobs); err != nil {
		t.Fatal(err)
	}
	if err := registerSupportTicketAgeOut(registry, nil, jobs); err == nil {
		t.Fatal("enabled job not registered")
	}
	for key, value := range map[string]string{
		"WITSELF_SUPPORT_TICKET_AGE_OUT_ENABLED": "maybe", "WITSELF_SUPPORT_TICKET_AGE_OUT_AFTER": "23h",
		"WITSELF_SUPPORT_TICKET_AGE_OUT_BATCH_SIZE": "101", "WITSELF_SUPPORT_TICKET_AGE_OUT_INTERVAL": "30s",
		"WITSELF_SUPPORT_TICKET_AGE_OUT_BATCH_TIMEOUT": "999ms",
	} {
		if _, err := jobConfigFromEnv(mapLookup(map[string]string{key: value})); err == nil {
			t.Fatalf("accepted %s", key)
		}
	}
}
