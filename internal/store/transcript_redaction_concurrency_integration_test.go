package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestTranscriptRedactionConcurrentApplyPostgres(t *testing.T) {
	st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	// Measure after Migrate, so queueing on the shared migrate lock does not
	// inflate the proportional wait bounds below.
	fixtureStarted := time.Now()
	// Lock probes use independent connections so all four apply goroutines can
	// occupy even a four-connection store pool without starving the observer.
	connect := func(t *testing.T, ctx context.Context) *pgx.Conn {
		t.Helper()
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		return conn
	}
	fixture := func(t *testing.T, ctx context.Context, p Principal, name string) RedactTranscriptEntriesInput {
		t.Helper()
		tr := createTranscriptStorageTestConversation(t, st, p, name)
		var batch []AppendTranscriptEntryInput
		for i := range 3 {
			batch = append(batch, AppendTranscriptEntryInput{
				ExternalID: fmt.Sprintf("entry-%d", i), Role: TranscriptRoleTool,
				Body: "ordinary fixture content", Payload: json.RawMessage(`{"kind":"tool.result"}`),
			})
		}
		if _, err := st.AppendTranscriptEntries(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, batch); err != nil {
			t.Fatal(err)
		}
		in := RedactTranscriptEntriesInput{TranscriptID: tr.ID, FromSequence: 1, UntilSequence: 3}
		preview, err := st.RedactTranscriptEntries(ctx, transcriptRedactionTestOwner(t, st, p), in)
		if err != nil {
			t.Fatal(err)
		}
		if preview.RedactableCount != 3 {
			t.Fatal("concurrency fixture must preview three unredacted entries")
		}
		in.Apply, in.ExpectedRedactionSetRevision = true, preview.RedactionSetRevision
		return in
	}

	for _, withLane := range []bool{true, false} {
		name := "lane-present"
		if !withLane {
			name = "lane-deleted"
		}
		t.Run(name, func(t *testing.T) {
			p := newTranscriptStorageTestPrincipal(t, st, "concurrent-"+name)
			owner := transcriptRedactionTestOwner(t, st, p)
			for round := range 4 {
				t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), max(10*time.Second, 5*time.Since(fixtureStarted)))
					defer cancel()
					in := fixture(t, ctx, p, fmt.Sprintf("round-%d", round))
					if !withLane {
						if _, err := st.pool.Exec(ctx, `DELETE FROM memory_curation_lanes WHERE account_id=$1`, p.AccountID); err != nil {
							t.Fatal(err)
						}
					}
					var laneExists bool
					if err := st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memory_curation_lanes WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3)`, p.AccountID, p.RealmID, p.ID).Scan(&laneExists); err != nil {
						t.Fatal(err)
					}
					if laneExists != withLane {
						t.Fatal("concurrent apply fixture has wrong lane state")
					}
					locker, observer := connect(t, ctx), connect(t, ctx)
					barrier, err := locker.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = barrier.Rollback(context.Background()) }()
					// This test-only table lock permits every read/row-lock stage,
					// but holds UPDATE until all contenders have reached a lock.
					// With proper serialization, three wait behind the first. With
					// all serialization locks removed, all four read the old revision
					// and wait here, making that regression deterministic.
					if _, err := barrier.Exec(ctx, `LOCK TABLE transcript_entries IN SHARE MODE`); err != nil {
						t.Fatal(err)
					}
					start := make(chan struct{})
					results := make(chan error, 4)
					done := make(chan struct{})
					var workers sync.WaitGroup
					for worker := range 4 {
						workers.Go(func() {
							<-start
							request := in
							request.IdempotencyKey = fmt.Sprintf("round-%d-worker-%d", round, worker)
							result, err := st.RedactTranscriptEntries(ctx, owner, request)
							if err == nil && (!result.Applied || result.Replayed || result.RedactedCount != 3) {
								err = errors.New("successful concurrent apply returned an invalid receipt")
							}
							results <- err
						})
					}
					go func() { workers.Wait(); close(done) }()
					defer func() { cancel(); <-done }()
					close(start)
					transcriptRedactionWaitForBlocked(ctx, t, observer, locker.PgConn().PID(), 4, true, done)
					if err := barrier.Rollback(ctx); err != nil {
						t.Fatal(err)
					}
					<-done
					successes, conflicts := 0, 0
					for range 4 {
						err := <-results
						switch {
						case err == nil:
							successes++
						case errors.Is(err, ErrTranscriptRedactionConflict):
							conflicts++
						default:
							t.Fatalf("concurrent apply returned unexpected error: %v", err)
						}
					}
					if successes != 1 || conflicts != 3 {
						t.Fatalf("concurrent results: successes=%d conflicts=%d, want 1 and 3", successes, conflicts)
					}
					var events int
					if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM account_events WHERE account_id=$1 AND verb=$2 AND metadata->>'transcript_id'=$3`, p.AccountID, VerbTranscriptEntriesRedacted, in.TranscriptID).Scan(&events); err != nil {
						t.Fatal(err)
					}
					if events != 1 {
						t.Fatalf("redaction events = %d, want 1", events)
					}
					assertTranscriptStorageUsageMatchesRecount(t, st, p.AccountID)
				})
			}
		})
	}

	t.Run("lane-before-conversation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), max(10*time.Second, 5*time.Since(fixtureStarted)))
		defer cancel()
		p := newTranscriptStorageTestPrincipal(t, st, "lane-order")
		in := fixture(t, ctx, p, "lane-order")
		in.IdempotencyKey = "lane-before-conversation"
		owner := transcriptRedactionTestOwner(t, st, p)
		locker, observer := connect(t, ctx), connect(t, ctx)
		locked, err := locker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = locked.Rollback(context.Background()) }()
		var one int
		if err := locked.QueryRow(ctx, `SELECT 1 FROM memory_curation_lanes WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3 FOR UPDATE`, p.AccountID, p.RealmID, p.ID).Scan(&one); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		var applyErr error
		go func() {
			defer close(done)
			_, applyErr = st.RedactTranscriptEntries(ctx, owner, in)
		}()
		defer func() { cancel(); <-done }()
		transcriptRedactionWaitForBlocked(ctx, t, observer, locker.PgConn().PID(), 1, false, done)
		probe, err := observer.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = probe.Rollback(context.Background()) }()
		if err := probe.QueryRow(ctx, `SELECT 1 FROM transcript_conversations WHERE id=$1 FOR NO KEY UPDATE NOWAIT`, in.TranscriptID).Scan(&one); err != nil {
			t.Fatalf("apply locked the conversation before the owner lane: %v", err)
		}
		if err := probe.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err := locked.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		<-done
		if applyErr != nil {
			t.Fatalf("apply after lane unlock: %v", applyErr)
		}
		assertTranscriptStorageUsageMatchesRecount(t, st, p.AccountID)
	})

	t.Run("evacuation-check-after-account-lock", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), max(10*time.Second, 5*time.Since(fixtureStarted)))
		defer cancel()
		p := newTranscriptStorageTestPrincipal(t, st, "evacuation-race")
		in := fixture(t, ctx, p, "evacuation-race")
		in.IdempotencyKey = "evacuation-race"
		owner := transcriptRedactionTestOwner(t, st, p)
		locker, observer := connect(t, ctx), connect(t, ctx)
		const snapshotSQL = `SELECT jsonb_build_object(
			'entries',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM transcript_entries e WHERE account_id=$1),
			'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM account_events e WHERE account_id=$1))::text`
		var before, after string
		if err := observer.QueryRow(ctx, snapshotSQL, p.AccountID).Scan(&before); err != nil {
			t.Fatal(err)
		}
		locked, err := locker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = locked.Rollback(context.Background()) }()
		var one int
		if err := locked.QueryRow(ctx, `SELECT 1 FROM accounts WHERE id=$1 FOR UPDATE`, p.AccountID).Scan(&one); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		var applyErr error
		go func() {
			defer close(done)
			_, applyErr = st.RedactTranscriptEntries(ctx, owner, in)
		}()
		defer func() { cancel(); <-done }()
		transcriptRedactionWaitForBlocked(ctx, t, observer, locker.PgConn().PID(), 1, false, done)
		// The 0070 trigger (SQLSTATE 55006) already prevents entry changes.
		// This race pins the check-after-lock order and its error for the 409
		// mapping, rather than accepting a later trigger error or stale guard.
		const evacuationID = "evac_redaction_concurrent"
		if err := setEvacuationAuthorityTx(ctx, locked, evacuationID); err != nil {
			t.Fatal(err)
		}
		if _, err := locked.Exec(ctx, `UPDATE accounts SET evacuation_id=$2, evacuation_started_at=clock_timestamp(), evacuation_role='source' WHERE id=$1`, p.AccountID, evacuationID); err != nil {
			t.Fatal(err)
		}
		if err := locked.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		<-done
		if !errors.Is(applyErr, ErrAccountEvacuationInProgress) {
			t.Fatalf("racing apply error = %v, want account evacuation in progress", applyErr)
		}
		if err := observer.QueryRow(ctx, snapshotSQL, p.AccountID).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatal("evacuation-racing apply changed entry or event bytes")
		}
		assertTranscriptStorageUsageMatchesRecount(t, st, p.AccountID)
	})
}

