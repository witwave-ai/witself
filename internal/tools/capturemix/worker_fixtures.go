package capturemix

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// These are candidate rows for the real registered worker loops. The driver
// never invokes a production job function directly. Seed only while capture is
// inactive, after the running worker's initial empty attempt, then verify these
// exact predicates both before activation and after the observed outcomes.
// Account scan state is itself covered, so a finite retention policy supplies a
// genuine write attempt even when there is no retained content to delete.
var workerCandidateSeeds = []struct{ name, sql string }{
	{"retention_policies", `UPDATE accounts SET plan_policies=plan_policies ||
 '{"transcript_retention_days":1,"message_retention_days":1,"agent_email_retention_days":1}'::jsonb
 WHERE id='mix_active'`},
	{"message_rate_bucket_cleanup", `INSERT INTO agent_message_rate_buckets
 (account_id,realm_id,dimension,scope,scope_id,theoretical_arrival_microseconds,updated_at)
 VALUES('mix_active','mix_active_realm','message_sent','agent','mix_active_agent',1,clock_timestamp()-interval '2 days')`},
	{"agent_email_rate_bucket_cleanup", `INSERT INTO agent_email_rate_buckets
 (account_id,realm_id,dimension,scope,scope_id,theoretical_arrival_nanoseconds,updated_at)
 VALUES('mix_active','mix_active_realm','email_received','realm','mix_active_realm',1,clock_timestamp()-interval '2 days')`},
	// Inbound failure must not starve the separate outbound table's cleanup.
	// This is a real inactive-account mutation by the same live worker process.
	{"inactive_worker_companion", `INSERT INTO agent_email_outbound_rate_buckets
 (account_id,realm_id,lane,scope,scope_id,theoretical_arrival_microseconds,updated_at)
 VALUES('mix_inactive','','admission','account','mix_inactive',1,clock_timestamp()-interval '2 days')`},
	{"avatar_style_pack", `INSERT INTO avatar_style_packs(account_id,realm_id,id,current_version)
 VALUES('mix_active','mix_active_realm','capturemix',1)`},
	{"avatar_style_version", `INSERT INTO avatar_style_pack_versions
 (account_id,realm_id,style_pack_id,version,name,description,style_spec,created_by_kind)
 VALUES('mix_active','mix_active_realm','capturemix',1,'Fixture','Disposable fixture','{}','system')`},
	{"avatar_style_selection", `INSERT INTO realm_avatar_styles(account_id,realm_id,style_pack_id,style_pack_version,revision)
 VALUES('mix_active','mix_active_realm','capturemix',1,1)
 ON CONFLICT(account_id,realm_id) DO UPDATE SET style_pack_id='capturemix',style_pack_version=1,revision=1`},
	{"avatar_style_rollout", `INSERT INTO avatar_style_rollout_jobs(account_id,realm_id,style_revision,style_pack_id,style_pack_version)
 VALUES('mix_active','mix_active_realm',1,'capturemix',1)`},
	{"support_ticket_age_out", `INSERT INTO support_tickets
 (id,account_id,opened_by_kind,opened_by_id,subject,state,last_activity_at)
 VALUES('mix_ticket','mix_active','owner','mix_owner','Disposable fixture','awaiting_customer',clock_timestamp()-interval '40 days')`},
	{"support_ticket_human_response", `INSERT INTO support_ticket_messages
 (id,ticket_id,account_id,author_kind,author_id,body)
 VALUES('mix_ticket_reply','mix_ticket','mix_active','fleet_admin','fixture','Disposable fixture')`},
	{"transcript_storage_reconcile", `INSERT INTO transcript_storage_usage(account_id,shard,retained_bytes,exact)
 VALUES('mix_active',0,1,false) ON CONFLICT(account_id,shard)
 DO UPDATE SET retained_bytes=1,exact=false,verified_at=NULL`},
	// Mail-address constraints require a canonical realm_<base32> identifier.
	{"outbound_realm", `INSERT INTO realms(id,account_id,name) VALUES('realm_aaaaaaaaaaaaaaaa','mix_active','capturemix_email')`},
	{"outbound_agent", `INSERT INTO agents(id,realm_id,name) VALUES('mix_email_agent','realm_aaaaaaaaaaaaaaaa','fixture_email')`},
	{"outbound_address", `INSERT INTO agent_email_addresses
 (id,account_id,realm_id,provisioned_agent_id,domain,agent_segment,realm_label,local_part,provisioning_kind)
 VALUES('eaddr_aaaaaaaaaaaaaaaa','mix_active','realm_aaaaaaaaaaaaaaaa','mix_email_agent','witmail.net',
 'fixture','aaaaaaaaaaaaaaaa','fixture.aaaaaaaaaaaaaaaa','derived')`},
	{"agent_email_outbound", `INSERT INTO agent_email_outbound_messages
 (id,account_id,realm_id,owner_agent_id,address_id,from_address,reply_to_address,to_address,
 body_text,request_kind,thread_key,idempotency_key_hash,request_hash,next_attempt_at)
 VALUES('esnd_aaaaaaaaaaaaaaaa','mix_active','realm_aaaaaaaaaaaaaaaa','mix_email_agent','eaddr_aaaaaaaaaaaaaaaa',
 'fixture.aaaaaaaaaaaaaaaa@send.witmail.net','fixture.aaaaaaaaaaaaaaaa@witmail.net','fixture@example.invalid',
 'Disposable fixture','direct','fixture',repeat('1',64),repeat('2',64),clock_timestamp())`},
	// Both reviewed binaries refuse purge at the selected-sync destruction
	// fence before domain DML. This is not a writer-admission guard receipt.
	{"account_purge", `INSERT INTO accounts(id,display_name,status,is_default,closed_at)
 VALUES('mix_purge','Disposable purge fixture','closed',false,clock_timestamp()-interval '40 days')`},
}

