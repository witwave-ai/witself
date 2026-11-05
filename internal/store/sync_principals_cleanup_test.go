package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/witwave-ai/witself/internal/testenv"
)

type syncPrincipalFailureReporter interface {
	Helper()
	Error(...any)
	Fatal(...any)
}

type syncPrincipalCleanupPool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type syncPrincipalCatalogPool interface {
	syncPrincipalCleanupPool
	Begin(context.Context) (pgx.Tx, error)
}

// Both the catalog experiment and the committed-residue fault control use this
// allocation path. The control must fail if its cleanup registration is removed.
func withSyncPrincipalCatalogRoles(ctx context.Context, t syncPrincipalFailureReporter, pool syncPrincipalCatalogPool, use func(pgx.Tx, []string)) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal("principal check catalog transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var suffix string
	if tx.QueryRow(ctx, `SELECT pg_backend_pid()::text||'_'||txid_current()::text`).Scan(&suffix) != nil {
		t.Fatal("principal check catalog unique identity failed")
	}
	roles := syncPrincipalRoleNames()
	for i := range roles {
		roles[i] += "_dry_" + suffix
	}
	defer cleanupSyncPrincipalCatalogRoles(t, tx, pool, roles)
	for _, role := range roles {
		if _, err := tx.Exec(ctx, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+
			` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS`); err != nil {
			t.Fatal("principal check transactional role setup failed")
		}
	}
	use(tx, roles)
}

func syncPrincipalSuffixedRoles(roles []string) bool {
	base := syncPrincipalRoleNames()
	if len(roles) != len(base) {
		return false
	}
	var suffix string
	for i, role := range roles {
		part, ok := strings.CutPrefix(role, base[i]+"_dry_")
		if !ok || part == "" || strings.Trim(part, "0123456789_") != "" {
			return false
		}
		if i == 0 {
			suffix = part
		} else if part != suffix {
			return false
		}
	}
	return true
}