// Wait for a database-observed lock dependency, never a scheduling sleep. The
// caller bounds the context proportionally to the fixture's measured setup.
// Only this database's heavyweight-lock waiters count: a CREATE INDEX
// CONCURRENTLY in another test's schema waits on any older transaction
// (wait_event virtualxid), including this test's idle locker, and must not
// satisfy or overshoot the count.
func transcriptRedactionWaitForBlocked(ctx context.Context, t *testing.T, observer *pgx.Conn, blockerPID uint32, want int, transitive bool, done <-chan struct{}) {
	t.Helper()
	const ownWaiter = `a.datname = current_database() AND a.wait_event_type = 'Lock' AND a.wait_event IS DISTINCT FROM 'virtualxid'`
	query := `SELECT count(*) FROM pg_stat_activity a WHERE ` + ownWaiter + ` AND $1::int = ANY(pg_blocking_pids(a.pid))`
	if transitive {
		query = `WITH RECURSIVE blocked(pid) AS (
			SELECT a.pid FROM pg_stat_activity a WHERE ` + ownWaiter + ` AND $1::int = ANY(pg_blocking_pids(a.pid))
			UNION
			SELECT a.pid FROM pg_stat_activity a JOIN blocked b ON b.pid=ANY(pg_blocking_pids(a.pid)) WHERE ` + ownWaiter + `
		) SELECT count(*) FROM blocked`
	}
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		var blocked int
		if err := observer.QueryRow(ctx, query, blockerPID).Scan(&blocked); err != nil {
			t.Fatalf("wait for %d database-observed blocked applies: %v", want, err)
		}
		if blocked == want {
			return
		}
		select {
		case <-done:
			t.Fatalf("apply completed before the expected lock dependency (%d/%d blocked)", blocked, want)
		case <-ctx.Done():
			t.Fatalf("wait for %d database-observed blocked applies (%d seen): %v", want, blocked, ctx.Err())
		case <-poll.C:
		}
	}
}
