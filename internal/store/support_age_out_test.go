package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSupportTicketAgeOutConfigBounds(t *testing.T) {
	defaults := DefaultSupportTicketAgeOutWorkerConfig()
	if defaults.After != 720*time.Hour || defaults.BatchSize != 100 || defaults.Interval != time.Hour || defaults.BatchTimeout != 10*time.Second {
		t.Fatalf("defaults = %+v", defaults)
	}
	for _, change := range []func(*SupportTicketAgeOutWorkerConfig){
		func(c *SupportTicketAgeOutWorkerConfig) { c.After = 24*time.Hour - time.Nanosecond },
		func(c *SupportTicketAgeOutWorkerConfig) { c.After = 8760*time.Hour + time.Nanosecond },
		func(c *SupportTicketAgeOutWorkerConfig) { c.BatchSize = 0 },
		func(c *SupportTicketAgeOutWorkerConfig) { c.BatchSize = 101 },
		func(c *SupportTicketAgeOutWorkerConfig) { c.Interval = time.Minute - time.Nanosecond },
		func(c *SupportTicketAgeOutWorkerConfig) { c.Interval = 24*time.Hour + time.Nanosecond },
		func(c *SupportTicketAgeOutWorkerConfig) { c.BatchTimeout = time.Second - time.Nanosecond },
		func(c *SupportTicketAgeOutWorkerConfig) { c.BatchTimeout = 5*time.Minute + time.Nanosecond },
	} {
		cfg := defaults
		change(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
	if err := defaults.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSupportTicketAgeOutWorkerRetriesAndCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := DefaultSupportTicketAgeOutWorkerConfig()
	attempts, failures, successes, waits := 0, 0, 0, 0
	err := runSupportTicketAgeOutWorker(ctx, cfg,
		func(attempt context.Context, now time.Time, got SupportTicketAgeOutWorkerConfig) (int64, error) {
			attempts++
			deadline, ok := attempt.Deadline()
			if !ok || time.Until(deadline) > cfg.BatchTimeout || now.IsZero() || got != cfg {
				t.Fatal("unbounded or inconsistent worker batch")
			}
			if attempts == 1 {
				return 0, errors.New("transient failure")
			}
			return 1, nil
		},
		func(_ context.Context, interval time.Duration) bool {
			waits++
			if interval != cfg.Interval {
				t.Fatalf("interval=%s", interval)
			}
			if waits == 2 {
				cancel()
				return false
			}
			return true
		}, func(n int64) {
			successes++
			if n != 1 {
				t.Fatalf("resolved=%d", n)
			}
		}, func(error) { failures++ })
	if err != nil || attempts != 2 || failures != 1 || successes != 1 || waits != 2 {
		t.Fatalf("worker=%v attempts/failures/successes/waits=%d/%d/%d/%d", err, attempts, failures, successes, waits)
	}
	if err := runSupportTicketAgeOutWorker(ctx, cfg, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSupportTicketAgeOutPostgres(t *testing.T) {
	st := supportLimitsTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Second)
	cfg := DefaultSupportTicketAgeOutWorkerConfig()
	cfg.BatchSize = 1
	cutoff := now.Add(-cfg.After)
	seed := func(ticketID, accountID, state, authorKind string, activity time.Time) {
		t.Helper()
		if _, err := st.pool.Exec(ctx, `INSERT INTO support_tickets
			(id,account_id,opened_by_kind,opened_by_id,subject,state,last_activity_at,first_response_at)
			VALUES($1,$2,'owner',$3,'private subject',$4,$5,$6)`, ticketID, accountID, accountID+"_one", state, activity, cutoff.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `INSERT INTO support_ticket_messages(id,ticket_id,account_id,author_kind,body)
			VALUES($1,$2,$3,$4,'private body')`, ticketID+"_msg", ticketID, accountID, authorKind); err != nil {
			t.Fatal(err)
		}
	}
	seed("tkt_boundary", "acc_support_a", TicketStateAwaitingCustomer, MessageAuthorFleetAdmin, cutoff)
	seed("tkt_older", "acc_support_a", TicketStateAwaitingCustomer, MessageAuthorFleetAdmin, cutoff.Add(-time.Hour))
	seed("tkt_other", "acc_support_b", TicketStateAwaitingCustomer, MessageAuthorFleetAdmin, cutoff.Add(-2*time.Hour))
	seed("tkt_recent", "acc_support_a", TicketStateAwaitingCustomer, MessageAuthorFleetAdmin, cutoff.Add(time.Microsecond))
	seed("tkt_unanswered", "acc_support_a", TicketStateAwaitingAdmin, MessageAuthorOwner, cutoff.Add(-time.Hour))
	seed("tkt_assistant", "acc_support_a", TicketStateAwaitingCustomer, MessageAuthorAssistant, cutoff.Add(-time.Hour))
	seed("tkt_closed", "acc_support_a", TicketStateClosed, MessageAuthorFleetAdmin, cutoff.Add(-time.Hour))
	seed("tkt_resolved", "acc_support_a", TicketStateResolved, MessageAuthorFleetAdmin, cutoff.Add(-time.Hour))
	seed("tkt_open", "acc_support_a", TicketStateOpen, MessageAuthorOwner, cutoff.Add(-time.Hour))
	checkBatch := func(want int64) {
		t.Helper()
		got, err := st.ResolveStaleSupportTickets(ctx, now, cfg)
		if err != nil || got != want {
			t.Fatalf("resolved=%d err=%v, want %d", got, err, want)
		}
	}
	// Holding A's account lock skips its tickets while making progress on B.
	accountLock, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = accountLock.Rollback(context.Background()) }()
	if _, err := accountLock.Exec(ctx, `SELECT id FROM accounts WHERE id='acc_support_a' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	checkBatch(1)
	if err := accountLock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// A concurrently edited ticket is skipped without waiting or consuming the batch.
	ticketLock, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ticketLock.Rollback(context.Background()) }()
	if _, err := ticketLock.Exec(ctx, `SELECT id FROM support_tickets WHERE id='tkt_older' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	checkBatch(1)
	if err := ticketLock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// An inactive account never mutates; returning it to active resumes work.
	if _, err := st.pool.Exec(ctx, `UPDATE accounts SET status='suspended' WHERE id='acc_support_a'`); err != nil {
		t.Fatal(err)
	}
	checkBatch(0)
	if _, err := st.pool.Exec(ctx, `UPDATE accounts SET status='active' WHERE id='acc_support_a'`); err != nil {
		t.Fatal(err)
	}
	checkBatch(1)
	checkBatch(0)
	var audits, messages int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM account_events WHERE verb=$1 AND actor_kind='system'
		AND metadata->>'reason'='awaiting_customer_timeout'`, VerbSupportTicketStateChanged).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM support_ticket_messages`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if audits != 3 || messages != 9 {
		t.Fatalf("audit/messages=%d/%d", audits, messages)
	}
	for ticketID, want := range map[string]string{
		"tkt_boundary": TicketStateResolved, "tkt_older": TicketStateResolved,
		"tkt_recent": TicketStateAwaitingCustomer, "tkt_unanswered": TicketStateAwaitingAdmin,
		"tkt_assistant": TicketStateAwaitingCustomer, "tkt_closed": TicketStateClosed,
		"tkt_resolved": TicketStateResolved, "tkt_open": TicketStateOpen,
	} {
		ticket, _, err := st.GetTicket(ctx, "acc_support_a", "acc_support_a_one", ticketID)
		if err != nil || ticket.State != want || ticket.FirstResponseAt == nil || !ticket.FirstResponseAt.Equal(cutoff.Add(-time.Hour)) {
			t.Fatalf("ticket %s=%+v err=%v", ticketID, ticket, err)
		}
		if ticketID == "tkt_boundary" && (ticket.ResolvedAt == nil || !ticket.ResolvedAt.Equal(now) || ticket.ClosedAt != nil) {
			t.Fatalf("age-out timestamps=%+v", ticket)
		}
	}
	if _, err := st.ReplyToTicket(ctx, "acc_support_a", "acc_support_a_one", "tkt_boundary", "customer can reopen"); err != nil {
		t.Fatal(err)
	}
	ticket, _, err := st.GetTicket(ctx, "acc_support_a", "acc_support_a_one", "tkt_boundary")
	if err != nil || ticket.State != TicketStateAwaitingAdmin || ticket.ResolvedAt != nil {
		t.Fatalf("reopen=%+v err=%v", ticket, err)
	}
}
