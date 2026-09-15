package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/store"
)

func supportTicketRateLimitFromEnv(lookup func(string) (string, bool)) (store.SupportTicketRateLimitConfig, error) {
	cfg := store.DefaultSupportTicketRateLimitConfig()
	if raw, ok := lookup("WITSELF_SUPPORT_TICKET_RATE_LIMIT"); ok {
		limit, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return cfg, fmt.Errorf("WITSELF_SUPPORT_TICKET_RATE_LIMIT must be an integer")
		}
		cfg.Limit = limit
	}
	if raw, ok := lookup("WITSELF_SUPPORT_TICKET_RATE_WINDOW"); ok {
		window, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return cfg, fmt.Errorf("WITSELF_SUPPORT_TICKET_RATE_WINDOW must be a duration")
		}
		cfg.Window = window
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("support ticket rate configuration: %w", err)
	}
	return cfg, nil
}
