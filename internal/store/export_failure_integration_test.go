package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestExportAccountWriterFailureRollsBackReconciliationPostgres(t *testing.T) {
	f := newExportFailureFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	opened := f.open(t, "coordinator", "export-failure-open", 1)
	f.offer(t, opened, "worker", "export-failure-offer")
	f.selectAgents(t, opened, "coordinator", "export-failure-select", "worker")
	claim := f.claim(t, opened, "worker", "export-failure-claim")
	// Follow the existing expiry-export fixture's database-clock convention.
	// No request API may reconcile this deliberately due row before export.
	if _, err := f.st.pool.Exec(ctx, `
		UPDATE agent_message_requests
		SET created_at=clock_timestamp()-interval '3 seconds',
		    offer_deadline=clock_timestamp()-interval '2 seconds',
		    expires_at=clock_timestamp()-interval '1 second',
		    updated_at=clock_timestamp()-interval '1 second'
		WHERE id=$1`, opened.Request.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SuspendAccountSystem(ctx, f.accountID, "evacuation", "archive writer failure fixture"); err != nil {
		t.Fatal(err)
	}
	before := exportFailureSnapshot(ctx, t, f.st, f.accountID)
	if len(before["accounts"]) != 1 || len(before["agent_message_requests"]) != 1 ||
		len(before["agent_message_request_claims"]) != 1 || len(before["agent_message_request_candidates"]) != 1 ||
		len(before["agent_message_request_selections"]) != 1 || len(before["agent_messages"]) != 2 ||
		len(before["agent_message_deliveries"]) != 2 || len(before["account_events"]) == 0 {
		t.Fatal("fixture omitted its account, request graph, protocol messages, deliveries, or events")
	}
	requestBefore, claimBefore := before["agent_message_requests"][0], before["agent_message_request_claims"][0]
	var due bool
	if err := f.st.pool.QueryRow(ctx, `SELECT expires_at <= clock_timestamp() FROM agent_message_requests WHERE id=$1`,
		opened.Request.ID).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if !due || before["accounts"][0]["status"] != "suspended" ||
		before["accounts"][0]["retained_agent_email_attachment_bytes"] != json.Number("0") ||
		requestBefore["state"] != MessageRequestStateOpen || requestBefore["expired_at"] != nil ||
		claimBefore["id"] != claim.ClaimID || claimBefore["state"] != MessageRequestClaimClaimed ||
		claim.Generation < 1 || claimBefore["generation"] != json.Number(strconv.FormatInt(claim.Generation, 10)) ||
		claimBefore["claim_key_hash"] == "" || claimBefore["lease_expires_at"] == nil || claimBefore["cancelled_at"] != nil {
		t.Fatal("fixture must retain a due open request and its previously acquired processing fence")
	}
	if exportFailureExpiryEvents(before) != 0 {
		t.Fatal("fixture was already reconciled before the failing export")
	}

	sentinel := errors.New("synthetic archive writer failure")
	writer := &exportFailureWriter{ctx: ctx, entered: make(chan struct{}), release: make(chan struct{}), err: sentinel}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(writer.release) }) }
	result := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		result <- f.st.ExportAccount(ctx, f.accountID, "fixture-cell", "test", writer)
	}()
	defer func() {
		release()
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("failed export did not stop during cleanup")
		}
	}()
	select {
	case <-writer.entered:
	case err := <-result:
		t.Fatalf("export returned before reaching the writer: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The first gzip write is after reconciliation but before row streaming.
	// A separate connection leaves the export pool free and proves the lock
	// through PostgreSQL's actual NOWAIT refusal, without timing assumptions.
	assertExportFailureAccountLock(ctx, t, f.st, f.accountID, true)
	release()
	select {
	case err := <-result:
		if !errors.Is(err, sentinel) {
			t.Fatalf("export writer error = %v, want sentinel", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	assertExportFailureAccountLock(ctx, t, f.st, f.accountID, false)
	if !reflect.DeepEqual(exportFailureSnapshot(ctx, t, f.st, f.accountID), before) {
		t.Fatal("failed operational export committed reconciliation state")
	}

	var committed exportFailureRows
	for attempt := range 2 {
		var archive bytes.Buffer
		if err := f.st.ExportAccount(ctx, f.accountID, "fixture-cell", "test", &archive); err != nil {
			t.Fatal(err)
		}
		after := exportFailureSnapshot(ctx, t, f.st, f.accountID)
		if len(after["agent_message_requests"]) != 1 || len(after["agent_message_request_claims"]) != 1 {
			t.Fatal("successful export changed request or claim cardinality")
		}
		requestAfter, claimAfter := after["agent_message_requests"][0], after["agent_message_request_claims"][0]
		if requestAfter["state"] != MessageRequestStateExpired || requestAfter["expired_at"] != requestBefore["expires_at"] ||
			claimAfter["state"] != MessageRequestClaimCancelled || claimAfter["cancelled_at"] != requestBefore["expires_at"] ||
			claimAfter["lease_expires_at"] != nil || claimAfter["generation"] != claimBefore["generation"] ||
			claimAfter["claim_key_hash"] != claimBefore["claim_key_hash"] || exportFailureExpiryEvents(after) != 1 ||
			len(after["account_events"]) != len(before["account_events"])+1 {
			t.Fatal("successful export did not commit exactly one deadline-bound expiry and fence cancellation")
		}
		wantRequest, wantClaim := maps.Clone(requestBefore), maps.Clone(claimBefore)
		wantRequest["state"], wantRequest["expired_at"] = MessageRequestStateExpired, requestBefore["expires_at"]
		wantClaim["state"], wantClaim["cancelled_at"], wantClaim["lease_expires_at"] = MessageRequestClaimCancelled, requestBefore["expires_at"], nil
		// updated_at is authored by the committing transaction; every other
		// field must equal the exact expected terminal transition.
		wantRequest["updated_at"], wantClaim["updated_at"] = requestAfter["updated_at"], claimAfter["updated_at"]
		if !reflect.DeepEqual(requestAfter, wantRequest) || !reflect.DeepEqual(claimAfter, wantClaim) {
			t.Fatal("successful reconciliation changed fields outside its expiry/cancellation transition")
		}
		var retainedEvents []map[string]any
		for _, event := range after["account_events"] {
			if event["verb"] != VerbMessageRequestExpired {
				retainedEvents = append(retainedEvents, event)
				continue
			}
			if event["actor_kind"] != ActorSystem || !reflect.DeepEqual(event["metadata"], map[string]any{
				"request_id": opened.Request.ID, "opening_message_id": opened.OpeningMessage.ID,
				"coordinator_agent_id": f.principals["coordinator"].ID, "max_assignees": "1",
			}) {
				t.Fatal("expiry event did not identify exactly the expected request transition")
			}
		}
		if !reflect.DeepEqual(retainedEvents, before["account_events"]) {
			t.Fatal("successful export changed pre-existing events")
		}
		for _, table := range []string{"accounts", "agent_messages", "agent_message_deliveries", "agent_message_request_candidates", "agent_message_request_selections"} {
			if !reflect.DeepEqual(after[table], before[table]) {
				t.Fatalf("successful reconciliation changed preserved %s rows", table)
			}
		}
		if attempt > 0 && !reflect.DeepEqual(after, committed) {
			t.Fatal("repeated successful export duplicated expiry effects or changed durable state")
		}
		committed = after
		archived := make(exportFailureRows)
		manifest, err := archiveexport.Read(ctx, bytes.NewReader(archive.Bytes()), archiveexport.ImportOptions{
			CurrentSchema: SchemaVersion(),
			Row: func(table string, raw []byte) error {
				if strings.Contains(table, "secret") || strings.Contains(table, "vault") || strings.Contains(table, "avatar") {
					return fmt.Errorf("synthetic fixture unexpectedly populated excluded %s rows", table)
				}
				if !slices.Contains(exportFailureTables, table) {
					return nil
				}
				row, err := decodeExportFailureRow(raw)
				if err != nil {
					return err
				}
				archived[table] = append(archived[table], row)
				return nil
			},
		})
		// Callbacks stage observations only. Trust them after Read validates
		// the entire gzip/tar stream and trailing per-table/chunk checksums.
		if err != nil {
			t.Fatalf("successful retry archive failed integrity verification: %v", err)
		}
		if manifest.AccountID != f.accountID || manifest.Status != "suspended" ||
			manifest.SchemaVersion != SchemaVersion() || manifest.Purpose != "" {
			t.Fatal("retry archive changed the operational account lifecycle or manifest scope")
		}
		for _, table := range exportFailureTables {
			sortExportFailureRows(archived[table])
			want := after[table]
			if table == "accounts" {
				// The portable account projection intentionally omits this
				// derived counter. Full database snapshots above still include
				// it and prove it remains zero through failure and retries.
				portable := maps.Clone(after[table][0])
				delete(portable, "retained_agent_email_attachment_bytes")
				want = []map[string]any{portable}
			}
			if !reflect.DeepEqual(archived[table], want) {
				t.Fatalf("verified archive %s rows differ from committed state", table)
			}
		}
	}
}

func newExportFailureFixture(t *testing.T) *messageRequestHardeningFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx, "archive-writer-failure@example.test", "archive writer failure", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	// Production realm/agent creation also initializes avatar state. Insert
	// only synthetic identity rows so this fixture leaves all avatar, secret
	// and vault tables empty; request lifecycle operations still use real APIs.
	realmID, err := id.New("realm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO realms(id,account_id,name) VALUES($1,$2,'default')`,
		realmID, provisioned.AccountID); err != nil {
		t.Fatal(err)
	}
	f := &messageRequestHardeningFixture{
		ctx: ctx, st: st, accountID: provisioned.AccountID, realm: Realm{ID: realmID, Name: "default"},
		agents: map[string]Agent{}, principals: map[string]Principal{},
	}
	for _, name := range []string{"coordinator", "worker"} {
		agentID, err := id.New("agent")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `INSERT INTO agents(id,realm_id,name) VALUES($1,$2,$3)`,
			agentID, realmID, name); err != nil {
			t.Fatal(err)
		}
		f.agents[name] = Agent{ID: agentID, Name: name}
		f.principals[name] = Principal{Kind: PrincipalAgent, ID: agentID, AccountID: f.accountID,
			RealmID: realmID, AgentName: name, AccountStatus: "active"}
	}
	return f
}

type exportFailureWriter struct {
	ctx     context.Context
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	err     error
}

func (w *exportFailureWriter) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	select {
	case <-w.release:
		return 0, w.err
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	}
}

func assertExportFailureAccountLock(ctx context.Context, t *testing.T, st *Store, accountID string, locked bool) {
	t.Helper()
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(probeCtx, st.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(probeCtx) }()
	tx, err := conn.Begin(probeCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(probeCtx) }()
	var id string
	err = tx.QueryRow(probeCtx, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE NOWAIT`, accountID).Scan(&id)
	if locked {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("paused export account lock error = %v, want PostgreSQL lock_not_available", err)
		}
	} else if err != nil || id != accountID {
		t.Fatalf("account lock was not released after writer failure: %v", err)
	}
	if err := tx.Rollback(probeCtx); err != nil {
		t.Fatal(err)
	}
}

type exportFailureRows map[string][]map[string]any

var exportFailureTables = []string{
	"accounts", "agent_messages", "agent_message_deliveries", "agent_message_requests",
	"agent_message_request_candidates", "agent_message_request_selections", "agent_message_request_claims", "account_events",
}

func exportFailureSnapshot(ctx context.Context, t *testing.T, st *Store, accountID string) exportFailureRows {
	t.Helper()
	out := make(exportFailureRows)
	for _, table := range exportFailureTables {
		column := "account_id"
		if table == "accounts" {
			column = "id"
		}
		rows, err := st.pool.Query(ctx, "SELECT to_jsonb(r) FROM "+pgx.Identifier{table}.Sanitize()+" r WHERE "+column+"=$1", accountID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			row, err := decodeExportFailureRow(raw)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			out[table] = append(out[table], row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		sortExportFailureRows(out[table])
	}
	return out
}

func decodeExportFailureRow(raw []byte) (map[string]any, error) {
	var row map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	err := decoder.Decode(&row)
	return row, err
}

func sortExportFailureRows(rows []map[string]any) {
	slices.SortFunc(rows, func(a, b map[string]any) int {
		left, _ := json.Marshal(a)
		right, _ := json.Marshal(b)
		return bytes.Compare(left, right)
	})
}

func exportFailureExpiryEvents(rows exportFailureRows) int {
	count := 0
	for _, event := range rows["account_events"] {
		if event["verb"] == VerbMessageRequestExpired {
			count++
		}
	}
	return count
}