var workerCandidatePredicates = []struct{ job, sql string }{
	{"avatar_style_rollout", `SELECT EXISTS(SELECT 1 FROM avatar_style_rollout_jobs WHERE account_id='mix_active' AND status='pending' AND batch_count=0 AND failure_count=0)`},
	{"message_rate_bucket_cleanup", `SELECT EXISTS(SELECT 1 FROM agent_message_rate_buckets WHERE account_id='mix_active' AND theoretical_arrival_microseconds=1)`},
	{"agent_email_rate_bucket_cleanup", `SELECT EXISTS(SELECT 1 FROM agent_email_rate_buckets WHERE account_id='mix_active' AND theoretical_arrival_nanoseconds=1)`},
	{"agent_email_outbound", `SELECT EXISTS(SELECT 1 FROM agent_email_outbound_messages WHERE id='esnd_aaaaaaaaaaaaaaaa' AND state='queued' AND attempt_count=0 AND claim_generation=0)`},
	{"transcript_retention", `SELECT NOT EXISTS(SELECT 1 FROM transcript_retention_account_scan_state WHERE account_id='mix_active')`},
	{"message_retention", `SELECT NOT EXISTS(SELECT 1 FROM message_retention_account_scan_state WHERE account_id='mix_active')`},
	{"agent_email_retention", `SELECT NOT EXISTS(SELECT 1 FROM agent_email_retention_account_scan_state WHERE account_id='mix_active')`},
	{"account_purge", `SELECT EXISTS(SELECT 1 FROM accounts WHERE id='mix_purge' AND status='closed' AND purged_at IS NULL)`},
	{"support_ticket_age_out", `SELECT EXISTS(SELECT 1 FROM support_tickets WHERE id='mix_ticket' AND state='awaiting_customer' AND resolved_at IS NULL)`},
	{"transcript_storage_reconcile", `SELECT EXISTS(SELECT 1 FROM transcript_storage_usage WHERE account_id='mix_active' AND shard=0 AND retained_bytes=1 AND NOT exact AND verified_at IS NULL)`},
}