// Rollback precedes the residue read, and every generated name is attempted even
// when a previous DROP fails. The pool interface permits an independent executor
// oracle without requiring committed role DDL for every failure-path test.
func cleanupSyncPrincipalCatalogRoles(t syncPrincipalFailureReporter, tx pgx.Tx, pool syncPrincipalCleanupPool, roles []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
	if !syncPrincipalSuffixedRoles(roles) {
		t.Error("principal check suffixed role cleanup names refused")
		return
	}
	var residue bool
	if pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=ANY($1))`, roles).Scan(&residue) != nil {
		t.Error("principal check transactional role residue query failed")
	}
	for _, role := range roles {
		if _, err := pool.Exec(ctx, `DROP ROLE IF EXISTS `+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Error("principal check suffixed role cleanup failed")
		}
	}
	if residue {
		t.Error("principal check transactional role residue found")
	}
}

// The production admission function is injected only into this test assertion.
// An unexpected handle is a failure even when the accompanying error is correct,
// and it must be released before Fatal can unwind fixture/pool cleanup.
func requireSyncPrincipalRevokedAdmission(t syncPrincipalFailureReporter, admit func() (*SyncTx, error)) {
	t.Helper()
	unexpected, err := admit()
	if unexpected != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = unexpected.Rollback(ctx)
		cancel()
	}
	if unexpected != nil || !errors.Is(err, ErrSyncCapabilityRevoked) {
		t.Fatal("principal check revoked admission did not refuse")
	}
}

type syncPrincipalRecordedFailures struct {
	events *[]string
}

func (*syncPrincipalRecordedFailures) Helper() {}
func (r *syncPrincipalRecordedFailures) Error(args ...any) {
	*r.events = append(*r.events, "error: "+fmt.Sprint(args...))
}
func (r *syncPrincipalRecordedFailures) Fatal(args ...any) {
	*r.events = append(*r.events, "fatal: "+fmt.Sprint(args...))
	// testing.T.Fatal uses Goexit, so the fixture sees the same deferred-unwind
	// behavior without deliberately failing a parent test's testing.T.
	runtime.Goexit()
}

// Keep setup failures attached to the real test while capturing the expected
// residue report from the deliberately committed role-only control.
type syncPrincipalCleanupDatabaseReporter struct {
	*testing.T
	events *[]string
}

func (r *syncPrincipalCleanupDatabaseReporter) Error(args ...any) {
	*r.events = append(*r.events, "error: "+fmt.Sprint(args...))
}

type syncPrincipalCleanupRow func(...any) error

func (r syncPrincipalCleanupRow) Scan(dest ...any) error { return r(dest...) }

type syncPrincipalCleanupRecordingTx struct {
	pgx.Tx
	events     *[]string
	rolledBack bool
}

func (tx *syncPrincipalCleanupRecordingTx) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	*tx.events = append(*tx.events, query)
	return pgconn.CommandTag{}, nil
}
func (*syncPrincipalCleanupRecordingTx) QueryRow(context.Context, string, ...any) pgx.Row {
	return syncPrincipalCleanupRow(func(dest ...any) error {
		*dest[0].(*string) = "123_456"
		return nil
	})
}
func (tx *syncPrincipalCleanupRecordingTx) Rollback(context.Context) error {
	if !tx.rolledBack {
		*tx.events = append(*tx.events, "rollback")
		tx.rolledBack = true
	}
	return nil
}

type syncPrincipalCleanupRecordingPool struct {
	events        *[]string
	failFirstDrop bool
	drops         int
}

func (p *syncPrincipalCleanupRecordingPool) Begin(context.Context) (pgx.Tx, error) {
	return &syncPrincipalCleanupRecordingTx{events: p.events}, nil
}
func (p *syncPrincipalCleanupRecordingPool) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	*p.events = append(*p.events, query)
	p.drops++
	if p.failFirstDrop && p.drops == 1 {
		return pgconn.CommandTag{}, errors.New("injected cleanup refusal")
	}
	return pgconn.CommandTag{}, nil
}
func (p *syncPrincipalCleanupRecordingPool) QueryRow(context.Context, string, ...any) pgx.Row {
	*p.events = append(*p.events, "residue")
	return syncPrincipalCleanupRow(func(dest ...any) error {
		*dest[0].(*bool) = true
		return nil
	})
}

func TestSyncPrincipalCatalogCleanupUnwinds(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		name := "early_return"
		if fatal {
			name = "fatal"
		}
		t.Run(name, func(t *testing.T) {
			var events []string
			reporter := &syncPrincipalRecordedFailures{events: &events}
			pool := &syncPrincipalCleanupRecordingPool{events: &events, failFirstDrop: true}
			done := make(chan struct{})
			go func() {
				defer close(done)
				withSyncPrincipalCatalogRoles(context.Background(), reporter, pool, func(pgx.Tx, []string) {
					if fatal {
						reporter.Fatal("injected early fatal")
					}
				})
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("principal cleanup fault control did not unwind")
			}
			var want []string
			roles := syncPrincipalRoleNames()
			for i := range roles {
				roles[i] += "_dry_123_456"
				want = append(want, `CREATE ROLE `+pgx.Identifier{roles[i]}.Sanitize()+
					` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS`)
			}
			if fatal {
				want = append(want, "fatal: injected early fatal")
			}
			want = append(want, "rollback", "residue")
			for i, role := range roles {
				want = append(want, `DROP ROLE IF EXISTS `+pgx.Identifier{role}.Sanitize())
				if i == 0 {
					want = append(want, "error: principal check suffixed role cleanup failed")
				}
			}
			want = append(want, "error: principal check transactional role residue found")
			if !reflect.DeepEqual(events, want) {
				t.Fatal("principal cleanup must roll back, report residue, and attempt every generated DROP after early exit")
			}
		})
	}
}

func TestSyncPrincipalCatalogCleanupRefusesFixedRoles(t *testing.T) {
	var events []string
	reporter := &syncPrincipalRecordedFailures{events: &events}
	pool := &syncPrincipalCleanupRecordingPool{events: &events}
	tx := &syncPrincipalCleanupRecordingTx{events: &events}
	cleanupSyncPrincipalCatalogRoles(reporter, tx, pool, syncPrincipalRoleNames())
	if !reflect.DeepEqual(events, []string{"rollback", "error: principal check suffixed role cleanup names refused"}) {
		t.Fatal("principal cleanup accepted fixed role names")
	}
}

func TestSyncPrincipalCatalogCleanupCommittedResiduePostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	if testenv.ValidateHarnessPostgresDSN(dsn) != nil {
		t.Fatal("principal cleanup database selection refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("principal cleanup pool unavailable")
	}
	defer pool.Close()
	var allowed bool
	if pool.QueryRow(ctx, `SELECT rolsuper OR rolcreaterole FROM pg_roles WHERE rolname=current_user`).Scan(&allowed) != nil || !allowed {
		t.Fatal("principal cleanup role allocation unavailable")
	}
	var generated, events []string
	// Independent emergency recovery prevents the deliberate deletion mutations
	// from leaving roles. Assertions below inspect state before this recovery.
	defer func() {
		if !syncPrincipalSuffixedRoles(generated) {
			return
		}
		recoveryCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		for _, role := range generated {
			if _, err := pool.Exec(recoveryCtx, `DROP ROLE IF EXISTS `+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Error("principal cleanup emergency DROP failed")
			}
		}
		var residue bool
		if pool.QueryRow(recoveryCtx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=ANY($1))`, generated).Scan(&residue) != nil || residue {
			t.Error("principal cleanup emergency residue check failed")
		}
	}()
	reporter := &syncPrincipalCleanupDatabaseReporter{T: t, events: &events}
	withSyncPrincipalCatalogRoles(ctx, reporter, pool, func(tx pgx.Tx, roles []string) {
		generated = append([]string(nil), roles...)
		// Commit only the six generated role declarations, never migrations,
		// ownership, memberships, or changes to the fixed shared identities.
		if tx.Commit(ctx) != nil {
			t.Fatal("principal cleanup committed-residue setup failed")
		}
	})
	var residue bool
	if pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=ANY($1))`, generated).Scan(&residue) != nil || residue {
		t.Error("principal cleanup registration or DROP loop left committed suffixed roles")
	}
	if !reflect.DeepEqual(events, []string{"error: principal check transactional role residue found"}) {
		t.Error("principal cleanup did not report its committed-residue trigger")
	}
}

func TestSyncPrincipalRevokedAdmissionUnexpectedHandle(t *testing.T) {
	var events []string
	reporter := &syncPrincipalRecordedFailures{events: &events}
	tx := &syncPrincipalCleanupRecordingTx{events: &events}
	done := make(chan struct{})
	go func() {
		defer close(done)
		requireSyncPrincipalRevokedAdmission(reporter, func() (*SyncTx, error) {
			return &SyncTx{tx: tx}, ErrSyncCapabilityRevoked
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("principal revoked admission fault control did not unwind")
	}
	if !reflect.DeepEqual(events, []string{"rollback", "fatal: principal check revoked admission did not refuse"}) {
		t.Fatal("principal revoked admission must roll back unexpected handle before reporting failure")
	}
}

func TestSyncPrincipalRevokedAdmissionUnexpectedHandlePoolClosesPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	if testenv.ValidateHarnessPostgresDSN(dsn) != nil {
		t.Fatal("principal cleanup database selection refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("principal revoked admission pool unavailable")
	}
	defer pool.Close()
	started := time.Now()
	raw, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal("principal revoked admission fault transaction unavailable")
	}
	closeBound := min(15*time.Second, max(5*time.Second, 10*time.Since(started)))
	// Retain a separate recovery reference so a discarded-handle mutation has a
	// clean, bounded failure and cannot hang the normal pool cleanup.
	defer func() {
		recoveryCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = raw.Rollback(recoveryCtx)
	}()
	var events []string
	reporter := &syncPrincipalRecordedFailures{events: &events}
	done := make(chan struct{})
	go func() {
		defer close(done)
		requireSyncPrincipalRevokedAdmission(reporter, func() (*SyncTx, error) {
			return &SyncTx{tx: raw}, ErrSyncCapabilityRevoked
		})
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("principal revoked admission fault assertion did not unwind")
	}
	closed := make(chan struct{})
	go func() {
		pool.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(closeBound):
		// This recovery is outside the mutated assertion and runs before testing
		// reports failure. Rollback also releases a pool transaction after error.
		recoveryCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		_ = raw.Rollback(recoveryCtx)
		stop()
		select {
		case <-closed:
		case <-time.After(closeBound):
			t.Error("principal revoked admission emergency pool release failed")
		}
		t.Error("principal revoked admission pool close exceeded proportional bound")
	}
	if !reflect.DeepEqual(events, []string{"fatal: principal check revoked admission did not refuse"}) {
		t.Error("principal revoked admission unexpected handle was not reported")
	}

}
