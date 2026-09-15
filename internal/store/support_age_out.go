package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SupportTicketAgeOutWorkerConfig controls a default-off worker that resolves
// stale customer-waiting tickets. Resolution is reopenable and retains content.
type SupportTicketAgeOutWorkerConfig struct {
	After        time.Duration
	BatchSize    int
	Interval     time.Duration
	BatchTimeout time.Duration
}

// DefaultSupportTicketAgeOutWorkerConfig waits 30 days and resolves at most 100
// tickets per hourly sweep. Worker registration remains disabled by default.
func DefaultSupportTicketAgeOutWorkerConfig() SupportTicketAgeOutWorkerConfig {
	return SupportTicketAgeOutWorkerConfig{
		After: 30 * 24 * time.Hour, BatchSize: 100,
		Interval: time.Hour, BatchTimeout: 10 * time.Second,
	}
}

// Validate bounds the customer wait, batch size, interval, and deadline.
func (c SupportTicketAgeOutWorkerConfig) Validate() error {
	if c.After < 24*time.Hour || c.After > 365*24*time.Hour {
		return errors.New("support ticket age-out after must be between 24h and 8760h")
	}
	if c.BatchSize < 1 || c.BatchSize > 100 {
		return errors.New("support ticket age-out batch size must be between 1 and 100")
	}
	if c.Interval < time.Minute || c.Interval > 24*time.Hour {
		return errors.New("support ticket age-out interval must be between 1m and 24h")
	}
	if c.BatchTimeout < time.Second || c.BatchTimeout > 5*time.Minute {
		return errors.New("support ticket age-out batch timeout must be between 1s and 5m")
	}
	return nil
}

// ResolveStaleSupportTickets runs one bounded, atomic batch. SKIP LOCKED on
// account and ticket rows fences replies, account moves, and concurrent sweeps.
// Awaiting-admin tickets and assistant-only replies are never aged out, keeping
// the published human first-response promise independent of queue maintenance.
func (s *Store) ResolveStaleSupportTickets(ctx context.Context, now time.Time, cfg SupportTicketAgeOutWorkerConfig) (int64, error) {
	if err := cfg.Validate(); err != nil {
		return 0, err
	}
	if now.IsZero() {
		return 0, errors.New("support ticket age-out time required")
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.BatchTimeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT t.account_id, t.id
		FROM support_tickets t JOIN accounts a ON a.id = t.account_id
		WHERE a.status = 'active' AND t.state = 'awaiting_customer'
		  AND t.last_activity_at <= $1
		  AND EXISTS (SELECT 1 FROM support_ticket_messages m
		    WHERE m.ticket_id = t.id AND m.account_id = t.account_id
		      AND m.author_kind = 'fleet_admin')
		ORDER BY t.last_activity_at, t.id LIMIT $2
		FOR UPDATE OF a, t SKIP LOCKED`, now.Add(-cfg.After), cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("select stale support tickets: %w", err)
	}
	type target struct{ accountID, ticketID string }
	targets := make([]target, 0, cfg.BatchSize)
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.accountID, &t.ticketID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan stale support tickets: %w", err)
		}
		targets = append(targets, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("read stale support tickets: %w", err)
	}
	for _, t := range targets {
		if _, err := tx.Exec(ctx, `UPDATE support_tickets
			SET state = 'resolved', resolved_at = $3, last_activity_at = $3
			WHERE account_id = $1 AND id = $2`, t.accountID, t.ticketID, now); err != nil {
			return 0, fmt.Errorf("resolve stale support ticket: %w", err)
		}
		if err := s.logEventTx(ctx, tx, EventInput{
			AccountID: t.accountID, ActorKind: MessageAuthorSystem,
			Verb: VerbSupportTicketStateChanged,
			Metadata: map[string]any{
				"ticket_id": t.ticketID, "state_from": TicketStateAwaitingCustomer,
				"state_to": TicketStateResolved, "reason": "awaiting_customer_timeout",
			},
		}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int64(len(targets)), nil
}

// RunSupportTicketAgeOutWorker follows the cell worker's bounded immediate
// batch, interval retry, per-batch timeout, and graceful cancellation pattern.
func (s *Store) RunSupportTicketAgeOutWorker(ctx context.Context, cfg SupportTicketAgeOutWorkerConfig, onResult func(int64), onError func(error)) error {
	return runSupportTicketAgeOutWorker(ctx, cfg, s.ResolveStaleSupportTickets,
		waitForMessageRateBucketCleanupInterval, onResult, onError)
}

func runSupportTicketAgeOutWorker(ctx context.Context, cfg SupportTicketAgeOutWorkerConfig,
	resolve func(context.Context, time.Time, SupportTicketAgeOutWorkerConfig) (int64, error),
	wait func(context.Context, time.Duration) bool, onResult func(int64), onError func(error),
) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	for ctx.Err() == nil {
		attemptCtx, cancel := context.WithTimeout(ctx, cfg.BatchTimeout)
		resolved, err := resolve(attemptCtx, time.Now().UTC(), cfg)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if onError != nil {
				onError(err)
			}
		} else if onResult != nil {
			onResult(resolved)
		}
		if !wait(ctx, cfg.Interval) {
			return nil
		}
	}
	return nil
}
