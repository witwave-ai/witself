package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/agentemail"
	"github.com/witwave-ai/witself/internal/testenv"
)

func cohortStore(t *testing.T, dsn string) *Store {
	t.Helper()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return st
}

func cohortAccount(t *testing.T, st *Store) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	a, err := st.ProvisionAccount(ctx, "cohort@example.test", "cohort fixture", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ActivateAccount(ctx, a.AccountID); err != nil || !ok {
		t.Fatal("activate fixture", err)
	}
	r, err := st.CreateRealm(ctx, a.AccountID, "cohort realm")
	if err != nil {
		t.Fatal(err)
	}
	g, err := st.CreateAgent(ctx, a.AccountID, r.ID, "cohort agent")
	if err != nil {
		t.Fatal(err)
	}
	return a.AccountID, r.ID, g.ID
}

func cohortScope(account, canary, audience string) AgentEmailReceiveScope {
	return AgentEmailReceiveScope{Enabled: true, Mode: AgentEmailReceiveModeProduction, Domain: "witmail.net", Audience: audience, AccountIDs: map[string]bool{account: true}, RetryCanaryAgentID: canary}
}

func requireResidency(t *testing.T, st *Store, scope AgentEmailReceiveScope, want AgentEmailProductionCohortResidency) {
	t.Helper()
	got, err := st.ValidateAgentEmailProductionCohort(context.Background(), scope)
	if err != nil || got != want {
		t.Fatalf("residency = %+v, error=%v; want %+v", got, err, want)
	}
}

func TestAgentEmailCohortAbsentAccountsPostgres(t *testing.T) {
	st := cohortStore(t, testenv.RequirePostgres(t))
	ctx := context.Background()
	account, _, _ := cohortAccount(t, st)
	const absent = "acc_zzzzzzzzzzzzzzzz"
	scope := cohortScope(account, "", "source-cell")
	scope.AccountIDs[absent] = true
	requireResidency(t, st, scope, AgentEmailProductionCohortResidency{2, 1, 0, 1, 0, AgentEmailRetryCanaryNone})
	if _, err := st.PreflightAgentEmailProductionCohort(ctx, scope); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("strict preflight: %v", err)
	}
	if _, err := st.ReconcileAgentEmailProductionCohortWithOverrides(ctx, scope, nil); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("strict reconcile: %v", err)
	}
	if _, err := st.ListAgentEmailProductionCanaryAgents(ctx, scope); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("strict manifest: %v", err)
	}
	var mailboxes int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM agent_email_mailboxes`).Scan(&mailboxes); err != nil || mailboxes != 0 {
		t.Fatal("refused reconcile wrote mailboxes", err)
	}
	for _, status := range []string{"suspended", "closed"} {
		if _, err := st.pool.Exec(ctx, `UPDATE accounts SET status=$2 WHERE id=$1`, account, status); err != nil {
			t.Fatal(err)
		}
		if status == "suspended" {
			requireResidency(t, st, scope, AgentEmailProductionCohortResidency{2, 1, 0, 1, 0, AgentEmailRetryCanaryNone})
		} else {
			requireResidency(t, st, scope, AgentEmailProductionCohortResidency{2, 0, 0, 1, 1, AgentEmailRetryCanaryNone})
		}
	}
	scope = cohortScope(absent, "", "source-cell")
	requireResidency(t, st, scope, AgentEmailProductionCohortResidency{1, 0, 0, 1, 0, AgentEmailRetryCanaryNone})
	// Historical schemas without the receipts table must classify absence as unknown.
	if _, err := st.pool.Exec(ctx, `DROP TABLE account_evacuation_finalizations`); err != nil {
		t.Fatal(err)
	}
	requireResidency(t, st, scope, AgentEmailProductionCohortResidency{1, 0, 0, 1, 0, AgentEmailRetryCanaryNone})
}

func TestAgentEmailCohortCanaryPostgres(t *testing.T) {
	st := cohortStore(t, testenv.RequirePostgres(t))
	ctx := context.Background()
	account, realm, agent := cohortAccount(t, st)
	scope := cohortScope(account, agent, "source-cell")
	requireResidency(t, st, scope, AgentEmailProductionCohortResidency{1, 1, 0, 0, 0, AgentEmailRetryCanaryReady})
	scope.RetryCanaryAgentID = ""
	requireResidency(t, st, scope, AgentEmailProductionCohortResidency{1, 1, 0, 0, 0, AgentEmailRetryCanaryNone})
	scope.RetryCanaryAgentID = "agent_zzzzzzzzzzzzzzzz"
	requireResidency(t, st, scope, AgentEmailProductionCohortResidency{1, 1, 0, 0, 0, AgentEmailRetryCanaryAbsent})
	if _, err := st.PreflightAgentEmailProductionCohort(ctx, scope); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("strict absent canary: %v", err)
	}
	scope.RetryCanaryAgentID = agent
	scope.AccountIDs = map[string]bool{"acc_zzzzzzzzzzzzzzzz": true}
	if _, err := st.ValidateAgentEmailProductionCohort(ctx, scope); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("non-cohort canary: %v", err)
	}
	scope.AccountIDs = map[string]bool{account: true}
	if _, err := st.pool.Exec(ctx, `UPDATE realms SET email_route_state='closing', email_route_generation=2, email_route_operation_id='cohort-test' WHERE id=$1`, realm); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ValidateAgentEmailProductionCohort(ctx, scope); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("non-live route: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE realms SET email_route_state='live', email_route_operation_id=NULL WHERE id=$1`, realm); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAgent(ctx, account, realm, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ValidateAgentEmailProductionCohort(ctx, scope); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("deleted canary: %v", err)
	}
}

