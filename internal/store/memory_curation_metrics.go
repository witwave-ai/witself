package store

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// MemoryCurationTransition contains only closed-vocabulary run states. "none"
// denotes insertion of a run; cancelled requests have abandoned runs, and
// expired leases have interrupted runs.
type MemoryCurationTransition struct {
	From string
	To   string
}

// MemoryCurationCounters is a detached, value-free snapshot of process-local
// committed mutations. Counters reset when this Store is replaced.
type MemoryCurationCounters struct {
	Transitions map[MemoryCurationTransition]uint64
	LeaseEvents map[string]uint64
}

type memoryCurationMetrics struct {
	mu          sync.Mutex
	transitions map[MemoryCurationTransition]uint64
	leaseEvents map[string]uint64
}

// MemoryCurationCounters returns the committed, value-free curation run-transition,
// lease-event and operation counters accumulated by this process.
func (s *Store) MemoryCurationCounters() MemoryCurationCounters {
	m := &s.memoryCurationMetrics
	m.mu.Lock()
	defer m.mu.Unlock()
	out := MemoryCurationCounters{
		Transitions: make(map[MemoryCurationTransition]uint64, len(m.transitions)),
		LeaseEvents: make(map[string]uint64, len(m.leaseEvents)),
	}
	for key, count := range m.transitions {
		out.Transitions[key] = count
	}
	for key, count := range m.leaseEvents {
		out.LeaseEvents[key] = count
	}
	return out
}

// MemoryCurationQueueMetrics aggregates the cell's queue without tenant or
// resource identifiers. Only active accounts and undeleted realms/agents count.
// Pending includes future retry work; age measures the oldest due, unclaimed
// request and is zero when none is due.
type MemoryCurationQueueMetrics struct {
	RequestsPending int64
	QueueAgeSeconds float64
}

// ReadMemoryCurationQueueMetrics reads the cell's curation queue for the pending
// request count and the age of the oldest due unclaimed request; it never
// returns request or account identifiers.
func (s *Store) ReadMemoryCurationQueueMetrics(ctx context.Context) (MemoryCurationQueueMetrics, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var out MemoryCurationQueueMetrics
	err := s.pool.QueryRow(ctx, `
		WITH observation AS (SELECT clock_timestamp() AS now), pending AS (
		    SELECT q.id, q.due_at FROM memory_curation_requests q
		    JOIN accounts a ON a.id=q.account_id AND a.status='active'
		    JOIN realms r ON r.id=q.realm_id AND r.account_id=q.account_id AND r.deleted_at IS NULL
		    JOIN agents g ON g.id=q.owner_id AND g.realm_id=q.realm_id AND g.deleted_at IS NULL
		    WHERE q.owner_kind='agent' AND q.state IN ('queued','retry_wait') AND q.claimed_run_id IS NULL
		)
		SELECT count(pending.id),
		       COALESCE(EXTRACT(EPOCH FROM (
		           observation.now - min(due_at) FILTER (WHERE due_at <= observation.now)
		       )), 0)::double precision
		FROM observation LEFT JOIN pending ON true
		GROUP BY observation.now`).Scan(&out.RequestsPending, &out.QueueAgeSeconds)
	return out, err
}

// Buffer observations beside the existing transaction and publish them only
// after its commit succeeds. Some methods commit expiry reconciliation while
// returning a domain error; observing public method success would miss those
// transitions. Idempotent replays enqueue nothing. Savepoints retain pgx's
// ordinary behavior; observations are added only to this outer transaction.
type memoryCurationMetricsTx struct {
	pgx.Tx
	metrics     *memoryCurationMetrics
	transitions []MemoryCurationTransition
	leaseEvents []string
}

func (s *Store) beginMemoryCurationMetricsTx(ctx context.Context) (*memoryCurationMetricsTx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &memoryCurationMetricsTx{Tx: tx, metrics: &s.memoryCurationMetrics}, nil
}

func (tx *memoryCurationMetricsTx) Commit(ctx context.Context) error {
	if err := tx.Tx.Commit(ctx); err != nil {
		return err
	}
	m := tx.metrics
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.transitions == nil {
		m.transitions = make(map[MemoryCurationTransition]uint64)
		m.leaseEvents = make(map[string]uint64)
	}
	for _, transition := range tx.transitions {
		m.transitions[transition]++
	}
	for _, event := range tx.leaseEvents {
		m.leaseEvents[event]++
	}
	tx.transitions = nil
	tx.leaseEvents = nil
	return nil
}

func observeMemoryCurationTransitionTx(tx pgx.Tx, from, to string) {
	observed, ok := tx.(*memoryCurationMetricsTx)
	if !ok || !validMemoryCurationTransition(from, to) {
		return
	}
	observed.transitions = append(observed.transitions, MemoryCurationTransition{From: from, To: to})
}

func validMemoryCurationTransition(from, to string) bool {
	switch from {
	case "none":
		return to == MemoryCurationRunOpen
	case MemoryCurationRunOpen:
		return to == MemoryCurationRunPlanned || to == MemoryCurationRunAbandoned || to == MemoryCurationRunInterrupted
	case MemoryCurationRunPlanned:
		return to == MemoryCurationRunApplied || to == MemoryCurationRunAbandoned || to == MemoryCurationRunInterrupted || to == MemoryCurationRunConflict
	case MemoryCurationRunApplied:
		return to == MemoryCurationRunRolledBack
	default:
		return false
	}
}

func observeMemoryCurationLeaseEventTx(tx pgx.Tx, event string) {
	observed, ok := tx.(*memoryCurationMetricsTx)
	if !ok {
		return
	}
	switch event {
	case "start", "renew", "expire", "reconcile":
		observed.leaseEvents = append(observed.leaseEvents, event)
	}
}
