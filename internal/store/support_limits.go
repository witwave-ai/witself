package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SupportTicketRateLimitConfig uses the support-email sender limiter's 10/60s
// defaults (infra/cloudflare/support-email-intake/wrangler.template.jsonc).
// API/CLI admission is account-scoped and shared by every operator and replica.
type SupportTicketRateLimitConfig struct {
	Limit  int
	Window time.Duration
}

// DefaultSupportTicketRateLimitConfig matches the email sender limit.
func DefaultSupportTicketRateLimitConfig() SupportTicketRateLimitConfig {
	return SupportTicketRateLimitConfig{Limit: 10, Window: time.Minute}
}

// Validate rejects unbounded or fractional-second admission windows.
func (c SupportTicketRateLimitConfig) Validate() error {
	if c.Limit < 1 || c.Limit > 1000 {
		return errors.New("support ticket rate limit must be between 1 and 1000")
	}
	if c.Window < time.Second || c.Window > 24*time.Hour || c.Window%time.Second != 0 {
		return errors.New("support ticket rate window must be whole seconds between 1s and 24h")
	}
	return nil
}

// WithSupportTicketRateLimit selects a validated process-lifetime limit.
// Invalid values fail closed when admission is attempted.
func WithSupportTicketRateLimit(cfg SupportTicketRateLimitConfig) Option {
	return func(s *Store) { s.supportTicketRateLimit = cfg }
}

// ErrSupportRateLimited signals a retryable per-account ticket-creation refusal.
var ErrSupportRateLimited = errors.New("support ticket creation rate limit exceeded")

// SupportRateLimitError is value-free and carries a conservative retry delay.
type SupportRateLimitError struct {
	Limit             int
	WindowSeconds     int
	RetryAfterSeconds int
}

func (e *SupportRateLimitError) Error() string { return ErrSupportRateLimited.Error() }
func (e *SupportRateLimitError) Unwrap() error { return ErrSupportRateLimited }

const supportTicketAdmissionCountSQL = `SELECT count(*) FROM (
	SELECT 1 FROM support_tickets
	WHERE account_id = $1 AND opened_at > $2
	ORDER BY opened_at DESC
	LIMIT $3
) AS recent`

// admitSupportTicketTx must run after locking the account row FOR UPDATE.
// Counting existing durable tickets makes admission replica-safe without
// ephemeral counters. Migration 0095 covers the account/window predicate and
// newest-first ordering; LIMIT bounds the aggregate input even for an account
// with imported high-cardinality data. Failed attempts add no rows.
func (s *Store) admitSupportTicketTx(ctx context.Context, tx pgx.Tx, accountID string) (time.Time, error) {
	cfg := s.supportTicketRateLimit
	if err := cfg.Validate(); err != nil {
		return time.Time{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("support ticket admission clock: %w", err)
	}
	var count int
	if err := tx.QueryRow(ctx, supportTicketAdmissionCountSQL, accountID, now.Add(-cfg.Window), cfg.Limit).Scan(&count); err != nil {
		return time.Time{}, fmt.Errorf("support ticket admission count: %w", err)
	}
	if count >= cfg.Limit {
		seconds := int(cfg.Window / time.Second)
		return time.Time{}, &SupportRateLimitError{
			Limit: cfg.Limit, WindowSeconds: seconds, RetryAfterSeconds: seconds,
		}
	}
	return now, nil
}