func (f *fixture) seedWorkerCandidates(ctx context.Context) error {
	tx, err := f.db.Begin(ctx)
	if err != nil {
		return errors.New("capturemix worker fixture transaction failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, seed := range workerCandidateSeeds {
		if _, err := tx.Exec(ctx, seed.sql); err != nil {
			// name is a reviewed fixed token, never provider/database data.
			return errors.New("capturemix worker fixture failed: " + seed.name)
		}
	}
	if tx.Commit(ctx) != nil {
		return errors.New("capturemix worker fixture commit failed")
	}
	return nil
}

// Only cell-local scheduler rows are changed here, never domain or guard rows.
// The target lane is made due deterministically so a bounded matrix does not
// depend on walking fifteen empty lanes before finding its seeded account.
func (f *fixture) scheduleWorkerCandidateLanes(ctx context.Context) error {
	for _, table := range []string{"transcript_retention_worker_lanes", "message_retention_worker_lanes", "agent_email_retention_worker_lanes"} {
		query := `UPDATE ` + pgx.Identifier{table}.Sanitize() + ` SET next_run_at=CASE
 WHEN lane_id=get_byte(sha256('mix_active'::bytea),0)%16 THEN clock_timestamp()-interval '1 day'
 ELSE clock_timestamp()+interval '1 day' END WHERE mode='enforce'`
		if _, err := f.db.Exec(ctx, query); err != nil {
			return errors.New("capturemix worker candidate scheduling failed")
		}
	}
	return nil
}

// The startup empty scan leases purge's excluded scheduler before candidates
// exist. Make that singleton due while the worker is paused so its first seeded
// tick attempts the candidate, independent of the startup lease's clock phase.
func (f *fixture) schedulePurgeCandidate(ctx context.Context) error {
	tag, err := f.db.Exec(ctx, `UPDATE account_purge_sweep_state SET next_run_at=clock_timestamp()-interval '1 day' WHERE singleton`)
	if err != nil || tag.RowsAffected() != 1 {
		return errors.New("capturemix purge scheduler rearm failed")
	}
	return nil
}

func (f *fixture) assertWorkerCandidateRows(ctx context.Context) error {
	for _, predicate := range workerCandidatePredicates {
		var unchanged bool
		if f.db.QueryRow(ctx, predicate.sql).Scan(&unchanged) != nil || !unchanged {
			return errors.New("capturemix worker candidate changed: " + predicate.job)
		}
	}
	return nil
}

func (f *fixture) inactiveWorkerCompanionCompleted(ctx context.Context) (bool, string) {
	var completed bool
	if err := f.db.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM agent_email_outbound_rate_buckets WHERE account_id='mix_inactive' AND lane='admission' AND scope='account')`).Scan(&completed); err != nil {
		return false, companionQueryFailureClass(ctx, err)
	}
	return completed, ""
}

// Classify before discarding the database error; only these fixed tokens may
// enter the worker-outcome diagnostic. Context termination takes precedence
// over a driver-specific error returned while that context is ending.
func companionQueryFailureClass(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "query_deadline"
	case errors.Is(err, context.Canceled):
		return "query_canceled"
	case errors.Is(err, pgconn.ErrConnClosed), errors.Is(err, sql.ErrConnDone),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
		// pgx returns the transport error on the first failed read, then its
		// closed-connection sentinel on later queries. Both are closed here;
		// the companion scans a boolean and has no custom decoder returning EOF.
		return "query_failed:connection_closed"
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, sql.ErrNoRows):
		return "query_failed:no_rows"
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr != nil && len(pgerr.Code) == 5 {
		// PostgreSQL SQLSTATEs contain five uppercase ASCII letters or digits.
		// Validate the entire code before exposing only its two-character class;
		// malformed provider data must never enter the value-free diagnostic.
		valid := true
		for _, char := range pgerr.Code {
			if (char < '0' || char > '9') && (char < 'A' || char > 'Z') {
				valid = false
				break
			}
		}
		if valid {
			return "query_failed:sqlstate_" + pgerr.Code[:2]
		}
	}
	return "query_failed:other"
}
