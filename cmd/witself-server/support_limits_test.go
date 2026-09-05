package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestSupportTicketRateLimitConfigFromEnv(t *testing.T) {
	lookup := func(values map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	}
	cfg, err := supportTicketRateLimitFromEnv(lookup(nil))
	if err != nil || cfg != store.DefaultSupportTicketRateLimitConfig() {
		t.Fatalf("defaults=%+v err=%v", cfg, err)
	}
	cfg, err = supportTicketRateLimitFromEnv(lookup(map[string]string{"WITSELF_SUPPORT_TICKET_RATE_LIMIT": "2", "WITSELF_SUPPORT_TICKET_RATE_WINDOW": "2m"}))
	if err != nil || cfg.Limit != 2 || cfg.Window != 2*time.Minute {
		t.Fatalf("overrides=%+v err=%v", cfg, err)
	}
	for key, values := range map[string][]string{
		"WITSELF_SUPPORT_TICKET_RATE_LIMIT":  {"0", "1001", "private-invalid-value"},
		"WITSELF_SUPPORT_TICKET_RATE_WINDOW": {"0s", "25h", "1500ms", "private-invalid-value"},
	} {
		for _, value := range values {
			_, err := supportTicketRateLimitFromEnv(lookup(map[string]string{key: value}))
			if err == nil || strings.Contains(err.Error(), "private-invalid-value") {
				t.Fatalf("config error=%v", err)
			}
		}
	}
}

func TestMapSupportRateLimitError(t *testing.T) {
	got := mapSupportError(fmt.Errorf("wrapped: %w", &store.SupportRateLimitError{Limit: 10, WindowSeconds: 60, RetryAfterSeconds: 60}))
	var detail *server.SupportRateLimitError
	if !errors.Is(got, server.ErrSupportRateLimited) || !errors.As(got, &detail) || detail.Limit != 10 || detail.WindowSeconds != 60 || detail.RetryAfterSeconds != 60 {
		t.Fatalf("mapped=%T %v", got, got)
	}
	if !errors.Is(mapSupportError(store.ErrSupportRateLimited), server.ErrSupportRateLimited) {
		t.Fatal("sentinel mapping missing")
	}
}