func cohortArchive(t *testing.T, st *Store, account, epoch string) []byte {
	t.Helper()
	ctx := context.Background()
	if _, err := st.BeginAccountEvacuation(ctx, account, epoch, "cohort move"); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := st.ExportAccountEvacuation(ctx, account, epoch, "source-cell", "test", &b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestAgentEmailCohortMovePostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	source, destination := cohortStore(t, dsn), cohortStore(t, dsn)
	ctx := context.Background()
	account, realm, agent := cohortAccount(t, source)
	srcScope := cohortScope(account, agent, "source-cell")
	dstScope := cohortScope(account, agent, "civo-prod-use1-serving")
	address, err := source.EnsureAgentEmailMailbox(ctx, srcScope, account, realm, agent, "")
	if err != nil {
		t.Fatal(err)
	}
	requireResidency(t, source, srcScope, AgentEmailProductionCohortResidency{1, 1, 0, 0, 0, AgentEmailRetryCanaryReady})
	requireResidency(t, destination, dstScope, AgentEmailProductionCohortResidency{1, 0, 0, 1, 0, AgentEmailRetryCanaryAbsent})
	move := func(from, to *Store, epoch string) {
		archive := cohortArchive(t, from, account, epoch)
		if _, _, err := to.ImportAccountEvacuation(ctx, account, epoch, bytes.NewReader(archive)); err != nil {
			t.Fatal(err)
		}
		if _, err := to.CompleteAccountEvacuation(ctx, account, epoch); err != nil {
			t.Fatal(err)
		}
		if _, err := from.FinalizeAccountEvacuationSource(ctx, account, epoch); err != nil {
			t.Fatal(err)
		}
	}
	move(source, destination, "cohort-forward")
	requireResidency(t, source, srcScope, AgentEmailProductionCohortResidency{1, 0, 1, 0, 0, AgentEmailRetryCanaryAbsent})
	requireResidency(t, destination, dstScope, AgentEmailProductionCohortResidency{1, 1, 0, 0, 0, AgentEmailRetryCanaryReady})
	raw := []byte("From: sender@example.test\r\nTo: " + address.Address + "\r\nSubject: cohort move\r\n\r\nhello\r\n")
	digest := sha256.Sum256(raw)
	in := AgentEmailIngestInput{Raw: raw, Relay: agentemail.RelayMetadata{Timestamp: time.Now().Unix(), KeyID: "test-relay", Audience: dstScope.Audience, EnvelopeSender: "sender@example.test", EnvelopeRecipient: address.Address, RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:])}}
	if _, err := destination.IngestAgentEmailPilot(ctx, dstScope, in); err != nil {
		t.Fatal("pre-move scope did not accept ingest", err)
	}
	omitted := cohortScope("acc_zzzzzzzzzzzzzzzz", "", dstScope.Audience)
	if _, err := destination.IngestAgentEmailPilot(ctx, omitted, in); !errors.Is(err, ErrAgentEmailReceiveCohortDeferred) {
		t.Fatalf("omitted cohort: %v", err)
	}
	in.Relay.Audience = srcScope.Audience
	if _, err := destination.IngestAgentEmailPilot(ctx, dstScope, in); !errors.Is(err, ErrAgentEmailInputInvalid) {
		t.Fatalf("source audience: %v", err)
	}
	move(destination, source, "cohort-reverse")
	requireResidency(t, destination, dstScope, AgentEmailProductionCohortResidency{1, 0, 1, 0, 0, AgentEmailRetryCanaryAbsent})
	requireResidency(t, source, srcScope, AgentEmailProductionCohortResidency{1, 1, 0, 0, 0, AgentEmailRetryCanaryReady})
}

