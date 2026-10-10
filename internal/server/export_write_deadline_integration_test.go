package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/testenv"
)

var exportWriteDeadlineSchema atomic.Uint64

// Each route owns its schema and pool. The observer uses a separate connection,
// so it can never borrow the connection whose export transaction it inspects.
func exportWriteDeadlineStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("witself_export_deadline_%d_%d", os.Getpid(), exportWriteDeadlineSchema.Add(1))
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	var st *store.Store
	t.Cleanup(func() {
		if st != nil {
			st.Close()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Error(err)
		}
		if err := admin.Close(cleanupCtx); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	st, err = store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return st, u.String()
}

func exportWriteDeadlineSeed(t *testing.T, st *store.Store) (accountID string, seededBytes int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	account, err := st.ProvisionAccount(ctx, "write-deadline@example.test", "write deadline fixture", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !ok {
		t.Fatalf("activate export fixture: activated=%t error=%v", ok, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "write deadline realm")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "write deadline agent")
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := st.CreateTranscript(ctx, account.AccountID, realm.ID, agent.ID, store.CreateTranscriptInput{
		Title: "write deadline fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seventy-two independent 60,000-character bodies total 4,320,000 bytes.
	// Base64 random data stays large after gzip and cannot fit the clamped
	// socket buffers. Neither content nor archive bytes enter test diagnostics.
	rng := rand.NewChaCha8([32]byte{1, 1, 4})
	for batch := 0; batch < 9; batch++ {
		entries := make([]store.AppendTranscriptEntryInput, 8)
		for i := range entries {
			data := make([]byte, 45_000)
			if _, err := io.ReadFull(rng, data); err != nil {
				t.Fatal(err)
			}
			entries[i] = store.AppendTranscriptEntryInput{
				Role: store.TranscriptRoleUser,
				Body: base64.StdEncoding.EncodeToString(data),
			}
		}
		if _, err := st.AppendTranscriptEntries(ctx, account.AccountID, realm.ID, agent.ID, transcript.ID, entries); err != nil {
			t.Fatalf("seed transcript batch %d: %v", batch, err)
		}
		for _, entry := range entries {
			seededBytes += len(entry.Body)
		}
	}
	if seededBytes < 4<<20 {
		t.Fatalf("seeded only %d body bytes; need at least 4 MiB", seededBytes)
	}
	return account.AccountID, seededBytes
}

type exportWriteDeadlineCountWriter struct{ n int64 }

func (w *exportWriteDeadlineCountWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

func TestExportWriteDeadlinePostgres(t *testing.T) {
	const d = 5 * time.Second
	for _, route := range []string{"evacuation", "backup"} {
		t.Run(route, func(t *testing.T) {
			st, dsn := exportWriteDeadlineStore(t)
			accountID, seededBytes := exportWriteDeadlineSeed(t, st)
			if route == "evacuation" {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				_, err := st.BeginAccountEvacuation(ctx, accountID, exportWriteTestEvacuationID, "write deadline fixture")
				cancel()
				if err != nil {
					t.Fatal(err)
				}
			}

			connectCtx, connectCancel := context.WithTimeout(context.Background(), 10*time.Second)
			observer, err := pgx.Connect(connectCtx, dsn)
			connectCancel()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := observer.Close(ctx); err != nil {
					t.Error(err)
				}
			})

			type failure struct {
				accountID string
				err       error
			}
			reports := make(chan failure, 8)
			cfg := exportWriteTestConfig(route)
			cfg.ExportWriteStallTimeout = d
			cfg.ReportAccountExportFailure = func(_ context.Context, id string, err error) {
				reports <- failure{accountID: id, err: err}
			}
			cfg.StreamAccountExport = func(ctx context.Context, id, evacuationID string, w io.Writer, flush func() error) error {
				return st.ExportAccountEvacuation(ctx, id, evacuationID, "test-cell", "test", w, flush)
			}
			cfg.StreamAccountBackup = func(ctx context.Context, id, backupID, cellName string, w io.Writer, flush func() error) error {
				return st.ExportAccountBackup(ctx, id, backupID, cellName, "test", w, flush)
			}
			// Cleanup registration order is deliberate: socket, server, observer,
			// store/schema. Closing the socket first also unblocks mutation M8.
			srv, returned := exportWriteTestServer(t, cfg, 16<<10)
			conn, resp := exportWriteRawClient(t, srv, route, accountID, d, 16<<10)
			if err := conn.SetReadDeadline(time.Now().Add(20 * d)); err != nil {
				t.Fatal(err)
			}
			received, err := io.CopyN(io.Discard, resp.Body, 64<<10)
			if err != nil {
				t.Fatalf("read initial archive body: %v", err)
			}
			if err := conn.SetReadDeadline(time.Time{}); err != nil {
				t.Fatal(err)
			}
			t0 := time.Now()

			queryCtx, queryCancel := context.WithTimeout(context.Background(), 10*time.Second)
			rows, err := observer.Query(queryCtx, `
				SELECT a.pid, a.xact_start IS NOT NULL
				  FROM pg_stat_activity a
				 WHERE a.pid <> pg_backend_pid()
				   AND a.backend_type = 'client backend'
				   AND EXISTS (
				         SELECT 1 FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
				          WHERE l.pid = a.pid AND l.granted
				            AND c.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema()))`)
			if err != nil {
				queryCancel()
				t.Fatal(err)
			}
			var pid, count int
			var openTransaction bool
			for rows.Next() {
				if err := rows.Scan(&pid, &openTransaction); err != nil {
					rows.Close()
					queryCancel()
					t.Fatal(err)
				}
				count++
			}
			err = rows.Err()
			rows.Close()
			queryCancel()
			if err != nil {
				t.Fatal(err)
			}
			if count != 1 || !openTransaction {
				t.Fatalf("export backend rows=%d open_transaction=%t; want one open transaction", count, openTransaction)
			}
			if route == "evacuation" {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				var one int
				err := observer.QueryRow(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE NOWAIT`, accountID).Scan(&one)
				cancel()
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
					t.Fatalf("export did not hold account row lock: %v", err)
				}
			}

			timer := time.NewTimer(time.Until(t0.Add(d + 30*time.Second)))
			defer timer.Stop()
			var report failure
			select {
			case report = <-reports:
			case <-timer.C:
				t.Fatalf("no write deadline fired after %s", time.Since(t0))
			}
			reportLatency := time.Since(t0)
			if report.accountID != accountID {
				t.Error("failure report used the wrong account ID")
			}
			if !errors.Is(report.err, os.ErrDeadlineExceeded) {
				t.Fatalf("write deadline error chain does not carry os.ErrDeadlineExceeded: %v", report.err)
			}

			releaseStart := time.Now()
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				var stillOpen bool
				err := observer.QueryRow(releaseCtx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND xact_start IS NOT NULL)`, pid).Scan(&stillOpen)
				if err != nil {
					releaseCancel()
					t.Fatalf("observe export transaction release: %v", err)
				}
				if !stillOpen {
					break
				}
				select {
				case <-ticker.C:
				case <-releaseCtx.Done():
					releaseCancel()
					t.Fatal("export transaction remained open for 10 s after the failure report")
				}
			}
			releaseCancel()
			releaseTime := time.Since(releaseStart)
			if route == "evacuation" {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				var one int
				err := observer.QueryRow(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE NOWAIT`, accountID).Scan(&one)
				cancel()
				if err != nil || one != 1 {
					t.Fatalf("account row lock was not released: %v", err)
				}
			}

			select {
			case <-returned:
			case <-time.After(2 * d):
				t.Fatal("export handler did not return after reporting its failure")
			}
			// Check uniqueness for a complete observation window after the
			// handler has returned, not merely at the first report receive.
			select {
			case duplicate := <-reports:
				t.Fatalf("export failure reported more than once: %v", duplicate.err)
			case <-time.After(5 * d):
			}

			if err := conn.SetReadDeadline(time.Now().Add(2 * d)); err != nil {
				t.Fatal(err)
			}
			drained, drainErr := io.Copy(io.Discard, resp.Body)
			totalReceived := received + drained
			if drainErr == nil || errors.Is(drainErr, io.EOF) {
				t.Fatalf("client drain ended with clean chunked EOF: drained=%d total_received=%d", drained, totalReceived)
			}

			full := &exportWriteDeadlineCountWriter{}
			exportCtx, exportCancel := context.WithTimeout(context.Background(), time.Minute)
			if route == "evacuation" {
				err = st.ExportAccountEvacuation(exportCtx, accountID, exportWriteTestEvacuationID, "test-cell", "test", full, func() error { return nil })
			} else {
				err = st.ExportAccountBackup(exportCtx, accountID, exportWriteTestBackupID, "test-cell", "test", full, func() error { return nil })
			}
			exportCancel()
			if err != nil {
				t.Fatalf("direct complete export: %v", err)
			}
			if totalReceived >= full.n {
				t.Fatalf("client received %d bytes after draining %d bytes; complete archive has only %d bytes", totalReceived, drained, full.n)
			}
			t.Logf("seeded_body_bytes=%d received_body_bytes=%d drained_body_bytes=%d total_received_body_bytes=%d full_archive_bytes=%d report_latency=%s pid_release_time=%s", seededBytes, received, drained, totalReceived, full.n, reportLatency, releaseTime)
		})
	}
}
