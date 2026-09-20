package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

type memoryCurationMetricsTestTx struct {
	pgx.Tx
	commitErr error
}

func assertMemoryCurationCounters(t *testing.T, st *Store, want MemoryCurationCounters) {
	t.Helper()
	if got := st.MemoryCurationCounters(); !reflect.DeepEqual(got, want) {
		t.Fatalf("curation counters = %#v, want %#v", got, want)
	}
}

func (tx *memoryCurationMetricsTestTx) Commit(context.Context) error { return tx.commitErr }

func TestMemoryCurationMetricsCommitAndClosedVocabulary(t *testing.T) {
	ctx := context.Background()
	st := &Store{}
	tx := &memoryCurationMetricsTx{Tx: &memoryCurationMetricsTestTx{}, metrics: &st.memoryCurationMetrics}
	want := MemoryCurationCounters{
		Transitions: map[MemoryCurationTransition]uint64{
			{"none", "open"}: 1, {"open", "planned"}: 1,
			{"planned", "applied"}: 1, {"applied", "rolled_back"}: 1,
			{"open", "abandoned"}: 1, {"planned", "abandoned"}: 1,
			{"open", "interrupted"}: 1, {"planned", "interrupted"}: 1,
			{"planned", "conflict"}: 1,
		},
		LeaseEvents: map[string]uint64{"start": 1, "renew": 1, "expire": 1, "reconcile": 1},
	}
	states := []string{"none", "open", "planned", "applied", "rolled_back", "abandoned", "interrupted", "conflict", "cancelled", "expired", "mrun_sensitive_owner"}
	for _, from := range states {
		for _, to := range states {
			observeMemoryCurationTransitionTx(tx, from, to)
		}
	}
	for _, event := range []string{"start", "renew", "expire", "reconcile", "mcrq_sensitive_request"} {
		observeMemoryCurationLeaseEventTx(tx, event)
	}
	if got := st.MemoryCurationCounters(); len(got.Transitions) != 0 || len(got.LeaseEvents) != 0 {
		t.Fatal("uncommitted observations escaped")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := st.MemoryCurationCounters(); !reflect.DeepEqual(got, want) {
		t.Fatalf("committed vocabulary = %#v, want %#v", got, want)
	}
	// Snapshots must not share their maps with subsequent writers or callers.
	got := st.MemoryCurationCounters()
	got.LeaseEvents["mcrq_sensitive_request"] = 999
	got.Transitions[MemoryCurationTransition{"mrun_sensitive_owner", "open"}] = 999
	if got := st.MemoryCurationCounters(); !reflect.DeepEqual(got, want) {
		t.Fatal("caller mutated store counters through snapshot")
	}
	failed := &memoryCurationMetricsTx{
		Tx:      &memoryCurationMetricsTestTx{commitErr: errors.New("synthetic commit failure")},
		metrics: &st.memoryCurationMetrics,
	}
	observeMemoryCurationTransitionTx(failed, "none", "open")
	observeMemoryCurationLeaseEventTx(failed, "start")
	if err := failed.Commit(ctx); err == nil {
		t.Fatal("expected synthetic commit failure")
	}
	if got := st.MemoryCurationCounters(); !reflect.DeepEqual(got, want) {
		t.Fatal("failed commit published counters")
	}
}

func TestMemoryCurationMetricsConcurrentSnapshots(t *testing.T) {
	st := &Store{}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				tx := &memoryCurationMetricsTx{Tx: &memoryCurationMetricsTestTx{}, metrics: &st.memoryCurationMetrics}
				observeMemoryCurationTransitionTx(tx, "none", "open")
				observeMemoryCurationLeaseEventTx(tx, "start")
				_ = tx.Commit(context.Background())
				_ = st.MemoryCurationCounters()
			}
		}()
	}
	wg.Wait()
	got := st.MemoryCurationCounters()
	if got.Transitions[MemoryCurationTransition{"none", "open"}] != 2000 || got.LeaseEvents["start"] != 2000 {
		t.Fatalf("lost concurrent increments: %#v", got)
	}
}