type cohortImportQueryer struct {
	agentEmailProductionQueryer
	afterMissing func()
}

func (q *cohortImportQueryer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := q.agentEmailProductionQueryer.QueryRow(ctx, sql, args...)
	if strings.HasPrefix(sql, "SELECT status FROM accounts") {
		return cohortImportRow{row: row, afterMissing: q.afterMissing}
	}
	return row
}

type cohortImportRow struct {
	row          pgx.Row
	afterMissing func()
}

func (r cohortImportRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		r.afterMissing()
	}
	return err
}

func TestAgentEmailCohortSnapshotPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	source := cohortStore(t, dsn)
	account, _, agent := cohortAccount(t, source)
	archive := cohortArchive(t, source, account, "cohort-snapshot")
	scope := cohortScope(account, agent, "destination-cell")
	ctx := context.Background()
	for _, snapshot := range []bool{true, false} {
		destination := cohortStore(t, dsn)
		var queryer agentEmailProductionQueryer = destination.pool
		var snapshotTx pgx.Tx
		if snapshot {
			tx, err := destination.beginAgentEmailCohortValidationTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			var isolation, readOnly string
			if err := tx.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
				t.Fatal(err)
			}
			if err := tx.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
				t.Fatal(err)
			}
			if isolation != "repeatable read" || readOnly != "on" {
				t.Fatal("validation transaction options regressed")
			}
			queryer = tx
			snapshotTx = tx
		}
		decorated := &cohortImportQueryer{agentEmailProductionQueryer: queryer, afterMissing: func() {
			if _, _, err := destination.ImportAccountEvacuation(ctx, account, "cohort-snapshot", bytes.NewReader(archive)); err != nil {
				t.Fatal(err)
			}
		}}
		got, err := validateAgentEmailProductionCohortQuery(ctx, decorated, scope)
		if snapshotTx != nil {
			if err := snapshotTx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if snapshot {
			want := AgentEmailProductionCohortResidency{1, 0, 0, 1, 0, AgentEmailRetryCanaryAbsent}
			if err != nil || got != want {
				t.Fatalf("snapshot = %+v / %v", got, err)
			}
		} else if !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
			t.Fatalf("pool negative control missed straddled import: %v", err)
		}
	}
}
