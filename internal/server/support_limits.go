package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// ErrSupportRateLimited signals the retryable account ticket-creation limit.
var ErrSupportRateLimited = errors.New("support ticket creation rate limit exceeded")

// SupportRateLimitError preserves the store's value-free account admission
// refusal so API and CLI callers can retry using the normal rate_limited code.
type SupportRateLimitError struct {
	Limit             int
	WindowSeconds     int
	RetryAfterSeconds int
}

func (e *SupportRateLimitError) Error() string { return ErrSupportRateLimited.Error() }
func (e *SupportRateLimitError) Unwrap() error { return ErrSupportRateLimited }

func writeSupportRateLimitError(w http.ResponseWriter, err error) {
	var detail *SupportRateLimitError
	if !errors.As(err, &detail) {
		detail = &SupportRateLimitError{}
	}
	if detail.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(detail.RetryAfterSeconds))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema_version": "witself.v0",
		"code":           "rate_limited", "error": ErrSupportRateLimited.Error(),
		"retryable": true, "retry_after": detail.RetryAfterSeconds,
		"details": map[string]any{
			"scope": "account", "limit": detail.Limit,
			"window_seconds": detail.WindowSeconds, "retry_after": detail.RetryAfterSeconds,
		},
	})
}
